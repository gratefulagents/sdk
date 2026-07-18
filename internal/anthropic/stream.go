package anthropic

import (
	"encoding/json"
	"errors"
	"io"
)

// CollectResponse reads all streaming events and assembles a complete CreateMessageResponse.
// This is a convenience method used by the old SSE reader path.
func (r *StreamReader) CollectResponse() (*CreateMessageResponse, error) {
	assembler := NewStreamAssembler()

	for {
		event, err := r.Next()
		if err != nil {
			// Only a clean io.EOF ends the stream successfully. A mid-stream
			// failure (e.g. io.ErrUnexpectedEOF from a dropped connection)
			// must surface as an error, not a silently truncated response.
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}

		assembler.Add(event)
	}

	return assembler.Response(), nil
}

// StreamAssembler incrementally rebuilds a CreateMessageResponse from stream
// events. Providers use it when they need to forward deltas live and still
// return a complete final ModelResponse.
type StreamAssembler struct {
	resp          CreateMessageResponse
	currentBlocks []ContentBlock
	builders      map[int]*blockAssembler
}

func NewStreamAssembler() *StreamAssembler {
	return &StreamAssembler{builders: make(map[int]*blockAssembler)}
}

func (a *StreamAssembler) Add(event StreamEvent) {
	if a == nil {
		return
	}
	if a.builders == nil {
		a.builders = make(map[int]*blockAssembler)
	}
	switch event.Type {
	case EventMessageStart:
		if event.Message != nil {
			a.resp.ID = event.Message.ID
			a.resp.Model = event.Message.Model
			a.resp.Role = event.Message.Role
			a.resp.Usage = event.Message.Usage
		}

	case EventContentBlockStart:
		if event.ContentBlock != nil {
			a.builders[event.Index] = &blockAssembler{
				blockType:        event.ContentBlock.Type,
				id:               event.ContentBlock.ID,
				name:             event.ContentBlock.Name,
				phase:            event.ContentBlock.Phase,
				signature:        event.ContentBlock.Signature,
				data:             event.ContentBlock.Data,
				content:          event.ContentBlock.Content,
				createdBy:        event.ContentBlock.CreatedBy,
				encryptedContent: event.ContentBlock.EncryptedContent,
			}
		}

	case EventContentBlockDelta:
		if b := a.builders[event.Index]; event.Delta != nil && b != nil {
			switch event.Delta.Type {
			case "text_delta":
				b.text += event.Delta.Text
			case "thinking_delta":
				b.thinking += event.Delta.Thinking
			case "signature_delta":
				b.signature += event.Delta.Signature
			case "reasoning_encrypted_content":
				b.encryptedContent = event.Delta.EncryptedContent
			case "input_json_delta":
				b.inputJSON += event.Delta.PartialJSON
			case "compaction_delta":
				b.content += event.Delta.Content
				if event.Delta.EncryptedContent != "" {
					b.encryptedContent = event.Delta.EncryptedContent
				}
			case "compaction_encrypted_content":
				b.encryptedContent = event.Delta.EncryptedContent
			}
		}

	case EventContentBlockStop:
		// A stop only applies to a still-open builder at that index; a
		// duplicated stop is ignored instead of appending the block twice.
		if b := a.builders[event.Index]; b != nil {
			a.currentBlocks = append(a.currentBlocks, b.build())
			delete(a.builders, event.Index)
		}

	case EventMessageDelta:
		if event.Delta != nil {
			if event.Delta.StopReason != "" {
				a.resp.StopReason = StopReason(event.Delta.StopReason)
			}
			if event.Delta.EndTurn != nil {
				endTurn := *event.Delta.EndTurn
				a.resp.EndTurn = &endTurn
			}
		}
		// message_delta usage is authoritative when present, but some backends
		// (and shims) only populate output_tokens there. Never let a zero field
		// clobber a non-zero value captured from message_start.
		if event.Usage != nil {
			if event.Usage.InputTokens != 0 {
				a.resp.Usage.InputTokens = event.Usage.InputTokens
			}
			if event.Usage.OutputTokens != 0 {
				a.resp.Usage.OutputTokens = event.Usage.OutputTokens
			}
			if event.Usage.CacheReadInputTokens != 0 {
				a.resp.Usage.CacheReadInputTokens = event.Usage.CacheReadInputTokens
			}
			if event.Usage.CacheCreationInputTokens != 0 {
				a.resp.Usage.CacheCreationInputTokens = event.Usage.CacheCreationInputTokens
			}
		}

	case EventMessageStop:
		// done
	}
}

func (a *StreamAssembler) Response() *CreateMessageResponse {
	if a == nil {
		return &CreateMessageResponse{}
	}
	resp := a.resp
	resp.Content = append([]ContentBlock(nil), a.currentBlocks...)
	return &resp
}

// blockAssembler accumulates streaming deltas for a single content block.
type blockAssembler struct {
	blockType        string
	id               string
	name             string
	phase            string
	text             string
	thinking         string
	signature        string
	data             string
	content          string
	createdBy        string
	encryptedContent string
	inputJSON        string
}

func (b *blockAssembler) build() ContentBlock {
	switch b.blockType {
	case "text":
		block := NewTextBlock(b.text)
		block.Phase = b.phase
		return block
	case "thinking":
		block := NewThinkingBlock(b.thinking)
		block.ID = b.id
		block.Signature = b.signature
		block.EncryptedContent = b.encryptedContent
		return block
	case "redacted_thinking":
		block := NewRedactedThinkingBlock(b.data)
		block.ID = b.id
		block.EncryptedContent = b.encryptedContent
		return block
	case "compaction":
		block := NewCompactionBlock(b.id, b.encryptedContent, b.createdBy)
		block.Content = b.content
		return block
	case "tool_use":
		var input json.RawMessage
		if b.inputJSON != "" {
			input = json.RawMessage(b.inputJSON)
		} else {
			input = json.RawMessage("{}")
		}
		return NewToolUseBlock(b.id, b.name, input)
	default:
		return NewTextBlock(b.text)
	}
}
