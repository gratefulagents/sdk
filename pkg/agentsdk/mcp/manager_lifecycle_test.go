package mcp

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestHelperMCPServer is not a real test: it is re-executed as a child
// process by TestConnectStdioServerChildOutlivesConnectReturn and speaks a
// minimal newline-delimited JSON-RPC MCP dialect over stdio (initialize,
// tools/list, ping). Implemented by hand instead of the go-sdk server so the
// helper has zero coupling to client-side code under test.
func TestHelperMCPServer(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_SERVER") != "1" {
		t.Skip("helper process for TestConnectStdioServerChildOutlivesConnectReturn")
	}
	dec := json.NewDecoder(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&req); err != nil {
			os.Exit(0) // client closed stdin
		}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "helper", "version": "0.0.1"},
			}})
		case "tools/list":
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"tools": []map[string]any{{
					"name":        "ping",
					"description": "test tool",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				}},
			}})
		default:
			if req.ID != nil { // reply to any other request; ignore notifications
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
			}
		}
	}
}

// TestConnectStdioServerChildOutlivesConnectReturn is the regression test for
// the bug where every MCP server died right after a successful handshake:
// the child was built with exec.CommandContext on the internal connect-timeout
// context, which connectStdioServer cancels on return — SIGKILLing the server
// before its first tools/list, which then read a blind EOF.
func TestConnectStdioServerChildOutlivesConnectReturn(t *testing.T) {
	t.Parallel()

	opts := resolveManagerOptions(WithCommandExecutor(directExecutor{}))
	cfg := ServerConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run", "^TestHelperMCPServer$"},
		Env:     map[string]string{"GO_WANT_MCP_SERVER": "1"},
	}
	conn, err := connectStdioServer(context.Background(), t.TempDir(), "helper", cfg, opts)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.session.Close()
		_ = terminateProcess(conn.cmd, 2*time.Second)
	})

	// connectStdioServer has returned: its internal connect context is now
	// cancelled. Give a buggy CommandContext kill time to land, then prove
	// the child is still alive and serving.
	time.Sleep(150 * time.Millisecond)
	tools, err := listAllTools(context.Background(), conn.session)
	if err != nil {
		t.Fatalf("list tools after connect returned: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "ping" {
		t.Fatalf("tools after connect = %+v, want [ping]", tools)
	}
}
