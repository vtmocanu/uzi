#!/usr/bin/env python3
"""Require every source-derived worker UID leaf to pass exactly once."""
import json
import sys
import xml.etree.ElementTree as ET


def check(expected, root):
    errors = []
    observed = {}
    expected_keys = {(test["file"], tuple(test["names"])) for test in expected}
    if len(expected_keys) != len(expected):
        errors.append("duplicate source leaf identity")

    def visit(element, names=(), blocked=False):
        blocked = blocked or any(element.find(tag) is not None for tag in ("skipped", "failure", "error"))
        if element.tag == "testsuite":
            names = (*names, element.get("name"))
        if element.tag == "testcase":
            raw = element.get("file", "").replace("\\", "/")
            file = "agent/test/" + raw.rsplit("/test/", 1)[-1] if "/test/" in raw else "agent/" + raw
            key = (file, (*names, element.get("name")))
            # Out-of-pattern leaves may appear as skipped on some Node versions.
            if key not in expected_keys:
                if element.find("skipped") is None:
                    errors.append(f"unexpected executed leaf: {file}: {' > '.join(key[1])}")
            else:
                observed.setdefault(key, []).append(blocked)
        for child in element:
            visit(child, names, blocked)

    visit(root)
    if not expected:
        errors.append("empty expected leaf inventory")
    for test in expected:
        key = (test["file"], tuple(test["names"]))
        matches = observed.get(key, [])
        label = " > ".join(test["names"])
        if len(matches) != 1:
            errors.append(f"{label}: expected exactly one result, found {len(matches)}")
        elif matches[0]:
            errors.append(f"{label}: skipped, failed or cancelled")
    return errors


if __name__ == "__main__":
    try:
        with open(sys.argv[1], encoding="utf-8") as source:
            expected = json.load(source)
        errors = check(expected, ET.parse(sys.argv[2]).getroot())
        if errors:
            raise ValueError("\n".join(errors))
        print(f"worker UID lane: {len(expected)} required leaves passed")
    except (OSError, ValueError, ET.ParseError, IndexError) as exc:
        print(f"worker UID lane FAIL: {exc}", file=sys.stderr)
        sys.exit(1)
