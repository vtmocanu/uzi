#!/bin/sh
# Hermetic cleanup-ownership tests for scripts/assert-web-nginx.sh (#2662).
#
# No real docker and no network: FAKE `docker`, `helm`, `yq`, `curl` and `cmp` on PATH log
# every call (gzip stays real, the fixtures need it). The fake `docker create` fails with a
# name conflict for the container named by FAKE_CREATE_FAIL, as it would when a container
# this run did not create already holds that name. The EXIT cleanup must then remove only
# what this run created: containers by the ID their successful create printed, never by
# name, and the network it created. A conflict on the first create removes no container.
#
# The image-source cases (#2698) run with the fake `docker image inspect` missing and the
# fake `docker pull` accepting only refs matching FAKE_PULL_OK: the script must run the
# digest-pinned image from mirror.gcr.io when it can, fall back to docker.io, and stop with
# exit 2 before creating anything when neither can be pulled.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
SUT="$ROOT/scripts/assert-web-nginx.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

failed=0
fail() {
  echo "assert-web-nginx.test.sh: FAIL: $*" >&2
  failed=1
}

mkdir -p "$TMP/bin"
cat > "$TMP/bin/docker" <<'EOF'
#!/bin/sh
echo "docker $*" >> "$FAKE_LOG"
case "$1" in
  info|cp|start|exec|logs|rm) exit 0 ;;
  image) [ "$2" = inspect ] && { [ "${FAKE_INSPECT_MISS:-}" = 1 ] && exit 1; exit 0; } ;;
  pull)
    case "$2" in
      ${FAKE_PULL_OK:-no-match}) exit 0 ;;
    esac
    echo "Error response from daemon: fake pull refused $2" >&2
    exit 1 ;;
  network) case "$2" in create|rm) exit 0 ;; esac ;;
  port) echo "127.0.0.1:1"; exit 0 ;;
  create)
    name=""
    prev=""
    for a in "$@"; do
      [ "$prev" = "--name" ] && name=$a
      prev=$a
    done
    case "$name" in
      $FAKE_CREATE_FAIL)
        echo "Error response from daemon: Conflict. The container name \"/$name\" is already in use" >&2
        exit 1 ;;
    esac
    n=$(($(cat "$FAKE_IDS" 2>/dev/null || echo 0) + 1))
    echo "$n" > "$FAKE_IDS"
    echo "fakeid-$n"
    exit 0 ;;
esac
echo "fake docker: unexpected: $*" >&2
exit 99
EOF
cat > "$TMP/bin/helm" <<'EOF'
#!/bin/sh
echo "helm $*" >> "$FAKE_LOG"
printf 'apiVersion: v1\nkind: ConfigMap\ndata:\n  default.conf: |\n    server {}\n'
EOF
cat > "$TMP/bin/yq" <<'EOF'
#!/bin/sh
echo "yq $*" >> "$FAKE_LOG"
echo "server {}"
EOF
# curl and cmp only have to exist: neither is reached before the failures under test.
for t in curl cmp; do
  cat > "$TMP/bin/$t" <<'EOF'
#!/bin/sh
echo "$(basename "$0") $*" >> "$FAKE_LOG"
EOF
done
chmod +x "$TMP/bin/docker" "$TMP/bin/helm" "$TMP/bin/yq" "$TMP/bin/curl" "$TMP/bin/cmp"

# run <case> <create-fail-glob>: runs the script against this repo, expects exit 2 (the
# instrument is broken) and leaves its calls in $TMP/<case>.log.
run() {
  case_name=$1
  FAKE_CREATE_FAIL=$2
  FAKE_LOG="$TMP/$case_name.log"
  FAKE_IDS="$TMP/$case_name.ids"
  export FAKE_CREATE_FAIL FAKE_LOG FAKE_IDS
  : > "$FAKE_LOG"
  mkdir -p "$TMP/$case_name.tmp"
  rc=0
  PATH="$TMP/bin:$PATH" TMPDIR="$TMP/$case_name.tmp" bash "$SUT" "$ROOT" > "$TMP/$case_name.out" 2>&1 || rc=$?
  [ "$rc" = 2 ] || fail "$case_name: exit $rc, want 2 ($(cat "$TMP/$case_name.out"))"
  grep -q 'docker create .* failed' "$TMP/$case_name.out" \
    || fail "$case_name: did not stop at the conflicting create ($(cat "$TMP/$case_name.out"))"
}

# run_pull <case> <pull-ok-glob>: image inspect misses, so the script must pull; only refs
# matching the glob pull. Sets pull_rc. A run that gets an image goes on to the (fake curl)
# properties and exits 1; exit 2 is the "no image" outcome.
run_pull() {
  case_name=$1
  FAKE_PULL_OK=$2
  FAKE_INSPECT_MISS=1
  FAKE_CREATE_FAIL='no-such-container'
  FAKE_LOG="$TMP/$case_name.log"
  FAKE_IDS="$TMP/$case_name.ids"
  export FAKE_PULL_OK FAKE_INSPECT_MISS FAKE_CREATE_FAIL FAKE_LOG FAKE_IDS
  : > "$FAKE_LOG"
  mkdir -p "$TMP/$case_name.tmp"
  pull_rc=0
  PATH="$TMP/bin:$PATH" TMPDIR="$TMP/$case_name.tmp" bash "$SUT" "$ROOT" > "$TMP/$case_name.out" 2>&1 || pull_rc=$?
  unset FAKE_INSPECT_MISS FAKE_PULL_OK
}

# creates_end_with <case> <ref>: exactly 4 docker create lines, each ending in " <ref>".
creates_end_with() {
  n=$(grep -c '^docker create ' "$TMP/$1.log" || true)
  [ "$n" = 4 ] || fail "$1: want 4 docker create lines, got $n"
  m=$(grep '^docker create ' "$TMP/$1.log" | grep -cF " $2" || true)
  [ "$m" = 4 ] || fail "$1: want all 4 creates to end with ' $2', got $m"
  grep '^docker create ' "$TMP/$1.log" | grep -qvF -- " $2" \
    && fail "$1: a docker create does not end with ' $2'"
  return 0
}

# rms <case>: every container removal the run made, one per line.
rms() { grep '^docker rm ' "$TMP/$1.log" || true; }

# The network was created, so it is removed; it is the only network removal.
expect_net_rm() {
  [ "$(grep -c '^docker network rm wnginx-[0-9]*-net$' "$TMP/$1.log" || true)" = 1 ] \
    || fail "$1: the created network was not removed exactly once"
}

# Case 1: the very first create (the stub upstream) conflicts. Nothing was created, so
# no container is removed at all, least of all the one holding the name.
run first '*-up'
[ -z "$(rms first)" ] || fail "first: removed a container it never created: $(rms first)"
expect_net_rm first

# Case 2: the stub is created and started, then the compose web create conflicts. Only
# the stub is removed, by the ID its create printed; the conflicting name never is.
run later '*-compose'
grep -q '^docker start wnginx-[0-9]*-up$' "$TMP/later.log" || fail "later: the stub was never started"
[ "$(rms later)" = "docker rm -f fakeid-1" ] \
  || fail "later: want exactly 'docker rm -f fakeid-1', got: $(rms later)"
expect_net_rm later

DOCKERFILE_REF=$(awk '/^FROM nginxinc\/nginx-unprivileged/ { print $2; exit }' "$ROOT/web/Dockerfile")
DIGEST=${DOCKERFILE_REF##*@}
MIRROR_REF="mirror.gcr.io/nginxinc/nginx-unprivileged@$DIGEST"
HUB_REF="docker.io/nginxinc/nginx-unprivileged@$DIGEST"

# Case 3: the mirror pulls; Docker Hub is never touched and every container runs the mirror ref.
run_pull mirror 'mirror.gcr.io/*'
[ "$pull_rc" != 2 ] || fail "mirror: exit 2 ($(cat "$TMP/mirror.out"))"
grep -qF "docker pull $MIRROR_REF" "$TMP/mirror.log" || fail "mirror: never pulled $MIRROR_REF"
! grep -qF 'docker pull docker.io/' "$TMP/mirror.log" || fail "mirror: pulled from docker.io although the mirror worked"
creates_end_with mirror "$MIRROR_REF"
grep -qF "nginx image: $MIRROR_REF" "$TMP/mirror.out" || fail "mirror: output does not name the mirror ref"

# Case 4: the mirror refuses; Docker Hub is tried second and every container runs its ref.
run_pull hub 'docker.io/*'
[ "$pull_rc" != 2 ] || fail "hub: exit 2 ($(cat "$TMP/hub.out"))"
mline=$(grep -nF "docker pull $MIRROR_REF" "$TMP/hub.log" | head -n 1 | cut -d: -f1)
hline=$(grep -nF "docker pull $HUB_REF" "$TMP/hub.log" | head -n 1 | cut -d: -f1)
{ [ -n "$mline" ] && [ -n "$hline" ] && [ "$mline" -lt "$hline" ]; } \
  || fail "hub: want the mirror pull before the Docker Hub pull (mirror=$mline hub=$hline)"
creates_end_with hub "$HUB_REF"
grep -qF "nginx image: $HUB_REF" "$TMP/hub.out" || fail "hub: output does not name the docker.io ref"
grep -qF 'registry docker.io' "$TMP/hub.out" || fail "hub: output does not name docker.io"

# Case 5: neither registry works. The instrument is broken (exit 2) before anything is created.
run_pull none 'no-match'
[ "$pull_rc" = 2 ] || fail "none: exit $pull_rc, want 2"
[ "$(grep -c '^docker create ' "$TMP/none.log" || true)" = 0 ] || fail "none: created a container without an image"
grep -qF "$MIRROR_REF" "$TMP/none.out" || fail "none: output does not name $MIRROR_REF"
grep -qF "$HUB_REF" "$TMP/none.out" || fail "none: output does not name $HUB_REF"
[ "$(grep -c 'fake pull refused' "$TMP/none.out" || true)" = 2 ] || fail "none: want both pull errors in the output"

if [ "$failed" != 0 ]; then
  for c in first later mirror hub none; do
    echo "--- $c docker calls:" >&2
    grep '^docker ' "$TMP/$c.log" >&2 || true
  done
  exit 1
fi
echo "assert-web-nginx.test.sh: PASS (5 cases)"
