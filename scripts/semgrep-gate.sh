#!/bin/sh
# Run semgrep over the tree, and PROVE THE SCANNER WAS LIVE before believing its
# verdict (PRD #862 M2).
#
# usage: scripts/semgrep-gate.sh <rules-dir> <canary-file>
#   e.g. scripts/semgrep-gate.sh semgrep scripts/semgrep-canary.txt
#
# A BYO-ON-PATH WRAPPER mirroring scripts/lint-yaml.sh: semgrep is a Python/OCaml
# tool with no static binary, so unlike gitleaks/golangci it is not fetched via a
# pinned `go run`/curl+sha256. It follows the repo's existing pattern for non-Go
# gate tools (yamllint, shellcheck) -- `command -v` + a loud fail-open SKIP
# locally, forced-required in CI. Acquisition on a uzi worker is the baked
# toolchain (PRD #862 M0); a dev installs it (`pipx install semgrep`, or nixpkgs
# `semgrep`).
#
# 🔴 A SCRIPT, NOT AN INLINE `cmds:` LINE, for the reason recorded at
# scripts/lint-shell.sh: gate:repo's lint:shell walks every tracked *.sh, so a
# committed script is LINTED by that check and an inline Taskfile recipe is not.
# This file is #!/bin/sh, POSIX, and lint-clean by design.
#
# 🔴 THE CANARY IS THE WHOLE POINT. A SAST gate whose healthy state is silence
# cannot, from an empty report, tell a clean tree from a scanner that never ran
# (a broken config, an unreadable rules dir, a semgrep that aborted before
# scanning). So this wrapper first runs the proof rule against the canary file and
# REQUIRES it to fire; only then does it trust semgrep's verdict on the real tree.
# That makes a clean run a positive observation, satisfying CLAUDE.md's "a control
# that produces no output is not a control" -- same discipline as scan-secrets.sh.
#
# 🔴 BOTH THE RULES DIR AND THE CANARY ARE ARGUMENTS, NOT HARDCODED, for the same
# reason lint-yaml.sh takes its config path: they are separate tracked files that
# can move, and this is where `task`'s echo lets a reader see them in play.
#
# EXIT CODES (the convention fmt-check:api, lint:api, deadcode-gate.sh and the
# three lint-*.sh / scan-secrets.sh scripts set):
#     2 = the instrument is broken (usage error, not a git tree, missing rules
#         dir, canary missing/untracked, DEAD canary, semgrep error, or
#         absent-while-required)
#     1 = there are findings
#     0 = clean, and the canary was seen -- or a loud, banner-printed SKIP (locally
#         only, when semgrep is absent)
# `task`'s own rc is 201 for all of them.
set -eu

RULES_DIR="${1:-}"
CANARY="${2:-}"

if [ -z "$RULES_DIR" ] || [ -z "$CANARY" ]; then
  echo "usage: scripts/semgrep-gate.sh <rules-dir> <canary-file>" >&2
  echo "  e.g. scripts/semgrep-gate.sh semgrep scripts/semgrep-canary.txt" >&2
  exit 2
fi

# Run from the repo root whatever the caller's directory -- `git ls-files` from a
# subdirectory silently narrows to that subtree, and semgrep's `.` target must be
# the tree root. See lint-yaml.sh.
ROOT="$(git rev-parse --show-toplevel)" || {
  echo "semgrep-gate: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || exit 2

# Identical to lint-yaml.sh's / lint-shell.sh's, deliberately duplicated: these
# scripts are standalone by design and a shared `source`d library would put the
# gate's fail-closed property behind a file-resolution step. Read tolerantly
# because a guard whose failure mode is to switch itself off must not be picky
# about spelling.
truthy() {
  case "${1:-}" in
    ''|0|[fF]alse|[fF]ALSE|[nN]o|[nN]O|[oO]ff|[oO]FF) return 1 ;;
    *) return 0 ;;
  esac
}
required() {
  truthy "${UZI_SAST_REQUIRED:-}" && return 0
  truthy "${CI:-}" && return 0
  return 1
}

# 🔴 IS THE TOOL EVEN HERE? Asserted before anything else. gate:repo runs FIRST
# inside `task gate`, so a hard failure on a missing tool would stop gate:api,
# gate:web and every other component gate from running at all (PRD #103 Decision 2
# -- a gate people cannot run is a gate that stops being run). So absent -> loud
# fail-open SKIP; required (CI, or UZI_SAST_REQUIRED) -> exit 2.
if ! command -v semgrep >/dev/null 2>&1; then
  if required; then
    echo "semgrep-gate: no semgrep on PATH, and this run is REQUIRED" >&2
    echo "  (UZI_SAST_REQUIRED and/or CI is set)." >&2
    echo "  In CI this means the job image no longer installs it; on a uzi worker it" >&2
    echo "  means the baked toolchain (PRD #862 M0) has not rolled to the fleet." >&2
    exit 2
  fi
  echo "semgrep-gate: ================================================================"
  echo "semgrep-gate: SKIPPED -- NO SAST SCAN WAS RUN."
  echo "semgrep-gate: semgrep is not on PATH. It is a Python package, so most"
  echo "semgrep-gate: contributors will not have it until they ask for it."
  echo "semgrep-gate:"
  echo "semgrep-gate: This is FAIL-OPEN and deliberate. gate:repo runs FIRST inside"
  echo "semgrep-gate: \`task gate\`, so failing here would stop gate:api, gate:web and"
  echo "semgrep-gate: every other component gate from running at all. CI sets"
  echo "semgrep-gate: UZI_SAST_REQUIRED, so the SAST rules ARE enforced on every MR."
  echo "semgrep-gate:"
  echo "semgrep-gate: To run it here: \`pipx install semgrep\` (or nixpkgs semgrep)."
  echo "semgrep-gate: ================================================================"
  exit 0
fi

# The rules dir and canary are separate tracked files that can be deleted or
# moved; assert them here so a missing one is an instrument failure (2), never a
# quiet scan-without-a-control.
if [ ! -d "$RULES_DIR" ]; then
  echo "semgrep-gate: rules dir not found (or not a directory): $RULES_DIR" >&2
  echo "  Restore it from git. Without the rules there is nothing to scan with." >&2
  exit 2
fi

if [ ! -f "$CANARY" ]; then
  echo "semgrep-gate: canary file not found: $CANARY" >&2
  echo "  Restore it from git. Without the canary this gate cannot tell a clean" >&2
  echo "  tree from a scanner that never ran, which is the only thing it is for." >&2
  exit 2
fi

# 🔴 THE CANARY MUST BE IN THE INDEX, and this is NOT the same check as the `-f`
# test above. A canary on disk but untracked is still scanned and still fires, so
# it still says DETECTED -- while the population it attests for has changed. Its
# green would then be signed by a witness that no longer speaks for anything CI
# checks out. See scan-secrets.sh's identical guard.
if ! git ls-files --error-unmatch "$CANARY" >/dev/null 2>&1; then
  echo "semgrep-gate: canary is NOT TRACKED: $CANARY" >&2
  echo "  It is on disk, so semgrep still scans and fires on it -- but a canary" >&2
  echo "  outside the git index attests liveness over a population CI does not" >&2
  echo "  check out. \`git add $CANARY\` (or restore it if removed on purpose)." >&2
  exit 2
fi

# 🔴 SEMGREP NEEDS A WRITABLE SETTINGS PATH OR IT ABORTS. Verified on a live
# 0.71.0 worker (2026-08-30): the baked binary aborts on first run unless it can
# write $HOME/.semgrep/settings.yml. Point it at a NON-EXISTENT writable path so
# semgrep writes a fresh valid file -- a pre-created empty file instead prints a
# "Bad settings format ... will be overridden" warning. `mktemp -u` yields a name
# that does not exist yet.
SEMGREP_SETTINGS_FILE="$(mktemp -u "${TMPDIR:-/tmp}/uzi-semgrep-settings.XXXXXX")" || {
  echo "semgrep-gate: mktemp -u failed" >&2
  exit 2
}
export SEMGREP_SETTINGS_FILE
# semgrep MATERIALIZES that path (it writes a fresh settings file even with
# --metrics=off, verified 2026-08-30), so without this the gate leaks one temp
# file per run into TMPDIR -- and it runs on every `task gate` and CI job. The
# EXIT trap removes it on every exit path (clean, findings, or instrument error).
trap 'rm -f "$SEMGREP_SETTINGS_FILE"' EXIT

# 🔴 SEMGREP ALSO ABORTS BUILDING ITS X509/OTel CLIENT WITHOUT A CA BUNDLE.
# Verified same session. Only set SSL_CERT_FILE when the caller has not, and only
# when the standard bundle actually exists (guard both, so this never points
# semgrep at a path that is not there).
if [ -z "${SSL_CERT_FILE:-}" ] && [ -f /etc/ssl/certs/ca-certificates.crt ]; then
  export SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
fi

CANARY_BASE="${CANARY##*/}"

# 🔴 LIVENESS RUN. Scan ONLY the canary file with the proof rule; it MUST fire.
# With `--error`, semgrep exits 1 on findings, 0 when clean, >=2 on an instrument
# error. An exit 0 here means the canary is DEAD -- semgrep did not actually scan,
# or the proof rule is broken -- which is the vacuous 0-findings pass this canary
# exists to prevent, so it is an instrument failure (2), not a clean gate.
rc=0
semgrep scan --config "$RULES_DIR" --error --metrics=off --disable-version-check \
  "$CANARY" >/dev/null 2>&1 || rc=$?
case "$rc" in
  1) : ;;  # canary fired -- the scanner is live, as required
  0)
    echo "semgrep-gate: THE CANARY DID NOT FIRE. THE SCANNER WAS BLIND." >&2
    echo "  Scanning $CANARY with the rules in $RULES_DIR produced NO finding, so" >&2
    echo "  semgrep did not actually look (or the proof rule is broken). A" >&2
    echo "  0-findings gate over the real tree would therefore be vacuous. This is" >&2
    echo "  an INSTRUMENT failure (exit 2), never a clean run. In order of" >&2
    echo "  likelihood: the canary token was edited, $RULES_DIR lost the proof" >&2
    echo "  rule, or its \`paths: include\` no longer names $CANARY." >&2
    exit 2
    ;;
  *)
    echo "semgrep-gate: semgrep exited $rc scanning the canary $CANARY, which is" >&2
    echo "  neither findings (1) nor clean (0). This is an INSTRUMENT failure --" >&2
    echo "  a bad rules dir, an unreadable file, or semgrep itself failing to run." >&2
    exit 2
    ;;
esac

# 🔴 GATE RUN. Scan the whole tree, EXCLUDING the canary file so the proof rule
# contributes nothing to the gate's verdict. semgrep's default ignores skip
# node_modules/, dist/, .git/ and friends, so `.` does not walk the JS deps
# (measured PRD #862 M2). `--error` makes the exit code parser-free: 0 clean,
# 1 findings, >=2 instrument error.
rc=0
semgrep scan --config "$RULES_DIR" --error --metrics=off --disable-version-check \
  --exclude "$CANARY_BASE" . >/dev/null 2>&1 || rc=$?

case "$rc" in
  0)
    echo "semgrep-gate: clean -- 0 findings under $RULES_DIR (canary $CANARY DETECTED)."
    echo "semgrep-gate: the canary firing is the positive observation: without it this"
    echo "semgrep-gate: green would be indistinguishable from a scanner that never looked."
    exit 0
    ;;
  1)
    echo "semgrep-gate: findings under $RULES_DIR. Re-running to print them:" >&2
    # Re-run WITHOUT suppressing output so the findings are shown. Its own exit is
    # ignored (`|| true`): the verdict was already decided by the quiet run above,
    # and set -eu would otherwise abort on this expected non-zero.
    semgrep scan --config "$RULES_DIR" --error --metrics=off --disable-version-check \
      --exclude "$CANARY_BASE" . >&2 || true
    exit 1
    ;;
  *)
    echo "semgrep-gate: semgrep exited $rc over the tree, which is neither clean (0)" >&2
    echo "  nor findings (1). This is an INSTRUMENT failure, not a scan result." >&2
    exit 2
    ;;
esac
