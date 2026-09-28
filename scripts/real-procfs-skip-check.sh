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
# Usage: scripts/real-procfs-skip-check.sh <command> [args...]
#   UZI_REAL_PROCFS_PROBE_DIR  the directory to probe (default: /proc); for this script's
#                              own test (scripts/real-procfs-skip-check.test.sh) only.
#
# EXIT CODES:
#   the command's own non-zero status, when it failed (the skip list still prints)
#   1 = the command passed but real-procfs tests skipped where they must run: in CI (CI
#       non-empty), on a host whose proc root enumerates, or where the probe hit an error
#       other than EACCES/EPERM
#   2 = usage / could not create the skip log / node missing for the probe
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

rc=0
UZI_REAL_PROCFS_SKIP_LOG="$log" "$@" || rc=$?

# One record per line: `label<TAB>reason`.
skips="$(wc -l < "$log" | tr -d ' ')"
if [ "$skips" -gt 0 ]; then
  tab="$(printf '\t')"
  while IFS="$tab" read -r label reason; do
    echo "real-procfs skip: $label — $reason"
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

probe_dir="${UZI_REAL_PROCFS_PROBE_DIR:-/proc}"
command -v node > /dev/null 2>&1 || {
  echo "real-procfs-skip-check: node is required to probe $probe_dir" >&2
  exit 2
}
# 0 = enumerable, 3 = denied (EACCES/EPERM), 4 = any other error.
probe=0
# shellcheck disable=SC2016 # the single-quoted program is JavaScript; its ${} is a JS template
node -e '
const fs = require("node:fs");
try { fs.readdirSync(process.argv[1]); process.exit(0); }
catch (err) {
  const code = err && err.code;
  console.error(`probe: readdir ${process.argv[1]}: ${code || err}`);
  process.exit(code === "EACCES" || code === "EPERM" ? 3 : 4);
}' -- "$probe_dir" || probe=$?

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
