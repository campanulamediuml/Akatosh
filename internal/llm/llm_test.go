package llm

import "testing"

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		// 没写版本段 → 补 /v1
		{"https://api.deepseek.com", "https://api.deepseek.com/v1"},
		{"https://api.deepseek.com/", "https://api.deepseek.com/v1"},
		{"  https://api.deepseek.com  ", "https://api.deepseek.com/v1"},
		{"http://localhost:11434", "http://localhost:11434/v1"},
		{"http://192.168.1.10:8000", "http://192.168.1.10:8000/v1"},
		// 已经带版本段 → 原样保留
		{"https://api.deepseek.com/v1", "https://api.deepseek.com/v1"},
		{"https://api.siliconflow.cn/v1", "https://api.siliconflow.cn/v1"},
		{"https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/paas/v4"},
		{"https://generativelanguage.googleapis.com/v1beta/openai", "https://generativelanguage.googleapis.com/v1beta/openai"},
		{"https://dashscope.aliyuncs.com/compatible-mode/v1", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		// 用户把完整端点粘进来了 → 帮他剥掉
		{"https://api.deepseek.com/v1/chat/completions", "https://api.deepseek.com/v1"},
		{"https://api.deepseek.com/chat/completions", "https://api.deepseek.com/v1"},
		{"https://api.deepseek.com/v1/models", "https://api.deepseek.com/v1"},
		// 空值
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeBaseURL(c.in); got != c.want {
			t.Errorf("NormalizeBaseURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestInferReasoning(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"deepseek-reasoner", true},
		{"deepseek-chat", false},
		{"deepseek-v3.1", true},
		// 实测这两个默认就会吐 reasoning_content，别被名字骗过去
		{"deepseek-flash", true},
		{"deepseek-v4-pro", true},
		{"qwq-32b", true},
		{"glm-4.5", true},
		{"glm-4-plus", false},
		{"gpt-5", true},
		{"gpt-4o", false},
		{"gpt-4o-mini", false},
		{"gpt-4.1", false},
		{"o1-preview", true},
		{"o3-mini", true},
		{"o4-mini", true},
		{"qwen3-max", true},
		{"qwen2.5-72b-instruct", false},
		{"claude-3-5-sonnet-20241022", false},
		{"claude-sonnet-4-20250514", true},
		{"llama-3.3-70b-versatile", false},
		{"kimi-k2-thinking", true},
		{"moonshot-v1-8k", false},
	}
	for _, c := range cases {
		if got := InferReasoning(c.model); got != c.want {
			t.Errorf("InferReasoning(%q) = %v，期望 %v", c.model, got, c.want)
		}
	}
}

// TestMimoCapabilities 验证 MiMo v2 系列的能力判定：
// 文本型号（v2.5/v2.6 全系）reasoning=true 且带通用三档（官方没写档位，
// 按「猜支持的代价很轻」原则放开，上游不认会自动去掉重试）；
// asr/tts 音频型号不标思考。
func TestMimoCapabilities(t *testing.T) {
	for _, id := range []string{"mimo-v2.5", "mimo-v2.5-pro", "mimo-v2.6-flash", "mimo-v2.6-pro"} {
		r, e := capabilitiesOf(id, nil, nil, nil)
		if !r || len(e) != len(DefaultEfforts()) {
			t.Errorf("%s 应为 reasoning=true 且带默认三档，实际 %v %v", id, r, e)
		}
	}
	for _, id := range []string{"mimo-v2.5-asr", "mimo-v2.5-tts", "mimo-v2.5-tts-voiceclone"} {
		if r, _ := capabilitiesOf(id, nil, nil, nil); r {
			t.Errorf("%s 是音频型号，不该标成思考模型", id)
		}
	}

	// 上下文长度（官方模型发布页）：v2.5 系 1M、v2-flash/omni 256K、v2.6 未官宣不显示
	ctxCases := []struct {
		model string
		want  int
	}{
		{"mimo-v2.5", 1048576},
		{"mimo-v2.5-pro", 1048576},
		{"mimo-v2-pro", 1048576},
		{"mimo-v2-flash", 262144},
		{"mimo-v2-omni", 262144},
		{"mimo-v2.6-pro", 0},
		{"mimo-v2.5-tts", 0},
	}
	for _, c := range ctxCases {
		if got := InferContextLen(c.model); got != c.want {
			t.Errorf("InferContextLen(%q) = %d，期望 %d", c.model, got, c.want)
		}
	}
}

func TestNormalizeEfforts(t *testing.T) {
	got := normalizeEfforts([]string{"high", "low", "HIGH", "bogus", "medium", "minimal"})
	want := []string{"minimal", "low", "medium", "high"}
	if len(got) != len(want) {
		t.Fatalf("normalizeEfforts 结果 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalizeEfforts 结果 %v，期望 %v（顺序应按强度升序）", got, want)
		}
	}
	if len(normalizeEfforts([]string{"bogus"})) != 0 {
		t.Fatal("无法识别的档位应当被丢弃")
	}
}
