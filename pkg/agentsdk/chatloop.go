package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	sdkmode "github.com/gratefulagents/sdk/pkg/agentsdk/mode"
)

// Cursor identifies the host's current position in a session stream.
type Cursor struct {
	MessageID int64  `json:"message_id,omitempty"`
	Token     string `json:"token,omitempty"`
}

// UserMessage is an SDK-native user input loaded from a host session store.
type UserMessage struct {
	ID        int64
	Content   string
	Mode      string
	CreatedAt time.Time
	Images    []ImageAttachment
}

// WorkingState is durable host-maintained context injected into the loop.
type WorkingState struct {
	Goal                  string
	CurrentMode           string
	CurrentPhase          string
	CurrentStep           string
	LastUserMessage       string
	LastAssistantSummary  string
	RecentTurnSummaries   []string
	HistoryFloorMessageID int64
	LastResponseID        string
	Data                  map[string]any
}

// ToolApprovalRequest is a host-native approval question.
type ToolApprovalRequest struct {
	ToolName string
	Input    []byte
	Reason   string
}

// PermissionMode is the host-level permission mode understood by ChatLoop.
type PermissionMode string

const (
	PermissionModeReadOnly         PermissionMode = "read-only"
	PermissionModeWorkspaceWrite   PermissionMode = "workspace-write"
	PermissionModeDangerFullAccess PermissionMode = "danger-full-access"
)

// MCPServerConfig is the SDK-native shape for one MCP server entry.
type MCPServerConfig struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// SessionStore loads user inputs and persists new run items.
type SessionStore interface {
	LoadMessages(ctx context.Context, cursor Cursor, limit int) ([]UserMessage, Cursor, error)
	AppendRunItems(ctx context.Context, items []RunItem) error
	WorkingState(ctx context.Context) (WorkingState, error)
}

// RunStatusSink publishes loop progress to the host platform.
type RunStatusSink interface {
	PublishProgress(ctx context.Context, snapshot ProgressSnapshot) error
	PublishTraceID(ctx context.Context, traceID string) error
	PublishFinalResult(ctx context.Context, result *RunResult) error
}

// ConfigSource supplies dynamic host configuration without leaking platform types.
type ConfigSource interface {
	PermissionMode(ctx context.Context) (PermissionMode, error)
	ModeSnapshot(ctx context.Context) (*sdkmode.TemplateSpec, error)
	GuardrailRules(ctx context.Context) ([]GuardrailRule, error)
	RoleCatalog(ctx context.Context) (RoleCatalog, error)
	MCPServers(ctx context.Context) (map[string]MCPServerConfig, error)
	PhaseDirective(ctx context.Context) (string, error)
	HandoffHistory(ctx context.Context) ([]RunItem, error)
}

// TraceStore is the trace persistence surface consumed by ChatLoop.
type TraceStore interface {
	RunDir(ctx context.Context) (string, error)
	AppendCategory(ctx context.Context, category, text string) error
	WriteFile(ctx context.Context, name string, data []byte) error
	Finalize(ctx context.Context, result *RunResult) error
}

// ApprovalGate asks the host whether a tool or MCP break-glass request is allowed.
type ApprovalGate interface {
	ApproveTool(ctx context.Context, request ToolApprovalRequest) (bool, string, error)
}

// PlatformToolFactory injects host/platform-specific tools into the SDK runner.
type PlatformToolFactory interface {
	BuildTools(ctx context.Context, base []Tool) ([]Tool, error)
}

// ChatLoopOptions configures a ChatLoop.
type ChatLoopOptions struct {
	Runner              *Runner
	Agent               *Agent
	SessionStore        SessionStore
	RunStatusSink       RunStatusSink
	ConfigSource        ConfigSource
	TraceStore          TraceStore
	ApprovalGate        ApprovalGate
	PlatformToolFactory PlatformToolFactory
	RunConfig           RunConfig
	Cursor              Cursor
	MessageLimit        int
	MaxResumes          int
}

// ChatLoop is the SDK-owned high-level orchestration primitive.
type ChatLoop struct {
	opts ChatLoopOptions
}

func NewChatLoop(opts ChatLoopOptions) *ChatLoop {
	return &ChatLoop{opts: opts}
}

// Run executes a host-backed chat loop.
//
// The loop can resume after SDK tool approvals and phase-changing pause tools.
// Hosts still own their platform side effects through SessionStore, ConfigSource,
// ApprovalGate, RunStatusSink, TraceStore, and PlatformToolFactory.
func (l *ChatLoop) Run(ctx context.Context) (*RunResult, error) {
	if l == nil {
		return nil, fmt.Errorf("chat loop is nil")
	}
	if l.opts.Runner == nil {
		return nil, fmt.Errorf("chat loop runner is required")
	}
	if l.opts.Agent == nil {
		return nil, fmt.Errorf("chat loop agent is required")
	}

	agent, runCfg, err := l.prepareRun(ctx)
	if err != nil {
		return nil, err
	}
	inputItems, err := l.loadInputItems(ctx)
	if err != nil {
		return nil, err
	}

	history := append([]RunItem(nil), inputItems...)
	var allNewItems []RunItem
	var allResponses []ModelResponse
	var totalUsage Usage
	maxResumes := l.opts.MaxResumes
	if maxResumes <= 0 {
		maxResumes = 12
	}

	for resumes := 0; ; resumes++ {
		result, err := l.opts.Runner.Run(ctx, &agent, history, runCfg)
		if err != nil {
			return nil, err
		}
		allNewItems = append(allNewItems, result.NewItems...)
		allResponses = append(allResponses, result.RawResponses...)
		totalUsage.Add(result.Usage)
		history = append(history, result.NewItems...)
		if l.opts.SessionStore != nil {
			if err := l.opts.SessionStore.AppendRunItems(ctx, result.NewItems); err != nil {
				combined := combineLoopResult(result, allNewItems, allResponses, totalUsage)
				return combined, fmt.Errorf("append run items: %w", err)
			}
		}

		combined := combineLoopResult(result, allNewItems, allResponses, totalUsage)
		if result.Interruption != nil {
			if l.opts.ApprovalGate == nil {
				return l.finalize(ctx, combined)
			}
			if resumes >= maxResumes {
				return combined, fmt.Errorf("too many chat loop resumes after approval interruption")
			}
			items, err := l.resolveToolApproval(ctx, &agent, runCfg, result.Interruption)
			if len(items) > 0 {
				allNewItems = append(allNewItems, items...)
				history = append(history, items...)
			}
			if len(items) > 0 && l.opts.SessionStore != nil {
				if err := l.opts.SessionStore.AppendRunItems(ctx, items); err != nil {
					combined = combineLoopResult(result, allNewItems, allResponses, totalUsage)
					return combined, fmt.Errorf("append approval items: %w", err)
				}
			}
			if err != nil {
				combined = combineLoopResult(result, allNewItems, allResponses, totalUsage)
				return combined, err
			}
			continue
		}

		if phase, ok := latestSuccessfulSetPhase(result.NewItems); ok && phase != "" && phase != runCfg.Phase {
			if resumes >= maxResumes {
				return combined, fmt.Errorf("too many automatic phase transitions; last phase %q", phase)
			}
			runCfg.Phase = phase
			runCfg.AdditionalInstructions = strings.TrimSpace(runCfg.AdditionalInstructions + "\n\n" + "Active phase changed to " + phase + ". Continue in this phase.")
			continue
		}

		return l.finalize(ctx, combined)
	}
}

func (l *ChatLoop) prepareRun(ctx context.Context) (Agent, RunConfig, error) {
	agent := *l.opts.Agent
	runCfg := l.opts.RunConfig

	if l.opts.ConfigSource != nil {
		mode, err := l.opts.ConfigSource.PermissionMode(ctx)
		if err != nil {
			return Agent{}, RunConfig{}, fmt.Errorf("load permission mode: %w", err)
		}
		if runCfg.ToolPolicy == nil {
			runCfg.ToolPolicy = toolPolicyFromPermissionMode(mode)
		}
		if runCfg.ToolAccessLevel == "" {
			runCfg.ToolAccessLevel = toolAccessLevelFromPermissionMode(mode)
		}
		if directive, err := l.opts.ConfigSource.PhaseDirective(ctx); err != nil {
			return Agent{}, RunConfig{}, fmt.Errorf("load phase directive: %w", err)
		} else if strings.TrimSpace(directive) != "" {
			runCfg.AdditionalInstructions = strings.TrimSpace(runCfg.AdditionalInstructions + "\n\n" + directive)
		}
		if rules, err := l.opts.ConfigSource.GuardrailRules(ctx); err != nil {
			return Agent{}, RunConfig{}, fmt.Errorf("load guardrail rules: %w", err)
		} else if len(rules) > 0 {
			inputGuardrails, outputGuardrails, errs := compileToolGuardrailsFromRules(rules)
			if len(errs) > 0 {
				return Agent{}, RunConfig{}, fmt.Errorf("compile guardrail rules: %w", errors.Join(errs...))
			}
			runCfg.ToolInputGuardrails = append(runCfg.ToolInputGuardrails, inputGuardrails...)
			runCfg.ToolOutputGuardrails = append(runCfg.ToolOutputGuardrails, outputGuardrails...)
		}
	}

	if l.opts.SessionStore != nil {
		state, err := l.opts.SessionStore.WorkingState(ctx)
		if err != nil {
			return Agent{}, RunConfig{}, fmt.Errorf("load working state: %w", err)
		}
		if state.LastAssistantSummary != "" || state.CurrentStep != "" {
			runCfg.WorkingStateContext = BuildWorkingStateContext(state)
		}
	}

	if l.opts.PlatformToolFactory != nil {
		tools, err := l.opts.PlatformToolFactory.BuildTools(ctx, agent.Tools)
		if err != nil {
			return Agent{}, RunConfig{}, fmt.Errorf("build platform tools: %w", err)
		}
		agent.Tools = tools
	}
	return agent, runCfg, nil
}

func (l *ChatLoop) loadInputItems(ctx context.Context) ([]RunItem, error) {
	var inputItems []RunItem
	if l.opts.ConfigSource != nil {
		history, err := l.opts.ConfigSource.HandoffHistory(ctx)
		if err != nil {
			return nil, fmt.Errorf("load handoff history: %w", err)
		}
		inputItems = append(inputItems, history...)
	}
	if l.opts.SessionStore != nil {
		limit := l.opts.MessageLimit
		if limit <= 0 {
			limit = 50
		}
		messages, _, err := l.opts.SessionStore.LoadMessages(ctx, l.opts.Cursor, limit)
		if err != nil {
			return nil, fmt.Errorf("load session messages: %w", err)
		}
		for _, msg := range messages {
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			inputItems = append(inputItems, RunItem{
				Type:    RunItemMessage,
				Message: &MessageOutput{Text: msg.Content},
			})
		}
	}
	return inputItems, nil
}

func compileToolGuardrailsFromRules(rules []GuardrailRule) ([]ToolInputGuardrail, []ToolOutputGuardrail, []error) {
	var inputGuardrails []ToolInputGuardrail
	var outputGuardrails []ToolOutputGuardrail
	var errs []error

	for _, rule := range rules {
		rule := rule
		re, err := regexp.Compile(rule.Regex)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid regex in guardrail rule %q: %w", rule.Name, err))
			continue
		}

		switch rule.Type {
		case "tool-input":
			inputGuardrails = append(inputGuardrails, ToolInputGuardrail{
				Name: fmt.Sprintf("config:%s", rule.Name),
				Fn: func(_ *RunContext, _ *Agent, tool Tool, input json.RawMessage) (*GuardrailResult, error) {
					if rule.ToolPattern != "" && !matchToolPattern(tool.Name(), rule.ToolPattern) {
						return &GuardrailResult{}, nil
					}
					if re.Match(input) {
						return guardrailResultForRule(rule, tool.Name(), false), nil
					}
					return &GuardrailResult{}, nil
				},
			})
		case "tool-output":
			outputGuardrails = append(outputGuardrails, ToolOutputGuardrail{
				Name: fmt.Sprintf("config:%s", rule.Name),
				Fn: func(_ *RunContext, _ *Agent, tool Tool, result ToolResult) (*GuardrailResult, error) {
					if rule.ToolPattern != "" && !matchToolPattern(tool.Name(), rule.ToolPattern) {
						return &GuardrailResult{}, nil
					}
					if re.MatchString(result.Content) {
						return guardrailResultForRule(rule, tool.Name(), true), nil
					}
					return &GuardrailResult{}, nil
				},
			})
		default:
			errs = append(errs, fmt.Errorf("unknown guardrail rule type %q for rule %q", rule.Type, rule.Name))
		}
	}

	return inputGuardrails, outputGuardrails, errs
}

func guardrailResultForRule(rule GuardrailRule, toolName string, output bool) *GuardrailResult {
	msg := strings.TrimSpace(rule.Message)
	if msg == "" {
		if output {
			msg = fmt.Sprintf("Guardrail %q triggered on tool %q output", rule.Name, toolName)
		} else {
			msg = fmt.Sprintf("Guardrail %q triggered on tool %q", rule.Name, toolName)
		}
	}

	switch rule.Action {
	case "block":
		return &GuardrailResult{Output: msg, TripwireTriggered: true}
	case "warn":
		log.Printf("WARN: guardrail %q triggered: %s", rule.Name, msg)
		return &GuardrailResult{Output: msg}
	case "log":
		log.Printf("INFO: guardrail %q triggered: %s", rule.Name, msg)
		return &GuardrailResult{}
	default:
		return &GuardrailResult{}
	}
}

func matchToolPattern(name, pattern string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	}
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(name, strings.TrimPrefix(pattern, "*"))
	}
	return name == pattern
}

func (l *ChatLoop) finalize(ctx context.Context, result *RunResult) (*RunResult, error) {
	if l.opts.TraceStore != nil {
		if err := l.opts.TraceStore.Finalize(ctx, result); err != nil {
			return result, fmt.Errorf("finalize trace: %w", err)
		}
	}
	if l.opts.RunStatusSink != nil {
		if err := l.opts.RunStatusSink.PublishFinalResult(ctx, result); err != nil {
			return result, fmt.Errorf("publish final result: %w", err)
		}
	}
	return result, nil
}

func (l *ChatLoop) resolveToolApproval(ctx context.Context, agent *Agent, cfg RunConfig, pending *Interruption) ([]RunItem, error) {
	if pending == nil {
		return nil, nil
	}
	approved, reason, err := l.opts.ApprovalGate.ApproveTool(ctx, ToolApprovalRequest{
		ToolName: pending.ToolName,
		Input:    append([]byte(nil), pending.ToolInput...),
		Reason:   "tool approval required",
	})
	if err != nil {
		return nil, fmt.Errorf("approve tool %q: %w", pending.ToolName, err)
	}
	items := []RunItem{{
		Type: RunItemToolApproval,
		ToolApproval: &ToolApprovalData{
			ToolName: pending.ToolName,
			Input:    cloneRawMessage(pending.ToolInput),
			CallID:   pending.ToolCallID,
			Approved: approved,
		},
	}}
	if !approved {
		content := strings.TrimSpace(reason)
		if content == "" {
			content = "tool call denied by host approval gate"
		}
		items = append(items, RunItem{
			Type: RunItemToolOutput,
			ToolOutput: &ToolOutputData{
				CallID:  pending.ToolCallID,
				Content: content,
				IsError: true,
			},
		})
		return items, nil
	}
	item, _, _, _, err := l.opts.Runner.ExecuteApprovedTool(ctx, agent, ToolCallData{
		ID:    pending.ToolCallID,
		Name:  pending.ToolName,
		Input: cloneRawMessage(pending.ToolInput),
	}, cfg)
	if item.ToolOutput != nil {
		items = append(items, item)
	}
	return items, err
}

func combineLoopResult(result *RunResult, newItems []RunItem, responses []ModelResponse, usage Usage) *RunResult {
	if result == nil {
		return nil
	}
	combined := *result
	combined.NewItems = append([]RunItem(nil), newItems...)
	combined.RawResponses = append([]ModelResponse(nil), responses...)
	combined.Usage = usage
	return &combined
}

func latestSuccessfulSetPhase(items []RunItem) (string, bool) {
	phaseByCall := map[string]string{}
	var latest string
	for _, item := range items {
		switch item.Type {
		case RunItemToolCall:
			if item.ToolCall == nil || item.ToolCall.Name != "set_phase" {
				continue
			}
			var payload struct {
				Phase string `json:"phase"`
			}
			if err := json.Unmarshal(item.ToolCall.Input, &payload); err == nil && strings.TrimSpace(payload.Phase) != "" {
				phaseByCall[item.ToolCall.ID] = strings.TrimSpace(payload.Phase)
			}
		case RunItemToolOutput:
			if item.ToolOutput == nil || item.ToolOutput.IsError {
				continue
			}
			if phase := phaseByCall[item.ToolOutput.CallID]; phase != "" {
				latest = phase
			}
		}
	}
	return latest, latest != ""
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out
}

func toolPolicyFromPermissionMode(mode PermissionMode) *ToolPolicy {
	switch normalizePermissionMode(mode) {
	case PermissionModeDangerFullAccess:
		return nil
	default:
		return &ToolPolicy{ApprovalRequired: true}
	}
}

func toolAccessLevelFromPermissionMode(mode PermissionMode) ToolAccessLevel {
	if normalizePermissionMode(mode) == PermissionModeReadOnly {
		return ToolAccessLevelReadOnly
	}
	return ToolAccessLevelFull
}

func normalizePermissionMode(mode PermissionMode) PermissionMode {
	switch strings.ToLower(strings.TrimSpace(string(mode))) {
	case string(PermissionModeReadOnly):
		return PermissionModeReadOnly
	case string(PermissionModeDangerFullAccess):
		return PermissionModeDangerFullAccess
	default:
		return PermissionModeWorkspaceWrite
	}
}
