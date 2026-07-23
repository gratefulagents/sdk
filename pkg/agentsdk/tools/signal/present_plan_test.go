package signal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPresentPlanSchemaDoesNotExposeMode(t *testing.T) {
	schema := string((&PresentPlanTool{}).InputSchema())
	if strings.Contains(schema, `"mode"`) {
		t.Fatalf("present_plan schema unexpectedly exposes mode: %s", schema)
	}
	if !strings.Contains(schema, `"additionalProperties": false`) {
		t.Fatalf("present_plan action schema must reject unknown fields: %s", schema)
	}
}

func TestPresentPlanExecuteStripsLegacyMode(t *testing.T) {
	result, err := (&PresentPlanTool{}).Execute(context.Background(), json.RawMessage(`{
		"summary":"Ready",
		"actions":[{"id":"approve","label":"Approve","mode":"build"}]
	}`), "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() result = %#v", result)
	}
	if strings.Contains(result.Content, `"mode"`) || strings.Contains(result.Content, `"build"`) {
		t.Fatalf("Execute() preserved legacy mode: %s", result.Content)
	}
}
