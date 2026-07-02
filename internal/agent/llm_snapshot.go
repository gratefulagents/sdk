package agent

import "encoding/json"

// LLMRequestSnapshot is the serializable, analysis-oriented form of a model
// request. It intentionally excludes executable callbacks while preserving the
// text, tool schemas, settings, and structured input that shaped the call.
type LLMRequestSnapshot struct {
	AgentName                    string               `json:"agent_name,omitempty"`
	Model                        string               `json:"model,omitempty"`
	Instructions                 string               `json:"instructions,omitempty"`
	InputItems                   []LLMRunItemSnapshot `json:"input_items,omitempty"`
	Tools                        []LLMToolSnapshot    `json:"tools,omitempty"`
	Settings                     ModelSettings        `json:"settings,omitempty"`
	OutputSchema                 *LLMOutputSchema     `json:"output_schema,omitempty"`
	InputTokenEstimate           int                  `json:"input_token_estimate,omitempty"`
	RequestOverheadTokenEstimate int                  `json:"request_overhead_token_estimate,omitempty"`
	TotalTokenEstimate           int                  `json:"total_token_estimate,omitempty"`
}

// LLMResponseSnapshot is the serializable, analysis-oriented form of a model
// response, including explicit provider-returned reasoning/thinking items when
// the provider exposes them.
type LLMResponseSnapshot struct {
	Items          []LLMRunItemSnapshot `json:"items,omitempty"`
	Usage          Usage                `json:"usage,omitempty"`
	Texts          []string             `json:"texts,omitempty"`
	Reasoning      []LLMReasoning       `json:"reasoning,omitempty"`
	ReasoningTexts []string             `json:"reasoning_texts,omitempty"`
	ThinkingTexts  []string             `json:"thinking_texts,omitempty"`
	ToolCalls      []LLMToolCall        `json:"tool_calls,omitempty"`
	Raw            json.RawMessage      `json:"raw,omitempty"`
	RawAvailable   bool                 `json:"raw_available"`
	RawError       string               `json:"raw_error,omitempty"`
}

// LLMRunItemSnapshot is a compact, non-recursive representation of a RunItem.
type LLMRunItemSnapshot struct {
	Type          string             `json:"type"`
	AgentName     string             `json:"agent_name,omitempty"`
	MessageText   string             `json:"message_text,omitempty"`
	ToolCall      *LLMToolCall       `json:"tool_call,omitempty"`
	ToolOutput    *ToolOutputData    `json:"tool_output,omitempty"`
	HandoffCall   *HandoffCallData   `json:"handoff_call,omitempty"`
	HandoffOutput *HandoffOutputData `json:"handoff_output,omitempty"`
	Reasoning     *LLMReasoning      `json:"reasoning,omitempty"`
	ReasoningText string             `json:"reasoning_text,omitempty"`
	ThinkingText  string             `json:"thinking_text,omitempty"`
	Compaction    *LLMCompaction     `json:"compaction,omitempty"`
	ToolApproval  *LLMToolApproval   `json:"tool_approval,omitempty"`
}

type LLMToolSnapshot struct {
	Name           string          `json:"name"`
	Description    string          `json:"description,omitempty"`
	InputSchema    json.RawMessage `json:"input_schema,omitempty"`
	ReadOnly       bool            `json:"read_only"`
	NeedsApproval  bool            `json:"needs_approval"`
	TimeoutSeconds int             `json:"timeout_seconds,omitempty"`
}

type LLMOutputSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict bool            `json:"strict"`
}

type LLMToolCall struct {
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type LLMReasoning struct {
	ID               string `json:"id,omitempty"`
	Text             string `json:"text,omitempty"`
	Thinking         string `json:"thinking,omitempty"`
	Signature        string `json:"signature,omitempty"`
	RedactedData     string `json:"redacted_data,omitempty"`
	EncryptedContent string `json:"encrypted_content,omitempty"`
}

type LLMCompaction struct {
	ID               string `json:"id,omitempty"`
	Content          string `json:"content,omitempty"`
	EncryptedContent string `json:"encrypted_content,omitempty"`
	CreatedBy        string `json:"created_by,omitempty"`
}

type LLMToolApproval struct {
	ToolName string          `json:"tool_name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	CallID   string          `json:"call_id,omitempty"`
	Approved bool            `json:"approved"`
}

// BuildLLMRequestSnapshot captures the full provider-agnostic request envelope
// sent to the model.
func BuildLLMRequestSnapshot(agentName string, req ModelRequest) *LLMRequestSnapshot {
	overhead := estimateModelRequestOverheadTokens(req.Instructions, req.Tools, req.Settings)
	inputEstimate := estimateRunItemsTokens(req.Input)
	snap := &LLMRequestSnapshot{
		AgentName:                    agentName,
		Model:                        req.Model,
		Instructions:                 req.Instructions,
		InputItems:                   SnapshotRunItems(req.Input),
		Tools:                        SnapshotTools(req.Tools),
		Settings:                     req.Settings,
		InputTokenEstimate:           inputEstimate,
		RequestOverheadTokenEstimate: overhead,
		TotalTokenEstimate:           inputEstimate + overhead,
	}
	if req.OutputSchema != nil {
		snap.OutputSchema = &LLMOutputSchema{
			Name:   req.OutputSchema.Name,
			Schema: snapshotRawMessage(req.OutputSchema.Schema),
			Strict: req.OutputSchema.Strict,
		}
	}
	return snap
}

// BuildLLMResponseSnapshot captures the full provider-agnostic response
// envelope returned by the model.
func BuildLLMResponseSnapshot(resp *ModelResponse) *LLMResponseSnapshot {
	if resp == nil {
		return nil
	}
	snap := &LLMResponseSnapshot{
		Items: SnapshotRunItems(resp.Items),
		Usage: resp.Usage,
	}
	for _, item := range resp.Items {
		switch item.Type {
		case RunItemMessage:
			if item.Message != nil && item.Message.Text != "" {
				snap.Texts = append(snap.Texts, item.Message.Text)
			}
		case RunItemReasoning:
			if item.Reasoning != nil {
				reasoning := snapshotReasoning(*item.Reasoning)
				snap.Reasoning = append(snap.Reasoning, reasoning)
				if item.Reasoning.Text != "" {
					snap.ReasoningTexts = append(snap.ReasoningTexts, item.Reasoning.Text)
					snap.ThinkingTexts = append(snap.ThinkingTexts, item.Reasoning.Text)
				}
			}
		case RunItemToolCall:
			if item.ToolCall != nil {
				snap.ToolCalls = append(snap.ToolCalls, snapshotToolCall(*item.ToolCall))
			}
		}
	}
	if resp.Raw != nil {
		raw, err := json.Marshal(resp.Raw)
		if err != nil {
			snap.RawError = err.Error()
		} else {
			snap.Raw = raw
			snap.RawAvailable = true
		}
	}
	return snap
}

func SnapshotTools(tools []Tool) []LLMToolSnapshot {
	if len(tools) == 0 {
		return nil
	}
	out := make([]LLMToolSnapshot, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		out = append(out, LLMToolSnapshot{
			Name:           tool.Name(),
			Description:    tool.Description(),
			InputSchema:    snapshotRawMessage(tool.InputSchema()),
			ReadOnly:       tool.IsReadOnly(),
			NeedsApproval:  tool.NeedsApproval(),
			TimeoutSeconds: tool.TimeoutSeconds(),
		})
	}
	return out
}

func SnapshotRunItems(items []RunItem) []LLMRunItemSnapshot {
	if len(items) == 0 {
		return nil
	}
	out := make([]LLMRunItemSnapshot, 0, len(items))
	for _, item := range items {
		snap := LLMRunItemSnapshot{Type: runItemTypeName(item.Type)}
		if item.Agent != nil {
			snap.AgentName = item.Agent.Name
		}
		switch item.Type {
		case RunItemMessage:
			if item.Message != nil {
				snap.MessageText = item.Message.Text
			}
		case RunItemToolCall:
			if item.ToolCall != nil {
				tc := snapshotToolCall(*item.ToolCall)
				snap.ToolCall = &tc
			}
		case RunItemToolOutput:
			if item.ToolOutput != nil {
				to := *item.ToolOutput
				snap.ToolOutput = &to
			}
		case RunItemHandoffCall:
			if item.HandoffCall != nil {
				hc := *item.HandoffCall
				snap.HandoffCall = &hc
			}
		case RunItemHandoffOutput:
			if item.HandoffOutput != nil {
				ho := *item.HandoffOutput
				snap.HandoffOutput = &ho
			}
		case RunItemReasoning:
			if item.Reasoning != nil {
				reasoning := snapshotReasoning(*item.Reasoning)
				snap.Reasoning = &reasoning
				snap.ReasoningText = item.Reasoning.Text
				snap.ThinkingText = item.Reasoning.Text
			}
		case RunItemCompaction:
			if item.Compaction != nil {
				compaction := snapshotCompaction(*item.Compaction)
				snap.Compaction = &compaction
			}
		case RunItemToolApproval:
			if item.ToolApproval != nil {
				snap.ToolApproval = &LLMToolApproval{
					ToolName: item.ToolApproval.ToolName,
					Input:    snapshotRawMessage(item.ToolApproval.Input),
					CallID:   item.ToolApproval.CallID,
					Approved: item.ToolApproval.Approved,
				}
			}
		}
		out = append(out, snap)
	}
	return out
}

func snapshotReasoning(reasoning ReasoningData) LLMReasoning {
	return LLMReasoning{
		ID:               reasoning.ID,
		Text:             reasoning.Text,
		Thinking:         reasoning.Text,
		Signature:        reasoning.Signature,
		RedactedData:     reasoning.RedactedData,
		EncryptedContent: reasoning.EncryptedContent,
	}
}

func snapshotCompaction(compaction CompactionData) LLMCompaction {
	return LLMCompaction(compaction)
}

func snapshotToolCall(call ToolCallData) LLMToolCall {
	return LLMToolCall{
		ID:    call.ID,
		Name:  call.Name,
		Input: snapshotRawMessage(call.Input),
	}
}

func snapshotRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	copyRaw := append(json.RawMessage(nil), raw...)
	if json.Valid(copyRaw) {
		return copyRaw
	}
	encoded, err := json.Marshal(string(copyRaw))
	if err != nil {
		return nil
	}
	return encoded
}

func runItemTypeName(t RunItemType) string {
	switch t {
	case RunItemMessage:
		return "message"
	case RunItemToolCall:
		return "tool_call"
	case RunItemToolOutput:
		return "tool_output"
	case RunItemHandoffCall:
		return "handoff_call"
	case RunItemHandoffOutput:
		return "handoff_output"
	case RunItemReasoning:
		return "reasoning"
	case RunItemToolApproval:
		return "tool_approval"
	case RunItemCompaction:
		return "compaction"
	default:
		return "unknown"
	}
}
