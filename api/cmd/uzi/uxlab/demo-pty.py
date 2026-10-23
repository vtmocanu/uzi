#!/usr/bin/env python3
"""Bounded, fixture-only drive of an already compiled uzi tui --demo.

This is a terminal screen interpreter, not a TUI model. Assertions read only the
current screen; raw output is retained separately for diagnosing parser gaps.
No build, installation, server, authentication or runtime repair is performed.
"""
import argparse
import codecs
import errno
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import select
import signal
import struct
import termios
import time
import unicodedata


class Screen:
    def __init__(self, cols, rows, reply):
        self.cols, self.rows, self.reply = cols, rows, reply
        self.cells = [[" "] * cols for _ in range(rows)]
        self.x = self.y = 0
        self.saved = (0, 0)
        self.pending = ""
        self.last = " "

    def resize(self, cols, rows):
        self.cells = [(row[:cols] + [" "] * cols)[:cols]
                      for row in self.cells[:rows]]
        self.cells += [[" "] * cols for _ in range(rows - len(self.cells))]
        self.cols, self.rows = cols, rows
        self.x, self.y = min(self.x, cols - 1), min(self.y, rows - 1)

    def text(self):
        return "\n".join("".join(row).rstrip() for row in self.cells)

    def linefeed(self):
        self.y += 1
        if self.y >= self.rows:
            self.cells.pop(0)
            self.cells.append([" "] * self.cols)
            self.y = self.rows - 1

    def put(self, ch):
        if unicodedata.combining(ch) or ch in ("\ufe0e", "\ufe0f", "\u200d"):
            return
        width = 2 if unicodedata.east_asian_width(ch) in ("W", "F") else 1
        if self.x + width > self.cols:
            self.x = 0
            self.linefeed()
        self.cells[self.y][self.x] = ch
        if width == 2:
            self.cells[self.y][self.x + 1] = ""
        self.x += width
        self.last = ch

    def csi(self, body, op):
        if op in ("m", "q"):  # SGR / cursor shape / version query
            return
        if op == "u" and body in ("?", ">1"):
            # Legacy-keyboard terminal: ignore the enhancement push and do not
            # advertise Kitty support in response to its query. Neither command
            # changes the displayed cells; the driver sends legacy key bytes.
            return
        if op == "p" and body in ("?2026$", "?2027$"):
            # Synchronized output supported but reset; grapheme mode unavailable.
            mode, state = (2026, 2) if body == "?2026$" else (2027, 0)
            self.reply(f"\x1b[?{mode};{state}$y".encode())
            return
        if op == "W" and body == "?5":  # DECST8C: tab stops every 8 columns
            # Screen.feed already models HT with this fixed tab-stop layout.
            return
        private = body.startswith(("?", ">"))
        raw = body.lstrip("?>")
        nums = [int(v or "0") for v in raw.split(";")] if raw else [0]
        n = nums[0] or 1
        if op in ("h", "l"):
            if body == "?1049" and op == "h":
                self.cells = [[" "] * self.cols for _ in range(self.rows)]
                self.x = self.y = 0
            return  # cursor, mouse, paste and synchronized-output modes
        if op == "c":  # primary / secondary terminal attributes
            self.reply(b"\x1b[>1;0;0c" if body.startswith(">") else b"\x1b[?1;2c")
            return
        if op == "n" and raw == "6":
            self.reply(f"\x1b[{self.y + 1};{min(self.x, self.cols - 1) + 1}R".encode())
            return
        if private:
            raise AssertionError(f"unsupported private CSI {body!r}{op}")
        if op in ("H", "f"):
            self.y = min(self.rows - 1, max(0, n - 1))
            self.x = min(self.cols - 1, max(0, (nums[1] or 1) - 1)) if len(nums) > 1 else 0
        elif op in ("A", "B", "C", "D", "E", "F", "G", "d", "\x60"):
            if op in ("A", "F"):
                self.y = max(0, self.y - n)
            if op in ("B", "E"):
                self.y = min(self.rows - 1, self.y + n)
            if op in ("E", "F"):
                self.x = 0
            if op == "C":
                self.x = min(self.cols - 1, self.x + n)
            if op == "D":
                self.x = max(0, self.x - n)
            if op in ("G", "\x60"):
                self.x = min(self.cols - 1, n - 1)
            if op == "d":
                self.y = min(self.rows - 1, n - 1)
        elif op == "J":
            mode = nums[0]
            if mode in (2, 3):
                self.cells = [[" "] * self.cols for _ in range(self.rows)]
            elif mode in (0, 1):
                start = self.y * self.cols + min(self.x, self.cols - 1)
                for i in range(self.rows * self.cols):
                    if (mode == 0 and i >= start) or (mode == 1 and i <= start):
                        self.cells[i // self.cols][i % self.cols] = " "
            else:
                raise AssertionError(f"unsupported erase {mode}")
        elif op in ("K", "X"):
            x = min(self.x, self.cols - 1)
            start, end = (x, min(self.cols, x + n)) if op == "X" else {
                0: (x, self.cols), 1: (0, x + 1), 2: (0, self.cols)
            }[nums[0]]
            self.cells[self.y][start:end] = [" "] * (end - start)
        elif op in ("s", "u"):
            if op == "s":
                self.saved = (self.x, self.y)
            else:
                self.x, self.y = self.saved
        elif op == "b":
            for _ in range(min(n, self.cols * self.rows)):
                self.put(self.last)
        else:
            raise AssertionError(f"unsupported CSI {body!r}{op}")

    def feed(self, text):
        self.pending += text
        while self.pending:
            ch = self.pending[0]
            if ch == "\x1b":
                if len(self.pending) < 2:
                    return
                if self.pending[1] == "[":
                    match = re.match(r"\x1b\[([0-?]*[ -/]*)([@-~])", self.pending)
                    if not match:
                        return
                    self.csi(match[1].rstrip(), match[2])
                    self.pending = self.pending[match.end():]
                    continue
                if self.pending[1] in ("]", "P", "_", "^"):
                    match = re.search(r"\x07|\x1b\\", self.pending[2:])
                    if not match:
                        return
                    body = self.pending[2:2 + match.start()]
                    if body in ("10;?", "11;?"):
                        color = "ffff/ffff/ffff" if body.startswith("10") else "1010/1010/1010"
                        self.reply(f"\x1b]{body[:2]};rgb:{color}\x1b\\".encode())
                    self.pending = self.pending[2 + match.end():]
                    continue
                if self.pending[1] in ("(", ")"):
                    if len(self.pending) < 3:
                        return
                    self.pending = self.pending[3:]
                    continue
                op = self.pending[1]
                if op == "7":
                    self.saved = (self.x, self.y)
                elif op == "8":
                    self.x, self.y = self.saved
                elif op == "D":
                    self.linefeed()
                elif op == "E":
                    self.x = 0
                    self.linefeed()
                elif op != "=" and op != ">":
                    raise AssertionError(f"unsupported ESC {op!r}")
                self.pending = self.pending[2:]
                continue
            self.pending = self.pending[1:]
            if ch == "\r":
                self.x = 0
            elif ch == "\n":
                self.linefeed()
            elif ch == "\b":
                self.x = max(0, self.x - 1)
            elif ch == "\t":
                self.x = min(self.cols - 1, (self.x // 8 + 1) * 8)
            elif ord(ch) >= 32 and ch != "\x7f":
                self.put(ch)


class Drive:
    def __init__(self, out):
        self.out, self.pid, self.fd = out, None, None
        self.exit_status = None
        self.deadline = time.monotonic() + 150
        self.decoder = codecs.getincrementaldecoder("utf-8")("replace")
        self.raw = (out / "terminal.ansi").open("wb")
        self.events = (out / "checks.jsonl").open("w")
        self.screen = Screen(120, 34, self.send)
        self.total = 0

    def start(self, binary):
        ready_r, ready_w = os.pipe()
        try:
            self.pid, self.fd = pty.fork()
            if self.pid == 0:
                os.close(ready_w)
                if os.read(ready_r, 1) != b"1":
                    os._exit(127)
                os.close(ready_r)
                env = {key: os.environ[key] for key in ("HOME", "PATH") if key in os.environ}
                # Root pre-run hooks execute before the demo branch. Disable
                # their documented filesystem/network work without changing HOME.
                env.update(TERM="xterm-256color", COLORTERM="truecolor", LANG="C.UTF-8",
                           UZI_SKILL_AUTO_UPGRADE="0", UZI_VERSION_CHECK="0")
                try:
                    os.chdir(binary.parent)
                    os.execve(str(binary), [str(binary), "tui", "--demo"], env)
                except BaseException:
                    os._exit(127)
            os.close(ready_r)
            ready_r = None
            self.resize(120, 34)
            os.write(ready_w, b"1")
        finally:
            if ready_r is not None:
                os.close(ready_r)
            os.close(ready_w)

    def send(self, data):
        os.write(self.fd, data)

    def resize(self, cols, rows):
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        self.screen.resize(cols, rows)

    def pump(self, seconds):
        end = min(time.monotonic() + seconds, self.deadline)
        while time.monotonic() < end:
            if not select.select([self.fd], [], [], min(0.05, max(0, end - time.monotonic())))[0]:
                continue
            try:
                data = os.read(self.fd, 65536)
            except OSError as exc:
                if exc.errno == errno.EIO:
                    raise AssertionError("demo exited before the scenario completed") from exc
                raise
            if not data:
                raise AssertionError("demo PTY closed before the scenario completed")
            self.total += len(data)
            if self.total > 8 * 1024 * 1024:
                raise AssertionError("terminal output exceeded 8 MiB")
            self.raw.write(data)
            self.raw.flush()
            self.screen.feed(self.decoder.decode(data))
        if time.monotonic() >= self.deadline:
            raise AssertionError("150-second total drive deadline exceeded")

    def check(self, name, predicate, keys=None, timeout=6):
        if keys is not None:
            self.send(keys)
        end = min(self.deadline, time.monotonic() + timeout)
        while time.monotonic() < end:
            self.pump(0.15)
            if predicate(self.screen.text()):
                self.pump(0.15)
                if predicate(self.screen.text()):
                    self.snapshot(name, True)
                    return self.screen.text()
        self.snapshot(name, False)
        raise AssertionError(f"{name}: current screen did not satisfy assertion")

    def snapshot(self, name, passed):
        (self.out / f"{name}.txt").write_text(self.screen.text() + "\n")
        self.events.write(json.dumps({"check": name, "passed": passed,
                                     "size": [self.screen.cols, self.screen.rows]}) + "\n")
        self.events.flush()

    def close(self):
        # Only our exact child PID is signalled. Attempts and waits are bounded;
        # a failed scenario still reaches terminate/kill and waitpid.
        if self.pid and self.pid > 0:
            for sig, delay in ((None, 0.5), (signal.SIGTERM, 1), (signal.SIGKILL, 1)):
                if sig is None:
                    try:
                        self.send(b"q")
                    except OSError:
                        pass
                else:
                    try:
                        os.kill(self.pid, sig)
                    except ProcessLookupError:
                        pass
                until = time.monotonic() + delay
                while time.monotonic() < until:
                    pid, status = os.waitpid(self.pid, os.WNOHANG)
                    if pid:
                        self.exit_status = status
                        break
                    time.sleep(0.02)
                if self.exit_status is not None:
                    break
            if self.exit_status is None:
                raise AssertionError("own child was not reaped after SIGKILL")
        if self.fd is not None:
            os.close(self.fd)
        self.raw.close()
        self.events.close()


def contains(*parts):
    return lambda screen: all(part in screen for part in parts)


def selected(name):
    return contains("selected " + name, "cordoned (manual)", "fleet operator", "Maintenance window")


def scenario(d):
    d.check("start", contains("floor", "? keys"))
    d.check("workers", contains("NAME", "STATE", "enter/→ worker"), b"2")
    d.check("recovery-filter", contains("recovery", "filter"), b"/recovery")
    d.check("recovery-selected", selected("recovery"), b"\r")
    d.check("recovery-list-help", contains("open selected worker", "this help"), b"?")
    d.check("recovery-list-help-closed", selected("recovery"), b"\x1b")
    d.check("recovery-detail", contains("recovery", "cordoned (manual)", "pgup/pgdn scroll"), b"\r")
    d.check("worker-help", contains("select reported run", "back to origin"), b"?")
    d.check("worker-help-closed", contains("recovery", "pgup/pgdn scroll"), b"\x1b")
    d.check("worker-back", selected("recovery"), b"\x1b")
    # The worker filter supports backspace, not a clear-line shortcut.
    previous_filter = "recovery"
    for name, state, extra in (
            ("forge-small", "offline", "only pinned work may claim"),
            ("forge-m-2", "draining", "only pinned work may claim"),
            ("laptop", "busy", "outbox")):
        d.check(name + "-filter", contains(name), b"/" + b"\x7f" * len(previous_filter) + name.encode())
        previous_filter = name
        d.check(name + "-selected", selected(name), b"\r")
        d.check(name + "-detail", contains(name, state, extra, "cordoned (manual)", "pgup/pgdn scroll"), b"\r")
        d.check(name + "-back", selected(name), b"\x1b")
    d.check("restore-recovery-filter", contains("recovery"), b"/" + b"\x7f" * len(previous_filter) + b"recovery")
    d.check("restore-recovery", selected("recovery"), b"\r")
    for cols, rows in ((80, 12), (80, 34), (160, 60)):
        d.resize(cols, rows)
        footer = "ctrl+w focus" if rows == 60 else "enter/→ worker"
        d.check(f"resize-{cols}x{rows}", lambda s: selected("recovery")(s) and footer in s.splitlines()[-1])
    d.check("bottom-focus", contains("[pulls]", "ctrl+w focus"), b"\x17")
    d.check("top-focus", lambda s: "[workers]" in s and "[pulls]" not in s, b"\x17")
    d.check("split-collapsed", lambda s: "ctrl+w focus" not in s and selected("recovery")(s), b"s")
    d.check("split-restored", contains("ctrl+w focus", "selected recovery"), b"s")
    d.resize(120, 34)
    d.check("return-small", contains("enter/→ worker", "selected recovery"))
    d.check("floor", contains("floor", "scheduler"), b"1")
    d.check("scheduler-filter", contains("scheduler"), b"/scheduler")
    # Enter accepts the filter, then opens the selected live run.
    d.check("scheduler-filter-accepted", contains("scheduler", "418"), b"\r")
    d.check("scheduler-detail", contains("418", "TRANSCRIPT", "⇣ following"), b"\r")
    # Read the focus marker from paneTitle, rather than inferring from input sent.
    d.check("tab-transcript", contains("▎TRANSCRIPT"), b"\t")
    d.check("left-crew", contains("▎CREW"), b"\x1b[D")
    d.check("right-transcript", contains("▎TRANSCRIPT"), b"\x1b[C")
    paused = d.check("paused", contains("⏸", "g", "new"), b"\x1b[5~")
    before = int(re.search(r"(\d+) new", paused)[1])
    d.check("live-ticks-while-paused",
            lambda s: bool(re.search(r"(\d+) new", s)) and
            int(re.search(r"(\d+) new", s)[1]) > before and "⏸" in s,
            timeout=5)
    d.check("follow-resumed", contains("⇣ following"), b"g")
    d.check("live-message-visible", lambda s: "⇣ following" in s and any(
        line in s for line in ("Reading api/internal/poller/scheduler.go",
                              "Backoff applied;", "Running go test",
                              "Tests green.", "Editing scheduler_test.go")), timeout=5)


def main():
    def interrupted(signum, frame):
        raise SystemExit(f"interrupted by signal {signum}")

    signal.signal(signal.SIGTERM, interrupted)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[4]
    out = args.out.resolve()
    scratch = (root / ".uzi" / "scratch").resolve()
    if not out.is_relative_to(scratch) or out == scratch:
        parser.error("--out must be a new directory beneath this checkout's .uzi/scratch")
    binary = args.binary.resolve(strict=True)
    if not binary.is_file() or not os.access(binary, os.X_OK):
        parser.error("--binary must be the prebuilt candidate executable")
    out.mkdir(parents=True, exist_ok=False)
    drive = None
    try:
        drive = Drive(out)
        drive.start(binary)
        scenario(drive)
    except BaseException as exc:
        (out / "result.txt").write_text(f"FAIL: {type(exc).__name__}: {exc}\n")
        if drive is not None:
            drive.snapshot("failure-current-screen", False)
        raise
    finally:
        if drive is not None:
            try:
                drive.close()
            except BaseException as exc:
                (out / "result.txt").write_text(f"FAIL during cleanup: {exc}\n")
                raise
    if os.waitstatus_to_exitcode(drive.exit_status) != 0:
        (out / "result.txt").write_text(f"FAIL: demo exit status {drive.exit_status}\n")
        raise AssertionError("demo did not exit successfully")
    (out / "result.txt").write_text("PASS: fixture-only demo drive\n")


if __name__ == "__main__":
    main()
