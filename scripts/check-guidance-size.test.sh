#!/bin/sh
# Hermetic contract tests for scripts/check-guidance-size.sh: each case builds a throwaway
# git repo and proves the exit code (0 clean, 1 findings, 2 broken instrument) plus a key
# substring of the output, so a gutted check cannot read green.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
SUT="$ROOT/scripts/check-guidance-size.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  echo "check-guidance-size.test.sh: FAIL: $*" >&2
  exit 1
}

CASES=0

# mk <case>: fixture repo with AGENTS.md (100 bytes), CLAUDE.md symlink, one rules file
# (50 bytes), a good canary and budgets "AGENTS.md 100 / rules/a.md 50". Sets D.
mk() {
  D="$TMP/$1"
  mkdir -p "$D/.claude/rules"
  git init -q "$D"
  head -c 100 /dev/zero | tr '\0' 'a' > "$D/AGENTS.md"
  head -c 50 /dev/zero | tr '\0' 'b' > "$D/.claude/rules/a.md"
  ln -s AGENTS.md "$D/CLAUDE.md"
  printf 'canary text\n' > "$D/canary.txt"
  printf '# header\n\nAGENTS.md\t100\n.claude/rules/a.md\t50\n' > "$D/budgets.tsv"
}

stage() {
  [ -z "${NOSTAGE:-}" ] || return 0
  (cd "$D" && git add -A)
}

# run <case> <rc> <substring> [<canary> [<budgets>]]
run() {
  stage
  rc=0
  (cd "$D" && "$SUT" "${4:-canary.txt}" "${5:-budgets.tsv}") > "$D.out" 2>&1 || rc=$?
  [ "$rc" = "$2" ] || fail "$1: exit $rc, want $2 ($(cat "$D.out"))"
  grep -qF -- "$3" "$D.out" || fail "$1: output lacks '$3' ($(cat "$D.out"))"
  CASES=$((CASES + 1))
}

mk atbudget
run atbudget 0 "AGENTS.md 100/100"

mk over
printf '# h\nAGENTS.md\t99\n.claude/rules/a.md\t50\n' > "$D/budgets.tsv"
run over 1 "over budget: AGENTS.md is 100 bytes, budget 99"

mk unbudgeted
printf 'x' > "$D/.claude/rules/b.md"
run unbudgeted 1 "no budget row for in-scope file: .claude/rules/b.md"

mk stale
printf 'AGENTS.md\t100\n.claude/rules/a.md\t50\n.claude/rules/gone.md\t10\n' > "$D/budgets.tsv"
run stale 1 "out-of-scope path"

mk stalegone
printf 'AGENTS.md\t100\n.claude/rules/a.md\t50\n' > "$D/budgets.tsv"
stage
rm "$D/.claude/rules/a.md"
NOSTAGE=1
run stalegone 1 "stale row, file missing: .claude/rules/a.md"
NOSTAGE=

mk cap
printf 'AGENTS.md\t65537\n.claude/rules/a.md\t50\n' > "$D/budgets.tsv"
run cap 1 "exceeds the 65536-byte runtime cap"

mk capok
printf 'AGENTS.md\t65536\n.claude/rules/a.md\t50\n' > "$D/budgets.tsv"
run capok 0 "AGENTS.md 100/65536"

mk symlink
run symlink 0 "AGENTS.md 100/100"
if grep -qF "CLAUDE.md" "$D.out"; then fail "symlink: CLAUDE.md appears in output ($(cat "$D.out"))"; fi

mk claudeRow
printf 'AGENTS.md\t100\n.claude/rules/a.md\t50\nCLAUDE.md\t100\n' > "$D/budgets.tsv"
run claudeRow 1 "out-of-scope path"

mk malformed
printf 'AGENTS.md\t100\n.claude/rules/a.md\tfifty\n' > "$D/budgets.tsv"
run malformed 1 "malformed row"

mk bigbudget
printf 'AGENTS.md\t99999999999999999999\n.claude/rules/a.md\t50\n' > "$D/budgets.tsv"
run bigbudget 1 "malformed row"

mk notab
printf 'AGENTS.md 100\n.claude/rules/a.md\t50\n' > "$D/budgets.tsv"
run notab 1 "malformed row"

mk dup
printf 'AGENTS.md\t100\nAGENTS.md\t100\n.claude/rules/a.md\t50\n' > "$D/budgets.tsv"
run dup 1 "duplicate row: AGENTS.md"

mk symrule
rm "$D/.claude/rules/a.md"
ln -s ../../AGENTS.md "$D/.claude/rules/a.md"
run symrule 1 "symlink or not a regular file"

mk emptycanary
: > "$D/canary.txt"
run emptycanary 2 "is empty"

mk nocanary
run nocanary 2 "canary file not found" missing.txt

mk nobudgets
run nobudgets 2 "budgets file not found" canary.txt missing.tsv

# An unreadable in-scope file must fail closed, never measure as empty and read clean.
# Root reads through mode 000, so the case is meaningless there and is skipped.
mk unreadable
stage
chmod 000 "$D/AGENTS.md"
if [ -r "$D/AGENTS.md" ]; then
  echo "check-guidance-size.test.sh: skip unreadable (running as root)"
else
  NOSTAGE=1 run unreadable 2 "cannot read in-scope file: AGENTS.md"
fi
chmod 644 "$D/AGENTS.md"

echo "check-guidance-size.test.sh: PASS ($CASES cases)"
