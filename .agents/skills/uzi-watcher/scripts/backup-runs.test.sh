#!/usr/bin/env bash
# Regression tests for backup-runs.sh's recovery-source and publication decisions.
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
#   - missing live clone     -> durable runner ref becomes a BARE bundle
#   - missing clone + ref    -> nonzero, last recoverable `latest` is preserved
#   - retention              -> only old timestamp-shaped directories are pruned
#   - attempt layout (cases 9-18) -> the clone is chosen by journal/ledger identity
#     plus branch, never by the newest <stem>.attempt-* path; none valid -> BARE;
#     a tab-bearing dir name cannot forge the branch field; same-second attemptIds
#     order by numeric generation (gx lowest); a trailing slash on the base is ignored
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
REPOS="$WORK/repos"
BARE="$REPOS/testrepo.git"
mkdir -p "$REPOS"
git clone -q --bare "$FORGE" "$BARE"
git --git-dir="$BARE" update-ref refs/remotes/origin/main "$MAIN"

# --- stubs ----------------------------------------------------------------------
# kubectl stub: answers the calls backup-runs.sh makes. Both on-pod scripts are run
# verbatim: the candidate listing receives the RUNNER_BASE the host forwarded, and
# the capture receives the concrete clone the host selected (CLONE, its last arg).
# This exercises the real host->exec argument path, not a stub-injected env var
# (which would not cross a real `kubectl exec`).
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
  *" get pods "*) [ "\${UZI_TEST_GETFAIL:-}" = 1 ] && exit 1
                                echo "pod/uzi-hw-${WID:-w0rker}-test"; exit 0 ;;
esac
# exec ...: distinguish the REALMAIN git read from the capture sh -c.
# A transport failure: the exec never ran the probe, so nothing reaches stdout.
[ "\${UZI_TEST_EXECFAIL:-}" = 1 ] && exit 1
if [ "\${cmd[0]:-}" = git ]; then
  # Execute the real bare-repo probes. This covers the public-main read and the
  # missing-clone fallback's show-ref lookup against the local fake worker bare.
  exec "\${cmd[@]}"
fi
if [ "\${cmd[0]:-}" = sh ]; then
  # cmd = (sh -c <LIST_CANDIDATES> _ <RUNNER_BASE> <STEM>) or
  # (sh -c <CAPTURE> _ <STEM> <REALMAIN> <CLONE>); the host already forwarded the
  # runner base / concrete clone as arguments, so run it verbatim.
  if [ "\${UZI_TEST_DUBIOUS:-}" = 1 ]; then export GIT_TEST_ASSUME_DIFFERENT_OWNER=1; fi
  exec "\${cmd[@]}"
fi
exit 0
STUB
chmod +x "$KSTUB"

# stat stub: reproduce GNU stat's surprising contract on every host. `stat -f %m`
# exits 0 but prints non-numeric filesystem information; `-c %Y` returns the real
# epoch. This makes the Linux CI failure a deterministic local regression too.
REAL_STAT="$(command -v stat)"
cat > "$WORK/stat" <<STUB
#!/usr/bin/env bash
if [ "\${1:-}" = -f ]; then echo 'filesystem report, not an epoch'; exit 0; fi
if [ "\${1:-}" = -c ]; then
  value="\$("$REAL_STAT" -c %Y "\${3:?}" 2>/dev/null || true)"
  if ! printf '%s' "\$value" | grep -Eq '^[0-9]+\$'; then value="\$("$REAL_STAT" -f %m "\${3:?}")"; fi
  printf '%s\n' "\$value"
  exit 0
fi
exec "$REAL_STAT" "\$@"
STUB
chmod +x "$WORK/stat"

# make_uzi_stub: write a fake `uzi` that answers `run get --json`. A run id containing
# "mrr" reports a mr_rework run EXACTLY as the real API does IN-FLIGHT: branch is null
# and the live branch is in pipeline_ref (agent/issue-9999), so the slug must come from
# pipeline_ref (-> agent-issue-9999); "mrrflat" is a mr_rework on the slash-free branch
# "hotfix" (stem hotfix); anything else is the issue-4242 run.
make_uzi_stub() {
  cat > "$WORK/uzi" <<STUB
#!/usr/bin/env bash
set -u
if [ "\${1:-}" = run ] && [ "\${2:-}" = get ]; then
  case "\${3:-}" in
    *mrrflat*) printf '%s' '{"status":"running","issue_iid":null,"kind":"mr_rework","branch":null,"pipeline_ref":"hotfix","worker_id":"${WID:-w0rker}","mr_iid":7778,"mr_web_url":null,"health_reason":"ok","anthropic_secret_label":"tok","anthropic_bind_mode":"default","milestones":[],"milestones_completed":[]}' ;;
    *mrr*) printf '%s' '{"status":"running","issue_iid":null,"kind":"mr_rework","branch":null,"pipeline_ref":"agent/issue-9999","worker_id":"${WID:-w0rker}","mr_iid":7777,"mr_web_url":null,"health_reason":"ok","anthropic_secret_label":"tok","anthropic_bind_mode":"default","milestones":[],"milestones_completed":[]}' ;;
    *failed*) printf '%s' '{"status":"failed","issue_iid":4242,"kind":"issue","branch":null,"pipeline_ref":null,"worker_id":"${WID:-w0rker}","mr_web_url":null,"health_reason":"ok","anthropic_secret_label":"tok","anthropic_bind_mode":"default","milestones":[],"milestones_completed":[]}' ;;
    *cancelled*) printf '%s' '{"status":"cancelled","issue_iid":4242,"kind":"issue","worker_id":"${WID:-w0rker}","milestones":[],"milestones_completed":[]}' ;;
    *completed*) printf '%s' '{"status":"completed","issue_iid":4242,"kind":"issue","branch":null,"pipeline_ref":null,"worker_id":"${WID:-w0rker}","mr_web_url":null,"health_reason":"ok","anthropic_secret_label":"tok","anthropic_bind_mode":"default","milestones":[],"milestones_completed":[]}' ;;
    *noworker*) printf '%s' '{"status":"running","issue_iid":4242,"kind":"issue","branch":null,"pipeline_ref":null,"worker_id":null,"mr_web_url":null,"health_reason":"ok","anthropic_secret_label":"tok","anthropic_bind_mode":"default","milestones":[],"milestones_completed":[]}' ;;
    *queuedbound*) printf '%s' '{"status":"queued","issue_iid":4242,"kind":"issue","branch":null,"pipeline_ref":null,"worker_id":"${WID:-w0rker}","mr_web_url":null,"health_reason":"ok","anthropic_secret_label":"tok","anthropic_bind_mode":"default","milestones":[],"milestones_completed":[]}' ;;
    *queued*) printf '%s' '{"status":"queued","issue_iid":4242,"kind":"issue","branch":null,"pipeline_ref":null,"worker_id":null,"mr_web_url":null,"health_reason":"ok","anthropic_secret_label":"tok","anthropic_bind_mode":"default","milestones":[],"milestones_completed":[]}' ;;
    *)     printf '%s' '{"status":"running","issue_iid":4242,"kind":"issue","branch":null,"pipeline_ref":null,"worker_id":"${WID:-w0rker}","mr_web_url":null,"health_reason":"ok","anthropic_secret_label":"tok","anthropic_bind_mode":"default","milestones":[],"milestones_completed":[]}' ;;
  esac
  exit 0
fi
# run logs / anything else: quiet success
exit 0
STUB
  chmod +x "$WORK/uzi"
}

# run_backup <tag> [runid]: run the real script once into a fresh dir; echo its `latest`.
# UZI_RUNNER_BASE (default $WORK/runner, override via TEST_RUNNER_BASE) is set on the
# HOST invocation, not the stub: the script forwards it into the on-pod candidate
# listing and passes the concrete selected clone to the capture, which is the
# production behavior under test.
run_backup() {
  local dest="$WORK/out.$1"; local rid="${2:-run-4242}"
  rm -rf "$dest"
  UZI_CTX=test-ctx UZI_WORKER_NS=ns UZI_REPO_SLUG=testrepo \
    UZI_RUNNER_BASE="${TEST_RUNNER_BASE:-$WORK/runner}" \
    UZI_REPOS_BASE="$REPOS" \
    UZI_BACKUP_DIR="$dest" UZI_KUBECTL="$KSTUB" UZI_BIN="$WORK/uzi" \
    bash "$SCRIPT" "$rid" >/dev/null 2>&1
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
git --git-dir="$BARE" config 'uzi-recovery.agent/issue-4242.clone' \
  "{\"runId\":\"run-4242\",\"clonePath\":\"$RUNNER\"}"
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

# The worker container can exec as a different UID than the runner clone owner.
# Git then refuses every clone operation unless this exact clone is trusted.
L1_DUBIOUS="$(UZI_TEST_DUBIOUS=1 run_backup dubious)"
[ -f "$L1_DUBIOUS/issue-4242.tgz" ] || fail "dubious owner: no archive"
tar -xOzf "$L1_DUBIOUS/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qF '+dirty' \
  || fail "dubious owner: live uncommitted work was not captured"
tar -xOzf "$L1_DUBIOUS/issue-4242.tgz" ./issue-4242.meta.txt | grep -qF "head=$MAIN" \
  || fail "dubious owner: Git HEAD was not captured"
echo "PASS dubious owner: exact clone trusted; patch and HEAD kept"

# A requeued run keeps its worker binding and may still have uncommitted work
# in the old clone. It must get an archive, not the never-claimed status-only path.
git --git-dir="$BARE" config 'uzi-recovery.agent/issue-4242.clone' \
  "{\"runId\":\"queuedbound-1\",\"clonePath\":\"$RUNNER\"}"
L1_QUEUED_BOUND="$(run_backup 1.queuedbound queuedbound-1)"
[ -f "$L1_QUEUED_BOUND/issue-4242.tgz" ] || fail "bound queued: no archive"
tar -xOzf "$L1_QUEUED_BOUND/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qF '+dirty' \
  || fail "bound queued: uncommitted work was not captured"
git --git-dir="$BARE" config 'uzi-recovery.agent/issue-4242.clone' \
  "{\"runId\":\"run-4242\",\"clonePath\":\"$RUNNER\"}"
echo "PASS bound queued: live clone work captured after requeue"

# A just-failed run keeps its worker binding while recovery custody holds the worker,
# so its clone is still the only copy of its uncommitted work: capture it.
git --git-dir="$BARE" config 'uzi-recovery.agent/issue-4242.clone' \
  "{\"runId\":\"failed-1\",\"clonePath\":\"$RUNNER\"}"
L1_FAILED="$(run_backup 1.failed failed-1)"
[ -f "$L1_FAILED/issue-4242.tgz" ] || fail "failed run: no archive; got: $(cat "$L1_FAILED/backup.log")"
tar -xOzf "$L1_FAILED/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qF '+dirty' \
  || fail "failed run: uncommitted work was not captured"
# Once the failed run's source is gone (the clone now belongs to another run and no
# tracking ref is owned by it), it is status-only and the cycle still succeeds.
git --git-dir="$BARE" config 'uzi-recovery.agent/issue-4242.clone' \
  "{\"runId\":\"run-4242\",\"clonePath\":\"$RUNNER\"}"
# backup_rc <dest> <rid> [VAR=value...]: run the real script once with extra env; print its exit code.
backup_rc() {
  local rc=0
  rm -rf "$1"
  env UZI_CTX=test-ctx UZI_WORKER_NS=ns UZI_REPO_SLUG=testrepo UZI_RUNNER_BASE="$WORK/runner" \
    UZI_REPOS_BASE="$REPOS" UZI_BACKUP_DIR="$1" UZI_KUBECTL="$KSTUB" UZI_BIN="$WORK/uzi" "${@:3}" \
    bash "$SCRIPT" "$2" >/dev/null 2>&1 || rc=$?
  echo "$rc"
}
D_FAILED_GONE="$WORK/out.1.failedgone"
rc="$(backup_rc "$D_FAILED_GONE" failed-2)"
[ "$rc" -eq 0 ] || fail "failed run without a source: exit $rc, want 0"
grep -q 'SNAP failed-2 .*status=failed' "$D_FAILED_GONE/latest-attempt/backup.log" \
  || fail "failed run without a source: expected SNAP; got: $(cat "$D_FAILED_GONE/latest-attempt/backup.log")"
[ ! -e "$D_FAILED_GONE/latest-attempt/issue-4242.tgz" ] || fail "failed run without a source: captured another run's clone"
# An inconclusive search (the pod listing or an exec failed) is not absence: the
# failed run's worker may still hold the only copy, so the cycle must fail and retry.
for mode in GETFAIL EXECFAIL; do
  D_INC="$WORK/out.1.failed.$mode"
  rc="$(backup_rc "$D_INC" failed-2 "UZI_TEST_$mode=1")"
  [ "$rc" -eq 1 ] || fail "failed run, $mode: exit $rc, want 1"
  grep -q 'FAIL failed-2 .*inconclusive' "$D_INC/latest-attempt/backup.log" \
    || fail "failed run, $mode: expected an inconclusive FAIL; got: $(cat "$D_INC/latest-attempt/backup.log")"
  [ ! -e "$D_INC/latest-attempt/.probe-inconclusive" ] || fail "failed run, $mode: probe marker left behind"
done
# A probe that runs but errors is not absence either: an unreadable bare repository
# (git exits 128, not 1) and a candidate clone whose .git cannot be read.
mkdir -p "$WORK/badrepos/testrepo.git"
BADGIT="$WORK/runner/issue-4242.attempt-20260101T000000Z-g1-0123456789abcdef"
mkdir -p "$BADGIT"; echo 'gitdir: /nonexistent' > "$BADGIT/.git"
for mode in badbare badclone; do
  D_INC="$WORK/out.1.failed.$mode"
  if [ "$mode" = badbare ]; then
    rm -rf "$BADGIT"
    rc="$(backup_rc "$D_INC" failed-2 "UZI_REPOS_BASE=$WORK/badrepos")"
  else
    mkdir -p "$BADGIT"; echo 'gitdir: /nonexistent' > "$BADGIT/.git"
    rc="$(backup_rc "$D_INC" failed-2)"
  fi
  [ "$rc" -eq 1 ] || fail "failed run, $mode: exit $rc, want 1"
  grep -q 'FAIL failed-2 .*inconclusive' "$D_INC/latest-attempt/backup.log" \
    || fail "failed run, $mode: expected an inconclusive FAIL; got: $(cat "$D_INC/latest-attempt/backup.log")"
done
rm -rf "$BADGIT" "$WORK/badrepos"
# A non-searchable dir is inconclusive only when the journal names it as this run's
# clone; an unnamed one (an empty root-owned attempt) is still absence. chmod cannot
# lock out root, so this pair needs an unprivileged user.
if [ "$(id -u)" -ne 0 ]; then
  LOCKED_AID=20260102T000000Z-g1-0123456789abcdef
  LOCKED="$WORK/runner/issue-4242.attempt-$LOCKED_AID"
  mkdir -p "$LOCKED"; chmod 000 "$LOCKED"
  git --git-dir="$BARE" config 'uzi-recovery.agent/issue-4242.clone' \
    "{\"runId\":\"failed-4\",\"clonePath\":\"$LOCKED\",\"attemptId\":\"$LOCKED_AID\"}"
  D_INC="$WORK/out.1.failed.locked"
  rc="$(backup_rc "$D_INC" failed-4)"
  [ "$rc" -eq 1 ] || fail "failed run, run-owned locked clone: exit $rc, want 1"
  grep -q 'FAIL failed-4 .*inconclusive' "$D_INC/latest-attempt/backup.log" \
    || fail "failed run, run-owned locked clone: expected an inconclusive FAIL; got: $(cat "$D_INC/latest-attempt/backup.log")"
  git --git-dir="$BARE" config 'uzi-recovery.agent/issue-4242.clone' \
    "{\"runId\":\"run-4242\",\"clonePath\":\"$RUNNER\"}"
  D_INC="$WORK/out.1.failed.lockedunnamed"
  rc="$(backup_rc "$D_INC" failed-2)"
  [ "$rc" -eq 0 ] || fail "failed run, unnamed locked dir: exit $rc, want 0"
  grep -q 'SNAP failed-2 .*status=failed' "$D_INC/latest-attempt/backup.log" \
    || fail "failed run, unnamed locked dir: expected SNAP; got: $(cat "$D_INC/latest-attempt/backup.log")"
  chmod 755 "$LOCKED"; rm -rf "$LOCKED"
fi
# A completed run stays status-only even when a clone for it still exists.
git --git-dir="$BARE" config 'uzi-recovery.agent/issue-4242.clone' \
  "{\"runId\":\"completed-1\",\"clonePath\":\"$RUNNER\"}"
run_backup 1.completed completed-1 >/dev/null
L1_DONE="$WORK/out.1.completed/latest-attempt"
grep -q 'SNAP completed-1 .*status=completed' "$L1_DONE/backup.log" || fail "completed run: expected SNAP"
[ ! -e "$L1_DONE/issue-4242.tgz" ] || fail "completed run: unexpected worker capture"
git --git-dir="$BARE" config 'uzi-recovery.agent/issue-4242.clone' \
  "{\"runId\":\"run-4242\",\"clonePath\":\"$RUNNER\"}"
echo "PASS failed run: bound clone captured; status-only once its source is gone; completed stays status-only"

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

# --- case 3: mr_rework REUSES the branch clone (agent/issue-9999 -> agent-issue-9999) --
# The run has no issue_iid and (in-flight) a NULL branch; its slug must come from
# pipeline_ref, not `mr_rework-<runid>`. Both the old kind-only derivation AND a naive
# .branch read look for a dir that never exists (branch is null until completion).
RUNNER3="$WORK/runner/agent-issue-9999"
git clone -q "$FORGE" "$RUNNER3"
git -C "$RUNNER3" config user.email t@example.com
git -C "$RUNNER3" config user.name tester
git_q "$RUNNER3" checkout -b agent/issue-9999
git --git-dir="$BARE" config 'uzi-recovery.agent/issue-9999.clone' \
  "{\"runId\":\"mrr-1\",\"clonePath\":\"$RUNNER3\"}"
echo rework >> "$RUNNER3/f.txt"
git_q "$RUNNER3" commit -am 'mr_rework commit'

L3="$(run_backup 3 mrr-1)"
[ -f "$L3/agent-issue-9999.tgz" ] \
  || fail "case3: expected agent-issue-9999.tgz (slug from branch)"
tar tzf "$L3/agent-issue-9999.tgz" 2>/dev/null | grep -q 'agent-issue-9999[.]bundle' \
  || fail "case3: mr_rework bundle missing (clone dir not resolved from branch)"
grep -q "^.*OK .*mrr-1" "$L3/backup.log" || fail "case3: expected OK for mr_rework; got: $(cat "$L3/backup.log")"
echo "PASS case3: mr_rework -> slug from branch (agent-issue-9999), bundle, OK"

# --- case 4: clone gone -> search the worker bare ref, emit BARE ----------------
# Publish the latest committed issue branch into the fake worker's durable runner ref,
# then remove the clone. This is the exact limit_wait/worker-roll shape observed on #1429.
git --git-dir="$BARE" fetch -q "$RUNNER" \
  agent/issue-4242:refs/uzi-runner/agent/issue-4242
git --git-dir="$BARE" config 'uzi-trackowner.agent/issue-4242.owner' run-4242
rm -rf "$RUNNER"

L4="$(run_backup 4)"
[ -f "$L4/issue-4242.tgz" ] || fail "case4: no bare-fallback .tgz produced"
tar tzf "$L4/issue-4242.tgz" 2>/dev/null | grep -q 'issue-4242[.]bundle' \
  || fail "case4: bare fallback bundle missing"
grep -q '^.*BARE .*run-4242' "$L4/backup.log" \
  || fail "case4: expected BARE in log; got: $(cat "$L4/backup.log")"
if tar tzf "$L4/issue-4242.tgz" 2>/dev/null | grep -q 'uncommitted[.]patch'; then
  fail "case4: bare fallback falsely claims an uncommitted patch"
fi
echo "PASS case4: missing clone -> durable runner-ref bundle, BARE"

# A failed run whose clone is gone (the #1724 shape): its owned tracking ref is captured.
git --git-dir="$BARE" config 'uzi-trackowner.agent/issue-4242.owner' failed-3
L4_FAILED="$(run_backup 4.failed failed-3)"
tar tzf "$L4_FAILED/issue-4242.tgz" 2>/dev/null | grep -q 'issue-4242[.]bundle' \
  || fail "failed run bare ref: bundle missing; got: $(cat "$WORK/out.4.failed/latest-attempt/backup.log")"
grep -q '^.*BARE .*failed-3 .*status=failed' "$L4_FAILED/backup.log" \
  || fail "failed run bare ref: expected BARE; got: $(cat "$L4_FAILED/backup.log")"
git --git-dir="$BARE" config 'uzi-trackowner.agent/issue-4242.owner' run-4242
echo "PASS failed run: owned tracking ref captured when the clone is gone"

# --- case 5: no clone/ref -> rc=1, latest preserved, safe retention ------------
git --git-dir="$BARE" update-ref -d refs/uzi-runner/agent/issue-4242
ROOT5="$WORK/out.5"
mkdir -p "$ROOT5/20000101T000000Z" "$ROOT5/not-a-backup"
touch -t 200001010000 "$ROOT5/20000101T000000Z" "$ROOT5/not-a-backup"
ln -s "$L4" "$ROOT5/latest"
set +e
UZI_CTX=test-ctx UZI_WORKER_NS=ns UZI_REPO_SLUG=testrepo UZI_RUNNER_BASE="$WORK/runner" \
  UZI_REPOS_BASE="$REPOS" UZI_BACKUP_DIR="$ROOT5" UZI_BACKUP_RETENTION_DAYS=14 \
  UZI_KUBECTL="$KSTUB" UZI_BIN="$WORK/uzi" UZI_STAT="$WORK/stat" \
  bash "$SCRIPT" run-4242 >/dev/null 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "case5: missing clone/ref rc=$rc, want 1"
[ "$(readlink "$ROOT5/latest")" = "$L4" ] \
  || fail "case5: failed attempt replaced the last recoverable latest"
[ -L "$ROOT5/latest-attempt" ] || fail "case5: latest-attempt was not updated"
[ ! -e "$ROOT5/20000101T000000Z" ] || fail "case5: old timestamp backup was not pruned"
[ -d "$ROOT5/not-a-backup" ] || fail "case5: prune removed a non-backup directory"
echo "PASS case5: active capture failure is nonzero, latest preserved, retention scoped"

# --- case 6: no worker binding must not adopt a stale same-issue ref -------------
git --git-dir="$BARE" fetch -q "$RUNNER3" \
  agent/issue-9999:refs/uzi-runner/agent/issue-4242
ROOT6="$WORK/out.6"
set +e
UZI_CTX=test-ctx UZI_WORKER_NS=ns UZI_REPO_SLUG=testrepo UZI_RUNNER_BASE="$WORK/runner" \
  UZI_REPOS_BASE="$REPOS" UZI_BACKUP_DIR="$ROOT6" UZI_BACKUP_RETENTION_DAYS=0 \
  UZI_KUBECTL="$KSTUB" UZI_BIN="$WORK/uzi" \
  bash "$SCRIPT" noworker-1 >/dev/null 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "case6: no-worker run rc=$rc, want 1"
if rg --files "$ROOT6" | grep -q '[.]tgz$'; then
  fail "case6: no-worker run adopted a stale same-issue ref"
fi
echo "PASS case6: no worker binding refuses stale all-pod branch adoption"

# A queued run has no clone to capture yet. Its status belongs in this cycle,
# and the next cycle must still be free to capture work once it is claimed.
run_backup 6.queued queued-1 >/dev/null  # status-only: read latest-attempt, not latest
L6_QUEUED_ATTEMPT="$WORK/out.6.queued/latest-attempt"
[ -f "$L6_QUEUED_ATTEMPT/issue-4242.run.json" ] || fail "queued: status missing"
if rg --files "$L6_QUEUED_ATTEMPT" | grep -q '[.]tgz$'; then
  fail "queued: adopted stale work before a worker claimed the run"
fi
grep -q 'SNAP .*status=queued' "$L6_QUEUED_ATTEMPT/backup.log" \
  || fail "queued: expected status-only snapshot"
echo "PASS queued: status saved without adopting stale work"

# --- case 7: relative latest target remains protected under a symlinked root ---
git --git-dir="$BARE" update-ref -d refs/uzi-runner/agent/issue-4242
ROOT7_REAL="$WORK/out.7.real"
ROOT7_LINK="$WORK/out.7.link"
mkdir -p "$ROOT7_REAL/20000101T000000Z"
touch -t 200001010000 "$ROOT7_REAL/20000101T000000Z"
ln -s 20000101T000000Z "$ROOT7_REAL/latest"
ln -s "$ROOT7_REAL" "$ROOT7_LINK"
set +e
UZI_CTX=test-ctx UZI_WORKER_NS=ns UZI_REPO_SLUG=testrepo UZI_RUNNER_BASE="$WORK/runner" \
  UZI_REPOS_BASE="$REPOS" UZI_BACKUP_DIR="$ROOT7_LINK" UZI_BACKUP_RETENTION_DAYS=14 \
  UZI_KUBECTL="$KSTUB" UZI_BIN="$WORK/uzi" UZI_STAT="$WORK/stat" \
  bash "$SCRIPT" run-4242 >/dev/null 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "case7: missing-source run rc=$rc, want 1"
[ -d "$ROOT7_REAL/20000101T000000Z" ] \
  || fail "case7: prune deleted the expired relative latest target"
[ "$(readlink "$ROOT7_REAL/latest")" = 20000101T000000Z ] \
  || fail "case7: relative latest link changed"
echo "PASS case7: symlinked root canonicalized, relative latest target protected"

# --- case 8: stale clone and ref owned by another run are rejected --------------
RUNNER8="$WORK/runner/issue-4242"
git clone -q "$FORGE" "$RUNNER8"
git -C "$RUNNER8" config user.email t@example.com
git -C "$RUNNER8" config user.name tester
git_q "$RUNNER8" checkout -b agent/issue-4242
echo foreign >> "$RUNNER8/f.txt"
git_q "$RUNNER8" commit -am foreign
git --git-dir="$BARE" fetch -q "$RUNNER8" \
  agent/issue-4242:refs/uzi-runner/agent/issue-4242
git --git-dir="$BARE" config 'uzi-recovery.agent/issue-4242.clone' \
  "{\"runId\":\"foreign-run\",\"clonePath\":\"$RUNNER8\"}"
git --git-dir="$BARE" config 'uzi-trackowner.agent/issue-4242.owner' foreign-run
ROOT8="$WORK/out.8"
set +e
UZI_CTX=test-ctx UZI_WORKER_NS=ns UZI_REPO_SLUG=testrepo UZI_RUNNER_BASE="$WORK/runner" \
  UZI_REPOS_BASE="$REPOS" UZI_BACKUP_DIR="$ROOT8" UZI_BACKUP_RETENTION_DAYS=0 \
  UZI_KUBECTL="$KSTUB" UZI_BIN="$WORK/uzi" \
  bash "$SCRIPT" run-4242 >/dev/null 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "case8: foreign-owned sources rc=$rc, want 1"
if rg --files "$ROOT8" | grep -q '[.]tgz$'; then
  fail "case8: foreign-owned clone/ref was captured as current-run work"
fi
echo "PASS case8: clone/ref ownership mismatch rejected"

# --- attempt layout (issue 1783): pick the clone by IDENTITY, never newest path ---
# A Docker-wired worker seeds <stem>.attempt-<attemptId> per execution attempt and
# keeps older ones; the bare's journal / attempt ledger say which one is this run's.
A1="20260901T000000Z-g1-0123456789abcdef"
A2="20260902T000000Z-g2-0123456789abcdef"
A3="20260903T000000Z-gx-0123456789abcdef"
RB="$WORK/runner"
JKEY='uzi-recovery.agent/issue-4242.clone'
LKEY='uzi-attempts.agent/issue-4242.entry'

# reset_layout: empty runner dir, no journal/ledger, no tracking ref.
reset_layout() {
  rm -rf "$RB"; mkdir -p "$RB"
  git --git-dir="$BARE" config --unset-all "$JKEY" 2>/dev/null || :
  git --git-dir="$BARE" config --unset-all "$LKEY" 2>/dev/null || :
  git --git-dir="$BARE" update-ref -d refs/uzi-runner/agent/issue-4242 2>/dev/null || :
  git --git-dir="$BARE" config 'uzi-trackowner.agent/issue-4242.owner' run-4242
}
# make_clone <dir> <branch> <marker>: a clone on <branch> with an uncommitted marker.
make_clone() {
  git clone -q "$FORGE" "$1"
  git_q "$1" checkout -b "$2"
  echo "$3" >> "$1/f.txt"
}
# journal <runId> <clonePath> [attemptId]
journal() {
  if [ -n "${3:-}" ]; then
    git --git-dir="$BARE" config "$JKEY" "{\"runId\":\"$1\",\"clonePath\":\"$2\",\"attemptId\":\"$3\"}"
  else
    git --git-dir="$BARE" config "$JKEY" "{\"runId\":\"$1\",\"clonePath\":\"$2\"}"
  fi
}
# ledger <attemptId> <runId> <clonePath> <state>: append one ledger value.
ledger() {
  git --git-dir="$BARE" config --add "$LKEY" \
    "{\"attemptId\":\"$1\",\"runId\":\"$2\",\"clonePath\":\"$3\",\"state\":\"$4\"}"
}
# expect_pick <tag> <clone> <marker>: the backup captured exactly <clone>'s WIP.
expect_pick() {
  local l meta
  l="$(run_backup "$1")"
  [ -f "$l/issue-4242.tgz" ] || fail "$1: no archive; log: $(cat "$WORK/out.$1/latest-attempt/backup.log" 2>/dev/null)"
  meta="$(tar -xOzf "$l/issue-4242.tgz" ./issue-4242.meta.txt)"
  printf '%s\n' "$meta" | grep -qxF "clone=$2" \
    || fail "$1: wrong clone captured, want $2; meta: $(printf '%s' "$meta" | grep '^clone=')"
  tar -xOzf "$l/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF "+$3" \
    || fail "$1: expected WIP marker $3 missing"
}

# case 9: canonical only (unwired worker; legacy journal without attemptId).
reset_layout
make_clone "$RB/issue-4242" agent/issue-4242 canon
journal run-4242 "$RB/issue-4242"
expect_pick 9 "$RB/issue-4242" canon
echo "PASS case9: canonical-only layout captured"

# case 10: several attempts; the newest VALID one wins. A3 is newer but on the wrong
# branch; the canonical dir is valid too but counts as oldest.
reset_layout
make_clone "$RB/issue-4242" agent/issue-4242 canon
make_clone "$RB/issue-4242.attempt-$A1" agent/issue-4242 a1
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 a2
make_clone "$RB/issue-4242.attempt-$A3" agent/issue-other a3
journal other-run "$RB/issue-4242"
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" retired
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
ledger "$A3" run-4242 "$RB/issue-4242.attempt-$A3" live
expect_pick 10 "$RB/issue-4242.attempt-$A2" a2
echo "PASS case10: newest valid attempt wins over a newer wrong-branch one"

# case 11: a delayed Docker create recreated a NEWER, empty predecessor path (no
# .git) beside the correct recovery clone; the journal-named clone is taken.
reset_layout
make_clone "$RB/issue-4242.attempt-$A1" agent/issue-4242 a1
mkdir -p "$RB/issue-4242.attempt-$A2"
journal run-4242 "$RB/issue-4242.attempt-$A1" "$A1"
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" live
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
expect_pick 11 "$RB/issue-4242.attempt-$A1" a1
echo "PASS case11: newer empty recreated attempt dir ignored"

# case 12: a ledger entry naming ANOTHER run is rejected. A2 was ours, but its LAST
# ledger value hands it to another run, so the older A1 is the valid pick.
reset_layout
make_clone "$RB/issue-4242.attempt-$A1" agent/issue-4242 a1
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 a2
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" live
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
ledger "$A2" other-run "$RB/issue-4242.attempt-$A2" live
expect_pick 12 "$RB/issue-4242.attempt-$A1" a1
echo "PASS case12: attempt whose last ledger value names another run rejected"

# case 13: a ledger entry naming a path OUTSIDE the stem's candidate set (here a
# quarantined residue / skills-plugin sibling, which the <stem>.attempt-* glob and
# the host's name check never list) cannot make that path the pick, even when newer.
# The residue name is the worker's pinned grammar .uzi-residue-<key>.residue-<uuid>
# (a lowercase v4 uuid; agent/src/attempt-path.ts formatResidueName).
RESIDUE_UUID=0f8fad5b-d9cb-469f-a165-70867728950e
reset_layout
make_clone "$RB/issue-4242.attempt-$A1" agent/issue-4242 a1
make_clone "$RB/.uzi-residue-issue-4242.residue-$RESIDUE_UUID" agent/issue-4242 residue
make_clone "$RB/.uzi-skills-issue-4242" agent/issue-4242 skills
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" live
ledger "$A3" run-4242 "$RB/.uzi-residue-issue-4242.residue-$RESIDUE_UUID" live
ledger "$A2" run-4242 "$RB/.uzi-skills-issue-4242" live
expect_pick 13 "$RB/issue-4242.attempt-$A1" a1
echo "PASS case13: ledger entries naming non-candidate sibling paths ignored"

# case 14: the journal-named valid attempt beats a newer valid ledger attempt.
reset_layout
make_clone "$RB/issue-4242.attempt-$A1" agent/issue-4242 a1
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 a2
journal run-4242 "$RB/issue-4242.attempt-$A1" "$A1"
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" live
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
expect_pick 14 "$RB/issue-4242.attempt-$A1" a1
echo "PASS case14: journal-named attempt beats a newer valid attempt"

# case 15: no valid candidate (wrong branch, empty dir, journal attemptId mismatch,
# unnamed attempt) -> the existing durable-ref BARE fallback, never a stray clone.
reset_layout
make_clone "$RB/issue-4242" agent/issue-other canon
mkdir -p "$RB/issue-4242.attempt-$A2"
make_clone "$RB/issue-4242.attempt-$A1" agent/issue-4242 a1
make_clone "$RB/issue-4242.attempt-$A3" agent/issue-4242 a3
journal run-4242 "$RB/issue-4242.attempt-$A1" "$A2"
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
git_q "$RB/issue-4242.attempt-$A1" -c user.email=t@example.com -c user.name=tester \
  commit -am committed-a1
git --git-dir="$BARE" fetch -q "$RB/issue-4242.attempt-$A1" \
  agent/issue-4242:refs/uzi-runner/agent/issue-4242
L15="$(run_backup 15)"
grep -q '^.*BARE .*run-4242' "$L15/backup.log" \
  || fail "case15: expected BARE fallback; got: $(cat "$WORK/out.15/latest-attempt/backup.log")"
if tar tzf "$L15/issue-4242.tgz" 2>/dev/null | grep -q 'uncommitted[.]patch'; then
  fail "case15: an invalid candidate clone was captured"
fi
grep -q '^.*BARE .*run-4242.*attempt state unknown' "$L15/backup.log" \
  || fail "case15: a live ledger entry must read 'attempt state unknown'; got: $(cat "$WORK/out.15/latest-attempt/backup.log")"
if grep -q 'attempt retired' "$L15/backup.log"; then fail "case15: retirement claimed without proof"; fi
echo "PASS case15: no valid candidate -> BARE fallback, attempt state unknown"

# case 16: a dir name carrying a TAB must not forge the branch field. A dir name
# cannot hold "/", so the forgery needs a slash-free branch: a mr_rework on "hotfix"
# (stem hotfix). The real A2 attempt is on the wrong branch; a sibling named
# "hotfix.attempt-<A2><TAB>hotfix" (no .git) would otherwise list as
# "<A2 path><TAB>hotfix<TAB>" and vouch for A2. The valid (older) A1 must be the pick.
reset_layout
FK='uzi-attempts.hotfix.entry'
git --git-dir="$BARE" config --unset-all "$FK" 2>/dev/null || :
make_clone "$RB/hotfix.attempt-$A1" hotfix a1
make_clone "$RB/hotfix.attempt-$A2" wrong-branch a2
mkdir "$RB/hotfix.attempt-$A2"$'\t'"hotfix"
for a in "$A1" "$A2"; do
  git --git-dir="$BARE" config --add "$FK" \
    "{\"attemptId\":\"$a\",\"runId\":\"mrrflat-1\",\"clonePath\":\"$RB/hotfix.attempt-$a\",\"state\":\"live\"}"
done
L16="$(run_backup 16 mrrflat-1)"
[ -f "$L16/hotfix.tgz" ] || fail "16: no archive; log: $(cat "$WORK/out.16/latest-attempt/backup.log" 2>/dev/null)"
tar -xOzf "$L16/hotfix.tgz" ./hotfix.meta.txt | grep -qxF "clone=$RB/hotfix.attempt-$A1" \
  || fail "16: tab-forged listing vouched for the wrong-branch attempt"
tar -xOzf "$L16/hotfix.tgz" ./hotfix.uncommitted.patch | grep -qxF "+a1" \
  || fail "16: expected WIP marker a1 missing"
git --git-dir="$BARE" config --unset-all "$FK"
echo "PASS case16: tab-bearing dir name cannot vouch for another attempt"

# case 17: same-second attemptIds order by NUMERIC generation (g10 > g9, which a
# lexical compare gets backwards), and gx (unknown generation) is lowest.
G9="20260905T000000Z-g9-0123456789abcdef"
G10="20260905T000000Z-g10-0123456789abcdef"
GX="20260906T000000Z-gx-ffffffffffffffff"
G1="20260906T000000Z-g1-0000000000000000"
reset_layout
make_clone "$RB/issue-4242.attempt-$G9" agent/issue-4242 g9
make_clone "$RB/issue-4242.attempt-$G10" agent/issue-4242 g10
ledger "$G9" run-4242 "$RB/issue-4242.attempt-$G9" live
ledger "$G10" run-4242 "$RB/issue-4242.attempt-$G10" live
expect_pick 17 "$RB/issue-4242.attempt-$G10" g10
reset_layout
make_clone "$RB/issue-4242.attempt-$GX" agent/issue-4242 gx
make_clone "$RB/issue-4242.attempt-$G1" agent/issue-4242 g1
ledger "$GX" run-4242 "$RB/issue-4242.attempt-$GX" live
ledger "$G1" run-4242 "$RB/issue-4242.attempt-$G1" live
expect_pick 17x "$RB/issue-4242.attempt-$G1" g1
echo "PASS case17: same-second attempts ordered by numeric generation, gx lowest"

# case 18: a trailing slash on UZI_RUNNER_BASE is normalized, so the journal's
# clonePath still matches the listed candidate byte for byte.
reset_layout
make_clone "$RB/issue-4242.attempt-$A1" agent/issue-4242 a1
journal run-4242 "$RB/issue-4242.attempt-$A1" "$A1"
TEST_RUNNER_BASE="$RB//" expect_pick 18 "$RB/issue-4242.attempt-$A1" a1
echo "PASS case18: trailing slash on the runner base ignored"

# Retained canonical predecessor discovery after handoff: no invented ledger id.
reset_layout
make_clone "$RB/issue-4242" agent/issue-4242 retained-canonical
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-other successor
git --git-dir="$BARE" config "$JKEY" \
  "{\"runId\":\"run-4242\",\"clonePath\":\"$RB/issue-4242.attempt-$A2\",\"attemptId\":\"$A2\",\"retainedSources\":[{\"runId\":\"run-4242\",\"clonePath\":\"$RB/issue-4242\"}]}"
expect_pick retained-canonical "$RB/issue-4242" retained-canonical
echo "PASS retained-canonical: predecessor without ledger identity is discoverable"

# Both paths are valid: current work wins over the earlier-listed predecessor
# and a newer ledger-only attempt.
rm -rf "$RB/issue-4242.attempt-$A2"
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 current-newer-bytes
make_clone "$RB/issue-4242.attempt-$A3" agent/issue-4242 ledger-newest
ledger "$A3" run-4242 "$RB/issue-4242.attempt-$A3" live
expect_pick current-over-retained "$RB/issue-4242.attempt-$A2" current-newer-bytes
echo "PASS current-over-retained: current journal clone takes precedence"

# case 19: a LIVE run whose clone is gone and whose every recorded attempt is retired or
# abandoned says so on the BARE line; a ledger-less bare (case 4) and a mixed ledger say
# `attempt state unknown`. The archive is kept in all of them.
reset_layout
make_clone "$RB/seed" agent/issue-4242 seed
git_q "$RB/seed" -c user.email=t@example.com -c user.name=tester commit -am committed-seed
git --git-dir="$BARE" fetch -q "$RB/seed" agent/issue-4242:refs/uzi-runner/agent/issue-4242
rm -rf "$RB/seed"
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" retired
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" abandoned
ledger "$A3" other-run "$RB/issue-4242.attempt-$A3" live
L19="$(run_backup 19)"
[ -f "$L19/issue-4242.tgz" ] || fail "case19: BARE archive must still be kept"
grep -q '^.*BARE .*run-4242.*attempt retired' "$L19/backup.log" \
  || fail "case19: expected 'attempt retired'; got: $(cat "$WORK/out.19/latest-attempt/backup.log")"
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
L19B="$(run_backup 19b)"
[ -f "$L19B/issue-4242.tgz" ] || fail "case19b: BARE archive must still be kept"
grep -q '^.*BARE .*run-4242.*attempt state unknown' "$L19B/backup.log" \
  || fail "case19b: a live attempt must read unknown; got: $(cat "$WORK/out.19b/latest-attempt/backup.log")"
if grep -q 'attempt retired' "$L19B/backup.log"; then fail "case19b: retirement claimed with a live attempt"; fi
echo "PASS case19: BARE line reports attempt retirement only on ledger proof"

# case 19c/19d: damaged ledger rows fail closed. A truncated newest row for an attemptId (or a
# newest row without runId) must not leave an older `retired` value as proof.
reset_layout
make_clone "$RB/seed" agent/issue-4242 seed
git_q "$RB/seed" -c user.email=t@example.com -c user.name=tester commit -am committed-seed
git --git-dir="$BARE" fetch -q "$RB/seed" agent/issue-4242:refs/uzi-runner/agent/issue-4242
rm -rf "$RB/seed"
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" retired
git --git-dir="$BARE" config --add "$LKEY" "{\"attemptId\":\"$A1\",\"runId\":\"run-4242\",\"state\":\"li"
L19C="$(run_backup 19c)"
[ -f "$L19C/issue-4242.tgz" ] || fail "case19c: BARE archive must still be kept"
grep -q '^.*BARE .*run-4242.*attempt state unknown' "$L19C/backup.log" \
  || fail "case19c: truncated newest row must read unknown; got: $(cat "$WORK/out.19c/latest-attempt/backup.log")"
if grep -q 'attempt retired' "$L19C/backup.log"; then fail "case19c: retirement claimed from an older row"; fi
git --git-dir="$BARE" config --unset-all "$LKEY"
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" retired
git --git-dir="$BARE" config --add "$LKEY" "{\"attemptId\":\"$A1\",\"state\":\"live\"}"
L19D="$(run_backup 19d)"
[ -f "$L19D/issue-4242.tgz" ] || fail "case19d: BARE archive must still be kept"
grep -q '^.*BARE .*run-4242.*attempt state unknown' "$L19D/backup.log" \
  || fail "case19d: row without runId must read unknown; got: $(cat "$WORK/out.19d/latest-attempt/backup.log")"
if grep -q 'attempt retired' "$L19D/backup.log"; then fail "case19d: retirement claimed despite a runId-less row"; fi
echo "PASS case19c/d: malformed ledger rows fail closed to attempt state unknown"

# case 19e/19f: an EMPTY ledger value is a malformed row too. It must not be dropped before
# validation (19e: empty row between a retired and an unrelated live entry) and must survive
# the pod capture when it is the LAST value (19f: command substitution strips trailing newlines).
reset_layout
make_clone "$RB/seed" agent/issue-4242 seed
git_q "$RB/seed" -c user.email=t@example.com -c user.name=tester commit -am committed-seed
git --git-dir="$BARE" fetch -q "$RB/seed" agent/issue-4242:refs/uzi-runner/agent/issue-4242
rm -rf "$RB/seed"
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" retired
git --git-dir="$BARE" config --add "$LKEY" ""
ledger "$A3" other-run "$RB/issue-4242.attempt-$A3" live
L19E="$(run_backup 19e)"
[ -f "$L19E/issue-4242.tgz" ] || fail "case19e: BARE archive must still be kept"
grep -q '^.*BARE .*run-4242.*attempt state unknown' "$L19E/backup.log" \
  || fail "case19e: an empty ledger row must read unknown; got: $(cat "$WORK/out.19e/latest-attempt/backup.log")"
if grep -q 'attempt retired' "$L19E/backup.log"; then fail "case19e: retirement claimed past an empty row"; fi
git --git-dir="$BARE" config --unset-all "$LKEY"
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" retired
git --git-dir="$BARE" config --add "$LKEY" ""
L19F="$(run_backup 19f)"
[ -f "$L19F/issue-4242.tgz" ] || fail "case19f: BARE archive must still be kept"
grep -q '^.*BARE .*run-4242.*attempt state unknown' "$L19F/backup.log" \
  || fail "case19f: a trailing empty ledger row must read unknown; got: $(cat "$WORK/out.19f/latest-attempt/backup.log")"
if grep -q 'attempt retired' "$L19F/backup.log"; then fail "case19f: retirement claimed past a trailing empty row"; fi
echo "PASS case19e/f: empty ledger rows fail closed to attempt state unknown"

# Protected terminal custody survives replacement of the branch's active slot.
# Both canonical and attempt sources retain their exact identity; no canonical
# attempt marker or invented ledger row is required.
for status in failed completed cancelled; do
  for layout in canonical attempt; do
    reset_layout
    rid="$status-protected-$layout"
    pkey="uzi-retained.$rid.journal"
    predecessor="$RB/issue-4242"
    [ "$layout" = canonical ] || predecessor="$RB/issue-4242.attempt-$A1"
    make_clone "$predecessor" agent/issue-4242 protected-predecessor
    make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 protected-current
    make_clone "$RB/issue-4242.attempt-$A3" agent/issue-4242 unrelated-newest
    ledger "$A2" "$rid" "$RB/issue-4242.attempt-$A2" reclaimed
    ledger "$A3" newer-run "$RB/issue-4242.attempt-$A3" live
    [ "$layout" = canonical ] || ledger "$A1" "$rid" "$predecessor" reclaimed
    envelope="$(jq -cn --arg rid "$rid" --arg pred "$predecessor" --arg a1 "$A1" --arg a2 "$A2" \
      --arg current "$RB/issue-4242.attempt-$A2" --arg layout "$layout" --arg tip "$MAIN" '
      ({runId:$rid,clonePath:$pred} + (if $layout == "attempt" then {attemptId:$a1} else {} end)) as $source
      | {runId:$rid,clonePath:$current,attemptId:$a2,restoreTip:$tip} as $successor
      | {version:1,branch:"agent/issue-4242",key:"issue-4242",journal:($successor +
          {retainedSources:[$source],recovery:{version:1,source:$source,attempts:2,
            startedAt:1000,deadline:301000,backoffMs:10,stage:"ready-for-model",restoreTip:$tip,successor:$successor}})}')"
    git --git-dir="$BARE" config "$pkey" "$envelope"
    journal newer-run "$RB/issue-4242.attempt-$A3" "$A3"
    dest="$WORK/out.protected-$rid"
    rc="$(backup_rc "$dest" "$rid")"
    [ "$rc" -eq 0 ] || fail "$rid: protected backup exit $rc"
    tar -xOzf "$dest/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+protected-current' \
      || fail "$rid: protected current source was not captured"

    # A missing primary still discovers the retained predecessor.
    rm -rf "$RB/issue-4242.attempt-$A2"
    rc="$(backup_rc "$dest" "$rid")"
    [ "$rc" -eq 0 ] || fail "$rid: predecessor backup exit $rc"
    tar -xOzf "$dest/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+protected-predecessor' \
      || fail "$rid: protected predecessor was not captured"

    # Full descriptor validation happens before picking a source. Every refusal
    # keeps the previously published backup and never captures the newer run.
    original_ledger="$(git --git-dir="$BARE" config --get-all "$LKEY")"
    for fault in malformed version key sibling counter conflict ledger duplicate empty crossbranch malformed-ledger; do
      bad="$envelope"
      case "$fault" in
        malformed) bad='{' ;;
        version) bad="$(printf '%s' "$envelope" | jq '.version=2')" ;;
        key) bad="$(printf '%s' "$envelope" | jq '.key="issue-other"')" ;;
        sibling) bad="$(printf '%s' "$envelope" | jq '.journal.retainedSources[0].clonePath="/outside/source"')" ;;
        counter) bad="$(printf '%s' "$envelope" | jq '.journal.recovery.attempts=4')" ;;
        conflict) git --git-dir="$BARE" config "$JKEY" "$(printf '%s' "$envelope" | jq '.journal | .recovery.attempts=3')" ;;
        ledger) ledger "$A2" newer-run "$RB/issue-4242.attempt-$A2" live ;;
        empty) bad='' ;;
        crossbranch) git --git-dir="$BARE" config --add uzi-attempts.agent/other.entry \
          "{\"attemptId\":\"$A2\",\"runId\":\"$rid\",\"clonePath\":\"$RB/issue-4242.attempt-$A2\",\"state\":\"reclaimed\"}" ;;
        malformed-ledger) git --git-dir="$BARE" config --add "$LKEY" "" ;;
      esac
      git --git-dir="$BARE" config "$pkey" "$bad"
      [ "$fault" != duplicate ] || git --git-dir="$BARE" config --add "$pkey" "$envelope"
      rc=0
      env UZI_CTX=test-ctx UZI_WORKER_NS=ns UZI_REPO_SLUG=testrepo UZI_RUNNER_BASE="$RB" \
        UZI_REPOS_BASE="$REPOS" UZI_BACKUP_DIR="$dest" UZI_KUBECTL="$KSTUB" UZI_BIN="$WORK/uzi" \
        bash "$SCRIPT" "$rid" >/dev/null 2>&1 || rc=$?
      [ "$rc" -ne 0 ] || fail "$rid/$fault: invalid evidence succeeded"
      tar -xOzf "$dest/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+protected-predecessor' \
        || fail "$rid/$fault: previous latest lost"
      git --git-dir="$BARE" config --unset-all "$pkey"
      git --git-dir="$BARE" config "$pkey" "$envelope"
      journal newer-run "$RB/issue-4242.attempt-$A3" "$A3"
      if [ "$fault" = ledger ]; then
        git --git-dir="$BARE" config --unset-all "$LKEY"
        while IFS= read -r row; do
          git --git-dir="$BARE" config --add "$LKEY" "$row"
        done <<< "$original_ledger"
      fi
      [ "$fault" != crossbranch ] || git --git-dir="$BARE" config --unset-all uzi-attempts.agent/other.entry
    done
    git --git-dir="$BARE" config --unset-all "$pkey"
    echo "PASS protected $status/$layout: exact sources and fail-closed full descriptor"
  done
done

# Episode-free completed singleton: the primary is also its retained source.
reset_layout
rid="completed-protected-singleton"
predecessor="$RB/issue-4242"
make_clone "$predecessor" agent/issue-4242 singleton-old-dirty
make_clone "$RB/issue-4242.attempt-$A3" agent/issue-4242 singleton-new-active
ledger "$A3" newer-run "$RB/issue-4242.attempt-$A3" live
envelope="$(jq -cn --arg rid "$rid" --arg pred "$predecessor" '
  {runId:$rid,clonePath:$pred} as $source
  | {version:1,branch:"agent/issue-4242",key:"issue-4242",
     journal:($source + {retainedSources:[$source]})}')"
git --git-dir="$BARE" config "uzi-retained.$rid.journal" "$envelope"
journal newer-run "$RB/issue-4242.attempt-$A3" "$A3"
dest="$WORK/out.protected-singleton"
rc="$(backup_rc "$dest" "$rid")"
[ "$rc" -eq 0 ] || fail "protected singleton: backup exit $rc"
tar -xOzf "$dest/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+singleton-old-dirty' \
  || fail "protected singleton: newer active run hid old dirty bytes"
tar -xOzf "$dest/latest/issue-4242.tgz" ./issue-4242.meta.txt | grep -qF "clone=$predecessor" \
  || fail "protected singleton: original path missing"
echo "PASS protected completed singleton: episode-free old dirty bytes survive newer active run"

echo "ALL PASS"
