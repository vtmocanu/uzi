#!/usr/bin/env python3
"""Bound foreground capture and verify resource ownership before removal."""
import json
import os
from pathlib import Path
import selectors
import signal
import subprocess
import sys
import time

REPORT_BYTES = 8 * 1024 * 1024
METADATA_BYTES = 64 * 1024
LABEL = "io.uzi.worker-uid.invocation"


def signal_group(proc, sig):
    # This private session/group was created by capture, never discovered by name.
    try:
        os.killpg(proc.pid, sig)
    except ProcessLookupError:
        pass


def capture(command, output, seconds, limit=REPORT_BYTES):
    """Keep at most limit bytes; terminate our watchdog on overflow or deadline.

    The watchdog owns its producer group. TERM gets five seconds, then the
    private group is killed. Pipe EOF is never required for completion.
    """
    deadline = time.monotonic() + max(0, seconds - 1)  # Reserve the final second for reaping.
    proc = subprocess.Popen(command, stdout=subprocess.PIPE, start_new_session=True)
    selector = selectors.DefaultSelector()
    selector.register(proc.stdout, selectors.EVENT_READ)
    os.set_blocking(proc.stdout.fileno(), False)
    total = 0
    failure = 0
    stopping = None
    interrupted = [False]
    old_handler = signal.signal(signal.SIGTERM, lambda *_: interrupted.__setitem__(0, True))
    try:
        with open(output, "wb") as target:
            while True:
                now = time.monotonic()
                if not failure and (now >= deadline - 5 or interrupted[0]):
                    failure = 124
                if failure and stopping is None:
                    # Signal the exact watchdog we launched; GNU timeout forwards TERM.
                    if proc.poll() is None:
                        proc.terminate()
                    else:
                        signal_group(proc, signal.SIGTERM)
                    stopping = now + 5
                if stopping is not None and now >= stopping:
                    signal_group(proc, signal.SIGKILL)
                    break
                events = selector.select(0.05)
                for key, _ in events:
                    chunk = os.read(key.fd, 65536)
                    if not chunk:
                        selector.unregister(key.fileobj)
                        continue
                    available = max(0, limit - total)
                    target.write(chunk[:available])
                    total += len(chunk)
                    if total > limit and not failure:
                        failure = 125
                        print(f"worker UID report overflow: limit {limit} bytes: {output}", file=sys.stderr)
                if proc.poll() is not None and not selector.get_map():
                    break
            if proc.poll() is None:
                signal_group(proc, signal.SIGKILL)
            rc = proc.wait(timeout=1)
        result = failure or (rc if rc >= 0 else 128 - rc)
        if result in (124, 137):
            print(f"worker UID deadline: command={' '.join(command[:6])} bound={seconds}s", file=sys.stderr)
        return result
    finally:
        signal.signal(signal.SIGTERM, old_handler)
        selector.close()
        proc.stdout.close()
        if failure:
            signal_group(proc, signal.SIGKILL)
        if proc.poll() is None:
            signal_group(proc, signal.SIGKILL)
            proc.wait(timeout=1)


def docker(args, directory, seconds=10):
    # All metadata/removal output is capped too; each call includes kill grace.
    output = directory / "docker-output"
    started = time.monotonic()
    print(f"worker UID Docker start: {' '.join(args[:3])}", file=sys.stderr)
    rc = capture(["timeout", "--kill-after=5s", f"{max(0.01, seconds - 5):g}s",
                  "docker", *args], output, seconds, METADATA_BYTES)
    print(f"worker UID Docker end: {' '.join(args[:3])} elapsed={time.monotonic()-started:.3f}s exit={rc}", file=sys.stderr)
    if rc:
        raise ValueError(f"Docker {' '.join(args[:2])} exit: {rc}")
    return output.read_bytes()


def owned(kind, name, token, directory, seconds=10, expected=None):
    if kind not in ("container", "volume", "image"):
        raise ValueError("unknown owned resource kind")
    prefix = ["image"] if kind == "image" else ["volume"] if kind == "volume" else []
    raw = docker(prefix + ["inspect", name], directory, seconds)
    items = json.loads(raw)
    if not isinstance(items, list) or len(items) != 1:
        raise ValueError(f"ambiguous ownership metadata: {name}")
    item = items[0]
    if not isinstance(item, dict):
        raise ValueError(f"invalid ownership metadata: {name}")
    config = item.get("Config")
    labels = item.get("Labels") if kind == "volume" else (config.get("Labels") if isinstance(config, dict) else None)
    if not isinstance(labels, dict) or labels.get(LABEL) != token:
        raise ValueError(f"ownership mismatch: {name}")
    handle = item.get("Name") if kind == "volume" else item.get("Id")
    if kind == "volume":
        valid = handle == name
    elif kind == "image":
        valid = isinstance(handle, str) and handle.startswith("sha256:") and len(handle) == 71 and all(c in "0123456789abcdef" for c in handle[7:])
    else:
        valid = isinstance(handle, str) and len(handle) == 64 and all(c in "0123456789abcdef" for c in handle)
    if not valid or (expected is not None and handle != expected):
        raise ValueError(f"invalid owned handle: {name}")
    return handle


def cleanup(directory, token):
    deadline = time.monotonic() + 29  # Reserve one second for scheduling and process reaping.
    failures = []
    journal = directory / "attempted"
    # Only requests recorded before creation are inspected, including lost ACKs.
    # Each sibling gets one attempt; failure never authorizes deletion or retries.
    entries = journal.read_text().splitlines() if journal.exists() else []
    for entry in reversed(entries):
        fields = entry.split(" ")
        if len(fields) not in (2, 3):
            failures.append("invalid ownership journal entry")
            continue
        kind, name = fields[:2]
        expected = fields[2] if len(fields) == 3 else None
        try:
            remaining = deadline - time.monotonic()
            if remaining <= 5:
                raise ValueError("cleanup deadline exhausted")
            handle = owned(kind, name, token, directory, min(10, remaining), expected)
            remaining = deadline - time.monotonic()
            if remaining <= 5:
                raise ValueError("cleanup deadline exhausted")
            args = (["image", "rm", "--no-prune", handle] if kind == "image" else
                    ["volume", "rm", handle] if kind == "volume" else ["rm", "-f", "--volumes", handle])
            docker(args, directory, min(10, remaining))
        except (OSError, ValueError, KeyError, TypeError) as exc:
            failures.append(str(exc))
    if failures:
        raise ValueError("worker UID cleanup exit: " + "; ".join(failures))


if __name__ == "__main__":
    try:
        action, directory, token, *args = sys.argv[1:]
        directory = Path(directory)
        if action == "cleanup":
            cleanup(directory, token)
        elif action == "verify-volume":
            owned("volume", args[0], token, directory)
        elif action == "capture":
            output, seconds, *command = args
            rc = capture(command, output, float(seconds))
            Path(output + ".status").write_text(f"EXIT={rc}\n")
            sys.exit(rc)
        else:
            raise ValueError("unknown bounded action")
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as exc:
        print(f"worker UID FAIL: {exc}", file=sys.stderr)
        sys.exit(1)
