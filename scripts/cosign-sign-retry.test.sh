#!/usr/bin/env bash
# Hermetic signer + sleep fixtures; never contact a registry or mint a signature.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SIGNER="${COSIGN_SIGN_RETRY_SCRIPT:-$ROOT/scripts/cosign-sign-retry.sh}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir "$WORK/bin"
cat > "$WORK/bin/cosign" <<'STUB'
#!/usr/bin/env bash
set -eu
count=$(cat "$CASE/count")
count=$((count + 1))
echo "$count" > "$CASE/count"
printf '%s\0' "$@" > "$CASE/argv"
if [ "$MODE" = clean ] || { [ "$MODE" != exhausted ] && [ "$count" -gt 1 ]; }; then
  echo signed; echo signer-diagnostic >&2; exit 0
fi
case "$MODE" in
  secondary) echo 'DENIED: permission_denied: HTTP status code 403 "Forbidden"' >&2
    echo '{"message":"You have exceeded a secondary rate limit. Please wait."}' >&2 ;;
  rate) echo 'unexpected status code 429 Too Many Requests' >&2 ;;
  server|exhausted) echo 'HTTP/1.1 503 Service Unavailable' >&2 ;;
  forbidden) echo 'DENIED: HTTP status code 403 Forbidden' >&2 ;;
  unauthorized) echo 'HTTP 401 Unauthorized' >&2 ;;
  unknown) echo 'certificate validation failed for image500' >&2 ;;
esac
exit 17
STUB
cat > "$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
echo "$1" >> "$CASE/sleeps"
STUB
chmod +x "$WORK/bin/cosign" "$WORK/bin/sleep"
fail() { echo "FAIL: $*" >&2; exit 1; }
ref='registry.example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
for mode in clean secondary rate server forbidden unauthorized unknown exhausted; do
  CASE="$WORK/$mode"; mkdir "$CASE"; echo 0 > "$CASE/count"; : > "$CASE/sleeps"
  rc=0
  CASE="$CASE" MODE="$mode" PATH="$WORK/bin:$PATH" bash "$SIGNER" "$ref" > "$CASE/out" 2> "$CASE/err" || rc=$?
  expected_rc=0; expected_count=2; expected_sleep=5
  case "$mode" in
    clean) expected_count=1; expected_sleep='' ;;
    forbidden|unauthorized|unknown) expected_rc=17; expected_count=1; expected_sleep='' ;;
    exhausted) expected_rc=17; expected_count=5; expected_sleep=$'5\n10\n20\n30' ;;
  esac
  [ "$rc" -eq "$expected_rc" ] || fail "$mode: rc=$rc expected $expected_rc"
  [ "$(cat "$CASE/count")" = "$expected_count" ] || fail "$mode: wrong attempt count"
  [ "$(cat "$CASE/sleeps")" = "$expected_sleep" ] || fail "$mode: wrong backoff"
  if [ "$rc" -eq 0 ]; then grep -Fxq signed "$CASE/out"; grep -Fxq signer-diagnostic "$CASE/err"; fi
  python3 - "$CASE/argv" "$ref" <<'PY'
import pathlib, sys
assert pathlib.Path(sys.argv[1]).read_bytes().split(b'\0')[:-1] == [b'sign', b'--yes', sys.argv[2].encode()]
PY
  echo "PASS: cosign signing $mode"
done
# Every sign site, including the runtime base and chart, uses this wrapper.
python3 - "$ROOT/.github/workflows/release.yml" <<'PY'
import pathlib, sys
text = pathlib.Path(sys.argv[1]).read_text()
lines = [line for line in text.splitlines() if not line.lstrip().startswith('#')]
assert sum(line.count('./scripts/cosign-sign-retry.sh ') for line in lines) == 6
assert not any('cosign sign --yes' in line for line in lines)
PY
echo 'PASS: all six release signing sites use the shared retry helper'
