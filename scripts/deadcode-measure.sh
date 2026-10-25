#!/bin/sh
# Best-effort diagnostics around one command. The command's own status wins.
# Internal interface: <scratch-dir> <module-dir> <command> [args...].
# No errexit: a failed measurement must not become a failed deadcode gate.
scratch=$1
module=$2
shift 2

note() {
  printf 'deadcode-gate: diagnostics: %s\n' "$*" >&2
}

os=$(uname -s 2>/dev/null) || os=unknown
note "module=$module os=$os"
note "runner_image_os=${ImageOS:-unavailable (ImageOS unset)}"
note "runner_image_version=${ImageVersion:-unavailable (ImageVersion unset)}"

case "$os" in
  Linux)
    memory=$(LC_ALL=C awk '
      $1 == "MemTotal:" && $2 ~ /^[0-9]+$/ && $3 == "kB" { total = $2 * 1024 }
      $1 == "MemAvailable:" && $2 ~ /^[0-9]+$/ && $3 == "kB" { available = $2 * 1024; found = 1 }
      END {
        if (total > 0) printf "total_memory_bytes=%.0f source=/proc/meminfo\n", total
        else print "total_memory_bytes=unavailable (MemTotal missing or malformed)"
        if (found) printf "available_memory_bytes=%.0f source=/proc/meminfo\n", available
        else print "available_memory_bytes=unavailable (MemAvailable missing or malformed)"
      }' /proc/meminfo 2>/dev/null) || memory="memory=unavailable (cannot read /proc/meminfo)"
    ;;
  Darwin)
    total=$(sysctl -n hw.memsize 2>/dev/null) || total=
    case "$total" in
      ''|*[!0-9]*) memory="total_memory_bytes=unavailable (sysctl hw.memsize missing or malformed)" ;;
      *) memory="total_memory_bytes=$total source=sysctl-hw.memsize" ;;
    esac
    pages=$(LC_ALL=C vm_stat 2>/dev/null) || pages=
    available=$(printf '%s\n' "$pages" | LC_ALL=C awk '
      /page size of [0-9]+ bytes/ {
        line = $0; sub(/^.*page size of /, "", line); sub(/ bytes.*$/, "", line)
        if (line ~ /^[0-9]+$/) size = line
      }
      /^Pages free:/ || /^Pages inactive:/ {
        line = $0; sub(/^.*:[[:space:]]*/, "", line); sub(/\.$/, "", line)
        if (line ~ /^[0-9]+$/) { sum += line; found++ }
      }
      END {
        if (size > 0 && found == 2)
          printf "available_memory_estimate_bytes=%.0f source=vm_stat-free-plus-inactive page_size_bytes=%.0f", sum * size, size
        else print "available_memory_estimate_bytes=unavailable (vm_stat missing or malformed)"
      }') || available="available_memory_estimate_bytes=unavailable (vm_stat parsing failed)"
    memory="$memory
$available"
    ;;
  *) memory="memory=unavailable (unsupported OS: $os)" ;;
esac
printf '%s\n' "$memory" | while IFS= read -r line; do note "$line"; done

# Both parsers emit only a number or an empty string. BSD RSS is bytes; GNU
# reports KiB. This is a per-process high-water mark, not summed tree RSS.
metric() {
  LC_ALL=C awk -v mode="$1" -v field="$2" '
    function seconds(value, parts, n, i, result) {
      n = split(value, parts, ":")
      if (n < 1 || n > 3) return ""
      result = 0
      for (i = 1; i <= n; i++) {
        if (parts[i] !~ /^[0-9]+([.][0-9]+)?$/) return ""
        result = result * 60 + parts[i]
      }
      return sprintf("%.3f", result)
    }
    mode == "gnu" && /Elapsed \(wall clock\) time/ {
      value = $0; sub(/^.*\):[[:space:]]*/, "", value)
      elapsed = seconds(value)
    }
    mode == "gnu" && /Maximum resident set size \(kbytes\):/ {
      value = $NF
      if (value ~ /^[0-9]+$/) rss = sprintf("%.0f", value * 1024)
    }
    mode == "bsd" {
      for (i = 2; i <= NF; i++) if ($i == "real") elapsed = seconds($(i - 1))
      if ($2 == "maximum" && $3 == "resident" && $4 == "set" && $5 == "size" && $1 ~ /^[0-9]+$/)
        rss = $1
    }
    END { if (field == "elapsed") print elapsed; else print rss }
  ' "$3" 2>/dev/null
}

timer=${DEADCODE_TIME_BIN:-/usr/bin/time}
mode=
reason="unsupported timer (only GNU -v and BSD -l are supported)"
version=$("$timer" --version 2>&1) || :
case "$version" in
  *BusyBox*) reason="unsupported BusyBox timer" ;;
  *"GNU time"*|*"GNU Time"*) mode=gnu ;;
  *)
    if [ "$os" = Darwin ]; then mode=bsd; fi
    ;;
esac
if [ ! -x "$timer" ]; then
  mode=
  reason="timer executable missing or not executable"
fi

# Select/reject the timer using a harmless probe, before the real command.
if [ -n "$mode" ]; then
  probe_rc=0
  if [ "$mode" = gnu ]; then
    LC_ALL=C "$timer" -v -o "$scratch/probe-time" sh -c ':' \
      >"$scratch/probe-out" 2>"$scratch/probe-err" || probe_rc=$?
  else
    LC_ALL=C "$timer" -l sh -c ':' \
      >"$scratch/probe-out" 2>"$scratch/probe-time" || probe_rc=$?
  fi
  elapsed=$(metric "$mode" elapsed "$scratch/probe-time")
  rss=$(metric "$mode" rss "$scratch/probe-time")
  if [ "$probe_rc" -ne 0 ] || [ -z "$elapsed" ] || [ -z "$rss" ]; then
    mode=
    reason="timer capability probe failed or output malformed"
  fi
fi

# Fixed shell code; all paths/command arguments are positional, never evaluated.
# The started marker permits untimed execution ONLY if the timer never launched
# the driver. Missing final metrics can never cause a second command invocation.
driver='
  scratch=$1
  module=$2
  shift 2
  : > "$scratch/started"
  rc=0
  (cd "$module" && "$@") >"$scratch/full" 2>"$scratch/err" || rc=$?
  printf "%s\n" "$rc" > "$scratch/child-status"
  exit 0
'
if [ -n "$mode" ]; then
  note "timer=$timer format=$mode"
  timer_rc=0
  if [ "$mode" = gnu ]; then
    LC_ALL=C "$timer" -v -o "$scratch/time" sh -c "$driver" deadcode-run "$scratch" "$module" "$@" \
      >"$scratch/timer-out" 2>"$scratch/timer-err" || timer_rc=$?
  else
    LC_ALL=C "$timer" -l sh -c "$driver" deadcode-run "$scratch" "$module" "$@" \
      >"$scratch/timer-out" 2>"$scratch/time" || timer_rc=$?
  fi
  if [ ! -f "$scratch/started" ]; then
    note "timing=unavailable (timer did not launch command); running untimed"
    mode=
    reason="timer failed to launch command"
  elif [ "$timer_rc" -ne 0 ]; then
    note "timing=unavailable (timer exited $timer_rc; child verdict retained)"
    note "command_elapsed_seconds=unavailable (timer exited $timer_rc)"
    note "peak_rss_bytes=unavailable (timer exited $timer_rc)"
  else
    elapsed=$(metric "$mode" elapsed "$scratch/time")
    rss=$(metric "$mode" rss "$scratch/time")
    note "command_elapsed_seconds=${elapsed:-unavailable (timer output missing or malformed)}"
    note "peak_rss_bytes=${rss:-unavailable (timer output missing or malformed)}"
    note "peak_scope=maximum individual-process RSS across command/descendants, including go run overhead; not simultaneous aggregate tree or whole-job memory"
  fi
fi
if [ -z "$mode" ]; then
  note "timer=$timer unavailable ($reason)"
  note "command_elapsed_seconds=unavailable ($reason)"
  note "peak_rss_bytes=unavailable ($reason)"
  sh -c "$driver" deadcode-run "$scratch" "$module" "$@"
fi

# The timer's status and diagnostics never override a recorded child status.
child_status=$(cat "$scratch/child-status" 2>/dev/null) || child_status=
case "$child_status" in
  ''|*[!0-9]*)
    note "command status unavailable (command failed to finish); no retry"
    exit 125
    ;;
  *) exit "$child_status" ;;
esac
