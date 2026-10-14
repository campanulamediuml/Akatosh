// Package media 负责图片的落地与派生。
//
// 目录约定（root 由 -misc 指定，默认 ./misc，方便 Docker 单独挂载）：
//
//	<misc>/images/<hash 前两位>/<hash>.<ext>          原图
//	<misc>/images/<hash 前两位>/<hash>.thumb.jpg      缩略图（给界面用，长边 320）
//	<misc>/images/<hash 前两位>/<hash>.send.jpg       发送版（给模型用，长边 1024）
//
// 用内容 sha256 做文件名：同一张图重复上传只占一份空间，URL 也可以放心地
// 设成 immutable 缓存。
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"

	// 注册解码器：png/gif/jpeg 是标准库，webp 只支持解码
	_ "golang.org/x/image/webp"
	_ "image/gif"
	_ "image/png"

	xdraw "golang.org/x/image/draw"
)

const (
	// ThumbLongEdge 缩略图长边。刻意压得比较小：一屏几十张图时，
	// 拉一堆 1024 的图会让恢复聊天记录非常慢。
	ThumbLongEdge = 320
	// SendLongEdge 发给模型的版本长边。
	// 手机照片动辄 4000px，原样 base64 会变成好几 MB 的请求体，又慢又贵；
	// 主流 vision API 本来也会自己降采样，这里先降到 1024。
	SendLongEdge = 1024

	thumbQuality = 80
	sendQuality  = 85

	// MaxUploadBytes 单个文件上限。
	MaxUploadBytes = 20 << 20
)

var (
	ErrTooLarge     = errors.New("文件超过大小上限")
	ErrNotImage     = errors.New("不是能识别的图片格式")
	ErrUnsupported  = errors.New("不支持的图片格式")
	supportedFormat = map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
		"image/gif":  ".gif",
		"image/webp": ".webp",
	}
)

// Store 是 misc 目录的句柄。
type Store struct {
	root string
}

func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("misc 目录不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	if err := os.MkdirAll(filepath.Join(abs, "images"), 0o755); err != nil {
		return nil, fmt.Errorf("创建图片目录失败: %w", err)
	}
	return &Store{root: abs}, nil
}

func (s *Store) Root() string { return s.root }

// Saved 是一次上传落盘后的结果。
type Saved struct {
	Hash   string
	Ext    string
	MIME   string
	Size   int64
	Width  int
	Height int
	// HasDerived 表示确实生成了缩略图/发送版（非图片或解码失败时为 false）
	HasDerived bool
}

// shardDir 用哈希前两位分桶，避免单目录塞几万个文件。
func (s *Store) shardDir(hash string) string {
	prefix := hash
	if len(prefix) > 2 {
		prefix = prefix[:2]
	}
	return filepath.Join(s.root, "images", prefix)
}

func (s *Store) OriginalPath(hash, ext string) string {
	return filepath.Join(s.shardDir(hash), hash+ext)
}

func (s *Store) ThumbPath(hash string) string {
	return filepath.Join(s.shardDir(hash), hash+".thumb.jpg")
}

func (s *Store) SendPath(hash string) string {
	return filepath.Join(s.shardDir(hash), hash+".send.jpg")
}

// Exists 判断这份内容是否已经落过盘（去重用）。
func (s *Store) Exists(hash string) bool {
	matches, _ := filepath.Glob(filepath.Join(s.shardDir(hash), hash+".*"))
	for _, m := range matches {
		if !strings.Contains(m, ".thumb.") && !strings.Contains(m, ".send.") {
			return true
		}
	}
	return false
}

// Save 收下一个上传：算哈希 → 落原图 → 生成缩略图与发送版。
// 内容已存在时直接返回已有结果（不重复写盘、不重复解码）。
func (s *Store) Save(r io.Reader, _ string) (*Saved, error) {
	// 目录可能在运行期被删掉（用户清理、Docker 卷被卸载后重挂），
	// 所以每次上传都确保它在，不然会报「系统找不到指定的路径」这种莫名其妙的错。
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return nil, fmt.Errorf("附件目录不可用（%s）: %w", s.root, err)
	}

	// 先落到临时文件，边写边算哈希，避免把大文件整个读进内存
	tmp, err := os.CreateTemp(s.root, "upload-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, MaxUploadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("写入失败: %w", err)
	}
	if size > MaxUploadBytes {
		return nil, ErrTooLarge
	}
	if size == 0 {
		return nil, ErrNotImage
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	hash := hex.EncodeToString(h.Sum(nil))

	// 去重：同一份内容已经存过就直接复用（但尺寸信息还得探测出来）
	if s.Exists(hash) {
		saved, err := s.inspectExisting(hash)
		if err == nil {
			return saved, nil
		}
		// 探测失败就当作新文件重走一遍
	}

	// 探测格式与尺寸
	f, err := os.Open(tmpPath)
	if err != nil {
		return nil, err
	}
	cfg, format, err := image.DecodeConfig(f)
	f.Close()
	if err != nil {
		return nil, ErrNotImage
	}
	ext, ok := supportedFormat["image/"+format]
	if !ok {
		if format == "jpeg" {
			ext = ".jpg"
		} else {
			return nil, ErrUnsupported
		}
	}

	if err := os.MkdirAll(s.shardDir(hash), 0o755); err != nil {
		return nil, err
	}
	origPath := s.OriginalPath(hash, ext)
	if err := os.Rename(tmpPath, origPath); err != nil {
		// 跨盘等情况退回复制
		if err := copyFile(tmpPath, origPath); err != nil {
			return nil, fmt.Errorf("保存原图失败: %w", err)
		}
		os.Remove(tmpPath)
	}

	saved := &Saved{
		Hash:   hash,
		Ext:    ext,
		MIME:   "image/" + format,
		Size:   size,
		Width:  cfg.Width,
		Height: cfg.Height,
	}
	// 派生图失败不算致命错误：原图还在，界面顶多退化成直接显示原图
	if err := s.derive(hash, origPath, cfg.Width, cfg.Height, ext); err == nil {
		saved.HasDerived = true
	}
	return saved, nil
}

func (s *Store) inspectExisting(hash string) (*Saved, error) {
	matches, _ := filepath.Glob(filepath.Join(s.shardDir(hash), hash+".*"))
	for _, p := range matches {
		base := filepath.Base(p)
		if strings.Contains(base, ".thumb.") || strings.Contains(base, ".send.") {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		cfg, format, err := image.DecodeConfig(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		mime := "image/" + format
		if format == "jpeg" {
			mime = "image/jpeg"
		}
		return &Saved{
			Hash:       hash,
			Ext:        filepath.Ext(p),
			MIME:       mime,
			Size:       st.Size(),
			Width:      cfg.Width,
			Height:     cfg.Height,
			HasDerived: fileExists(s.ThumbPath(hash)),
		}, nil
	}
	return nil, os.ErrNotExist
}

// derive 生成缩略图与发送版。两个都失败才算错。
func (s *Store) derive(hash, origPath string, w, h int, ext string) error {
	f, err := os.Open(origPath)
	if err != nil {
		return err
	}
	defer f.Close()

	src, _, err := image.Decode(f)
	if err != nil {
		return err
	}

	_ = ext // 发送版是否生成只看尺寸与体积，与格式无关

	errThumb := s.writeScaled(src, s.ThumbPath(hash), ThumbLongEdge, thumbQuality)
	// 原图本身就够小、尺寸也没超发送上限 → 发送时直接用它，**不重编码**。
	// 截图这类图转成 JPEG 会把文字糊掉，而它本来就不大，没必要动。
	if w <= SendLongEdge && h <= SendLongEdge && fileSize(origPath) <= 900<<10 {
		return errThumb
	}
	errSend := s.writeScaled(src, s.SendPath(hash), SendLongEdge, sendQuality)
	if errThumb != nil && errSend != nil {
		return errThumb
	}
	return nil
}

// writeScaled 等比缩放到长边不超过 maxEdge，编码成 JPEG。
// 透明像素先合成到白底上，否则 JPEG 会把透明区域变成黑块。
func (s *Store) writeScaled(src image.Image, dst string, maxEdge, quality int) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return fmt.Errorf("图片尺寸非法")
	}
	tw, th := fit(w, h, maxEdge)

	out := image.NewRGBA(image.Rect(0, 0, tw, th))
	// 先铺白底
	draw.Draw(out, out.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
	// CatmullRom 比默认的最近邻好太多，缩略图不会一脸锯齿
	xdraw.CatmullRom.Scale(out, image.Rect(0, 0, tw, th), src, b, draw.Over, nil)

	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(f, out, &jpeg.Options{Quality: quality}); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// fit 算等比缩放后的尺寸；已经比 maxEdge 小就不放大。
func fit(w, h, maxEdge int) (int, int) {
	if w <= maxEdge && h <= maxEdge {
		return w, h
	}
	if w >= h {
		nh := h * maxEdge / w
		if nh < 1 {
			nh = 1
		}
		return maxEdge, nh
	}
	nw := w * maxEdge / h
	if nw < 1 {
		nw = 1
	}
	return nw, maxEdge
}

// Open 打开原图。
func (s *Store) Open(hash, ext string) (*os.File, error) {
	return os.Open(s.OriginalPath(hash, ext))
}

// OpenThumb 打开缩略图；没生成过就退回原图。
func (s *Store) OpenThumb(hash, ext string) (*os.File, string, error) {
	if p := s.ThumbPath(hash); fileExists(p) {
		f, err := os.Open(p)
		return f, "image/jpeg", err
	}
	f, err := s.Open(hash, ext)
	return f, "", err
}

// OpenForModel 打开「发给模型用」的那份；优先发送版，其次原图。
// 返回字节、MIME、以及是否用了发送版。
func (s *Store) OpenForModel(hash, ext, mime string) ([]byte, string, error) {
	if p := s.SendPath(hash); fileExists(p) {
		b, err := os.ReadFile(p)
		return b, "image/jpeg", err
	}
	b, err := os.ReadFile(s.OriginalPath(hash, ext))
	if err != nil {
		return nil, "", err
	}
	if mime == "" {
		mime = "image/" + strings.TrimPrefix(ext, ".")
	}
	return b, mime, nil
}

// Remove 删掉某个内容的所有文件（原图 + 派生图）。
func (s *Store) Remove(hash, ext string) error {
	os.Remove(s.OriginalPath(hash, ext))
	os.Remove(s.ThumbPath(hash))
	os.Remove(s.SendPath(hash))
	return nil
}

// ---------- 小工具 ----------

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
