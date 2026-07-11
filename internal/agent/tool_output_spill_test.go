package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSpillToolOutputWritesAbsoluteRestrictedTemporaryFile(t *testing.T) {
	spillDir := t.TempDir()
	output := strings.Repeat("full output\n", 2000)
	path, ok := spillToolOutput(RunConfig{toolOutputSpillDir: spillDir}, output)
	if !ok || !filepath.IsAbs(path) || filepath.Dir(path) != spillDir {
		t.Fatalf("spill = %q, %v", path, ok)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != output {
		t.Fatal("spill did not preserve full raw output")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("spill mode = %o, want 600", info.Mode().Perm())
	}
}

func TestSpillToolOutputDisabledWithoutRunTempDirAndForReadOnly(t *testing.T) {
	if _, ok := spillToolOutput(RunConfig{}, "output"); ok {
		t.Fatal("spill outside a Runner.Run temp directory must be disabled")
	}
	if _, ok := spillToolOutput(RunConfig{toolOutputSpillDir: t.TempDir(), ToolAccessLevel: ToolAccessLevelReadOnly}, "output"); ok {
		t.Fatal("read-only run must not spill")
	}
}

func TestRunnerToolOutputSpillIsModelFacingAndCleanedOnReturn(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "call-1", Name: "large", Input: json.RawMessage(`{}`)}}}},
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
	}}
	agent := &Agent{Name: "spill", Tools: []Tool{&FunctionTool{
		ToolName: "large",
		Schema:   json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("界", 1000), nil
		},
	}}}
	_, err := NewRunnerWithModel(model).Run(context.Background(), agent, nil, RunConfig{MaxToolOutputBytes: 300})
	if err != nil {
		t.Fatal(err)
	}
	var modelOutput string
	for _, item := range model.requests[1].Input {
		if item.ToolOutput != nil {
			modelOutput = item.ToolOutput.Content
		}
	}
	if len(modelOutput) > 300 || !utf8.ValidString(modelOutput) {
		t.Fatalf("model output len=%d valid=%v", len(modelOutput), utf8.ValidString(modelOutput))
	}
	const prefix = "[full output saved to "
	start := strings.Index(modelOutput, prefix)
	if start < 0 || !strings.Contains(modelOutput[start:], "]") {
		t.Fatalf("missing spill hint in %q", modelOutput)
	}
	end := strings.Index(modelOutput[start:], "]") + start
	path := modelOutput[start+len(prefix) : end]
	if !filepath.IsAbs(path) {
		t.Fatalf("spill hint path = %q, want absolute", path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("spill path remains after Run: %v", err)
	}
}

func TestTruncateMiddleBytesBoundedUTF8(t *testing.T) {
	for _, input := range []string{strings.Repeat("abcdef", 100), strings.Repeat("界🙂", 100)} {
		for cap := 1; cap < 80; cap++ {
			got := truncateMiddleBytes(input, cap)
			if len(got) > cap {
				t.Fatalf("len = %d, cap = %d", len(got), cap)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8 at cap %d: %q", cap, got)
			}
		}
	}
}
