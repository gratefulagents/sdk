package vision

import (
	"context"
	"encoding/base64"
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

func TestExecuteReturnsNativeImage(t *testing.T) {
	dir := t.TempDir()
	data := []byte{0x89, 'P', 'N', 'G'}
	if err := os.WriteFile(filepath.Join(dir, "pixel.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, configured := range []bool{false, true} {
		tool := &Tool{}
		if configured {
			tool.AnalyzeWithDetailFn = func(context.Context, []byte, string, string, string) (string, error) {
				t.Fatal("separate analyzer must not be called")
				return "", nil
			}
		}
		result, err := tool.Execute(context.Background(), []byte(`{"image_path":"pixel.png","prompt":"inspect","detail_level":"low"}`), dir)
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError || result.Content != "inspect" || len(result.Images) != 1 {
			t.Fatalf("result = %+v", result)
		}
		image := result.Images[0]
		if image.MediaType != "image/png" || image.Data != base64.StdEncoding.EncodeToString(data) || image.Detail != "low" {
			t.Fatalf("image = %+v", image)
		}
	}
}

func TestLoadImageFromFileInDirsAllowsManagedAbsolutePath(t *testing.T) {
	workDir := t.TempDir()
	imageDir := t.TempDir()
	imagePath := filepath.Join(imageDir, "browser-shot.png")
	if err := os.WriteFile(imagePath, []byte{0x89, 'P', 'N', 'G'}, 0o600); err != nil {
		t.Fatal(err)
	}

	data, mimeType, err := LoadImageFromFileInDirs(workDir, imagePath, imageDir)
	if err != nil {
		t.Fatalf("LoadImageFromFileInDirs() error = %v", err)
	}
	if len(data) != 4 || mimeType != "image/png" {
		t.Fatalf("data=%v mimeType=%q", data, mimeType)
	}
}

func TestLoadImageFromFileInDirsRejectsOtherAbsolutePath(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	imageDir := filepath.Join(root, "screenshots")
	outside := filepath.Join(root, "outside.png")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte{0x89, 'P', 'N', 'G'}, 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadImageFromFileInDirs(workDir, outside, imageDir)
	if err == nil || !strings.Contains(err.Error(), "outside the workspace root") {
		t.Fatalf("LoadImageFromFileInDirs() error = %v, want path rejection", err)
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
