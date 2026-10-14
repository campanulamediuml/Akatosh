package search

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"query":"今天天气"}`, "今天天气"},
		{`{"query": "  多个 词  "}`, "多个 词"},
		{`不是JSON`, "不是JSON"},             // 畸形输入退化为整串
		{`{"other":1}`, `{"other":1}`},   // 没有 query 字段也退化为整串
		{`{"query":""}`, `{"query":""}`}, // 空 query 同样退化
	}
	for _, c := range cases {
		if got := ParseQuery(c.in); got != c.want {
			t.Errorf("ParseQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("短", 10); got != "短" {
		t.Errorf("短文本不该截断: %q", got)
	}
	long := strings.Repeat("字", 400)
	got := truncateRunes(long, 300)
	if len([]rune(got)) != 301 { // 300 + 省略号
		t.Errorf("截断长度不对: %d runes", len([]rune(got)))
	}
}

func TestFormatToolResult(t *testing.T) {
	if got := FormatToolResult(nil, fmt.Errorf("超时")); !strings.Contains(got, "超时") {
		t.Errorf("错误要如实告诉模型: %q", got)
	}
	if got := FormatToolResult(nil, nil); !strings.Contains(got, "没有搜到") {
		t.Errorf("空结果要有说明: %q", got)
	}
	got := FormatToolResult([]Result{{Title: "t", URL: "u", Content: "c"}}, nil)
	if !strings.Contains(got, `"title":"t"`) {
		t.Errorf("正常结果要是 JSON: %q", got)
	}
}

func TestSearchTrimsAndCaps(t *testing.T) {
	// 返回 10 条、摘要超长：应截到 6 条、每条摘要 ≤300 rune
	var sb strings.Builder
	sb.WriteString(`{"results":[`)
	for i := 0; i < 10; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"title":"标题%d","url":"https://%d.example.com","content":%q}`, i, i, strings.Repeat("长", 400))
	}
	sb.WriteString(`]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sb.String())
	}))
	t.Cleanup(srv.Close)

	results, err := New(srv.URL).Search(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != maxResults {
		t.Fatalf("应截到 %d 条，实际 %d", maxResults, len(results))
	}
	for _, r := range results {
		if n := len([]rune(r.Content)); n > maxContentRunes+1 {
			t.Fatalf("摘要超长: %d runes", n)
		}
	}
}

func TestSearchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "forbidden")
	}))
	t.Cleanup(srv.Close)
	if _, err := New(srv.URL).Search(context.Background(), "q"); err == nil {
		t.Fatal("非 2xx 要返回错误")
	}
}

func TestNewRecord(t *testing.T) {
	rec := NewRecord("q", []Result{{Title: "t", URL: "u", Content: "很长的摘要不该进 record"}}, nil)
	if len(rec.Results) != 1 || rec.Results[0].Title != "t" {
		t.Fatalf("record 内容不对: %+v", rec)
	}
	rec2 := NewRecord("q", nil, fmt.Errorf("挂了"))
	if rec2.Error == "" {
		t.Fatal("失败要记 error")
	}
}

func TestToolDefShape(t *testing.T) {
	def := ToolDef()
	if def.Type != "function" || def.Function.Name != ToolName {
		t.Fatalf("工具声明不对: %+v", def)
	}
	if def.Function.Description == "" || def.Function.Parameters == nil {
		t.Fatal("description / parameters 不能为空")
	}
}
