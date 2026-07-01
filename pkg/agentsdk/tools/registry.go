package tools

import (
	"io"
	"sort"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkmemory "github.com/gratefulagents/sdk/pkg/agentsdk/memory"
	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/browser"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/fs"
	sdkgit "github.com/gratefulagents/sdk/pkg/agentsdk/tools/git"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/lsp"
	memorytool "github.com/gratefulagents/sdk/pkg/agentsdk/tools/memory"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/search"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/shell"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/signal"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/vision"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/web"
)

// Registry holds SDK tools with permission-aware registration.
type Registry struct {
	tools                   map[string]agentsdk.Tool
	workDir                 string
	permissionMode          policy.PermissionMode
	signals                 bool
	allowMutating           map[string]struct{}
	browser                 bool
	disableWeb              bool
	allowPrivateNetworkURLs bool
	visionTool              *vision.Tool
	memoryTool              *memorytool.Tool
	commandSandboxConfig    *sandbox.Config
	asyncShell              bool
	asyncShellManager       *shell.AsyncManager
	thinkTool               bool
	interactiveTerminal     bool
	terminalManager         *shell.TerminalManager
	attachRepositoryTool    *sdkgit.AttachRepositoryTool
}

// RegistryOption configures a Registry.
type RegistryOption func(*Registry)

func WithReadOnlyTools() RegistryOption {
	return WithPermissionMode(policy.PermissionModeReadOnly)
}

func WithPermissionMode(mode policy.PermissionMode) RegistryOption {
	return func(r *Registry) { r.permissionMode = policy.NormalizePermissionMode(string(mode)) }
}

func WithSignalTools() RegistryOption {
	return func(r *Registry) { r.signals = true }
}

func WithBrowserTools() RegistryOption {
	return func(r *Registry) { r.browser = true }
}

func WithoutWebTools() RegistryOption {
	return func(r *Registry) { r.disableWeb = true }
}

func WithPrivateNetworkURLs(allowed bool) RegistryOption {
	return func(r *Registry) { r.allowPrivateNetworkURLs = allowed }
}

func WithCommandSandboxConfig(config sandbox.Config) RegistryOption {
	return func(r *Registry) { r.commandSandboxConfig = &config }
}

func WithAsyncShellTools() RegistryOption {
	return func(r *Registry) { r.asyncShell = true }
}

// WithThinkTool registers the think scratchpad tool, which lets the model
// record reasoning between actions without changing state.
func WithThinkTool() RegistryOption {
	return func(r *Registry) { r.thinkTool = true }
}

// WithInteractiveTerminal registers the persistent PTY Terminal tool for
// interactive programs and long-lived foreground processes. Requires a write
// permission mode; Unix only.
func WithInteractiveTerminal() RegistryOption {
	return func(r *Registry) { r.interactiveTerminal = true }
}

func WithVisionTools(analyzeFn vision.AnalyzeFn) RegistryOption {
	return func(r *Registry) { r.visionTool = &vision.Tool{AnalyzeFn: analyzeFn} }
}

func WithVisionToolsWithDetail(analyzeFn vision.AnalyzeWithDetailFn) RegistryOption {
	return func(r *Registry) { r.visionTool = &vision.Tool{AnalyzeWithDetailFn: analyzeFn} }
}

func WithMemoryStore(store sdkmemory.Store, namespace, sourceRun, repoURL string) RegistryOption {
	return func(r *Registry) {
		r.memoryTool = memorytool.New(store, namespace, sourceRun, repoURL)
	}
}

func WithAttachRepositoryTool(opts ...sdkgit.AttachRepositoryOption) RegistryOption {
	return func(r *Registry) {
		r.attachRepositoryTool = sdkgit.NewAttachRepositoryTool(nil, opts...)
	}
}

func WithAllowedMutatingTools(names ...string) RegistryOption {
	return func(r *Registry) {
		if r.allowMutating == nil {
			r.allowMutating = make(map[string]struct{}, len(names))
		}
		for _, name := range names {
			if name != "" {
				r.allowMutating[name] = struct{}{}
			}
		}
	}
}

// NewRegistry creates a registry with the default SDK tool set.
func NewRegistry(workDir string, opts ...RegistryOption) *Registry {
	r := &Registry{
		tools:          make(map[string]agentsdk.Tool),
		workDir:        workDir,
		permissionMode: policy.PermissionModeWorkspaceWrite,
	}
	for _, opt := range opts {
		opt(r)
	}

	var allTools []agentsdk.Tool
	if r.permissionMode == policy.PermissionModeWorkspaceWrite {
		allTools = []agentsdk.Tool{
			&search.ListFilesTool{},
			&search.ReadFileTool{},
			&search.GlobTool{},
			&search.GrepTool{},
			&lsp.Tool{},
			&shell.WorkspaceWriteBashTool{BashTool: shell.BashTool{Executor: r.bashExecutor()}},
			&fs.WorkspaceWriteFileTool{},
			&fs.WorkspaceEditTool{},
		}
	} else {
		allTools = []agentsdk.Tool{
			&search.ListFilesTool{},
			&search.ReadFileTool{},
			&search.GlobTool{},
			&search.GrepTool{},
			&lsp.Tool{},
		}
		if !r.permissionMode.AllowsWriteTools() {
			allTools = append(allTools, &shell.ReadOnlyBashTool{BashTool: shell.BashTool{Executor: r.bashExecutor()}})
		} else {
			allTools = append(allTools,
				&shell.BashTool{Executor: r.bashExecutor()},
				&fs.FileWriteTool{},
				&fs.FileEditTool{},
			)
		}
	}
	if !r.disableWeb {
		allTools = append(allTools, &web.FetchTool{AllowPrivateNetworkURLs: r.allowPrivateNetworkURLs})
	}
	if r.signals {
		allTools = append(allTools, &signal.AskUserQuestionTool{}, &signal.PresentPlanTool{})
	}
	for _, tool := range allTools {
		r.Register(tool)
	}
	if r.asyncShell && r.permissionMode.AllowsWriteTools() {
		manager := shell.NewAsyncManager(r.bashExecutor())
		r.asyncShellManager = manager
		r.Register(&shell.BashStartTool{Manager: manager, Mode: r.permissionMode})
		r.Register(&shell.BashPollTool{Manager: manager})
		r.Register(&shell.BashKillTool{Manager: manager})
	}
	if r.interactiveTerminal && r.permissionMode.AllowsWriteTools() {
		manager := shell.NewTerminalManager(r.bashExecutor())
		r.terminalManager = manager
		r.Register(&shell.TerminalTool{Manager: manager, Mode: r.permissionMode})
	}
	if r.thinkTool {
		r.Register(&signal.ThinkTool{})
	}
	if r.browser {
		r.Register(&browser.Tool{AllowPrivateNetworkURLs: r.allowPrivateNetworkURLs})
	}
	if r.visionTool != nil {
		r.visionTool.AllowPrivateNetworkURLs = r.allowPrivateNetworkURLs
		r.Register(r.visionTool)
	}
	if r.memoryTool != nil {
		r.Register(r.memoryTool)
	}
	if r.attachRepositoryTool != nil {
		r.Register(r.attachRepositoryTool)
	}
	return r
}

func (r *Registry) bashExecutor() sandbox.Executor {
	if r == nil || r.commandSandboxConfig == nil {
		return nil
	}
	return sandbox.DefaultWithConfig(*r.commandSandboxConfig)
}

// WorkDir returns the workspace root the registry was constructed with.
// Tools that need a workspace at construction time (rather than per-Execute)
// can read this value via the registry.
func (r *Registry) WorkDir() string {
	if r == nil {
		return ""
	}
	return r.workDir
}

// Register adds a tool to the registry.
func (r *Registry) Register(tool agentsdk.Tool) {
	if tool == nil {
		return
	}
	if !r.permissionMode.AllowsWriteTools() {
		if adapter, ok := tool.(agentsdk.ToolAccessAdapter); ok {
			if adapted := adapter.ToolForAccess(agentsdk.ToolAccessLevelReadOnly); adapted != nil {
				tool = adapted
			}
		}
	}
	if !tool.IsReadOnly() && !isRegistryControlFlowTool(tool.Name()) && !r.permissionMode.AllowsWriteTools() {
		if _, ok := r.allowMutating[tool.Name()]; !ok {
			return
		}
	}
	r.tools[tool.Name()] = tool
}

func isRegistryControlFlowTool(name string) bool {
	switch name {
	case "finish", "set_phase", "save_plan", "get_plan", "RequestMCPBreakGlass":
		return true
	}
	return false
}

func (r *Registry) Get(name string) agentsdk.Tool {
	return r.tools[name]
}

func (r *Registry) Tools() []agentsdk.Tool {
	out := make([]agentsdk.Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		out = append(out, tool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func (r *Registry) Closers() []io.Closer {
	if r == nil {
		return nil
	}
	var closers []io.Closer
	if r.asyncShellManager != nil {
		closers = append(closers, r.asyncShellManager)
	}
	if r.terminalManager != nil {
		closers = append(closers, r.terminalManager)
	}
	return closers
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) ToolSummaries() string {
	skipTools := map[string]bool{
		"bash": true, "read": true, "write": true, "edit": true,
		"list_directory": true,
	}
	type entry struct {
		name string
		desc string
	}
	var entries []entry
	for name, tool := range r.tools {
		if skipTools[strings.ToLower(name)] {
			continue
		}
		desc := tool.Description()
		if desc == "" {
			continue
		}
		if len(desc) > 200 {
			desc = desc[:197] + "..."
		}
		entries = append(entries, entry{name, desc})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, "- **"+e.name+"**: "+e.desc)
	}
	return strings.Join(lines, "\n")
}

func (r *Registry) PermissionMode() policy.PermissionMode {
	if r == nil {
		return policy.PermissionModeWorkspaceWrite
	}
	return policy.NormalizePermissionMode(string(r.permissionMode))
}
