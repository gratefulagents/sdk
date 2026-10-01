package search

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/vision"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 64, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readFileJSON(path string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"path":%q}`, path))
}

func TestReadFileToolReturnsImageAttachment(t *testing.T) {
	dir := t.TempDir()
	data := pngBytes(t, 32, 16)
	if err := os.MkdirAll(filepath.Join(dir, "img"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "img", "pixel.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := (&ReadFileTool{Images: true}).Execute(context.Background(), readFileJSON("img/pixel.png"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Images) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if want := "Image file img/pixel.png (32x16, image/png) attached."; result.Content != want {
		t.Fatalf("Content = %q, want %q", result.Content, want)
	}
	if got := result.Images[0]; got.MediaType != "image/png" || got.Data != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("image = %+v", got.MediaType)
	}
}

func TestReadFileToolSniffsExtensionlessImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "capture"), pngBytes(t, 4, 4), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := (&ReadFileTool{Images: true}).Execute(context.Background(), readFileJSON("capture"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 1 || !strings.HasPrefix(result.Content, "Image file capture (4x4, image/png)") {
		t.Fatalf("result = %q images=%d", result.Content, len(result.Images))
	}
}

func TestReadFileToolDownscalesLargeImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wide.png"), pngBytes(t, 4000, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := (&ReadFileTool{Images: true}).Execute(context.Background(), readFileJSON("wide.png"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 1 || !strings.Contains(result.Content, "(2000x50, image/png; downscaled from 4000x100)") {
		t.Fatalf("result = %q", result.Content)
	}
	data, err := base64.StdEncoding.DecodeString(result.Images[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > vision.MaxImageBytes {
		t.Fatalf("attached %d bytes", len(data))
	}
}

func TestReadFileToolImagesDisabledKeepsTextBehavior(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pixel.png"), pngBytes(t, 2, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := (&ReadFileTool{}).Execute(context.Background(), readFileJSON("pixel.png"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 0 || strings.HasPrefix(result.Content, "Image file") {
		t.Fatalf("result = %+v, want text read", result)
	}
}

func TestReadFileToolImagesStillReadsText(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "notes.txt", "hello")
	result, err := (&ReadFileTool{Images: true}).Execute(context.Background(), readFileJSON("notes.txt"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "hello" || len(result.Images) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestReadFileToolImageAllowedDir(t *testing.T) {
	workDir := t.TempDir()
	shotDir := t.TempDir()
	shot := filepath.Join(shotDir, "shot.png")
	if err := os.WriteFile(shot, pngBytes(t, 8, 8), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&ReadFileTool{Images: true}).Execute(context.Background(), readFileJSON(shot), workDir); err == nil {
		t.Fatal("read outside workspace without allowed dir succeeded")
	}
	tool := &ReadFileTool{Images: true, AllowedImageDirs: []string{shotDir}}
	result, err := tool.Execute(context.Background(), readFileJSON(shot), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 1 || !strings.Contains(result.Content, "shot.png (8x8, image/png)") {
		t.Fatalf("result = %q", result.Content)
	}
	if _, err := tool.Execute(context.Background(), readFileJSON("../"+filepath.Base(shotDir)+"/shot.png"), workDir); err == nil {
		t.Fatal("relative path escaped workspace via allowed dir")
	}
	writeTestFile(t, shotDir, "secret.txt", "secret")
	if _, err := tool.Execute(context.Background(), readFileJSON(filepath.Join(shotDir, "secret.txt")), workDir); err == nil {
		t.Fatal("allowed image dir authorized a text read")
	}
}

func TestReadFileToolImageRejectsOtherAbsolutePath(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "ws")
	shotDir := filepath.Join(root, "shots")
	for _, d := range []string{workDir, shotDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(root, "outside.png")
	if err := os.WriteFile(outside, pngBytes(t, 2, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &ReadFileTool{Images: true, AllowedImageDirs: []string{shotDir}}
	if _, err := tool.Execute(context.Background(), readFileJSON(outside), workDir); err == nil {
		t.Fatal("read of unrelated absolute image succeeded")
	}
}

func TestReadFileToolImageRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "ws")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.png")
	if err := os.WriteFile(outside, pngBytes(t, 2, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workDir, "link.png")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := (&ReadFileTool{Images: true}).Execute(context.Background(), readFileJSON("link.png"), workDir); err == nil {
		t.Fatal("symlinked image escaped workspace")
	}
}

func TestReadFileToolInvalidImageIsToolError(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "fake.png", "not an image")
	result, err := (&ReadFileTool{Images: true}).Execute(context.Background(), readFileJSON("fake.png"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || len(result.Images) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestReadFileToolImageNotFound(t *testing.T) {
	result, err := (&ReadFileTool{Images: true}).Execute(context.Background(), readFileJSON("missing.png"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "no such file") {
		t.Fatalf("result = %+v", result)
	}
}
