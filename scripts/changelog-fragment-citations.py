#!/usr/bin/env python3
"""Map captured fragment snapshots to their first-parent introducing identities."""
import hashlib
import re
import subprocess
import sys
from pathlib import Path


class Refusal(Exception):
    pass


def git(*args):
    result = subprocess.run(
        ["git", "-c", "core.fsmonitor=false", *args], capture_output=True, check=False
    )
    if result.returncode:
        raise Refusal(f"git failed: {result.stderr.decode(errors='replace').strip()}")
    return result.stdout


def tree(sha):
    entries = {}
    for record in git("ls-tree", "-rz", sha, "--", "changelog.d").split(b"\0"):
        if record:
            metadata, path = record.split(b"\t", 1)
            mode, kind, oid = metadata.split()
            entries[path] = (mode, kind, oid)
    return entries


def citations(base, head, work, shipping_file):
    for oid in (base, head):
        if not re.fullmatch(r"(?:[0-9a-f]{40}|[0-9a-f]{64})", oid):
            raise Refusal("expected captured commit IDs")
        if git("rev-parse", "--verify", oid + "^{commit}").strip().decode() != oid:
            raise Refusal("captured commit ID changed")

    paths = (work / "paths").read_bytes().splitlines()
    if len(paths) > 1024 or len(paths) != len(set(paths)):
        raise Refusal("invalid captured fragment inventory")
    pattern = rb"changelog\.d/[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.md"
    captured = tree(head)
    snapshots = []
    for index, path in enumerate(paths):
        if not re.fullmatch(pattern, path):
            raise Refusal("invalid captured fragment path")
        with (work / f"fragment-{index}").open("rb") as source:
            data = source.read(65537)
        if len(data) > 65536:
            raise Refusal("fragment exceeds 65536 bytes")
        mode, kind, oid = captured.get(path, (b"", b"", b""))
        algorithm = "sha1" if len(head) == 40 else "sha256"
        digest = hashlib.new(algorithm, b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest().encode()
        if (mode, kind, oid) != (b"100644", b"blob", digest):
            raise Refusal("fragment snapshot is not the captured regular committed input")
        snapshots.append(data)

    history = git("rev-list", "--first-parent", "--reverse", base + ".." + head).splitlines()
    shipping = set(Path(shipping_file).read_bytes().splitlines())
    if not shipping.issubset(set(history)):
        raise Refusal("shipping identities do not belong to the captured history")
    baseline = tree(base)
    current = {path: (b"", True) if path in baseline else None for path in paths}
    additions = {}
    for sha in history:
        parents = git("rev-list", "--parents", "-n", "1", sha.decode()).split()
        if len(parents) < 2:
            raise Refusal("introduction history has no first parent")
        raw = git("diff", "--raw", "--no-renames", "--no-abbrev", "-z",
                  parents[1].decode(), sha.decode(), "--", "changelog.d")
        fields = raw.split(b"\0")
        if fields[-1] or len(fields) % 2 != 1:
            raise Refusal("incomplete introduction diff")
        added = []
        for index in range(0, len(fields) - 1, 2):
            metadata, path = fields[index:index + 2]
            oldmode, newmode, _, _, status = metadata.split()
            if not oldmode.startswith(b":"):
                raise Refusal("invalid introduction diff")
            if path not in current:
                continue
            if status == b"D":
                current[path] = None
            elif status == b"A":
                current[path] = (sha, newmode == b"100644")
                added.append(path)
        additions[sha] = added

    rows = []
    for index, path in enumerate(paths):
        event = current[path]
        if event is None or not event[1]:
            raise Refusal("cannot establish a regular fragment introduction")
        sha = event[0]
        if sha not in shipping or len(additions.get(sha, [])) != 1:
            continue
        subject = git("log", "-1", "--format=%s", sha.decode()).rstrip(b"\n")
        match = re.search(rb"\(#([1-9][0-9]*)\)$", subject)
        identity = b"#" + match[1] if match else git("rev-parse", "--short", sha.decode()).strip()
        if match:
            present = re.search(rb"#0*" + match[1] + rb"(?![0-9])", snapshots[index])
        else:
            present = identity in snapshots[index]
        if not present:
            rows.append(f"{index + 1}\t{identity.decode()}\n")
    return "".join(rows)


if __name__ == "__main__":
    try:
        if len(sys.argv) != 5:
            raise Refusal("expected base, head, snapshot directory and shipping identities")
        sys.stdout.write(citations(sys.argv[1], sys.argv[2], Path(sys.argv[3]), sys.argv[4]))
    except (OSError, ValueError, UnicodeError, IndexError, Refusal) as error:
        print(f"fragment-citations: {error}", file=sys.stderr)
        raise SystemExit(1) from error
