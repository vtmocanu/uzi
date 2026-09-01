# shellcheck shell=bash
# phase:    handoff
# title:    PRD #966 M5: task/handoff run kind via `uzi handoff` (+ host-gitconfig transport)
# critical: no
# lane:     gitlab
# executor: any
# requires: REPO_ID UZI_BIN UZI_TOKEN_VAL
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #966 M5 — the `task` run kind (dispatched by `uzi handoff`, alias `task`) had zero
# wire coverage. Unlike an issue or a scheduled run, handoff drives git CLIENT-SIDE from
# the CALLER's cwd: it CreateTaskRun → `git push origin HEAD:refs/heads/uzi/task/<id>`
# (handoff.go:145) → dispatch. That push, and `handoff rm`'s `git push --delete`, run
# under uzi_cli's `env -i`, so the git child sees only that env — which is why lib.sh
# now writes $RUNROOT/host-gitconfig (host-path insteadOf → local bare, plus
# safe.directory=* for the #366 dubious-ownership class) and points uzi_cli's
# GIT_CONFIG_GLOBAL at it. This phase proves that transport with a positive control
# BEFORE any handoff (so a transport mistake fails in one line, not as a worker-clone
# timeout), then drives both handoff legs: a plain (no-MR) task whose branch is rm-able,
# and an --mr task whose branch is exempt from rm.
#
# A task run AUTO-RUNS: auto_approve is baked true (task.sql), so there is NO plan gate
# and NO approve step (unlike the issue phase) — wait_status ... completed works
# directly. The kind-agnostic stub seeds the branch off the pushed origin HEAD, writes
# UZI_RUN.md + one "uzi stub" commit, and ALWAYS pushes the branch back (even for a
# no-MR task), giving the topology seed ← marker ← stub on uzi/task/<id>.
say "PRD #966 M5: task/handoff run kind via \`uzi handoff\` (+ host-gitconfig transport)"

# --- Setup: a host-side clone of the bare, under the host gitconfig -----------
# hgit runs the phase's OWN host git (clone/commit/fetch/ls-remote/remote) — these are
# NOT uzi_cli, so they need GIT_CONFIG_GLOBAL set explicitly to reach the same host
# gitconfig the handoff CLI reads (insteadOf rewrites the https URL to the local bare;
# safe.directory=* trusts the bind-mounted bare on a CI runner). remote.origin.url stays
# the https URL — only the transport is rewritten.
WC="$RUNROOT/handoff-wc"
rm -rf "$WC"
hgit() { GIT_CONFIG_GLOBAL="$RUNROOT/host-gitconfig" git -C "$WC" "$@"; }
GIT_CONFIG_GLOBAL="$RUNROOT/host-gitconfig" git clone -q https://forge-fake.e2e/group/repo.git "$WC" \
  || fail "handoff: host-side clone of the fake bare (via host-gitconfig insteadOf) FAILED — the transport is misconfigured. Check \$RUNROOT/host-gitconfig's insteadOf + safe.directory (see the lib.sh anchor 'HOST-side CLI's GIT_CONFIG_GLOBAL for \`uzi handoff\`')."

# --- Positive control: origin resolves to the https URL AND the bare is reachable ----
# resolveHandoffRepo (handoff.go:180) reads `git remote get-url origin` and parses it to
# group/repo; the phase reads the SAME url. ls-remote proves the insteadOf rewrite + bare
# reachability BEFORE the handoff, so a transport mistake fails here in one line rather
# than as a worker-clone timeout deep in the run.
ORIGIN_URL="$(hgit remote get-url origin)" || fail "handoff: git remote get-url origin failed"
[ "$ORIGIN_URL" = 'https://forge-fake.e2e/group/repo.git' ] \
  || fail "handoff: origin url should be the https forge-fake URL (what resolveHandoffRepo parses), got '$ORIGIN_URL'"
hgit ls-remote origin >/dev/null \
  || fail "handoff: ls-remote origin failed — the host-gitconfig insteadOf does not reach the local bare"
pass "transport control: origin=$ORIGIN_URL, ls-remote reaches the bare"

# --- Leg 1: plain handoff (no MR) --------------------------------------------
# Commit a marker so the pushed branch tip is distinguishable, then hand off from inside
# $WC (cwd is where handoff reads origin and pushes from).
printf 'e2e handoff marker (leg 1, no MR)\n' > "$WC/HANDOFF_MARKER.md"
hgit add HANDOFF_MARKER.md || fail "handoff: git add HANDOFF_MARKER.md failed"
hgit -c user.name=e2e -c user.email=e2e@uzi.e2e -c commit.gpgsign=false commit -q -m 'e2e handoff marker' \
  || fail "handoff: committing the leg-1 marker failed"

HJSON_T="$(cd "$WC" && uzi_cli handoff --message 'e2e handoff' --json)" \
  || fail "handoff (no MR) failed (exit $?)"
RUN_T="$(printf '%s' "$HJSON_T" | jq -r '.id')"
{ [ -n "$RUN_T" ] && [ "$RUN_T" != null ]; } || fail "handoff returned no run id: $HJSON_T"
# Assert kind + branch from the SAME --json the dispatch printed (mr_iid is NULL here at
# dispatch — read the terminal value via apiget below).
[ "$(printf '%s' "$HJSON_T" | jq -r '.kind')" = task ] \
  || fail "handoff run kind should be 'task': $(printf '%s' "$HJSON_T" | jq -c '.kind')"
[ "$(printf '%s' "$HJSON_T" | jq -r '.branch')" = "uzi/task/$RUN_T" ] \
  || fail "handoff run branch should be 'uzi/task/$RUN_T': $(printf '%s' "$HJSON_T" | jq -c '.branch')"
[ "$(printf '%s' "$HJSON_T" | jq -r '.issue_iid')" = null ] \
  || fail "a task run must have a null issue_iid: $(printf '%s' "$HJSON_T" | jq -c '.issue_iid')"
pass "handoff (no MR) dispatched run $RUN_T (kind=task, branch=uzi/task/$RUN_T, issue_iid null)"

# A task run auto-runs — no approve step — so wait for terminal directly.
wait_status "$RUN_T" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"

KIND_T="$(uzi_cli run get "$RUN_T" --field kind)" || fail "uzi run get --field kind failed (exit $?)"
[ "$KIND_T" = task ] || fail "completed handoff run kind should be 'task', got '$KIND_T'"
RUN_T_JSON="$(apiget "/api/runs/$RUN_T")"
[ "$(printf '%s' "$RUN_T_JSON" | jq -r '.run.branch')" = "uzi/task/$RUN_T" ] \
  || fail "run branch should be uzi/task/$RUN_T: $(printf '%s' "$RUN_T_JSON" | jq -c '.run.branch')"
MR_T="$(printf '%s' "$RUN_T_JSON" | jq -r '.run.mr_iid')"
{ [ "$MR_T" = null ] || [ -z "$MR_T" ] || [ "$MR_T" = 0 ]; } \
  || fail "the plain (no-MR) handoff run must have no mr_iid, got '$MR_T'"
pass "handoff run $RUN_T completed through the stub as kind=task with no MR"

# The stub built ON the marker: fetch the pushed tip and assert seed ← marker ← stub.
hgit fetch -q origin "uzi/task/$RUN_T" || fail "handoff: fetch of uzi/task/$RUN_T failed"
hgit cat-file -e "FETCH_HEAD:HANDOFF_MARKER.md" 2>/dev/null \
  || fail "handoff: the marker (HANDOFF_MARKER.md) is missing from the stub's tip — the branch was not seeded off the pushed HEAD"
hgit cat-file -e "FETCH_HEAD:UZI_RUN.md" 2>/dev/null \
  || fail "handoff: the stub's UZI_RUN.md is missing from the tip — the stub did not build on the branch"
hgit cat-file -e "FETCH_HEAD:README.md" 2>/dev/null \
  || fail "handoff: the seed's README.md is missing from the tip — the seed commit was not preserved"
hgit log -1 --format=%s FETCH_HEAD | grep -qF 'uzi stub' \
  || fail "handoff: the tip commit subject should be the stub's 'uzi stub ...' commit, got '$(hgit log -1 --format=%s FETCH_HEAD)'"
pass "stub built on the marker: seed(README) <- marker(HANDOFF_MARKER.md) <- stub(UZI_RUN.md, 'uzi stub' commit)"

# `handoff rm` on a completed non-MR task succeeds and deletes the branch.
RM_JSON="$(cd "$WC" && uzi_cli handoff rm "$RUN_T" --json)" || fail "handoff rm on the completed no-MR run failed (exit $?): $RM_JSON"
[ "$(printf '%s' "$RM_JSON" | jq -r '.deleted')" = "uzi/task/$RUN_T" ] \
  || fail "handoff rm --json should report the deleted branch: $(printf '%s' "$RM_JSON" | jq -c '.')"
LSR="$(hgit ls-remote origin "refs/heads/uzi/task/$RUN_T")" || fail "handoff: ls-remote after rm failed"
[ -z "$LSR" ] || fail "handoff rm did not delete uzi/task/$RUN_T from origin: $LSR"
pass "handoff rm deleted uzi/task/$RUN_T (ls-remote now empty)"

# --- Leg 2: --mr handoff (MR opened, rm refused/exempt) ----------------------
# A second marker so this leg's pushed tip is a new HEAD, then hand off WITH --mr.
printf 'e2e handoff marker (leg 2, --mr)\n' > "$WC/HANDOFF_MARKER2.md"
hgit add HANDOFF_MARKER2.md || fail "handoff: git add HANDOFF_MARKER2.md failed"
hgit -c user.name=e2e -c user.email=e2e@uzi.e2e -c commit.gpgsign=false commit -q -m 'e2e handoff marker (mr leg)' \
  || fail "handoff: committing the leg-2 marker failed"

HJSON_M="$(cd "$WC" && uzi_cli handoff --mr --message 'e2e handoff mr' --json)" \
  || fail "handoff --mr failed (exit $?)"
RUN_M="$(printf '%s' "$HJSON_M" | jq -r '.id')"
{ [ -n "$RUN_M" ] && [ "$RUN_M" != null ]; } || fail "handoff --mr returned no run id: $HJSON_M"
[ "$(printf '%s' "$HJSON_M" | jq -r '.kind')" = task ] \
  || fail "handoff --mr run kind should be 'task': $(printf '%s' "$HJSON_M" | jq -c '.kind')"
[ "$(printf '%s' "$HJSON_M" | jq -r '.branch')" = "uzi/task/$RUN_M" ] \
  || fail "handoff --mr run branch should be 'uzi/task/$RUN_M': $(printf '%s' "$HJSON_M" | jq -c '.branch')"
pass "handoff --mr dispatched run $RUN_M (kind=task, branch=uzi/task/$RUN_M)"

wait_status "$RUN_M" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
# POSITIVE control that this leg differs from leg 1: an MR was opened (mr_iid > 0). This
# is also the positive control for the absence-style rm-refusal assertion below.
MR_M="$(apiget "/api/runs/$RUN_M" | jq -r '.run.mr_iid')"
{ [ "$MR_M" != null ] && [ -n "$MR_M" ] && [ "$MR_M" -gt 0 ]; } \
  || fail "the --mr handoff run should have opened an MR (mr_iid > 0), got '$MR_M'"
pass "handoff --mr run $RUN_M completed and opened MR !$MR_M"

# `handoff rm` on a run whose branch has an open MR is REFUSED (F1, handoff.go:393):
# non-zero exit, message names the open merge request / exemption. Its positive controls
# are leg 1's SUCCESSFUL rm (a working rm exists) and this leg's mr_iid>0 (an MR exists).
if RM_OUT_M="$(cd "$WC" && uzi_cli handoff rm "$RUN_M" 2>&1)"; then
  fail "handoff rm on the --mr run (MR !$MR_M) should have been REFUSED (exempt), but it succeeded: $RM_OUT_M"
fi
printf '%s' "$RM_OUT_M" | grep -qiE 'merge request|exempt' \
  || fail "handoff rm refusal should name the open merge request / exemption, got: $RM_OUT_M"
pass "handoff rm on the --mr run was refused (exempt): $(printf '%s' "$RM_OUT_M" | tr '\n' ' ' | cut -c1-120)"

# Both runs are terminal (completed), so quarantine is clean by construction. The --mr
# run's branch + open MR are left in place BY DESIGN (exempt from rm) and are harmless to
# later phases (M6's mr_rework operates on the phase-15 happy-path MR, not this one).
pass "task/handoff coverage complete (no-MR leg rm'd; --mr leg exempt, left by design)"
