#!/usr/bin/env bash
# Regression test for backup-runs.sh's bundle decision.
#
# The bug (observed on run #1349, 2026-09-14): when a run has committed nothing
# beyond public main (its work is still uncommitted), `git bundle create BR --not
# BASE` refuses an empty bundle, and the old `|| git bundle create BR` fallback
# dumped the FULL repo history (tens of MB, every byte already on the forge).
# The fix skips the bundle entirely in that case; the uncommitted.patch carries
# the work and the run logs PART.
#
# This drives the REAL backup-runs.sh end to end with stub `kubectl`/`uzi` on
# PATH and a local fake runner clone (UZI_RUNNER_BASE), asserting:
#   - uncommitted-only work  -> .tgz has NO .bundle member, log says PART
#   - one committed commit   -> .tgz HAS a .bundle member,  log says OK
# Run: bash backup-runs.test.sh   (exit 0 = pass)
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/backup-runs.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# fail <msg>: print and abort the test run non-zero.
fail() { echo "FAIL: $*" >&2; exit 1; }

# git_q <dir> <git-args...>: run git in <dir>, silencing all output.
git_q() { git -C "$1" "${@:2}" >/dev/null 2>&1; }

# --- "the forge": a base repo whose HEAD is public main -------------------------
FORGE="$WORK/forge"
mkdir -p "$FORGE"
git_q "$FORGE" init
git -C "$FORGE" config user.email t@example.com
git -C "$FORGE" config user.name tester
echo base > "$FORGE/f.txt"
git_q "$FORGE" add f.txt
git_q "$FORGE" commit -m base
MAIN="$(git -C "$FORGE" rev-parse HEAD)"

# --- stubs ----------------------------------------------------------------------
# kubectl stub: answers the three calls backup-runs.sh makes. The capture exec is
# run verbatim, so the RUNNER_BASE the host forwarded as its last arg is what
# points CLONE at the fake clone — this exercises the real host->exec argument
# path, not a stub-injected env var (which would not cross a real `kubectl exec`).
KSTUB="$WORK/kubectl"
cat > "$KSTUB" <<STUB
#!/usr/bin/env bash
set -u
args=("\$@")
# Drop everything up to and including "--" for exec commands.
cmd=()
seen=0
for a in "\${args[@]}"; do
  if [ "\$seen" = 1 ]; then cmd+=("\$a"); continue; fi
  [ "\$a" = "--" ] && seen=1
done
case " \${args[*]} " in
  *" config current-context "*) echo test-ctx; exit 0 ;;
  *" get pods "*)               echo "pod/uzi-hw-${WID:-w0rker}-test"; exit 0 ;;
esac
# exec ...: distinguish the REALMAIN git read from the capture sh -c.
if [ "\${cmd[0]:-}" = git ]; then
  # rev-parse origin/main from the bare repo -> the true public main
  echo "$MAIN"; exit 0
fi
if [ "\${cmd[0]:-}" = sh ]; then
  # cmd = (sh -c <CAPTURE> _ <STEM> <REALMAIN> <RUNNER_BASE>); the host already
  # forwarded RUNNER_BASE as the last arg, so run it verbatim.
  exec "\${cmd[@]}"
fi
exit 0
STUB
chmod +x "$KSTUB"

# make_uzi_stub: write a fake `uzi` that answers `run get --json` for the test run.
make_uzi_stub() {
  cat > "$WORK/uzi" <<STUB
#!/usr/bin/env bash
set -u
if [ "\${1:-}" = run ] && [ "\${2:-}" = get ]; then
  printf '%s' '{"status":"running","issue_iid":4242,"kind":"issue","worker_id":"${WID:-w0rker}","mr_web_url":null,"health_reason":"ok","anthropic_secret_label":"tok","anthropic_bind_mode":"default","milestones":[],"milestones_completed":[]}'
  exit 0
fi
# run logs / anything else: quiet success
exit 0
STUB
  chmod +x "$WORK/uzi"
}

# run_backup <tag>: run the real script once into a fresh dir; echo its `latest`.
# UZI_RUNNER_BASE is set on the HOST invocation (not the stub): the script must
# forward it into the remote capture, which is the production behavior under test.
run_backup() {
  local dest="$WORK/out.$1"
  rm -rf "$dest"
  UZI_CTX=test-ctx UZI_WORKER_NS=ns UZI_REPO_SLUG=testrepo UZI_RUNNER_BASE="$WORK/runner" \
    UZI_BACKUP_DIR="$dest" UZI_KUBECTL="$KSTUB" UZI_BIN="$WORK/uzi" \
    bash "$SCRIPT" run-4242 >/dev/null 2>&1
  echo "$dest/latest"
}

WID=w0rker
make_uzi_stub

# --- case 1: uncommitted-only work -> NO bundle, PART ---------------------------
RUNNER="$WORK/runner/issue-4242"
rm -rf "$WORK/runner"; mkdir -p "$WORK/runner"
git clone -q "$FORGE" "$RUNNER"
git -C "$RUNNER" config user.email t@example.com
git -C "$RUNNER" config user.name tester
git_q "$RUNNER" checkout -b agent/issue-4242
echo dirty >> "$RUNNER/f.txt"   # uncommitted change only

L1="$(run_backup 1)"
[ -f "$L1/issue-4242.tgz" ] || fail "case1: no .tgz produced"
if tar tzf "$L1/issue-4242.tgz" 2>/dev/null | grep -q 'issue-4242[.]bundle'; then
  fail "case1: .tgz contains a bundle (full-history dump not skipped)"
fi
grep -q "^.*PART .*run-4242" "$L1/backup.log" || fail "case1: expected PART in log; got: $(cat "$L1/backup.log")"
tar tzf "$L1/issue-4242.tgz" 2>/dev/null | grep -q 'issue-4242[.]uncommitted[.]patch' \
  || fail "case1: uncommitted.patch missing (the work was not captured)"
echo "PASS case1: uncommitted-only -> no bundle, PART, patch kept"

# --- case 2: one committed commit -> a (small) bundle, OK -----------------------
git_q "$RUNNER" checkout f.txt   # drop the uncommitted change
echo more >> "$RUNNER/f.txt"
git_q "$RUNNER" commit -am work

L2="$(run_backup 2)"
[ -f "$L2/issue-4242.tgz" ] || fail "case2: no .tgz produced"
tar tzf "$L2/issue-4242.tgz" 2>/dev/null | grep -q 'issue-4242[.]bundle' \
  || fail "case2: expected a .bundle member for a run with a new commit"
grep -q "^.*OK .*run-4242" "$L2/backup.log" || fail "case2: expected OK in log; got: $(cat "$L2/backup.log")"
# The base-excluded bundle must carry just the new commit, not full history.
BUN="$WORK/c2"; rm -rf "$BUN"; mkdir -p "$BUN"
tar xzf "$L2/issue-4242.tgz" -C "$BUN" ./issue-4242.bundle 2>/dev/null
if git -C "$FORGE" bundle verify "$BUN/issue-4242.bundle" 2>&1 | grep -q 'complete history'; then
  fail "case2: bundle records complete history (base was not excluded)"
fi
echo "PASS case2: one commit -> base-excluded bundle, OK"

echo "ALL PASS"
