package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"aiassistant/internal/store"
)

// 上传文件（文档）的处理：原始二进制按内容哈希存到 filesDir/<hash><ext>，
// 文本提取由 pybox 沙箱按需进行（read_file 工具调它），Go 不碰格式解析。

// maxUploadFileBytes 是单文件上限（PDF/DOCX 文档类）。
const maxUploadFileBytes = 100 << 20

// 支持的扩展名（read_file 的提取脚本按这些分发；不支持的明确拒绝，
// 避免用户传个视频然后纳闷为什么 AI 说读不了）。
var uploadFileExts = map[string]bool{
	// 纯文本 / 代码 / 数据
	"txt": true, "md": true, "markdown": true, "log": true, "csv": true,
	"json": true, "jsonl": true, "xml": true, "yaml": true, "yml": true,
	"toml": true, "ini": true, "sql": true, "html": true, "css": true,
	"py": true, "js": true, "ts": true, "go": true, "java": true,
	"c": true, "h": true, "cpp": true, "rs": true, "sh": true,
	// 文档（pybox 提取文本；PDF 也可用 pymupdf 将指定页渲染成图片）
	"pdf": true, "docx": true, "xlsx": true,
}

// fileView 是文件记录对外的样子（不含内容——内容要 read_file 去沙箱提取）。
func fileView(f *store.File) map[string]any {
	if f == nil {
		return nil
	}
	return map[string]any{
		"id":   f.ID,
		"name": f.Name,
		"ext":  f.Ext,
		"size": f.Size,
	}
}

// handleUploadFile 收文档：算哈希落盘（去重）+ 登记元信息。
func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	if s.filesDir == "" {
		writeErr(w, http.StatusInternalServerError, "internal", "这个部署没有启用文件上传（misc 目录未配置）")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadFileBytes*2)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", uploadTooLargeErr(err, maxUploadFileBytes>>20))
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
		ext := strings.ToLower(filepath.Ext(fh.Filename))
		if !uploadFileExts[strings.TrimPrefix(ext, ".")] {
			writeErr(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("暂不支持的格式 %s（支持：文本/代码/CSV/JSON/PDF/DOCX/XLSX）", ext))
			return
		}
		f, err := fh.Open()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "打开上传文件失败: "+err.Error())
			return
		}
		rec, err := s.saveUploadedFile(userOf(r).ID, f, fh.Filename, ext)
		f.Close()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		log.Printf("[upload-file] %s  %.0f KB → /files/%s%s", fh.Filename, float64(rec.Size)/1024, rec.Hash[:8], ext)
		out = append(out, fileView(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": out})
}

// saveUploadedFile 把上传内容按内容哈希落盘（去重），并登记 files 表（记录属主）。
func (s *Server) saveUploadedFile(userID int64, r io.Reader, name, ext string) (*store.File, error) {
	if err := os.MkdirAll(s.filesDir, 0o755); err != nil {
		return nil, fmt.Errorf("文件目录不可用: %w", err)
	}
	// 边写边算哈希（大文件不整个读进内存）
	tmp, err := os.CreateTemp(s.filesDir, "up-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, maxUploadFileBytes+1))
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		return nil, fmt.Errorf("写入失败: %w", err)
	}
	if n > maxUploadFileBytes {
		os.Remove(tmp.Name())
		return nil, fmt.Errorf("文件超过 %dMB 上限", maxUploadFileBytes>>20)
	}
	hash := hex.EncodeToString(h.Sum(nil))
	dst := filepath.Join(s.filesDir, hash+ext)
	if _, err := os.Stat(dst); err != nil {
		// 新内容：挪到正式名（已存在则复用——内容哈希去重）
		if err := os.Rename(tmp.Name(), dst); err != nil {
			os.Remove(tmp.Name())
			return nil, fmt.Errorf("保存失败: %w", err)
		}
	} else {
		os.Remove(tmp.Name())
	}
	// CreateTemp 是 0600；pybox 沙箱以 nobody 身份挂 ro 读这个目录，得保证别的用户可读
	if err := os.Chmod(dst, 0o644); err != nil {
		return nil, fmt.Errorf("设置文件权限失败: %w", err)
	}
	return s.st.CreateFile(store.File{
		UserID: userID, Hash: hash, Name: name, Ext: ext, Size: int(n),
	})
}
