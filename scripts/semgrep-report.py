#!/usr/bin/env python3
"""Private scanner capture and bounded, non-source Semgrep verdicts (stdlib only)."""
import json
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import sys
import time

JSON_LIMIT = 32 * 1024 * 1024
STDERR_LIMIT = 1024 * 1024
CHUNK = 65536
POLL = 0.1
DRAIN = 2.0
GRACE = 0.5
ERROR_TYPES = frozenset((
    "Timeout", "ParseError", "LexicalError", "PatternParseError",
    "RuleParseError", "InvalidRuleSchemaError", "SemgrepError",
    "FatalError", "PartialParsing", "TooManyMatches", "MatchingError",
))
REASONS = frozenset((
    "ok", "startup_failure", "capture_failure", "capture_incomplete",
    "stdout_overflow", "stderr_overflow", "interrupted", "cleanup_unsettled",
))
# Reject credential-shaped substrings even inside otherwise valid identifiers.
TOKEN = re.compile(
    r"(?:gl(?:pat|oas|rt|cbt|ptt|soat|imt|agent|dt)-|gh[pousr]_|"
    r"github_pat_|sk-|xox[baprs]-|xoxe-|xapp-|uz[capfrsw]_|npm_|AKIA|ASIA)"
    r"[A-Za-z0-9_-]{8,}", re.I
)
IDENT = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,159}\Z")
TARGET = re.compile(r"[A-Za-z0-9_. /-]{1,240}\Z")


def capture(directory, argv):
    """Drain both pipes; every failure blocks the verdict and cleans our group.

    Each chunk is checked before writing. Once direct-child exit is observed,
    pipe drain has at most two seconds. Cleanup attempts TERM once, then KILL
    once after 0.5 seconds, and waits at most another 0.5 seconds for the child.
    A failed cleanup is explicit; no unrelated process is inspected or signaled.
    """
    interrupted = False

    def stop(_signum, _frame):
        nonlocal interrupted
        interrupted = True

    previous = {sig: signal.signal(sig, stop)
                for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)}
    child = None
    raw = None
    reason = "capture_failure"
    selector = selectors.DefaultSelector()
    streams = []
    outputs = []
    try:
        for name in ("stdout", "stderr"):
            outputs.append(open(Path(directory) / name, "xb", buffering=0))
        try:
            child = subprocess.Popen(argv, stdout=subprocess.PIPE,
                                     stderr=subprocess.PIPE, start_new_session=True)
        except OSError:
            reason = "startup_failure"
        else:
            streams = [child.stdout, child.stderr]
            for index, stream in enumerate(streams):
                os.set_blocking(stream.fileno(), False)
                selector.register(stream, selectors.EVENT_READ, index)
            sizes = [0, 0]
            limits = [JSON_LIMIT, STDERR_LIMIT]
            deadline = None
            reason = "ok"
            while True:
                raw = child.poll()
                if raw is not None and deadline is None:
                    deadline = time.monotonic() + DRAIN
                if interrupted:
                    reason = "interrupted"
                    break
                if not selector.get_map() and raw is not None:
                    break
                if deadline is not None and time.monotonic() >= deadline:
                    reason = "capture_incomplete"
                    break
                interval = POLL
                if deadline is not None:
                    interval = min(POLL, max(0.0, deadline - time.monotonic()))
                for key, _events in selector.select(interval):
                    index = key.data
                    data = os.read(key.fd, CHUNK)
                    if not data:
                        selector.unregister(key.fileobj)
                        continue
                    sizes[index] += len(data)
                    if sizes[index] > limits[index]:
                        reason = ("stdout_overflow", "stderr_overflow")[index]
                        break
                    if outputs[index].write(data) != len(data):
                        raise OSError("short capture write")
                if reason != "ok":
                    break
    except Exception:
        reason = "capture_failure"
    finally:
        # Always retire the owned group, including silent descendants that
        # closed their pipes. Group ID is the session-leading child's saved PID.
        if child is not None:
            try:
                os.killpg(child.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            except OSError:
                reason = "cleanup_unsettled"
            end = time.monotonic() + GRACE
            while time.monotonic() < end:
                observed = child.poll()
                if observed is not None:
                    raw = observed
                time.sleep(min(POLL, max(0.0, end - time.monotonic())))
            try:
                os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            except OSError:
                reason = "cleanup_unsettled"
            try:
                raw = child.wait(timeout=GRACE)
            except subprocess.TimeoutExpired:
                reason = "cleanup_unsettled"
        for stream in streams + outputs:
            try:
                stream.close()
            except OSError:
                reason = "capture_failure"
        selector.close()
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    if interrupted and reason == "ok":
        reason = "interrupted"
    with open(Path(directory) / "status", "x", encoding="ascii") as output:
        json.dump({"raw_status": raw, "reason": reason}, output)
    return 0 if reason == "ok" else 2


def safe_identifier(value):
    return (isinstance(value, str) and IDENT.fullmatch(value)
            and not TOKEN.search(value))


def relative_target(value, root):
    if not isinstance(value, str) or len(value) > 1024:
        return None
    path = Path(value)
    if path.is_absolute():
        try:
            value = str(path.relative_to(root))
        except ValueError:
            return None
    if value.startswith("./"):
        value = value[2:]
    if (not TARGET.fullmatch(value) or TOKEN.search(value)
            or any(part in ("", ".", "..") for part in value.split("/"))
            or not (root / value).is_file()):
        return None
    # Tracked symlinks must not make an outside target printable.
    try:
        (root / value).resolve().relative_to(root)
    except (ValueError, OSError):
        return None
    return value


def load_json(path):
    with open(path, "rb") as source:
        data = source.read(JSON_LIMIT + 1)
    if len(data) > JSON_LIMIT:
        raise ValueError("oversize")
    return json.loads(data)


def report(directory, stage, canary, config):
    root = Path.cwd().resolve()
    status = load_json(Path(directory) / "status")
    raw = status.get("raw_status")
    reason = status.get("reason")
    raw_valid = type(raw) is int and -255 <= raw <= 255
    raw_label = str(raw) if raw_valid else "missing"
    if reason not in REASONS:
        reason = "capture_failure"
    if reason != "ok":
        print(f"semgrep-gate: stage={stage} raw={raw_label} reason={reason}")
        return 2
    json_path = Path(directory) / "stdout"
    try:
        if json_path.stat().st_size == 0:
            raise FileNotFoundError("empty capture")
        data = load_json(json_path)
    except OSError:
        print(f"semgrep-gate: stage={stage} raw={raw_label} reason=startup_failure")
        return 2
    except ValueError:
        print(f"semgrep-gate: stage={stage} raw={raw_label} reason=invalid_json")
        return 2
    if (not isinstance(data, dict) or not isinstance(data.get("results"), list)
            or not isinstance(data.get("errors"), list)):
        print(f"semgrep-gate: stage={stage} raw={raw_label} reason=invalid_json")
        return 2
    results, errors = data["results"], data["errors"]
    # At most the canary plus 20 display targets; failures block publication.
    # Each independent lookup has one attempt and a one-second timeout.
    tracked = {}

    def tracked_target(value):
        path = relative_target(value, root)
        if path is None:
            return None
        if path not in tracked:
            if len(tracked) >= 21:
                raise ValueError("tracked_lookup_failure")
            try:
                result = subprocess.run(
                    ["git", "--literal-pathspecs", "ls-files", "--error-unmatch",
                     "--", path], stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL, timeout=1)
            except (OSError, subprocess.TimeoutExpired):
                raise ValueError("tracked_lookup_failure") from None
            if result.returncode not in (0, 1):
                raise ValueError("tracked_lookup_failure")
            tracked[path] = result.returncode == 0
        return path if tracked[path] else None

    target = tracked_target(canary)
    if target is None:
        raise ValueError("tracked_lookup_failure")
    # Semgrep prefixes IDs with the config's directory namespace, not arbitrary
    # suffixes. Permit that engine-generated namespace and the unprefixed ID.
    config_path = Path(config).resolve().relative_to(root)
    prefix = ".".join(config_path.parts) + "."
    canary_ids = {"semgrep-canary", prefix + "semgrep-canary"}
    live = any(isinstance(item, dict)
               and item.get("check_id") in canary_ids
               and target is not None
               and relative_target(item.get("path"), root) == target
               for item in results)
    verdict = 2
    if raw_valid and not errors:
        if stage == "canary":
            if raw == 1 and live:
                verdict = 0
        elif raw == 0 and not results:
            verdict = 0
        elif raw == 1 and results:
            verdict = 1
    lines = [f"semgrep-gate: stage={stage} raw={raw_label} "
             f"results={len(results)} errors={len(errors)} verdict={verdict}"]
    entries = 0
    used = len(lines[0].encode("utf-8")) + 1

    def add_entry(text):
        nonlocal entries, used
        size = len(text.encode("utf-8")) + 1
        # Leave room for the fixed omission summary. Sibling entries remain
        # independent; an unsafe or too-large entry never blocks another.
        if entries < 20 and used + size <= 8000:
            lines.append(text)
            entries += 1
            used += size

    for item in errors[:20]:
        kind = item.get("type") if isinstance(item, dict) else None
        kind = kind if isinstance(kind, str) and kind in ERROR_TYPES else "ScannerError"
        label = f"  error={kind}"
        if isinstance(item, dict):
            identifier = item.get("rule_id")
            if safe_identifier(identifier):
                label += f" rule={identifier}"
            # CliError has path but no line; ErrorSpan supplies file/start.
            # CoreError instead supplies Location.path/start. Never recover
            # a location from CliError.message, which can contain source.
            location = item.get("location")
            candidate = item.get("path")
            line = None
            if isinstance(location, dict):
                candidate = location.get("path")
                start = location.get("start")
                line = start.get("line") if isinstance(start, dict) else None
            else:
                spans = item.get("spans")
                if isinstance(spans, list) and spans and isinstance(spans[0], dict):
                    candidate = spans[0].get("file")
                    start = spans[0].get("start")
                    line = start.get("line") if isinstance(start, dict) else None
            path = tracked_target(candidate)
            if path is not None:
                label += f" target={path}"
                if type(line) is int and 1 <= line <= 10000000:
                    label += f" line={line}"
        add_entry(label)
    for item in results[:max(0, 20 - len(errors))]:
        label = "finding"
        if isinstance(item, dict):
            identifier = item.get("check_id")
            path = tracked_target(item.get("path"))
            start = item.get("start")
            line = start.get("line") if isinstance(start, dict) else None
            if (safe_identifier(identifier) and path is not None
                    and type(line) is int and 1 <= line <= 10000000):
                label += f" rule={identifier} target={path} line={line}"
        add_entry("  " + label)
    omitted = len(results) + len(errors) - entries
    lines.append(f"semgrep-gate: omitted={omitted}")
    # Construct completely before publishing: a renderer exception cannot
    # produce an apparently clean report. Reserve room for omission evidence.
    output = "\n".join(lines) + "\n"
    if len(output.encode("utf-8")) > 8192:
        raise ValueError("renderer_limit")
    sys.stdout.write(output)
    return verdict


def main():
    os.umask(0o077)
    try:
        mode, directory = sys.argv[1:3]
        if mode == "capture" and sys.argv[3] == "--":
            return capture(directory, sys.argv[4:])
        if mode == "report":
            stage, canary, config = sys.argv[3:6]
            if stage not in ("canary", "tree"):
                raise ValueError("stage")
            return report(directory, stage, canary, config)
    except Exception as error:
        reason = ("tracked_lookup_failure" if isinstance(error, ValueError)
                  and str(error) == "tracked_lookup_failure" else "renderer_failure")
        print(f"semgrep-gate: reason={reason}")
        return 2
    return 2


if __name__ == "__main__":
    sys.exit(main())
