#!/bin/sh
# Hermetic gate contracts; --case NAME selects an independent scenario.
# Set UZI_SQLC_KEEP_TEST_ARTIFACTS=1 to preserve this run's artifacts for debugging.
set -eu
# Fixture repos must not read host git config: a global core.fsmonitor=true
# leaves a detached fsmonitor daemon per repo on macOS.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
SOURCE_GATE="${UZI_SQLC_DRIFT_GATE:-$ROOT/scripts/sqlc-drift-gate.sh}"
CASE="${2:-all}"
[ "${1:---case}" = --case ] || exit 2
TMPDIR="$ROOT/.uzi/scratch"
mkdir -p "$TMPDIR"
export TMPDIR
TMP="$(mktemp -d "$TMPDIR/sqlc-drift-tests.XXXXXX")"
trap 'if [ "${UZI_SQLC_KEEP_TEST_ARTIFACTS:-0}" != 1 ]; then rm -rf "$TMP"; fi' 0
printf 'ARTIFACTS=%s\n' "$TMP"
mkdir -p "$TMP/scripts"
GATE="$TMP/scripts/sqlc-drift-gate.sh"
cp "$SOURCE_GATE" "$GATE"
chmod +x "$GATE"

trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  echo "sqlc-drift-gate.test.sh: FAIL: $*" >&2
  exit 1
}

assert_eq() {
  [ "$2" = "$1" ] || fail "$3: expected '$1', got '$2'"
}

assert_contains() {
  grep -Fq -- "$2" "$1" || fail "$1 does not contain: $2"
}

# A Go sentinel fails and logs every invocation, including fallback attempts.
FAKE_BIN="$TMP/bin"
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/go" <<'EOF'
#!/bin/sh
printf 'forbidden\n' >> "$GO_LOG"
exit 97
EOF
cat > "$TMP/scripts/sqlc.sh" <<'EOF'
#!/bin/sh
printf 'called\n' >> "$WRAPPER_LOG"
[ "$#" -eq 2 ] && [ "$1" = v1.31.1 ] && [ "$2" = generate ] && [ "$PWD" = "$EXPECTED_API" ] || exit 3
case "$FAKE_SQLC_MODE" in
  clean) exit 0 ;;
  drift) printf 'regenerated\n' > internal/store/models.go ;;
  newfile) printf 'package store\n' > internal/store/new_query.sql.go ;;
  broken) echo 'fake sqlc: config error' >&2; exit 1 ;;
  acquisition) echo 'sqlc.sh: bounded download failed' >&2; exit 2 ;;
  *) exit 3 ;;
esac
EOF
chmod +x "$FAKE_BIN/go" "$TMP/scripts/sqlc.sh"

# Build a throwaway repo with a committed api/internal/store corpus and a ci.yml pin.
# $1 = whether ci.yml carries the SQLC_VERSION line ("with-version" | "no-version").
make_repo() {
  repo="$(mktemp -d "$TMP/repo.XXXXXX")"
  mkdir -p "$repo/api/internal/store" "$repo/.github/workflows"
  printf 'package store\n\n// committed codegen\n' > "$repo/api/internal/store/models.go"
  printf 'module example.com/api\n\ngo 1.27\n' > "$repo/api/go.mod"
  if [ "$1" = "with-version" ]; then
    printf '          SQLC_VERSION="v1.31.1"        # v-prefixed git tag\n' \
      > "$repo/.github/workflows/ci.yml"
  else
    printf 'jobs:\n  validate-api:\n    steps: []\n' > "$repo/.github/workflows/ci.yml"
  fi
  git -C "$repo" -c init.defaultBranch=main init -q
  git -C "$repo" add -- api/internal/store/models.go api/go.mod .github/workflows/ci.yml
  git -C "$repo" -c user.email=t@example.com -c user.name=test commit -qm init
  printf '%s\n' "$repo"
}

# Copy the gate beside the wrapper, never use the source repo as the fixture.
GATE_RC=0
NO_PIN_REPO=""
run_gate() {
  GATE_RC=0
  rm -f "$TMP/wrapper.log"
  ( cd "$1" && env PATH="$FAKE_BIN:$PATH" FAKE_SQLC_MODE="$2" EXPECTED_API="$1/api" WRAPPER_LOG="$TMP/wrapper.log" GO_LOG="$TMP/go.log" "$GATE" ) \
    > "$3" 2>&1 || GATE_RC=$?
  [ ! -s "$TMP/go.log" ] || fail 'Go was invoked'
  if [ "$1" != "$NO_PIN_REPO" ] && [ ! -f "$FAKE_BIN/curl" ]; then
    [ -s "$TMP/wrapper.log" ] || fail 'wrapper was not called'
  fi
}

if [ "$CASE" = all ] || [ "$CASE" = clean ]; then
# 1. Clean: regenerate reproduces the committed tree -> exit 0.
repo="$(make_repo with-version)"
run_gate "$repo" clean "$TMP/clean.out"
assert_eq 0 "$GATE_RC" "clean run exit code"
assert_contains "$TMP/clean.out" "clean"

fi

if [ "$CASE" = all ] || [ "$CASE" = drift ]; then
# 2. Drift: regenerate changes a committed file -> exit 1, names the regenerate command.
repo="$(make_repo with-version)"
run_gate "$repo" drift "$TMP/drift.out"
assert_eq 1 "$GATE_RC" "drift run exit code"
assert_contains "$TMP/drift.out" "DRIFT"
assert_contains "$TMP/drift.out" "../scripts/sqlc.sh v1.31.1 generate"

fi

if [ "$CASE" = all ] || [ "$CASE" = broken ]; then
# 3. Instrument broken: sqlc itself fails -> exit 2, never a silent green.
repo="$(make_repo with-version)"
run_gate "$repo" broken "$TMP/broken.out"
assert_eq 2 "$GATE_RC" "broken tool exit code"
assert_contains "$TMP/broken.out" "INSTRUMENT failure"

fi

if [ "$CASE" = all ] || [ "$CASE" = missing-pin ]; then
# 4. Version unreadable from ci.yml -> exit 2 (fail closed, never a wrong sqlc).
repo="$(make_repo no-version)"
NO_PIN_REPO="$repo"
run_gate "$repo" clean "$TMP/noversion.out"
assert_eq 2 "$GATE_RC" "missing version exit code"
assert_contains "$TMP/noversion.out" "could not read SQLC_VERSION"

fi

if [ "$CASE" = all ] || [ "$CASE" = newfile ]; then
# 5. New untracked file: regenerate creates an uncommitted *.sql.go under the codegen
#    dir -> exit 1. `git diff` alone reads clean (tracked-only); the untracked-file check
#    is what catches a generated file missing from the commit.
repo="$(make_repo with-version)"
run_gate "$repo" newfile "$TMP/newfile.out"
assert_eq 1 "$GATE_RC" "new untracked file exit code"
assert_contains "$TMP/newfile.out" "untracked"

fi

if [ "$CASE" = all ] || [ "$CASE" = orphan ]; then
# 6. Generated file already on disk but never `git add`ed (produced in an earlier run,
#    forgotten at commit) while regeneration is a no-op -> exit 1. `git diff` reads clean
#    and a before/after snapshot would cancel it; scoping to the *.sql.go suffix catches it.
repo="$(make_repo with-version)"
printf 'package store\n' > "$repo/api/internal/store/orphan.sql.go"
run_gate "$repo" clean "$TMP/orphan.out"
assert_eq 1 "$GATE_RC" "pre-existing untracked generated file exit code"
assert_contains "$TMP/orphan.out" "untracked"

fi

if [ "$CASE" = all ] || [ "$CASE" = unrelated ]; then
# 7. Untracked NON-generated files under the codegen dir (a WIP test, a new query source)
#    are NOT drift -- only the generated *.sql.go suffix is scoped, since that dir also
#    holds hand-written *_test.go and the queries/ + migrations/ sources. Clean -> exit 0.
repo="$(make_repo with-version)"
printf 'package store\n' > "$repo/api/internal/store/wip_test.go"
mkdir -p "$repo/api/internal/store/queries"
printf '%s\n' '-- name: Wip :one' 'SELECT 1;' > "$repo/api/internal/store/queries/wip.sql"
run_gate "$repo" clean "$TMP/wip.out"
assert_eq 0 "$GATE_RC" "untracked non-generated files are not drift"
assert_contains "$TMP/wip.out" "clean"

fi

if [ "$CASE" = all ] || [ "$CASE" = acquisition ]; then
  repo="$(make_repo with-version)"
  run_gate "$repo" acquisition "$TMP/acquisition.out"
  assert_eq 2 "$GATE_RC" 'acquisition failure'
  assert_contains "$TMP/acquisition.out" 'sqlc.sh: bounded download failed'
  assert_contains "$TMP/acquisition.out" 'INSTRUMENT failure'
fi

if [ "$CASE" = all ] || [ "$CASE" = real-transport ]; then
  repo="$(make_repo with-version)"
  cp "$ROOT/scripts/sqlc.sh" "$TMP/scripts/sqlc.sh"
  cat > "$FAKE_BIN/curl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$TRANSPORT_LOG"
exit 7
EOF
  cat > "$FAKE_BIN/uname" <<'EOF'
#!/bin/sh
case "$1" in -s) echo Linux;; -m) echo x86_64;; esac
EOF
  chmod +x "$FAKE_BIN/curl" "$FAKE_BIN/uname"
  UZI_SQLC_DIR="$TMP/cache"
  TRANSPORT_LOG="$TMP/transport.log"
  export UZI_SQLC_DIR TRANSPORT_LOG
  run_gate "$repo" clean "$TMP/real-transport.out"
  assert_eq 2 "$GATE_RC" 'real wrapper acquisition failure'
  assert_contains "$TMP/real-transport.out" 'sqlc.sh: bounded download failed'
  assert_contains "$TMP/real-transport.out" 'INSTRUMENT failure'
  assert_contains "$TMP/transport.log" 'sqlc_1.31.1_linux_amd64.tar.gz'
fi

if [ "$CASE" = all ] || [ "$CASE" = fixture-cleanup ]; then
  # Exercise the real harnesses, not the fixture wrapper. Each child is bounded
  # to 30 seconds; the first failure stops the matrix after its finally cleanup.
  python3 - "$ROOT" "$TMP" <<'PY'
import os
from pathlib import Path
import shutil
import subprocess
import sys

root, diagnostics = map(Path, sys.argv[1:])
scratch = root / ".uzi/scratch"
for script, prefix, success_case in (
    ("sqlc.test.sh", "sqlc-tests.", "missing-version"),
    ("sqlc-drift-gate.test.sh", "sqlc-drift-tests.", "clean"),
):
    for keep in (False, True):
        for success in (True, False):
            mode = "opt-in" if keep else "default"
            outcome = "success" if success else "failure"
            name = f"fixture-cleanup/{script}/{mode}/{outcome}"
            env = dict(os.environ)
            env.pop("UZI_SQLC_KEEP_TEST_ARTIFACTS", None)
            env.pop("UZI_SQLC_DRIFT_GATE", None)
            env["UZI_SQLC_KEEP_TEST_ARTIFACTS"] = "1" if keep else "0"
            selected = success_case
            if not success:
                if script == "sqlc.test.sh":
                    selected = "__fixture_cleanup_unknown_case__"
                else:
                    # The parent's fresh TMP owns this guaranteed absent path.
                    absent = diagnostics / "absent-cleanup-gate.sh"
                    assert not absent.exists(), name + ": failure fixture must be absent"
                    env["UZI_SQLC_DRIFT_GATE"] = str(absent)
            result = subprocess.run(
                ["sh", str(root / "scripts" / script), "--case", selected],
                cwd=root / "scripts", env=env, capture_output=True, text=True,
                timeout=30,
            )
            (diagnostics / f"{script}.{mode}.{outcome}.log").write_text(
                result.stdout + result.stderr + f"\nEXIT={result.returncode}\n"
            )
            paths = [line.removeprefix("ARTIFACTS=")
                     for line in result.stdout.splitlines() if line.startswith("ARTIFACTS=")]
            assert len(paths) == 1, name + ": expected one child ARTIFACTS path"
            owned = Path(paths[0])
            assert owned.parent == scratch and owned.name.startswith(prefix), (
                name + ": unexpected child ARTIFACTS path: " + str(owned)
            )
            try:
                assert (result.returncode == 0) == success, name + ": unexpected child status"
                if keep:
                    assert owned.is_dir(), name + ": opt-in artifacts missing"
                else:
                    assert not owned.exists(), name + ": default artifacts retained"
                print("PASS " + name, flush=True)
            finally:
                # The child is reaped; remove only its allocated tree, including
                # a default-mode leak detected by the assertion above.
                if owned.exists():
                    shutil.rmtree(owned)
PY
fi

printf 'sqlc-drift-gate tests: PASS\n'
