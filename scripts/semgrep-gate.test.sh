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
                                          "many", "hold", "closed", "hang", "termhang")
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
if case in ("hold", "closed"):
    recorded = len(Path(os.environ["OWNED"]).read_text().splitlines())
    pid = os.fork()
    if pid == 0:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        with open(os.environ["OWNED"], "a") as f:
            f.write(str(os.getpid()) + "\n")
        if case == "closed":
            os.close(1)
            os.close(2)
        while True:
            time.sleep(0.1)
    deadline = time.monotonic() + 2
    while len(Path(os.environ["OWNED"]).read_text().splitlines()) <= recorded:
        if time.monotonic() >= deadline:
            sys.exit(2)
        time.sleep(0.01)
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


def gone(pid, proc_root=Path("/proc")):
    # Identity-level check of saved OWNED handles, never enumerate processes.
    try:
        if not (proc_root / "self/stat").is_file():
            raise RuntimeError("OWNED verification unavailable: procfs required")
        state = (proc_root / str(pid) / "stat").read_text().split(") ", 1)[1].split()[0]
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


# Missing procfs cannot turn every recorded live handle into a settled handle.
try:
    gone(os.getpid(), scratch / "absent-proc")
except RuntimeError:
    print("PASS unavailable procfs verification fails explicitly", flush=True)
else:
    raise AssertionError("absent procfs falsely settled a live handle")
assert not gone(os.getpid()), "current recorded handle must be live"

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
output, elapsed = run("closed", 0, 2)
assert elapsed < 5, elapsed
assert len(owned.read_text().splitlines()) == 4
print("PASS closed-pipes TERM-resistant descendants settled", flush=True)

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

# Publishing failure must stop before a clean verdict or second scanner call.
cat = repo / "bin/cat"
cat.write_text("#!/bin/sh\nexit 1\n")
cat.chmod(0o755)
output, _ = run("clean", 2, 1)
assert output.strip() == b"semgrep-gate: reason=renderer_failure"
print("PASS final cat failure status=2 calls=1 cleanup=0", flush=True)
fixture_wrapper = repo / "scripts/semgrep-gate.sh"
wrapper_source = fixture_wrapper.read_text()
guard = '''      if ! cat "$stage_dir/report"; then
        echo 'semgrep-gate: reason=renderer_failure' >&2
        return 2
      fi'''
assert guard in wrapper_source
fixture_wrapper.write_text(wrapper_source.replace(guard, '      cat "$stage_dir/report"'))
try:
    run("clean", 2, 1)
except AssertionError as error:
    assert "expected=2 actual=0" in str(error), error
    settled()
    assert len(records()) == 2
    assert not list((repo / ".uzi/scratch").iterdir())
    print("PASS sensitivity cat guard mutation RED actual=0 expected=2", flush=True)
else:
    raise AssertionError("cat guard mutation escaped regression")
finally:
    fixture_wrapper.write_text(wrapper_source)
    cat.unlink()

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
# Provider-shaped common credentials are omitted on every rule/path surface.
# Prefix/body fragments remain separate in source, including npm's 36-char body.
providers = ["np" + "m_", "xapp-", "xoxe-",
             *["gl" + family + "-" for family in
               ("oas", "rt", "cbt", "ptt", "soat", "imt", "agent", "dt")],
             *["uz" + family + "_" for family in "capfrsw"]]
for prefix in providers:
    token = prefix + "a1" * 18
    token_path = token + ".py"
    (repo / token_path).write_text("fixture")
    subprocess.run(["git", "-C", str(repo), "add", "--", token_path],
                   env=git_env, check=True)
    (directory / "stdout").write_text(json.dumps({
        "results": [
            {"check_id": token, "path": "tracked.py", "start": {"line": 1}},
            {"check_id": "fixture.rule", "path": token_path, "start": {"line": 1}}],
        "errors": [
            {"type": "ParseError", "rule_id": token, "path": token_path},
            {"type": "ParseError", "location": {"path": token_path}},
            {"type": "ParseError", "spans": [{"file": token_path}]}]}))
    result = subprocess.run(["python3", "-B", str(helper), "report", str(directory),
                             "tree", "scripts/semgrep-canary.txt", "rules"],
                            cwd=repo, env=env, capture_output=True, timeout=4)
    assert result.returncode == 2 and token.encode() not in result.stdout, prefix
    assert b"renderer_failure" not in result.stdout
    if prefix == providers[0]:
        # Retiring just npm suppression must fail this exact rule/path fixture.
        mutant = helper_source.replace("|npm_|", "|")
        assert mutant != helper_source
        helper.write_text(mutant)
        try:
            leaked = subprocess.run(
                ["python3", "-B", str(helper), "report", str(directory), "tree",
                 "scripts/semgrep-canary.txt", "rules"],
                cwd=repo, env=env, capture_output=True, timeout=4)
            assert leaked.returncode == 2
            assert b"rule=" + token.encode() in leaked.stdout
            assert b"target=" + token_path.encode() in leaked.stdout
            print("PASS sensitivity npm suppression mutation RED rule/path leaked",
                  flush=True)
        finally:
            helper.write_text(helper_source)
    passed += 1
print(f"PASS provider suppression rule/path/error surfaces families={len(providers)}",
      flush=True)

# Status-only targeted Git checks: no full index capture, even if a whole-list
# query would emit more than the scanner JSON bound. All subprocess argv is fixed.
real_git = shutil.which("git", path=os.environ["PATH"])
git_calls = scratch / "git-calls"
fake_git = repo / "bin/git"
fake_git.write_text(r'''#!/usr/bin/env python3
import json
import os
import stat
import subprocess
import sys
import time
args = sys.argv[1:]
with open(os.environ["GIT_CALLS"], "a") as output:
    output.write(json.dumps(args) + "\n")
if args == ["ls-files", "-z"]:
    os.write(1, b"tracked.py\0" * 3500000)
    sys.exit(0)
assert args[:5] == ["--literal-pathspecs", "ls-files", "--error-unmatch", "--",
                   args[-1]] and len(args) == 5
assert stat.S_ISCHR(os.fstat(1).st_mode) and stat.S_ISCHR(os.fstat(2).st_mode)
mode = os.environ.get("LOOKUP_MODE")
if mode == "failure":
    sys.exit(128)
if mode == "timeout":
    time.sleep(2)
sys.exit(subprocess.run([os.environ["REAL_GIT"], *args]).returncode)
''')
fake_git.chmod(0o755)
lookup_env = env | {"GIT_CALLS": str(git_calls), "REAL_GIT": real_git}


def lookup_report(data, raw=0, stage="tree", mode=None):
    git_calls.write_text("")
    (directory / "status").write_text(json.dumps({"raw_status": raw, "reason": "ok"}))
    (directory / "stdout").write_text(json.dumps(data))
    result = subprocess.run(["python3", "-B", str(helper), "report", str(directory),
                             stage, "scripts/semgrep-canary.txt", "rules"],
                            cwd=repo, env=lookup_env | ({"LOOKUP_MODE": mode} if mode else {}),
                            capture_output=True, timeout=6)
    queries = [json.loads(line) for line in git_calls.read_text().splitlines()]
    return result, queries


clean = {"results": [], "errors": []}
result, queries = lookup_report(clean)
assert result.returncode == 0 and len(queries) == 1, (result.stdout, queries)
assert queries[0][-1] == "scripts/semgrep-canary.txt"
print("PASS oversized-index clean status=0 targeted queries=1 DEVNULL", flush=True)

# Reintroduce the retired enumeration without depending on Git history (shallow
# CI clones must run this fixture too). The actual old reporter was also probed
# during rework; this mutation keeps its unbounded buffering failure reproducible.
enumeration = '''    root = Path.cwd().resolve()
    tracked_list = set(subprocess.check_output(
        ["git", "ls-files", "-z"], stderr=subprocess.DEVNULL
    ).decode("utf-8").split("\\0"))
'''
mutant = helper_source.replace("    root = Path.cwd().resolve()\n", enumeration)
assert mutant != helper_source
helper.write_text(mutant)
try:
    result, queries = lookup_report(clean)
    assert result.returncode == 0 and queries[0] == ["ls-files", "-z"], (
        result.stdout, queries)
    print("PASS sensitivity enumeration mutation RED full-list buffered >32MiB clean=0",
          flush=True)
finally:
    helper.write_text(helper_source)

for mode in ("failure", "timeout"):
    result, queries = lookup_report(clean, mode=mode)
    assert result.returncode == 2 and b"reason=tracked_lookup_failure" in result.stdout
    assert b"verdict=0" not in result.stdout and len(queries) == 1
    print(f"PASS lookup {mode} status=2 queries=1", flush=True)

targets = [f"candidate-{index}.py" for index in range(40)]
for target in targets:
    (repo / target).write_text("fixture")
subprocess.run([real_git, "-C", str(repo), "add", "--", *targets],
               env=git_env, check=True)
results = [{"check_id": "rules.semgrep-canary", "path": targets[index % 40],
            "start": {"line": 1}} for index in range(10000)]
result, queries = lookup_report({"results": results, "errors": []}, 1, "canary")
assert result.returncode == 2 and len(queries) == 21, (result.stdout, queries)
assert all(query[-1] in targets[:20] or query[-1] == "scripts/semgrep-canary.txt"
           for query in queries)
print("PASS liveness 10000 noncanary targets query cap=21", flush=True)
errors = [{"type": "ParseError", "path": targets[20 + index],
           "location": {"path": targets[index]}} for index in range(20)]
result, queries = lookup_report({"results": results, "errors": errors}, 1)
assert result.returncode == 2 and len(queries) == 21
assert all(query[-1] not in targets[20:] for query in queries)
print("PASS error/result display query cap=21 single selected location", flush=True)
errors = [{"type": "ParseError", "rule_id": "r" * 160,
           "location": {"path": long_target, "start": {"line": 10000000}}}] * 20
result, queries = lookup_report({"results": results, "errors": errors}, 1)
assert result.returncode == 2 and len(queries) == 2
assert all(query[-1] in (long_target, "scripts/semgrep-canary.txt") for query in queries)
assert result.stdout.count(b"  error=") < 20
print("PASS byte omissions do not expand candidate query population", flush=True)

(repo / "untracked.py").write_text("fixture")
(repo / "directory").mkdir()
unsafe = ["untracked.py", "candidate-*.py", ":(glob)candidate-*.py", "directory",
          "../tracked.py"]
result, queries = lookup_report({
    "results": [{"check_id": "fixture.rule", "path": target, "start": {"line": 1}}
                for target in unsafe], "errors": []}, 1)
assert result.returncode == 1 and b"target=" not in result.stdout
assert len(queries) == 2 and queries[-1][-1] == "untracked.py"
print("PASS untracked omission literal/wildcard/directory path rejection", flush=True)
fake_git.unlink()
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
