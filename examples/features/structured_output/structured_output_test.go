package structured_output_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liverunner"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

type decision struct {
	Answer     string  `json:"answer"`
	Confidence float64 `json:"confidence"`
	NextAction string  `json:"next_action"`
}

func TestStructuredOutputExample(t *testing.T) {
	schema := agentsdk.NewOutputSchema("decision", json.RawMessage(`{
		"type":"object",
		"properties":{
			"answer":{"type":"string"},
			"confidence":{"type":"number"},
			"next_action":{"type":"string"}
		},
		"required":["answer","confidence","next_action"],
		"additionalProperties":false
	}`))
	schema.Strict = true
	schema.ParseFn = func(raw string) (any, error) {
		var out decision
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return nil, err
		}
		return out, nil
	}

	runner, model := liverunner.Runner(t)
	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "structured",
		Model:        model,
		Instructions: "Return a JSON decision with fields answer (yes|no), confidence (0..1), next_action.",
		OutputType:   schema,
	}, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Can we ship the new feature today? Reply as JSON."},
		},
	}, agentsdk.RunConfig{MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}

	out, ok := result.FinalOutput.(decision)
	if !ok {
		t.Fatalf("FinalOutput = %T, want decision", result.FinalOutput)
	}
	if out.Answer == "" || out.NextAction == "" {
		t.Fatalf("decision missing fields: %+v", out)
	}
}
