#!/usr/bin/env bash
# Offline registry/verifier fixtures for agent-image-assert.sh: the tag names a signed
# multi-arch index of exactly linux/amd64 + linux/arm64, and every child is the release.
set -euo pipefail
scripts="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export AIA_FIXTURE="$work"
version=0.86.0 short=abc1234 commit="abc1234$(printf '%033d' 0)"
cat > "$work/docker" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[ "$1 $2 $3" = 'buildx imagetools inspect' ]
d() { printf 'sha256:%064d' "$1"; }
case "$4" in
  *:0.86.0 | *:abc1234) [ "$6" = '{{.Manifest.Digest}}' ]; d 1;;
  *"@$(d 1)") [ "$5" = --raw ]; cat "$AIA_FIXTURE/index.json";;
  *"@$(d 2)") [ "$6" = '{{json .Image.Config}}' ]; cat "$AIA_FIXTURE/amd64.json";;
  *"@$(d 3)") [ "$6" = '{{json .Image.Config}}' ]; cat "$AIA_FIXTURE/arm64.json";;
  *) echo "unexpected ref $4" >&2; exit 1;;
esac
STUB
cat > "$work/cosign" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = verify ]
for a in "$@"; do last="$a"; done
! grep -qxF "$last" "$AIA_FIXTURE/unsigned" 2>/dev/null
STUB
chmod +x "$work/docker" "$work/cosign"
export PATH="$work:$PATH"
digest() { printf 'sha256:%064d' "$1"; }
index() { # platform:digest-n ...
  local m=""
  for p in "$@"; do m+="{\"platform\":{\"os\":\"linux\",\"architecture\":\"${p%%:*}\"},\"digest\":\"$(digest "${p#*:}")\"},"; done
  printf '{"manifests":[%s]}\n' "${m%,}" > "$work/index.json"
}
config() { # file stamp
  printf '{"Env":["UZI_AGENT_VERSION=%s"],"Labels":{"org.opencontainers.image.version":"%s","org.opencontainers.image.revision":"%s","io.github.vtmocanu.uzi.runtime-base":"ghcr.io/vtmocanu/uzi/agent-runtime-base@%s"}}\n' \
    "$2" "$2" "$commit" "$(digest 9)" > "$work/$1.json"
}
reset() {
  index amd64:2 arm64:3
  config amd64 "${version}+g${short}"; config arm64 "${version}+g${short}"
  : > "$work/unsigned"
}
cases=0
run() { "$scripts/agent-image-assert.sh" base "$version" "$short" "$commit" > "$work/out" 2>&1; }
accept() { run || { cat "$work/out" >&2; echo "FAIL: rejected $1" >&2; exit 1; }; cases=$((cases + 1)); echo "PASS: $1"; }
reject() { if run; then echo "FAIL: accepted $1" >&2; exit 1; fi; cases=$((cases + 1)); echo "PASS: rejects $1"; }

reset; accept 'signed amd64 + arm64 index, both children the release'
reset; printf '{"config":{}}\n' > "$work/index.json"; reject 'a single-arch manifest'
reset; index amd64:2; reject 'an index missing arm64'
reset; index amd64:2 arm64:3 riscv64:3; reject 'an index with an extra platform'
reset; config arm64 "${version}+g1234567"; reject 'an arm64 child with another stamp'
reset; digest 3 | sed 's|^|ghcr.io/vtmocanu/uzi/agent-base@|' > "$work/unsigned"; reject 'an unsigned arm64 child'
reset; digest 1 | sed 's|^|ghcr.io/vtmocanu/uzi/agent-base@|' > "$work/unsigned"; reject 'an unsigned index'
echo "cases=$cases passed=$cases"
