package openai

import "testing"

func TestValidateChatCompletionsModel(t *testing.T) {
	t.Run("accepts chat model", func(t *testing.T) {
		if err := ValidateChatCompletionsModel("gpt-4.1"); err != nil {
			t.Fatalf("ValidateChatCompletionsModel returned error for chat model: %v", err)
		}
	})

	t.Run("rejects codex model", func(t *testing.T) {
		err := ValidateChatCompletionsModel("gpt-5.2-codex")
		if err == nil {
			t.Fatal("ValidateChatCompletionsModel returned nil for codex model")
		}
	})
}
