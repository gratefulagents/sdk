package anthropic

import (
	"encoding/json"
	"testing"
)

// A tool whose schema has no properties (e.g. `{"type":"object"}`) must still
// serialize an input_schema object. The anthropic-sdk-go BetaToolParam tags
// input_schema with omitzero, so a zero-value BetaToolInputSchemaParam is
// dropped from the wire request and the API rejects it with
// "tools.N.custom.input_schema: Field required".
func TestToSDKParamsAlwaysSendsToolInputSchema(t *testing.T) {
	req := &CreateMessageRequest{
		Model:     "claude-sonnet-5",
		MaxTokens: 100,
		Messages:  []Message{{Role: RoleUser, Content: []ContentBlock{NewTextBlock("hi")}}},
		Tools: []ToolDefinition{
			{Name: "get_fleet_runs", Description: "list runs", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "no_schema", Description: "degenerate"},
			{Name: "with_props", Description: "normal", InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)},
		},
	}
	params, _ := toSDKParams(req)
	raw, err := params.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 3 {
		t.Fatalf("tools = %d, want 3: %s", len(body.Tools), raw)
	}
	for i, tool := range body.Tools {
		schema, ok := tool["input_schema"].(map[string]any)
		if !ok {
			t.Fatalf("tools.%d (%v) input_schema missing or not an object: %s", i, tool["name"], raw)
		}
		if schema["type"] != "object" {
			t.Fatalf("tools.%d input_schema.type = %v, want object", i, schema["type"])
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Fatalf("tools.%d input_schema.properties missing: %s", i, raw)
		}
	}
	if req, _ := body.Tools[2]["input_schema"].(map[string]any)["required"].([]any); len(req) != 1 {
		t.Fatalf("tools.2 required lost: %s", raw)
	}
}
