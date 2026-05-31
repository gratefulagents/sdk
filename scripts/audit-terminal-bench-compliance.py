#!/usr/bin/env python3
from __future__ import annotations

import json
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable


@dataclass(frozen=True)
class Issue:
    kind: str
    path: Path
    reason: str
    tool: str = ""
    snippet: str = ""


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: audit-terminal-bench-compliance.py jobs/<run-id>", file=sys.stderr)
        return 2

    job_dir = Path(sys.argv[1])
    if not job_dir.exists():
        print(f"missing job dir: {job_dir}", file=sys.stderr)
        return 2

    issues = list(audit_job(job_dir))
    print(f"job={job_dir}")
    print(f"issues={len(issues)}")
    counts: dict[str, int] = {}
    for issue in issues:
        counts[issue.kind] = counts.get(issue.kind, 0) + 1
    for kind, count in sorted(counts.items()):
        print(f"{kind}={count}")

    if issues:
        print("\nFirst issues:")
        for issue in issues[:40]:
            rel = issue.path.relative_to(job_dir) if issue.path.is_relative_to(job_dir) else issue.path
            tool = f" tool={issue.tool}" if issue.tool else ""
            snippet = f" snippet={issue.snippet}" if issue.snippet else ""
            print(f"- {issue.kind}: {rel}{tool} reason={issue.reason}{snippet}")
        if len(issues) > 40:
            print(f"... {len(issues) - 40} more")
        return 1

    print("status=pass")
    return 0


def audit_job(job_dir: Path) -> Iterable[Issue]:
    yield from audit_configs(job_dir)
    yield from audit_trace_metadata(job_dir)
    yield from audit_tool_call_traces(job_dir)
    yield from audit_output_json(job_dir)


def audit_configs(job_dir: Path) -> Iterable[Issue]:
    for path in sorted(job_dir.glob("*/config.json")):
        data = read_json(path)
        if not isinstance(data, dict):
            continue
        for key in ("timeout_multiplier",):
            value = data.get(key)
            if value not in (None, 1, 1.0):
                yield Issue("timeout_override", path, f"{key}={value!r}")
        for key in (
            "agent_timeout_multiplier",
            "verifier_timeout_multiplier",
            "agent_setup_timeout_multiplier",
            "environment_build_timeout_multiplier",
        ):
            value = data.get(key)
            if value is not None:
                yield Issue("timeout_override", path, f"{key}={value!r}")

        agent = data.get("agent") or {}
        if isinstance(agent, dict):
            for key in ("override_timeout_sec", "override_setup_timeout_sec", "max_timeout_sec"):
                value = agent.get(key)
                if value is not None:
                    yield Issue("timeout_override", path, f"agent.{key}={value!r}")

        verifier = data.get("verifier") or {}
        if isinstance(verifier, dict):
            for key in ("override_timeout_sec", "max_timeout_sec"):
                value = verifier.get(key)
                if value is not None:
                    yield Issue("timeout_override", path, f"verifier.{key}={value!r}")

        environment = data.get("environment") or {}
        if isinstance(environment, dict):
            for key in ("override_cpus", "override_memory_mb", "override_storage_mb", "override_gpus"):
                value = environment.get(key)
                if value is not None:
                    yield Issue("resource_override", path, f"environment.{key}={value!r}")


def audit_trace_metadata(job_dir: Path) -> Iterable[Issue]:
    for path in sorted(job_dir.glob("*/agent/traces/**/metadata.json")):
        data = read_json(path)
        if not isinstance(data, dict):
            continue
        tools = data.get("tools") or []
        if "WebFetch" in tools:
            yield Issue("webfetch_advertised", path, "WebFetch present in SDK tool list")


def audit_tool_call_traces(job_dir: Path) -> Iterable[Issue]:
    seen: set[tuple[str, str, str, str]] = set()
    for path in sorted(job_dir.glob("*/agent/traces/**/tool_calls.jsonl")):
        for obj in iter_jsonl(path):
            if not isinstance(obj, dict) or obj.get("type") != "tool_start":
                continue
            tool = str(obj.get("tool") or "")
            input_obj = obj.get("input")
            parts = [json_text(input_obj)]
            if obj.get("bash_command"):
                parts.append(str(obj["bash_command"]))
            text = "\n".join(parts)
            yield from tool_input_issues(path, tool, text, seen)


def audit_output_json(job_dir: Path) -> Iterable[Issue]:
    seen: set[tuple[str, str, str, str]] = set()
    for path in sorted(job_dir.glob("*/agent/grateful-agent-output.json")):
        data = read_json(path)
        if not isinstance(data, dict):
            continue
        calls = data.get("tool_calls") or []
        if not isinstance(calls, list):
            continue
        for call in calls:
            if not isinstance(call, dict):
                continue
            tool = str(call.get("name") or "")
            text = json_text(call.get("input"))
            yield from tool_input_issues(path, tool, text, seen)


def tool_input_issues(
    path: Path, tool: str, text: str, seen: set[tuple[str, str, str, str]]
) -> Iterable[Issue]:
    if tool == "WebFetch":
        key = (str(path), tool, "WebFetch call", snippet(text))
        if key not in seen:
            seen.add(key)
            yield Issue("webfetch_call", path, "WebFetch call", tool, snippet(text))

    reason = forbidden_lookup(text)
    if reason:
        key = (str(path), tool, reason, snippet(text))
        if key not in seen:
            seen.add(key)
            yield Issue("forbidden_lookup", path, reason, tool, snippet(text))


def forbidden_lookup(input_text: str) -> str:
    text = normalize(input_text)
    for marker in (
        "terminal-bench.org",
        "terminalbench.org",
        "terminal-bench.github.io",
        "terminalbench.github.io",
        "github.com/terminal-bench",
        "github.com/terminalbench",
        "raw.githubusercontent.com/terminal-bench",
        "raw.githubusercontent.com/terminalbench",
        "api.github.com/repos/terminal-bench",
        "api.github.com/repos/terminalbench",
        "harborframework/terminal-bench",
        "terminal-bench-2-leaderboard",
    ):
        if marker in text:
            return marker
    if "api.github.com/search" in text and contains_terminal_bench_name(text):
        return "api.github.com Terminal-Bench search"
    if contains_terminal_bench_name(text) and any(marker in text for marker in network_markers()):
        return "Terminal-Bench network/repository lookup"
    return ""


def normalize(input_text: str) -> str:
    text = input_text.lower()
    replacements = {
        "\\u002d": "-",
        "\\u005f": "_",
        "%2d": "-",
        "%5f": "_",
        "%2f": "/",
        "%20": " ",
        "+": " ",
    }
    for old, new in replacements.items():
        text = text.replace(old, new)
    return text


def contains_terminal_bench_name(text: str) -> bool:
    return (
        "terminal-bench" in text
        or "terminalbench" in text
        or "terminal bench" in text
    )


def network_markers() -> tuple[str, ...]:
    return (
        "http://",
        "https://",
        "curl",
        "wget",
        "git clone",
        "gh repo",
        "gh api",
        "github.com",
        "raw.githubusercontent.com",
        "api.github.com",
        "google.com/search",
        "bing.com/search",
        "duckduckgo.com",
        "search?q=",
    )


def iter_jsonl(path: Path) -> Iterable[Any]:
    try:
        with path.open(encoding="utf-8") as handle:
            for line in handle:
                line = line.strip()
                if not line:
                    continue
                try:
                    yield json.loads(line)
                except json.JSONDecodeError:
                    continue
    except OSError:
        return


def read_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None


def json_text(value: Any) -> str:
    if isinstance(value, str):
        return value
    try:
        return json.dumps(value, sort_keys=True)
    except TypeError:
        return str(value)


def snippet(text: str, limit: int = 160) -> str:
    compact = " ".join(text.split())
    if len(compact) <= limit:
        return compact
    return compact[: limit - 3] + "..."


if __name__ == "__main__":
    raise SystemExit(main())
