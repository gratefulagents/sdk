package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildLLMRequestSnapshotCapturesModelEnvelope(t *testing.T) {
	temp := 0.2
	tool := &FunctionTool{
		ToolName:        "Search",
		ToolDescription: "searches docs",
		Schema:          json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		ReadOnly:        true,
		Approval:        true,
		Timeout:         12,
	}
	req := ModelRequest{
		Model:        "gpt-5.5",
		Instructions: "system instructions",
		Input: []RunItem{
			{Type: RunItemMessage, Message: &MessageOutput{Text: "user asks"}},
			{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "call_1", Content: "result"}},
		},
		Tools: []Tool{tool},
		Settings: ModelSettings{
			Temperature:     &temp,
			MaxTokens:       2048,
			ReasoningEffort: "high",
		},
		OutputSchema: NewOutputSchema("answer", json.RawMessage(`{"type":"object"}`)),
	}

	snap := BuildLLMRequestSnapshot("agent", req)

	if snap.AgentName != "agent" || snap.Model != "gpt-5.5" {
		t.Fatalf("snapshot identity = %q/%q", snap.AgentName, snap.Model)
	}
	if snap.Instructions != "system instructions" {
		t.Fatalf("instructions = %q", snap.Instructions)
	}
	if len(snap.InputItems) != 2 || snap.InputItems[0].MessageText != "user asks" {
		t.Fatalf("input items not captured: %+v", snap.InputItems)
	}
	if len(snap.Tools) != 1 || snap.Tools[0].Name != "Search" || !snap.Tools[0].ReadOnly || !snap.Tools[0].NeedsApproval {
		t.Fatalf("tool not captured: %+v", snap.Tools)
	}
	if !strings.Contains(string(snap.Tools[0].InputSchema), `"properties"`) {
		t.Fatalf("tool schema not captured: %s", snap.Tools[0].InputSchema)
	}
	if snap.OutputSchema == nil || snap.OutputSchema.Name != "answer" {
		t.Fatalf("output schema not captured: %+v", snap.OutputSchema)
	}
	if snap.TotalTokenEstimate == 0 || snap.TotalTokenEstimate < snap.InputTokenEstimate {
		t.Fatalf("token estimates not populated: %+v", snap)
	}
}

func TestBuildLLMResponseSnapshotCapturesReasoningAndRaw(t *testing.T) {
	resp := &ModelResponse{
		Items: []RunItem{
			{Type: RunItemReasoning, Reasoning: &ReasoningData{
				ID:               "rs_1",
				Text:             "provider-visible thinking",
				Signature:        "sig_1",
				EncryptedContent: "encrypted_1",
			}},
			{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "call_1", Name: "Read", Input: json.RawMessage(`{"path":"README.md"}`)}},
			{Type: RunItemMessage, Message: &MessageOutput{Text: "final answer"}},
		},
		Usage: Usage{InputTokens: 10, OutputTokens: 5},
		Raw: map[string]any{
			"id":     "resp_1",
			"output": []any{"raw content"},
		},
	}

	snap := BuildLLMResponseSnapshot(resp)

	if len(snap.Items) != 3 {
		t.Fatalf("items len = %d", len(snap.Items))
	}
	if len(snap.ReasoningTexts) != 1 || snap.ReasoningTexts[0] != "provider-visible thinking" {
		t.Fatalf("reasoning texts = %+v", snap.ReasoningTexts)
	}
	if len(snap.ThinkingTexts) != 1 || snap.ThinkingTexts[0] != "provider-visible thinking" {
		t.Fatalf("thinking texts = %+v", snap.ThinkingTexts)
	}
	if len(snap.Reasoning) != 1 || snap.Reasoning[0].Signature != "sig_1" || snap.Reasoning[0].EncryptedContent != "encrypted_1" {
		t.Fatalf("reasoning details = %+v", snap.Reasoning)
	}
	if len(snap.Texts) != 1 || snap.Texts[0] != "final answer" {
		t.Fatalf("texts = %+v", snap.Texts)
	}
	if len(snap.ToolCalls) != 1 || snap.ToolCalls[0].Name != "Read" {
		t.Fatalf("tool calls = %+v", snap.ToolCalls)
	}
	if !snap.RawAvailable || !strings.Contains(string(snap.Raw), `"resp_1"`) {
		t.Fatalf("raw response not captured: available=%v raw=%s err=%q", snap.RawAvailable, snap.Raw, snap.RawError)
	}
}
