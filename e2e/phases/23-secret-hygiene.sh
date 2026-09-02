# shellcheck shell=bash
# phase:    secret-hygiene
# title:    secret-hygiene assertions
# critical: no
# lane:     gitlab
# executor: any
# requires: WTOKEN
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# ⛔ DO NOT DROP — PRD #97 M4 guard list. ⛔
#
# M4 removed or collapsed several phases whose properties a cheaper layer already
# proves (#94 triage, #53 rate-limits, #46 Phase B, #33's stop_kind SQL, #40's usage
# rollup, most of #16's authz matrix). The FOUR phases that follow in this block —
# and the #83 Decision-3 leg at the foot of the file — look like the same kind of
# candidate and are NOT. Do not "finish the job":
#
#   • secret-hygiene (just below) — scans the LIVE container logs and the LIVE /data
#     volume of the running stack. No unit test can observe what this deployment
#     actually wrote to disk and to stdout.
#   • PRD #51 uid boundary (below) — reads real kernel state: file modes on a real
#     Docker secret, a real setpriv drop to uid 10002, a real EACCES. A Go/TS test
#     asserting the intended uids proves intent, not that the image enforces it.
#   • PRD #58 XFF (below) — covers the COMPOSE topology's shipped empty
#     TRUSTED_PROXIES default (forged X-Forwarded-For from the agent container must
#     collapse into ONE rate-limit bucket). The unit test covers the K8S pod-CIDR
#     case. Different deployment, different default, different bug.
#   • PRD #83 Decision-3 (end of file, --profile agent-docker) — a DIFFERENT topology
#     again (rootless DinD sidecar): it proves a container the sidecar started cannot
#     read the worker's join token. Nothing below the live daemon can show that.
#
# If you are here to shrink the suite: shrink somewhere else.
# =============================================================================
say "secret-hygiene assertions"
# POSITIVE CONTROL (PRD #97 M3): both scans below assert a secret is ABSENT, which passes
# VACUOUSLY on an empty corpus (a `compose logs` that errored, an /data exec that returned
# nothing). Prove the corpus is real BEFORE asserting absence — mirror the /proc control
# (:1312) that proves the cmdline is readable first, the Decision-3 control (:2943) that
# proves a container reads its own /etc/hostname, and the CI test:api-store-it
# gate-on-the-gate. Here: postgres unconditionally logs this benign banner on boot, so its
# presence proves the log corpus is the real, populated stream.
#
# 🔴 THE READ IS RETRIED, because a single `compose logs` CAN COME BACK SHORT — and it does
# so with rc=0, which is what makes it worth writing down. Measured 2026-07-28 across three
# consecutive runs after the PRD #35 M6 phase was moved above this one: rc=0, 128429 bytes,
# 844 lines, every service present in the corpus INCLUDING db-1 — and no boot banner. The
# same command against the same still-running stack seconds later returned it 8 times out
# of 8 (db-1: 11 lines, banner every time). So the corpus was not empty, not unreadable and
# not rotated; the fan-out over per-container streams simply returned before db's had been
# drained, and only under the load left by the phase that now runs before this one.
#
# Retrying is not weakening the control: the control is exactly the thing that detects an
# unusable corpus, so it is also the right thing to gate a re-read on. What WOULD weaken it
# is dropping it, or scanning a corpus this never vouched for — note the scans below run on
# the LAST corpus read, the one the control passed against.
#
# The failure message REPORTS WHAT IT MEASURED rather than naming a cause. The old wording
# said "empty/unreadable", which is one hypothesis out of several — a compose error, a
# partial read, a corpus missing one service, a rotated-out banner — and it sent this
# session to check the wrong ones for three runs. Print the rc, the size, and which
# services the corpus actually contains, so the next reader can tell them apart at a glance.
#
# 🔴 EVERY MATCH AGAINST $LOGS USES BASH PATTERN MATCHING, NEVER `printf … | grep -q`.
# This is a CORRECTNESS fix, not a style preference, and it is the reason this phase began
# failing when PRD #35 M6 moved above it.
#
# Under this file's `set -euo pipefail`, `printf '%s' "$BIG" | grep -q PAT` reports FAILURE
# when PAT *matches*: `grep -q` exits the instant it finds the match, `printf` still has
# data to write, and it dies of SIGPIPE (141) — which `pipefail` promotes to the pipeline's
# status. The match was real; the exit code lies about it.
#
# 🔴 THE TRIGGER IS THE MATCH'S POSITION, NOT THE CORPUS SIZE. The condition is that MORE
# THAN A PIPE BUFFER'S WORTH OF DATA FOLLOWS THE MATCH, so the writer still has to block
# after the reader is gone. A LATE match is safe at ANY size, because `grep -q` reaches EOF
# before `printf` ever blocks. Size is necessary, not sufficient. Measured 2026-07-28 on
# this host (64 KiB pipe buffer), and the boundary is exactly that:
#
#   secret EARLY, 15 MB -> rc=141 DISARMED    secret EARLY, 64 KB -> rc=141 DISARMED
#   secret LATE,  15 MB -> rc=0   fires       secret EARLY, 63 KB -> rc=0   fires
#   secret EARLY, 15 KB -> rc=0   fires
#
# That is worse than "big corpora are unsafe", which is why the distinction is kept: a
# secret logged EARLY — at worker boot, exactly when a join token or PAT surfaces — sat in
# the blind spot, while one logged in the last few KB was caught. The scan was least
# reliable precisely where a leak is most likely.
#
# It is invisible on small inputs and arrives the day some phase above makes the logs
# bigger. The three failing runs printed `db boot banner: 1, agent uid-split banner: 1`
# from `grep -c` in the very failure message saying the banners were absent — `-c` reads to
# EOF, so it never triggers the bug that produced the failure.
#
# NOT the `set -e`/AND-list exemption documented ~1700 lines below (`echo hi | grep -qF
# nope && fail`). That one is about a NON-match in an AND-list; this is a MATCH being
# reported as a non-match. Opposite direction, different mechanism, and neither implies the
# other — do not merge the two notes.
#
# 🔴 THE SAME BUG SILENTLY DISARMED THE LEAK SCAN ITSELF, which is the part that matters
# beyond this control. `printf … | grep -qF "$sec" && fail "a secret leaked"` can only fire
# when grep MATCHES — precisely the case that SIGPIPEs — so on a corpus this size a real
# leaked token would have been scanned, found, and then silently passed over. The control
# above existed to stop this scan passing vacuously and was defeated by the same mechanism
# it was guarding. Rewritten below; keep it out of pipelines.
LOGS_RC=0
LOGS="$("${COMPOSE[@]}" logs --no-color 2>&1)" || LOGS_RC=$?
# Two markers, one per stream a later assertion depends on. The db banner alone was never
# enough: the leak scan below is overwhelmingly about the AGENT (the process holding the
# decrypted PAT and the join token), so a corpus vouched for only by postgres could omit the
# agent stream entirely and still report "no secret leaked". The uid-split control ~60 lines
# down reads this same $LOGS and needs the agent's boot line too.
if [[ "$LOGS" != *"database system is ready to accept connections"* || "$LOGS" != *"A1 uid-split active"* ]]; then
  fail "positive control: the container-log corpus is incomplete (db boot banner present: $([[ "$LOGS" == *"database system is ready to accept connections"* ]] && echo yes || echo NO), agent uid-split banner present: $([[ "$LOGS" == *"A1 uid-split active"* ]] && echo yes || echo NO)), so the secret-absence scan below would pass vacuously. compose-logs rc=$LOGS_RC bytes=${#LOGS}; services present: $(printf '%s' "$LOGS" | sed 's/ *|.*//' | sort -u | tr '\n' ' ')"
fi
for sec in "$WTOKEN" "$DUMMY_FORGE_PAT" "$DUMMY_ANTHROPIC"; do
  # `$sec` is quoted inside the pattern, so it is matched LITERALLY (no globbing) — the
  # `[[ ]]` equivalent of grep -F. See the SIGPIPE note above for why this is not a pipe.
  if [[ "$LOGS" == *"$sec"* ]]; then fail "a secret leaked into container logs"; fi
done
pass "no PAT / Anthropic token / join token in any container log (corpus vouched for by BOTH the db and agent boot banners, and scanned without a pipe so a hit can actually fire)"

# POSITIVE CONTROL (PRD #97 M3): prove /data has scannable files first, else a failed exec
# or an empty /data makes the absence grep pass vacuously. By now the worker has cloned the
# bare cache + worktrees, so /data holds files; assert at least one before scanning.
"${COMPOSE[@]}" exec -T agent sh -c 'find /data -type f 2>/dev/null | head -1' | grep -q . \
  || fail "positive control: the worker's /data has no files to scan — the secret-absence scan below would pass vacuously"
for sec in "$WTOKEN" "$DUMMY_FORGE_PAT" "$DUMMY_ANTHROPIC"; do
  if "${COMPOSE[@]}" exec -T agent sh -c "grep -rlF '$sec' /data 2>/dev/null | head -1" | grep -q .; then
    fail "a secret is present on the worker's /data disk"
  fi
done
pass "no secret on the worker's /data (bare clone cache, worktrees, sessions; corpus non-empty)"

# /proc hardening (M6): the join token is delivered by file (the `worker_token` Docker
# secret), not env, so it must not appear in any environ. Two NON-vacuous checks — the
# earlier root-exec scan was partially vacuous (auditor M6): a root docker-exec lacks
# in-container CAP_SYS_PTRACE, so it reads a cross-uid /proc/<pid>/environ as EMPTY and a
# leak there would go unseen.
#   (a) STRUCTURAL: the token must not be a CONFIGURED env var (image ENV / compose
#       `environment:`) — the exact regression this guards. A fresh `exec … env` shows
#       that configured env; assert the raw token value is absent (only …_FILE is set).
"${COMPOSE[@]}" exec -T agent env 2>/dev/null | grep -qF -- "$WTOKEN" \
  && fail "the join token appears in the worker's configured env — it must be file-delivered, not an env var"
pass "/proc hardening (a): join token is NOT a configured env var (file-delivered only)"
#   (b) RUNTIME: scan /proc environs AS THE WORKER uid (not root) so same-uid dumpable=1
#       environs are GENUINELY read (a root scan cannot); the credential-holding worker
#       node is dumpable=0 → its environ is unreadable even same-uid (a hardening), and the
#       token is not there anyway. Token passed as argv so the probe can't self-match.
ENV_HITS="$("${COMPOSE[@]}" exec -T agent /bin/setpriv --reuid worker --regid worker --init-groups -- sh -c '
  n=0
  for e in /proc/[0-9]*/environ; do
    # 2>/dev/null BEFORE < "$e": the open of a dumpable=0 (unreadable) environ fails at the
    # redirection, so fd 2 must already point at /dev/null or the error leaks to the log.
    tr "\0" "\n" 2>/dev/null < "$e" | grep -qF "$1" && n=$((n+1))
  done
  echo "$n"' _ "$WTOKEN" | tr -d '\r')"
[ "$ENV_HITS" = 0 ] || fail "the join token is present in $ENV_HITS worker-readable process environ(s) — /proc leak NOT closed"
pass "/proc hardening (b): join token absent from every worker-readable process environ (worker-uid scan)"

# PRD #51 M4/M5 uid boundary: the join-token secret is 0400 worker:worker, so ONLY the
# worker uid may read it — the runner uid (which runs the untrusted agent/checks/
# provision) is DENIED. This is the real containment; the reads below prove it is NOT
# vacuous. `docker compose exec` enters as ROOT (no USER line), and root bypasses 0400,
# so we DROP to each uid with setpriv (--init-groups for real supplementary groups) and
# assert the runner FAILS and the worker SUCCEEDS — no `|| true` swallow.
#
# First, the split must genuinely be active (a silent fallback to the #58 single-uid
# branch would collapse both uids into one and make the negative read pass vacuously).
# Already gated at the top of the secret-hygiene phase (this marker is one of the two the
# corpus must carry before anything is scanned). Kept as an assertion rather than deleted:
# it states what THIS phase requires, so narrowing that gate fails here with the reason
# attached. Bash matching, not a pipe — see the SIGPIPE note there.
if [[ "$LOGS" != *"A1 uid-split active"* ]]; then
  fail "the agent never logged 'A1 uid-split active' — the uid split did not engage (single-uid fallback would make the boundary vacuous)"
fi
[ "$("${COMPOSE[@]}" exec -T agent id -u worker | tr -d '\r')" = 10001 ] \
  && [ "$("${COMPOSE[@]}" exec -T agent id -u runner | tr -d '\r')" = 10002 ] \
  || fail "expected distinct worker(10001)/runner(10002) uids in the running agent"
pass "PRD #51 uid split active: A1 root drop logged; distinct worker(10001)/runner(10002) uids"

TOK=/run/secrets/worker_token
if "${COMPOSE[@]}" exec -T agent /bin/setpriv --reuid runner --regid runner --init-groups -- cat "$TOK" >/dev/null 2>&1; then
  fail "the RUNNER uid could READ the worker join token ($TOK) — uid boundary NOT enforced"
fi
pass "PRD #51 uid boundary: runner uid is DENIED read of the worker join token"
"${COMPOSE[@]}" exec -T agent /bin/setpriv --reuid worker --regid worker --init-groups -- cat "$TOK" >/dev/null 2>&1 \
  || fail "the WORKER uid could NOT read its own join token ($TOK) — over-tightened"
pass "PRD #51 uid boundary: worker uid CAN read its own join token"
TOKMODE="$("${COMPOSE[@]}" exec -T agent stat -c '%a %U %G' "$TOK" | tr -d '\r')"
[ "$TOKMODE" = "400 worker worker" ] \
  || fail "worker-token perms are '$TOKMODE', expected '400 worker worker' (a 0444/root regression would let the runner read it, making the boundary vacuous)"
pass "PRD #51 uid boundary: worker-token secret is 0400 worker:worker (read denial is real)"

