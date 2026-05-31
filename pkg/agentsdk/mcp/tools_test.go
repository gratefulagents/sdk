package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeToolManager struct {
	descriptors []ToolDescriptor
	resources   []ResourceDescriptor
	callResult  *mcpsdk.CallToolResult
	readResult  *mcpsdk.ReadResourceResult
	callArgs    map[string]any
}

func (f *fakeToolManager) ToolDescriptors() []ToolDescriptor {
	return append([]ToolDescriptor(nil), f.descriptors...)
}

func (f *fakeToolManager) ConnectedServerNames() []string { return []string{"files"} }

func (f *fakeToolManager) HasResources() bool { return f.resources != nil || f.readResult != nil }

func (f *fakeToolManager) CallTool(ctx context.Context, qualifiedName string, args map[string]any) (*mcpsdk.CallToolResult, error) {
	f.callArgs = args
	return f.callResult, nil
}

func (f *fakeToolManager) ListResources(ctx context.Context, serverName string) ([]ResourceDescriptor, error) {
	return append([]ResourceDescriptor(nil), f.resources...), nil
}

func (f *fakeToolManager) ReadResource(ctx context.Context, serverName, uri string) (*mcpsdk.ReadResourceResult, error) {
	return f.readResult, nil
}

func TestDynamicToolExecutesAndReturnsSingleTextContent(t *testing.T) {
	t.Parallel()

	manager := &fakeToolManager{
		callResult: &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "created"}},
		},
	}
	tool := &DynamicTool{
		Manager: manager,
		Descriptor: ToolDescriptor{
			QualifiedName: "mcp__github__create_issue",
			ServerName:    "github",
			ToolName:      "create_issue",
		},
	}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"title":"bug"}`), t.TempDir())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true, want false")
	}
	if result.Content != "created" {
		t.Fatalf("Content = %q, want created", result.Content)
	}
	if manager.callArgs["title"] != "bug" {
		t.Fatalf("call args = %#v, want title", manager.callArgs)
	}
}

func TestNormalizeInputSchemaAddsEmptyPropertiesForObjectTools(t *testing.T) {
	t.Parallel()

	got := normalizeInputSchema(map[string]any{
		"type":                 "object",
		"additionalProperties": false,
	})
	var schema map[string]any
	if err := json.Unmarshal(got, &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("type = %v, want object", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) != 0 {
		t.Fatalf("properties = %#v, want empty object", schema["properties"])
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %v, want false", schema["additionalProperties"])
	}
}

func TestFormatCallToolResultSavesBinaryContent(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	rendered, err := FormatCallToolResult(workDir, ToolDescriptor{
		ServerName: "files",
		ToolName:   "preview",
	}, &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte("png-data")},
		},
	})
	if err != nil {
		t.Fatalf("FormatCallToolResult() error = %v", err)
	}
	if !strings.Contains(rendered, `"blobSavedTo"`) {
		t.Fatalf("rendered = %s, want saved blob", rendered)
	}

	var payload struct {
		Content []struct {
			BlobSavedTo string `json:"blobSavedTo"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(rendered), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(payload.Content) != 1 || payload.Content[0].BlobSavedTo == "" {
		t.Fatalf("payload = %#v, want saved blob path", payload)
	}
	data, err := os.ReadFile(payload.Content[0].BlobSavedTo)
	if err != nil {
		t.Fatalf("ReadFile(blob) error = %v", err)
	}
	if string(data) != "png-data" {
		t.Fatalf("blob = %q, want png-data", string(data))
	}
	info, err := os.Stat(payload.Content[0].BlobSavedTo)
	if err != nil {
		t.Fatalf("Stat(blob) error = %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("blob mode = %o, want 0o600", mode)
	}
	dirInfo, err := os.Stat(filepath.Dir(payload.Content[0].BlobSavedTo))
	if err != nil {
		t.Fatalf("Stat(blob dir) error = %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Fatalf("blob dir mode = %o, want 0o700", mode)
	}
}

func TestFormatCallToolResultTruncatesLargeText(t *testing.T) {
	t.Parallel()

	rendered, err := FormatCallToolResult(t.TempDir(), ToolDescriptor{
		ServerName: "files",
		ToolName:   "read",
	}, &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: strings.Repeat("x", maxMCPTextBytes+1)},
		},
	})
	if err != nil {
		t.Fatalf("FormatCallToolResult() error = %v", err)
	}
	if len(rendered) > maxMCPTextBytes+len(mcpTruncationMessage) || !strings.Contains(rendered, mcpTruncationMessage) {
		t.Fatalf("rendered text was not truncated as expected; len=%d", len(rendered))
	}
}

func TestFormatCallToolResultRejectsOversizedBinaryContent(t *testing.T) {
	t.Parallel()

	rendered, err := FormatCallToolResult(t.TempDir(), ToolDescriptor{
		ServerName: "files",
		ToolName:   "preview",
	}, &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			&mcpsdk.ImageContent{MIMEType: "image/png", Data: make([]byte, maxMCPBlobBytes+1)},
		},
	})
	if err != nil {
		t.Fatalf("FormatCallToolResult() error = %v", err)
	}
	if strings.Contains(rendered, `"blobSavedTo"`) || !strings.Contains(rendered, "max allowed") {
		t.Fatalf("rendered = %s, want oversized binary error without saved blob", rendered)
	}
}

func TestFormatCallToolResultRejectsSymlinkBlobDirectoryEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outsideDir := filepath.Join(root, "outside")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, ".mcp")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	rendered, err := FormatCallToolResult(workDir, ToolDescriptor{
		ServerName: "files",
		ToolName:   "preview",
	}, &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte("png-data")},
		},
	})
	if err != nil {
		t.Fatalf("FormatCallToolResult() error = %v", err)
	}
	if strings.Contains(rendered, `"blobSavedTo"`) {
		t.Fatalf("rendered = %s, should not save blob outside workspace", rendered)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "blobs")); !os.IsNotExist(err) {
		t.Fatalf("outside blob dir stat error = %v, want not exist", err)
	}
}

func TestTrustedMCPReadOnlyRequiresServerOptIn(t *testing.T) {
	t.Parallel()

	tool := &mcpsdk.Tool{Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}
	if trustedMCPReadOnly(ServerConfig{}, tool) {
		t.Fatal("untrusted server readOnlyHint should not mark tool read-only")
	}
	if !trustedMCPReadOnly(ServerConfig{TrustReadOnlyHint: true}, tool) {
		t.Fatal("trusted server readOnlyHint should mark tool read-only")
	}
}

func TestBuildToolsIncludesResourcesWhenAvailable(t *testing.T) {
	t.Parallel()

	manager := &fakeToolManager{
		descriptors: []ToolDescriptor{{
			QualifiedName: "mcp__files__read",
			ServerName:    "files",
			ToolName:      "read",
			ReadOnly:      true,
		}},
		resources: []ResourceDescriptor{{URI: "file://README.md", Server: "files"}},
	}
	tools := BuildTools(manager)
	if len(tools) != 3 {
		t.Fatalf("len(BuildTools()) = %d, want 3", len(tools))
	}
	if tools[0].Name() != "mcp__files__read" || tools[1].Name() != "ListMcpResourcesTool" || tools[2].Name() != "ReadMcpResourceTool" {
		t.Fatalf("tool names = %q, %q, %q", tools[0].Name(), tools[1].Name(), tools[2].Name())
	}
}
