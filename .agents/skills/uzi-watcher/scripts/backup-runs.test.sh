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
  # Execute the real bare-repo probes. This covers the public-main read and the
  # missing-clone fallback's show-ref lookup against the local fake worker bare.
  exec "\${cmd[@]}"
fi
if [ "\${cmd[0]:-}" = sh ]; then
  # cmd = (sh -c <CAPTURE> _ <STEM> <REALMAIN> <RUNNER_BASE>); the host already
  # forwarded RUNNER_BASE as the last arg, so run it verbatim.
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
# pipeline_ref (-> agent-issue-9999); anything else is the issue-4242 run.
make_uzi_stub() {
  cat > "$WORK/uzi" <<STUB
#!/usr/bin/env bash
set -u
if [ "\${1:-}" = run ] && [ "\${2:-}" = get ]; then
  case "\${3:-}" in
    *mrr*) printf '%s' '{"status":"running","issue_iid":null,"kind":"mr_rework","branch":null,"pipeline_ref":"agent/issue-9999","worker_id":"${WID:-w0rker}","mr_iid":7777,"mr_web_url":null,"health_reason":"ok","anthropic_secret_label":"tok","anthropic_bind_mode":"default","milestones":[],"milestones_completed":[]}' ;;
    *noworker*) printf '%s' '{"status":"running","issue_iid":4242,"kind":"issue","branch":null,"pipeline_ref":null,"worker_id":null,"mr_web_url":null,"health_reason":"ok","anthropic_secret_label":"tok","anthropic_bind_mode":"default","milestones":[],"milestones_completed":[]}' ;;
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
# UZI_RUNNER_BASE is set on the HOST invocation (not the stub): the script must
# forward it into the remote capture, which is the production behavior under test.
run_backup() {
  local dest="$WORK/out.$1"; local rid="${2:-run-4242}"
  rm -rf "$dest"
  UZI_CTX=test-ctx UZI_WORKER_NS=ns UZI_REPO_SLUG=testrepo UZI_RUNNER_BASE="$WORK/runner" \
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
tar -xOzf "$L1_DUBIOUS/issue-4242.tgz" issue-4242.uncommitted.patch | grep -qF '+dirty' \
  || fail "dubious owner: live uncommitted work was not captured"
tar -xOzf "$L1_DUBIOUS/issue-4242.tgz" issue-4242.meta.txt | grep -qF "head=$MAIN" \
  || fail "dubious owner: Git HEAD was not captured"
echo "PASS dubious owner: exact clone trusted; patch and HEAD kept"

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
L6_QUEUED="$(run_backup 6.queued queued-1)"
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

echo "ALL PASS"
