import os
import shlex
from pathlib import Path

from terminal_bench.agents.base_agent import AgentResult, BaseAgent
from terminal_bench.terminal.models import TerminalCommand
from terminal_bench.terminal.tmux_session import TmuxSession


class GratefulAgent(BaseAgent):
    """Terminal-Bench adapter for a prebuilt local grateful-agent-run binary."""

    CONTAINER_BINARY_PATH = "/usr/local/bin/grateful-agent-run"
    CONTAINER_AUTH_PATH = "/tmp/grateful-openai-auth.json"
    CONTAINER_ENV_PATH = "/tmp/grateful-agent-env.sh"

    @staticmethod
    def name() -> str:
        return "grateful-agent"

    def __init__(self, model_name: str | None = None, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._model_name = model_name or os.environ.get("GRATEFUL_MODEL", "gpt-5.5")
        self._binary_path = Path(
            os.environ.get("GRATEFUL_AGENT_BINARY", "bin/grateful-agent-run")
        ).resolve()

    def perform_task(
        self,
        instruction: str,
        session: TmuxSession,
        logging_dir: Path | None = None,
    ) -> AgentResult:
        if not self._binary_path.exists():
            raise FileNotFoundError(
                f"build the harness binary first: go build -o {self._binary_path} ./cmd/grateful-agent-run"
            )

        session.copy_to_container(
            self._binary_path,
            container_dir="/usr/local/bin",
            container_filename="grateful-agent-run",
        )
        session.container.exec_run(["chmod", "+x", self.CONTAINER_BINARY_PATH])

        env = self._container_env()
        auth_path = self._host_oauth_path()
        if auth_path is not None:
            session.copy_to_container(
                auth_path,
                container_dir="/tmp",
                container_filename=Path(self.CONTAINER_AUTH_PATH).name,
            )
            env["OPENAI_AUTH_MODE"] = "oauth"
            env["OPENAI_OAUTH_AUTH_JSON_PATH"] = self.CONTAINER_AUTH_PATH
            env.setdefault(
                "GRATEFUL_BASE_URL", "https://chatgpt.com/backend-api/codex"
            )
            provider = os.environ.get("GRATEFUL_PROVIDER", "openai").strip().lower()
            if provider == "openai":
                env.pop("GRATEFUL_API_KEY", None)
                env.pop("OPENAI_API_KEY", None)

        env_script = "\n".join(
            f"export {key}={shlex.quote(value)}" for key, value in env.items()
        )
        session.container.exec_run(
            [
                "sh",
                "-c",
                "cat > "
                + shlex.quote(self.CONTAINER_ENV_PATH)
                + " <<'EOF'\n"
                + env_script
                + "\nEOF\nchmod 600 "
                + shlex.quote(self.CONTAINER_ENV_PATH),
            ]
        )
        session.send_keys(
            ["source " + self.CONTAINER_ENV_PATH, "Enter"],
            block=True,
            max_timeout_sec=float("inf"),
        )

        rendered_instruction = self._render_instruction(instruction)
        prompt = shlex.quote(rendered_instruction)
        model = shlex.quote(self._model_name)
        max_turns = shlex.quote(os.environ.get("GRATEFUL_MAX_TURNS", "150"))
        max_tokens = shlex.quote(os.environ.get("GRATEFUL_MAX_TOKENS", "8192"))
        permission_mode = shlex.quote(
            os.environ.get("GRATEFUL_PERMISSION_MODE", "danger-full-access")
        )
        tool_access = shlex.quote(os.environ.get("GRATEFUL_TOOL_ACCESS", "full"))
        trace_root = shlex.quote(
            os.environ.get("GRATEFUL_TRACE_ROOT", "/tmp/grateful-agent-traces")
        )
        command = (
            "printf '%s' "
            + prompt
            + " | "
            + self.CONTAINER_BINARY_PATH
            + " --stdin"
            + " --provider "
            + shlex.quote(os.environ.get("GRATEFUL_PROVIDER", "openai"))
            + " --model "
            + model
            + " --workdir /app"
            + " --max-turns "
            + max_turns
            + " --max-tokens "
            + max_tokens
            + " --permission-mode "
            + permission_mode
            + " --tool-access "
            + tool_access
            + " --output json"
            + " --trace-root "
            + trace_root
            + " --candidate-id "
            + shlex.quote("grateful-agent/" + self._model_name)
        )
        session.send_command(
            TerminalCommand(
                command=command,
                max_timeout_sec=float("inf"),
                block=True,
            )
        )
        return AgentResult()

    def _container_env(self) -> dict[str, str]:
        keys = [
            "GRATEFUL_API_KEY",
            "OPENAI_API_KEY",
            "OPENAI_AUTH_MODE",
            "OPENAI_API_MODE",
            "OPENAI_BASE_URL",
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
            "GRATEFUL_COMPACTION",
            "GRATEFUL_FORCE_FINAL",
            "GRATEFUL_TB_COMPLIANCE",
            "GRATEFULAGENTS_ALLOW_PRIVATE_NETWORK_URLS",
        ]
        env = {key: os.environ[key] for key in keys if os.environ.get(key)}
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
        env.setdefault("GRATEFUL_COMPACTION", "true")
        env.setdefault("GRATEFUL_FORCE_FINAL", "true")
        env.setdefault("GRATEFUL_TB_COMPLIANCE", "true")
        env.setdefault("GRATEFULAGENTS_ALLOW_PRIVATE_NETWORK_URLS", "false")
        env.setdefault("GRATEFUL_AGENT_INSTRUCTIONS", self._default_instructions())
        env.setdefault("GRATEFUL_API_KEY", self._provider_api_key())
        if not env["GRATEFUL_API_KEY"]:
            env.pop("GRATEFUL_API_KEY")
        return env

    def _default_instructions(self) -> str:
        return (
            "You are a headless Grateful Agents SDK harness agent solving a Terminal-Bench task. "
            "Complete the task autonomously in /app using the available tools. "
            "Do not access Terminal-Bench websites, leaderboards, task repositories, GitHub searches, or raw task sources. "
            "Verify the expected deliverable before finishing and report the outcome concisely."
        )

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
