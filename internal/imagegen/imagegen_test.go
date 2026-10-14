package imagegen

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 假上游 + 假图床：生成接口回一个 URL，再从这个 URL 下载图片字节
func fakeUpstream(t *testing.T, imgBody string, imgMIME string) (*Client, *string) {
	t.Helper()
	var gotBody string
	// 图床
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", imgMIME)
		fmt.Fprint(w, imgBody)
	}))
	t.Cleanup(imgSrv.Close)
	// 生图接口
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(strings.Builder)
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		buf.Write(b)
		gotBody = buf.String()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":[{"url":%q}]}`, imgSrv.URL+"/img.png")
	}))
	t.Cleanup(apiSrv.Close)
	return New(apiSrv.URL, "k", "m"), &gotBody
}

func TestGenerateOK(t *testing.T) {
	cli, gotBody := fakeUpstream(t, "PNG-BYTES", "image/png")
	b, mime, err := cli.Generate(context.Background(), "a blue whale", "1024x1024")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "PNG-BYTES" || mime != "image/png" {
		t.Fatalf("下载结果不对: %q %s", b, mime)
	}
	if !strings.Contains(*gotBody, `"image_size":"1024x1024"`) || !strings.Contains(*gotBody, `"model":"m"`) {
		t.Fatalf("请求体不对: %s", *gotBody)
	}
}

func TestGenerateRejectsNonImage(t *testing.T) {
	cli, _ := fakeUpstream(t, "not an image", "text/html")
	if _, _, err := cli.Generate(context.Background(), "x", ""); err == nil {
		t.Fatal("下载到非图片要报错")
	}
}

func TestGenerateUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"key 不对"}`)
	}))
	t.Cleanup(srv.Close)
	if _, _, err := New(srv.URL, "bad", "m").Generate(context.Background(), "x", ""); err == nil {
		t.Fatal("上游非 2xx 要报错")
	}
}

func TestParsePrompt(t *testing.T) {
	p, s := ParsePrompt(`{"prompt":"a cat","size":"1024x576"}`)
	if p != "a cat" || s != "1024x576" {
		t.Fatalf("解析不对: %q %q", p, s)
	}
	p, s = ParsePrompt(`{"prompt":"a cat"}`)
	if p != "a cat" || s != "" {
		t.Fatalf("缺省 size 应为空串: %q %q", p, s)
	}
	p, _ = ParsePrompt(`畸形`)
	if p != "畸形" {
		t.Fatalf("畸形输入退化为整串: %q", p)
	}
}

func TestFormatToolResult(t *testing.T) {
	if got := FormatToolResult(nil, 8); !strings.Contains(got, "已生成") || !strings.Contains(got, `"attachment_id": 8`) {
		t.Errorf("成功回执: %q", got)
	}
	if got := FormatToolResult(fmt.Errorf("超时"), 0); !strings.Contains(got, "超时") {
		t.Errorf("失败要如实告诉模型: %q", got)
	}
}

func TestToolDefShape(t *testing.T) {
	def := ToolDef()
	if def.Type != "function" || def.Function.Name != ToolName || def.Function.Description == "" {
		t.Fatalf("工具声明不对: %+v", def)
	}
}
