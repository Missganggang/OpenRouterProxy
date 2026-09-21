package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/config"
)

func TestNodeBinaryDownload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.NodeBinaryPath = dir
	h := &Handlers{app: &app.App{Config: cfg}}
	r := gin.New()
	r.GET("/api/node/binary/:arch", h.NodeBinary)
	payload := append([]byte{0x7f, 'E', 'L', 'F'}, bytes.Repeat([]byte{1, 2, 3, 4}, 128)...)
	write := func(path string, content []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, arch := range []string{"amd64", "amd64v3", "arm64"} {
		write(filepath.Join(dir, arch, "rel_nodeclient"), payload)
	}
	write(filepath.Join(dir, "nc-test", "amd64", "rel_nodeclient"), payload)
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"amd64", 200}, {"amd64v3", 200}, {"arm64", 200},
		{"amd64?version=latest", 200}, {"amd64?version=nc-test", 200},
		{"386", 400}, {"amd64?version=missing", 404},
		{"amd64?version=..", 400}, {"amd64?version=..%2F..", 400},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/node/binary/"+tc.path, nil))
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.status == 200 && !bytes.Equal(w.Body.Bytes(), payload) {
				t.Fatal("download changed binary bytes")
			}
			if tc.status == 200 && w.Header().Get("Content-Type") != "application/octet-stream" {
				t.Fatal("wrong content type")
			}
		})
	}
	write(filepath.Join(dir, "arm64", "rel_nodeclient"), []byte("not a binary"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/node/binary/arm64", nil))
	if w.Code != 500 {
		t.Fatalf("invalid binary status=%d", w.Code)
	}
	if err := os.Remove(filepath.Join(dir, "arm64", "rel_nodeclient")); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/node/binary/arm64", nil))
	if w.Code != 404 {
		t.Fatalf("missing binary status=%d", w.Code)
	}
}
