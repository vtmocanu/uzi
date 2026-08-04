#!/bin/sh
# Print the one-line statement-coverage total for a Go module's profile, and refuse
# to print one that came from a run that measured nothing (PRD #103 M6).
#
# WHY A SCRIPT AND NOT A `cmds:` LINE. Two reasons, both of which this repo has
# already paid for once:
#
#   * `go tool cover -func=coverage.out | tail -1` puts the status on `tail`, which
#     succeeds at printing anything -- the exact shape of the three gate-status
#     reports that failed in the reassuring direction on 2026-07-28. `set -o
#     pipefail` is not an option: Task runs `sh -c`, and /bin/sh in the pinned
#     `golang:1.26` CI image is dash, which does not have it. So the rc is read on
#     the line after the command and no pipe carries a verdict.
#   * a committed script gets LINTED by `task lint:shell`; an inline `cmds:` string
#     gets nothing. `deadcode-gate.sh` and `shellcheck-tracked.sh` set that precedent.
#
# 🔴 THE EMPTINESS GUARD IS AN INSTRUMENT CHECK, NOT A COVERAGE THRESHOLD, and the
# distinction is load-bearing because PRD #103 Decision 6 forbids a threshold in this
# milestone. A profile from a run in which no test executed is not 0% coverage -- it
# is a one-line file holding just `mode: atomic`, and `go tool cover -func` reports
# `total: (statements) 0.0%` for it with rc=0. That reads exactly like a real,
# terrible coverage number. This refuses to report it. It never compares the
# PERCENTAGE against anything; the only thing it asserts is that the profile has at
# least one measured block.
#
# Exit codes follow the convention fmt-check:api and deadcode-gate.sh already set,
# because `task`'s own rc is 201 for all of them and this is the only place the
# distinction can live:
#   2 = the instrument is broken (no profile, empty profile, `go tool cover` failed)
#   0 = a total was produced
# There is deliberately no `1`: this script has no findings tier to report.
set -eu

PROFILE="${1:?usage: coverage-total.sh <coverage profile>}"

# 🔴 `go tool cover -func` RESOLVES THE PROFILE'S PACKAGE PATHS AGAINST THE MODULE IN
# THE CURRENT DIRECTORY, so it dies with `go.mod file not found in current directory
# or any parent directory` when called from anywhere but the module root. Found by the
# CONTROL arm of the calibration (probes/prd-103-mrc-m6-coder/b13-coverage-calibration.txt),
# not by the two failure arms: both rc=2 arms passed while the control -- the same
# script on a REAL profile, from the repo root -- also returned 2, which would have
# made this script look like it worked and made the guard above look proven. The
# Taskfile calls it with `dir: api`/`dir: controller` so it would have shipped
# working, with a hidden cwd dependency and a calibration that had proved nothing.
#
# So: make the cwd a property of the ARGUMENT rather than of the caller. The profile
# lives in its module root by convention, so its directory is the right module.
PROFILE_DIR=$(dirname "$PROFILE")
PROFILE_BASE=$(basename "$PROFILE")
cd "$PROFILE_DIR" || exit 2
PROFILE="$PROFILE_BASE"

if [ ! -s "$PROFILE" ]; then
  echo "coverage-total: $PROFILE is missing or empty -- the test run wrote no profile" >&2
  exit 2
fi

# A valid profile is `mode: <set|count|atomic>` followed by one line per block.
# ONE line means the header alone: every package was skipped, cached, or failed to
# build. Counting lines rather than trusting `go tool cover`, which is happy to
# summarise nothing.
BLOCKS=$(($(wc -l < "$PROFILE") - 1))
if [ "$BLOCKS" -lt 1 ]; then
  echo "coverage-total: $PROFILE holds 0 coverage blocks (header only) -- nothing was measured" >&2
  exit 2
fi

# No pipe carries a verdict here, and the failure branch is an `if !` rather than a
# `$?` read on the next line: under `set -e` a failing simple command exits the script
# BEFORE any `rc=$?` line runs, so the obvious form would make that check dead code.
# In a condition context `set -e` is suppressed, which is the whole reason this shape
# is used. The full per-function listing is thousands of lines and would bury the test
# output the gate exists to make readable, so it goes to a temp file and only the
# total line is printed.
FUNCS=$(mktemp) || exit 2
trap 'rm -f "$FUNCS"' EXIT
if ! go tool cover -func="$PROFILE" -o "$FUNCS"; then
  echo "coverage-total: go tool cover -func failed" >&2
  exit 2
fi

# The `total:` line is what .gitlab-ci.yml's `coverage:` regex reads. Its shape is
# `total:\t(statements)\t75.4%` -- tab-separated, which is why the regex there does
# not assume spaces.
TOTAL=$(grep '^total:' "$FUNCS") || {
  echo "coverage-total: no 'total:' line in go tool cover output" >&2
  exit 2
}
echo "$TOTAL"
echo "coverage-total: $BLOCKS blocks measured in $PROFILE"
