#!/usr/bin/env bash
# Guard: no NEWLY-ADDED complete provider-token-shaped literal in tracked source,
# even when an inline `//gitleaks:allow` is present (PRD #1120 M3).
#
# usage: scripts/check-token-literals.sh <canary> <canary> [<canary> ...]
#   e.g. scripts/check-token-literals.sh scripts/gitleaks-canary.txt \
#          api/internal/config/gitleaks_canary_test.go
#
# 🔴 WHY THIS EXISTS, AND WHY IT IS NOT gitleaks. A run can be gate-green through
# every milestone and STILL be unpushable at finalize: GitHub Push Protection scans
# every commit against provider-token patterns and refuses the whole push
# (`GH013 … Push cannot contain secrets`). `task scan:secrets` (gitleaks) catches the
# same class, but Push Protection honours NONE of gitleaks' silencers -- not
# `.gitleaks.toml`, not `.gitleaksignore`, not an inline `//gitleaks:allow`. So a
# literal a contributor deliberately allow-listed for gitleaks still bricks the push.
# This check is a SEPARATE, INDEPENDENT source-hygiene gate: it does NOT invoke,
# weaken, or replace gitleaks -- it is an ADDITIONAL check that flags a complete
# provider-token literal wherever it lands, IGNORING every in-file allow directive,
# so the failure reddens CI (`gate:repo`) offline instead of surfacing at the doomed
# push. The authoring-side fix is to ASSEMBLE the fixture from parts at runtime
# (`"glpat-" + "AAAA…"`) so the source never carries a contiguous token shape; the
# assembled form passes here and passes Push Protection (.claude/rules/prds.md,
# "A PRD whose tests need secret-shaped strings").
#
# 🔴 NARROW ALLOWLIST -- THE TWO gitleaks CANARIES, PASSED AS EXPLICIT ARGS. The only
# complete `glpat-`+20 literals that legitimately live in tracked source are the two
# gitleaks liveness canaries (scripts/gitleaks-canary.txt and
# api/internal/config/gitleaks_canary_test.go). They are the allowlist, and they are
# arguments -- not a list baked into this script -- so a reader sees exactly which
# files are exempt, the caller (Taskfile / this echo) shows them moving, and a canary
# that is deleted/moved/renamed trips the "missing" guard below rather than silently
# widening the exemption. Every OTHER tracked file is in scope.
#
# 🔴 ONE PCRE DETECTOR, USED FOR SELF-TEST AND THE MAIN SCAN ALIKE (so the two cannot
# drift), run through git's PCRE2 (`git grep -P`). This host's grep is ugrep, whose
# plain/`-E` negated classes and brace intervals misbehave (root CLAUDE.md), so PCRE
# via git grep is mandatory -- never a plain grep/ugrep. The pattern mirrors gitleaks
# v8.30.1's default provider rules (provenance per alternative in PATTERN below), with
# ONE deliberate divergence: the GitLab `glpat-` alternative carries NO entropy floor,
# so a low-entropy `glpat-` + 20 chars is caught too. That is stricter than gitleaks on
# purpose, because GitHub Push Protection is pattern-based, not entropy-gated.
#
# 🔴 THE PATTERN STRING IS NOT ITSELF A CONTIGUOUS LITERAL, so this script does not
# self-flag when the main scan reaches it: a `[` or `(` follows each token prefix in
# PATTERN, so no `glpat-…`/`ghp_…`/etc. run in this file is a complete literal.
# The main scan proves it: this script (a tracked, in-scope file) stays clean when the
# very same detector is run over its own source.
#
# 🔴 SELF-TEST (positive/negative/canary controls) BEFORE THE MAIN SCAN, because a
# silent pass is the failure mode -- a detector that matched nothing reads identical
# to a clean tree. ONE POSITIVE CONTROL PER DETECTOR ALTERNATIVE builds a complete
# literal for that provider AT RUNTIME (never a contiguous literal in this source) and
# asserts the detector MATCHES it, so a PCRE/pattern regression that blinds a SINGLE
# alternative is caught by name rather than hidden behind the four branches that still
# fire; the negative control writes the ASSEMBLED form (prefix and body as separate
# quoted fragments) and asserts the detector does NOT; and each allowlisted canary is
# asserted to still carry a detectable complete shape. Any control that comes out wrong
# is exit 2 (instrument broken), not a finding. Scratch files live under a `mktemp -d`
# temp dir OUTSIDE the repo (trap-cleaned on EXIT), scanned with `git grep --no-index`
# from inside that dir -- `--no-index` so it reads a file that is not in the index, run
# from within the dir because `git grep --no-index` refuses a path outside the enclosing
# repository.
#
# EXIT CODES (the check-skill-size.sh / check-migration-additive.sh convention):
#     2 = the instrument is broken (bad args, a canary missing/untracked/lost its
#         shape, a self-test control failed, or git grep errored)
#     1 = there are findings (a complete provider-token literal in tracked source
#         outside the allowlist)
#     0 = clean, and all controls confirmed (the canaries remain shape-intact)
# `task`'s own rc is 201 for all of them.
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: scripts/check-token-literals.sh <canary> <canary> [<canary> ...]" >&2
  echo "  e.g. scripts/check-token-literals.sh scripts/gitleaks-canary.txt api/internal/config/gitleaks_canary_test.go" >&2
  echo "  At least two allowlisted canary paths are required (the two gitleaks canaries)." >&2
  exit 2
fi

ROOT="$(git rev-parse --show-toplevel)" || {
  echo "check-token-literals: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || {
  echo "check-token-literals: cannot cd to repo root: $ROOT" >&2
  exit 2
}

# Each canary arg must EXIST and be TRACKED. A canary on disk but out of the index is
# not scanned by the index-aware main sweep, so exempting it would silently widen the
# allowlist over a file the sweep never gates on; assert tracked separately from exists.
for c in "$@"; do
  if [ ! -f "$c" ]; then
    echo "check-token-literals: allowlisted canary not found: $c" >&2
    echo "  The canaries double as the detector's shape-intact controls; without one," >&2
    echo "  a clean tree cannot be told from a detector that matched nothing." >&2
    exit 2
  fi
  if ! git ls-files --error-unmatch "$c" >/dev/null 2>&1; then
    echo "check-token-literals: allowlisted canary is not tracked by git: $c" >&2
    echo "  An untracked canary is not in the index-aware sweep, so exempting it would" >&2
    echo "  widen the allowlist over a file this gate never scans." >&2
    exit 2
  fi
done

# 🔴 ONE PCRE DETECTOR. A single alternation mirroring gitleaks v8.30.1's default
# provider rules; the `[`/`(` after each prefix keeps the pattern STRING itself from
# being a contiguous complete literal (so this file never self-flags).
#   glpat-[0-9A-Za-z_-]{20}                     GitLab PAT (gitleaks `gitlab-pat`),
#                                               NO entropy floor -- stricter on purpose
#                                               (GitHub Push Protection is pattern-based).
#   ghp_[0-9A-Za-z]{36}                         GitHub classic PAT (gitleaks `github-pat`).
#   github_pat_[0-9A-Za-z_]{82}                 GitHub fine-grained PAT
#                                               (gitleaks `github-fine-grained-pat`).
#   sk-ant-(api03|admin01)-[0-9A-Za-z_-]{93}AA  Anthropic API / admin key
#                                               (gitleaks `anthropic-api-key` family).
#   xoxb-[0-9]{10,13}-[0-9]{10,13}[0-9A-Za-z-]* Slack bot token (gitleaks `slack-bot-token`).
PATTERN='glpat-[0-9A-Za-z_-]{20}|ghp_[0-9A-Za-z]{36}|github_pat_[0-9A-Za-z_]{82}|sk-ant-(api03|admin01)-[0-9A-Za-z_-]{93}AA|xoxb-[0-9]{10,13}-[0-9]{10,13}[0-9A-Za-z-]*'

# Scan a scratch file that is NOT in the index. `git grep --no-index` refuses a path
# outside the enclosing repo, so run it from INSIDE the scratch dir on a relative name.
# Echoes git grep's exit code: 0 = matched, 1 = no match, >1 = error.
scan_scratch() {
  ( cd "$1" && git grep --no-index -nP -e "$PATTERN" -- "$2" ) >/dev/null 2>&1
}

# --- SELF-TEST: positive / negative / canary controls, before the main scan. ---
SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/check-token-literals.XXXXXX")" || {
  echo "check-token-literals: INSTRUMENT BROKEN -- could not create a scratch temp dir." >&2
  exit 2
}
trap 'rm -rf "$SCRATCH"' EXIT

# Token BODIES, each a run of 'A' of the exact minimum length its rule needs, built at
# runtime (brace expansion, no unquoted command substitution). Building the body -- and
# joining it to its prefix -- at runtime is why no line in this source is itself a
# complete token: the prefix lives ONLY inside a printf FORMAT string, where a `%`
# immediately follows it and breaks the shape.
b20="$(printf 'A%.0s' {1..20})"    # GitLab   glpat-       body (20 chars)
b36="$(printf 'A%.0s' {1..36})"    # GitHub classic  ghp_ body (36 chars)
b82="$(printf 'A%.0s' {1..82})"    # GitHub fine-grained github_pat_ body (82 chars)
b93="$(printf 'A%.0s' {1..93})"    # Anthropic sk-ant-api03- body (93 chars, then AA)
s10a="$(printf '1%.0s' {1..10})"   # Slack    xoxb- first  digit run (10 digits)
s10b="$(printf '2%.0s' {1..10})"   # Slack    xoxb- second digit run (10 digits)

# POSITIVE controls, ONE PER DETECTOR ALTERNATIVE. Each provider's COMPLETE literal is
# assembled AT RUNTIME (never written contiguous in this source), then the detector is
# asserted to MATCH it -- so a PCRE/pattern regression that silently kills a SINGLE
# branch is caught and NAMED, instead of hiding behind the branches that still fire.
# The label/value pairs are kept side by side so the failure message can name the dead
# branch; the prefix appears only in the printf format strings below (a `%` follows it).
pos_labels=(glpat- ghp_ github_pat_ sk-ant-api03- xoxb-)
pos_values=(
  "$(printf 'glpat-%s' "$b20")"
  "$(printf 'ghp_%s' "$b36")"
  "$(printf 'github_pat_%s' "$b82")"
  "$(printf 'sk-ant-api03-%sAA' "$b93")"
  "$(printf 'xoxb-%s-%sabcd' "$s10a" "$s10b")"
)
for i in "${!pos_labels[@]}"; do
  printf 'x = "%s"\n' "${pos_values[$i]}" > "$SCRATCH/positive.txt"
  if ! scan_scratch "$SCRATCH" positive.txt; then
    echo "check-token-literals: ================================================================" >&2
    echo "check-token-literals: INSTRUMENT BROKEN -- the detector did NOT match a complete" >&2
    echo "check-token-literals: provider-token literal built at runtime for the '${pos_labels[$i]}'" >&2
    echo "check-token-literals: branch. That detector alternative is DEAD: a real leak of this" >&2
    echo "check-token-literals: shape would ride through green. The pattern or git's PCRE support" >&2
    echo "check-token-literals: has regressed for this provider; a clean tree would mean nothing." >&2
    echo "check-token-literals: ================================================================" >&2
    exit 2
  fi
done

# NEGATIVE control: the ASSEMBLED form -- prefix and body as two separate quoted
# fragments joined at runtime. The detector MUST NOT match it, or the sanctioned
# runtime-assembly fix would itself be flagged. One is sufficient: it proves the
# sanctioned assembly technique passes, and the technique is provider-agnostic.
printf 'fake := "glpat-" + "%s"\n' "$b20" > "$SCRATCH/negative.txt"
if scan_scratch "$SCRATCH" negative.txt; then
  echo "check-token-literals: ================================================================" >&2
  echo "check-token-literals: INSTRUMENT BROKEN -- the detector matched the ASSEMBLED form" >&2
  echo "check-token-literals: (\"glpat-\" + \"...\"), which is the sanctioned fix. It would flag" >&2
  echo "check-token-literals: every correctly-split fixture. Fix PATTERN." >&2
  echo "check-token-literals: ================================================================" >&2
  exit 2
fi

# CANARY shape-intact control: each allowlisted canary MUST still carry a detectable
# complete provider shape (index-aware, since a canary is tracked). If one no longer
# matches, the canary lost its shape -- the detector's shape-intact regression.
for c in "$@"; do
  if ! git grep -qP -e "$PATTERN" -- "$c"; then
    echo "check-token-literals: ================================================================" >&2
    echo "check-token-literals: INSTRUMENT BROKEN -- allowlisted canary no longer carries a" >&2
    echo "check-token-literals: detectable complete provider-token shape: $c" >&2
    echo "check-token-literals: A canary that lost its shape means the detector can no longer be" >&2
    echo "check-token-literals: proven live over it. Restore the canary's token shape." >&2
    echo "check-token-literals: ================================================================" >&2
    exit 2
  fi
done

# --- MAIN SCAN: index-aware, over tracked files, excluding the allowlisted canaries. ---
excludes=()
for c in "$@"; do
  excludes+=(":!$c")
done

# git grep under `set -e`: 0 = matches (findings), 1 = no matches (clean), >1 = error.
# Capture the status without aborting.
set +e
hits="$(git grep -nP -e "$PATTERN" -- . "${excludes[@]}")"
rc=$?
set -e

case "$rc" in
  0)
    echo "check-token-literals: complete provider-token-shaped literal(s) in tracked source" >&2
    echo "check-token-literals: outside the allowlist (this would break GitHub Push Protection at push):" >&2
    echo "" >&2
    printf '%s\n' "$hits" >&2
    echo "" >&2
    echo "  An inline //gitleaks:allow does NOT help: GitHub Push Protection honours no" >&2
    echo "  in-file allow directive. ASSEMBLE the value from parts at runtime instead --" >&2
    echo "  e.g. \"glpat-\" + \"<20 chars>\" (see .claude/rules/prds.md, \"A PRD whose tests" >&2
    echo "  need secret-shaped strings\"). If this is a NEW legitimate canary, add it to the" >&2
    echo "  explicit allowlist args in Taskfile.yml's check:token-literals task." >&2
    exit 1
    ;;
  1)
    echo "check-token-literals: clean -- no complete provider-token literal in tracked source"
    echo "check-token-literals: outside the allowlist. $# allowlisted canary/canaries confirmed"
    echo "check-token-literals: shape-intact and detectable ($*), so this green is a positive"
    echo "check-token-literals: observation rather than a detector that never looked."
    exit 0
    ;;
  *)
    echo "check-token-literals: INSTRUMENT BROKEN -- git grep exited $rc (a PCRE or I/O error," >&2
    echo "check-token-literals: not a finding). Fix the environment or the pattern." >&2
    exit 2
    ;;
esac
