# Providers

This example shows how to configure the built-in OpenAI-compatible and Anthropic providers without making network calls. The provider integration examples in `examples/openai_api`, `examples/openai_oauth`, `examples/anthropic_api`, and `examples/openrouter` show real credentialed calls.

Run it without live provider calls:

```sh
GRATEFUL_LIVE_TESTS=skip go test ./examples/features/providers
```

## Live tests

Three live test files in this package exercise real provider APIs:

- `anthropic_live_test.go` — basic chat, tool calling, and streaming against the live Anthropic API. Requires `ANTHROPIC_API_KEY`.
- `openrouter_live_test.go` — basic chat and streaming against OpenRouter via the OpenAI-compatible chat-completions endpoint. Requires `OPENROUTER_API_KEY`.
- `multi_provider_live_test.go` — registers the live OpenAI OAuth provider and the live Anthropic provider behind a single `MultiProvider` and routes by `openai/<model>` and `anthropic/<model>` prefixes. Requires both `$HOME/.codex/auth.json` (Codex OAuth) and `ANTHROPIC_API_KEY`.

These tests skip when credentials are missing. To force fail-fast credential checks for live CI, export `GRATEFUL_LIVE_TESTS=required`. To skip live provider calls entirely, export `GRATEFUL_LIVE_TESTS=skip`.

```sh
ANTHROPIC_API_KEY=sk-...     go test -run TestAnthropicLive   ./examples/features/providers
OPENROUTER_API_KEY=sk-or-... go test -run TestOpenRouterLive  ./examples/features/providers
ANTHROPIC_API_KEY=sk-...     go test -run TestMultiProvider   ./examples/features/providers
```

Optional overrides: `ANTHROPIC_LIVE_MODEL`, `ANTHROPIC_BASE_URL`, `OPENROUTER_LIVE_MODEL`, `OPENROUTER_BASE_URL`.

## Anthropic OAuth

The Anthropic provider accepts either an API key or a host-supplied OAuth access token. For OAuth, pass the access token as `APIKey` and set `AuthMode: "oauth"`:

```go
provider := sdkanthropic.NewProviderWithConfig(sdkanthropic.ProviderConfig{
	APIKey:   anthropicOAuthAccessToken,
	AuthMode: "oauth",
})
```

The SDK then sends `Authorization: Bearer <token>` and the Anthropic OAuth beta header instead of `x-api-key`. Hosts remain responsible for acquiring and refreshing Anthropic OAuth tokens. When using `providers.NewProviderFromConfig` with `Provider: "multi"`, `AuthMode: "oauth"` applies to Anthropic only when Anthropic is the selected/default provider; Anthropic fallback providers continue to use their configured API key.

## Routing the same provider under multiple auths (`Routes`)

To expose one base provider under several prefixes with independent auth — e.g. API key vs OAuth for the same backend — declare `Routes` on the `ProviderSpec`. Each route is registered under its `Prefix`, so callers select the auth by model prefix at request time. A non-empty `Routes` list implies multi-provider behavior, and a route whose `Prefix` matches a canonical name (e.g. `anthropic`) overrides that default registration.

```go
provider, _ := providers.NewProviderFromConfig(providers.ProviderSpec{
	Provider: "anthropic",
	Routes: []providers.ProviderRoute{
		{Prefix: "anthropic", Provider: "anthropic", APIKey: anthropicAPIKey},
		{Prefix: "anthropic-oauth", Provider: "anthropic", AuthMode: "oauth", APIKey: anthropicOAuthToken},
	},
})
// "anthropic/<model>"       → x-api-key
// "anthropic-oauth/<model>" → Authorization: Bearer + OAuth beta
```

Per-agent/role selection is then just the model string: a role with `ModelOverride: "anthropic-oauth/claude-opus-4.8"` uses OAuth while `"anthropic/claude-sonnet-4.5"` uses the API key. Prefixed `FallbackModels` resolve through the same routing, so you can also fail over across auths (e.g. OAuth subscription → API key on rate limit).

For the lower-level Anthropic client, use `sdkanthropic.WithOAuthToken(token)` with `sdkanthropic.NewClient`.

How to use this feature:

- Use `sdkopenai.NewProviderWithConfig` for OpenAI, OpenAI-compatible gateways, OpenRouter, or local `/v1` servers.
- Use `sdkanthropic.NewProviderWithConfig` for Anthropic API-key or OAuth bearer-token auth.
- Pass the provider to `agentsdk.NewRunnerWithProvider`.
- Use `sdkopenai.NewClient` plus `sdkopenai.NewModelWithClient` when a gateway model ID contains slashes and should be sent untouched.
- Use `sdkopenai.ValidateChatCompletionsModel` before forcing Chat Completions mode.

Runnable source: [providers_test.go](providers_test.go).
