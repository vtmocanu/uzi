#!/usr/bin/env python3
"""Run one takeover snapshot with a portable wall deadline and owned-process cleanup."""
import math
import os
import signal
import subprocess
import sys
import time


def stop_group(process):
    # A hung lookup can leave descendants holding the snapshot's output pipes open.
    # Only signal the fresh process group created for this invocation.
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    # Leave a short grace for claim-lock cleanup before stopping remaining children.
    time.sleep(0.1)
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    except PermissionError:
        # macOS may refuse a second group signal after TERM has emptied the group.
        # Confirm the owned leader has exited; never hide a refusal for a live leader.
        if process.poll() is None:
            raise
    process.wait()


def main():
    try:
        seconds = float(sys.argv[1])
        if not math.isfinite(seconds) or seconds <= 0 or len(sys.argv) < 3:
            raise ValueError
    except (IndexError, ValueError):
        print("takeover: deadline must be positive finite seconds", file=sys.stderr)
        print("UNKNOWN=1\nNEXT=unknown (invalid snapshot deadline)", flush=True)
        return 3
    try:
        process = subprocess.Popen(sys.argv[2:], start_new_session=True)
    except OSError:
        print("takeover: could not start snapshot", file=sys.stderr)
        print("UNKNOWN=1\nNEXT=unknown (snapshot could not start)", flush=True)
        return 3

    finishing = False

    def interrupted(signum, _frame):
        nonlocal finishing
        if finishing:
            return
        finishing = True
        try:
            stop_group(process)
        finally:
            print("SNAPSHOT_INTERRUPTED=1\nUNKNOWN=1\nNEXT=unknown (snapshot interrupted)", flush=True)
        raise SystemExit(128 + signum)

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    try:
        result = process.wait(timeout=seconds)
        return result if result >= 0 else 128 - result
    except subprocess.TimeoutExpired:
        finishing = True
        stop_group(process)
        print("takeover: snapshot deadline exceeded; rerun before acting", file=sys.stderr)
        print("SNAPSHOT_TIMEOUT=1\nUNKNOWN=1\nNEXT=unknown (snapshot deadline exceeded)", flush=True)
        return 124


if __name__ == "__main__":
    sys.exit(main())
