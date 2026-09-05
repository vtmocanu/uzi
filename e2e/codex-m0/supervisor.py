"""M0 Linux characterization helper, not a production supervisor.

Single-threaded per-run subreaper, above app-server and its detached code host.
stdin/stdout/stderr are app-server transport. fd3 is trusted controller input;
fd4 is supervisor evidence. The child closes both before exec. No global sweep.
"""
import ctypes
import json
import os
from pathlib import Path
import select
import signal
import sys
import time

WALL = 0x40000000
libc = ctypes.CDLL(None, use_errno=True)
libc.prctl.argtypes = [ctypes.c_int, ctypes.c_ulong, ctypes.c_ulong, ctypes.c_ulong, ctypes.c_ulong]
libc.prctl.restype = ctypes.c_int


def emit(value):
    os.write(4, (json.dumps(value) + "\n").encode())


def children(pid=None):
    pid = os.getpid() if pid is None else pid
    result = set()
    try:
        for task in Path(f"/proc/{pid}/task").iterdir():
            result.update(int(p) for p in (task / "children").read_text().split())
    except FileNotFoundError:
        pass
    return sorted(result)


def snapshot():
    pending, rows, visited = children(), [], set()
    while pending:
        pid = pending.pop()
        if pid in visited:
            continue
        visited.add(pid)
        if len(visited) > 100:
            raise RuntimeError("M0 snapshot exceeds 100 descendants")
        try:
            stat = Path(f"/proc/{pid}/stat").read_text().split(") ", 1)[1].split()
            cmd = Path(f"/proc/{pid}/cmdline").read_bytes().replace(b"\0", b" ").decode()
            rows.append({"pid": pid, "ppid": int(stat[1]), "pgid": int(stat[2]), "command": cmd})
            pending.extend(children(pid))
        except FileNotFoundError:
            pass
    return rows


def drain(timeout_ms, no_signal=False):
    end = time.monotonic() + timeout_ms / 1000
    killed, reaped = set(), set()
    while True:
        for pid in children():
            if no_signal:
                continue
            try:
                fd = os.pidfd_open(pid)
            except ProcessLookupError:
                continue
            try:
                signal.pidfd_send_signal(fd, signal.SIGKILL)
                killed.add(pid)
            except ProcessLookupError:
                pass
            finally:
                os.close(fd)
        while True:
            try:
                pid, status = os.waitpid(-1, os.WNOHANG | WALL)
            except ChildProcessError:
                if children():
                    return {"state": "unconfirmed", "reason": "ECHILD contradicted by children"}
                return {"state": "drained", "authority": "ECHILD+__WALL", "killed": sorted(killed), "reaped": sorted(reaped)}
            if pid == 0:
                break
            reaped.add(pid)
        if time.monotonic() >= end:
            return {"state": "unconfirmed", "reason": "deadline", "children": children(), "killed": sorted(killed), "reaped": sorted(reaped)}
        time.sleep(0.002)


def establish_profile():
    """Check the measured container profile and enable subreaping before fork."""
    signal.signal(signal.SIGCHLD, signal.SIG_DFL)
    if libc.prctl(36, 1, 0, 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "PR_SET_CHILD_SUBREAPER")
    flag = ctypes.c_int()
    if libc.prctl(37, ctypes.addressof(flag), 0, 0, 0) != 0 or flag.value != 1:
        raise RuntimeError("subreaper not confirmed")
    status = dict(line.split(":", 1) for line in Path("/proc/self/status").read_text().splitlines() if ":" in line)
    assert os.getuid() == 10002
    assert all(int(status[key], 16) == 0 for key in ["CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"])
    assert status["NoNewPrivs"].strip() == "1"
    return {"subreaper": True, "uid": os.getuid(), "caps": 0, "nnp": True}


def main():
    profile = establish_profile()
    child = os.fork()
    if child == 0:
        os.close(3)
        os.close(4)
        os.setsid()
        os.execv(sys.argv[1], sys.argv[1:])
    emit({"event": "started", "pid": os.getpid(), "appServerPid": child, **profile})
    buffer = b""
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        ready, _, _ = select.select([3], [], [], 0.1)
        if not ready:
            continue
        data = os.read(3, 4096)
        if not data:
            # Transport EOF is an abnormal controller loss, never clean success.
            emit({"event": "abnormal", "reason": "control EOF", "cleanup": drain(2000)})
            return 2
        buffer += data
        if len(buffer) > 8192:
            raise RuntimeError("oversized control input")
        while b"\n" in buffer:
            line, buffer = buffer.split(b"\n", 1)
            command = json.loads(line)
            if command["op"] == "snapshot":
                emit({"event": "snapshot", "id": command["id"], "processes": snapshot()})
            elif command["op"] == "dispose":
                result = drain(min(int(command.get("timeoutMs", 2000)), 2000), command.get("noSignal", False))
                emit({"event": "dispose", "id": command["id"], **result})
                if result["state"] == "drained":
                    return 0
            else:
                raise RuntimeError("unknown supervisor command")
    emit({"event": "abnormal", "reason": "lifetime deadline", "cleanup": drain(2000)})
    return 2


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:
        emit({"event": "abnormal", "reason": str(error), "cleanup": drain(2000)})
        sys.exit(2)
