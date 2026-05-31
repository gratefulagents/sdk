package vision

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/web"
)

// AnalyzeFn performs vision analysis for a loaded image.
type AnalyzeFn func(ctx context.Context, imageData []byte, mimeType, prompt string) (string, error)

// AnalyzeWithDetailFn performs vision analysis for a loaded image with the
// caller-requested image detail level.
type AnalyzeWithDetailFn func(ctx context.Context, imageData []byte, mimeType, prompt, detailLevel string) (string, error)

// Tool analyzes images using a caller-supplied multimodal model function.
type Tool struct {
	AnalyzeFn               AnalyzeFn
	AnalyzeWithDetailFn     AnalyzeWithDetailFn
	AllowPrivateNetworkURLs bool
}

type input struct {
	ImagePath   string `json:"image_path"`
	URL         string `json:"url"`
	Prompt      string `json:"prompt"`
	DetailLevel string `json:"detail_level"`
}

func (t *Tool) Name() string { return "AnalyzeImage" }

func (t *Tool) Description() string {
	return "Analyzes an image using vision AI. Accepts a local file path or URL. Returns a text description or analysis based on the prompt. Useful for reviewing screenshots, UI designs, diagrams, and visual content."
}

func (t *Tool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"image_path": {
				"type": "string",
				"description": "Local file path to the image (relative to workspace)"
			},
			"url": {
				"type": "string",
				"description": "URL of the image to analyze"
			},
			"prompt": {
				"type": "string",
				"description": "What to analyze or describe (e.g., 'describe the UI layout', 'find visual bugs', 'extract text')"
			},
			"detail_level": {
				"type": "string",
				"enum": ["low", "high"],
				"description": "Analysis detail level (default: high)"
			}
		},
		"required": ["prompt"]
	}`)
}

func (t *Tool) IsReadOnly() bool { return true }

func (t *Tool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *Tool) NeedsApproval() bool { return false }

func (t *Tool) TimeoutSeconds() int { return 0 }

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if in.Prompt == "" {
		return agentsdk.ToolResult{Content: "prompt is required", IsError: true}, nil
	}
	if in.ImagePath == "" && in.URL == "" {
		return agentsdk.ToolResult{Content: "either image_path or url is required", IsError: true}, nil
	}

	var imageData []byte
	var mimeType string
	var err error
	if in.ImagePath != "" {
		imageData, mimeType, err = LoadImageFromFile(workDir, in.ImagePath)
	} else {
		imageData, mimeType, err = LoadImageFromURLWithOptions(ctx, in.URL, web.URLSecurityOptions{
			AllowPrivateNetworkURLs: t.AllowPrivateNetworkURLs,
		})
	}
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to load image: %v", err), IsError: true}, nil
	}

	if t.AnalyzeFn == nil && t.AnalyzeWithDetailFn == nil {
		b64 := base64.StdEncoding.EncodeToString(imageData)
		return agentsdk.ToolResult{Content: fmt.Sprintf(
			"Image loaded (%s, %d bytes, base64 length: %d). Vision provider not configured - set up an LLM with vision capabilities to enable analysis.",
			mimeType, len(imageData), len(b64),
		)}, nil
	}

	detailLevel := normalizeDetailLevel(in.DetailLevel)
	if t.AnalyzeWithDetailFn != nil {
		analysis, err := t.AnalyzeWithDetailFn(ctx, imageData, mimeType, in.Prompt, detailLevel)
		if err != nil {
			return agentsdk.ToolResult{Content: fmt.Sprintf("Vision analysis failed: %v", err), IsError: true}, nil
		}
		return agentsdk.ToolResult{Content: analysis}, nil
	}

	analysis, err := t.AnalyzeFn(ctx, imageData, mimeType, in.Prompt)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Vision analysis failed: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: analysis}, nil
}

func normalizeDetailLevel(detailLevel string) string {
	switch strings.ToLower(strings.TrimSpace(detailLevel)) {
	case "low":
		return "low"
	default:
		return "high"
	}
}

const maxImageSize = 20 * 1024 * 1024

var newSafeHTTPClientWithOptions = web.NewSafeHTTPClientWithOptions

func LoadImageFromFile(workDir, path string) ([]byte, string, error) {
	absPath, err := pathutil.ResolveWorkspace(workDir, path)
	if err != nil {
		return nil, "", err
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
	if info.Size() > maxImageSize {
		return nil, "", fmt.Errorf("image too large (%d bytes, max %d)", info.Size(), maxImageSize)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxImageSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading file: %w", err)
	}
	if len(data) > maxImageSize {
		return nil, "", fmt.Errorf("image too large (> %d bytes)", maxImageSize)
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

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading response: %w", err)
	}
	if len(data) > maxImageSize {
		return nil, "", fmt.Errorf("image too large (> %d bytes)", maxImageSize)
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
