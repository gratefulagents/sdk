package runtime

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

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

	Trace                     *agentsdk.Trace
	ParentSpanID              string
	ImmediateInputPoller      agentsdk.ImmediateInputPoller
	CompactionConfig          *agentsdk.CompactionConfig
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
	if cfg.EnableProjectState {
		b.emitStatus("loading project state")
		store, err := b.projectStateStore(ctx, cfg)
		if err != nil {
			return nil, err
		}
		cfg.ProjectStateStore = store
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

	b.emitStatus("building runner")
	runner, err := BuildRunner(cfg)
	if err != nil {
		return nil, err
	}

	tracker := agentsdk.NewProgressTracker(agentsdk.WithTracingProcessor(cfg.TracingProcessor))
	tracker.SetSession(sessionNumber(cfg), runtimePhase(cfg))

	var toolBundle ToolBundle
	if cfg.EnableTools || cfg.EnableSubAgents {
		b.emitStatus("initializing tools")
		toolBundle, err = BuildToolBundle(ctx, cfg)
		if err != nil {
			b.emitLog("tool setup warning: " + err.Error())
		}
	}

	agent, agentTools, specialistTools, specialistAgents := BuildAgentWithSpecialists(cfg, runner, toolBundle)

	hooks := agentsdk.NewPlatformHooks(tracker, nil)
	var eventStream *agentsdk.EventStream
	if cfg.EventWriter != nil {
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
	if cfg.EnableSubAgents {
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
	}
}

func BuildToolBundle(ctx context.Context, cfg Config) (ToolBundle, error) {
	mode := permissionMode(cfg)
	bundle := ToolBundle{}
	if cfg.EnableProjectState && cfg.ProjectStateStore == nil {
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
	if !cfg.DisableDefaultTools {
		registryOptions := []sdktools.RegistryOption{
			sdktools.WithPermissionMode(mode),
			sdktools.WithPrivateNetworkURLs(cfg.AllowPrivateNetworkURLs),
		}
		if cfg.CommandSandboxConfig != nil {
			registryOptions = append(registryOptions, sdktools.WithCommandSandboxConfig(*cfg.CommandSandboxConfig))
		}
		if cfg.EnableAsyncShell {
			registryOptions = append(registryOptions, sdktools.WithAsyncShellTools())
		}
		if cfg.DisableWebTools {
			registryOptions = append(registryOptions, sdktools.WithoutWebTools())
		}
		registry := sdktools.NewRegistry(cfg.WorkDir, registryOptions...)
		bundle.Tools = append(bundle.Tools, registry.Tools()...)
		bundle.Closers = append(bundle.Closers, registry.Closers()...)
	}
	if !cfg.DisableSignalTools {
		bundle.Tools = append(bundle.Tools, signalTools(cfg)...)
	}
	if cfg.EnableProjectState && cfg.ProjectStateStore != nil {
		bundle.Tools = append(bundle.Tools, sdkprojectstatetools.Tools(cfg.ProjectStateStore, projectStateActor(cfg))...)
	}
	bundle.Tools = append(bundle.Tools, cfg.ExtraTools...)
	bundle.Tools = attachOpenAIVisionAnalyzer(cfg, bundle.Tools)

	if cfg.EnableMCP {
		var manager *sdkmcp.Manager
		var err error
		managerOptions := []sdkmcp.ManagerOption{sdkmcp.WithPermissionMode(mode)}
		if cfg.CommandSandboxConfig != nil {
			managerOptions = append(managerOptions, sdkmcp.WithCommandExecutor(sdksandbox.DefaultWithConfig(*cfg.CommandSandboxConfig)))
		}
		if cfg.MCPConfig != nil {
			manager, err = sdkmcp.NewManagerFromConfig(ctx, cfg.WorkDir, *cfg.MCPConfig, managerOptions...)
		} else {
			manager, err = sdkmcp.NewManager(ctx, cfg.WorkDir, managerOptions...)
		}
		if manager != nil {
			bundle.Tools = append(bundle.Tools, sdkmcp.BuildTools(manager)...)
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
	var tools []agentsdk.Tool
	var specialistTools []agentsdk.Tool
	var specialistAgents map[string]*agentsdk.Agent
	if cfg.EnableTools {
		tools = append(tools, cloneTools(hostBundle.Tools)...)
	}
	if cfg.EnableSubAgents {
		catalog := cfg.RoleCatalog
		if len(catalog) == 0 {
			catalog = agentsdk.RoleCatalog{{
				Name:        "agent",
				Description: "Generic sub-agent. Instructions come entirely from the parent's prompt.",
				ToolAccess:  "full",
			}}
		}
		specialists := agentsdk.BuildSpecialistToolsFromCatalog(catalog, agentsdk.SpecialistBuildOptions{
			Runner:            runner,
			Tools:             cloneTools(hostBundle.Tools),
			BaseModel:         cfg.Model,
			Provider:          modelRoutingProvider(cfg),
			BaseModelSettings: modelSettings(cfg),
			ModeSnapshot:      cfg.ModeSnapshot,
			OutputExtractor:   cfg.SpecialistOutputExtractor,
		})
		specialistTools = cloneTools(specialists.Tools)
		specialistAgents = specialists.Agents
		tools = append(tools, specialistTools...)
	}

	agent := &agentsdk.Agent{
		Name:          firstNonEmpty(cfg.AgentName, "agent"),
		Model:         cfg.Model,
		ModelSettings: modelSettings(cfg),
		Tools:         tools,
		MCPServers:    cloneStrings(hostBundle.MCPServers),
		InstructionsFn: func(_ *agentsdk.RunContext, a *agentsdk.Agent) string {
			workspace := agentsdk.BuildWorkspaceContext(cfg.WorkDir, cfg.ToolAccess)
			budget := agentsdk.BuildRunBudgetContext(cfg.MaxTurns)
			delegation := agentsdk.BuildDelegationGuide(a)
			return strings.Join(nonEmptyStrings(
				cfg.Instructions,
				delegation,
				"Session mode: "+string(cfg.SessionMode),
				"Active mode: "+modeLabel(cfg),
				"Active phase: "+runtimePhase(cfg),
				agentsdk.BuildPhaseTrackingDirective(cfg.ModeSnapshot),
				workspace,
				budget,
			), "\n\n")
		},
	}
	if cfg.OutputSchema != nil {
		agent.OutputType = cfg.OutputSchema
	}
	if cfg.EnableHandoffs {
		agent.Handoffs = buildCatalogHandoffs(cfg, specialistAgents)
	}
	return agent, tools, specialistTools, specialistAgents
}

// buildCatalogHandoffs creates one handoff per catalog specialist so the parent
// agent can transfer the conversation to a specialist that owns the request.
// Targets are resolved from the already-built specialist agents (which carry
// the role's instructions and HandoffDescription). When no catalog specialists
// are available, it falls back to a single generic handoff specialist.
func buildCatalogHandoffs(cfg Config, specialistAgents map[string]*agentsdk.Agent) []*agentsdk.Handoff {
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
	if agent == nil || len(specialistAgents) == 0 {
		return agentTools
	}
	scheduler := state.configureSubAgentScheduler(agentsdk.SubAgentSchedulerConfig{
		MaxConcurrent:    cfg.MaxConcurrentSubAgents,
		Runner:           runner,
		Agents:           specialistAgents,
		Tracker:          tracker,
		EventStream:      eventStream,
		WorkDir:          cfg.WorkDir,
		ToolAccessLevel:  cfg.ToolAccess,
		ToolPolicy:       toolPolicy(cfg),
		CompactionConfig: runCompactionConfig(cfg),
		MaxTurns:         cfg.SubAgentMaxTurns,
	})
	asyncTools := agentsdk.BuildSubAgentTaskTools(scheduler, defaultAsyncSubAgent(specialistAgents))
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
	return agentsdk.RunConfig{
		MaxTurns:         cfg.MaxTurns,
		SubAgentMaxTurns: cfg.SubAgentMaxTurns,
		Hooks:            hooks,
		ModelSettings:    runModelSettings(cfg),
		TracingProcessor: cfg.TracingProcessor,
		Trace:            cfg.Trace,
		ParentSpanID:     cfg.ParentSpanID,

		ImmediateInputPoller:      cfg.ImmediateInputPoller,
		CompactionConfig:          runCompactionConfig(cfg),
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
		ForceFinalSummaryTurn:  cfg.ForceFinalSummary,
		Debug:                  cfg.Debug,
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

func signalTools(cfg Config) []agentsdk.Tool {
	return []agentsdk.Tool{
		&sdksignal.AskUserQuestionTool{},
		&sdksignal.PresentPlanTool{},
		&sdksignal.FinishTool{},
		&sdksignal.SetPhaseTool{
			Phases:        signalPhaseOptions(cfg),
			CurrentPhase:  cfg.ActivePhase,
			PauseOnChange: true,
		},
	}
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

func modelSettings(cfg Config) agentsdk.ModelSettings {
	settings := agentsdk.ModeRoutingSettings(cfg.Reasoning, cfg.Verbosity)
	if cfg.MaxTokens > 0 {
		settings.MaxTokens = cfg.MaxTokens
	}
	parallel := true
	settings.ParallelToolCalls = &parallel
	return settings
}

func runModelSettings(cfg Config) agentsdk.ModelSettings {
	if cfg.ModelSettings != nil {
		return *cfg.ModelSettings
	}
	return modelSettings(cfg)
}

func compactionConfig(cfg Config) agentsdk.CompactionConfig {
	if !cfg.EnableCompaction {
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
		Enabled:             cfg.EnableCompaction,
		MaxTokens:           2400,
		TargetTokens:        1200,
		PreserveRecentItems: 6,
		SummaryBulletLimit:  4,
	}
}

func retryPolicy(cfg Config) *agentsdk.RetryPolicy {
	if !cfg.EnableRetry {
		return nil
	}
	policy := agentsdk.DefaultRetryPolicy()
	policy.Backoff.InitialDelayMS = 250
	policy.Backoff.MaxDelayMS = 2000
	return &policy
}

func toolInputGuardrails(cfg Config) []agentsdk.ToolInputGuardrail {
	var out []agentsdk.ToolInputGuardrail
	if cfg.EnableGuardrails {
		out = append(out, guardrails.BuiltinToolInputGuardrails()...)
	}
	out = append(out, cfg.ToolInputRules...)
	return out
}

func toolOutputGuardrails(cfg Config) []agentsdk.ToolOutputGuardrail {
	var out []agentsdk.ToolOutputGuardrail
	if cfg.EnableGuardrails {
		out = append(out, guardrails.BuiltinToolOutputGuardrails()...)
	}
	out = append(out, cfg.ToolOutputRules...)
	return out
}

func toolPolicy(cfg Config) *agentsdk.ToolPolicy {
	if !cfg.EnableApproval && cfg.ToolTimeout <= 0 {
		return nil
	}
	return &agentsdk.ToolPolicy{
		ApprovalRequired: cfg.EnableApproval,
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
