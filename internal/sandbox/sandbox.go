// Package sandbox 是 Python 代码执行工具的执行端：调 pybox 沙箱的 HTTP API。
// 隔离由 pybox 容器承担（断网/只读/限额），本包只做请求与结果修剪。
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"aiassistant/internal/llm"
)

const (
	// ToolName 是工具名，session 按它识别「这是代码执行调用」。
	ToolName = "run_python"
	// 喂回给模型 / 落库展示的长度上限（模型写的 code 本身落库存全量，见 NewRecord）
	maxOutputRunes = 4000
)

// Result 是一次代码执行的结果。
type Result struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	DurationMs int    `json:"duration_ms"`
}

// Record 是「这轮跑了什么代码」的持久摘要，落库到消息的 search_log。
// Type 字段用于和搜索记录区分（前端按它渲染成不同形态）。
type Record struct {
	Type     string `json:"type"` // 固定 "python"
	Code     string `json:"code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

// Client 调 pybox。
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		HTTP: &http.Client{
			// pybox 单次执行上限是 20s，这里留一点网络和处理富余。
			Timeout: 25 * time.Second,
		},
	}
}

// Run 执行一段 Python 代码，返回修剪后的结果。
func (c *Client) Run(ctx context.Context, code string) (Result, error) {
	buf, err := json.Marshal(map[string]any{"code": code})
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/run", bytes.NewReader(buf))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Result{}, fmt.Errorf("沙箱返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out Result
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{}, fmt.Errorf("解析沙箱响应失败: %w", err)
	}
	out.Stdout = truncateRunes(out.Stdout, maxOutputRunes)
	out.Stderr = truncateRunes(out.Stderr, maxOutputRunes)
	return out, nil
}

const maxRenderResponseBytes = 50 << 20

type PDFPageImage struct {
	Page int    `json:"page"`
	MIME string `json:"mime"`
	Data string `json:"data"`
}

type PDFRenderResult struct {
	TotalPages int            `json:"total_pages"`
	Images     []PDFPageImage `json:"images"`
}

func (c *Client) RenderPDF(ctx context.Context, path string, pages []int) (PDFRenderResult, error) {
	buf, err := json.Marshal(map[string]any{"path": path, "pages": pages})
	if err != nil {
		return PDFRenderResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/render-pdf", bytes.NewReader(buf))
	if err != nil {
		return PDFRenderResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return PDFRenderResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return PDFRenderResult{}, fmt.Errorf("沙箱返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRenderResponseBytes+1))
	if err != nil {
		return PDFRenderResult{}, err
	}
	if len(body) > maxRenderResponseBytes {
		return PDFRenderResult{}, fmt.Errorf("PDF 渲染结果超过 %dMB", maxRenderResponseBytes>>20)
	}
	var out PDFRenderResult
	if err := json.Unmarshal(body, &out); err != nil {
		return PDFRenderResult{}, fmt.Errorf("解析 PDF 渲染响应失败: %w", err)
	}
	if out.TotalPages < 1 || len(out.Images) == 0 || len(out.Images) > 5 {
		return PDFRenderResult{}, fmt.Errorf("PDF 渲染响应里的页数无效")
	}
	for _, img := range out.Images {
		if img.Page < 1 || img.Page > out.TotalPages || img.MIME != "image/jpeg" || img.Data == "" {
			return PDFRenderResult{}, fmt.Errorf("PDF 渲染响应里有无效图片")
		}
	}
	return out, nil
}

// NewRecord 把一次执行整理成可落库的摘要。runErr 是「没能执行」的错误
// （沙箱不可达等），代码本身的非零退出不算错误——那是正常结果。
// Code 存全量：这是给用户回看「模型到底写了什么」的展示数据，
// 截断只发生在喂给模型的历史摘要里（formatToolLogSummary）。
func NewRecord(code string, r Result, runErr error) Record {
	rec := Record{
		Type:     "python",
		Code:     code,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		ExitCode: r.ExitCode,
	}
	if runErr != nil {
		rec.Error = runErr.Error()
	}
	return rec
}

// FormatToolResult 把执行结果拼成 role=tool 消息的内容（给模型看的）。
// 执行失败也如实告诉模型，让它自己换写法重试或坦白。
func FormatToolResult(r Result, runErr error) string {
	if runErr != nil {
		return fmt.Sprintf(`{"error": %q}`, "代码执行失败："+runErr.Error())
	}
	buf, err := json.Marshal(r)
	if err != nil {
		return `{"error": "结果序列化失败"}`
	}
	return string(buf)
}

// ParseCode 从 tool call 的 arguments（JSON 字符串）里挖出 code。
// 模型偶尔给畸形 JSON，退化时把整串当代码 —— 跑挂了也比这轮断掉强。
func ParseCode(arguments string) string {
	var v struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(arguments), &v); err == nil && strings.TrimSpace(v.Code) != "" {
		return v.Code
	}
	return strings.TrimSpace(arguments)
}

// ToolDef 是 run_python 的工具声明（OpenAI tools 格式）。
// description 直接决定模型什么时候愿意调它、写什么风格的代码。
func ToolDef() llm.Tool {
	var t llm.Tool
	t.Type = "function"
	t.Function.Name = ToolName
	t.Function.Description = "在沙箱里执行 Python 代码并返回 stdout/stderr/退出码。环境：无网络，预装 numpy/matplotlib/sympy/pypdf/pymupdf/python-docx/openpyxl/chardet；用户上传的文件在 /files/（只读），/tmp 可写但易失。每次调用是独立进程、变量不保留，请写自包含代码。适合：精确计算、数据处理、读取上传的文档、验证猜想。单次限时 20 秒。结果用 print 输出。"
	t.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code": map[string]any{
				"type":        "string",
				"description": "要执行的 Python 代码，结果用 print 打到 stdout",
			},
		},
		"required": []string{"code"},
	}
	return t
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
