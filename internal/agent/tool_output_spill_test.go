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

func TestRunnerToolOutputSpillUsesConfiguredRootAndPersistsAfterReturn(t *testing.T) {
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
	workDir := t.TempDir()
	outputRoot := t.TempDir()
	_, err := NewRunnerWithModel(model).Run(context.Background(), agent, nil, RunConfig{
		MaxToolOutputBytes: 300,
		WorkDir:            workDir,
		ToolOutputDir:      outputRoot,
	})
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
	if rel, err := filepath.Rel(outputRoot, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("spill hint path = %q, want under configured output root %q", path, outputRoot)
	}
	if rel, err := filepath.Rel(workDir, path); err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		t.Fatalf("spill hint path = %q, want outside workspace %q", path, workDir)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read retained spill after Run: %v", err)
	}
	if string(got) != strings.Repeat("界", 1000) {
		t.Fatal("retained spill did not preserve the full tool output")
	}
}

func TestPrepareToolOutputSpillRejectsWorkspaceLocations(t *testing.T) {
	workDir := t.TempDir()
	for name, outputDir := range map[string]string{
		"workspace":  workDir,
		"descendant": filepath.Join(workDir, "output"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.MkdirAll(outputDir, 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := RunConfig{WorkDir: workDir, ToolOutputDir: outputDir}
			cleanup := prepareToolOutputSpill(&cfg)
			defer cleanup()
			if cfg.toolOutputSpillDir != "" {
				t.Fatalf("spill dir = %q, want disabled inside workspace", cfg.toolOutputSpillDir)
			}
		})
	}

	outside := t.TempDir()
	link := filepath.Join(workDir, "linked-output")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cfg := RunConfig{WorkDir: outside, ToolOutputDir: link}
	cleanup := prepareToolOutputSpill(&cfg)
	defer cleanup()
	if cfg.toolOutputSpillDir != "" {
		t.Fatalf("spill dir = %q, want disabled for symlink into workspace", cfg.toolOutputSpillDir)
	}
}

func TestPrepareToolOutputSpillDefaultsToOSTempOutsideWorkspace(t *testing.T) {
	workDir := t.TempDir()
	outputRoot := t.TempDir()
	t.Setenv("TMPDIR", outputRoot)
	cfg := RunConfig{WorkDir: workDir}
	cleanup := prepareToolOutputSpill(&cfg)
	spillDir := cfg.toolOutputSpillDir
	if spillDir == "" {
		t.Fatal("default OS temp did not create spill directory")
	}
	if !pathWithin(outputRoot, spillDir) {
		t.Fatalf("spill dir = %q, want beneath OS temp %q", spillDir, outputRoot)
	}
	cleanup()
	if _, err := os.Stat(spillDir); !os.IsNotExist(err) {
		t.Fatalf("spill dir remains after cleanup: %v", err)
	}
}

func TestExecuteApprovedToolUsesConfiguredOutputRoot(t *testing.T) {
	workDir := t.TempDir()
	outputRoot := t.TempDir()
	agent := &Agent{Name: "spill", Tools: []Tool{&FunctionTool{
		ToolName: "large",
		Schema:   json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("approved output ", 100), nil
		},
	}}}
	item, _, _, _, err := NewRunnerWithModel(&mockModel{}).ExecuteApprovedTool(
		context.Background(), agent, ToolCallData{ID: "call-1", Name: "large", Input: json.RawMessage(`{}`)},
		RunConfig{MaxToolOutputBytes: 300, WorkDir: workDir, ToolOutputDir: outputRoot},
	)
	if err != nil {
		t.Fatal(err)
	}
	if item.ToolOutput == nil {
		t.Fatal("missing tool output")
	}
	const prefix = "[full output saved to "
	start := strings.Index(item.ToolOutput.Content, prefix)
	if start < 0 {
		t.Fatalf("missing spill hint in %q", item.ToolOutput.Content)
	}
	end := strings.Index(item.ToolOutput.Content[start:], "]") + start
	path := item.ToolOutput.Content[start+len(prefix) : end]
	if !pathWithin(outputRoot, path) {
		t.Fatalf("spill path = %q, want beneath %q", path, outputRoot)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read retained spill after approved tool returns: %v", err)
	}
	if string(got) != strings.Repeat("approved output ", 100) {
		t.Fatal("retained approved-tool spill did not preserve the full output")
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
