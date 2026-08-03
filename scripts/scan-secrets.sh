#!/bin/sh
# Scan the working tree for secrets with gitleaks, and PROVE THE SCANNER WAS LIVE
# before believing its verdict (PRD #103 M5).
#
# usage: scripts/scan-secrets.sh <gitleaks-version> <canary-path>
#   e.g. scripts/scan-secrets.sh v8.30.1 scripts/gitleaks-canary.txt
#
# A SCRIPT, NOT AN INLINE `cmds:` LINE, for the reason recorded in full at
# scripts/lint-shell.sh: M5 adds shellcheck over every tracked script, so a
# committed script is linted and an inline Taskfile recipe is not.
#
# 🔴 THE CANARY IS THE WHOLE POINT OF THIS FILE, AND IT IS NOT DEFENSIVE PADDING.
# gitleaks AUTO-DISCOVERS `.gitleaks.toml` OUT OF THE TREE IT IS SCANNING, so the
# scanner's configuration is an ordinary tracked file that anyone who can open a
# merge request can add or widen -- and that merge request's own `scan:secrets`
# job then reads it. Measured 2026-08-03, same directory, same two planted
# secrets, no `-c` in either run: config absent -> 2 leaks; a config carrying
# `[allowlist] regexes = ['''.*''']` -> "no leaks found", exit 0. Re-measured
# against the explicit-file-list form this script actually uses, because the
# discovery rule is written in terms of the target path and a file list is not a
# directory: a root `.gitleaks.toml` STILL applies. The hole is not closed by the
# scope choice below and must not be assumed to be. A scanner a contributor can
# switch off in the same commit that adds the secret is worse than no scanner,
# because it reports GREEN.
#
# What closes it is that EVERY ALLOWLIST BROAD ENOUGH TO BLIND THE SCANNER ALSO
# BLINDS THE CANARY -- measured on that same pair of runs, the canary's own finding
# disappeared under the disarming config. So this script never reads, parses or
# trusts a config. It asks the REPORT one question ("is the canary in here?"), and
# every way of switching the scanner off answers it "no". That makes the check a
# property of the recipe rather than of anyone's discipline, and it is the only
# mechanism in this milestone that satisfies CLAUDE.md's "a control that produces
# no output is not a control" for a check whose healthy state is silence: a clean
# run PRINTS the canary it detected, so a green here is a positive observation
# rather than an absence of complaint.
#
# 🔴 AND IT CANNOT BE REPLACED BY READING GITLEAKS' EXIT CODE. Measured
# 2026-08-03: clean 0, findings 1, `-c` pointing at a missing file 1, `-c` at a
# malformed toml 1, scan target absent 1, and an EMPTY DIRECTORY 0. Findings and
# three distinct instrument failures share one value, and the silent-no-op case
# shares the other. The 2/1/0 convention this repo uses is not derivable from it,
# which is why `--exit-code 0` is passed (gitleaks' status is deliberately taken
# out of the decision) and the report is what is read.
#
# 🔴 WHAT THE CANARY DOES NOT COVER, MEASURED RATHER THAN LEFT TO BE DISCOVERED.
# A NARROW allowlist -- `[allowlist] paths = ['''.*_test\.go''']`, the tempting
# disposal for this repo's fake-token fixtures -- turns off secret scanning for
# every test file forever and leaves the canary (a .txt under scripts/) reporting
# happily. Measured 2026-08-03 as a discriminating pair, one planted secret in a
# tracked `_test.go`:
#     no config          -> exit 1, the finding named, 28.24 MB scanned
#     that paths= config -> exit 0, canary still detected, 24.40 MB scanned
# The 3.84 MB the second run did not read is the whole test corpus, and NOTHING IN
# THE OUTPUT SAYS SO -- the byte counter is the only tell and it takes two runs to
# read. So: the canary catches DISARM, not NARROWING.
#
# `.gitleaks.toml` is deliberately NOT banned here even so, for two reasons. It is
# visible in a diff, which makes narrowing a review question rather than a silent
# one; and banning it would make the canary's own failure branch UNREACHABLE, so
# nobody could ever demonstrate that the mechanism this file is built around
# works. A guard that forecloses its own calibration is worth less than the hole
# it closes. The two routes that are NOT visible in a diff are refused outright
# below.
#
# 🔴 NO SKIP BRANCH, WHICH IS A DELIBERATE ASYMMETRY WITH THE OTHER THREE
# `gate:repo` CHECKS. shellcheck, yamllint and ruby print a loud SKIP and exit 0
# when absent, because they are brew/pip/system tools a contributor may simply not
# have, and `gate:repo` runs FIRST inside `task gate` (PRD #103 Decision 2 -- a
# gate people cannot run is a gate that stops being run). gitleaks is not in that
# category: it arrives through `go run pkg@version`, the Go toolchain is mandatory
# in this repo, and `gate:api` ALREADY `go run`s two pinned remote modules
# (golangci-lint, deadcode). So the population that cannot obtain gitleaks is the
# population that cannot run `gate:api` either -- a skip would buy nobody anything
# and would put a fail-open branch in the one check where fail-open is worst.
# There is consequently NO `UZI_SCAN_SECRETS_REQUIRED`: a variable guarding a
# branch that does not exist is the vacuous-directive class this milestone is
# about, and `lint:repo` must not grow one.
#
# EXIT CODES (the convention `fmt-check:api`, `lint:api`, deadcode-gate.sh and the
# three `lint-*.sh` scripts set):
#     2 = the instrument is broken (canary missing from the report, scanner
#         disarmed, config arriving by a route that is not reviewable, gitleaks
#         itself failing)
#     1 = there are findings
#     0 = clean, and the canary was seen
# `task`'s own rc is 201 for all of them.
set -eu

VERSION="${1:-}"
CANARY="${2:-}"

if [ -z "$VERSION" ] || [ -z "$CANARY" ]; then
  echo "usage: scripts/scan-secrets.sh <gitleaks-version> <canary-path>" >&2
  echo "  e.g. scripts/scan-secrets.sh v8.30.1 scripts/gitleaks-canary.txt" >&2
  exit 2
fi

# 🔴 THE RULE THE CANARY MUST TRIP, AND WHY IT IS PINNED RATHER THAN "any finding
# in the canary file". A TARGETED allowlist -- one that silences `gitlab-pat` and
# nothing else -- would leave `generic-api-key` firing on the canary's token line,
# so a File-only assertion would pass while the rule that matters most in a repo
# whose forge is GitLab had been switched off. Asserting the rule ID closes that.
# The cost is that a gitleaks release renaming this rule exits 2 -- which is the
# instrument changing under us, exactly what 2 means, and the version is pinned by
# the caller so it can only happen on a deliberate bump.
CANARY_RULE="gitlab-pat"

# The report template is a committed file rather than a heredoc: it is what keeps
# secrets out of the report (see its own header), and a malformed one makes
# gitleaks exit non-zero, which lands in the instrument-broken branch below.
TEMPLATE="scripts/gitleaks-report.tmpl"

# Run from the repo root whatever the caller's directory. Two reasons, and neither
# is tidiness. `git ls-files` invoked from a subdirectory silently narrows to that
# subtree (lint-shell.sh's header states the same). And gitleaks' File field is the
# scan target AS SPELLED -- a target given as `scripts/x` is reported as
# `scripts/x`, one given as `/abs/scripts/x` as `/abs/scripts/x`. The canary
# assertion below compares that field against the path it was passed, so both the
# working directory and the spelling of the targets have to be fixed for the
# comparison to mean anything. Same measurement that rules out `.gitleaksignore`
# fingerprints, which are built from that field.
ROOT="$(git rev-parse --show-toplevel)" || {
  echo "scan-secrets: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || exit 2

# 🔴 A CONFIG ARRIVING BY ENVIRONMENT IS REFUSED, AND THIS IS THE A1 HAZARD IN A
# SECOND TOOL. gitleaks' own `--help` documents the precedence: `-c` beats
# `GITLEAKS_CONFIG` beats `GITLEAKS_CONFIG_TOML` beats `<target>/.gitleaks.toml`.
# The middle two mean a PROJECT-LEVEL or MANUAL-PIPELINE CI variable can replace
# this gate's entire ruleset with no file, no diff and nothing for a reviewer to
# see -- and GitLab ranks pipeline variables (3) and project variables (4) above
# job variables (8), which is the same precedence argument `.gitlab-ci.yml` makes
# for inlining the task checksum. A `.gitleaks.toml` is at least reviewable; these
# are not, so they are the two routes this script refuses rather than detects.
if [ -n "${GITLEAKS_CONFIG:-}" ] || [ -n "${GITLEAKS_CONFIG_TOML:-}" ]; then
  echo "scan-secrets: GITLEAKS_CONFIG / GITLEAKS_CONFIG_TOML is set in the environment." >&2
  echo "  Either one REPLACES this gate's ruleset without touching a tracked file," >&2
  echo "  so nothing in a merge request would show it. This gate runs on gitleaks'" >&2
  echo "  default ruleset; change it in a reviewable file, not in a CI variable." >&2
  exit 2
fi

# 🔴 `.gitleaksignore` IS REFUSED, AND NOT FOR TIDINESS: ITS FINGERPRINTS CANNOT
# WORK HERE. A fingerprint is "<target-as-spelled>:<rule>:<startline>", measured
# 2026-08-03: `gitleaks dir .` gives `prod.go:gitlab-pat:3`, `dir /abs` gives
# `/abs/prod.go:gitlab-pat:3`, `dir ./prod.go` gives `./prod.go:gitlab-pat:3` --
# three spellings, three baselines, so one generated locally does not match CI's.
# It is also POSITION-dependent: inserting one unrelated line above a baselined
# finding makes it reappear. Unlike a `.gitleaks.toml` the canary cannot see it
# (it silences named findings, never the scanner), so refusing it is the only
# thing that stops it narrowing the scan silently. The supported disposal is a
# per-instance `//gitleaks:allow` carrying a written justification.
if [ -e .gitleaksignore ]; then
  echo "scan-secrets: .gitleaksignore exists and this gate refuses to run with one." >&2
  echo "  Its fingerprints are invocation- AND position-dependent, so a baseline" >&2
  echo "  generated locally does not match CI's and one inserted line above a" >&2
  echo "  baselined finding brings it back. Dispose of a finding with a" >&2
  echo "  per-instance //gitleaks:allow carrying a written justification instead." >&2
  exit 2
fi

if [ ! -f "$CANARY" ]; then
  echo "scan-secrets: canary file not found: $CANARY" >&2
  echo "  Restore it from git. Without it this gate cannot tell a clean tree from" >&2
  echo "  a scanner that was switched off, which is the only thing it is for." >&2
  exit 2
fi

if [ ! -f "$TEMPLATE" ]; then
  echo "scan-secrets: report template not found: $TEMPLATE" >&2
  echo "  Restore it from git. It is what keeps matched secrets out of the report." >&2
  exit 2
fi

# `go`, not `gitleaks`: this check has no binary of its own by design (see the
# no-skip paragraph in the header). A missing Go toolchain is an instrument
# failure here for the same reason it is one for lint:api.
if ! command -v go >/dev/null 2>&1; then
  echo "scan-secrets: no go on PATH." >&2
  echo "  gitleaks arrives via \`go run …@$VERSION\`, the same pinned-module route" >&2
  echo "  gate:api already uses for golangci-lint and deadcode. Install Go." >&2
  exit 2
fi

# 🔴 SCOPE IS THE GIT INDEX, NOT THE DIRECTORY TREE -- the same rule lint-shell.sh
# and lint-yaml.sh take their scope by, and for a reason `dir .` does not satisfy:
# GITLEAKS DOES NOT HONOUR `.gitignore`. Measured 2026-08-03 in a throwaway repo:
# a file under a gitignored directory AND an untracked file at the top level were
# both reported by `gitleaks dir .`. So `dir .` scans whatever happens to be lying
# in a contributor's worktree -- `web/dist`, a local `.env`, an old build output --
# none of which exists in CI's checkout. That is a gate whose scope differs between
# the two places Success Criterion 1 exists to keep identical, and the local half
# is the one that reddens on files the contributor cannot fix by editing tracked
# code. PRD #103 Decision 2: a gate people cannot run is a gate that stops being
# run.
#
# `gitleaks dir` ACCEPTS MULTIPLE PATHS, which is what makes this possible at all
# and is not documented in its `--help` (it reads `dir [flags] [path]`, singular).
# Measured: two targets, both scanned, each reported under its own spelling.
#
# THE TRADE, STATED: a brand-new file the contributor has not `git add`ed yet is
# out of scope until they add it. That is exactly the trade lint-shell.sh already
# makes and MR-A's calibration pinned both ways (identical bytes untracked -> not
# in scope; `git add`ed -> in scope, finding named). One rule for all three
# repo-wide checks beats a special case here.
#
# NOT A REASON, RECORDED SO NOBODY RE-DERIVES IT: `node_modules` is NOT why. It is
# skipped by gitleaks' own default allowlist, measured directly -- a planted secret
# under `node_modules/foo/a.go` is not reported while an identical one under
# `src/b.go` is, and the byte counter shows the directory was never opened. It is
# skipped by name, not by `.gitignore` and not by this scope choice.
REPORT="$(mktemp -t uzi-scan-secrets)" || {
  echo "scan-secrets: mktemp failed" >&2
  exit 2
}
trap 'rm -f "$REPORT"' EXIT HUP INT TERM

FILES="$(git ls-files)" || exit 2

if [ -z "$FILES" ]; then
  echo "scan-secrets: the git index lists no files under $ROOT." >&2
  echo "  An empty scope is an instrument failure, never a clean run. gitleaks with" >&2
  echo "  no target scans the working directory, so this must never be allowed to" >&2
  echo "  fall through to the invocation below." >&2
  exit 2
fi

# Split on NEWLINE ONLY, globbing off -- see lint-shell.sh for why the default IFS
# is wrong here (git does not quote spaces) and why `set -f` is needed. A path
# containing a newline is C-quoted by git and will not stat, which makes gitleaks
# exit non-zero into the instrument-broken branch below rather than silently
# narrowing the scope.
oldIFS="${IFS-}"
IFS='
'
set -f
# shellcheck disable=SC2086  # PRE-ARMED, NOT VACUOUS: SC2086 is `info`, so it does
# not fire at this gate's `--severity=warning` -- the same directive and the same
# reason as lint-shell.sh's and lint-yaml.sh's. Deliberate word split, see above.
set -- $FILES
set +f
IFS="$oldIFS"

# 🔴 `dir`, NEVER `git`. History mode ran past 25 minutes on this repo's 2471
# commits without finishing, and in CI it would read only the shallow clone that
# `GIT_DEPTH` leaves behind -- a narrowed scan that looks exactly like a pass.
# `dir` reads the files as they are on disk, so it sees an uncommitted secret in a
# tracked file, and its scope does not depend on how the runner cloned.
#
# 🔴 `--exit-code 0` IS LOAD-BEARING, NOT A CONVENIENCE. It takes gitleaks' status
# out of the decision entirely (see the header's collapse table), so that the ONLY
# non-zero status this run can produce is gitleaks failing to run at all -- which
# is unambiguously an instrument failure, and is treated as one below. The verdict
# is derived from the report.
#
# `--redact` belts the template's braces: the template emits no secret, and this
# stops one reaching gitleaks' own console output either.
#
# NOTE `go run` FLATTENS EXIT CODES -- a tool exiting 3 surfaces as 1 with
# "exit status 3" on stderr. That costs nothing here precisely because
# `--exit-code 0` already made every non-zero status mean the same thing.
rc=0
go run "github.com/zricethezav/gitleaks/v8@$VERSION" dir \
  --exit-code 0 \
  --no-banner \
  --redact \
  -f template \
  --report-template "$TEMPLATE" \
  --report-path "$REPORT" \
  -- "$@" || rc=$?

if [ "$rc" -ne 0 ]; then
  echo "scan-secrets: gitleaks exited $rc under --exit-code 0, so this is NOT a" >&2
  echo "  finding -- with that flag set, findings exit 0. Something stopped the" >&2
  echo "  scanner running: an unreachable module proxy, a malformed" >&2
  echo "  $TEMPLATE, or a .gitleaks.toml gitleaks could not parse." >&2
  echo "  INSTRUMENT failure, not a scan result." >&2
  exit 2
fi

# Parse: File<TAB>StartLine<TAB>RuleID, one finding per line. Redirected from the
# file rather than piped so the loop runs in THIS shell and the counters survive.
canary_hits=0
canary_rule_hits=0
other_count=0
others=""
while IFS='	' read -r f_file f_line f_rule; do
  [ -n "$f_file" ] || continue
  if [ "$f_file" = "$CANARY" ]; then
    canary_hits=$((canary_hits + 1))
    if [ "$f_rule" = "$CANARY_RULE" ]; then
      canary_rule_hits=$((canary_rule_hits + 1))
    fi
    continue
  fi
  other_count=$((other_count + 1))
  others="${others}  ${f_file}:${f_line}  ${f_rule}
"
done < "$REPORT"

if [ "$canary_rule_hits" -eq 0 ]; then
  echo "scan-secrets: ================================================================" >&2
  echo "scan-secrets: THE CANARY IS NOT IN THE REPORT. THE SCANNER WAS BLIND." >&2
  echo "scan-secrets:" >&2
  echo "scan-secrets: $CANARY holds a fake GitLab token that gitleaks' \`$CANARY_RULE\`" >&2
  echo "scan-secrets: rule reports on every healthy run. It is missing, so this run" >&2
  echo "scan-secrets: proves nothing about the rest of the tree and its \"no findings\"" >&2
  echo "scan-secrets: is NOT a clean bill of health." >&2
  echo "scan-secrets: (findings in the canary file under other rules: $canary_hits)" >&2
  echo "scan-secrets:" >&2
  echo "scan-secrets: In order of likelihood:" >&2
  echo "scan-secrets:   * a .gitleaks.toml was added or widened -- an allowlist broad" >&2
  echo "scan-secrets:     enough to hide a secret also hides the canary, which is the" >&2
  echo "scan-secrets:     entire reason this check exists;" >&2
  echo "scan-secrets:   * the canary's token line was edited, or gained a" >&2
  echo "scan-secrets:     suppression comment (the annotation matches as a substring" >&2
  echo "scan-secrets:     of the LINE, so it would silence the canary);" >&2
  echo "scan-secrets:   * gitleaks $VERSION renamed or dropped the \`$CANARY_RULE\` rule." >&2
  echo "scan-secrets: ================================================================" >&2
  exit 2
fi

if [ "$other_count" -gt 0 ]; then
  echo "scan-secrets: $other_count secret finding(s) outside the canary:" >&2
  printf '%s' "$others" >&2
  echo "  Canary seen ($canary_rule_hits x $CANARY_RULE), so the scanner WAS live and" >&2
  echo "  these are real reports about real lines." >&2
  echo "" >&2
  echo "  If a finding is a test fixture, annotate THAT LINE with" >&2
  echo "  \`//gitleaks:allow\` FOLLOWED BY A WRITTEN JUSTIFICATION. Do not reach for" >&2
  echo "  a .gitleaks.toml \`paths\` allowlist: \`.*_test\\.go\` turns off secret" >&2
  echo "  scanning for every test file in the repo, forever, and nothing reports" >&2
  echo "  that it did. gitleaks has no unused-directive report either, so an" >&2
  echo "  annotation suppresses whatever its line LATER comes to contain -- which" >&2
  echo "  is why the justification is not paperwork." >&2
  exit 1
fi

echo "scan-secrets: clean -- 0 findings outside the canary over $# tracked files."
echo "scan-secrets: canary DETECTED at $CANARY ($canary_rule_hits x $CANARY_RULE, gitleaks $VERSION)."
echo "scan-secrets: that line is the positive observation: without it this green would"
echo "scan-secrets: be indistinguishable from a scanner that never looked."
exit 0
