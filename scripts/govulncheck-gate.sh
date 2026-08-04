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
# SHIPPED code". `-test` would add 293 api and 10 controller test files to the
# call graph, none of which is in any image -- and because these targets are
# reachable from jobs in `.publish_needs`, a test-only finding would block a
# RELEASE for a path that does not ship. govulncheck has NO suppression,
# baseline or allowlist mechanism of any kind (`-h` at v1.1.4 offers only
# -C -db -format -mode -scan -show -tags -test), so every widening of scope is a
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
  echo "govulncheck-gate: could not install $TOOL (exit $irc)." >&2
  echo "  This is an INSTRUMENT failure, not a finding. On a cold module cache" >&2
  echo "  this step fetches and builds the tool and therefore needs the network." >&2
  exit 2
fi

# 🔴 RC FIRST. Redirect to files and read `$?` on the very next line -- do not
# pipe, do not `$( )` into a test. `|| rc=$?` puts the command in a condition
# context so errexit does not pre-empt the capture.
rc=0
(cd "$MODULE_DIR" && "$TMP/bin/govulncheck" -test=false ./...) \
  >"$TMP/out" 2>"$TMP/err" || rc=$?

case "$rc" in
  0)
    # Print the report even when clean: it names the residual UNCALLED
    # vulnerabilities, which are the ones that become a red the day somebody
    # writes a call to them, and there is no other place they surface.
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
