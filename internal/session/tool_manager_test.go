package session

import (
	"strings"
	"testing"

	"aiassistant/internal/llm"
)

func namedToolCall(name string) llm.ToolCall {
	var call llm.ToolCall
	call.Function.Name = name
	return call
}

func TestToolManagerPreservesOrderAndLabels(t *testing.T) {
	manager := &ToolManager{tools: map[string]ToolRegistration{}}
	register := func(name string) {
		manager.Register(ToolRegistration{
			Name: name,
			Definition: func() llm.Tool {
				var def llm.Tool
				def.Function.Name = name
				return def
			},
			Enabled: func(*Session) bool { return true },
			Call:    func(*Session, *generation, *toolExecEntry) {},
		})
	}
	register("second")
	register("first")
	defs := manager.Definitions(&Session{})
	if len(defs) != 2 || defs[0].Function.Name != "second" || defs[1].Function.Name != "first" {
		t.Fatalf("工具声明顺序不稳定: %+v", defs)
	}
	if got := manager.Label(namedToolCall("second")); got != "正在调用：second" {
		t.Fatalf("默认标签异常: %q", got)
	}
	if got := manager.Label(namedToolCall("missing")); got != "调用了未知工具：missing" {
		t.Fatalf("未知工具标签异常: %q", got)
	}
}

func TestToolManagerRejectsUnknownAndDisabledCalls(t *testing.T) {
	up := newUpstream(t, 0)
	defer up.Close()
	mgr, st := newTestManager(t, up.URL)
	conv, _ := st.CreateConversation(1, "tools", 0, "m-reason", "high")
	s := newSession(conv.ID, mgr, conv)

	unknown := &toolExecEntry{}
	unknown.call.Function.Name = "not_registered"
	s.execToolCall(&generation{}, unknown)
	if !strings.Contains(unknown.output, "未知工具") {
		t.Fatalf("未知工具没有错误回执: %q", unknown.output)
	}

	disabled := &toolExecEntry{}
	disabled.call.Function.Name = memoryReadSessionToolName
	s.execToolCall(&generation{}, disabled)
	if !strings.Contains(disabled.output, "未启用工具") {
		t.Fatalf("禁用工具没有被执行层拦截: %q", disabled.output)
	}
}
