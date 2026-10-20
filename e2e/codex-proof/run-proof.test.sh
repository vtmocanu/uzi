#!/usr/bin/env bash
# Behavioral test for run-proof.sh (#3148), driven by a FAKE `docker` and `timeout` on PATH and
# fake leg runners in a copied repo layout. No daemon, no image. Proves the strict verdict:
#   - all checks green -> exit 0, status pass, receipt.json and one CANDIDATE_RECEIPT line;
#   - Landlock unavailable / probe error, a failing leg, a profile mode with no clean SUMMARY
#     (missing, PASS=0, FAIL>0) or an image ID that changed mid-proof -> exit 1, status fail,
#     no receipt, and every leg still ran;
#   - a stale receipt from an earlier run never survives a failed run;
#   - bad arguments -> exit 2 before anything runs.
# Prints a mandatory `cases=N passed=N` tally and exits non-zero unless every case passes.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ROOT="$TMP/repo"
mkdir -p "$ROOT/e2e/codex-proof" "$ROOT/e2e/codex-m3a" "$ROOT/e2e/codex-m3b" "$TMP/bin"
cp "$HERE/run-proof.sh" "$ROOT/e2e/codex-proof/run-proof.sh"
STATE="$TMP/state"
mkdir -p "$STATE"

# Fake docker: `image inspect` prints the current image ID from $STATE/id; `run ... --probe`
# exits $FAKE_PROBE_RC; everything else succeeds silently.
cat > "$TMP/bin/docker" <<EOF
#!/usr/bin/env bash
case "\$1 \$2" in
  "image inspect") cat "$STATE/id" ;;
  run*)
    for a in "\$@"; do [ "\$a" = "--probe" ] && exit "\${FAKE_PROBE_RC:-0}"; done
    exit 0 ;;
  *) exit 0 ;;
esac
EOF
# Fake timeout: drop its options and duration, run the command.
cat > "$TMP/bin/timeout" <<'EOF'
#!/usr/bin/env bash
while [ "${1#--}" != "$1" ]; do shift; done
shift
exec "$@"
EOF
# Fake legs: record that they ran; behave per env.
cat > "$ROOT/e2e/codex-m3b/run-lifecycle.sh" <<EOF
#!/usr/bin/env bash
echo m3b >> "$STATE/ran"
[ "\${UZI_CODEX_PROOF_STRICT:-}" = 1 ] || { echo "m3b not strict"; exit 9; }
[ -n "\${UZI_M3B_SKIP_BUILD:-}" ] || exit 9
[ "\${FAKE_MUTATE_ID:-}" = 1 ] && echo sha256:2222 > "$STATE/id"
exit "\${FAKE_M3B_RC:-0}"
EOF
cat > "$ROOT/e2e/codex-m3a/run.sh" <<EOF
#!/usr/bin/env bash
echo profile >> "$STATE/ran"
printf '%b' "\${FAKE_PROFILE_OUT:-========== SUMMARY mode=main PASS=46 FAIL=0 ==========\n========== SUMMARY mode=nnp PASS=10 FAIL=0 ==========\n}"
exit "\${FAKE_PROFILE_RC:-0}"
EOF
cat > "$ROOT/e2e/codex-m3a/run-lifecycle.sh" <<EOF
#!/usr/bin/env bash
echo m3a >> "$STATE/ran"
[ "\${UZI_CODEX_PROOF_STRICT:-}" = 1 ] || exit 9
exit "\${FAKE_M3A_RC:-0}"
EOF
chmod +x "$TMP/bin/"* "$ROOT/e2e/codex-m3a/"*.sh "$ROOT/e2e/codex-m3b/"*.sh "$ROOT/e2e/codex-proof/run-proof.sh"

COMMIT=0123456789abcdef0123456789abcdef01234567
OUT="$TMP/out"
cases=0
passed=0

# run_case NAME TEMPLATE WANT_RC WANT_STATUS WANT_RECEIPT [ENV=VAL ...]
run_case() {
  local name="$1" template="$2" want_rc="$3" want_status="$4" want_receipt="$5"
  shift 5
  cases=$((cases + 1))
  echo sha256:1111 > "$STATE/id"
  rm -f "$STATE/ran"
  rm -rf "$OUT"
  if [ "${PRE_RECEIPT:-}" = 1 ]; then
    mkdir -p "$OUT"
    echo '{"stale":true}' > "$OUT/receipt.json"
  fi
  local rc=0
  env PATH="$TMP/bin:$PATH" UZI_CODEX_PROOF_SKIP_BUILD=1 UZI_CODEX_PROOF_OUT="$OUT" \
    UZI_CODEX_PROOF_COMMIT="$COMMIT" "$@" \
    "$ROOT/e2e/codex-proof/run-proof.sh" "$template" > "$TMP/stdout" 2>&1 || rc=$?
  local why=""
  [ "$rc" -eq "$want_rc" ] || why="exit $rc, want $want_rc"
  if [ -z "$why" ] && [ "$want_status" != none ]; then
    grep -q -F "\"status\":\"$want_status\"" "$OUT/status.json" || why="status.json: $(cat "$OUT/status.json" 2>/dev/null)"
    [ -n "$why" ] || [ "$(wc -l < "$STATE/ran" | tr -d ' ')" = 3 ] || why="not every leg ran: $(tr '\n' ' ' < "$STATE/ran")"
  fi
  if [ -z "$why" ]; then
    if [ "$want_receipt" = yes ]; then
      [ -f "$OUT/receipt.json" ] || why="no receipt.json"
      [ "$(grep -c -F 'CANDIDATE_RECEIPT template=base commit='"$COMMIT" "$TMP/stdout")" = 1 ] || why="no CANDIDATE_RECEIPT line"
    else
      [ ! -e "$OUT/receipt.json" ] || why="receipt.json written on a failed run"
      ! grep -q -F CANDIDATE_RECEIPT "$TMP/stdout" || why="CANDIDATE_RECEIPT printed on a failed run"
    fi
  fi
  if [ -z "$why" ]; then
    passed=$((passed + 1))
    echo "ok   $name"
  else
    echo "FAIL $name: $why"
    sed 's/^/     | /' "$TMP/stdout" | tail -n 20
  fi
}

run_case "all green writes a receipt" base 0 pass yes
PRE_RECEIPT=1 run_case "a stale receipt does not survive a failure" base 1 fail no FAKE_M3B_RC=1
run_case "Landlock unavailable fails" base 1 fail no FAKE_PROBE_RC=10
run_case "Landlock probe error fails" base 1 fail no FAKE_PROBE_RC=11
run_case "a failing m3b leg fails" base 1 fail no FAKE_M3B_RC=1
run_case "a failing m3a lifecycle leg fails" base 1 fail no FAKE_M3A_RC=124
run_case "a failing profile run fails" base 1 fail no FAKE_PROFILE_RC=1
run_case "a profile mode without SUMMARY fails" base 1 fail no \
  'FAKE_PROFILE_OUT=========== SUMMARY mode=main PASS=46 FAIL=0 ==========\n'
run_case "a profile mode with PASS=0 fails" base 1 fail no \
  'FAKE_PROFILE_OUT=========== SUMMARY mode=main PASS=46 FAIL=0 ==========\n========== SUMMARY mode=nnp PASS=0 FAIL=0 ==========\n'
run_case "a profile mode with FAIL>0 fails" base 1 fail no \
  'FAKE_PROFILE_OUT=========== SUMMARY mode=main PASS=45 FAIL=1 ==========\n========== SUMMARY mode=nnp PASS=10 FAIL=0 ==========\n'
run_case "an image ID change mid-proof fails" base 1 fail no FAKE_MUTATE_ID=1
run_case "a non-hex commit is refused" base 2 none no UZI_CODEX_PROOF_COMMIT=HEAD
run_case "an unknown template is refused" jre 2 none no

echo "cases=$cases passed=$passed"
[ "$cases" -ge 13 ] && [ "$passed" -eq "$cases" ]
