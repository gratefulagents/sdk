package agent

import (
	"context"
	"testing"
)

// stubModel is a minimal Model implementation for testing.
type stubModel struct {
	provider string
	model    string
}

func (s *stubModel) GetResponse(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	return &ModelResponse{}, nil
}
func (s *stubModel) StreamResponse(ctx context.Context, req ModelRequest) (*ModelStream, error) {
	return nil, nil
}
func (s *stubModel) GetRetryAdvice(err error) *ModelRetryAdvice {
	return &ModelRetryAdvice{ShouldRetry: false}
}
func (s *stubModel) CalculateCost(usage Usage) float64 { return 0 }
func (s *stubModel) Provider() string                  { return s.provider }

// stubProvider returns a stubModel with a specific provider name.
type stubProvider struct {
	name string
}

func (sp *stubProvider) GetModel(name string) (Model, error) {
	return &stubModel{provider: sp.name, model: name}, nil
}
func (sp *stubProvider) Close() error { return nil }

func TestParseModelPrefix(t *testing.T) {
	tests := []struct {
		input      string
		wantPrefix string
		wantModel  string
	}{
		{"anthropic/claude-sonnet-4-6", "anthropic", "claude-sonnet-4-6"},
		{"openai/gpt-4.1", "openai", "gpt-4.1"},
		{"gpt-4.1", "", "gpt-4.1"},
		{"claude-sonnet-4-6", "", "claude-sonnet-4-6"},
		{"  openai/gpt-5  ", "openai", "gpt-5"},
		{"gemini/gemini-2.0-flash", "gemini", "gemini-2.0-flash"},
		{"", "", ""},
	}
	for _, tt := range tests {
		prefix, model := ParseModelPrefix(tt.input)
		if prefix != tt.wantPrefix || model != tt.wantModel {
			t.Errorf("ParseModelPrefix(%q) = (%q, %q), want (%q, %q)",
				tt.input, prefix, model, tt.wantPrefix, tt.wantModel)
		}
	}
}

func TestMultiProvider_GetModel_WithPrefix(t *testing.T) {
	mp := NewMultiProvider("openai")
	mp.Register("openai", &stubProvider{name: "openai"})
	mp.Register("anthropic", &stubProvider{name: "anthropic"})

	m, err := mp.GetModel("anthropic/claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Provider() != "anthropic" {
		t.Errorf("got provider %q, want %q", m.Provider(), "anthropic")
	}
}

func TestMultiProvider_GetModel_NoPrefix_UsesDefault(t *testing.T) {
	mp := NewMultiProvider("openai")
	mp.Register("openai", &stubProvider{name: "openai"})
	mp.Register("anthropic", &stubProvider{name: "anthropic"})

	m, err := mp.GetModel("gpt-4.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Provider() != "openai" {
		t.Errorf("got provider %q, want %q", m.Provider(), "openai")
	}
}

func TestMultiProvider_GetModel_UnknownPrefix(t *testing.T) {
	mp := NewMultiProvider("openai")
	mp.Register("openai", &stubProvider{name: "openai"})

	_, err := mp.GetModel("unknown/model-x")
	if err == nil {
		t.Fatal("expected error for unknown prefix")
	}
}

func TestMultiProvider_Close(t *testing.T) {
	mp := NewMultiProvider("openai")
	mp.Register("openai", &stubProvider{name: "openai"})
	mp.Register("anthropic", &stubProvider{name: "anthropic"})

	if err := mp.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewRunnerWithProvider(t *testing.T) {
	mp := NewMultiProvider("openai")
	mp.Register("openai", &stubProvider{name: "openai"})

	runner := NewRunnerWithProvider(mp)
	if runner == nil {
		t.Fatal("NewRunnerWithProvider returned nil")
	}
	if runner.provider == nil {
		t.Error("runner.provider should not be nil")
	}
}
