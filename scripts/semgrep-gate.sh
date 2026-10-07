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
# gate tools (yamllint, shellcheck) -- `command -v` + a loud fail-open SKIP when
# absent, required only when UZI_SAST_REQUIRED is set. Acquisition on a uzi worker is the baked
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
  # Keyed ONLY on UZI_SAST_REQUIRED, NOT on a bare CI=true. GitHub Actions always
  # sets CI=true, but the lint-repo job (which runs `task gate:repo`) does not
  # install semgrep until M5 wires it (a .github/workflows edit the uzi worker PAT
  # cannot push). Requiring on bare CI therefore reddened main the moment M2 landed
  # semgrep into gate:repo ahead of the tool being present. So CI enforcement is
  # opt-in via UZI_SAST_REQUIRED, set by M5 in the same job that installs semgrep --
  # the exact UZI_LINT_YAML_REQUIRED precedent. Until then CI skips gracefully, while
  # a uzi worker (semgrep baked, M0) still enforces because command -v succeeds there.
  truthy "${UZI_SAST_REQUIRED:-}" && return 0
  return 1
}

# 🔴 IS THE TOOL EVEN HERE? Asserted before anything else. gate:repo runs FIRST
# inside `task gate`, so a hard failure on a missing tool would stop gate:api,
# gate:web and every other component gate from running at all (PRD #103 Decision 2
# -- a gate people cannot run is a gate that stops being run). So absent -> loud
# fail-open SKIP; required (UZI_SAST_REQUIRED) -> exit 2.
if ! command -v semgrep >/dev/null 2>&1; then
  if required; then
    echo "semgrep-gate: no semgrep on PATH, and this run is REQUIRED" >&2
    echo "  (UZI_SAST_REQUIRED is set)." >&2
    echo "  In a CI job that set it, the job image no longer installs semgrep; on a" >&2
    echo "  uzi worker it means the baked toolchain (PRD #862 M0) has not rolled." >&2
    exit 2
  fi
  echo "semgrep-gate: ================================================================"
  echo "semgrep-gate: SKIPPED -- NO SAST SCAN WAS RUN."
  echo "semgrep-gate: semgrep is not on PATH. It is a Python package, so most"
  echo "semgrep-gate: contributors will not have it until they ask for it."
  echo "semgrep-gate:"
  echo "semgrep-gate: This is FAIL-OPEN and deliberate. gate:repo runs FIRST inside"
  echo "semgrep-gate: \`task gate\`, so failing here would stop gate:api, gate:web and"
  echo "semgrep-gate: every other component gate from running at all. A uzi worker has"
  echo "semgrep-gate: semgrep baked in (PRD #862 M0), so the gate enforces there; CI"
  echo "semgrep-gate: enforcement is wired by M5, which installs semgrep and sets"
  echo "semgrep-gate: UZI_SAST_REQUIRED in the lint-repo job."
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
if ! git --literal-pathspecs ls-files --error-unmatch -- "$CANARY" >/dev/null 2>&1; then
  echo "semgrep-gate: canary is NOT TRACKED: $CANARY" >&2
  echo "  It is on disk, so semgrep still scans and fires on it -- but a canary" >&2
  echo "  outside the git index attests liveness over a population CI does not" >&2
  echo "  check out. \`git add $CANARY\` (or restore it if removed on purpose)." >&2
  exit 2
fi

# Capture lives in a private directory, including a settings path which does not
# yet exist. Explicitly exclude its verified physical path from both scans.
umask 077
if ! command -v python3 >/dev/null 2>&1; then
  echo 'semgrep-gate: reason=missing_capture_tool' >&2
  exit 2
fi
mkdir -p "$ROOT/.uzi/scratch"
CAPTURE_DIR="$(mktemp -d "$ROOT/.uzi/scratch/semgrep-capture.XXXXXX")" || exit 2
HELPER_PID=''
# shellcheck disable=SC2329 # Invoked by the EXIT trap; exercised by the focused suite.
cleanup() {
  rm -rf -- "$CAPTURE_DIR"
}
# shellcheck disable=SC2329 # Invoked by signal traps; exercised by the focused suite.
interrupt() {
  # Python owns scanner-group retirement; wait for it before removing evidence.
  trap '' INT TERM HUP
  if [ -n "$HELPER_PID" ]; then
    kill -TERM "$HELPER_PID" 2>/dev/null || :
    wait "$HELPER_PID" 2>/dev/null || :
  fi
  echo 'semgrep-gate: reason=interrupted' >&2
  exit 2
}
trap 'cleanup' EXIT
trap 'interrupt' INT TERM HUP
CAPTURE_DIR="$(cd "$CAPTURE_DIR" && pwd -P)" || exit 2
case "$CAPTURE_DIR" in
  "$ROOT"/*) CAPTURE_EXCLUDE="${CAPTURE_DIR#"$ROOT"/}" ;;
  *) echo 'semgrep-gate: reason=invalid_capture_path' >&2; exit 2 ;;
esac
chmod 700 "$CAPTURE_DIR"
SEMGREP_SETTINGS_FILE="$CAPTURE_DIR/settings.yml"
export SEMGREP_SETTINGS_FILE
if [ -z "${SSL_CERT_FILE:-}" ] && [ -f /etc/ssl/certs/ca-certificates.crt ]; then
  export SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
fi
REPORTER="$ROOT/scripts/semgrep-report.py"
if [ ! -f "$REPORTER" ] || [ ! -r "$REPORTER" ]; then
  echo 'semgrep-gate: reason=renderer_failure' >&2
  exit 2
fi
run_stage() {
  stage="$1"
  shift
  stage_dir="$CAPTURE_DIR/$stage"
  mkdir "$stage_dir"
  # One requested argv, no shell interpretation. The helper drains both pipes
  # concurrently and owns the POSIX process group until cleanup settles.
  python3 -B "$REPORTER" capture "$stage_dir" -- \
    semgrep scan --config "$RULES_DIR" --error --strict --metrics=off \
    --disable-version-check --json --timeout 30 --timeout-threshold=0 \
    --exclude "$CAPTURE_EXCLUDE" "$@" 2>/dev/null &
  HELPER_PID=$!
  capture_rc=0
  wait "$HELPER_PID" || capture_rc=$?
  HELPER_PID=''
  report_rc=0
  python3 -B "$REPORTER" report "$stage_dir" "$stage" "$CANARY" "$RULES_DIR" \
    > "$stage_dir/report" 2>/dev/null || report_rc=$?
  # An interpreter failure can exit 2 just like a structured scanner error.
  # Require report evidence before accepting any renderer exit status.
  if [ ! -s "$stage_dir/report" ]; then
    echo 'semgrep-gate: reason=renderer_failure' >&2
    return 2
  fi
  if [ "$capture_rc" -ne 0 ]; then
    if [ "$report_rc" -eq 2 ]; then
      cat "$stage_dir/report"
    fi
    echo 'semgrep-gate: reason=capture_failure' >&2
    return 2
  fi
  case "$report_rc" in
    0|1|2)
      if ! cat "$stage_dir/report"; then
        echo 'semgrep-gate: reason=renderer_failure' >&2
        return 2
      fi
      return "$report_rc"
      ;;
    *) echo 'semgrep-gate: reason=renderer_failure' >&2; return 2 ;;
  esac
}
rc=0
run_stage canary "$CANARY" || rc=$?
if [ "$rc" -ne 0 ]; then
  exit 2
fi
rc=0
run_stage tree --exclude "${CANARY##*/}" . || rc=$?
exit "$rc"
