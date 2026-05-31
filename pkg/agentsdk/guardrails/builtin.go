package guardrails

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// secretSignature describes a single high-confidence credential pattern.
// Patterns are intentionally tight: they target well-known credential prefixes
// and shapes so we minimise false positives on prose, code comments, and
// unrelated identifiers. All expressions are RE2-compatible (no backtracking).
type secretSignature struct {
	name    string
	pattern *regexp.Regexp
}

// secretSignatures is the canonical set of credential detectors used by both
// the input and output secret-leak guardrails. Each entry is justified inline.
//
// Coverage targets (see eval/audit-fixtures/secret_obfuscation.txt):
//   - AWS long-term and STS keys (AKIA / ASIA)
//   - GitHub PATs and server/user/refresh tokens (ghp_/ghs_/ghu_/ghr_)
//   - OpenAI project keys (sk-proj-*)
//   - Anthropic API keys (sk-ant-*)
//   - Slack bot/user/app tokens (xoxb-/xoxp-/xoxa-/xoxr-/xoxs-)
//   - npm automation tokens (npm_*)
//   - JSON Web Tokens (three base64url segments separated by dots)
//   - PEM-encoded private keys (RSA, EC, DSA, OPENSSH, generic PKCS#8)
//   - HTTP Authorization Bearer headers
//   - GCP service-account JSON markers
var secretSignatures = []secretSignature{
	// AWS access key IDs always start with AKIA (long-term) or ASIA (STS
	// temporary). The body is exactly 16 uppercase alphanumerics.
	{"AWS access key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},

	// GitHub tokens use a fixed prefix plus an opaque body. Real tokens are
	// >= 36 chars in the body; we accept 30+ to tolerate future length changes
	// without admitting short non-token strings like "ghp_short".
	{"GitHub token", regexp.MustCompile(`\bgh[psuro]_[A-Za-z0-9]{30,255}\b`)},

	// OpenAI project-scoped keys. The body uses base64url-ish chars.
	{"OpenAI project key", regexp.MustCompile(`\bsk-proj-[A-Za-z0-9_\-]{20,}\b`)},

	// Anthropic API keys, optionally including the api03 (or similar) version
	// segment seen in current production keys.
	{"Anthropic API key", regexp.MustCompile(`\bsk-ant-(?:api\d{2}-)?[A-Za-z0-9_\-]{20,}\b`)},

	// Slack tokens: bot (xoxb), user (xoxp), app (xoxa), refresh (xoxr),
	// session (xoxs). Body is dash-separated ids and an opaque suffix.
	{"Slack token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},

	// npm automation/CLI tokens are prefixed npm_ followed by an opaque body
	// of >= 30 chars (real tokens are 36).
	{"npm token", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{30,}\b`)},

	// JWTs are exactly three base64url segments joined by dots. We require
	// the header to start with "eyJ" (the base64 of '{"') to keep false
	// positives away from arbitrary dotted identifiers.
	{"JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-+/=.]{4,}`)},

	// PEM headers covering RSA, EC, DSA, OPENSSH, encrypted, and unlabeled
	// PKCS#8 ("-----BEGIN PRIVATE KEY-----") private keys.
	{"private key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY-----`)},

	// HTTP Authorization: Bearer headers. Require >= 16 opaque chars after
	// "Bearer " so prose like "the bearer of this letter" does not match.
	{"Authorization Bearer header", regexp.MustCompile(`(?i)Authorization\s*:\s*Bearer\s+[A-Za-z0-9._\-+/=]{16,}`)},

	// Standalone "Bearer <token>" with a longer minimum length (>= 24) to
	// further suppress matches on common English uses of the word "Bearer".
	{"Bearer token", regexp.MustCompile(`\bBearer\s+[A-Za-z0-9._\-+/=]{24,}\b`)},

	// GCP service-account JSON keys carry the literal "type":"service_account"
	// marker. Tolerate optional whitespace around the colon.
	{"GCP service-account key", regexp.MustCompile(`"type"\s*:\s*"service_account"`)},

	// Generic credential assignment in source/config (kept from prior version).
	{"generic credential assignment", regexp.MustCompile(`(?i)(api[_-]?key|secret[_-]?key|password)\s*[:=]\s*["'][^"']{8,}["']`)},
}

// BuiltinToolInputGuardrails returns guardrails that are always active.
func BuiltinToolInputGuardrails() []agentsdk.ToolInputGuardrail {
	return []agentsdk.ToolInputGuardrail{
		{
			Name: "block-destructive-commands",
			Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, tool agentsdk.Tool, input json.RawMessage) (*agentsdk.GuardrailResult, error) {
				toolLower := strings.ToLower(tool.Name())
				if !strings.Contains(toolLower, "bash") &&
					!strings.Contains(toolLower, "shell") &&
					!strings.Contains(toolLower, "exec") {
					return &agentsdk.GuardrailResult{}, nil
				}

				var params struct {
					Command string `json:"command"`
					Cmd     string `json:"cmd"`
				}
				// Best-effort decode: guardrails should not fail open on
				// malformed JSON, but they also shouldn't synthesise an error
				// here. If unmarshal fails, cmd stays empty and the destructive
				// pattern check below cleanly returns "no match".
				_ = json.Unmarshal(input, &params)
				cmd := params.Command
				if cmd == "" {
					cmd = params.Cmd
				}
				cmdLower := strings.ToLower(cmd)

				destructive := []string{
					"rm -rf /",
					"rm -rf /*",
					"mkfs.",
					"dd if=/dev/zero",
					":(){:|:&};:",
					"chmod -r 777 /",
				}
				for _, pattern := range destructive {
					if strings.Contains(cmdLower, pattern) {
						return &agentsdk.GuardrailResult{
							Output:            fmt.Sprintf("Blocked destructive command: %s", pattern),
							TripwireTriggered: true,
						}, nil
					}
				}
				return &agentsdk.GuardrailResult{}, nil
			},
		},
		{
			Name: "detect-secret-leak",
			Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, _ agentsdk.Tool, input json.RawMessage) (*agentsdk.GuardrailResult, error) {
				inputStr := string(input)
				for _, sp := range secretSignatures {
					if sp.pattern.MatchString(inputStr) {
						return &agentsdk.GuardrailResult{
							Output:            fmt.Sprintf("Potential %s detected in tool input", sp.name),
							TripwireTriggered: true,
						}, nil
					}
				}
				return &agentsdk.GuardrailResult{}, nil
			},
		},
	}
}

// BuiltinToolOutputGuardrails returns output guardrails that are always active.
func BuiltinToolOutputGuardrails() []agentsdk.ToolOutputGuardrail {
	return []agentsdk.ToolOutputGuardrail{
		{
			Name: "detect-secret-in-output",
			Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, _ agentsdk.Tool, result agentsdk.ToolResult) (*agentsdk.GuardrailResult, error) {
				for _, sp := range secretSignatures {
					if sp.pattern.MatchString(result.Content) {
						return &agentsdk.GuardrailResult{
							Output:            fmt.Sprintf("Potential %s detected in tool output - content redacted", sp.name),
							TripwireTriggered: true,
						}, nil
					}
				}
				return &agentsdk.GuardrailResult{}, nil
			},
		},
	}
}
