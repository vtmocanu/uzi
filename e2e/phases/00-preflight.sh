# shellcheck shell=bash
# phase:    preflight
# title:    PRD #966 M3: preflight — harness invariants (#366 dubious-ownership / #372 cross-uid push race)
# critical: yes
# lane:     any
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
#
# Runs FIRST (NN=00 sorts before 05) and asserts, before any product phase, the two
# harness invariants the two recurring red classes came from: #366 (git "detected
# dubious ownership") and #372 (cross-uid push race on the shared bare). It fails FAST
# with a precise remedy that quotes the lib.sh comment anchors, so a red here reads as
# "the harness is mis-provisioned" rather than as a product regression 40 phases later.
#
# Leg 4 (a cross-uid receive-pack FROM the agent container) is CONDITIONAL and at phase
# 00 ALWAYS takes the deferred-note path: the agent container is not up yet (only
# db/api/web/forge-fake are, until phase 13 brings the worker online), so there is no
# container to exec into. That is correct and intended — the real cross-uid push is
# exercised for real by git-push-basic-auth (phase 20). The conditional real probe below
# exists only for the (non-default) case of running preflight with the agent already up.
# --- PRD #966 M3: preflight — harness invariants ------------------------------
say "preflight: harness invariants before any product phase (#366 dubious-ownership / #372 cross-uid push race)"

# --- 1. core.sharedRepository is world-shared on BOTH fake bares (#372) -------
# git records lib.sh's `git init --bare --shared=0777` as core.sharedRepository=0666
# (it strips the execute bits, re-adding them for directories itself); an omitted
# --shared (the E2E_FAULT_PREFLIGHT positive control) leaves the key UNSET. Either an
# unset or a non-world value lets the #372 cross-uid push race reappear, so assert the
# key is set to a world-shared value — NOT the literal 0777, which git never stores.
for r in repo repo2; do
  cfg="$RUNROOT/fakeremote/$r.git/config"
  shared_val="$(git config --file "$cfg" core.sharedRepository 2>/dev/null || true)"
  case "$shared_val" in
    ""|0|false|umask|1|group|true)
      fail "preflight: $r.git core.sharedRepository is '${shared_val:-unset}', want a world-shared value (lib.sh sets --shared=0777, which git records as 0666). Without it the bare is written by more than one uid (forge-fake receive-pack as root, the worker as its own uid) and a cross-uid object write fails 'unable to migrate objects to permanent storage'. Remedy: keep the bare init at --shared=0777 — see the lib.sh anchor '--shared=0777 is LOAD-BEARING'. (E2E_FAULT_PREFLIGHT=1 reproduces this by omitting --shared.)" ;;
  esac
done
pass "both fake bares carry a world-shared core.sharedRepository"

# --- 2. safe.directory = * in the worker's GIT_CONFIG_GLOBAL (#366) -----------
gc="$RUNROOT/agent-gitconfig/gitconfig"
grep -qF 'directory = *' "$gc" \
  || fail "preflight: safe.directory=* missing from $gc. The bind-mounted bare's owner uid differs from the in-container uid on a CI runner, so git trips 'detected dubious ownership' and refuses the push; gitEnv()'s command-scope pin is stripped before the spawned receive-pack, so the trust MUST live in this GIT_CONFIG_GLOBAL file. Remedy: see the lib.sh anchor 'safe.directory=* is REQUIRED here'."
pass "worker gitconfig carries safe.directory=*"

# --- 3. host-side receive-pack smoke: a non-main branch push is accepted ------
# Proves the bare accepts a non-main push and the pre-receive hook permits it (the hook
# must refuse ONLY main). No container needed. -c safe.directory='*' on every host git
# op because the bind-mount uid may differ from ours on a CI runner (the #366 caveat).
probe_ref="refs/heads/e2e-preflight-probe"
probe_wc="$RUNROOT/.preflight-probe-wc"
rm -rf "$probe_wc"
git -c safe.directory='*' clone -q "$RUNROOT/fakeremote/repo.git" "$probe_wc" \
  || fail "preflight: could not clone the fake bare repo.git host-side (see the lib.sh anchor 'safe.directory=* is REQUIRED here')"
git -C "$probe_wc" -c safe.directory='*' -c user.name=preflight -c user.email=preflight@uzi.e2e \
  -c commit.gpgsign=false commit -q --allow-empty -m "preflight probe" \
  || fail "preflight: could not create a probe commit host-side"
git -C "$probe_wc" -c safe.directory='*' push -q origin "HEAD:$probe_ref" \
  || fail "preflight: host-side receive-pack REFUSED a non-main branch push to repo.git — the bare rejects branch pushes (or the pre-receive hook is over-broad, refusing more than main). See the lib.sh anchor '--shared=0777 is LOAD-BEARING'."
git -C "$probe_wc" -c safe.directory='*' push -q origin ":$probe_ref" >/dev/null 2>&1 || true  # delete the throwaway ref
rm -rf "$probe_wc"
pass "host-side receive-pack accepts a non-main branch push (pre-receive permits it)"

# --- 4. cross-uid container receive-pack — CONDITIONAL (#372) -----------------
# See the header note: at phase 00 the agent is DOWN, so this takes the deferred-note
# path by default. A real probe runs only if the agent is already online.
if "${COMPOSE[@]}" ps --status running --services 2>/dev/null | grep -Fxq agent; then
  # shellcheck disable=SC2016  # $d/$var must expand INSIDE the container, not host-side
  "${COMPOSE[@]}" exec -T agent sh -lc '
    set -e
    d="$(mktemp -d)"
    git -c safe.directory="*" clone -q /fakeremote/repo.git "$d/wc"
    git -C "$d/wc" -c safe.directory="*" -c user.name=preflight -c user.email=preflight@uzi.e2e -c commit.gpgsign=false commit -q --allow-empty -m preflight-container
    git -C "$d/wc" -c safe.directory="*" push -q origin HEAD:refs/heads/e2e-preflight-container
    git -C "$d/wc" -c safe.directory="*" push -q origin :refs/heads/e2e-preflight-container
    rm -rf "$d"
  ' || fail "preflight: cross-uid receive-pack from the agent container FAILED — the #372 class (a uid that did not create objects/xx/ cannot add into it without sharedRepository). See the lib.sh anchor '--shared=0777 is LOAD-BEARING'."
  pass "cross-uid receive-pack from the agent container succeeds"
else
  pass "agent not yet online at preflight; cross-uid receive-pack is exercised for real by git-push-basic-auth (phase 20)"
fi
