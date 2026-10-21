#!/bin/sh
# Hermetic pull/retry/output contract: fake docker and sleep, no network or waits.
set -eu
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
SUT="$ROOT/scripts/docker-pull-fallback.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
fail() { echo "docker-pull-fallback.test.sh: FAIL: $*" >&2; exit 1; }
mkdir -p "$TMP/bin"
cat > "$TMP/bin/docker" <<'FAKE'
#!/bin/sh
# Simulate docker's noisy stdout too: --print-ref must not leak it.
echo "docker progress"
echo "$*" >> "$FAKE_LOG"
case "$1 $2" in
  "pull mirror.gcr.io/"*) registry=mirror; failures=${FAKE_MIRROR_FAILURES:-0} ;;
  "pull docker.io/"*) registry=hub; failures=${FAKE_HUB_FAILURES:-0} ;;
  "tag "*)
    case "$3" in *@*) echo "digest target rejected" >&2; exit 99 ;; esac
    exit "${FAKE_TAG_RC:-0}" ;;
  *) echo "unexpected docker call: $*" >&2; exit 99 ;;
esac
count=0
[ ! -f "$FAKE_LOG.$registry" ] || count=$(cat "$FAKE_LOG.$registry")
count=$((count + 1))
printf '%s\n' "$count" > "$FAKE_LOG.$registry"
[ "$count" -gt "$failures" ]
FAKE
cat > "$TMP/bin/sleep" <<'FAKE'
#!/bin/sh
echo "sleep $*" >> "$FAKE_LOG"
FAKE
chmod +x "$TMP/bin/docker" "$TMP/bin/sleep"
cases=0
run() {
  case_name=$1; want=$2; shift 2
  FAKE_LOG="$TMP/$case_name.log"; export FAKE_LOG
  : > "$FAKE_LOG"
  rc=0
  PATH="$TMP/bin:$PATH" "$SUT" "$@" > "$TMP/$case_name.out" 2> "$TMP/$case_name.err" || rc=$?
  [ "$rc" = "$want" ] || fail "$case_name: exit $rc, want $want ($(cat "$TMP/$case_name.err"))"
  cases=$((cases + 1))
  # POSIX shells may retain prefix assignments to functions between calls.
  unset FAKE_MIRROR_FAILURES FAKE_HUB_FAILURES FAKE_TAG_RC
}
expect_log() {
  printf '%s\n' "$2" > "$TMP/$1.want"
  diff -u "$TMP/$1.want" "$TMP/$1.log" >&2 || fail "$1: docker/sleep calls differ"
}
expect_ref() {
  printf '%s\n' "$2" > "$TMP/$1.ref"
  diff -u "$TMP/$1.ref" "$TMP/$1.out" >&2 || fail "$1: stdout is not exactly the selected ref"
}
# A failed registry has exactly three pulls and two sleeps, no terminal delay.
exhausted() { printf 'pull %s\nsleep 5\npull %s\nsleep 15\npull %s\n' "$1" "$1" "$1"; }
mirror=mirror.gcr.io/library/postgres:17
hub=docker.io/library/postgres:17
run hit 0 postgres:17
expect_log hit "pull $mirror"
[ ! -s "$TMP/hit.out" ] || fail 'tag mode leaked stdout'
FAKE_MIRROR_FAILURES=1 run transient 0 postgres:17
expect_log transient "pull $mirror
sleep 5
pull $mirror"
FAKE_MIRROR_FAILURES=2 run third 0 postgres:17
expect_log third "$(exhausted "$mirror")"
FAKE_MIRROR_FAILURES=3 run fallback 0 postgres:17
expect_log fallback "$(exhausted "$mirror")
pull $hub
tag $hub $mirror"
FAKE_MIRROR_FAILURES=3 FAKE_HUB_FAILURES=1 run hubtransient 0 postgres:17
expect_log hubtransient "$(exhausted "$mirror")
pull $hub
sleep 5
pull $hub
tag $hub $mirror"
FAKE_MIRROR_FAILURES=3 FAKE_HUB_FAILURES=3 run both 1 postgres:17
expect_log both "$(exhausted "$mirror")
$(exhausted "$hub")"
FAKE_MIRROR_FAILURES=3 FAKE_TAG_RC=1 run tagfail 1 postgres:17
expect_log tagfail "$(exhausted "$mirror")
pull $hub
tag $hub $mirror"
run notag 0 alpine
expect_log notag 'pull mirror.gcr.io/library/alpine:latest'
FAKE_MIRROR_FAILURES=3 run namespaced 0 moby/buildkit:buildx-stable-1
expect_log namespaced "$(exhausted mirror.gcr.io/moby/buildkit:buildx-stable-1)
pull docker.io/moby/buildkit:buildx-stable-1
tag docker.io/moby/buildkit:buildx-stable-1 mirror.gcr.io/moby/buildkit:buildx-stable-1"
digest=sha256:5c855ad7b85e68e48a62f34662853f38b57c1c1d80f3a927ab58034fd6d31c5e
run digestmirror 0 --print-ref "postgres:17@$digest"
expect_log digestmirror "pull $mirror@$digest"
expect_ref digestmirror "$mirror@$digest"
FAKE_MIRROR_FAILURES=3 run digesthub 0 --print-ref "postgres:17@$digest"
expect_log digesthub "$(exhausted "$mirror@$digest")
pull $hub@$digest"
expect_ref digesthub "$hub@$digest"
FAKE_MIRROR_FAILURES=3 FAKE_HUB_FAILURES=3 run digestboth 1 --print-ref "postgres:17@$digest"
expect_log digestboth "$(exhausted "$mirror@$digest")
$(exhausted "$hub@$digest")"
[ ! -s "$TMP/digestboth.out" ] || fail 'failed digest pull printed a ref'
FAKE_MIRROR_FAILURES=3 run printtag 0 --print-ref postgres:17
expect_ref printtag "$mirror"
# Invalid input must never call docker or sleep.
invalid() {
  invalid_case=$1; shift
  run "$invalid_case" 2 "$@"
  [ ! -s "$TMP/$invalid_case.log" ] || fail "$invalid_case: tool called on usage error"
  [ ! -s "$TMP/$invalid_case.out" ] || fail "$invalid_case: usage error printed stdout"
}
invalid noarg
invalid digest "postgres:17@$digest"
invalid host ghcr.io/example/uzi:1
invalid two postgres:17 alpine
invalid hostport localhost:5000/postgres:17
invalid domainport registry.example:5000/repo:tag
invalid localhost localhost/postgres:17
invalid emptytag postgres:
invalid short --print-ref postgres:17@sha256:abc
invalid algorithm --print-ref "postgres:17@sha512:${digest#sha256:}"
invalid notagdig --print-ref "postgres@$digest"
invalid twodig --print-ref "postgres:17@$digest@$digest"
invalid printmissing --print-ref
invalid whitespace 'postgres:17 x'
echo "docker-pull-fallback.test.sh: PASS ($cases cases)"
