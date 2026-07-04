package oauth

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// copilotRefreshFailureCooldown throttles token-exchange retries after a
// failed refresh so per-request callers don't hammer the endpoint.
const copilotRefreshFailureCooldown = 15 * time.Second

// CopilotTokenSourceConfig configures a CopilotTokenSource.
type CopilotTokenSourceConfig struct {
	// Auth seeds the in-memory material. Optional when AuthPath is set.
	Auth CopilotAuth
	// AuthPath, when set, points at serialized Copilot auth JSON (flat SDK
	// shape or GitHub hosts.json shape) that is re-read on demand so external
	// rotations (e.g. a refreshed Kubernetes Secret mount or a re-login) are
	// picked up without restarting the process.
	AuthPath string
	// Refresh configures the token-exchange requests.
	Refresh RefreshConfig
}

// CopilotTokenSource yields a valid Copilot API token on demand. GitHub mints
// Copilot API tokens with a short (~25–30 minute) lifetime; this source
// re-exchanges the long-lived GitHub OAuth token for a fresh API token when
// the current one is missing or near expiry, so long-running processes never
// serve requests with an expired token.
type CopilotTokenSource struct {
	mu       sync.Mutex
	auth     CopilotAuth
	authPath string
	cfg      RefreshConfig

	lastAttempt time.Time
	lastErr     error
}

// NewCopilotTokenSource creates a CopilotTokenSource.
func NewCopilotTokenSource(cfg CopilotTokenSourceConfig) *CopilotTokenSource {
	return &CopilotTokenSource{
		auth:     cfg.Auth,
		authPath: strings.TrimSpace(cfg.AuthPath),
		cfg:      cfg.Refresh,
	}
}

// Token returns a Copilot API token that is valid now, refreshing it first
// when it is missing or inside the refresh lead window. When a refresh fails
// but the current token has not expired yet, the current token is returned so
// transient exchange failures don't take down in-flight work.
func (s *CopilotTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := refreshNow(s.cfg)
	s.reloadFromPathLocked()

	token := strings.TrimSpace(s.auth.Token)
	if token != "" && !CopilotNeedsRefresh(s.auth, now) {
		return token, nil
	}
	if strings.TrimSpace(s.auth.OAuthToken) == "" {
		// Static-token-only material: nothing to exchange, serve what we have.
		if token != "" {
			return token, nil
		}
		return "", fmt.Errorf("copilot auth material has neither an API token nor a GitHub OAuth token")
	}
	if s.lastErr != nil && now.Sub(s.lastAttempt) < copilotRefreshFailureCooldown {
		if fallback, ok := s.unexpiredTokenLocked(now); ok {
			return fallback, nil
		}
		return "", fmt.Errorf("copilot token refresh recently failed: %w", s.lastErr)
	}

	s.lastAttempt = now
	updated, err := RefreshCopilotToken(ctx, s.auth, s.cfg)
	if err != nil {
		s.lastErr = err
		if fallback, ok := s.unexpiredTokenLocked(now); ok {
			return fallback, nil
		}
		return "", err
	}
	s.lastErr = nil
	parsed, err := ParseCopilotAuthJSON(updated)
	if err != nil {
		return "", fmt.Errorf("parse refreshed copilot auth material: %w", err)
	}
	s.auth = parsed
	return strings.TrimSpace(s.auth.Token), nil
}

// CurrentToken returns the current API token without triggering a refresh,
// re-reading AuthPath first when configured. It may be empty or expired.
func (s *CopilotTokenSource) CurrentToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadFromPathLocked()
	return strings.TrimSpace(s.auth.Token)
}

// unexpiredTokenLocked returns the current token when it is still within its
// validity window (refreshes lead expiry, so a failed refresh can still fall
// back to it until it actually expires).
func (s *CopilotTokenSource) unexpiredTokenLocked(now time.Time) (string, bool) {
	token := strings.TrimSpace(s.auth.Token)
	if token == "" {
		return "", false
	}
	if !s.auth.ExpiresAt.IsZero() && !s.auth.ExpiresAt.After(now) {
		return "", false
	}
	return token, true
}

// reloadFromPathLocked merges auth material from AuthPath into memory: the
// file is authoritative for the GitHub OAuth token (a re-login rotates it),
// and its API token is adopted when it outlives the in-memory one (an
// external refresher rotated the mounted Secret). Read or parse failures keep
// the in-memory material.
func (s *CopilotTokenSource) reloadFromPathLocked() {
	if s.authPath == "" {
		return
	}
	data, err := os.ReadFile(s.authPath)
	if err != nil {
		return
	}
	parsed, err := ParseCopilotAuthJSON(data)
	if err != nil {
		return
	}
	if oauthToken := strings.TrimSpace(parsed.OAuthToken); oauthToken != "" {
		s.auth.OAuthToken = oauthToken
	}
	fileToken := strings.TrimSpace(parsed.Token)
	if fileToken == "" {
		return
	}
	if strings.TrimSpace(s.auth.Token) == "" || parsed.ExpiresAt.After(s.auth.ExpiresAt) {
		s.auth.Token = fileToken
		s.auth.ExpiresAt = parsed.ExpiresAt
		s.auth.LastRefresh = parsed.LastRefresh
	}
}
