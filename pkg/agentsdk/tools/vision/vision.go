package vision

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/web"
)

// MaxImageFileSize bounds the bytes read for a single image before decoding.
const MaxImageFileSize = 20 * 1024 * 1024

var newSafeHTTPClientWithOptions = web.NewSafeHTTPClientWithOptions

func LoadImageFromFile(workDir, path string) ([]byte, string, error) {
	return LoadImageFromFileInDirs(workDir, path)
}

// LoadImageFromFileInDirs loads an image from the workspace or from one of the
// host-managed allowed directories. Relative paths always resolve against the
// workspace; allowed directories only authorize absolute paths they contain.
func LoadImageFromFileInDirs(workDir, path string, allowedDirs ...string) ([]byte, string, error) {
	absPath, workspaceErr := pathutil.ResolveWorkspace(workDir, path)
	if workspaceErr != nil {
		if !filepath.IsAbs(path) {
			return nil, "", workspaceErr
		}
		absPath = ""
		for _, dir := range allowedDirs {
			dir = strings.TrimSpace(dir)
			if dir == "" {
				continue
			}
			candidate, err := pathutil.ResolveWorkspace(dir, path)
			if err == nil {
				absPath = candidate
				break
			}
		}
		if absPath == "" {
			return nil, "", workspaceErr
		}
	}
	f, err := pathutil.OpenFileNoFollow(absPath, os.O_RDONLY, 0)
	if err != nil {
		return nil, "", fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("stat file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s is not a regular file", absPath)
	}
	if info.Size() > MaxImageFileSize {
		return nil, "", fmt.Errorf("image too large (%d bytes, max %d)", info.Size(), MaxImageFileSize)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxImageFileSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading file: %w", err)
	}
	if len(data) > MaxImageFileSize {
		return nil, "", fmt.Errorf("image too large (> %d bytes)", MaxImageFileSize)
	}
	return data, DetectImageMIME(absPath, data), nil
}

func LoadImageFromURL(ctx context.Context, imageURL string) ([]byte, string, error) {
	return LoadImageFromURLWithOptions(ctx, imageURL, web.URLSecurityOptions{})
}

func LoadImageFromURLWithOptions(ctx context.Context, imageURL string, opts web.URLSecurityOptions) ([]byte, string, error) {
	parsedURL, err := web.ValidateHTTPURL(ctx, imageURL, opts)
	if err != nil {
		return nil, "", err
	}

	client := newSafeHTTPClientWithOptions(15*time.Second, opts)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", "gratefulagents-bot/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetching image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("HTTP %d fetching image", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxImageFileSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading response: %w", err)
	}
	if len(data) > MaxImageFileSize {
		return nil, "", fmt.Errorf("image too large (> %d bytes)", MaxImageFileSize)
	}

	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = DetectImageMIME("", data)
	}
	return data, mimeType, nil
}

func DetectImageMIME(path string, data []byte) string {
	if path != "" {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".png":
			return "image/png"
		case ".jpg", ".jpeg":
			return "image/jpeg"
		case ".gif":
			return "image/gif"
		case ".webp":
			return "image/webp"
		case ".svg":
			return "image/svg+xml"
		}
	}
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0xd8 {
		return "image/jpeg"
	}
	if len(data) >= 4 {
		if data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' {
			return "image/png"
		}
		if string(data[:4]) == "GIF8" {
			return "image/gif"
		}
		if string(data[:4]) == "RIFF" && len(data) >= 12 && string(data[8:12]) == "WEBP" {
			return "image/webp"
		}
	}
	return "application/octet-stream"
}
