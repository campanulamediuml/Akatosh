package media

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makePNG 造一张 w×h 的 PNG（可选透明背景）。
func makePNG(t *testing.T, w, h int, transparent bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if transparent && x < w/2 {
				img.Set(x, y, color.RGBA{0, 0, 0, 0})
				continue
			}
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{200, uint8(x % 256), 100, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func dims(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("解码 %s 失败: %v", path, err)
	}
	return cfg.Width, cfg.Height
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "misc"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// makeNoisyPNG 造一张「照片感」的图：像素带噪点，PNG 压不动，
// 这样「缩略图远小于原图」才是可验证的（渐变图 PNG 会压到比缩略图还小）。
func makeNoisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(12345)
	next := func() uint8 {
		seed = seed*1664525 + 1013904223
		return uint8(seed >> 24)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{next(), next(), next(), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSaveGeneratesThumbAndSend(t *testing.T) {
	s := newTestStore(t)

	// 一张 1600×1200 的"照片"（噪声图，PNG 压不动，体积接近真实照片的量级）
	raw := makeNoisyPNG(t, 1600, 1200)
	saved, err := s.Save(bytes.NewReader(raw), "big.png")
	if err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if saved.Width != 1600 || saved.Height != 1200 {
		t.Fatalf("原图尺寸不对: %dx%d", saved.Width, saved.Height)
	}
	if !saved.HasDerived {
		t.Fatal("没有生成派生图")
	}

	tw, th := dims(t, s.ThumbPath(saved.Hash))
	if tw != ThumbLongEdge || th != 1200*ThumbLongEdge/1600 {
		t.Fatalf("缩略图尺寸不对: %dx%d（长边应为 %d）", tw, th, ThumbLongEdge)
	}
	sw, sh := dims(t, s.SendPath(saved.Hash))
	if sw != SendLongEdge || sh != 1200*SendLongEdge/1600 {
		t.Fatalf("发送版尺寸不对: %dx%d（长边应为 %d）", sw, sh, SendLongEdge)
	}

	// 缩略图必须远小于原图与发送版 —— 这正是「图多也不卡」的关键
	thumbSize := fileSize(s.ThumbPath(saved.Hash))
	sendSize := fileSize(s.SendPath(saved.Hash))
	if thumbSize == 0 {
		t.Fatal("缩略图是空的")
	}
	if thumbSize >= saved.Size {
		t.Fatalf("缩略图(%d) 没有比原图(%d) 小", thumbSize, saved.Size)
	}
	if thumbSize >= sendSize {
		t.Fatalf("缩略图(%d) 没有比发送版(%d) 小", thumbSize, sendSize)
	}
	if thumbSize > 100<<10 {
		t.Fatalf("缩略图太大了（%d KB），一屏几十张会拖慢恢复", thumbSize>>10)
	}
	t.Logf("原图 %d KB → 缩略图 %d KB / 发送版 %d KB",
		saved.Size/1024, thumbSize/1024, sendSize/1024)
}

func TestSaveDeduplicates(t *testing.T) {
	s := newTestStore(t)
	raw := makePNG(t, 400, 300, false)

	first, err := s.Save(bytes.NewReader(raw), "a.png")
	if err != nil {
		t.Fatal(err)
	}
	// 同一份内容再传一次
	second, err := s.Save(bytes.NewReader(raw), "b.png")
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash {
		t.Fatalf("相同内容哈希不同: %s vs %s", first.Hash, second.Hash)
	}
	if second.Width != 400 || second.Height != 300 {
		t.Fatalf("去重路径没探测出尺寸: %dx%d", second.Width, second.Height)
	}

	// 目录里应当只有：原图 + 缩略图（这张没超尺寸，不生成发送版）
	entries, _ := os.ReadDir(s.shardDir(first.Hash))
	if len(entries) != 2 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("去重后目录里应当只有 2 个文件，实际 %d 个: %v", len(entries), names)
	}
}

// 原图本身就够小、尺寸也没超上限时，发送版直接用原图 —— 避免把截图上的文字重编码糊掉。
func TestSmallImageSkipsSendDerivative(t *testing.T) {
	s := newTestStore(t)
	raw := makeJPEG(t, 800, 600)

	saved, err := s.Save(bytes.NewReader(raw), "shot.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if fileExists(s.SendPath(saved.Hash)) {
		t.Fatal("小尺寸图不该再生成一张发送版")
	}
	// 但缩略图要有
	if !fileExists(s.ThumbPath(saved.Hash)) {
		t.Fatal("缩略图没生成")
	}
	// OpenForModel 应当退回原图
	b, mime, err := s.OpenForModel(saved.Hash, saved.Ext, saved.MIME)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != len(raw) {
		t.Fatalf("发送时应当直接用原图，长度 %d vs %d", len(b), len(raw))
	}
	if mime != "image/jpeg" {
		t.Fatalf("MIME 不对: %s", mime)
	}
}

// PNG 截图也走同一条规则：小图不重编码，否则文字会被 JPEG 糊掉。
func TestSmallPNGAlsoSkipsSendDerivative(t *testing.T) {
	s := newTestStore(t)
	raw := makePNG(t, 600, 400, false)

	saved, err := s.Save(bytes.NewReader(raw), "ui.png")
	if err != nil {
		t.Fatal(err)
	}
	if fileExists(s.SendPath(saved.Hash)) {
		t.Fatal("小尺寸 PNG 也不该重编码出发送版")
	}
	b, mime, err := s.OpenForModel(saved.Hash, saved.Ext, saved.MIME)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != len(raw) || mime != "image/png" {
		t.Fatalf("应当原样返回 PNG，实际 %d 字节 / %s", len(b), mime)
	}
}

func TestRejectsNonImage(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Save(bytes.NewReader([]byte("这不是图片，只是一段文字")), "a.txt")
	if !errors.Is(err, ErrNotImage) {
		t.Fatalf("应当报 ErrNotImage，实际 %v", err)
	}
	// 空文件也一样
	if _, err := s.Save(bytes.NewReader(nil), "empty.png"); !errors.Is(err, ErrNotImage) {
		t.Fatalf("空文件应当报 ErrNotImage，实际 %v", err)
	}
}

func TestTooLarge(t *testing.T) {
	s := newTestStore(t)
	// 造一个超过上限的字节流（不真的分配 20MB 图片，只要长度够就会先被拦下）
	big := make([]byte, MaxUploadBytes+1024)
	_, err := s.Save(bytes.NewReader(big), "huge.png")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("应当报 ErrTooLarge，实际 %v", err)
	}
}

// 透明像素要合成到白底，否则转成 JPEG 会变成黑块。
func TestTransparencyCompositeToWhite(t *testing.T) {
	s := newTestStore(t)
	raw := makePNG(t, 200, 200, true) // 左半边全透明
	saved, err := s.Save(bytes.NewReader(raw), "alpha.png")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(s.ThumbPath(saved.Hash))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	// 左上角原本是全透明的，现在应当是白的（允许 JPEG 的轻微偏差）
	r, g, b, _ := img.At(2, 2).RGBA()
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("透明区域没有合成成白底，实际 rgb=%d,%d,%d", r>>8, g>>8, b>>8)
	}
}

func TestFitDoesNotUpscale(t *testing.T) {
	cases := []struct{ w, h, max, wantW, wantH int }{
		{100, 50, 320, 100, 50},   // 本来就小，不放大
		{640, 480, 320, 320, 240}, // 横图
		{480, 640, 320, 240, 320}, // 竖图
		{1000, 1000, 320, 320, 320},
		{4000, 10, 320, 320, 1}, // 极端长条不能算出 0
	}
	for _, c := range cases {
		w, h := fit(c.w, c.h, c.max)
		if w != c.wantW || h != c.wantH {
			t.Errorf("fit(%d,%d,%d) = %d,%d，期望 %d,%d", c.w, c.h, c.max, w, h, c.wantW, c.wantH)
		}
	}
}

func TestStorePathsShardAndSurviveReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "misc")
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Save(bytes.NewReader(makePNG(t, 300, 200, false)), "x.png")
	if err != nil {
		t.Fatal(err)
	}
	// 分桶目录名应当是哈希前两位
	if got := filepath.Base(s.shardDir(saved.Hash)); got != saved.Hash[:2] {
		t.Fatalf("分桶目录不对: %s", got)
	}
	// 重新打开（模拟重启）还能读到
	s2, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := s2.OpenThumb(saved.Hash, saved.Ext)
	if err != nil {
		t.Fatalf("重开后读不到缩略图: %v", err)
	}
	f.Close()
	if !s2.Exists(saved.Hash) {
		t.Fatal("重开后 Exists 应当为 true")
	}
}

// 运行期目录被删掉（用户清理、Docker 卷卸载重挂）之后，上传不该报莫名其妙的错。
func TestSaveRecreatesMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "misc")
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟运行期被删
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Save(bytes.NewReader(makePNG(t, 120, 90, false)), "a.png")
	if err != nil {
		t.Fatalf("目录没了之后 Save 应当自愈，实际报错: %v", err)
	}
	if !fileExists(s.ThumbPath(saved.Hash)) {
		t.Fatal("自愈之后缩略图没生成")
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		t.Fatalf("根目录没有被重建: err=%v", err)
	}
}

func TestSaveIsReasonablyFast(t *testing.T) {
	s := newTestStore(t)
	raw := makePNG(t, 2000, 1500, false)
	start := time.Now()
	if _, err := s.Save(bytes.NewReader(raw), "big.png"); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	t.Logf("2000×1500 PNG 落盘+两张派生图耗时 %v", elapsed)
	if elapsed > 5*time.Second {
		t.Fatalf("太慢了: %v", elapsed)
	}
}
