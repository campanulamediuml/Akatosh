package httpapi

import (
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aiassistant/internal/store"
)

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func uploadDocumentOfSize(t *testing.T, s *Server, user *store.User, size int64) *httptest.ResponseRecorder {
	t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	writeDone := make(chan error, 1)
	go func() {
		part, err := mw.CreateFormFile("file", "boundary.txt")
		if err == nil {
			_, err = io.Copy(part, io.LimitReader(zeroReader{}, size))
		}
		if closeErr := mw.Close(); err == nil {
			err = closeErr
		}
		writeDone <- pw.CloseWithError(err)
	}()

	req := httptest.NewRequest(http.MethodPost, "/api/files", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req = req.WithContext(contextWithUser(req, user))
	rec := httptest.NewRecorder()
	s.handleUploadFile(rec, req)
	_ = req.Body.Close()
	if err := <-writeDone; err != nil {
		t.Fatalf("写入 multipart 上传内容失败: %v", err)
	}
	return rec
}

func TestUploadDocumentSizeLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		want int
	}{
		{name: "恰好100MB", size: maxUploadFileBytes, want: http.StatusOK},
		{name: "超过100MB一个字节", size: maxUploadFileBytes + 1, want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAuthTestServer(t, "upload-test-token")
			s.filesDir = t.TempDir()
			user := s.resolveUser("upload-test-token")
			rec := uploadDocumentOfSize(t, s, user, tc.size)
			if rec.Code != tc.want {
				t.Fatalf("状态码 %d，期望 %d，响应 %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusOK {
				var body struct {
					Files []struct {
						ID   int64 `json:"id"`
						Size int64 `json:"size"`
					} `json:"files"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("解析上传响应失败: %v", err)
				}
				if len(body.Files) != 1 || body.Files[0].ID == 0 || body.Files[0].Size != tc.size {
					t.Errorf("上传记录不符合预期: %+v", body.Files)
				}
			} else if !strings.Contains(rec.Body.String(), "文件超过 100MB 上限") {
				t.Errorf("超限提示不正确: %s", rec.Body.String())
			}
		})
	}
}
