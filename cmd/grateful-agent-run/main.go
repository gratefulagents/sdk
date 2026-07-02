package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkpolicy "github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	sdkruntime "github.com/gratefulagents/sdk/pkg/agentsdk/runtime"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tracestore"
)

type cliConfig struct {
	Provider                 string
	Model                    string
	BaseURL                  string
	APIKey                   string
	AuthMode                 string
	APIMode                  string
	OpenAIOAuthPath          string
	OpenAIOAuthAccountID     string
	OpenAIOAuthAccountIDPath string

	WorkDir      string
	AgentName    string
	Instructions string
	Prompt       string
	PromptFile   string
	ReadStdin    bool

	Reasoning              string
	Verbosity              string
	MaxTokens              int
	MaxTurns               int
	SubAgentMaxTurns       int
	MaxConcurrentSubAgents int
	ToolTimeout            int
	Timeout                time.Duration
	ToolAccess             string
	PermissionMode         string

	EnableTools             bool
	EnableWebTools          bool
	EnableMCP               bool
	EnableHandoffs          bool
	EnableSubAgents         bool
	EnableGuardrails        bool
	EnableCompaction        bool
	EnableApproval          bool
	EnableRetry             bool
	EnableAsyncShell        bool
	EnableFinalCheck        bool
	ExitZeroOnTimeout       bool
	ForceFinal              bool
	Debug                   bool
	AllowPrivateNetworkURLs bool
	TerminalBenchCompliance bool

	Output      string
	EventLog    string
	TraceRoot   string
	RunID       string
	TaskID      string
	CandidateID string
	Mode        string
}

type outputEnvelope struct {
	RunID        string                 `json:"run_id,omitempty"`
	TaskID       string                 `json:"task_id,omitempty"`
	CandidateID  string                 `json:"candidate_id,omitempty"`
	TraceDir     string                 `json:"trace_dir,omitempty"`
	FinalText    string                 `json:"final_text"`
	FinalOutput  any                    `json:"final_output,omitempty"`
	Interrupted  bool                   `json:"interrupted"`
	Interruption *agentsdk.Interruption `json:"interruption,omitempty"`
	Error        string                 `json:"error,omitempty"`
	Usage        agentsdk.Usage         `json:"usage"`
	Metrics      runMetrics             `json:"metrics"`
	ToolCalls    []toolCallSummary      `json:"tool_calls,omitempty"`
}

type runMetrics struct {
	DurationSec    float64 `json:"duration_sec"`
	ToolCalls      int     `json:"tool_calls"`
	TurnsUsed      int     `json:"turns_used"`
	CompactionHits int     `json:"compaction_hits"`
}

type toolCallSummary struct {
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

type runBundle struct {
	runner    *agentsdk.Runner
	agent     *agentsdk.Agent
	runConfig agentsdk.RunConfig
	tools     []agentsdk.Tool
	closers   []io.Closer
	policy    sdkpolicy.PermissionMode
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cfg, remaining, err := parseConfig(args)
	if err != nil {
		return err
	}
	if !cfg.Debug {
		log.SetOutput(io.Discard)
	}

	prompt, err := resolvePrompt(cfg, remaining, stdin)
	if err != nil {
		return err
	}

	workDir, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}

	ctx := context.Background()
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	var eventFile *os.File
	if strings.TrimSpace(cfg.EventLog) != "" {
		eventFile, err = os.OpenFile(cfg.EventLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("open event log: %w", err)
		}
		defer eventFile.Close()
	}

	var traceWriter *tracestore.TraceWriter
	var traceStore *tracestore.FilesystemTraceStore
	var traceDir string
	runID := cfg.RunID
	if strings.TrimSpace(cfg.TraceRoot) != "" {
		if runID == "" {
			runID = defaultRunID()
		}
		traceStore, err = tracestore.NewFilesystemTraceStore(cfg.TraceRoot)
		if err != nil {
			return fmt.Errorf("create trace store: %w", err)
		}
		traceWriter = tracestore.NewTraceWriter(traceStore)
	}

	bundle, err := buildBundle(ctx, cfg, workDir, eventFile, traceWriter)
	if err != nil {
		return err
	}
	defer closeAll(bundle.closers, stderr)

	if traceWriter != nil {
		tools := make([]string, 0, len(bundle.tools))
		for _, tool := range bundle.tools {
			tools = append(tools, tool.Name())
		}
		meta := tracestore.RunMetadata{
			RunID:          runID,
			CandidateID:    cfg.CandidateID,
			Model:          cfg.Model,
			Mode:           cfg.Mode,
			PermissionMode: string(bundle.policy),
			Cwd:            workDir,
			MaxTurns:       cfg.MaxTurns,
			Tools:          tools,
			McpServers:     bundle.agent.MCPServers,
			StartedAt:      time.Now().UTC(),
		}
		if err := traceWriter.InitRun(meta); err != nil {
			return fmt.Errorf("initialize trace run: %w", err)
		}
		bundle.runConfig.Hooks = agentsdk.NewCompositeHooks(bundle.runConfig.Hooks, traceWriter)
		bundle.runner.DefaultHooks = bundle.runConfig.Hooks
		traceDir, err = traceStore.RunDir(runID)
		if err != nil {
			return fmt.Errorf("resolve trace run dir: %w", err)
		}
	}

	items := []agentsdk.RunItem{{
		Type:    agentsdk.RunItemMessage,
		Message: &agentsdk.MessageOutput{Text: prompt},
	}}

	start := time.Now()
	result, runErr := bundle.runner.Run(ctx, bundle.agent, items, bundle.runConfig)
	duration := time.Since(start)

	envelope := buildOutput(cfg, runID, traceDir, result, duration, runErr)
	if traceWriter != nil {
		status := "success"
		if runErr != nil {
			status = "error"
		} else if result != nil && result.IsInterrupted() {
			status = "interrupted"
		}
		traceWriter.WriteMetrics(map[string]any{
			"duration_sec":    envelope.Metrics.DurationSec,
			"tool_calls":      envelope.Metrics.ToolCalls,
			"turns_used":      envelope.Metrics.TurnsUsed,
			"compaction_hits": envelope.Metrics.CompactionHits,
			"input_tokens":    envelope.Usage.InputTokens,
			"output_tokens":   envelope.Usage.OutputTokens,
			"total_tokens":    envelope.Usage.TotalTokens(),
			"status":          status,
		})
		if err := traceStore.WriteScore(runID, tracestore.Score{
			TaskID:      firstNonEmpty(cfg.TaskID, runID),
			CandidateID: cfg.CandidateID,
			Success:     status == "success",
			Metrics: tracestore.ScoreMetrics{
				Accuracy:       boolScore(status == "success"),
				TokensUsed:     envelope.Usage.TotalTokens(),
				DurationSec:    envelope.Metrics.DurationSec,
				ToolCalls:      envelope.Metrics.ToolCalls,
				TurnsUsed:      envelope.Metrics.TurnsUsed,
				CompactionHits: envelope.Metrics.CompactionHits,
			},
		}); err != nil {
			return fmt.Errorf("write trace score: %w", err)
		}
		traceWriter.FinalizeRun(status)
	}

	if err := writeOutput(stdout, cfg.Output, envelope); err != nil {
		return err
	}
	if runErr != nil && !(cfg.ExitZeroOnTimeout && errors.Is(runErr, context.DeadlineExceeded)) {
		return runErr
	}
	if result != nil && result.IsInterrupted() {
		return errors.New("run interrupted")
	}
	return nil
}

func buildBundle(ctx context.Context, cfg cliConfig, workDir string, eventFile io.Writer, traceWriter *tracestore.TraceWriter) (*runBundle, error) {
	toolAccess, err := parseToolAccess(cfg.ToolAccess)
	if err != nil {
		return nil, err
	}
	permissionMode, err := parsePermissionMode(cfg.PermissionMode)
	if err != nil {
		return nil, err
	}
	bundle, err := sdkruntime.NewBuilder(sdkruntime.Config{
		Provider:                 cfg.Provider,
		Model:                    cfg.Model,
		BaseURL:                  cfg.BaseURL,
		APIKey:                   cfg.APIKey,
		AuthMode:                 cfg.AuthMode,
		APIMode:                  cfg.APIMode,
		OpenAIOAuthPath:          cfg.OpenAIOAuthPath,
		OpenAIOAuthAccountID:     cfg.OpenAIOAuthAccountID,
		OpenAIOAuthAccountIDPath: cfg.OpenAIOAuthAccountIDPath,
		WorkDir:                  workDir,
		AgentName:                firstNonEmpty(cfg.AgentName, "grateful-agent-run"),
		Instructions:             cfg.Instructions,
		Reasoning:                cfg.Reasoning,
		Verbosity:                cfg.Verbosity,
		MaxTokens:                cfg.MaxTokens,
		MaxTurns:                 cfg.MaxTurns,
		SubAgentMaxTurns:         cfg.SubAgentMaxTurns,
		MaxConcurrentSubAgents:   cfg.MaxConcurrentSubAgents,
		ToolTimeout:              cfg.ToolTimeout,
		ToolAccess:               toolAccess,
		PermissionMode:           permissionMode,
		EnableTools:              cfg.EnableTools,
		EnableMCP:                cfg.EnableMCP,
		EnableHandoffs:           cfg.EnableHandoffs,
		EnableSubAgents:          cfg.EnableSubAgents,
		EnableGuardrails:         cfg.EnableGuardrails,
		EnableCompaction:         cfg.EnableCompaction,
		EnableApproval:           cfg.EnableApproval,
		EnableRetry:              cfg.EnableRetry,
		EnableAsyncShell:         cfg.EnableAsyncShell,
		ForceFinalSummary:        cfg.ForceFinal,
		FinalCheckInstructions:   finalCheckInstructions(cfg),
		Debug:                    cfg.Debug,
		AllowPrivateNetworkURLs:  cfg.AllowPrivateNetworkURLs,
		DisableWebTools:          !cfg.EnableWebTools,
		ToolInputRules:           terminalBenchComplianceGuardrails(cfg.TerminalBenchCompliance),
		EventWriter:              eventFile,
		TracingProcessor:         traceWriter,
		WorkingStateText:         "Runtime state is maintained by the headless evaluation harness.",
		ModeDirectiveText:        runtimeDirectiveText(cfg),
		ActiveMode:               firstNonEmpty(cfg.Mode, "eval"),
		DisableSignalTools:       true,
	}).Build(ctx)
	if err != nil {
		return nil, err
	}
	return &runBundle{
		runner:    bundle.Runner,
		agent:     bundle.Agent,
		runConfig: bundle.Config,
		tools:     bundle.Tools,
		closers:   bundle.Closers,
		policy:    resolvedPermissionMode(permissionMode, toolAccess),
	}, nil
}

func parseConfig(args []string) (cliConfig, []string, error) {
	cwd, _ := os.Getwd()
	cfg := cliConfig{
		Provider:                 envOr("GRATEFUL_PROVIDER", "openai"),
		BaseURL:                  envOr("GRATEFUL_BASE_URL", ""),
		APIKey:                   envOr("GRATEFUL_API_KEY", ""),
		AuthMode:                 envOr("OPENAI_AUTH_MODE", "api-key"),
		APIMode:                  envOr("OPENAI_API_MODE", ""),
		OpenAIOAuthPath:          envOr("OPENAI_OAUTH_AUTH_JSON_PATH", ""),
		OpenAIOAuthAccountID:     envOr("OPENAI_OAUTH_ACCOUNT_ID", ""),
		OpenAIOAuthAccountIDPath: envOr("OPENAI_OAUTH_ACCOUNT_ID_PATH", ""),
		WorkDir:                  envOr("GRATEFUL_WORKDIR", cwd),
		AgentName:                envOr("GRATEFUL_AGENT_NAME", "grateful-agent-run"),
		Instructions:             envOr("GRATEFUL_AGENT_INSTRUCTIONS", defaultInstructions()),
		Reasoning:                envOr("GRATEFUL_REASONING", string(agentsdk.ReasoningMedium)),
		Verbosity:                envOr("GRATEFUL_VERBOSITY", string(agentsdk.TextVerbosityMedium)),
		MaxTokens:                envInt("GRATEFUL_MAX_TOKENS", 4096),
		MaxTurns:                 envInt("GRATEFUL_MAX_TURNS", 20),
		SubAgentMaxTurns:         envInt("GRATEFUL_SUBAGENT_MAX_TURNS", 5),
		MaxConcurrentSubAgents:   envInt("GRATEFUL_MAX_CONCURRENT_SUBAGENTS", 2),
		ToolTimeout:              envInt("GRATEFUL_TOOL_TIMEOUT", 0),
		ToolAccess:               envOr("GRATEFUL_TOOL_ACCESS", string(agentsdk.ToolAccessLevelFull)),
		PermissionMode:           envOr("GRATEFUL_PERMISSION_MODE", ""),
		EnableTools:              envBool("GRATEFUL_TOOLS", true),
		EnableWebTools:           envBool("GRATEFUL_WEB_TOOLS", true),
		EnableMCP:                envBool("GRATEFUL_MCP", false),
		EnableHandoffs:           envBool("GRATEFUL_HANDOFFS", false),
		EnableSubAgents:          envBool("GRATEFUL_SUBAGENTS", false),
		EnableGuardrails:         envBool("GRATEFUL_GUARDRAILS", true),
		EnableCompaction:         envBool("GRATEFUL_COMPACTION", true),
		EnableApproval:           envBool("GRATEFUL_APPROVAL", false),
		EnableRetry:              envBool("GRATEFUL_RETRY", true),
		EnableAsyncShell:         envBool("GRATEFUL_ASYNC_BASH", false),
		EnableFinalCheck:         envBool("GRATEFUL_FINAL_CHECK", false),
		ExitZeroOnTimeout:        envBool("GRATEFUL_EXIT_ZERO_ON_TIMEOUT", false),
		ForceFinal:               envBool("GRATEFUL_FORCE_FINAL", true),
		AllowPrivateNetworkURLs:  envBool("GRATEFULAGENTS_ALLOW_PRIVATE_NETWORK_URLS", false),
		TerminalBenchCompliance:  envBool("GRATEFUL_TB_COMPLIANCE", false),
		Output:                   envOr("GRATEFUL_OUTPUT", "text"),
		TraceRoot:                envOr("GRATEFUL_TRACE_ROOT", ""),
		CandidateID:              envOr("GRATEFUL_CANDIDATE_ID", ""),
		Mode:                     envOr("GRATEFUL_MODE", "eval"),
	}

	fs := flag.NewFlagSet("grateful-agent-run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.Provider, "provider", cfg.Provider, "provider: openai, multi, anthropic, openrouter, or local")
	fs.StringVar(&cfg.Model, "model", cfg.Model, "model name or provider-prefixed model")
	fs.StringVar(&cfg.BaseURL, "base-url", cfg.BaseURL, "provider base URL override")
	fs.StringVar(&cfg.APIKey, "api-key", cfg.APIKey, "provider API key override")
	fs.StringVar(&cfg.AuthMode, "auth-mode", cfg.AuthMode, "OpenAI auth mode: api-key or oauth")
	fs.StringVar(&cfg.APIMode, "api-mode", cfg.APIMode, "OpenAI-compatible API mode: responses or chat-completions")
	fs.StringVar(&cfg.OpenAIOAuthPath, "openai-oauth-path", cfg.OpenAIOAuthPath, "OpenAI OAuth auth JSON path")
	fs.StringVar(&cfg.OpenAIOAuthAccountID, "openai-oauth-account-id", cfg.OpenAIOAuthAccountID, "OpenAI OAuth account id override")
	fs.StringVar(&cfg.OpenAIOAuthAccountIDPath, "openai-oauth-account-id-path", cfg.OpenAIOAuthAccountIDPath, "OpenAI OAuth account id file")
	fs.StringVar(&cfg.WorkDir, "workdir", cfg.WorkDir, "working directory for tools")
	fs.StringVar(&cfg.AgentName, "agent", cfg.AgentName, "agent name")
	fs.StringVar(&cfg.Instructions, "instructions", cfg.Instructions, "system instructions")
	fs.StringVar(&cfg.Prompt, "prompt", cfg.Prompt, "task prompt")
	fs.StringVar(&cfg.PromptFile, "prompt-file", cfg.PromptFile, "file containing the task prompt")
	fs.BoolVar(&cfg.ReadStdin, "stdin", cfg.ReadStdin, "read task prompt from stdin")
	fs.StringVar(&cfg.Reasoning, "reasoning", cfg.Reasoning, "reasoning level")
	fs.StringVar(&cfg.Verbosity, "verbosity", cfg.Verbosity, "text verbosity")
	fs.IntVar(&cfg.MaxTokens, "max-tokens", cfg.MaxTokens, "maximum output tokens")
	fs.IntVar(&cfg.MaxTurns, "max-turns", cfg.MaxTurns, "maximum runner turns")
	fs.IntVar(&cfg.SubAgentMaxTurns, "subagent-max-turns", cfg.SubAgentMaxTurns, "maximum nested sub-agent turns")
	fs.IntVar(&cfg.MaxConcurrentSubAgents, "max-concurrent-subagents", cfg.MaxConcurrentSubAgents, "maximum concurrent sub-agent tools")
	fs.IntVar(&cfg.ToolTimeout, "tool-timeout", cfg.ToolTimeout, "default tool timeout in seconds")
	fs.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "overall run timeout, for example 30m")
	fs.StringVar(&cfg.ToolAccess, "tool-access", cfg.ToolAccess, "tool access: full or read-only")
	fs.StringVar(&cfg.PermissionMode, "permission-mode", cfg.PermissionMode, "permission mode: read-only, workspace-write, or danger-full-access")
	fs.BoolVar(&cfg.EnableTools, "tools", cfg.EnableTools, "enable filesystem, shell, search, web, and LSP tools")
	fs.BoolVar(&cfg.EnableWebTools, "web-tools", cfg.EnableWebTools, "enable SDK WebFetch tools")
	fs.BoolVar(&cfg.EnableMCP, "mcp", cfg.EnableMCP, "enable workspace MCP tools")
	fs.BoolVar(&cfg.EnableHandoffs, "handoffs", cfg.EnableHandoffs, "enable handoffs")
	fs.BoolVar(&cfg.EnableSubAgents, "subagents", cfg.EnableSubAgents, "enable sub-agent tools")
	fs.BoolVar(&cfg.EnableGuardrails, "guardrails", cfg.EnableGuardrails, "enable built-in guardrails")
	fs.BoolVar(&cfg.EnableCompaction, "compaction", cfg.EnableCompaction, "enable context compaction")
	fs.BoolVar(&cfg.EnableApproval, "approval", cfg.EnableApproval, "require approvals for write tools")
	fs.BoolVar(&cfg.EnableRetry, "retry", cfg.EnableRetry, "enable retry policy")
	fs.BoolVar(&cfg.EnableAsyncShell, "async-bash", cfg.EnableAsyncShell, "enable background bash job tools")
	fs.BoolVar(&cfg.EnableFinalCheck, "final-check", cfg.EnableFinalCheck, "inject a generic final artifact verification reminder")
	fs.BoolVar(&cfg.ExitZeroOnTimeout, "exit-zero-on-timeout", cfg.ExitZeroOnTimeout, "exit 0 after writing JSON when the run's own timeout expires")
	fs.BoolVar(&cfg.ForceFinal, "force-final-summary", cfg.ForceFinal, "reserve final turn for summary")
	fs.BoolVar(&cfg.Debug, "debug", cfg.Debug, "enable SDK debug logging")
	fs.BoolVar(&cfg.AllowPrivateNetworkURLs, "allow-private-network-urls", cfg.AllowPrivateNetworkURLs, "allow WebFetch, Browser, and Vision tools to access private/local network URLs")
	fs.BoolVar(&cfg.TerminalBenchCompliance, "terminal-bench-compliance", cfg.TerminalBenchCompliance, "block Terminal-Bench website and repository lookups from tool inputs")
	fs.StringVar(&cfg.Output, "output", cfg.Output, "output format: text or json")
	fs.StringVar(&cfg.EventLog, "event-log", cfg.EventLog, "write session event JSONL to this file")
	fs.StringVar(&cfg.TraceRoot, "trace-root", cfg.TraceRoot, "write trace artifacts under this root")
	fs.StringVar(&cfg.RunID, "run-id", cfg.RunID, "trace run id")
	fs.StringVar(&cfg.TaskID, "task-id", cfg.TaskID, "evaluation task id")
	fs.StringVar(&cfg.CandidateID, "candidate-id", cfg.CandidateID, "evaluation candidate id")
	fs.StringVar(&cfg.Mode, "mode", cfg.Mode, "mode label for traces")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), usageText())
	}
	if err := fs.Parse(args); err != nil {
		return cfg, nil, fmt.Errorf("%w\n%s", err, usageText())
	}
	if cfg.Model == "" {
		cfg.Model = defaultModelForProvider(cfg.Provider)
	}
	if cfg.CandidateID == "" {
		cfg.CandidateID = cfg.Provider + "/" + cfg.Model
	}
	cfg.Output = strings.ToLower(strings.TrimSpace(cfg.Output))
	if cfg.Output != "text" && cfg.Output != "json" {
		return cfg, nil, fmt.Errorf("unknown output format %q", cfg.Output)
	}
	return cfg, fs.Args(), nil
}

func resolvePrompt(cfg cliConfig, args []string, stdin io.Reader) (string, error) {
	if strings.TrimSpace(cfg.Prompt) != "" {
		return cfg.Prompt, nil
	}
	if strings.TrimSpace(cfg.PromptFile) != "" {
		data, err := os.ReadFile(cfg.PromptFile)
		if err != nil {
			return "", fmt.Errorf("read prompt file: %w", err)
		}
		return string(data), nil
	}
	if prompt := promptFromArgs(args); strings.TrimSpace(prompt) != "" {
		return prompt, nil
	}
	if cfg.ReadStdin || stdinHasData(stdin) {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		if strings.TrimSpace(string(data)) != "" {
			return string(data), nil
		}
	}
	return "", errors.New("provide a task prompt with --prompt, --prompt-file, a positional argument, or stdin")
}

func runtimeDirectiveText(cfg cliConfig) string {
	return strings.Join(nonEmptyStrings(
		"Mode label: "+firstNonEmpty(cfg.Mode, "eval"),
		deadlineInstructions(cfg.Timeout),
		asyncShellInstructions(cfg.EnableAsyncShell),
	), "\n\n")
}

func deadlineInstructions(timeout time.Duration) string {
	if timeout <= 0 {
		return ""
	}
	buffer := minDuration(timeout/10, 5*time.Minute)
	if buffer < time.Minute {
		buffer = time.Minute
	}
	return fmt.Sprintf(
		"<deadline>\nThis run has an overall wall-clock deadline of about %s. Preserve at least %s for final artifact checks and finalization. Near the deadline, stop launching long-running commands and verify the best deliverable already produced.\n</deadline>",
		roundSeconds(timeout),
		roundSeconds(buffer),
	)
}

func asyncShellInstructions(enabled bool) string {
	if !enabled {
		return ""
	}
	return "<async_shell>\nBashStart/BashPoll are available for commands that need to run in the background or are likely to exceed the normal Bash timeout. Prefer regular Bash for one-shot builds, installs, tests, and scripts that can finish within the Bash timeout. When polling an async job, use BashPoll with wait_ms around 30000-60000 instead of tight polling so you preserve LLM turns. Stop jobs that are no longer useful.\n</async_shell>"
}

func finalCheckInstructions(cfg cliConfig) string {
	if !cfg.EnableFinalCheck {
		return ""
	}
	return "<final_artifact_check>\nBefore finalizing, perform a task-agnostic artifact audit: confirm required files exist at the requested paths, parse any required JSON/TOML/YAML or schema-like output, check executable bits for scripts/binaries, confirm services or background processes are still running when the task requires them, and run available local verification commands or smoke tests. Do not assume success from a build or edit alone; verify the deliverable that the hidden tests will inspect.\n</final_artifact_check>"
}

func terminalBenchComplianceGuardrails(enabled bool) []agentsdk.ToolInputGuardrail {
	if !enabled {
		return nil
	}
	return []agentsdk.ToolInputGuardrail{{
		Name: "terminal_bench_compliance",
		Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, tool agentsdk.Tool, input json.RawMessage) (*agentsdk.GuardrailResult, error) {
			if reason := terminalBenchForbiddenLookup(string(input)); reason != "" {
				return &agentsdk.GuardrailResult{
					TripwireTriggered: true,
					Output:            fmt.Sprintf("blocked %s in %s tool input", reason, tool.Name()),
				}, nil
			}
			return &agentsdk.GuardrailResult{}, nil
		},
	}}
}

func terminalBenchForbiddenLookup(input string) string {
	text := normalizeTerminalBenchProbe(input)
	for _, marker := range []string{
		"terminal-bench.org",
		"terminalbench.org",
		"terminal-bench.github.io",
		"terminalbench.github.io",
		"github.com/terminal-bench",
		"github.com/terminalbench",
		"raw.githubusercontent.com/terminal-bench",
		"raw.githubusercontent.com/terminalbench",
		"api.github.com/repos/terminal-bench",
		"api.github.com/repos/terminalbench",
		"harborframework/terminal-bench",
		"terminal-bench-2-leaderboard",
	} {
		if strings.Contains(text, marker) {
			return marker
		}
	}
	if strings.Contains(text, "api.github.com/search") && containsTerminalBenchName(text) {
		return "api.github.com Terminal-Bench search"
	}
	if containsTerminalBenchName(text) && containsAny(text, terminalBenchNetworkMarkers()) {
		return "Terminal-Bench network/repository lookup"
	}
	return ""
}

func normalizeTerminalBenchProbe(input string) string {
	text := strings.ToLower(input)
	replacer := strings.NewReplacer(
		"\\u002d", "-",
		"\\u005f", "_",
		"%2d", "-",
		"%5f", "_",
		"%2f", "/",
		"%20", " ",
		"+", " ",
	)
	return replacer.Replace(text)
}

func containsTerminalBenchName(text string) bool {
	return strings.Contains(text, "terminal-bench") ||
		strings.Contains(text, "terminalbench") ||
		strings.Contains(text, "terminal bench")
}

func terminalBenchNetworkMarkers() []string {
	return []string{
		"http://",
		"https://",
		"curl",
		"wget",
		"git clone",
		"gh repo",
		"gh api",
		"github.com",
		"raw.githubusercontent.com",
		"api.github.com",
		"google.com/search",
		"bing.com/search",
		"duckduckgo.com",
		"search?q=",
	}
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func roundSeconds(d time.Duration) time.Duration {
	return d.Round(time.Second)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func promptFromArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if len(args) >= 3 && looksLikeJSONObject(args[1]) && looksLikeJSONObject(args[2]) {
		return args[0]
	}
	return strings.Join(args, " ")
}

func looksLikeJSONObject(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "{") || !strings.HasSuffix(value, "}") {
		return false
	}
	var obj map[string]any
	return json.Unmarshal([]byte(value), &obj) == nil
}

func stdinHasData(r io.Reader) bool {
	file, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}

func buildOutput(cfg cliConfig, runID, traceDir string, result *agentsdk.RunResult, duration time.Duration, runErr error) outputEnvelope {
	out := outputEnvelope{
		RunID:       runID,
		TaskID:      cfg.TaskID,
		CandidateID: cfg.CandidateID,
		TraceDir:    traceDir,
		Metrics: runMetrics{
			DurationSec: duration.Seconds(),
		},
	}
	if runErr != nil {
		out.Error = runErr.Error()
	}
	if result == nil {
		return out
	}
	out.FinalText = result.FinalText()
	out.FinalOutput = result.FinalOutput
	out.Interrupted = result.IsInterrupted()
	out.Interruption = result.Interruption
	out.Usage = result.Usage
	out.ToolCalls = collectToolCalls(result.NewItems)
	out.Metrics.ToolCalls = len(out.ToolCalls)
	out.Metrics.TurnsUsed = len(result.RawResponses)
	out.Metrics.CompactionHits = countCompactions(result.NewItems)
	return out
}

func collectToolCalls(items []agentsdk.RunItem) []toolCallSummary {
	var calls []toolCallSummary
	byID := map[string]int{}
	for _, item := range items {
		switch item.Type {
		case agentsdk.RunItemToolCall:
			if item.ToolCall == nil {
				continue
			}
			byID[item.ToolCall.ID] = len(calls)
			calls = append(calls, toolCallSummary{
				ID:    item.ToolCall.ID,
				Name:  item.ToolCall.Name,
				Input: item.ToolCall.Input,
			})
		case agentsdk.RunItemToolOutput:
			if item.ToolOutput == nil {
				continue
			}
			if idx, ok := byID[item.ToolOutput.CallID]; ok {
				calls[idx].Output = item.ToolOutput.Content
				calls[idx].IsError = item.ToolOutput.IsError
				continue
			}
			calls = append(calls, toolCallSummary{
				ID:      item.ToolOutput.CallID,
				Output:  item.ToolOutput.Content,
				IsError: item.ToolOutput.IsError,
			})
		}
	}
	return calls
}

func countCompactions(items []agentsdk.RunItem) int {
	var count int
	for _, item := range items {
		if item.Type == agentsdk.RunItemCompaction {
			count++
		}
	}
	return count
}

func writeOutput(w io.Writer, format string, envelope outputEnvelope) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(envelope)
	}
	_, err := fmt.Fprintln(w, envelope.FinalText)
	return err
}

func closeAll(closers []io.Closer, stderr io.Writer) {
	for _, closer := range closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil {
			fmt.Fprintf(stderr, "close warning: %v\n", err)
		}
	}
}

func parseToolAccess(access string) (agentsdk.ToolAccessLevel, error) {
	raw := strings.ToLower(strings.TrimSpace(access))
	switch raw {
	case "", "full", "write", "workspace-write", "workspace_write", "danger-full-access", "danger_full_access":
		return agentsdk.ToolAccessLevelFull, nil
	case "read-only", "read_only", "readonly", "analysis":
		return agentsdk.ToolAccessLevelReadOnly, nil
	default:
		return "", fmt.Errorf("invalid --tool-access %q (use full or read-only)", access)
	}
}

func parsePermissionMode(mode string) (sdkpolicy.PermissionMode, error) {
	raw := strings.ToLower(strings.TrimSpace(mode))
	switch raw {
	case "":
		return "", nil
	case "read-only", "read_only", "readonly", "analysis":
		return sdkpolicy.PermissionModeReadOnly, nil
	case "workspace-write", "workspace_write", "workspace", "write", "full":
		return sdkpolicy.PermissionModeWorkspaceWrite, nil
	case "danger-full-access", "danger_full_access", "danger", "unrestricted":
		return sdkpolicy.PermissionModeDangerFullAccess, nil
	default:
		return "", fmt.Errorf("invalid --permission-mode %q (use read-only, workspace-write, or danger-full-access)", mode)
	}
}

func resolvedPermissionMode(mode sdkpolicy.PermissionMode, access agentsdk.ToolAccessLevel) sdkpolicy.PermissionMode {
	if mode != "" {
		return sdkpolicy.NormalizePermissionMode(string(mode))
	}
	if agentsdk.NormalizeToolAccessLevel(access) == agentsdk.ToolAccessLevelReadOnly {
		return sdkpolicy.PermissionModeReadOnly
	}
	return sdkpolicy.PermissionModeWorkspaceWrite
}

func defaultInstructions() string {
	return "You are a headless Grateful Agents SDK harness agent. Complete the user's task autonomously using the available tools. Keep the final answer concise and report the outcome."
}

func defaultModelForProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic":
		return "claude-haiku-4-5"
	case "multi":
		return "openai/gpt-4.1-mini"
	case "openrouter":
		return "openai/gpt-4o-mini"
	case "local":
		return "llama3.1"
	default:
		return "gpt-4.1-mini"
	}
}

func defaultRunID() string {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "run-" + time.Now().UTC().Format("20060102T150405Z")
	}
	return "run-" + time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(suffix)
}

func boolScore(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func usageText() string {
	return strings.TrimSpace(`Usage:
  grateful-agent-run [flags] "task prompt"
  grateful-agent-run [flags] --prompt-file task.txt
  echo "task prompt" | grateful-agent-run [flags] --stdin

Useful flags:
  --provider openai|anthropic|openrouter|local
  --model MODEL
  --workdir DIR
  --output text|json
  --trace-root DIR
  --max-turns N
  --timeout 30m
  --async-bash
  --final-check
  --exit-zero-on-timeout
  --tool-access full|read-only
  --permission-mode read-only|workspace-write|danger-full-access`)
}
