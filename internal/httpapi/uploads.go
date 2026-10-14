package httpapi

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"aiassistant/internal/media"
	"aiassistant/internal/store"
)

// zeroTime 表示「不知道修改时间」，交给 ServeContent 只用 ETag 判断缓存。
var zeroTime time.Time

// maxUploadTotal 是一次请求里所有文件加起来的上限。
const maxUploadTotal = media.MaxUploadBytes * 4

// attachmentView 是附件对外的样子。
//
// 刻意只给**相对 URL**，不塞任何图片数据：恢复聊天记录时前端只请求 thumb，
// 原图要等用户点开才拉。图多的会话这点很关键。
func attachmentView(a *store.Attachment) map[string]any {
	if a == nil {
		return nil
	}
	return map[string]any{
		"id":         a.ID,
		"kind":       a.Kind,
		"mime":       a.MIME,
		"name":       a.Name,
		"size":       a.Size,
		"width":      a.Width,
		"height":     a.Height,
		"thumb_url":  fmt.Sprintf("/api/attachments/%d/thumb", a.ID),
		"raw_url":    fmt.Sprintf("/api/attachments/%d/raw", a.ID),
		"created_at": a.CreatedAt,
	}
}

// handleUpload 收图片：算哈希落盘、生成缩略图与发送版、登记元数据。
// 同一张图重复上传会命中去重，只返回已有记录。
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.media == nil {
		writeErr(w, http.StatusInternalServerError, "internal", "这个部署没有启用图片（misc 目录未配置）")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadTotal)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "解析上传内容失败（可能是文件过大）: "+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "没有收到文件（字段名应为 file）")
		return
	}

	out := make([]map[string]any, 0, len(files))
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "打开上传文件失败: "+err.Error())
			return
		}
		started := time.Now()
		saved, err := s.media.Save(f, fh.Filename)
		f.Close()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", friendlyMediaErr(err))
			return
		}
		log.Printf("[upload] %s  %.0f KB %dx%d → 缩略图 %.0f KB / 发送版 %.0f KB  用时 %dms",
			fh.Filename, float64(saved.Size)/1024, saved.Width, saved.Height,
			float64(fileSizeOf(s.media.ThumbPath(saved.Hash)))/1024,
			float64(fileSizeOf(s.media.SendPath(saved.Hash)))/1024,
			time.Since(started).Milliseconds())

		att, err := s.st.CreateAttachment(store.Attachment{
			Hash:   saved.Hash,
			Kind:   "image",
			MIME:   saved.MIME,
			Ext:    saved.Ext,
			Name:   fh.Filename,
			Size:   saved.Size,
			Width:  saved.Width,
			Height: saved.Height,
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		out = append(out, attachmentView(att))
	}
	writeJSON(w, http.StatusOK, map[string]any{"attachments": out})
}

// fileSizeOf 拿文件大小（不存在返回 0），只用于日志。
func fileSizeOf(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

func friendlyMediaErr(err error) string {
	switch {
	case errors.Is(err, media.ErrTooLarge):
		return fmt.Sprintf("图片超过了 %d MB 的上限", media.MaxUploadBytes>>20)
	case errors.Is(err, media.ErrNotImage):
		return "这个文件不是能识别的图片（支持 png / jpg / gif / webp）"
	case errors.Is(err, media.ErrUnsupported):
		return "不支持的图片格式（支持 png / jpg / gif / webp）"
	default:
		return "处理图片失败: " + err.Error()
	}
}

// handleAttachmentRaw 出原图（点了才拉）。
func (s *Server) handleAttachmentRaw(w http.ResponseWriter, r *http.Request) {
	s.serveAttachment(w, r, false)
}

// handleAttachmentThumb 出缩略图（界面默认拉的）。
func (s *Server) handleAttachmentThumb(w http.ResponseWriter, r *http.Request) {
	s.serveAttachment(w, r, true)
}

func (s *Server) serveAttachment(w http.ResponseWriter, r *http.Request, thumb bool) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if s.media == nil {
		writeErr(w, http.StatusInternalServerError, "internal", "这个部署没有启用图片")
		return
	}
	att, err := s.st.GetAttachment(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if att == nil {
		writeErr(w, http.StatusNotFound, "not_found", "附件不存在")
		return
	}

	var (
		f    *os.File
		mime string
	)
	if thumb {
		f, mime, err = s.media.OpenThumb(att.Hash, att.Ext)
	} else {
		f, err = s.media.Open(att.Hash, att.Ext)
		mime = att.MIME
	}
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "附件文件不在 misc 目录里（可能被清理过）")
		return
	}
	defer f.Close()

	if mime != "" {
		w.Header().Set("Content-Type", mime)
	}
	// 内容是内容寻址的（文件名就是 sha256）→ 永远不会变，可以放心长缓存。
	// 这也是"图多的会话恢复起来不慢"的关键：第二次打开直接走浏览器缓存。
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	etag := `"` + att.Hash + `"`
	if thumb {
		etag = `"` + att.Hash + `-thumb"`
	}
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, strings.Trim(etag, `"`)) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// ServeContent 负责 Range / If-Modified-Since；modtime 用 0 表示"不知道"
	http.ServeContent(w, r, att.Name, zeroTime, f)
}
