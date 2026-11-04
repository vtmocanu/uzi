#!/usr/bin/env python3
"""Fail closed unless every expected worker-env proof leaf ran exactly once and passed.

Expected leaves come from proofs.json, the same file the test registers its leaves from, so a
leaf that is declared but never executed (or executed twice, skipped, or failed) is an error.
"""
import json
import sys
import xml.etree.ElementTree as ET

MAX_REPORT_BYTES = 8 * 1024 * 1024
MAX_TEXT = 2000


def read_bounded(path, limit):
    with open(path, "rb") as source:
        data = source.read(limit + 1)
    if len(data) > limit:
        raise ValueError(f"report byte limit exceeded: {limit}")
    return data


def parse_report(path):
    data = read_bounded(path, MAX_REPORT_BYTES)
    if b"<!DOCTYPE" in data or b"<!ENTITY" in data:
        raise ValueError("XML DTD forbidden")
    return ET.fromstring(data)


def check(proofs, root):
    """Return (errors, timings) for a parsed JUnit root against the proofs.json mapping."""
    errors = []
    timings = []
    names = list(proofs["leaves"].values())
    if not names or len(set(names)) != len(names):
        errors.append("proofs.json must list at least one distinct leaf name")
    seen = {}
    for case in root.iter("testcase"):
        name = case.get("name")
        raw = (case.get("file") or "").replace("\\", "/")
        if raw and not raw.endswith("/" + proofs["file"]) and raw != proofs["file"]:
            errors.append(f"{name}: result comes from an unexpected file: {raw}")
            continue
        seen.setdefault(name, []).append(case)
    for name in seen:
        if name not in names:
            errors.append(f"unexpected executed leaf: {name}")
    for name in names:
        cases = seen.get(name, [])
        if len(cases) != 1:
            errors.append(f"{name}: expected exactly one result, found {len(cases)}")
            continue
        case = cases[0]
        timings.append((name, case.get("time") or "?"))
        for tag in ("failure", "error", "skipped"):
            for reason in case.findall(tag):
                detail = (reason.get("message") or "") + " " + "".join(reason.itertext())
                errors.append(f"{name}: {tag}: {detail.strip()[:MAX_TEXT] or 'no reason supplied'}")
    return errors, timings


def main(argv):
    try:
        with open(argv[1], "rb") as source:
            proofs = json.loads(source.read(1024 * 1024))
        errors, timings = check(proofs, parse_report(argv[2]))
    except (OSError, ValueError, KeyError, IndexError, ET.ParseError) as exc:
        print(f"worker env lane FAIL: {exc}", file=sys.stderr)
        return 1
    for name, seconds in timings:
        print(f"worker env proof elapsed: {seconds}s: {name}")
    if errors:
        print("worker env lane FAIL: " + "\n".join(errors), file=sys.stderr)
        return 1
    print(f"worker env lane: {len(timings)} required leaves passed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
