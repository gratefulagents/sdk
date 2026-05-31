package openai

import (
	"fmt"
	"strings"
)

const (
	// DefaultChatModel/DefaultChatMiniModel are the default OpenAI-compatible
	// runtime models used when callers omit an explicit model selection.
	// These defaults should be Responses-compatible.
	DefaultChatModel     = "gpt-5.5"
	DefaultChatMiniModel = "gpt-5.3-codex-spark"

	chatCompletionsExampleModel     = "gpt-4.1"
	chatCompletionsExampleMiniModel = "gpt-4.1-mini"
)

// SupportsChatCompletions reports whether the configured model is compatible
// with the Chat Completions endpoint.
func SupportsChatCompletions(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	if trimmed == "" {
		return true
	}

	// Codex-family models are not chat-completions models. Sending them to
	// /v1/chat/completions produces the 404 seen in failed plan/task workers.
	if strings.Contains(trimmed, "codex") {
		return false
	}

	return true
}

// ValidateChatCompletionsModel returns an actionable error for incompatible
// models when a caller needs Chat-Completions-only behavior.
func ValidateChatCompletionsModel(model string) error {
	if SupportsChatCompletions(model) {
		return nil
	}

	return fmt.Errorf("model %q is not compatible with the OpenAI chat completions API used by gratefulagents; use a chat-completions model such as %s or %s", model, chatCompletionsExampleModel, chatCompletionsExampleMiniModel)
}
