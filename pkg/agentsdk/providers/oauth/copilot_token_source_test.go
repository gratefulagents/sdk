package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func copilotTokenEndpoint(t *testing.T, calls *atomic.Int64, token string, expiresAt time.Time, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"nope"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      token,
			"expires_at": expiresAt.Unix(),
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCopilotTokenSourceServesFreshTokenWithoutRefresh(t *testing.T) {
	now := time.Now()
	var calls atomic.Int64
	srv := copilotTokenEndpoint(t, &calls, "unused", now.Add(time.Hour), http.StatusOK)

	source := NewCopilotTokenSource(CopilotTokenSourceConfig{
		Auth: CopilotAuth{
			OAuthToken: "github-oauth",
			Token:      "fresh-token",
			ExpiresAt:  now.Add(time.Hour),
		},
		Refresh: RefreshConfig{CopilotTokenURL: srv.URL, Now: func() time.Time { return now }},
	})
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "fresh-token" {
		t.Fatalf("Token() = %q, want fresh-token", token)
	}
	if calls.Load() != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", calls.Load())
	}
}

func TestCopilotTokenSourceRefreshesNearExpiry(t *testing.T) {
	now := time.Now()
	var calls atomic.Int64
	srv := copilotTokenEndpoint(t, &calls, "minted-token", now.Add(25*time.Minute), http.StatusOK)

	source := NewCopilotTokenSource(CopilotTokenSourceConfig{
		Auth: CopilotAuth{
			OAuthToken: "github-oauth",
			Token:      "stale-token",
			ExpiresAt:  now.Add(time.Minute), // inside CopilotRefreshLead
		},
		Refresh: RefreshConfig{CopilotTokenURL: srv.URL, Now: func() time.Time { return now }},
	})
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "minted-token" {
		t.Fatalf("Token() = %q, want minted-token", token)
	}
	if calls.Load() != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", calls.Load())
	}
	// Second call is served from the refreshed in-memory material.
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatalf("Token() second call error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("token endpoint calls after second Token() = %d, want 1", calls.Load())
	}
}

func TestCopilotTokenSourceRefreshesWhenTokenMissing(t *testing.T) {
	now := time.Now()
	var calls atomic.Int64
	srv := copilotTokenEndpoint(t, &calls, "minted-token", now.Add(25*time.Minute), http.StatusOK)

	source := NewCopilotTokenSource(CopilotTokenSourceConfig{
		Auth:    CopilotAuth{OAuthToken: "github-oauth"},
		Refresh: RefreshConfig{CopilotTokenURL: srv.URL, Now: func() time.Time { return now }},
	})
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "minted-token" {
		t.Fatalf("Token() = %q, want minted-token", token)
	}
}

func TestCopilotTokenSourceStaticTokenOnlyMaterial(t *testing.T) {
	source := NewCopilotTokenSource(CopilotTokenSourceConfig{
		Auth: CopilotAuth{Token: "static-token", ExpiresAt: time.Now().Add(-time.Hour)},
	})
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "static-token" {
		t.Fatalf("Token() = %q, want static-token", token)
	}

	empty := NewCopilotTokenSource(CopilotTokenSourceConfig{})
	if _, err := empty.Token(context.Background()); err == nil {
		t.Fatal("Token() with empty material error = nil, want error")
	}
}

func TestCopilotTokenSourceFallsBackToUnexpiredTokenOnFailure(t *testing.T) {
	now := time.Now()
	var calls atomic.Int64
	srv := copilotTokenEndpoint(t, &calls, "", now, http.StatusInternalServerError)

	source := NewCopilotTokenSource(CopilotTokenSourceConfig{
		Auth: CopilotAuth{
			OAuthToken: "github-oauth",
			Token:      "still-valid",
			ExpiresAt:  now.Add(2 * time.Minute), // inside lead but not expired
		},
		Refresh: RefreshConfig{CopilotTokenURL: srv.URL, Now: func() time.Time { return now }},
	})
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v, want graceful fallback", err)
	}
	if token != "still-valid" {
		t.Fatalf("Token() = %q, want still-valid", token)
	}
	// Within the failure cooldown no new exchange is attempted.
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatalf("Token() during cooldown error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("token endpoint calls = %d, want 1 (cooldown)", calls.Load())
	}
}

func TestCopilotTokenSourceErrorsWhenExpiredAndRefreshFails(t *testing.T) {
	now := time.Now()
	var calls atomic.Int64
	srv := copilotTokenEndpoint(t, &calls, "", now, http.StatusUnauthorized)

	source := NewCopilotTokenSource(CopilotTokenSourceConfig{
		Auth: CopilotAuth{
			OAuthToken: "github-oauth",
			Token:      "expired-token",
			ExpiresAt:  now.Add(-time.Minute),
		},
		Refresh: RefreshConfig{CopilotTokenURL: srv.URL, Now: func() time.Time { return now }},
	})
	if _, err := source.Token(context.Background()); err == nil {
		t.Fatal("Token() error = nil, want error for expired token with failing refresh")
	}
}

func TestCopilotTokenSourceReloadsRotatedAuthPath(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	writeAuth := func(token string, expiresAt time.Time) {
		t.Helper()
		payload := fmt.Sprintf(`{"oauth_token":"github-oauth","token":%q,"expires_at":%d}`, token, expiresAt.Unix())
		if err := os.WriteFile(authPath, []byte(payload), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	writeAuth("mounted-token", now.Add(time.Hour))

	var calls atomic.Int64
	srv := copilotTokenEndpoint(t, &calls, "unused", now, http.StatusInternalServerError)
	source := NewCopilotTokenSource(CopilotTokenSourceConfig{
		AuthPath: authPath,
		Refresh:  RefreshConfig{CopilotTokenURL: srv.URL, Now: func() time.Time { return now }},
	})

	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "mounted-token" {
		t.Fatalf("Token() = %q, want mounted-token", token)
	}

	// An external refresher rotates the mounted secret: the newer token wins.
	writeAuth("rotated-token", now.Add(2*time.Hour))
	token, err = source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() after rotation error = %v", err)
	}
	if token != "rotated-token" {
		t.Fatalf("Token() = %q, want rotated-token", token)
	}
	// A stale file (older expiry) must not clobber fresher in-memory material.
	writeAuth("stale-token", now.Add(time.Minute))
	token, err = source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() with stale file error = %v", err)
	}
	if token != "rotated-token" {
		t.Fatalf("Token() = %q, want rotated-token (stale file ignored)", token)
	}
	if calls.Load() != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", calls.Load())
	}
}

func TestCopilotTokenSourceSelfRefreshUsesOAuthTokenFromPath(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	// Mounted material is near expiry: the source must self-exchange.
	payload := fmt.Sprintf(`{"oauth_token":"github-oauth","token":"near-expiry","expires_at":%d}`, now.Add(time.Minute).Unix())
	if err := os.WriteFile(authPath, []byte(payload), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var calls atomic.Int64
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "self-minted",
			"expires_at": now.Add(25 * time.Minute).Unix(),
		})
	}))
	defer srv.Close()

	source := NewCopilotTokenSource(CopilotTokenSourceConfig{
		AuthPath: authPath,
		Refresh:  RefreshConfig{CopilotTokenURL: srv.URL, Now: func() time.Time { return now }},
	})
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "self-minted" {
		t.Fatalf("Token() = %q, want self-minted", token)
	}
	if got := gotAuth.Load(); got != "token github-oauth" {
		t.Fatalf("Authorization = %v, want token github-oauth", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", calls.Load())
	}
}
