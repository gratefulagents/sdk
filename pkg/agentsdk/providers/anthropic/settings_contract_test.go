package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	internalanthropic "github.com/gratefulagents/sdk/internal/anthropic"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestPublicSettingsHTTPContract(t *testing.T) {
	zero, one, topP := 0.0, 1.0, 0.95
	parallel, serial := true, false
	for _, tc := range []struct {
		name     string
		settings agentsdk.ModelSettings
		want     map[string]any
	}{
		{"defaults", agentsdk.ModelSettings{}, nil},
		{"temperature zero", agentsdk.ModelSettings{Temperature: &zero}, map[string]any{"temperature": float64(0)}},
		{"top p zero", agentsdk.ModelSettings{TopP: &zero}, map[string]any{"top_p": float64(0)}},
		{"sampling and stops", agentsdk.ModelSettings{Temperature: &one, TopP: &topP, StopSequences: []string{"STOP", "\nEND"}}, map[string]any{"temperature": one, "top_p": topP, "stop_sequences": []any{"STOP", "\nEND"}}},
		{"none", agentsdk.ModelSettings{ToolChoice: "none"}, map[string]any{"tool_choice": map[string]any{"type": "none"}}},
		{"none serial", agentsdk.ModelSettings{ToolChoice: "none", ParallelToolCalls: &serial}, map[string]any{"tool_choice": map[string]any{"type": "none"}}},
		{"none parallel allowed", agentsdk.ModelSettings{ToolChoice: "none", ParallelToolCalls: &parallel}, map[string]any{"tool_choice": map[string]any{"type": "none"}}},
		{"auto", agentsdk.ModelSettings{ToolChoice: "auto"}, map[string]any{"tool_choice": map[string]any{"type": "auto"}}},
		{"required", agentsdk.ModelSettings{ToolChoice: "required"}, map[string]any{"tool_choice": map[string]any{"type": "any"}}},
		{"named", agentsdk.ModelSettings{ToolChoice: "lookup"}, map[string]any{"tool_choice": map[string]any{"type": "tool", "name": "lookup"}}},
		{"parallel default choice", agentsdk.ModelSettings{ParallelToolCalls: &parallel}, map[string]any{"tool_choice": map[string]any{"type": "auto", "disable_parallel_tool_use": false}}},
		{"serial default choice", agentsdk.ModelSettings{ParallelToolCalls: &serial}, map[string]any{"tool_choice": map[string]any{"type": "auto", "disable_parallel_tool_use": true}}},
		{"required serial", agentsdk.ModelSettings{ToolChoice: "required", ParallelToolCalls: &serial}, map[string]any{"tool_choice": map[string]any{"type": "any", "disable_parallel_tool_use": true}}},
		{"named parallel", agentsdk.ModelSettings{ToolChoice: "lookup", ParallelToolCalls: &parallel}, map[string]any{"tool_choice": map[string]any{"type": "tool", "name": "lookup", "disable_parallel_tool_use": false}}},
	} {
		for _, model := range []string{"claude-sonnet-4-5", "claude-sonnet-4-6"} {
			for _, streaming := range []bool{false, true} {
				t.Run(tc.name+"/"+model+"/"+map[bool]string{false: "blocking", true: "streaming"}[streaming], func(t *testing.T) {
					body := settingsContractCall(t, agentsdk.ModelRequest{Model: model, Settings: tc.settings, Tools: settingsContractTools()}, streaming, "")
					for _, key := range []string{"temperature", "top_p", "stop_sequences", "tool_choice", "thinking", "output_config"} {
						got, present := body[key]
						want, wanted := tc.want[key]
						if present != wanted || !reflect.DeepEqual(got, want) {
							t.Errorf("%s = %#v (present %v), want %#v (present %v)", key, got, present, want, wanted)
						}
					}
				})
			}
		}
	}
}

func TestPublicSettingsThinkingHTTPContract(t *testing.T) {
	one, topP, parallel := 1.0, 0.95, false
	for _, tc := range []struct{ model, shape string }{{"claude-sonnet-4-5", "enabled"}, {"claude-sonnet-5", "adaptive"}} {
		for _, streaming := range []bool{false, true} {
			t.Run(tc.model+"/"+map[bool]string{false: "blocking", true: "streaming"}[streaming], func(t *testing.T) {
				body := settingsContractCall(t, agentsdk.ModelRequest{
					Model: tc.model, Tools: settingsContractTools(),
					Settings: agentsdk.ModelSettings{ReasoningEffort: "high", ToolChoice: "auto", Temperature: &one, TopP: &topP, ParallelToolCalls: &parallel},
				}, streaming, "")
				thinking := body["thinking"].(map[string]any)
				if thinking["type"] != tc.shape || body["temperature"] != one || body["top_p"] != topP {
					t.Fatalf("settings or thinking changed: %#v", body)
				}
				if tc.shape == "enabled" && thinking["budget_tokens"] != float64(8192) {
					t.Fatalf("thinking budget changed: %#v", thinking)
				}
				if tc.shape == "adaptive" && (thinking["display"] != "summarized" || body["output_config"].(map[string]any)["effort"] != "high") {
					t.Fatalf("adaptive thinking changed: %#v", body)
				}
			})
		}
	}
}

func TestPublicSettingsValidationBeforeHTTP(t *testing.T) {
	zero, negative, high, nan, inf := 0.0, -0.1, 1.1, math.NaN(), math.Inf(1)
	for _, tc := range []struct {
		name     string
		settings agentsdk.ModelSettings
		noTools  bool
		want     string
	}{
		{"unknown tool", agentsdk.ModelSettings{ToolChoice: "missing"}, false, "unknown tool"},
		{"required without tools", agentsdk.ModelSettings{ToolChoice: "required"}, true, "requires tools"},
		{"auto without tools", agentsdk.ModelSettings{ToolChoice: "auto"}, true, "requires tools"},
		{"negative temperature", agentsdk.ModelSettings{Temperature: &negative}, false, "Temperature must be finite"},
		{"high temperature", agentsdk.ModelSettings{Temperature: &high}, false, "Temperature must be finite"},
		{"nan temperature", agentsdk.ModelSettings{Temperature: &nan}, false, "Temperature must be finite"},
		{"infinite top p", agentsdk.ModelSettings{TopP: &inf}, false, "TopP must be finite"},
		{"negative top p", agentsdk.ModelSettings{TopP: &negative}, false, "TopP must be finite"},
		{"high top p", agentsdk.ModelSettings{TopP: &high}, false, "TopP must be finite"},
		{"thinking required", agentsdk.ModelSettings{ToolChoice: "required", ReasoningEffort: "high"}, false, "forced ToolChoice"},
		{"thinking named", agentsdk.ModelSettings{ToolChoice: "lookup", ThinkingBudget: 4096}, false, "forced ToolChoice"},
		{"thinking temperature", agentsdk.ModelSettings{Temperature: &zero, ReasoningEffort: "high"}, false, "Temperature must be 1"},
		{"thinking top p", agentsdk.ModelSettings{TopP: &zero, ReasoningEffort: "high"}, false, "TopP must be between 0.95"},
	} {
		for _, model := range []string{"claude-sonnet-4-5", "claude-sonnet-5"} {
			for _, streaming := range []bool{false, true} {
				t.Run(tc.name+"/"+model+"/"+map[bool]string{false: "blocking", true: "streaming"}[streaming], func(t *testing.T) {
					req := agentsdk.ModelRequest{Model: model, Settings: tc.settings}
					if !tc.noTools {
						req.Tools = settingsContractTools()
					}
					settingsContractCall(t, req, streaming, tc.want)
				})
			}
		}
	}
}

func TestPublicSettingsWithoutToolsHTTPContract(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		for _, choice := range []string{"", "none"} {
			for _, streaming := range []bool{false, true} {
				body := settingsContractCall(t, agentsdk.ModelRequest{Settings: agentsdk.ModelSettings{ToolChoice: choice, ParallelToolCalls: &parallel}}, streaming, "")
				if choice == "" {
					if _, ok := body["tool_choice"]; ok {
						t.Fatal("parallel preference without tools introduced tool_choice")
					}
				} else if !reflect.DeepEqual(body["tool_choice"], map[string]any{"type": "none"}) {
					t.Fatalf("tool_choice = %#v", body["tool_choice"])
				}
			}
		}
	}
}

func settingsContractTools() []agentsdk.Tool {
	return []agentsdk.Tool{&agentsdk.FunctionTool{ToolName: "lookup", Schema: json.RawMessage(`{"type":"object"}`)}}
}

func settingsContractCall(t *testing.T, req agentsdk.ModelRequest, streaming bool, wantError string) map[string]any {
	t.Helper()
	var calls atomic.Int32
	bodies := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies <- body
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_settings\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	model := NewAnthropicModelWithClient(internalanthropic.NewClient("test", internalanthropic.WithBaseURL(srv.URL)))
	req.Input = []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "hello"}}}
	var err error
	if streaming {
		var stream *agentsdk.ModelStream
		stream, err = model.StreamResponse(context.Background(), req)
		if err == nil {
			for event := range stream.Events {
				if event.Error != nil {
					t.Fatal(event.Error)
				}
			}
			if stream.Final() == nil {
				t.Fatal("missing final response")
			}
		}
	} else {
		_, err = model.GetResponse(context.Background(), req)
	}
	if wantError != "" {
		if err == nil || !strings.Contains(err.Error(), wantError) || calls.Load() != 0 {
			t.Fatalf("error = %v, HTTP calls = %d; want %q before HTTP", err, calls.Load(), wantError)
		}
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("HTTP calls = %d, want 1", calls.Load())
	}
	return <-bodies
}
