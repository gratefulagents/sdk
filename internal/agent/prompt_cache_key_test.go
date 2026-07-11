package agent

import (
	"context"
	"strings"
	"testing"
)

func TestPromptCacheWireKeyStableAndNamespaced(t *testing.T) {
	logical := strings.Repeat("../../evil/💥", 100)
	got := promptCacheWireKey("namespace-a", logical)
	if len(got) != 64 {
		t.Fatalf("wire key length = %d, want 64", len(got))
	}
	if got != promptCacheWireKey("namespace-a", logical) {
		t.Fatal("wire key is not stable")
	}
	if got == promptCacheWireKey("namespace-b", logical) {
		t.Fatal("different namespaces produced the same wire key")
	}
	if got == promptCacheWireKey("namespace-a", "child") {
		t.Fatal("different logical child keys produced the same wire key")
	}
}

func TestPromptCacheKeyStableForParentRun(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}}}}
	trace := NewTrace("parent")
	_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "parent"}, []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "go"}}}, RunConfig{Trace: trace})
	if err != nil {
		t.Fatal(err)
	}
	want := promptCacheWireKey(trace.ID, "run")
	if len(model.requests) != 1 || model.requests[0].PromptCacheKey != want {
		t.Fatalf("requests = %+v, want prompt cache key %q", model.requests, want)
	}
}

func TestRunSubAgentOnceForcesStableTaskPromptCacheKey(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}}}}
	runSubAgentOnce(context.Background(), subAgentRunSpec{
		Runner:  NewRunnerWithModel(model),
		Agent:   &Agent{Name: "child"},
		Message: "go",
		TaskID:  "task-stable-123",
		RunConfig: RunConfig{
			PromptCacheKey:       "parent-key-must-not-leak",
			PromptCacheNamespace: "shared-run",
		},
	})
	want := promptCacheWireKey("shared-run", "task-stable-123")
	if len(model.requests) != 1 || model.requests[0].PromptCacheKey != want {
		t.Fatalf("requests = %+v, want child task prompt cache key %q", model.requests, want)
	}
}
