package guardrails

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// stubTool is a no-op Tool implementation used only to satisfy the guardrail
// signature in unit tests.
var stubTool agentsdk.Tool = &agentsdk.FunctionTool{ToolName: "stub"}

// loadCorpusLines reads the corpus, returning non-empty lines.
func loadCorpusLines(t *testing.T) []string {
	t.Helper()
	// Test runs from pkg/agentsdk/guardrails/; corpus lives at repo-root/eval/audit-fixtures/.
	path := filepath.Join("..", "..", "..", "eval", "audit-fixtures", "secret_obfuscation.txt")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		raw := scanner.Text()
		// Corpus lines are formatted as "<n>. <secret>"; strip the index prefix.
		if i := strings.Index(raw, ". "); i >= 0 && i <= 4 {
			raw = raw[i+2:]
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		lines = append(lines, raw)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan corpus: %v", err)
	}
	return lines
}

// runOutputGuardrails returns true if any built-in tool-output guardrail trips
// for the supplied content.
func runOutputGuardrails(t *testing.T, content string) (bool, string) {
	t.Helper()
	for _, gr := range BuiltinToolOutputGuardrails() {
		res, err := gr.Fn(nil, nil, stubTool, agentsdk.ToolResult{Content: content})
		if err != nil {
			t.Fatalf("guardrail %q error: %v", gr.Name, err)
		}
		if res != nil && res.TripwireTriggered {
			return true, gr.Name
		}
	}
	return false, ""
}

// TestSecretOutputGuardrail_CorpusAllDetected asserts that every line in the
// secret_obfuscation corpus trips the secret-output guardrail. This is the
// corpus-driven regression test for C5.
func TestSecretOutputGuardrail_CorpusAllDetected(t *testing.T) {
	lines := loadCorpusLines(t)
	if len(lines) == 0 {
		t.Fatal("corpus is empty")
	}
	var missed []string
	for _, line := range lines {
		tripped, _ := runOutputGuardrails(t, line)
		if !tripped {
			missed = append(missed, line)
		}
	}
	if len(missed) > 0 {
		t.Fatalf("secret-output guardrail missed %d/%d corpus lines:\n  - %s",
			len(missed), len(lines), strings.Join(missed, "\n  - "))
	}
}

// TestSecretOutputGuardrail_NegativeCorpus ensures benign English text and
// near-miss strings do NOT trigger the guardrail.
func TestSecretOutputGuardrail_NegativeCorpus(t *testing.T) {
	negatives := []string{
		"This documentation describes how AWS access keys are formatted.",
		"The ghp_ prefix is used by GitHub for personal access tokens.",
		"Authorization for the bearer of this letter is granted by the council.",
		"Please bear with us while we investigate the issue.",
		"Set api_key in your config file before running the program.",
		"sk-ate boards are popular among teenagers in California.",
		"The npm registry hosts JavaScript packages such as lodash and react.",
		"Bearer bonds were once a common financial instrument.",
	}
	for _, neg := range negatives {
		tripped, name := runOutputGuardrails(t, neg)
		if tripped {
			t.Errorf("false positive: guardrail %q tripped on benign text: %q", name, neg)
		}
	}
}
