#!/bin/sh
# Hermetic contract tests for scripts/oasdiff.sh (PRD #1908 M5): the verified-archive cache,
# the per-arch pins, the archive-structure guard and the status contract. No network and no
# real release binary: a fake curl/wget copies a FIXTURE tarball (a real tar.gz holding
# LICENSE and a stub `oasdiff`), a fake uname picks the host, and a fake sha256sum accepts
# only "the pin the wrapper is supposed to check for this asset" together with "the fixture's
# real digest", so a wrapper that skipped the check, or checked the wrong pin, fails here.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
WRAPPER="$ROOT/scripts/oasdiff.sh"
TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  echo "oasdiff.test.sh: FAIL: $*" >&2
  exit 1
}
assert_eq() { [ "$2" = "$1" ] || fail "$3: expected '$1', got '$2'"; }
assert_contains() { grep -Fq -- "$2" "$1" || fail "$1 does not contain: $2"; }
assert_not_contains() { ! grep -Fq -- "$2" "$1" || fail "$1 unexpectedly contains: $2"; }
file_bytes() { wc -c < "$1" | tr -d ' '; }

REAL_MKTEMP="$(command -v mktemp)"
REAL_SHA256SUM="$(command -v sha256sum || true)"
[ -n "$REAL_SHA256SUM" ] || fail "sha256sum is required to build the fixture digest"
sha_of() { "$REAL_SHA256SUM" "$1" | awk '{print $1}'; }

# --- the pins, read from the wrapper -----------------------------------------------
pin() { awk -F\" -v k="$1=" '$1 == k { print $2; exit }' "$WRAPPER"; }
TASK_PIN="$(awk '$1 == "OASDIFF_VERSION:" { print $2; exit }' "$ROOT/Taskfile.yml")"
[ -n "$TASK_PIN" ] || fail "Taskfile OASDIFF_VERSION is missing"
for k in OASDIFF_DARWIN_ALL OASDIFF_LINUX_AMD64 OASDIFF_LINUX_ARM64; do
  assert_eq "$TASK_PIN" "$(pin "${k}_VERSION")" "Taskfile/$k version pin"
  assert_eq 64 "$(pin "${k}_SHA256" | tr -d '\n' | wc -c | tr -d ' ')" "${k}_SHA256 length"
  case "$(pin "${k}_SHA256")" in
    *[!0-9a-f]*) fail "${k}_SHA256 is not lowercase hex" ;;
  esac
done
VER_NO_V="${TASK_PIN#v}"

# --- fixture archives -----------------------------------------------------------------
mkdir -p "$TMP/fx/good" "$TMP/fx/extra" "$TMP/fx/link" "$TMP/fx/other"
cat > "$TMP/fx/good/oasdiff" <<'STUB'
#!/bin/sh
printf 'stub oasdiff args: %s\n' "$*"
exit "${FAKE_STUB_RC:-0}"
STUB
chmod +x "$TMP/fx/good/oasdiff"
echo "license" > "$TMP/fx/good/LICENSE"
cp "$TMP/fx/good/oasdiff" "$TMP/fx/good/LICENSE" "$TMP/fx/extra/"
echo "surprise" > "$TMP/fx/extra/README.md"
cp "$TMP/fx/good/oasdiff" "$TMP/fx/link/"
ln -s /etc/passwd "$TMP/fx/link/LICENSE"
printf '#!/bin/sh\necho OTHER\n' > "$TMP/fx/other/oasdiff"
chmod +x "$TMP/fx/other/oasdiff"
echo "license" > "$TMP/fx/other/LICENSE"
(cd "$TMP/fx/good" && tar -czf "$TMP/good.tar.gz" LICENSE oasdiff)
(cd "$TMP/fx/extra" && tar -czf "$TMP/extra.tar.gz" LICENSE oasdiff README.md)
(cd "$TMP/fx/link" && tar -czf "$TMP/link.tar.gz" LICENSE oasdiff)
(cd "$TMP/fx/other" && tar -czf "$TMP/other.tar.gz" LICENSE oasdiff)
GOOD_SHA="$(sha_of "$TMP/good.tar.gz")"

# --- fakes ------------------------------------------------------------------------------
FAKE_BIN="$TMP/bin"
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/uname" <<'FAKE'
#!/bin/sh
case "${1:-}" in
  -s) printf '%s\n' "$FAKE_UNAME_S" ;;
  -m) printf '%s\n' "$FAKE_UNAME_M" ;;
  *) exit 2 ;;
esac
FAKE
# Accepts only the pin the test expects the wrapper to check AND the trusted fixture's real
# digest (FAKE_GOOD_SHA), so both the pin choice and the comparison are observable.
cat > "$FAKE_BIN/sha256sum" <<'FAKE'
#!/bin/sh
IFS= read -r line || exit 1
expected="${line%%  *}"
file="${line#*  }"
[ "$expected" = "$FAKE_EXPECTED_PIN" ] || exit 1
actual="$("$FAKE_REAL_SHA256SUM" "$file" | awk '{print $1}')"
[ "$actual" = "$FAKE_GOOD_SHA" ]
FAKE
cat > "$FAKE_BIN/curl" <<'FAKE'
#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_CURL_LOG"
[ "${FAKE_DOWNLOAD_FAIL:-0}" != "1" ] || exit 22
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) shift; out="${1:-}" ;;
  esac
  shift
done
[ -n "$out" ] || exit 2
cp "$FAKE_ARCHIVE" "$out"
FAKE
cat > "$FAKE_BIN/wget" <<'FAKE'
#!/bin/sh
n=0
[ ! -s "$FAKE_WGET_COUNT" ] || n="$(cat "$FAKE_WGET_COUNT")"
n=$((n + 1))
printf '%s\n' "$n" > "$FAKE_WGET_COUNT"
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -qO) shift; out="${1:-}" ;;
  esac
  shift
done
[ -n "$out" ] || exit 2
if [ -e "$out" ]; then
  echo "wget target preexisted on attempt $n" >> "$FAKE_WGET_LOG"
fi
if [ "$n" -le "${FAKE_WGET_FAIL_FIRST:-0}" ]; then
  printf 'partial' >> "$out"
  exit 4
fi
cp "$FAKE_ARCHIVE" "$out"
FAKE
printf '#!/bin/sh\nexit 0\n' > "$FAKE_BIN/sleep"
chmod +x "$FAKE_BIN/uname" "$FAKE_BIN/sha256sum" "$FAKE_BIN/curl" "$FAKE_BIN/wget" "$FAKE_BIN/sleep"

# Model BSD's bare -d behavior on every host, and record the returned path so
# the cleanup assertion cannot pass vacuously when scratch escapes TMPDIR.
cat > "$FAKE_BIN/mktemp" <<'STUB'
#!/bin/sh
set -eu
if [ "$#" -eq 1 ] && [ "$1" = -d ]; then
  scratch="$("$FAKE_REAL_MKTEMP" -d "${TMPDIR%/tmp}/escaped.XXXXXX")"
else
  scratch="$("$FAKE_REAL_MKTEMP" "$@")"
fi
printf '%s\n' "$scratch" >> "$FAKE_MKTEMP_LOG"
printf '%s\n' "$scratch"
STUB
chmod +x "$FAKE_BIN/mktemp"

# A PATH with curl absent, for the wget cases: only the tools the wrapper needs.
NOCURL_BIN="$TMP/nocurl"
mkdir -p "$NOCURL_BIN"
for tool in awk cat chmod cmp cp mkdir mv rm sort tr wc tar gzip head dirname; do
  p="$(command -v "$tool")" || fail "required tool not found: $tool"
  ln -s "$p" "$NOCURL_BIN/$tool"
done
for f in uname sha256sum wget sleep mktemp; do ln -s "$FAKE_BIN/$f" "$NOCURL_BIN/$f"; done

# --- runner -------------------------------------------------------------------------------
CASE=0
FAKE_STUB_RC=0
FAKE_DOWNLOAD_FAIL=0
FAKE_WGET_FAIL_FIRST=0
CACHE_OVERRIDE=""
# run_case <name> <uname -s> <uname -m> <archive> <pin-var> <PATH> <args...>
# Sets OUT (stdout+stderr file), RC, CACHE (the cache root) and the per-case logs.
run_case() {
  name="$1"; us="$2"; um="$3"; archive="$4"; pinvar="$5"; path="$6"; shift 6
  CASE=$((CASE + 1))
  CASEDIR="$TMP/case$CASE"
  mkdir -p "$CASEDIR/tmp"
  OUT="$CASEDIR/out"
  CURL_LOG="$CASEDIR/curl.log"
  WGET_COUNT="$CASEDIR/wget.count"
  WGET_LOG="$CASEDIR/wget.log"
  : > "$CASEDIR/mktemp.log"
  : > "$CURL_LOG"
  : > "$WGET_LOG"
  rm -f "$WGET_COUNT"
  CACHE="${CACHE_OVERRIDE:-$CASEDIR/cache}"
  RC=0
  env PATH="$path" TMPDIR="$CASEDIR/tmp" UZI_OASDIFF_DIR="$CACHE" HOME="$CASEDIR" \
    FAKE_REAL_MKTEMP="$REAL_MKTEMP" FAKE_MKTEMP_LOG="$CASEDIR/mktemp.log" \
    FAKE_UNAME_S="$us" FAKE_UNAME_M="$um" FAKE_ARCHIVE="$archive" \
    FAKE_EXPECTED_PIN="$(pin "$pinvar")" FAKE_REAL_SHA256SUM="$REAL_SHA256SUM" FAKE_GOOD_SHA="$GOOD_SHA" \
    FAKE_CURL_LOG="$CURL_LOG" FAKE_WGET_COUNT="$WGET_COUNT" FAKE_WGET_LOG="$WGET_LOG" \
    FAKE_STUB_RC="$FAKE_STUB_RC" FAKE_DOWNLOAD_FAIL="$FAKE_DOWNLOAD_FAIL" FAKE_WGET_FAIL_FIRST="$FAKE_WGET_FAIL_FIRST" \
    /bin/sh "$WRAPPER" "$@" > "$OUT" 2>&1 || RC=$?
  while IFS= read -r scratch; do
    case "$scratch" in
      "$CASEDIR/tmp/"*) ;;
      *) fail "$name: temporary extraction outside case scratch: $scratch" ;;
    esac
    [ ! -e "$scratch" ] || fail "$name: temporary extraction was not cleaned: $scratch"
  done < "$CASEDIR/mktemp.log"
  # Whatever the outcome, the private extraction must be gone.
  leftover="$(ls -A "$CASEDIR/tmp")"
  [ -z "$leftover" ] || fail "$name: the wrapper left temp files behind: $leftover"
}
WITH_CURL="$FAKE_BIN:$PATH"
LINUX_AMD64_CACHED() { echo "$CACHE/$VER_NO_V/linux_amd64/oasdiff_${VER_NO_V}_linux_amd64.tar.gz"; }

# 1. usage and pin refusals ---------------------------------------------------------------
run_case "no version" Linux x86_64 "$TMP/good.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$WITH_CURL"
assert_eq 2 "$RC" "no version"
assert_contains "$OUT" "usage:"
run_case "wrong version" Linux x86_64 "$TMP/good.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$WITH_CURL" v0.0.1 --version
assert_eq 2 "$RC" "wrong version"
assert_contains "$OUT" "pinned archives are for $TASK_PIN"
assert_eq 0 "$(file_bytes "$CURL_LOG")" "a refused version must not download"
run_case "unsupported host" Linux mips "$TMP/good.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$WITH_CURL" "$TASK_PIN" --version
assert_eq 2 "$RC" "unsupported host"
assert_contains "$OUT" "unsupported host Linux/mips"

# 2. happy path: the right asset URL, retrying download, args forwarded, archive cached --------
run_case "linux amd64" Linux x86_64 "$TMP/good.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$WITH_CURL" "$TASK_PIN" breaking a.yaml b.yaml --fail-on ERR
assert_eq 0 "$RC" "linux amd64"
[ -s "$CASEDIR/mktemp.log" ] || fail "linux amd64: no temporary extraction observed"
assert_contains "$OUT" "stub oasdiff args: breaking a.yaml b.yaml --fail-on ERR"
assert_contains "$CURL_LOG" "https://github.com/oasdiff/oasdiff/releases/download/v${VER_NO_V}/oasdiff_${VER_NO_V}_linux_amd64.tar.gz"
assert_contains "$CURL_LOG" "--retry-all-errors"
[ -f "$(LINUX_AMD64_CACHED)" ] || fail "the verified archive was not cached"

# 3. the cache is used; a corrupt entry is replaced --------------------------------------------
CACHE_OVERRIDE="$CACHE"
run_case "cache hit" Linux x86_64 "$TMP/good.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$WITH_CURL" "$TASK_PIN" --version
assert_eq 0 "$RC" "cache hit"
assert_eq 0 "$(file_bytes "$CURL_LOG")" "a verified cache entry must not re-download"
printf 'corrupt' > "$(LINUX_AMD64_CACHED)"
run_case "corrupt cache" Linux x86_64 "$TMP/good.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$WITH_CURL" "$TASK_PIN" --version
assert_eq 0 "$RC" "corrupt cache"
[ "$(file_bytes "$CURL_LOG")" -gt 0 ] || fail "a corrupt cache entry was not re-downloaded"
assert_eq "$GOOD_SHA" "$(sha_of "$(LINUX_AMD64_CACHED)")" "the corrupt cache entry must be replaced by the verified archive"
CACHE_OVERRIDE=""

# 4. the other hosts pick their own asset and pin --------------------------------------------
run_case "darwin arm64" Darwin arm64 "$TMP/good.tar.gz" OASDIFF_DARWIN_ALL_SHA256 "$WITH_CURL" "$TASK_PIN" --version
assert_eq 0 "$RC" "darwin arm64"
assert_contains "$CURL_LOG" "oasdiff_${VER_NO_V}_darwin_all.tar.gz"
run_case "darwin x86_64" Darwin x86_64 "$TMP/good.tar.gz" OASDIFF_DARWIN_ALL_SHA256 "$WITH_CURL" "$TASK_PIN" --version
assert_eq 0 "$RC" "darwin x86_64"
assert_contains "$CURL_LOG" "oasdiff_${VER_NO_V}_darwin_all.tar.gz"
run_case "linux aarch64" Linux aarch64 "$TMP/good.tar.gz" OASDIFF_LINUX_ARM64_SHA256 "$WITH_CURL" "$TASK_PIN" --version
assert_eq 0 "$RC" "linux aarch64"
assert_contains "$CURL_LOG" "oasdiff_${VER_NO_V}_linux_arm64.tar.gz"

# 5. oasdiff's own status is the wrapper's status ---------------------------------------------
FAKE_STUB_RC=1
run_case "status 1" Linux x86_64 "$TMP/good.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$WITH_CURL" "$TASK_PIN" breaking
assert_eq 1 "$RC" "a findings status must pass through"
FAKE_STUB_RC=102
run_case "status 102" Linux x86_64 "$TMP/good.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$WITH_CURL" "$TASK_PIN" breaking
assert_eq 102 "$RC" "a tool-error status must pass through"
FAKE_STUB_RC=0

# 6. refusals: nothing is cached and the binary never runs ------------------------------------
run_case "checksum mismatch" Linux x86_64 "$TMP/other.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$WITH_CURL" "$TASK_PIN" --version
assert_eq 2 "$RC" "checksum mismatch"
assert_contains "$OUT" "tarball checksum mismatch"
assert_not_contains "$OUT" "OTHER"
[ ! -e "$(LINUX_AMD64_CACHED)" ] || fail "an unverified archive was cached"

FAKE_DOWNLOAD_FAIL=1
run_case "download failure" Linux x86_64 "$TMP/good.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$WITH_CURL" "$TASK_PIN" --version
assert_eq 2 "$RC" "download failure"
assert_contains "$OUT" "bounded download failed"
FAKE_DOWNLOAD_FAIL=0

# The structure guard runs on a digest-VERIFIED archive, so each bad fixture is presented as the
# trusted one (the fake sha256sum is told its digest is the good one).
SAVED_GOOD_SHA="$GOOD_SHA"
for bad in extra link; do
  GOOD_SHA="$(sha_of "$TMP/$bad.tar.gz")"
  run_case "$bad member" Linux x86_64 "$TMP/$bad.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$WITH_CURL" "$TASK_PIN" --version
  assert_eq 2 "$RC" "$bad member"
  assert_not_contains "$OUT" "stub oasdiff"
  [ ! -e "$(LINUX_AMD64_CACHED)" ] || fail "$bad: an archive that failed the structure guard was cached"
done
assert_contains "$OUT" "non-regular or extra member"
GOOD_SHA="$SAVED_GOOD_SHA"

# 7. wget fallback: retried, each attempt from a clean target --------------------------------------
FAKE_WGET_FAIL_FIRST=2
run_case "wget retry" Linux x86_64 "$TMP/good.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$NOCURL_BIN" "$TASK_PIN" --version
assert_eq 0 "$RC" "wget retry"
assert_eq 3 "$(cat "$WGET_COUNT")" "wget attempts (two failures then success)"
assert_eq 0 "$(file_bytes "$WGET_LOG")" "a retry must start from an absent target"
FAKE_WGET_FAIL_FIRST=9
run_case "wget exhausted" Linux x86_64 "$TMP/good.tar.gz" OASDIFF_LINUX_AMD64_SHA256 "$NOCURL_BIN" "$TASK_PIN" --version
assert_eq 2 "$RC" "wget exhausted"
assert_eq 4 "$(cat "$WGET_COUNT")" "wget gives up after 4 attempts"
FAKE_WGET_FAIL_FIRST=0

echo "oasdiff.test.sh: all cases passed"
