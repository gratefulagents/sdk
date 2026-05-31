#!/usr/bin/env python3
from __future__ import annotations

import json
import sys
from collections import Counter, defaultdict
from pathlib import Path


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: analyze-terminal-bench-results.py jobs/<run-id>", file=sys.stderr)
        return 2
    job_dir = Path(sys.argv[1])
    result_path = job_dir / "result.json"
    if not result_path.exists():
        print(f"missing result.json: {result_path}", file=sys.stderr)
        return 2

    top = json.loads(result_path.read_text())
    stats = top["stats"]
    eval_stats = next(iter(stats["evals"].values()))
    reward_stats = eval_stats.get("reward_stats", {}).get("reward", {})
    pass_ids = set(reward_stats.get("1.0", []))
    fail_ids = set(reward_stats.get("0.0", []))
    timeout_ids = {p.parent.name for p in job_dir.glob("*/exception.txt")}

    tasks: dict[str, Counter[str]] = defaultdict(Counter)
    for trial_id in pass_ids | fail_ids:
        task = trial_id.split("__", 1)[0]
        tasks[task]["total"] += 1
        if trial_id in pass_ids:
            tasks[task]["pass"] += 1
        else:
            tasks[task]["fail"] += 1
        if trial_id in timeout_ids:
            tasks[task]["timeout"] += 1

    print(f"job={job_dir}")
    print(f"raw_mean={eval_stats['metrics'][0]['mean']:.6f}")
    for key, value in sorted(eval_stats.get("pass_at_k", {}).items(), key=lambda item: int(item[0])):
        print(f"pass_at_{key}={value:.6f}")
    print(f"trials={top['n_total_trials']} errors={stats['n_errored_trials']} retries={stats['n_retries']}")
    print(f"reward_1={len(pass_ids)} reward_0={len(fail_ids)}")

    timeout_rewards = Counter("pass" if trial_id in pass_ids else "fail" for trial_id in timeout_ids)
    print(
        "timeouts="
        + str(len(timeout_ids))
        + f" timeout_pass={timeout_rewards['pass']} timeout_fail={timeout_rewards['fail']}"
    )

    print("\nWorst tasks by raw mean:")
    for task, counts in sorted(
        tasks.items(),
        key=lambda item: (item[1]["pass"] / max(1, item[1]["total"]), -item[1]["timeout"], item[0]),
    )[:25]:
        total = counts["total"]
        mean = counts["pass"] / total if total else 0
        print(
            f"{task:36} mean={mean:.3f} pass={counts['pass']} "
            f"fail={counts['fail']} timeout={counts['timeout']}"
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
