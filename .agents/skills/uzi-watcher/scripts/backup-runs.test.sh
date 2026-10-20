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
                                if [ -n "\${UZI_TEST_PODS:-}" ]; then
                                  for p in \$UZI_TEST_PODS; do echo "\$p"; done
                                else
                                  echo "pod/uzi-hw-${WID:-w0rker}-test"
                                fi
                                exit 0 ;;
esac
# exec ...: distinguish the REALMAIN git read from the capture sh -c.
# A transport failure: the exec never ran the probe, so nothing reaches stdout.
[ "\${UZI_TEST_EXECFAIL:-}" = 1 ] && exit 1
# Per-pod transport failure: the pod name is the argument after "exec".
if [ -n "\${UZI_TEST_EXECFAIL_POD:-}" ]; then
  prev=""
  for a in "\${args[@]}"; do
    if [ "\$prev" = exec ] && [ "\$a" = "\$UZI_TEST_EXECFAIL_POD" ]; then exit 1; fi
    prev="\$a"
  done
fi
# Per-pod filesystem view (UZI_TEST_ALT_POD): on that pod the runner-base paths in the exec
# args are remapped from UZI_TEST_HOST_BASE to UZI_TEST_ALT_BASE, and the candidate
# listing's output is mapped back, so two pods can hold different clone dirs.
if [ -n "\${UZI_TEST_ALT_POD:-}" ] && [ "\${cmd[0]:-}" = sh ]; then
  prev=""; alt=0
  for a in "\${args[@]}"; do
    if [ "\$prev" = exec ] && [ "\$a" = "\$UZI_TEST_ALT_POD" ]; then alt=1; fi
    prev="\$a"
  done
  if [ "\$alt" = 1 ]; then
    mapped=()
    for a in "\${cmd[@]}"; do mapped+=("\${a/#\$UZI_TEST_HOST_BASE/\$UZI_TEST_ALT_BASE}"); done
    if [ "\${cmd[4]:-}" = sh ]; then
      "\${mapped[@]}" | sed "s#\$UZI_TEST_ALT_BASE#\$UZI_TEST_HOST_BASE#g"
      exit 0
    fi
    exec "\${mapped[@]}"
  fi
fi
if [ "\${cmd[0]:-}" = git ]; then
  # Execute the real bare-repo probes. This covers the public-main read and the
  # missing-clone fallback's show-ref lookup against the local fake worker bare.
  exec "\${cmd[@]}"
fi
if [ "\${cmd[0]:-}" = sh ]; then
  # cmd = (sh -c <LIST_CANDIDATES> _ <RUNNER_BASE> <STEM>) or
  # (sh -c <CAPTURE> _ <STEM> <REALMAIN> <CLONE>); the host already forwarded the
  # runner base / concrete clone as arguments, so run it verbatim.
  # UZI_TEST_CAPFAIL_PATH: the capture (its last arg is the clone path) exits 3, no stdout.
  if [ -n "\${UZI_TEST_CAPFAIL_PATH:-}" ] && [ "\${cmd[\$((\${#cmd[@]} - 1))]}" = "\$UZI_TEST_CAPFAIL_PATH" ]; then exit 3; fi
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
# A completed run whose only evidence is the journal (no stopped marker, no protected
# descriptor, no ledger attempt) stays status-only even when the named clone still exists:
# a journal entry names a path, not an exact attempt identity.
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
  local r
  for r in run-4242 failed-1 completed-1; do
    git --git-dir="$BARE" config --unset-all "uzi-stopped.$r.entry" 2>/dev/null || :
  done
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

# #2958: detached protected descriptors and ordinary journals carry an optional
# positive safe-integer claim fence. Missing fences remain valid legacy progress.
for kind in protected ordinary; do
  reset_layout
  rid=completed-1
  [ "$kind" = protected ] || rid=run-4242
  predecessor="$RB/issue-4242"
  make_clone "$predecessor" agent/issue-4242 claim-generation-dirty
  progress_journal="$(jq -cn --arg rid "$rid" --arg pred "$predecessor" '
    {runId:$rid,clonePath:$pred} as $source
    | $source + {recovery:{version:1,source:$source,attempts:2,
        startedAt:1000,deadline:301000,backoffMs:10,stage:"blocked",blocker:"budget_exhausted"}}')"
  dest="$WORK/out.claim-generation-$kind"
  for generation in legacy 1 9007199254740991; do
    value="$progress_journal"
    [ "$generation" = legacy ] || value="$(printf '%s' "$value" | jq --argjson gen "$generation" '.recovery.claimGeneration=$gen')"
    if [ "$kind" = protected ]; then
      value="$(printf '%s' "$value" | jq '{version:1,branch:"agent/issue-4242",key:"issue-4242",journal:.}')"
      git --git-dir="$BARE" config "uzi-retained.$rid.journal" "$value"
    else
      git --git-dir="$BARE" config "$JKEY" "$value"
    fi
    rc="$(backup_rc "$dest" "$rid")"
    [ "$rc" -eq 0 ] || fail "claimGeneration $kind/$generation: exit $rc; log: $(cat "$dest/latest-attempt/backup.log")"
    tar -xOzf "$dest/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+claim-generation-dirty' \
      || fail "claimGeneration $kind/$generation: retained dirty bytes missing"
    echo "PASS claimGeneration $kind/$generation"
  done
  if [ "$kind" = protected ]; then
    for blocker in readiness_cleanup_unconfirmed unknown_cleanup_blocker; do
      value="$(printf '%s' "$progress_journal" | jq --arg blocker "$blocker" '.recovery.blocker=$blocker | {version:1,branch:"agent/issue-4242",key:"issue-4242",journal:.}')"
      git --git-dir="$BARE" config "uzi-retained.$rid.journal" "$value"
      rc="$(backup_rc "$dest" "$rid")"
      if [ "$blocker" = readiness_cleanup_unconfirmed ]; then
        [ "$rc" -eq 0 ] || fail "cleanup blocker: valid protected descriptor rejected"
        tar -xOzf "$dest/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+claim-generation-dirty' \
          || fail "cleanup blocker: retained dirty bytes missing"
      else
        [ "$rc" -ne 0 ] || fail "cleanup blocker: unknown blocker accepted"
        [ ! -e "$dest/latest-attempt/issue-4242.tgz" ] || fail "cleanup blocker: invalid descriptor attributed source"
      fi
      echo "PASS protected blocker $blocker"
    done
  fi
  for generation in 0 -1 1.5 9007199254740992 '"1"' null true; do
    value="$(printf '%s' "$progress_journal" | jq --argjson gen "$generation" '.recovery.claimGeneration=$gen')"
    if [ "$kind" = protected ]; then
      value="$(printf '%s' "$value" | jq '{version:1,branch:"agent/issue-4242",key:"issue-4242",journal:.}')"
      git --git-dir="$BARE" config "uzi-retained.$rid.journal" "$value"
    else
      git --git-dir="$BARE" config "$JKEY" "$value"
    fi
    rc="$(backup_rc "$dest" "$rid")"
    [ "$rc" -ne 0 ] || fail "claimGeneration $kind/$generation: malformed fence accepted"
    [ ! -e "$dest/latest-attempt/issue-4242.tgz" ] || fail "claimGeneration $kind/$generation: invalid journal attributed source"
    echo "PASS claimGeneration $kind/$generation refused"
  done
  [ "$kind" != protected ] || git --git-dir="$BARE" config --unset-all "uzi-retained.$rid.journal"
done

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
# The empty A2 dir has no .git, so it is absent (not an unreadable source): no
# coverage gap, the run exits 0 and the journal-named clone is captured.
D_11="$WORK/out.11"
rc="$(backup_rc "$D_11" run-4242)"
[ "$rc" -eq 0 ] || fail "11: exit $rc, want 0; log: $(cat "$D_11/latest-attempt/backup.log")"
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

# --- stopped-source marker (issue 1786): a detached kept canonical clone ---------
# markStoppedCanonical clears the plain journal and appends uzi-stopped.<runId>.entry;
# the marker alone then names the kept canonical dir for this run.
# log_of <dest>: the latest attempt's backup.log.
log_of() { cat "$1/latest-attempt/backup.log"; }
DIG="$(printf 'ab%.0s' $(seq 32))"
# stopped_marker <runId> <branch> <key> <clonePath> [runId-in-value]: append one marker.
stopped_marker() {
  git --git-dir="$BARE" config --add "uzi-stopped.$1.entry" "$(jq -cn \
    --arg rid "${5:-$1}" --arg b "$2" --arg k "$3" --arg p "$4" --arg d "$DIG" \
    '{version:1,runId:$rid,generation:null,branch:$b,key:$k,clonePath:$p,journalDigest:$d,reason:"stop",at:"2026-10-12T00:00:00Z"}')"
}

# case M-a: no journal (cleared value, then absent) + a valid marker -> canonical dirty bytes.
reset_layout
make_clone "$RB/issue-4242" agent/issue-4242 kept-canon
git --git-dir="$BARE" config "$JKEY" ""
stopped_marker run-4242 agent/issue-4242 issue-4242 "$RB/issue-4242"
expect_pick marker-a1 "$RB/issue-4242" kept-canon
git --git-dir="$BARE" config --unset-all "$JKEY"
expect_pick marker-a2 "$RB/issue-4242" kept-canon
echo "PASS marker-a: cleared/absent journal + valid marker -> kept canonical captured"

# case M-b: a newer valid ledger attempt of the same run does not displace it.
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 newer-attempt
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
expect_pick marker-b "$RB/issue-4242" kept-canon
echo "PASS marker-b: marker-named canonical beats a newer ledger attempt"

# case M-c: well-formed markers that do not attribute this key are ignored; the
# older fallback (the ledger attempt) is picked as before.
CANON="$RB/issue-4242"
for variant in other-run other-branch other-key attempt-path; do
  reset_layout
  make_clone "$CANON" agent/issue-4242 kept-canon
  make_clone "$RB/issue-4242.attempt-$A1" agent/issue-4242 a1
  ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" retired
  case "$variant" in
    other-run) stopped_marker run-4242 agent/issue-4242 issue-4242 "$CANON" other-run ;;
    other-branch) stopped_marker run-4242 agent/issue-other issue-4242 "$CANON" ;;
    other-key) stopped_marker run-4242 agent/issue-4242 issue-other "$CANON" ;;
    attempt-path) stopped_marker run-4242 agent/issue-4242 issue-4242 "$RB/issue-4242.attempt-$A1" ;;
  esac
  expect_pick "marker-c-$variant" "$RB/issue-4242.attempt-$A1" a1
done
echo "PASS marker-c: markers for another run/branch/key or an attempt path are not used"

# A value that is not a marker of the exact shape is unreadable evidence: the search
# is inconclusive (FAIL, retry), never a silent fallback. Extra field, bad digest,
# wrong version and non-JSON all count. failed-1 is a failed run, which reports it.
for bad in 'not json' '{"version":1}' \
  "{\"version\":2,\"runId\":\"failed-1\",\"generation\":null,\"branch\":\"agent/issue-4242\",\"key\":\"issue-4242\",\"clonePath\":\"$CANON\",\"journalDigest\":\"$DIG\",\"reason\":\"stop\",\"at\":\"t\"}" \
  "{\"version\":1,\"runId\":\"failed-1\",\"generation\":null,\"branch\":\"agent/issue-4242\",\"key\":\"issue-4242\",\"clonePath\":\"$CANON\",\"journalDigest\":\"XYZ\",\"reason\":\"stop\",\"at\":\"t\"}" \
  "{\"version\":1,\"runId\":\"failed-1\",\"generation\":null,\"branch\":\"agent/issue-4242\",\"key\":\"issue-4242\",\"clonePath\":\"$CANON\",\"journalDigest\":\"$DIG\",\"reason\":\"stop\",\"at\":\"t\",\"extra\":1}"; do
  reset_layout
  make_clone "$CANON" agent/issue-4242 kept-canon
  journal failed-1 "$CANON"
  git --git-dir="$BARE" config --add "uzi-stopped.failed-1.entry" "$bad"
  D_MB="$WORK/out.marker-bad"
  rc="$(backup_rc "$D_MB" failed-1)"
  [ "$rc" -eq 1 ] || fail "marker-bad ($bad): exit $rc, want 1"
  grep -q 'FAIL failed-1 .*inconclusive' "$D_MB/latest-attempt/backup.log" \
    || fail "marker-bad ($bad): expected an inconclusive FAIL; got: $(cat "$D_MB/latest-attempt/backup.log")"
done
echo "PASS marker-c: a value that is not an exact-shape marker -> inconclusive"

# source_check <tag> <primary-clone> <primary-marker> <extra-clone> <extra-marker>: exit 0,
# the primary holds only its own bytes, source-2 holds the extra's bytes and clone path.
source_check() {
  local d="$WORK/out.$1" rc p x clone_ere
  rc="$(backup_rc "$d" run-4242)"
  [ "$rc" -eq 0 ] || fail "$1: exit $rc, want 0; log: $(cat "$d/latest-attempt/backup.log")"
  p="$d/latest/issue-4242.tgz"; x="$d/latest/issue-4242.source-2.tgz"
  [ -f "$p" ] && [ -f "$x" ] || fail "$1: missing primary or source-2 archive; log: $(cat "$d/latest-attempt/backup.log")"
  tar -xOzf "$p" ./issue-4242.meta.txt | grep -qxF "clone=$2" || fail "$1: primary is not $2"
  tar -xOzf "$p" ./issue-4242.uncommitted.patch | grep -qxF "+$3" || fail "$1: primary lacks +$3"
  tar -xOzf "$p" ./issue-4242.uncommitted.patch | grep -qxF "+$5" && fail "$1: extra bytes leaked into the primary"
  tar -xOzf "$x" ./issue-4242.source-2.meta.txt | grep -qxF "clone=$4" || fail "$1: source-2 is not $4"
  tar -xOzf "$x" ./issue-4242.source-2.uncommitted.patch | grep -qxF "+$5" || fail "$1: source-2 lacks +$5"
  clone_ere="$(printf '%s' "$4" | sed 's/[][\\.^$*+?(){}|]/\\&/g')"
  grep -qE "(OK|PART) .*pod=[^ ]+ clone=${clone_ere}[: ].*issue-4242[.]source-2[.]tgz" "$d/latest-attempt/backup.log" \
    || fail "$1: missing its own OK/PART line for source-2 naming pod and clone; log: $(cat "$d/latest-attempt/backup.log")"
}
# Exercise the real backup with an explicit '+' in the clone path on every host.
# Run before marker-d so a '+' checkout reaches this regression first.
(
  mkdir -p "$WORK/plus+path"
  cp "$WORK/uzi" "$WORK/plus+path/uzi"
  WORK="$WORK/plus+path"
  RB="$WORK/runner"
  CANON="$RB/issue-4242"
  reset_layout
  make_clone "$CANON" agent/issue-4242 plus-kept-canon
  make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 plus-live-attempt
  journal run-4242 "$RB/issue-4242.attempt-$A2" "$A2"
  ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
  stopped_marker run-4242 agent/issue-4242 issue-4242 "$CANON"
  source_check marker-plus-path "$RB/issue-4242.attempt-$A2" plus-live-attempt "$CANON" plus-kept-canon
) || fail "marker-plus-path: regression failed"
echo "PASS marker-plus-path: literal '+' clone path retains source-2 log and archive evidence"

# case M-d: the journal names a live attempt A2; the detached kept canonical (valid
# marker) is a second attributed source, captured as issue-4242.source-2.tgz.
reset_layout
make_clone "$CANON" agent/issue-4242 kept-canon
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 live-attempt
journal run-4242 "$RB/issue-4242.attempt-$A2" "$A2"
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
stopped_marker run-4242 agent/issue-4242 issue-4242 "$CANON"
source_check marker-d "$RB/issue-4242.attempt-$A2" live-attempt "$CANON" kept-canon
echo "PASS marker-d: journal-current attempt is primary; the kept canonical is captured as source-2"

# case M-e: a completed run (terminal_capture=2) captures exactly attributed sources (a
# marker, a protected descriptor, a live/reclaimed ledger attempt of this run); the
# journal alone stays status-only.
reset_layout
make_clone "$CANON" agent/issue-4242 kept-canon
journal completed-1 "$CANON"
D_ME="$WORK/out.marker-e"
rc="$(backup_rc "$D_ME" completed-1)"
[ "$rc" -eq 0 ] || fail "marker-e journal-only: exit $rc, want 0"
grep -q 'SNAP completed-1 .*status=completed' "$D_ME/latest-attempt/backup.log" \
  || fail "marker-e journal-only: expected SNAP; got: $(cat "$D_ME/latest-attempt/backup.log")"
[ ! -e "$D_ME/latest-attempt/issue-4242.tgz" ] || fail "marker-e journal-only: a terminal run was captured from the journal"
stopped_marker completed-1 agent/issue-4242 issue-4242 "$CANON"
rc="$(backup_rc "$D_ME" completed-1)"
[ "$rc" -eq 0 ] || fail "marker-e marker: exit $rc, want 0"
tar -xOzf "$D_ME/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+kept-canon' \
  || fail "marker-e marker: kept canonical dirty bytes not captured; log: $(cat "$D_ME/latest-attempt/backup.log")"
# A live ledger attempt of the terminal run is an attributed source beside the marker:
# the marker canonical stays primary and the attempt is source-2.
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 newer-attempt
ledger "$A2" completed-1 "$RB/issue-4242.attempt-$A2" live
rc="$(backup_rc "$D_ME" completed-1)"
[ "$rc" -eq 0 ] || fail "marker-e ledger: exit $rc, want 0"
tar -xOzf "$D_ME/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+kept-canon' \
  || fail "marker-e ledger: a ledger attempt displaced the marker-named canonical"
tar -xOzf "$D_ME/latest/issue-4242.source-2.tgz" ./issue-4242.source-2.uncommitted.patch | grep -qxF '+newer-attempt' \
  || fail "marker-e ledger: the live ledger attempt was not captured as source-2; log: $(log_of "$D_ME")"
# A journal-current on-branch attempt of the terminal run is not promoted by the journal:
# the marker canonical stays primary (kills a mutation that drops the terminal-mode
# journal clearing); the attempt is still captured, through its live ledger entry.
reset_layout
make_clone "$CANON" agent/issue-4242 kept-canon
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 live-attempt
journal completed-1 "$RB/issue-4242.attempt-$A2" "$A2"
ledger "$A2" completed-1 "$RB/issue-4242.attempt-$A2" live
stopped_marker completed-1 agent/issue-4242 issue-4242 "$CANON"
D_ME2="$WORK/out.marker-e2"
rc="$(backup_rc "$D_ME2" completed-1)"
[ "$rc" -eq 0 ] || fail "marker-e journal-current: exit $rc, want 0"
tar -xOzf "$D_ME2/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+kept-canon' \
  || fail "marker-e journal-current: marker canonical not captured; log: $(cat "$D_ME2/latest-attempt/backup.log")"
tar -xOzf "$D_ME2/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+live-attempt' \
  && fail "marker-e journal-current: the journal-current attempt became the primary of a terminal run"
tar -xOzf "$D_ME2/latest/issue-4242.source-2.tgz" ./issue-4242.source-2.uncommitted.patch | grep -qxF '+live-attempt' \
  || fail "marker-e journal-current: the live ledger attempt was not captured as source-2; log: $(log_of "$D_ME2")"
# A marker whose canonical dir is gone: the run's live ledger attempt is still an
# attributed source, so it is captured (primary) instead of falling to SNAP.
reset_layout
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 ledger-attempt
ledger "$A2" completed-1 "$RB/issue-4242.attempt-$A2" live
stopped_marker completed-1 agent/issue-4242 issue-4242 "$CANON"
D_ME3="$WORK/out.marker-e3"
rc="$(backup_rc "$D_ME3" completed-1)"
[ "$rc" -eq 0 ] || fail "marker-e canonical-gone: exit $rc, want 0"
tar -xOzf "$D_ME3/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+ledger-attempt' \
  || fail "marker-e canonical-gone: the live ledger attempt was not captured; log: $(log_of "$D_ME3")"
echo "PASS marker-e: completed run captures marker canonical and live ledger attempts; journal alone stays status-only"

# case M-f(a): live successor (journal-current, ledger live) + detached dirty canonical
# predecessor with a valid marker: both captured (same layout as marker-d, asserted
# through source_check including that no predecessor byte is in the primary).
reset_layout
make_clone "$CANON" agent/issue-4242 kept-canon
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 live-attempt
journal run-4242 "$RB/issue-4242.attempt-$A2" "$A2"
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
stopped_marker run-4242 agent/issue-4242 issue-4242 "$CANON"
source_check marker-f-a "$RB/issue-4242.attempt-$A2" live-attempt "$CANON" kept-canon
tar -tzf "$WORK/out.marker-f-a/latest/issue-4242.source-2.tgz" | grep -q 'issue-4242[.]source-2[.]meta[.]txt' \
  || fail "marker-f-a: source-2 inner members not named by the source stem"
[ ! -e "$WORK/out.marker-f-a/latest-attempt/.coverage-gap" ] || fail "marker-f-a: gap state file left behind"

# (b) Docker-wired: live successor A2 (journal-current) + dirty predecessor attempt A1
# whose ledger state is reclaimed: both captured.
reset_layout
make_clone "$RB/issue-4242.attempt-$A1" agent/issue-4242 reclaimed-a1
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 live-attempt
journal run-4242 "$RB/issue-4242.attempt-$A2" "$A2"
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" reclaimed
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
source_check marker-f-b "$RB/issue-4242.attempt-$A2" live-attempt "$RB/issue-4242.attempt-$A1" reclaimed-a1

# A retired (or abandoned) ledger predecessor is NOT an extra source.
for st_retired in retired abandoned; do
  reset_layout
  make_clone "$RB/issue-4242.attempt-$A1" agent/issue-4242 old-a1
  make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 live-attempt
  journal run-4242 "$RB/issue-4242.attempt-$A2" "$A2"
  ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" "$st_retired"
  ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
  D_RET="$WORK/out.marker-f-$st_retired"
  rc="$(backup_rc "$D_RET" run-4242)"
  [ "$rc" -eq 0 ] || fail "marker-f $st_retired: exit $rc, want 0"
  [ -f "$D_RET/latest/issue-4242.tgz" ] || fail "marker-f $st_retired: no primary"
  [ ! -e "$D_RET/latest/issue-4242.source-2.tgz" ] || fail "marker-f $st_retired: a $st_retired attempt was captured as an extra"
done

# (c) as (a) but the extra's capture fails: exit 1, incomplete coverage, primary kept.
reset_layout
make_clone "$CANON" agent/issue-4242 kept-canon
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 live-attempt
journal run-4242 "$RB/issue-4242.attempt-$A2" "$A2"
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
stopped_marker run-4242 agent/issue-4242 issue-4242 "$CANON"
D_FC="$WORK/out.marker-f-c"
rc="$(backup_rc "$D_FC" run-4242 "UZI_TEST_CAPFAIL_PATH=$CANON")"
[ "$rc" -eq 1 ] || fail "marker-f-c: exit $rc, want 1"
grep -q 'incomplete coverage' "$D_FC/latest-attempt/backup.log" \
  || fail "marker-f-c: missing 'incomplete coverage'; got: $(cat "$D_FC/latest-attempt/backup.log")"
[ -f "$D_FC/latest-attempt/issue-4242.tgz" ] || fail "marker-f-c: primary artifact missing"
[ ! -e "$D_FC/latest" ] || fail "marker-f-c: latest advanced despite incomplete coverage"
[ ! -e "$D_FC/latest-attempt/.coverage-gap" ] || fail "marker-f-c: gap state file left behind"

# (d) two pods, exec into the second fails: its source search is inconclusive, so the
# run fails closed even though the primary (current worker's pod) was captured.
D_FD="$WORK/out.marker-f-d"
rc="$(backup_rc "$D_FD" run-4242 "UZI_TEST_PODS=pod/uzi-hw-$WID-test pod/uzi-hw-second-test" "UZI_TEST_EXECFAIL_POD=uzi-hw-second-test")"
[ "$rc" -eq 1 ] || fail "marker-f-d: exit $rc, want 1"
grep -q 'incomplete coverage' "$D_FD/latest-attempt/backup.log" \
  || fail "marker-f-d: missing 'incomplete coverage'; got: $(cat "$D_FD/latest-attempt/backup.log")"
grep -q 'incomplete coverage.*exec into pod ns/uzi-hw-second-test failed' "$D_FD/latest-attempt/backup.log" \
  || fail "marker-f-d: the FAIL line does not name the failing pod; got: $(cat "$D_FD/latest-attempt/backup.log")"
[ -f "$D_FD/latest-attempt/issue-4242.tgz" ] || fail "marker-f-d: primary artifact missing"
echo "PASS marker-f: every attributed source captured; retired not extra; a coverage gap fails closed"

# --- coverage review fixes (issue 1786) --------------------------------------------
BADA1="$RB/issue-4242.attempt-$A1"
# bad_dir <path>: a dir whose .git exists but cannot be read.
bad_dir() { mkdir -p "$1"; echo 'gitdir: /nonexistent' > "$1/.git"; }

# B1+B2: no readable clone, the ledger names an unreadable attempt live for the run, and
# the run owns a tracking ref. The BARE artifact is kept, but it is not full coverage:
# exit 1, latest not advanced, and the unreadable attributed source is in the total.
reset_layout
bad_dir "$BADA1"
ledger "$A1" run-4242 "$BADA1" live
TRK="$WORK/trk"
make_clone "$TRK" agent/issue-4242 trk
git -C "$TRK" -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m work
git --git-dir="$BARE" fetch -q "$TRK" agent/issue-4242:refs/uzi-runner/agent/issue-4242
D_B1="$WORK/out.cov-b1"
rc="$(backup_rc "$D_B1" run-4242)"
[ "$rc" -eq 1 ] || fail "cov-b1: exit $rc, want 1; log: $(log_of "$D_B1")"
grep -q 'BARE run-4242' "$D_B1/latest-attempt/backup.log" || fail "cov-b1: BARE artifact not logged"
[ -f "$D_B1/latest-attempt/issue-4242.tgz" ] || fail "cov-b1: BARE artifact not kept"
grep -q 'FAIL run-4242 .*incomplete coverage: 1 of 2 attributed sources captured' "$D_B1/latest-attempt/backup.log" \
  || fail "cov-b1: missing 'incomplete coverage: 1 of 2'; log: $(log_of "$D_B1")"
[ ! -e "$D_B1/latest" ] || fail "cov-b1: latest advanced despite incomplete coverage"
git --git-dir="$BARE" update-ref -d refs/uzi-runner/agent/issue-4242

# B2 (clone capture): one captured + one unreadable attributed source is "1 of 2".
reset_layout
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 live-attempt
bad_dir "$BADA1"
journal run-4242 "$RB/issue-4242.attempt-$A2" "$A2"
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
ledger "$A1" run-4242 "$BADA1" live
D_B2="$WORK/out.cov-b2"
rc="$(backup_rc "$D_B2" run-4242)"
[ "$rc" -eq 1 ] || fail "cov-b2: exit $rc, want 1"
grep -q 'incomplete coverage: 1 of 2 attributed sources captured' "$D_B2/latest-attempt/backup.log" \
  || fail "cov-b2: missing '1 of 2'; log: $(log_of "$D_B2")"
[ -f "$D_B2/latest-attempt/issue-4242.tgz" ] || fail "cov-b2: primary not kept"

# R2: a failed PRIMARY capture also reports the run's incomplete coverage (0 captured).
reset_layout
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 live-attempt
journal run-4242 "$RB/issue-4242.attempt-$A2" "$A2"
D_R2="$WORK/out.cov-r2"
rc="$(backup_rc "$D_R2" run-4242 "UZI_TEST_CAPFAIL_PATH=$RB/issue-4242.attempt-$A2")"
[ "$rc" -eq 1 ] || fail "cov-r2: exit $rc, want 1"
grep -q 'incomplete coverage: 0 of 1 attributed sources captured' "$D_R2/latest-attempt/backup.log" \
  || fail "cov-r2: missing 'incomplete coverage: 0 of 1'; log: $(log_of "$D_R2")"

# R3: a completed run with a valid protected descriptor captures the descriptor's
# source as the primary; a live ledger attempt of the exact run, which the descriptor
# does not name, is an attributed source too (source-2).
reset_layout
rid=completed-1
predecessor="$RB/issue-4242"
make_clone "$predecessor" agent/issue-4242 desc-old-dirty
make_clone "$RB/issue-4242.attempt-$A3" agent/issue-4242 ledger-only-live
ledger "$A3" "$rid" "$RB/issue-4242.attempt-$A3" live
envelope="$(jq -cn --arg rid "$rid" --arg pred "$predecessor" '
  {runId:$rid,clonePath:$pred} as $source
  | {version:1,branch:"agent/issue-4242",key:"issue-4242",journal:($source + {retainedSources:[$source]})}')"
git --git-dir="$BARE" config "uzi-retained.$rid.journal" "$envelope"
D_R3="$WORK/out.cov-r3"
rc="$(backup_rc "$D_R3" "$rid")"
[ "$rc" -eq 0 ] || fail "cov-r3: exit $rc, want 0; log: $(log_of "$D_R3")"
tar -xOzf "$D_R3/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+desc-old-dirty' \
  || fail "cov-r3: descriptor source not captured"
tar -xOzf "$D_R3/latest/issue-4242.source-2.tgz" ./issue-4242.source-2.uncommitted.patch | grep -qxF '+ledger-only-live' \
  || fail "cov-r3: the ledger-only live attempt was not captured as source-2; log: $(log_of "$D_R3")"
git --git-dir="$BARE" config --unset-all "uzi-retained.$rid.journal"

# N1: an unreadable dir that nothing names is not an attributed source: with the
# journal-named clone captured, the run is complete.
reset_layout
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 live-attempt
journal run-4242 "$RB/issue-4242.attempt-$A2" "$A2"
bad_dir "$BADA1"
D_N1="$WORK/out.cov-n1"
rc="$(backup_rc "$D_N1" run-4242)"
[ "$rc" -eq 0 ] || fail "cov-n1: exit $rc, want 0; log: $(log_of "$D_N1")"
tar -xOzf "$D_N1/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+live-attempt' \
  || fail "cov-n1: primary not captured"
! grep -q 'incomplete coverage' "$D_N1/latest-attempt/backup.log" || fail "cov-n1: unnamed unreadable dir reported as a gap"
# An unreadable dir the ledger names only as a RETIRED (or abandoned) attempt of the run
# is not an attributed source either: no gap. Named live, it is one.
ledger "$A1" run-4242 "$BADA1" retired
rc="$(backup_rc "$D_N1" run-4242)"
[ "$rc" -eq 0 ] || fail "cov-n1 retired: exit $rc, want 0; log: $(log_of "$D_N1")"
! grep -q 'incomplete coverage' "$D_N1/latest-attempt/backup.log" || fail "cov-n1 retired: an unreadable retired attempt reported as a gap"
ledger "$A1" run-4242 "$BADA1" live
rc="$(backup_rc "$D_N1" run-4242)"
[ "$rc" -eq 1 ] || fail "cov-n1 live: exit $rc, want 1"
grep -q 'incomplete coverage: 1 of 2' "$D_N1/latest-attempt/backup.log" || fail "cov-n1 live: missing gap; log: $(log_of "$D_N1")"

# N2: on a pod other than the one supplying the primary, only ATTRIBUTED sources are
# extras. Pod 2 holds a retired ledger attempt (select_clone's own fallback primary
# there): not captured. Once the ledger marks it live, it is source-2 from pod 2.
ALT="$WORK/altpod/runner"
reset_layout
rm -rf "$WORK/altpod"; mkdir -p "$ALT"
make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 pod1-live
make_clone "$ALT/issue-4242.attempt-$A1" agent/issue-4242 pod2-attempt
journal run-4242 "$RB/issue-4242.attempt-$A2" "$A2"
ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
ledger "$A1" run-4242 "$ALT/issue-4242.attempt-$A1" retired
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" retired
two_pods=("UZI_TEST_PODS=pod/uzi-hw-$WID-test pod/uzi-hw-second-test" "UZI_TEST_ALT_POD=uzi-hw-second-test"
  "UZI_TEST_ALT_BASE=$ALT" "UZI_TEST_HOST_BASE=$RB")
D_N2="$WORK/out.cov-n2"
rc="$(backup_rc "$D_N2" run-4242 "${two_pods[@]}")"
[ "$rc" -eq 0 ] || fail "cov-n2 retired: exit $rc, want 0; log: $(log_of "$D_N2")"
tar -xOzf "$D_N2/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+pod1-live' \
  || fail "cov-n2 retired: primary is not pod 1's journal-current clone"
[ ! -e "$D_N2/latest/issue-4242.source-2.tgz" ] || fail "cov-n2: a retired fallback attempt on pod 2 was captured as an extra"
ledger "$A1" run-4242 "$RB/issue-4242.attempt-$A1" live
D_N2B="$WORK/out.cov-n2b"
rc="$(backup_rc "$D_N2B" run-4242 "${two_pods[@]}")"
[ "$rc" -eq 0 ] || fail "cov-n2 live: exit $rc, want 0; log: $(log_of "$D_N2B")"
tar -xOzf "$D_N2B/latest/issue-4242.source-2.tgz" ./issue-4242.source-2.uncommitted.patch | grep -qxF '+pod2-attempt' \
  || fail "cov-n2 live: pod 2's attributed attempt was not captured as source-2; log: $(log_of "$D_N2B")"
rm -rf "$WORK/altpod"
echo "PASS coverage review fixes: bare gap, total, primary failure, terminal descriptor, unnamed dir, per-pod extras"

# --- review fixes on PR #2793 ----------------------------------------------------------
# T1: a completed/cancelled run captures its exactly attributed LEDGER sources even with
# no marker and no protected descriptor (the journal alone stays status-only).
for rid in completed-1 cancelled-1; do
  reset_layout
  make_clone "$BADA1" agent/issue-4242 dirty-predecessor
  ledger "$A1" "$rid" "$BADA1" reclaimed
  D_T1="$WORK/out.t1.$rid"
  rc="$(backup_rc "$D_T1" "$rid")"
  [ "$rc" -eq 0 ] || fail "t1 $rid: exit $rc, want 0; log: $(log_of "$D_T1")"
  tar -xOzf "$D_T1/latest/issue-4242.tgz" ./issue-4242.uncommitted.patch | grep -qxF '+dirty-predecessor' \
    || fail "t1 $rid: the reclaimed attempt's dirty bytes were not captured; log: $(log_of "$D_T1")"
done
# An unreadable reclaimed attempt of the terminal run is an attributed source we could
# not read: FAIL, retry, latest not advanced, and the FAIL line says why.
reset_layout
bad_dir "$BADA1"
ledger "$A1" completed-1 "$BADA1" reclaimed
D_T1B="$WORK/out.t1.unreadable"
rc="$(backup_rc "$D_T1B" completed-1)"
[ "$rc" -eq 1 ] || fail "t1 unreadable: exit $rc, want 1; log: $(log_of "$D_T1B")"
grep -q 'FAIL completed-1 .*inconclusive (.*attributed source .* on pod ns/uzi-hw-w0rker-test is listed but unreadable.*); retry' "$D_T1B/latest-attempt/backup.log" \
  || fail "t1 unreadable: FAIL line does not give the unreadable-source reason; log: $(log_of "$D_T1B")"
[ ! -e "$D_T1B/latest" ] || fail "t1 unreadable: latest advanced"
# A retired attempt is never a source: status-only, success.
reset_layout
make_clone "$BADA1" agent/issue-4242 retired-dirty
ledger "$A1" completed-1 "$BADA1" retired
D_T1C="$WORK/out.t1.retired"
rc="$(backup_rc "$D_T1C" completed-1)"
[ "$rc" -eq 0 ] || fail "t1 retired: exit $rc, want 0; log: $(log_of "$D_T1C")"
grep -q 'SNAP completed-1 .*status=completed' "$D_T1C/latest-attempt/backup.log" \
  || fail "t1 retired: expected SNAP; log: $(log_of "$D_T1C")"
[ ! -e "$D_T1C/latest-attempt/issue-4242.tgz" ] || fail "t1 retired: a retired attempt was captured"
echo "PASS t1: terminal runs capture live/reclaimed ledger attempts; retired is status-only; unreadable fails"

# T2: an attributed attempt dir with no .git that holds a file is not "absent": it may
# be an unreadable source, so coverage is incomplete. Unattributed, it is ignored.
t2_layout() {
  reset_layout
  make_clone "$RB/issue-4242.attempt-$A2" agent/issue-4242 live-attempt
  journal run-4242 "$RB/issue-4242.attempt-$A2" "$A2"
  ledger "$A2" run-4242 "$RB/issue-4242.attempt-$A2" live
  mkdir -p "$BADA1"; echo stray > "$BADA1/stray-file"
}
t2_layout
ledger "$A1" run-4242 "$BADA1" live
D_T2="$WORK/out.t2.ledger"
rc="$(backup_rc "$D_T2" run-4242)"
[ "$rc" -eq 1 ] || fail "t2 ledger: exit $rc, want 1; log: $(log_of "$D_T2")"
grep -q 'incomplete coverage: 1 of 2 attributed sources captured' "$D_T2/latest-attempt/backup.log" \
  || fail "t2 ledger: missing 'incomplete coverage: 1 of 2'; log: $(log_of "$D_T2")"
[ -f "$D_T2/latest-attempt/issue-4242.tgz" ] || fail "t2 ledger: primary artifact not kept"
[ ! -e "$D_T2/latest" ] || fail "t2 ledger: latest advanced"
# Variant: the journal (not the ledger) names the .git-less dir.
t2_layout
journal run-4242 "$BADA1" "$A1"
D_T2B="$WORK/out.t2.journal"
rc="$(backup_rc "$D_T2B" run-4242)"
[ "$rc" -eq 1 ] || fail "t2 journal: exit $rc, want 1; log: $(log_of "$D_T2B")"
grep -q 'incomplete coverage: 1 of 2 attributed sources captured' "$D_T2B/latest-attempt/backup.log" \
  || fail "t2 journal: missing 'incomplete coverage: 1 of 2'; log: $(log_of "$D_T2B")"
[ -f "$D_T2B/latest-attempt/issue-4242.tgz" ] || fail "t2 journal: primary artifact not kept"
[ ! -e "$D_T2B/latest" ] || fail "t2 journal: latest advanced"
# Variant: the ledger-named dir cannot be listed (mode 000). chmod cannot lock out root,
# so this one needs an unprivileged user.
if [ "$(id -u)" -ne 0 ]; then
  t2_layout
  ledger "$A1" run-4242 "$BADA1" live
  chmod 000 "$BADA1"
  D_T2L="$WORK/out.t2.locked"
  rc="$(backup_rc "$D_T2L" run-4242)"
  chmod 755 "$BADA1"
  [ "$rc" -eq 1 ] || fail "t2 locked: exit $rc, want 1; log: $(log_of "$D_T2L")"
  grep -q 'incomplete coverage: 1 of 2 attributed sources captured' "$D_T2L/latest-attempt/backup.log" \
    || fail "t2 locked: missing 'incomplete coverage: 1 of 2'; log: $(log_of "$D_T2L")"
  [ -f "$D_T2L/latest-attempt/issue-4242.tgz" ] || fail "t2 locked: primary artifact not kept"
  [ ! -e "$D_T2L/latest" ] || fail "t2 locked: latest advanced"
fi
# Variant: nothing names the dir, so it is not a gap.
t2_layout
D_T2C="$WORK/out.t2.unattributed"
rc="$(backup_rc "$D_T2C" run-4242)"
[ "$rc" -eq 0 ] || fail "t2 unattributed: exit $rc, want 0; log: $(log_of "$D_T2C")"
[ -f "$D_T2C/latest/issue-4242.tgz" ] || fail "t2 unattributed: no capture"
echo "PASS t2: an attributed .git-less dir holding files is a gap; unattributed is ignored"

# T3: a no-capture FAIL names what failed instead of a fixed parenthetical.
reset_layout
D_T3="$WORK/out.t3.exec"
rc="$(backup_rc "$D_T3" failed-2 UZI_TEST_EXECFAIL=1)"
[ "$rc" -eq 1 ] || fail "t3 exec: exit $rc, want 1"
grep -q 'FAIL failed-2 .*inconclusive (.*exec into pod ns/uzi-hw-w0rker-test failed.*); retry' "$D_T3/latest-attempt/backup.log" \
  || fail "t3 exec: FAIL line does not name the pod; log: $(log_of "$D_T3")"
D_T3B="$WORK/out.t3.list"
rc="$(backup_rc "$D_T3B" failed-2 UZI_TEST_GETFAIL=1)"
[ "$rc" -eq 1 ] || fail "t3 list: exit $rc, want 1"
grep -q 'FAIL failed-2 .*inconclusive (.*listing Running pods in namespace ns failed.*); retry' "$D_T3B/latest-attempt/backup.log" \
  || fail "t3 list: FAIL line does not name the namespace; log: $(log_of "$D_T3B")"
# Only the candidate listing's rc 3 (an unattributed unreadable .git) set the marker.
reset_layout
bad_dir "$BADA1"
D_T3C="$WORK/out.t3.rc3"
rc="$(backup_rc "$D_T3C" failed-2)"
[ "$rc" -eq 1 ] || fail "t3 rc3: exit $rc, want 1"
grep -qF "FAIL failed-2 (#4242) status=failed: status saved, but the source search was inconclusive (a candidate's .git on pod ns/uzi-hw-w0rker-test could not be read); retry" "$D_T3C/latest-attempt/backup.log" \
  || fail "t3 rc3: FAIL line mismatch; log: $(log_of "$D_T3C")"
! grep -q 'inconclusive ()' "$D_T3C/latest-attempt/backup.log" || fail "t3 rc3: empty parenthetical"
[ ! -e "$D_T3C/latest-attempt/.unreadable-git" ] || fail "t3 rc3: .unreadable-git state file left behind"
# An unreadable .git on one pod is named even when another pod's exec also failed.
D_T3D="$WORK/out.t3.both"
rc="$(backup_rc "$D_T3D" failed-2 "UZI_TEST_PODS=pod/uzi-hw-$WID-test pod/uzi-hw-second-test" "UZI_TEST_EXECFAIL_POD=uzi-hw-second-test")"
[ "$rc" -eq 1 ] || fail "t3 both: exit $rc, want 1"
grep -q "FAIL failed-2 .*inconclusive (.*exec into pod ns/uzi-hw-second-test failed.*a candidate's .git on pod ns/uzi-hw-$WID-test could not be read); retry" "$D_T3D/latest-attempt/backup.log" \
  || fail "t3 both: FAIL line must name both pods; log: $(log_of "$D_T3D")"
echo "PASS t3: the no-capture FAIL line names the failing pod, namespace or unreadable .git"

echo "ALL PASS"
