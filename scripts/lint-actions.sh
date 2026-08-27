#!/bin/sh
# Gate the GitHub Actions workflows on actionlint + zizmor (issue: workflow linting).
#
# usage: scripts/lint-actions.sh [workflows-dir]     default: .github/workflows
#
# WHY TWO TOOLS, ONE GATE. yamllint (lint:yaml) checks a workflow file's YAML
# structure and shellcheck (lint:shell) checks tracked *.sh -- but NEITHER sees the
# shell embedded in a workflow `run:` scalar, nor the Actions-specific hazards
# (bad `${{ }}` contexts, `uses:` pinning, credential persistence). actionlint
# covers the first class (it feeds every `run:` block to shellcheck and validates
# expressions/contexts); zizmor covers the second (supply-chain + injection audits).
# They live in ONE recipe because they share a scope, an install story, and this
# fail-open/REQUIRED contract; keeping them together is the same seam gate:repo
# already holds for shell+yaml+formula+secrets.
#
# 🔴 ACTIONLINT RUNS ITS EMBEDDED SHELLCHECK AT --severity=warning, NOT THE DEFAULT.
# The default surfaces `info` (e.g. SC2016 on a literal backtick in a PR-body
# string), which lint:shell deliberately does NOT gate on -- its header records
# "SC2086 is info, so it does not fire at this gate's --severity=warning". Aligning
# the two here means a shell construct is judged the SAME whether it sits in a *.sh
# file or a workflow `run:` block; without it, the two gates would disagree about
# what a finding is, which is exactly the divergence this repo keeps closing.
#
# 🔴 ZIZMOR RUNS --offline AND GATES ON --min-confidence=high, AND BOTH ARE LOAD-
# BEARING, MEASURED CHOICES:
#   * --offline: the network audits (known-vulnerable-actions, impostor-commit,
#     ref-confusion, stale-action-refs) need a GitHub API token and would make the
#     gate's verdict depend on network + upstream state -- non-deterministic, the
#     opposite of assert-worker-tag-decoupled.sh's "OFFLINE, AND NON-VACUOUS". They
#     are covered instead by the SHA pins + Renovate (helpers:pinGitHubActionDigests).
#   * --min-confidence=high: after SHA-pinning every `uses:`, the only High-confidence
#     findings are `unpinned-uses` (which now gates any FUTURE unpinned action) and
#     High-confidence `template-injection` (a `${{ }}` context that can carry
#     attacker input into a shell). The remaining audits on this repo -- `artipacked`
#     (Medium/Low: persist-credentials on checkout) and Low-confidence
#     template-injection -- are REPORTED by a bare `zizmor` run but are NOT gated,
#     the same severity-staging this repo uses for golangci's warn tier. Lowering
#     this threshold is a deliberate decision that owes fixing the backlog first.
#
# 🔴 TOOL ABSENT IS A SKIP, NOT A FAILURE (fail-open) -- same argument as
# lint-yaml.sh: gate:repo runs FIRST inside `task gate`, so a hard failure here
# would stop every component gate from running on a contributor laptop that lacks
# these tools. CI must NOT take the skip: UZI_LINT_ACTIONS_REQUIRED (set by the
# lint-repo job ON THE TASK LINE) and CI each turn the skip into exit 2. The CI job
# installs the PINNED versions; a local run uses whatever is on PATH and the
# authoritative verdict is CI's (the lint-yaml.sh precedent -- no in-wrapper version
# pin, because the gate that counts installs a known one).
#
# EXIT CODES (the convention fmt-check:api, lint:api, lint-yaml.sh set):
#     2 = the instrument is broken (or a REQUIRED run found a tool missing)
#     1 = there are findings
#     0 = clean, or a loud banner-printed SKIP (locally only)
# `task`'s own rc is 201 for any non-zero.
set -eu

DIR="${1:-.github/workflows}"

ROOT="$(git rev-parse --show-toplevel)" || {
  echo "lint-actions: not inside a git work tree" >&2
  exit 2
}
cd "$ROOT" || exit 2

[ -d "$DIR" ] || { echo "lint-actions: no workflows dir at $DIR" >&2; exit 2; }

# Read tolerantly: a guard whose failure mode is to switch itself off must not be
# picky about spelling (see lint-yaml.sh).
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

# Absent-tool handling: fail-closed when REQUIRED, else a banner-printed skip.
missing() {
  _tool="$1"; _how="$2"
  if required; then
    echo "lint-actions: no $_tool on PATH, and this run is REQUIRED (UZI_LINT_ACTIONS_REQUIRED and/or CI is set)." >&2
    echo "  In CI this means the lint-repo job no longer installs it." >&2
    exit 2
  fi
  echo "lint-actions: ================================================================"
  echo "lint-actions: SKIPPED -- workflows were NOT linted ($_tool is not on PATH)."
  echo "lint-actions: This is FAIL-OPEN and deliberate: gate:repo runs FIRST inside"
  echo "lint-actions: \`task gate\`, so failing here would stop every component gate."
  echo "lint-actions: CI sets UZI_LINT_ACTIONS_REQUIRED, so workflows ARE linted on"
  echo "lint-actions: every PR. To check here: $_how"
  echo "lint-actions: ================================================================"
  exit 0
}

command -v actionlint >/dev/null 2>&1 || missing actionlint "download actionlint (github.com/rhysd/actionlint) or 'brew install actionlint'."
command -v zizmor      >/dev/null 2>&1 || missing zizmor      "'pipx install zizmor' / 'uvx zizmor' / 'brew install zizmor'."

fail=0

# --- actionlint: expressions/contexts + every run: block through shellcheck --------
# --severity=warning aligns the embedded shellcheck with lint:shell (see header).
# actionlint exits 0 clean / 1 findings / other = its own error (broken instrument).
al_rc=0
SHELLCHECK_OPTS="--severity=warning" actionlint "$DIR"/*.yml || al_rc=$?
case "$al_rc" in
  0) echo "lint-actions: actionlint clean over $DIR" ;;
  1) echo "lint-actions: actionlint findings (see above)." >&2; fail=1 ;;
  *) echo "lint-actions: actionlint exited $al_rc (neither clean nor findings) -- instrument failure." >&2; exit 2 ;;
esac

# --- zizmor: supply-chain + injection audits, offline, high-confidence gate --------
# zizmor's exit code is 0 = no findings, or 10-14 = findings keyed on the HIGHEST
# finding's SEVERITY (10 unknown [<=1.13], 11 informational, 12 low, 13 medium, 14
# high); any OTHER non-zero is a run error. Matching only 14 is wrong: --min-confidence
# filters on CONFIDENCE, a separate axis, so a high-confidence finding of medium
# severity exits 13 -- which a 14-only match would mislabel as a broken instrument.
zz_rc=0
zizmor --offline --no-progress --min-confidence=high "$DIR" || zz_rc=$?
case "$zz_rc" in
  0)                    echo "lint-actions: zizmor clean over $DIR (--offline --min-confidence=high)" ;;
  10|11|12|13|14)       echo "lint-actions: zizmor findings at high confidence (see above)." >&2; fail=1 ;;
  *)                    echo "lint-actions: zizmor exited $zz_rc (not 0 or a 10-14 finding code) -- instrument failure." >&2; exit 2 ;;
esac

if [ "$fail" -ne 0 ]; then
  echo "lint-actions: workflow linting FAILED." >&2
  exit 1
fi
echo "lint-actions: OK -- actionlint + zizmor clean over $DIR."
exit 0
