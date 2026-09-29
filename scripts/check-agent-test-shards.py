#!/usr/bin/env python3
"""Prove both Node test shards ran every tracked agent test file exactly once."""

import argparse
import subprocess
import tempfile
import xml.etree.ElementTree as ET
from pathlib import Path


REPO = Path(__file__).resolve().parent.parent
REPORTS = ("shard-1.xml", "shard-2.xml")


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
    errors = []
    observed = {}
    present = {p.name for p in report_dir.glob("*.xml")}
    if present != set(REPORTS):
        errors.append(f"expected exactly {', '.join(REPORTS)}; found {', '.join(sorted(present)) or '(none)'}")
    marker = report_dir / "shard-1.codex-m4"
    if not marker.is_file() or marker.read_text().strip() != "passed":
        errors.append("Codex M4 did not complete in shard 1")
    if (report_dir / "shard-2.codex-m4").exists():
        errors.append("Codex M4 also ran in shard 2")
    for name in REPORTS:
        path = report_dir / name
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
    print("agent shard checker self-test passed: duplicate, dropped, unexpected, empty, missing, and M4-count cases fail")


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
