#!/usr/bin/env bash
# Poll a uzi run's status until it reaches a stop-set state, printing each
# transition with a timestamp. Designed to be launched with Claude Code's
# run_in_background so the harness re-invokes the model when it exits.
#
# WHY a script and not `uzi run wait`: this harness reaps long-lived background
# processes, so a backgrounded `uzi run wait` gets killed mid-run and you never
# learn the outcome. A short poll loop that exits on the stop set survives,
# because each exit is a clean notification and a killed poll simply re-fires.
#
# uzi's benign "CLI is behind server" line goes to stderr and is discarded here.
#
# Usage: watch-run.sh <run-id> [stop-csv] [interval-secs] [max-polls] [min-plan-seq]
#   stop-csv (default) covers a plan gate, a question park, and every terminal
#   state: completed,failed,cancelled,awaiting_approval,awaiting_input
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
# Attention stop (read-only; never changes run state). A dead run can read `running`
# forever, so status-only polling cannot see it. When the run is non-terminal and NOT yet in
# the stop set, each poll also reads `uzi run get --json` and counts a poll as "attention"
# when either holds:
#   - health is set, not `ok`, and not in WATCH_ATTENTION_IGNORE_HEALTH (default `slow`,
#     the near-timeout hint): stalled, looping, waiting_worker, approval_idle, ...
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
# Exit codes: 0 stop-set state or ELAPSED (unchanged), 4 needs_attention.
set -u
RID="${1:?usage: watch-run.sh <run-id> [stop-csv] [interval] [max] [min-plan-seq]}"
STOP="${2:-completed,failed,cancelled,awaiting_approval,awaiting_input}"
# 30s default balances gate-detection latency against poll volume; MAX 240 keeps
# total coverage at ~2h (30*240=7200s) so a long implementation phase still fits.
# Pass a coarser interval (e.g. 60) for a to-completion watch where the state you
# await is hours off and snappy detection buys nothing.
INT="${3:-30}"
MAX="${4:-240}"
MIN_PLAN_SEQ="${5:-}"
ATTN_POLLS="${WATCH_ATTENTION_POLLS:-3}"
HB_STALE="${WATCH_HEARTBEAT_STALE_SECS:-180}"
ATTN_IGNORE="${WATCH_ATTENTION_IGNORE_HEALTH:-slow}"
ATTN_ACK="${WATCH_ATTENTION_ACK:-}"

max_plan_seq() {
  uzi run logs "$RID" --json 2>/dev/null \
    | jq -rs '[.[] | select(.kind=="plan") | .seq] | max // 0'
}

# attention_check returns 0 (and sets ATTN_EVIDENCE) when this poll shows attention, 1 when
# it does not or nothing is readable. Never writes to uzi.
attention_check() {
  local status="$1" rj health reason since wid hb_age="" wstatus="" hb dat=0 wl w
  rj="$(uzi run get "$RID" --json 2>/dev/null)" || return 1
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
  ATTN_EVIDENCE="status=$status health=${health:-none} health_reason='${reason}' health_since=${since:-none} worker=${wid:-none} worker_status=${wstatus:-unknown} heartbeat_age_secs=${hb_age:-unknown} (stale after ${HB_STALE}s)"
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
        exit 0
      fi
      printf 'STOP=%s\n' "$s"
      exit 0
      ;;
  esac
  case "$s" in
    ''|completed|failed|cancelled) attn=0 ;;
    *)
      if [ "$ATTN_POLLS" -gt 0 ] && attention_check "$s"; then
        attn=$((attn + 1))
        if [ "$attn" -ge "$ATTN_POLLS" ]; then
          printf 'STOP=needs_attention\n'
          printf 'EVIDENCE %s consecutive_polls=%s\n' "$ATTN_EVIDENCE" "$attn"
          printf 'NOTE investigate only: stalled health does not prove the run is dead and a stale heartbeat does not prove work was lost; this poller changed nothing\n'
          ack="$ATTN_ACK"
          if [ -n "$ATTN_EPISODE" ]; then ack="${ack:+$ack,}$ATTN_EPISODE"; fi
          printf 'REARM=WATCH_ATTENTION_ACK=%q WATCH_ATTENTION_POLLS=%q WATCH_HEARTBEAT_STALE_SECS=%q WATCH_ATTENTION_IGNORE_HEALTH=%q %q %q %q %q %q %q\n' \
            "$ack" "$ATTN_POLLS" "$HB_STALE" "$ATTN_IGNORE" "$0" "$RID" "$STOP" "$INT" "$MAX" "$MIN_PLAN_SEQ"
          printf 'NOTE coverage ended: after investigating, run the REARM line in the background (a stale heartbeat is never acked and stops again)\n'
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
