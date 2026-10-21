#!/usr/bin/env bash
# Native-architecture Codex package execution check (#1565 scope 5). Runs the image build's own
# install-codex.sh and assert-codex.sh for <arch> inside the worker templates' pinned runtime base,
# with no image build. codex-packaged-proof.yml runs it for arm64 on a native arm64 runner; only
# amd64 images ship, so this is the only check that downloads and executes the arm64 artifact.
#
# What a pass proves, exactly: the <arch> artifact's SHA256, members and manifest fields match the
# lock; `codex --version` executes as the cap-less runner uid and prints the pinned version; the
# code-mode host is present and executable (assert-codex.sh does not execute it). It is NOT the
# packaged OS/uid/Landlock proof (run-proof.sh, amd64 only).
#
# Usage: package-exec.sh <amd64|arm64>
# Refuses (exit 2) an unknown arch, a docker daemon of another architecture (an emulated run would
# not prove native execution), or templates that disagree on the runtime base.
set -euo pipefail

die()   { printf 'package-exec: FATAL: %s\n' "$*" >&2; exit 1; }
usage() { printf 'package-exec: %s\n' "$*" >&2; exit 2; }

ARCH="${1:-}"
case "$ARCH" in
  amd64) NATIVE=x86_64 ;;
  arm64) NATIVE=aarch64 ;;
  *) usage "usage: package-exec.sh <amd64|arm64> (got '${ARCH}')" ;;
esac

ROOT="$(git rev-parse --show-toplevel)"

# The runtime base both templates build FROM. The image build installs Codex in that stage, so the
# check runs in the same pinned base; the two templates must agree or the check would cover one.
runtime_base() {
  local file="$1" ref
  ref="$(awk '$1 == "FROM" && $3 == "AS" && $4 == "runtime" { print $2; exit }' "$file")"
  [ -n "$ref" ] || usage "no 'FROM <image> AS runtime' line in ${file}"
  printf '%s' "$ref"
}
BASE_IMAGE="$(runtime_base "$ROOT/agent/templates/base/Dockerfile")"
JVM_IMAGE="$(runtime_base "$ROOT/agent/templates/jvm/Dockerfile")"
[ "$BASE_IMAGE" = "$JVM_IMAGE" ] \
  || usage "templates disagree on the runtime base: base=${BASE_IMAGE} jvm=${JVM_IMAGE}"
case "$BASE_IMAGE" in
  *@sha256:*) ;;
  *) usage "runtime base is not digest-pinned: ${BASE_IMAGE}" ;;
esac

DAEMON_ARCH="$(docker info --format '{{.Architecture}}')" || die "docker info failed"
[ "$DAEMON_ARCH" = "$NATIVE" ] \
  || usage "docker daemon is ${DAEMON_ARCH}, not native ${NATIVE}: refusing an emulated run"

printf 'package-exec: %s on %s, runtime base %s\n' "$ARCH" "$DAEMON_ARCH" "$BASE_IMAGE"

# Inside: the runtime stage's prerequisites for the two scripts (util-linux setpriv, which the
# busybox applet cannot replace, and the runner uid from templates/base/Dockerfile), then the
# scripts themselves from a read-only copy of agent/codex.
docker run --rm --platform "linux/${ARCH}" \
  -v "$ROOT/agent/codex:/src:ro" \
  "$BASE_IMAGE" sh -euc '
    n=0
    until apk add --no-cache bash curl tar setpriv >/dev/null; do
      n=$((n+1)); [ "$n" -ge 5 ] && { echo "apk add: giving up after $n attempts" >&2; exit 1; }
      sleep "$((n*3))"
    done
    /bin/setpriv --help 2>&1 | grep -q -- --reuid
    addgroup -g 10002 runner && adduser -u 10002 -G runner -h /home/runner -D runner
    [ "$(uname -m)" = "$1" ] || { echo "container is $(uname -m), expected $1" >&2; exit 1; }
    mkdir /tmp/codex && cp /src/codex-package.lock /src/install-codex.sh /src/assert-codex.sh /tmp/codex/
    bash /tmp/codex/install-codex.sh "$2"
    bash /tmp/codex/assert-codex.sh
  ' package-exec "$NATIVE" "$ARCH"

printf 'package-exec: PASS %s\n' "$ARCH"
