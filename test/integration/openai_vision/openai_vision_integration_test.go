package openai_vision_integration_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
	sdkruntime "github.com/gratefulagents/sdk/pkg/agentsdk/runtime"
)

const liveVisionModel = sdkopenai.DefaultChatModel

func TestLiveOpenAIOAuthVisionAnalyzerUsesGPT55(t *testing.T) {
	if liveTestsSkipped() {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}
	authPath := openAIOAuthPath(t)
	if authPath == "" {
		t.Skip("set OPENAI_OAUTH_AUTH_JSON_PATH or provide $HOME/.codex/auth.json to run live OpenAI OAuth vision integration tests")
	}

	text := analyzeLiveImage(t, sdkruntime.Config{
		Provider:                 "openai",
		Model:                    liveVisionModel,
		BaseURL:                  envOr("OPENAI_BASE_URL", "https://chatgpt.com/backend-api/codex"),
		AuthMode:                 string(sdkopenai.AuthModeOAuth),
		OpenAIOAuthPath:          authPath,
		OpenAIOAuthAccountID:     strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ACCOUNT_ID")),
		OpenAIOAuthAccountIDPath: strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ACCOUNT_ID_PATH")),
	})
	requireVisionLiveOK(t, text)
}

func TestLiveOpenAIAPIKeyVisionAnalyzerUsesGPT55(t *testing.T) {
	if liveTestsSkipped() {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		t.Skip("set OPENAI_API_KEY to run live OpenAI API-key vision integration tests")
	}

	text := analyzeLiveImage(t, sdkruntime.Config{
		Provider: "openai",
		Model:    liveVisionModel,
		APIKey:   apiKey,
		BaseURL:  strings.TrimSpace(os.Getenv("OPENAI_API_BASE_URL")),
		APIMode:  "responses",
	})
	requireVisionLiveOK(t, text)
}

func analyzeLiveImage(t *testing.T, cfg sdkruntime.Config) string {
	t.Helper()
	cfg.WorkDir = t.TempDir()
	analyzer := sdkruntime.VisionAnalyzer(cfg)
	if analyzer == nil {
		t.Fatal("runtime did not provide an OpenAI vision analyzer")
	}
	text, err := analyzer(context.Background(), testPNG(t), "image/png", "Look at the image. Reply exactly with: vision live ok", "low")
	if err != nil {
		t.Fatalf("vision analyzer error: %v", err)
	}
	return text
}

func requireVisionLiveOK(t *testing.T, text string) {
	t.Helper()
	if !strings.Contains(normalize(text), "vision live ok") {
		t.Fatalf("vision analyzer text = %q, want phrase %q", text, "vision live ok")
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
