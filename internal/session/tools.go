package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"aiassistant/internal/imagegen"
	"aiassistant/internal/llm"
	"aiassistant/internal/store"
)

// read_attachment 工具：让模型回看本会话里的图片（用户贴的、或它自己生成的）。
//
// 协议依据：实测 DeepSeek 接受 tool 消息的多段 content（text + image_url），
// 模型确实能「看到」这张图。这补上了「模型对自己生成的图是盲的」这个缺口
// （assistant 消息不能带图，见 buildHistory 的注释）。
const readAttachmentToolName = "read_attachment"

// readFileToolName 是「读文档」的工具名：模型用它读取用户上传的文件，
// 后端把提取工作丢给 pybox 沙箱（pypdf/python-docx/openpyxl/chardet），Go 不碰格式解析。
const readFileToolName = "read_file"

func readAttachmentToolDef() llm.Tool {
	var t llm.Tool
	t.Type = "function"
	t.Function.Name = readAttachmentToolName
	t.Function.Description = "回看本会话里的一张图片（用户贴的，或你自己用 generate_image 生成的）。需要检查生成结果、回答关于某张图的细节问题、或基于某张图继续创作时调用。参数 id 来自 generate_image 工具回执里的 attachment_id。"
	t.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "integer",
				"description": "附件 id（generate_image 回执里的 attachment_id）",
			},
		},
		"required": []string{"id"},
	}
	return t
}

// parseAttachmentID 从 tool call 的 arguments 里挖附件 id。
func parseAttachmentID(arguments string) int64 {
	var v struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(arguments), &v); err == nil && v.ID > 0 {
		return v.ID
	}
	// 退化：模型有时直接给数字字符串
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(arguments), "%d", &n); err == nil {
		return n
	}
	return 0
}

// formatToolLogSummary 把落库的工具调用摘要（search_log JSON）转成历史里的文本标注。
//
// 为什么需要它：tool 交互只活在当轮内存（assistant 的 tool_calls 和 tool 结果不落库），
// 跨轮后模型看不到自己上轮搜了什么、跑了什么——这份摘要让模型在追问时记得，
// 而不是重新搜一遍。只放摘要（关键词/标题/代码截断），不放搜索结果全文（太长）。
func formatToolLogSummary(logJSON string) string {
	if strings.TrimSpace(logJSON) == "" {
		return ""
	}
	var recs []map[string]any
	if err := json.Unmarshal([]byte(logJSON), &recs); err != nil || len(recs) == 0 {
		return ""
	}
	var bits []string
	for _, r := range recs {
		switch r["type"] {
		case "search", "": // 历史数据没有 type 字段，缺省按搜索处理
			q, _ := r["query"].(string)
			var titles []string
			if rs, ok := r["results"].([]any); ok {
				for _, it := range rs {
					if m, ok := it.(map[string]any); ok {
						if t, ok := m["title"].(string); ok && len(titles) < 4 {
							titles = append(titles, truncateRunes(t, 40))
						}
					}
				}
			}
			if q != "" {
				bit := fmt.Sprintf("搜索了「%s」", q)
				if len(titles) > 0 {
					bit += "，来源：" + strings.Join(titles, "；")
				}
				bits = append(bits, bit)
			}
		case "python":
			code, _ := r["code"].(string)
			stdout, _ := r["stdout"].(string)
			bit := fmt.Sprintf("执行了 Python：%s", truncateRunes(strings.TrimSpace(code), 200))
			if s := strings.TrimSpace(stdout); s != "" {
				bit += fmt.Sprintf("，输出：%s", truncateRunes(s, 150))
			}
			bits = append(bits, bit)
		case "file":
			name, _ := r["name"].(string)
			if name != "" {
				action := "读取了"
				if images, _ := r["images"].(bool); images {
					action = "按图片读取了"
				}
				bit := fmt.Sprintf("%s文件「%s」", action, name)
				if pages, ok := r["pages"].([]any); ok && len(pages) > 0 {
					numbers := make([]string, 0, len(pages))
					for _, page := range pages {
						if n, ok := page.(float64); ok {
							numbers = append(numbers, fmt.Sprintf("%.0f", n))
						}
					}
					bit += "（第 " + strings.Join(numbers, "、") + " 页）"
				}
				bits = append(bits, bit)
			}
			// image 条目不进文本摘要：它们由 imageToolRounds 重放成结构化工具轮
		}
	}
	if len(bits) == 0 {
		return ""
	}
	return "[上一轮的工具调用：" + strings.Join(bits, "；") + "]"
}

// imageToolRounds 把落库的图像生成记录（search_log 里的 image 条目）重放成
// 结构化的工具轮：assistant(tool_calls) + tool 回执，插在对应正文消息之前。
//
// 为什么不用文本标注：写进正文的「（系统注：这条回复附带了图片…）」之类的
// 标注，模型会当成格式学着写——没调用工具也自己编一条出来假装画过图
// （实测抓到，连「[上一轮的工具调用：…]」都编）。tool_calls/tool 是 API 层
// 结构，模型的正文模仿不出来：它学到的模式变成「图 = 真的发起了
// generate_image 调用并收到回执」，而不是「图 = 写一句特定格式的旁白」。
//
// image 记录本身不落 attachment id，附件按顺序从消息的附件列表里配
// （助手消息的附件全部来自 generate_image）。reasoning 挂在第一条重放的
// assistant 消息上：思考模式下带 tool_calls 的消息必须回传 reasoning_content
// （上游 400 硬规则），落库的 CoT 是各段拼接的全文，近似放在这里。
//
// imgFor 非 nil 时把生成图（原图）随回执一起回传；返回 "" 表示这张不带
// （读失败等），退回纯文本回执。调用方策略是全量原图——无状态 API 每轮
// 全量重放，长度交给压缩阈值兜底。
func imageToolRounds(logJSON string, atts []store.Attachment, reasoning string, imgFor func(attID int64) string) []llm.ChatMessage {
	var recs []map[string]any
	_ = json.Unmarshal([]byte(logJSON), &recs)

	var out []llm.ChatMessage
	attIdx := 0
	seq := 0
	emit := func(prompt, errStr string, attID int64, hasAtt bool) {
		seq++
		args, _ := json.Marshal(map[string]string{"prompt": prompt})
		call := llm.ToolCall{ID: fmt.Sprintf("call_replay_%d", seq), Type: "function"}
		call.Function.Name = imagegen.ToolName
		call.Function.Arguments = string(args)
		msg := llm.ChatMessage{Role: store.RoleAssistant, Content: "", ToolCalls: []llm.ToolCall{call}}
		if reasoning != "" {
			msg.Reasoning = reasoning
			reasoning = "" // 只挂第一条
		}
		out = append(out, msg)

		var result any
		switch {
		case errStr != "":
			result = imagegen.FormatToolResult(errors.New(errStr), 0)
		case !hasAtt:
			// 老数据：记录丢了/附件没了，id 不可考，但「画过这张图」这个事实留住
			result = `{"ok": true, "note": "图片已生成并展示给用户（历史记录，附件 id 不可考，无法 read_attachment 回看）"}`
		default:
			dataURL := ""
			if imgFor != nil {
				dataURL = imgFor(attID)
			}
			if dataURL != "" {
				result = []llm.ContentPart{
					llm.TextPart(imagegen.FormatToolResultWithImage(attID)),
					llm.ImagePart(dataURL),
				}
			} else {
				result = imagegen.FormatToolResult(nil, attID)
			}
		}
		out = append(out, llm.ChatMessage{Role: "tool", Content: result, ToolCallID: call.ID})
	}

	for _, r := range recs {
		if r["type"] != "image" {
			continue
		}
		prompt, _ := r["prompt"].(string)
		// 旧记录是 500 字截断落库的、末尾带「…」。原样回放的话模型会把这个
		// 省略号当成 prompt 的一部分抄进新调用（实测抓到：生图上游收到
		// 带「…」的截断 prompt）。剥掉它——丢的文本找不回，但至少别传染。
		prompt = strings.TrimSuffix(prompt, "…")
		errStr, _ := r["error"].(string)
		if errStr != "" {
			emit(prompt, errStr, 0, false)
			continue
		}
		if attIdx < len(atts) {
			emit(prompt, "", atts[attIdx].ID, true)
			attIdx++
		} else {
			emit(prompt, "", 0, false)
		}
	}
	// 附件比 image 记录多（老数据只有附件没有记录）：也补成回放轮，prompt 不可考
	for ; attIdx < len(atts); attIdx++ {
		emit("（历史图片，prompt 未记录）", "", atts[attIdx].ID, true)
	}
	return out
}

// recordToolRounds 把本轮的工具交互整理成可落库的 JSON（没调工具返回空串）。
// 三条纪律：
//   - tool 消息只留文本段：图片段（生图回执/read_attachment 回图）是几百 KB 的
//     base64，不落库——回放时按 attachment id 从附件库现取；
//   - 「半截轮」（assistant 发了 tool_calls 但结果没回来——比如被停止）整轮丢掉：
//     回放一个 dangling tool_call 上游会直接报错；
//   - 分段 usage 一并落库：前端工具轮卡片按段展示 token（累计值是成本口径，
//     不分段会让人以为「瞬间海量 token」）。
func recordToolRounds(g *generation) string {
	if g.historyBase >= len(g.history) {
		return ""
	}
	rounds := g.history[g.historyBase:]

	// 结果都齐了的 call id（结果在 onToolResult 里整批追加，要么全有要么全没有）
	done := map[string]bool{}
	for _, m := range rounds {
		if m.Role == "tool" {
			done[m.ToolCallID] = true
		}
	}
	kept := map[string]bool{}
	out := make([]llm.ChatMessage, 0, len(rounds))
	for _, m := range rounds {
		if m.Role == store.RoleSystem {
			continue // 「工具用完了」提示只在当轮有效，不带入跨轮回放
		}
		if len(m.ToolCalls) > 0 {
			complete := true
			for _, c := range m.ToolCalls {
				if !done[c.ID] {
					complete = false
					break
				}
			}
			if !complete {
				continue
			}
			for _, c := range m.ToolCalls {
				kept[c.ID] = true
			}
		}
		if m.Role == "tool" {
			if !kept[m.ToolCallID] {
				continue // 对应调用已被丢（半截轮），结果不能单独留
			}
			m.Content = textOnlyContent(m.Content)
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return ""
	}
	buf, err := json.Marshal(store.ToolRoundsPayload{
		Rounds:         out,
		FinalReasoning: g.reasoning.String(),
		SegUsages:      g.segUsages,
	})
	if err != nil {
		return ""
	}
	return string(buf)
}

// textOnlyContent 把多段 content 里的图片段剥掉、文本段拼回字符串。
func textOnlyContent(c any) any {
	parts, ok := c.([]llm.ContentPart)
	if !ok {
		return c
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// replayRecordedRounds 把消息落库的完整工具轮展开进历史（返回 false = 没有
// 记录，调用方走老路：生图重建 + 文本摘要）。图片段在录制时被剥掉了，
// 这里按 attachment id 现取回补（imgFor 为 nil 的模型保持纯文本回执）。
// 正文消息的 reasoning 换成「最后一段」的原文；编辑过 CoT 就保留编辑后的
// 完整版（接受和中间段的重复——编辑后的内容本来映射不回分段）。
func replayRecordedRounds(out *[]llm.ChatMessage, m *store.Message, imgFor func(attID int64) string) bool {
	p := store.ParseToolRounds(m.ToolRounds)
	if p == nil || len(p.Rounds) == 0 {
		return false
	}
	calls := map[string]llm.ToolCall{}
	for _, rm := range p.Rounds {
		for _, c := range rm.ToolCalls {
			calls[c.ID] = c
		}
	}
	for _, rm := range p.Rounds {
		if rm.Role == "tool" && imgFor != nil {
			if id := replayImageAttID(rm, calls); id > 0 {
				if dataURL := imgFor(id); dataURL != "" {
					text, _ := rm.Content.(string)
					rm.Content = []llm.ContentPart{llm.TextPart(text), llm.ImagePart(dataURL)}
				}
			}
		}
		*out = append(*out, rm)
	}
	if !m.Edited && p.FinalReasoning != "" {
		m.Reasoning = p.FinalReasoning
	}
	return true
}

// replayImageAttID 判断这条 tool 结果该不该在回放时补图（生图回执/read_attachment
// 回图），该补则返回附件 id；不需要或拿不到返回 0。
func replayImageAttID(toolMsg llm.ChatMessage, calls map[string]llm.ToolCall) int64 {
	call, ok := calls[toolMsg.ToolCallID]
	if !ok {
		return 0
	}
	switch call.Function.Name {
	case imagegen.ToolName:
		// 成功回执的 JSON 里有 attachment_id（失败回执没有，自然为 0）
		s, _ := toolMsg.Content.(string)
		var v struct {
			AttachmentID int64 `json:"attachment_id"`
		}
		if json.Unmarshal([]byte(s), &v) == nil {
			return v.AttachmentID
		}
	case readAttachmentToolName:
		return parseAttachmentID(call.Function.Arguments)
	}
	return 0
}

// supportsImageToolReceipt 判断这个模型的上游是否接受 tool 消息里带 image_url。// 实测（2026-10，本机 probe 脚本打真实上游）：
//   - DeepSeek（含快快中转）✓ 能根据回执里的图回答问题
//   - Kimi/Moonshot（kimi-k3 直连）✓
//   - OpenAI 系（快快 gpt-5.5）✗ 不报错但静默丢图（对照组 user 消息带图能认出），
//     静默降级比报错更坑——模型会对着没有的图瞎答，所以宁可回纯文本
//
// 按模型 ID 判定而不是供应商：同一个中转（kkidc）同时代理 deepseek 和 gpt。
func supportsImageToolReceipt(modelID string) bool {
	m := strings.ToLower(modelID)
	return strings.Contains(m, "deepseek") ||
		strings.Contains(m, "kimi") || strings.Contains(m, "moonshot") ||
		strings.HasPrefix(m, "k2") || strings.HasPrefix(m, "k3")
}

const maxReadFilePages = 5

type readFileArgs struct {
	ID     int64 `json:"id"`
	Pages  []int `json:"pages"`
	Images bool  `json:"images"`
}

func parseReadFileArgs(arguments string) (readFileArgs, error) {
	var args readFileArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		if id := parseAttachmentID(arguments); id > 0 {
			return readFileArgs{ID: id}, nil
		}
		return readFileArgs{}, fmt.Errorf("参数不是合法 JSON")
	}
	if args.ID <= 0 {
		return readFileArgs{}, fmt.Errorf("缺少有效的文件 id")
	}
	if len(args.Pages) > maxReadFilePages {
		return readFileArgs{}, fmt.Errorf("每次最多读取 %d 页", maxReadFilePages)
	}
	seen := map[int]bool{}
	for _, page := range args.Pages {
		if page < 1 {
			return readFileArgs{}, fmt.Errorf("页码必须从 1 开始")
		}
		if seen[page] {
			return readFileArgs{}, fmt.Errorf("页码不能重复")
		}
		seen[page] = true
	}
	if args.Images && len(args.Pages) == 0 {
		return readFileArgs{}, fmt.Errorf("按图片读取 PDF 时必须指定 pages")
	}
	return args, nil
}

// readFileToolDef 是 read_file 的工具声明：模型用它读用户上传的文档，
// 提取和 PDF 页面渲染由 pybox 沙箱完成（Go 不碰格式解析）。
func readFileToolDef() llm.Tool {
	var t llm.Tool
	t.Type = "function"
	t.Function.Name = readFileToolName
	t.Function.Description = "读取本会话里用户上传的文档内容。支持 txt/md/csv/json/代码（py/js/go 等）/PDF/DOCX/XLSX。参数 id 来自消息里的 [文件#N] 标注。PDF 默认提取文本并返回总页数；扫描件或需要看版面时，设 images=true 并用 pages 指定要看的页（从 1 开始，每次最多 5 页），页面会以图片返回。可分批读取后续页。"
	t.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "integer",
				"description": "文件 id（消息里的 [文件#N] 标注）",
			},
			"pages": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "integer", "minimum": 1},
				"maxItems":    maxReadFilePages,
				"description": "可选 PDF 页码，从 1 开始；文本模式按页提取，图片模式必填",
			},
			"images": map[string]any{
				"type":        "boolean",
				"description": "PDF 专用：true 时将 pages 指定的页面渲染成图片，适用于扫描件、表格和复杂版面",
			},
		},
		"required": []string{"id"},
	}
	return t
}

// fileExtractScript 生成发给 pybox 的提取脚本：按扩展名分发，
// pypdf / python-docx / openpyxl / chardet 已在沙箱预装（Dockerfile.pybox）。
func fileExtractScript(path string, pages []int) string {
	pagesJSON, _ := json.Marshal(pages)
	if pages == nil {
		pagesJSON = []byte("None")
	}
	return `import sys
path = ` + pyQuote(path) + `
selected_pages = ` + string(pagesJSON) + `
ext = path.rsplit(".", 1)[-1].lower() if "." in path else ""
TEXT_EXT = {"txt","md","markdown","log","csv","json","jsonl","xml","yaml","yml","toml","ini","sql","html","css",
            "py","js","ts","go","java","c","h","cpp","rs","sh"}
try:
    if ext in TEXT_EXT:
        import chardet
        raw = open(path, "rb").read()
        print(raw.decode(chardet.detect(raw).get("encoding") or "utf-8", errors="replace"))
    elif ext == "pdf":
        from pypdf import PdfReader
        r = PdfReader(path)
        total = len(r.pages)
        print(f"PDF 共 {total} 页。扫描件或需要看版面时，用 images=true 和 pages 指定页码。")
        if selected_pages is not None and any(p > total for p in selected_pages):
            raise ValueError(f"页码超出范围（共 {total} 页）")
        for i, p in enumerate(r.pages, 1):
            if selected_pages is not None and i not in selected_pages:
                continue
            print(f"--- 第 {i} 页 ---")
            text = p.extract_text() or ""
            if text.strip():
                print(text)
            else:
                print("[此页未提取到文本，可能是扫描件；请用 images=true 重新读取此页]")
    elif ext == "docx":
        import docx
        d = docx.Document(path)
        print("\n".join(p.text for p in d.paragraphs))
    elif ext == "xlsx":
        import openpyxl
        wb = openpyxl.load_workbook(path, read_only=True)
        for ws in wb.worksheets:
            print(f"### 工作表: {ws.title}")
            for row in ws.iter_rows(values_only=True):
                print("\t".join("" if c is None else str(c) for c in row))
    else:
        print(f"暂不支持的格式 .{ext}", file=sys.stderr)
except Exception as e:
    print(f"提取失败: {e}", file=sys.stderr)
`
}

// pyQuote 把 Go 字符串转成 Python 字符串字面量（防路径里的引号/反斜杠）。
func pyQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
