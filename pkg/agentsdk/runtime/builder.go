package runtime

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/guardrails"
	sdkmcp "github.com/gratefulagents/sdk/pkg/agentsdk/mcp"
	sdkmode "github.com/gratefulagents/sdk/pkg/agentsdk/mode"
	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	sdkprojectstate "github.com/gratefulagents/sdk/pkg/agentsdk/projectstate"
	sdkproviders "github.com/gratefulagents/sdk/pkg/agentsdk/providers"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
	sdksandbox "github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
	sdktools "github.com/gratefulagents/sdk/pkg/agentsdk/tools"
	sdkprojectstatetools "github.com/gratefulagents/sdk/pkg/agentsdk/tools/projectstate"
	sdksignal "github.com/gratefulagents/sdk/pkg/agentsdk/tools/signal"
	sdkvision "github.com/gratefulagents/sdk/pkg/agentsdk/tools/vision"
)

// Config is the SDK-native runtime builder input. Hosts should populate this
// from flags, CRDs, files, or environment variables, then provide only the
// remaining platform adapters.
type Config struct {
	Provider                 string
	DefaultProvider          string
	Model                    string
	BaseURL                  string
	APIKey                   string
	AuthMode                 string
	APIMode                  string
	OpenAIOAuthPath          string
	OpenAIOAuthAccountID     string
	OpenAIOAuthAccountIDPath string
	OpenAIAuthSession        *sdkopenai.AuthSession
	ProviderAPIKeys          map[string]string
	ProviderBaseURLs         map[string]string
	ProviderAPIModes         map[string]string
	// Routes declares named provider instances registered under arbitrary
	// routing prefixes, letting the same base provider be exposed under
	// multiple prefixes with independent auth (e.g. "anthropic" via API key and
	// "anthropic-oauth" via OAuth). Callers select the auth by model prefix.
	Routes []sdkproviders.ProviderRoute
	// ModelFallbacks is an ordered list of fallback model identifiers used by
	// OpenAI-compatible providers (e.g. OpenRouter) to retry the next model when
	// one is unavailable. Empty disables fallback routing.
	ModelFallbacks []string
	// FallbackModels is an ordered list of SDK-level fallback model identifiers.
	// When a model call fails with a rate-limit or quota-style error, the runner
	// retries the request through the next model, including cross-provider models
	// such as "anthropic/claude-sonnet-4-6" or "copilot/gpt-4.1".
	FallbackModels []string

	WorkDir      string
	AgentName    string
	Instructions string
	SessionMode  agentsdk.SessionMode
	ActiveMode   string
	ActivePhase  string
	ModeSnapshot *sdkmode.TemplateSpec
	RoleCatalog  agentsdk.RoleCatalog

	Reasoning              string
	Verbosity              string
	ModelSettings          *agentsdk.ModelSettings
	MaxTokens              int
	MaxTurns               int
	SubAgentMaxTurns       int
	MaxConcurrentSubAgents int
	ToolTimeout            int
	ToolAccess             agentsdk.ToolAccessLevel
	PermissionMode         policy.PermissionMode
	CommandSandboxConfig   *sdksandbox.Config
	Features               *Features

	EnableTools              bool
	EnableMCP                bool
	EnableHandoffs           bool
	EnableSubAgents          bool
	EnableGuardrails         bool
	EnableCompaction         bool
	EnableApproval           bool
	EnableRetry              bool
	EnableAsyncShell         bool
	ForceFinalSummary        bool
	FinalCheckInstructions   string
	Debug                    bool
	AllowPrivateNetworkURLs  bool
	DisableWebTools          bool
	OutputSchema             *agentsdk.OutputSchema
	MCPConfig                *sdkmcp.Config
	ExtraTools               []agentsdk.Tool
	DisableDefaultTools      bool
	DisableSignalTools       bool
	ToolInputRules           []agentsdk.ToolInputGuardrail
	ToolOutputRules          []agentsdk.ToolOutputGuardrail
	EventWriter              io.Writer
	EventSession             int
	TracingProcessor         agentsdk.TracingProcessor
	WorkingStateText         string
	FeatureSummary           string
	ModeDirectiveText        string
	EnableProjectState       bool
	ProjectID                string
	ProjectStateDir          string
	ProjectStateActor        string
	ProjectStateActiveTaskID string
	ProjectStateStore        sdkprojectstate.Store

	Trace                *agentsdk.Trace
	ParentSpanID         string
	ImmediateInputPoller agentsdk.ImmediateInputPoller
	CompactionConfig     *agentsdk.CompactionConfig
	// CompactionModelResolver, when set, resolves per-model compaction
	// thresholds (typically from authoritative provider /models metadata with a
	// static fallback) so each agent — including sub-agents on different models
	// than the parent — compacts at its own model's context window. When nil the
	// builder synthesizes a default resolver from OpenAIAuthSession + BaseURL,
	// falling back to the static CompactionDefaultsForModel table.
	CompactionModelResolver   agentsdk.CompactionModelResolver
	CompactionRecorder        func(tokensBefore, tokensAfter int, summary string)
	CompactionFailureReporter func(scope, reason string, tokensBefore, tokensAfter int)
	CompactionCarryForward    func(context.Context) string
	HandoffHistory            *agentsdk.HandoffHistoryConfig
	SessionState              *SessionState

	// SpecialistOutputExtractor, when set, post-processes each specialist
	// sub-agent's RunResult before its text is returned to the parent agent.
	// Use it to return a compact or structured view of specialist work.
	SpecialistOutputExtractor func(*agentsdk.RunResult) string
}

// ToolBundle is the SDK-built host-neutral tool set.
type ToolBundle struct {
	Tools      []agentsdk.Tool
	MCPServers []string
	Closers    []io.Closer
}

// Bundle is the complete runnable runtime produced by Builder.
type Bundle struct {
	Runner           *agentsdk.Runner
	Agent            *agentsdk.Agent
	Config           agentsdk.RunConfig
	Tracker          *agentsdk.ProgressTracker
	Tools            []agentsdk.Tool
	SpecialistTools  []agentsdk.Tool
	SpecialistAgents map[string]*agentsdk.Agent
	SessionState     *SessionState
	Closers          []io.Closer
}

// SessionState holds SDK runtime state that should survive multiple runtime
// builds for one host session. Reuse the same SessionState across user turns
// when managed async sub-agent tasks should remain listable, waitable, and
// collectible across host rebuilds.
type SessionState struct {
	mu                sync.Mutex
	subAgentScheduler *agentsdk.SubAgentScheduler
}

func NewSessionState() *SessionState {
	return &SessionState{}
}

func (s *SessionState) SubAgentScheduler() *agentsdk.SubAgentScheduler {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subAgentScheduler
}

func (s *SessionState) configureSubAgentScheduler(cfg agentsdk.SubAgentSchedulerConfig) *agentsdk.SubAgentScheduler {
	if s == nil {
		return agentsdk.NewSubAgentScheduler(cfg)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subAgentScheduler == nil {
		s.subAgentScheduler = agentsdk.NewSubAgentScheduler(cfg)
	} else {
		agentsdk.ConfigureSubAgentScheduler(s.subAgentScheduler, cfg)
	}
	return s.subAgentScheduler
}

// ModeOverrides are runtime settings derived from a mode template snapshot.
type ModeOverrides struct {
	Model                  string
	FallbackModels         []string
	Reasoning              string
	ModelSettings          agentsdk.ModelSettings
	MaxTurns               int
	SubAgentMaxTurns       int
	MaxConcurrentSubAgents int
	ModeInstructions       string
}

type Builder struct {
	cfg    Config
	status func(string)
	log    func(string)
}

type Option func(*Builder)

func WithStatusFunc(fn func(string)) Option {
	return func(b *Builder) { b.status = fn }
}

func WithLogFunc(fn func(string)) Option {
	return func(b *Builder) { b.log = fn }
}

func NewBuilder(cfg Config, opts ...Option) *Builder {
	b := &Builder{cfg: cfg}
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	return b
}

func (b *Builder) Build(ctx context.Context) (*Bundle, error) {
	if b == nil {
		return nil, fmt.Errorf("runtime builder is nil")
	}
	cfg := b.cfg.normalized()
	features := resolveFeatures(cfg)
	if features.value.ProjectState.needsStore() {
		b.emitStatus("loading project state")
		store, err := b.projectStateStore(ctx, cfg)
		if err != nil {
			return nil, err
		}
		cfg.ProjectStateStore = store
		if features.value.ProjectState.PrimeContext {
			b.emitStatus("priming project state")
			if prime, err := store.PrimeContext(ctx, sdkprojectstate.PrimeOptions{
				Actor:        projectStateActor(cfg),
				ActiveTaskID: cfg.ProjectStateActiveTaskID,
				ReadyLimit:   8,
				MemoryLimit:  8,
			}); err != nil {
				b.emitLog("project state warning: " + err.Error())
			} else if strings.TrimSpace(prime) != "" {
				cfg.WorkingStateText = strings.Join(nonEmptyStrings(cfg.WorkingStateText, prime), "\n\n")
			}
		}
	}

	b.emitStatus("building runner")
	runner, err := BuildRunner(cfg)
	if err != nil {
		return nil, err
	}

	tracker := agentsdk.NewProgressTracker(agentsdk.WithTracingProcessor(tracingProcessor(cfg, features)))
	tracker.SetSession(sessionNumber(cfg), runtimePhase(cfg))

	var toolBundle ToolBundle
	if shouldBuildToolBundle(features) {
		b.emitStatus("initializing tools")
		toolBundle, err = BuildToolBundle(ctx, cfg)
		if err != nil {
			b.emitLog("tool setup warning: " + err.Error())
		}
	}

	agent, agentTools, specialistTools, specialistAgents := BuildAgentWithSpecialists(cfg, runner, toolBundle)

	hooks := agentsdk.NewPlatformHooks(tracker, nil)
	var eventStream *agentsdk.EventStream
	if cfg.EventWriter != nil && features.value.Runtime.EventStream {
		eventStream = agentsdk.NewSessionEventStream(cfg.EventWriter, agentsdk.SessionEventStreamOptions{
			Session: sessionNumber(cfg),
			Phase:   runtimePhase(cfg),
		})
		hooks = agentsdk.NewPlatformHooks(tracker, eventStream)
	}
	runner.DefaultHooks = hooks

	state := cfg.SessionState
	if state == nil {
		state = NewSessionState()
	}
	if features.value.SubAgents.asyncEnabled() {
		agentTools = attachAsyncSubAgentTools(cfg, state, runner, tracker, eventStream, agent, agentTools, specialistAgents)
	}
	names := toolNames(agentTools)
	if eventStream != nil {
		eventStream.EmitSystemInit(cfg.Model, string(permissionMode(cfg)), cfg.WorkDir, cfg.MaxTurns, names, agent.MCPServers)
	}
	tracker.RecordSystemInit(cfg.Model, cfg.WorkDir, names, cfg.MaxTurns, agent.MCPServers, permissionMode(cfg))

	runCfg := BuildRunConfig(cfg, hooks)
	return &Bundle{
		Runner:           runner,
		Agent:            agent,
		Config:           runCfg,
		Tracker:          tracker,
		Tools:            agentTools,
		SpecialistTools:  specialistTools,
		SpecialistAgents: specialistAgents,
		SessionState:     state,
		Closers:          toolBundle.Closers,
	}, nil
}

func (b *Builder) projectStateStore(ctx context.Context, cfg Config) (sdkprojectstate.Store, error) {
	if cfg.ProjectStateStore != nil {
		return cfg.ProjectStateStore, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	store, err := sdkprojectstate.NewFilesystemStore(sdkprojectstate.FilesystemOptions{
		StateDir:  cfg.ProjectStateDir,
		ProjectID: cfg.ProjectID,
		WorkDir:   cfg.WorkDir,
		Actor:     projectStateActor(cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("initialize project state: %w", err)
	}
	return store, nil
}

func (b *Builder) emitStatus(text string) {
	if b.status != nil {
		b.status(text)
	}
}

func (b *Builder) emitLog(text string) {
	if b.log != nil {
		b.log(text)
	}
}

func BuildRunner(cfg Config) (*agentsdk.Runner, error) {
	return sdkproviders.NewRunnerFromConfig(ProviderSpec(cfg))
}

func ProviderSpec(cfg Config) sdkproviders.ProviderSpec {
	provider := strings.TrimSpace(cfg.Provider)
	if provider == "" {
		provider = "openai"
	}
	authMode := strings.TrimSpace(cfg.AuthMode)
	if authMode == "" {
		authMode = string(sdkopenai.AuthModeAPIKey)
	}
	return sdkproviders.ProviderSpec{
		Provider:                 provider,
		DefaultProvider:          cfg.DefaultProvider,
		Model:                    cfg.Model,
		BaseURL:                  cfg.BaseURL,
		APIKey:                   cfg.APIKey,
		AuthMode:                 authMode,
		APIMode:                  cfg.APIMode,
		OpenAIOAuthPath:          cfg.OpenAIOAuthPath,
		OpenAIOAuthAccountID:     cfg.OpenAIOAuthAccountID,
		OpenAIOAuthAccountIDPath: strings.TrimSpace(cfg.OpenAIOAuthAccountIDPath),
		OpenAIAuthSession:        cfg.OpenAIAuthSession,
		ProviderAPIKeys:          cloneStringMap(cfg.ProviderAPIKeys),
		ProviderBaseURLs:         cloneStringMap(cfg.ProviderBaseURLs),
		ProviderAPIModes:         cloneStringMap(cfg.ProviderAPIModes),
		ModelFallbacks:           append([]string(nil), cfg.ModelFallbacks...),
		Routes:                   append([]sdkproviders.ProviderRoute(nil), cfg.Routes...),
	}
}

func BuildToolBundle(ctx context.Context, cfg Config) (ToolBundle, error) {
	mode := permissionMode(cfg)
	features := resolveFeatures(cfg)
	bundle := ToolBundle{}
	if features.value.ProjectState.needsStore() && cfg.ProjectStateStore == nil {
		select {
		case <-ctx.Done():
			return bundle, ctx.Err()
		default:
		}
		store, err := sdkprojectstate.NewFilesystemStore(sdkprojectstate.FilesystemOptions{
			StateDir:  cfg.ProjectStateDir,
			ProjectID: cfg.ProjectID,
			WorkDir:   cfg.WorkDir,
			Actor:     projectStateActor(cfg),
		})
		if err != nil {
			return bundle, fmt.Errorf("initialize project state: %w", err)
		}
		cfg.ProjectStateStore = store
	}
	if features.value.Tools.hasRegistryTools() {
		registryOptions := []sdktools.RegistryOption{
			sdktools.WithPermissionMode(mode),
			sdktools.WithPrivateNetworkURLs(cfg.AllowPrivateNetworkURLs),
		}
		if cfg.CommandSandboxConfig != nil {
			registryOptions = append(registryOptions, sdktools.WithCommandSandboxConfig(*cfg.CommandSandboxConfig))
		}
		if features.value.Tools.AsyncShell {
			registryOptions = append(registryOptions, sdktools.WithAsyncShellTools())
		}
		if !features.value.Tools.WebFetch {
			registryOptions = append(registryOptions, sdktools.WithoutWebTools())
		}
		registry := sdktools.NewRegistry(cfg.WorkDir, registryOptions...)
		bundle.Tools = append(bundle.Tools, filterNamedTools(registry.Tools(), registryToolNames(features.value.Tools))...)
		bundle.Closers = append(bundle.Closers, registry.Closers()...)
	}
	if features.value.Tools.hasSignals() {
		bundle.Tools = append(bundle.Tools, signalTools(cfg, features.value.Tools.Signals)...)
	}
	if cfg.ProjectStateStore != nil && (features.value.ProjectState.TaskTools || features.value.ProjectState.MemoryTools || features.value.ProjectState.PrimeTool) {
		bundle.Tools = append(bundle.Tools, projectStateTools(cfg, features.value.ProjectState)...)
	}
	if features.value.Tools.ExtraTools {
		bundle.Tools = append(bundle.Tools, cfg.ExtraTools...)
	}
	if features.value.Tools.VisionAnalyzer {
		bundle.Tools = attachOpenAIVisionAnalyzer(cfg, bundle.Tools)
	}

	if features.value.MCP.Enabled && features.value.MCP.hasServerSelection() && features.value.MCP.hasToolSelection() {
		var manager *sdkmcp.Manager
		var err error
		managerOptions := []sdkmcp.ManagerOption{sdkmcp.WithPermissionMode(mode)}
		if cfg.CommandSandboxConfig != nil {
			managerOptions = append(managerOptions, sdkmcp.WithCommandExecutor(sdksandbox.DefaultWithConfig(*cfg.CommandSandboxConfig)))
		}
		manager, err = buildMCPManager(ctx, cfg, features.value.MCP, managerOptions...)
		if manager != nil {
			bundle.Tools = append(bundle.Tools, filterMCPTools(sdkmcp.BuildTools(manager), features.value.MCP)...)
			bundle.MCPServers = manager.ConnectedServerNames()
			bundle.Closers = append(bundle.Closers, manager)
		}
		return bundle, err
	}
	return bundle, nil
}

type openAIVisionModel interface {
	AnalyzeImageWithDetail(ctx context.Context, imageData []byte, mimeType, prompt, detail string) (string, error)
}

func attachOpenAIVisionAnalyzer(cfg Config, tools []agentsdk.Tool) []agentsdk.Tool {
	if !openAIVisionEligible(cfg) {
		return tools
	}
	analyzeFn := openAIVisionAnalyzeFn(cfg)
	for _, tool := range tools {
		visionTool, ok := tool.(*sdkvision.Tool)
		if !ok || visionTool == nil {
			continue
		}
		if visionTool.AnalyzeFn != nil || visionTool.AnalyzeWithDetailFn != nil {
			continue
		}
		visionTool.AnalyzeWithDetailFn = analyzeFn
	}
	return tools
}

func openAIVisionEligible(cfg Config) bool {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = "openai"
	}
	return provider == "openai" || provider == "multi"
}

func openAIVisionAnalyzeFn(cfg Config) sdkvision.AnalyzeWithDetailFn {
	spec := ProviderSpec(cfg)
	modelName := sdkopenai.DefaultChatModel
	if strings.EqualFold(strings.TrimSpace(spec.Provider), "multi") {
		modelName = "openai/" + modelName
	}

	var once sync.Once
	var analyzer openAIVisionModel
	var initErr error
	return func(ctx context.Context, imageData []byte, mimeType, prompt, detail string) (string, error) {
		once.Do(func() {
			provider, err := sdkproviders.NewProviderFromConfig(spec)
			if err != nil {
				initErr = fmt.Errorf("initialize OpenAI vision provider: %w", err)
				return
			}
			model, err := provider.GetModel(modelName)
			if err != nil {
				initErr = fmt.Errorf("initialize OpenAI vision model %s: %w", modelName, err)
				return
			}
			var ok bool
			analyzer, ok = model.(openAIVisionModel)
			if !ok {
				initErr = fmt.Errorf("model %s does not support image analysis", modelName)
			}
		})
		if initErr != nil {
			return "", initErr
		}
		return analyzer.AnalyzeImageWithDetail(ctx, imageData, mimeType, prompt, detail)
	}
}

func BuildAgent(cfg Config, runner *agentsdk.Runner, hostBundle ToolBundle) (*agentsdk.Agent, []agentsdk.Tool) {
	agent, tools, _, _ := BuildAgentWithSpecialists(cfg, runner, hostBundle)
	return agent, tools
}

func BuildAgentWithSpecialists(cfg Config, runner *agentsdk.Runner, hostBundle ToolBundle) (*agentsdk.Agent, []agentsdk.Tool, []agentsdk.Tool, map[string]*agentsdk.Agent) {
	cfg = cfg.normalized()
	features := resolveFeatures(cfg)
	var tools []agentsdk.Tool
	var specialistTools []agentsdk.Tool
	var specialistAgents map[string]*agentsdk.Agent
	if parentToolSurfaceEnabled(features) {
		tools = append(tools, cloneTools(hostBundle.Tools)...)
	}
	if features.value.SubAgents.enabled() {
		catalog := cfg.RoleCatalog
		if len(catalog) == 0 && features.value.SubAgents.GenericFallback {
			catalog = agentsdk.RoleCatalog{{
				Name:        "agent",
				Description: "Generic sub-agent. Instructions come entirely from the parent's prompt.",
				ToolAccess:  "full",
			}}
		}
		modeSnapshot := cfg.ModeSnapshot
		if !features.value.Modes.ModelRouting {
			modeSnapshot = nil
		}
		specialists := agentsdk.BuildSpecialistToolsFromCatalog(catalog, agentsdk.SpecialistBuildOptions{
			Runner:            runner,
			Tools:             cloneTools(hostBundle.Tools),
			BaseModel:         cfg.Model,
			FallbackModels:    cloneStrings(cfg.FallbackModels),
			Provider:          modelRoutingProvider(cfg),
			BaseModelSettings: modelSettings(cfg),
			ModeSnapshot:      modeSnapshot,
			OutputExtractor:   cfg.SpecialistOutputExtractor,
		})
		specialistAgents = specialists.Agents
		if features.value.SubAgents.SyncTools {
			specialistTools = cloneTools(specialists.Tools)
			tools = append(tools, specialistTools...)
		}
	}

	agent := &agentsdk.Agent{
		Name:           firstNonEmpty(cfg.AgentName, "agent"),
		Model:          cfg.Model,
		FallbackModels: cloneStrings(cfg.FallbackModels),
		ModelSettings:  modelSettings(cfg),
		Tools:          tools,
		MCPServers:     cloneStrings(hostBundle.MCPServers),
		InstructionsFn: func(_ *agentsdk.RunContext, a *agentsdk.Agent) string {
			blocks := []string{
				cfg.Instructions,
				agentsdk.BuildDelegationGuide(a),
			}
			blocks = append(blocks, modeInstructionBlocks(cfg, features)...)
			blocks = append(blocks,
				runtimeWorkspaceContext(cfg, a.Tools, features),
				agentsdk.BuildRunBudgetContext(cfg.MaxTurns),
			)
			return strings.Join(nonEmptyStrings(blocks...), "\n\n")
		},
	}
	if cfg.OutputSchema != nil {
		agent.OutputType = cfg.OutputSchema
	}
	if features.value.Handoffs.Enabled {
		agent.Handoffs = buildCatalogHandoffs(cfg, specialistAgents, features.value.Handoffs)
	}
	return agent, tools, specialistTools, specialistAgents
}

// buildCatalogHandoffs creates one handoff per catalog specialist so the parent
// agent can transfer the conversation to a specialist that owns the request.
// Targets are resolved from the already-built specialist agents (which carry
// the role's instructions and HandoffDescription). When no catalog specialists
// are available, it falls back to a single generic handoff specialist.
func buildCatalogHandoffs(cfg Config, specialistAgents map[string]*agentsdk.Agent, features HandoffFeatures) []*agentsdk.Handoff {
	var handoffs []*agentsdk.Handoff
	seen := map[string]bool{}
	for _, role := range cfg.RoleCatalog {
		name := strings.TrimSpace(role.Name)
		if name == "" || seen[name] {
			continue
		}
		target := specialistAgents[name]
		if target == nil {
			continue
		}
		seen[name] = true
		description := strings.TrimSpace(role.Description)
		if description == "" {
			description = "Transfer the conversation to the " + name + " specialist."
		}
		handoffs = append(handoffs, agentsdk.NewHandoff(
			target,
			agentsdk.WithToolName("transfer_to_"+sanitizeHandoffName(name)),
			agentsdk.WithDescription(description),
			agentsdk.WithInputFilter(agentsdk.RemoveAllToolsHandoffInputFilter),
		))
	}
	if len(handoffs) > 0 {
		return handoffs
	}
	if !features.GenericFallback {
		return nil
	}
	specialist := &agentsdk.Agent{
		Name:               "specialist",
		Model:              cfg.Model,
		Instructions:       agentsdk.WithRecommendedHandoffInstructions("You are the handoff specialist. Resolve the delegated request and explain the result briefly."),
		HandoffDescription: "Specialist for handoff requests.",
		ModelSettings:      modelSettings(cfg),
	}
	return []*agentsdk.Handoff{
		agentsdk.NewHandoff(
			specialist,
			agentsdk.WithToolName("transfer_to_specialist"),
			agentsdk.WithDescription("Transfer to a specialist agent."),
			agentsdk.WithInputFilter(agentsdk.RemoveAllToolsHandoffInputFilter),
		),
	}
}

// sanitizeHandoffName converts a role name into a tool-name-safe suffix.
func sanitizeHandoffName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "specialist"
	}
	return out
}

func attachAsyncSubAgentTools(cfg Config, state *SessionState, runner *agentsdk.Runner, tracker *agentsdk.ProgressTracker, eventStream *agentsdk.EventStream, agent *agentsdk.Agent, agentTools []agentsdk.Tool, specialistAgents map[string]*agentsdk.Agent) []agentsdk.Tool {
	features := resolveFeatures(cfg)
	if agent == nil || len(specialistAgents) == 0 {
		return agentTools
	}
	scheduler := state.configureSubAgentScheduler(agentsdk.SubAgentSchedulerConfig{
		MaxConcurrent:           cfg.MaxConcurrentSubAgents,
		Runner:                  runner,
		Agents:                  specialistAgents,
		Tracker:                 tracker,
		EventStream:             eventStream,
		WorkDir:                 cfg.WorkDir,
		ToolAccessLevel:         cfg.ToolAccess,
		ToolPolicy:              toolPolicy(cfg),
		CompactionConfig:        runCompactionConfig(cfg),
		CompactionModelResolver: compactionModelResolver(cfg),
		MaxTurns:                cfg.SubAgentMaxTurns,
	})
	asyncTools := filterNamedTools(agentsdk.BuildSubAgentTaskTools(scheduler, defaultAsyncSubAgent(specialistAgents)), asyncSubAgentToolNames(features.value.SubAgents.Async))
	agent.Tools = append(agent.Tools, asyncTools...)
	return append(agentTools, asyncTools...)
}

func defaultAsyncSubAgent(agents map[string]*agentsdk.Agent) string {
	if _, ok := agents["agent"]; ok {
		return "agent"
	}
	names := make([]string, 0, len(agents))
	for name := range agents {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func projectStateActor(cfg Config) string {
	return firstNonEmpty(cfg.ProjectStateActor, cfg.AgentName, "agent")
}

func BuildRunConfig(cfg Config, hooks agentsdk.RunHooks) agentsdk.RunConfig {
	cfg = cfg.normalized()
	features := resolveFeatures(cfg)
	return agentsdk.RunConfig{
		MaxTurns:         cfg.MaxTurns,
		SubAgentMaxTurns: cfg.SubAgentMaxTurns,
		Hooks:            hooks,
		ModelSettings:    runModelSettings(cfg),
		FallbackModels:   cloneStrings(cfg.FallbackModels),
		TracingProcessor: tracingProcessor(cfg, features),
		Trace:            cfg.Trace,
		ParentSpanID:     cfg.ParentSpanID,

		ImmediateInputPoller:      immediateInputPoller(cfg, features),
		CompactionConfig:          runCompactionConfig(cfg),
		CompactionModelResolver:   compactionModelResolver(cfg),
		CompactionRecorder:        runCompactionRecorder(cfg),
		CompactionFailureReporter: cfg.CompactionFailureReporter,
		CompactionCarryForward:    runCompactionCarryForward(cfg),
		HandoffHistory:            runHandoffHistory(cfg),

		ToolInputGuardrails:    toolInputGuardrails(cfg),
		ToolOutputGuardrails:   toolOutputGuardrails(cfg),
		WorkDir:                cfg.WorkDir,
		RetryPolicy:            retryPolicy(cfg),
		AdditionalInstructions: additionalInstructions(cfg),
		WorkingStateContext:    firstNonEmpty(cfg.WorkingStateText, "Runtime state is maintained by the host adapter."),
		ToolAccessLevel:        cfg.ToolAccess,
		Phase:                  runtimePhase(cfg),
		ToolPolicy:             toolPolicy(cfg),
		MaxConcurrentSubAgents: cfg.MaxConcurrentSubAgents,
		ForceFinalSummaryTurn:  features.value.Runtime.ForceFinalSummary,
		Debug:                  cfg.Debug,
		UntrustedToolOutputs:   untrustedToolOutputs(features),
	}
}

func ModeOverridesFromSnapshot(spec *sdkmode.TemplateSpec, instructionsOverride string) ModeOverrides {
	if spec == nil {
		return ModeOverrides{}
	}

	var out ModeOverrides
	if spec.ModelRouting != nil {
		if strings.TrimSpace(spec.ModelRouting.DefaultModel) != "" {
			out.Model = strings.TrimSpace(spec.ModelRouting.DefaultModel)
		}
		out.FallbackModels = cloneStrings(spec.ModelRouting.FallbackModels)
		if strings.TrimSpace(spec.ModelRouting.ReasoningLevel) != "" {
			out.Reasoning = strings.TrimSpace(spec.ModelRouting.ReasoningLevel)
			out.ModelSettings = out.ModelSettings.Merge(agentsdk.ModeRoutingSettings(out.Reasoning, ""))
		}
		if strings.TrimSpace(spec.ModelRouting.TextVerbosity) != "" {
			out.ModelSettings = out.ModelSettings.Merge(agentsdk.ModeRoutingSettings("", strings.TrimSpace(spec.ModelRouting.TextVerbosity)))
		}
	}

	if spec.Constraints != nil {
		out.MaxTurns = spec.Constraints.MaxTurns
		out.SubAgentMaxTurns = spec.Constraints.SubAgentMaxTurns
		out.MaxConcurrentSubAgents = spec.Constraints.MaxConcurrentSubAgents
	}

	out.ModeInstructions = strings.TrimSpace(instructionsOverride)
	if out.ModeInstructions == "" {
		out.ModeInstructions = strings.TrimSpace(spec.Instructions)
	}
	return out
}

func (cfg Config) normalized() Config {
	if strings.TrimSpace(cfg.Provider) == "" {
		cfg.Provider = "openai"
	}
	if strings.TrimSpace(cfg.DefaultProvider) == "" {
		cfg.DefaultProvider = defaultProviderFromModel(cfg.Model)
		if strings.TrimSpace(cfg.DefaultProvider) == "" {
			if strings.EqualFold(strings.TrimSpace(cfg.Provider), "multi") {
				cfg.DefaultProvider = "openai"
			} else {
				cfg.DefaultProvider = cfg.Provider
			}
		}
	}
	if strings.TrimSpace(cfg.AgentName) == "" {
		cfg.AgentName = "agent"
	}
	if cfg.SessionMode == "" {
		cfg.SessionMode = agentsdk.SessionModeChat
	}
	if strings.TrimSpace(cfg.Reasoning) == "" {
		cfg.Reasoning = string(agentsdk.ReasoningMedium)
	}
	if strings.TrimSpace(cfg.Verbosity) == "" {
		cfg.Verbosity = string(agentsdk.TextVerbosityMedium)
	}
	if cfg.ToolAccess == "" {
		cfg.ToolAccess = agentsdk.ToolAccessLevelFull
	}
	cfg.ToolAccess = agentsdk.NormalizeToolAccessLevel(cfg.ToolAccess)
	if cfg.PermissionMode == "" {
		cfg.PermissionMode = permissionModeFromAccess(cfg.ToolAccess)
	}
	return cfg
}

func modelRoutingProvider(cfg Config) string {
	provider := strings.TrimSpace(cfg.Provider)
	if strings.EqualFold(provider, "multi") {
		if defaultProvider := strings.TrimSpace(cfg.DefaultProvider); defaultProvider != "" {
			return defaultProvider
		}
	}
	return provider
}

func defaultProviderFromModel(model string) string {
	prefix, _ := agentsdk.ParseModelPrefix(model)
	return strings.TrimSpace(prefix)
}

func signalTools(cfg Config, features SignalFeatures) []agentsdk.Tool {
	var tools []agentsdk.Tool
	if features.AskUserQuestion {
		tools = append(tools, &sdksignal.AskUserQuestionTool{})
	}
	if features.PresentPlan {
		tools = append(tools, &sdksignal.PresentPlanTool{})
	}
	if features.Finish {
		tools = append(tools, &sdksignal.FinishTool{})
	}
	if features.SetPhase {
		tools = append(tools, &sdksignal.SetPhaseTool{
			Phases:        signalPhaseOptions(cfg),
			CurrentPhase:  cfg.ActivePhase,
			PauseOnChange: true,
		})
	}
	return tools
}

func signalPhaseOptions(cfg Config) []sdksignal.PhaseOption {
	if cfg.ModeSnapshot == nil || len(cfg.ModeSnapshot.Phases) == 0 {
		return nil
	}
	out := make([]sdksignal.PhaseOption, 0, len(cfg.ModeSnapshot.Phases))
	for _, phase := range cfg.ModeSnapshot.Phases {
		out = append(out, sdksignal.PhaseOption{
			ID:               phase.ID,
			ReadOnly:         phase.ReadOnly,
			RequiresApproval: phase.RequiresApproval,
			Description:      phase.Description,
		})
	}
	return out
}

func parentToolSurfaceEnabled(features resolvedFeatures) bool {
	f := features.value
	return f.Tools.hasRegistryTools() ||
		f.Tools.hasSignals() ||
		f.Tools.ExtraTools ||
		(f.MCP.Enabled && f.MCP.hasServerSelection() && f.MCP.hasToolSelection()) ||
		f.ProjectState.TaskTools ||
		f.ProjectState.MemoryTools ||
		f.ProjectState.PrimeTool
}

func registryToolNames(features ToolFeatures) map[string]bool {
	names := map[string]bool{}
	add := func(enabled bool, values ...string) {
		if !enabled {
			return
		}
		for _, value := range values {
			names[value] = true
		}
	}
	add(features.ListFiles, "list_files")
	add(features.ReadFile, "read_file")
	add(features.Glob, "glob")
	add(features.Grep, "grep")
	add(features.LSP, "LSP")
	add(features.Bash, "Bash")
	add(features.Write, "Write")
	add(features.Edit, "Edit")
	add(features.WebFetch, "WebFetch")
	add(features.AsyncShell, "BashStart", "BashPoll", "BashKill")
	return names
}

func projectStateTools(cfg Config, features ProjectStateFeatures) []agentsdk.Tool {
	allowed := map[string]bool{}
	if features.TaskTools {
		for _, name := range []string{
			"task_create",
			"task_ready",
			"task_show",
			"task_update",
			"task_claim",
			"task_close",
			"task_comment",
			"task_link",
		} {
			allowed[name] = true
		}
	}
	if features.MemoryTools {
		for _, name := range []string{
			"memory_remember",
			"memory_recall",
			"memory_list",
			"memory_update",
			"memory_delete",
			"memory_stats",
		} {
			allowed[name] = true
		}
	}
	if features.PrimeTool {
		allowed["prime_context"] = true
	}
	return filterNamedTools(sdkprojectstatetools.Tools(cfg.ProjectStateStore, projectStateActor(cfg)), allowed)
}

func asyncSubAgentToolNames(features AsyncSubAgentFeatures) map[string]bool {
	names := map[string]bool{}
	add := func(enabled bool, name string) {
		if enabled {
			names[name] = true
		}
	}
	add(features.Spawn, "spawn_subagent_task")
	add(features.Run, "run_subagent_task")
	add(features.Graph, "spawn_subagent_graph")
	add(features.List, "list_subagent_tasks")
	add(features.Status, "get_subagent_task_status")
	add(features.Activity, "get_subagent_activity")
	add(features.TaskGraph, "get_subagent_task_graph")
	add(features.Message, "send_message_to_subagent_task")
	add(features.Cancel, "cancel_subagent_task")
	return names
}

func filterNamedTools(tools []agentsdk.Tool, allowed map[string]bool) []agentsdk.Tool {
	if len(allowed) == 0 {
		return nil
	}
	out := make([]agentsdk.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		if allowed[tool.Name()] {
			out = append(out, tool)
		}
	}
	return out
}

func buildMCPManager(ctx context.Context, cfg Config, features MCPFeatures, opts ...sdkmcp.ManagerOption) (*sdkmcp.Manager, error) {
	if features.AllowAllServers {
		if cfg.MCPConfig != nil {
			return sdkmcp.NewManagerFromConfig(ctx, cfg.WorkDir, *cfg.MCPConfig, opts...)
		}
		return sdkmcp.NewManager(ctx, cfg.WorkDir, opts...)
	}
	allowedServers := stringSet(features.AllowedServers)
	if len(allowedServers) == 0 {
		return nil, nil
	}
	mcpCfg, exists, err := loadMCPConfigForRuntime(cfg)
	if err != nil || !exists {
		return nil, err
	}
	filtered := sdkmcp.Config{MCPServers: map[string]sdkmcp.ServerConfig{}}
	for name, server := range mcpCfg.MCPServers {
		if allowedServers[name] {
			filtered.MCPServers[name] = server
		}
	}
	if len(filtered.MCPServers) == 0 {
		return nil, nil
	}
	return sdkmcp.NewManagerFromConfig(ctx, cfg.WorkDir, filtered, opts...)
}

func loadMCPConfigForRuntime(cfg Config) (sdkmcp.Config, bool, error) {
	if cfg.MCPConfig != nil {
		return *cfg.MCPConfig, true, nil
	}
	return sdkmcp.LoadConfig(sdkmcp.ConfigPathForWorkDir(cfg.WorkDir))
}

func filterMCPTools(tools []agentsdk.Tool, features MCPFeatures) []agentsdk.Tool {
	allowedTools := stringSet(features.AllowedTools)
	out := make([]agentsdk.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		switch typed := tool.(type) {
		case *sdkmcp.DynamicTool:
			if features.AllowAllTools || allowedTools[typed.Descriptor.QualifiedName] || allowedTools[typed.Descriptor.ToolName] {
				out = append(out, tool)
			}
		case *sdkmcp.ListResourcesTool, *sdkmcp.ReadResourceTool:
			if features.ResourceTools {
				out = append(out, tool)
			}
		default:
			if features.AllowAllTools || allowedTools[tool.Name()] {
				out = append(out, tool)
			}
		}
	}
	return out
}

func stringSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out[trimmed] = true
		}
	}
	return out
}

func runtimeWorkspaceContext(cfg Config, tools []agentsdk.Tool, features resolvedFeatures) string {
	if !features.strict {
		return agentsdk.BuildWorkspaceContext(cfg.WorkDir, cfg.ToolAccess)
	}
	if strings.TrimSpace(cfg.WorkDir) == "" {
		return ""
	}
	names := toolNames(tools)
	toolList := "none"
	if len(names) > 0 {
		toolList = strings.Join(names, ", ")
	}
	return fmt.Sprintf(`<environment>
Working directory: %s
All file paths are relative to this directory. Use relative paths instead of absolute paths.
Tool access: %s
Available tools include: %s.
</environment>`, cfg.WorkDir, cfg.ToolAccess, toolList)
}

func modeInstructionBlocks(cfg Config, features resolvedFeatures) []string {
	if !features.value.Modes.Instructions && !features.value.Modes.PhaseTracking {
		return nil
	}
	var blocks []string
	if features.value.Modes.Instructions {
		blocks = append(blocks,
			"Session mode: "+string(cfg.SessionMode),
			"Active mode: "+modeLabel(cfg),
			"Active phase: "+runtimePhase(cfg),
		)
	}
	if features.value.Modes.PhaseTracking {
		blocks = append(blocks, agentsdk.BuildPhaseTrackingDirective(cfg.ModeSnapshot))
	}
	return blocks
}

func tracingProcessor(cfg Config, features resolvedFeatures) agentsdk.TracingProcessor {
	if !features.value.Runtime.Tracing {
		return nil
	}
	return cfg.TracingProcessor
}

func immediateInputPoller(cfg Config, features resolvedFeatures) agentsdk.ImmediateInputPoller {
	if !features.value.Runtime.ImmediateInputPolling {
		return nil
	}
	return cfg.ImmediateInputPoller
}

func untrustedToolOutputs(features resolvedFeatures) *bool {
	if !features.strict {
		return nil
	}
	value := features.value.Runtime.UntrustedToolOutputs
	return &value
}

func modelSettings(cfg Config) agentsdk.ModelSettings {
	settings := agentsdk.ModeRoutingSettings(cfg.Reasoning, cfg.Verbosity)
	if cfg.MaxTokens > 0 {
		settings.MaxTokens = cfg.MaxTokens
	}
	parallel := resolveFeatures(cfg).value.Runtime.ParallelToolCalls
	settings.ParallelToolCalls = &parallel
	return settings
}

func runModelSettings(cfg Config) agentsdk.ModelSettings {
	if cfg.ModelSettings != nil {
		settings := *cfg.ModelSettings
		if settings.ParallelToolCalls == nil {
			parallel := resolveFeatures(cfg).value.Runtime.ParallelToolCalls
			settings.ParallelToolCalls = &parallel
		}
		return settings
	}
	return modelSettings(cfg)
}

func compactionConfig(cfg Config) agentsdk.CompactionConfig {
	if !resolveFeatures(cfg).value.Runtime.Compaction {
		return agentsdk.CompactionConfig{}
	}
	trigger, target := agentsdk.CompactionDefaultsForModel(cfg.Model)
	return agentsdk.CompactionConfig{
		Enabled:                     true,
		TriggerTokens:               trigger,
		TargetTokens:                target,
		PreserveRecentItems:         10,
		PreserveInitialUserMessages: 2,
		SummaryBulletLimit:          5,
	}
}

func runCompactionConfig(cfg Config) agentsdk.CompactionConfig {
	if cfg.CompactionConfig != nil {
		return *cfg.CompactionConfig
	}
	return compactionConfig(cfg)
}

// compactionModelResolver returns a per-model compaction threshold resolver.
// Priority: caller-provided resolver → authoritative provider /models metadata
// (when an OpenAI-compatible auth session + base URL are available) → static
// CompactionDefaultsForModel. The static fallback always returns a value, so the
// resolver is authoritative for the model actually being used and sub-agents
// stop inheriting the parent model's thresholds.
func compactionModelResolver(cfg Config) agentsdk.CompactionModelResolver {
	if !resolveFeatures(cfg).value.Runtime.Compaction {
		return nil
	}
	if cfg.CompactionModelResolver != nil {
		return cfg.CompactionModelResolver
	}
	var meta *sdkopenai.CompactionMetadataResolver
	if baseURL := strings.TrimSpace(cfg.BaseURL); baseURL != "" && cfg.OpenAIAuthSession != nil {
		meta = sdkopenai.NewCompactionMetadataResolver(baseURL, cfg.OpenAIAuthSession)
	}
	return func(ctx context.Context, model string) (int, int, bool) {
		if meta != nil {
			// Bound the (one-time, cached) metadata fetch so a hung /models
			// endpoint can never stall the runner; fall back to static on miss.
			lookupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			m, ok := meta.Lookup(lookupCtx, model)
			cancel()
			if ok {
				if trigger, target, ok := sdkopenai.CompactionDefaultsFromModelMetadata(m); ok {
					return trigger, target, true
				}
			}
		}
		trigger, target := agentsdk.CompactionDefaultsForModel(model)
		return trigger, target, true
	}
}

func runCompactionRecorder(cfg Config) func(tokensBefore, tokensAfter int, summary string) {
	if cfg.CompactionRecorder != nil {
		return cfg.CompactionRecorder
	}
	return func(before, after int, summary string) {
		_ = before
		_ = after
		_ = summary
	}
}

func runCompactionCarryForward(cfg Config) func(context.Context) string {
	if cfg.CompactionCarryForward != nil {
		return cfg.CompactionCarryForward
	}
	return func(context.Context) string {
		return "Runtime state: provider=" + cfg.Provider + ", mode=" + modeLabel(cfg) + ", phase=" + runtimePhase(cfg)
	}
}

func runHandoffHistory(cfg Config) agentsdk.HandoffHistoryConfig {
	if cfg.HandoffHistory != nil {
		return *cfg.HandoffHistory
	}
	return agentsdk.HandoffHistoryConfig{
		Enabled:             resolveFeatures(cfg).value.Runtime.HandoffHistory,
		MaxTokens:           2400,
		TargetTokens:        1200,
		PreserveRecentItems: 6,
		SummaryBulletLimit:  4,
	}
}

func retryPolicy(cfg Config) *agentsdk.RetryPolicy {
	if !resolveFeatures(cfg).value.Runtime.Retry {
		return nil
	}
	policy := agentsdk.DefaultRetryPolicy()
	policy.Backoff.InitialDelayMS = 250
	policy.Backoff.MaxDelayMS = 2000
	return &policy
}

func toolInputGuardrails(cfg Config) []agentsdk.ToolInputGuardrail {
	var out []agentsdk.ToolInputGuardrail
	if resolveFeatures(cfg).value.Guardrails.Builtin {
		out = append(out, guardrails.BuiltinToolInputGuardrails()...)
	}
	out = append(out, cfg.ToolInputRules...)
	return out
}

func toolOutputGuardrails(cfg Config) []agentsdk.ToolOutputGuardrail {
	var out []agentsdk.ToolOutputGuardrail
	if resolveFeatures(cfg).value.Guardrails.Builtin {
		out = append(out, guardrails.BuiltinToolOutputGuardrails()...)
	}
	out = append(out, cfg.ToolOutputRules...)
	return out
}

func toolPolicy(cfg Config) *agentsdk.ToolPolicy {
	approval := resolveFeatures(cfg).value.Runtime.Approval
	if !approval && cfg.ToolTimeout <= 0 {
		return nil
	}
	return &agentsdk.ToolPolicy{
		ApprovalRequired: approval,
		DefaultTimeout:   cfg.ToolTimeout,
	}
}

func additionalInstructions(cfg Config) string {
	return strings.TrimSpace(strings.Join(nonEmptyStrings(
		prefixNonEmpty("Runtime surface: ", cfg.FeatureSummary),
		cfg.ModeDirectiveText,
		cfg.FinalCheckInstructions,
	), "\n\n"))
}

func permissionMode(cfg Config) policy.PermissionMode {
	if cfg.PermissionMode != "" {
		return policy.NormalizePermissionMode(string(cfg.PermissionMode))
	}
	return permissionModeFromAccess(cfg.ToolAccess)
}

func permissionModeFromAccess(access agentsdk.ToolAccessLevel) policy.PermissionMode {
	if agentsdk.NormalizeToolAccessLevel(access) == agentsdk.ToolAccessLevelReadOnly {
		return policy.PermissionModeReadOnly
	}
	return policy.PermissionModeWorkspaceWrite
}

func runtimePhase(cfg Config) string {
	if strings.TrimSpace(cfg.ActivePhase) != "" {
		return strings.TrimSpace(cfg.ActivePhase)
	}
	if strings.TrimSpace(cfg.ActiveMode) != "" {
		return strings.TrimSpace(cfg.ActiveMode)
	}
	return string(cfg.SessionMode)
}

func modeLabel(cfg Config) string {
	if cfg.ModeSnapshot != nil {
		if cfg.ModeSnapshot.DisplayName != "" {
			return cfg.ModeSnapshot.DisplayName
		}
		if cfg.ModeSnapshot.Name != "" {
			return cfg.ModeSnapshot.Name
		}
	}
	if strings.TrimSpace(cfg.ActiveMode) != "" {
		return cfg.ActiveMode
	}
	return string(cfg.SessionMode)
}

func sessionNumber(cfg Config) int32 {
	if cfg.EventSession > 0 {
		return int32(cfg.EventSession)
	}
	return 1
}

func cloneTools(tools []agentsdk.Tool) []agentsdk.Tool {
	if tools == nil {
		return nil
	}
	out := make([]agentsdk.Tool, len(tools))
	copy(out, tools)
	return out
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func toolNames(tools []agentsdk.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func prefixNonEmpty(prefix, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return prefix + value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
