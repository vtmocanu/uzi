#!/usr/bin/env python3
"""Prove both Node test shards ran every tracked agent test file exactly once."""

import argparse
import re
import subprocess
import tempfile
import xml.etree.ElementTree as ET
from pathlib import Path


REPO = Path(__file__).resolve().parent.parent
REPORTS = ("shard-1.xml", "shard-2.xml")


def latest_reports(report_dir):
    """Select each shard independently; a partial rerun need not rerun both legs."""
    artifacts = sorted(report_dir.glob("agent-tests-*"))
    if not artifacts:
        # Keep the standalone flat-report input used by local checks.
        return {name: report_dir / name for name in REPORTS}, []
    errors = []
    if list(report_dir.glob("*.xml")):
        errors.append("flat reports cannot be mixed with per-attempt artifacts")
    latest = {}
    for artifact in artifacts:
        match = re.fullmatch(r"agent-tests-([12])-attempt-([1-9][0-9]*)", artifact.name)
        if not match or not artifact.is_dir():
            errors.append(f"unrecognized shard artifact: {artifact.name}")
            continue
        shard, attempt = map(int, match.groups())
        if shard not in latest or attempt > latest[shard][0]:
            latest[shard] = (attempt, artifact)
    paths = {}
    for shard, name in enumerate(REPORTS, 1):
        directory = latest.get(shard, (0, report_dir))[1]
        paths[name] = directory / name
        if directory != report_dir:
            unexpected = sorted(p.name for p in directory.glob("*.xml") if p.name != name)
            if unexpected:
                errors.append(f"{directory.name}: unexpected reports: {', '.join(unexpected)}")
    return paths, errors


def tracked_tests():
    result = subprocess.run(
        ["git", "ls-files", "-z", "--", ":(glob)agent/test/*.test.ts"],
        cwd=REPO,
        check=True,
        capture_output=True,
    )
    files = set(result.stdout.decode().rstrip("\0").split("\0"))
    if not files or files == {""}:
        raise ValueError("git found no tracked agent test files")
    return files


def test_file(raw):
    path = raw.replace("\\", "/")
    if "/agent/test/" in path:
        return "agent/test/" + path.rsplit("/agent/test/", 1)[1]
    if path.startswith("agent/test/"):
        return path
    if path.startswith("test/"):
        return "agent/" + path
    raise ValueError(f"JUnit reported an unexpected test path: {raw}")


def describe_files(files):
    names = sorted(files)
    sample = ", ".join(names[:10])
    remaining = f", ... ({len(names) - 10} more)" if len(names) > 10 else ""
    return f"{len(names)}: {sample}{remaining}"


def check_reports(report_dir, expected):
    paths, errors = latest_reports(report_dir)
    observed = {}
    if all(path.parent == report_dir for path in paths.values()):
        present = {p.name for p in report_dir.glob("*.xml")}
    else:
        present = {name for name, path in paths.items() if path.is_file()}
    if present != set(REPORTS):
        errors.append(f"expected exactly {', '.join(REPORTS)}; found {', '.join(sorted(present)) or '(none)'}")
    marker = paths[REPORTS[0]].with_suffix(".codex-m4")
    if not marker.is_file() or marker.read_text().strip() != "passed":
        errors.append("Codex M4 did not complete in shard 1")
    if paths[REPORTS[1]].with_suffix(".codex-m4").exists():
        errors.append("Codex M4 also ran in shard 2")
    for name in REPORTS:
        path = paths[name]
        if not path.is_file():
            continue
        try:
            cases = list(ET.parse(path).iter("testcase"))
            if not cases:
                errors.append(f"{name}: no tests ran")
                continue
            files = set()
            for case in cases:
                raw = case.get("file")
                if not raw:
                    raise ValueError("testcase has no file attribute")
                files.add(test_file(raw))
            observed[name] = files
        except (ET.ParseError, ValueError) as exc:
            errors.append(f"{name}: {exc}")
    if len(observed) == len(REPORTS):
        first, second = (observed[name] for name in REPORTS)
        repeated = first & second
        actual = first | second
        if repeated:
            errors.append("files ran in both shards: " + describe_files(repeated))
        if missing := expected - actual:
            errors.append("tracked files not run: " + describe_files(missing))
        if unexpected := actual - expected:
            errors.append("untracked or unintended files ran: " + describe_files(unexpected))
    return errors, observed


def self_test():
    expected = {"agent/test/a.test.ts", "agent/test/b.test.ts", "agent/test/c.test.ts"}

    def write(path, files):
        root = ET.Element("testsuites")
        for name in files:
            ET.SubElement(root, "testcase", file=name, name=name)
        ET.ElementTree(root).write(path, encoding="utf-8", xml_declaration=True)

    def require(errors, text, scenario):
        if not any(text in error for error in errors):
            raise RuntimeError(f"self-test failed: {scenario}; errors={errors}")

    with tempfile.TemporaryDirectory(prefix="uzi-agent-shard-check-") as tmp:
        directory = Path(tmp)
        write(directory / REPORTS[0], ["agent/test/a.test.ts", "agent/test/c.test.ts"])
        write(directory / REPORTS[1], ["agent/test/b.test.ts"])
        (directory / "shard-1.codex-m4").write_text("passed\n")
        errors, _ = check_reports(directory, expected)
        if errors:
            raise RuntimeError(f"self-test failed: valid disjoint union was refused; errors={errors}")
        (directory / "shard-1.codex-m4").unlink()
        errors, _ = check_reports(directory, expected)
        require(errors, "did not complete", "missing M4 run went unnoticed")
        (directory / "shard-1.codex-m4").write_text("passed\n")
        (directory / "shard-2.codex-m4").write_text("passed\n")
        errors, _ = check_reports(directory, expected)
        require(errors, "also ran", "duplicate M4 run went unnoticed")
        (directory / "shard-2.codex-m4").unlink()
        write(directory / REPORTS[1], ["agent/test/a.test.ts", "agent/test/b.test.ts"])
        errors, _ = check_reports(directory, expected)
        require(errors, "both shards", "duplicate shard index went unnoticed")
        write(directory / REPORTS[1], ["agent/test/b.test.ts"])
        write(directory / REPORTS[0], ["agent/test/a.test.ts"])
        errors, _ = check_reports(directory, expected)
        require(errors, "not run", "dropped test file went unnoticed")
        write(directory / REPORTS[0], ["agent/test/a.test.ts", "agent/test/c.test.ts"])
        write(directory / REPORTS[1], ["agent/test/b.test.ts", "agent/test/d.test.ts"])
        errors, _ = check_reports(directory, expected)
        require(errors, "untracked or unintended", "unexpected test file went unnoticed")
        write(directory / REPORTS[1], [])
        errors, _ = check_reports(directory, expected)
        require(errors, "no tests ran", "empty shard went unnoticed")
        (directory / REPORTS[1]).unlink()
        errors, _ = check_reports(directory, expected)
        require(errors, "expected exactly", "missing shard report went unnoticed")
        attempts = directory / "attempts"
        attempts.mkdir()
        old = attempts / "agent-tests-1-attempt-1"
        retried = attempts / "agent-tests-1-attempt-2"
        second = attempts / "agent-tests-2-attempt-1"
        for artifact in (old, retried, second):
            artifact.mkdir()
        write(old / REPORTS[0], ["agent/test/a.test.ts", "agent/test/b.test.ts"])
        write(retried / REPORTS[0], ["agent/test/a.test.ts", "agent/test/c.test.ts"])
        write(second / REPORTS[1], ["agent/test/b.test.ts"])
        (retried / "shard-1.codex-m4").write_text("passed\n")
        errors, _ = check_reports(attempts, expected)
        if errors:
            raise RuntimeError(f"self-test failed: latest reports from different attempts were refused; errors={errors}")
        # A whole-workflow rerun selects both new legs and ignores old payloads.
        second_retry = attempts / "agent-tests-2-attempt-2"
        second_retry.mkdir()
        write(second_retry / REPORTS[1], ["agent/test/b.test.ts"])
        write(second / REPORTS[1], ["agent/test/c.test.ts"])
        (second / "shard-2.codex-m4").write_text("passed\n")
        errors, _ = check_reports(attempts, expected)
        if errors:
            raise RuntimeError(f"self-test failed: whole-workflow retry selected stale payloads; errors={errors}")
        # An old success cannot fill in a missing marker from the latest attempt.
        (old / "shard-1.codex-m4").write_text("passed\n")
        (retried / "shard-1.codex-m4").unlink()
        errors, _ = check_reports(attempts, expected)
        require(errors, "did not complete", "a stale M4 marker satisfied the latest attempt")
        # Attempts sort numerically, not lexically; an aggregator-only rerun needs
        # the most recent AVAILABLE report for each shard, not its own attempt.
        tenth = attempts / "agent-tests-1-attempt-10"
        tenth.mkdir()
        write(tenth / REPORTS[0], ["agent/test/a.test.ts", "agent/test/c.test.ts"])
        (tenth / "shard-1.codex-m4").write_text("passed\n")
        errors, _ = check_reports(attempts, expected)
        if errors:
            raise RuntimeError(f"self-test failed: numeric latest attempt was refused; errors={errors}")
        (tenth / REPORTS[0]).unlink()
        errors, _ = check_reports(attempts, expected)
        require(errors, "expected exactly", "a missing latest report fell back to an older report")
    print("agent shard checker self-test passed: file union, M4 count, mixed-attempt reruns, numeric ordering and stale-report cases")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("report_dir", nargs="?", type=Path)
    args = parser.parse_args()
    if args.self_test:
        self_test()
        return 0
    if args.report_dir is None:
        parser.error("report_dir is required unless --self-test is set")
    try:
        expected = tracked_tests()
        errors, observed = check_reports(args.report_dir, expected)
    except (OSError, subprocess.CalledProcessError, ValueError) as exc:
        print(f"FAIL: agent shard check could not run: {exc}")
        return 2
    for name in REPORTS:
        print(f"{name}: {len(observed.get(name, set()))} distinct test files")
    if errors:
        for error in errors:
            print(f"FAIL: {error}")
        return 1
    print(f"PASS: {len(expected)} tracked agent test files ran exactly once across two shards")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
