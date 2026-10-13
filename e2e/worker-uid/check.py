#!/usr/bin/env python3
"""Require every source-derived worker UID leaf to pass exactly once."""
import json
import sys
import unicodedata
import xml.etree.ElementTree as ET

DIAGNOSTIC_PREFIX = "advice-teardown-diagnostic "
MAX_DIAGNOSTIC_LINES = 8
MAX_DIAGNOSTIC_BYTES = 16384


def diagnostic_lines(texts, budget):
    result = []
    for text in texts:
        for line in text.split("\n"):
            if not line.startswith(DIAGNOSTIC_PREFIX):
                continue
            if budget[0] == 0 or budget[1] <= len(DIAGNOSTIC_PREFIX):
                return result
            clean = "".join(char for char in line if unicodedata.category(char) not in ("Cc", "Cf", "Zl", "Zp"))
            encoded = clean.encode("utf-8", errors="replace")
            clean = encoded.decode("utf-8")
            available = budget[1] - 1  # Include the printed line's newline in the total cap.
            if len(encoded) > available:
                suffix = " [truncated]"
                if available < len(DIAGNOSTIC_PREFIX) + len(suffix):
                    return result
                clean = encoded[:available - len(suffix)].decode("utf-8", errors="ignore") + suffix
            budget[0] -= 1
            budget[1] -= len(clean.encode("utf-8")) + 1
            result.append(clean)
    return result


def check(expected, root):
    errors = []
    observed = {}
    captured = {}
    expected_keys = {(test["file"], tuple(test["names"])) for test in expected}
    if len(expected_keys) != len(expected):
        errors.append("duplicate source leaf identity")

    def visit(element, names=(), diagnostics=()):
        if element.tag == "testsuite":
            names = (*names, element.get("name"))
        origin_names = (*names, element.get("name")) if element.tag == "testcase" else names
        origin = f"{element.tag}: {' > '.join(origin_names)}"
        local = tuple((origin, tag, tuple(reason.attrib.items()), "".join(reason.itertext()).strip())
                      for tag in ("skipped", "failure", "error") for reason in element.findall(tag))
        diagnostics = (*diagnostics, *local)
        if element.tag == "testcase":
            raw = element.get("file", "").replace("\\", "/")
            file = "agent/test/" + raw.rsplit("/test/", 1)[-1] if "/test/" in raw else "agent/" + raw
            key = (file, (*names, element.get("name")))
            # Out-of-pattern leaves may appear as skipped on some Node versions.
            if key not in expected_keys:
                if element.find("skipped") is None:
                    errors.append(f"unexpected executed leaf: {file}: {' > '.join(key[1])}")
            else:
                observed.setdefault(key, []).append((diagnostics, element))
                captured[element] = ["".join(output.itertext()) for tag in ("system-out", "system-err")
                                     for output in element.findall(tag)]
                captured[element].extend((child.text or "").strip() for child in element if child.tag is ET.Comment)
        preceding_case = None
        for child in element:
            if child.tag is ET.Comment:
                # Node's JUnit reporter writes t.diagnostic() comments just after
                # their testcase. Never inherit suite/global output into a leaf.
                if preceding_case in captured:
                    captured[preceding_case].append((child.text or "").strip())
                continue
            visit(child, names, diagnostics)
            preceding_case = child if child.tag == "testcase" else None

    visit(root)
    if not expected:
        errors.append("empty expected leaf inventory")
    budget = [MAX_DIAGNOSTIC_LINES, MAX_DIAGNOSTIC_BYTES]
    for test in expected:
        key = (test["file"], tuple(test["names"]))
        matches = observed.get(key, [])
        label = f"{test['file']}: {' > '.join(test['names'])}"
        if len(matches) != 1:
            errors.append(f"{label}: expected exactly one result, found {len(matches)}")
        elif matches[0][0]:
            reasons, element = matches[0]
            for origin, tag, attributes, text in reasons:
                details = [f"{name}={value}" for name, value in attributes if value.strip()]
                if text:
                    details.append(text)
                reason = "\n".join(details) or "no reason supplied"
                errors.append(f"{label}: {tag} from {origin}: {reason}")
            if any(element.find(tag) is not None for tag in ("failure", "error", "skipped")):
                errors.extend(diagnostic_lines(captured[element], budget))
    return errors


if __name__ == "__main__":
    try:
        with open(sys.argv[1], encoding="utf-8") as source:
            expected = json.load(source)
        parser = ET.XMLParser(target=ET.TreeBuilder(insert_comments=True))
        errors = check(expected, ET.parse(sys.argv[2], parser=parser).getroot())
        if errors:
            raise ValueError("\n".join(errors))
        print(f"worker UID lane: {len(expected)} required leaves passed")
    except (OSError, ValueError, ET.ParseError, IndexError) as exc:
        print(f"worker UID lane FAIL: {exc}", file=sys.stderr)
        sys.exit(1)
