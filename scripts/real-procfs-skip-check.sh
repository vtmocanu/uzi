#!/bin/sh
# Run a test command, then fail it if real-procfs tests skipped where they should have run
# (issue #1863).
#
# WHY: agent tests that read the REAL process table skip through agent/test/real-procfs.ts
# when enumerating the proc root is denied (EACCES/EPERM), as it is inside the Landlock
# Codex command sandbox. A skip is green, so a broken detector, or a host that denies
# enumeration by mistake, would silently drop that coverage everywhere. This is the gate on
# the gate: every skip is appended to $UZI_REAL_PROCFS_SKIP_LOG, and afterwards this script
# lists them and probes enumeration itself, INDEPENDENTLY of the TypeScript helper. A skip
# is allowed only where the probe confirms enumeration is denied, and never in CI.
# `task test:agent` wraps its `npm test` with this, so CI's test-agent job runs it.
#
# SECOND DUTY: the same probe runs once BEFORE the command. Where enumeration is denied,
# libuv may also fail to discover its cgroup CPU quota through procfs. In that case
# os.availableParallelism() reports the host's CPU count, and `node --test`, whose
# default concurrency is max(availableParallelism() - 1, 1), runs a host-sized batch of agent test
# files at once on a small quota, which fails timing-sensitive tests. On a denied probe
# this exports UZI_AGENT_TEST_CONCURRENCY=1, which agent/package.json's test script turns
# into --test-concurrency=1 (not settable through NODE_OPTIONS). A caller's value is kept
# when it is a positive integer; any other value (empty, 0, text) is replaced by 1 with a
# warning, since the npm script splices it into the node command line verbatim. Where the
# probe says enumerable, the caller's value is left alone.
#
# With no caller value set and no denied-procfs cap, availableParallelism() == 2
# otherwise gives one file at a time. Floor that case at 2 (issue #2240); leave a
# one-CPU worker unchanged. Print the runtime, quota, effective concurrency/source
# and unit-stage duration so hosted gate measurements can be compared honestly.
#
# Usage: scripts/real-procfs-skip-check.sh <command> [args...]
#   UZI_REAL_PROCFS_PROBE_DIR    the directory to probe, before and after the command
#                                (default: /proc); for this script's own test
#                                (scripts/real-procfs-skip-check.test.sh) only.
#   UZI_AGENT_TEST_CONCURRENCY   kept when a positive integer (digits, no leading zero);
#                                a denied pre-run probe sets 1 when it is unset or invalid.
#
# EXIT CODES:
#   the command's own non-zero status, when it failed (the skip list still prints)
#   1 = the command passed but real-procfs tests skipped where they must run: in CI (CI
#       non-empty), on a host whose proc root enumerates, or where the probe hit an error
#       other than EACCES/EPERM; or the skip log was missing or uncountable afterwards
#   2 = usage / could not create the skip log / node missing for the post-run probe (a
#       missing node before the command only skips the concurrency cap)
#   0 = the command passed, and either nothing skipped or the probe confirms enumeration
#       is denied (a banner states how many tests skipped)
# POSIX sh on purpose: runs under busybox sh, dash and macOS /bin/sh alike.
set -u

if [ "$#" -eq 0 ]; then
  echo "real-procfs-skip-check: usage: $0 <command> [args...]" >&2
  exit 2
fi

# Portable template form (check:mktemp-portability): a full path with 6 X's, no -t.
log="$(mktemp "${TMPDIR:-/tmp}/uzi-real-procfs-skips.XXXXXX")" || {
  echo "real-procfs-skip-check: cannot create the skip log" >&2
  exit 2
}
trap 'rm -f "$log"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

probe_dir="${UZI_REAL_PROCFS_PROBE_DIR:-/proc}"
# run_probe: sets probe to 0 = enumerable, 3 = denied (EACCES/EPERM), 4 = any other error,
# and probe_msg to the probe's stderr line (empty when enumerable). Caller checks for node.
run_probe() {
  probe=0
  # shellcheck disable=SC2016 # the single-quoted program is JavaScript; its ${} is a JS template
  probe_msg="$(node -e '
const fs = require("node:fs");
try { fs.readdirSync(process.argv[1]); process.exit(0); }
catch (err) {
  const code = err && err.code;
  console.error(`probe: readdir ${process.argv[1]}: ${code || err}`);
  process.exit(code === "EACCES" || code === "EPERM" ? 3 : 4);
}' -- "$probe_dir" 2>&1 > /dev/null)" || probe=$?
  probed=1
}

# Probe once up front; the post-run verdict reuses this result. No node here is not an error
# yet: it only matters (exit 2) if a skip later needs the verdict.
probed=0
available_parallelism=unknown
concurrency_source=node-default
if [ -n "${UZI_AGENT_TEST_CONCURRENCY:-}" ]; then concurrency_source=caller-override; fi
if command -v node > /dev/null 2>&1; then
  run_probe
  if [ "$probe" -eq 3 ]; then
    case "${UZI_AGENT_TEST_CONCURRENCY:-}" in
      '')
        UZI_AGENT_TEST_CONCURRENCY=1
        export UZI_AGENT_TEST_CONCURRENCY
        concurrency_source=procfs-denied-cap
        echo "real-procfs-skip-check: $probe_dir is not enumerable; node may over-report availableParallelism without its cgroup CPU quota, so running agent test files serially (UZI_AGENT_TEST_CONCURRENCY=1) rather than at the host CPU count"
        ;;
      *[!0-9]* | 0*)
        echo "real-procfs-skip-check: warning: UZI_AGENT_TEST_CONCURRENCY='$UZI_AGENT_TEST_CONCURRENCY' is not a positive integer; $probe_dir is not enumerable, so using UZI_AGENT_TEST_CONCURRENCY=1" >&2
        UZI_AGENT_TEST_CONCURRENCY=1
        export UZI_AGENT_TEST_CONCURRENCY
        concurrency_source=procfs-denied-cap
        ;;
    esac
  fi
  available_parallelism="$(node -p 'require("node:os").availableParallelism()' 2>/dev/null)" || available_parallelism=unknown
  if [ "${UZI_AGENT_TEST_CONCURRENCY+x}" != x ] && [ "$available_parallelism" = 2 ]; then
    UZI_AGENT_TEST_CONCURRENCY=2
    export UZI_AGENT_TEST_CONCURRENCY
    concurrency_source=floor
  fi
  # shellcheck disable=SC2016 # JavaScript template expressions, not shell variables
  node -e '
const fs = require("node:fs");
const [parallelism, override, source] = process.argv.slice(1);
const count = Number(parallelism);
const fallback = Number.isSafeInteger(count) && count > 0 ? Math.max(count - 1, 1) : "unknown";
const concurrency = override ? (/^[1-9][0-9]*$/.test(override) ? override : "invalid") : fallback;
let quota = "unreadable";
try { quota = fs.readFileSync("/sys/fs/cgroup/cpu.max", "utf8").trim().replace(/\s+/g, " "); } catch {}
console.log(`agent-tests: node=${process.versions.node} libuv=${process.versions.uv} availableParallelism=${parallelism} cpu.max=${quota} --test-concurrency=${concurrency} source=${source}`);
' -- "$available_parallelism" "${UZI_AGENT_TEST_CONCURRENCY:-}" "$concurrency_source" ||
    echo "agent-tests: runtime diagnostics unavailable" >&2
else
  echo "agent-tests: node unavailable; runtime diagnostics and concurrency selection unavailable" >&2
fi

rc=0
unit_started_at="$(date +%s)"
UZI_REAL_PROCFS_SKIP_LOG="$log" "$@" || rc=$?
printf 'agent-stage: unit duration_seconds=%s exit=%s\n' "$(($(date +%s) - unit_started_at))" "$rc"

# Fail closed when the log is gone (the command deleted it): the skip count is unknowable, and
# counting a missing file as 0 would pass a run whose skips were never seen. A non-zero command
# status still wins.
if [ ! -f "$log" ]; then
  echo "real-procfs-skip-check: the skip log $log is missing (the command removed it); cannot count real-procfs skips" >&2
  if [ "$rc" -ne 0 ]; then exit "$rc"; fi
  exit 1
fi
# One record per line: `label<TAB>reason`.
skips="$(wc -l < "$log" | tr -d ' ')"
case "$skips" in
  '' | *[!0-9]*)
    echo "real-procfs-skip-check: cannot count the skip log $log (got '$skips')" >&2
    if [ "$rc" -ne 0 ]; then exit "$rc"; fi
    exit 1
    ;;
esac
if [ "$skips" -gt 0 ]; then
  tab="$(printf '\t')"
  while IFS="$tab" read -r label reason; do
    printf 'real-procfs skip: %s — %s\n' "$label" "$reason"
  done < "$log"
fi

if [ "$rc" -ne 0 ]; then
  echo "real-procfs-skip-check: the command failed (exit $rc); real-procfs skips: $skips" >&2
  exit "$rc"
fi

if [ "$skips" -eq 0 ]; then
  echo "real-procfs skips: 0"
  exit 0
fi

if [ -n "${CI:-}" ]; then
  echo "real-procfs-skip-check: $skips real-procfs test(s) skipped in CI; real-procfs tests must run in CI (CI=$CI)" >&2
  exit 1
fi

if [ "$probed" -eq 0 ]; then
  command -v node > /dev/null 2>&1 || {
    echo "real-procfs-skip-check: node is required to probe $probe_dir" >&2
    exit 2
  }
  run_probe
fi
if [ -n "$probe_msg" ]; then echo "$probe_msg" >&2; fi

case "$probe" in
  0)
    echo "real-procfs-skip-check: coverage vanished: $skips real-procfs test(s) skipped on a host where the proc root ($probe_dir) is enumerable" >&2
    exit 1
    ;;
  3)
    echo "================================================================================"
    echo "real-procfs-skip-check: $skips real-procfs test(s) SKIPPED: enumerating $probe_dir"
    echo "is denied here (EACCES/EPERM, e.g. the Landlock command sandbox). They did NOT run;"
    echo "they run, and must pass, on CI and on any host where the proc root enumerates."
    echo "================================================================================"
    exit 0
    ;;
  *)
    echo "real-procfs-skip-check: $skips real-procfs test(s) skipped, but probing $probe_dir failed with something other than EACCES/EPERM (probe exit $probe)" >&2
    exit 1
    ;;
esac
