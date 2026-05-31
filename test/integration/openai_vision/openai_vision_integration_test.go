package openai_vision_integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
	sdkruntime "github.com/gratefulagents/sdk/pkg/agentsdk/runtime"
	sdkvision "github.com/gratefulagents/sdk/pkg/agentsdk/tools/vision"
)

const liveVisionModel = sdkopenai.DefaultChatModel

func TestLiveOpenAIOAuthAnalyzeImageToolUsesGPT55(t *testing.T) {
	if liveTestsSkipped() {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}
	authPath := openAIOAuthPath(t)
	if authPath == "" {
		t.Skip("set OPENAI_OAUTH_AUTH_JSON_PATH or provide $HOME/.codex/auth.json to run live OpenAI OAuth vision integration tests")
	}

	result := executeLiveVisionTool(t, sdkruntime.Config{
		Provider:                 "openai",
		Model:                    liveVisionModel,
		BaseURL:                  envOr("OPENAI_BASE_URL", "https://chatgpt.com/backend-api/codex"),
		AuthMode:                 string(sdkopenai.AuthModeOAuth),
		OpenAIOAuthPath:          authPath,
		OpenAIOAuthAccountID:     strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ACCOUNT_ID")),
		OpenAIOAuthAccountIDPath: strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ACCOUNT_ID_PATH")),
	})
	requireVisionLiveOK(t, result.Content)
}

func TestLiveOpenAIAPIKeyAnalyzeImageToolUsesGPT55(t *testing.T) {
	if liveTestsSkipped() {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		t.Skip("set OPENAI_API_KEY to run live OpenAI API-key vision integration tests")
	}

	result := executeLiveVisionTool(t, sdkruntime.Config{
		Provider: "openai",
		Model:    liveVisionModel,
		APIKey:   apiKey,
		BaseURL:  strings.TrimSpace(os.Getenv("OPENAI_API_BASE_URL")),
		APIMode:  "responses",
	})
	requireVisionLiveOK(t, result.Content)
}

func executeLiveVisionTool(t *testing.T, cfg sdkruntime.Config) agentsdk.ToolResult {
	t.Helper()
	ctx := context.Background()
	workDir := t.TempDir()
	imagePath := filepath.Join(workDir, "vision-test.png")
	if err := os.WriteFile(imagePath, testPNG(t), 0o644); err != nil {
		t.Fatal(err)
	}

	visionTool := &sdkvision.Tool{}
	cfg.WorkDir = workDir
	cfg.EnableTools = true
	cfg.DisableDefaultTools = true
	cfg.DisableSignalTools = true
	cfg.ExtraTools = []agentsdk.Tool{visionTool}

	bundle, err := sdkruntime.BuildToolBundle(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if visionTool.AnalyzeWithDetailFn == nil {
		t.Fatal("runtime did not wire OpenAI AnalyzeImage")
	}

	var tool agentsdk.Tool
	for _, candidate := range bundle.Tools {
		if candidate.Name() == "AnalyzeImage" {
			tool = candidate
			break
		}
	}
	if tool == nil {
		t.Fatalf("AnalyzeImage tool missing; tools=%v", toolNames(bundle.Tools))
	}

	result, err := tool.Execute(ctx, json.RawMessage(`{
		"image_path": "vision-test.png",
		"prompt": "Look at the image. Reply exactly with: vision live ok",
		"detail_level": "low"
	}`), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("AnalyzeImage returned error: %s", result.Content)
	}
	return result
}

func requireVisionLiveOK(t *testing.T, text string) {
	t.Helper()
	if !strings.Contains(normalize(text), "vision live ok") {
		t.Fatalf("AnalyzeImage content = %q, want phrase %q", text, "vision live ok")
	}
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{G: 255, A: 255})
	img.Set(0, 1, color.RGBA{B: 255, A: 255})
	img.Set(1, 1, color.RGBA{R: 255, G: 255, B: 255, A: 255})

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func openAIOAuthPath(t *testing.T) string {
	t.Helper()
	authPath := strings.TrimSpace(os.Getenv("OPENAI_OAUTH_AUTH_JSON_PATH"))
	if authPath == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			candidate := filepath.Join(home, ".codex", "auth.json")
			if _, err := os.Stat(candidate); err == nil {
				authPath = candidate
			}
		}
	}
	if authPath == "" {
		return ""
	}
	if _, err := os.Stat(authPath); err != nil {
		t.Skipf("OAuth auth JSON not available at %s: %v", authPath, err)
	}
	return authPath
}

func normalize(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	return strings.Trim(text, " \t\r\n`\"'.!")
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func liveTestsSkipped() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GRATEFUL_LIVE_TESTS")), "skip")
}

func toolNames(tools []agentsdk.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool != nil {
			names = append(names, tool.Name())
		}
	}
	return names
}
