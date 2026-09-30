#!/bin/sh
# Gate the /api/v1 OpenAPI document on backward compatibility (PRD #1908 Decision 11).
#
# usage: scripts/check-api-v1-compat.sh <oasdiff-version> <canary-dir> [spec-path]
#   e.g. scripts/check-api-v1-compat.sh v1.32.1 scripts/api-v1-compat-canary api/openapi/v1.yaml
#
# WHAT IT CHECKS. The stable external API is a contract (api/openapi/v1.yaml's header:
# additive changes only). This diffs the spec as it is on origin/main against the copy in
# the working tree with oasdiff and fails on ANY breaking change (severity ERR): a removed
# path, a removed or renamed response field, a new required request field, a removed enum
# value in a request. No diff logic lives in the repo; oasdiff is the pinned tool that
# scripts/oasdiff.sh acquires (sha256-verified release archive).
#
# HOW THE BASE IS RESOLVED, AND WHY IT FAILS CLOSED. CI's lint-repo job checks out
# shallow, where origin/main does not exist. When refs/remotes/origin/main does not resolve
# this script fetches exactly that ref (`git fetch --no-tags --depth=1 origin
# +refs/heads/main:refs/remotes/origin/main`). If the fetch fails, or the base has no spec,
# the check exits 2: a gate that cannot see its baseline must not read as "no breaking
# changes". A ref that already resolves is used as is, so a developer's offline
# `task gate:repo` does not need the network for the base.
#
# 🔴 A LIVENESS CANARY, BECAUSE A SILENT PASS IS THE FAILURE MODE. Before the real
# comparison it runs oasdiff over a committed pair (<canary-dir>/base.yaml and
# revision.yaml, the second dropping one response field) and REQUIRES oasdiff to report a
# breaking change (exit 1), and it runs the base against itself and REQUIRES a clean exit.
# A tool that never fires (a wrong flag, a changed default, an archive that runs but
# compares nothing) would otherwise pass every real diff. A clean run prints that the
# canary fired.
#
# EXIT CODES (the check-migration-numbering.sh convention; `task` itself reports 201):
#     2 = the instrument is broken (no git, base unresolvable, base has no spec, the
#         wrapper or oasdiff failed to run, or the canary did not fire as required)
#     1 = there is a breaking change against origin/main
#     0 = no breaking change, and the canary fired
#
# oasdiff's own statuses: 0 clean, 1 findings at --fail-on level, 100+ tool errors (102
# is a spec it could not load); anything but 0/1 is reported here as exit 2.
#
# The wrapper path is overridable (OASDIFF_WRAPPER) only so scripts/check-api-v1-compat.test.sh
# can drive this script with a fake; production always uses scripts/oasdiff.sh.
set -eu

if [ "$#" -lt 2 ]; then
  echo "usage: scripts/check-api-v1-compat.sh <oasdiff-version> <canary-dir> [spec-path]" >&2
  echo "  e.g. scripts/check-api-v1-compat.sh v1.32.1 scripts/api-v1-compat-canary api/openapi/v1.yaml" >&2
  exit 2
fi
VERSION="$1"
CANARY_DIR="$2"
SPEC="${3:-api/openapi/v1.yaml}"

ROOT="$(git rev-parse --show-toplevel)" || {
  echo "check-api-v1-compat.sh: not inside a git repository (the base spec is read from git)" >&2
  exit 2
}
cd "$ROOT"

WRAPPER="${OASDIFF_WRAPPER:-scripts/oasdiff.sh}"
BASE_REF="refs/remotes/origin/main"

for f in "$CANARY_DIR/base.yaml" "$CANARY_DIR/revision.yaml" "$SPEC"; do
  [ -f "$f" ] || { echo "check-api-v1-compat.sh: $f is missing" >&2; exit 2; }
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# run_oasdiff <base> <revision>: prints oasdiff's output, returns its exit status.
run_oasdiff() {
  "$WRAPPER" "$VERSION" breaking "$1" "$2" --fail-on ERR
}

# --- 1. Liveness canary ---------------------------------------------------------
rc=0
run_oasdiff "$CANARY_DIR/base.yaml" "$CANARY_DIR/revision.yaml" > "$TMP/canary.out" 2>&1 || rc=$?
if [ "$rc" -ne 1 ]; then
  echo "check-api-v1-compat.sh: INSTRUMENT BROKEN: the canary pair (a removed response field) exited $rc, want 1" >&2
  cat "$TMP/canary.out" >&2
  exit 2
fi
rc=0
run_oasdiff "$CANARY_DIR/base.yaml" "$CANARY_DIR/base.yaml" > "$TMP/self.out" 2>&1 || rc=$?
if [ "$rc" -ne 0 ]; then
  echo "check-api-v1-compat.sh: INSTRUMENT BROKEN: the canary base against itself exited $rc, want 0" >&2
  cat "$TMP/self.out" >&2
  exit 2
fi
echo "check-api-v1-compat.sh: canary fired (a removed response field is reported as breaking; an unchanged spec is clean)"

# --- 2. Resolve the base --------------------------------------------------------
if ! git rev-parse --verify -q "$BASE_REF^{commit}" >/dev/null; then
  echo "check-api-v1-compat.sh: origin/main is not present locally; fetching it"
  if ! git fetch --no-tags --depth=1 origin "+refs/heads/main:$BASE_REF"; then
    echo "check-api-v1-compat.sh: could not fetch origin/main; failing closed (the base spec is unknown)" >&2
    exit 2
  fi
  git rev-parse --verify -q "$BASE_REF^{commit}" >/dev/null || {
    echo "check-api-v1-compat.sh: origin/main still does not resolve after the fetch; failing closed" >&2
    exit 2
  }
fi
git show "$BASE_REF:$SPEC" > "$TMP/base.yaml" 2>"$TMP/show.err" || {
  echo "check-api-v1-compat.sh: origin/main has no $SPEC ($(cat "$TMP/show.err")); failing closed" >&2
  exit 2
}

# --- 3. The real comparison -----------------------------------------------------
rc=0
run_oasdiff "$TMP/base.yaml" "$SPEC" > "$TMP/real.out" 2>&1 || rc=$?
case "$rc" in
  0)
    echo "check-api-v1-compat.sh: no breaking change in $SPEC against origin/main"
    ;;
  1)
    cat "$TMP/real.out" >&2
    echo "check-api-v1-compat.sh: $SPEC breaks compatibility with origin/main (see above)." >&2
    echo "  /api/v1 changes are additive only: add a new path, an optional request field or a new" >&2
    echo "  response field instead, or introduce /api/v2 (see the header of $SPEC)." >&2
    exit 1
    ;;
  *)
    cat "$TMP/real.out" >&2
    echo "check-api-v1-compat.sh: oasdiff could not compare the specs (exit $rc); failing closed" >&2
    exit 2
    ;;
esac
