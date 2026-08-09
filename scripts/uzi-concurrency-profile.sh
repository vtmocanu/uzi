#!/usr/bin/env bash
# Subagent-concurrency profile for one run (PRD #215 M0).
#
# Given a run id, this reads the run's message history and reports how many
# subagents the lead kept busy over the run's wall clock: the time-weighted
# average concurrency, the peak, the share of time at 0 / exactly 1 / 2+ busy
# subagents, and a per-instance table (start offset + span). It is the baseline
# instrument the pipelining change (per-unit review, overlapped gate, seam-split)
# is measured against, before and after.
#
# Usage:
#   scripts/uzi-concurrency-profile.sh <run-id>      # calls `uzi run logs --json`
#   scripts/uzi-concurrency-profile.sh --stdin       # reads NDJSON on stdin
#   uzi run logs <run-id> --json > cap.ndjson
#     scripts/uzi-concurrency-profile.sh --stdin < cap.ndjson
#
# Archive captures + output under probes/ (see probes/README.md), never fixtures/.
#
# HEURISTIC, stated up front so the number is read honestly: a subagent instance
# is treated as "busy" from its FIRST to its LAST run_messages row
# (`agent_instance` groups a lane, `created_at` times it). A lane that emits one
# message has zero measured span and so adds nothing to the time-weighted average;
# a lane that is thinking without emitting reads as idle. This tracks the shape of
# a run's parallelism, not a per-token utilisation. Rows with a null
# `agent_instance` (the lead and pre-lane messages) set the run's wall-clock span
# but are not themselves counted as a busy subagent.
set -euo pipefail

usage() {
	sed -n '2,25p' "$0" >&2
	exit "${1:-2}"
}

need() {
	command -v "$1" >/dev/null 2>&1 || {
		echo "error: '$1' is required but not on PATH" >&2
		exit 2
	}
}

need jq

mode=""
run_id=""
case "${1:-}" in
"") usage 2 ;;
-h | --help) usage 0 ;;
--stdin | -) mode="stdin" ;;
-*)
	echo "error: unknown flag '$1'" >&2
	usage 2
	;;
*)
	mode="cli"
	run_id="$1"
	;;
esac

# Emit the run's NDJSON message stream to stdout.
emit_ndjson() {
	if [ "$mode" = "stdin" ]; then
		cat
	else
		need uzi
		uzi run logs "$run_id" --json
	fi
}

# jq program: NDJSON stream (slurped with -s) -> plain-text profile.
# Timestamps are normalised (sub-seconds dropped, any numeric offset folded to Z)
# before fromdateiso8601; only differences within the run are used, so a uniform
# offset is harmless.
read -r -d '' JQ_PROG <<'JQ' || true
def to_epoch:
  sub("\\.[0-9]+"; "")
  | sub("[+-][0-9]{2}:[0-9]{2}$"; "Z")
  | fromdateiso8601;

# All rows carrying a timestamp fix the run's wall-clock span.
( map(select(.created_at != null) | (.created_at | to_epoch)) ) as $all
| if ($all | length) == 0 then "no timestamped messages found\n"
else
  ($all | min) as $t0
| ($all | max) as $t1
| (($t1 - $t0) | if . <= 0 then 1 else . end) as $span
# Per-instance busy intervals: group the subagent lanes by agent_instance.
| ( map(select(.agent_instance != null and .created_at != null)
        | {inst: .agent_instance, agent: (.agent // "?"), t: (.created_at | to_epoch)})
    | group_by(.inst)
    | map({ inst: .[0].inst,
            agent: .[0].agent,
            start: (map(.t) | min),
            end:   (map(.t) | max) }) ) as $lanes
# Sweep line: +1 at each lane start, -1 at each lane end. Ends before starts on a
# tie, so a hand-off does not read as a phantom extra concurrent lane. A d:0
# sentinel at the run's end forces the trailing idle segment to be counted.
| ( $lanes | map({t: .start, d: 1}, {t: .end, d: -1})
    | . + [{t: $t1, d: 0}]
    | sort_by(.t, .d) ) as $events
| ( reduce $events[] as $e (
      {cur: 0, prev: $t0, wsum: 0, peak: 0, b0: 0, b1: 0, b2: 0};
      ( ($e.t - .prev) as $dt
        | .wsum += (.cur * $dt)
        | (if .cur == 0 then .b0 += $dt elif .cur == 1 then .b1 += $dt else .b2 += $dt end) )
      | .cur += $e.d
      | .peak = ([.peak, .cur] | max)
      | .prev = $e.t
    ) ) as $s
| ( ($s.wsum / $span) ) as $avg
| ( "run wall clock : \($span | floor)s across \($lanes | length) subagent instance(s)\n"
  + "avg concurrency: \($avg * 100 | round / 100) (time-weighted)\n"
  + "peak concurrency: \($s.peak)\n"
  + "time at 0 busy : \($s.b0 | floor)s (\(($s.b0 / $span) * 100 | round)%)\n"
  + "time at 1 busy : \($s.b1 | floor)s (\(($s.b1 / $span) * 100 | round)%)\n"
  + "time at 2+ busy: \($s.b2 | floor)s (\(($s.b2 / $span) * 100 | round)%)\n"
  + "\nper-instance (offset from run start, span):\n"
  + ( $lanes
      | sort_by(.start)
      | map("  +\((.start - $t0) | floor)s  \((.end - .start) | floor)s  \(.agent)  \(.inst)")
      | join("\n") )
  + "\n" )
end
JQ

emit_ndjson | jq -s -r "$JQ_PROG"
