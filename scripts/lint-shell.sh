#!/bin/sh
# Gate every tracked shell script on shellcheck (PRD #103 M5).
#
# usage: scripts/lint-shell.sh <severity> <exact-shellcheck-version>
#        e.g. scripts/lint-shell.sh warning 0.11.0
#
# 🔴 THE VERSION IS ASSERTED EXACTLY, AND A MISMATCH IS `exit 2`. Decision 7 has
# the Taskfile invoke tools BY NAME and stay indifferent to what put them on PATH,
# so a contributor's brew shellcheck and CI's pinned build can drift apart. That
# would be harmless if versions only differed in speed. They do not -- measured
# 2026-08-03 over the same 14 tracked scripts:
#
#              -S error   -S warning   -S info   -S style
#     0.10.0       0           1          34        36
#     0.11.0       0           4          13        15
#
# 0.10.0 DOES NOT EMIT SC3067 AT ALL, and the info/style counts move in OPPOSITE
# directions across the two, so the tiers are not nested subsets: this is a
# different instrument, not a rounding difference. Two consequences, and the second
# is why an exact assert beats a floor:
#
#   * The three per-instance `# shellcheck disable=SC3067` in
#     agent/templates/entrypoint.sh would be VACUOUS under 0.10.0 -- suppressions
#     for a diagnostic the tool never emits, invisible forever because shellcheck
#     has no unused-directive report. That is carry-forward item 3's silent-no-op
#     class, installed by the design that bans it.
#   * Under 0.10.0 this gate reaches green at `--severity=warning` after ONE source
#     edit instead of one plus three suppressions. That reads as easier and is
#     strictly weaker: GREEN BY THE TOOL'S BLINDNESS RATHER THAN BY THE CODE BEING
#     RIGHT.
#
# So a contributor on 0.11.1 is BLOCKED until they match, which is the correct side
# to err on: the alternative is a local green and a CI red (or worse, the reverse)
# with nothing in either output explaining why. CI installs the pin as a
# sha256-verified tarball (see `lint:repo` in .gitlab-ci.yml); the same
# `darwin.aarch64` asset exists, so a contributor can install the identical build.
#
# 🔴 WHY THIS IS A SCRIPT AND NOT AN INLINE `cmds:` LINE. M5 is the milestone that
# adds shellcheck, so a committed script is LINTED BY THE CHECK IT IMPLEMENTS and
# an inline Taskfile recipe is not -- yamllint checks YAML, not shell embedded in a
# `cmds:` string. The argument is recorded at `Taskfile.yml`'s `deadcode:api`
# target, which made it on M5's behalf a milestone early. This file is in
# `git ls-files '*.sh'`, so it is inside its own scope.
#
# 🔴 AND THE OBVIOUS ONE-LINER IS FAIL-OPEN ON macOS AND FAIL-CLOSED IN CI.
# Measured 2026-08-03 with an EMPTY file list:
#
#     git ls-files '*.sh' | xargs shellcheck
#       BSD xargs (macOS)   never runs the command, exits 0   <- GREEN, checked nothing
#       GNU xargs 4.10.0    exits 123
#       busybox xargs       exits 123
#
# Same recipe, opposite verdicts, and the lying half is the CONTRIBUTOR'S -- which
# is PRD #103 Success Criterion 1's exact failure mode (local and CI disagreeing
# about what a finding is). The list is therefore captured into a variable with
# `|| exit 2`, asserted non-empty, and only then passed to the tool. That is
# `fmt-check:api`'s assignment shape, reused for its SECOND property (an assignment
# whose command substitution fails can be made to exit) rather than copied for its
# first.
#
# EXIT CODES, using the convention `fmt-check:api`, `lint:api` and
# `scripts/deadcode-gate.sh` already set:
#
#     2 = the instrument is broken (not a git repo, empty scope, unreadable file,
#         bad usage, unknown severity)
#     1 = there are findings
#     0 = clean
#
# `task`'s own exit code is 201 for every one of those, so this script is the only
# place that distinction can live. shellcheck's own statuses map cleanly onto it
# and were measured at 0.11.0 rather than read from a man page:
#
#     clean                   0
#     findings                1
#     file does not exist     2      "openBinaryFile: does not exist"
#     no files given          3      usage text
#     bad --severity value    4      "Unknown value for --severity"
#
# so anything above 1 is an instrument failure and is reported as 2.
set -eu

SEVERITY="${1:-}"
WANT_VERSION="${2:-}"

if [ -z "$SEVERITY" ] || [ -z "$WANT_VERSION" ]; then
  echo "usage: scripts/lint-shell.sh <severity> <exact-shellcheck-version>" >&2
  echo "  severity: error|warning|info|style" >&2
  echo "  e.g. scripts/lint-shell.sh warning 0.11.0" >&2
  exit 2
fi

# The severity is validated HERE rather than left to shellcheck, so a typo dies as
# "the instrument is broken" (2) instead of shellcheck's own 4. It is passed as an
# ARGUMENT rather than hardcoded so that `task`'s echo shows the gate's threshold:
# Taskfile.yml's header calls that echo the mechanism by which a teammate notices a
# load-bearing flag going missing, and for this check the threshold IS the gate.
case "$SEVERITY" in
  error|warning|info|style) ;;
  *)
    echo "lint-shell: unknown severity '$SEVERITY' (want error|warning|info|style)" >&2
    exit 2
    ;;
esac

# Run from the repo root whatever the caller's directory. This is NOT tidiness:
# `git ls-files` invoked from a subdirectory lists only that subtree, so a run from
# `scripts/` would check 8 files instead of 14 and pass, having silently narrowed
# its own scope. Failing closed when there is no repo at all is the other half.
ROOT="$(git rev-parse --show-toplevel)" || {
  echo "lint-shell: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || exit 2

# 🔴 VERSION ASSERT, BEFORE ANYTHING IS LINTED. See the header for the 0.10.0 vs
# 0.11.0 table and why an EXACT match rather than a floor. `command -v` is checked
# separately so "no shellcheck at all" does not surface as an unparseable version.
if ! command -v shellcheck >/dev/null 2>&1; then
  echo "lint-shell: no shellcheck on PATH (want exactly $WANT_VERSION)." >&2
  echo "  CI installs it as a sha256-verified tarball; see the lint:repo job." >&2
  echo "  Locally: https://github.com/koalaman/shellcheck/releases/tag/v$WANT_VERSION" >&2
  exit 2
fi

# Assignment, not a pipe: `$?` after a pipe reads the LAST command, so a piped
# form here would report sed's status and never shellcheck's.
SC_VERSION_RAW="$(shellcheck --version 2>/dev/null)" || {
  echo "lint-shell: 'shellcheck --version' failed." >&2
  exit 2
}
# `shellcheck --version` prints four labelled lines; the one that matters reads
# `version: 0.11.0`.
SC_VERSION="$(printf '%s\n' "$SC_VERSION_RAW" | sed -n 's/^version: //p')"

if [ -z "$SC_VERSION" ]; then
  echo "lint-shell: could not parse a version out of 'shellcheck --version':" >&2
  printf '%s\n' "$SC_VERSION_RAW" | sed -e 's/^/    /' >&2
  exit 2
fi

if [ "$SC_VERSION" != "$WANT_VERSION" ]; then
  echo "lint-shell: shellcheck $SC_VERSION is on PATH; this gate is pinned to $WANT_VERSION." >&2
  echo "  This is an INSTRUMENT failure (exit 2), NOT a finding, and the pin is exact" >&2
  echo "  on purpose: 0.10.0 does not emit SC3067 AT ALL, so under it the three" >&2
  echo "  per-instance disables in agent/templates/entrypoint.sh become vacuous and" >&2
  echo "  this gate goes green by the tool's blindness rather than by the code being" >&2
  echo "  right. The info/style tiers also move in opposite directions between those" >&2
  echo "  two releases, so versions are different instruments, not rounding." >&2
  echo "  Install the pin: https://github.com/koalaman/shellcheck/releases/tag/v$WANT_VERSION" >&2
  echo "  (darwin.aarch64 / darwin.x86_64 / linux.x86_64 assets, same build as CI's.)" >&2
  exit 2
fi

# 🔴 `git ls-files '*.sh'`, NOT `e2e/*.sh` + `scripts/*.sh`. Those two globs miss a
# third of the tracked set, INCLUDING agent/templates/entrypoint.sh -- the worker
# container entrypoint that runs in every hosted worker pod, i.e. the one place in
# this repo where a shell bug reaches production. Using the index also stops the
# scope going stale as scripts are added, which a hand-maintained glob list cannot.
#
# ONE CONSEQUENCE OF READING THE INDEX, and it is the same for lint-yaml.sh: A
# BRAND-NEW SCRIPT IS OUT OF SCOPE UNTIL IT IS `git add`ed. Locally that means a
# fresh untracked file can be green here and red in CI, which is the local/CI
# divergence PRD #103 exists to remove -- so `git add` before you trust a green.
# It is not a hole in the gate: nothing reaches CI, or a reviewer, without being in
# the index. Stated because the symptom (a script that passes and then fails the
# pipeline) reads like a flake.
FILES="$(git ls-files -- '*.sh')" || exit 2

if [ -z "$FILES" ]; then
  echo "lint-shell: git ls-files '*.sh' matched nothing under $ROOT." >&2
  echo "  An empty scope is an instrument failure, never a clean run: this repo has" >&2
  echo "  tracked shell scripts, so either the index is broken or the pathspec is." >&2
  exit 2
fi

# Split on NEWLINE ONLY, with globbing off. git C-quotes any path containing a
# control character (newline included) unless -z is given, so one path is one line;
# splitting on the default IFS would additionally break on SPACES, which git does
# NOT quote. `set -f` stops a path containing `*` or `?` from being re-expanded
# against the working tree.
oldIFS="${IFS-}"
IFS='
'
set -f
# shellcheck disable=SC2086  # deliberate word split; see the paragraph above.
set -- $FILES
set +f
IFS="$oldIFS"

# `--norc` MAKES THE PER-INSTANCE-DISABLE RULING STRUCTURAL RATHER THAN A
# CONVENTION. shellcheck walks UP from each file's directory looking for
# `.shellcheckrc`, so without this flag a file placed anywhere above the repo --
# including outside it, in the bare clone's parent or a contributor's home -- would
# silently govern this gate, and a contributor and CI would disagree about what a
# finding is. PRD #103 M5 ruled that SC3067 is suppressed per instance in
# agent/templates/entrypoint.sh, with the busybox measurement written at each site,
# precisely BECAUSE an rc-file disable is blanket across all tracked scripts. This
# flag is what stops that ruling being undone by adding a file nobody reviews.
# No `.shellcheckrc` exists today, so the flag changes no current verdict; it is a
# guard, not a fix. Same argument as `oxlint --config .oxlintrc.json` in
# web/package.json.
rc=0
shellcheck --norc --severity="$SEVERITY" -- "$@" || rc=$?

case "$rc" in
  0)
    echo "lint-shell: clean at severity=$SEVERITY, shellcheck $SC_VERSION ($# tracked scripts)"
    exit 0
    ;;
  1)
    echo "lint-shell: findings at severity=$SEVERITY over $# tracked scripts." >&2
    echo "  Fix them. If a finding is a genuine false positive for the shell this" >&2
    echo "  file actually runs under, add a PER-INSTANCE '# shellcheck disable=SCxxxx'" >&2
    echo "  directly above the line WITH a written justification -- never a" >&2
    echo "  .shellcheckrc entry (blanket across every tracked script, and --norc" >&2
    echo "  above means it would not be read anyway) and never a whole-file" >&2
    echo "  'shell=' declaration (it goes on asserting a dialect after the shebang" >&2
    echo "  changes). shellcheck has no unused-directive report, so a directive that" >&2
    echo "  stops matching is invisible forever: keep them narrow." >&2
    exit 1
    ;;
  *)
    echo "lint-shell: shellcheck exited $rc, which is neither 0 (clean) nor 1 (findings)." >&2
    echo "  2 = a file could not be read, 3 = no files were given, 4 = bad usage." >&2
    echo "  This is an INSTRUMENT failure over $# paths, not a lint result." >&2
    exit 2
    ;;
esac
