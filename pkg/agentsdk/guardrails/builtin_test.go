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

// runOutputGuardrails returns true if any built-in tool-output guardrail
// detects a secret (by tripping or redacting) in the supplied content.
func runOutputGuardrails(t *testing.T, content string) (bool, string) {
	t.Helper()
	for _, gr := range BuiltinToolOutputGuardrails() {
		res, err := gr.Fn(nil, nil, stubTool, agentsdk.ToolResult{Content: content})
		if err != nil {
			t.Fatalf("guardrail %q error: %v", gr.Name, err)
		}
		if res != nil && (res.TripwireTriggered || res.ContentReplaced) {
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

// TestSecretOutputGuardrail_RedactsGitHubToken verifies that a matched token
// is masked in place, surrounding text survives, the notice is appended, and
// the tripwire does not trip.
func TestSecretOutputGuardrail_RedactsGitHubToken(t *testing.T) {
	token := "ghp" + "_" + strings.Repeat("a1B2", 9)
	content := "config before\ntoken=" + token + "\nconfig after"
	gr := BuiltinToolOutputGuardrails()[0]
	res, err := gr.Fn(nil, nil, stubTool, agentsdk.ToolResult{Content: content})
	if err != nil {
		t.Fatalf("guardrail error: %v", err)
	}
	if res.TripwireTriggered {
		t.Fatal("tripwire triggered, want redaction only")
	}
	if !res.ContentReplaced {
		t.Fatal("ContentReplaced = false, want true")
	}
	if strings.Contains(res.ReplacementContent, token) {
		t.Fatalf("token survived redaction: %q", res.ReplacementContent)
	}
	if !strings.Contains(res.ReplacementContent, "token=[REDACTED:GitHub token]") {
		t.Fatalf("missing redaction marker: %q", res.ReplacementContent)
	}
	if !strings.Contains(res.ReplacementContent, "config before") || !strings.Contains(res.ReplacementContent, "config after") {
		t.Fatalf("surrounding text not preserved: %q", res.ReplacementContent)
	}
	if !strings.Contains(res.ReplacementContent, "[guardrail detect-secret-in-output: redacted 1 potential secret(s): GitHub token.") {
		t.Fatalf("missing notice: %q", res.ReplacementContent)
	}
}

// TestSecretOutputGuardrail_RedactsPEMBlockFully verifies that the whole PEM
// block from BEGIN through END is removed, not just the header.
func TestSecretOutputGuardrail_RedactsPEMBlockFully(t *testing.T) {
	const keyBody = "MIIEpAIBAAKCAQEAfakekeymaterial"
	pem := "-----BEGIN RSA PRIVATE " + "KEY-----\n" + keyBody + "\n-----END RSA PRIVATE " + "KEY-----"
	content := "before\n" + pem + "\nafter"
	gr := BuiltinToolOutputGuardrails()[0]
	res, err := gr.Fn(nil, nil, stubTool, agentsdk.ToolResult{Content: content})
	if err != nil {
		t.Fatalf("guardrail error: %v", err)
	}
	if res.TripwireTriggered {
		t.Fatal("tripwire triggered, want redaction only")
	}
	if !res.ContentReplaced {
		t.Fatal("ContentReplaced = false, want true")
	}
	if strings.Contains(res.ReplacementContent, keyBody) || strings.Contains(res.ReplacementContent, "-----END") {
		t.Fatalf("PEM body or END marker survived redaction: %q", res.ReplacementContent)
	}
	if !strings.Contains(res.ReplacementContent, "before\n[REDACTED:private key]\nafter") {
		t.Fatalf("expected block replaced by marker with context intact: %q", res.ReplacementContent)
	}
	if !strings.Contains(res.ReplacementContent, "private key. If these are placeholders") {
		t.Fatalf("missing notice: %q", res.ReplacementContent)
	}
}

// TestSecretOutputGuardrail_UnterminatedPEMRedactedToEnd verifies that a PEM
// block without an END marker is redacted through the end of the content.
func TestSecretOutputGuardrail_UnterminatedPEMRedactedToEnd(t *testing.T) {
	const keyBody = "MIIEpAIBAAKCAQEAfakekeymaterial"
	content := "before\n-----BEGIN OPENSSH PRIVATE " + "KEY-----\n" + keyBody
	gr := BuiltinToolOutputGuardrails()[0]
	res, err := gr.Fn(nil, nil, stubTool, agentsdk.ToolResult{Content: content})
	if err != nil {
		t.Fatalf("guardrail error: %v", err)
	}
	if !res.ContentReplaced || strings.Contains(res.ReplacementContent, keyBody) {
		t.Fatalf("key body survived redaction: %+v", res)
	}
}

func TestSecretOutputGuardrail_BlocksPairedAWSCredentialBlock(t *testing.T) {
	// Built by concatenation so this test file never contains a contiguous
	// signature match itself.
	keyID := "AKIA" + "IOSFODNN7EXAMPLE"
	secretKey := "wJalrXUtnFEMI/K7MDENG/bPxRfiCY" + "EXAMPLEKEY"
	content := "AWS_ACCESS_KEY_ID=" + keyID + "\nAWS_SECRET_ACCESS_KEY=" + secretKey + "\n"
	gr := BuiltinToolOutputGuardrails()[0]
	res, err := gr.Fn(nil, nil, stubTool, agentsdk.ToolResult{Content: content})
	if err != nil {
		t.Fatalf("guardrail error: %v", err)
	}
	if !res.TripwireTriggered {
		t.Fatal("tripwire not triggered, want whole output blocked for paired credentials")
	}
	if res.ContentReplaced {
		t.Fatal("ContentReplaced = true, want hard block without partial redaction")
	}
	out, _ := res.Output.(string)
	if !strings.Contains(out, "AWS access key") {
		t.Fatalf("Output = %q, want signature name", out)
	}
	if strings.Contains(out, secretKey) || strings.Contains(out, keyID) {
		t.Fatalf("Output leaked credential material: %q", out)
	}
}

func TestSecretOutputGuardrail_BlocksGCPServiceAccountJSON(t *testing.T) {
	content := `{"type"` + `: "service_account", "project_id": "acme", "private_key_id": "abc123"}`
	gr := BuiltinToolOutputGuardrails()[0]
	res, err := gr.Fn(nil, nil, stubTool, agentsdk.ToolResult{Content: content})
	if err != nil {
		t.Fatalf("guardrail error: %v", err)
	}
	if !res.TripwireTriggered {
		t.Fatal("tripwire not triggered, want whole output blocked for service-account marker")
	}
	if res.ContentReplaced {
		t.Fatal("ContentReplaced = true, want hard block without partial redaction")
	}
	out, _ := res.Output.(string)
	if !strings.Contains(out, "GCP service-account key") {
		t.Fatalf("Output = %q, want signature name", out)
	}
}

func TestPartialSecretSignatureNamesExist(t *testing.T) {
	known := make(map[string]bool, len(secretSignatures))
	for _, sp := range secretSignatures {
		known[sp.name] = true
	}
	for name := range partialSecretSignatures {
		if !known[name] {
			t.Fatalf("partialSecretSignatures references unknown signature %q", name)
		}
	}
}
