#!/bin/sh
# Gate one npm package on `npm audit` (PRD #103 M5 MR-C).
#
# 🔴 THIS WRAPPER EXISTS BECAUSE npm audit's rc=1 IS AMBIGUOUS AND ONE BRANCH OF
# IT IS A NETWORK FAILURE. Measured 2026-08-04, npm 11.17:
#
#     findings at or above the level     rc=1
#     registry unreachable               rc=1   + "npm error audit endpoint returned an error"
#
# Same code, and `--json` does not separate them either. It fails CLOSED, which is
# right, but it collapses exactly the two states the Go half of this milestone is
# carefully engineered to keep apart. This repo's convention is
#
#     2 = the instrument is broken     1 = there are findings     0 = clean
#
# so the network branch is detected in the OUTPUT and reported as 2.
#
# 🔴 THE FLAGS ARE THE GATE, AND ONE OF THEM CLOSES A DISARM THAT EXITS 0.
# A one-file `.npmrc` carrying `omit=dev` takes `npm audit` on `web` from rc=1 to
# rc=0, printing "found 0 vulnerabilities" -- because all three of web's
# high-and-above findings are dev-tree. `--audit-level` on the command line DOES
# beat `.npmrc` for its own key, but `omit` is a DIFFERENT key, so nothing on the
# command line contradicts it. Measured:
#
#     no .npmrc                  rc=1   <- armed
#     .npmrc: omit=dev           rc=0   <- DISARMED, silently
#     .npmrc: audit-level=none   rc=1   <- CLI --audit-level wins for THAT key
#     .npmrc: omit=dev + --include=dev on the command line   rc=1   <- re-armed
#
# So `--include=dev` is not belt-and-braces, it is the remedy. And unlike the
# gitleaks scan this milestone shipped earlier, npm audit HAS NO IN-BAND CANARY:
# `metadata.dependencies` is byte-identical armed and disarmed, so nothing in a
# disarmed run's own output distinguishes it from a clean one. `web/.npmrc` and
# `agent/.npmrc` therefore belong on the list of gate-config files reviewers
# watch; there is none in this repo today, at any level.
#
# THE LEVEL IS AN ARGUMENT, not a default inside this file, so `task`'s echo shows
# it -- the same reason `lint:shell` passes its severity and `scan:secrets` passes
# its version. For this check the threshold IS the gate.
#
# WHY `high` AND NOT ZERO. User ruling 1 is "the full tree, fixed first, at zero",
# and "at zero" means high-and-above at zero: two MODERATE advisories survive every
# option on `web` (`react-router` and `react-router-dom`, GHSA-wrjc-x8rr-h8h6 and
# GHSA-337j-9hxr-rhxg). Both are patched only at 7.18.0, the installed 6.30.4 is
# the newest 6.x that exists, the whole 6.x line sits inside the advisory range,
# `overrides` has nothing patched to point at, and `npm audit fix --force`
# emits no react-router entry at all. Clearing them is a React Router 6 -> 7 major
# through shipped SPA routing code, which is filed as its own issue rather than
# pulled into a quality-gates milestone.
#
# NOT DELEGATED TO A `package.json` SCRIPT THE WAY EVERY OTHER npm TARGET IS, AND
# THAT IS DELIBERATE: it IS delegated, but through this file rather than around it.
# `package.json`'s `audit` script carries the flags (so the Taskfile header's rule
# holds and a rewrite cannot drop them silently), and this script exists solely for
# the exit-code mapping, which a package.json script cannot express. A committed
# script also gets linted by `lint:shell`, where an inline `cmds:` string gets
# nothing.
set -eu

PKG_DIR="${1:-}"
LEVEL="${2:-}"

if [ -z "$PKG_DIR" ] || [ -z "$LEVEL" ]; then
  echo "usage: scripts/npm-audit-gate.sh <package-dir> <audit-level>" >&2
  echo "  e.g. scripts/npm-audit-gate.sh web high" >&2
  exit 2
fi

if [ ! -f "$PKG_DIR/package.json" ]; then
  echo "npm-audit-gate: no package.json in: $PKG_DIR" >&2
  exit 2
fi

# The level is asserted against what package.json's `audit` script actually
# encodes, rather than being passed to npm from here. Without this the argument
# would be decorative: the script would keep using its own baked-in level while
# `task`'s echo advertised whatever was typed on the Taskfile line. A mismatch is
# an instrument failure, not a finding.
if ! grep -q -F -- "--audit-level=$LEVEL" "$PKG_DIR/package.json"; then
  echo "npm-audit-gate: $PKG_DIR/package.json's audit script does not carry --audit-level=$LEVEL." >&2
  echo "  The Taskfile line and the script must agree, or this file's echo advertises" >&2
  echo "  a threshold that is not the one being enforced." >&2
  exit 2
fi

# No `-t` on mktemp: it is not portable, and only CI could show it (f0e3c438).
TMP="$(mktemp -d)"
# shellcheck disable=SC2064  # expand TMP now: the trap must survive its unset.
trap "rm -rf '$TMP'" EXIT INT TERM

# 🔴 RC FIRST, AND INTO A FILE. Redirect and read `$?` on the very next line -- do
# not pipe into grep and read the pipeline's status, and do not feed a shell
# builtin's multi-line output into an early-exiting reader: `printf '%s' "$BIG" |
# grep -q` returns 141 on SIGPIPE while the pattern is present, which is measured
# in this repo (scripts/assert-changelog-covers-release.sh) and blocked a release.
# A file has no writer process, so it cannot SIGPIPE, in any shell.
#
# 🔴 NO `--silent`, AND THAT IS THE WHOLE DISCRIMINATOR. `npm run --silent audit`
# is the tidier-looking form and it DESTROYS this script's only signal. Measured
# 2026-08-04, npm 11.17, against an unreachable registry, all three forms rc=1:
#
#   npm audit ...                  npm warn audit request ... ECONNREFUSED
#                                  undefined
#                                  npm error audit endpoint returned an error
#   npm run audit                  identical, plus the script banner
#   npm run --silent audit         undefined            <- AND NOTHING ELSE
#
# `--silent` suppresses BOTH the warn and the error line, so a registry outage
# arrives as a one-word output and rc=1 -- indistinguishable, from the output, from
# a tree full of critical advisories. This script shipped with `--silent` for
# exactly one calibration run, which reported a network failure as
# "web has advisories at or above 'high'". Do not put it back.
rc=0
(cd "$PKG_DIR" && npm run audit) >"$TMP/out" 2>&1 || rc=$?

cat "$TMP/out"

if [ "$rc" -eq 0 ]; then
  echo "npm-audit-gate: $PKG_DIR clean at --audit-level=$LEVEL"
  exit 0
fi

# THE NETWORK BRANCH. npm's own wording for an unreachable registry is
# "audit endpoint returned an error"; the ENOTFOUND/ECONNREFUSED/ETIMEDOUT codes
# cover the DNS and connection cases that never reach the endpoint at all. Any of
# them means the verdict is about the network, not about the tree.
if grep -q -F -e 'audit endpoint returned an error' \
             -e 'ENOTFOUND' -e 'ECONNREFUSED' -e 'ETIMEDOUT' -e 'ERR_SOCKET_TIMEOUT' \
             "$TMP/out"; then
  echo "npm-audit-gate: $PKG_DIR -- npm audit could not reach the registry." >&2
  echo "  INSTRUMENT FAILURE, NOT FINDINGS. npm audit returns 1 for both, so this" >&2
  echo "  distinction cannot live in its exit code and lives here instead." >&2
  exit 2
fi

# 🔴 THE FINDINGS BRANCH NEEDS A POSITIVE OBSERVATION, NOT MERELY THE ABSENCE OF
# THE NETWORK ONE. The grep above is a NEGATIVE test, and a negative test cannot
# tell "no network error" from "npm failed in a way this script has never seen" --
# which is precisely how the --silent bug above reported an outage as advisories.
# So classifying as FINDINGS requires npm's own report header to be present. Any
# other nonzero exit is an instrument failure by default, because a run that
# produced no report has not measured the tree.
if ! grep -q -F 'npm audit report' "$TMP/out"; then
  echo "npm-audit-gate: $PKG_DIR -- npm exited $rc without printing an audit report." >&2
  echo "  INSTRUMENT FAILURE, NOT FINDINGS. The output above is the whole of what" >&2
  echo "  npm produced; nothing in it is a verdict about this tree." >&2
  exit 2
fi

echo "npm-audit-gate: $PKG_DIR has advisories at or above '$LEVEL' (gate exit 1)." >&2
echo "  Fix them in this MR. Run 'cd $PKG_DIR && npm audit --json' for the package" >&2
echo "  names -- the human-readable output leaves transitive findings unlabelled," >&2
echo "  so a fix list built by reading it omits them." >&2
exit 1
