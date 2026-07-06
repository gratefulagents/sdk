package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// Anthropic interactive (PKCE) login endpoints. Anthropic's flow has no
// localhost redirect: the user opens the authorize URL, approves access, and
// the callback page displays a "code#state" string to paste back into the
// caller, which exchanges it for tokens.
const (
	// AnthropicAuthorizeMaxURL signs in a Claude Pro/Max subscription account.
	AnthropicAuthorizeMaxURL = "https://claude.ai/oauth/authorize"
	// AnthropicAuthorizeConsoleURL signs in a Claude Console (platform) account.
	AnthropicAuthorizeConsoleURL = "https://platform.claude.com/oauth/authorize"
	// AnthropicCodeCallbackURL is the redirect target whose page displays the
	// authorization code for the user to copy.
	AnthropicCodeCallbackURL = "https://platform.claude.com/oauth/code/callback"
)

// PKCECodes holds a PKCE verifier and its S256 challenge (RFC 7636).
type PKCECodes struct {
	Verifier  string
	Challenge string
}

// GeneratePKCE returns a fresh PKCE verifier/challenge pair using the S256
// challenge method.
func GeneratePKCE() (PKCECodes, error) {
	verifier, err := randomURLSafe(32)
	if err != nil {
		return PKCECodes{}, fmt.Errorf("generate pkce verifier: %w", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	return PKCECodes{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// AnthropicAuthorization describes one started interactive login: the URL the
// user must open plus the state and PKCE verifier required to complete the
// code exchange. Callers that hop processes between authorize and exchange
// (e.g. a stateless API in front of a browser) can round-trip State and
// Verifier through the initiating client, per PKCE's design.
type AnthropicAuthorization struct {
	URL         string
	RedirectURI string
	State       string
	Verifier    string
}

// NewAnthropicAuthorization starts an interactive Anthropic OAuth login. mode
// selects the account flavor: "max" (claude.ai Pro/Max subscription, the
// default when empty) or "console" (platform.claude.com workspace).
func NewAnthropicAuthorization(mode string) (AnthropicAuthorization, error) {
	authorizeURL := AnthropicAuthorizeMaxURL
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "max":
	case "console":
		authorizeURL = AnthropicAuthorizeConsoleURL
	default:
		return AnthropicAuthorization{}, fmt.Errorf("unsupported anthropic oauth mode %q (want \"max\" or \"console\")", mode)
	}
	pkce, err := GeneratePKCE()
	if err != nil {
		return AnthropicAuthorization{}, err
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return AnthropicAuthorization{}, fmt.Errorf("generate oauth state: %w", err)
	}

	u, err := url.Parse(authorizeURL)
	if err != nil {
		return AnthropicAuthorization{}, fmt.Errorf("parse authorize url: %w", err)
	}
	q := u.Query()
	q.Set("code", "true")
	q.Set("client_id", AnthropicOAuthClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", AnthropicCodeCallbackURL)
	q.Set("scope", AnthropicOAuthScope)
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	u.RawQuery = q.Encode()

	return AnthropicAuthorization{
		URL:         u.String(),
		RedirectURI: AnthropicCodeCallbackURL,
		State:       state,
		Verifier:    pkce.Verifier,
	}, nil
}

// ParseAnthropicCallbackInput extracts the authorization code and state from
// what the user pasted: Anthropic's "code#state" string, a full callback URL
// (…?code=…&state=…), or a bare query string. state is empty when the input
// carries none.
func ParseAnthropicCallbackInput(input string) (code, state string, err error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", "", fmt.Errorf("authorization code is empty")
	}

	if u, uerr := url.Parse(trimmed); uerr == nil && u.Scheme != "" && u.Host != "" {
		q := u.Query()
		if q.Get("code") != "" {
			return strings.TrimSpace(q.Get("code")), strings.TrimSpace(q.Get("state")), nil
		}
		return "", "", fmt.Errorf("callback URL is missing the code parameter")
	}

	if before, after, found := strings.Cut(trimmed, "#"); found {
		code, state = strings.TrimSpace(before), strings.TrimSpace(after)
		if code == "" {
			return "", "", fmt.Errorf("authorization code is empty")
		}
		return code, state, nil
	}

	if values, verr := url.ParseQuery(trimmed); verr == nil && values.Get("code") != "" {
		return strings.TrimSpace(values.Get("code")), strings.TrimSpace(values.Get("state")), nil
	}

	// Bare code with no state marker.
	return trimmed, "", nil
}

// ExchangeAnthropicCode exchanges a pasted authorization code for tokens and
// returns serialized flat auth JSON — the same shape RefreshAnthropicTokens
// writes, so the result feeds directly into stored auth.json material. When
// the pasted input carries a state it must match authz.State.
func ExchangeAnthropicCode(ctx context.Context, pasted string, authz AnthropicAuthorization, cfg RefreshConfig) ([]byte, error) {
	code, state, err := ParseAnthropicCallbackInput(pasted)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(authz.Verifier) == "" {
		return nil, fmt.Errorf("anthropic code exchange requires the PKCE verifier from the authorization step")
	}
	if state != "" && authz.State != "" && state != authz.State {
		return nil, fmt.Errorf("authorization state mismatch; restart the sign-in and paste the newest code")
	}
	if state == "" {
		state = authz.State
	}
	body := map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"state":         state,
		"client_id":     firstNonEmpty(cfg.AnthropicClientID, AnthropicOAuthClientID),
		"redirect_uri":  firstNonEmpty(authz.RedirectURI, AnthropicCodeCallbackURL),
		"code_verifier": authz.Verifier,
	}
	raw, err := postAnthropicToken(ctx, cfg, "code exchange", body)
	if err != nil {
		return nil, err
	}
	auth, err := adoptAnthropicTokenResponse(AnthropicAuth{}, raw, "code exchange", refreshNow(cfg))
	if err != nil {
		return nil, err
	}
	return MarshalAnthropicAuthJSON(auth)
}

// randomURLSafe returns n random bytes encoded as unpadded base64url.
func randomURLSafe(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
