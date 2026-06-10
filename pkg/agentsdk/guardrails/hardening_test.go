package guardrails

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

var bashStubTool agentsdk.Tool = &agentsdk.FunctionTool{ToolName: "Bash"}

func runDestructiveGuardrail(t *testing.T, tool agentsdk.Tool, input string) *agentsdk.GuardrailResult {
	t.Helper()
	for _, g := range BuiltinToolInputGuardrails() {
		if g.Name != "block-destructive-commands" {
			continue
		}
		res, err := g.Fn(nil, nil, tool, json.RawMessage(input))
		if err != nil {
			t.Fatalf("guardrail returned error: %v", err)
		}
		return res
	}
	t.Fatal("block-destructive-commands guardrail not found")
	return nil
}

// The destructive-command guardrail must use the tokenizer-backed classifier,
// catching variants that defeat naive substring matching.
func TestDestructiveCommandGuardrail_BlocksVariants(t *testing.T) {
	blocked := []string{
		`rm -rf /`,
		`rm -fr /`,            // reordered flags
		`rm -r -f /`,          // split flags
		`sudo rm -rf /`,       // sudo wrapper
		`env rm -rf /etc`,     // env wrapper
		`\rm -rf /`,           // backslash escape
		`"rm" -rf /`,          // quoting
		`bash -c "rm -rf /"`,  // shell -c wrapper
		`echo hi && rm -rf /`, // chaining
		`true; rm -rf /usr`,   // chaining with ;
		`mkfs.ext4 /dev/sda1`,
		`dd if=/dev/zero of=/dev/sda`,
	}
	for _, cmd := range blocked {
		payload, _ := json.Marshal(map[string]string{"command": cmd})
		res := runDestructiveGuardrail(t, bashStubTool, string(payload))
		if !res.TripwireTriggered {
			t.Errorf("command %q was not blocked", cmd)
		}
	}
}

func TestDestructiveCommandGuardrail_AllowsSafeCommands(t *testing.T) {
	allowed := []string{
		`ls -la`,
		`rm -rf ./build`,
		`go test ./...`,
		`git status`,
		`grep -r "rm -rf /" docs/`, // the phrase inside a grep pattern argument
	}
	for _, cmd := range allowed {
		payload, _ := json.Marshal(map[string]string{"command": cmd})
		res := runDestructiveGuardrail(t, bashStubTool, string(payload))
		if res.TripwireTriggered {
			t.Errorf("safe command %q was blocked: %s", cmd, res.Output)
		}
	}
}

// Malformed JSON for a shell-like tool cannot be classified and must fail
// closed instead of silently passing.
func TestDestructiveCommandGuardrail_MalformedJSONFailsClosed(t *testing.T) {
	res := runDestructiveGuardrail(t, bashStubTool, `{"command": "rm -rf /"`)
	if !res.TripwireTriggered {
		t.Fatal("malformed JSON input for shell tool must trip the guardrail")
	}
}

func TestDestructiveCommandGuardrail_IgnoresNonShellTools(t *testing.T) {
	res := runDestructiveGuardrail(t, stubTool, `{"command":"rm -rf /"}`)
	if res.TripwireTriggered {
		t.Fatal("non-shell tools must not be classified")
	}
}

// --- Rule compilation fail-closed behavior ----------------------------------

func TestToolGuardrailsFromRules_UnknownTypeIsError(t *testing.T) {
	_, _, errs := ToolGuardrailsFromRules([]agentsdk.GuardrailRule{
		{Name: "r1", Type: "tool-inptu", Regex: "x", Action: "block"},
	})
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly one unknown-type error", errs)
	}
}

func TestToolGuardrailsFromRules_UnknownActionIsError(t *testing.T) {
	_, _, errs := ToolGuardrailsFromRules([]agentsdk.GuardrailRule{
		{Name: "r1", Type: "tool-input", Regex: "x", Action: "deny"},
	})
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly one unknown-action error", errs)
	}
}

func TestToolGuardrailsFromRules_EmptyActionDefaultsToBlock(t *testing.T) {
	inputs, _, errs := ToolGuardrailsFromRules([]agentsdk.GuardrailRule{
		{Name: "r1", Type: "tool-input", Regex: "danger"},
	})
	if len(errs) != 0 {
		t.Fatalf("unexpected errs: %v", errs)
	}
	if len(inputs) != 1 {
		t.Fatalf("input guardrails = %d, want 1", len(inputs))
	}
	res, err := inputs[0].Fn(nil, nil, stubTool, json.RawMessage(`{"x":"danger"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.TripwireTriggered {
		t.Fatal("empty action must default to block (fail closed)")
	}
}

func TestMakeToolInputGuardrailFn_UnvalidatedUnknownActionBlocks(t *testing.T) {
	rule := agentsdk.GuardrailRule{Name: "r1", Type: "tool-input", Regex: "danger", Action: "deny"}
	fn := MakeToolInputGuardrailFn(rule, regexp.MustCompile(rule.Regex))
	res, err := fn(nil, nil, stubTool, json.RawMessage(`{"x":"danger"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.TripwireTriggered {
		t.Fatal("unknown action on a matched rule must fail closed at runtime")
	}
}

// --- New secret signatures ---------------------------------------------------

func TestSecretSignatures_NewPatterns(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"GitHub fine-grained PAT", "token=github_pat_11ABCDEFG0123456789_" + repeatChar('a', 60)},
		{"OpenAI API key", "OPENAI_API_KEY=sk-" + repeatChar('A', 48)},
	}
	guards := BuiltinToolOutputGuardrails()
	for _, tc := range cases {
		tripped := false
		for _, g := range guards {
			res, err := g.Fn(nil, nil, stubTool, agentsdk.ToolResult{Content: tc.input})
			if err != nil {
				t.Fatal(err)
			}
			if res.TripwireTriggered {
				tripped = true
			}
		}
		if !tripped {
			t.Errorf("%s was not detected in %q", tc.name, tc.input)
		}
	}
}

func repeatChar(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
