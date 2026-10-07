#!/bin/sh
# Hermetic wrapper/capture/report tests; each fixture owns its Git index.
set -eu
ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"
mkdir -p .uzi/scratch
TEST_DIR="$(mktemp -d "$ROOT/.uzi/scratch/semgrep-test.XXXXXX")"
trap 'rm -rf -- "$TEST_DIR"' EXIT
trap 'exit 2' INT TERM HUP
python3 -B - "$ROOT" "$TEST_DIR" "${1:-}" <<'PY'
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import time

root, scratch, baseline = sys.argv[1:]
scratch = Path(scratch)
repo = scratch / "fixture"
repo.mkdir()
(repo / "scripts").mkdir()
(repo / "rules").mkdir()
(repo / "bin").mkdir()
(repo / ".uzi/scratch").mkdir(parents=True)
wrapper = Path(baseline) if baseline else Path(root) / "scripts/semgrep-gate.sh"
shutil.copyfile(wrapper, repo / "scripts/semgrep-gate.sh")
helper_source = (Path(root) / "scripts/semgrep-report.py").read_text()
helper = repo / "scripts/semgrep-report.py"
helper.write_text(helper_source)
(repo / "scripts/semgrep-canary.txt").write_text("CANARY\n")
(repo / "tracked.py").write_text("print('fixture')\n")
(repo / "rules/canary.yml").write_text("rules: []\n")
# Never discover the parent index: initialize and address this fixture explicitly.
git_env = dict(os.environ)
for key in ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"):
    git_env.pop(key, None)
subprocess.run(["git", "init", "-q", str(repo)], check=True, env=git_env)
subprocess.run(["git", "-C", str(repo), "add", "scripts/semgrep-canary.txt",
                "tracked.py", "rules/canary.yml"], check=True, env=git_env)
fake = repo / "bin/semgrep"
fake.write_text(r'''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import signal
import sys
import time
args = sys.argv[1:]
stage = "tree" if args[-1] == "." else "canary"
case = os.environ["CASE"]
settings = Path(os.environ["SEMGREP_SETTINGS_FILE"])
with open(os.environ["CALLS"], "a") as f:
    f.write(json.dumps({"args": args, "stage": stage, "settings": str(settings),
                        "exists": settings.exists(),
                        "mode": settings.parent.stat().st_mode & 0o777}) + "\n")
if case == "hang":
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
with open(os.environ["OWNED"], "a") as f:
    f.write(str(os.getpid()) + "\n")
item = {"check_id": "rules.semgrep-canary", "path": "scripts/semgrep-canary.txt",
        "start": {"line": 1}, "extra": {"message": "SOURCE_PAYLOAD"}}
results = [item] if stage == "canary" else []
errors = []
rc = 1 if stage == "canary" else 0
active = case.endswith(stage) or case in ("malformedrules", "stderr", "payload",
                                          "many", "hold", "hang", "termhang")
name = case.removesuffix(stage) if active else ""
if case == "malformedrules":
    errors = [{"type": "RuleParseError"}]; rc = 2
elif name == "dead":
    results = []; rc = 0
elif name == "findings":
    results = [{"check_id": "fixture.rule", "path": "tracked.py", "start": {"line": 4}}]
    rc = 1
elif name in ("timeout", "mixed"):
    errors = [{"type": "Timeout", "message": "SOURCE_PAYLOAD"}]
    results = results if name == "mixed" else []
    rc = 1
elif name == "errors0":
    errors = [{"type": "ParseError"}]; rc = 0
elif name == "errors1":
    errors = [{"type": "FatalError"}]; rc = 1
elif name == "empty1":
    results = []; rc = 1
elif name == "results0":
    results = [item]; rc = 0
elif name == "wrongrule":
    item["check_id"] = "unrelated.rules.semgrep-canary"
elif name == "wrongtarget":
    item["path"] = "tracked.py"
elif name == "bare":
    item["check_id"] = "semgrep-canary"
elif name == "absolute":
    item["path"] = str(Path.cwd() / "scripts/semgrep-canary.txt")
elif name == "malformed":
    print("{"); sys.exit(rc)
elif name == "overflowout":
    for _ in range(530):
        os.write(1, b"x" * 65536)
    sys.exit(rc)
elif name == "overflowerr":
    for _ in range(20):
        os.write(2, b"x" * 65536)
    sys.exit(rc)
elif name == "raw2":
    rc = 2
elif name == "schema":
    print(json.dumps({"results": []})); sys.exit(0)
if case == "stderr":
    os.write(2, b"ARBITRARY_STDERR\x1b[31m/outside/private\n")
if case == "payload" and stage == "tree":
    secret = "glpat-" + "a" * 20
    results = [
        {"check_id": secret, "path": "tracked.py", "start": {"line": 1}},
        {"check_id": "valid", "path": "/outside/private", "start": {"line": 1}},
        {"check_id": "bad\x1b[31m", "path": "tracked.py", "start": {"line": 1}},
        {"check_id": "valid", "path": "tracked.py", "start": {"line": -1}},
        {"check_id": "valid", "path": "tracked.py", "start": {"line": 8},
         "extra": {"message": "SOURCE_PAYLOAD", "lines": "SOURCE_PAYLOAD",
                   "metavars": {"SECRET": secret}}},
    ]
    errors = [{"type": "evil\x1b[31m", "message": secret}]
    rc = 1
if case == "many" and stage == "tree":
    results = [{"check_id": "fixture.rule", "path": "tracked.py",
                "start": {"line": 1}}] * 100
    rc = 1
if case in ("hang", "termhang"):
    if case == "hang":
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
    while True:
        time.sleep(0.1)
if case == "hold":
    pid = os.fork()
    if pid == 0:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        with open(os.environ["OWNED"], "a") as f:
            f.write(str(os.getpid()) + "\n")
        while True:
            time.sleep(0.1)
print(json.dumps({"results": results, "errors": errors}), flush=True)
sys.exit(rc)
''')
fake.chmod(0o755)
env = dict(git_env, PATH=str(repo / "bin") + os.pathsep + os.environ["PATH"],
           CALLS=str(scratch / "calls"), OWNED=str(scratch / "owned"))
env.pop("UZI_SAST_REQUIRED", None)
env.pop("PYTHONPATH", None)
calls = Path(env["CALLS"])
owned = Path(env["OWNED"])
passed = 0


def reset(case):
    calls.write_text("")
    owned.write_text("")
    env["CASE"] = case


def records():
    return [json.loads(line) for line in calls.read_text().splitlines()]


def gone(pid):
    # Identity-level check of saved OWNED handles, never enumerate processes.
    try:
        state = Path(f"/proc/{pid}/stat").read_text().split(") ", 1)[1].split()[0]
        return state == "Z"
    except FileNotFoundError:
        return True


def settled():
    for value in owned.read_text().splitlines():
        assert gone(int(value)), f"OWNED handle still running: {value}"


def run(case, expected, count, extra_env=None):
    global passed
    reset(case)
    start = time.monotonic()
    result = subprocess.run(["sh", "scripts/semgrep-gate.sh", "rules",
                             "scripts/semgrep-canary.txt"],
                            cwd=repo, env=env | (extra_env or {}),
                            capture_output=True, timeout=15)
    output = result.stdout + result.stderr
    assert result.returncode == expected, (
        f"{case}: expected={expected} actual={result.returncode} output={output!r}")
    rows = records()
    assert len(rows) == count, (case, rows)
    if not baseline:
        for row in rows:
            args = row["args"]
            for flag in ("--error", "--strict", "--metrics=off",
                         "--disable-version-check", "--json", "--timeout-threshold=0"):
                assert args.count(flag) == 1, (case, flag)
            assert args[args.index("--timeout") + 1] == "30"
            assert args[args.index("--config") + 1] == "rules"
            assert row["mode"] == 0o700 and not row["exists"]
            private = Path(row["settings"]).parent
            exclude = str(private.relative_to(repo))
            assert exclude in args and args[args.index(exclude) - 1] == "--exclude"
            assert not private.exists(), f"capture dir leaked: {private}"
            if row["stage"] == "tree":
                assert "semgrep-canary.txt" in args
        assert not list((repo / ".uzi/scratch").iterdir()), "private evidence leaked"
        assert len(output) <= 16384
        for bad in (b"SOURCE_PAYLOAD", b"ARBITRARY_STDERR", b"/outside/private",
                    b"\x1b", ("glpat-" + "a" * 20).encode()):
            assert bad not in output, (case, bad)
        settled()
    passed += 1
    print(f"PASS {case} status={result.returncode} calls={len(rows)}", flush=True)
    return output, time.monotonic() - start


if baseline:
    # New behavior: exit 1 plus a structured timeout is an instrument failure.
    run("timeouttree", 2, 2)
    raise AssertionError("baseline unexpectedly passed")

for case, expected, count in (
    ("clean", 0, 2), ("findingstree", 1, 2), ("deadcanary", 2, 1),
    ("malformedrules", 2, 1), ("wrongrulecanary", 2, 1),
    ("wrongtargetcanary", 2, 1), ("barecanary", 0, 2),
    ("absolutecanary", 0, 2), ("stderr", 0, 2),
    ("payload", 2, 2), ("many", 1, 2),
):
    output, _ = run(case, expected, count)
    if case == "payload":
        assert b"rule=valid target=tracked.py line=8" in output
        assert b"error=ScannerError" in output
    if case == "many":
        assert b"omitted=80" in output
for stage in ("canary", "tree"):
    for name in ("timeout", "mixed", "errors0", "errors1", "empty1",
                 "results0", "malformed", "overflowout", "overflowerr",
                 "raw2", "schema"):
        output, _ = run(name + stage, 2, 1 if stage == "canary" else 2)
        if name in ("timeout", "mixed"):
            assert b"raw=1" in output and b"error=Timeout" in output
            assert b"renderer_failure" not in output
        if name.startswith("overflow"):
            assert (b"stdout_overflow" if name == "overflowout"
                    else b"stderr_overflow") in output
output, elapsed = run("hold", 2, 1)
assert b"capture_incomplete" in output and b"raw=1" in output
assert 2 <= elapsed < 5, elapsed

# Capture interruption independently tests catchable signals, TERM-resistant
# direct children, KILL escalation, recorded status, and direct-child settlement.
for scenario, sig, expected_raw in (
    ("hang", signal.SIGINT, -signal.SIGKILL),
    ("hang", signal.SIGTERM, -signal.SIGKILL),
    ("hang", signal.SIGHUP, -signal.SIGKILL),
    ("termhang", signal.SIGTERM, -signal.SIGTERM),
):
    reset(scenario)
    directory = scratch / f"capture-{scenario}-{sig}"
    directory.mkdir(mode=0o700)
    local_env = env | {"SEMGREP_SETTINGS_FILE": str(directory / "settings.yml")}
    proc = subprocess.Popen(["python3", "-B", str(helper), "capture", str(directory),
                             "--", str(fake), "scripts/semgrep-canary.txt"],
                            cwd=repo, env=local_env, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE)
    deadline = time.monotonic() + 3
    while not owned.read_text() and time.monotonic() < deadline:
        time.sleep(0.02)
    assert owned.read_text(), "scanner did not start"
    proc.send_signal(sig)
    stdout, stderr = proc.communicate(timeout=4)
    assert proc.returncode == 2, (sig, stdout, stderr)
    status = json.loads((directory / "status").read_text())
    assert status == {"raw_status": expected_raw, "reason": "interrupted"}, status
    settled()
    passed += 1
    print(f"PASS interrupt signal={sig} raw={status['raw_status']}", flush=True)
    shutil.rmtree(directory)

# Capture seam: actual retained bytes stay under each independent limit,
# startup failure records missing raw evidence, and a capture write failure is
# never a completed scan. All requested argv below are literal fixture tools.
for case in ("overflowoutcanary", "overflowerrcanary", "startup", "collision"):
    reset(case)
    directory = scratch / f"capture-{case}"
    directory.mkdir(mode=0o700)
    if case == "collision":
        (directory / "stdout").write_text("collision")
    executable = str(fake) if case != "startup" else str(repo / "absent-scanner")
    result = subprocess.run(
        ["python3", "-B", str(helper), "capture", str(directory), "--",
         executable, "scripts/semgrep-canary.txt"], cwd=repo,
        env=env | {"SEMGREP_SETTINGS_FILE": str(directory / "settings.yml")},
        capture_output=True, timeout=4)
    assert result.returncode == 2, case
    status = json.loads((directory / "status").read_text())
    expected_reason = {
        "overflowoutcanary": "stdout_overflow",
        "overflowerrcanary": "stderr_overflow",
        "startup": "startup_failure", "collision": "capture_failure",
    }[case]
    assert status["reason"] == expected_reason, status
    if case in ("startup", "collision"):
        assert status["raw_status"] is None
    else:
        assert type(status["raw_status"]) is int
        assert (directory / "stdout").stat().st_size <= 32 * 1024 * 1024
        assert (directory / "stderr").stat().st_size <= 1024 * 1024
        for filename in ("stdout", "stderr", "status"):
            assert (directory / filename).stat().st_mode & 0o777 == 0o600
    settled()
    passed += 1
    print(f"PASS capture {case} reason={expected_reason}", flush=True)
    shutil.rmtree(directory)

# Shell forwards interruption to capture before its own directory cleanup.
reset("hang")
proc = subprocess.Popen(["sh", "scripts/semgrep-gate.sh", "rules",
                         "scripts/semgrep-canary.txt"], cwd=repo, env=env,
                        stdout=subprocess.PIPE, stderr=subprocess.PIPE)
deadline = time.monotonic() + 3
while not owned.read_text() and time.monotonic() < deadline:
    time.sleep(0.02)
assert owned.read_text()
proc.send_signal(signal.SIGTERM)
stdout, stderr = proc.communicate(timeout=4)
assert proc.returncode == 2 and b"interrupted" in stdout + stderr
settled()
assert not list((repo / ".uzi/scratch").iterdir())
passed += 1
print("PASS shell interrupt cleanup", flush=True)

# Report seam: bounded input before parse, missing raw evidence, and invalid JSON.
directory = scratch / "report"
directory.mkdir(mode=0o700)
for name, status, data in (
    ("oversize", {"raw_status": 0, "reason": "ok"}, b" " * (32 * 1024 * 1024 + 1)),
    ("missing-status", {"reason": "ok"}, b'{"results":[],"errors":[]}'),
    ("invalid-json", {"raw_status": 0, "reason": "ok"}, b"not-json"),
    ("missing-json", {"raw_status": 0, "reason": "ok"}, None),
):
    (directory / "status").write_text(json.dumps(status))
    if data is None:
        (directory / "stdout").unlink(missing_ok=True)
    else:
        (directory / "stdout").write_bytes(data)
    result = subprocess.run(["python3", "-B", str(helper), "report", str(directory),
                             "tree", "scripts/semgrep-canary.txt", "rules"],
                            cwd=repo, env=env, capture_output=True, timeout=4)
    assert result.returncode == 2, name
    passed += 1
    print(f"PASS report {name}", flush=True)
# Installed Semgrep CliError uses rule_id/path and spans[].file/start;
# CoreError uses location.path/start. Messages and source fields stay private.
secret = "glpat-" + "a" * 20
safe_span = {"file": str(repo / "tracked.py"), "start": {"line": 7},
             "end": {"line": 7}}
error_cases = [
    ({"rule_id": "fixture.rule", "path": "tracked.py"},
     b"error=ParseError rule=fixture.rule target=tracked.py"),
    ({"rule_id": "fixture.rule", "spans": [safe_span]},
     b"error=ParseError rule=fixture.rule target=tracked.py line=7"),
    ({"rule_id": "fixture.rule", "location": {
        "path": str(repo / "tracked.py"), "start": {"line": 9}}},
     b"error=ParseError rule=fixture.rule target=tracked.py line=9"),
    ({"rule_id": secret, "path": "/outside/private"}, b"error=ParseError\n"),
    ({"rule_id": "bad\x1b[31m", "spans": [
        {"file": "tracked.py\x1b[31m", "start": {"line": 1}}]},
     b"error=ParseError\n"),
    ({"rule_id": "print('SOURCE_PAYLOAD')", "location": {
        "path": secret, "start": {"line": 1}}}, b"error=ParseError\n"),
    ({"location": {"path": "../tracked.py", "start": {"line": 1}}},
     b"error=ParseError\n"),
    ({"spans": [{"file": "tracked.py", "start": {"line": True}}]},
     b"error=ParseError target=tracked.py\n"),
]
for fields, expected in error_cases:
    (directory / "status").write_text(json.dumps({"raw_status": 1, "reason": "ok"}))
    (directory / "stdout").write_text(json.dumps({
        "results": [], "errors": [dict(type="ParseError", message="SOURCE_PAYLOAD",
                                     long_msg=secret, **fields)]}))
    result = subprocess.run(["python3", "-B", str(helper), "report", str(directory),
                             "tree", "scripts/semgrep-canary.txt", "rules"],
                            cwd=repo, env=env, capture_output=True, timeout=4)
    output = result.stdout + result.stderr
    assert result.returncode == 2 and expected in output, output
    for bad in (b"SOURCE_PAYLOAD", secret.encode(), b"\x1b", b"/outside/private",
                str(repo).encode(), b"renderer_failure"):
        assert bad not in output, (fields, bad, output)
    passed += 1
print(f"PASS report safe error fields cases={len(error_cases)}", flush=True)
# Long but valid safe fields exercise the output byte budget independently of
# the entry count. This target belongs to this fixture's index alone.
long_target = "target-" + "a" * 220 + ".py"
(repo / long_target).write_text("fixture")
subprocess.run(["git", "-C", str(repo), "add", "--", long_target],
               env=git_env, check=True)
(directory / "status").write_text(json.dumps({"raw_status": 1, "reason": "ok"}))
(directory / "stdout").write_text(json.dumps({
    "results": [{"check_id": "r" * 160, "path": long_target,
                 "start": {"line": 10000000}}] * 100, "errors": []}))
result = subprocess.run(["python3", "-B", str(helper), "report", str(directory),
                         "tree", "scripts/semgrep-canary.txt", "rules"],
                        cwd=repo, env=env, capture_output=True, timeout=4)
assert result.returncode == 1 and len(result.stdout) <= 8192, result.stderr
printed = result.stdout.count(b"  finding")
assert 0 < printed <= 20
assert f"omitted={100 - printed}".encode() in result.stdout
passed += 1
print(f"PASS report byte budget bytes={len(result.stdout)} entries={printed}", flush=True)
shutil.rmtree(directory)

# Helper launch, syntax and renderer failures publish only fixed diagnostics.
# Test both invocations: replacing the on-disk helper after capture leaves the
# already-loaded capture process intact, then breaks the report invocation.
private_payload = "SOURCE_PAYLOAD /outside/private " + str(repo) + " " + secret
syntax_source = "def SOURCE_PAYLOAD /outside/private\n"
for scenario, replacement, count in (
    ("absent", None, 0),
    ("unreadable-kind", "directory", 0),
    ("syntax", syntax_source, 0),
    ("capture-exception", helper_source.replace(
        '            return capture(directory, sys.argv[4:])',
        '            raise RuntimeError(' + repr(private_payload) + ')'), 0),
    ("report-syntax", helper_source.replace(
        '    return 0 if reason == "ok" else 2',
        '    Path(__file__).write_text(' + repr(syntax_source) + ')\n'
        '    return 0 if reason == "ok" else 2'), 1),
    ("renderer-exception", helper_source.replace(
        "    root = Path.cwd().resolve()",
        "    raise RuntimeError(" + repr(private_payload) + ")\n"
        "    root = Path.cwd().resolve()", 1), 1),
    ("renderer-empty", helper_source.replace(
        "            return report(directory, stage, canary, config)",
        "            return 0"), 1),
    ("renderer-exit", helper_source.replace(
        "            return report(directory, stage, canary, config)",
        "            sys.stderr.write(" + repr(private_payload + "\x1b[31m") + ")\n"
        "            return 3"), 1),
):
    helper.unlink()
    if replacement == "directory":
        helper.mkdir()
    elif replacement is not None:
        helper.write_text(replacement)
    output, _ = run("clean", 2, count)
    assert b"renderer_failure" in output, (scenario, output)
    for bad in (b"verdict=0", b"verdict=1", b"results=0 errors=0",
                str(repo).encode(), b"SyntaxError", b"Traceback"):
        assert bad not in output, (scenario, bad, output)
    print(f"PASS helper {scenario} instrument=2 cleanup=0", flush=True)
    if helper.is_dir():
        helper.rmdir()
        helper.write_text(helper_source)
    elif not helper.exists():
        helper.write_text(helper_source)
helper.write_text(helper_source)

# Tool absence is isolated from the worker PATH, with only wrapper prerequisites.
missing_bin = repo / "missing-bin"
missing_bin.mkdir()
for tool in ("sh", "git"):
    (missing_bin / tool).symlink_to(shutil.which(tool))
for required, expected in (("0", 0), ("1", 2)):
    reset("clean")
    result = subprocess.run([str(missing_bin / "sh"), "scripts/semgrep-gate.sh",
                             "rules", "scripts/semgrep-canary.txt"],
                            cwd=repo, env=env | {"PATH": str(missing_bin),
                                                "UZI_SAST_REQUIRED": required},
                            capture_output=True, timeout=4)
    assert result.returncode == expected, result.stderr
    assert not records()
    if expected == 0:
        assert b"SKIPPED" in result.stdout
    passed += 1
    print(f"PASS missing tool required={required} status={expected}", flush=True)

# Existing tracked-canary and rules guards remain before scanner launch.
subprocess.run(["git", "-C", str(repo), "rm", "--cached", "-q",
                "scripts/semgrep-canary.txt"], env=git_env, check=True)
run("clean", 2, 0)
subprocess.run(["git", "-C", str(repo), "add", "scripts/semgrep-canary.txt"],
               env=git_env, check=True)
shutil.rmtree(repo / "rules")
run("clean", 2, 0)
print(f"PASS total={passed}", flush=True)
PY
