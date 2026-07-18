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
//
// ChatLoop applies PermissionMode, ModeSnapshot (read-only tool-access clamp
// and turn constraints), GuardrailRules, ModeDirective, and HandoffHistory in
// prepareRun. RoleCatalog is surfaced for hosts that build specialist tools
// (e.g. via BuildSpecialistToolsFromCatalog in a PlatformToolFactory); ChatLoop
// does not consume it directly.
type ConfigSource interface {
	PermissionMode(ctx context.Context) (PermissionMode, error)
	ModeSnapshot(ctx context.Context) (*sdkmode.TemplateSpec, error)
	GuardrailRules(ctx context.Context) ([]GuardrailRule, error)
	RoleCatalog(ctx context.Context) (RoleCatalog, error)
	ModeDirective(ctx context.Context) (string, error)
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

// Cursor returns the loop's current session-store position. After Run it
// reflects the messages already consumed, so hosts can persist it and later
// loops (or a rebuilt ChatLoop) do not re-feed the same messages.
func (l *ChatLoop) Cursor() Cursor {
	if l == nil {
		return Cursor{}
	}
	return l.opts.Cursor
}

// Run executes a host-backed chat loop.
//
// The loop can resume after SDK tool approvals. Hosts still own their platform
// side effects through SessionStore, ConfigSource, ApprovalGate, RunStatusSink,
// TraceStore, and PlatformToolFactory.
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
	if runCfg.Trace == nil && strings.TrimSpace(runCfg.PromptCacheNamespace) == "" {
		runCfg.PromptCacheNamespace = NewTrace(agent.Name).ID
	}

	history := append([]RunItem(nil), inputItems...)
	var allNewItems []RunItem
	var allResponses []ModelResponse
	var allToolInputResults []ToolGuardrailResult
	var allToolOutputResults []ToolGuardrailResult
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
		allToolInputResults = append(allToolInputResults, result.ToolInputGuardrailResults...)
		allToolOutputResults = append(allToolOutputResults, result.ToolOutputGuardrailResults...)
		totalUsage.Add(result.Usage)
		if len(result.FinalHistory) > 0 {
			// Adopt the runner's post-run conversation state: mid-run
			// compaction rewrites the input items, so replaying
			// history+NewItems would resend the uncompacted transcript.
			history = append([]RunItem(nil), result.FinalHistory...)
		} else {
			history = append(history, result.NewItems...)
		}
		if l.opts.SessionStore != nil {
			if err := l.opts.SessionStore.AppendRunItems(ctx, result.NewItems); err != nil {
				combined := combineLoopResult(result, allNewItems, allResponses, allToolInputResults, allToolOutputResults, totalUsage)
				return combined, fmt.Errorf("append run items: %w", err)
			}
		}

		combined := combineLoopResult(result, allNewItems, allResponses, allToolInputResults, allToolOutputResults, totalUsage)
		if result.IsInterrupted() {
			if l.opts.ApprovalGate == nil {
				// No gate can resolve the pending calls: pair each dangling
				// tool_use with a denied approval and error output so the
				// persisted history stays replayable.
				denied := denyPendingInterruptions(result.AllInterruptions(), "tool call denied: no approval gate configured")
				if len(denied) > 0 {
					allNewItems = append(allNewItems, denied...)
					if l.opts.SessionStore != nil {
						if err := l.opts.SessionStore.AppendRunItems(ctx, denied); err != nil {
							combined = combineLoopResult(result, allNewItems, allResponses, allToolInputResults, allToolOutputResults, totalUsage)
							return combined, fmt.Errorf("append denied approval items: %w", err)
						}
					}
					combined = combineLoopResult(result, allNewItems, allResponses, allToolInputResults, allToolOutputResults, totalUsage)
				}
				return l.finalize(ctx, combined)
			}
			if resumes >= maxResumes {
				return combined, fmt.Errorf("too many chat loop resumes after approval interruption")
			}
			// Resolve every pending approval from the turn: parallel tool
			// calls can trigger several, and each pending tool_use needs a
			// paired output before the next model call.
			var items []RunItem
			var resolveErr error
			var pauseRequested bool
			for _, pending := range result.AllInterruptions() {
				resolved, inputResults, outputResults, shouldPause, err := l.resolveToolApproval(ctx, &agent, runCfg, pending)
				items = append(items, resolved...)
				allToolInputResults = append(allToolInputResults, inputResults...)
				allToolOutputResults = append(allToolOutputResults, outputResults...)
				if shouldPause {
					pauseRequested = true
				}
				if err != nil {
					resolveErr = err
					break
				}
			}
			if len(items) > 0 {
				allNewItems = append(allNewItems, items...)
				history = append(history, items...)
			}
			if len(items) > 0 && l.opts.SessionStore != nil {
				if err := l.opts.SessionStore.AppendRunItems(ctx, items); err != nil {
					combined = combineLoopResult(result, allNewItems, allResponses, allToolInputResults, allToolOutputResults, totalUsage)
					return combined, fmt.Errorf("append approval items: %w", err)
				}
			}
			combined = combineLoopResult(result, allNewItems, allResponses, allToolInputResults, allToolOutputResults, totalUsage)
			if resolveErr != nil {
				return combined, resolveErr
			}
			if pauseRequested {
				// An approved tool requested a pause (ToolResult.ShouldPause);
				// hand control back to the host like the runner's own pause
				// path instead of immediately resuming another model call.
				// The approvals were resolved above, so the interrupted
				// result's pending state must not leak: clear the
				// interruption fields and adopt the loop's history (which
				// includes the resolved approval + tool output items) so
				// hosts neither re-prompt for the same approval nor replay
				// an unpaired tool call.
				combined.Interruption = nil
				combined.Interruptions = nil
				combined.FinalHistory = append([]RunItem(nil), history...)
				return l.finalize(ctx, combined)
			}
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
		if directive, err := l.opts.ConfigSource.ModeDirective(ctx); err != nil {
			return Agent{}, RunConfig{}, fmt.Errorf("load mode directive: %w", err)
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
			// Copy before appending: runCfg shares slice headers with
			// l.opts.RunConfig, and appending in place could write into a
			// host-owned backing array shared across loops or Run calls.
			runCfg.ToolInputGuardrails = append(append([]ToolInputGuardrail(nil), runCfg.ToolInputGuardrails...), inputGuardrails...)
			runCfg.ToolOutputGuardrails = append(append([]ToolOutputGuardrail(nil), runCfg.ToolOutputGuardrails...), outputGuardrails...)
		}
		if snapshot, err := l.opts.ConfigSource.ModeSnapshot(ctx); err != nil {
			return Agent{}, RunConfig{}, fmt.Errorf("load mode snapshot: %w", err)
		} else if snapshot != nil {
			// A read-only mode clamps tool access regardless of what the
			// permission mode granted (mirrors runtime/builder enforcement).
			if NormalizeToolAccessLevel(ToolAccessLevel(snapshot.ToolAccess)) == ToolAccessLevelReadOnly {
				runCfg.ToolAccessLevel = ToolAccessLevelReadOnly
			}
			if snapshot.Constraints != nil {
				if runCfg.MaxTurns == 0 && snapshot.Constraints.MaxTurns > 0 {
					runCfg.MaxTurns = snapshot.Constraints.MaxTurns
				}
				if runCfg.SubAgentMaxTurns == 0 && snapshot.Constraints.SubAgentMaxTurns > 0 {
					runCfg.SubAgentMaxTurns = snapshot.Constraints.SubAgentMaxTurns
				}
			}
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
		// Drain the store page by page so messages beyond one page are not
		// silently dropped; the advancing cursor is kept on the loop so a
		// later Run does not re-feed already consumed messages.
		for {
			messages, next, err := l.opts.SessionStore.LoadMessages(ctx, l.opts.Cursor, limit)
			if err != nil {
				return nil, fmt.Errorf("load session messages: %w", err)
			}
			for _, msg := range messages {
				if strings.TrimSpace(msg.Content) == "" && len(msg.Images) == 0 {
					continue
				}
				inputItems = append(inputItems, RunItem{
					Type:    RunItemMessage,
					Message: &MessageOutput{Text: msg.Content, Images: msg.Images},
				})
			}
			cursorAdvanced := next != l.opts.Cursor
			l.opts.Cursor = next
			if len(messages) < limit || !cursorAdvanced {
				break
			}
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

		action, actionErr := normalizeGuardrailRuleAction(rule)
		if actionErr != nil {
			errs = append(errs, actionErr)
			continue
		}
		rule.Action = action

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
	case "warn":
		log.Printf("WARN: guardrail %q triggered: %s", rule.Name, msg)
		return &GuardrailResult{Output: msg}
	case "log":
		log.Printf("INFO: guardrail %q triggered: %s", rule.Name, msg)
		return &GuardrailResult{}
	default:
		// "block" and any unvalidated action fail closed.
		return &GuardrailResult{Output: msg, TripwireTriggered: true}
	}
}

// normalizeGuardrailRuleAction validates a rule's action. An empty action
// defaults to "block" (fail closed) and unknown values are rejected so a typo
// like "deny" cannot silently disable a rule.
func normalizeGuardrailRuleAction(rule GuardrailRule) (string, error) {
	action := strings.ToLower(strings.TrimSpace(rule.Action))
	switch action {
	case "":
		return "block", nil
	case "block", "warn", "log":
		return action, nil
	default:
		return "", fmt.Errorf("unknown action %q in guardrail rule %q (valid: block, warn, log)", rule.Action, rule.Name)
	}
}

// matchToolPattern matches a tool name against a glob-style pattern where
// each "*" matches any (possibly empty) substring. Patterns like "*sql*" and
// "mcp_*_write" are handled correctly instead of silently matching nothing.
func matchToolPattern(name, pattern string) bool {
	segments := strings.Split(pattern, "*")
	if len(segments) == 1 {
		return name == pattern
	}
	if !strings.HasPrefix(name, segments[0]) {
		return false
	}
	rest := name[len(segments[0]):]
	last := segments[len(segments)-1]
	for _, seg := range segments[1 : len(segments)-1] {
		if seg == "" {
			continue
		}
		idx := strings.Index(rest, seg)
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(seg):]
	}
	return strings.HasSuffix(rest, last)
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

func (l *ChatLoop) resolveToolApproval(ctx context.Context, agent *Agent, cfg RunConfig, pending *Interruption) ([]RunItem, []ToolGuardrailResult, []ToolGuardrailResult, bool, error) {
	if pending == nil {
		return nil, nil, nil, false, nil
	}
	approved, reason, err := l.opts.ApprovalGate.ApproveTool(ctx, ToolApprovalRequest{
		ToolName: pending.ToolName,
		Input:    append([]byte(nil), pending.ToolInput...),
		Reason:   "tool approval required",
	})
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("approve tool %q: %w", pending.ToolName, err)
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
		return items, nil, nil, false, nil
	}
	item, inputResults, outputResults, shouldPause, err := l.opts.Runner.ExecuteApprovedTool(ctx, agent, ToolCallData{
		ID:    pending.ToolCallID,
		Name:  pending.ToolName,
		Input: cloneRawMessage(pending.ToolInput),
	}, cfg)
	if item.ToolOutput != nil {
		items = append(items, item)
	}
	return items, inputResults, outputResults, shouldPause, err
}

// denyPendingInterruptions synthesizes denied approval + error output pairs
// for pending tool calls that cannot be approved (no ApprovalGate). Without
// the paired outputs, persisted history would end with dangling tool_use
// items that providers reject on replay.
func denyPendingInterruptions(pending []*Interruption, reason string) []RunItem {
	var items []RunItem
	for _, p := range pending {
		if p == nil {
			continue
		}
		items = append(items,
			RunItem{
				Type: RunItemToolApproval,
				ToolApproval: &ToolApprovalData{
					ToolName: p.ToolName,
					Input:    cloneRawMessage(p.ToolInput),
					CallID:   p.ToolCallID,
					Approved: false,
				},
			},
			RunItem{
				Type: RunItemToolOutput,
				ToolOutput: &ToolOutputData{
					CallID:  p.ToolCallID,
					Content: reason,
					IsError: true,
				},
			},
		)
	}
	return items
}

func combineLoopResult(result *RunResult, newItems []RunItem, responses []ModelResponse, toolInputResults, toolOutputResults []ToolGuardrailResult, usage Usage) *RunResult {
	if result == nil {
		return nil
	}
	combined := *result
	combined.NewItems = append([]RunItem(nil), newItems...)
	combined.RawResponses = append([]ModelResponse(nil), responses...)
	combined.ToolInputGuardrailResults = append([]ToolGuardrailResult(nil), toolInputResults...)
	combined.ToolOutputGuardrailResults = append([]ToolGuardrailResult(nil), toolOutputResults...)
	combined.Usage = usage
	return &combined
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
	case string(PermissionModeWorkspaceWrite), "":
		return PermissionModeWorkspaceWrite
	default:
		// Fail closed: an unrecognized non-empty mode (e.g. a typo like
		// "workspace-wrte") must not silently grant write access. This
		// mirrors internal/agent/policy.NormalizePermissionMode.
		return PermissionModeReadOnly
	}
}
