package vision

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/web"
)

func TestExecuteUsesDetailAwareAnalyzer(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pixel.png"), []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}

	var gotDetail string
	tool := &Tool{AnalyzeWithDetailFn: func(_ context.Context, imageData []byte, mimeType, prompt, detail string) (string, error) {
		gotDetail = detail
		if len(imageData) == 0 || mimeType != "image/png" || prompt != "inspect" {
			t.Fatalf("imageData=%d mime=%q prompt=%q", len(imageData), mimeType, prompt)
		}
		return "analysis", nil
	}}

	result, err := tool.Execute(context.Background(), []byte(`{"image_path":"pixel.png","prompt":"inspect","detail_level":"low"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.Content != "analysis" {
		t.Fatalf("result = %+v", result)
	}
	if gotDetail != "low" {
		t.Fatalf("detail = %q, want low", gotDetail)
	}
}

func TestLoadImageFromFileRejectsAbsoluteWorkspaceEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside.png")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("not really an image"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadImageFromFile(workDir, outside)
	if err == nil || !strings.Contains(err.Error(), "outside the workspace root") {
		t.Fatalf("LoadImageFromFile() error = %v, want workspace escape rejection", err)
	}
}

func TestLoadImageFromFileRejectsSymlinkWorkspaceEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside.png")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("not really an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workDir, "link.png")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, _, err := LoadImageFromFile(workDir, "link.png")
	if err == nil || !strings.Contains(err.Error(), "outside the workspace root") {
		t.Fatalf("LoadImageFromFile() error = %v, want symlink escape rejection", err)
	}
}

func TestLoadImageFromFileRejectsOversizedBeforeFullRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxImageSize + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, err = LoadImageFromFile(dir, "large.png")
	if err == nil || !strings.Contains(err.Error(), "image too large") {
		t.Fatalf("LoadImageFromFile() error = %v, want size cap", err)
	}
}

func TestLoadImageFromURLRejectsLocalhost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	defer server.Close()

	_, _, err := LoadImageFromURL(context.Background(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "private or local") {
		t.Fatalf("LoadImageFromURL() error = %v, want localhost SSRF rejection", err)
	}
}

func TestLoadImageFromURLUsesSafeHTTPClientFactory(t *testing.T) {
	oldFactory := newSafeHTTPClientWithOptions
	t.Cleanup(func() { newSafeHTTPClientWithOptions = oldFactory })

	called := false
	newSafeHTTPClientWithOptions = func(timeout time.Duration, opts web.URLSecurityOptions) *http.Client {
		called = true
		if timeout != 15*time.Second {
			t.Fatalf("timeout = %s, want 15s", timeout)
		}
		if opts.AllowPrivateNetworkURLs {
			t.Fatal("AllowPrivateNetworkURLs = true, want false")
		}
		return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "93.184.216.34" {
				t.Fatalf("request host = %q, want validated public host", req.URL.Host)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader(string([]byte{0x89, 'P', 'N', 'G'}))),
				Request:    req,
			}, nil
		})}
	}

	data, mimeType, err := LoadImageFromURL(context.Background(), "http://93.184.216.34/pixel.png")
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("LoadImageFromURL did not use safe HTTP client factory")
	}
	if mimeType != "image/png" || len(data) == 0 {
		t.Fatalf("mime=%q len=%d, want image/png response", mimeType, len(data))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
