#!/bin/sh
# Gate every tracked YAML file on yamllint --strict (PRD #103 M5).
#
# usage: scripts/lint-yaml.sh <config-path>     e.g. scripts/lint-yaml.sh .yamllint
#
# A SCRIPT, NOT AN INLINE `cmds:` LINE, for the reason recorded in full at
# scripts/lint-shell.sh: M5 adds shellcheck, so a committed script is linted and an
# inline Taskfile recipe is not.
#
# 🔴 `--strict` IS THE WHOLE GATE AND IT IS DELIBERATELY NOT AN ARGUMENT.
# Measured 2026-08-03 at yamllint 1.38.0 over this exact scope: WITHOUT it the run
# printed three findings and exited 0, because yamllint puts most style rules at
# `warning` severity and warnings do not set the exit code. A caller must not be
# able to turn that off from the Taskfile, so the config path is the argument and
# the flag is not. The config path is the argument because it is the thing a reader
# must be able to see moving.
#
# 🔴 AND `--strict` INVERTS THIS REPO'S EXIT CONVENTION, which is why this wrapper
# translates rather than passes yamllint's status through. Measured, same version:
#
#     clean                       0
#     ERROR-severity findings     1
#     warnings only, --strict     2      <- "findings", not "instrument broken"
#     no file arguments at all    2      <- usage error, SAME STATUS
#     -c pointing at a missing file
#                                 1 + a Python traceback   <- reads as "findings"
#     -c pointing at a malformed config
#                               255
#
# Two of those collide with the meaning this repo assigns to a status, and each is
# closed by a PRE-FLIGHT rather than by reading the number more carefully:
#   * rc=2 is "warnings" AND "you gave me no files". The non-empty assertion below
#     is what makes the surviving reading unambiguous.
#   * rc=1 is "errors" AND "your config does not exist". The existence assertion
#     below is what makes the surviving reading unambiguous.
# Neither guard is defensive padding; delete one and this script starts reporting
# an instrument failure as a lint result.
#
# 🔴 TOOL ABSENT IS A SKIP, NOT A FAILURE -- and yamllint is the worse case of the
# three repo-wide checks, which is why this is not symmetry for its own sake. It is
# a Python package most contributors will not have, where shellcheck at least ships
# in brew. `gate:repo` runs FIRST inside `task gate`, so a hard failure here means a
# contributor cannot run ANY component gate: gate:api, gate:web and the rest never
# execute. PRD #103 Decision 2 -- a gate people cannot run is a gate that stops
# being run -- which is the same argument that produced amendment #6 for
# `lint:formula`, applied to the tool it bites hardest.
#
# NO VERSION PIN HERE, unlike lint-shell.sh, and the asymmetry is measured rather
# than an oversight: 1.37.1 (what CI's image installs) and 1.38.0 (what brew
# installs) were both run against the shipped `.yamllint` over this exact scope and
# both return zero findings. shellcheck's two releases genuinely disagree about this
# repo; yamllint's do not.
#
# 🔴 THE SKIP IS FAIL-OPEN, SO CI MUST NOT TAKE IT -- two independent signals,
# `UZI_LINT_YAML_REQUIRED` (assigned by `lint:repo` ON THE SCRIPT LINE, never in a
# job `variables:` block, which pipeline- and project-level variables outrank) and
# `CI`. Read lint-shell.sh's header for the full argument; it is the same guard.
#
# EXIT CODES (the convention `fmt-check:api`, `lint:api` and deadcode-gate.sh set):
#     2 = the instrument is broken   1 = there are findings   0 = clean, or a loud
#     banner-printed SKIP (locally only)
# `task`'s own rc is 201 for all of them.
set -eu

CONFIG="${1:-}"

if [ -z "$CONFIG" ]; then
  echo "usage: scripts/lint-yaml.sh <config-path>" >&2
  echo "  e.g. scripts/lint-yaml.sh .yamllint" >&2
  exit 2
fi

# Run from the repo root whatever the caller's directory -- `git ls-files` from a
# subdirectory silently narrows to that subtree. See lint-shell.sh.
ROOT="$(git rev-parse --show-toplevel)" || {
  echo "lint-yaml: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || exit 2

# Identical to lint-shell.sh's and lint-formula.sh's, deliberately duplicated:
# these scripts are standalone by design and a shared `source`d library would put
# the gate's fail-closed property behind a file-resolution step. Read tolerantly
# because a guard whose failure mode is to switch itself off must not be picky
# about spelling -- an exact-match-on-`1` form disarms silently on `true`, `yes`
# or a trailing space.
truthy() {
  case "${1:-}" in
    ''|0|[fF]alse|[fF]ALSE|[nN]o|[nN]O|[oO]ff|[oO]FF) return 1 ;;
    *) return 0 ;;
  esac
}
required() {
  truthy "${UZI_LINT_YAML_REQUIRED:-}" && return 0
  truthy "${CI:-}" && return 0
  return 1
}

# 🔴 IS THE TOOL EVEN HERE? Asserted BEFORE anything else, and not only for the
# skip: without this pre-flight a missing yamllint reaches the invocation, `sh`
# returns 127, and this script's own error path reports it as
# "yamllint exited 127 … 255 is an invalid config" -- the right exit code with a
# message pointing at the wrong cause, which is carry-forward item 3's
# fail-loud-but-misleading class.
if ! command -v yamllint >/dev/null 2>&1; then
  if required; then
    echo "lint-yaml: no yamllint on PATH, and this run is REQUIRED" >&2
    echo "  (UZI_LINT_YAML_REQUIRED and/or CI is set)." >&2
    echo "  In CI this means the job image no longer installs it; the skip this" >&2
    echo "  replaces exists for contributor laptops only." >&2
    exit 2
  fi
  echo "lint-yaml: ================================================================"
  echo "lint-yaml: SKIPPED -- NO TRACKED YAML FILE WAS CHECKED."
  echo "lint-yaml: yamllint is not on PATH. It is a Python package, so most"
  echo "lint-yaml: contributors will not have it until they ask for it."
  echo "lint-yaml:"
  echo "lint-yaml: This is FAIL-OPEN and deliberate. gate:repo runs FIRST inside"
  echo "lint-yaml: \`task gate\`, so failing here would stop gate:api, gate:web and"
  echo "lint-yaml: every other component gate from running at all. CI sets"
  echo "lint-yaml: UZI_LINT_YAML_REQUIRED, so your YAML IS checked on every MR."
  echo "lint-yaml:"
  echo "lint-yaml: To check it here: \`brew install yamllint\` (or pipx/pip install"
  echo "lint-yaml: yamllint). No version pin -- 1.37.1 and 1.38.0 were both measured"
  echo "lint-yaml: against this repo's .yamllint and agree."
  echo "lint-yaml: ================================================================"
  exit 0
fi

# EXPLICIT `-c`, NEVER DISCOVERY, AND THE FILE'S EXISTENCE IS ASSERTED HERE.
# yamllint auto-discovers `.yamllint` from the working directory, so a stray config
# could otherwise govern the gate silently. And `-c <missing>` exits 1 with a Python
# traceback, which this script would report as "there are findings" -- a red for the
# wrong reason, sending the reader to the YAML instead of to the config.
if [ ! -f "$CONFIG" ]; then
  echo "lint-yaml: config not found: $CONFIG" >&2
  echo "  Restore it from git. A missing config must not read as a lint result:" >&2
  echo "  yamllint exits 1 for BOTH, and 1 is this repo's 'there are findings'." >&2
  exit 2
fi

# 🔴 SCOPE: EVERY TRACKED *.yml/*.yaml EXCEPT deploy/chart/templates/.
#
# WIDER THAN THE PRD ASKED FOR, deliberately. The PRD names `.gitlab-ci.yml` and
# `deploy/values/` (3 files); this is 10, and the extra ones are the ones with
# stakes: `deploy/chart/values.yaml` and `deploy/chart/Chart.yaml` ship to Harbor
# in the OCI chart and carry the Model-B version/appVersion pair, and `Taskfile.yml`
# is the one file in this repo with a RECORDED YAML parse failure (a bare `: ` inside
# a `desc:`, task rc=109).
#
# 🔴 THE EXCLUSION IS NOT A CONVENIENCE. Helm templates are not YAML: they are Go
# templates that PRODUCE YAML. Measured 2026-08-03, `yamllint deploy/chart/templates/`
# returns 239 findings and the FIRST is a hard syntax error
# ("web-deployment.yaml:1:3: expected the node content, but found '-'"), because the
# file opens with `{{- if ... }}`. There is nothing to fix there and no config that
# makes it lintable. What DOES check those files is `scripts/assert-chart-render.sh`
# in the `helm_chart` job, which renders the chart and asserts one `kind:` per
# document -- a stronger check than yamllint could give, and the one that exists
# because a `*/ -}}` glued a `---` onto the previous value and deleted a
# ServiceAccount from the chart for days.
#
# The exclusion is a git PATHSPEC rather than a grep filter on purpose: `grep -v`
# returns 1 when it eats every line, which is indistinguishable from "no matches",
# and this host's grep is ugrep (negated classes misbehave in POSIX modes). Letting
# git do the filtering removes the instrument entirely.
FILES="$(git ls-files -- '*.yml' '*.yaml' ':(exclude)deploy/chart/templates/*')" || exit 2

if [ -z "$FILES" ]; then
  echo "lint-yaml: the tracked-YAML pathspec matched nothing under $ROOT." >&2
  echo "  An empty scope is an instrument failure, never a clean run -- and it is the" >&2
  echo "  one that matters most here: yamllint with no file arguments exits 2, which" >&2
  echo "  is also its 'warnings only' status under --strict." >&2
  exit 2
fi

# Split on NEWLINE ONLY, globbing off -- see lint-shell.sh for why the default IFS
# is wrong here (git does not quote spaces) and why `set -f` is needed.
oldIFS="${IFS-}"
IFS='
'
set -f
# shellcheck disable=SC2086  # deliberate word split; see the paragraph above.
set -- $FILES
set +f
IFS="$oldIFS"

rc=0
yamllint --strict -c "$CONFIG" -- "$@" || rc=$?

case "$rc" in
  0)
    echo "lint-yaml: clean under $CONFIG ($# tracked YAML files, --strict)"
    exit 0
    ;;
  1|2)
    # 1 = error-severity findings, 2 = warning-severity only (which --strict makes
    # fatal). Both are "there are findings" in this repo's convention; the printed
    # output above already names the rule and the line, which is the part that
    # matters. Neither can be a usage or config failure here: both guards above
    # ran first.
    echo "lint-yaml: findings under $CONFIG over $# tracked YAML files (yamllint rc=$rc)." >&2
    echo "  Fix the YAML. Adding a rule to $CONFIG is a DELIBERATE SUPPRESSION for" >&2
    echo "  every tracked file at once and owes a written reason in that file --" >&2
    echo "  read its header before reaching for one." >&2
    exit 1
    ;;
  *)
    echo "lint-yaml: yamllint exited $rc over $# paths, which is neither clean (0)" >&2
    echo "  nor findings (1|2). 255 is an invalid config. This is an INSTRUMENT" >&2
    echo "  failure, not a lint result." >&2
    exit 2
    ;;
esac
