package oauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// AnthropicTokenSourceLead is the in-process refresh window for
// AnthropicFileTokenSource. It is deliberately much shorter than
// AnthropicRefreshLead: when a central refresher (e.g. the platform operator)
// owns rotation, the mounted file is normally updated long before this window,
// so the in-process refresh only fires when central rotation failed or does
// not exist (standalone SDK use). Keeping the window small avoids racing the
// central refresher for the single-use refresh token.
const AnthropicTokenSourceLead = 2 * time.Minute

// AnthropicFileTokenSource resolves Anthropic OAuth access tokens per request
// from an auth JSON file (Claude Code .credentials.json or the SDK's flat
// shape). It re-reads the file when it changes — picking up tokens rotated by
// an external refresher, such as a Kubernetes Secret mount — and, as a last
// resort, self-refreshes near expiry using the refresh token, writing the
// result back best-effort (read-only mounts keep the refreshed token in
// memory only).
//
// It implements the anthropic client's TokenSource contract:
// Token(ctx) (string, error) and Invalidate().
type AnthropicFileTokenSource struct {
	path string
	cfg  RefreshConfig
	lead time.Duration

	mu          sync.Mutex
	auth        AnthropicAuth
	loaded      bool
	fileModTime time.Time
	fileSize    int64
	// badToken is an access token rejected by the API (Invalidate); it forces
	// a re-read and, if the file still holds the same token, a refresh.
	badToken string
}

// NewAnthropicFileTokenSource creates a token source backed by the auth JSON
// file at path. cfg controls the refresh HTTP client and endpoint overrides;
// zero values use the production Anthropic OAuth defaults.
func NewAnthropicFileTokenSource(path string, cfg RefreshConfig) *AnthropicFileTokenSource {
	return &AnthropicFileTokenSource{
		path: strings.TrimSpace(path),
		cfg:  cfg,
		lead: AnthropicTokenSourceLead,
	}
}

// WithRefreshLead overrides the near-expiry window that triggers an
// in-process refresh. Returns the receiver for chaining.
func (s *AnthropicFileTokenSource) WithRefreshLead(lead time.Duration) *AnthropicFileTokenSource {
	if lead > 0 {
		s.lead = lead
	}
	return s
}

// Token returns a current access token, re-reading the backing file when it
// changed and refreshing via the OAuth token endpoint when the token is
// missing, rejected, or near expiry.
func (s *AnthropicFileTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := refreshNow(s.cfg)
	s.reloadIfChangedLocked()

	token := strings.TrimSpace(s.auth.AccessToken)
	stale := token == "" || (s.badToken != "" && token == s.badToken) || s.nearExpiryLocked(now)
	if stale && strings.TrimSpace(s.auth.RefreshToken) != "" {
		if err := s.refreshLocked(ctx); err != nil {
			// Grace: a not-yet-expired token may still be accepted even when
			// the refresh endpoint is flaky; only fail hard when the token is
			// unusable (missing, rejected, or past expiry).
			if token == "" || token == s.badToken || s.expiredLocked(now) {
				return "", err
			}
		}
	}

	token = strings.TrimSpace(s.auth.AccessToken)
	if token == "" {
		return "", errors.New("anthropic oauth material is missing access token")
	}
	return token, nil
}

// Invalidate marks the current access token as rejected so the next Token
// call re-reads the file and refreshes if the file still holds the same
// credential. Called by the API client after a 401.
func (s *AnthropicFileTokenSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.badToken = strings.TrimSpace(s.auth.AccessToken)
	// Force a re-read: an external refresher may already have rotated the file.
	s.loaded = false
	s.fileModTime = time.Time{}
	s.fileSize = 0
}

// reloadIfChangedLocked re-reads the backing file when it is unread or its
// stat signature changed. Read failures keep the in-memory credential, which
// still works for read-only or racy mounts.
func (s *AnthropicFileTokenSource) reloadIfChangedLocked() {
	if s.path == "" {
		return
	}
	info, statErr := os.Stat(s.path)
	if statErr == nil && s.loaded && info.ModTime().Equal(s.fileModTime) && info.Size() == s.fileSize {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	auth, err := ParseAnthropicAuthJSON(raw)
	if err != nil {
		return
	}
	s.auth = auth
	s.loaded = true
	// Cache the signature captured *before* the read: if the file rotated
	// between stat and read, the stale signature forces a harmless re-read on
	// the next call, whereas re-stating here could pair the new file's
	// signature with the old contents and suppress reloads until Invalidate.
	if statErr == nil {
		s.fileModTime = info.ModTime()
		s.fileSize = info.Size()
	} else {
		s.fileModTime = time.Time{}
		s.fileSize = 0
	}
	// A rotated file supersedes any previous rejection.
	if strings.TrimSpace(auth.AccessToken) != s.badToken {
		s.badToken = ""
	}
}

func (s *AnthropicFileTokenSource) nearExpiryLocked(now time.Time) bool {
	if s.auth.ExpiresAt.IsZero() {
		return false
	}
	return !s.auth.ExpiresAt.After(now.Add(s.lead))
}

func (s *AnthropicFileTokenSource) expiredLocked(now time.Time) bool {
	if s.auth.ExpiresAt.IsZero() {
		return false
	}
	return !s.auth.ExpiresAt.After(now)
}

// refreshLocked exchanges the refresh token and adopts + persists the result.
// It runs on a context detached from the request: refresh tokens are
// single-use, so aborting the exchange mid-flight (request cancelled) could
// consume the token without capturing its replacement.
func (s *AnthropicFileTokenSource) refreshLocked(ctx context.Context) error {
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	updated, err := RefreshAnthropicTokens(refreshCtx, s.auth, s.cfg)
	if err != nil {
		return fmt.Errorf("refresh anthropic oauth token: %w", err)
	}
	auth, err := ParseAnthropicAuthJSON(updated)
	if err != nil {
		return fmt.Errorf("parse refreshed anthropic oauth material: %w", err)
	}
	s.auth = auth
	s.badToken = ""
	s.writeBackLocked(updated)
	return nil
}

// writeBackLocked persists refreshed material so restarts and sibling
// processes see it. Failures are expected on read-only mounts and ignored;
// the refreshed token stays usable in memory.
func (s *AnthropicFileTokenSource) writeBackLocked(raw []byte) {
	if s.path == "" {
		return
	}
	if err := os.WriteFile(s.path, raw, 0o600); err != nil {
		return
	}
	if info, err := os.Stat(s.path); err == nil {
		s.fileModTime = info.ModTime()
		s.fileSize = info.Size()
	}
}
