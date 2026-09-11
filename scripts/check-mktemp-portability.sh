#!/usr/bin/env bash
# Guard: no `mktemp -t <template>` with too few X's in a tracked shell script.
#
# 🔴 WHY THIS EXISTS. GNU coreutils rejects a `mktemp -t` template that lacks a run
# of at least 3 consecutive `X`s ("too few X's in template") while BSD/macOS accepts
# it, so such a call passes on a contributor's mac and dies only on the Linux
# worker/CI -- e2e/run-store-it.sh regressed exactly this, and commit f0e3c438 fixed
# the same class in scripts/{scan-secrets,deps-check-gate,govulncheck-gate,npm-audit-
# gate}.sh. A template carrying a run of 3+ X's (e.g. `mktemp -t foo.XXXXXX`) is
# accepted by GNU and is NOT a defect, so it is deliberately NOT flagged; a 0-, 1- or
# 2-X template all die the same way and are all flagged. Fix a hit with the portable
# idiom:
#   LOG="$(mktemp "${TMPDIR:-/tmp}/<name>.XXXXXX")"
#
# 🔴 NON-VACUOUS. A positive-control CANARY file (the sole argument) carries the
# broken pattern and MUST match, or the detector is declared broken (exit 2) rather
# than silently passing -- this also catches a git built without PCRE. The canary
# and the tree sweep share ONE pattern and ONE engine (`git grep -P`); do not split
# them into two literals or a future typo goes green on both. Mirrors
# scripts/check-binary-text.sh.
#
# 🔴 `git grep -P` (PCRE), NOT plain/`-E`: this host's grep is ugrep, whose negated
# bracket classes (`[^X...]`) misbehave in plain/`-E` mode (root CLAUDE.md, "grep on
# this host"). Never regress this to plain grep.
#
# Scope: tracked `*.sh` only (the same scope as `lint:shell`, which greps
# `git ls-files '*.sh'`). The `^[^#]*` prefix skips a line whose `mktemp` sits behind
# a `#` (a comment that merely names the anti-pattern, e.g. in scan-secrets.sh).
#
# EXIT CODES:
#     2 = instrument broken (no/absent canary arg, not in a git tree, or the detector
#         did not fire on the canary -- pattern or PCRE support regressed)
#     1 = a tracked *.sh carries a `mktemp -t` template with too few X's
#     0 = clean
# `task`'s own rc is 201 for any non-zero.
set -euo pipefail

CANARY="${1:-}"
if [ -z "$CANARY" ]; then
  echo "check-mktemp-portability: a positive-control canary file argument is required" >&2
  exit 2
fi

ROOT="$(git rev-parse --show-toplevel)" || {
  echo "check-mktemp-portability: not inside a git work tree" >&2
  exit 2
}
cd "$ROOT" || {
  echo "check-mktemp-portability: cannot cd to repo root: $ROOT" >&2
  exit 2
}

if [ ! -f "$CANARY" ]; then
  echo "check-mktemp-portability: canary file not found: $CANARY" >&2
  exit 2
fi

# ONE pattern, used for both the canary control and the tree sweep. `mktemp` then a
# `-t` flag then a whitespace-delimited template token that lacks a run of 3+ X's
# (the `(?![^[:space:]]*XXX)` negative lookahead rejects the token when `XXX` occurs
# anywhere in it -- GNU needs >=3 consecutive X's), up to whitespace or end-of-line;
# the leading `^[^#]*` keeps the match out of a comment.
PAT='^[^#]*mktemp[[:space:]]+-t[[:space:]]+(?![^[:space:]]*XXX)[^[:space:]]+([[:space:]]|$)'

# Positive control: the canary MUST trip the detector (which also proves PCRE is
# available), else the guard is vacuous -- fail loudly instead of passing. `git grep`
# only searches tracked files, so the canary must be committed; a no-match (rc 1) or
# a PCRE error (rc >= 2) both land here as "did not fire".
if ! git grep -qP "$PAT" -- "$CANARY"; then
  echo "check-mktemp-portability: detector did not fire on canary '$CANARY' (instrument broken -- pattern or PCRE support regressed, or the canary is untracked)" >&2
  exit 2
fi

# Whole-tree sweep over tracked shell scripts. `git grep` exits 1 on no-match (the
# CLEAN case), so capture with `|| true` -- an unguarded non-zero would abort here
# under `set -e`. A genuine PCRE error cannot reach this point: the canary control
# above already exited 2. The canary is a non-.sh file, so it is not in this pathspec.
hits="$(git grep -nP "$PAT" -- '*.sh' || true)"
if [ -n "$hits" ]; then
  echo "check-mktemp-portability: \`mktemp -t\` with too few X's found (breaks on GNU coreutils, 'too few X's in template' -- needs >=3 consecutive X's):" >&2
  echo "$hits" >&2
  echo "  fix: use an explicit template, e.g. mktemp \"\${TMPDIR:-/tmp}/<name>.XXXXXX\"  (see scripts/scan-secrets.sh)" >&2
  exit 1
fi

echo "check-mktemp-portability: clean"
