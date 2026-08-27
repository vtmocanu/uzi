#!/bin/sh
# Scan the working tree for secrets with gitleaks, and PROVE THE SCANNER WAS LIVE
# before believing its verdict (PRD #103 M5).
#
# usage: scripts/scan-secrets.sh <gitleaks-version> <canary-path>...
#   e.g. scripts/scan-secrets.sh v8.30.1 scripts/gitleaks-canary.txt \
#          api/internal/config/gitleaks_canary_test.go
#
# A SCRIPT, NOT AN INLINE `cmds:` LINE, for the reason recorded in full at
# scripts/lint-shell.sh: M5 adds shellcheck over every tracked script, so a
# committed script is linted and an inline Taskfile recipe is not.
#
# 🔴 THE CANARIES ARE THE WHOLE POINT OF THIS FILE, AND THEY ARE NOT DEFENSIVE
# PADDING. gitleaks AUTO-DISCOVERS `.gitleaks.toml` OUT OF THE TREE IT IS
# SCANNING, so the scanner's configuration is an ordinary tracked file that anyone
# who can open a merge request can add or widen -- and that merge request's own
# `scan:secrets` job then reads it. Measured 2026-08-03, same directory, same two
# planted secrets, no `-c` in either run: config absent -> 2 leaks; a config
# carrying `[allowlist] regexes = ['''.*''']` -> "no leaks found", exit 0. A
# scanner a contributor can switch off in the same commit that adds the secret is
# worse than no scanner, because it reports GREEN.
#
# What closes it is that EVERY ALLOWLIST BROAD ENOUGH TO BLIND THE SCANNER ALSO
# BLINDS THE CANARIES -- measured on that same pair of runs, the canary's own
# finding disappeared under the disarming config. So this script never reads,
# parses or trusts a config. It asks the REPORT one question ("are the canaries in
# here?"), and every way of switching the scanner off answers it "no". That makes
# the check a property of the recipe rather than of anyone's discipline, and it is
# the only mechanism in this milestone that satisfies CLAUDE.md's "a control that
# produces no output is not a control" for a check whose healthy state is silence:
# a clean run PRINTS the canaries it detected, so a green here is a positive
# observation rather than an absence of complaint.
#
# 🔴 TWO CANARIES, IN TWO DIFFERENT REGIONS OF THE TREE, and the second one closes
# a hole the first cannot see. A DISARMING allowlist kills any canary. A NARROW
# one does not -- and BE PRECISE ABOUT WHICH NARROW ONE, because the two spellings
# behave oppositely and the dangerous one is the CORRECT one:
#
#     [allowlist] paths = [...]                      <- no `[extend]`: REPLACES the
#                                                       whole ruleset, so nothing
#                                                       loads, every canary dies,
#                                                       and this is CAUGHT at 2
#     [extend] useDefault = true + [allowlist] paths <- rules still load, only the
#                                                       named paths go unread. THIS
#                                                       is the one that slipped
#
# So the form a contributor writes first, and the form the error text below warns
# against, was already caught; the form a careful contributor writes was not. With
# `[extend]` present and `paths` matching `_test.go` -- the tempting disposal for
# this repo's fake-token fixtures -- secret scanning is off for every test file
# forever while a canary living in `scripts/` reports happily. Measured
# 2026-08-03 as a discriminating pair, one planted secret in a tracked `_test.go`:
#     no config          -> exit 1, the finding named, 28.24 MB scanned
#     that paths= config -> exit 0, canary still detected, 24.40 MB scanned
# The 3.84 MB the second run did not read is the whole test corpus, and NOTHING IN
# THE OUTPUT SAID SO. So the second canary lives INSIDE the test corpus, and with
# it that config takes this script to exit 2 instead of 0.
#
# 🔴 THAT CLOSES THE MEASURED INSTANCE, NOT NARROWING IN GENERAL, and the limit is
# written here rather than papered over: an allowlist scoped to, say,
# `api/internal/config/` still slips past both canaries. A canary can only see the
# regions it is planted in. `.gitleaks.toml` is deliberately still PERMITTED --
# it is visible in a diff, so narrowing is a review question rather than a silent
# one, and that reason settles it on its own. (An earlier version of this comment
# gave a second reason, "banning it would make the canary's failure branch
# unreachable". That was REFUTED: with no config anywhere, mutating a canary's
# token takes this script to exit 2, so the branch is reachable without one. The
# canary file's own "WHAT BREAKS IF YOU TOUCH THIS FILE" list said so already.)
#
# 🔴 AND IT CANNOT BE REPLACED BY READING GITLEAKS' EXIT CODE. Measured
# 2026-08-03: clean 0, findings 1, `-c` pointing at a missing file 1, `-c` at a
# malformed toml 1, scan target absent 1, and an EMPTY DIRECTORY 0. Findings and
# three distinct instrument failures share one value, and the silent-no-op case
# shares the other. The 2/1/0 convention this repo uses is not derivable from it,
# which is why `--exit-code 0` is passed (gitleaks' status is deliberately taken
# out of the decision) and the report is what is read.
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
#     2 = the instrument is broken (a canary missing from the report, a canary
#         missing from the INDEX, scanner disarmed or narrowed, config arriving by
#         a route that is not reviewable, gitleaks itself failing)
#     1 = there are findings in TRACKED files
#     0 = clean, and every canary was seen
# `task`'s own rc is 201 for all of them.
set -eu

VERSION="${1:-}"
if [ "$#" -lt 2 ] || [ -z "$VERSION" ]; then
  echo "usage: scripts/scan-secrets.sh <gitleaks-version> <canary-path>..." >&2
  echo "  e.g. scripts/scan-secrets.sh v8.30.1 scripts/gitleaks-canary.txt \\" >&2
  echo "         api/internal/config/gitleaks_canary_test.go" >&2
  exit 2
fi
shift
# Remaining positionals are the canary paths. Kept as "$@" rather than folded into
# a string: a space-joined variable is the zsh trap CLAUDE.md documents, and this
# list is what the gate's liveness rests on.

# 🔴 THE RULE THE CANARIES MUST TRIP, AND WHY IT IS PINNED RATHER THAN "any
# finding in a canary file". A TARGETED allowlist -- one that silences
# `gitlab-pat` and nothing else -- would leave `generic-api-key` firing on a
# canary's token line, so a File-only assertion would pass while the rule that
# matters most in a repo whose forge is GitLab had been switched off. Asserting
# the rule ID closes that. The cost is that a gitleaks release renaming this rule
# exits 2 -- which is the instrument changing under us, exactly what 2 means, and
# the version is pinned by the caller so it can only happen on a deliberate bump.
CANARY_RULE="gitlab-pat"

# The report template is a committed file rather than a heredoc: it is what keeps
# secrets out of the report (see its own header), and a malformed one makes
# gitleaks exit non-zero, which lands in the instrument-broken branch below.
TEMPLATE="scripts/gitleaks-report.tmpl"

# Run from the repo root whatever the caller's directory. Two reasons, and neither
# is tidiness. `git ls-files` invoked from a subdirectory silently narrows to that
# subtree (lint-shell.sh's header states the same). And gitleaks' File field is the
# scan target AS SPELLED -- scanning `.` from the root yields repo-relative paths
# (`scripts/gitleaks-canary.txt`), while `dir /abs/path` yields absolute ones. Both
# the canary assertion and the tracked-set filter below compare that field against
# repo-relative paths, so the working directory and the spelling of the target have
# to be fixed for either comparison to mean anything. Same measurement that rules
# out `.gitleaksignore` fingerprints, which are built from that field.
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
# see -- and GitLab ranked pipeline variables (3) and project variables (4) above
# job variables (8), which was the same precedence argument the retired GitLab
# pipeline made for inlining the task checksum. A `.gitleaks.toml` is at least
# reviewable; these are not, so they are the two routes this script refuses rather
# than detects.
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
# finding makes it reappear. Unlike a `.gitleaks.toml` the canaries cannot see it
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

for _c in "$@"; do
  if [ ! -f "$_c" ]; then
    echo "scan-secrets: canary file not found: $_c" >&2
    echo "  Restore it from git. Without every canary this gate cannot tell a clean" >&2
    echo "  tree from a scanner that was switched off or narrowed, which is the only" >&2
    echo "  thing it is for." >&2
    exit 2
  fi
done

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

# 🔴 AN EXPLICIT `XXXXXX` TEMPLATE, NOT `mktemp -t <name>`. The `-t` form is not
# portable and it fails in the direction that only CI can show you: BSD/macOS
# mktemp treats the argument as a PREFIX and appends randomness, while GNU
# coreutils treats it as a TEMPLATE and requires at least three X's. So
# `mktemp -t uzi-scan-secrets` succeeds on a contributor's mac and dies on the
# Debian-based CI image with `too few X's in template`. Measured: that is
# exactly how this job failed on its first ever pipeline run (MR !175,
# pipeline 20332), after every other step in it had passed. A full template
# path is accepted by both implementations.
REPORT="$(mktemp "${TMPDIR:-/tmp}/uzi-scan-secrets.XXXXXX")" || {
  echo "scan-secrets: mktemp failed" >&2
  exit 2
}
TRACKED="$(mktemp "${TMPDIR:-/tmp}/uzi-scan-tracked.XXXXXX")" || {
  echo "scan-secrets: mktemp failed" >&2
  exit 2
}
trap 'rm -f "$REPORT" "$TRACKED"' EXIT HUP INT TERM

# 🔴 THE GATING SCOPE IS THE GIT INDEX, AND IT IS ENFORCED HERE, ON THE REPORT --
# NOT BY WHAT IS HANDED TO GITLEAKS. THAT DISTINCTION IS THE WHOLE OF H1.
#
# The first version of this script passed all 1465 tracked paths as scan targets
# and claimed the index as its scope. `gitleaks dir` HONOURS ONE TARGET; give it
# two or more and it silently widens to `.`. Measured on a three-file fixture,
# with the disconfirming control the original probe lacked -- a file that is NOT a
# target:
#     one target      40 bytes scanned, 1 finding   (the target)
#     TWO targets    120 bytes scanned, 3 findings  (INCLUDING the non-target)
#     `dir -- .`     120 bytes scanned, 3 findings  (byte-identical)
# and on this repo the 1465-target invocation scanned 28,426,804 bytes against a
# tracked-byte sum of 22.5 MB -- 26% excess, byte-identical to `dir -- .`.
#
# The measurement that established multi-target support ("two targets, both
# scanned, each reported under its own spelling") was TRUE. The inference was not:
# both targets are scanned AND SO IS EVERYTHING ELSE. An instrument that cannot
# produce the disconfirming answer is not evidence.
#
# So: hand gitleaks ONE target and filter its report. The claim then becomes true
# rather than merely intended, and -- unlike passing a file list -- it is
# checkable, because the filtering happens in code a reviewer can read.
#
# WHY THE INDEX AND NOT THE WORKING TREE. gitleaks does NOT honour `.gitignore`
# (measured: a file under a gitignored directory and an untracked file at the top
# level were both reported). So gating on everything it walks means gating on
# whatever is lying in a contributor's worktree -- `web/dist`, a local `.env`,
# stale build output -- none of which is in CI's checkout.  Same index-scoped rule
# lint-shell.sh and lint-yaml.sh already take.
#
# UNTRACKED FINDINGS ARE REPORTED BUT DO NOT GATE, and the security argument is
# stronger than the consistency one. A secret reaches the forge, other clones, CI
# logs and images BY BEING TRACKED; an untracked file is by construction not in
# the artifact anyone else receives, so gating on it adds no coverage of the
# exposure path. What it would cost is the thing this design cannot defend: a gate
# that reddens on files a contributor cannot fix by editing tracked code is a gate
# people switch off, and the switch available here is `.gitleaks.toml` -- the
# narrowing route the canaries can only partly see. Dropping them silently would
# instead throw away the most useful local signal there is (a secret in a file you
# just created), so they are counted and named at exit 0 under a NOT GATING
# banner. `git add` the file and the next run gates on it.
#
# 🔴 THAT MAKES THE CLASSIFICATION THE SECURITY BOUNDARY. Anything that makes a
# tracked file's REPORTED path differ from its INDEX spelling silently demotes a
# gating finding to a note. Three routes exist and all three are answered here:
#   * the report's spelling -- `dir -- .` from the repo root yields repo-relative
#     paths, which is what `git ls-files` prints (fixed above, and the reason the
#     working directory is fixed);
#   * the index's spelling -- C-quoting, refused below;
#   * SUBMODULES -- `git ls-files` lists the GITLINK and never the contents, so
#     nothing inside a submodule can ever be in the tracked set. A secret
#     committed in one is therefore printed and does NOT gate. That is defensible
#     (it is not this repo's content, and `lint-shell.sh`/`lint-yaml.sh` skip
#     submodule contents for the same reason) but it is not obvious, and it is not
#     hypothetical here: `inspiration/` WAS three submodules until 2026-08-03. If
#     they come back, this gate quietly changes what it gates on while still
#     printing the findings.
#
# `core.quotePath=false` IS LOAD-BEARING, NOT COSMETIC. By default git C-quotes any
# path with a byte above ASCII, so a single accented filename anywhere in the repo
# would make the refusal below fire and brick this gate. gitleaks reports the RAW
# path, and with this flag `git ls-files` prints the raw path too -- measured
# byte-identical on `café.txt`, and measured to leave a newline-containing path
# still C-quoted, which is the one case a line-based comparison genuinely cannot
# handle. So the flag narrows the refusal from "any non-ASCII path" to "any path
# holding a control character".
FILES="$(git -c core.quotePath=false ls-files)" || exit 2

if [ -z "$FILES" ]; then
  echo "scan-secrets: the git index lists no files under $ROOT." >&2
  echo "  An empty tracked set is an instrument failure, never a clean run: with" >&2
  echo "  nothing to compare the report against, every finding would be classed as" >&2
  echo "  untracked and this gate would pass whatever gitleaks found." >&2
  exit 2
fi
printf '%s\n' "$FILES" > "$TRACKED"

# Even with `core.quotePath=false` above, git C-QUOTES a path whose name holds a
# CONTROL character, so a tracked path with a newline in it would reach $TRACKED
# as the literal `"a\nb"` and never match the raw path gitleaks reports -- that
# finding would silently fall into the non-gating untracked bucket, which is the
# security boundary named above. This refuses to run instead of degrading quietly.
#
# The branch is not merely asserted-by-absence: it was exercised. Before the
# `core.quotePath=false` flag, adding a `café.txt` fired it (rc=2) and removing it
# restored rc=0; with the flag that same file passes and a newline-named one still
# fires. So this now covers the case a line-based comparison genuinely cannot
# handle, and nothing else.
if grep -q '^"' "$TRACKED"; then
  echo "scan-secrets: the index contains a C-quoted path -- one whose name holds a" >&2
  echo "  CONTROL character (a newline, a tab). Note this is NOT about accents:" >&2
  echo "  the list is read with core.quotePath=false, so non-ASCII names are fine." >&2
  echo "  The tracked-set comparison is line-based and cannot match a quoted path," >&2
  echo "  so such a file would be classed untracked and would stop gating. Rename" >&2
  echo "  it." >&2
  exit 2
fi

# 🔴 EVERY CANARY MUST BE IN THE INDEX, AND THIS IS NOT THE SAME CHECK AS THE
# `-f` PRE-FLIGHT ABOVE. A canary that exists on disk but is not tracked is still
# walked by `dir -- .` and still reported, so it still says "DETECTED" -- while
# the population it attests for has changed underneath it. Measured 2026-08-03:
# `git rm --cached` on the corpus canary, file untouched on disk, gave rc=0,
# "canaries DETECTED", "clean", with the index count quietly falling 1467 -> 1466.
#
# THE FRAMING IS WHAT MAKES THIS WORTH A GUARD RATHER THAN A NOTE. Findings in
# untracked files do not gate here, so a canary outside the index attests LIVENESS
# OVER THE NON-GATING POPULATION. The green is then signed by a witness that no
# longer speaks for anything that was gated -- precisely the substitution the
# canaries exist to make impossible.
#
# Neither guard above covers it: the `-f` test asks whether the file is on disk,
# and the two below it ask about the index as a whole (empty, C-quoted), never
# about these paths' membership in it. Low severity in practice, because CI checks
# out from git so the file is simply ABSENT there and takes the `-f` branch --
# local-green / CI-red, the safe direction. It is still a green that means less
# than it says, on the one check whose entire job is to make a green mean
# something.
for _c in "$@"; do
  if ! grep -q -F -x -e "$_c" -- "$TRACKED"; then
    echo "scan-secrets: canary is NOT TRACKED: $_c" >&2
    echo "  It is on disk, so gitleaks still scans and reports it -- but findings" >&2
    echo "  in untracked files do not gate here, so this canary would be attesting" >&2
    echo "  liveness over the population this gate IGNORES. A green signed by that" >&2
    echo "  witness says nothing about what was gated." >&2
    echo "  \`git add $_c\` (or restore it from git if it was removed on purpose)." >&2
    exit 2
  fi
done

# 🔴 `dir`, NEVER `git`. History mode ran past 25 minutes on this repo's 2471
# commits without finishing, and in CI it would read only the shallow clone that
# `GIT_DEPTH` leaves behind -- a narrowed scan that looks exactly like a pass.
# `dir` reads the files as they are on disk, so it sees an uncommitted secret in a
# tracked file, and its scope does not depend on how the runner cloned.
#
# 🔴 EXACTLY ONE TARGET. See the H1 paragraph above: a second one is not an
# addition, it is a silent widening to `.`.
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
  -- . || rc=$?

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
canary_seen=""
tracked_count=0
tracked_list=""
untracked_count=0
untracked_list=""
while IFS='	' read -r f_file f_line f_rule; do
  [ -n "$f_file" ] || continue

  _is_canary=0
  for _c in "$@"; do
    if [ "$f_file" = "$_c" ]; then
      _is_canary=1
      # Only a hit under the PINNED rule counts as a canary sighting. A finding on
      # a canary file under some OTHER rule is neither a sighting nor a gating
      # finding, so it is swallowed here deliberately: the file's whole purpose is
      # to hold a fake credential, and reporting it as a leak would be noise.
      if [ "$f_rule" = "$CANARY_RULE" ]; then
        canary_seen="$canary_seen$_c
"
      fi
      break
    fi
  done
  [ "$_is_canary" -eq 0 ] || continue

  if grep -q -F -x -e "$f_file" -- "$TRACKED"; then
    tracked_count=$((tracked_count + 1))
    tracked_list="${tracked_list}  ${f_file}:${f_line}  ${f_rule}
"
  else
    untracked_count=$((untracked_count + 1))
    if [ "$untracked_count" -le 10 ]; then
      untracked_list="${untracked_list}  ${f_file}:${f_line}  ${f_rule}
"
    fi
  fi
done < "$REPORT"

missing=""
for _c in "$@"; do
  case "
$canary_seen" in
    *"
$_c
"*) ;;
    *) missing="$missing  $_c
" ;;
  esac
done

if [ -n "$missing" ]; then
  echo "scan-secrets: ================================================================" >&2
  echo "scan-secrets: A CANARY IS NOT IN THE REPORT. THE SCANNER WAS BLIND." >&2
  echo "scan-secrets:" >&2
  echo "scan-secrets: Missing (each must yield a \`$CANARY_RULE\` finding):" >&2
  printf '%s' "$missing" >&2
  echo "scan-secrets:" >&2
  echo "scan-secrets: Each canary holds a fake GitLab token that gitleaks reports on" >&2
  echo "scan-secrets: every healthy run. One is missing, so this run proves nothing" >&2
  echo "scan-secrets: about the region it lives in, and its \"no findings\" is NOT a" >&2
  echo "scan-secrets: clean bill of health." >&2
  echo "scan-secrets:" >&2
  echo "scan-secrets: In order of likelihood:" >&2
  echo "scan-secrets:   * a .gitleaks.toml was added or widened. An allowlist broad" >&2
  echo "scan-secrets:     enough to hide a secret also hides a canary -- and if only" >&2
  echo "scan-secrets:     ONE canary died, read WHICH: they sit in different regions" >&2
  echo "scan-secrets:     of the tree precisely so a NARROWING shows up here;" >&2
  echo "scan-secrets:   * a canary's token line was edited, or gained a suppression" >&2
  echo "scan-secrets:     comment (the annotation matches as a substring of the LINE," >&2
  echo "scan-secrets:     so it would silence that canary);" >&2
  echo "scan-secrets:   * gitleaks $VERSION renamed or dropped the \`$CANARY_RULE\` rule." >&2
  echo "scan-secrets: ================================================================" >&2
  exit 2
fi

if [ "$untracked_count" -gt 0 ]; then
  echo "scan-secrets: NOTE -- $untracked_count finding(s) in UNTRACKED files. NOT GATING."
  printf '%s' "$untracked_list"
  if [ "$untracked_count" -gt 10 ]; then
    echo "scan-secrets:   … and $((untracked_count - 10)) more (listing capped at 10)."
  fi
  echo "scan-secrets: These are outside the git index, so CI's checkout does not have"
  echo "scan-secrets: them and gating on them would make your verdict differ from CI's."
  echo "scan-secrets: If one is a file you are about to commit, \`git add\` it and run"
  echo "scan-secrets: again -- it will gate then."
fi

if [ "$tracked_count" -gt 0 ]; then
  echo "scan-secrets: $tracked_count secret finding(s) in TRACKED files:" >&2
  printf '%s' "$tracked_list" >&2
  echo "  Every canary was seen, so the scanner WAS live over every region they" >&2
  echo "  cover, and these are real reports about real lines." >&2
  echo "" >&2
  echo "  If a finding is a test fixture, annotate THAT LINE with" >&2
  echo "  \`//gitleaks:allow\` FOLLOWED BY A WRITTEN JUSTIFICATION. Do not reach for" >&2
  echo "  a .gitleaks.toml \`paths\` allowlist: \`.*_test\\.go\` turns off secret" >&2
  echo "  scanning for every test file in the repo, forever. Note WHICH spelling is" >&2
  echo "  dangerous -- a bare [allowlist] paths replaces the whole ruleset, so every" >&2
  echo "  canary dies and this gate exits 2; it is the CORRECT one, [extend]" >&2
  echo "  useDefault = true plus paths, that leaves the canaries alive. The second" >&2
  echo "  canary makes that one exit 2 too, but an allowlist scoped to a directory" >&2
  echo "  holding no canary still slips past both. gitleaks has no unused-directive" >&2
  echo "  report either, so an annotation suppresses whatever its line LATER comes" >&2
  echo "  to contain -- which is why the justification is not paperwork." >&2
  exit 1
fi

tracked_total="$(wc -l < "$TRACKED" | tr -d ' ')"
echo "scan-secrets: clean -- 0 findings in tracked files ($tracked_total in the index)."
echo "scan-secrets: canaries DETECTED ($CANARY_RULE, gitleaks $VERSION):"
printf '%s' "$canary_seen" | sed 's/^/scan-secrets:   /'
echo "scan-secrets: those lines are the positive observation: without them this green"
echo "scan-secrets: would be indistinguishable from a scanner that never looked."
exit 0
