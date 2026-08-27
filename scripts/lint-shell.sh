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
# sha256-verified tarball (see the `lint-repo` job in .github/workflows/ci.yml);
# the same `darwin.aarch64` asset exists, so a contributor can install the identical build.
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
# THE NON-EMPTY ASSERTION HERE IS DEFENCE IN DEPTH, NOT THE THING HOLDING THE LINE
# -- and lint-yaml.sh's identically-shaped guard IS load-bearing, so do not read
# across. Measured by deleting each (parse-checked first): without its guard THIS
# script still exits 2, because shellcheck's rc 3 for "no files" falls through to
# the catch-all arm below. Without its guard `lint-yaml.sh` exits 1 -- "there are
# findings" -- over a usage error, because yamllint's usage status is 2 and
# `--strict` also spends 2 on warnings-only. Same idiom, two different loads.
# The bare `| xargs` one-liner above remains fail-open regardless; that is what
# both guards exist for.
#
# 🔴 TOOL ABSENT IS A SKIP; TOOL PRESENT AT THE WRONG VERSION IS A HARD FAIL. The
# distinction is the whole design and the two halves answer different questions.
# The version pin above is about someone who HAS shellcheck and would get a
# different gate from CI's; it says nothing about someone who has none. `gate:repo`
# runs FIRST inside `task gate`, so a hard fail on a missing tool means a
# contributor cannot run ANY component gate -- `gate:api`, `gate:web` and the rest
# never execute. That is PRD #103 Decision 2's "a gate people cannot run is a gate
# that stops being run", and it is the same argument that produced amendment #6 for
# `lint:formula`. So: absent -> loud skip, exit 0. Wrong version -> exit 2.
#
# 🔴 THE SKIP IS FAIL-OPEN, SO CI MUST NOT TAKE IT, and the guard is deliberately
# TWO independent signals rather than one variable in one place:
#   * `UZI_LINT_SHELL_REQUIRED` set to anything but a falsy value, which the
#     `lint-repo` job assigns as a step `env:` in .github/workflows/ci.yml. The
#     retired GitLab pipeline instead set it ON THE SCRIPT LINE, not in a job
#     `variables:` block, because GitLab ranked PIPELINE variables (manual run,
#     trigger, schedule) at 3 and PROJECT variables at 4 against job variables at 8,
#     so a same-named manual-pipeline variable would DISPLACE the job's value and
#     the job would go green having checked nothing. That precedence hazard was
#     GitLab-specific; the two-signal guard is kept regardless as defence in depth.
#   * `CI`, which GitLab (and every other runner worth naming) sets. This exists
#     because the defect the audit found was not the variable's VALUE, it was that
#     the whole guarantee rested on one line in one file that nothing reddens if
#     deleted.
# Both are read tolerantly -- `1`, `true`, `yes`, `on`, or anything else non-falsy
# arms them -- because the first version accepted ONLY the literal `1`, so `true`,
# `yes` and a trailing space all disarmed it SILENTLY. A guard whose failure mode is
# to switch itself off must not be picky about spelling.
#
# EXIT CODES, using the convention `fmt-check:api`, `lint:api` and
# `scripts/deadcode-gate.sh` already set:
#
#     2 = the instrument is broken (not a git repo, empty scope, unreadable file,
#         bad usage, unknown severity, WRONG shellcheck version, or absent-while-
#         required)
#     1 = there are findings
#     0 = clean -- or a loud, banner-printed SKIP, locally only
#
# 🔴 AND WHAT THIS GATE STRUCTURALLY CANNOT SEE, stated because "shellcheck now
# gates the worker entrypoint" reads as covering injection and does not: SC2086
# (unquoted expansion) is INFO severity, so the shipped `--severity=warning`
# threshold excludes that whole class -- including in agent/templates/entrypoint.sh.
# The tree has zero SC2086 today. Tightening to `info` is NOT free and is
# deliberately deferred to its own follow-up: the `-S style` tail is 11 findings,
# and they are 6x SC2016 (deliberate single-quoting in run-e2e.sh), 2x SC2001 and
# 1x SC2329 -- all benign or intentional. Buying the SC2086 class at the price of
# eleven new suppressions is a policy argument that deserves to be made on its own
# merits, not smuggled in behind a threshold change.
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

# Is a missing tool a hard failure? Two independent signals, both read tolerantly.
# See the header for why this is not one variable in one place, and why "only the
# literal 1 arms it" was itself the defect. IDENTICAL in lint-yaml.sh and
# lint-formula.sh, deliberately duplicated: these scripts are standalone by design
# (deadcode-gate.sh's shape), and a shared `source`d library would put the gate's
# fail-closed property behind a file-resolution step.
truthy() {
  case "${1:-}" in
    ''|0|[fF]alse|[fF]ALSE|[nN]o|[nN]O|[oO]ff|[oO]FF) return 1 ;;
    *) return 0 ;;
  esac
}
required() {
  truthy "${UZI_LINT_SHELL_REQUIRED:-}" && return 0
  truthy "${CI:-}" && return 0
  return 1
}

# 🔴 TOOL ABSENT -> SKIP (or exit 2 when required). NOT the same branch as a
# version mismatch below: see the header.
if ! command -v shellcheck >/dev/null 2>&1; then
  if required; then
    echo "lint-shell: no shellcheck on PATH, and this run is REQUIRED" >&2
    echo "  (UZI_LINT_SHELL_REQUIRED and/or CI is set)." >&2
    echo "  In CI this means the job image no longer installs the pinned tarball;" >&2
    echo "  the skip this replaces exists for contributor laptops only." >&2
    exit 2
  fi
  echo "lint-shell: ================================================================"
  echo "lint-shell: SKIPPED -- NO TRACKED SHELL SCRIPT WAS CHECKED."
  echo "lint-shell: shellcheck is not on PATH. This gate is pinned to EXACTLY"
  echo "lint-shell: $WANT_VERSION, because 0.10.0 and 0.11.0 disagree about this repo"
  echo "lint-shell: (0.10.0 does not emit SC3067 at all), so any other build would be"
  echo "lint-shell: a different gate rather than the same one running late."
  echo "lint-shell:"
  echo "lint-shell: This is FAIL-OPEN and deliberate. gate:repo runs FIRST inside"
  echo "lint-shell: \`task gate\`, so failing here would stop gate:api, gate:web and"
  echo "lint-shell: every other component gate from running at all. CI sets"
  echo "lint-shell: UZI_LINT_SHELL_REQUIRED, so your scripts ARE checked on every MR."
  echo "lint-shell:"
  echo "lint-shell: To check them here, install the pin:"
  echo "lint-shell:   https://github.com/koalaman/shellcheck/releases/tag/v$WANT_VERSION"
  echo "lint-shell:   (darwin.aarch64 / darwin.x86_64 / linux.x86_64, same build as CI's)"
  echo "lint-shell: ================================================================"
  exit 0
fi

# 🔴 VERSION ASSERT. See the header for the 0.10.0 vs 0.11.0 table and why an EXACT
# match rather than a floor -- and why this is exit 2 while ABSENT is a skip.

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

# 🔴 SCOPE IS THE INDEX PLUS A SHEBANG SCAN, NOT A GLOB, AND THE GLOB'S OWN COMMENT
# WAS WHY. That comment claimed reading the index "stops the scope going stale as
# scripts are added, which a hand-maintained glob list cannot" -- but `'*.sh'` IS a
# glob, and it was already stale when it shipped: `agent/bin/agent-browser` is
# tracked, is `#!/bin/sh`, and is `COPY --chmod=0755`d into /usr/local/bin in BOTH
# agent/templates/base/Dockerfile and agent/templates/jvm/Dockerfile. It is the shim
# every `agent-browser` invocation in every worker pod execs through, and it was
# outside the gate.
#
# That is the SAME CLASS the design used to reject `e2e/*.sh` + `scripts/*.sh` --
# "those globs miss part of the tracked set, INCLUDING the worker container
# entrypoint" -- one notch smaller and inside the fix. An extension is a naming
# convention; a shebang is what the kernel actually honours, so the shebang is the
# thing to enumerate on.
#
# Cheap, because `git grep -l` narrows first: one git process yields the handful of
# tracked files containing a shebang-shaped line ANYWHERE, and only those get their
# real first line read. Measured on this tree: 19 candidates, of which exactly two
# are non-`.sh` with a line-1 shebang -- agent/bin/agent-browser (`#!/bin/sh`, IN)
# and web/scripts/check-docs.mjs (`#!/usr/bin/env node`, OUT). So the interpreter
# filter is load-bearing rather than decorative, and that pair is its positive and
# negative control.
#
# ONE CONSEQUENCE OF READING THE INDEX, and it is the same for lint-yaml.sh: A
# BRAND-NEW SCRIPT IS OUT OF SCOPE UNTIL IT IS `git add`ed. Locally that means a
# fresh untracked file can be green here and red in CI, which is the local/CI
# divergence PRD #103 exists to remove -- so `git add` before you trust a green.
# It is not a hole in the gate: nothing reaches CI, or a reviewer, without being in
# the index. Stated because the symptom (a script that passes and then fails the
# pipeline) reads like a flake.

# Is the first line a shebang naming a SHELL? `set --` inside a function scopes to
# that function's own positionals, so this cannot disturb the caller's file list.
shebang_is_shell() {
  case "${1:-}" in '#!'*) ;; *) return 1 ;; esac
  # shellcheck disable=SC2086  # PRE-ARMED, NOT VACUOUS: SC2086 is `info`, so it does
  # not fire at this gate's `--severity=warning` -- strip-and-restore gives rc=0 and
  # zero hits with or without it. It fires at `-S info`/`-S style`, which is the
  # burn-down deferred to its own follow-up. Deliberate word split, see above.
  set -- ${1#\#!}
  [ "$#" -gt 0 ] || return 1
  _cmd="${1##*/}"
  # `#!/usr/bin/env bash` and `#!/bin/busybox sh` both put the real interpreter in
  # the NEXT word.
  case "$_cmd" in
    env|busybox)
      shift
      [ "$#" -gt 0 ] || return 1
      _cmd="${1##*/}"
      ;;
  esac
  case "$_cmd" in
    sh|bash|dash|ksh|ash|zsh) return 0 ;;
  esac
  return 1
}

FILES="$(git ls-files -- '*.sh')" || exit 2

# `|| true`: git grep exits 1 when nothing matches, which is a legitimate state
# here (it would just mean no extension-less scripts) and must not abort the run.
# Read line by line rather than word-split: git does not quote SPACES in a path,
# so an unquoted `for` over this list would split one path into two.
CANDIDATES="$(git grep -I -l -E '^#!' -- . || true)"
while IFS= read -r _f; do
  [ -n "$_f" ] || continue
  case "$_f" in *.sh) continue ;; esac
  [ -f "$_f" ] || continue
  # `read` returns nonzero on a file with no trailing newline but still assigns,
  # so the status is deliberately ignored and `_first` is tested instead.
  _first=""
  IFS= read -r _first < "$_f" 2>/dev/null || true
  shebang_is_shell "$_first" || continue
  FILES="$FILES
$_f"
done <<CANDIDATE_LIST
$CANDIDATES
CANDIDATE_LIST

if [ -z "$FILES" ]; then
  echo "lint-shell: no tracked shell scripts found under $ROOT." >&2
  echo "  An empty scope is an instrument failure, never a clean run: this repo has" >&2
  echo "  tracked shell scripts, so either the index is broken or the scan is." >&2
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
# shellcheck disable=SC2086  # PRE-ARMED, NOT VACUOUS: SC2086 is `info`, so it does
# not fire at this gate's `--severity=warning` -- strip-and-restore gives rc=0 and
# zero hits with or without it. It fires at `-S info`/`-S style`, which is the
# burn-down deferred to its own follow-up. Deliberate word split, see above.
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
    # 🔴 THIS GLOSS IS THE ONE A READER SEES AT DIAGNOSIS TIME, SO IT MUST NOT BE A
    # PARAPHRASE OF THE HEADER'S. It used to read "3 = no files were given, 4 = bad
    # usage", which widened both halves: 3 is ALSO every unrecognised flag -- so the
    # commonest bad usage, a typo'd flag, returned 3 and this line sent the reader
    # to "no files were given", the one failure the non-empty assertion above
    # guarantees cannot happen. And 4 is not bad usage generally; it is an invalid
    # VALUE for a RECOGNISED option. Re-derived at 0.11.0: unrecognised long flag 3,
    # unrecognised short flag 3, bad --severity value 4, nonexistent file 2,
    # no files 3.
    echo "  2 = a file could not be read; 3 = no files given OR an unrecognised flag;" >&2
    echo "  4 = a recognised flag given an invalid value." >&2
    echo "  This is an INSTRUMENT failure over $# paths, not a lint result." >&2
    exit 2
    ;;
esac
