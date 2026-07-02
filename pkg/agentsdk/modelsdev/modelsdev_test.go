package modelsdev

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const sampleCatalog = `{
  "github-copilot": {
    "models": {
      "claude-fable-5": {"limit": {"context": 1000000, "output": 128000}},
      "gpt-5.3-codex": {"limit": {"context": 400000, "output": 128000}}
    }
  },
  "openai": {
    "models": {
      "gpt-5.3-codex-spark": {"limit": {"context": 128000, "output": 32000}},
      "gpt-5.4": {"limit": {"context": 1050000, "output": 128000}}
    }
  },
  "anthropic": {
    "models": {
      "claude-fable-5": {"limit": {"context": 1000000, "output": 128000}},
      "claude-fable-5-20260601": {"limit": {"context": 1000000, "output": 128000}}
    }
  }
}`

func testResolver(t *testing.T) (*Resolver, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(sampleCatalog))
	}))
	t.Cleanup(server.Close)
	r := NewResolver(
		WithURL(server.URL),
		WithCachePath(filepath.Join(t.TempDir(), "catalog.json")),
	)
	return r, &hits
}

func TestLookupProviderAliasesAndPrefixes(t *testing.T) {
	r, _ := testResolver(t)
	ctx := context.Background()

	// copilot alias -> github-copilot; prefixed model ID stripped.
	limits, ok := r.Lookup(ctx, "copilot", "copilot/claude-fable-5")
	if !ok || limits.ContextTokens != 1000000 {
		t.Fatalf("copilot fable = %+v ok=%v, want 1M", limits, ok)
	}
	// Same model, different provider, different window.
	limits, ok = r.Lookup(ctx, "openai-oauth", "gpt-5.3-codex-spark")
	if !ok || limits.ContextTokens != 128000 {
		t.Fatalf("openai spark = %+v ok=%v, want 128K", limits, ok)
	}
	// Dated snapshot resolves via suffix tolerance.
	limits, ok = r.Lookup(ctx, "anthropic-oauth", "claude-fable-5-20270101")
	if !ok || limits.ContextTokens != 1000000 {
		t.Fatalf("dated fable = %+v ok=%v, want 1M", limits, ok)
	}
	if _, ok := r.Lookup(ctx, "copilot", "totally-unknown-model"); ok {
		t.Fatal("unknown model must not resolve")
	}
}

func TestCompactionThresholdsDerivation(t *testing.T) {
	r, _ := testResolver(t)
	trigger, target, ok := r.CompactionThresholds(context.Background(), "copilot", "claude-fable-5")
	if !ok || trigger != 900000 || target != 500000 {
		t.Fatalf("fable thresholds = %d/%d ok=%v, want 900000/500000", trigger, target, ok)
	}
}

func TestCompactionResolverFuncUsesModelPrefixAsProvider(t *testing.T) {
	r, _ := testResolver(t)
	resolve := r.CompactionResolverFunc("copilot")

	// Unprefixed -> default provider (copilot -> github-copilot).
	trigger, _, ok := resolve(context.Background(), "gpt-5.3-codex")
	if !ok || trigger != 360000 {
		t.Fatalf("default provider codex trigger = %d ok=%v, want 360000", trigger, ok)
	}
	// Prefix overrides default provider: openai gpt-5.4 is 1.05M.
	trigger, _, ok = resolve(context.Background(), "openai-oauth/gpt-5.4")
	if !ok || trigger != 945000 {
		t.Fatalf("openai-oauth gpt-5.4 trigger = %d ok=%v, want 945000", trigger, ok)
	}
}

func TestCatalogCachedInMemoryAndOnDisk(t *testing.T) {
	r, hits := testResolver(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, ok := r.Lookup(ctx, "copilot", "claude-fable-5"); !ok {
			t.Fatal("lookup failed")
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected 1 catalog fetch, got %d", got)
	}

	// A fresh resolver pointed at a dead server must serve from disk cache.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)
	r2 := NewResolver(WithURL(dead.URL), WithCachePath(r.cachePath), WithTTL(time.Nanosecond))
	if limits, ok := r2.Lookup(ctx, "copilot", "claude-fable-5"); !ok || limits.ContextTokens != 1000000 {
		t.Fatalf("disk-cache fallback = %+v ok=%v", limits, ok)
	}
}

func TestParseCatalogRejectsEmpty(t *testing.T) {
	if _, err := ParseCatalog([]byte(`{}`)); err == nil {
		t.Fatal("empty catalog must error")
	}
	if _, err := ParseCatalog([]byte(`not json`)); err == nil {
		t.Fatal("bad json must error")
	}
}
