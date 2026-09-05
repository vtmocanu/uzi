"""M0 Linux supervisor mechanisms: real child effects, not production code.

Run with python3 -B inside the documented unprivileged disposable container.
No Codex binary or network is needed; all processes are this fixture's descendants.
"""
import ctypes
import errno
import json
import os
from pathlib import Path
import signal
import sys
import tempfile
import time

from supervisor import WALL, children as own_children, drain, establish_profile, libc

ROOT = Path(tempfile.mkdtemp(prefix="cdr-supervisor-mechanism-"))


def report(name, **details):
    print(json.dumps({"case": name, **details}), flush=True)


def wait_for(predicate, label, timeout=2.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = predicate()
        if result:
            return result
        time.sleep(0.002)
    raise AssertionError(f"timed out: {label}")


def detached_family(name):
    ready, late = ROOT / f"{name}-ready.json", ROOT / f"{name}-late"
    leader = os.fork()
    if leader == 0:
        try:
            os.setsid()
            middle = os.fork()
            if middle == 0:
                os.setsid()
                daemon = os.fork()
                if daemon != 0:
                    os._exit(0)
                ready.write_text(json.dumps({"pid": os.getpid(), "pgid": os.getpgrp(), "ppid": os.getppid()}))
                time.sleep(0.4)
                late.write_text("owned detached late marker\n")
                os._exit(0)
            time.sleep(30)
            os._exit(0)
        except BaseException:
            os._exit(90)
    wait_for(lambda: ready.exists() and ready.stat().st_size, "detached ready")
    identity = json.loads(ready.read_text())
    assert identity["pgid"] != leader, "daemon escaped original shell process group"
    # The intermediate parent has exited, so the orphan must belong to this helper.
    wait_for(lambda: identity["pid"] in own_children(), "daemon adopted by subreaper")
    return leader, identity, late


def main():
    profile = establish_profile()
    report("profile", **profile)

    leader, daemon, late = detached_family("group-control")
    os.killpg(leader, signal.SIGKILL)
    wait_for(lambda: late.exists(), "PGID-only control late marker")
    assert late.read_text() == "owned detached late marker\n"
    result = drain(2000)
    assert result["state"] == "drained"
    report("PGID-only control", escaped_group=True, adopted=True, late_marker=True, cleanup=result)

    leader, daemon, late = detached_family("subreaper-drain")
    os.killpg(leader, signal.SIGKILL)
    result = drain(2000)
    assert result["state"] == "drained"
    assert len(result["reaped"]) >= 3
    time.sleep(0.5)
    assert not late.exists()
    assert drain(2000)["state"] == "drained"
    report("subreaper drain", escaped_group=True, adopted=True, late_marker=False, idempotent=True, cleanup=result)

    # No-signal injection stands for an unavailable/broken kill path; it must
    # produce unconfirmed disposal, not claim that WNOHANG=0 means completion.
    child = os.fork()
    if child == 0:
        time.sleep(30)
        os._exit(0)
    started = time.monotonic()
    result = drain(30, no_signal=True)
    elapsed = time.monotonic() - started
    assert result["state"] == "unconfirmed" and result["children"] == [child]
    assert 0.03 <= elapsed < 0.5
    assert child in own_children()
    cleanup = drain(2000)
    assert cleanup["state"] == "drained"
    report("bounded drain failure", injected="no signal", elapsed_ms=round(elapsed * 1000), result=result, cleanup=cleanup)

    # A clone child with no SIGCHLD on exit is excluded by ordinary waitpid.
    # Keep the ctypes callback and stack live until the child is reaped.
    ready = ROOT / "clone-ready"
    callback_type = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_void_p)
    @callback_type
    def clone_child(_):
        try:
            ready.write_text("clone child running\n")
            time.sleep(30)
        except BaseException:
            pass
        return 0
    stack = ctypes.create_string_buffer(1024 * 1024)
    libc.clone.argtypes = [callback_type, ctypes.c_void_p, ctypes.c_int, ctypes.c_void_p]
    libc.clone.restype = ctypes.c_int
    clone_pid = libc.clone(clone_child, ctypes.addressof(stack) + len(stack), 0, None)
    assert clone_pid > 0, ctypes.get_errno()
    wait_for(lambda: ready.exists(), "clone ready")
    assert clone_pid in own_children()
    try:
        os.waitpid(-1, os.WNOHANG)
        raise AssertionError("ordinary wait unexpectedly saw clone child")
    except ChildProcessError as error:
        assert error.errno == errno.ECHILD
    assert os.waitpid(-1, os.WNOHANG | WALL) == (0, 0)
    result = drain(2000)
    assert result["state"] == "drained" and len(result["reaped"]) == 1
    report("clone non-SIGCHLD control", ordinary_wait_false_ECHILD=True, wall_observed_live_child=True, cleanup=result)
    report("complete", cases=4, root=str(ROOT), result="pass")


try:
    main()
finally:
    cleanup = drain(2000)
    report("final cleanup", **cleanup)
    if cleanup["state"] != "drained":
        sys.exit(2)
