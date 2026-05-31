package guardrails

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// ToolGuardrailsFromRules compiles SDK-native guardrail rules into executable
// input and output guardrails. Invalid regexes are returned as errors and skipped.
func ToolGuardrailsFromRules(rules []agentsdk.GuardrailRule) ([]agentsdk.ToolInputGuardrail, []agentsdk.ToolOutputGuardrail, []error) {
	var inputGuardrails []agentsdk.ToolInputGuardrail
	var outputGuardrails []agentsdk.ToolOutputGuardrail
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
			inputGuardrails = append(inputGuardrails, agentsdk.ToolInputGuardrail{
				Name: fmt.Sprintf("crd:%s", rule.Name),
				Fn:   MakeToolInputGuardrailFn(rule, re),
			})
		case "tool-output":
			outputGuardrails = append(outputGuardrails, agentsdk.ToolOutputGuardrail{
				Name: fmt.Sprintf("crd:%s", rule.Name),
				Fn:   MakeToolOutputGuardrailFn(rule, re),
			})
		}
	}

	return inputGuardrails, outputGuardrails, errs
}

func MakeToolInputGuardrailFn(rule agentsdk.GuardrailRule, re *regexp.Regexp) func(*agentsdk.RunContext, *agentsdk.Agent, agentsdk.Tool, json.RawMessage) (*agentsdk.GuardrailResult, error) {
	return func(_ *agentsdk.RunContext, _ *agentsdk.Agent, tool agentsdk.Tool, input json.RawMessage) (*agentsdk.GuardrailResult, error) {
		if rule.ToolPattern != "" && !MatchToolPattern(tool.Name(), rule.ToolPattern) {
			return &agentsdk.GuardrailResult{}, nil
		}

		if re.Match(input) {
			msg := rule.Message
			if msg == "" {
				msg = fmt.Sprintf("Guardrail %q triggered on tool %q", rule.Name, tool.Name())
			}
			switch rule.Action {
			case "block":
				return &agentsdk.GuardrailResult{Output: msg, TripwireTriggered: true}, nil
			case "warn":
				log.Printf("WARN: guardrail %q triggered: %s", rule.Name, msg)
				return &agentsdk.GuardrailResult{Output: msg}, nil
			case "log":
				log.Printf("INFO: guardrail %q triggered: %s", rule.Name, msg)
				return &agentsdk.GuardrailResult{}, nil
			}
		}
		return &agentsdk.GuardrailResult{}, nil
	}
}

func MakeToolOutputGuardrailFn(rule agentsdk.GuardrailRule, re *regexp.Regexp) func(*agentsdk.RunContext, *agentsdk.Agent, agentsdk.Tool, agentsdk.ToolResult) (*agentsdk.GuardrailResult, error) {
	return func(_ *agentsdk.RunContext, _ *agentsdk.Agent, tool agentsdk.Tool, result agentsdk.ToolResult) (*agentsdk.GuardrailResult, error) {
		if rule.ToolPattern != "" && !MatchToolPattern(tool.Name(), rule.ToolPattern) {
			return &agentsdk.GuardrailResult{}, nil
		}

		if re.MatchString(result.Content) {
			msg := rule.Message
			if msg == "" {
				msg = fmt.Sprintf("Guardrail %q triggered on tool %q output", rule.Name, tool.Name())
			}
			switch rule.Action {
			case "block":
				return &agentsdk.GuardrailResult{Output: msg, TripwireTriggered: true}, nil
			case "warn":
				log.Printf("WARN: guardrail %q triggered: %s", rule.Name, msg)
				return &agentsdk.GuardrailResult{Output: msg}, nil
			case "log":
				log.Printf("INFO: guardrail %q triggered: %s", rule.Name, msg)
				return &agentsdk.GuardrailResult{}, nil
			}
		}
		return &agentsdk.GuardrailResult{}, nil
	}
}

// MatchToolPattern matches a tool name against a glob-like pattern.
func MatchToolPattern(name, pattern string) bool {
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
