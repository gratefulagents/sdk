package agent

import (
	"strings"
	"testing"
)

func TestToolGuardrailTripwireErrorsIncludeOutputDetail(t *testing.T) {
	out := &ToolOutputGuardrailTripwireTriggered{
		GuardrailName: "detect-secret-in-output",
		ToolName:      "read_file",
		Result:        GuardrailResult{Output: "Potential AWS access key detected"},
	}
	if got := out.Error(); !strings.Contains(got, "read_file") || !strings.Contains(got, "Potential AWS access key detected") {
		t.Fatalf("output tripwire Error() = %q, want tool name and detail", got)
	}

	in := &ToolInputGuardrailTripwireTriggered{
		GuardrailName: "block-destructive-commands",
		ToolName:      "Bash",
		Result:        GuardrailResult{Output: "Blocked destructive command: recursive removal"},
	}
	if got := in.Error(); !strings.Contains(got, "Blocked destructive command: recursive removal") {
		t.Fatalf("input tripwire Error() = %q, want detail", got)
	}

	bare := &ToolOutputGuardrailTripwireTriggered{GuardrailName: "g", ToolName: "t"}
	if got, want := bare.Error(), `tool output guardrail "g" tripwire triggered for tool "t"`; got != want {
		t.Fatalf("bare Error() = %q, want %q", got, want)
	}
}
