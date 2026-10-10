#!/bin/sh
# Hermetic contract tests for scripts/docker-pull-fallback.sh.
#
# No real docker and no network: a FAKE `docker` on PATH logs every call and fails a
# pull of the mirror or of Docker Hub on demand. That proves the mirror is tried first,
# that a mirror miss falls back to Docker Hub AND re-tags under the mirror name (the
# name every consumer uses), and the exit codes (0 present, 1 both failed, 2 usage).
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
SUT="$ROOT/scripts/docker-pull-fallback.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  echo "docker-pull-fallback.test.sh: FAIL: $*" >&2
  exit 1
}

mkdir -p "$TMP/bin"
cat > "$TMP/bin/docker" <<'EOF'
#!/bin/sh
echo "$*" >> "$FAKE_LOG"
case "$1 $2" in
  "pull mirror.gcr.io/"*) [ "${FAKE_MIRROR_OK:-1}" = 1 ] ;;
  "pull docker.io/"*)     [ "${FAKE_HUB_OK:-1}" = 1 ] ;;
  "tag "*)                exit 0 ;;
  *)                      echo "fake docker: unexpected: $*" >&2; exit 99 ;;
esac
EOF
chmod +x "$TMP/bin/docker"

# run <case> <expected-rc> <args...>: runs the script, leaves its calls in $TMP/<case>.log
run() {
  case_name=$1; want=$2; shift 2
  FAKE_LOG="$TMP/$case_name.log"; export FAKE_LOG
  : > "$FAKE_LOG"
  rc=0
  PATH="$TMP/bin:$PATH" "$SUT" "$@" > "$TMP/$case_name.out" 2>&1 || rc=$?
  [ "$rc" = "$want" ] || fail "$case_name: exit $rc, want $want ($(cat "$TMP/$case_name.out"))"
}

expect_log() {
  printf '%s\n' "$2" > "$TMP/$1.want"
  diff -u "$TMP/$1.want" "$TMP/$1.log" >&2 || fail "$1: docker calls differ"
}

# Mirror hit: one pull, from the mirror, official image under library/.
FAKE_MIRROR_OK=1 FAKE_HUB_OK=1 run hit 0 postgres:17
expect_log hit "pull mirror.gcr.io/library/postgres:17"

# Mirror miss: Docker Hub pull, then re-tag under the mirror name.
FAKE_MIRROR_OK=0 FAKE_HUB_OK=1 run miss 0 moby/buildkit:buildx-stable-1
expect_log miss "pull mirror.gcr.io/moby/buildkit:buildx-stable-1
pull docker.io/moby/buildkit:buildx-stable-1
tag docker.io/moby/buildkit:buildx-stable-1 mirror.gcr.io/moby/buildkit:buildx-stable-1"

# Both fail: exit 1, nothing tagged.
FAKE_MIRROR_OK=0 FAKE_HUB_OK=0 run both 1 postgres:17
expect_log both "pull mirror.gcr.io/library/postgres:17
pull docker.io/library/postgres:17"

# No tag means latest.
FAKE_MIRROR_OK=1 run notag 0 alpine
expect_log notag "pull mirror.gcr.io/library/alpine:latest"

# Usage errors: no docker call at all.
run noarg 2
run digest 2 postgres:17@sha256:5c855ad7b85e68e48a62f34662853f38b57c1c1d80f3a927ab58034fd6d31c5e
run host 2 ghcr.io/vtmocanu/uzi:1
run two 2 postgres:17 alpine
run hostport 2 localhost:5000/postgres:17
run domainport 2 registry.example:5000/repo:tag
run localhost 2 localhost/postgres:17
run emptytag 2 postgres:
for c in noarg digest host two hostport domainport localhost emptytag; do
  [ ! -s "$TMP/$c.log" ] || fail "$c: docker was called on a usage error"
done

echo "docker-pull-fallback.test.sh: PASS (12 cases)"
