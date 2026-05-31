package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var invalidBlobNameChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

const (
	maxMCPTextBytes      = 256 * 1024
	maxMCPBlobBytes      = 10 * 1024 * 1024
	maxBlobPrefixLength  = 96
	mcpTruncationMessage = "\n[truncated MCP text output]"
)

// ToolManager is the MCP manager surface needed by SDK MCP tools.
type ToolManager interface {
	ToolDescriptors() []ToolDescriptor
	ConnectedServerNames() []string
	HasResources() bool
	CallTool(ctx context.Context, qualifiedName string, args map[string]any) (*mcpsdk.CallToolResult, error)
	ListResources(ctx context.Context, serverName string) ([]ResourceDescriptor, error)
	ReadResource(ctx context.Context, serverName, uri string) (*mcpsdk.ReadResourceResult, error)
}

// BuildTools converts connected MCP descriptors into SDK tools.
func BuildTools(manager ToolManager) []agentsdk.Tool {
	if manager == nil {
		return nil
	}

	tools := make([]agentsdk.Tool, 0, len(manager.ToolDescriptors())+2)
	for _, desc := range manager.ToolDescriptors() {
		tools = append(tools, &DynamicTool{
			Manager:    manager,
			Descriptor: desc,
		})
	}
	if manager.HasResources() {
		tools = append(tools, &ListResourcesTool{Manager: manager}, &ReadResourceTool{Manager: manager})
	}
	return tools
}

// DynamicTool exposes one MCP descriptor as an SDK tool.
type DynamicTool struct {
	Manager    ToolManager
	Descriptor ToolDescriptor
}

func (t *DynamicTool) Name() string { return t.Descriptor.QualifiedName }

func (t *DynamicTool) Description() string {
	return SanitizeMCPToolDescription(t.Descriptor.ServerName, t.Descriptor.ToolName, t.Descriptor.Description)
}

func (t *DynamicTool) InputSchema() json.RawMessage {
	if len(t.Descriptor.InputSchema) == 0 {
		return json.RawMessage(`{"type":"object","additionalProperties":true}`)
	}
	return t.Descriptor.InputSchema
}

func (t *DynamicTool) IsReadOnly() bool { return t.Descriptor.ReadOnly }

func (t *DynamicTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	if ctx != nil && ctx.ToolAccessLevel == agentsdk.ToolAccessLevelReadOnly {
		return t.IsReadOnly()
	}
	return true
}

func (t *DynamicTool) NeedsApproval() bool { return false }

func (t *DynamicTool) TimeoutSeconds() int { return 0 }

func (t *DynamicTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	args := map[string]any{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
		}
	}

	callResult, err := t.Manager.CallTool(ctx, t.Descriptor.QualifiedName, args)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("MCP tool error: %v", err), IsError: true}, nil
	}

	content, err := FormatCallToolResult(workDir, t.Descriptor, callResult)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("MCP output formatting error: %v", err), IsError: true}, nil
	}
	if strings.TrimSpace(content) == "" {
		content = "(no output)"
	}
	return agentsdk.ToolResult{Content: content, IsError: callResult != nil && callResult.IsError}, nil
}

// ListResourcesTool lists resources available from connected MCP servers.
type ListResourcesTool struct {
	Manager ToolManager
}

func (t *ListResourcesTool) Name() string { return "ListMcpResourcesTool" }

func (t *ListResourcesTool) Description() string {
	return "List resources from connected MCP servers. Optionally filter by server name."
}

func (t *ListResourcesTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"server":{
				"type":"string",
				"description":"Optional MCP server name to filter resources by"
			}
		}
	}`)
}

func (t *ListResourcesTool) IsReadOnly() bool { return true }

func (t *ListResourcesTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *ListResourcesTool) NeedsApproval() bool { return false }

func (t *ListResourcesTool) TimeoutSeconds() int { return 0 }

func (t *ListResourcesTool) Execute(ctx context.Context, input json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		Server string `json:"server"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
		}
	}

	resources, err := t.Manager.ListResources(ctx, strings.TrimSpace(in.Server))
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("MCP resource error: %v", err), IsError: true}, nil
	}
	if len(resources) == 0 {
		return agentsdk.ToolResult{Content: "No resources found. MCP servers may still provide tools without resources."}, nil
	}

	data, err := json.Marshal(resources)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("MCP resource formatting error: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: string(data)}, nil
}

// ReadResourceTool reads a specific resource from an MCP server.
type ReadResourceTool struct {
	Manager ToolManager
}

func (t *ReadResourceTool) Name() string { return "ReadMcpResourceTool" }

func (t *ReadResourceTool) Description() string {
	return "Read a specific MCP resource by server name and URI."
}

func (t *ReadResourceTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"server":{"type":"string","description":"MCP server name"},
			"uri":{"type":"string","description":"Resource URI"}
		},
		"required":["server","uri"]
	}`)
}

func (t *ReadResourceTool) IsReadOnly() bool { return true }

func (t *ReadResourceTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *ReadResourceTool) NeedsApproval() bool { return false }

func (t *ReadResourceTool) TimeoutSeconds() int { return 0 }

func (t *ReadResourceTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in struct {
		Server string `json:"server"`
		URI    string `json:"uri"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}

	in.Server = strings.TrimSpace(in.Server)
	in.URI = strings.TrimSpace(in.URI)
	if in.Server == "" {
		return agentsdk.ToolResult{Content: "server is required", IsError: true}, nil
	}
	if in.URI == "" {
		return agentsdk.ToolResult{Content: "uri is required", IsError: true}, nil
	}

	readResult, err := t.Manager.ReadResource(ctx, in.Server, in.URI)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("MCP resource error: %v", err), IsError: true}, nil
	}

	content, err := FormatReadResourceResult(workDir, in.Server, readResult)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("MCP resource formatting error: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: content}, nil
}

type readResourceOutput struct {
	Contents []readResourceContent `json:"contents"`
}

type readResourceContent struct {
	URI        string `json:"uri"`
	MIMEType   string `json:"mimeType,omitempty"`
	Text       string `json:"text,omitempty"`
	BlobSaved  string `json:"blobSavedTo,omitempty"`
	BlobSize   int    `json:"blobSize,omitempty"`
	BinaryHint string `json:"binaryHint,omitempty"`
}

// FormatReadResourceResult renders an MCP read-resource response as model-safe JSON.
func FormatReadResourceResult(workDir, serverName string, result *mcpsdk.ReadResourceResult) (string, error) {
	if result == nil {
		return `{"contents":[]}`, nil
	}

	out := readResourceOutput{
		Contents: make([]readResourceContent, 0, len(result.Contents)),
	}

	for i, content := range result.Contents {
		if content == nil {
			continue
		}
		item := readResourceContent{
			URI:      content.URI,
			MIMEType: content.MIMEType,
		}
		if content.Text != "" {
			item.Text = truncateMCPText(content.Text)
		}
		if len(content.Blob) > 0 {
			path, err := persistBinaryBlob(workDir, serverName, "resource", content.MIMEType, content.Blob, i)
			if err != nil {
				item.Text = fmt.Sprintf("Binary content could not be saved: %v", err)
			} else {
				item.BlobSaved = path
				item.BlobSize = len(content.Blob)
				item.BinaryHint = binarySavedMessage(path, content.MIMEType, len(content.Blob))
				if item.Text == "" {
					item.Text = item.BinaryHint
				}
			}
		}
		out.Contents = append(out.Contents, item)
	}

	data, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// FormatCallToolResult renders an MCP tool call response as model-safe text/JSON.
func FormatCallToolResult(workDir string, desc ToolDescriptor, result *mcpsdk.CallToolResult) (string, error) {
	if result == nil {
		return "", nil
	}

	if len(result.Content) == 1 && result.StructuredContent == nil {
		if text, ok := result.Content[0].(*mcpsdk.TextContent); ok {
			return truncateMCPText(text.Text), nil
		}
	}

	payload := map[string]any{}
	if len(result.Content) > 0 {
		blocks := make([]any, 0, len(result.Content))
		for i, block := range result.Content {
			rendered, err := renderContentBlock(workDir, desc, block, i)
			if err != nil {
				return "", err
			}
			blocks = append(blocks, rendered)
		}
		payload["content"] = blocks
	}
	if result.StructuredContent != nil {
		payload["structuredContent"] = result.StructuredContent
	}
	if len(payload) == 0 {
		return "", nil
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func renderContentBlock(workDir string, desc ToolDescriptor, block mcpsdk.Content, index int) (any, error) {
	switch content := block.(type) {
	case *mcpsdk.TextContent:
		return map[string]any{
			"type": "text",
			"text": truncateMCPText(content.Text),
		}, nil

	case *mcpsdk.ImageContent:
		path, err := persistBinaryBlob(workDir, desc.ServerName, desc.ToolName, content.MIMEType, content.Data, index)
		if err != nil {
			return map[string]any{
				"type":     "image",
				"mimeType": content.MIMEType,
				"error":    err.Error(),
			}, nil
		}
		return map[string]any{
			"type":        "image",
			"mimeType":    content.MIMEType,
			"blobSavedTo": path,
			"note":        binarySavedMessage(path, content.MIMEType, len(content.Data)),
		}, nil

	case *mcpsdk.AudioContent:
		path, err := persistBinaryBlob(workDir, desc.ServerName, desc.ToolName, content.MIMEType, content.Data, index)
		if err != nil {
			return map[string]any{
				"type":     "audio",
				"mimeType": content.MIMEType,
				"error":    err.Error(),
			}, nil
		}
		return map[string]any{
			"type":        "audio",
			"mimeType":    content.MIMEType,
			"blobSavedTo": path,
			"note":        binarySavedMessage(path, content.MIMEType, len(content.Data)),
		}, nil

	case *mcpsdk.ResourceLink:
		return map[string]any{
			"type":        "resource_link",
			"uri":         content.URI,
			"name":        content.Name,
			"title":       content.Title,
			"description": content.Description,
			"mimeType":    content.MIMEType,
		}, nil

	case *mcpsdk.EmbeddedResource:
		if content.Resource == nil {
			return map[string]any{"type": "resource", "resource": nil}, nil
		}
		resource := map[string]any{
			"type":     "resource",
			"uri":      content.Resource.URI,
			"mimeType": content.Resource.MIMEType,
		}
		if content.Resource.Text != "" {
			resource["text"] = truncateMCPText(content.Resource.Text)
		}
		if len(content.Resource.Blob) > 0 {
			path, err := persistBinaryBlob(workDir, desc.ServerName, desc.ToolName, content.Resource.MIMEType, content.Resource.Blob, index)
			if err != nil {
				resource["error"] = err.Error()
			} else {
				resource["blobSavedTo"] = path
				resource["note"] = binarySavedMessage(path, content.Resource.MIMEType, len(content.Resource.Blob))
			}
		}
		return resource, nil

	default:
		raw, err := block.MarshalJSON()
		if err != nil {
			return map[string]any{"type": "unknown", "error": err.Error()}, nil
		}
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return string(raw), nil
		}
		return parsed, nil
	}
}

func persistBinaryBlob(workDir, serverName, sourceName, mimeType string, data []byte, index int) (string, error) {
	if len(data) > maxMCPBlobBytes {
		return "", fmt.Errorf("binary content is %d bytes; max allowed is %d bytes", len(data), maxMCPBlobBytes)
	}
	relDir := filepath.Join(".mcp", "blobs")
	dir, err := resolveWorkspaceOutputDir(workDir, relDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create blob dir: %w", err)
	}

	prefix := sanitizeBlobPrefix(serverName + "-" + sourceName)
	if prefix == "" {
		prefix = "mcp"
	}
	ext := fileExtFromMIME(mimeType)
	filename := fmt.Sprintf("%s-%d-%d%s", prefix, time.Now().UnixNano(), index, ext)
	path, err := resolveWorkspaceOutputDir(workDir, filepath.Join(relDir, filename))
	if err != nil {
		return "", err
	}

	if err := writeFileNoFollow(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write blob: %w", err)
	}
	return path, nil
}

func writeFileNoFollow(path string, data []byte, perm os.FileMode) error {
	f, err := openFileNoFollow(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func resolveWorkspaceOutputDir(workDir, relDir string) (string, error) {
	if strings.TrimSpace(workDir) == "" {
		return "", fmt.Errorf("workspace root is required")
	}
	baseAbs, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	base, err := filepath.EvalSymlinks(baseAbs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root symlinks: %w", err)
	}
	target := filepath.Clean(filepath.Join(baseAbs, relDir))
	checked, err := resolveExistingOutputPath(target)
	if err != nil {
		return "", fmt.Errorf("resolve output path symlinks: %w", err)
	}
	rel, err := filepath.Rel(base, checked)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("output path %s is outside workspace root %s", relDir, workDir)
	}
	return target, nil
}

func resolveExistingOutputPath(path string) (string, error) {
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	for parent := clean; ; parent = filepath.Dir(parent) {
		if _, err := os.Lstat(parent); err == nil {
			resolved, evalErr := filepath.EvalSymlinks(parent)
			if evalErr != nil {
				return "", evalErr
			}
			rel, relErr := filepath.Rel(parent, clean)
			if relErr != nil {
				return "", relErr
			}
			return filepath.Clean(filepath.Join(resolved, rel)), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return clean, nil
		}
	}
}

func sanitizeBlobPrefix(s string) string {
	s = invalidBlobNameChars.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "mcp"
	}
	if len(s) > maxBlobPrefixLength {
		s = strings.TrimRight(s[:maxBlobPrefixLength], "._-")
		if s == "" {
			return "mcp"
		}
	}
	return s
}

func truncateMCPText(s string) string {
	if len(s) <= maxMCPTextBytes {
		return s
	}
	cut := maxMCPTextBytes
	for cut > 0 && (s[cut]&0xc0) == 0x80 {
		cut--
	}
	if cut <= 0 {
		cut = maxMCPTextBytes
	}
	return s[:cut] + mcpTruncationMessage
}

func fileExtFromMIME(mimeType string) string {
	if strings.TrimSpace(mimeType) == "" {
		return ".bin"
	}
	exts, err := mime.ExtensionsByType(mimeType)
	if err != nil || len(exts) == 0 {
		return ".bin"
	}
	return exts[0]
}

func binarySavedMessage(path, mimeType string, size int) string {
	if strings.TrimSpace(mimeType) == "" {
		mimeType = "application/octet-stream"
	}
	return fmt.Sprintf("Binary content saved to %s (%d bytes, %s)", path, size, mimeType)
}
