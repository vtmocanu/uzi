#!/usr/bin/env python3
"""Exercise the skill's actual issue-list snippets under zsh, with hostile fields."""

import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--skill", type=Path)
    parser.add_argument("--snippet", type=int, choices=(1, 2, 3))
    parser.add_argument("--live", action="store_true", help="also run read-only gh lists")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[4]
    skill = args.skill or Path(__file__).resolve().parents[1] / "SKILL.md"
    blocks = [block for block in re.findall(r"```sh\n(.*?)```", skill.read_text(), re.S)
              if "gh issue list" in block and "jq -r" in block]
    assert len(blocks) == 3, f"expected three issue-list renderers, found {len(blocks)}"
    hostile = "\x1b]52;c;Y2xpcGJvYXJk\x07\u202e"
    fixture = [{"number": 1, "title": hostile + "safe title", "body": "untrusted body",
                "author": {"login": hostile + "external"}, "assignees": [],
                "labels": [{"name": hostile + "label"}]}]
    raw_fields = "\n".join([fixture[0]["title"], fixture[0]["author"]["login"],
                             fixture[0]["labels"][0]["name"]])
    print("Before (title/login/label bytes):", flush=True)
    subprocess.run(["od", "-c"], input=raw_fields.encode(), check=True)
    shells = ["bash"]
    if shutil.which("zsh"):
        shells.append("zsh")
    else:
        print("zsh unavailable: bash-only", flush=True)
    with tempfile.TemporaryDirectory(prefix="triage-render-") as tmp:
        payload = Path(tmp) / "issues.json"
        payload.write_text(json.dumps(fixture))
        for shell, index, block in [(shell, index, block) for shell in shells
                                    for index, block in enumerate(blocks, 1)]:
            if args.snippet and args.snippet != index:
                continue
            # Mock only the read-only forge call; run each documented pipeline unchanged.
            prelude = 'gh() { cat "$TRIAGE_FIXTURE"; }\n'
            result = subprocess.run([shell, "-f", "-c", prelude + block], cwd=root,
                                    env={**os.environ, "TRIAGE_FIXTURE": str(payload)},
                                    capture_output=True, check=True)
            rendered = result.stdout.decode()
            assert "safe title" in rendered and "label" in rendered, rendered
            assert not any(ord(c) < 32 and c not in "\t\n" or 127 <= ord(c) <= 159
                           or c == "\u202e" for c in rendered), repr(rendered)
            assert "Y2xpcGJvYXJk" not in rendered, repr(rendered)
            if index == 1:
                assert "external" in rendered, rendered
            print(f"After {shell} snippet {index} (bytes):", flush=True)
            subprocess.run(["od", "-c"], input=result.stdout, check=True)
            if args.live:
                live = subprocess.run([shell, "-f", "-c", block], cwd=root,
                                      capture_output=True, check=True)
                print(f"Live {shell} snippet {index}: PASS ({len(live.stdout.splitlines())} rows)")
    print("PASS: all three documented renderers strip hostile title/login/label bytes")


if __name__ == "__main__":
    main()
