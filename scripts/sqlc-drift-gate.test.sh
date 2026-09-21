#!/bin/sh
# Hermetic contract tests for scripts/sqlc-drift-gate.sh.
#
# No real sqlc, no network, no touching this repo: each case builds a throwaway git
# repo (its own `.github/workflows/ci.yml` pin + `api/internal/store` corpus) and puts
# a FAKE `go` on PATH that simulates `go run ...sqlc@<ver> generate`. That proves the
# gate's exit-code discipline (2 = instrument broken, 1 = drift, 0 = clean) fires as
# designed, so a future edit that guts the check reddens here instead of shipping a
# gate that reports green on drift.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
GATE="$ROOT/scripts/sqlc-drift-gate.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
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

# A fake `go` shared by every case. On `go run <pkg>@<ver> generate` (invoked by the
# gate from inside api/) it acts on FAKE_SQLC_MODE; any other invocation is a contract
# breach (the gate changed how it calls go) and exits 3 so the test notices.
FAKE_BIN="$TMP/bin"
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/go" <<'EOF'
#!/bin/sh
# Expect: run github.com/sqlc-dev/sqlc/cmd/sqlc@vX.Y.Z generate
[ "${1:-}" = "run" ] || { echo "fake go: unexpected subcommand '${1:-}'" >&2; exit 3; }
case "${2:-}" in
  github.com/sqlc-dev/sqlc/cmd/sqlc@v*) : ;;
  *) echo "fake go: unexpected package '${2:-}'" >&2; exit 3 ;;
esac
[ "${3:-}" = "generate" ] || { echo "fake go: expected 'generate', got '${3:-}'" >&2; exit 3; }
case "${FAKE_SQLC_MODE:-clean}" in
  clean)  exit 0 ;;                                            # regenerate == committed
  drift)  printf 'regenerated\n' > internal/store/models.go   # cwd is api/
          exit 0 ;;
  broken) echo "fake sqlc: config error" >&2; exit 1 ;;       # tool failed to run
  *) echo "fake go: unknown FAKE_SQLC_MODE" >&2; exit 3 ;;
esac
EOF
chmod +x "$FAKE_BIN/go"

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
  git -C "$repo" -c user.email=t@example.com -c user.name=test add -A
  git -C "$repo" -c user.email=t@example.com -c user.name=test commit -qm init
  printf '%s\n' "$repo"
}

# Run the real gate inside a repo with the fake go on PATH; capture rc and output.
GATE_RC=0
run_gate() {
  GATE_RC=0
  ( cd "$1" && env PATH="$FAKE_BIN:$PATH" FAKE_SQLC_MODE="$2" "$GATE" ) \
    > "$3" 2>&1 || GATE_RC=$?
}

# 1. Clean: regenerate reproduces the committed tree -> exit 0.
repo="$(make_repo with-version)"
run_gate "$repo" clean "$TMP/clean.out"
assert_eq 0 "$GATE_RC" "clean run exit code"
assert_contains "$TMP/clean.out" "clean"

# 2. Drift: regenerate changes a committed file -> exit 1, names the regenerate command.
repo="$(make_repo with-version)"
run_gate "$repo" drift "$TMP/drift.out"
assert_eq 1 "$GATE_RC" "drift run exit code"
assert_contains "$TMP/drift.out" "DRIFT"
assert_contains "$TMP/drift.out" "go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate"

# 3. Instrument broken: sqlc itself fails -> exit 2, never a silent green.
repo="$(make_repo with-version)"
run_gate "$repo" broken "$TMP/broken.out"
assert_eq 2 "$GATE_RC" "broken tool exit code"
assert_contains "$TMP/broken.out" "INSTRUMENT failure"

# 4. Version unreadable from ci.yml -> exit 2 (fail closed, never a wrong sqlc).
repo="$(make_repo no-version)"
run_gate "$repo" clean "$TMP/noversion.out"
assert_eq 2 "$GATE_RC" "missing version exit code"
assert_contains "$TMP/noversion.out" "could not read SQLC_VERSION"

printf 'sqlc-drift-gate tests: PASS\n'
