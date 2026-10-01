package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func integrationImageHistory() []RunItem {
	return []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "original", Images: []ImageAttachment{{Data: "old-user"}, {Data: "recent-user"}}}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "prior", Name: "image", Input: json.RawMessage(`{}`)}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "prior", Content: "screenshot", Images: []ImageAttachment{{Data: "tool-1"}, {Data: "tool-2"}}}},
	}
}

func integrationImageData(items []RunItem) []string {
	var data []string
	for _, item := range items {
		if item.Message != nil {
			for _, image := range item.Message.Images {
				data = append(data, image.Data)
			}
		}
		if item.ToolOutput != nil {
			for _, image := range item.ToolOutput.Images {
				data = append(data, image.Data)
			}
		}
	}
	return data
}

func TestRunnerImagesRemainLiveButNotPersisted(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocking", true: "streamed"}[streamed], func(t *testing.T) {
			input := integrationImageHistory()
			model := &mockModel{responses: []*ModelResponse{
				{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "next", Name: "image", Input: json.RawMessage(`{}`)}}}},
				{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "seen"}}}},
			}}
			traces := &spanRecorder{}
			var checkpoints []DurableCheckpoint
			cfg := RunConfig{TracingProcessor: traces, MaxRetainedToolImages: 1, Durable: &DurableRunConfig{
				Checkpoint: func(_ context.Context, cp DurableCheckpoint) error {
					checkpoints = append(checkpoints, cp)
					return nil
				},
			}}
			agent := &Agent{Name: "viewer", Tools: []Tool{&imageResultTool{FunctionTool: FunctionTool{ToolName: "image", ReadOnly: true}}}}
			runner := NewRunnerWithModel(model)
			var result *RunResult
			var err error
			if streamed {
				stream := runner.RunStreamed(context.Background(), agent, input, cfg)
				for range stream.Events {
				}
				result, err = stream.FinalResult(), stream.Err()
			} else {
				result, err = runner.Run(context.Background(), agent, input, cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(model.requests) != 2 {
				t.Fatalf("model requests = %d", len(model.requests))
			}
			for i, want := range [][]string{{"recent-user", "tool-1", "tool-2"}, {"tool-1", "tool-2", "cG5n"}} {
				if got := integrationImageData(model.requests[i].Input); !reflect.DeepEqual(got, want) {
					t.Fatalf("request %d images = %v, want %v", i, got, want)
				}
			}
			if len(checkpoints) == 0 {
				t.Fatal("no checkpoints")
			}
			for _, cp := range checkpoints {
				assertIntegrationSnapshotsOmitImages(t, cp.History)
			}
			generations := traces.generations()
			if len(generations) != 2 {
				t.Fatalf("generation spans = %d", len(generations))
			}
			for _, generation := range generations {
				assertIntegrationSnapshotsOmitImages(t, generation.Request.InputItems)
			}
			if got := integrationImageData(input); len(got) != 4 {
				t.Fatalf("caller input mutated: %v", got)
			}
			if got := integrationImageData(result.FinalHistory); !reflect.DeepEqual(got, []string{"tool-1", "tool-2", "cG5n"}) {
				t.Fatalf("live final history stripped: %v", got)
			}
		})
	}
}

func assertIntegrationSnapshotsOmitImages(t *testing.T, snapshots []LLMRunItemSnapshot) {
	t.Helper()
	for _, item := range snapshots {
		if len(item.MessageImages) != 0 || item.ToolOutput != nil && len(item.ToolOutput.Images) != 0 {
			t.Fatalf("persisted image bytes: %+v", item)
		}
	}
}

func TestImageSnapshotsUsePlaceholdersWithoutMutatingHistory(t *testing.T) {
	input := integrationImageHistory()
	for name, snapshots := range map[string][]LLMRunItemSnapshot{
		"history":  SnapshotRunItems(input),
		"request":  BuildLLMRequestSnapshot("viewer", ModelRequest{Input: input}).InputItems,
		"response": BuildLLMResponseSnapshot(&ModelResponse{Items: input}).Items,
	} {
		t.Run(name, func(t *testing.T) {
			assertIntegrationSnapshotsOmitImages(t, snapshots)
			if snapshots[0].MessageText != "original\n[image omitted]" || snapshots[2].ToolOutput.Content != "screenshot\n[image omitted]" {
				t.Fatalf("missing placeholders: %+v", snapshots)
			}
			snapshots[2].ToolOutput.Content = "changed snapshot"
		})
	}
	if input[0].Message.Text != "original" || input[2].ToolOutput.Content != "screenshot" || len(integrationImageData(input)) != 4 {
		t.Fatalf("live history mutated: %+v", input)
	}
}

type integrationImageCompactor struct {
	mockModel
	compactRequests []ModelRequest
}

func (*integrationImageCompactor) SupportsContextCompaction() bool { return true }
func (m *integrationImageCompactor) CompactContext(_ context.Context, req ModelRequest) (*CompactionResult, error) {
	m.compactRequests = append(m.compactRequests, req)
	return &CompactionResult{Items: req.Input, Summary: "compacted", Usage: Usage{InputTokens: 7}}, nil
}

func TestRunnerElidesImagesBeforeProactiveAndForcedCompaction(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "proactive", true: "forced"}[forced], func(t *testing.T) {
			model := &integrationImageCompactor{mockModel: mockModel{responses: []*ModelResponse{{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}}}}}
			trigger := 1
			if forced {
				trigger = 1000000
				model.responses = append([]*ModelResponse{nil}, model.responses...)
				model.errors = []error{errors.New("context_length_exceeded")}
			}
			recorded := 0
			result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "viewer"}, integrationImageHistory(), RunConfig{
				MaxTurns:         1,
				CompactionConfig: CompactionConfig{Enabled: true, TriggerTokens: trigger},
				CompactionRecorder: func(before, after int, summary string) {
					recorded++
					if before < requestSafetyBufferTokens || after < requestSafetyBufferTokens || !strings.Contains(summary, "compacted") {
						t.Errorf("compaction totals/summary = %d, %d, %q", before, after, summary)
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(model.compactRequests) != 1 || recorded != 1 || result.Usage.InputTokens != 7 {
				t.Fatalf("compactions=%d recorded=%d usage=%+v", len(model.compactRequests), recorded, result.Usage)
			}
			for _, req := range append(model.compactRequests, model.requests...) {
				if got := integrationImageData(req.Input); !reflect.DeepEqual(got, []string{"recent-user", "tool-1", "tool-2"}) {
					t.Fatalf("request images = %v", got)
				}
			}
		})
	}
}

func TestModelRetryDelayPreservesPolicyAndAdvice(t *testing.T) {
	policy := &RetryPolicy{MaxRetries: 2, Backoff: RetryBackoffSettings{InitialDelayMS: 10, Multiplier: 2}}
	for _, tt := range []struct {
		name    string
		policy  *RetryPolicy
		advice  *ModelRetryAdvice
		attempt int
		want    time.Duration
		retry   bool
	}{
		{"none", nil, nil, 1, 0, false},
		{"policy", policy, nil, 2, 20 * time.Millisecond, true},
		{"exhausted", policy, nil, 3, 0, false},
		{"provider veto", policy, &ModelRetryAdvice{Reason: "permanent"}, 1, 0, false},
		{"provider minimum", policy, &ModelRetryAdvice{ShouldRetry: true, RetryAfterMS: 40}, 1, 40 * time.Millisecond, true},
		{"advice beyond policy", policy, &ModelRetryAdvice{ShouldRetry: true}, 3, 40 * time.Millisecond, true},
		{"advice default floor", nil, &ModelRetryAdvice{ShouldRetry: true}, 1, time.Second, true},
		{"advice bound", nil, &ModelRetryAdvice{ShouldRetry: true}, maxAdviceRetriesPerTurn + 1, 0, false},
		{"explicit immediate policy", &RetryPolicy{MaxRetries: 1}, nil, 1, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, retry := modelRetryDelay(tt.policy, tt.advice, tt.attempt)
			if got != tt.want || retry != tt.retry {
				t.Fatalf("delay/retry = %v/%v, want %v/%v", got, retry, tt.want, tt.retry)
			}
		})
	}
}

func TestCommittedModelOutputDisablesContextRecovery(t *testing.T) {
	for _, cause := range []string{"context_length_exceeded", "invalid_encrypted_content"} {
		t.Run(cause, func(t *testing.T) {
			model := &integrationImageCompactor{mockModel: mockModel{errors: []error{&streamOutputCommittedError{cause: errors.New(cause)}}}}
			input := []RunItem{{Type: RunItemCompaction, Compaction: &CompactionData{EncryptedContent: "opaque"}}}
			_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "test"}, input, RunConfig{MaxTurns: 1, CompactionConfig: CompactionConfig{Enabled: true, TriggerTokens: 1000000}})
			if err == nil || model.callIdx != 1 || len(model.compactRequests) != 0 {
				t.Fatalf("err=%v calls=%d compactions=%d", err, model.callIdx, len(model.compactRequests))
			}
		})
	}
}

func TestRunnerResultBuilderRetainsInputGuardrailsAtToolBoundaries(t *testing.T) {
	for _, boundary := range []string{"approval", "pause", "stop"} {
		t.Run(boundary, func(t *testing.T) {
			tool := &FunctionTool{ToolName: "act", ReadOnly: true, Approval: boundary == "approval", Fn: func(context.Context, json.RawMessage) (string, error) { return "done", nil }}
			if boundary == "pause" {
				tool.ToolName = "AskUserQuestion"
			}
			agent := &Agent{Name: "test", Tools: []Tool{tool}, InputGuardrails: []InputGuardrail{{Name: "input-check", Fn: func(*RunContext, *Agent, []RunItem) (*GuardrailResult, error) {
				return &GuardrailResult{Output: "checked"}, nil
			}}}}
			if boundary == "stop" {
				agent.ToolUseBehavior = StopOnFirstTool
			}
			model := &mockModel{responses: []*ModelResponse{{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "call", Name: tool.Name(), Input: json.RawMessage(`{}`)}}}}}}
			result, err := NewRunnerWithModel(model).Run(context.Background(), agent, []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "request"}}}, RunConfig{})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.InputGuardrailResults) != 1 || result.InputGuardrailResults[0].Output != "checked" {
				t.Fatalf("input guardrail results missing: %+v", result.InputGuardrailResults)
			}
			if len(result.FinalHistory) != 3 || len(result.NewItems) != 2 || len(result.RawResponses) != 1 || result.LastAgent != agent {
				t.Fatalf("incomplete result: %+v", result)
			}
			if boundary == "approval" && (len(result.Interruptions) != 1 || result.Interruption != result.Interruptions[0]) {
				t.Fatalf("approval result: %+v", result)
			}
		})
	}
}

func TestRunnerFinalizerFoldsCandidateAndFeedbackOnce(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{finalResponse("candidate"), finalResponse("done")}}
	finalizations := 0
	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "test"}, nil, RunConfig{
		MaxTurns: 1,
		ImmediateInputFinalizer: func(context.Context) ([]RunItem, error) {
			finalizations++
			if finalizations == 1 {
				return []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "steering"}}}, nil
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if finalizations != 2 || len(model.requests) != 2 {
		t.Fatalf("finalizations=%d requests=%d", finalizations, len(model.requests))
	}
	for name, items := range map[string][]RunItem{"history": result.FinalHistory, "new": result.NewItems} {
		var texts []string
		for _, item := range items {
			texts = append(texts, item.Message.Text)
		}
		if !reflect.DeepEqual(texts, []string{"candidate", "steering", "done"}) {
			t.Fatalf("%s order = %v", name, texts)
		}
	}
	if len(model.requests[1].Input) != 2 || model.requests[1].Input[0].Message.Text != "candidate" || model.requests[1].Input[1].Message.Text != "steering" {
		t.Fatalf("replacement request = %+v", model.requests[1].Input)
	}
}
