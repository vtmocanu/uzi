#!/bin/sh
# Gate one Go module on `golang.org/x/tools/cmd/deadcode` (PRD #103 M4).
#
# WHY A WRAPPER EXISTS AT ALL, given both baselines ship EMPTY. It is not the
# baseline that needs it -- it is the EXIT CODE.
#
# 🔴 deadcode EXITS 0 WHETHER IT FINDS 0, 1 OR 44 DEAD FUNCTIONS. rc is 1 ONLY
# when the packages fail to load, AND ON THAT PATH STDOUT IS 0 BYTES. Measured
# 2026-08-02 by two agents independently (probes/prd-103-m4-{architect,reviewer}.txt),
# reproduced by the lead. So the naive wrapper --
#
#     run | sort > cur; comm -13 baseline cur; fail if additions
#
# -- is SILENT GREEN on a module that does not compile: the empty finding set
# diffs against the baseline as removals-only, there are no additions, and the
# gate passes. Measured on a tree holding `func broken( {`: RC=0, 0 findings
# captured. That is M2's `test -z "$(gofmt -l .)"` hole arriving through a
# different door -- an EMPTY RESULT SET INDISTINGUISHABLE FROM A CLEAN ONE.
#
# So this script READS THE EXIT CODE BEFORE IT LOOKS AT THE OUTPUT, and uses the
# convention `fmt-check:api` and `lint:api` already set:
#
#     2 = the instrument is broken (load error, missing baseline, bad usage)
#     1 = there are findings, or the baseline holds a stale entry
#     0 = clean
#
# `task`'s own exit code is 201 for every one of those, so this script is the
# only place the distinction can live.
#
# THE KEY IS POSITION-FREE, AND THAT IS LOAD-BEARING (PRD #103 Decision 11).
# deadcode's default output is `path:line:col: unreachable func: Name`. Measured:
# inserting ONE comment line above a baselined symbol moved
# `skillhook.go:75:24` -> `:76:24`, so a baseline of raw output lines reports a
# spurious ADDITION plus a spurious REMOVAL on any unrelated edit above any
# baselined symbol -- which is how a gate stops being run. The `-f` template
# below keys on (import path, function name) and carries the file:line:col as a
# THIRD field that is printed for the human and never compared.
#
# SORT BOTH SIDES, WITH `LC_ALL=C`. deadcode's output is stable across runs
# (three runs x three shapes, byte-identical, including a 7475-line shape) but it
# is DETERMINISTIC, NOT LEXICOGRAPHIC: packages by import path, then functions
# within a package by (file, LINE NUMBER), which its own source comments explain
# as keeping `(T).Marshal` next to `(*T).Unmarshal`. So `sort` is not idempotent
# on tool output, and `comm` requires both inputs in one collation. The locale is
# part of the baseline file's contract, not an implementation detail.
#
# THE PACKAGE PATTERN IS `./...` FROM THE MODULE ROOT AND MUST NOT BE NARROWED.
# Only `main` packages are call-graph roots, so scoping to `./internal/...` --
# the natural "just our code" instinct -- orphans everything reachable only from
# `cmd/` and inflates api's finding count 1 -> 86, AT rc=0, with no error of any
# kind. Measured.
set -eu

MODULE_DIR="${1:-}"
TOOL="${2:-}"
MODE="${3:-}"

if [ -z "$MODULE_DIR" ] || [ -z "$TOOL" ]; then
  echo "usage: scripts/deadcode-gate.sh <module-dir> <pkg@version> [--write]" >&2
  echo "  e.g. scripts/deadcode-gate.sh api golang.org/x/tools/cmd/deadcode@v0.48.0" >&2
  exit 2
fi

# The pinned tool package is passed IN rather than defaulted here, so that
# `task`'s echo shows the version. The Taskfile header calls that echo the
# mechanism by which a teammate notices a load-bearing pin going missing; moving
# the pin into this file would take it out of the echo's reach the way
# `.golangci.yml` did for the lint ratchet. One deliberate hiding is enough.
BASELINE="$MODULE_DIR/.deadcode-baseline"

if [ ! -d "$MODULE_DIR" ]; then
  echo "deadcode-gate: no such module directory: $MODULE_DIR" >&2
  exit 2
fi

TMP="$(mktemp -d)"
# shellcheck disable=SC2064  # expand TMP now: the trap must survive its unset.
trap "rm -rf '$TMP'" EXIT INT TERM

# One field per line: import path, function name, file:line:col. Fields 1-2 are
# the baseline key; field 3 is for the human only.
TEMPLATE='{{range .Funcs}}{{printf "%s\t%s\t%s:%d:%d\n" $.Path .Name .Position.File .Position.Line .Position.Col}}{{end}}'

# 🔴 RC FIRST. Redirect to files and read `$?` on the very next line -- do not
# pipe, do not `$( )` into a test. `|| rc=$?` puts the command in a condition
# context so errexit does not pre-empt the capture.
rc=0
(cd "$MODULE_DIR" && go run "$TOOL" -test -f="$TEMPLATE" ./...) \
  >"$TMP/full" 2>"$TMP/err" || rc=$?
if [ "$rc" -ne 0 ]; then
  cat "$TMP/err" >&2
  echo "deadcode-gate: $TOOL exited $rc over $MODULE_DIR (0 findings were captured)." >&2
  echo "  This is a LOAD ERROR, not a clean run. deadcode exits 0 when it finds dead" >&2
  echo "  code and 1 only when the packages do not build -- so an empty result here" >&2
  echo "  means the module is broken, not that it is clean. Fix the build first." >&2
  exit 2
fi
# stderr on a successful run is still worth seeing (deadcode warns about e.g.
# packages it skipped); it just does not decide the verdict. Written as an `if`
# and not `[ -s ... ] && cat`, because that idiom's status is the TEST's when the
# file is empty, which is the common case here and is a coin-flip against errexit.
if [ -s "$TMP/err" ]; then
  cat "$TMP/err" >&2
fi

LC_ALL=C cut -f1,2 <"$TMP/full" | LC_ALL=C sort >"$TMP/current"

if [ "$MODE" = "--write" ]; then
  # NOT A TASKFILE TARGET, ON PURPOSE. `.claude/agents/tester.md` tells testers to
  # refuse a gate slot whose command looks like a fixing variant, and "regenerate
  # the baseline" is the fixing variant of this check. It lives here so the
  # baseline's header can name ONE command instead of restating the invocation
  # above (which would then drift from it), and it is invoked by hand.
  #
  # 🔴 ADDING A LINE HERE IS A DELIBERATE SUPPRESSION, NOT THE ROUTINE FIX. Both
  # baselines ship EMPTY. The routine fix for a deadcode finding is to DELETE the
  # function.
  {
    sed -e '/^[[:space:]]*#/!d' "$BASELINE" 2>/dev/null || true
    cat "$TMP/current"
  } >"$TMP/new-baseline"
  mv "$TMP/new-baseline" "$BASELINE"
  echo "deadcode-gate: wrote $BASELINE ($(wc -l <"$TMP/current" | tr -d ' ') entries)"
  exit 0
fi

if [ ! -f "$BASELINE" ]; then
  echo "deadcode-gate: baseline missing: $BASELINE" >&2
  echo "  A missing baseline must not read as a clean module. Restore it from git," >&2
  echo "  or regenerate with: ./scripts/deadcode-gate.sh $MODULE_DIR $TOOL --write" >&2
  exit 2
fi

# Comments and blank lines are stripped: no import path and no Go identifier can
# contain `#`, so a full-line comment is unambiguous. The header in the baseline
# file is the only thing that makes an empty file self-explanatory.
LC_ALL=C sed -e '/^[[:space:]]*#/d' -e '/^[[:space:]]*$/d' "$BASELINE" \
  | LC_ALL=C sort >"$TMP/baseline"

LC_ALL=C comm -13 "$TMP/baseline" "$TMP/current" >"$TMP/added"
LC_ALL=C comm -23 "$TMP/baseline" "$TMP/current" >"$TMP/gone"

status=0

if [ -s "$TMP/added" ]; then
  echo "deadcode-gate: $MODULE_DIR has unreachable functions not in $BASELINE:"
  # Re-join to field 3 so the human gets file:line:col. The comparison never
  # touched it, which is the whole point of the two-field key.
  while IFS= read -r key; do
    awk -F'\t' -v k="$key" '($1 "\t" $2) == k { printf "  %s: unreachable func: %s\n", $3, $2 }' \
      <"$TMP/full"
  done <"$TMP/added"
  echo "  Delete them. If one is genuinely reachable in a way deadcode cannot see"
  echo "  (it does not understand //go:linkname, and the analysis is valid for one"
  echo "  GOOS/GOARCH/-tags configuration), add it to $BASELINE WITH A COMMENT"
  echo "  saying why -- that file ships empty and every entry is a suppression."
  status=1
fi

if [ -s "$TMP/gone" ]; then
  # FATAL, NOT A WARNING, and the reason is the vacuous-negative class this repo
  # keeps paying for: a baseline entry whose symbol no longer exists is a live
  # suppression for a name that could be re-introduced later and silently pass.
  # "A warning nobody must act on" is exactly what PRD #103 Decision 3 rejects.
  # With an EMPTY baseline this branch cannot fire on an ordinary deletion; it can
  # only fire when a deliberate suppression has become unnecessary, and removing
  # it in the same commit is the correct hygiene rather than an imposition.
  echo "deadcode-gate: $BASELINE holds entries that are no longer reported:"
  sed -e 's/^/  /' <"$TMP/gone"
  echo "  The suppression is stale. Delete those lines (and their comment) --"
  echo "  a suppression for a symbol that no longer exists silently covers the next"
  echo "  symbol to take the name."
  status=1
fi

if [ "$status" -eq 0 ]; then
  echo "deadcode-gate: $MODULE_DIR clean ($(wc -l <"$TMP/current" | tr -d ' ') findings, $(wc -l <"$TMP/baseline" | tr -d ' ') baselined)"
fi

exit "$status"
