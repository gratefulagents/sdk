package tracestore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agent "github.com/gratefulagents/sdk/pkg/agentsdk"
)

// canary builds a credential-shaped string at runtime so this source file
// never contains a contiguous secret pattern itself.
func canarySecrets() map[string]string {
	return map[string]string{
		"aws":    "AKIA" + "IOSFODNN" + "7EXAMPLE",
		"slack":  "xoxb-" + strings.Repeat("1234567890", 2),
		"github": "ghp_" + strings.Repeat("a", 36),
		"pem": "-----BEGIN RSA " + "PRIVATE KEY-----\nMIIkeymaterialkeymaterial\n-----END RSA " +
			"PRIVATE KEY-----",
		"bearer": "Bearer " + strings.Repeat("b", 30),
	}
}

func readRunFiles(t *testing.T, root, runID string) map[string]string {
	t.Helper()
	files := map[string]string{}
	runDir := filepath.Join(root, "traces", runID)
	err := filepath.Walk(runDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(runDir, path)
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func emitAllSinks(tw *TraceWriter, marker string) {
	ctx := &agent.RunContext{}
	a := &agent.Agent{Name: "engineer", Model: "gpt-5.5"}
	tool := &agent.FunctionTool{ToolName: "Bash"}
	input, _ := json.Marshal(map[string]any{
		"command": "echo " + marker,
		"nested":  map[string]any{"deep": marker},
		"escaped": `{"api_key":"` + marker + `"}`,
	})

	tw.OnAgentStart(ctx, a)
	tw.OnToolStart(ctx, a, tool, agent.ToolCallData{ID: "call-1", Name: "Bash", Input: input})
	tw.OnToolEnd(ctx, a, tool, agent.ToolCallData{ID: "call-1", Name: "Bash", Input: input},
		agent.ToolResult{Content: "output " + marker})
	tw.OnLLMEnd(ctx, a, &agent.ModelResponse{Items: []agent.RunItem{
		{Type: agent.RunItemMessage, Message: &agent.MessageOutput{Text: "text " + marker}},
		{Type: agent.RunItemReasoning, Reasoning: &agent.ReasoningData{Text: "reasoning " + marker}},
	}})

	genSpan := agent.NewSpan("generation", "", &agent.GenerationSpanData{
		RequestedModel: "gpt-5.5",
		Turn:           1,
		AttemptNumber:  1,
		Request:        &agent.LLMRequestSnapshot{AgentName: "engineer", Instructions: "instructions " + marker},
		Response:       &agent.LLMResponseSnapshot{Texts: []string{"resp " + marker}},
	})
	tw.OnSpanStart(genSpan)
	genSpan.Finish()
	tw.OnSpanEnd(genSpan)

	subSpan := agent.NewSpan("subagent", "", &agent.SubagentSpanData{
		TaskID:     "task-1",
		Type:       "explore",
		Prompt:     "prompt " + marker,
		ResultText: "result " + marker,
		FilesRead:  []string{"secret-file-" + marker + ".txt"},
	})
	tw.OnSpanStart(subSpan)
	subSpan.Finish()
	tw.OnSpanEnd(subSpan)

	tw.WriteResolvedInstructions(1, "resolved "+marker)
	tw.WriteMetrics(map[string]any{"tool_calls": 2})
	tw.FinalizeRun("success")
}

// TestTraceWriterDefaultCaptureIsMetadataOnly verifies the default capture
// policy withholds prompts, reasoning, tool payloads, and file names across
// every sink, persisting content digests instead.
func TestTraceWriterDefaultCaptureIsMetadataOnly(t *testing.T) {
	root := t.TempDir()
	store, err := NewFilesystemTraceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewTraceWriter(store)
	if err := tw.InitRun(RunMetadata{RunID: "run-1", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	const marker = "VERYUNIQUECONTENTMARKER"
	emitAllSinks(tw, marker)

	files := readRunFiles(t, root, "run-1")
	if len(files) < 5 {
		t.Fatalf("files = %v, want trace artifacts", files)
	}
	for name, content := range files {
		if strings.Contains(content, marker) {
			t.Fatalf("metadata-only capture leaked content into %s: %s", name, content)
		}
	}
	toolCalls := files["tool_calls.jsonl"]
	if !strings.Contains(toolCalls, `"captured":false`) || !strings.Contains(toolCalls, `"sha256"`) {
		t.Fatalf("tool_calls missing content digest: %s", toolCalls)
	}

	// Every NDJSON record carries the schema version and run id.
	for _, name := range []string{"tool_calls.jsonl", "llm_calls.jsonl", "spans.jsonl", "agent_transitions.jsonl"} {
		for _, line := range strings.Split(strings.TrimSpace(files[name]), "\n") {
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("%s line %q: %v", name, line, err)
			}
			if entry["schema_version"] != float64(TraceSchemaVersion) {
				t.Fatalf("%s missing schema_version: %s", name, line)
			}
			if entry["run_id"] != "run-1" {
				t.Fatalf("%s missing run_id: %s", name, line)
			}
		}
	}

	// llm_end is metadata-only: no mirrored response payload.
	for _, line := range strings.Split(strings.TrimSpace(files["llm_calls.jsonl"]), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["type"] != "llm_end" {
			continue
		}
		if _, ok := entry["response"]; ok {
			t.Fatalf("llm_end mirrors response payload: %s", line)
		}
		if entry["text_count"] != float64(1) || entry["reasoning_count"] != float64(1) {
			t.Fatalf("llm_end missing content counts: %s", line)
		}
	}

	// Resolved instructions file holds a digest descriptor, not the prompt.
	resolved := files[filepath.Join("resolved_instructions", "turn_001.txt")]
	var digest map[string]any
	if err := json.Unmarshal([]byte(resolved), &digest); err != nil {
		t.Fatalf("resolved instructions is not a digest: %q", resolved)
	}
	if digest["captured"] != false {
		t.Fatalf("resolved instructions digest = %v", digest)
	}

	// Health artifact is present and reports the writes.
	var health TraceHealth
	if err := json.Unmarshal([]byte(files["trace_health.json"]), &health); err != nil {
		t.Fatal(err)
	}
	if health.EventsWritten == 0 || health.EventsDropped != 0 {
		t.Fatalf("health = %+v", health)
	}
}

// TestTraceWriterFullCaptureRedactsSecretsAcrossSinks verifies that the
// explicit full-capture opt-in still passes nested and JSON-escaped
// credentials through the canonical detector plus operator redactors on
// every sink.
func TestTraceWriterFullCaptureRedactsSecretsAcrossSinks(t *testing.T) {
	root := t.TempDir()
	store, err := NewFilesystemTraceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	operatorSecret := "OPERATOR" + "ONLYSECRET"
	tw := NewTraceWriterWithOptions(store, TraceWriterOptions{
		Capture: CaptureFull,
		Redactors: []func(string) string{
			func(s string) string { return strings.ReplaceAll(s, operatorSecret, "[OPERATOR-REDACTED]") },
		},
	})
	if err := tw.InitRun(RunMetadata{RunID: "run-1", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	secrets := canarySecrets()
	for name, secret := range secrets {
		emitAllSinks(tw, name+" "+secret)
	}
	emitAllSinks(tw, operatorSecret)

	files := readRunFiles(t, root, "run-1")
	for fileName, content := range files {
		for secretName, secret := range secrets {
			// The PEM canary is matched by its BEGIN header; check the key
			// body line instead of the whole block.
			needle := secret
			if secretName == "pem" {
				needle = "MIIkeymaterialkeymaterial"
			}
			if strings.Contains(content, needle) {
				t.Fatalf("full capture leaked %s canary into %s: %s", secretName, fileName, content)
			}
		}
		if strings.Contains(content, operatorSecret) {
			t.Fatalf("operator redactor not applied in %s", fileName)
		}
	}
	// Full capture must still record surrounding non-secret content.
	if !strings.Contains(files["tool_calls.jsonl"], "echo aws") {
		t.Fatalf("full capture lost non-secret content: %s", files["tool_calls.jsonl"])
	}
	if !strings.Contains(files["tool_calls.jsonl"], "[REDACTED") {
		t.Fatalf("full capture has no redaction markers: %s", files["tool_calls.jsonl"])
	}
	if !strings.Contains(files[filepath.Join("resolved_instructions", "turn_001.txt")], "resolved") {
		t.Fatal("full capture dropped resolved instructions content")
	}
}

// TestTraceWriterHealthCountsDropsAndTruncation verifies quota exhaustion and
// oversized events are surfaced in Health() and trace_health.json instead of
// being silently discarded.
func TestTraceWriterHealthCountsDropsAndTruncation(t *testing.T) {
	root := t.TempDir()
	store, err := NewFilesystemTraceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	store.maxAppendFileBytes = 512
	store.maxRotations = 1
	tw := NewTraceWriter(store)
	if err := tw.InitRun(RunMetadata{RunID: "run-1", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// Overflow the agent_transitions category (2 chunks x 512 bytes).
	for i := 0; i < 40; i++ {
		tw.RecordPhaseChange("phase-" + strings.Repeat("x", 64))
	}
	// An event over the per-event limit is replaced by a truncation marker.
	tw.appendJSON("tool_calls", map[string]any{
		"type":  "tool_start",
		"input": strings.Repeat("y", defaultMaxTraceEventBytes+1),
	})
	tw.FinalizeRun("success")

	health := tw.Health()
	if health.EventsDropped == 0 {
		t.Fatalf("health = %+v, want dropped events counted", health)
	}
	if health.EventsTruncated == 0 {
		t.Fatalf("health = %+v, want truncated events counted", health)
	}
	if health.LastError == "" {
		t.Fatalf("health = %+v, want last error recorded", health)
	}

	files := readRunFiles(t, root, "run-1")
	var persisted TraceHealth
	if err := json.Unmarshal([]byte(files["trace_health.json"]), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.EventsDropped == 0 || persisted.EventsTruncated == 0 {
		t.Fatalf("persisted health = %+v", persisted)
	}
	if !strings.Contains(files["tool_calls.jsonl"], `"type":"event_truncated"`) {
		t.Fatalf("truncation marker missing: %s", files["tool_calls.jsonl"])
	}
}
