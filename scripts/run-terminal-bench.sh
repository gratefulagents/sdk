#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CACHE_DIR="${GRATEFUL_TB_CACHE_DIR:-$ROOT/.grateful-evals/terminal-bench}"
VENV_DIR="${GRATEFUL_TB_VENV:-$CACHE_DIR/venv}"
PYTHON_BIN="${PYTHON:-python3}"
OUTPUT_PATH="${TB_OUTPUT_PATH:-${JOBS_DIR:-jobs}}"
RAW_RUN_ID="${RUN_ID:-grateful-terminal-bench-$(date -u +%Y%m%dt%H%M%Sz)}"
RUN_ID="$(printf '%s' "$RAW_RUN_ID" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9_-]+/-/g; s/^[^a-z0-9]+//; s/[^a-z0-9]+$//; s/-+/-/g')"
if [[ -z "$RUN_ID" ]]; then
  RUN_ID="grateful-terminal-bench-$(date -u +%Y%m%dt%H%M%Sz)"
fi
DATASET="${TB_DATASET:-${DATASET:-terminal-bench/terminal-bench-2-1}}"
HARBOR_ENV="${HARBOR_ENV:-docker}"
TB_PROFILE="${TB_PROFILE:-stability}"
case "$TB_PROFILE" in
  leaderboard|stability|debug) ;;
  *)
    printf '[terminal-bench] ERROR: unsupported TB_PROFILE=%s; use leaderboard, stability, or debug\n' "$TB_PROFILE" >&2
    exit 1
    ;;
esac
if [[ -z "${TB_N_CONCURRENT+x}" ]]; then
  if [[ "$HARBOR_ENV" == "docker" && "$TB_PROFILE" != "leaderboard" ]]; then
    TB_N_CONCURRENT="8"
  else
    TB_N_CONCURRENT="32"
  fi
fi
TB_N_ATTEMPTS="${TB_N_ATTEMPTS:-5}"
TASK_NAMES="${TB_TASK_NAMES:-${TB_TASK_IDS:-${TASK_NAMES:-${TASK_IDS:-}}}}"
if [[ -z "${TB_MAX_RETRIES+x}" ]]; then
  if [[ "$TB_PROFILE" == "stability" ]]; then
    TB_MAX_RETRIES="2"
  else
    TB_MAX_RETRIES="0"
  fi
fi
TB_AGENT_TIMEOUT_MULTIPLIER="${TB_AGENT_TIMEOUT_MULTIPLIER:-}"
TB_VERIFIER_TIMEOUT_MULTIPLIER="${TB_VERIFIER_TIMEOUT_MULTIPLIER:-}"
TB_ENVIRONMENT_BUILD_TIMEOUT_MULTIPLIER="${TB_ENVIRONMENT_BUILD_TIMEOUT_MULTIPLIER:-}"
TB_DOCKER_CLEANUP="${TB_DOCKER_CLEANUP:-1}"
TB_DOCKER_PREPULL="${TB_DOCKER_PREPULL:-1}"
TB_DOCKER_PULL_PARALLELISM="${TB_DOCKER_PULL_PARALLELISM:-4}"
TB_DOCKER_PULL_RETRIES="${TB_DOCKER_PULL_RETRIES:-3}"
TB_DOCKER_PULL_EXISTING="${TB_DOCKER_PULL_EXISTING:-0}"
TB_DOCKER_IMAGES_FILE="${TB_DOCKER_IMAGES_FILE:-}"
TB_PREPULL_IMAGES="${TB_PREPULL_IMAGES:-}"
TB_AUTO_PREPULL_IMAGES="${TB_AUTO_PREPULL_IMAGES:-1}"
TB_TASK_CACHE_DIR="${TB_TASK_CACHE_DIR:-$HOME/.cache/harbor/tasks/packages}"
TB_PREPULL_ONLY="${TB_PREPULL_ONLY:-0}"

trap 'status=$?; printf "[terminal-bench] ERROR: failed at line %s: %s\n" "$LINENO" "$BASH_COMMAND" >&2; exit "$status"' ERR

log() {
  printf '[terminal-bench] %s\n' "$*"
}

die() {
  printf '[terminal-bench] ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Run Terminal-Bench 2.1 through Harbor against the Grateful Agents SDK harness.

Usage:
  ./scripts/run-terminal-bench.sh [extra harbor run args...]

Common environment variables:
  OPENAI_OAUTH_AUTH_JSON_PATH=~/.codex/auth.json
                                             Use OpenAI OAuth auth for tests.
  OPENAI_AUTH_MODE=api-key OPENAI_API_KEY=...
                                             Opt into OpenAI API-key auth.
  GRATEFUL_PROVIDER=openai|anthropic|openrouter|local
  GRATEFUL_MODEL=gpt-5.5
  TB_DATASET=terminal-bench/terminal-bench-2-1
  TB_TASK_NAMES=task_one,task_two            Run selected task names.
  TB_PROFILE=stability|leaderboard|debug     Stability uses safer Docker defaults.
  TB_N_CONCURRENT=8                          Override parallelism.
  TB_N_ATTEMPTS=5                            Leaderboard-style attempts.
  TB_MAX_RETRIES=2                           Retry infrastructure exceptions.
  TB_AGENT_TIMEOUT_MULTIPLIER=2              Optional Harbor timeout multiplier.
  GRATEFUL_ASYNC_BASH=false                  Enable SDK background shell tools.
  GRATEFUL_WEB_TOOLS=false                   Disable SDK WebFetch in Terminal-Bench.
  GRATEFUL_TB_COMPLIANCE=true                Block Terminal-Bench web/repo lookups from tool inputs.
  GRATEFUL_FINAL_CHECK=true                  Enable generic final artifact check.
  GRATEFUL_EXIT_ZERO_ON_TIMEOUT=true         Let verifier grade artifacts after SDK timeout.
  TB_AUTO_PREPULL_IMAGES=1                   Discover task images from Harbor cache.
  TB_DOCKER_PULL_EXISTING=0                  Skip images already present locally.
  TB_PREPULL_IMAGES=image1,image2            Optional images to pull before trials.
  TB_DOCKER_IMAGES_FILE=images.txt           Optional newline-delimited pull list.
  TB_PREPULL_ONLY=1                          Pull images and exit without running eval.
  HARBOR_ENV=docker|daytona|modal|e2b|runloop
  RUN_ID=my-run                              Job name under jobs/.

The script creates a local Python 3.12+ venv under .grateful-evals/, installs
Harbor when needed, builds cmd/grateful-agent-run for Linux, and runs:

  harbor run -d terminal-bench/terminal-bench-2-1 \
    --agent-import-path eval.terminal_bench.harbor_grateful_agent:GratefulHarborAgent \
    -m "$GRATEFUL_MODEL" -n "$TB_N_CONCURRENT" -k "$TB_N_ATTEMPTS"

Output is written to jobs/<run-id>/ by default, with the full Harbor console log
also saved as jobs/<run-id>.harbor-run.log.
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

if [[ "$(uname -s)" != "Linux" ]]; then
  die "this bootstrap script is intended for Linux hosts"
fi

arch_from_uname() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) die "unsupported CPU architecture: $(uname -m)" ;;
  esac
}

default_model_for_provider() {
  case "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" in
    anthropic) echo "claude-sonnet-4-6" ;;
    openrouter) echo "deepseek/deepseek-v4-flash" ;;
    local) echo "llama3.1" ;;
    *) echo "gpt-5.5" ;;
  esac
}

ensure_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required: $2"
}

ensure_go() {
  if command -v go >/dev/null 2>&1; then
    return
  fi

  ensure_cmd curl "install Go or make curl available so this script can install Go locally"
  ensure_cmd tar "install tar"
  ensure_cmd gzip "install gzip"

  local go_version go_arch install_root archive tmp_dir
  go_version="${GO_VERSION:-$(awk '/^go / {print $2; exit}' "$ROOT/go.mod")}"
  go_arch="$(arch_from_uname)"
  install_root="$CACHE_DIR/go/go${go_version}.linux-${go_arch}"

  if [[ ! -x "$install_root/bin/go" ]]; then
    log "Go not found; installing Go ${go_version} locally"
    mkdir -p "$CACHE_DIR/go"
    tmp_dir="$(mktemp -d)"
    archive="$tmp_dir/go.tgz"
    curl -fsSL "https://go.dev/dl/go${go_version}.linux-${go_arch}.tar.gz" -o "$archive"
    tar -C "$tmp_dir" -xzf "$archive"
    rm -rf "$install_root"
    mv "$tmp_dir/go" "$install_root"
    rm -rf "$tmp_dir"
  fi

  export PATH="$install_root/bin:$PATH"
}

ensure_python_venv() {
  ensure_cmd "$PYTHON_BIN" "install Python 3.12+ or set PYTHON=/path/to/python3.12"

  "$PYTHON_BIN" - <<'PY' || die "Harbor requires Python 3.12+; install it or set PYTHON=/path/to/python3.12"
import sys
raise SystemExit(0 if sys.version_info >= (3, 12) else 1)
PY

  if [[ ! -x "$VENV_DIR/bin/python" ]]; then
    log "Creating Python venv at $VENV_DIR"
    mkdir -p "$(dirname "$VENV_DIR")"
    if ! "$PYTHON_BIN" -m venv "$VENV_DIR"; then
      die "$PYTHON_BIN -m venv failed; on Debian/Ubuntu install python3-venv"
    fi
    "$VENV_DIR/bin/python" -m pip install --upgrade pip
  fi

  if [[ ! -x "$VENV_DIR/bin/harbor" || "${HARBOR_REFRESH:-0}" == "1" ]]; then
    log "Installing Harbor into $VENV_DIR"
    "$VENV_DIR/bin/python" -m pip install --upgrade "${HARBOR_PACKAGE:-harbor}"
  fi

  HARBOR_BIN="$VENV_DIR/bin/harbor"
}

ensure_harbor_host() {
  ensure_cmd git "install git"
  if [[ "$HARBOR_ENV" == "docker" ]]; then
    ensure_cmd docker "install Docker Engine and start the daemon"
    docker info >/dev/null 2>&1 || die "Docker is not reachable; start Docker or add your user to the docker group"
  fi
}

docker_auth_configured() {
  local config="${DOCKER_CONFIG:-$HOME/.docker}/config.json"
  [[ -f "$config" ]] && grep -q '"auths"[[:space:]]*:' "$config"
}

cleanup_docker_run_resources() {
  if [[ "$HARBOR_ENV" != "docker" || "$TB_DOCKER_CLEANUP" == "0" || "$TB_DOCKER_CLEANUP" == "false" ]]; then
    return 0
  fi

  local containers networks
  containers="$(docker ps -aq --filter "name=$RUN_ID" 2>/dev/null || true)"
  if [[ -n "$containers" ]]; then
    log "Removing stale Docker containers matching run id $RUN_ID"
    docker rm -f $containers >/dev/null
  fi

  networks="$(docker network ls -q --filter "name=$RUN_ID" 2>/dev/null || true)"
  if [[ -n "$networks" ]]; then
    log "Removing stale Docker networks matching run id $RUN_ID"
    docker network rm $networks >/dev/null 2>&1 || true
  fi
}

preflight_docker() {
  if [[ "$HARBOR_ENV" != "docker" ]]; then
    return 0
  fi

  if docker_auth_configured; then
    log "Docker auth config found; prebuilt image pulls should avoid anonymous rate limits"
  elif [[ "${TB_REQUIRE_DOCKER_AUTH:-0}" == "1" || "${TB_REQUIRE_DOCKER_AUTH:-}" == "true" ]]; then
    die "Docker auth config not found; run docker login or unset TB_REQUIRE_DOCKER_AUTH"
  else
    log "Docker auth config not found; anonymous Docker Hub pulls may be rate limited"
  fi

  cleanup_docker_run_resources
}

collect_auto_prepull_images() {
  if [[ "$HARBOR_ENV" != "docker" || "$TB_AUTO_PREPULL_IMAGES" == "0" || "$TB_AUTO_PREPULL_IMAGES" == "false" ]]; then
    return 0
  fi

  "$PYTHON_BIN" - "$TB_TASK_CACHE_DIR" "$DATASET" "$TASK_NAMES" <<'PY'
import re
import sys
from pathlib import Path

cache_dir = Path(sys.argv[1]).expanduser()
dataset = sys.argv[2]
task_names = {
    item.strip()
    for item in sys.argv[3].split(",")
    if item.strip()
}
namespace = dataset.split("/", 1)[0] if "/" in dataset else dataset
search_root = cache_dir / namespace
if not search_root.exists():
    search_root = cache_dir
if not search_root.exists():
    raise SystemExit(0)

image_re = re.compile(r'^\s*docker_image\s*=\s*["\']([^"\']+)["\']\s*$')
images = set()
for path in search_root.rglob("task.toml"):
    task_name = path.parent.parent.name if len(path.parents) >= 2 else path.parent.name
    if task_names and task_name not in task_names and f"{namespace}/{task_name}" not in task_names:
        continue
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError:
        continue
    for line in lines:
        match = image_re.match(line)
        if match:
            images.add(match.group(1))
            break

for image in sorted(images):
    print(image)
PY
}

collect_prepull_images() {
  local images=()
  local old_ifs image trimmed
  if [[ -n "$TB_DOCKER_IMAGES_FILE" ]]; then
    [[ -f "$TB_DOCKER_IMAGES_FILE" ]] || die "TB_DOCKER_IMAGES_FILE does not exist: $TB_DOCKER_IMAGES_FILE"
    while IFS= read -r image; do
      image="${image%%#*}"
      trimmed="${image#"${image%%[![:space:]]*}"}"
      trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"
      [[ -n "$trimmed" ]] && images+=("$trimmed")
    done < "$TB_DOCKER_IMAGES_FILE"
  fi

  if [[ -n "$TB_PREPULL_IMAGES" ]]; then
    old_ifs="$IFS"
    IFS=','
    for image in $TB_PREPULL_IMAGES; do
      trimmed="${image#"${image%%[![:space:]]*}"}"
      trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"
      [[ -n "$trimmed" ]] && images+=("$trimmed")
    done
    IFS="$old_ifs"
  fi

  while IFS= read -r image; do
    [[ -n "$image" ]] && images+=("$image")
  done < <(collect_auto_prepull_images)

  if ((${#images[@]})); then
    printf '%s\n' "${images[@]}"
  fi
}

prepull_docker_images() {
  if [[ "$HARBOR_ENV" != "docker" || "$TB_DOCKER_PREPULL" == "0" || "$TB_DOCKER_PREPULL" == "false" ]]; then
    return 0
  fi

  local images_file pending_file image count skipped running pull_failed
  pull_failed=0
  images_file="$(mktemp)"
  pending_file="$(mktemp)"
  collect_prepull_images | sort -u > "$images_file"
  count="$(wc -l < "$images_file" | tr -d '[:space:]')"
  if [[ "$count" == "0" ]]; then
    rm -f "$images_file"
    rm -f "$pending_file"
    log "No Docker pre-pull images found; set TB_PREPULL_IMAGES/TB_DOCKER_IMAGES_FILE or warm Harbor's task cache for TB_AUTO_PREPULL_IMAGES"
    return 0
  fi

  skipped=0
  while IFS= read -r image; do
    if [[ "$TB_DOCKER_PULL_EXISTING" != "1" && "$TB_DOCKER_PULL_EXISTING" != "true" ]] && docker image inspect "$image" >/dev/null 2>&1; then
      skipped=$((skipped + 1))
      continue
    fi
    printf '%s\n' "$image" >> "$pending_file"
  done < "$images_file"
  rm -f "$images_file"

  count="$(wc -l < "$pending_file" | tr -d '[:space:]')"
  if [[ "$count" == "0" ]]; then
    rm -f "$pending_file"
    log "All configured Docker pre-pull images are already present locally"
    return 0
  fi

  if [[ "$skipped" -gt 0 ]]; then
    log "Skipping $skipped Docker images already present locally"
  fi
  log "Pre-pulling $count Docker images with parallelism=$TB_DOCKER_PULL_PARALLELISM retries=$TB_DOCKER_PULL_RETRIES"
  while IFS= read -r image; do
    (
      local attempt
      for attempt in $(seq 1 "$TB_DOCKER_PULL_RETRIES"); do
        docker pull "$image" && exit 0
        sleep "$attempt"
      done
      exit 1
    ) &
    while true; do
      running="$(jobs -rp | wc -l | tr -d '[:space:]')"
      [[ "$running" -lt "$TB_DOCKER_PULL_PARALLELISM" ]] && break
      wait -n || pull_failed=1
    done
  done < "$pending_file"
  rm -f "$pending_file"
  wait || pull_failed=1
  [[ "$pull_failed" == "0" ]] || die "one or more Docker pre-pulls failed"
}

configure_provider_env() {
  export GRATEFUL_PROVIDER="${GRATEFUL_PROVIDER:-openai}"
  export GRATEFUL_MODEL="${GRATEFUL_MODEL:-$(default_model_for_provider "$GRATEFUL_PROVIDER")}"
  export GRATEFUL_REASONING="${GRATEFUL_REASONING:-high}"
  export GRATEFUL_VERBOSITY="${GRATEFUL_VERBOSITY:-medium}"
  export GRATEFUL_MAX_TURNS="${GRATEFUL_MAX_TURNS:-150}"
  export GRATEFUL_MAX_TOKENS="${GRATEFUL_MAX_TOKENS:-8192}"
  export GRATEFUL_TOOL_ACCESS="${GRATEFUL_TOOL_ACCESS:-full}"
  export GRATEFUL_PERMISSION_MODE="${GRATEFUL_PERMISSION_MODE:-danger-full-access}"
  export GRATEFUL_TOOLS="${GRATEFUL_TOOLS:-true}"
  export GRATEFUL_WEB_TOOLS="${GRATEFUL_WEB_TOOLS:-false}"
  export GRATEFUL_MCP="${GRATEFUL_MCP:-false}"
  export GRATEFUL_HANDOFFS="${GRATEFUL_HANDOFFS:-false}"
  export GRATEFUL_SUBAGENTS="${GRATEFUL_SUBAGENTS:-false}"
  export GRATEFUL_APPROVAL="${GRATEFUL_APPROVAL:-false}"
  export GRATEFUL_GUARDRAILS="${GRATEFUL_GUARDRAILS:-false}"
  export GRATEFUL_RETRY="${GRATEFUL_RETRY:-true}"
  export GRATEFUL_ASYNC_BASH="${GRATEFUL_ASYNC_BASH:-false}"
  export GRATEFUL_COMPACTION="${GRATEFUL_COMPACTION:-true}"
  export GRATEFUL_FORCE_FINAL="${GRATEFUL_FORCE_FINAL:-true}"
  export GRATEFUL_FINAL_CHECK="${GRATEFUL_FINAL_CHECK:-true}"
  export GRATEFUL_EXIT_ZERO_ON_TIMEOUT="${GRATEFUL_EXIT_ZERO_ON_TIMEOUT:-true}"
  export GRATEFUL_TB_COMPLIANCE="${GRATEFUL_TB_COMPLIANCE:-true}"
  export GRATEFULAGENTS_ALLOW_PRIVATE_NETWORK_URLS="${GRATEFULAGENTS_ALLOW_PRIVATE_NETWORK_URLS:-false}"
  export GRATEFUL_BASH_DEFAULT_TIMEOUT_MS="${GRATEFUL_BASH_DEFAULT_TIMEOUT_MS:-120000}"
  export GRATEFUL_BASH_MAX_TIMEOUT_MS="${GRATEFUL_BASH_MAX_TIMEOUT_MS:-1800000}"
  export GRATEFUL_BASH_MAX_OUTPUT_BYTES="${GRATEFUL_BASH_MAX_OUTPUT_BYTES:-262144}"

  if [[ -z "${GRATEFUL_CA_BUNDLE:-}" && -n "${SSL_CERT_FILE:-}" ]]; then
    export GRATEFUL_CA_BUNDLE="$SSL_CERT_FILE"
  fi

  case "$(printf '%s' "$GRATEFUL_PROVIDER" | tr '[:upper:]' '[:lower:]')" in
    openai)
      local oauth_path="${OPENAI_OAUTH_AUTH_JSON_PATH:-$HOME/.codex/auth.json}"
      local auth_mode="${OPENAI_AUTH_MODE:-oauth}"
      auth_mode="$(printf '%s' "$auth_mode" | tr '[:upper:]' '[:lower:]' | tr '_' '-')"
      case "$auth_mode" in
        oauth)
          [[ -f "$oauth_path" ]] || die "OpenAI tests use OAuth by default; set OPENAI_OAUTH_AUTH_JSON_PATH or place auth.json at $HOME/.codex/auth.json"
          export OPENAI_AUTH_MODE="oauth"
          export OPENAI_OAUTH_AUTH_JSON_PATH="$oauth_path"
          export GRATEFUL_BASE_URL="${GRATEFUL_BASE_URL:-https://chatgpt.com/backend-api/codex}"
          ;;
        api-key)
          if [[ -z "${GRATEFUL_API_KEY:-}" && -n "${OPENAI_API_KEY:-}" ]]; then
            export GRATEFUL_API_KEY="$OPENAI_API_KEY"
          fi
          [[ -n "${GRATEFUL_API_KEY:-}" ]] || die "set OPENAI_API_KEY or GRATEFUL_API_KEY when OPENAI_AUTH_MODE=api-key"
          export OPENAI_AUTH_MODE="api-key"
          ;;
        *)
          die "unsupported OPENAI_AUTH_MODE=$OPENAI_AUTH_MODE; use oauth or api-key"
          ;;
      esac
      ;;
    anthropic)
      if [[ -z "${GRATEFUL_API_KEY:-}" && -n "${ANTHROPIC_API_KEY:-}" ]]; then
        export GRATEFUL_API_KEY="$ANTHROPIC_API_KEY"
      fi
      [[ -n "${GRATEFUL_API_KEY:-}" ]] || die "set ANTHROPIC_API_KEY or GRATEFUL_API_KEY"
      ;;
    openrouter)
      if [[ -z "${GRATEFUL_API_KEY:-}" && -n "${OPENROUTER_API_KEY:-}" ]]; then
        export GRATEFUL_API_KEY="$OPENROUTER_API_KEY"
      fi
      [[ -n "${GRATEFUL_API_KEY:-}" ]] || die "set OPENROUTER_API_KEY or GRATEFUL_API_KEY"
      ;;
    local)
      export GRATEFUL_API_KEY="${GRATEFUL_API_KEY:-local-key}"
      ;;
    *)
      [[ -n "${GRATEFUL_API_KEY:-}" ]] || die "set GRATEFUL_API_KEY for provider $GRATEFUL_PROVIDER"
      ;;
  esac
}

build_harness() {
  local goarch_value binary
  goarch_value="${GOARCH_VALUE:-${GOARCH:-$(arch_from_uname)}}"
  binary="$ROOT/bin/grateful-agent-run-linux-${goarch_value}"
  mkdir -p "$ROOT/bin"

  log "Building grateful-agent-run for linux/${goarch_value}"
  (
    cd "$ROOT"
    GOFLAGS="${GOFLAGS:--mod=mod}" CGO_ENABLED=0 GOOS=linux GOARCH="$goarch_value" \
      go build -o "$binary" ./cmd/grateful-agent-run
  )

  export GRATEFUL_AGENT_BINARY="$binary"
}

append_task_names() {
  local raw="$1"
  if [[ -z "$raw" ]]; then
    return 0
  fi

  local old_ifs name trimmed
  old_ifs="$IFS"
  IFS=','
  for name in $raw; do
    trimmed="${name#"${name%%[![:space:]]*}"}"
    trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"
    if [[ -n "$trimmed" ]]; then
      if [[ "$trimmed" != */* && "$DATASET" == */* ]]; then
        trimmed="${DATASET%%/*}/$trimmed"
      fi
      HARBOR_ARGS+=(--include-task-name "$trimmed")
    fi
  done
  IFS="$old_ifs"
}

print_summary() {
  local job_dir="$ROOT/$OUTPUT_PATH/$RUN_ID"
  local result="$job_dir/result.json"
  if [[ ! -f "$result" ]]; then
    log "No Harbor result.json found at $result"
    return
  fi

  "$VENV_DIR/bin/python" - "$job_dir" <<'PY'
import json
import sys
from pathlib import Path

job_dir = Path(sys.argv[1])
top = json.loads((job_dir / "result.json").read_text())

def number(value):
    try:
        return float(value)
    except (TypeError, ValueError):
        return None

score = None
for key in ("accuracy", "score", "reward", "mean_reward"):
    score = number(top.get(key))
    if score is not None:
        break

pass_at_k = {}
errors = None
trial_rewards = []
eval_stats = None
for candidate in (top.get("stats") or {}).get("evals", {}).values():
    eval_stats = candidate
    break
if eval_stats:
    for metric in eval_stats.get("metrics", []):
        score = number(metric.get("mean"))
        if score is not None:
            break
    pass_at_k = eval_stats.get("pass_at_k") or {}
    reward_ids = (eval_stats.get("reward_stats") or {}).get("reward") or {}
    if reward_ids:
        trial_rewards = [1.0] * len(reward_ids.get("1.0", []))
        trial_rewards.extend([0.0] * len(reward_ids.get("0.0", [])))
    errors = number((top.get("stats") or {}).get("n_errored_trials"))

if not trial_rewards:
    for path in sorted(job_dir.glob("*/result.json")):
        try:
            data = json.loads(path.read_text())
        except Exception:
            continue
        reward = ((data.get("verifier_result") or {}).get("rewards") or {}).get("reward")
        for value in (reward, data.get("reward"), data.get("score"), data.get("accuracy")):
            parsed = number(value)
            if parsed is not None:
                trial_rewards.append(parsed)
                break

if score is None and trial_rewards:
    score = sum(trial_rewards) / len(trial_rewards)

score_text = "n/a" if score is None else f"{score * 100:.2f}%"
passed = sum(1 for reward in trial_rewards if reward >= 1.0)
failed = len(trial_rewards) - passed
extra = []
if pass_at_k:
    for key in sorted(pass_at_k, key=lambda item: int(item)):
        value = number(pass_at_k[key])
        if value is not None:
            extra.append(f"pass@{key}={value * 100:.2f}%")
if errors is not None:
    extra.append(f"errors={int(errors)}")
print(f"score={score_text} trials={len(trial_rewards)} passed={passed} failed={failed}" + ((" " + " ".join(extra)) if extra else ""))
PY
}

ensure_harbor_host
preflight_docker
ensure_python_venv
if [[ "$TB_PREPULL_ONLY" == "1" || "$TB_PREPULL_ONLY" == "true" ]]; then
  prepull_docker_images
  log "Docker image pre-pull completed"
  exit 0
fi
configure_provider_env
ensure_go
build_harness
prepull_docker_images

export PYTHONPATH="$ROOT${PYTHONPATH:+:$PYTHONPATH}"
export BUILDKIT_PROGRESS="${BUILDKIT_PROGRESS:-plain}"
export COMPOSE_PROGRESS="${COMPOSE_PROGRESS:-plain}"
export COMPOSE_STATUS_STDOUT="${COMPOSE_STATUS_STDOUT:-1}"

HARBOR_ARGS=(
  "$HARBOR_BIN" run
  --dataset "$DATASET"
  --agent-import-path eval.terminal_bench.harbor_grateful_agent:GratefulHarborAgent
  --model "$GRATEFUL_MODEL"
  --env "$HARBOR_ENV"
  --n-concurrent "$TB_N_CONCURRENT"
  -k "$TB_N_ATTEMPTS"
  --jobs-dir "$OUTPUT_PATH"
  --job-name "$RUN_ID"
)

if [[ "$TB_MAX_RETRIES" != "0" ]]; then
  HARBOR_ARGS+=(--max-retries "$TB_MAX_RETRIES")
fi
if [[ -n "$TB_AGENT_TIMEOUT_MULTIPLIER" ]]; then
  HARBOR_ARGS+=(--agent-timeout-multiplier "$TB_AGENT_TIMEOUT_MULTIPLIER")
fi
if [[ -n "$TB_VERIFIER_TIMEOUT_MULTIPLIER" ]]; then
  HARBOR_ARGS+=(--verifier-timeout-multiplier "$TB_VERIFIER_TIMEOUT_MULTIPLIER")
fi
if [[ -n "$TB_ENVIRONMENT_BUILD_TIMEOUT_MULTIPLIER" ]]; then
  HARBOR_ARGS+=(--environment-build-timeout-multiplier "$TB_ENVIRONMENT_BUILD_TIMEOUT_MULTIPLIER")
fi

if [[ "${HARBOR_DEBUG:-0}" == "1" || "${HARBOR_DEBUG:-}" == "true" ]]; then
  HARBOR_ARGS+=(--debug)
fi

append_task_names "$TASK_NAMES"
HARBOR_ARGS+=("$@")

OUTPUT_DIR="$ROOT/$OUTPUT_PATH"
RUN_DIR="$OUTPUT_DIR/$RUN_ID"
RUN_LOG="$OUTPUT_DIR/${RUN_ID}.harbor-run.log"
mkdir -p "$OUTPUT_DIR"

if [[ "$RAW_RUN_ID" != "$RUN_ID" ]]; then
  log "Normalized RUN_ID from $RAW_RUN_ID to $RUN_ID for Harbor job naming"
fi
log "Running Terminal-Bench dataset=$DATASET job=$RUN_ID model=$GRATEFUL_MODEL n=$TB_N_CONCURRENT k=$TB_N_ATTEMPTS env=$HARBOR_ENV profile=$TB_PROFILE retries=$TB_MAX_RETRIES"
log "Streaming full output to $RUN_LOG"
set +e
"${HARBOR_ARGS[@]}" 2>&1 | tee "$RUN_LOG"
harbor_status=${PIPESTATUS[0]}
set -e
if [[ "$harbor_status" -ne 0 ]]; then
  if [[ -d "$RUN_DIR" ]]; then
    cp "$RUN_LOG" "$RUN_DIR/harbor-run.log"
  fi
  die "harbor run failed with exit code $harbor_status; inspect $RUN_LOG"
fi
if [[ -d "$RUN_DIR" ]]; then
  cp "$RUN_LOG" "$RUN_DIR/harbor-run.log"
fi
print_summary
