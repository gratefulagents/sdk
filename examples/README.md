# SDK Examples

These examples are Go tests. Live examples skip when their credentials are missing or `GRATEFUL_LIVE_TESTS=skip` is set. Set `GRATEFUL_LIVE_TESTS=required` when a live run should fail fast on missing credentials.

Feature examples are in [features](features/README.md):

```sh
GRATEFUL_LIVE_TESTS=skip go test ./examples/features/...
```

Run all examples:

```sh
GRATEFUL_LIVE_TESTS=skip go test ./examples/...
```

Run one provider example:

```sh
OPENAI_API_KEY=... go test ./examples/openai_api
OPENAI_OAUTH_AUTH_JSON_PATH=/path/to/auth.json go test ./examples/openai_oauth
ANTHROPIC_API_KEY=... go test ./examples/anthropic_api
OPENROUTER_API_KEY=... go test ./examples/openrouter
```

Useful optional overrides:

```sh
OPENAI_MODEL=gpt-4.1-mini
OPENAI_BASE_URL=https://api.openai.com/v1
OPENAI_API_MODE=responses

OPENAI_OAUTH_MODEL=gpt-4.1-mini
OPENAI_OAUTH_ACCOUNT_ID=...
OPENAI_OAUTH_ACCOUNT_ID_PATH=/path/to/account-id

ANTHROPIC_MODEL=claude-haiku-4-5
ANTHROPIC_BASE_URL=https://api.anthropic.com

OPENROUTER_MODEL=openai/gpt-4o-mini
OPENROUTER_BASE_URL=https://openrouter.ai/api/v1
```
