package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeAuthFile(t *testing.T, dir string, auth AnthropicAuth) string {
	t.Helper()
	raw, err := MarshalAnthropicAuthJSON(auth)
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return path
}

func TestAnthropicFileTokenSource_ReturnsFileToken(t *testing.T) {
	path := writeAuthFile(t, t.TempDir(), AnthropicAuth{
		AccessToken:  "tok-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    time.Now().Add(4 * time.Hour),
	})
	src := NewAnthropicFileTokenSource(path, RefreshConfig{})
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if got != "tok-1" {
		t.Fatalf("Token() = %q, want tok-1", got)
	}
}

func TestAnthropicFileTokenSource_PicksUpExternalRotation(t *testing.T) {
	dir := t.TempDir()
	path := writeAuthFile(t, dir, AnthropicAuth{
		AccessToken:  "tok-old",
		RefreshToken: "refresh-1",
		ExpiresAt:    time.Now().Add(4 * time.Hour),
	})
	src := NewAnthropicFileTokenSource(path, RefreshConfig{})
	if got, _ := src.Token(context.Background()); got != "tok-old" {
		t.Fatalf("first Token() = %q, want tok-old", got)
	}

	// Simulate the central refresher rotating the mounted secret.
	raw, _ := MarshalAnthropicAuthJSON(AnthropicAuth{
		AccessToken:  "tok-new",
		RefreshToken: "refresh-2",
		ExpiresAt:    time.Now().Add(8 * time.Hour),
	})
	// Ensure the mtime signature changes even on coarse filesystems.
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("rotate auth file: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)

	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() after rotation error = %v", err)
	}
	if got != "tok-new" {
		t.Fatalf("Token() after rotation = %q, want tok-new", got)
	}
}

func TestAnthropicFileTokenSource_SelfRefreshNearExpiry(t *testing.T) {
	var refreshCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "tok-refreshed",
			"refresh_token": "refresh-2",
			"expires_in":    28800,
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := writeAuthFile(t, dir, AnthropicAuth{
		AccessToken:  "tok-stale",
		RefreshToken: "refresh-1",
		ExpiresAt:    time.Now().Add(30 * time.Second), // inside the 2-minute lead
	})
	src := NewAnthropicFileTokenSource(path, RefreshConfig{AnthropicTokenURL: srv.URL})

	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if got != "tok-refreshed" {
		t.Fatalf("Token() = %q, want tok-refreshed", got)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}

	// Refreshed material must be written back for restarts/siblings.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back auth file: %v", err)
	}
	auth, err := ParseAnthropicAuthJSON(raw)
	if err != nil {
		t.Fatalf("parse written-back auth: %v", err)
	}
	if auth.AccessToken != "tok-refreshed" || auth.RefreshToken != "refresh-2" {
		t.Fatalf("written-back auth = %+v, want refreshed tokens", auth)
	}

	// Cached: a second call must not refresh again.
	if got, _ := src.Token(context.Background()); got != "tok-refreshed" {
		t.Fatalf("second Token() = %q, want tok-refreshed", got)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls after second Token = %d, want 1", refreshCalls)
	}
}

func TestAnthropicFileTokenSource_RefreshFailureGrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	path := writeAuthFile(t, t.TempDir(), AnthropicAuth{
		AccessToken:  "tok-still-valid",
		RefreshToken: "refresh-1",
		ExpiresAt:    time.Now().Add(90 * time.Second), // near expiry but not expired
	})
	src := NewAnthropicFileTokenSource(path, RefreshConfig{AnthropicTokenURL: srv.URL})

	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v, want grace success", err)
	}
	if got != "tok-still-valid" {
		t.Fatalf("Token() = %q, want tok-still-valid", got)
	}
}

func TestAnthropicFileTokenSource_RefreshFailureExpiredFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"refresh_token_expired"}`))
	}))
	defer srv.Close()

	path := writeAuthFile(t, t.TempDir(), AnthropicAuth{
		AccessToken:  "tok-expired",
		RefreshToken: "refresh-1",
		ExpiresAt:    time.Now().Add(-time.Minute),
	})
	src := NewAnthropicFileTokenSource(path, RefreshConfig{AnthropicTokenURL: srv.URL})

	if _, err := src.Token(context.Background()); err == nil {
		t.Fatalf("Token() error = nil, want refresh failure for expired token")
	}
}

func TestAnthropicFileTokenSource_InvalidateForcesRefresh(t *testing.T) {
	var refreshCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "tok-after-401",
			"refresh_token": "refresh-2",
			"expires_in":    28800,
		})
	}))
	defer srv.Close()

	path := writeAuthFile(t, t.TempDir(), AnthropicAuth{
		AccessToken:  "tok-rejected",
		RefreshToken: "refresh-1",
		ExpiresAt:    time.Now().Add(4 * time.Hour), // looks valid, but the API rejected it
	})
	src := NewAnthropicFileTokenSource(path, RefreshConfig{AnthropicTokenURL: srv.URL})

	if got, _ := src.Token(context.Background()); got != "tok-rejected" {
		t.Fatalf("first Token() = %q, want tok-rejected", got)
	}
	src.Invalidate()
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() after Invalidate error = %v", err)
	}
	if got != "tok-after-401" {
		t.Fatalf("Token() after Invalidate = %q, want tok-after-401", got)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
}
