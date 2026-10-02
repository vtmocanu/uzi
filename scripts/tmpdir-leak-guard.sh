#!/bin/sh
# Run a command with TMPDIR pointed at a fresh, empty scratch directory, then fail
# if the command left anything behind in it (PRD #1809 M2).
#
# WHY: every test helper that calls os.tmpdir()/mkdtemp and forgets its cleanup
# leaks into the runner's scratch dir, and on a uzi worker that scratch dir is the
# data volume a run's disk-safety controls are measuring. A leak is invisible in a
# green test run, so the only standing control is to look at the directory
# afterwards. `task test:agent` wraps `npm test` with this.
#
# Two entries are tolerated, both tool caches created on every run regardless of test
# hygiene: tsx's compile cache `tsx-<uid>` (tsx 4.x FileCache, under os.tmpdir()), and
# `node-compile-cache`, which npm's own CLI creates by calling
# module.enableCompileCache() at startup (npm 11, lib/cli.js), so `npm test` makes it
# before any test runs. Nothing else is allowed.
#
# Every leftover is reported with evidence (issue #2020: a leftover `uzi-agent-test-*`
# dir was seen with 0 failing tests and nothing but its bare name to go on). The guard
# creates a LEDGER file outside the scratch dir and exports its path to the command as
# UZI_TMPDIR_GUARD_LEDGER; agent/test/fixture-repo.ts appends one JSON line per fixture
# created and per fixture removed. For each leftover the guard prints the ledger lines
# naming it (`"entry":"<name>"`), a verdict, and a bounded listing of its contents:
#   cleanup ran; the directory was recreated afterwards   (a removed line exists)
#   no successful cleanup recorded                        (created line(s) only; ledger
#     writes are best-effort, so this does not prove cleanup never ran)
#   no creator recorded in the ledger                     (no line names it)
# This is evidence only: the exit codes below do not depend on the ledger.
#
# Usage: scripts/tmpdir-leak-guard.sh <command> [args...]
#
# EXIT CODES:
#   the command's own non-zero status, when it failed (the leak report still
#     prints, and the scratch dir is still removed)
#   1 = the command passed but left something in the scratch dir
#   2 = usage / could not create the scratch dir or the ledger file
#   0 = the command passed and the scratch dir holds nothing but the two tool caches
# POSIX sh on purpose: runs under busybox sh, dash and macOS /bin/sh alike.
set -u

if [ "$#" -eq 0 ]; then
  echo "tmpdir-leak-guard: usage: $0 <command> [args...]" >&2
  exit 2
fi

# Portable template form (check:mktemp-portability): a full path with 6 X's, no -t.
scratch="$(mktemp -d "${TMPDIR:-/tmp}/uzi-tmpdir-guard.XXXXXX")" || {
  echo "tmpdir-leak-guard: cannot create a scratch dir" >&2
  exit 2
}

# Outside the scratch dir, so it is never itself reported as a leftover.
ledger="$(mktemp "${TMPDIR:-/tmp}/uzi-tmpdir-guard-ledger.XXXXXX")" || {
  rm -rf "$scratch"
  echo "tmpdir-leak-guard: cannot create a ledger file" >&2
  exit 2
}

# shellcheck disable=SC2329 # invoked by the EXIT trap below
cleanup() {
  rm -f "$ledger"
  # Tests may leave read-only trees (git objects, chmod'ed fixtures): make them
  # writable first so the removal cannot fail on permissions.
  chmod -R u+rwx "$scratch" 2>/dev/null
  rm -rf "$scratch"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

rc=0
TMPDIR="$scratch" UZI_TMPDIR_GUARD_LEDGER="$ledger" "$@" || rc=$?

tsx_cache="tsx-$(id -u)"
leftover=0
for entry in "$scratch"/* "$scratch"/.[!.]* "$scratch"/..?*; do
  # An unmatched glob stays literal; skip it unless something by that name exists.
  [ -e "$entry" ] || [ -L "$entry" ] || continue
  name="${entry##*/}"
  case "$name" in
    "$tsx_cache" | node-compile-cache) continue ;;
  esac
  if [ "$leftover" -eq 0 ]; then
    echo "tmpdir-leak-guard: the command left these entries in its TMPDIR ($scratch):" >&2
  fi
  leftover=$((leftover + 1))
  # printf, not echo: dash and macOS sh echo interpret backslashes in untrusted text.
  printf '  %s\n' "$name" >&2
  matched="$(grep -F "\"entry\":\"$name\"" "$ledger" 2>/dev/null)"
  if [ -n "$matched" ]; then
    printf '%s\n' "$matched" | while IFS= read -r line; do
      printf '    ledger: %s\n' "$line" >&2
    done
    case "$matched" in
      *'"event":"removed"'*) echo "    verdict: cleanup ran; the directory was recreated afterwards" >&2 ;;
      *) echo "    verdict: no successful cleanup recorded" >&2 ;;
    esac
  else
    echo "    verdict: no creator recorded in the ledger" >&2
  fi
  echo "    contents (bounded):" >&2
  find "$entry" -maxdepth 4 2>/dev/null | head -n 40 | while IFS= read -r p; do
    printf '      %s\n' "${p#"$scratch"/}" >&2
  done
done

if [ "$leftover" -gt 0 ]; then
  echo "tmpdir-leak-guard: $leftover leftover entr$( [ "$leftover" -eq 1 ] && echo y || echo ies); a test is missing its temp-dir cleanup" >&2
fi

if [ "$rc" -ne 0 ]; then
  exit "$rc"
fi
if [ "$leftover" -gt 0 ]; then
  exit 1
fi
exit 0
