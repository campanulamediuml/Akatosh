package sandbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/run" {
			t.Errorf("路径不对: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"stdout":"hello\n","stderr":"","exit_code":0,"timed_out":false,"duration_ms":12}`)
	}))
	t.Cleanup(srv.Close)

	res, err := New(srv.URL).Run(context.Background(), "print('hello')")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "hello\n" || res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("结果不对: %+v", res)
	}
}

func TestRunHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	if _, err := New(srv.URL).Run(context.Background(), "x=1"); err == nil {
		t.Fatal("非 2xx 要返回错误")
	}
}

func TestRunTruncatesLongOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"stdout":%q,"stderr":"","exit_code":0}`, strings.Repeat("字", 9000))
	}))
	t.Cleanup(srv.Close)
	res, err := New(srv.URL).Run(context.Background(), "print('x'*9000)")
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(res.Stdout)); n > maxOutputRunes+1 {
		t.Fatalf("输出没截断: %d runes", n)
	}
}

func TestParseCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"code":"print(1)"}`, "print(1)"},
		{`{"code": "  x = 2  "}`, "  x = 2  "}, // code 内容原样保留（不去空白）
		{`畸形`, "畸形"},                           // 解析失败退化为整串
	}
	for _, c := range cases {
		if got := ParseCode(c.in); got != c.want {
			t.Errorf("ParseCode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatToolResult(t *testing.T) {
	if got := FormatToolResult(Result{}, fmt.Errorf("沙箱不可达")); !strings.Contains(got, "沙箱不可达") {
		t.Errorf("错误要如实告诉模型: %q", got)
	}
	got := FormatToolResult(Result{Stdout: "42\n", ExitCode: 0}, nil)
	if !strings.Contains(got, `"stdout":"42\n"`) {
		t.Errorf("正常结果要是 JSON: %q", got)
	}
}

func TestNewRecord(t *testing.T) {
	rec := NewRecord("print(1)", Result{Stdout: "1\n", ExitCode: 0}, nil)
	if rec.Type != "python" || rec.Code != "print(1)" || rec.Stdout != "1\n" {
		t.Fatalf("record 不对: %+v", rec)
	}
	rec2 := NewRecord("x", Result{}, fmt.Errorf("挂了"))
	if rec2.Error == "" {
		t.Fatal("执行失败要记 error")
	}
}

func TestToolDefShape(t *testing.T) {
	def := ToolDef()
	if def.Type != "function" || def.Function.Name != ToolName || def.Function.Description == "" {
		t.Fatalf("工具声明不对: %+v", def)
	}
}

func TestRenderPDF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/render-pdf" {
			t.Errorf("路径不对: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"total_pages":3,"images":[{"page":1,"mime":"image/jpeg","data":"aGVsbG8="},{"page":3,"mime":"image/jpeg","data":"d29ybGQ="}]}`)
	}))
	t.Cleanup(srv.Close)
	out, err := New(srv.URL).RenderPDF(context.Background(), "/files/scan.pdf", []int{1, 3})
	if err != nil {
		t.Fatal(err)
	}
	if out.TotalPages != 3 || len(out.Images) != 2 || out.Images[1].Page != 3 || out.Images[1].Data != "d29ybGQ=" {
		t.Fatalf("渲染结果不对: %+v", out)
	}
}

func TestRenderPDFRejectsBadResponses(t *testing.T) {
	for name, body := range map[string]string{
		" malformed ":     `{"total_pages":`,
		" invalid image ": `{"total_pages":2,"images":[{"page":3,"mime":"image/jpeg","data":"aGVsbG8="}]}`,
	} {
		t.Run(strings.TrimSpace(name), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			t.Cleanup(srv.Close)
			if _, err := New(srv.URL).RenderPDF(context.Background(), "/files/scan.pdf", []int{1}); err == nil {
				t.Fatal("无效渲染响应应返回错误")
			}
		})
	}
}

func TestSandboxTimeoutCoversPyboxExecution(t *testing.T) {
	if got := New("http://pybox").HTTP.Timeout.Seconds(); got < 25 {
		t.Fatalf("Go 客户端不应早于 pybox 的 20 秒执行上限超时: %.0fs", got)
	}
}
