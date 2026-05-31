#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

export TB_DATASET="${TB_DATASET:-terminal-bench/terminal-bench-2-1}"
export TB_PROFILE="${TB_PROFILE:-stability}"
export TB_N_ATTEMPTS="${TB_N_ATTEMPTS:-5}"
export GRATEFUL_PROVIDER="${GRATEFUL_PROVIDER:-openai}"
export GRATEFUL_MODEL="${GRATEFUL_MODEL:-gpt-5.5}"
export GRATEFUL_MAX_TURNS="${GRATEFUL_MAX_TURNS:-150}"
export GRATEFUL_MAX_TOKENS="${GRATEFUL_MAX_TOKENS:-8192}"
export GRATEFUL_PERMISSION_MODE="${GRATEFUL_PERMISSION_MODE:-danger-full-access}"
export GRATEFUL_TOOL_ACCESS="${GRATEFUL_TOOL_ACCESS:-full}"
export GRATEFUL_WEB_TOOLS="${GRATEFUL_WEB_TOOLS:-false}"
export GRATEFUL_TB_COMPLIANCE="${GRATEFUL_TB_COMPLIANCE:-true}"
export GRATEFULAGENTS_ALLOW_PRIVATE_NETWORK_URLS="${GRATEFULAGENTS_ALLOW_PRIVATE_NETWORK_URLS:-false}"

exec "$ROOT/scripts/run-terminal-bench.sh" "$@"
