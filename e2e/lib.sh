#!/usr/bin/env bash
# sourced by run-e2e.sh; shared helpers used by >=2 phases (PRD #966 M1)
# --- output helpers ----------------------------------------------------------
say()  { printf '\n\033[1m==> %s\033[0m\n' "$1"; }
pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }

cleanup() {
  local code=$?
  # 🔴 BREADCRUMB FIRST, BEFORE ANYTHING THAT CAN FAIL. It exists to answer one question
  # that was unanswerable after a real run: did the EXIT trap fire at all?
  #
  #   breadcrumb + a teardown/KEEP_STACK line  -> cleanup ran to completion
  #   breadcrumb + NEITHER                     -> cleanup ran and DIED INSIDE
  #   NEITHER                                  -> the trap never fired (signal, or the
  #                                               capture ended before this point)
  #
  # A run ended with no margin report and no teardown line, and none of the three could be
  # distinguished from the log. Three structural hypotheses were eliminated by minimal
  # repro — `set -e` from a failing pipeline in a function, the `env -i` re-exec, and
  # `set -u` in a function all fire the trap normally — and the cause is still unknown.
  # One line converts the next occurrence from an investigation into a reading.
  printf '\n[cleanup] EXIT trap entered (code %s)\n' "$code"
  # Margin report BEFORE teardown (PRD #97 M9) — on the failure path too, where it is
  # most useful: a red run's margins usually show the whole suite running hot, which is
  # the difference between "this assertion is wrong" and "this host was slow".
  #
  # 🔴 THE `2>/dev/null` THAT USED TO BE HERE IS GONE, AND ITS REMOVAL IS THE FIX. The
  # wrapper written to stop a broken diagnostic from doing harm was what made its own
  # failure unreportable. `|| true` is what protects the exit code; the stderr suppression
  # protected nothing and cost the only evidence. REPRODUCED: with MARGINS_FILE unbound,
  # `report_margins 2>/dev/null || true` under `set -u` kills the shell MID-CLEANUP and
  # prints nothing at all — `|| true` does not catch it, because an unbound variable is a
  # fatal shell error rather than a command failure, and the redirect eats the one message
  # that would have named it. Empty log, no teardown, no cause.
  #
  # That was not merely hypothetical here: the trap is registered ~200 lines ABOVE
  # MARGINS_FILE's first assignment, so any failure in that window — which covers stack
  # bring-up, where failures are COMMON — hit exactly this. `${MARGINS_FILE:-}` in
  # report_margins closes it at the source; dropping the redirect is what makes the next
  # one visible.
  report_margins || true
  # KEEP_STACK leaves the whole stack running (containers + volumes + rundir) so
  # the auditor can inspect logs, the claim payload path, and the worker's /data
  # against a live run. Tear it down manually with the printed command.
  if [ -n "${KEEP_STACK:-}" ]; then
    say "leaving the stack UP for inspection (KEEP_STACK set)"
    printf '  project:  %s\n  web:      %s\n  rundir:   %s\n' "$PROJECT" "$BASE" "$RUNROOT"
    printf '  logs:     docker compose -p %s logs\n' "$PROJECT"
    printf '  worker:   docker compose -p %s exec agent sh\n' "$PROJECT"
    printf '  teardown: docker compose -p %s --env-file %s -f %s -f %s --profile agent%s down -v\n' \
      "$PROJECT" "$ENVFILE" "$ROOT/docker-compose.yml" "$ROOT/e2e/docker-compose.e2e.yml" \
      "${DOCKER_PROFILE:+ --profile agent-docker}"
    exit $code
  fi
  say "tearing down (down -v)"
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  # --- rundir retention on a RED run (PRD #966 M3, D5) ------------------------
  # A red run KEEPS its scratch dir so the per-phase artifacts/ the driver captured
  # survive for post-mortem (and #967's upload). "Red" = a non-zero exit $code OR the
  # driver's `$RUNROOT/.keep-rundir` sentinel, which the driver `touch`es on ANY
  # FAIL/LEAK — so a non-strict LEAK, which exits 0, still keeps the dir per D5. The
  # existing KEEP (KEEP_RUNDIR) path keeps it unconditionally and is handled first.
  if [ -n "$KEEP" ]; then
    exit $code
  fi
  if [ "$code" -ne 0 ] || [ -f "$RUNROOT/.keep-rundir" ]; then
    printf '[cleanup] red run — rundir kept: %s\n' "$RUNROOT"
    printf '[cleanup] artifacts: %s\n' "$RUNROOT/artifacts"
    exit $code
  fi
  # The scratch removal MUST NOT flip a passed run to red. Containers write into the
  # bind-mounted fakeremote/ as uids other than the host user — forge-fake's receive-pack
  # as root, the worker as its own in-image uid — so on a CI runner the non-root runner
  # user cannot rm those ref files and `rm -rf` returns non-zero. Under `set -e` that
  # would become the script's exit status, overriding `exit $code` below and failing a
  # green run at teardown. Locally the files are user-owned, so this still cleans fully;
  # on CI the runner is ephemeral and any leftover scratch is discarded with the VM.
  rm -rf "$RUNROOT" || true
  exit $code
}
trap cleanup EXIT

# The worker join token, minted once (after the API is up) then handed to every
# `up`/recreate via the base compose `worker_token` Docker secret (env source
# UZI_WORKER_TOKEN, exported below). The entrypoint hardens that secret to 0400
# worker:worker on every start, so it persists read-only across restarts and the
# runner uid cannot read it (PRD #51 M5) — no per-start file re-delivery needed.
# shellcheck disable=SC2034  # set by phase 13 (provides: WTOKEN), read by later phases (PRD #966 M1 split)
WTOKEN=""

# retry_read CMD [ARGS...] — run a READ-ONLY command with a short bounded retry, so one
# transient curl/exec blip does not abort the ~20-min run under `set -euo pipefail` (a
# bare point-in-time GET that hiccups would otherwise kill the whole suite). RESTRICTED to
# idempotent GETs — only apiget + fake_state are wrapped below. A retried WRITE could
# double-execute after an ambiguous failure, so the write helpers (apipost/apiput/apipatch,
# fake_post) and db_psql (also used for WRITES, in the #32, #68, #98 and rate-limit phases)
# are deliberately NOT wrapped (PRD #97 M3, fable review). Still returns the last attempt's
# non-zero after 3 tries: this smooths a blip, it never masks a persistent failure. curl -f
# writes nothing to stdout on a failed attempt, so a retry cannot double the captured body.
#
# TWO CORRECTIONS TO THIS COMMENT, both found while building PRD #98 M8b, both the kind of
# defect the comment's own subject matter is about. (1) It listed `apidelete` among the
# helpers "deliberately NOT wrapped". There is no apidelete — it appeared nowhere else in
# this file and was never defined; the harness's single DELETE is an inline `curl -X DELETE`
# against /api/me/secrets/anthropic_token/. It is struck rather than defined, because a
# helper nothing calls is dead code PLUS a comment that has become true about a function
# nobody uses. (2) It cited the db_psql writes by LINE (`:2087/:2191/:2843`), and at the
# time of this edit all three pointed at unrelated text — one blank line, one wait_status,
# and one line of a comment written minutes earlier. This repo's own rule is that a line
# number is meaningless without a SHA; naming the PHASES survives edits.
#
# THEN A THIRD, because the second repair introduced the same family of defect it was
# fixing — one abstraction up. It read "INSERTs, in the #46, #68, #98 and rate-limit
# phases", and both halves were wrong. The `user_secrets` write belongs to **#32**, the
# per-user vault phase; the #46 phase contains no db_psql write at all, only two SELECTs.
# And db_psql performs DELETEs as well, so enumerating only INSERTs understates this
# comment's own argument — which is that a retried WRITE could double-execute. `writes` is
# both accurate and edit-proof. Re-derived by listing the `say "` phase headers and the
# `db_psql "INSERT|DELETE|UPDATE` sites and reading the line numbers against each other,
# rather than by trusting the previous sentence.
retry_read() {
  local n=1 rc
  while :; do
    "$@" && return 0
    rc=$?
    [ "$n" -ge 3 ] && return "$rc"
    n=$((n + 1))
    sleep 0.5
  done
}

# --- api helpers (session cookie + CSRF, like scripts/smoke.sh) --------------
csrf() { awk '$6=="uzi_csrf"{print $7}' "$JAR"; }
login() {
  curl -fsS -c "$JAR" -X POST "$BASE/api/auth/login" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" >/dev/null
}
apiget()  { retry_read curl -fsS -b "$JAR" "$BASE$1"; }
apipost() { curl -fsS -b "$JAR" -X POST "$BASE$1" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $(csrf)" -d "$2"; }
apiput()  { curl -fsS -b "$JAR" -X PUT "$BASE$1" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $(csrf)" -d "$2"; }
apipatch() { curl -fsS -b "$JAR" -X PATCH "$BASE$1" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $(csrf)" -d "$2"; }
# db_psql SQL — a bare scalar out of the e2e db (PGPASSWORD from the env-file).
#
# Defined HERE with the other helpers rather than beside its first caller. It used to
# live in the PRD #32 vault-helper block ~1400 lines down, which silently made it
# unavailable to every phase above that point: bash resolves a function at CALL time,
# so an earlier phase using it dies with `db_psql: command not found` (127) — and
# inside a `X="$(db_psql ...)"` assignment `set -e` takes that as the assignment's own
# status and aborts the run with no `fail` line and no diagnosis. Found 2026-07-28
# moving the PRD #35 M6 block earlier. A helper's definition site is a constraint on
# which phases may use it; keep them all up here so there is no such constraint.
#
# The password is read LAZILY, on first call, and that is the other half of hoisting
# this: $ENVFILE is WRITTEN during setup, hundreds of lines below here, so the eager
# `PGPW="$(grep ... "$ENVFILE")"` this replaced aborted the whole run at startup with a
# bare `grep: …/e2e.env: No such file or directory` under `set -e` — before the trap
# had its helpers, so the cleanup itself then died on `report_margins: command not
# found` and the real cause was two errors up. Memoized, so repeat calls cost nothing.
PGPW=""
db_psql() {
  [ -n "$PGPW" ] || PGPW="$(grep '^POSTGRES_PASSWORD=' "$ENVFILE" | cut -d= -f2-)"
  "${COMPOSE[@]}" exec -T -e PGPASSWORD="$PGPW" db psql -U uzi -d uzi -tAc "$1" | tr -d '\r\n'
}
# create_run REPO_ID ISSUE_IID — POST a run, tolerating ONLY the transient
# `404 "issue not found on this repo's board"`. That 404 is a create-then-immediately-use
# race against the fast (2s) poller the PRD #24 MR-close phase leaves running: a board
# reconcile that snapshotted the forge BEFORE the just-created issue can momentarily drop
# it from the board cache, and the NEXT poll re-adds it. Bounded-retry ONLY that exact
# 404; fail loudly on any OTHER status or a persistent 404 (never blanket-swallow 404s —
# that would mask a real regression). Prints the run id on stdout; diagnostics to stderr.
# e2e-only (prod polls at 24h, so this race cannot occur there). PRD #51 M6 hardening.
create_run() {
  local repo="$1" iid="$2" code out="$RUNROOT/.run-create.json"
  for _ in 1 2 3 4 5 6; do
    code="$(curl -sS -b "$JAR" -o "$out" -w '%{http_code}' -X POST "$BASE/api/repos/$repo/runs" \
      -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf)" -d "{\"issue_iid\":$iid}")"
    case "$code" in
      200|201) jq -r '.run.id' "$out"; return 0 ;;
      404)
        grep -q "issue not found on this repo's board" "$out" \
          || { echo "create_run: non-transient 404 for issue #$iid: $(cat "$out")" >&2; return 1; }
        sleep 1 ;;  # transient board-reconcile race; the next poll re-adds the issue
      *) echo "create_run: HTTP $code for issue #$iid: $(cat "$out")" >&2; return 1 ;;
    esac
  done
  echo "create_run: still transient-404 'issue not found on this repo's board' after 6 tries (issue #$iid)" >&2
  return 1
}
# fresh_code METHOD PATH [BODY] — a non-admin (fresh user) request; prints only the
# HTTP status (no -f), for authz assertions. CSRF from the fresh user's jar.
fresh_code() {
  local method="$1" p="$2" body="${3:-}" tok
  tok="$(awk '$6=="uzi_csrf"{print $7}' "$FRESHJAR")"
  if [ -n "$body" ]; then
    curl -sS -b "$FRESHJAR" -o /dev/null -w '%{http_code}' -X "$method" "$BASE$p" \
      -H 'Content-Type: application/json' -H "X-CSRF-Token: $tok" -d "$body"
  else
    curl -sS -b "$FRESHJAR" -o /dev/null -w '%{http_code}' -X "$method" "$BASE$p" \
      -H 'Content-Type: application/json' -H "X-CSRF-Token: $tok"
  fi
}
# apiput_code — like apiput but never -f: echoes the HTTP status and swallows the
# body, so the caller can assert on 200-vs-4xx (the concurrent-PUT race).
apiput_code() { curl -sS -o /dev/null -w '%{http_code}' -b "$JAR" -X PUT "$BASE$1" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf)" -d "$2"; }
# apipost_code — like apipost but never -f: echoes only the HTTP status, for
# asserting a 422 (the uzi run-eligibility gate refusing a non-uzi issue).
apipost_code() { curl -sS -o /dev/null -w '%{http_code}' -b "$JAR" -X POST "$BASE$1" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf)" -d "$2"; }

# wait_http — poll /api/health until the stack answers. Instrumented (PRD #97 M9b)
# even though it is defined ABOVE record_margin: bash resolves function names at call
# time, and the first wait_http call (~:700) is far below the MARGINS_FILE arming
# (~:535), so this is correct as written — do not "fix" the ordering.
wait_http() {
  local timeout=90
  local start=$SECONDS deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    if curl -fsS "$BASE/api/health" >/dev/null 2>&1; then
      record_margin "api health" "$((SECONDS - start))" "$timeout"; return 0
    fi
    sleep 0.3
  done
  fail "api health never came up at $BASE"
}

# wait_eq WANT TIMEOUT DESC GETTER [ARGS...] — the generic "reaches state X" wait:
# poll GETTER until its output equals WANT. Every simple equality wait shares this
# skeleton. Bespoke loops remain where the wait is not a plain equality: extra
# early-fail conditions (wait_status, wait_autopilot_done, wait_notes), a non-equality
# predicate (wait_http, wait_review, wait_msg_kind, wait_msg_text, wait_tool_result),
# or a value to echo back on success (wait_run_for_issue).
# ── margin instrumentation (PRD #97 M9, completed in M9b) ─────────────────────
# Every wait_* helper records how long it ACTUALLY waited against the ceiling it was
# given; the tightest are printed at the end of the run. A bare PASS cannot distinguish
# "settled instantly" from "made it with 200ms to spare on a 20s ceiling", and that
# difference is the whole question when deciding whether a phase is safe to speed up or
# is quietly one slow host away from red.
#
# Coverage is now EVERY wait_* helper in this file. Instrumenting the shared wait_eq
# covers its seven wrappers in one place (wait_worker_online / wait_board_pipeline /
# wait_card_pipeline / wait_verdict / wait_card_column / wait_run_mr_state /
# wait_health); the nine helpers with bespoke loops each record at their own success
# return: wait_status, wait_http, wait_run_for_issue, wait_autopilot_done, wait_notes,
# wait_review, wait_msg_kind, wait_msg_text, wait_tool_result. The last three are the
# 20s chat-phase ceilings — the tightest in the suite, and blind until M9b.
#
# Deliberately NOT instrumented, named individually so this claim is checkable rather
# than asserted (M9b exists because M9's coverage claim was not). Each is cited by a
# UNIQUE CONTENT ANCHOR — grep the quoted string — never by line number: this file's
# refs have already gone stale by ~400 lines once, and the first draft of this very
# block cited six line numbers that its own 11 added lines invalidated as it was
# written. One of them then resolved to `wait_run_for_issue()`, an INSTRUMENTED helper,
# inside the bullet claiming it was uninstrumented. A ref that resolves to something
# plausible and wrong costs more than no ref at all. Anchors cannot drift.
#   * four INLINE `while [ $SECONDS -lt … ]` loops that are not helpers, each named by
#     the variable holding its deadline: `deadline=$((SECONDS + 20))` (second proposal
#     card, 20s), `rv_deadline` (plan-revision v2 pickup, 60s), `cap_deadline` (worker
#     cap re-advertise, 40s), `det_end` (docker self-detect, 45s). All four `break` on
#     success and leave the verdict to an assertion AFTER the loop, so the loop has no
#     success return to hang a record on, and the deadline is a give-up point rather
#     than a ceiling anything is asserted against.
#   * a fifth inline loop, `if_end` (PRD #47 (c), 90s), is INVERTED: it polls for a
#     condition that must NEVER become true (health flipping to `stalled`), so running
#     the full 90s IS the success. A record there would print "waited 90s of 90s,
#     headroom 0s" on every green run — the single most alarming row in the report, for
#     a loop behaving exactly as designed.
#   * `assert_no_run_for_issue()` is a fixed `sleep`, not a wait: it settles for a few
#     detector ticks and then reads once. There is no polling, no success event to time,
#     and no ceiling — elapsed is the sleep argument by construction, so a record would
#     carry zero information. Out of scope.
# Five inline loops in total, plus the one fixed sleep. Give any of them a real ceiling
# contract of its own before instrumenting it.
#
# Every helper binds its ceiling into a LOCAL and uses that local for both the deadline
# and the record, so the reported ceiling can never drift from the enforced one. The
# two-statement shape below is LOAD-BEARING, not style:
#
#     local run="$1" timeout="${3:-90}"            # statement 1: bind the ceiling
#     local start=$SECONDS deadline=$((SECONDS + timeout))   # statement 2: use it
#
# bash expands a `local`'s arguments BEFORE executing it, so assignments within ONE
# `local` do not sequence — `timeout` is still unset while `$((SECONDS + timeout))` is
# expanded. Under this script's `set -euo pipefail`, that is not a wrong number, it is a
# HARD ABORT: `bash: timeout: unbound variable`, exit 1, on the very first wait the
# suite reaches. Collapsing these two statements into one therefore takes the whole run
# down, and the failure names the loop variable rather than the edit that caused it.
# (Verified: the collapsed form aborts under `set -u`; without `set -u` it silently
# yields a garbage ceiling instead, which is how it reads as harmless when tried in a
# bare shell.) The code as written is correct and always has been — this note exists to
# stop a future tidy-up, not to flag a live defect.
#
# Diagnostic ONLY: record_margin never asserts and never fails a run — a broken
# diagnostic must not be able to turn a good run red. It is called on the SUCCESS path
# only, never on a fail/early-exit path, and never in a position that could change a
# helper's exit status or pollute its stdout (wait_run_for_issue echoes a run id).
# Resolution is whole seconds ($SECONDS, portable everywhere); that cannot resolve
# sub-second waits, but the question it answers is "which ceilings are we approaching",
# where 1s granularity is ample. Sub-second precision where it matters is done per site
# (see PRD #95). Descriptions carry run ids / issue iids / status+kind literals only —
# never bodies, tokens, or vault material.
MARGINS_FILE=""   # assigned once RUNROOT exists (see the mkdir below)
record_margin() { # record_margin DESC WAITED_S TIMEOUT_S
  [ -n "${MARGINS_FILE:-}" ] || return 0
  printf '%s\t%s\t%s\n' "$2" "$3" "$1" >> "$MARGINS_FILE" 2>/dev/null || true
}

# report_margins — print the waits that came closest to their ceiling. Sorted by
# headroom (timeout - waited) ascending, so the most fragile are first. The cut-off is
# 20 (was 12): M9b roughly doubled the number of instrumented sites, and a top-12 taken
# over twice the population hides exactly the newly-visible tight waits it was added
# to expose.
report_margins() {
  # ${MARGINS_FILE:-}, not $MARGINS_FILE: cleanup calls this, and cleanup can fire BEFORE the
  # assignment ~200 lines below the trap. Under `set -u` a bare reference there is a FATAL
  # shell error, not a return — it kills cleanup mid-flight with no teardown and no message.
  [ -n "${MARGINS_FILE:-}" ] && [ -s "${MARGINS_FILE:-}" ] || return 0
  say "PRD #97 M9 — wait_* margin report (tightest first; headroom = ceiling - actual)"
  # Emit "<headroom>\t<line>", numeric-sort on the leading key, then strip it — sorting
  # the rendered text directly does not work (the number is not at a field boundary).
  # Truncate with `sed -n '1,20p'`, NOT `head -20`: head closes the pipe after 20 lines,
  # sending SIGPIPE upstream to cut ("cut: write error: Broken pipe" in the log). sed drains
  # the whole stream (no early exit without `q`), so nothing upstream gets a broken pipe.
  awk -F'\t' '{ printf "%d\t  %-46s waited %3ss of %3ss ceiling (headroom %3ss)\n", $2-$1, substr($3,1,46), $1, $2, $2-$1 }' \
    "$MARGINS_FILE" | sort -n -k1,1 | cut -f2- | sed -n '1,20p'
  printf '  (%s instrumented waits this run; whole-second resolution)\n' "$(wc -l < "$MARGINS_FILE" | tr -d ' ')"
}

wait_eq() {
  local want="$1" timeout="$2" desc="$3"; shift 3
  local start=$SECONDS deadline=$((SECONDS + timeout)) got
  while [ $SECONDS -lt $deadline ]; do
    got="$("$@")"
    if [ "$got" = "$want" ]; then record_margin "$desc -> $want" "$((SECONDS - start))" "$timeout"; return 0; fi
    sleep 0.3
  done
  fail "timeout: $desc never reached '$want' (last: '${got:-none}')"
}

# One-shot getters for wait_eq (and for direct point-in-time assertions).
worker_status()         { apiget /api/workers | jq -r '.workers[0].status // empty'; }
# Worker resource stats (PRD #49): the sample the worker self-reports from its own
# cgroup v2 files on each heartbeat, surfaced on the workers DTO. source is the
# non-empty enum once a sample lands; mem_bytes is the working-set byte count.
worker_stats_source()   { apiget /api/workers | jq -r '.workers[0].stats_source // empty'; }
worker_stats_mem()      { apiget /api/workers | jq -r '.workers[0].stats_mem_bytes // empty'; }
board_pipeline_status() { apiget "/api/repos/$REPO_ID/board" | jq -r '.board.pipeline.status // empty'; }
card_pipeline_status()  { apiget "/api/repos/$REPO_ID/board" | jq -r --argjson iid "$1" '.board.cards[] | select(.iid==$iid) | .pipeline.status // empty'; }
run_verdict()           { apiget "/api/runs/$1" | jq -r '.run.fix_verdict // empty'; }
run_mr_state()          { apiget "/api/runs/$1" | jq -r '.run.mr_state // empty'; }
run_health()            { apiget "/api/runs/$1" | jq -r '.run.health'; }
# card_column IID — the board's resolved column for one issue (empty = Open).
card_column()           { apiget "/api/repos/$REPO_ID/board" | jq -r --argjson iid "$1" '.board.cards[] | select(.iid==$iid) | .column'; }
# run_input_delivery RUN — the delivery state of the run's newest follow_up in its
# owner-scoped steer queue (PRD #95), derived from consumed_at EXACTLY as the web/CLI
# derive it: "delivered" once consumed_at is set, "queued" while null, "none" when the
# queue is empty. The read endpoint returns newest-first, so .[0] is the latest.
run_input_delivery()    { apiget "/api/runs/$1/inputs" | jq -r '(.inputs // []) | if length == 0 then "none" elif (.[0].consumed_at != null) then "delivered" else "queued" end'; }

wait_worker_online()  { wait_eq online 40 "worker status" worker_status; }
# Poller-driven waits: PRD #6 CI badges + verification stamp, PRD #24 card moves
# and the watcher-maintained runs.mr_state (PRD #33 surfaces it on the run).
wait_board_pipeline() { wait_eq "$1" "${2:-30}" "board pipeline" board_pipeline_status; }
wait_card_pipeline()  { wait_eq "$2" "${3:-30}" "card #$1 pipeline" card_pipeline_status "$1"; }
wait_verdict()        { wait_eq "$2" "${3:-30}" "run $1 fix_verdict" run_verdict "$1"; }
wait_card_column()    { wait_eq "$2" "${3:-40}" "card #$1 column" card_column "$1"; }
wait_run_mr_state()   { wait_eq "$2" "${3:-30}" "run $1 mr_state" run_mr_state "$1"; }
# wait_health: a run's health flag (PRD #47) rides the run DTO. Generous default
# (a stall needs ~75s of quiet plus a sweep tick).
wait_health()         { wait_eq "$2" "${3:-120}" "run $1 health" run_health "$1"; }

# wait_status RUN WANT [TIMEOUT] — poll a run until it reaches WANT; abort early
# if it lands in an unexpected terminal state.
wait_status() {
  local run="$1" want="$2" timeout="${3:-90}" s
  local start=$SECONDS deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    s="$(apiget "/api/runs/$run" | jq -r '.run.status')"
    if [ "$s" = "$want" ]; then record_margin "run status -> $want" "$((SECONDS - start))" "$timeout"; return 0; fi
    case "$s" in
      failed|cancelled)
        [ "$s" = "$want" ] && return 0
        local reason
        reason="$(apiget "/api/runs/$run" | jq -r '.run.failure_reason // empty')"
        fail "run $run entered '$s' (${reason:-no reason}) while waiting for '$want'";;
    esac
    sleep 0.3
  done
  fail "timeout: run $run never reached '$want' (last: ${s:-none})"
}

# --- forge-fake /_e2e helpers --------------------------------------------------
# The overlay publishes forge-fake's 443 on a per-run loopback port so the
# harness reaches the /_e2e mutators/introspection with plain curl (no
# `compose exec node -e` round-trip per call — wait_notes polls one of these
# every 300ms). -k because the self-signed cert names forge-fake.e2e, not
# 127.0.0.1; TLS fidelity toward uzi is exercised by the api/worker paths, the
# harness's own probes assert application state only.

# fake_post PATH JSON — POST to a forge-fake /_e2e mutator, echoing the response
# body. Used to stage PRD/autopilot-labelled issues and label events the way a
# human filing/labelling an issue would, which uzi's own CreateIssue
# (PRD-label-only) cannot.
fake_post() { curl -fsSk -X POST "$FAKE_BASE$1" -H 'Content-Type: application/json' -d "$2"; }

# flip_mr IID STATE — the harness stand-in for a reviewer closing/reopening/
# merging an MR (PRD #24). Takes a STATE because the MR-close watcher acts in BOTH
# directions and this file calls it both ways (closed and reopened, in the #24 phase).
# Its issue counterpart is `close_issue` and takes no state — see there for why the
# asymmetry tracks a real product difference rather than an oversight.
flip_mr() { fake_post "/_e2e/mrs/$1/state" "$(jq -nc --arg s "$2" '{state:$s}')" >/dev/null; }

# close_issue IID — the harness stand-in for a human closing an issue on the forge
# (PRD #98 M6). Deliberately NOT a two-argument `flip_issue IID STATE` mirroring
# flip_mr, even though the route accepts "opened": the product consumes the
# open→closed EDGE ONLY, and a reopen is explicitly not acted on
# (judge_issue_close.sql: close_synced_at stays stamped, so a flapping issue cannot
# ping-pong a user's backlog). A helper spelled `flip_issue` would advertise a
# round trip the product does not make, and the next reader would write a reopen
# assertion against a guarantee that does not exist.
#
# THE HARNESS DEMONSTRATES THAT BETTER THAN THE ARGUMENT DOES: `flip_mr` is genuinely
# called BOTH ways in the #24 phase (closed, then reopened), because the MR-close
# watcher acts in both directions. Nothing in the product acts on an issue reopen, and
# TestFiledIssueCloseReopenDoesNotReopenLiveDB already pins that NON-behaviour at the
# live-DB layer. So the asymmetry between these two helpers is the product's, not a
# gap in this one.
close_issue() { fake_post "/_e2e/issues/$1/state" '{"state":"closed"}' >/dev/null; }

# fake_state — the fake's recorded state (issues, MRs, notes, label events). Read-only
# GET, wrapped in retry_read (like apiget) so a transient blip on a point-in-time read
# does not abort the run; the /_e2e mutators (fake_post) stay unwrapped (writes).
fake_state() { retry_read curl -fsSk "$FAKE_BASE/_e2e/state"; }

# fake_has_label IID LABEL — "yes" if the fake forge currently shows LABEL on issue
# IID, else "no". Use with wait_eq to POLL rather than read once: a label toggle is
# forge-first and the fake forge applies it synchronously, but under heavy concurrent-
# e2e host contention the /_e2e/state read can momentarily race the write, so a one-shot
# check flakes. Polling matches how every other forge-state assertion here waits.
fake_has_label() {
  fake_state | jq -r --argjson iid "$1" --arg lbl "$2" \
    'if any(.issues[]?; .iid==$iid and ((.labels // []) | index($lbl))) then "yes" else "no" end'
}

# note_count IID / notes_text IID — issue-comment introspection.
note_count() { fake_state | jq --argjson iid "$1" '[.notes[]? | select(.issue_iid==$iid)] | length'; }
notes_text() { fake_state | jq -r --argjson iid "$1" '.notes[]? | select(.issue_iid==$iid) | .body'; }

# add_label_event IID ACTION USERNAME — append an add/remove of the `autopilot`
# label by USERNAME (a fresh, larger event id each time).
add_label_event() {
  fake_post "/_e2e/issues/$1/label-events" \
    "$(jq -nc --arg ac "$2" --arg u "$3" '{action:$ac,label:"autopilot",username:$u}')" >/dev/null
}

# create_autopilot_issue TITLE DESC ADDER AUTHOR — stage a PRD+autopilot issue on
# the fake authored by AUTHOR, then record the initial autopilot-label add by
# ADDER. Echoes the new issue iid.
create_autopilot_issue() {
  local iid
  # PRD #764 D7: autopilot candidacy requires BOTH the autopilot label AND the uzi
  # run-eligibility label (the query repointed from the PRD label to uzi_label).
  iid="$(fake_post /_e2e/issues \
    "$(jq -nc --arg t "$1" --arg d "$2" --arg a "$4" '{title:$t,description:$d,labels:["uzi","autopilot"],author:$a}')" \
    | jq -r '.iid')"
  [ -n "$iid" ] && [ "$iid" != null ] || fail "could not stage autopilot issue on the fake"
  add_label_event "$iid" add "$3"
  printf '%s' "$iid"
}

# wait_run_for_issue IID [TIMEOUT] — poll until an autopilot run exists for IID
# (the poller creates it unattended); echoes the run id.
wait_run_for_issue() {
  local iid="$1" timeout="${2:-40}" rid
  local start=$SECONDS deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    rid="$(apiget "/api/runs?issue_iid=$iid" | jq -r '.runs[0].id // empty')"
    if [ -n "$rid" ]; then
      # record_margin writes only to MARGINS_FILE — it must not touch the stdout this
      # helper uses to hand the run id back to the caller.
      record_margin "autopilot run for issue #$iid" "$((SECONDS - start))" "$timeout"
      printf '%s' "$rid"; return 0
    fi
    sleep 0.3
  done
  fail "timeout: no autopilot run appeared for issue #$iid"
}

# assert_no_run_for_issue IID [SETTLE] — let a few detector ticks pass, then prove
# the issue only drew a comment, never a run.
assert_no_run_for_issue() {
  sleep "${2:-6}"
  local rid; rid="$(apiget "/api/runs?issue_iid=$1" | jq -r '.runs[0].id // empty')"
  [ -z "$rid" ] || fail "issue #$1 unexpectedly spawned run $rid (autopilot should have only commented)"
}

# wait_autopilot_done RUN WANT [TIMEOUT] — like wait_status, but treats
# awaiting_approval as a hard failure: an autopilot run that parks at the plan
# gate means auto-approve is broken (the run would hang there forever).
wait_autopilot_done() {
  local run="$1" want="$2" timeout="${3:-120}" s
  local start=$SECONDS deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    s="$(apiget "/api/runs/$run" | jq -r '.run.status')"
    if [ "$s" = "$want" ]; then record_margin "autopilot run status -> $want" "$((SECONDS - start))" "$timeout"; return 0; fi
    [ "$s" = awaiting_approval ] && fail "autopilot run $run parked at awaiting_approval — auto-approve did not fire"
    case "$s" in
      failed|cancelled) [ "$s" = "$want" ] || fail "autopilot run $run entered '$s' while waiting for '$want'";;
    esac
    sleep 0.3
  done
  fail "timeout: autopilot run $run never reached '$want' (last: ${s:-none})"
}

# wait_notes IID WANT [TIMEOUT] — poll until IID has exactly WANT comments; fail
# fast if it ever exceeds WANT (the exactly-once guarantee is the whole point).
wait_notes() {
  local iid="$1" want="$2" timeout="${3:-40}" n
  local start=$SECONDS deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    n="$(note_count "$iid")"
    if [ "$n" = "$want" ]; then record_margin "issue #$iid notes -> $want" "$((SECONDS - start))" "$timeout"; return 0; fi
    { [ -n "$n" ] && [ "$n" -gt "$want" ] 2>/dev/null; } && fail "issue #$iid has $n comments, expected $want (over-commented)"
    sleep 0.3
  done
  fail "timeout: issue #$iid never reached $want comment(s) (last: ${n:-none})"
}

# =============================================================================
say "provisioning scratch dir $RUNROOT (project $PROJECT, web $BASE, executor $EXECUTOR)"
mkdir -p "$RUNROOT/certs" "$RUNROOT/agent-gitconfig" "$RUNROOT/fakeremote" "$RUNROOT/forge-fake-state"
chmod a+rwX "$RUNROOT/forge-fake-state"  # forge-fake persists its recorded state here (survives the restart)
# Arm the wait_* margin recorder now that the scratch dir exists (PRD #97 M9). Waits
# before this point (there are none that matter) simply go unrecorded.
MARGINS_FILE="$RUNROOT/wait-margins.tsv"
: > "$MARGINS_FILE"

# Self-signed cert for forge-fake.e2e (trusted by api/worker/git in the overlay).
cat > "$RUNROOT/certs/openssl.cnf" <<'EOF'
[req]
distinguished_name = dn
x509_extensions = v3
prompt = no
[dn]
CN = forge-fake.e2e
[v3]
basicConstraints = critical,CA:TRUE
subjectAltName = DNS:forge-fake.e2e
EOF
openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
  -keyout "$RUNROOT/certs/key.pem" -out "$RUNROOT/certs/cert.pem" \
  -config "$RUNROOT/certs/openssl.cnf" >/dev/null 2>&1
chmod 0644 "$RUNROOT/certs/"*.pem

# Stub `devbox` for the PRD #18 tool-provisioning scenario: the isolated stack has
# no substituter egress, so a real `devbox install` is neither possible nor wanted.
# This fake satisfies the worker's provision path — `install` is a no-op; `shellenv`
# prints one allowlisted PATH line filterShellenv keeps — so the provisioning wiring
# is exercised end to end, fast, with no network. Bind-mounted over the baked-in
# binary by the overlay; only invoked when a run has tier-1 packages.
cat > "$RUNROOT/fake-devbox" <<'EOF'
#!/bin/sh
case "$1" in
  install)  exit 0 ;;
  shellenv) printf 'export PATH="/nix/e2e-tools/bin:${PATH}"\n'; exit 0 ;;
  *)        exit 0 ;;
esac
EOF
chmod 0755 "$RUNROOT/fake-devbox"

# Local bare repos standing in for GitLab's git server, each seeded with a main
# commit. repo2 exists only for the PRD #42 M5 bounded-concurrency scenario: two
# runs on two DIFFERENT repos must clone into independent bare-caches so the
# per-repo GitCache lock (agent/src/git.ts) never serializes them. It stays
# dormant for every other phase (the seed enables only group/repo, and no phase
# clones repo2 until M5 enables the repo).
for r in repo repo2; do
  # --shared=0777 is LOAD-BEARING, not tidiness: this one bind-mounted bare is written
  # by MORE THAN ONE uid (forge-fake's receive-pack as root over smart-HTTP, the worker
  # as its own in-image uid over the local-path insteadOf push — the same multi-uid fact
  # the teardown-rm comment above records). Without it, whichever uid first creates an
  # objects/xx/ dir owns it 0755, and a later push by the OTHER uid whose object sha lands
  # in that same dir fails: "unable to write file ./objects/xx/...: Permission denied ->
  # unable to migrate objects to permanent storage", failing the push and the run. It is
  # sha-prefix-dependent, so it flakes (green until a collision hits). sharedRepository
  # makes every receive-pack create objects/ and refs/ dirs 0777, so any uid can add into
  # a dir another uid created (object files stay 0444, but they are immutable and never
  # rewritten, only added). Set at init so it governs the seed push and every push after.
  #
  # NOTE: git records `--shared=0777` in core.sharedRepository as `0666` (it strips the
  # execute bits, which it re-adds for directories on its own); an omitted `--shared`
  # leaves the key unset. 00-preflight.sh asserts the key is set to a world-shared value,
  # not the literal 0777 — see that phase.
  #
  # E2E_FAULT_PREFLIGHT (PRD #966 M3) is the positive control for 00-preflight's first
  # assertion: when set, the bares are inited WITHOUT sharedRepository, so the key is
  # absent and preflight FAILs naming core.sharedRepository. The later
  # `chmod -R a+rwX "$RUNROOT/fakeremote"` changes only filesystem bits, NOT this tracked
  # config value, so the fault genuinely removes the fact preflight checks.
  shared=--shared=0777
  [ -n "${E2E_FAULT_PREFLIGHT:-}" ] && shared=
  # shellcheck disable=SC2086  # ${shared:+"$shared"} drops the flag entirely when empty
  git init --bare -q ${shared:+"$shared"} "$RUNROOT/fakeremote/$r.git"
  git -C "$RUNROOT/fakeremote/$r.git" symbolic-ref HEAD refs/heads/main
  # Allow pushes over git smart-HTTP (the E2E_GIT_SMART_HTTP variant); a no-op for
  # the default local-path remote.
  git -C "$RUNROOT/fakeremote/$r.git" config http.receivepack true
  seedwc="$RUNROOT/.seedwc-$r"
  git -C "$RUNROOT" clone -q "$RUNROOT/fakeremote/$r.git" ".seedwc-$r"
  git -C "$seedwc" checkout -q -b main
  printf '# %s\n\nSeeded by the uzi E2E harness.\n' "$r" > "$seedwc/README.md"
  if [ "$r" = repo ]; then
    # A repo-borne skill for the PRD #16 M6 opt-in path. Carries a capability key
    # (allowed-tools) that MUST be stripped when the flag is on; the worker loads only
    # name+description. It stays invisible unless the repo owner enables repo skills.
    mkdir -p "$seedwc/.claude/skills/e2e-repo-skill"
    printf -- '---\nname: e2e-repo-skill\ndescription: A repo-borne skill for the M6 opt-in E2E.\nallowed-tools: Bash, Write\n---\n\n# E2E repo skill\n\nProves the repo-skill opt-in path end to end.\n' \
      > "$seedwc/.claude/skills/e2e-repo-skill/SKILL.md"
    # A repo-borne agent roster for the PRD #37 detect→choose→apply path. The worker
    # parses these after clone (settingSources stays []) and reports them on the run;
    # the gate then defaults to the repo source. Two files so the harness can approve
    # with the repo source while EXCLUDING one — proving choose+apply end to end.
    mkdir -p "$seedwc/.claude/agents"
    printf -- '---\nname: repo-coder\ndescription: Repo-defined coder for the M6 E2E.\ntools: Read, Edit, Bash\n---\n\nYou implement changes for this repo.\n' \
      > "$seedwc/.claude/agents/repo-coder.md"
    printf -- '---\nname: repo-reviewer\ndescription: Repo-defined reviewer for the M6 E2E.\ntools: Read, Grep\n---\n\nYou review changes for this repo.\n' \
      > "$seedwc/.claude/agents/repo-reviewer.md"
    git -C "$seedwc" add README.md .claude
  else
    git -C "$seedwc" add README.md
  fi
  git -C "$seedwc" -c user.name=seed -c user.email=seed@uzi.e2e -c commit.gpgsign=false commit -q -m "seed: initial commit ($r)"
  git -C "$seedwc" push -q origin main
  rm -rf "$seedwc"
done

# PRD #97 M1 — protected-branch backstop on the fake remote (Decision 2 / Risk 3).
# The top directive is "main is never touched"; the fake bare would otherwise ACCEPT a
# push to main (http.receivepack true, no ref filter), so the "main-push refused"
# assertion would be vacuous. Install a pre-receive hook that refuses refs/heads/main.
#
# It is LOAD-BEARING and must fire under BOTH e2e git transports. Because the bare is
# ONE shared host dir (mounted /fakeremote in the agent, /gitroot in forge-fake),
# receive-pack reads THIS hook whether it runs in the agent image (local-path insteadOf
# push) or in forge-fake (smart-HTTP). Verified: a pushing client's own core.hooksPath
# override (as the worker's gitEnv sets) does NOT suppress a server-side pre-receive, so
# the hook fires for worker pushes too — and since it rejects ONLY main, every legitimate
# agent-branch push still lands. Installed AFTER the seed `main` push above (else it would
# refuse the seed). POSIX sh + no external tools, so it is portable across the Alpine
# (agent) and Debian (forge-fake) images.
for r in repo repo2; do
  cat > "$RUNROOT/fakeremote/$r.git/hooks/pre-receive" <<'EOF'
#!/bin/sh
# uzi e2e: refuse any update to the protected branch main (see run-e2e.sh PRD #97 M1).
status=0
while read -r _old _new ref; do
  case "$ref" in
    refs/heads/main)
      echo "pre-receive: refusing push to protected branch main (uzi never touches main)" >&2
      status=1
      ;;
  esac
done
exit $status
EOF
  chmod 0755 "$RUNROOT/fakeremote/$r.git/hooks/pre-receive"
done

chmod -R a+rwX "$RUNROOT/fakeremote"

if [ -n "${E2E_GIT_SMART_HTTP:-}" ]; then
  # Fidelity variant: leave the clone/push URL pointing at forge-fake's git
  # smart-HTTP endpoint (no insteadOf), so the worker's git-over-HTTPS Basic auth
  # is genuinely exercised (forge-fake 401s without a valid Authorization: Basic).
  # safe.directory=* is carried even here: this file is the worker's GIT_CONFIG_GLOBAL,
  # a config FILE, which is what an ownership check in a spawned receive-pack actually
  # reads — gitEnv()'s inline command-scope pin gets stripped before that child.
  # See the default-branch note below for why the file (not the pin) is load-bearing.
  cat > "$RUNROOT/agent-gitconfig/gitconfig" <<'EOF'
[safe]
	directory = *
EOF
  GIT_MODE="smart-HTTP (Basic auth exercised)"
else
  # Default: rewrite the https clone/push URL to the local bare remote (fast,
  # hermetic; does NOT exercise git-over-HTTPS auth — see README).
  #
  # safe.directory=* is REQUIRED here, not optional: the worker pushes to the local
  # bare /fakeremote/repo.git, whose bind-mounted host dir has an owner uid that
  # differs from the in-container uid on a CI runner (they happen to match on a dev
  # laptop, which is why this passed locally for a long time). git then trips
  # `detected dubious ownership` on the bare and refuses the push. gitEnv() DOES pin
  # safe.directory=* — but only at COMMAND scope (GIT_CONFIG_COUNT/KEY/VALUE), and git
  # STRIPS those vars from the environment when it spawns the local `receive-pack`, so
  # the pin never reaches the ownership check, which runs in that child (verified on
  # git 2.55: the child sees GIT_CONFIG_COUNT unset and no safe.directory; a global
  # config FILE, by contrast, receive-pack reads on its own). So the trust MUST live in
  # this file, which is the worker's GIT_CONFIG_GLOBAL. This mirrors exactly what the
  # `neutral` config below already does for the harness's own git probes.
  cat > "$RUNROOT/agent-gitconfig/gitconfig" <<'EOF'
[safe]
	directory = *
[url "/fakeremote/repo.git"]
	insteadOf = https://forge-fake.e2e/group/repo.git
[url "/fakeremote/repo2.git"]
	insteadOf = https://forge-fake.e2e/group/repo2.git
EOF
  GIT_MODE="local bare remote via insteadOf"
fi
say "git remote mode: $GIT_MODE"

# A NEUTRAL global gitconfig for the PRD #97 M1 main-reject backstop's harness-driven
# git ops inside the agent: it carries ONLY safe.directory=* (trusted, since it is a
# global config — needed because the bind-mounted bare's owner uid differs from the
# in-container uid) and deliberately NO insteadOf, so a push to a forge-fake.e2e https URL
# actually speaks smart-HTTP instead of being rewritten to the local bare. Used via
# `compose exec -e GIT_CONFIG_GLOBAL=/e2e-git/neutral` so those probes are a plain git
# client, independent of the worker's own gitconfig + pins.
cat > "$RUNROOT/agent-gitconfig/neutral" <<'EOF'
[safe]
	directory = *
EOF

# Per-run env-file: strong generated secrets for the base stack + the scratch dir
# the overlay bind-mounts. UZI_WORKER_TOKEN is only a placeholder here (the worker is
# not up yet); run-e2e.sh EXPORTS the real minted token before the agent starts, and a
# shell export overrides the --env-file value for the `worker_token` secret source
# (verified: compose ranks shell env above --env-file for an env-sourced secret).
cat > "$ENVFILE" <<EOF
E2E_RUN_DIR=$RUNROOT
E2E_WEB_PORT=$WEB_PORT
E2E_FAKE_PORT=$FAKE_PORT
UZI_E2E_EXECUTOR=$EXECUTOR
POSTGRES_USER=uzi
POSTGRES_DB=uzi
POSTGRES_PASSWORD=$(openssl rand -hex 16)
JWT_SECRET=$(openssl rand -hex 64)
UZI_SECRET_KEY=$(openssl rand -base64 32)
UZI_WORKER_TOKEN=e2e-placeholder-unused
# Tighten the heartbeat-stale window (default 45s) from BOOT so the sweeper's
# worker-loss recovery is bounded for the PRD #42 mid-run-kill step without a
# dedicated mid-suite api recreate. 15s is still 3x the 5s heartbeat interval, so
# a LIVE worker is never spuriously swept; the overlay's api service reads this
# via the E2E_WORKER_HEARTBEAT_STALE interpolation default.
E2E_WORKER_HEARTBEAT_STALE=15s
EOF


# hoisted from phase 18 (PRD #966 M1): used by multiple phases
uzi_cli() { env -i HOME="$RUNROOT" PATH="$PATH" UZI_URL="$BASE" UZI_TOKEN="$UZI_TOKEN_VAL" UZI_SKILL_AUTO_UPGRADE=0 UZI_VERSION_CHECK=0 "$UZI_BIN" "$@"; }

# hoisted from phase 37 (PRD #966 M1): used by multiple phases
run_printed_instructions() {
  local label="$1" shape="$2" want="$3" out="$4" matches n cmd
  matches="$(printf '%s\n' "$out" | grep -oE "$shape" || true)"
  n="$(printf '%s' "$matches" | grep -c . || true)"
  [ "$n" = "$want" ] || fail "$label: expected exactly $want printed instruction(s) matching /$shape/ in the emitting command's OWN output, got $n. The output was:
$out"
  : > "$PRINTED_OUT"
  # PER-INSTRUCTION CAPTURE, alongside the concatenated $PRINTED_OUT the older rows grep.
  #
  # WHY BOTH. $PRINTED_OUT is the UNION of every execution, so a `grep -q` over it is
  # satisfied by the FIRST instruction alone: N lifted, N executed, ONE certified. That is
  # the same shape as a loop that runs with one element actually checked, and it is the
  # weakness these rows exist to close — the undo row seeds a coordinate on TWO reviews
  # precisely so a single-address regression cannot pass as a green.
  #
  # A COUNT OVER THE UNION IS NOT THE FIX, and that was learned by shipping it: the B4' row
  # replaced `grep -q` with `grep -c … -ge 2` and turned a check satisfiable by ONE line into
  # one satisfiable by NONE, because the two anchored re-reads legitimately print DIFFERENT
  # things. Per-instruction files let a caller assert what is true of EACH execution instead
  # of guessing a number that is true of the pile.
  rm -f "$PRINTED_OUT".[0-9]* 2>/dev/null || true
  PRINTED_N=0
  while IFS= read -r cmd; do
    [ -n "$cmd" ] || continue
    case "$cmd" in
      "uzi "*) ;;
      *) fail "$label: lifted span is not a uzi instruction: $cmd" ;;
    esac
    # THE FLOOR (mechanism 4 above). Anchored at both ends, positive class only. Every span
    # the FOUR current rows lift already satisfies it, so it reddens nothing today — which
    # is exactly why it was exercised deliberately rather than assumed; see the commit.
    #
    # 🔴 `[[ =~ ]]`, NOT `grep -qE`, AND THAT IS THE WHOLE POINT OF THE GUARD. grep is
    # LINE-oriented: `^…$` anchors per LINE, so `grep -q` returns 0 when ANY line matches
    # and the rest of the span is never examined. Measured against the first version of this
    # check: the span "uzi repo list\n; touch /tmp/PWNED" was ACCEPTED, because line 1
    # matched. Bash's `=~` matches the WHOLE STRING and `$` is end-of-string; the same span
    # is rejected and the legal `review undo <uuid> <uuid>` span still passes.
    #
    # Reachability of that hole today is ZERO — `while IFS= read -r cmd` yields one line per
    # iteration, so `cmd` cannot contain a newline. But the floor exists precisely because
    # the safety burden sat on an invariant OUTSIDE the helper, and for newline the grep form
    # left it outside: it moved from the caller's ERE to the read loop. Newline is the one
    # excluded character that is itself a command separator, and a future edit to the lift
    # path (`grep -oEz`, `mapfile`, a `for` over $matches) re-opens it silently while the
    # comment above still reads as if it could not. This form owes nothing to the read loop.
    [[ "$cmd" =~ ^uzi\ [A-Za-z0-9\ ._:/=-]+$ ]] \
      || fail "$label: lifted span carries a character outside the executable allowlist, so it is not runnable verbatim: $cmd"
    PRINTED_N=$((PRINTED_N + 1))
    eval "uzi_cli ${cmd#uzi }" > "$PRINTED_OUT.$PRINTED_N" 2>&1 \
      || fail "$label: the printed instruction FAILED when run VERBATIM: $cmd
$(cat "$PRINTED_OUT.$PRINTED_N")"
    cat "$PRINTED_OUT.$PRINTED_N" >> "$PRINTED_OUT"
    # THE HEREDOC BELOW IS LOAD-BEARING — do not "tidy" it into `printf … | while read`.
    # `fail` ends in `exit 1`. Fed by a heredoc, this loop runs in the CURRENT shell, so a
    # `fail` inside it kills the script. Fed by a PIPE, the loop would run in a subshell and
    # that `exit 1` would kill only the subshell — the run would continue past a failed
    # instruction with its diagnostic swallowed. That is not hypothetical: the probe written
    # to verify this very guard had exactly that bug in its stub, and read as "the floor did
    # not fire" when it had.
  done <<PI_EOF
$matches
PI_EOF
}


# hoisted from phase 40 (PRD #966 M1): used by multiple phases
wait_msg_kind() {
  local run="$1" kind="$2" timeout="${3:-20}"
  local start=$SECONDS deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    if apiget "/api/runs/$run/messages" \
      | jq -e --arg k "$kind" '[.messages[] | select(.kind==$k)] | length >= 1' >/dev/null 2>&1; then
      record_margin "chat msg kind -> $kind" "$((SECONDS - start))" "$timeout"; return 0
    fi
    sleep 0.3
  done
  fail "timeout: run $run never emitted a '$2' message"
}
