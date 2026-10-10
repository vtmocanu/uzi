#!/bin/sh
# Hermetic cleanup-ownership tests for scripts/assert-web-nginx.sh (#2662).
#
# No real docker and no network: FAKE `docker`, `helm`, `yq`, `curl` and `cmp` on PATH log
# every call (gzip stays real, the fixtures need it). The fake `docker create` fails with a
# name conflict for the container named by FAKE_CREATE_FAIL, as it would when a container
# this run did not create already holds that name. The EXIT cleanup must then remove only
# what this run created: containers by the ID their successful create printed, never by
# name, and the network it created. A conflict on the first create removes no container.
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
  image) [ "$2" = inspect ] && exit 0 ;;
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

if [ "$failed" != 0 ]; then
  for c in first later; do
    echo "--- $c docker calls:" >&2
    grep '^docker ' "$TMP/$c.log" >&2 || true
  done
  exit 1
fi
echo "assert-web-nginx.test.sh: PASS (2 cases)"
