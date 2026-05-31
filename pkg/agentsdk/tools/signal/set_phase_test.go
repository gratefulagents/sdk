package signal

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSetPhaseToolPausesWhenPhaseChanges(t *testing.T) {
	tool := &SetPhaseTool{
		CurrentPhase:  "scoping",
		PauseOnChange: true,
		Phases: []PhaseOption{
			{ID: "scoping", ReadOnly: true},
			{ID: "implementing"},
		},
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"phase":"implementing"}`), t.TempDir())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.IsError {
		t.Fatalf("result is error: %s", result.Content)
	}
	if !result.ShouldPause {
		t.Fatal("ShouldPause = false, want true")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("result content is not JSON: %v", err)
	}
	if payload["phase"] != "implementing" || payload["tool_access"] != "full" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestSetPhaseToolRejectsUnknownModePhase(t *testing.T) {
	tool := &SetPhaseTool{Phases: []PhaseOption{{ID: "scoping"}}}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"phase":"shipping"}`), t.TempDir())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.IsError {
		t.Fatalf("result should be an error: %+v", result)
	}
}
