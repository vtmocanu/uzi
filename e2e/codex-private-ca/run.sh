#!/usr/bin/env bash
# Container proof against baked production sources, without host credentials.
set -euo pipefail

# One direct CLI child per call; SIGKILL at the deadline cannot be ignored.
# The launcher's EXIT trap separately attempts removal of its owned container.
bounded_child() {
  node - "$@" <<'NODE'
const { spawnSync } = require("node:child_process");
const [milliseconds, command, ...args] = process.argv.slice(2);
const timeout = Number(milliseconds);
if (!Number.isSafeInteger(timeout) || timeout <= 0 || !command) process.exit(2);
const result = spawnSync(command, args, { stdio: "inherit", timeout, killSignal: "SIGKILL" });
if (result.error?.code === "ETIMEDOUT") process.exit(124);
if (result.error) console.error(result.error.message);
process.exit(result.status ?? 1);
NODE
}

fixture_hashes() {
  node - "$@" <<'NODE'
const { createHash } = require("node:crypto");
const { readFileSync } = require("node:fs");
for (const path of process.argv.slice(2)) {
  console.log(createHash("sha256").update(readFileSync(path)).digest("hex") + "  " + path);
}
NODE
}

# Permit focused helper tests without launching Docker or creating fixture files.
if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  return 0
fi
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
cd "$REPO"
if [[ -n "${FIXTURE_IMAGE+x}" || -n "${FIXTURE_EXPECTED_ID+x}" ]]; then
  : "${FIXTURE_IMAGE:?explicit image requires FIXTURE_IMAGE and FIXTURE_EXPECTED_ID}"
  : "${FIXTURE_EXPECTED_ID:?explicit image requires FIXTURE_IMAGE and FIXTURE_EXPECTED_ID}"
fi
scratch=$(mktemp -d "$REPO/.uzi/scratch/private-ca.XXXXXX")
name="cdr-private-ca-$$"
cleanup() {
  bounded_child 20000 docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT
revision=$(git rev-parse HEAD)
build_started=$(date +%s)
if [[ -n "${FIXTURE_IMAGE+x}" ]]; then
  IMAGE=$FIXTURE_IMAGE
  EXPECTED=$FIXTURE_EXPECTED_ID
else
  # Build this checkout with ordinary Docker content caching; run by immutable ID.
  rc=0
  bounded_child 1200000 docker build \
    -f agent/templates/base/Dockerfile --build-arg "UZI_SRC_SHA=$revision" \
    --iidfile "$scratch/image-id" . || rc=$?
  printf 'BUILD_DURATION_SECONDS=%s BUILD_EXIT=%s\n' "$(($(date +%s) - build_started))" "$rc"
  if [[ "$rc" -ne 0 ]]; then
    exit "$rc"
  fi
  IMAGE=$(cat "$scratch/image-id")
  EXPECTED=$IMAGE
fi
actual=$(docker image inspect "$IMAGE" --format '{{.Id}}')
printf 'image=%s id=%s revision=%s\n' "$IMAGE" "$actual" "$revision"
test "$actual" = "$EXPECTED"
fixture_hashes "$HERE/run.sh" "$HERE/fixture.test.ts" "$HERE/README.md"
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
bounded_child 240000 docker run --rm --name "$name" --network none \
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
    cp /fixture/ca.pem /run/cdr-private-ca/exact-limit.pem
    ca_bytes=$(wc -c < /fixture/ca.pem)
    padding=$((1048576 - ca_bytes))
    test "$padding" -gt 0
    dd if=/dev/zero bs=1 count="$padding" 2>/dev/null | tr "\\000" " " >> /run/cdr-private-ca/exact-limit.pem
    cp /run/cdr-private-ca/exact-limit.pem /run/cdr-private-ca/oversized.pem
    printf " " >> /run/cdr-private-ca/oversized.pem
    i=0
    while [ "$i" -lt 65 ]; do
      cat /fixture/ca.pem >> /run/cdr-private-ca/65certs.pem
      i=$((i + 1))
    done
    chown 0:10001 /run/cdr-private-ca/*.pem
    chmod 0440 /run/cdr-private-ca/*.pem
    chmod 0400 /run/cdr-private-ca/unreadable.pem
    exec /usr/local/sbin/uzi-entrypoint /bin/sh -c "cd /app && exec node --import tsx --test --test-reporter=tap --test-concurrency=1 --test-timeout=120000 /work/private-ca/fixture.test.ts"
  '
rc=$?
set +x
set -e
printf 'RUN_DURATION_SECONDS=%s\n' "$(($(date +%s) - started_seconds))"
printf 'CONTAINER_EXIT=%s\n' "$rc"
if [ "$rc" = 124 ] || [ "$rc" = 137 ]; then
  printf 'FAIL: outer watchdog is not product timeout evidence\n'
fi
exit "$rc"
