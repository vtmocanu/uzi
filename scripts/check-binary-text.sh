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
#     file in the set NUL-delimited (`git ls-files -z`) and let perl itself iterate the
#     names and open each in raw mode, scanning for control bytes outside `\t`=09,
#     `\n`=0a, `\r`=0d. NUL-delimited + perl-iterated opens (no shell `read` loop) is
#     LOAD-BEARING: git C-QUOTES any path with a non-ASCII/control byte, quote or
#     backslash (core.quotePath defaults true), so a `while read` loop would hand perl
#     a quoted literal it cannot open -- and a failed open silently reports that file
#     CLEAN, a fail-OPEN on the exact class this gate guards. A file perl cannot open is
#     printed as a finding (fail CLOSED), never skipped. Clause 2 SUBSUMES clause 1's
#     NUL detection (a NUL anywhere, not just the first ~8KB) and additionally catches
#     the non-NUL control-byte case git does not treat as binary at all.
# perl (not plain `grep`) does the control-byte test because this host's `grep` is
# ugrep, which mis-handles negated character classes in POSIX modes (root CLAUDE.md).
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
# carries a deliberately planted NUL; the clause-2 control-byte detector is run over it
# FIRST and MUST fire, or the instrument is declared broken. A clean run therefore
# confirms the canary fired -- a positive observation, so a green here is not "the
# detector never looked".
#
# NO SKIP BRANCH AND NO *_REQUIRED ENV VAR, deliberately -- the asymmetry with
# lint-yaml.sh / lint-formula.sh is that those wrap brew/pip tools a contributor may
# lack, whereas this check needs only sh/git/perl/awk/sort, which are always present.
#
# EXIT CODES (the check-spec-numbering.sh / check-migration-numbering.sh convention):
#     2 = the instrument is broken (bad args, not-a-git-tree, missing canary, or the
#         canary control byte did not fire)
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

if [ ! -f "$CANARY" ]; then
  echo "check-binary-text: canary file not found: $CANARY" >&2
  echo "  The canary is what proves the control-byte detector fires; without it a" >&2
  echo "  clean tree cannot be told from a detector that matched nothing." >&2
  exit 2
fi

# The clause-2 control-byte detector, as a function so the exact same test runs over
# the canary (explicitly) and over each tracked file. Returns non-zero (perl exit 1)
# when the file carries a control byte outside \t \n \r.
has_control_byte() {
  ! perl -ne 'exit 1 if /[\x00-\x08\x0b\x0c\x0e-\x1f]/' "$1"
}

# 🔴 CANARY FIRST: prove the instrument fires before trusting any tree verdict. Scan
# the canary EXPLICITLY (its own path, never via the excluded whole-tree scan). If the
# planted NUL does not fire the detector, the whole-tree "clean" would be meaningless.
if ! has_control_byte "$CANARY"; then
  echo "check-binary-text: ================================================================" >&2
  echo "check-binary-text: INSTRUMENT BROKEN -- the control-byte detector did not fire on the" >&2
  echo "check-binary-text: canary ($CANARY)." >&2
  echo "check-binary-text:" >&2
  echo "check-binary-text: That fixture carries a deliberately planted NUL byte. Finding none" >&2
  echo "check-binary-text: means the perl control-byte scan matched nothing (a changed pattern," >&2
  echo "check-binary-text: a mangled canary), so a \"clean\" tree would mean nothing. Restore the" >&2
  echo "check-binary-text: canary's NUL or fix has_control_byte()." >&2
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
# outside \t \n \r. 🔴 NUL-DELIMITED (`ls-files -z`) so a path carrying a non-ASCII or
# control byte -- or an embedded newline -- is NEITHER C-quoted (git's core.quotePath
# default) NOR split. Either would hand perl a filename it cannot open, and a failed
# open would silently report that file CLEAN: a fail-OPEN on the exact class this gate
# guards. perl reads the NUL-delimited names, opens each in raw mode, and prints the
# offenders; a tracked file it cannot open is itself printed as a finding (fail CLOSED),
# never skipped.
clause2="$(git ls-files -z -- "$@" | perl -0 -ne '
  chomp(my $f = $_);
  if (open my $fh, "<:raw", $f) {
    local $/;
    my $data = <$fh>;
    close $fh;
    print "$f\n" if defined($data) && $data =~ /[\x00-\x08\x0b\x0c\x0e-\x1f]/;
  } else {
    print "$f\n";
  }
')"

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
