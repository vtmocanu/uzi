#!/bin/sh
# Shellcheck the embedded `run:` scripts of composite actions (issue #948).
#
# usage: scripts/lint-actions-embedded.sh <severity> <exact-shellcheck-version>
#        e.g. scripts/lint-actions-embedded.sh warning 0.11.0
#
# THE ONE CLASS lint:shell AND lint:actions BOTH MISS. `lint:shell` shellchecks
# `git ls-files '*.sh'` (plus shebang-shell scripts), and `lint:actions`
# (scripts/lint-actions.sh) scopes actionlint/zizmor to `.github/workflows`. A
# composite action's `run:` block is bash but lives in
# `.github/actions/**/action.yml`, which is neither a `*.sh` file nor a workflow,
# so it is gated by NOTHING. This closes that gap: it extracts each composite
# `run:` script and feeds it to the SAME pinned shellcheck lint:shell uses, at the
# SAME `--severity=warning`, so a shell construct is judged identically whether it
# sits in a tracked `*.sh` file, a workflow `run:` block (via the actionlint
# embedded shellcheck, aligned in lint-actions.sh) or a composite `run:` block.
#
# 🔴 SHELL IS PER-STEP, NOT HARDCODED. Composite steps declare `shell:` (bash by
# convention here, but `sh` is legal and carries a different dialect). The step's
# own `shell:` is mapped to shellcheck's `-s` so a `shell: sh` step is not judged
# by bash rules (which would let a genuine `sh` bashism through) and a `shell: bash`
# step is not falsely flagged for bashisms it is entitled to use (e.g. `declare -A`,
# which install-cosign relies on). Non-shell steps (pwsh/powershell/python/cmd, or
# a `uses:` step with no `run:`) are skipped: shellcheck cannot lint them.
#
# 🔴 SCOPE IS THE INDEX, NOT A GLOB WALK. Files come from `git ls-files`, so a NEW
# `.github/actions/**/action.yml` is out of scope until it is `git add`ed -- the
# same property (and the same local/CI-divergence caveat) lint-shell.sh documents:
# a fresh untracked composite action can be green here and red in CI, so `git add`
# before trusting a green. It is not a hole: nothing reaches CI or a reviewer
# without being in the index. An EMPTY scope is not treated as an instrument
# failure here (unlike lint:shell, which knows this repo always has `*.sh` files):
# a repo may legitimately carry zero composite actions, so no composite action.yml
# is a clean pass, not an error.
#
# 🔴 TWO TOOLS, SAME FAIL-OPEN CONTRACT AS THE OTHER gate:repo LINTERS. This needs
# both shellcheck (the extraction target) and yq (mikefarah v4, to read the YAML). Either
# absent on a contributor laptop is a banner-printed SKIP, exit 0, for the same
# reason lint-shell/lint-yaml/lint-actions skip: gate:repo runs FIRST inside
# `task gate`, so a hard failure here would stop every component gate from running.
# CI must NOT take the skip: UZI_LINT_ACTIONS_REQUIRED (set by the lint-repo job)
# and CI each turn the skip into exit 2. shellcheck's version is asserted EXACTLY
# (lint-shell's argument: 0.10.0 and 0.11.0 are different instruments); yq carries
# no pin -- it only reads YAML, whose output is stable across versions, the same
# reasoning lint-yaml.sh applies to yamllint. The lint-repo runner (ubuntu-latest)
# provides yq; a missing yq THERE means the runner image changed under us and the
# REQUIRED path says so loudly.
#
# EXIT CODES (the convention fmt-check:api, lint:api, lint-shell.sh set):
#     2 = the instrument is broken (bad usage, wrong shellcheck version, unreadable
#         action.yml, or a REQUIRED run found a tool missing)
#     1 = there are findings
#     0 = clean, or a loud banner-printed SKIP (locally only)
# `task`'s own rc is 201 for any non-zero.
set -eu

SEVERITY="${1:-}"
WANT_VERSION="${2:-}"

if [ -z "$SEVERITY" ] || [ -z "$WANT_VERSION" ]; then
  echo "usage: scripts/lint-actions-embedded.sh <severity> <exact-shellcheck-version>" >&2
  echo "  severity: error|warning|info|style" >&2
  echo "  e.g. scripts/lint-actions-embedded.sh warning 0.11.0" >&2
  exit 2
fi

# Validate the severity HERE so a typo dies as "instrument broken" (2) rather than
# the shellcheck-native exit 4. Passed as an argument (not hardcoded) so `task`'s echo shows
# the gate's threshold -- for this check the threshold is part of the gate.
case "$SEVERITY" in
  error|warning|info|style) ;;
  *)
    echo "lint-actions-embedded: unknown severity '$SEVERITY' (want error|warning|info|style)" >&2
    exit 2
    ;;
esac

# Run from the repo root whatever the caller's directory: `git ls-files` from a
# subdirectory lists only that subtree, silently narrowing scope. Fail closed when
# there is no repo at all.
ROOT="$(git rev-parse --show-toplevel)" || {
  echo "lint-actions-embedded: not inside a git work tree" >&2
  exit 2
}
cd "$ROOT" || exit 2

# Is a missing tool a hard failure? Two independent signals, read tolerantly -- the
# same guard lint-shell.sh/lint-yaml.sh/lint-actions.sh use (a guard whose failure
# mode is to switch itself off must not be picky about spelling).
truthy() {
  case "${1:-}" in
    ''|0|[fF]alse|[fF]ALSE|[nN]o|[nN]O|[oO]ff|[oO]FF) return 1 ;;
    *) return 0 ;;
  esac
}
required() {
  truthy "${UZI_LINT_ACTIONS_REQUIRED:-}" && return 0
  truthy "${CI:-}" && return 0
  return 1
}

missing() {
  _tool="$1"; _how="$2"
  if required; then
    echo "lint-actions-embedded: no $_tool on PATH, and this run is REQUIRED (UZI_LINT_ACTIONS_REQUIRED and/or CI is set)." >&2
    echo "  In CI this means the lint-repo job / runner image no longer provides it." >&2
    exit 2
  fi
  echo "lint-actions-embedded: ============================================================"
  echo "lint-actions-embedded: SKIPPED -- composite actions were NOT linted ($_tool not on PATH)."
  echo "lint-actions-embedded: This is FAIL-OPEN and deliberate: gate:repo runs FIRST inside"
  echo "lint-actions-embedded: \`task gate\`, so failing here would stop every component gate."
  echo "lint-actions-embedded: CI sets UZI_LINT_ACTIONS_REQUIRED, so they ARE linted on every"
  echo "lint-actions-embedded: PR. To check here: $_how"
  echo "lint-actions-embedded: ============================================================"
  exit 0
}

command -v shellcheck >/dev/null 2>&1 || missing shellcheck "install the pinned shellcheck (see lint:shell / the lint-repo job)."
command -v yq         >/dev/null 2>&1 || missing yq         "install mikefarah yq v4 ('brew install yq')."

# 🔴 yq MUST BE MIKEFARAH v4, AND A WRONG FLAVOUR IS exit 2 (NOT a skip). Two YAML
# tools answer to `yq`: mikefarah (Go, whose expression syntax this script uses) and
# python-yq (a jq wrapper with a different CLI). A present-but-wrong `yq` would run and
# misread the manifests silently, so this asserts the flavour rather than trusting the
# name -- the same "present at the wrong build is a hard fail" split lint-shell.sh draws
# for shellcheck's version. It is NOT an exact pin: yq only READS YAML, whose output is
# stable across v4 patches (lint-yaml.sh's reasoning for leaving yamllint unpinned), so
# the assertion is the major line (mikefarah, v4), not a literal version. This guard
# runs both locally and in CI, so it stays in lockstep with no workflow edit -- the CI
# job needs no yq step; gate:repo already carries UZI_LINT_ACTIONS_REQUIRED.
YQ_VERSION_RAW="$(yq --version 2>/dev/null)" || {
  echo "lint-actions-embedded: 'yq --version' failed." >&2
  exit 2
}
case "$YQ_VERSION_RAW" in
  *mikefarah*version\ v4.*|*mikefarah*version\ 4.*) ;;
  *)
    echo "lint-actions-embedded: need mikefarah yq v4; got: $YQ_VERSION_RAW" >&2
    echo "  (python-yq or an older mikefarah use a different CLI and would misread the" >&2
    echo "  composite manifests. Install mikefarah yq v4: https://github.com/mikefarah/yq) (exit 2)" >&2
    exit 2
    ;;
esac

# 🔴 VERSION ASSERT (lint-shell.sh's argument: an exact pin, not a floor -- 0.10.0
# does not emit SC3067 at all, so a different build is a different gate). Assignment,
# not a pipe: `$?` after a pipe reads the LAST command, never shellcheck's.
SC_VERSION_RAW="$(shellcheck --version 2>/dev/null)" || {
  echo "lint-actions-embedded: 'shellcheck --version' failed." >&2
  exit 2
}
SC_VERSION="$(printf '%s\n' "$SC_VERSION_RAW" | sed -n 's/^version: //p')"
if [ -z "$SC_VERSION" ]; then
  echo "lint-actions-embedded: could not parse a version out of 'shellcheck --version'." >&2
  exit 2
fi
if [ "$SC_VERSION" != "$WANT_VERSION" ]; then
  echo "lint-actions-embedded: shellcheck $SC_VERSION is on PATH; this gate is pinned to $WANT_VERSION." >&2
  echo "  Same instrument as lint:shell -- align them, or the embedded run: blocks would" >&2
  echo "  be judged by a different shellcheck than the tracked *.sh files. (exit 2)" >&2
  exit 2
fi

# Map a composite step's `shell:` onto a shellcheck dialect. Returns non-zero for a
# non-shell (or unrecognised) shell, which the caller reads as "skip this step".
#
# 🔴 `shell:` MAY BE A CUSTOM TEMPLATE, NOT A BARE NAME. GitHub Actions lets a step
# set `shell: bash {0}` (or `/usr/bin/perl {0}`, etc.): the FIRST whitespace-delimited
# token is the command and `{0}` is the script path. So the command is parsed off the
# front and basename'd before matching -- otherwise a legitimate `bash {0}` step would
# fall through to `*)` and be SILENTLY SKIPPED, unlinted, which is the gap this whole
# gate exists to close (CodeRabbit, PR #1070).
sc_dialect() {
  # Take the command token off a possible "cmd [args] {0}" template via PARAMETER
  # EXPANSION, deliberately NOT `set -- $1` word splitting: the caller runs this inside
  # a loop with IFS set to newline, under which a word split would NOT break on the
  # space and "bash {0}" would stay one token and be skipped (measured on PR #1070 --
  # the isolated unit test passed under the default IFS and hid it). `%% *` strips from
  # the first space (GitHub templates use a single space before {0}); `##*/` a path.
  _shcmd="${1:-bash}"
  _shcmd="${_shcmd%% *}"  # "bash {0}" -> "bash"; "bash" -> "bash"
  _shcmd="${_shcmd##*/}"  # "/usr/bin/bash" -> "bash"
  case "$_shcmd" in
    bash)            printf 'bash' ;;
    sh|dash)         printf 'sh'   ;;  # no `dash` dialect exists; sh is the POSIX one
    ksh)             printf 'ksh'  ;;
    *)               return 1      ;;  # pwsh, powershell, python, cmd, or a null run: step
  esac
}

# Composite action manifests, from the INDEX. Both spellings; git does not quote
# spaces, so read line by line.
FILES="$(git ls-files -- '.github/actions/**/action.yml' '.github/actions/**/action.yaml')" || exit 2

if [ -z "$FILES" ]; then
  echo "lint-actions-embedded: no tracked composite action manifests under .github/actions/."
  echo "  (A repo may legitimately carry none -- clean pass, not an error.)"
  exit 0
fi

fail=0
checked=0
tmp="$(mktemp)" || { echo "lint-actions-embedded: mktemp failed" >&2; exit 2; }
# shellcheck disable=SC2064  # expand $tmp NOW: it never changes and the file must
# be removed even if a later mktemp reassignment were added.
trap "rm -f '$tmp'" EXIT INT TERM

oldIFS="${IFS-}"
IFS='
'
# `set -f` (noglob) so a tracked path containing a `*`/`?` is not re-expanded
# against the working tree when the unquoted `$FILES` is split -- lint-shell.sh's
# idiom, kept here for parity even though composite-action paths rarely hit it.
set -f
for action in $FILES; do
  [ -n "$action" ] || continue

  # Only composite actions carry embedded shell; a node/docker action has no
  # steps[].run. Guard on `using` so a malformed/absent field is an instrument
  # error rather than a silent skip.
  using="$(yq -r '.runs.using // ""' "$action" 2>/dev/null)" || {
    echo "lint-actions-embedded: could not read $action (yq failed)." >&2
    fail=2
    continue
  }
  [ "$using" = "composite" ] || continue

  nsteps="$(yq -r '.runs.steps | length' "$action" 2>/dev/null)" || nsteps=0
  case "$nsteps" in ''|*[!0-9]*) nsteps=0 ;; esac

  i=0
  while [ "$i" -lt "$nsteps" ]; do
    # A step with no `run:` (a `uses:` step) yields the string "null" under -r.
    run="$(yq -r ".runs.steps[$i].run // \"\"" "$action" 2>/dev/null)"
    if [ -z "$run" ]; then
      i=$((i + 1))
      continue
    fi
    shell="$(yq -r ".runs.steps[$i].shell // \"bash\"" "$action" 2>/dev/null)"
    name="$(yq -r ".runs.steps[$i].name // \"step $i\"" "$action" 2>/dev/null)"

    if dialect="$(sc_dialect "$shell")"; then
      printf '%s\n' "$run" > "$tmp"
      rc=0
      # `--norc` so no stray .shellcheckrc up the tree governs this gate (lint-shell's
      # argument). The finding lines reference $tmp; the banner below names the source.
      shellcheck --norc -s "$dialect" --severity="$SEVERITY" -- "$tmp" || rc=$?
      case "$rc" in
        0) : ;;
        1)
          echo "lint-actions-embedded: findings in $action -> step '$name' (shell: $shell)." >&2
          fail=1
          ;;
        *)
          echo "lint-actions-embedded: shellcheck exited $rc on $action -> step '$name' (instrument failure)." >&2
          fail=2
          ;;
      esac
      checked=$((checked + 1))
    fi
    i=$((i + 1))
  done
done
set +f
IFS="$oldIFS"

if [ "$fail" = "2" ]; then
  echo "lint-actions-embedded: instrument failure (see above)." >&2
  exit 2
fi
if [ "$fail" = "1" ]; then
  echo "lint-actions-embedded: composite-action shell linting FAILED." >&2
  exit 1
fi
echo "lint-actions-embedded: OK -- shellcheck $SC_VERSION clean over $checked composite run: block(s) at severity=$SEVERITY."
exit 0
