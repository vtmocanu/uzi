#!/bin/sh
# Gate one Go module on `golang.org/x/vuln/cmd/govulncheck` (PRD #103 M5 MR-C).
#
# 🔴 THIS WRAPPER EXISTS FOR THE EXIT CODES, AND govulncheck's ARE INVERTED
# AGAINST THIS REPO'S CONVENTION. Every other gate here uses
#
#     2 = the instrument is broken     1 = there are findings     0 = clean
#
# (`fmt-check:api`, `lint:api`'s pre-flight, `scripts/deadcode-gate.sh`).
# govulncheck v1.1.4 uses, read out of its own source and then measured:
#
#     0  clean
#     3  FINDINGS                 internal/scan/text.go, errVulnerabilitiesFound
#     2  usage error              internal/scan/errors.go, errUsage
#     1  EVERYTHING ELSE          main.go's default: arm
#
# So a straight pass-through reports a `vuln.go.dev` OUTAGE as "there are
# findings" -- loud, wrong, and pointed at the branch author rather than at the
# network. This script maps 3 -> 1 and every other nonzero -> 2.
#
# 🔴 AND IT INSTALLS A BINARY RATHER THAN USING `go run`, WHICH IS THE ONLY WAY
# TO KEEP THAT DISTINCTION. `go run pkg@version` FLATTENS every nonzero exit to
# 1 -- which is precisely "there are findings" in this repo's convention, so the
# outage and the finding become the same code. Measured 2026-08-04 on a fixture
# module with a genuinely CALLED vulnerability (golang.org/x/text@v0.3.5 calling
# language.Parse, GO-2021-0113), go1.26.5:
#
#     binary,  text,           called vuln       rc=3
#     go run,  text,           SAME called vuln  rc=1
#     binary,  unreachable -db                   rc=1
#     go run,  unreachable -db                   rc=1   <- collapsed
#     binary,  unparseable .go file              rc=1
#     go run,  unparseable .go file              rc=1   <- collapsed
#
# The install is into a THROWAWAY GOBIN, so it keeps everything `go run` was
# chosen for elsewhere in this repo: the version is pinned identically local and
# in CI, the fetch is sumdb-verified, and neither go.mod is touched.
#
# 🔴 TEXT FORMAT, AND EVERY OTHER FORMAT FAILS OPEN. Measured on the same
# fixture: `-format json`, `-json`, `-format sarif` and `-format openvex` ALL
# return rc=0 with the called vulnerability present. That is not a quirk to work
# around -- `errVulnerabilitiesFound` has exactly ONE return site, in the TEXT
# formatter, and its own source comment says "This returns exit status 3 when
# running without the -json flag." So a future job reaching for sarif to feed a
# security dashboard would be permanently green. If you need machine-readable
# output, run govulncheck a SECOND time for it; do not gate on it.
#
# `-test=false` IS PASSED EXPLICITLY EVEN THOUGH IT IS THE DEFAULT, and that is a
# scope decision rather than noise. `scripts/deadcode-gate.sh` runs `deadcode
# -test`, so a reader arriving from that file assumes symmetry; writing the value
# out makes the asymmetry visible instead of hidden in a default. The reason for
# the value: this gate's predicate is "is a vulnerable function reachable from
# SHIPPED code". `-test` would add 293 api and 11 controller test files to the
# call graph, none of which is in any image -- and because these targets are
# reachable from jobs in `.publish_needs`, a test-only finding would block a
# RELEASE for a path that does not ship. govulncheck has NO suppression,
# baseline or allowlist mechanism of any kind (`-h` at v1.1.4 offers only
# -C -db -format -json -mode -scan -show -tags -test), so every widening of scope is a
# widening of the UNREMEDIABLE-red surface. Enabling it changes nothing today
# (both modules read 0 called either way), which is exactly when the decision is
# cheap to make deliberately.
#
# 🔴 A FINDING PATH AND A LOADER-ERROR PATH ARE NOT THE SAME SHAPE. On the
# unparseable-file arm govulncheck prints ABSOLUTE paths. So the "a `../` or a
# foreign tree in a finding path means an invalid run" check that carry-forward 4
# prescribes applies to FINDING lines, not to this tool's loader errors -- which
# is why those go to stderr and produce exit 2 here rather than being parsed.
set -eu

MODULE_DIR="${1:-}"
TOOL="${2:-}"

if [ -z "$MODULE_DIR" ] || [ -z "$TOOL" ]; then
  echo "usage: scripts/govulncheck-gate.sh <module-dir> <pkg@version>" >&2
  echo "  e.g. scripts/govulncheck-gate.sh api golang.org/x/vuln/cmd/govulncheck@v1.1.4" >&2
  exit 2
fi

# The pinned tool package is passed IN rather than defaulted here, so that
# `task`'s echo shows the version -- the same reason deadcode-gate.sh takes it as
# an argument. The Taskfile header calls that echo the mechanism by which a
# teammate notices a pin going missing.
if [ ! -d "$MODULE_DIR" ]; then
  echo "govulncheck-gate: no such module directory: $MODULE_DIR" >&2
  exit 2
fi

# 🔴 THIS GATE'S ENVIRONMENT IS NOT TRUSTED, AND TWO VARIABLES CAN MAKE IT REPORT
# CLEAN OVER A GENUINELY CALLED VULNERABILITY. Not hypothetical and not a missed
# finding: in both cases the gate AFFIRMATIVELY PRINTS "No vulnerabilities found."
# and exits 0. These targets are reachable from `.publish_needs`, and GitLab ranks
# project-level and manual-pipeline variables ABOVE job variables, so the disarm
# needs no file, no diff and nothing a reviewer can see. Same class and same remedy
# as `scripts/scan-secrets.sh`'s refusal of GITLEAKS_CONFIG; read its comment.
#
# Measured 2026-08-04 against this script, on a fixture calling
# golang.org/x/text@v0.3.5's language.Parse (GO-2021-0113, genuinely called):
#
#   no env                                       rc=1   GO-2021-0113 named
#   + GOPACKAGESDRIVER=<3-line stub>             rc=0   "No vulnerabilities found."
#   + GOFLAGS=-tags=<a tag the fixture guards>   rc=0   same
#   + GOFLAGS=-tags=<a tag guarding nothing>     rc=1   <- THE NON-CONTROL
#
# THAT LAST ROW IS WHY THIS GUARD ALMOST DID NOT EXIST. The first attempt to
# demonstrate the -tags disarm used a tag that excluded no file, got rc=3, and was
# banked as "no analogue here" -- an instrument that could not produce the
# disconfirming answer returning the reassuring one, in a note whose whole purpose
# was to stop the next person looking. The discriminating input is a tag the
# fixture actually guards on.
if [ -n "${GOPACKAGESDRIVER:-}" ]; then
  echo "govulncheck-gate: GOPACKAGESDRIVER is set in the environment. Refused." >&2
  echo "  It names an external program that REPLACES go/packages' package loading," >&2
  echo "  which is how govulncheck builds its call graph. A three-line stub printing" >&2
  echo "  an empty package set makes this gate report a clean tree, with no file and" >&2
  echo "  no diff. Unset it, or use a job-level variable that does not." >&2
  exit 2
fi

# GOFLAGS gets the NARROW treatment: refuse `-tags` only, never the variable
# wholesale. `GOFLAGS=-buildvcs=false` is a DOCUMENTED workflow here -- Taskfile.yml's
# header, `.claude/agents/coder.md`, `.claude/agents/tester.md` and `specs/ai.md` all
# tell contributors to export it in a linked worktree -- so a blanket refusal would
# make this gate unrunnable in exactly the shells that run `task gate`. Build tags
# are what shrink the package set; `-buildvcs` cannot.
#
# 🔴 READ `go env GOFLAGS`, NOT `$GOFLAGS`. The environment variable is only one of
# three routes to the same file selection, and the other two leave it EMPTY, so a
# `case "${GOFLAGS:-}"` guard passes while the tags are in force: `GOENV=<file>`
# naming a file that sets GOFLAGS, and a persisted `go env -w GOFLAGS=-tags=...` in
# the user's default env file. Measured by the Unit B reviewer via GOENV -- the
# stubbed build produced `[main.go stub.go]` against the control's
# `[main.go normal.go]` -- with an empty GOFLAGS throughout. `go env GOFLAGS`
# resolves all three with one read, because it is the same resolution the build
# itself performs. No GOENV file exists on this host today; the guard is for the
# route, not the instance.
effective_goflags="$(go env GOFLAGS 2>/dev/null || true)"
case "$effective_goflags" in
  *-tags*)
    echo "govulncheck-gate: the effective GOFLAGS contains -tags. Refused." >&2
    echo "  Read from \`go env GOFLAGS\`, which resolves the environment variable," >&2
    echo "  a GOENV=<file> override and a persisted \`go env -w\` alike." >&2
    echo "  Build tags select which files compile, so a tag that excludes the file" >&2
    echo "  holding the vulnerable call makes this gate report clean. GOFLAGS itself" >&2
    echo "  is fine and is deliberately still honoured: -buildvcs=false is a" >&2
    echo "  documented workflow in a linked git worktree. Drop the -tags." >&2
    exit 2
    ;;
esac

# GOOS/GOARCH are NOT refused and fail closed, and the MECHANISM is the install
# refusing outright -- recorded so nobody "tidies away" the check that catches them.
#
# 🔴 CORRECTED 2026-08-04 (PRD #103 MR-C, Unit B review). This said the cross-built
# binary lands in $GOBIN/<goos>_<goarch>/ so the `[ ! -x ... ]` test below misses it,
# and warned that "if that install check is ever loosened, this hole opens." Both
# halves are wrong. Measured twice independently on go1.26.5:
#
#   GOBIN=<tmp> GOOS=linux GOARCH=amd64 go install <pkg@version>
#     -> rc=1, "go: cannot install cross-compiled binaries when GOBIN is set",
#        $GOBIN never created
#
# So the exit 2 comes from the `irc -ne 0` arm and NEVER from the `-x` arm, and
# loosening the `-x` test opens nothing. The <goos>_<goarch>/ subdirectory is what
# `go install` does when GOBIN is UNSET; this script always sets it, three lines
# down, which is precisely why the refusal fires.

# No `-t` on mktemp: it is not portable, and only CI could show it (f0e3c438).
TMP="$(mktemp -d)"
# shellcheck disable=SC2064  # expand TMP now: the trap must survive its unset.
trap "rm -rf '$TMP'" EXIT INT TERM

# THROWAWAY GOBIN. `go install pkg@version` ignores the current module's go.mod
# by construction (Go 1.16+), so this writes nothing to either module.
irc=0
GOBIN="$TMP/bin" go install "$TOOL" >"$TMP/install.out" 2>&1 || irc=$?
if [ "$irc" -ne 0 ] || [ ! -x "$TMP/bin/govulncheck" ]; then
  cat "$TMP/install.out" >&2
  # Two arms, two messages. The `-x` arm is reachable at irc=0 -- a pkg@version
  # whose binary is not named `govulncheck` installs fine and lands elsewhere in
  # $GOBIN -- and reporting that as "could not install (exit 0)" is a
  # self-contradiction that sends the reader after a build failure that did not
  # happen. The usage text advertises a generic <pkg@version>, so this is a
  # configuration mistake worth naming precisely rather than an unreachable arm.
  if [ "$irc" -ne 0 ]; then
    echo "govulncheck-gate: could not install $TOOL (exit $irc)." >&2
  else
    echo "govulncheck-gate: $TOOL installed (exit 0) but produced no" >&2
    echo "  \$GOBIN/govulncheck. This gate runs that exact binary name; a package" >&2
    echo "  whose main is named anything else cannot be used here." >&2
  fi
  echo "  This is an INSTRUMENT failure, not a finding. On a cold module cache" >&2
  echo "  this step fetches and builds the tool and therefore needs the network." >&2
  exit 2
fi

# 🔴 RC FIRST. Redirect to files and read `$?` on the very next line -- do not
# pipe, do not `$( )` into a test. `|| rc=$?` puts the command in a condition
# context so errexit does not pre-empt the capture.
#
# `-show verbose` IS LOAD-BEARING ON THE CLEAN ARM AND IS NOT DECORATION. Without
# it the clean report prints a bare COUNT -- "2 vulnerabilities in modules you
# require" plus the tool's own "Use '-show verbose' for more details." -- and names
# nothing. Measured (Unit B review, `probes/prd-103-mrc-b-reviewer/p4-...`): same
# binary, same module, same `-test=false`, differing in this one flag, 0 GO-IDs
# named against 2 (GO-2026-5932, GO-2026-5942). The residual UNCALLED set is the
# thing that becomes a red the day somebody writes a call to it, so a count is not
# enough to act on. It is a DISPLAY flag: it cannot change the verdict, and the
# rc 0/3/* mapping below is untouched by it.
rc=0
(cd "$MODULE_DIR" && "$TMP/bin/govulncheck" -test=false -show verbose ./...) \
  >"$TMP/out" 2>"$TMP/err" || rc=$?

case "$rc" in
  0)
    # Print the report even when clean: with `-show verbose` above it NAMES the
    # residual UNCALLED vulnerabilities, and there is no other place they surface.
    # (Before that flag was added this comment claimed the same thing and was
    # false -- the report gave a count. Fixed on both sides rather than by
    # softening the sentence, because the count alone is not actionable.)
    cat "$TMP/out"
    # Written as an `if` and not `[ -s ... ] && cat`, for the reason
    # scripts/deadcode-gate.sh records at the same spot: that idiom's status is
    # the TEST's when the file is empty, which is the common case here, and it is
    # a coin-flip against errexit.
    if [ -s "$TMP/err" ]; then cat "$TMP/err" >&2; fi
    echo "govulncheck-gate: $MODULE_DIR clean (0 called vulnerabilities)"
    exit 0
    ;;
  3)
    cat "$TMP/out"
    if [ -s "$TMP/err" ]; then cat "$TMP/err" >&2; fi
    echo "govulncheck-gate: $MODULE_DIR has CALLED vulnerabilities (govulncheck exit 3 -> gate exit 1)." >&2
    echo "  The fix is a dependency BUMP, in this MR. There is no suppression" >&2
    echo "  mechanism -- govulncheck has no baseline, allowlist or ignore file." >&2
    echo "  If a fix genuinely does not exist upstream yet, that is a decision for" >&2
    echo "  the user, not a flag: say so on the MR." >&2
    exit 1
    ;;
  *)
    cat "$TMP/out"
    cat "$TMP/err" >&2
    echo "govulncheck-gate: $TOOL exited $rc over $MODULE_DIR -- INSTRUMENT FAILURE, NOT FINDINGS." >&2
    echo "  govulncheck uses 3 for findings, 2 for a usage error and 1 for everything" >&2
    echo "  else, including an unreachable vulnerability database and a module that" >&2
    echo "  does not compile. Do not read this as a security result." >&2
    exit 2
    ;;
esac
