from __future__ import annotations

import json
import os
import shlex
import tempfile
import tomllib
from pathlib import Path
from typing import Any

try:
    from harbor.agents.installed import BaseInstalledAgent
except ImportError:  # pragma: no cover - depends on Harbor packaging version.
    from harbor.agents.installed.base import BaseInstalledAgent


class GratefulHarborAgent(BaseInstalledAgent):
    """Harbor installed-agent adapter for Terminal-Bench 2.1."""

    CONTAINER_BINARY_PATH = "/usr/local/bin/grateful-agent-run"
    CONTAINER_AUTH_PATH = "/tmp/grateful-openai-auth.json"
    OUTPUT_JSON_PATH = "/logs/agent/grateful-agent-output.json"
    STDERR_PATH = "/logs/agent/grateful-agent-stderr.log"
    EVENT_LOG_PATH = "/logs/agent/grateful-agent-events.jsonl"
    TRACE_ROOT = "/logs/agent/traces"
    PROMPT_PATH = "/tmp/grateful-terminal-bench-instruction.txt"
    CA_BUNDLE_PATH = "/tmp/grateful-ca-certificates.crt"
    DEFAULT_TIMEOUT_BUFFER_SEC = 15.0

    @staticmethod
    def name() -> str:
        return "grateful-agent"

    def __init__(self, model_name: str | None = None, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._model_name = model_name or os.environ.get("GRATEFUL_MODEL", "gpt-5.5")
        self._binary_path = Path(
            os.environ.get("GRATEFUL_AGENT_BINARY", "bin/grateful-agent-run")
        ).expanduser().resolve()
        self._oauth_uploaded = False
        self._ca_uploaded = False

    async def install(self, environment):
        if not self._binary_path.exists():
            raise FileNotFoundError(
                f"build the harness binary first: go build -o {self._binary_path} ./cmd/grateful-agent-run"
            )

        await self._upload_file(
            environment, self._binary_path, self.CONTAINER_BINARY_PATH
        )
        await self.exec_as_root(
            environment,
            "chmod 0755 "
            + shlex.quote(self.CONTAINER_BINARY_PATH)
            + " && mkdir -p /logs/agent",
        )

        auth_path = self._host_oauth_path()
        if auth_path is not None:
            await self._upload_file(environment, auth_path, self.CONTAINER_AUTH_PATH)
            await self.exec_as_root(
                environment,
                "chmod 0600 " + shlex.quote(self.CONTAINER_AUTH_PATH),
            )
            self._oauth_uploaded = True

        ca_path = self._host_ca_bundle_path()
        if ca_path is not None:
            await self._upload_file(environment, ca_path, self.CA_BUNDLE_PATH)
            await self.exec_as_root(
                environment,
                "chmod 0644 " + shlex.quote(self.CA_BUNDLE_PATH),
            )
            self._ca_uploaded = True

    async def run(self, environment, instruction: str, context):
        await self._upload_instruction(environment, instruction)
        env = self._container_env()
        if self._oauth_uploaded:
            env["OPENAI_AUTH_MODE"] = "oauth"
            env["OPENAI_OAUTH_AUTH_JSON_PATH"] = self.CONTAINER_AUTH_PATH
            env.setdefault("GRATEFUL_BASE_URL", "https://chatgpt.com/backend-api/codex")
            if env.get("GRATEFUL_PROVIDER", "openai").strip().lower() == "openai":
                env.pop("GRATEFUL_API_KEY", None)
                env.pop("OPENAI_API_KEY", None)

        command = self._command(self._sdk_timeout_sec())
        await self.exec_as_agent(environment, command, env=env)

    def populate_context_post_run(self, context):
        output_path = Path(self.logs_dir) / Path(self.OUTPUT_JSON_PATH).name
        if not output_path.exists():
            return

        try:
            data = json.loads(output_path.read_text())
        except (json.JSONDecodeError, OSError):
            return

        usage = data.get("usage") or {}
        self._set_context_value(context, "total_input_tokens", usage.get("input_tokens"))
        self._set_context_value(context, "total_output_tokens", usage.get("output_tokens"))
        total = usage.get("total_tokens")
        if total is None:
            total = (usage.get("input_tokens") or 0) + (usage.get("output_tokens") or 0)
        self._set_context_value(context, "total_tokens", total)

        metrics = getattr(context, "metrics", None)
        if isinstance(metrics, dict):
            metrics["grateful_duration_sec"] = (data.get("metrics") or {}).get(
                "duration_sec"
            )
            metrics["grateful_tool_calls"] = (data.get("metrics") or {}).get(
                "tool_calls"
            )
            metrics["grateful_turns_used"] = (data.get("metrics") or {}).get(
                "turns_used"
            )

    async def _upload_file(self, environment, source: Path, destination: str):
        try:
            await environment.upload_file(str(source), destination)
        except TypeError:
            await environment.upload_file(
                source,
                container_dir=str(Path(destination).parent),
                container_filename=Path(destination).name,
            )

    async def _upload_instruction(self, environment, instruction: str) -> None:
        tmp_path: Path | None = None
        try:
            with tempfile.NamedTemporaryFile(
                mode="w", encoding="utf-8", delete=False
            ) as tmp:
                tmp.write(instruction)
                tmp_path = Path(tmp.name)
            await self._upload_file(environment, tmp_path, self.PROMPT_PATH)
            await self.exec_as_root(
                environment,
                "chmod 0644 " + shlex.quote(self.PROMPT_PATH),
            )
        finally:
            if tmp_path is not None:
                try:
                    tmp_path.unlink()
                except OSError:
                    pass

    def _command(self, timeout_sec: float | None = None) -> str:
        provider = os.environ.get("GRATEFUL_PROVIDER", "openai")
        max_turns = os.environ.get("GRATEFUL_MAX_TURNS", "150")
        max_tokens = os.environ.get("GRATEFUL_MAX_TOKENS", "8192")
        permission_mode = os.environ.get("GRATEFUL_PERMISSION_MODE", "danger-full-access")
        tool_access = os.environ.get("GRATEFUL_TOOL_ACCESS", "full")
        run_id = os.environ.get("RUN_ID", "terminal-bench")
        candidate_id = os.environ.get(
            "GRATEFUL_CANDIDATE_ID", "grateful-agent/" + self._model_name
        )

        args = [
            self.CONTAINER_BINARY_PATH,
            "--prompt-file",
            self.PROMPT_PATH,
            "--provider",
            provider,
            "--model",
            self._model_name,
            "--workdir",
            "/app",
            "--max-turns",
            max_turns,
            "--max-tokens",
            max_tokens,
            "--permission-mode",
            permission_mode,
            "--tool-access",
            tool_access,
            "--output",
            "json",
            "--event-log",
            self.EVENT_LOG_PATH,
            "--trace-root",
            self.TRACE_ROOT,
            "--run-id",
            run_id,
            "--candidate-id",
            candidate_id,
        ]
        if timeout_sec is not None and timeout_sec > 0:
            args.extend(["--timeout", f"{int(timeout_sec)}s"])
        argv = " ".join(shlex.quote(arg) for arg in args)
        return (
            "mkdir -p /logs/agent && "
            + argv
            + " > "
            + shlex.quote(self.OUTPUT_JSON_PATH)
            + " 2> "
            + shlex.quote(self.STDERR_PATH)
            + "; status=$?; "
            + "if [ -f "
            + shlex.quote(self.OUTPUT_JSON_PATH)
            + " ]; then cat "
            + shlex.quote(self.OUTPUT_JSON_PATH)
            + "; fi; "
            + "exit $status"
        )

    def _container_env(self) -> dict[str, str]:
        keys = [
            "GRATEFUL_API_KEY",
            "OPENAI_API_KEY",
            "OPENAI_AUTH_MODE",
            "OPENAI_API_MODE",
            "OPENAI_OAUTH_ACCOUNT_ID",
            "OPENAI_OAUTH_ACCOUNT_ID_PATH",
            "ANTHROPIC_API_KEY",
            "ANTHROPIC_BASE_URL",
            "OPENROUTER_API_KEY",
            "GRATEFUL_PROVIDER",
            "GRATEFUL_BASE_URL",
            "GRATEFUL_AGENT_INSTRUCTIONS",
            "GRATEFUL_REASONING",
            "GRATEFUL_VERBOSITY",
            "GRATEFUL_MAX_TURNS",
            "GRATEFUL_MAX_TOKENS",
            "GRATEFUL_TOOL_ACCESS",
            "GRATEFUL_PERMISSION_MODE",
            "GRATEFUL_TOOLS",
            "GRATEFUL_WEB_TOOLS",
            "GRATEFUL_MCP",
            "GRATEFUL_HANDOFFS",
            "GRATEFUL_SUBAGENTS",
            "GRATEFUL_APPROVAL",
            "GRATEFUL_GUARDRAILS",
            "GRATEFUL_RETRY",
            "GRATEFUL_ASYNC_BASH",
            "GRATEFUL_COMPACTION",
            "GRATEFUL_FORCE_FINAL",
            "GRATEFUL_FINAL_CHECK",
            "GRATEFUL_EXIT_ZERO_ON_TIMEOUT",
            "GRATEFUL_TB_COMPLIANCE",
            "GRATEFULAGENTS_ALLOW_PRIVATE_NETWORK_URLS",
            "GRATEFUL_CA_BUNDLE",
            "SSL_CERT_FILE",
            "REQUESTS_CA_BUNDLE",
            "GRATEFUL_BASH_DEFAULT_TIMEOUT_MS",
            "GRATEFUL_BASH_MAX_TIMEOUT_MS",
            "GRATEFUL_BASH_MAX_OUTPUT_BYTES",
        ]
        env = {key: os.environ[key] for key in keys if os.environ.get(key)}
        env.setdefault("GRATEFUL_PROVIDER", os.environ.get("GRATEFUL_PROVIDER", "openai"))
        env.setdefault("GRATEFUL_REASONING", "high")
        env.setdefault("GRATEFUL_VERBOSITY", "medium")
        env.setdefault("GRATEFUL_MAX_TURNS", "150")
        env.setdefault("GRATEFUL_MAX_TOKENS", "8192")
        env.setdefault("GRATEFUL_TOOL_ACCESS", "full")
        env.setdefault("GRATEFUL_PERMISSION_MODE", "danger-full-access")
        env.setdefault("GRATEFUL_TOOLS", "true")
        env.setdefault("GRATEFUL_WEB_TOOLS", "false")
        env.setdefault("GRATEFUL_MCP", "false")
        env.setdefault("GRATEFUL_HANDOFFS", "false")
        env.setdefault("GRATEFUL_SUBAGENTS", "false")
        env.setdefault("GRATEFUL_APPROVAL", "false")
        env.setdefault("GRATEFUL_GUARDRAILS", "false")
        env.setdefault("GRATEFUL_RETRY", "true")
        env.setdefault("GRATEFUL_ASYNC_BASH", "false")
        env.setdefault("GRATEFUL_COMPACTION", "true")
        env.setdefault("GRATEFUL_FORCE_FINAL", "true")
        env.setdefault("GRATEFUL_FINAL_CHECK", "true")
        env.setdefault("GRATEFUL_EXIT_ZERO_ON_TIMEOUT", "true")
        env.setdefault("GRATEFUL_TB_COMPLIANCE", "true")
        env.setdefault("GRATEFULAGENTS_ALLOW_PRIVATE_NETWORK_URLS", "false")
        env.setdefault("GRATEFUL_BASH_DEFAULT_TIMEOUT_MS", "120000")
        env.setdefault("GRATEFUL_BASH_MAX_TIMEOUT_MS", "1800000")
        env.setdefault("GRATEFUL_BASH_MAX_OUTPUT_BYTES", "262144")
        env.setdefault("GRATEFUL_AGENT_INSTRUCTIONS", self._default_instructions())
        if self._ca_uploaded:
            env.setdefault("GRATEFUL_CA_BUNDLE", self.CA_BUNDLE_PATH)
            env.setdefault("SSL_CERT_FILE", self.CA_BUNDLE_PATH)
            env.setdefault("REQUESTS_CA_BUNDLE", self.CA_BUNDLE_PATH)
        env.setdefault("GRATEFUL_API_KEY", self._provider_api_key())
        if not env["GRATEFUL_API_KEY"]:
            env.pop("GRATEFUL_API_KEY")
        return env

    def _sdk_timeout_sec(self) -> float | None:
        configured = os.environ.get("GRATEFUL_RUN_TIMEOUT_SEC") or os.environ.get(
            "GRATEFUL_RUN_TIMEOUT_SECONDS"
        )
        if configured:
            try:
                return max(1.0, float(configured))
            except ValueError:
                pass

        timeout = self._task_agent_timeout_sec()
        if timeout is None:
            return None
        buffer = self.DEFAULT_TIMEOUT_BUFFER_SEC
        configured_buffer = os.environ.get("GRATEFUL_TIMEOUT_BUFFER_SEC")
        if configured_buffer:
            try:
                buffer = max(0.0, float(configured_buffer))
            except ValueError:
                pass
        return max(1.0, timeout - buffer)

    def _task_agent_timeout_sec(self) -> float | None:
        task_name = self._trial_task_name()
        if not task_name:
            return None
        cache_root = Path(
            os.environ.get(
                "TB_TASK_CACHE_DIR", str(Path.home() / ".cache/harbor/tasks/packages")
            )
        ).expanduser()
        dataset = os.environ.get("TB_DATASET") or os.environ.get(
            "DATASET", "terminal-bench/terminal-bench-2-1"
        )
        namespace = dataset.split("/", 1)[0] if "/" in dataset else dataset
        search_root = cache_root / namespace
        if not search_root.exists():
            search_root = cache_root
        task_root = search_root / task_name
        if not task_root.exists():
            return None
        for path in sorted(task_root.glob("*/task.toml")):
            try:
                data = tomllib.loads(path.read_text(encoding="utf-8"))
            except (OSError, tomllib.TOMLDecodeError):
                continue
            timeout = (data.get("agent") or {}).get("timeout_sec")
            if isinstance(timeout, (int, float)):
                return float(timeout)
        return None

    def _trial_task_name(self) -> str | None:
        try:
            trial_name = Path(self.logs_dir).parent.name
        except Exception:
            return None
        if "__" not in trial_name:
            return None
        task_name, _ = trial_name.split("__", 1)
        return task_name or None

    def _provider_api_key(self) -> str:
        provider = os.environ.get("GRATEFUL_PROVIDER", "openai").strip().lower()
        auth_mode = os.environ.get("OPENAI_AUTH_MODE", "").strip().lower()
        if provider == "openai" and auth_mode == "oauth":
            return ""
        provider_keys = {
            "openai": ["OPENAI_API_KEY"],
            "anthropic": ["ANTHROPIC_API_KEY"],
            "openrouter": ["OPENROUTER_API_KEY"],
        }
        for key in provider_keys.get(provider, []):
            if os.environ.get(key):
                return os.environ[key]
        return ""

    def _host_oauth_path(self) -> Path | None:
        configured = os.environ.get("OPENAI_OAUTH_AUTH_JSON_PATH")
        candidates = [Path(configured).expanduser()] if configured else []
        candidates.append(Path.home() / ".codex" / "auth.json")
        for candidate in candidates:
            if candidate.exists():
                return candidate.resolve()
        return None

    def _host_ca_bundle_path(self) -> Path | None:
        candidates: list[Path] = []
        for value in (
            os.environ.get("GRATEFUL_CA_BUNDLE"),
            os.environ.get("SSL_CERT_FILE"),
            os.environ.get("REQUESTS_CA_BUNDLE"),
        ):
            if value:
                candidates.append(Path(value).expanduser())
        candidates.extend(
            Path(path)
            for path in (
                "/etc/ssl/certs/ca-certificates.crt",
                "/etc/pki/tls/certs/ca-bundle.crt",
                "/etc/ssl/ca-bundle.pem",
                "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
            )
        )
        for candidate in candidates:
            if candidate.exists() and candidate.is_file():
                return candidate.resolve()
        return None

    def _default_instructions(self) -> str:
        return (
            "You are a headless Grateful Agents SDK harness agent solving a Terminal-Bench task. "
            "Complete the task autonomously in /app using the available tools. "
            "Do not access Terminal-Bench websites, leaderboards, task repositories, GitHub searches, or raw task sources. "
            "Redirect verbose install, build, training, and test output to log files and inspect head/tail snippets instead of streaming huge logs. "
            "If a convenience command such as file, xxd, gdb, python3, or /usr/bin/time is missing, recover with POSIX shell or available Python alternatives. "
            "Verify the expected deliverable before finishing, keep needed services running, and report the outcome concisely."
        )

    def _set_context_value(self, context: Any, name: str, value: Any) -> None:
        if value is None:
            return
        try:
            setattr(context, name, value)
        except Exception:
            return
