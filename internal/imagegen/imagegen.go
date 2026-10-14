// Package imagegen 是图像生成工具的执行端：调 OpenAI 兼容的
// /images/generations（硅基流动等），把返回的临时 URL 里的图下载回来。
// 落盘入库（缩略图/发送版/原图）复用 media + store 的附件链路，不在本包做。
package imagegen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // image.Decode 需要注册 PNG 解码器
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"aiassistant/internal/llm"
)

const (
	// ToolName 是工具名，session 按它识别「这是图像生成调用」。
	ToolName = "generate_image"
	// 图片下载上限（与上传链路的单文件上限一致）
	maxImageBytes = 20 << 20
)

// Record 是「这轮生成了什么图」的持久摘要，落库到消息的 search_log。
// 图片本体是消息附件，这里只记 prompt 供折叠块展示。
type Record struct {
	Type   string `json:"type"` // 固定 "image"
	Prompt string `json:"prompt"`
	Error  string `json:"error,omitempty"`
}

// Client 调图像生成上游。
type Client struct {
	BaseURL string
	Key     string
	Model   string
	HTTP    *http.Client
}

func New(baseURL, key, model string) *Client {
	return &Client{
		// 归一化复用 llm 的现成逻辑（结尾补 /v1、剥掉粘多的端点路径）
		BaseURL: llm.NormalizeBaseURL(baseURL),
		Key:     strings.TrimSpace(key),
		Model:   model,
		HTTP: &http.Client{
			// 生图本身是慢活（几秒到几十秒），下载另算
			Timeout: 120 * time.Second,
		},
	}
}

// generationResponse 同时认两种返回：OpenAI 标准的 data[].url，
// 以及硅基流动扩展的 images[].url。
type generationResponse struct {
	Data []struct {
		URL     string `json:"url"`
		B64JSON string `json:"b64_json"`
	} `json:"data"`
	Images []struct {
		URL string `json:"url"`
	} `json:"images"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Generate 生成一张图并把字节下载回来（上游给的是限时 URL，不立刻抓就失效）。
// 返回图片字节与 MIME。size 形如 "1024x1024"，空串用上游默认。
func (c *Client) Generate(ctx context.Context, prompt, size string) ([]byte, string, error) {
	body := map[string]any{"model": c.Model, "prompt": prompt, "n": 1}
	if size != "" {
		body["image_size"] = size
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/images/generations", strings.NewReader(string(buf)))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", fmt.Errorf("生图上游返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out generationResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", fmt.Errorf("解析生图响应失败: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return nil, "", fmt.Errorf("%s", out.Error.Message)
	}
	imgURL := ""
	if len(out.Data) > 0 {
		imgURL = out.Data[0].URL
	}
	if imgURL == "" && len(out.Images) > 0 {
		imgURL = out.Images[0].URL
	}
	if imgURL == "" {
		return nil, "", fmt.Errorf("上游没返回图片地址")
	}
	img, mime, err := c.download(ctx, imgURL)
	if err != nil {
		return nil, "", err
	}
	// 统一转 JPEG：上游给的是 PNG（1920 壁纸动辄 3MB），JPEG q90 视觉无损且
	// 只有几百 KB——用户端隧道带宽 ~270KB/s，点开大图的等待从 12s 级降到 2s 级。
	if j, err := toJPEG(img); err == nil {
		return j, "image/jpeg", nil
	}
	return img, mime, nil
}

// toJPEG 把 PNG/JPEG 图片重编码成 JPEG q90。
// 只对 AI 生成的图用：用户上传的 PNG 截图不走这条路（文字会糊）。
func toJPEG(b []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// download 把限时 URL 里的图抓回来，校验类型与大小（URL 来自上游响应，防一手）。
func (c *Client) download(ctx context.Context, u string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("下载图片失败: %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > maxImageBytes {
		return nil, "", fmt.Errorf("图片超过 %dMB 上限", maxImageBytes>>20)
	}
	mime := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(mime, "image/") {
		// 有些图床（硅基流动的临时 S3）不给图片 MIME：按内容魔数嗅探
		mime = http.DetectContentType(b)
		if !strings.HasPrefix(mime, "image/") {
			return nil, "", fmt.Errorf("下载到的不是图片（内容嗅探: %s）", mime)
		}
	}
	return b, mime, nil
}

// FormatToolResult 给模型的回执：图片已经直接展示给用户了，模型不用描述图片内容。
// attachmentID 让模型可以随后用 read_attachment 回看这张图（自检/迭代）。
func FormatToolResult(genErr error, attachmentID int64) string {
	if genErr != nil {
		return fmt.Sprintf(`{"error": %q}`, "生成图片失败："+genErr.Error())
	}
	return fmt.Sprintf(
		`{"ok": true, "attachment_id": %d, "note": "图片已生成并直接展示给用户，无需描述图片内容；需要检查或基于它再创作时用 read_attachment 回看"}`,
		attachmentID)
}

// FormatToolResultWithImage 是「回执带图」版本的文本部分：图片本体以 image_url
// 段随同一 tool 消息回传（仅 supportsImageToolReceipt 判定支持的模型）。
func FormatToolResultWithImage(attachmentID int64) string {
	return fmt.Sprintf(
		`{"ok": true, "attachment_id": %d, "note": "图片已生成并直接展示给用户，图片内容已随本回执附上，请先自检是否满足 prompt；如需再次查看可用 read_attachment 回看"}`,
		attachmentID)
}

// ParsePrompt 从 tool call 的 arguments 里挖 prompt（可选 size）。
func ParsePrompt(arguments string) (prompt, size string) {
	var v struct {
		Prompt string `json:"prompt"`
		Size   string `json:"size"`
	}
	if err := json.Unmarshal([]byte(arguments), &v); err == nil {
		return strings.TrimSpace(v.Prompt), strings.TrimSpace(v.Size)
	}
	return strings.TrimSpace(arguments), ""
}

// ToolDef 是 generate_image 的工具声明（OpenAI tools 格式）。
func ToolDef() llm.Tool {
	var t llm.Tool
	t.Type = "function"
	t.Function.Name = ToolName
	t.Function.Description = "根据文字描述生成一张图片并展示给用户。用户明确要求画图、生成图片/海报/插画时使用。prompt 用英文写效果更好，要具体（主体、风格、构图、光线）。"
	t.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "英文画面描述，越具体越好",
			},
			"size": map[string]any{
				"type":        "string",
				"description": "尺寸：1024x1024（方形，默认）、1920x1080（横版）、1080x1920（竖版）",
			},
		},
		"required": []string{"prompt"},
	}
	return t
}

// TruncatePrompt 给落库记录用的安全截断。
func TruncatePrompt(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
