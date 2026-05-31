package agent

import (
	"encoding/json"
)

func summarizeProviderCompactionOutput(id string, items []RunItem) string {
	type outputItem struct {
		Type             string `json:"type"`
		Role             string `json:"role,omitempty"`
		ID               string `json:"id,omitempty"`
		Text             string `json:"text,omitempty"`
		EncryptedContent string `json:"encrypted_content,omitempty"`
		CreatedBy        string `json:"created_by,omitempty"`
	}
	out := struct {
		Type  string       `json:"type"`
		ID    string       `json:"id,omitempty"`
		Items []outputItem `json:"items"`
	}{
		Type: "openai_responses_compaction",
		ID:   id,
	}
	for _, item := range items {
		switch item.Type {
		case RunItemMessage:
			if item.Message == nil {
				continue
			}
			role := "assistant"
			if item.Agent == nil {
				role = "user"
			}
			out.Items = append(out.Items, outputItem{
				Type: "message",
				Role: role,
				Text: item.Message.Text,
			})
		case RunItemCompaction:
			if item.Compaction == nil {
				continue
			}
			out.Items = append(out.Items, outputItem{
				Type:             "compaction",
				ID:               item.Compaction.ID,
				EncryptedContent: item.Compaction.EncryptedContent,
				CreatedBy:        item.Compaction.CreatedBy,
			})
		case RunItemToolCall:
			if item.ToolCall != nil {
				out.Items = append(out.Items, outputItem{Type: "tool_call", ID: item.ToolCall.ID, Text: item.ToolCall.Name})
			}
		case RunItemToolOutput:
			if item.ToolOutput != nil {
				out.Items = append(out.Items, outputItem{Type: "tool_output", ID: item.ToolOutput.CallID, Text: item.ToolOutput.Content})
			}
		case RunItemReasoning:
			if item.Reasoning != nil {
				out.Items = append(out.Items, outputItem{
					Type:             "reasoning",
					ID:               item.Reasoning.ID,
					Text:             item.Reasoning.Text,
					EncryptedContent: item.Reasoning.EncryptedContent,
				})
			}
		}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "[OPENAI RESPONSES COMPACTION]"
	}
	return "[OPENAI RESPONSES COMPACTION]\n" + string(data)
}
