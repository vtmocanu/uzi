#!/bin/sh
# Gate NEW text-extension source files against being git-binary or control-byte
# carrying, whole-tracked-tree (PRD #500 M2).
#
# usage: scripts/check-binary-text.sh <canary-file>
#   e.g. scripts/check-binary-text.sh scripts/binary-text-canary.go
#
# 🔴 WHY THIS EXISTS. A NEW text-extension file that git treats as BINARY (a raw
# NUL in the first ~8000 bytes) -- or that carries other control bytes -- passes
# lint, typecheck, every test and check-styles, because the bytes are behaviorally
# invisible. One such file was caught only because a human happened to notice
# `git diff --stat` reporting it as `Bin 0 -> N`. There is no `.gitattributes` and
# no `--numstat`/`check-attr`/NUL scan anywhere in the repo, so git classifies purely
# by its content heuristic and nothing gates on the result. This makes it mechanical.
#
# 🔴 THE TEXT-EXTENSION PATHSPEC SET (fixed, documented here):
#   '*.go' '*.ts' '*.tsx' '*.js' '*.jsx' '*.mjs' '*.sql' '*.sh' '*.md' '*.json'
#   '*.jsonc' '*.yaml' '*.yml' '*.html' '*.css'
# This set is intentionally SOURCE-CODE-ORIENTED, not every text extension. It covers
# real new source/config files while excluding image/archive extensions (`.png`,
# `.log`, `.lock`, `.svg`) that may legitimately carry non-text bytes.
#
# TWO CLAUSES, because git's binary heuristic is NUL-presence only:
#   Clause 1 (git-binary via NUL): `git diff --numstat <empty-tree> HEAD -- <set>`
#     marks a binary-treated row with `-` in BOTH count columns (added AND deleted);
#     flag any such row. `4b825dc…` is git's empty-tree constant, so this is a
#     whole-tree floor with no network op (Decision 2) -- it mirrors git's own
#     `diff --stat` view and is kept as DEFENSE-IN-DEPTH.
#   Clause 2 (non-NUL control bytes git's heuristic misses): enumerate every tracked
#     file in the set NUL-delimited (`git ls-files -z`) and run scan_control_bytes()
#     over them. That ONE function is ALSO what the canary liveness step below runs,
#     so a change to the detector cannot pass the canary green while silently blinding
#     the whole-tree scan (the vacuous-pass hole a canary-only second detector would
#     leave). It opens each file in raw mode and scans in 64 KiB chunks with an early
#     exit on the first control byte outside `\t`=09, `\n`=0a, `\r`=0d. NUL-delimited
#     names are LOAD-BEARING: git C-QUOTES any path with a non-ASCII/control byte
#     (core.quotePath defaults true), so a `while read` loop would hand perl a quoted
#     literal it cannot open. A tracked file ABSENT from the worktree (locally deleted,
#     sparse checkout, mid-rebase) has no content to judge and is SKIPPED; a file that
#     EXISTS but cannot be opened is printed as a finding (fail CLOSED). Clause 2
#     SUBSUMES clause 1's NUL detection (a NUL anywhere, not just the first ~8KB) and
#     additionally catches the non-NUL control-byte case git does not treat as binary.
# perl (not plain `grep`) does the control-byte test because this host's `grep` is
# ugrep, which mis-handles negated character classes in POSIX modes (root CLAUDE.md).
#
# 🔴 perl IS REQUIRED, AND CHECKED UP FRONT. If perl is missing the instrument is
# declared BROKEN (exit 2), never a silent clean pass -- a missing perl must not read
# as "0 control-byte files" on the exact class this gate guards.
#
# 🔴 LOAD-BEARING: `:(exclude)scripts/binary-text-canary.*` IS ON BOTH WHOLE-TREE
# CLAUSES. The committed canary carries a planted NUL and lives at a text extension
# (`.go`), so WITHOUT the exclude the whole-tree scan would flag the canary itself and
# this gate would be PERMANENTLY red on its own fixture (Success Criterion 2's "exits 0
# on the current tree" AND "canary fires every run" would be unsatisfiable). The
# canary-first liveness step below scans the canary EXPLICITLY (its own path); the two
# whole-tree clauses exclude it.
#
# 🔴 A LIVENESS CANARY, BECAUSE A SILENT PASS IS THE FAILURE MODE. If the detector
# ever matched nothing (a changed pattern, a broken perl), the tree would read "0
# binary/control-byte files" and this gate would pass VACUOUSLY. The canary file ($1)
# carries a deliberately planted NUL; scan_control_bytes() -- the SAME detector the
# whole-tree scan uses -- is run over it FIRST and MUST fire, or the instrument is
# declared broken. A clean run therefore confirms the canary fired -- a positive
# observation, so a green here is not "the detector never looked".
#
# NO SKIP BRANCH AND NO *_REQUIRED ENV VAR, deliberately -- the asymmetry with
# lint-yaml.sh / lint-formula.sh is that those wrap brew/pip tools a contributor may
# lack, whereas this check needs only sh/git/perl/awk/sort, which are always present
# (and perl's presence is asserted above rather than assumed).
#
# EXIT CODES (the check-spec-numbering.sh / check-migration-numbering.sh convention):
#     2 = the instrument is broken (bad args, not-a-git-tree, missing perl, missing
#         canary, or the canary control byte did not fire)
#     1 = there are findings (a binary/control-byte text file from clause 1 or 2)
#     0 = clean, and the canary control byte was seen
# `task`'s own rc is 201 for all of them.
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: scripts/check-binary-text.sh <canary-file>" >&2
  echo "  e.g. scripts/check-binary-text.sh scripts/binary-text-canary.go" >&2
  exit 2
fi

CANARY="$1"

# git's empty-tree constant -- the whole-tree floor's base for clause 1.
EMPTY_TREE="4b825dc642cb6eb9a060e54bf8d69288fbee4904"

# Run from the repo root whatever the caller's directory, like the sibling scripts.
ROOT="$(git rev-parse --show-toplevel)" || {
  echo "check-binary-text: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || {
  echo "check-binary-text: cannot cd to repo root: $ROOT" >&2
  exit 2
}

# 🔴 perl runs the control-byte detector; a missing perl must be an INSTRUMENT-BROKEN
# error, never a silent clean pass. (has_control_byte's old `! perl …` form returned
# "found" on a 127 exit, so a missing perl silently passed the canary AND blanked the
# scan -- a fail-OPEN on the class this gate guards.)
if ! command -v perl >/dev/null 2>&1; then
  echo "check-binary-text: INSTRUMENT BROKEN -- perl not found on PATH; the" >&2
  echo "check-binary-text: control-byte scan cannot run, so no tree verdict is trustworthy." >&2
  exit 2
fi

if [ ! -f "$CANARY" ]; then
  echo "check-binary-text: canary file not found: $CANARY" >&2
  echo "  The canary is what proves the control-byte detector fires; without it a" >&2
  echo "  clean tree cannot be told from a detector that matched nothing." >&2
  exit 2
fi

# THE clause-2 control-byte detector -- used for BOTH the canary liveness check
# (explicit path) and the whole-tree scan, so the two CANNOT drift. Reads
# NUL-delimited filenames on stdin; for each: opens raw and scans in 64 KiB chunks,
# early-exiting on the first control byte outside \t(09) \n(0a) \r(0d). Prints any
# offender (newline-terminated). A tracked file ABSENT from the worktree is SKIPPED
# (no content to judge -- e.g. a locally deleted-but-tracked file mid-rebase); one
# that EXISTS but cannot be opened is printed (fail CLOSED).
scan_control_bytes() {
  perl -0 -ne '
    chomp(my $f = $_);
    if (open my $fh, "<:raw", $f) {
      my $hit = 0;
      while (read($fh, my $buf, 65536)) {
        if ($buf =~ /[\x00-\x08\x0b\x0c\x0e-\x1f]/) { $hit = 1; last; }
      }
      close $fh;
      print "$f\n" if $hit;
    } elsif (-e $f) {
      print "$f\n";
    }
  '
}

# 🔴 CANARY FIRST: prove the instrument fires before trusting any tree verdict, using
# the SAME scan_control_bytes the whole-tree scan uses. Scan the canary EXPLICITLY
# (its own path, never via the excluded whole-tree scan). If the planted NUL does not
# fire the detector, the whole-tree "clean" would be meaningless.
if [ -z "$(printf '%s\0' "$CANARY" | scan_control_bytes)" ]; then
  echo "check-binary-text: ================================================================" >&2
  echo "check-binary-text: INSTRUMENT BROKEN -- the control-byte detector did not fire on the" >&2
  echo "check-binary-text: canary ($CANARY)." >&2
  echo "check-binary-text:" >&2
  echo "check-binary-text: That fixture carries a deliberately planted NUL byte. Finding none" >&2
  echo "check-binary-text: means the perl control-byte scan matched nothing (a changed pattern," >&2
  echo "check-binary-text: a mangled canary), so a \"clean\" tree would mean nothing. Restore the" >&2
  echo "check-binary-text: canary's NUL or fix scan_control_bytes()." >&2
  echo "check-binary-text: ================================================================" >&2
  exit 2
fi

# The fixed text-extension pathspec set, PLUS the load-bearing canary exclude, as
# positional params so both whole-tree git commands reuse the identical set. 🔴 The
# `:(exclude)scripts/binary-text-canary.*` is MANDATORY on BOTH clauses (see header).
set -- \
  '*.go' '*.ts' '*.tsx' '*.js' '*.jsx' '*.mjs' '*.sql' '*.sh' '*.md' '*.json' \
  '*.jsonc' '*.yaml' '*.yml' '*.html' '*.css' \
  ':(exclude)scripts/binary-text-canary.*'

# Clause 1: git-binary rows (added AND deleted columns both `-`) in the whole-tree diff
# from the empty tree to HEAD, over the text-extension set (canary excluded).
clause1="$(git -c core.quotePath=false diff --numstat "$EMPTY_TREE" HEAD -- "$@" \
  | awk -F'\t' '$1 == "-" && $2 == "-" { print $3 }')"

# Clause 2: every tracked file in the set (canary excluded), scanned for control bytes
# outside \t \n \r via scan_control_bytes (the same detector the canary proved live).
# 🔴 NUL-DELIMITED (`ls-files -z`) so a path carrying a non-ASCII or control byte -- or
# an embedded newline -- is NEITHER C-quoted (git's core.quotePath default) NOR split.
clause2="$(git ls-files -z -- "$@" | scan_control_bytes)"

# Union of both clauses, blank lines dropped, deduplicated.
findings="$(printf '%s\n%s\n' "$clause1" "$clause2" | awk 'NF' | sort -u)"

if [ -n "$findings" ]; then
  echo "check-binary-text: BINARY / CONTROL-BYTE text file(s) detected:" >&2
  printf '%s\n' "$findings" | while IFS= read -r bad; do
    printf '  %s\n' "$bad" >&2
  done
  echo "" >&2
  echo "  A NEW text-extension file that git treats as binary (a raw NUL) or that" >&2
  echo "  carries other control bytes passes lint/typecheck/tests/check-styles because" >&2
  echo "  the bytes are behaviorally invisible. Remove the control byte(s) from the" >&2
  echo "  file(s) above, or (if the content is genuinely binary) give it a non-source" >&2
  echo "  extension outside this check's set. Scanned set: '*.go' '*.ts' '*.tsx' '*.js'" >&2
  echo "  '*.jsx' '*.mjs' '*.sql' '*.sh' '*.md' '*.json' '*.jsonc' '*.yaml' '*.yml'" >&2
  echo "  '*.html' '*.css'." >&2
  exit 1
fi

echo "check-binary-text: clean -- no git-binary or control-byte text files across the"
echo "check-binary-text: source-code-oriented pathspec set '*.go' '*.ts' '*.tsx' '*.js'"
echo "check-binary-text:   '*.jsx' '*.mjs' '*.sql' '*.sh' '*.md' '*.json' '*.jsonc'"
echo "check-binary-text:   '*.yaml' '*.yml' '*.html' '*.css' (canary excluded from the whole-tree scan)."
echo "check-binary-text: canary NUL DETECTED in $CANARY -- the detector is live, so this"
echo "check-binary-text: green is a positive observation rather than a check that never looked."
exit 0
