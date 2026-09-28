#!/bin/sh
# Hermetic contract tests for scripts/scan-secrets.sh's fetch/scan split.
#
# No real gitleaks, no network, no touching this repo: each case builds a throwaway
# git repo holding two canary files and the report template, and puts a FAKE `go`
# on PATH. The fake `go install` fails FAKE_GO_FAILS times before "installing" a
# fake gitleaks that writes both canaries into the report (or, with
# FAKE_GITLEAKS_BROKEN=1, exits 1 as a scanner that could not run). That proves a
# transient module-proxy failure is retried to a clean verdict, a persistent one is
# still an instrument failure (exit 2), and a broken SCAN is never retried.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
GATE="$ROOT/scripts/scan-secrets.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  echo "scan-secrets.test.sh: FAIL: $*" >&2
  exit 1
}

assert_eq() {
  [ "$2" = "$1" ] || fail "$3: expected '$1', got '$2'"
}

assert_contains() {
  grep -Fq -- "$2" "$1" || fail "$1 does not contain: $2"
}

count_lines() {
  if [ -f "$1" ]; then wc -l < "$1" | tr -d ' '; else echo 0; fi
}

FAKE_BIN="$TMP/bin"
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/go" <<'EOF'
#!/bin/sh
# Expect: install github.com/zricethezav/gitleaks/v8@vX.Y.Z, with GOBIN set.
[ "${1:-}" = "install" ] || { echo "fake go: unexpected subcommand '${1:-}'" >&2; exit 3; }
case "${2:-}" in
  github.com/zricethezav/gitleaks/v8@v*) : ;;
  *) echo "fake go: unexpected package '${2:-}'" >&2; exit 3 ;;
esac
[ -n "${GOBIN:-}" ] || { echo "fake go: GOBIN unset" >&2; exit 3; }
echo x >> "$FAKE_STATE/installs"
if [ "$(wc -l < "$FAKE_STATE/installs" | tr -d ' ')" -le "${FAKE_GO_FAILS:-0}" ]; then
  echo "go: github.com/zricethezav/gitleaks/v8@v8.30.1: Get \"https://proxy.golang.org/...\": dial tcp: i/o timeout" >&2
  exit 1
fi
cat > "$GOBIN/gitleaks" <<'INNER'
#!/bin/sh
echo x >> "$FAKE_STATE/scans"
[ "${FAKE_GITLEAKS_BROKEN:-0}" = "1" ] && { echo "fake gitleaks: cannot parse config" >&2; exit 1; }
report=""
while [ "$#" -gt 0 ]; do
  [ "$1" = "--report-path" ] && { report="$2"; shift; }
  shift
done
[ -n "$report" ] || { echo "fake gitleaks: no --report-path" >&2; exit 3; }
printf 'canary-a.txt\t1\tgitlab-pat\ncanary-b.txt\t1\tgitlab-pat\n' > "$report"
INNER
chmod +x "$GOBIN/gitleaks"
EOF
chmod +x "$FAKE_BIN/go"

make_repo() {
  repo="$(mktemp -d "$TMP/repo.XXXXXX")"
  mkdir -p "$repo/scripts"
  cp "$ROOT/scripts/gitleaks-report.tmpl" "$repo/scripts/"
  printf 'fake canary a\n' > "$repo/canary-a.txt"
  printf 'fake canary b\n' > "$repo/canary-b.txt"
  git -C "$repo" -c init.defaultBranch=main init -q
  git -C "$repo" -c user.email=t@example.com -c user.name=test add -A
  git -C "$repo" -c user.email=t@example.com -c user.name=test commit -qm init
  printf '%s\n' "$repo"
}

GATE_RC=0
run_gate() {
  repo="$(make_repo)"
  FAKE_STATE="$(mktemp -d "$TMP/state.XXXXXX")"
  OUT="$FAKE_STATE/out"
  GATE_RC=0
  (
    cd "$repo"
    env -u GITLEAKS_CONFIG -u GITLEAKS_CONFIG_TOML \
      PATH="$FAKE_BIN:$PATH" FAKE_STATE="$FAKE_STATE" \
      SCAN_SECRETS_FETCH_ATTEMPTS=3 SCAN_SECRETS_FETCH_DELAY=0 "$@" \
      "$GATE" v8.30.1 canary-a.txt canary-b.txt
  ) > "$OUT" 2>&1 || GATE_RC=$?
}

# 1. Transient proxy failure: two failed fetches, the third succeeds -> clean (0).
run_gate FAKE_GO_FAILS=2
assert_eq 0 "$GATE_RC" "transient fetch failure: rc"
assert_eq 3 "$(count_lines "$FAKE_STATE/installs")" "transient fetch failure: install attempts"
assert_eq 1 "$(count_lines "$FAKE_STATE/scans")" "transient fetch failure: scans"
assert_contains "$OUT" "retrying"
assert_contains "$OUT" "scan-secrets: clean"

# 2. Persistent proxy failure: every attempt fails -> instrument failure (2), no scan.
run_gate FAKE_GO_FAILS=99
assert_eq 2 "$GATE_RC" "persistent fetch failure: rc"
assert_eq 3 "$(count_lines "$FAKE_STATE/installs")" "persistent fetch failure: install attempts"
assert_eq 0 "$(count_lines "$FAKE_STATE/scans")" "persistent fetch failure: scans"
assert_contains "$OUT" "fetching gitleaks v8.30.1 failed 3 time(s)"

# 3. Scanner fails after a good fetch -> instrument failure (2), and NOT retried.
run_gate FAKE_GO_FAILS=0 FAKE_GITLEAKS_BROKEN=1
assert_eq 2 "$GATE_RC" "broken scanner: rc"
assert_eq 1 "$(count_lines "$FAKE_STATE/installs")" "broken scanner: install attempts"
assert_eq 1 "$(count_lines "$FAKE_STATE/scans")" "broken scanner: scans"
assert_contains "$OUT" "INSTRUMENT failure, not a scan result."

echo "scan-secrets.test.sh: PASS (3 cases)"
