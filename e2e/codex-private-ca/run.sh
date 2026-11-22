#!/usr/bin/env bash
# Container proof against baked production sources, without host credentials.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
cd "$REPO"
if [[ -n "${FIXTURE_IMAGE+x}" || -n "${FIXTURE_EXPECTED_ID+x}" ]]; then
  : "${FIXTURE_IMAGE:?explicit image requires FIXTURE_IMAGE and FIXTURE_EXPECTED_ID}"
  : "${FIXTURE_EXPECTED_ID:?explicit image requires FIXTURE_IMAGE and FIXTURE_EXPECTED_ID}"
fi
IMAGE=${FIXTURE_IMAGE:-cdr-codex-ca:6b4d31d7e}
EXPECTED=${FIXTURE_EXPECTED_ID:-sha256:eda31cfe5c9f9e9d819174eb33ecd060f69cf5e9c5e239d390ff5c909c4d2b1a}
actual=$(docker image inspect "$IMAGE" --format '{{.Id}}')
printf 'image=%s id=%s revision=%s\n' "$IMAGE" "$actual" "$(git rev-parse HEAD)"
test "$actual" = "$EXPECTED"
sha256sum "$HERE/run.sh" "$HERE/fixture.test.ts" "$HERE/README.md"
scratch=$(mktemp -d "$REPO/.uzi/scratch/private-ca.XXXXXX")
name="cdr-private-ca-$$"
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$scratch/ca.key" -out "$scratch/ca.pem" -days 1 -subj /CN=cdr-private-ca >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -keyout "$scratch/server.key" -out "$scratch/server.csr" -subj /CN=localhost >/dev/null 2>&1
printf 'subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' > "$scratch/extensions"
openssl x509 -req -in "$scratch/server.csr" -CA "$scratch/ca.pem" -CAkey "$scratch/ca.key" -CAcreateserial -out "$scratch/server.pem" -days 1 -extfile "$scratch/extensions" >/dev/null 2>&1
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$scratch/unrelated.key" -out "$scratch/unrelated.pem" -days 1 -subj /CN=cdr-unrelated-ca >/dev/null 2>&1
mkdir "$scratch/mount"
cp "$scratch/unrelated.pem" "$scratch/ca.pem" "$scratch/server.key" "$scratch/server.pem" "$scratch/mount/"
chmod 755 "$scratch" "$scratch/mount"
chmod 644 "$scratch/mount/"*
set +e
set -x
started_seconds=$(date +%s)
timeout --kill-after=10s 240s docker run --rm --name "$name" --network none \
  --env "FIXTURE_CONTAINER_STARTED_SECONDS=$started_seconds" \
  --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges --tmpfs /data \
  --mount "type=bind,src=$HERE,dst=/work/private-ca,readonly" \
  --mount "type=bind,src=$scratch/mount,dst=/fixture,readonly" \
  --entrypoint /bin/sh "$IMAGE" -c '
    set -eu
    mkdir -p /run/cdr-private-ca
    cp /fixture/ca.pem /run/cdr-private-ca/source.pem
    chown 0:10001 /run/cdr-private-ca/source.pem
    chmod 0440 /run/cdr-private-ca/source.pem
    cp /fixture/unrelated.pem /run/cdr-private-ca/unrelated.pem
    cp /fixture/ca.pem /run/cdr-private-ca/unreadable.pem
    printf "%s\n" "-----BEGIN CERTIFICATE-----" "invalid" "-----END CERTIFICATE-----" > /run/cdr-private-ca/malformed.pem
    head -c 1048577 /dev/zero > /run/cdr-private-ca/oversized.pem
    i=0
    while [ "$i" -lt 65 ]; do
      cat /fixture/ca.pem >> /run/cdr-private-ca/65certs.pem
      i=$((i + 1))
    done
    chown 0:10001 /run/cdr-private-ca/*.pem
    chmod 0440 /run/cdr-private-ca/*.pem
    chmod 0400 /run/cdr-private-ca/unreadable.pem
    exec /usr/local/sbin/uzi-entrypoint /bin/sh -c "cd /app && exec node --import tsx --test --test-concurrency=1 --test-timeout=120000 /work/private-ca/fixture.test.ts"
  '
rc=$?
set +x
set -e
printf 'CONTAINER_EXIT=%s\n' "$rc"
if [ "$rc" = 124 ] || [ "$rc" = 137 ]; then
  printf 'FAIL: outer watchdog is not product timeout evidence\n'
fi
exit "$rc"
