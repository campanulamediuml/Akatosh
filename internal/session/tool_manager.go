package session

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"aiassistant/internal/imagegen"
	"aiassistant/internal/llm"
	"aiassistant/internal/sandbox"
	"aiassistant/internal/search"
)

// ToolRegistration 把一个工具的协议声明、权限、展示和执行放在同一处。
// Call 总是收到当前 Session，不能用脱离用户上下文的全局函数替代。
type ToolRegistration struct {
	Name       string
	Definition func() llm.Tool
	Enabled    func(*Session) bool
	Label      func(llm.ToolCall) string
	Call       func(*Session, *generation, *toolExecEntry)
}

// ToolManager 按稳定的 function name 分发；call.ID 只用于 tool 回执关联，不参与注册。
type ToolManager struct {
	order []string
	tools map[string]ToolRegistration
}

func newToolManager() *ToolManager {
	m := &ToolManager{tools: map[string]ToolRegistration{}}
	m.Register(ToolRegistration{
		Name: search.ToolName, Definition: search.ToolDef,
		Enabled: func(s *Session) bool { return s.conv != nil && s.conv.WebSearch && s.mgr.search != nil },
		Label:   func(call llm.ToolCall) string { return "正在搜索：" + search.ParseQuery(call.Function.Arguments) },
		Call:    executeSearchTool,
	})
	m.Register(ToolRegistration{
		Name: sandbox.ToolName, Definition: sandbox.ToolDef,
		Enabled: func(s *Session) bool { return s.mgr.sandbox != nil },
		Label:   func(llm.ToolCall) string { return "正在执行 Python…" },
		Call:    executePythonTool,
	})
	m.Register(ToolRegistration{
		Name: imagegen.ToolName, Definition: imagegen.ToolDef,
		Enabled: func(s *Session) bool { return s.mgr.imagegen != nil },
		Label: func(call llm.ToolCall) string {
			prompt, _ := imagegen.ParsePrompt(call.Function.Arguments)
			return "正在生成图片：" + imagegen.TruncatePrompt(prompt, 40)
		},
		Call: executeImageTool,
	})
	m.Register(ToolRegistration{
		Name: readAttachmentToolName, Definition: readAttachmentToolDef,
		Enabled: func(s *Session) bool { return s.mgr.media != nil },
		Label: func(call llm.ToolCall) string {
			return fmt.Sprintf("正在回看图片 #%d…", parseAttachmentID(call.Function.Arguments))
		},
		Call: executeReadAttachmentTool,
	})
	m.Register(ToolRegistration{
		Name: readFileToolName, Definition: readFileToolDef,
		Enabled: func(s *Session) bool { return s.mgr.sandbox != nil && s.mgr.media != nil },
		Label: func(call llm.ToolCall) string {
			return fmt.Sprintf("正在读取文件 #%d…", parseAttachmentID(call.Function.Arguments))
		},
		Call: executeReadFileTool,
	})
	m.Register(ToolRegistration{
		Name: memoryReadSessionToolName, Definition: memoryReadSessionToolDef,
		Enabled: func(s *Session) bool {
			return s.mgr.memory != nil && s.mgr.memory.Enabled(s.ownerID(), s.convID)
		},
		Label: func(llm.ToolCall) string { return "正在读取会话记忆…" },
		Call:  executeMemoryReadTool,
	})
	return m
}

func (m *ToolManager) Register(tool ToolRegistration) {
	if tool.Name == "" || tool.Definition == nil || tool.Enabled == nil || tool.Call == nil {
		panic("工具注册缺少必要字段")
	}
	if _, exists := m.tools[tool.Name]; exists {
		panic("重复注册工具: " + tool.Name)
	}
	if tool.Label == nil {
		tool.Label = func(llm.ToolCall) string { return "正在调用：" + tool.Name }
	}
	m.order = append(m.order, tool.Name)
	m.tools[tool.Name] = tool
}

func (m *ToolManager) Definitions(s *Session) []llm.Tool {
	out := []llm.Tool{}
	for _, name := range m.order {
		tool := m.tools[name]
		if tool.Enabled(s) {
			out = append(out, tool.Definition())
		}
	}
	return out
}

func (m *ToolManager) Label(call llm.ToolCall) string {
	if tool, ok := m.tools[call.Function.Name]; ok {
		return tool.Label(call)
	}
	return "调用了未知工具：" + call.Function.Name
}

func (m *ToolManager) Execute(s *Session, g *generation, e *toolExecEntry) {
	tool, ok := m.tools[e.call.Function.Name]
	if !ok {
		log.Printf("[gen] 会话 %d 模型调用了未知工具 %q，按失败结果回传", s.convID, e.call.Function.Name)
		e.output = fmt.Sprintf(`{"error": %q}`, "未知工具 "+e.call.Function.Name)
		return
	}
	if !tool.Enabled(s) {
		e.output = fmt.Sprintf(`{"error": %q}`, "当前会话未启用工具 "+e.call.Function.Name)
		return
	}
	tool.Call(s, g, e)
}

func executeSearchTool(s *Session, g *generation, e *toolExecEntry) {
	query := search.ParseQuery(e.call.Function.Arguments)
	log.Printf("[gen] 会话 %d 联网搜索（第 %d 轮）: %q", s.convID, g.toolRounds, query)
	results, err := s.mgr.search.Search(g.ctx, query)
	if err != nil {
		log.Printf("[gen] 会话 %d 搜索 %q 失败: %v", s.convID, query, err)
	}
	e.output = search.FormatToolResult(results, err)
	e.record = search.NewRecord(query, results, err)
}

func executePythonTool(s *Session, g *generation, e *toolExecEntry) {
	code := sandbox.ParseCode(e.call.Function.Arguments)
	log.Printf("[gen] 会话 %d 执行 Python（第 %d 轮，%d 字符）", s.convID, g.toolRounds, len(code))
	r, err := s.mgr.sandbox.Run(g.ctx, code)
	if err != nil {
		log.Printf("[gen] 会话 %d Python 执行失败: %v", s.convID, err)
	}
	e.output = sandbox.FormatToolResult(r, err)
	e.record = sandbox.NewRecord(code, r, err)
}

func executeImageTool(s *Session, g *generation, e *toolExecEntry) {
	prompt, size := imagegen.ParsePrompt(e.call.Function.Arguments)
	log.Printf("[gen] 会话 %d 生成图片（第 %d 轮）: %q", s.convID, g.toolRounds, imagegen.TruncatePrompt(prompt, 60))
	img, mime, err := s.mgr.imagegen.Generate(g.ctx, prompt, size)
	if err != nil {
		log.Printf("[gen] 会话 %d 生成图片失败: %v", s.convID, err)
		e.output = imagegen.FormatToolResult(err, 0)
		e.record = imagegen.Record{Type: "image", Prompt: prompt, Error: err.Error()}
		return
	}
	attID, err := s.attachGeneratedImage(g.msgID, img)
	if err != nil {
		log.Printf("[gen] 会话 %d 图片落盘失败: %v", s.convID, err)
		e.output = imagegen.FormatToolResult(fmt.Errorf("图片保存失败: %v", err), 0)
		e.record = imagegen.Record{Type: "image", Prompt: prompt, Error: err.Error()}
		return
	}
	if att, err := s.mgr.st.GetAttachmentInConv(attID, s.convID); err == nil && att != nil {
		log.Printf("[gen] 会话 %d 图片生成成功  %dx%d %.0fKB", s.convID, att.Width, att.Height, float64(att.Size)/1024)
	} else {
		log.Printf("[gen] 会话 %d 图片生成成功（%.0fKB）", s.convID, float64(len(img))/1024)
	}
	e.output = imagegen.FormatToolResult(nil, attID)
	if supportsImageToolReceipt(g.modelID) {
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img)
		e.outputParts = []llm.ContentPart{
			llm.TextPart(imagegen.FormatToolResultWithImage(attID)),
			llm.ImagePart(dataURL),
		}
		log.Printf("[gen] 会话 %d 生图回执带图（%s，base64 约 %.0f KB）",
			s.convID, g.modelID, float64(len(dataURL))/1024)
	}
	e.record = imagegen.Record{Type: "image", Prompt: prompt}
}

func executeReadAttachmentTool(s *Session, g *generation, e *toolExecEntry) {
	id := parseAttachmentID(e.call.Function.Arguments)
	a, err := s.mgr.st.GetAttachmentInConv(id, s.convID)
	if err != nil || a == nil {
		e.output = `{"error": "附件不存在或不属于这个会话"}`
		return
	}
	dataURL, _, err := s.imageDataURL(*a)
	if err != nil {
		e.output = fmt.Sprintf(`{"error": %q}`, "读取图片失败："+err.Error())
		return
	}
	e.outputParts = []llm.ContentPart{llm.TextPart("图片内容如下："), llm.ImagePart(dataURL)}
}

func executeReadFileTool(s *Session, g *generation, e *toolExecEntry) {
	args, err := parseReadFileArgs(e.call.Function.Arguments)
	if err != nil {
		e.output = fmt.Sprintf(`{"error": %q}`, err.Error())
		return
	}
	f, err := s.mgr.st.GetFileInConv(args.ID, s.convID)
	if err != nil || f == nil {
		e.output = `{"error": "文件不存在或不属于这个会话"}`
		return
	}
	e.record = map[string]any{"type": "file", "name": f.Name, "id": f.ID, "images": args.Images}
	if len(args.Pages) > 0 {
		e.record.(map[string]any)["pages"] = args.Pages
	}
	isPDF := strings.EqualFold(f.Ext, ".pdf")
	if (args.Images || len(args.Pages) > 0) && !isPDF {
		e.output = `{"error": "pages 和 images 参数只支持 PDF 文件"}`
		return
	}
	path := "/files/" + f.Hash + f.Ext
	if args.Images {
		if !supportsImageToolReceipt(g.modelID) {
			e.output = `{"error": "当前模型不支持工具结果带图，无法按图片读取 PDF"}`
			return
		}
		rendered, err := s.mgr.sandbox.RenderPDF(g.ctx, path, args.Pages)
		if err != nil {
			e.output = fmt.Sprintf(`{"error": %q}`, "PDF 页面渲染失败："+err.Error())
			return
		}
		parts := []llm.ContentPart{llm.TextPart(fmt.Sprintf(
			"PDF 文件 %q 共 %d 页。以下是指定页面的图片；图片仅在当前工具轮可用，后续需要时请重新调用 read_file。",
			f.Name, rendered.TotalPages))}
		for _, img := range rendered.Images {
			parts = append(parts,
				llm.TextPart(fmt.Sprintf("\n--- PDF 第 %d 页 ---", img.Page)),
				llm.ImagePart("data:"+img.MIME+";base64,"+img.Data))
		}
		e.outputParts = parts
		return
	}
	r, err := s.mgr.sandbox.Run(g.ctx, fileExtractScript(path, args.Pages))
	var text string
	if err != nil {
		text = "提取失败: " + err.Error()
	} else {
		text = r.Stdout
		if strings.TrimSpace(r.Stderr) != "" {
			text += "\n[提取提示] " + strings.TrimSpace(r.Stderr)
		}
		if strings.TrimSpace(text) == "" {
			text = "（提取完成，但没有文本内容——可能是扫描件或空文件）"
		}
	}
	e.output = fmt.Sprintf(`{"name": %q, "content": %q}`, f.Name, truncateRunes(text, 30000))
}

func executeMemoryReadTool(s *Session, g *generation, e *toolExecEntry) {
	var args struct {
		SessionID int64 `json:"session_id"`
		Offset    int   `json:"offset"`
		Limit     int   `json:"limit"`
	}
	if err := json.Unmarshal([]byte(e.call.Function.Arguments), &args); err != nil || args.SessionID <= 0 {
		e.output = `{"error":"session_id 必须是正整数"}`
		return
	}
	content, total, more, err := s.mgr.memory.ReadArchive(s.ownerID(), args.SessionID, args.Offset, args.Limit)
	if err != nil {
		e.output = fmt.Sprintf(`{"error":%q}`, err.Error())
		return
	}
	next := args.Offset + len([]rune(content))
	e.output = fmt.Sprintf(`{"session_id":%d,"content":%q,"total_chars":%d,"next_offset":%d,"has_more":%t}`,
		args.SessionID, content, total, next, more)
	e.record = map[string]any{"type": "memory", "session_id": args.SessionID}
}
