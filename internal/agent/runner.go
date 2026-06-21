package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

const finalSummaryTurnDirective = `<final_turn>
This is your final available turn. No tools are available now.
Return the best concise summary you can from the evidence already gathered.
Include concrete findings, files checked, important gaps/unknowns, and recommended next steps.
Do not ask for more tools or continue exploring.
</final_turn>`

const compactionCarryForwardPrefix = "[COMPACTION CARRY-FORWARD]"

// completionConfirmationPrompt bounces the first final answer back to the
// model when RunConfig.RequireCompletionConfirmation is set. Terminus 2 uses
// the same double-confirm pattern to prevent premature task completion.
const completionConfirmationPrompt = `[SYSTEM] Before this answer is accepted as final: verify your work now.
- Re-read the original task and confirm every requirement is satisfied.
- If there are runnable checks (tests, builds, linters, the command the task asks about), run them and confirm they pass.
- Confirm any files or artifacts the task requires actually exist with the expected content.
If anything is unverified or incomplete, continue working instead of finalizing.
If you are certain the task is complete, provide your final answer again.`

// droppedToolCallNudge is injected when a model response signalled tool calls
// (stop_reason == "tool_use") but arrived with none — a known failure mode on
// proxied/streamed Claude responses where the tool_use blocks are lost in
// transit and only a leading "I'll start by…" preamble survives. classifyResponse
// would otherwise treat that preamble as a final answer, so a sub-agent
// "completes" on turn 1 having done nothing. Re-prompt instead of finalizing.
const droppedToolCallNudge = `[SYSTEM] Your previous response indicated tool calls (stop_reason=tool_use) but none were received — they were dropped in transit, not by you. Re-issue the tool calls you intended now. Do not just describe what you will do; actually call the tools.`

// untrustedToolOutputBegin/End delimit tool result content fed back to the
// model on subsequent turns. See RunConfig.UntrustedToolOutputs and
// docs/security.md.
const (
	untrustedToolOutputBegin = "BEGIN UNTRUSTED TOOL OUTPUT"
	untrustedToolOutputEnd   = "END UNTRUSTED TOOL OUTPUT"
)

// maxRetryAfterMS caps any caller- or provider-supplied retry delay so a
// single misbehaving advice value can't stall the run for hours. See finding M1.
const maxRetryAfterMS = int64(5 * 60 * 1000) // 5 minutes

// maxToolCallRecoveryAttempts bounds how many times a single run will
// re-request after a response signalled tool_use but arrived with no tool
// calls. Recovery re-rolls the turn with a corrective nudge; the cap ensures a
// deterministically-truncating provider still terminates.
const maxToolCallRecoveryAttempts = 2

// capRetryAfterMS returns the retry delay clamped to maxRetryAfterMS. It logs
// when capping is applied so operators can detect runaway advice values.
func capRetryAfterMS(requestedMS int64) int64 {
	if requestedMS <= maxRetryAfterMS {
		return requestedMS
	}
	log.Printf("[runner] capping retry_after_ms %d → %d (5m max)", requestedMS, maxRetryAfterMS)
	return maxRetryAfterMS
}

// wrapToolOutputContent applies the untrusted-tool-output delimiters around
// raw tool result content. Idempotent: already-wrapped content is returned
// unchanged so repeated next-turn appends don't double-wrap.
func wrapToolOutputContent(content string) string {
	if strings.Contains(content, untrustedToolOutputBegin) {
		return content
	}
	return untrustedToolOutputBegin + "\n" + content + "\n" + untrustedToolOutputEnd
}

// wrapToolOutputsForInput returns a copy of items with RunItemToolOutput
// content wrapped in untrusted-output delimiters. Used when splicing tool
// results into the next turn's input. Non-tool-output items pass through.
func wrapToolOutputsForInput(items []RunItem) []RunItem {
	out := make([]RunItem, len(items))
	for i, item := range items {
		out[i] = item
		if item.Type == RunItemToolOutput && item.ToolOutput != nil {
			cloned := *item.ToolOutput
			cloned.Content = wrapToolOutputContent(cloned.Content)
			out[i].ToolOutput = &cloned
		}
	}
	return out
}

// isSubAgentTool reports whether tool t is a wrapped Agent acting as a tool.
// Used by the runner's concurrency semaphore so its decision is based on type
// rather than name prefix (finding M2).
func isSubAgentTool(t Tool) bool {
	for {
		switch v := t.(type) {
		case *agentTool:
			return true
		case *policyToolWrapper:
			t = v.inner
			continue
		default:
			return false
		}
	}
}

// controlFlowToolNames is the explicit allowlist of control-flow tool names
// (finding M3). These tools manage run lifecycle / user interaction and are
// always allowed regardless of tool access level.
var controlFlowToolNames = map[string]struct{}{
	"finish":                        {},
	"set_phase":                     {},
	"AskUserQuestion":               {},
	"present_plan":                  {},
	"save_plan":                     {},
	"get_plan":                      {},
	"RequestMCPBreakGlass":          {},
	"spawn_subagent_task":           {},
	"run_subagent_task":             {},
	"spawn_subagent_graph":          {},
	"list_subagent_tasks":           {},
	"get_subagent_task_status":      {},
	"get_subagent_activity":         {},
	"get_subagent_task_graph":       {},
	"wait_for_subagent_progress":    {},
	"wait_for_subagent_change":      {},
	"send_message_to_subagent_task": {},
	"collect_subagent_result":       {},
	"cancel_subagent_task":          {},
}

type subAgentFinalJoinProvider interface {
	JoinSubAgentResults(context.Context) ([]RunItem, error)
}

type subAgentFinalJoinStateProvider interface {
	HasPendingSubAgentFinalJoin() bool
}

// subAgentResultPoller exposes a non-blocking drain of terminal, undelivered
// managed sub-agent results. The runner polls it at every turn boundary so the
// parent can incorporate early results while slower siblings keep running,
// instead of receiving everything in one batch at final-join.
type subAgentResultPoller interface {
	PollSubAgentResults() []RunItem
}

// checkDuplicateToolNames returns an error if any two tools share a name
// (finding M4). Duplicate registrations cause non-deterministic dispatch and
// can be exploited to shadow safe tools with malicious ones.
func checkDuplicateToolNames(tools []Tool) error {
	seen := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		name := t.Name()
		if _, dup := seen[name]; dup {
			return &AgentError{Message: fmt.Sprintf("duplicate tool registration: %q", name)}
		}
		seen[name] = struct{}{}
	}
	return nil
}

// Runner executes agents. It holds the Model implementation and orchestrates the run loop.
type Runner struct {
	model    Model
	provider ModelProvider // optional; when set, resolves model per-call
	// DefaultHooks are applied to sub-agent runs that don't specify their own hooks.
	// This ensures nested agent tool calls (e.g. agent_explore → Grep) appear in the
	// activity log alongside the parent's events.
	DefaultHooks RunHooks
}

// NewRunner creates a Runner for the given provider ("anthropic" or "openai").
func NewRunner(provider string) (*Runner, error) {
	return nil, fmt.Errorf("default provider %q is not wired in agent core; use NewRunnerWithProvider or sdk providers.NewRunner", provider)
}

// NewRunnerWithProvider creates a Runner backed by the given ModelProvider.
// The provider resolves model names to Model implementations on each call.
func NewRunnerWithProvider(provider ModelProvider) *Runner {
	return &Runner{provider: provider}
}

// NewRunnerWithModel creates a Runner with an existing Model implementation.
func NewRunnerWithModel(model Model) *Runner {
	return &Runner{model: model}
}

// CalculateCost returns the USD cost estimate for the given usage.
// Safe to call regardless of how the runner was constructed.
func (r *Runner) CalculateCost(usage Usage) float64 {
	if r.model != nil {
		return r.model.CalculateCost(usage)
	}
	return 0
}

// Run executes an agent with the given input and returns the result.
// It runs the agent loop: LLM call → process response → tools/handoff/final → repeat.
func (r *Runner) Run(ctx context.Context, agent *Agent, input []RunItem, cfg RunConfig) (*RunResult, error) {
	if r == nil {
		return nil, fmt.Errorf("runner is nil")
	}
	if agent == nil {
		return nil, fmt.Errorf("agent is nil")
	}
	return r.run(ctx, agent, input, cfg, nil)
}

// ExecuteApprovedTool resumes a previously interrupted approval request using
// the same runner preparation path as ordinary tool execution. The tool has
// already been approved by the host, so this method executes the matching tool
// directly while still applying access adapters, IsEnabled filtering, policy
// timeouts, hooks, tracing, and tool input/output guardrails.
func (r *Runner) ExecuteApprovedTool(ctx context.Context, agent *Agent, call ToolCallData, cfg RunConfig) (RunItem, []ToolGuardrailResult, []ToolGuardrailResult, bool, error) {
	if r == nil {
		return RunItem{}, nil, nil, false, fmt.Errorf("runner is nil")
	}
	if agent == nil {
		return RunItem{}, nil, nil, false, fmt.Errorf("agent is nil")
	}
	cfg.ToolAccessLevel = NormalizeToolAccessLevel(cfg.ToolAccessLevel)

	runCtx := newRunContext(ctx, cfg)
	runCtx.ToolAccessLevel = cfg.ToolAccessLevel

	tp := cfg.TracingProcessor
	if tp == nil || cfg.TracingDisabled {
		tp = NoOpTracingProcessor{}
	}
	trace := cfg.Trace
	ownTrace := trace == nil
	if ownTrace {
		trace = NewTrace(agent.Name)
		tp.OnTraceStart(trace)
		defer func() {
			trace.Finish()
			tp.OnTraceEnd(trace)
		}()
	}
	spanParent := cfg.ParentSpanID
	if spanParent == "" {
		spanParent = trace.ID
	}
	runCtx.TracingProcessor = tp
	runCtx.Trace = trace
	runCtx.SpanParentID = spanParent

	tools := prepareToolsForRun(agent.GetAllTools(runCtx), cfg, runCtx)
	var tool Tool
	for _, candidate := range tools {
		if candidate.Name() == call.Name {
			tool = candidate
			break
		}
	}
	if tool == nil {
		return RunItem{
			Type: RunItemToolOutput, Agent: agent,
			ToolOutput: &ToolOutputData{CallID: call.ID, Content: fmt.Sprintf("tool %q is no longer available", call.Name), IsError: true},
		}, nil, nil, false, nil
	}

	result := r.executeSingleTool(ctx, runCtx, agent, tool, call, cfg)
	return result.item, result.inputGuardrails, result.outputGuardrails, result.shouldPause, result.guardrailErr
}

func (r *Runner) run(ctx context.Context, agent *Agent, input []RunItem, cfg RunConfig, streamEvents chan<- StreamEvent) (*RunResult, error) {
	if r == nil {
		return nil, fmt.Errorf("runner is nil")
	}
	if agent == nil {
		return nil, fmt.Errorf("agent is nil")
	}
	cfg.ToolAccessLevel = NormalizeToolAccessLevel(cfg.ToolAccessLevel)
	runCtx := newRunContext(ctx, cfg)
	runCtx.ToolAccessLevel = cfg.ToolAccessLevel
	currentAgent := agent
	currentInput := make([]RunItem, len(input))

	if dupErr := checkDuplicateToolNames(currentAgent.GetAllTools(runCtx)); dupErr != nil {
		return nil, dupErr
	}

	// Initialize tracing.
	tp := cfg.TracingProcessor
	if tp == nil || cfg.TracingDisabled {
		tp = NoOpTracingProcessor{}
	}
	// Use a shared trace if the caller provided one (multi-phase runs share a
	// single OTel trace). Otherwise create a per-Run trace.
	trace := cfg.Trace
	ownTrace := trace == nil
	if ownTrace {
		trace = NewTrace(currentAgent.Name)
		tp.OnTraceStart(trace)
	}
	// Resolve parent span for runner-created spans: explicit > trace root.
	spanParent := cfg.ParentSpanID
	if spanParent == "" {
		spanParent = trace.ID
	}
	runCtx.TracingProcessor = tp
	runCtx.Trace = trace
	runCtx.SpanParentID = spanParent
	defer func() {
		if ownTrace {
			trace.Finish()
			tp.OnTraceEnd(trace)
		}
	}()

	// Initialize logger.
	logLevel := LogLevelNormal
	if cfg.Debug {
		logLevel = LogLevelDebug
	}
	al := NewAgentLogger(logLevel)

	copy(currentInput, input)

	var allItems []RunItem
	var allResponses []ModelResponse
	var llmAttempt int32
	// turnRetryAttempt counts consecutive failed model calls since the last
	// successful response. RetryPolicy budgets retries per failing call site,
	// so the counter resets on success; using the cumulative llmAttempt here
	// would exhaust the retry budget after MaxRetries successful turns.
	var turnRetryAttempt int
	activeFallbackModels := map[*Agent]string{}
	var allToolInputResults []ToolGuardrailResult
	var allToolOutputResults []ToolGuardrailResult
	// pendingCompletion implements double-confirm completion: the first final
	// answer triggers a verification turn, only a second consecutive final
	// answer ends the run (Terminus 2 anti-premature-exit pattern).
	pendingCompletion := false
	// consecutiveToolErrorTurns counts tool turns where every executed tool
	// returned an error; escalation fires at the configured limit.
	consecutiveToolErrorTurns := 0
	toolErrorEscalated := false
	// stopGateBlocks counts consecutive StopGate blocks; the gate is bypassed
	// once the cap is hit so a broken gate cannot loop forever.
	stopGateBlocks := 0
	// toolCallRecoveryAttempts counts how many times this run has re-requested
	// after a response signalled tool_use but arrived with no tool calls
	// (dropped in transit). Capped so a deterministically-broken response can
	// still finalize instead of looping forever.
	toolCallRecoveryAttempts := 0
	verifierRan := false

	maxTurns := cfg.EffectiveMaxTurns()

	var inputGuardrailResults []InputGuardrailResult
	if len(currentAgent.InputGuardrails) > 0 {
		var err error
		inputGuardrailResults, err = runInputGuardrails(runCtx, currentAgent, currentInput)
		if err != nil {
			return nil, err
		}
	}

	for turn := 0; ; turn++ {
		if turn >= maxTurns {
			tools := prepareToolsForRun(currentAgent.GetAllTools(runCtx), cfg, runCtx)
			if hasPendingSubAgentFinalJoin(tools) {
				joinItems, joinErr := collectSubAgentFinalJoinItems(ctx, tools)
				if joinErr != nil {
					return nil, joinErr
				}
				if len(joinItems) > 0 {
					currentInput = append(currentInput, joinItems...)
					allItems = append(allItems, joinItems...)
					emitRunItems(streamEvents, joinItems)
					maxTurns = turn + 2
					continue
				}
			}
			return nil, &MaxTurnsExceeded{MaxTurns: maxTurns}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if cfg.ImmediateInputPoller != nil {
			immediateItems, err := cfg.ImmediateInputPoller(ctx)
			if err != nil {
				log.Printf("[runner] WARN: immediate input poll failed: %v", err)
			} else if len(immediateItems) > 0 {
				currentInput = append(currentInput, immediateItems...)
				allItems = append(allItems, immediateItems...)
			}
		}

		// Build model request.
		instructions := buildRunInstructions(currentAgent.GetInstructions(runCtx), currentAgent, cfg)
		tools := prepareToolsForRun(currentAgent.GetAllTools(runCtx), cfg, runCtx)
		if dupErr := checkDuplicateToolNames(tools); dupErr != nil {
			return nil, dupErr
		}
		// Incremental delivery: inject terminal managed sub-agent results as
		// soon as they are available so the parent can act on fast tasks while
		// slower siblings keep running, instead of receiving everything at
		// final-join.
		if pollItems := pollSubAgentResultItems(tools); len(pollItems) > 0 {
			currentInput = append(currentInput, pollItems...)
			allItems = append(allItems, pollItems...)
			emitRunItems(streamEvents, pollItems)
		}
		pendingSubAgentJoin := hasPendingSubAgentFinalJoin(tools)
		if cfg.ForceFinalSummaryTurn && turn == maxTurns-1 && !pendingSubAgentJoin {
			instructions = instructions + "\n\n" + finalSummaryTurnDirective
			tools = nil
		}
		settings := currentAgent.ModelSettings.Merge(cfg.ModelSettings)

		// Resolve model name.
		primaryModelName := currentAgent.Model
		if cfg.ModelOverride != "" {
			primaryModelName = cfg.ModelOverride
		}
		requestedModelName := primaryModelName
		if fallbackModel := strings.TrimSpace(activeFallbackModels[currentAgent]); fallbackModel != "" {
			requestedModelName = fallbackModel
		}
		modelName := requestedModelName

		// Resolve model via provider if available (per-call resolution).
		activeModel := r.model
		if r.provider != nil {
			resolved, resolveErr := r.provider.GetModel(requestedModelName)
			if resolveErr != nil {
				return nil, &AgentError{Message: fmt.Sprintf("resolve model %q", requestedModelName), Cause: resolveErr}
			}
			activeModel = resolved
			// Strip the provider routing prefix (e.g. "openai/gpt-5.4" →
			// "gpt-5.4") so the bare model name is sent in API requests.
			// Model IDs that legitimately contain "/" (e.g. OpenRouter's
			// "anthropic/claude-...") are preserved.
			modelName = resolveRequestModelName(r.provider, activeModel, requestedModelName)
		}
		if activeModel == nil {
			return nil, &AgentError{Message: "no model configured: set agent.Model, cfg.ModelOverride, or use NewRunnerWithProvider"}
		}

		requestOverheadTokens := estimateModelRequestOverheadTokens(instructions, tools, settings)
		compactRequest := ModelRequest{
			Model:        modelName,
			Instructions: instructions,
			Input:        currentInput,
			Tools:        tools,
			Settings:     settings,
			OutputSchema: currentAgent.OutputType,
		}
		if compactResult, before, after, ok, compactErr := compactRunItemsWithModelAPI(ctx, activeModel, compactRequest, requestOverheadTokens, cfg.CompactionConfig, false); ok {
			var carryForward string
			currentInput, carryForward = applyCompactionCarryForward(ctx, compactResult.Items, currentInput, cfg)
			if guardErr := guardCompactionCarryForward(runCtx, currentAgent, carryForward); guardErr != nil {
				return nil, guardErr
			}
			after = estimateRunItemsTokens(currentInput) + requestOverheadTokens
			runCtx.Usage.Add(compactResult.Usage)
			if cfg.CompactionRecorder != nil {
				cfg.CompactionRecorder(before, after, appendCompactionSummaryCarryForward(compactResult.Summary, carryForward))
			}
		} else {
			if compactErr != nil {
				log.Printf("[runner] WARN: provider compaction failed; falling back to local compaction: %v", compactErr)
			}
			if compacted, before, after, ok, reason := MaybeCompactRunItemsForRequest(currentInput, cfg.CompactionConfig, requestOverheadTokens); ok {
				var carryForward string
				currentInput, carryForward = applyCompactionCarryForward(ctx, compacted, currentInput, cfg)
				if guardErr := guardCompactionCarryForward(runCtx, currentAgent, carryForward); guardErr != nil {
					return nil, guardErr
				}
				after = estimateRunItemsTokens(currentInput) + requestOverheadTokens
				if cfg.CompactionRecorder != nil {
					cfg.CompactionRecorder(before, after, appendCompactionSummaryCarryForward(ExtractCompactionSummary(compacted), carryForward))
				}
			} else if cfg.CompactionFailureReporter != nil && reason != "disabled" && reason != "below-threshold" {
				if compactErr != nil {
					reason = fmt.Sprintf("provider-compaction-failed: %v; local-compaction: %s", compactErr, reason)
				}
				cfg.CompactionFailureReporter("run", reason, before, after)
			}
		}

		// --- Structured turn logging ---
		{
			var toolNames []string
			for _, t := range tools {
				toolNames = append(toolNames, t.Name())
			}
			var handoffNames []string
			for _, h := range currentAgent.Handoffs {
				handoffNames = append(handoffNames, h.ToolName)
			}
			al.Turn(turn, currentAgent.Name, currentAgent.Model)
			al.Tools(toolNames, handoffNames, cfg.ToolAccessLevel, cfg.Phase)
			al.Instructions(instructions)
			al.InputItems(currentInput)
			al.TurnEnd(turn)
		}

		// Fire hooks.
		fireRunHook(cfg.Hooks, func(h RunHooks) { h.OnAgentStart(runCtx, currentAgent) })
		fireAgentHook(currentAgent.Hooks, func(h AgentHooks) { h.OnStart(runCtx, currentAgent) })
		fireRunHook(cfg.Hooks, func(h RunHooks) { h.OnLLMStart(runCtx, currentAgent) })

		llmAttempt++
		modelIdentity := NormalizeModelIdentity(requestedModelName, activeModel.Provider())
		parentCallID := ParentCallIDFromContext(runCtx.Context())
		taskID := TaskIDFromContext(runCtx.Context())
		llmScope := "top_level"
		if taskID != "" || parentCallID != "" {
			llmScope = "subagent"
		}
		if taskID == "" && parentCallID != "" {
			// Legacy fallback for nested runs that only threaded the parent tool call ID.
			taskID = parentCallID
		}

		// Plan recitation: append a transient plan/goals message as the last
		// input item of this request only. It is regenerated each turn and
		// never persisted to history, so the append-only conversation and
		// cache-stable prefix are preserved while goals stay in the model's
		// high-attention window.
		requestInput := currentInput
		if cfg.PlanRecitation != nil {
			if recitation := strings.TrimSpace(cfg.PlanRecitation(ctx)); recitation != "" {
				requestInput = append(append([]RunItem(nil), currentInput...), RunItem{
					Type:    RunItemMessage,
					Message: &MessageOutput{Text: "<current_plan>\n" + recitation + "\n</current_plan>"},
				})
			}
		}

		modelRequest := ModelRequest{
			Model:               modelName,
			Instructions:        instructions,
			Input:               requestInput,
			Tools:               tools,
			Settings:            settings,
			OutputSchema:        currentAgent.OutputType,
			CompactionThreshold: modelCompactionThreshold(activeModel, cfg.CompactionConfig),
		}
		requestSnapshot := BuildLLMRequestSnapshot(currentAgent.Name, modelRequest)
		genData := &GenerationSpanData{
			RequestedModel:               modelIdentity.Raw,
			ResolvedModel:                modelName,
			ModelProvider:                modelIdentity.Provider,
			ModelCanonical:               modelIdentity.Canonical,
			AttemptNumber:                llmAttempt,
			Turn:                         int32(turn + 1),
			Scope:                        llmScope,
			TaskID:                       taskID,
			Phase:                        cfg.Phase,
			ToolCount:                    int32(len(tools)),
			InputItemCount:               int32(len(currentInput)),
			InstructionsLength:           int32(len(instructions)),
			InputTokenEstimate:           int32(requestSnapshot.InputTokenEstimate),
			RequestOverheadTokenEstimate: int32(requestSnapshot.RequestOverheadTokenEstimate),
			TotalRequestTokenEstimate:    int32(requestSnapshot.TotalTokenEstimate),
			Request:                      requestSnapshot,
		}
		genSpan := NewSpan("generation", spanParent, genData)
		attemptID := genSpan.ID
		attemptEvent := func(status string) ContentEvent {
			return ContentEvent{
				ToolUseID:      attemptID,
				LLMAttempt:     llmAttempt,
				LLMScope:       llmScope,
				AttemptNumber:  llmAttempt,
				AttemptStatus:  status,
				Scope:          llmScope,
				RequestedModel: modelIdentity.Raw,
				ResolvedModel:  modelName,
				CanonicalModel: modelIdentity.Canonical,
				Provider:       modelIdentity.Provider,
				AgentName:      currentAgent.Name,
				TaskID:         taskID,
				Status:         status,
				Model:          modelIdentity.Canonical,
				Turn:           int32(turn + 1),
			}
		}
		emitLLMAttemptEvent(cfg.Hooks, attemptEvent("started"))
		tp.OnSpanStart(genSpan)

		// Call model.
		llmStartedAt := time.Now()
		resp, err := r.callModel(ctx, activeModel, modelRequest, streamEvents)

		if err != nil {
			turnRetryAttempt++
			advice := activeModel.GetRetryAdvice(err)
			genData.LatencyMS = time.Since(llmStartedAt).Milliseconds()
			genData.Error = err.Error()
			genData.FailureKind = generationFailureKind(err, advice)
			genData.Status = "failed"
			genSpan.Data = genData
			genSpan.Finish()
			tp.OnSpanEnd(genSpan)
			trace.AddSpan(genSpan)

			if isContextCancellation(err) {
				ev := attemptFailureEvent(attemptEvent("failed"), genData)
				emitLLMAttemptEvent(cfg.Hooks, ev)
				return nil, err
			}

			if isContextLengthExceededError(err) {
				forcedRequest := ModelRequest{
					Model:        modelName,
					Instructions: instructions,
					Input:        currentInput,
					Tools:        tools,
					Settings:     settings,
					OutputSchema: currentAgent.OutputType,
				}
				if compactResult, before, after, ok, compactErr := compactRunItemsWithModelAPI(ctx, activeModel, forcedRequest, requestOverheadTokens, cfg.CompactionConfig, true); ok {
					var carryForward string
					currentInput, carryForward = applyCompactionCarryForward(ctx, compactResult.Items, currentInput, cfg)
					if guardErr := guardCompactionCarryForward(runCtx, currentAgent, carryForward); guardErr != nil {
						return nil, guardErr
					}
					after = estimateRunItemsTokens(currentInput) + requestOverheadTokens
					runCtx.Usage.Add(compactResult.Usage)
					if cfg.CompactionRecorder != nil {
						cfg.CompactionRecorder(before, after, appendCompactionSummaryCarryForward(compactResult.Summary, carryForward))
					}
					ev := attemptFailureEvent(attemptEvent("retrying"), genData)
					ev.RetryPlanned = true
					emitLLMAttemptEvent(cfg.Hooks, ev)
					continue
				} else if compactErr != nil {
					log.Printf("[runner] WARN: provider compaction after context error failed; falling back to local compaction: %v", compactErr)
				}

				forcedCfg := cfg.CompactionConfig.normalized()
				beforeEstimate := estimateRunItemsTokens(currentInput)
				forcedCfg.TriggerTokens = 1
				forcedCfg.TargetTokens = minInt(forcedCfg.TargetTokens, maxInt(1, beforeEstimate/2))
				if compacted, before, after, ok, reason := MaybeCompactRunItems(currentInput, forcedCfg); ok {
					var carryForward string
					currentInput, carryForward = applyCompactionCarryForward(ctx, compacted, currentInput, cfg)
					if guardErr := guardCompactionCarryForward(runCtx, currentAgent, carryForward); guardErr != nil {
						return nil, guardErr
					}
					after = estimateRunItemsTokens(currentInput)
					if cfg.CompactionRecorder != nil {
						cfg.CompactionRecorder(before, after, appendCompactionSummaryCarryForward(ExtractCompactionSummary(compacted), carryForward))
					}
					ev := attemptFailureEvent(attemptEvent("retrying"), genData)
					ev.RetryPlanned = true
					emitLLMAttemptEvent(cfg.Hooks, ev)
					continue
				} else if cfg.CompactionFailureReporter != nil && reason != "disabled" && reason != "below-threshold" {
					cfg.CompactionFailureReporter("run", reason, before, after)
				}
			}

			if r.provider != nil && shouldFallbackModelCall(err, advice) {
				if fallbackModel, ok := nextFallbackModel(primaryModelName, requestedModelName, effectiveFallbackModels(currentAgent, cfg)); ok {
					activeFallbackModels[currentAgent] = fallbackModel
					reason := fallbackReason(advice)
					if reason == "" {
						reason = genData.FailureKind
					}
					genData.FallbackScheduled = true
					genData.FallbackFromModel = requestedModelName
					genData.FallbackToModel = fallbackModel
					genData.FallbackReason = reason
					genData.Status = "fallback"
					genSpan.Data = genData
					ev := attemptFailureEvent(attemptEvent("fallback"), genData)
					ev.RetryPlanned = true
					ev.FallbackPlanned = true
					ev.FallbackFromModel = requestedModelName
					ev.FallbackToModel = fallbackModel
					ev.FallbackReason = reason
					emitLLMAttemptEvent(cfg.Hooks, ev)
					continue
				}
			}

			// Consult error handler if configured.
			if cfg.ErrorHandler != nil {
				decision := cfg.ErrorHandler(RunErrorData{Error: err, Agent: currentAgent, Turn: turn})
				switch decision.Action {
				case ErrorActionRetry:
					genData.Status = "retrying"
					genSpan.Data = genData
					ev := attemptFailureEvent(attemptEvent("retrying"), genData)
					ev.RetryPlanned = true
					emitLLMAttemptEvent(cfg.Hooks, ev)
					continue
				case ErrorActionContinue:
					log.Printf("[runner] error handler chose continue on turn %d: %v", turn, err)
					ev := attemptFailureEvent(attemptEvent("failed"), genData)
					emitLLMAttemptEvent(cfg.Hooks, ev)
					continue
				case ErrorActionAbort:
					ev := attemptFailureEvent(attemptEvent("failed"), genData)
					emitLLMAttemptEvent(cfg.Hooks, ev)
					return nil, &AgentError{Message: fmt.Sprintf("model call failed on turn %d", turn), Cause: err}
				}
			}

			if shouldRetryWithPolicy(cfg.RetryPolicy, turnRetryAttempt) && !retryPolicyBlockedByAdvice(advice) {
				delay := cfg.RetryPolicy.DelayForAttempt(turnRetryAttempt - 1)
				cappedMS := capRetryAfterMS(delay.Milliseconds())
				delay = time.Duration(cappedMS) * time.Millisecond
				genData.RetryScheduled = true
				genData.RetryAfterMS = cappedMS
				genData.Status = "retrying"
				genSpan.Data = genData
				ev := attemptFailureEvent(attemptEvent("retrying"), genData)
				ev.RetryPlanned = true
				ev.RetryAfterMs = genData.RetryAfterMS
				emitLLMAttemptEvent(cfg.Hooks, ev)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(delay):
				}
				continue
			}

			if advice != nil && advice.ShouldRetry {
				cappedMS := capRetryAfterMS(int64(advice.RetryAfterMS))
				genData.RetryScheduled = true
				genData.RetryAfterMS = cappedMS
				genData.Status = "retrying"
				genSpan.Data = genData
				ev := attemptFailureEvent(attemptEvent("retrying"), genData)
				ev.RetryPlanned = true
				ev.RetryAfterMs = genData.RetryAfterMS
				emitLLMAttemptEvent(cfg.Hooks, ev)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(cappedMS) * time.Millisecond):
				}
				continue // retry this turn
			}
			ev := attemptFailureEvent(attemptEvent("failed"), genData)
			emitLLMAttemptEvent(cfg.Hooks, ev)
			return nil, &AgentError{Message: fmt.Sprintf("model call failed on turn %d", turn), Cause: err}
		}

		costUSD, costKnown := estimateModelCost(activeModel, resp.Usage)
		turnRetryAttempt = 0
		resp.CostUSD = costUSD
		resp.CostKnown = costKnown
		genData.PromptTokens = int64(resp.Usage.InputTokens)
		genData.CompletionTokens = int64(resp.Usage.OutputTokens)
		genData.CacheReadTokens = int64(resp.Usage.CacheReadTokens)
		genData.CacheCreateTokens = int64(resp.Usage.CacheCreateTokens)
		genData.TotalTokens = resp.Usage.TotalTokens()
		genData.CostUSD = costUSD
		genData.CostKnown = costKnown
		genData.LatencyMS = time.Since(llmStartedAt).Milliseconds()
		genData.UsageAvailable = true
		genData.OutputItemCount = int32(len(resp.Items))
		genData.Response = BuildLLMResponseSnapshot(resp)
		genData.Success = true
		genData.Status = "completed"
		genSpan.Data = genData
		genSpan.Finish()
		tp.OnSpanEnd(genSpan)
		trace.AddSpan(genSpan)
		ev := attemptEvent("completed")
		ev.UsageAvailable = true
		ev.HasPromptTokens = true
		ev.HasCompletionTokens = true
		ev.HasTotalTokens = true
		ev.InputTokens = genData.PromptTokens
		ev.OutputTokens = genData.CompletionTokens
		ev.CacheReadInputTokens = resp.Usage.CacheReadTokens
		ev.CacheCreationInputTokens = resp.Usage.CacheCreateTokens
		ev.TotalTokens = genData.TotalTokens
		ev.CostUsd = genData.CostUSD
		ev.CostKnown = genData.CostKnown
		ev.AttemptLatencyMs = genData.LatencyMS
		emitLLMAttemptEvent(cfg.Hooks, ev)

		fireRunHook(cfg.Hooks, func(h RunHooks) { h.OnLLMEnd(runCtx, currentAgent, resp) })

		// Track usage.
		runCtx.Usage.Add(resp.Usage)
		allResponses = append(allResponses, *resp)

		// Process response items.
		newItems := resp.ToRunItems()
		for i := range newItems {
			newItems[i].Agent = currentAgent
		}
		allItems = append(allItems, newItems...)
		emitRunItems(streamEvents, newItems)
		responseCompactionSummary, responseHasCompaction := providerCompactionSummaryFromItems(newItems)
		responseCompactionRecorded := false
		recordAndPruneResponseCompaction := func(items []RunItem) []RunItem {
			if !responseHasCompaction || responseCompactionRecorded {
				return items
			}
			pruned, prunedOK := pruneBeforeLatestProviderCompaction(items)
			if !prunedOK {
				return items
			}
			before := requestSnapshot.TotalTokenEstimate
			var carryForward string
			pruned, carryForward = applyCompactionCarryForward(ctx, pruned, items, cfg)
			after := estimateRunItemsTokens(pruned) + requestOverheadTokens
			if cfg.CompactionRecorder != nil {
				cfg.CompactionRecorder(before, after, appendCompactionSummaryCarryForward(responseCompactionSummary, carryForward))
			}
			responseCompactionRecorded = true
			return pruned
		}

		// Classify the response.
		step := classifyResponse(newItems, currentAgent)

		// Dropped-tool-call recovery: a response that signalled tool_use but
		// classified as a final output means the tool calls were lost in
		// transit (only a leading preamble survived). Finalizing here would end
		// the run on an empty turn — the failure mode where a sub-agent
		// "completes" on turn 1 having done nothing. Re-request with a
		// corrective nudge instead, bounded by maxToolCallRecoveryAttempts.
		if _, isFinal := step.(*finalOutputStep); isFinal &&
			strings.EqualFold(resp.StopReason, "tool_use") &&
			toolCallRecoveryAttempts < maxToolCallRecoveryAttempts {
			toolCallRecoveryAttempts++
			log.Printf("[runner] agent %q returned stop_reason=tool_use with no tool calls (dropped in transit); re-requesting (recovery %d/%d)",
				currentAgent.Name, toolCallRecoveryAttempts, maxToolCallRecoveryAttempts)
			nudge := RunItem{Type: RunItemMessage, Message: &MessageOutput{Text: droppedToolCallNudge}}
			currentInput = append(currentInput, newItems...)
			currentInput = append(currentInput, nudge)
			allItems = append(allItems, nudge)
			emitRunItems(streamEvents, []RunItem{nudge})
			if turn >= maxTurns-1 {
				maxTurns++
			}
			continue
		}

		switch s := step.(type) {
		case *finalOutputStep:
			joinItems, joinErr := collectSubAgentFinalJoinItems(ctx, tools)
			if joinErr != nil {
				return nil, joinErr
			}
			if len(joinItems) > 0 {
				currentInput = append(currentInput, newItems...)
				currentInput = append(currentInput, joinItems...)
				allItems = append(allItems, joinItems...)
				emitRunItems(streamEvents, joinItems)
				if turn >= maxTurns-1 {
					maxTurns++
				}
				continue
			}

			// Double-confirm completion: bounce the first final answer back
			// with a verification prompt. Tool availability (len(tools) > 0)
			// guards the forced no-tool final summary turn, where bouncing
			// would be pointless.
			if cfg.RequireCompletionConfirmation && !pendingCompletion && len(tools) > 0 {
				pendingCompletion = true
				confirmItem := RunItem{
					Type:    RunItemMessage,
					Message: &MessageOutput{Text: completionConfirmationPrompt},
				}
				currentInput = append(currentInput, newItems...)
				currentInput = append(currentInput, confirmItem)
				allItems = append(allItems, confirmItem)
				emitRunItems(streamEvents, []RunItem{confirmItem})
				if turn >= maxTurns-1 {
					maxTurns++
				}
				continue
			}

			// Deterministic stop gate: block finalization with feedback until
			// the gate passes or the consecutive-block cap is hit.
			if cfg.StopGate != nil && len(tools) > 0 && stopGateBlocks < cfg.EffectiveStopGateMaxBlocks() {
				if ok, feedback := cfg.StopGate(ctx, finalOutputText(s.output)); !ok {
					stopGateBlocks++
					if strings.TrimSpace(feedback) == "" {
						feedback = "the finalization check failed; continue working until it passes"
					}
					gateItem := RunItem{
						Type:    RunItemMessage,
						Message: &MessageOutput{Text: "[SYSTEM] Final answer blocked by the completion gate (" + strconv.Itoa(stopGateBlocks) + "/" + strconv.Itoa(cfg.EffectiveStopGateMaxBlocks()) + "):\n" + feedback},
					}
					currentInput = append(currentInput, newItems...)
					currentInput = append(currentInput, gateItem)
					allItems = append(allItems, gateItem)
					emitRunItems(streamEvents, []RunItem{gateItem})
					if turn >= maxTurns-1 {
						maxTurns++
					}
					continue
				}
				stopGateBlocks = 0
			}

			// Adversarial verification: give a critic one chance to refute
			// the candidate final answer.
			if cfg.FinalAnswerVerifier != nil && !verifierRan && len(tools) > 0 {
				verifierRan = true
				feedback, vErr := cfg.FinalAnswerVerifier(ctx, finalOutputText(s.output))
				if vErr != nil {
					log.Printf("[runner] WARN: final answer verifier failed: %v", vErr)
				} else if strings.TrimSpace(feedback) != "" {
					verifyItem := RunItem{
						Type:    RunItemMessage,
						Message: &MessageOutput{Text: "[SYSTEM] An independent reviewer examined your answer before finalization and raised these points. Address the valid ones (with tools if needed), then provide your final answer again:\n" + feedback},
					}
					currentInput = append(currentInput, newItems...)
					currentInput = append(currentInput, verifyItem)
					allItems = append(allItems, verifyItem)
					emitRunItems(streamEvents, []RunItem{verifyItem})
					pendingCompletion = false
					if turn >= maxTurns-1 {
						maxTurns++
					}
					continue
				}
			}

			// recordAndPruneResponseCompaction is invoked purely for its side
			// effect (recording compaction stats / firing the recorder hook);
			// the pruned slice is unused on the final-output branch because the
			// run is about to return.
			_ = recordAndPruneResponseCompaction(append(append([]RunItem(nil), currentInput...), newItems...))

			// Run output guardrails.
			var outputGuardrailResults []OutputGuardrailResult
			if len(currentAgent.OutputGuardrails) > 0 {
				outputGuardrailResults, err = runOutputGuardrails(runCtx, currentAgent, s.output)
				if err != nil {
					return nil, err
				}
			}

			fireAgentHook(currentAgent.Hooks, func(h AgentHooks) { h.OnEnd(runCtx, currentAgent, s.output) })
			fireRunHook(cfg.Hooks, func(h RunHooks) { h.OnAgentEnd(runCtx, currentAgent, s.output) })

			return &RunResult{
				FinalOutput:                s.output,
				LastAgent:                  currentAgent,
				NewItems:                   allItems,
				RawResponses:               allResponses,
				InputGuardrailResults:      inputGuardrailResults,
				OutputGuardrailResults:     outputGuardrailResults,
				ToolInputGuardrailResults:  allToolInputResults,
				ToolOutputGuardrailResults: allToolOutputResults,
				Usage:                      runCtx.Usage,
			}, nil

		case *handoffStep:
			handoffSpan := NewSpan("handoff", spanParent, &HandoffSpanData{FromAgent: currentAgent.Name, ToAgent: s.target.Name})
			tp.OnSpanStart(handoffSpan)

			fireAgentHook(currentAgent.Hooks, func(h AgentHooks) { h.OnHandoff(runCtx, currentAgent, s.target) })
			fireRunHook(cfg.Hooks, func(h RunHooks) { h.OnHandoff(runCtx, currentAgent, s.target) })

			if s.handoff.OnHandoff != nil {
				input := handoffCallInput(newItems, s.handoff.ToolName)
				if s.handoff.InputType != nil && len(input) > 0 {
					if _, vErr := s.handoff.InputType.Validate(string(input)); vErr != nil {
						log.Printf("[runner] WARN: handoff %q input failed schema validation: %v", s.handoff.ToolName, vErr)
					}
				}
				s.handoff.OnHandoff(runCtx, input)
			}

			handoffOutputs := handoffToolOutputs(newItems, s.handoff, currentAgent, s.target)
			if len(handoffOutputs) > 0 {
				allItems = append(allItems, handoffOutputs...)
				emitRunItems(streamEvents, handoffOutputs)
			}

			nextInput := append([]RunItem(nil), currentInput...)
			if s.handoff.InputFilter != nil {
				nextInput = s.handoff.InputFilter(nextInput, allItems)
			} else {
				nextInput = append(nextInput, newItems...)
				nextInput = append(nextInput, handoffOutputs...)
			}
			nextInput = recordAndPruneResponseCompaction(nextInput)
			if compacted, before, after, ok, reason := MaybeCompactHandoffInput(nextInput, cfg.HandoffHistory); ok {
				var carryForward string
				nextInput, carryForward = applyCompactionCarryForward(ctx, compacted, nextInput, cfg)
				after = estimateRunItemsTokens(nextInput)
				if cfg.CompactionRecorder != nil {
					cfg.CompactionRecorder(before, after, appendCompactionSummaryCarryForward(ExtractCompactionSummary(compacted), carryForward))
				}
			} else if cfg.CompactionFailureReporter != nil && reason != "disabled" && reason != "below-threshold" {
				cfg.CompactionFailureReporter("handoff", reason, before, after)
			}
			currentInput = nextInput
			currentAgent = s.target
			pendingCompletion = false

			handoffSpan.Finish()
			tp.OnSpanEnd(handoffSpan)
			trace.AddSpan(handoffSpan)

		case *toolCallStep:
			pendingCompletion = false
			stopGateBlocks = 0
			toolCallRecoveryAttempts = 0
			toolResults, toolInResults, toolOutResults, toolShouldPause, toolGuardErr := r.executeTools(ctx, runCtx, currentAgent, s.toolCalls, cfg)
			if toolGuardErr != nil {
				return nil, toolGuardErr
			}
			allToolInputResults = append(allToolInputResults, toolInResults...)
			allToolOutputResults = append(allToolOutputResults, toolOutResults...)
			allItems = append(allItems, toolResults...)
			emitRunItems(streamEvents, toolResults)
			currentInput = append(currentInput, newItems...)
			// H3: wrap tool outputs as untrusted before they become next-turn
			// model input. Original toolResults retained in allItems untouched
			// so traces and final results show the raw payload.
			nextTurnToolResults := toolResults
			if cfg.ShouldTagUntrustedToolOutputs() {
				nextTurnToolResults = wrapToolOutputsForInput(toolResults)
			}
			currentInput = append(currentInput, nextTurnToolResults...)
			currentInput = recordAndPruneResponseCompaction(currentInput)

			// Three-strike escalation: when several consecutive tool turns
			// produce only errors, inject a corrective note instead of letting
			// the model keep grinding on a failing approach.
			if limit := cfg.EffectiveConsecutiveToolErrorLimit(); limit > 0 {
				outputs, errored := 0, 0
				for _, tr := range toolResults {
					if tr.Type == RunItemToolOutput && tr.ToolOutput != nil {
						outputs++
						if tr.ToolOutput.IsError {
							errored++
						}
					}
				}
				if outputs > 0 {
					if errored == outputs {
						consecutiveToolErrorTurns++
					} else {
						consecutiveToolErrorTurns = 0
						toolErrorEscalated = false
					}
					if consecutiveToolErrorTurns >= limit && !toolErrorEscalated {
						toolErrorEscalated = true
						escalation := RunItem{
							Type: RunItemMessage,
							Message: &MessageOutput{Text: fmt.Sprintf(
								"[SYSTEM] Your last %d tool turns all failed. Stop repeating the same approach. Re-read the error messages above carefully, then either: (1) try a fundamentally different approach or tool, (2) inspect the environment to understand why the calls fail, or (3) if the task is genuinely blocked, report the blocker and what you tried instead of retrying.",
								consecutiveToolErrorTurns)},
						}
						currentInput = append(currentInput, escalation)
						allItems = append(allItems, escalation)
						emitRunItems(streamEvents, []RunItem{escalation})
					}
				}
			}

			// Check for tool approval interruptions.
			for _, tr := range toolResults {
				if tr.Type == RunItemToolApproval {
					return &RunResult{
						LastAgent:                  currentAgent,
						NewItems:                   allItems,
						RawResponses:               allResponses,
						ToolInputGuardrailResults:  allToolInputResults,
						ToolOutputGuardrailResults: allToolOutputResults,
						Usage:                      runCtx.Usage,
						Interruption: &Interruption{
							ToolName:   tr.ToolApproval.ToolName,
							ToolInput:  tr.ToolApproval.Input,
							ToolCallID: tr.ToolApproval.CallID,
						},
					}, nil
				}
			}

			if currentAgent.ToolUseBehavior == StopOnFirstTool || shouldStopAtTools(currentAgent, s.toolCalls) {
				finalOutput := finalOutputFromToolResults(currentAgent, toolResults)
				var outputGuardrailResults []OutputGuardrailResult
				if len(currentAgent.OutputGuardrails) > 0 {
					var err error
					outputGuardrailResults, err = runOutputGuardrails(runCtx, currentAgent, finalOutput)
					if err != nil {
						return nil, err
					}
				}
				return &RunResult{
					FinalOutput:                finalOutput,
					LastAgent:                  currentAgent,
					NewItems:                   allItems,
					RawResponses:               allResponses,
					ToolInputGuardrailResults:  allToolInputResults,
					ToolOutputGuardrailResults: allToolOutputResults,
					OutputGuardrailResults:     outputGuardrailResults,
					Usage:                      runCtx.Usage,
				}, nil
			}

			// Pause when a tool explicitly requests it (e.g. set_phase with
			// approval gate) or when the LLM called a pause tool like
			// present_plan or AskUserQuestion.
			shouldPause := toolShouldPause
			if !shouldPause {
				for _, tc := range s.toolCalls {
					if isPauseTool(tc.Name) {
						shouldPause = true
						break
					}
				}
			}
			if shouldPause {
				return &RunResult{
					LastAgent:                  currentAgent,
					NewItems:                   allItems,
					RawResponses:               allResponses,
					ToolInputGuardrailResults:  allToolInputResults,
					ToolOutputGuardrailResults: allToolOutputResults,
					Usage:                      runCtx.Usage,
				}, nil
			}
		}
	}

}

// RunStreamed executes an agent with streaming, returning events via a channel.
func (r *Runner) RunStreamed(ctx context.Context, agent *Agent, input []RunItem, cfg RunConfig) *StreamedRunResult {
	events := make(chan StreamEvent, 64)
	done := make(chan *RunResult, 1)
	streamed := &StreamedRunResult{Events: events, done: done}
	if r == nil {
		streamed.setErr(fmt.Errorf("runner is nil"))
		close(events)
		done <- &RunResult{LastAgent: agent, Usage: Usage{}}
		close(done)
		return streamed
	}
	if agent == nil {
		streamed.setErr(fmt.Errorf("agent is nil"))
		close(events)
		done <- &RunResult{Usage: Usage{}}
		close(done)
		return streamed
	}

	go func() {
		defer close(done)
		var unsubscribe func()
		var contentForwardDone chan struct{}
		if es := streamedRunEventStream(r, cfg); es != nil {
			contentEvents, unsub := es.Subscribe(256)
			unsubscribe = unsub
			contentForwardDone = make(chan struct{})
			go func() {
				defer close(contentForwardDone)
				for ev := range contentEvents {
					if !emitContentStreamEvent(ctx, events, ev) {
						return
					}
				}
			}()
		}
		defer func() {
			if unsubscribe != nil {
				unsubscribe()
			}
			if contentForwardDone != nil {
				<-contentForwardDone
			}
			close(events)
		}()
		defer func() {
			if rec := recover(); rec != nil {
				streamed.setErr(fmt.Errorf("streamed run panicked: %v", rec))
				done <- &RunResult{LastAgent: agent, Usage: Usage{}}
			}
		}()
		result, err := r.run(ctx, agent, input, cfg, events)
		if err != nil {
			streamed.setErr(err)
			done <- &RunResult{LastAgent: agent, Usage: Usage{}}
			return
		}
		done <- result
	}()

	return streamed
}

func streamedRunEventStream(r *Runner, cfg RunConfig) *EventStream {
	if platformHooks, ok := cfg.Hooks.(*PlatformHooks); ok && platformHooks != nil && platformHooks.EventStream != nil {
		return platformHooks.EventStream
	}
	if r != nil {
		if platformHooks, ok := r.DefaultHooks.(*PlatformHooks); ok && platformHooks != nil && platformHooks.EventStream != nil {
			return platformHooks.EventStream
		}
	}
	return nil
}

func emitContentStreamEvent(ctx context.Context, events chan<- StreamEvent, ev ContentEvent) bool {
	content := ev
	streamEvent := StreamEvent{
		Type:    StreamEventContent,
		Content: &content,
		Name:    ev.Type,
	}
	if subagent, ok := SubAgentStreamEventFromContentEvent(ev); ok {
		streamEvent.Type = StreamEventSubAgent
		streamEvent.SubAgent = subagent
	}
	select {
	case events <- streamEvent:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *Runner) callModel(ctx context.Context, model Model, req ModelRequest, streamEvents chan<- StreamEvent) (*ModelResponse, error) {
	if streamEvents == nil {
		return model.GetResponse(ctx, req)
	}
	stream, err := model.StreamResponse(ctx, req)
	if err != nil {
		return nil, err
	}
	var streamedResp *ModelResponse
	outputCommitted := false
	for ev := range stream.Events {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		switch ev.Type {
		case ModelStreamDelta:
			outputCommitted = true
			select {
			case streamEvents <- StreamEvent{Type: StreamEventRawResponse, Name: "model.delta", Delta: ev.Delta}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case ModelStreamComplete:
			streamedResp = ev.Response
		case ModelStreamError:
			if ev.Error != nil {
				if outputCommitted {
					return nil, &streamOutputCommittedError{cause: ev.Error}
				}
				return nil, ev.Error
			}
			if outputCommitted {
				return nil, &streamOutputCommittedError{cause: fmt.Errorf("model stream failed")}
			}
			return nil, fmt.Errorf("model stream failed")
		}
	}
	finalResp := stream.Final()
	if finalResp != nil {
		streamedResp = finalResp
	}
	if streamedResp == nil {
		streamedResp = &ModelResponse{}
	}
	return streamedResp, nil
}

func emitRunItems(events chan<- StreamEvent, items []RunItem) {
	if events == nil {
		return
	}
	for i := range items {
		item := items[i]
		events <- StreamEvent{
			Type: StreamEventRunItem,
			Item: &item,
		}
	}
}

func collectSubAgentFinalJoinItems(ctx context.Context, tools []Tool) ([]RunItem, error) {
	var joined []RunItem
	for _, tool := range tools {
		provider, ok := tool.(subAgentFinalJoinProvider)
		if !ok {
			continue
		}
		items, err := provider.JoinSubAgentResults(ctx)
		if err != nil {
			return nil, err
		}
		joined = append(joined, items...)
	}
	return joined, nil
}

// pollSubAgentResultItems drains already-terminal managed sub-agent results
// without blocking on still-active tasks.
func pollSubAgentResultItems(tools []Tool) []RunItem {
	var items []RunItem
	for _, tool := range tools {
		poller, ok := tool.(subAgentResultPoller)
		if !ok {
			continue
		}
		items = append(items, poller.PollSubAgentResults()...)
	}
	return items
}

func hasPendingSubAgentFinalJoin(tools []Tool) bool {
	for _, tool := range tools {
		provider, ok := tool.(subAgentFinalJoinStateProvider)
		if !ok {
			continue
		}
		if provider.HasPendingSubAgentFinalJoin() {
			return true
		}
	}
	return false
}

// --- internal step types ---

type responseStep interface{ step() }

type finalOutputStep struct {
	output any
}

func (*finalOutputStep) step() {}

type handoffStep struct {
	handoff *Handoff
	target  *Agent
}

func (*handoffStep) step() {}

type toolCallStep struct {
	toolCalls []ToolCallData
}

func (*toolCallStep) step() {}

// finalOutputText renders a candidate final output as text for StopGate and
// FinalAnswerVerifier callbacks.
func finalOutputText(output any) string {
	switch v := output.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// classifyResponse determines what the model wants to do: final output, handoff, or tool calls.
func classifyResponse(items []RunItem, agent *Agent) responseStep {
	var toolCalls []ToolCallData
	var hasText bool
	var lastText string

	for _, item := range items {
		switch item.Type {
		case RunItemToolCall:
			if item.ToolCall != nil {
				for _, h := range agent.Handoffs {
					if h.ToolName == item.ToolCall.Name {
						return &handoffStep{handoff: h, target: h.Agent}
					}
				}
				toolCalls = append(toolCalls, *item.ToolCall)
			}
		case RunItemMessage:
			if item.Message != nil && item.Message.Text != "" {
				hasText = true
				lastText = item.Message.Text
			}
		}
	}

	if len(toolCalls) > 0 {
		return &toolCallStep{toolCalls: toolCalls}
	}

	var output any = lastText
	if hasText && agent.OutputType != nil {
		parsed, err := agent.OutputType.Validate(lastText)
		if err != nil {
			log.Printf("[runner] WARN: output schema validation failed for agent %q: %v", agent.Name, err)
		} else {
			output = parsed
		}
	}
	return &finalOutputStep{output: output}
}

// handoffCallInput returns the raw JSON arguments the model supplied for the
// handoff tool call matching toolName, or nil if none is present.
func handoffCallInput(items []RunItem, toolName string) json.RawMessage {
	for _, item := range items {
		if item.Type == RunItemToolCall && item.ToolCall != nil && item.ToolCall.Name == toolName {
			return item.ToolCall.Input
		}
	}
	return nil
}

func handoffToolOutputs(items []RunItem, handoff *Handoff, fromAgent, toAgent *Agent) []RunItem {
	if handoff == nil || toAgent == nil {
		return nil
	}
	var outputs []RunItem
	for _, item := range items {
		if item.Type != RunItemToolCall || item.ToolCall == nil || item.ToolCall.Name != handoff.ToolName {
			continue
		}
		outputs = append(outputs, RunItem{
			Type:  RunItemToolOutput,
			Agent: fromAgent,
			ToolOutput: &ToolOutputData{
				CallID:  item.ToolCall.ID,
				Content: fmt.Sprintf("Handing off to %s", toAgent.Name),
			},
		})
	}
	return outputs
}

// toolCallResult holds the output of a single parallel tool execution.
type toolCallResult struct {
	item             RunItem
	inputGuardrails  []ToolGuardrailResult
	outputGuardrails []ToolGuardrailResult
	guardrailErr     error
	shouldPause      bool // Propagated from ToolResult.ShouldPause (e.g. set_phase approval gate).
}

// executeTools runs all tool calls in parallel and returns the results as RunItems.
// Order is preserved: results[i] corresponds to calls[i].
// Matches the OpenAI Agents SDK's _FunctionToolBatchExecutor pattern.
func (r *Runner) executeTools(ctx context.Context, runCtx *RunContext, agent *Agent, calls []ToolCallData, cfg RunConfig) ([]RunItem, []ToolGuardrailResult, []ToolGuardrailResult, bool, error) {
	tools := prepareToolsForRun(agent.GetAllTools(runCtx), cfg, runCtx)
	toolMap := make(map[string]Tool, len(tools))
	for _, t := range tools {
		name := t.Name()
		if _, exists := toolMap[name]; exists {
			// Reject duplicate tool registrations (finding M4) instead of
			// silently last-write-wins, which can mask hostile shadowing.
			return nil, nil, nil, false, &AgentError{Message: fmt.Sprintf("duplicate tool registration: %q", name)}
		}
		toolMap[name] = t
	}

	// Pre-allocate order-preserving slots.
	slots := make([]toolCallResult, len(calls))

	// Partition: resolve synchronous results (unknown tool, needs approval) vs async execution.
	type asyncWork struct {
		idx  int
		call ToolCallData
		tool Tool
	}
	var work []asyncWork

	for i, call := range calls {
		t, ok := toolMap[call.Name]
		if !ok {
			slots[i] = toolCallResult{item: RunItem{
				Type: RunItemToolOutput, Agent: agent,
				ToolOutput: &ToolOutputData{CallID: call.ID, Content: fmt.Sprintf("unknown tool: %s", call.Name), IsError: true},
			}}
			continue
		}
		if t.NeedsApproval() {
			slots[i] = toolCallResult{item: RunItem{
				Type: RunItemToolApproval, Agent: agent,
				ToolApproval: &ToolApprovalData{ToolName: call.Name, Input: call.Input, CallID: call.ID},
			}}
			continue
		}
		work = append(work, asyncWork{idx: i, call: call, tool: t})
	}

	// Execute tools in parallel, with optional concurrency limit for sub-agent tools.
	maxSubAgents := cfg.MaxConcurrentSubAgents
	if len(work) > 1 {
		log.Printf("[runner] executing %d tool calls in parallel: %v", len(work), func() []string {
			names := make([]string, len(work))
			for i, w := range work {
				names[i] = w.call.Name
			}
			return names
		}())
	}

	var wg sync.WaitGroup
	// Build a semaphore for sub-agent concurrency if configured.
	var sem chan struct{}
	if maxSubAgents > 0 {
		sem = make(chan struct{}, maxSubAgents)
	}
	// Turn-scoped read/write lock (codex parallel-execution pattern):
	// parallel-safe tools (read-only and control-flow/sub-agent tools) share a
	// read lock and run concurrently; mutating tools (Edit, Write, Bash, …)
	// take the write lock and run exclusively. This prevents two concurrent
	// tool calls from editing the same file or racing a shell command against
	// an in-flight edit, while keeping read fan-out and parallel delegation fast.
	var mutationLock sync.RWMutex
	wg.Add(len(work))
	for _, w := range work {
		go func(w asyncWork) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					slots[w.idx] = toolCallResult{
						item: RunItem{
							Type:  RunItemToolOutput,
							Agent: agent,
							ToolOutput: &ToolOutputData{
								CallID:  w.call.ID,
								Content: fmt.Sprintf("tool %q panicked: %v", w.call.Name, rec),
								IsError: true,
							},
						},
					}
				}
			}()
			// Apply semaphore only for sub-agent tools (Agent.AsTool wrappers),
			// detected by Go type rather than name prefix (finding M2).
			isSubAgent := isSubAgentTool(w.tool)
			if sem != nil && isSubAgent {
				sem <- struct{}{}
				defer func() { <-sem }()
			}
			if isParallelSafeTool(w.tool) {
				mutationLock.RLock()
				defer mutationLock.RUnlock()
			} else {
				mutationLock.Lock()
				defer mutationLock.Unlock()
			}
			slots[w.idx] = r.executeSingleTool(ctx, runCtx, agent, w.tool, w.call, cfg)
		}(w)
	}
	wg.Wait()

	// Collect results in order.
	var results []RunItem
	var allInputGuardrails []ToolGuardrailResult
	var allOutputGuardrails []ToolGuardrailResult
	shouldPause := false

	for _, slot := range slots {
		results = append(results, slot.item)
		allInputGuardrails = append(allInputGuardrails, slot.inputGuardrails...)
		allOutputGuardrails = append(allOutputGuardrails, slot.outputGuardrails...)
		if slot.shouldPause {
			shouldPause = true
		}
		if slot.guardrailErr != nil {
			return results, allInputGuardrails, allOutputGuardrails, shouldPause, slot.guardrailErr
		}
	}

	return results, allInputGuardrails, allOutputGuardrails, shouldPause, nil
}

// executeSingleTool runs one tool call with guardrails and hooks. Thread-safe.
func (r *Runner) executeSingleTool(ctx context.Context, runCtx *RunContext, agent *Agent, t Tool, call ToolCallData, cfg RunConfig) toolCallResult {
	var res toolCallResult

	// Tool input guardrails.
	if len(cfg.ToolInputGuardrails) > 0 {
		inputResults, guardrailErr := runToolInputGuardrails(runCtx, agent, t, call.Input, cfg.ToolInputGuardrails)
		res.inputGuardrails = inputResults
		if guardrailErr != nil {
			if !isToolGuardrailTripwire(guardrailErr) {
				res.guardrailErr = guardrailErr
			}
			res.item = RunItem{Type: RunItemToolOutput, Agent: agent,
				ToolOutput: &ToolOutputData{CallID: call.ID, Content: guardrailErr.Error(), IsError: true}}
			return res
		}
	}

	// Create a per-tool-call RunContext with the call ID so hooks can record it.
	toolRunCtx := *runCtx
	toolRunCtx.ctx = WithToolCallID(runCtx.ctx, call.ID)

	fireRunHook(cfg.Hooks, func(h RunHooks) { h.OnToolStart(&toolRunCtx, agent, t, call) })
	fireAgentHook(agent.Hooks, func(h AgentHooks) { h.OnToolStart(&toolRunCtx, agent, t, call) })

	// Trace: function span wraps the tool execution.
	funcSpan := NewSpan("function", runCtx.SpanParentID, &FunctionSpanData{ToolName: call.Name, Input: string(call.Input)})
	if runCtx.TracingProcessor != nil {
		runCtx.TracingProcessor.OnSpanStart(funcSpan)
	}

	var result ToolResult
	var err error
	// Inject call ID and tracing context so nested agent runs can stitch their
	// spans under this tool invocation instead of starting a disconnected trace.
	execCtx := WithParentCallID(ctx, call.ID)
	execCtx = WithTraceContext(execCtx, runCtx.Trace, runCtx.TracingProcessor, funcSpan.ID)
	execCtx = WithNestedRunConfig(execCtx, cfg)
	timeout := t.TimeoutSeconds()
	if timeout > 0 {
		toolCtx, cancel := context.WithTimeout(execCtx, time.Duration(timeout)*time.Second)
		result, err = safeExecuteTool(t, toolCtx, call.Input, runCtx.WorkDir)
		cancel()
		if toolCtx.Err() == context.DeadlineExceeded {
			err = &ToolTimeoutError{ToolName: call.Name, Timeout: time.Duration(timeout) * time.Second}
		}
	} else {
		result, err = safeExecuteTool(t, execCtx, call.Input, runCtx.WorkDir)
	}

	if err != nil {
		result = ToolResult{Content: err.Error(), IsError: true}
	}

	// Tool output guardrails.
	var outputGuardrailErr error
	if len(cfg.ToolOutputGuardrails) > 0 {
		outputResults, guardrailErr := runToolOutputGuardrails(runCtx, agent, t, result, cfg.ToolOutputGuardrails)
		res.outputGuardrails = outputResults
		if guardrailErr != nil {
			outputGuardrailErr = guardrailErr
			if !isToolGuardrailTripwire(guardrailErr) {
				res.guardrailErr = guardrailErr
			}
			result = ToolResult{Content: guardrailErr.Error(), IsError: true}
		}
	}

	// Finish function span after output guardrails so blocked content is not
	// exported as the tool output in tracing.
	funcSpan.Data = &FunctionSpanData{ToolName: call.Name, Input: string(call.Input), Output: result.Content, IsError: result.IsError}
	funcSpan.Finish()
	if runCtx.TracingProcessor != nil {
		runCtx.TracingProcessor.OnSpanEnd(funcSpan)
	}
	if runCtx.Trace != nil {
		runCtx.Trace.AddSpan(funcSpan)
	}

	fireRunHook(cfg.Hooks, func(h RunHooks) { h.OnToolEnd(&toolRunCtx, agent, t, call, result) })
	fireAgentHook(agent.Hooks, func(h AgentHooks) { h.OnToolEnd(&toolRunCtx, agent, t, call, result) })

	// Cap the model-facing output. Hooks, traces, and the event stream above
	// received the raw content; only the conversation item is truncated so one
	// oversized tool result cannot flood the context window.
	content := result.Content
	if maxBytes := cfg.EffectiveMaxToolOutputBytes(); maxBytes > 0 && len(content) > maxBytes {
		content = TruncateMiddle(content, maxBytes)
	}

	res.item = RunItem{
		Type: RunItemToolOutput, Agent: agent,
		ToolOutput: &ToolOutputData{CallID: call.ID, Content: content, IsError: result.IsError},
	}
	if outputGuardrailErr != nil {
		return res
	}
	res.shouldPause = result.ShouldPause
	return res
}

func isToolGuardrailTripwire(err error) bool {
	var inputTripwire *ToolInputGuardrailTripwireTriggered
	if errors.As(err, &inputTripwire) {
		return true
	}
	var outputTripwire *ToolOutputGuardrailTripwireTriggered
	return errors.As(err, &outputTripwire)
}

// isParallelSafeTool reports whether a tool may run concurrently with other
// tool calls in the same turn. Read-only tools cannot mutate shared state, and
// control-flow/sub-agent tools manage their own isolation (sub-agent tasks are
// instructed to own disjoint files), so both classes share a read lock.
// Everything else (Edit, Write, Bash, custom mutating tools) runs exclusively.
func isParallelSafeTool(t Tool) bool {
	if t == nil {
		return true
	}
	return t.IsReadOnly() || isControlFlowToolInstance(t)
}

// isControlFlowTool returns true for tools that manage run lifecycle or user interaction.
// These are always allowed regardless of tool access level.
func isControlFlowTool(name string) bool {
	if _, ok := controlFlowToolNames[name]; ok {
		return true
	}
	// agent_* sub-agent invocation tools are dynamic (one per registered Agent)
	// and remain control-flow. Detected by exact prefix on the canonical
	// agent tool naming convention from agent_tool.go.
	if strings.HasPrefix(name, "agent_") {
		return true
	}
	return false
}

// isControlFlowToolInstance reports whether this concrete tool is a runner
// control-flow primitive. Name-only checks are intentionally not enough for
// agent_* tools: otherwise an arbitrary mutating FunctionTool named
// "agent_delete_everything" would bypass read-only filtering and approval.
func isControlFlowToolInstance(t Tool) bool {
	if t == nil {
		return false
	}
	if _, ok := controlFlowToolNames[t.Name()]; ok {
		return true
	}
	return isSubAgentTool(t)
}

// isPauseTool returns true for tools that require the runner to stop immediately
// so the agent loop can pause and await user input. Without this, the AI can
// call present_plan and finish in the same turn, bypassing the pause.
func isPauseTool(name string) bool {
	return name == "present_plan" || name == "AskUserQuestion"
}

// filterByAccess filters tools according to the ToolAccessLevel tier:
//   - "full" (or empty) → all tools
//   - "read-only" → read-only tools + control-flow tools
func filterByAccess(tools []Tool, level ToolAccessLevel, ctx *RunContext) []Tool {
	level = NormalizeToolAccessLevel(level)
	var filtered []Tool
	for _, t := range tools {
		if t == nil {
			continue
		}
		t = adaptToolForAccess(t, level)
		if t == nil || !t.IsEnabled(ctx) {
			continue
		}
		if level == ToolAccessLevelReadOnly && !t.IsReadOnly() && !isControlFlowToolInstance(t) {
			continue
		}
		filtered = append(filtered, t)
	}
	return filtered
}

func prepareToolsForRun(tools []Tool, cfg RunConfig, ctx *RunContext) []Tool {
	return applyToolPolicy(filterByAccess(tools, cfg.ToolAccessLevel, ctx), cfg.ToolPolicy)
}

func applyToolPolicy(tools []Tool, policy *ToolPolicy) []Tool {
	if policy == nil || (!policy.ApprovalRequired && policy.DefaultTimeout <= 0) {
		return tools
	}
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		approval := policy.ApprovalRequired && !t.IsReadOnly() && !isControlFlowToolInstance(t)
		out = append(out, WrapWithPolicy(t, approval, policy.DefaultTimeout))
	}
	return out
}

func adaptToolForAccess(t Tool, level ToolAccessLevel) Tool {
	if adapter, ok := t.(ToolAccessAdapter); ok {
		if adapted := adapter.ToolForAccess(level); adapted != nil {
			return adapted
		}
	}
	return t
}

func buildRunInstructions(base string, agent *Agent, cfg RunConfig) string {
	var parts []string
	if strings.TrimSpace(base) != "" {
		parts = append(parts, base)
	}
	if extra := strings.TrimSpace(cfg.AdditionalInstructions); extra != "" {
		parts = append(parts, extra)
	} else if legacy := strings.TrimSpace(cfg.ModeInstructions); legacy != "" {
		parts = append(parts, legacy)
	}
	if schemaContext := buildOutputSchemaPromptContext(agent.OutputType); schemaContext != "" {
		parts = append(parts, schemaContext)
	}
	if mcp := buildMCPPromptContext(agent.MCPServers); mcp != "" {
		parts = append(parts, mcp)
	}
	return strings.Join(parts, "\n\n---\n\n")
}

func buildOutputSchemaPromptContext(schema *OutputSchema) string {
	if schema == nil {
		return ""
	}
	name := strings.TrimSpace(schema.Name)
	if name == "" {
		name = "final_output"
	}
	var b strings.Builder
	b.WriteString("<structured_output>\n")
	b.WriteString("When producing a final answer, return JSON only.")
	b.WriteString("\nOutput schema name: ")
	b.WriteString(name)
	if schema.Strict {
		b.WriteString("\nStrict mode: do not include prose, markdown fences, or fields outside the schema.")
	}
	if len(schema.Schema) > 0 {
		b.WriteString("\nJSON schema:\n")
		b.WriteString(string(schema.Schema))
	}
	b.WriteString("\n</structured_output>")
	return b.String()
}

func shouldRetryWithPolicy(policy *RetryPolicy, attempt int) bool {
	if policy == nil || policy.MaxRetries <= 0 {
		return false
	}
	return attempt <= policy.MaxRetries
}

func retryPolicyBlockedByAdvice(advice *ModelRetryAdvice) bool {
	return advice != nil && !advice.ShouldRetry && strings.TrimSpace(advice.Reason) != ""
}

// resolveRequestModelName returns the model name to place in API requests
// after provider routing. Prefix-aware providers (e.g. MultiProvider) decide
// themselves whether the prefix was consumed for routing; for plain providers
// the prefix is stripped only when it names the provider itself, so model IDs
// that contain "/" as part of the ID (e.g. OpenRouter "vendor/model") pass
// through unchanged.
func resolveRequestModelName(provider ModelProvider, model Model, name string) string {
	if normalizer, ok := provider.(ModelNameNormalizer); ok {
		return normalizer.NormalizeModelName(name)
	}
	prefix, bare := ParseModelPrefix(name)
	if prefix == "" || bare == "" {
		return name
	}
	if model != nil && strings.EqualFold(strings.TrimSpace(prefix), strings.TrimSpace(model.Provider())) {
		return bare
	}
	return name
}

func isContextCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func shouldStopAtTools(agent *Agent, calls []ToolCallData) bool {
	if agent.StopAtTools == nil {
		return false
	}
	for _, call := range calls {
		if agent.StopAtTools.Contains(call.Name) {
			return true
		}
	}
	return false
}

func finalOutputFromToolResults(agent *Agent, results []RunItem) any {
	if agent.ToolsToFinalOutput == nil || !agent.ToolsToFinalOutput.IsFinalOutput {
		return nil
	}
	if len(agent.ToolsToFinalOutput.Output) > 0 {
		return agent.ToolsToFinalOutput.Output
	}
	for _, item := range results {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil {
			return item.ToolOutput.Content
		}
	}
	return nil
}

// safeExecuteTool invokes Tool.Execute with panic recovery. A panicking tool
// is converted to a normal error result so it cannot crash the host process.
func safeExecuteTool(t Tool, ctx context.Context, input json.RawMessage, workDir string) (result ToolResult, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("tool %q panicked: %v", t.Name(), rec)
		}
	}()
	return t.Execute(ctx, input, workDir)
}

// fireRunHook safely calls a RunHooks callback if hooks is non-nil.
// A panic in user-supplied hooks is recovered so it cannot crash the host.
func fireRunHook(hooks RunHooks, fn func(RunHooks)) {
	if hooks == nil {
		return
	}
	defer func() { _ = recover() }()
	fn(hooks)
}

func emitLLMAttemptEvent(hooks RunHooks, event ContentEvent) {
	platformHooks, ok := hooks.(*PlatformHooks)
	if !ok || platformHooks == nil || platformHooks.EventStream == nil {
		return
	}
	platformHooks.EventStream.EmitLLMAttempt(event)
}

func attemptFailureEvent(event ContentEvent, data *GenerationSpanData) ContentEvent {
	event.AttemptLatencyMs = data.LatencyMS
	event.FailureKind = data.FailureKind
	event.Reason = data.FailureKind
	event.FallbackPlanned = data.FallbackScheduled
	event.FallbackFromModel = data.FallbackFromModel
	event.FallbackToModel = data.FallbackToModel
	event.FallbackReason = data.FallbackReason
	if data.FailureKind != "" {
		event.Message = data.FailureKind
	}
	if data.Error != "" {
		event.IsError = true
		event.Output = TruncateBytes(data.Error, eventStreamMaxToolBytes)
	}
	return event
}

func modelCompactionThreshold(model Model, cfg CompactionConfig) int {
	compactor, ok := model.(ContextCompactor)
	if !ok || !compactor.SupportsContextCompaction() {
		return 0
	}
	cfg = cfg.normalized()
	if !cfg.Enabled {
		return 0
	}
	return cfg.TriggerTokens
}

func compactRunItemsWithModelAPI(ctx context.Context, model Model, req ModelRequest, requestOverheadTokens int, cfg CompactionConfig, force bool) (*CompactionResult, int, int, bool, error) {
	cfg = cfg.normalized()
	if !cfg.Enabled || len(req.Input) == 0 {
		return nil, 0, 0, false, nil
	}
	if requestOverheadTokens < 0 {
		requestOverheadTokens = 0
	}
	before := estimateRunItemsTokens(req.Input) + requestOverheadTokens
	if !force && before <= cfg.TriggerTokens {
		return nil, before, before, false, nil
	}
	compactor, ok := model.(ContextCompactor)
	if !ok || !compactor.SupportsContextCompaction() {
		return nil, before, before, false, nil
	}
	result, err := compactor.CompactContext(ctx, req)
	if err != nil {
		return nil, before, before, false, err
	}
	if result == nil || len(result.Items) == 0 {
		return nil, before, before, false, errors.New("provider returned empty compaction output")
	}
	after := estimateRunItemsTokens(result.Items) + requestOverheadTokens
	return result, before, after, true, nil
}

func providerCompactionSummaryFromItems(items []RunItem) (string, bool) {
	var compacted []RunItem
	for _, item := range items {
		if item.Type == RunItemCompaction && item.Compaction != nil && strings.TrimSpace(item.Compaction.EncryptedContent) != "" {
			compacted = append(compacted, item)
		}
	}
	if len(compacted) == 0 {
		return "", false
	}
	return summarizeProviderCompactionOutput("", compacted), true
}

func applyCompactionCarryForward(ctx context.Context, compacted, previous []RunItem, cfg RunConfig) ([]RunItem, string) {
	carryForward := buildCompactionCarryForward(ctx, cfg)
	out := make([]RunItem, 0, len(compacted)+1)
	for _, item := range compacted {
		if isCompactionCarryForwardItem(item) {
			continue
		}
		out = append(out, item)
	}
	if !hasLocalCompactionSummary(out) {
		out = appendMissingRecentRunItems(out, previous, cfg.CompactionConfig.normalized().PreserveRecentItems)
	}
	out = repairToolPairsForModelInput(out, previous)
	if carryForward == "" {
		return out, ""
	}
	out = append(out, RunItem{
		Type: RunItemMessage,
		Message: &MessageOutput{
			Text: carryForward,
		},
	})
	return out, carryForward
}

// guardCompactionCarryForward re-runs the agent's input guardrails on a
// carry-forward payload introduced after compaction. Mitigates finding H7:
// without this, malicious content (e.g. a tool-supplied working-state summary)
// could survive compaction and reach the model unchecked. A nil/empty
// carryForward is a no-op.
func guardCompactionCarryForward(runCtx *RunContext, agent *Agent, carryForward string) error {
	if carryForward == "" || len(agent.InputGuardrails) == 0 {
		return nil
	}
	probe := []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: carryForward}}}
	_, err := runInputGuardrails(runCtx, agent, probe)
	return err
}

func buildCompactionCarryForward(ctx context.Context, cfg RunConfig) string {
	var sections []string
	dynamicAdded := false
	if cfg.CompactionCarryForward != nil {
		if dynamic := strings.TrimSpace(cfg.CompactionCarryForward(ctx)); dynamic != "" {
			sections = append(sections, dynamic)
			dynamicAdded = true
		}
	}
	if !dynamicAdded {
		if cfg.WorkingStateContext != "" {
			sections = append(sections, cfg.WorkingStateContext)
		}
		if cfg.Phase != "" {
			sections = append(sections, "Current phase: "+cfg.Phase)
		}
	}
	sections = dedupeCompactionCarryForwardSections(sections)
	if len(sections) == 0 {
		return ""
	}
	return compactionCarryForwardPrefix + "\nThis live runtime state was injected after context compaction. Treat it as current and higher priority than older compacted history.\n\n" + strings.Join(sections, "\n\n")
}

func appendCompactionSummaryCarryForward(summary, carryForward string) string {
	summary = strings.TrimSpace(summary)
	carryForward = strings.TrimSpace(carryForward)
	if carryForward == "" {
		return summary
	}
	if summary == "" {
		return carryForward
	}
	return summary + "\n\n" + carryForward
}

func isCompactionCarryForwardItem(item RunItem) bool {
	return item.Type == RunItemMessage && item.Message != nil && strings.HasPrefix(strings.TrimSpace(item.Message.Text), compactionCarryForwardPrefix)
}

func hasLocalCompactionSummary(items []RunItem) bool {
	for _, item := range items {
		if item.Type == RunItemMessage && item.Message != nil && item.Agent != nil && item.Agent.Name == "context-summary" && strings.HasPrefix(strings.TrimSpace(item.Message.Text), "[COMPACTED HISTORY SUMMARY]") {
			return true
		}
	}
	return false
}

func appendMissingRecentRunItems(items, previous []RunItem, limit int) []RunItem {
	if limit <= 0 || len(previous) == 0 {
		return items
	}
	start := maxInt(0, len(previous)-limit)
	minPairIdx := 0
	if latest := latestProviderCompactionIndex(previous); latest >= 0 {
		start = maxInt(start, latest+1)
		minPairIdx = latest + 1
	}
	protected := make(map[int]struct{}, len(previous)-start)
	for idx := start; idx < len(previous); idx++ {
		protected[idx] = struct{}{}
	}
	ensureToolPairIntegrityAfter(previous, protected, minPairIdx)

	out := append([]RunItem(nil), items...)
	seen := make(map[string]struct{}, len(out))
	for _, item := range out {
		if key := runItemDedupeKey(item); key != "" {
			seen[key] = struct{}{}
		}
	}
	for idx, item := range previous {
		if _, ok := protected[idx]; !ok {
			continue
		}
		if isCompactionCarryForwardItem(item) {
			continue
		}
		key := runItemDedupeKey(item)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func repairToolPairsForModelInput(items, reference []RunItem) []RunItem {
	if len(items) == 0 {
		return items
	}
	refCalls := map[string]RunItem{}
	refOutputs := map[string]RunItem{}
	indexToolPairReferences(refCalls, refOutputs, reference)
	indexToolPairReferences(refCalls, refOutputs, items)

	currentCalls := map[string]struct{}{}
	currentOutputs := map[string]struct{}{}
	for _, item := range items {
		switch item.Type {
		case RunItemToolCall:
			if item.ToolCall != nil && item.ToolCall.ID != "" {
				currentCalls[item.ToolCall.ID] = struct{}{}
			}
		case RunItemToolOutput:
			if item.ToolOutput != nil && item.ToolOutput.CallID != "" {
				currentOutputs[item.ToolOutput.CallID] = struct{}{}
			}
		}
	}

	out := make([]RunItem, 0, len(items))
	emittedCalls := map[string]struct{}{}
	emittedOutputs := map[string]struct{}{}
	for _, item := range items {
		switch item.Type {
		case RunItemToolCall:
			if item.ToolCall == nil || item.ToolCall.ID == "" {
				continue
			}
			id := item.ToolCall.ID
			if _, already := emittedCalls[id]; already {
				continue
			}
			if _, paired := currentOutputs[id]; !paired {
				if _, ok := refOutputs[id]; !ok {
					continue
				}
			}
			out = append(out, item)
			emittedCalls[id] = struct{}{}
			if _, paired := currentOutputs[id]; !paired {
				if ref, ok := refOutputs[id]; ok {
					out = append(out, cloneRunItem(ref))
					emittedOutputs[id] = struct{}{}
				}
			}
		case RunItemToolOutput:
			if item.ToolOutput == nil || item.ToolOutput.CallID == "" {
				continue
			}
			id := item.ToolOutput.CallID
			if _, already := emittedOutputs[id]; already {
				continue
			}
			if _, callAlreadyEmitted := emittedCalls[id]; !callAlreadyEmitted {
				if ref, ok := refCalls[id]; ok {
					out = append(out, cloneRunItem(ref))
					emittedCalls[id] = struct{}{}
				} else if _, paired := currentCalls[id]; !paired {
					continue
				}
			}
			out = append(out, item)
			emittedOutputs[id] = struct{}{}
		default:
			out = append(out, item)
		}
	}
	return out
}

func indexToolPairReferences(calls, outputs map[string]RunItem, items []RunItem) {
	for _, item := range items {
		switch item.Type {
		case RunItemToolCall:
			if item.ToolCall != nil && item.ToolCall.ID != "" {
				calls[item.ToolCall.ID] = item
			}
		case RunItemToolOutput:
			if item.ToolOutput != nil && item.ToolOutput.CallID != "" {
				outputs[item.ToolOutput.CallID] = item
			}
		}
	}
}

func latestProviderCompactionIndex(items []RunItem) int {
	latest := -1
	for i, item := range items {
		if item.Type == RunItemCompaction && item.Compaction != nil && strings.TrimSpace(item.Compaction.EncryptedContent) != "" {
			latest = i
		}
	}
	return latest
}

func runItemDedupeKey(item RunItem) string {
	switch item.Type {
	case RunItemMessage:
		if item.Message == nil {
			return ""
		}
		role := "user"
		if item.Agent != nil {
			role = "assistant:" + item.Agent.Name
		}
		return fmt.Sprintf("message:%s:%s", role, item.Message.Text)
	case RunItemToolCall:
		if item.ToolCall == nil {
			return ""
		}
		return fmt.Sprintf("tool_call:%s:%s:%s", item.ToolCall.ID, item.ToolCall.Name, string(item.ToolCall.Input))
	case RunItemToolOutput:
		if item.ToolOutput == nil {
			return ""
		}
		return fmt.Sprintf("tool_output:%s:%t:%s", item.ToolOutput.CallID, item.ToolOutput.IsError, item.ToolOutput.Content)
	case RunItemHandoffCall:
		if item.HandoffCall == nil {
			return ""
		}
		return fmt.Sprintf("handoff_call:%s:%s", item.HandoffCall.FromAgent, item.HandoffCall.ToAgent)
	case RunItemHandoffOutput:
		if item.HandoffOutput == nil {
			return ""
		}
		return fmt.Sprintf("handoff_output:%s:%s", item.HandoffOutput.FromAgent, item.HandoffOutput.ToAgent)
	case RunItemToolApproval:
		if item.ToolApproval == nil {
			return ""
		}
		return fmt.Sprintf("tool_approval:%s:%s:%t:%s", item.ToolApproval.CallID, item.ToolApproval.ToolName, item.ToolApproval.Approved, string(item.ToolApproval.Input))
	case RunItemCompaction:
		if item.Compaction == nil {
			return ""
		}
		return fmt.Sprintf("compaction:%s:%s", item.Compaction.ID, item.Compaction.EncryptedContent)
	default:
		return ""
	}
}

func dedupeCompactionCarryForwardSections(sections []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(sections))
	for _, section := range sections {
		section = strings.TrimSpace(section)
		if section == "" {
			continue
		}
		if _, ok := seen[section]; ok {
			continue
		}
		seen[section] = struct{}{}
		out = append(out, section)
	}
	return out
}

func pruneBeforeLatestProviderCompaction(items []RunItem) ([]RunItem, bool) {
	latest := -1
	for i, item := range items {
		if item.Type == RunItemCompaction && item.Compaction != nil && strings.TrimSpace(item.Compaction.EncryptedContent) != "" {
			latest = i
		}
	}
	if latest < 0 {
		return items, false
	}
	if latest == 0 {
		return items, true
	}
	out := append([]RunItem(nil), items[latest:]...)
	return out, true
}

func generationFailureKind(err error, advice *ModelRetryAdvice) string {
	if advice != nil {
		if reason := strings.TrimSpace(advice.Reason); reason != "" {
			return reason
		}
	}
	if isContextLengthExceededError(err) {
		return "context_length_exceeded"
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	}
	var modelBehaviorErr *ModelBehaviorError
	if errors.As(err, &modelBehaviorErr) {
		return "model_behavior"
	}
	var agentErr *AgentError
	if errors.As(err, &agentErr) && agentErr.Cause != nil {
		return generationFailureKind(agentErr.Cause, nil)
	}
	return "error"
}

func isContextLengthExceededError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context_length_exceeded") ||
		strings.Contains(msg, "exceeds the context window")
}

// fireAgentHook safely calls an AgentHooks callback if hooks is non-nil.
// A panic in user-supplied hooks is recovered so it cannot crash the host.
func fireAgentHook(hooks AgentHooks, fn func(AgentHooks)) {
	if hooks == nil {
		return
	}
	defer func() { _ = recover() }()
	fn(hooks)
}

// runInputGuardrails runs all input guardrails and returns results.
func runInputGuardrails(ctx *RunContext, agent *Agent, input []RunItem) ([]InputGuardrailResult, error) {
	results := make([]InputGuardrailResult, 0, len(agent.InputGuardrails))
	for _, g := range agent.InputGuardrails {
		gr, err := callInputGuardrail(g, ctx, agent, input)
		if err != nil {
			return nil, &AgentError{Message: fmt.Sprintf("input guardrail %q failed", g.Name), Cause: err}
		}
		if gr == nil {
			gr = &GuardrailResult{}
		}
		results = append(results, InputGuardrailResult{
			GuardrailName:     g.Name,
			Output:            gr.Output,
			TripwireTriggered: gr.TripwireTriggered,
		})
		if gr.TripwireTriggered {
			return results, &InputGuardrailTripwireTriggered{GuardrailName: g.Name, Result: *gr}
		}
	}
	return results, nil
}

// callInputGuardrail invokes a guardrail Fn with panic recovery so a buggy
// host guardrail cannot bring the runner down — the panic is converted into
// an error and the run aborts cleanly.
func callInputGuardrail(g InputGuardrail, ctx *RunContext, agent *Agent, input []RunItem) (gr *GuardrailResult, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	return g.Fn(ctx, agent, input)
}

// runOutputGuardrails runs all output guardrails and returns results.
func runOutputGuardrails(ctx *RunContext, agent *Agent, output any) ([]OutputGuardrailResult, error) {
	results := make([]OutputGuardrailResult, 0, len(agent.OutputGuardrails))
	for _, g := range agent.OutputGuardrails {
		gr, err := callOutputGuardrail(g, ctx, agent, output)
		if err != nil {
			return nil, &AgentError{Message: fmt.Sprintf("output guardrail %q failed", g.Name), Cause: err}
		}
		if gr == nil {
			gr = &GuardrailResult{}
		}
		results = append(results, OutputGuardrailResult{
			GuardrailName:     g.Name,
			Output:            gr.Output,
			TripwireTriggered: gr.TripwireTriggered,
		})
		if gr.TripwireTriggered {
			return results, &OutputGuardrailTripwireTriggered{GuardrailName: g.Name, Result: *gr}
		}
	}
	return results, nil
}

func callOutputGuardrail(g OutputGuardrail, ctx *RunContext, agent *Agent, output any) (gr *GuardrailResult, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	return g.Fn(ctx, agent, output)
}

// runToolInputGuardrails runs all tool input guardrails for a single tool call.
func runToolInputGuardrails(ctx *RunContext, agent *Agent, tool Tool, input json.RawMessage, guardrails []ToolInputGuardrail) ([]ToolGuardrailResult, error) {
	results := make([]ToolGuardrailResult, 0, len(guardrails))
	for _, g := range guardrails {
		gr, err := callToolInputGuardrail(g, ctx, agent, tool, input)
		if err != nil {
			return nil, &AgentError{Message: fmt.Sprintf("tool input guardrail %q failed for tool %q", g.Name, tool.Name()), Cause: err}
		}
		if gr == nil {
			gr = &GuardrailResult{}
		}
		results = append(results, ToolGuardrailResult{
			GuardrailName:     g.Name,
			ToolName:          tool.Name(),
			Output:            gr.Output,
			TripwireTriggered: gr.TripwireTriggered,
		})
		if gr.TripwireTriggered {
			return results, &ToolInputGuardrailTripwireTriggered{GuardrailName: g.Name, ToolName: tool.Name(), Result: *gr}
		}
	}
	return results, nil
}

func callToolInputGuardrail(g ToolInputGuardrail, ctx *RunContext, agent *Agent, tool Tool, input json.RawMessage) (gr *GuardrailResult, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	return g.Fn(ctx, agent, tool, input)
}

// runToolOutputGuardrails runs all tool output guardrails for a single tool result.
func runToolOutputGuardrails(ctx *RunContext, agent *Agent, tool Tool, result ToolResult, guardrails []ToolOutputGuardrail) ([]ToolGuardrailResult, error) {
	results := make([]ToolGuardrailResult, 0, len(guardrails))
	for _, g := range guardrails {
		gr, err := callToolOutputGuardrail(g, ctx, agent, tool, result)
		if err != nil {
			return nil, &AgentError{Message: fmt.Sprintf("tool output guardrail %q failed for tool %q", g.Name, tool.Name()), Cause: err}
		}
		if gr == nil {
			gr = &GuardrailResult{}
		}
		results = append(results, ToolGuardrailResult{
			GuardrailName:     g.Name,
			ToolName:          tool.Name(),
			Output:            gr.Output,
			TripwireTriggered: gr.TripwireTriggered,
		})
		if gr.TripwireTriggered {
			return results, &ToolOutputGuardrailTripwireTriggered{GuardrailName: g.Name, ToolName: tool.Name(), Result: *gr}
		}
	}
	return results, nil
}

func callToolOutputGuardrail(g ToolOutputGuardrail, ctx *RunContext, agent *Agent, tool Tool, result ToolResult) (gr *GuardrailResult, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	return g.Fn(ctx, agent, tool, result)
}
