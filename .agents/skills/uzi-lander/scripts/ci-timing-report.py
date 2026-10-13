#!/usr/bin/env python3
"""Read-only, advisory GitHub CI timing report. Exit 2 means an instrument failure."""

import argparse
import datetime as dt
import json
import re
import statistics
import subprocess
import sys
import unicodedata
import xml.etree.ElementTree as ET
from pathlib import Path
from urllib.parse import quote


# Five ordinary runs from the latest 100 candidates limit API work and exclude reruns.
DEFAULT_SAMPLE_SIZE = 5
DEFAULT_SAMPLE_WINDOW = 100
MIN_TIMED_SAMPLE = 3


class InstrumentError(Exception):
    """An input or external read could not be trusted."""


def plain(value):
    if value is None:
        return "unknown"
    text = str(value)
    return "".join(c for c in text if not unicodedata.category(c).startswith("C"))


def timestamp(value):
    if not isinstance(value, str):
        return None
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
        return parsed if parsed.tzinfo else None
    except ValueError:
        return None


def elapsed(start, end):
    start, end = timestamp(start), timestamp(end)
    if start is None or end is None or end < start:
        return None
    return round((end - start).total_seconds(), 3)


def completed_interval(item, start="started_at"):
    if item.get("status") != "completed":
        return None
    return elapsed(item.get(start), item.get("completed_at"))


def read_json(path):
    try:
        if path.stat().st_size > 64 * 1024 * 1024:
            raise InstrumentError("input exceeds 64 MiB")
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise InstrumentError("cannot read input JSON") from exc


def gh_json(endpoint, pages=False):
    # GET is explicit. No gh workflow/run mutation or publishing command is used.
    args = ["gh", "api", "--method", "GET", endpoint]
    if pages:
        args += ["--paginate", "--slurp"]
    try:
        result = subprocess.run(args, capture_output=True, text=True, timeout=90)
        if result.returncode:
            raise InstrumentError("GitHub API read failed (report unavailable)")
        return json.loads(result.stdout)
    except (OSError, ValueError, subprocess.TimeoutExpired) as exc:
        raise InstrumentError("GitHub API read unavailable or invalid") from exc


def collect_run(repo, run):
    number = run.get("run_attempt")
    if type(number) is not int or number < 1:
        raise InstrumentError("run attempt count missing or invalid")
    attempts = []
    for attempt in range(1, number + 1):
        endpoint = f"repos/{repo}/actions/runs/{run['id']}/attempts/{attempt}"
        metadata = gh_json(endpoint)
        pages = gh_json(endpoint + "/jobs?per_page=100", pages=True)
        if not isinstance(pages, list) or any(
            not isinstance(p, dict) or not isinstance(p.get("jobs"), list) for p in pages
        ):
            raise InstrumentError("invalid job pages")
        attempts.append({"number": attempt, "run": metadata,
                         "jobs": [job for page in pages for job in page["jobs"]]})
    return {"run": run, "attempts": attempts}


def collect(repo, run_id, sample):
    endpoint = f"repos/{repo}/actions/workflows/ci.yml/runs?branch=main&event=push&status=success&per_page={DEFAULT_SAMPLE_WINDOW}"
    listed = gh_json(endpoint)
    if not isinstance(listed, dict) or not isinstance(listed.get("workflow_runs"), list):
        raise InstrumentError("invalid main run list")
    if any(not isinstance(r, dict) for r in listed["workflow_runs"]):
        raise InstrumentError("invalid main run entry")
    ordinary = [r for r in listed["workflow_runs"] if
                r.get("run_attempt") == 1 and r.get("conclusion") == "success"]
    if run_id is None:
        if not ordinary:
            raise InstrumentError("no ordinary successful main run found")
        run_id = ordinary[0]["id"]
    run = gh_json(f"repos/{repo}/actions/runs/{run_id}")
    if not isinstance(run, dict) or run.get("id") != run_id:
        raise InstrumentError("invalid run metadata")
    pulls = run.get("pull_requests", [])
    if not isinstance(pulls, list) or (pulls and
            (not isinstance(pulls[0], dict) or not isinstance(pulls[0].get("base"), dict))):
        raise InstrumentError("invalid pull request base metadata")
    branch = pulls[0]["base"].get("ref") if pulls else "main"
    if not isinstance(branch, str) or not re.fullmatch(r"[A-Za-z0-9_./-]+", branch):
        raise InstrumentError("invalid base branch")
    rules = gh_json(f"repos/{repo}/rules/branches/{quote(branch, safe='')}")
    required = set()
    if not isinstance(rules, list) or any(not isinstance(rule, dict) for rule in rules):
        raise InstrumentError("invalid branch rules")
    for rule in rules:
        if rule.get("type") == "required_status_checks":
            parameters = rule.get("parameters")
            contexts = parameters.get("required_status_checks") if isinstance(parameters, dict) else None
            if not isinstance(contexts, list) or any(
                not isinstance(c, dict) or not isinstance(c.get("context"), str) for c in contexts
            ):
                raise InstrumentError("invalid required contexts")
            required.update(c["context"] for c in contexts)
    if not required:
        raise InstrumentError("required contexts unavailable")
    target = collect_run(repo, run)
    samples = []
    for item in ordinary:
        created, target_created = timestamp(item.get("created_at")), timestamp(run.get("created_at"))
        if item["id"] == run_id or created is None or target_created is None or created >= target_created:
            continue
        if len(samples) == sample:
            break
        samples.append(collect_run(repo, item))
    return {"repo": repo, "required_contexts": sorted(required),
            "target": target, "samples": samples}


def job_report(job, origin):
    if not isinstance(job, dict) or not isinstance(job.get("name"), str):
        raise InstrumentError("job name missing or invalid")
    steps = job.get("steps", [])
    if not isinstance(steps, list) or any(not isinstance(s, dict) for s in steps):
        raise InstrumentError("invalid steps")
    wall = completed_interval(job)
    ready = elapsed(origin, job.get("created_at"))
    queue = elapsed(job.get("created_at"), job.get("started_at"))
    # Skipped jobs can carry stale or reversed timestamps on GitHub. Never time them.
    if job.get("conclusion") == "skipped":
        wall, queue = None, None
    timed = [{"name": plain(s.get("name", "unknown")),
              "seconds": completed_interval(s)} for s in steps]
    timed.sort(key=lambda s: (s["seconds"] is not None, s["seconds"] or 0), reverse=True)
    cache = [s for s in timed if re.search(r"go-cache|actions/cache|(?:restore|save).*cache",
                                         s["name"], re.I)]
    return {"name": plain(job["name"]), "status": job.get("status"),
            "conclusion": job.get("conclusion"), "execution_seconds": wall,
            "activation_delay_seconds": ready, "queue_seconds": queue,
            "start_offset_seconds": elapsed(origin, job.get("started_at")),
            "completion_offset_seconds": elapsed(origin, job.get("completed_at")) if wall is not None else None,
            "top_steps": timed[:3], "cache_action_steps": cache,
            "cache_transfer_seconds": None}


def analyze(item, required):
    if not isinstance(item, dict) or not isinstance(item.get("run"), dict):
        raise InstrumentError("run metadata missing")
    run = item["run"]
    attempts = item.get("attempts")
    if not isinstance(attempts, list) or not attempts:
        raise InstrumentError("attempts missing")
    if any(not isinstance(a, dict) or type(a.get("number")) is not int for a in attempts):
        raise InstrumentError("invalid attempt")
    origin = run.get("created_at")
    reported = []
    latest = {}
    numbers = set()
    for attempt in sorted(attempts, key=lambda a: a.get("number", 0)):
        if not isinstance(attempt, dict) or type(attempt.get("number")) is not int:
            raise InstrumentError("invalid attempt")
        if attempt["number"] < 1 or attempt["number"] in numbers:
            raise InstrumentError("duplicate/invalid attempt number")
        numbers.add(attempt["number"])
        metadata, jobs = attempt.get("run"), attempt.get("jobs")
        if not isinstance(metadata, dict) or not isinstance(jobs, list):
            raise InstrumentError("attempt metadata/jobs missing")
        names = set()
        normalized = []
        for job in jobs:
            report = job_report(job, origin)
            if job["name"] in names:
                raise InstrumentError("duplicate job name in one attempt")
            names.add(job["name"])
            normalized.append(report)
            latest[job["name"]] = report
        normalized.sort(key=lambda j: (j["execution_seconds"] is not None,
                                      j["execution_seconds"] or 0), reverse=True)
        reported.append({"number": attempt["number"],
                         "started_at": metadata.get("run_started_at"),
                         "finished_at": metadata.get("updated_at"),
                         "conclusion": metadata.get("conclusion"),
                         "elapsed_seconds": elapsed(metadata.get("run_started_at"), metadata.get("updated_at"))
                             if metadata.get("status") == "completed" else None,
                         "jobs": normalized})
    present = set(required) & latest.keys()
    missing = sorted(set(required) - latest.keys())
    values = [latest[n]["completion_offset_seconds"] for n in present]
    required_end = max(values) if values and all(v is not None for v in values) else None
    unknown = sorted(n for n in present if latest[n]["completion_offset_seconds"] is None)
    success = all(latest[n]["conclusion"] == "success" for n in present)
    all_jobs = list(latest.values())
    completed = [j for j in all_jobs if j["completion_offset_seconds"] is not None]
    last_job = max(completed, key=lambda j: j["completion_offset_seconds"]) if completed else None
    agents = [j for j in all_jobs if j["name"].startswith("test-agent-shard") and
              j["execution_seconds"] is not None]
    others = [j for j in all_jobs if j["name"] != "test-agent" and
              not j["name"].startswith("test-agent-shard") and
              j["execution_seconds"] is not None]
    gap = max(j["execution_seconds"] for j in agents) - max(j["execution_seconds"] for j in others) if agents and others else None
    return {"run_id": run.get("id"), "sha": run.get("head_sha"),
            "created_at": origin, "updated_at": run.get("updated_at"),
            "workflow_wall_seconds": elapsed(origin, run.get("updated_at"))
                if run.get("status") == "completed" else None,
            "attempts": reported, "reported_required_ci_completion_seconds": required_end,
            "reported_required_contexts": sorted(present),
            "missing_required_contexts": missing, "untimed_required_contexts": unknown,
            "required_readiness": "unknown" if missing or unknown else "all reported" if success else "not successful",
            "last_timed_job": last_job["name"] if last_job else None,
            "last_timed_job_completion_seconds": last_job["completion_offset_seconds"] if last_job else None,
            "agent_gap_seconds": gap, "rebalance_advisory": gap > 60 if gap is not None else None}


def junit_report(directory):
    files = sorted(directory.glob("*.xml"))
    files += sorted(directory.glob("agent-tests-*-attempt-*/*.xml"))
    if not files:
        raise InstrumentError("no existing JUnit reports found")
    reports = []
    for path in files:
        try:
            if path.stat().st_size > 32 * 1024 * 1024:
                raise InstrumentError("JUnit exceeds 32 MiB")
            root = ET.parse(path).getroot()
            cases = list(root.iter("testcase"))
            if not cases:
                raise InstrumentError("JUnit contains no cases")
            reports.append({"report": plain(str(path.relative_to(directory))),
                            "files": len({c.get("file") for c in cases})
                                if all(c.get("file") for c in cases) else None,
                            "cases": len(cases),
                            "coverage_note": "counts only; use check:agent-shards for coverage proof"})
        except (OSError, ET.ParseError) as exc:
            raise InstrumentError("cannot read JUnit report") from exc
    return reports


def report(bundle, junit=None):
    if not isinstance(bundle, dict) or not isinstance(bundle.get("required_contexts"), list):
        raise InstrumentError("required contexts missing")
    required = bundle["required_contexts"]
    if not required or any(not isinstance(n, str) for n in required):
        raise InstrumentError("required contexts empty/invalid")
    target = analyze(bundle.get("target"), required)
    samples = bundle.get("samples", [])
    if not isinstance(samples, list):
        raise InstrumentError("invalid sample list")
    compared = []
    ids = {target["run_id"]}
    for item in samples:
        if not isinstance(item, dict) or not isinstance(item.get("run"), dict):
            raise InstrumentError("invalid sample")
        run = item.get("run", {})
        if run.get("id") in ids:
            raise InstrumentError("duplicate sample run")
        ids.add(run.get("id"))
        if run.get("conclusion") != "success" or run.get("run_attempt") != 1 or run.get("head_branch") != "main" or run.get("event") != "push":
            raise InstrumentError("sample must be an ordinary successful main push")
        compared.append(analyze(item, required))
    known = [s["workflow_wall_seconds"] for s in compared if s["workflow_wall_seconds"] is not None]
    median = statistics.median(known) if len(known) >= MIN_TIMED_SAMPLE else None
    latest_target = {j["name"]: j for a in target["attempts"] for j in a["jobs"]}
    latest_samples = [{j["name"]: j for a in s["attempts"] for j in a["jobs"]} for s in compared]
    job_medians = []
    for name, job in latest_target.items():
        durations = [s[name]["execution_seconds"] for s in latest_samples if name in s and
                     s[name]["execution_seconds"] is not None]
        middle = statistics.median(durations) if len(durations) >= MIN_TIMED_SAMPLE else None
        job_medians.append({"name": name, "timed_sample_size": len(durations),
                            "execution_median_seconds": middle,
                            "execution_delta_seconds": round(job["execution_seconds"] - middle, 3)
                                if middle is not None and job["execution_seconds"] is not None else None})
    return {"advisory_only": True, "target": target,
            "comparison": {"sample_size": len(compared), "timed_sample_size": len(known),
                           "status": "sufficient sample" if median is not None else "insufficient sample",
                           "samples": [{"run_id": s["run_id"], "created_at": s["created_at"],
                                        "updated_at": s["updated_at"],
                                        "workflow_wall_seconds": s["workflow_wall_seconds"]} for s in compared],
                           "workflow_median_seconds": median,
                           "job_medians": job_medians,
                           "workflow_delta_seconds": round(target["workflow_wall_seconds"] - median, 3)
                               if median is not None and target["workflow_wall_seconds"] is not None else None},
            "junit": junit_report(junit) if junit else None}


def seconds(value):
    return "unknown" if value is None else f"{value:g}s"


def render(data):
    target = data["target"]
    print(f"ADVISORY CI timing: run {target['run_id']} sha {plain(target['sha'])}")
    print(f"Workflow: {seconds(target['workflow_wall_seconds'])} ({plain(target['created_at'])} to {plain(target['updated_at'])})")
    print(f"Reported required CI completion: {seconds(target['reported_required_ci_completion_seconds'])} from workflow creation")
    print("Required context coverage:", target["required_readiness"],
          "; not reported here:", ", ".join(map(plain, target["missing_required_contexts"])) or "none")
    print("Overall merge/reviewer readiness: not evaluated")
    print(f"Last timed job: {target['last_timed_job']} at {seconds(target['last_timed_job_completion_seconds'])}; not a dependency-graph critical-path proof")
    for attempt in target["attempts"]:
        print(f"Attempt {attempt['number']}: {seconds(attempt['elapsed_seconds'])} ({plain(attempt['started_at'])} to {plain(attempt['finished_at'])}), {plain(attempt['conclusion'])}")
        for index, job in enumerate(attempt["jobs"]):
            print(f"  {job['name']}: execute={seconds(job['execution_seconds'])} activation={seconds(job['activation_delay_seconds'])} queue={seconds(job['queue_seconds'])}")
            if index < 3:
                for step in job["top_steps"]:
                    print(f"    {step['name']}: {seconds(step['seconds'])}")
            for step in job["cache_action_steps"]:
                print(f"    cache action: {step['name']} {seconds(step['seconds'])} (pure transfer unknown)")
    print("Activation includes dependency/service scheduling; reason is not inferred. Queue starts at job created_at.")
    print(f"Agent gap vs next-longest other job: {seconds(target['agent_gap_seconds'])}; >60s rebalance advisory={target['rebalance_advisory']}")
    comparison = data["comparison"]
    against = ", ".join(str(s["run_id"]) for s in comparison["samples"]) or "no eligible sample runs"
    print(f"Compared against: {against}")
    print(f"Ordinary successful main comparison: {comparison['status']}; {comparison['sample_size']} samples, {comparison['timed_sample_size']} timed; median={seconds(comparison['workflow_median_seconds'])} delta={seconds(comparison['workflow_delta_seconds'])}")
    for sample in comparison["samples"]:
        print(f"  run {sample['run_id']}: {seconds(sample['workflow_wall_seconds'])} {plain(sample['created_at'])} to {plain(sample['updated_at'])}")
    for job in comparison["job_medians"]:
        print(f"  {job['name']}: {job['timed_sample_size']} timed samples; execute median={seconds(job['execution_median_seconds'])} delta={seconds(job['execution_delta_seconds'])}")
    if data["junit"] is not None:
        for item in data["junit"]:
            files = item["files"] if item["files"] is not None else "unknown"
            print(f"JUnit {item['report']}: {item['cases']} cases, {files} files; counts are not coverage proof")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("repo", nargs="?", help="OWNER/REPO (live GitHub reads)")
    parser.add_argument("run_id", nargs="?", type=int, help="default: latest ordinary successful main CI")
    parser.add_argument("--sample", type=int, default=DEFAULT_SAMPLE_SIZE,
                        help=f"up to {DEFAULT_SAMPLE_SIZE} ordinary main runs in the latest {DEFAULT_SAMPLE_WINDOW} successful candidates, excluding target; median needs {MIN_TIMED_SAMPLE}")
    parser.add_argument("--input", type=Path, help="offline saved report bundle; no network")
    parser.add_argument("--junit-dir", type=Path, help="existing local XML reports; never downloaded")
    parser.add_argument("--json", action="store_true", help="structured report")
    args = parser.parse_args(argv)
    try:
        if not 1 <= args.sample <= DEFAULT_SAMPLE_SIZE:
            raise InstrumentError(f"sample must be between 1 and {DEFAULT_SAMPLE_SIZE}")
        if args.input:
            bundle = read_json(args.input)
        else:
            if not args.repo or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repo):
                raise InstrumentError("repo must be OWNER/REPO")
            if args.run_id is not None and args.run_id < 1:
                raise InstrumentError("run ID must be positive")
            bundle = collect(args.repo, args.run_id, args.sample)
        data = report(bundle, args.junit_dir)
        if args.json:
            print(json.dumps(data, indent=2))
        else:
            render(data)
        return 0  # advisory latency/findings never become a gate
    except InstrumentError as exc:
        print(f"ci-timing-report: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
