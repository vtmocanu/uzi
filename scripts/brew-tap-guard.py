#!/usr/bin/env python3
"""Uzi tap gate: 0 = newer, 3 = equal/older, 2 = unreadable/malformed input.

Read formula source as text, never execute it. The current templates derive their
version solely from the uzi tag URL; an explicit override requires manual review.
The workflow's shared concurrency group serializes this check and the tap push.
"""

import re
import sys
from pathlib import Path

NUMBER = r"(?:0|[1-9][0-9]*)"
TAG = re.compile(rf"v({NUMBER})\.({NUMBER})\.({NUMBER})(?:-rc\.([1-9][0-9]*))?")
URL = re.compile(r'^\s*url\s+"https://github\.com/vtmocanu/uzi/archive/refs/tags/([^"/]+)\.tar\.gz"\s*$', re.MULTILINE)


def version(tag):
    match = TAG.fullmatch(tag)
    if match is None:
        raise ValueError("expected a strict stable or RC uzi tag")
    major, minor, patch, rc = match.groups()
    return (int(major), int(minor), int(patch), rc is None, int(rc or 0))


def main():
    if len(sys.argv) != 3:
        print("usage: brew-tap-guard.py <incoming-tag> <current-formula>", file=sys.stderr)
        return 2
    incoming, path = sys.argv[1:]
    try:
        wanted = version(incoming)
        source = Path(path).read_text(encoding="utf-8")
        urls = URL.findall(source)
        # Multiple URLs or an explicit version can change Homebrew's effective
        # version. Refuse a layout this guard cannot interpret unambiguously.
        if (len(urls) != 1
                or len(re.findall(r"^\s*url\b", source, re.MULTILINE)) != 1
                or re.search(r"^\s*version(?:_scheme)?\b", source, re.MULTILINE)):
            raise ValueError("current formula must have one uzi tag URL and no version override")
        current = urls[0]
        existing = version(current)
    except (OSError, UnicodeError, ValueError) as error:
        print(f"refusing tap write: {error}", file=sys.stderr)
        return 2
    if wanted <= existing:
        print(f"Skipping {incoming}: tap is already at {current}")
        return 3
    print(f"Advancing tap from {current} to {incoming}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
