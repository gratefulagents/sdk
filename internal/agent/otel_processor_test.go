package agent

import (
	"testing"
)

func TestNormalizeModelIdentity(t *testing.T) {
	id := NormalizeModelIdentity(" openai/gpt-5.4 ", "")
	if id.Provider != "openai" || id.Canonical != "openai/gpt-5.4" || id.Raw != "openai/gpt-5.4" {
		t.Fatalf("unexpected identity: %+v", id)
	}
}
