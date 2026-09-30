#!/bin/sh
# Hermetic exit-code contract for scripts/check-api-v1-compat.sh (PRD #1908 M5): throwaway git
# repos with a local bare origin and a FAKE oasdiff wrapper (OASDIFF_WRAPPER), so no network and
# no real release binary. It proves the gate's three statuses and, above all, that it FAILS
# CLOSED: a dead diff tool, an unreachable origin/main and a base without the spec all exit 2
# rather than reading as "no breaking change".
#
# The fake treats a spec as breaking when the revision LACKS a line the base has (the shape of
# a removed field), which is enough to drive every branch; the real oasdiff semantics are
# exercised by `task check:api-v1-compat` itself.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
CHECK="$ROOT/scripts/check-api-v1-compat.sh"
TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  echo "check-api-v1-compat.test.sh: FAIL: $*" >&2
  exit 1
}
assert_eq() { [ "$2" = "$1" ] || fail "$3: expected '$1', got '$2'"; }
assert_contains() { grep -Fq -- "$2" "$1" || fail "$1 does not contain: $2"; }
assert_not_contains() { ! grep -Fq -- "$2" "$1" || fail "$1 unexpectedly contains: $2"; }

# --- the fake wrapper -------------------------------------------------------------------
FAKE="$TMP/fake-oasdiff.sh"
cat > "$FAKE" <<'FAKE'
#!/bin/sh
# usage: fake <version> breaking <base> <revision> --fail-on ERR
printf '%s\n' "$*" >> "$FAKE_LOG"
[ "$2" = "breaking" ] || exit 2
base="$3"
rev="$4"
[ -r "$base" ] && [ -r "$rev" ] || exit 102
case "$base" in
  *canary*)
    # The liveness canary pair. A dead tool answers 0 to everything.
    [ "${FAKE_DEAD:-0}" != "1" ] || exit 0
    ;;
  *)
    [ -z "${FAKE_REAL_RC:-}" ] || exit "$FAKE_REAL_RC"
    ;;
esac
# Breaking iff the revision lacks a line of the base.
if [ -n "$(grep -vxFf "$rev" "$base" || true)" ]; then
  echo "error [response-required-property-removed]"
  exit 1
fi
exit 0
FAKE
chmod +x "$FAKE"

# --- fixture repos ----------------------------------------------------------------------
git_q() { git -c user.email=t@example.com -c user.name=t -c init.defaultBranch=main -c commit.gpgsign=false "$@"; }

make_repos() {  # $1 = case dir; leaves $1/origin.git and $1/work (origin/main fetched)
  d="$1"
  mkdir -p "$d"
  git_q init -q --bare "$d/origin.git"
  git_q init -q "$d/work"
  (
    cd "$d/work"
    mkdir -p api/openapi scripts/api-v1-compat-canary
    printf 'paths: a\nfields: one\nfields: two\n' > api/openapi/v1.yaml
    printf 'canary: base\nfield: color\n' > scripts/api-v1-compat-canary/base.yaml
    printf 'canary: base\n' > scripts/api-v1-compat-canary/revision.yaml
    git_q add .
    git_q commit -q -m base
    git_q remote add origin "$d/origin.git"
    git_q push -q origin main
    git_q fetch -q origin main
  )
}

CASE=0
FAKE_DEAD=0
FAKE_REAL_RC=""
# run_check <dir> <args...>: runs the gate inside <dir>/work; sets OUT and RC.
run_check() {
  d="$1"; shift
  CASE=$((CASE + 1))
  OUT="$TMP/out$CASE"
  FAKE_LOG="$TMP/log$CASE"
  : > "$FAKE_LOG"
  RC=0
  (cd "$d/work" && env OASDIFF_WRAPPER="$FAKE" FAKE_LOG="$FAKE_LOG" FAKE_DEAD="$FAKE_DEAD" FAKE_REAL_RC="$FAKE_REAL_RC" \
    /bin/sh "$CHECK" "$@") > "$OUT" 2>&1 || RC=$?
}
ARGS="v9.9.9 scripts/api-v1-compat-canary api/openapi/v1.yaml"

# 1. unchanged spec: clean, canary reported, the version is passed through -------------------
D="$TMP/c1"; make_repos "$D"
# shellcheck disable=SC2086
run_check "$D" $ARGS
assert_eq 0 "$RC" "unchanged spec"
assert_contains "$OUT" "canary fired"
assert_contains "$OUT" "no breaking change"
assert_contains "$TMP/log$CASE" "v9.9.9 breaking"
assert_contains "$TMP/log$CASE" "--fail-on ERR"

# 2. an additive change is clean; a removed line is breaking (exit 1, with the guidance) ------
printf 'paths: a\nfields: one\nfields: two\nfields: three\n' > "$D/work/api/openapi/v1.yaml"
# shellcheck disable=SC2086
run_check "$D" $ARGS
assert_eq 0 "$RC" "additive change"
printf 'paths: a\nfields: one\n' > "$D/work/api/openapi/v1.yaml"
# shellcheck disable=SC2086
run_check "$D" $ARGS
assert_eq 1 "$RC" "breaking change"
assert_contains "$OUT" "response-required-property-removed"
assert_contains "$OUT" "additive only"

# 3. a dead instrument (the tool never reports the canary's removed field) is exit 2, and the
#    real comparison never ran ---------------------------------------------------------------
D="$TMP/c3"; make_repos "$D"
FAKE_DEAD=1
# shellcheck disable=SC2086
run_check "$D" $ARGS
assert_eq 2 "$RC" "dead instrument"
assert_contains "$OUT" "INSTRUMENT BROKEN"
assert_not_contains "$TMP/log$CASE" "base.yaml api/openapi"
FAKE_DEAD=0

# 4. a tool error on the real comparison (oasdiff could not load a spec) fails closed ----------
D="$TMP/c4"; make_repos "$D"
FAKE_REAL_RC=102
# shellcheck disable=SC2086
run_check "$D" $ARGS
assert_eq 2 "$RC" "tool error"
assert_contains "$OUT" "failing closed"
FAKE_REAL_RC=""

# 5. origin/main absent (a shallow CI checkout): the gate fetches exactly that ref ----------------
D="$TMP/c5"; make_repos "$D"
git -C "$D/work" update-ref -d refs/remotes/origin/main
# shellcheck disable=SC2086
run_check "$D" $ARGS
assert_eq 0 "$RC" "fetch when absent"
assert_contains "$OUT" "fetching it"
git -C "$D/work" rev-parse --verify -q refs/remotes/origin/main >/dev/null || fail "origin/main was not fetched"

# 6. origin/main absent and unfetchable: FAIL CLOSED ------------------------------------------------
D="$TMP/c6"; make_repos "$D"
git -C "$D/work" update-ref -d refs/remotes/origin/main
git -C "$D/work" remote set-url origin "$TMP/does-not-exist.git"
# shellcheck disable=SC2086
run_check "$D" $ARGS
assert_eq 2 "$RC" "unfetchable origin"
assert_contains "$OUT" "failing closed"

# 7. a resolvable origin/main is used as is, with no network (the origin can be gone) ----------------
D="$TMP/c7"; make_repos "$D"
git -C "$D/work" remote set-url origin "$TMP/does-not-exist.git"
# shellcheck disable=SC2086
run_check "$D" $ARGS
assert_eq 0 "$RC" "resolvable origin/main needs no fetch"
assert_not_contains "$OUT" "fetching it"

# 8. origin/main without the spec: fail closed ---------------------------------------------------------
D="$TMP/c8"; make_repos "$D"
(
  cd "$D/work"
  git_q rm -q api/openapi/v1.yaml
  git_q commit -q -m "drop the spec"
  git_q push -q origin main
  git_q fetch -q origin main
  mkdir -p api/openapi
  printf 'paths: a\n' > api/openapi/v1.yaml
)
# shellcheck disable=SC2086
run_check "$D" $ARGS
assert_eq 2 "$RC" "base without the spec"
assert_contains "$OUT" "has no api/openapi/v1.yaml"

# 9. missing inputs and usage --------------------------------------------------------------------------
D="$TMP/c9"; make_repos "$D"
rm "$D/work/api/openapi/v1.yaml"
# shellcheck disable=SC2086
run_check "$D" $ARGS
assert_eq 2 "$RC" "missing spec in the tree"
run_check "$D" v9.9.9
assert_eq 2 "$RC" "usage"
assert_contains "$OUT" "usage:"
run_check "$D" v9.9.9 "$TMP/nowhere" api/openapi/v1.yaml
assert_eq 2 "$RC" "missing canary dir"

echo "check-api-v1-compat.test.sh: all cases passed"
