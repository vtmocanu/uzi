# shellcheck shell=bash
# phase:    uid-boundary
# title:    PRD #51 M6: uid-boundary regression assertions (live image, setpriv-to-uid)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #51 M6 — uid-boundary REGRESSION assertions (image-level; every /proc read DROPS TO
# A UID via setpriv, so it is NON-vacuous where a root docker-exec would be — a root exec
# lacks in-container CAP_SYS_PTRACE and reads a cross-uid /proc/environ as EMPTY, auditor
# M6). These lock the A1 containment so it cannot silently regress. The config-source /
# packObjectsHook / commondir / env-scrub / distinct-TMPDIR-env / cap-clear-args invariants
# ALSO have agent/test unit guards (git-hardening / sdk-env / self-improve / provision /
# templates-guardrails / runner-uid); these are the LIVE-kernel half.
say "PRD #51 M6: uid-boundary regression assertions (live image, setpriv-to-uid)"

# The credential-holding worker node (uid 10001, runs src/main.ts): its /proc/<pid>/environ
# would hold any leaked join token / PAT, and it stands in for the push git-child (same
# 0400-owner boundary, uniform across worker processes).
WPID="$("${COMPOSE[@]}" exec -T agent sh -c 'for p in /proc/[0-9]*; do pid=${p#/proc/}; u=$(awk "/^Uid:/{print \$2}" "$p/status" 2>/dev/null); c=$(tr "\0" " " < "$p/cmdline" 2>/dev/null); case "$u:$c" in 10001:*node*main.ts*) echo "$pid"; break;; esac; done' | tr -d '\r')"
[ -n "$WPID" ] || fail "M6: could not find the worker node process (uid 10001, src/main.ts)"

# E1 — the RUNNER uid cannot read a WORKER process's /proc/<pid>/environ (the PAT-race
# close: a runner survivor during the worker's push cannot read the PAT out of the push
# git-child's environ). NON-vacuous control: as the SAME runner uid, that pid's world-
# readable cmdline (0444) IS readable, so the environ denial is the 0400-owner permission,
# not a dead/untraversable pid.
"${COMPOSE[@]}" exec -T agent /bin/setpriv --reuid runner --regid runner --init-groups -- sh -c "head -c1 /proc/$WPID/cmdline >/dev/null 2>&1" \
  || fail "M6 E1 control: runner could not read the worker's world-readable /proc/$WPID/cmdline (pid not live/traversable)"
if "${COMPOSE[@]}" exec -T agent /bin/setpriv --reuid runner --regid runner --init-groups -- sh -c "head -c1 /proc/$WPID/environ >/dev/null 2>&1"; then
  fail "M6 E1: the RUNNER uid could READ the worker's /proc/$WPID/environ — the PAT-race boundary is NOT enforced"
fi
pass "M6 E1: runner is DENIED the worker's /proc/environ (its cmdline IS readable → non-vacuous)"

# E2 + E3 — a runner child spawned by a process that HOLDS ambient CAP_SETUID/SETGID (as
# the real worker does) ends with EVERY capability set cleared (A1: a plain reuid would
# leak ambient CAP_SETUID → the runner could climb back to worker/root), is a member of
# ONLY group runner (never worker), and inherits only clean stdio (no leaked worker fds).
# The two-hop reproduces the exact production drop: the entrypoint's worker drop (ambient
# setuid/setgid) then runner-uid.ts's setpriv args. BODY stays simple (grep + echo); the
# outer shell parses.
PROBE="$("${COMPOSE[@]}" exec -T agent /bin/setpriv --reuid worker --regid worker --init-groups --bounding-set -all,+setuid,+setgid --inh-caps -all,+setuid,+setgid --ambient-caps -all,+setuid,+setgid -- /bin/setpriv --reuid runner --regid runner --init-groups --bounding-set -all --inh-caps -all --ambient-caps -all -- sh -c 'grep -E "^Cap(Inh|Prm|Eff|Amb):" /proc/self/status; echo "MAXFD=$(ls /proc/self/fd | sort -n | tail -1)"; echo "RUID=$(id -u)"; echo "RGROUPS=$(id -G)"' | tr -d '\r' || true)"
for cap in CapInh CapPrm CapEff CapAmb; do
  v="$(printf '%s\n' "$PROBE" | grep "^$cap:" | tr -d '[:space:]')"
  [ "$v" = "$cap:0000000000000000" ] || fail "M6 E2: runner child $cap not fully cleared (ambient cap leak?): got '$v' | probe: $PROBE"
done
[ "$(printf '%s\n' "$PROBE" | sed -n 's/^RUID=//p')" = 10002 ] || fail "M6 E2: runner child uid != 10002: $PROBE"
if printf '%s\n' "$PROBE" | sed -n 's/^RGROUPS=//p' | grep -qw 10001; then
  fail "M6 E3: runner child is a member of the worker group (10001) — group boundary leak: $PROBE"
fi
pass "M6 E2: runner child CapInh/Prm/Eff/Amb all zero, uid 10002, not in the worker group (ambient CAP_SETUID cleared → no climb-back)"
MAXFD="$(printf '%s\n' "$PROBE" | sed -n 's/^MAXFD=//p')"
{ [ -n "$MAXFD" ] && [ "$MAXFD" -le 3 ]; } 2>/dev/null \
  || fail "M6 E3: runner child leaked an fd > 3 (only stdio {0,1,2} + the transient readdir fd expected): MAXFD='$MAXFD' | probe: $PROBE"
pass "M6 E3: runner child /proc/self/fd is only {0,1,2}+the readdir fd (no leaked worker fds)"

# E4 — the worker node has no Node inspector debug port: no --inspect in its argv AND no
# listener on the inspector port (9229). A debug port would expose the worker's memory
# (which holds the decrypted PAT) to anything that can reach it — and NODE_OPTIONS=--inspect
# would NOT show in argv, so the port listener check is what catches that.
if "${COMPOSE[@]}" exec -T agent sh -c "tr '\0' '\n' < /proc/$WPID/cmdline | grep -q -- --inspect"; then
  fail "M6 E4: the worker node process was started with --inspect (debug port exposed)"
fi
# Presence belt (reviewer M6 follow-up): the listener check is non-vacuous only while
# `netstat` exists in the image; a future slim-down dropping it would make the grep
# below silently pass. Fail loudly if it's gone (then switch to /proc/net/tcp).
"${COMPOSE[@]}" exec -T agent sh -c 'command -v netstat >/dev/null 2>&1' \
  || fail "M6 E4: netstat is absent in the image — the inspector-port check would be vacuous; switch it to /proc/net/tcp (:240D)"
if "${COMPOSE[@]}" exec -T agent sh -c "netstat -tlnp 2>/dev/null | grep -qE ':9229([^0-9]|$)'"; then
  fail "M6 E4: something is listening on the Node inspector port 9229 (debug port exposed)"
fi
pass "M6 E4: worker node has no --inspect flag and no inspector-port (9229) listener"

# E5 — worker and runner have DISTINCT 0700 TMPDIRs (5-bis); each is owner-only, so neither
# uid can read the other's scratch (git packs, lockfiles, node temp).
TW="$("${COMPOSE[@]}" exec -T agent stat -c '%a %U' /tmp/uzi-worker | tr -d '\r')"
TR="$("${COMPOSE[@]}" exec -T agent stat -c '%a %U' /tmp/uzi-runner | tr -d '\r')"
[ "$TW" = "700 worker" ] || fail "M6 E5: /tmp/uzi-worker is '$TW', expected '700 worker'"
[ "$TR" = "700 runner" ] || fail "M6 E5: /tmp/uzi-runner is '$TR', expected '700 runner'"
if "${COMPOSE[@]}" exec -T agent /bin/setpriv --reuid runner --regid runner --init-groups -- sh -c 'ls /tmp/uzi-worker >/dev/null 2>&1'; then
  fail "M6 E5: the runner uid could list the worker's TMPDIR /tmp/uzi-worker (not owner-isolated)"
fi
if "${COMPOSE[@]}" exec -T agent /bin/setpriv --reuid worker --regid worker --init-groups -- sh -c 'ls /tmp/uzi-runner >/dev/null 2>&1'; then
  fail "M6 E5: the worker uid could list the runner's TMPDIR /tmp/uzi-runner (not owner-isolated)"
fi
pass "M6 E5: worker/runner TMPDIRs are distinct 0700 owner-only trees (neither reads the other's)"

# E7 — a runner-planted repo-local uploadpack.packObjectsHook must NOT execute in a local
# clone on the IMAGE's git (the protected-config gate: repo-local packObjectsHook is ignored
# — B2 invariant 6; the git-hardening.test.ts unit guard runs on the host git, this is the
# IMAGE git 2.54.0). Exercised as the runner uid.
POH="$("${COMPOSE[@]}" exec -T agent /bin/setpriv --reuid runner --regid runner --init-groups -- sh -c '
  d=$(mktemp -d) || exit 3
  git init -q "$d/src" && git -C "$d/src" -c user.name=t -c user.email=t@t -c commit.gpgsign=false commit -q --allow-empty -m c || exit 3
  git -C "$d/src" config uploadpack.packObjectsHook "touch $d/FIRED" || exit 3
  if git clone -q --no-local "file://$d/src" "$d/dst" >/dev/null 2>&1; then
    if [ -e "$d/FIRED" ]; then echo FIRED; else echo IGNORED; fi
  else
    echo CLONE_FAILED
  fi
  rm -rf "$d"' | tr -d '\r' || true)"
# A clone FAILURE must not be accepted as a pass: the old `|| true` let an errored clone
# fall through to the else-branch and print IGNORED, so a broken clone read as "hook ignored".
# CLONE_FAILED is now distinct and fails hard — the boundary is only proven when the clone ran.
[ "$POH" = IGNORED ] || fail "M6 E7: expected IGNORED (repo-local uploadpack.packObjectsHook ignored on the image git 2.54.0); got '$POH' (FIRED = protected-config gate regressed; CLONE_FAILED = the clone errored, so the boundary was never exercised)"
pass "M6 E7: repo-local uploadpack.packObjectsHook ignored on the image git 2.54.0 (protected-config gate holds)"

