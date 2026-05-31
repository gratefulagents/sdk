package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkpolicy "github.com/gratefulagents/sdk/pkg/agentsdk/policy"
)

func TestPromptFromArgsUsesPromptfooFirstArgument(t *testing.T) {
	got := promptFromArgs([]string{
		"actual prompt",
		`{"id":"provider"}`,
		`{"vars":{"case":"one"}}`,
	})
	if got != "actual prompt" {
		t.Fatalf("promptFromArgs() = %q", got)
	}
}

func TestPromptFromArgsJoinsShellWords(t *testing.T) {
	got := promptFromArgs([]string{"fix", "the", "repo"})
	if got != "fix the repo" {
		t.Fatalf("promptFromArgs() = %q", got)
	}
}

func TestResolvePromptReadsRequestedStdin(t *testing.T) {
	got, err := resolvePrompt(cliConfig{ReadStdin: true}, nil, strings.NewReader("from stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "from stdin" {
		t.Fatalf("resolvePrompt() = %q", got)
	}
}

func TestParseConfigEvalFlags(t *testing.T) {
	cfg, _, err := parseConfig([]string{
		"--async-bash",
		"--final-check",
		"--exit-zero-on-timeout",
		"--web-tools=false",
		"--terminal-bench-compliance",
		"--timeout", "2m",
		"task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.EnableAsyncShell || !cfg.EnableFinalCheck || !cfg.ExitZeroOnTimeout {
		t.Fatalf("eval flags not parsed: %+v", cfg)
	}
	if cfg.EnableWebTools || !cfg.TerminalBenchCompliance {
		t.Fatalf("web/compliance flags not parsed: %+v", cfg)
	}
	if cfg.Timeout != 2*time.Minute {
		t.Fatalf("Timeout = %s, want 2m", cfg.Timeout)
	}
}

func TestRuntimeDirectiveTextIncludesDeadlineAndAsyncShell(t *testing.T) {
	text := runtimeDirectiveText(cliConfig{Mode: "eval", Timeout: 10 * time.Minute, EnableAsyncShell: true})
	if !strings.Contains(text, "<deadline>") || !strings.Contains(text, "<async_shell>") {
		t.Fatalf("runtimeDirectiveText missing sections: %s", text)
	}
}

func TestFinalCheckInstructionsOptIn(t *testing.T) {
	if got := finalCheckInstructions(cliConfig{}); got != "" {
		t.Fatalf("finalCheckInstructions disabled = %q", got)
	}
	got := finalCheckInstructions(cliConfig{EnableFinalCheck: true})
	if !strings.Contains(got, "final_artifact_check") || !strings.Contains(got, "required files") {
		t.Fatalf("finalCheckInstructions enabled = %q", got)
	}
}

func TestTerminalBenchForbiddenLookup(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		blocked bool
	}{
		{
			name:    "terminal bench site",
			input:   `{"command":"curl -L https://terminal-bench.org/tasks"}`,
			blocked: true,
		},
		{
			name:    "github search",
			input:   `{"url":"https://api.github.com/search/code?q=Terminal-Bench+dna-insert"}`,
			blocked: true,
		},
		{
			name:    "raw task repo",
			input:   `{"command":"wget https://raw.githubusercontent.com/terminal-bench/tasks/main/README.md"}`,
			blocked: true,
		},
		{
			name:    "normal package index",
			input:   `{"command":"curl -fsSL https://pypi.org/simple/numpy/"}`,
			blocked: false,
		},
		{
			name:    "local prompt path",
			input:   `{"command":"ls /tmp/grateful-terminal-bench-instruction.txt"}`,
			blocked: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := terminalBenchForbiddenLookup(tt.input) != ""
			if got != tt.blocked {
				t.Fatalf("terminalBenchForbiddenLookup(%q) blocked=%v, want %v", tt.input, got, tt.blocked)
			}
		})
	}
}

func TestTerminalBenchComplianceGuardrail(t *testing.T) {
	rules := terminalBenchComplianceGuardrails(true)
	if len(rules) != 1 {
		t.Fatalf("guardrails len=%d, want 1", len(rules))
	}
	result, err := rules[0].Fn(nil, nil, &agentsdk.FunctionTool{ToolName: "Bash"}, json.RawMessage(`{"command":"git clone https://github.com/terminal-bench/terminal-bench"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.TripwireTriggered {
		t.Fatalf("guardrail result=%+v, want tripwire", result)
	}
}

func TestParseToolAccessRejectsUnknownValue(t *testing.T) {
	if _, err := parseToolAccess("readonly"); err != nil {
		t.Fatalf("parseToolAccess(readonly) error = %v", err)
	}
	if _, err := parseToolAccess("typo-full"); err == nil {
		t.Fatal("parseToolAccess accepted unknown tool access")
	}
}

func TestParsePermissionMode(t *testing.T) {
	got, err := parsePermissionMode("danger-full-access")
	if err != nil {
		t.Fatal(err)
	}
	if got != sdkpolicy.PermissionModeDangerFullAccess {
		t.Fatalf("parsePermissionMode() = %q", got)
	}

	got, err = parsePermissionMode("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("empty parsePermissionMode() = %q", got)
	}

	if _, err := parsePermissionMode("mostly-write"); err == nil {
		t.Fatal("parsePermissionMode accepted unknown mode")
	}
}

func TestResolvedPermissionModeDefaultsFromToolAccess(t *testing.T) {
	got := resolvedPermissionMode("", agentsdk.ToolAccessLevelFull)
	if got != sdkpolicy.PermissionModeWorkspaceWrite {
		t.Fatalf("resolvedPermissionMode(full) = %q", got)
	}
	got = resolvedPermissionMode("", agentsdk.ToolAccessLevelReadOnly)
	if got != sdkpolicy.PermissionModeReadOnly {
		t.Fatalf("resolvedPermissionMode(read-only) = %q", got)
	}
	got = resolvedPermissionMode(sdkpolicy.PermissionModeDangerFullAccess, agentsdk.ToolAccessLevelFull)
	if got != sdkpolicy.PermissionModeDangerFullAccess {
		t.Fatalf("resolvedPermissionMode(danger) = %q", got)
	}
}

func TestBuildOutputCollectsMetrics(t *testing.T) {
	result := &agentsdk.RunResult{
		FinalOutput: "done",
		RawResponses: []agentsdk.ModelResponse{
			{},
			{},
		},
		Usage: agentsdk.Usage{InputTokens: 10, OutputTokens: 5},
		NewItems: []agentsdk.RunItem{
			{
				Type: agentsdk.RunItemToolCall,
				ToolCall: &agentsdk.ToolCallData{
					ID:    "call-1",
					Name:  "ReadFile",
					Input: []byte(`{"path":"README.md"}`),
				},
			},
			{
				Type: agentsdk.RunItemToolOutput,
				ToolOutput: &agentsdk.ToolOutputData{
					CallID:  "call-1",
					Content: "ok",
				},
			},
			{Type: agentsdk.RunItemCompaction},
		},
	}

	out := buildOutput(cliConfig{TaskID: "task-1", CandidateID: "candidate"}, "run-1", "/tmp/trace", result, 2*time.Second, nil)
	if out.FinalText != "done" || out.Metrics.TurnsUsed != 2 || out.Metrics.ToolCalls != 1 || out.Metrics.CompactionHits != 1 {
		t.Fatalf("unexpected output envelope: %+v", out)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != "ReadFile" || out.ToolCalls[0].Output != "ok" {
		t.Fatalf("tool summaries = %#v", out.ToolCalls)
	}
}
