#!/bin/sh
# Gate loaded agent guidance on per-file BYTE BUDGETS (a size ratchet, issue #1630).
#
# usage: scripts/check-guidance-size.sh <canary-file> <budgets-file>
#   e.g. scripts/check-guidance-size.sh scripts/guidance-size-canary.txt scripts/guidance-size-budgets.tsv
#
# WHY THIS EXISTS. A worker run loads only the root CLAUDE.md (= AGENTS.md) into context,
# via agent/src/repo-instructions.ts; .claude/rules/*.md is never loaded by a worker run
# (settingSources is empty). The rules files load into interactive Claude Code sessions on
# this repo, when a matching file is read, so they are budgeted too.
# AGENTS.md is also subject to a hard runtime cap: a repo-instructions file strictly larger
# than REPO_INSTRUCTIONS_MAX_BYTES (64 * 1024, agent/src/repo-instructions.ts) is DROPPED
# SILENTLY ("too_large"). Growth toward that cap is invisible until the drop. Each file gets
# a budget in the budgets file; growth past it reddens CI, and raising a budget is a
# reviewed edit to that file.
#
# BUDGETS FILE. Tab-separated `path<TAB>bytes`; blank lines and `#` comments are allowed.
# Every in-scope file needs exactly one row.
#
# SCOPE. AGENTS.md plus `git ls-files -- '.claude/rules/*.md'` (that pathspec also matches
# nested subdirectories under .claude/rules/). CLAUDE.md is a symlink to
# AGENTS.md and is never enumerated, so the content counts once. Excluded on purpose:
# .claude/agents/*.md (upstream-synced roster), .claude/agent-team.md (generated manifest),
# skills (check-skill-size.sh), ARCHITECTURE.md and docs/ (not loaded as run guidance).
#
# SIZE PREDICATE. `wc -c < file` compared with `-gt`, like the runtime's strictly-greater
# test: a file exactly at its budget is accepted. A row for AGENTS.md above 65536 is itself
# a finding. Keep 65536 in sync with REPO_INSTRUCTIONS_MAX_BYTES.
#
# CANARY. The same over() predicate is run FIRST on the canary file against a threshold one
# byte below its own size and must fire, or the instrument is broken (exit 2), so a clean
# result is never a detector that measured nothing.
#
# FINDINGS (exit 1, all collected and printed together): a file over budget; an unbudgeted
# in-scope file; a stale row (file missing); a malformed or duplicate row; a row naming an
# out-of-scope path (for example CLAUDE.md, which would double count); an AGENTS.md budget
# above the runtime cap; an in-scope path that is a symlink or not a regular file.
#
# EXIT CODES: 2 = instrument broken or bad usage (missing canary or budgets file, empty
# canary, canary predicate silent, nothing in scope); 1 = findings; 0 = clean.
# `task`'s own rc is 201 for all of them.
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: scripts/check-guidance-size.sh <canary-file> <budgets-file>" >&2
  exit 2
fi

CANARY="$1"
BUDGETS="$2"

# keep in sync with REPO_INSTRUCTIONS_MAX_BYTES (agent/src/repo-instructions.ts)
CAP=65536

ROOT="$(git rev-parse --show-toplevel)" || {
  echo "check-guidance-size: not inside a git work tree" >&2
  exit 2
}
cd "$ROOT" || {
  echo "check-guidance-size: cannot cd to repo root: $ROOT" >&2
  exit 2
}

over() {
  [ "$(wc -c < "$1")" -gt "$2" ]
}

if [ ! -f "$CANARY" ]; then
  echo "check-guidance-size: canary file not found: $CANARY" >&2
  exit 2
fi
canary_bytes="$(wc -c < "$CANARY")"
if [ "$canary_bytes" -lt 1 ]; then
  echo "check-guidance-size: INSTRUMENT BROKEN -- canary $CANARY is empty" >&2
  exit 2
fi
if ! over "$CANARY" "$((canary_bytes - 1))"; then
  echo "check-guidance-size: INSTRUMENT BROKEN -- the size predicate did not fire on the" >&2
  echo "check-guidance-size: canary ($CANARY, $canary_bytes bytes) against $((canary_bytes - 1))." >&2
  exit 2
fi
if [ ! -f "$BUDGETS" ]; then
  echo "check-guidance-size: budgets file not found: $BUDGETS" >&2
  exit 2
fi

TAB="$(printf '\t')"
NL='
'
findings=""
add() {
  findings="${findings}  $1
"
}

# In-scope paths, one per line: AGENTS.md first, then the tracked rules files.
scope="AGENTS.md
$(git ls-files -- '.claude/rules/*.md')"

present=0
for p in $scope; do
  if [ -e "$p" ] || [ -L "$p" ]; then
    present=$((present + 1))
  fi
done
if [ "$present" -eq 0 ]; then
  echo "check-guidance-size: nothing in scope (no AGENTS.md, no tracked .claude/rules/*.md)" >&2
  exit 2
fi

# Parse the budgets file into validated "path<TAB>bytes" rows.
rows=""
seen="$NL"
while IFS="$TAB" read -r bpath bbytes extra || [ -n "$bpath" ]; do
  case "$bpath" in
    '' | '#'*) continue ;;
  esac
  # More than 9 digits is malformed: a huge value overflows the shell's integer tests
  # ([ -gt ] errors, which is false inside if/&&) and the run would print clean.
  case "$bbytes" in
    '' | *[!0-9]* | ??????????*)
      add "malformed row (need path<TAB>integer bytes): $bpath"
      continue
      ;;
  esac
  if [ -n "$extra" ]; then
    add "malformed row (extra fields): $bpath"
    continue
  fi
  case "$seen" in
    *"$NL$bpath$NL"*)
      add "duplicate row: $bpath"
      continue
      ;;
  esac
  seen="${seen}${bpath}${NL}"
  rows="${rows}${bpath}${TAB}${bbytes}${NL}"
  case "$scope" in
    "$bpath" | "$bpath$NL"* | *"$NL$bpath" | *"$NL$bpath$NL"*) ;;
    *) add "row for out-of-scope path (only AGENTS.md and tracked .claude/rules/*.md are budgeted; CLAUDE.md is a symlink counted via AGENTS.md): $bpath" ;;
  esac
  if [ "$bpath" = "AGENTS.md" ] && [ "$bbytes" -gt "$CAP" ]; then
    add "AGENTS.md budget $bbytes exceeds the $CAP-byte runtime cap (REPO_INSTRUCTIONS_MAX_BYTES)"
  fi
done < "$BUDGETS"

budget_of() {
  printf '%s' "$rows" | awk -F "$TAB" -v p="$1" '$1 == p { print $2; exit }'
}

scanned=0
for p in $scope; do
  if [ ! -e "$p" ] && [ ! -L "$p" ]; then
    if [ -n "$(budget_of "$p")" ]; then
      add "stale row, file missing: $p"
    else
      add "in-scope file missing: $p"
    fi
    continue
  fi
  if [ -L "$p" ] || [ ! -f "$p" ]; then
    add "in-scope path is a symlink or not a regular file: $p"
    continue
  fi
  scanned=$((scanned + 1))
  budget="$(budget_of "$p")"
  # Measure once, without a pipe: in sh a pipeline's status is its last command's, so a
  # failed read would surface as an empty size and the comparison below would read clean.
  if ! size="$(wc -c < "$p")"; then
    echo "check-guidance-size: INSTRUMENT BROKEN -- cannot read in-scope file: $p" >&2
    exit 2
  fi
  size="$(printf '%s' "$size" | tr -d ' ')"
  case "$size" in
    '' | *[!0-9]*)
      echo "check-guidance-size: INSTRUMENT BROKEN -- non-numeric size '$size' for $p" >&2
      exit 2
      ;;
  esac
  if [ -z "$budget" ]; then
    echo "$p $size/none"
    add "no budget row for in-scope file: $p ($size bytes)"
    continue
  fi
  echo "$p $size/$budget"
  if [ "$size" -gt "$budget" ]; then
    add "over budget: $p is $size bytes, budget $budget"
  fi
done

if [ -n "$findings" ]; then
  echo "check-guidance-size: findings:" >&2
  printf '%s' "$findings" >&2
  echo "  Trim the file, or raise its row in $BUDGETS as a reviewed change." >&2
  exit 1
fi

echo "check-guidance-size: clean -- $scanned file(s) within budget."
echo "check-guidance-size: canary DETECTED in $CANARY ($canary_bytes bytes registered as over $((canary_bytes - 1)))"
exit 0
