#!/bin/sh
# Gate the committed sqlc codegen against its source: regenerate and fail on drift
# (the standalone check PRD #1359 lists as an M1 deliverable, landed early per an
# improve_uzi run-review recommendation).
#
# WHY THIS EXISTS. CI already runs this check inline in the `validate-api` job
# (.github/workflows/ci.yml, step "sqlc drift (regenerate must be a no-op)":
# `sqlc generate` then `git diff --exit-code -- internal/store`). But NO `task`
# target reproduces it, so a branch can pass every local/uzi-run gate (`task gate`,
# `task gate:api`) and still redden CI on sqlc drift -- e.g. an edited migration or
# query whose generated *.sql.go was never regenerated, or a default-branch query
# the branch never saw. PR #1357 was finalized "gates green" and reddened CI on
# exactly this (prds/1359-finalize-against-current-main.md). Wiring this into
# `gate:api` closes that local-gate blind spot.
#
# EXIT CODES follow the convention scripts/deadcode-gate.sh sets (task collapses all
# of them to 201, so this script is the only place the distinction can live):
#
#     2 = the instrument is broken (sqlc could not run: toolchain/network/config, or
#         the version pin could not be read) -- NEVER a silent green
#     1 = drift: the committed codegen does not match `sqlc generate` output
#     0 = clean
#
# VERSION PARITY WITH CI IS BY CONSTRUCTION. The pinned sqlc version is READ from
# .github/workflows/ci.yml's `SQLC_VERSION="v..."` line -- the one Renovate keeps
# current (renovate.json's `# renovate-tarball:` manager) -- rather than hardcoded
# here, so the local `go run` codegen can never drift from the version CI's pinned
# release binary produces. Same version => byte-identical codegen (ci.yml documents
# this). Local uses `go run` (portable, no install), per .claude/rules/go.md; CI
# uses the sha256-verified amd64 release binary for speed. If that line ever changes
# shape, this script fails closed (exit 2) rather than silently using a wrong sqlc.
set -eu

# The repo root is wherever this is invoked (Taskfile runs it from the root); use
# git rather than $0's dirname so the gate operates on the repo under test -- which
# also lets scripts/sqlc-drift-gate.test.sh drive it inside a throwaway repo.
ROOT=""
ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [ -z "$ROOT" ]; then
  echo "sqlc-drift-gate: not inside a git work tree (cannot diff generated code)." >&2
  exit 2
fi

API_DIR="$ROOT/api"
CI_YML="$ROOT/.github/workflows/ci.yml"
GEN_SUBDIR="internal/store" # sqlc.yaml's sole `out:`; matches CI's diff scope exactly.

if [ ! -d "$API_DIR" ]; then
  echo "sqlc-drift-gate: no api module directory at $API_DIR." >&2
  exit 2
fi
if [ ! -f "$CI_YML" ]; then
  echo "sqlc-drift-gate: cannot find $CI_YML to read the pinned SQLC_VERSION." >&2
  exit 2
fi

# The v-prefixed git tag from ci.yml's `SQLC_VERSION="v1.31.1"` line. awk -F\" takes
# the quoted value; the trailing `# comment` and indentation are ignored. Matching on
# `SQLC_VERSION="v` keeps it off any unrelated line that merely mentions the name.
SQLC_VERSION="$(awk -F'"' '/SQLC_VERSION="v/ { print $2; exit }' "$CI_YML")"
if [ -z "$SQLC_VERSION" ]; then
  echo "sqlc-drift-gate: could not read SQLC_VERSION=\"v...\" from $CI_YML." >&2
  echo "  The pin's shape changed; update this reader so local sqlc stays in lockstep with CI." >&2
  exit 2
fi

SQLC_PKG="github.com/sqlc-dev/sqlc/cmd/sqlc@$SQLC_VERSION"
echo "sqlc-drift-gate: regenerating with $SQLC_PKG (pinned via $CI_YML)"

TMP="$(mktemp -d)"
# shellcheck disable=SC2064  # expand TMP now: the trap must survive its unset.
trap "rm -rf '$TMP'" EXIT INT TERM

# 🔴 RC FIRST. Redirect to a file and read `$?` on the very next line; `|| rc=$?`
# puts the command in a condition context so errexit does not pre-empt the capture.
rc=0
(cd "$API_DIR" && go run "$SQLC_PKG" generate) >"$TMP/gen.out" 2>&1 || rc=$?
if [ "$rc" -ne 0 ]; then
  cat "$TMP/gen.out" >&2
  echo "sqlc-drift-gate: \`go run $SQLC_PKG generate\` exited $rc." >&2
  echo "  This is an INSTRUMENT failure (toolchain, module fetch/network, or a SQL error)," >&2
  echo "  not a clean run -- an empty diff below it would be meaningless. Fix the tool first." >&2
  exit 2
fi

# A generated *.sql.go that sqlc produces but that was never committed stays UNTRACKED,
# and `git diff --exit-code` below (tracked-only) reads it as clean -- so required codegen
# can be missing from the commit while the gate reports green. sqlc's out: dir
# (api/internal/store) also holds hand-written *_test.go and the queries/ + migrations/
# sources, so scope to the generated suffix: any untracked *.sql.go here is codegen missing
# from the commit, whether sqlc just created it or it was generated earlier and never staged.
# --exclude-standard so a gitignored scratch file is not counted; `|| true` keeps a no-match
# grep (rc 1) from tripping errexit.
untracked_gen="$(git -C "$ROOT" ls-files --others --exclude-standard -- "api/$GEN_SUBDIR" | grep '\.sql\.go$' || true)"
if [ -n "$untracked_gen" ]; then
  echo "sqlc-drift-gate: DRIFT -- untracked generated file(s) under api/$GEN_SUBDIR (missing from the commit):" >&2
  printf '%s\n' "$untracked_gen" >&2
  echo "  Regenerate and stage the codegen:  cd api && go run $SQLC_PKG generate && git add -A internal/store" >&2
  echo "  (CI's validate-api job would report clean here; this untracked-file check catches it before the push.)" >&2
  exit 1
fi

# `git diff --exit-code` returns 1 for "differences found" and >1 (e.g. 128) for a
# git error. Keep the 2/1/0 contract honest: only rc==1 is drift; a git failure is
# an instrument problem (exit 2), not a stale-codegen finding.
rc=0
git -C "$ROOT" diff --exit-code -- "api/$GEN_SUBDIR" >"$TMP/diff.out" 2>&1 || rc=$?
if [ "$rc" -gt 1 ]; then
  cat "$TMP/diff.out" >&2
  echo "sqlc-drift-gate: \`git diff\` exited $rc -- cannot determine drift (instrument failure)." >&2
  exit 2
fi
if [ "$rc" -eq 1 ]; then
  echo "sqlc-drift-gate: DRIFT -- committed codegen under api/$GEN_SUBDIR is stale." >&2
  git -C "$ROOT" diff --stat -- "api/$GEN_SUBDIR" >&2 || true
  echo "  Regenerate and commit:  cd api && go run $SQLC_PKG generate" >&2
  echo "  (CI's validate-api job runs the same check; this catches it before the push.)" >&2
  exit 1
fi

echo "sqlc-drift-gate: clean -- api/$GEN_SUBDIR matches \`sqlc generate\` output."
exit 0
