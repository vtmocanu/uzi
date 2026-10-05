#!/bin/sh
# Hermetic fetch/reuse/scan contract: fake Go metadata and scanners, no network.
# Each case owns a throwaway git repo and excludes host-installed tools from PATH.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
GATE="$ROOT/scripts/scan-secrets.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() { echo "scan-secrets.test.sh: FAIL: $*" >&2; exit 1; }
assert_eq() { [ "$2" = "$1" ] || fail "$3: expected '$1', got '$2'"; }
assert_contains() { grep -Fq -- "$2" "$1" || fail "$1 does not contain: $2"; }
count_lines() { if [ -f "$1" ]; then wc -l < "$1" | tr -d ' '; else echo 0; fi; }

FAKE_BIN="$TMP/bin"
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/scanner" <<'EOF'
#!/bin/sh
echo x >> "$FAKE_STATE/scans"
echo "$0" >> "$FAKE_STATE/used"
[ "${FAKE_GITLEAKS_BROKEN:-0}" = "1" ] && { echo "fake gitleaks: cannot parse config" >&2; exit 1; }
report=""
while [ "$#" -gt 0 ]; do
  [ "$1" = "--report-path" ] && { report="$2"; shift; }
  shift
done
[ -n "$report" ] || { echo "fake gitleaks: no --report-path" >&2; exit 3; }
if [ "${FAKE_GITLEAKS_DISARMED:-0}" = "1" ]; then
  : > "$report"
else
  printf 'canary-a.txt\t1\tgitlab-pat\ncanary-b.txt\t1\tgitlab-pat\n' > "$report"
fi
EOF
chmod +x "$FAKE_BIN/scanner"
cat > "$FAKE_BIN/go" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "version" ] && [ "${2:-}" = "-m" ]; then
  [ "${3:-}" = "$FAKE_INSTALLED_PATH" ] || exit 1
  case "$FAKE_METADATA_MODE" in
    unknown) echo 'not a Go executable'; exit 0 ;;
    unreadable) printf '\tmod\tgithub.com/zricethezav/gitleaks/v8\tv8.30.1\n'; exit 1 ;;
    *) : ;;
  esac
  module=github.com/zricethezav/gitleaks/v8
  version=v8.30.1
  [ "$FAKE_METADATA_MODE" = mismatch ] && version=v8.30.0
  [ "$FAKE_METADATA_MODE" = wrong-module ] && module=example.com/another-scanner
  printf '%s: go1.27.1\n\tpath\t%s\n\tmod\t%s\t%s\th1:fixture\n' "$3" "$module" "$module" "$version"
  [ "$FAKE_METADATA_MODE" = replaced ] && printf '\t=>\texample.com/replacement\tv8.30.1\th1:fixture\n'
  [ "$FAKE_METADATA_MODE" = duplicate ] && printf '\tmod\t%s\t%s\th1:fixture\n' "$module" "$version"
  exit 0
fi
[ "${1:-}" = install ] || { echo "fake go: unexpected subcommand '${1:-}'" >&2; exit 3; }
case "${2:-}" in
  github.com/zricethezav/gitleaks/v8@v*) : ;;
  *) echo "fake go: unexpected package '${2:-}'" >&2; exit 3 ;;
esac
[ -n "${GOBIN:-}" ] || { echo 'fake go: GOBIN unset' >&2; exit 3; }
echo x >> "$FAKE_STATE/installs"
if [ "$(wc -l < "$FAKE_STATE/installs" | tr -d ' ')" -le "${FAKE_GO_FAILS:-0}" ]; then
  echo 'go: module proxy unavailable' >&2
  exit 1
fi
cp "$FAKE_SCANNER_SOURCE" "$GOBIN/gitleaks"
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

run_gate() {
  mode="$1"; shift
  repo="$(make_repo)"
  FAKE_STATE="$(mktemp -d "$TMP/state.XXXXXX")"
  OUT="$FAKE_STATE/out"
  case_bin="$FAKE_STATE/bin"
  mkdir -p "$case_bin"
  cp "$FAKE_BIN/go" "$case_bin/go"
  FAKE_INSTALLED_PATH="$case_bin/gitleaks"
  [ "$mode" = absent ] || cp "$FAKE_BIN/scanner" "$FAKE_INSTALLED_PATH"
  GATE_RC=0
  (
    cd "$repo"
    env -u GITLEAKS_CONFIG -u GITLEAKS_CONFIG_TOML \
      PATH="$case_bin:/usr/bin:/bin:/usr/sbin:/sbin" FAKE_STATE="$FAKE_STATE" \
      FAKE_METADATA_MODE="$mode" FAKE_INSTALLED_PATH="$FAKE_INSTALLED_PATH" \
      FAKE_SCANNER_SOURCE="$FAKE_BIN/scanner" \
      SCAN_SECRETS_FETCH_ATTEMPTS=3 SCAN_SECRETS_FETCH_DELAY=0 "$@" \
      "$GATE" v8.30.1 canary-a.txt canary-b.txt
  ) > "$OUT" 2>&1 || GATE_RC=$?
}

run_gate absent FAKE_GO_FAILS=2
assert_eq 0 "$GATE_RC" 'transient fetch failure: rc'
assert_eq 3 "$(count_lines "$FAKE_STATE/installs")" 'transient fetch failure: install attempts'
assert_eq 1 "$(count_lines "$FAKE_STATE/scans")" 'transient fetch failure: scans'
assert_contains "$OUT" retrying
assert_contains "$OUT" 'scan-secrets: clean'

run_gate absent FAKE_GO_FAILS=99
assert_eq 2 "$GATE_RC" 'persistent fetch failure: rc'
assert_eq 3 "$(count_lines "$FAKE_STATE/installs")" 'persistent fetch failure: install attempts'
assert_eq 0 "$(count_lines "$FAKE_STATE/scans")" 'persistent fetch failure: scans'
assert_contains "$OUT" 'fetching gitleaks v8.30.1 failed 3 time(s)'

run_gate absent FAKE_GO_FAILS=0 FAKE_GITLEAKS_BROKEN=1
assert_eq 2 "$GATE_RC" 'broken fetched scanner: rc'
assert_eq 1 "$(count_lines "$FAKE_STATE/installs")" 'broken fetched scanner: install attempts'
assert_eq 1 "$(count_lines "$FAKE_STATE/scans")" 'broken fetched scanner: scans'
assert_contains "$OUT" 'INSTRUMENT failure, not a scan result.'

run_gate match FAKE_GO_FAILS=99
assert_eq 0 "$GATE_RC" 'matching installed scanner works offline'
assert_eq 0 "$(count_lines "$FAKE_STATE/installs")" 'matching scanner never fetches'
assert_eq 1 "$(count_lines "$FAKE_STATE/scans")" 'matching scanner runs once'
assert_eq "$FAKE_INSTALLED_PATH" "$(cat "$FAKE_STATE/used")" 'matching scanner is the one executed'
assert_contains "$OUT" 'canaries DETECTED'

for mode in mismatch unknown unreadable replaced wrong-module duplicate; do
  run_gate "$mode"
  assert_eq 0 "$GATE_RC" "$mode metadata: fetched scanner verdict"
  assert_eq 1 "$(count_lines "$FAKE_STATE/installs")" "$mode metadata: fetches pinned version"
  assert_eq 1 "$(count_lines "$FAKE_STATE/scans")" "$mode metadata: scans once"
  [ "$(cat "$FAKE_STATE/used")" != "$FAKE_INSTALLED_PATH" ] || fail "$mode metadata: ran unverified installed scanner"
done

run_gate match FAKE_GITLEAKS_BROKEN=1
assert_eq 2 "$GATE_RC" 'broken installed scanner: instrument failure'
assert_eq 0 "$(count_lines "$FAKE_STATE/installs")" 'broken installed scanner: no fetch retries'
assert_eq 1 "$(count_lines "$FAKE_STATE/scans")" 'broken installed scanner: no scan retries'

run_gate match FAKE_GITLEAKS_DISARMED=1
assert_eq 2 "$GATE_RC" 'installed scanner still must detect canaries'
assert_eq 0 "$(count_lines "$FAKE_STATE/installs")" 'disarmed installed scanner: no fetch'
assert_eq 1 "$(count_lines "$FAKE_STATE/scans")" 'disarmed installed scanner: one scan'

echo 'scan-secrets.test.sh: PASS (12 cases)'
