#!/bin/sh
# Run knip over an npm component (web / agent) -- and SKIP LOUDLY, exit 0, when
# knip is not installed locally (PRD #293 M3).
#
# usage: scripts/deadcode-knip.sh <component-dir>
#        e.g. scripts/deadcode-knip.sh web
#
# A SCRIPT, NOT AN INLINE `cmds:` LINE, and modelled EXACTLY on
# scripts/lint-formula.sh -- read that file's header for the full argument; this
# one states only what is different.
#
# 🔴 WHY THIS FILE EXISTS: a bare `npm run knip` execs node_modules/.bin/knip,
# which is `sh: knip: not found` (exit 127) when the component's toolchain is not
# installed. The umbrella `task deadcode` runs `deadcode:web` AND `deadcode:agent`
# regardless of which component a change touched, so a change that only touched
# `api/` would red on `agent`'s ABSENT knip -- a FALSE RED on a sibling whose
# toolchain was never the point of the run. A missing instrument is not a finding,
# and it must not read as one.
#
# 🔴 THE FIX IS THE *LiveDB / lint-formula SHAPE: fail-open locally, required in
# CI. If knip is absent this SKIPS (loud banner, exit 0) so a contributor who only
# installed one component's deps still gets a green umbrella gate. CI always
# `npm ci`s knip, and arms this with UZI_DEADCODE_<COMPONENT>_REQUIRED=1, which
# turns a missing knip into `exit 2` -- so a skip in CI (which means the image
# regressed) can never look like a pass. A SKIPPED RUN AND A PASSING RUN MUST NOT
# LOOK ALIKE, which is the whole reason the skip branch prints a banner instead of
# nothing.
#
# 🔴 IT DELEGATES TO `npm run knip`, NEVER knip DIRECTLY. package.json's `knip`
# script is a bare `knip` on purpose: every flag lives in the component's
# knip.jsonc (staging of the exports/types family at `warn`, etc.), so invoking
# knip directly here would drop those flags silently. And NEVER `npx knip`: npx
# FETCHES FROM THE NETWORK when the dep is missing, which a gate may not do -- so
# presence is resolved by looking for the local binary that `npm run knip` would
# exec, not by asking npx to conjure one.
#
# EXIT CODES (the convention lint-formula.sh / deadcode-gate.sh set):
#     2 = the instrument is broken / required-but-absent   0 = clean OR loud skip
# and whatever `npm run knip` itself exits when knip IS present (knip gates unused
# files/deps at zero; the warn tier prints and does not set the code).
set -eu

DIR="${1:-}"

if [ -z "$DIR" ]; then
  echo "usage: scripts/deadcode-knip.sh <component-dir>" >&2
  echo "  e.g. scripts/deadcode-knip.sh web" >&2
  exit 2
fi

# Restrict the component to lowercase ASCII letters (web, agent, ...) BEFORE it is
# uppercased into a variable name and read via `eval` below. Defense in depth: the
# only callers pass fixed literals, but this makes the `eval` on the derived name
# safe by construction rather than by caller invariant — a name with shell-active
# characters can never reach the eval. Fail CLOSED (exit 2), never a silent skip.
case "$DIR" in
  *[!a-z]*)
    echo "deadcode-knip: component must be lowercase ASCII letters only: '$DIR'" >&2
    exit 2
    ;;
esac

# Run from the repo root so a relative component dir means the same thing from any
# caller's directory. Resolved from THIS SCRIPT'S OWN LOCATION, deliberately NOT via
# `git rev-parse --show-toplevel` (which lint-formula.sh uses): this wrapper runs inside
# validate:web's `node:22-alpine` CI image, which ships NO git, so a `git rev-parse` here
# fails and reds the very gate it exists to keep honest (PRD #293 M3; caught in CI
# 2026-08-10, pipeline 20725). The script lives in scripts/, so its parent is the root.
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)" || {
  echo "deadcode-knip: cannot resolve repo root from script location ($0)" >&2
  exit 2
}
cd "$ROOT" || exit 2

if [ ! -d "$DIR" ]; then
  echo "deadcode-knip: no such component directory: $DIR" >&2
  exit 2
fi

# Derive the required-var name from the component: web -> UZI_DEADCODE_WEB_REQUIRED.
UPPER="$(printf '%s' "$DIR" | tr 'a-z' 'A-Z')"
REQ_VAR="UZI_DEADCODE_${UPPER}_REQUIRED"
# Read the component-specific required var by its derived name.
eval "REQ_VAL=\"\${${REQ_VAR}:-}\""

# Identical to lint-formula.sh's -- read tolerantly, so `true`/`yes`/`1 ` all arm.
# A guard whose failure mode is to switch itself off must not be picky about spelling.
truthy() {
  case "${1:-}" in
    ''|0|[fF]alse|[fF]ALSE|[nN]o|[nN]O|[oO]ff|[oO]FF) return 1 ;;
    *) return 0 ;;
  esac
}
required() {
  truthy "${REQ_VAL:-}" && return 0
  truthy "${CI:-}" && return 0
  return 1
}

# Resolve knip presence WITHOUT the network: check for the local binary that
# `npm run knip` would exec. NEVER `npx knip` -- npx fetches from the network.
KNIP_BIN="$DIR/node_modules/.bin/knip"

if [ -x "$KNIP_BIN" ] || [ -f "$KNIP_BIN" ]; then
  # Present: delegate to package.json's `knip` script EXACTLY as the bare target
  # did, preserving every flag in the component's knip.jsonc.
  cd "$DIR" || exit 2
  exec npm run knip
fi

# knip is ABSENT below this point.
if required; then
  echo "deadcode-knip: knip is not installed for '$DIR', and this run is REQUIRED" >&2
  echo "  ($REQ_VAR and/or CI is set)." >&2
  echo "  Looked for: $KNIP_BIN. This is an INSTRUMENT failure (exit 2), not a finding." >&2
  echo "  In CI this means the job image no longer installs the knip it is supposed to." >&2
  echo "  The skip this replaces is fail-open and exists for contributor laptops that" >&2
  echo "  installed only one component's deps; CI must never be allowed to take it," >&2
  echo "  which is what this variable enforces." >&2
  exit 2
fi

# A SKIPPED RUN AND A PASSING RUN MUST NOT LOOK ALIKE. Hence the banner: a passing
# run prints knip's own output, and says nothing about skipping.
echo "deadcode-knip: ================================================================"
echo "deadcode-knip: SKIPPED -- knip was NOT run over '$DIR'."
echo "deadcode-knip: No knip binary at $KNIP_BIN, so this component's toolchain is"
echo "deadcode-knip: not installed. Running a bare 'npm run knip' here would exit 127"
echo "deadcode-knip: ('knip: not found'), which would red the umbrella 'task deadcode'"
echo "deadcode-knip: on a sibling component you may not have touched -- a false red on"
echo "deadcode-knip: an absent instrument, not a real finding."
echo "deadcode-knip:"
echo "deadcode-knip: This is FAIL-OPEN and deliberate: it is the same shape lint:formula"
echo "deadcode-knip: and the *LiveDB tests use (self-skip locally, required in CI). CI"
echo "deadcode-knip: runs this with $REQ_VAR=1, so knip IS run over '$DIR' on every MR"
echo "deadcode-knip: regardless of what you have installed locally."
echo "deadcode-knip:"
echo "deadcode-knip: To run it here: 'cd $DIR && npm ci' (or 'npm install'), then rerun."
echo "deadcode-knip: ================================================================"
exit 0
