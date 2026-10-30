#!/usr/bin/env bash
# Poll a uzi run's status until it reaches a stop-set state, printing each
# transition with a timestamp. Launch with a verified native completion callback
# or an explicitly delegated observer; background execution alone is not coverage.
#
# Foreground tool calls have finite limits, not multi-hour coverage. This bounded
# poller emits stop/expiry evidence for an explicitly owned completion route;
# no process-lifetime guarantee follows from running in the background.
# Restarting a killed or expired poll is the owner's responsibility. This
# script neither notifies a session nor re-arms itself.
#
# uzi's benign "CLI is behind server" line goes to stderr and is discarded here.
#
# Usage: watch-run.sh <run-id> [stop-csv] [interval-secs] [max-polls] [min-plan-seq] [min-question-seq]
#   stop-csv (default) covers a plan gate, a question park, and every terminal
#   state: completed,failed,cancelled,awaiting_approval,awaiting_input
#   (`paused`, recovery_wait/worker_requeue_exhausted, recovery_wait/retained_recovery_adoption_failed and recovery_wait/worker_memory_pressure always stop; retained_recovery_exhausted stops when owner-held; see below)
#   For "watch to the end only" (after you have approved), pass:
#     watch-run.sh <id> completed,failed,cancelled 60
#
#   min-plan-seq (optional 5th arg) — use it after `uzi run revise` to wait for
#   the REVISED plan. When set, an `awaiting_approval` state only stops the watch
#   once the run has a plan message with seq > min-plan-seq. This is immune to
#   whether the poll happened to catch the transient re-planning `running` window
#   (a "did I observe running?" heuristic loops forever if it starts too late).
#   Terminal and `awaiting_input` states still stop unconditionally. Capture the
#   baseline BEFORE revising:
#     SEQ=$(uzi run logs <id> --json | jq -rs '[.[]|select(.kind=="plan")|.seq]|max // 0')
#     uzi run revise <id> -m "…"
#     watch-run.sh <id> "" "" "" "$SEQ"
#
#   min-question-seq (optional 6th arg) — use it when re-arming right after
#   `uzi run answer` or `uzi run decide`: the status can still read the answered
#   `awaiting_input` for a poll or two, and a plain re-arm stops on it at once,
#   leaving the run unwatched. When set, `awaiting_input` only stops the watch once
#   the newest question message is no longer the answered one: only a newest
#   question seq exactly equal to min-question-seq keeps polling (attention checks
#   still run); a newer, missing, unreadable or lower seq stops, never masking a
#   park. Capture it before answering:
#     QSEQ=$(uzi run logs <id> --json | jq -rs '[.[]|select(.kind=="question")|.seq]|max // 0')
#
# Attention stop (read-only; never changes run state). A dead run can read `running`
# forever, so status-only polling cannot see it. When the run is non-terminal and NOT yet in
# the stop set, each poll also reads `uzi run get --json` and counts a poll as "attention"
# when any of these holds:
#   - health is set, not `ok`, and not in WATCH_ATTENTION_IGNORE_HEALTH (default `slow`,
#     the near-timeout hint): stalled, looping, waiting_worker, approval_idle, ...
#   - status is recovery_wait with recovery_wait_cause `data_volume_full` (a counted disk park: the
#     worker's data volume is nearly full; the run fails once its counted parks reach the cap, so
#     an unattended loop of parks must not pass silently). Its episode is
#     `data_volume_full@park<disk_park_count>`, so acking one park still alerts on the next.
#   - status is running/claimed, the run has a worker_id, and `uzi worker list --json`
#     shows that worker's last_heartbeat_at older than WATCH_HEARTBEAT_STALE_SECS
#     (default 180; the api default WORKER_HEARTBEAT_STALE is 45s, configurable). An unreadable worker list
#     or an absent worker row is no signal.
# After WATCH_ATTENTION_POLLS consecutive attention polls (default 3, so a transient single
# poll never fires; 0 disables the check) it prints STOP=needs_attention plus the evidence
# (status, health, health_reason, health_since, worker, worker_status, heartbeat age) and
# exits 4. The stop means "investigate": stalled health does not prove the run is dead and a
# stale heartbeat does not prove work was lost.
# An exit-4 stop ends coverage, so it also prints REARM=<command>: the same watch (arguments and
# effective WATCH_* settings), re-launched with WATCH_ATTENTION_ACK set to the observed health episode. Investigate, then run that line
# in the background; never leave a watched run unpolled. WATCH_ATTENTION_ACK is a comma list of
# `<health>@<health_since>` episodes whose health signal is ignored (a run that stays stalled in
# the SAME episode, e.g. a long test suite, then stops only on the stop set, a NEW non-ok episode,
# or a stale heartbeat, which is never acked).
# Owner-decision park: `recovery_wait` with recovery_wait_cause `worker_requeue_exhausted` never
# resumes on its own (the owner must Resume or Cancel), so it always stops the watch, whatever the
# stop set (also when the stop set names recovery_wait): STOP=recovery_wait:worker_requeue_exhausted,
# RESULT=stop:recovery_wait, exit 0. So does an owner-held `retained_recovery_exhausted` park (#3495:
# recovery_retry_not_before present and explicitly null = retries capped):
# STOP=recovery_wait:retained_recovery_exhausted. A stamped, omitted or malformed exhaustion stamp keeps polling.
# Every worker_memory_pressure park requires an owner decision, regardless of memory_hold:
# remaining counts interventions, not automatic retries. Always emit
# STOP=recovery_wait:worker_memory_pressure, RESULT=stop:recovery_wait, exit 0. Back up available
# work first, then ask the owner to Resume or Cancel.
# A retained_recovery_adoption_failed park always stops as an owner decision regardless of retry stamp:
# integrity checks refused adoption; local work and custody are preserved, with no automatic retry.
# Other recovery_wait causes keep polling unless the stop set names
# recovery_wait, except a `data_volume_full` park, which is an attention signal (above).
# `paused` never resumes on its own either (an owner pause, or hold_reason budget_exhausted /
# completion hold that needs extend, resume or decide), so it always stops the watch, whatever the
# stop set: STOP=paused, a NOTE with hold_reason, RESULT=stop:paused, exit 0.
# Exit codes: 0 stop-set state (not necessarily success), 3 coverage elapsed,
# 4 needs_attention. The final RESULT line distinguishes these outcomes.
set -u
RID="${1:?usage: watch-run.sh <run-id> [stop-csv] [interval] [max] [min-plan-seq] [min-question-seq]}"
STOP="${2:-completed,failed,cancelled,awaiting_approval,awaiting_input}"
# 30s default balances gate-detection latency against poll volume; MAX 240 keeps
# total coverage at ~2h (30*240=7200s) so a long implementation phase still fits.
# Pass a coarser interval (e.g. 60) for a to-completion watch where the state you
# await is hours off and snappy detection buys nothing.
INT="${3:-30}"
MAX="${4:-240}"
MIN_PLAN_SEQ="${5:-}"
MIN_QUESTION_SEQ="${6:-}"
ATTN_POLLS="${WATCH_ATTENTION_POLLS:-3}"
HB_STALE="${WATCH_HEARTBEAT_STALE_SECS:-180}"
ATTN_IGNORE="${WATCH_ATTENTION_IGNORE_HEALTH:-slow}"
ATTN_ACK="${WATCH_ATTENTION_ACK:-}"

max_plan_seq() {
  uzi run logs "$RID" --json 2>/dev/null \
    | jq -rs '[.[] | select(.kind=="plan") | .seq] | max // 0'
}

# Prints the newest question seq, or nothing when the logs are unreadable or hold none.
max_question_seq() {
  local logs
  logs="$(uzi run logs "$RID" --json 2>/dev/null)" || return 0
  # Any question without a positive integer seq makes the evidence uncertain: print nothing.
  printf '%s' "$logs" | jq -rs '[.[] | select(.kind=="question") | .seq]
    | if length > 0 and all(type=="number" and . > 0 and . == floor) then max else empty end' 2>/dev/null || true
}

# attention_check returns 0 (and sets ATTN_EVIDENCE) when this poll shows attention, 1 when
# it does not or nothing is readable. Never writes to uzi.
attention_check() {
  local status="$1" rj="$2" health reason since wid hb_age="" wstatus="" hb dat=0 wl w parks ep disk=""
  printf '%s' "$rj" | jq -e 'type=="object"' >/dev/null 2>&1 || return 1
  health="$(printf '%s' "$rj" | jq -r '.health // ""')"
  reason="$(printf '%s' "$rj" | jq -r '(.health_reason // "")|gsub("\n";" ")|.[0:200]')"
  since="$(printf '%s' "$rj" | jq -r '.health_since // ""')"
  wid="$(printf '%s' "$rj" | jq -r '.worker_id // ""')"
  ATTN_EPISODE=""
  case "$health" in
    ''|ok) ;;
    *) case ",$ATTN_IGNORE," in
         *",$health,"*) ;;
         *) case ",$ATTN_ACK," in
              *",$health@$since,"*) ;;
              *) dat=1; ATTN_EPISODE="$health@$since" ;;
            esac ;;
       esac ;;
  esac
  if [ "$status" = recovery_wait ] && [ "$(printf '%s' "$rj" | jq -r '.recovery_wait_cause // ""')" = data_volume_full ]; then
    parks="$(printf '%s' "$rj" | jq -r '.disk_park_count // "unknown"')"
    ep="data_volume_full@park$parks"
    disk=" recovery_wait_cause=data_volume_full disk_park_count=$parks"
    case ",$ATTN_ACK," in
      *",$ep,"*) ;;
      *) dat=1; ATTN_EPISODE="${ATTN_EPISODE:+$ATTN_EPISODE,}$ep" ;;
    esac
  fi
  if [ -n "$wid" ] && { [ "$status" = running ] || [ "$status" = claimed ]; }; then
    wl="$(uzi worker list --json 2>/dev/null)" || wl=""
    w="$(printf '%s' "$wl" | jq -c --arg id "$wid" '[.[]?|select(.id==$id)]|first // empty' 2>/dev/null)" || w=""
    if [ -n "$w" ]; then
      wstatus="$(printf '%s' "$w" | jq -r '.status // ""')"
      hb="$(printf '%s' "$w" | jq -r '.last_heartbeat_at // ""')"
      if [ -n "$hb" ]; then
        hb_age="$(jq -rn --arg t "$hb" --argjson now "$(date +%s)" '$now - ($t|sub("\\.[0-9]+";"")|fromdateiso8601)' 2>/dev/null)" || hb_age=""
        case "$hb_age" in ''|*[!0-9-]*) hb_age="" ;; esac
        if [ -n "$hb_age" ] && [ "$hb_age" -gt "$HB_STALE" ]; then dat=1; fi
      fi
    fi
  fi
  [ "$dat" -eq 1 ] || return 1
  ATTN_EVIDENCE="status=$status health=${health:-none} health_reason='${reason}' health_since=${since:-none} worker=${wid:-none} worker_status=${wstatus:-unknown} heartbeat_age_secs=${hb_age:-unknown} (stale after ${HB_STALE}s)$disk"
  return 0
}

last=""
i=0
attn=0
ATTN_EVIDENCE=""
while [ "$i" -lt "$MAX" ]; do
  s="$(uzi run get "$RID" --field status 2>/dev/null)"
  if [ -n "$s" ] && [ "$s" != "$last" ]; then
    printf '%s status=%s\n' "$(date +%H:%M:%S)" "$s"
    last="$s"
  fi
  rj=""
  if [ "$s" = recovery_wait ]; then
    rj="$(uzi run get "$RID" --json 2>/dev/null)" || rj=""
    rc_cause="$(printf '%s' "$rj" | jq -r '.recovery_wait_cause // ""' 2>/dev/null)" || rc_cause=""
    if [ "$rc_cause" = worker_requeue_exhausted ]; then
      printf 'STOP=recovery_wait:worker_requeue_exhausted\n'
      printf 'NOTE owner decision: automatic worker recovery stopped; back up the work, then uzi run resume or cancel\n'
      printf 'RESULT=stop:recovery_wait\n'
      exit 0
    fi
    if [ "$rc_cause" = worker_memory_pressure ]; then
      printf 'STOP=recovery_wait:worker_memory_pressure\n'
      printf 'NOTE back up available work first; owner decision: memory-pressure holds never resume automatically; only owner uzi run resume or cancel\n'
      printf 'RESULT=stop:recovery_wait\n'
      exit 0
    fi
    if [ "$rc_cause" = retained_recovery_adoption_failed ]; then
      printf 'STOP=recovery_wait:retained_recovery_adoption_failed\n'
      printf 'NOTE owner decision: integrity check refused adoption; local work and custody preserved; no automatic retry; owner uzi run resume or cancel; source/archive availability unverified\n'
      printf 'RESULT=stop:recovery_wait\n'
      exit 0
    fi
    if [ "$rc_cause" = retained_recovery_exhausted ] &&
       printf '%s' "$rj" | jq -e 'has("recovery_retry_not_before") and .recovery_retry_not_before == null' >/dev/null 2>&1; then
      printf 'STOP=recovery_wait:retained_recovery_exhausted\n'
      printf 'NOTE owner decision: automatic retained-recovery retries are capped (no retry stamp); local work and custody are retained; uzi run resume or cancel\n'
      printf 'RESULT=stop:recovery_wait\n'
      exit 0
    fi
  fi
  if [ "$s" = paused ]; then
    rj="$(uzi run get "$RID" --json 2>/dev/null)" || rj=""
    hr="$(printf '%s' "$rj" | jq -r '.hold_reason // ""' 2>/dev/null)" || hr=""
    printf 'STOP=paused\n'
    printf 'NOTE paused never resumes on its own: hold_reason=%s; read it, then extend, resume, decide or wait for the owner\n' "${hr:-unknown}"
    printf 'RESULT=stop:paused\n'
    exit 0
  fi
  case ",$STOP," in
    *",$s,"*)
      # A revised-plan watch (min-plan-seq set) must not stop at the OLD gate:
      # only stop at awaiting_approval once a newer plan message exists.
      if [ "$s" = "awaiting_approval" ] && [ -n "$MIN_PLAN_SEQ" ]; then
        cur="$(max_plan_seq)"
        if [ "${cur:-0}" -le "$MIN_PLAN_SEQ" ]; then
          i=$((i + 1))
          sleep "$INT"
          continue
        fi
        printf 'STOP=%s (revised plan seq=%s > %s)\n' "$s" "$cur" "$MIN_PLAN_SEQ"
        printf 'RESULT=stop:%s\n' "$s"
        exit 0
      fi
      # An answered question can still read awaiting_input briefly: keep polling (with the
      # attention checks below) only while the newest question is positively the answered one.
      answered=0
      if [ "$s" = "awaiting_input" ] && [ -n "$MIN_QUESTION_SEQ" ]; then
        cur="$(max_question_seq)"
        if [ "$cur" = "$MIN_QUESTION_SEQ" ]; then
          answered=1
        else
          printf 'STOP=%s (newest question seq=%s, answered=%s)\n' "$s" "${cur:-none}" "$MIN_QUESTION_SEQ"
          printf 'RESULT=stop:%s\n' "$s"
          exit 0
        fi
      fi
      if [ "$answered" -eq 0 ]; then
        printf 'STOP=%s\n' "$s"
        printf 'RESULT=stop:%s\n' "$s"
        exit 0
      fi
      ;;
  esac
  case "$s" in
    ''|completed|failed|cancelled) attn=0 ;;
    *)
      if [ -z "$rj" ] && [ "$ATTN_POLLS" -gt 0 ]; then
        rj="$(uzi run get "$RID" --json 2>/dev/null)" || rj=""
      fi
      if [ "$ATTN_POLLS" -gt 0 ] && attention_check "$s" "$rj"; then
        attn=$((attn + 1))
        if [ "$attn" -ge "$ATTN_POLLS" ]; then
          printf 'STOP=needs_attention\n'
          printf 'EVIDENCE %s consecutive_polls=%s\n' "$ATTN_EVIDENCE" "$attn"
          printf 'NOTE investigate only: stalled health does not prove the run is dead and a stale heartbeat does not prove work was lost; this poller changed nothing\n'
          ack="$ATTN_ACK"
          if [ -n "$ATTN_EPISODE" ]; then ack="${ack:+$ack,}$ATTN_EPISODE"; fi
          printf 'REARM=WATCH_ATTENTION_ACK=%q WATCH_ATTENTION_POLLS=%q WATCH_HEARTBEAT_STALE_SECS=%q WATCH_ATTENTION_IGNORE_HEALTH=%q %q %q %q %q %q %q %q\n' \
            "$ack" "$ATTN_POLLS" "$HB_STALE" "$ATTN_IGNORE" "$0" "$RID" "$STOP" "$INT" "$MAX" "$MIN_PLAN_SEQ" "$MIN_QUESTION_SEQ"
          printf 'NOTE coverage ended: after investigating, run the REARM line in the background (a stale heartbeat is never acked and stops again)\n'
          printf 'RESULT=attention\n'
          exit 4
        fi
      else
        attn=0
      fi
      ;;
  esac
  i=$((i + 1))
  sleep "$INT"
done
printf 'ELAPSED last=%s (raise max-polls or re-launch)\n' "$last"
printf 'RESULT=elapsed\n'
exit 3
