package projectstatetools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/projectstate"
)

func TestMemoryManagementToolsUpdateStatsAndDelete(t *testing.T) {
	store := newTestStore(t)
	tools := toolsByName(Tools(store, "assistant"))
	for _, name := range []string{"memory_remember", "memory_update", "memory_delete", "memory_stats"} {
		if tools[name] == nil {
			t.Fatalf("missing tool %q; names=%v", name, tools)
		}
	}

	remembered := execTool(t, tools["memory_remember"], `{
		"content": "User prefers concise answers.",
		"kind": "semantic",
		"scope": "user",
		"tags": ["preference"]
	}`)
	var mem projectstate.Memory
	if err := json.Unmarshal([]byte(remembered.Content), &mem); err != nil {
		t.Fatal(err)
	}
	if mem.ID == "" {
		t.Fatalf("remembered memory missing id: %#v", mem)
	}

	updated := execTool(t, tools["memory_update"], `{
		"id": "`+mem.ID+`",
		"content": "User prefers compact answers.",
		"kind": "procedural",
		"tags": ["preference", "style"]
	}`)
	var updatedMem projectstate.Memory
	if err := json.Unmarshal([]byte(updated.Content), &updatedMem); err != nil {
		t.Fatal(err)
	}
	if updatedMem.ID != mem.ID {
		t.Fatalf("updated ID = %q, want %q", updatedMem.ID, mem.ID)
	}
	if updatedMem.Content != "User prefers compact answers." || updatedMem.Kind != projectstate.MemoryKindProcedural {
		t.Fatalf("updated memory = %#v", updatedMem)
	}
	if len(updatedMem.Tags) != 2 {
		t.Fatalf("updated tags = %#v", updatedMem.Tags)
	}

	statsResult := execTool(t, tools["memory_stats"], `{}`)
	var stats memoryStats
	if err := json.Unmarshal([]byte(statsResult.Content), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Total != 1 || stats.ByKind[projectstate.MemoryKindProcedural] != 1 || stats.ByScope[projectstate.MemoryScopeUser] != 1 || stats.ByTag["style"] != 1 {
		t.Fatalf("stats = %#v", stats)
	}

	deleted := execTool(t, tools["memory_delete"], `{"id":"`+mem.ID+`"}`)
	if !strings.Contains(deleted.Content, `"deleted"`) {
		t.Fatalf("delete result = %s", deleted.Content)
	}
	memories, err := store.ListMemories(t.Context(), projectstate.MemoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 0 {
		t.Fatalf("memories after delete = %#v, want empty", memories)
	}
}

func TestMemoryUpdateRequiresExistingID(t *testing.T) {
	store := newTestStore(t)
	tools := toolsByName(Tools(store, "assistant"))
	result, err := tools["memory_update"].Execute(t.Context(), json.RawMessage(`{"id":"missing","content":"Nope."}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, `memory "missing" not found`) {
		t.Fatalf("result = %#v, want not found error", result)
	}
}

func newTestStore(t *testing.T) projectstate.Store {
	t.Helper()
	store, err := projectstate.NewFilesystemStore(projectstate.FilesystemOptions{
		StateDir:  t.TempDir(),
		ProjectID: "test-project",
		Actor:     "assistant",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func toolsByName(tools []agentsdk.Tool) map[string]agentsdk.Tool {
	out := map[string]agentsdk.Tool{}
	for _, tool := range tools {
		out[tool.Name()] = tool
	}
	return out
}

func execTool(t *testing.T, tool agentsdk.Tool, input string) agentsdk.ToolResult {
	t.Helper()
	result, err := tool.Execute(t.Context(), json.RawMessage(input), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s returned error: %s", tool.Name(), result.Content)
	}
	return result
}
