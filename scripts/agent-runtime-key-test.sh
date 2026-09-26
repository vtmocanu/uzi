#!/usr/bin/env bash
# agent-runtime-key-test.sh -- offline fixture test for scripts/agent-runtime-key.sh (#1720).
#
# Builds a throwaway git repo with a two-stage template Dockerfile and drives the real
# script against it. Asserts the three properties release.yml relies on:
#   * STABLE: the same commit yields the same key;
#   * COMPLETE: changing any runtime-stage input changes the key (stage text, ARG default,
#     parent digest, a copied file's content / mode / symlink target, the dockerignore, the
#     platform), while release-stage-only changes do not;
#   * FAIL-CLOSED: every form the parser cannot account for, and any input that differs
#     from HEAD (staged, unstaged, untracked, ignored), exits 2 instead of printing a key.
# Watch it fail on a broken script: drop a class from the key material and its row reddens.
set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/agent-runtime-key.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/agent-runtime-key-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   $*"; }
bad() { fail=$((fail + 1)); echo "FAIL $*"; }

DIGEST_A="sha256:$(printf 'a%.0s' $(seq 64))"
DIGEST_B="sha256:$(printf 'b%.0s' $(seq 64))"

write_dockerfile() {  # $1 = runtime-stage body, $2 = release-stage body
  printf 'FROM node:24-alpine@%s AS runtime\n%s\nFROM runtime AS release\n%s\n' "$PARENT" "$1" "$2" \
    > agent/templates/t/Dockerfile
}

# shellcheck disable=SC2016  # literal Dockerfile text; the \${...} are Docker's, not ours
RUNTIME_OK='ARG TARGETARCH
ARG TOOL_VERSION=1.0
RUN echo install "${TOOL_VERSION}"
COPY agent/lock.txt /tmp/lock.txt
COPY --chmod=0755 agent/tool.sh /usr/local/bin/tool
COPY agent/supervisor /build/supervisor'
# shellcheck disable=SC2016  # literal Dockerfile text
RELEASE_OK='COPY agent/src ./src
ARG UZI_AGENT_VERSION=
ENV UZI_AGENT_VERSION=${UZI_AGENT_VERSION#v}'

reset_fixture() {
  rm -rf "$TMP/repo"
  mkdir -p "$TMP/repo"
  cd "$TMP/repo"
  git init -q
  git config user.email t@example.com
  git config user.name t
  mkdir -p agent/templates/t agent/supervisor agent/src
  PARENT="$DIGEST_A"
  write_dockerfile "$RUNTIME_OK" "$RELEASE_OK"
  printf '.git\n**/node_modules\n' > agent/templates/t/Dockerfile.dockerignore
  printf 'lock-1\n' > agent/lock.txt
  printf '#!/bin/sh\necho tool\n' > agent/tool.sh
  printf 'package main\n' > agent/supervisor/main.go
  ln -s main.go agent/supervisor/link.go
  printf 'console.log(1)\n' > agent/src/index.ts
  printf 'agent/supervisor/build-out/\n' > .gitignore
  git add -A
  git commit -q -m fixture
}

commit_all() { git add -A && git commit -q -m change; }
key() { "$SCRIPT" t "${1:-linux/amd64}"; }

expect_changed() {  # $1 = label, $2 = key before
  local after
  if after="$(key 2>&1)" && [ "$after" != "$2" ]; then ok "key changes: $1"; else bad "key did not change: $1 ($after)"; fi
}
expect_same() {
  local after
  if after="$(key 2>&1)" && [ "$after" = "$2" ]; then ok "key unchanged: $1"; else bad "key changed or failed: $1 ($after)"; fi
}
expect_refused() {  # $1 = label; runs the script, requires exit 2
  local rc=0
  key >/dev/null 2>&1 || rc=$?
  if [ "$rc" -eq 2 ]; then ok "refused: $1"; else bad "not refused (rc=$rc): $1"; fi
}

# --- stability -----------------------------------------------------------------------
reset_fixture
k0="$(key)"
case "$k0" in v1-[0-9a-f]*) ok "key format ($k0)";; *) bad "key format ($k0)";; esac
if [ "$(key)" = "$k0" ]; then ok "stable across runs"; else bad "unstable across runs"; fi

# --- completeness: each runtime input class changes the key -----------------------------
reset_fixture; k0="$(key)"
write_dockerfile "${RUNTIME_OK/echo install/echo installing}" "$RELEASE_OK"; commit_all
expect_changed "RUN line" "$k0"

reset_fixture; k0="$(key)"
write_dockerfile "${RUNTIME_OK/TOOL_VERSION=1.0/TOOL_VERSION=1.1}" "$RELEASE_OK"; commit_all
expect_changed "ARG default" "$k0"

reset_fixture; k0="$(key)"
PARENT="$DIGEST_B"; write_dockerfile "$RUNTIME_OK" "$RELEASE_OK"; commit_all
expect_changed "parent image digest" "$k0"

reset_fixture; k0="$(key)"
printf 'lock-2\n' > agent/lock.txt; commit_all
expect_changed "copied file content" "$k0"

reset_fixture; k0="$(key)"
chmod +x agent/lock.txt; commit_all
expect_changed "copied file mode" "$k0"

reset_fixture; k0="$(key)"
printf 'package other\n' > agent/supervisor/other.go; commit_all
expect_changed "file added under a copied directory" "$k0"

reset_fixture; k0="$(key)"
rm agent/supervisor/link.go; ln -s other.go agent/supervisor/link.go; commit_all
expect_changed "symlink target under a copied directory" "$k0"

reset_fixture; k0="$(key)"
printf '.git\n' > agent/templates/t/Dockerfile.dockerignore; commit_all
expect_changed "dockerignore" "$k0"

reset_fixture; k0="$(key)"
if [ "$(key linux/arm64)" != "$k0" ]; then ok "key changes: platform"; else bad "key did not change: platform"; fi

# --- release-stage-only changes keep the key ------------------------------------------
reset_fixture; k0="$(key)"
printf 'console.log(2)\n' > agent/src/index.ts; commit_all
expect_same "agent/src change (release stage only)" "$k0"

reset_fixture; k0="$(key)"
write_dockerfile "$RUNTIME_OK" "$RELEASE_OK
LABEL x=y"; commit_all
expect_same "release-stage Dockerfile line" "$k0"

# --- fail-closed forms ------------------------------------------------------------------
refuse_runtime() {  # $1 = label, $2 = runtime body
  reset_fixture
  write_dockerfile "$2" "$RELEASE_OK"; commit_all
  expect_refused "$1"
}
refuse_runtime "ADD" "$RUNTIME_OK
ADD agent/lock.txt /x"
refuse_runtime "COPY --from" "$RUNTIME_OK
COPY --from=builder /a /b"
refuse_runtime "COPY --from hidden on a continuation line" "$RUNTIME_OK
COPY \\
  --from=builder /a /b"
refuse_runtime "JSON-form COPY" "$RUNTIME_OK
COPY [\"agent/lock.txt\", \"/x\"]"
refuse_runtime "heredoc COPY" "$RUNTIME_OK
COPY <<EOF /x
hi
EOF"
refuse_runtime "glob COPY" "$RUNTIME_OK
COPY agent/*.txt /x/"
refuse_runtime "variable COPY" "$RUNTIME_OK
COPY \${SRC} /x"
refuse_runtime "RUN --mount" "$RUNTIME_OK
RUN --mount=type=bind,source=agent,target=/a true"
refuse_runtime "ARG without default" "$RUNTIME_OK
ARG SOMETHING"
refuse_runtime "second FROM in the runtime stage" "$RUNTIME_OK
FROM alpine@$DIGEST_B AS other"
refuse_runtime "untracked COPY source" "$RUNTIME_OK
COPY agent/missing.txt /x"
refuse_runtime "COPY of the whole context (.)" "$RUNTIME_OK
COPY . /opt/src"
refuse_runtime "COPY of the whole context (./)" "$RUNTIME_OK
COPY ./ /opt/src"
refuse_runtime "non-canonical COPY path (./ prefix)" "$RUNTIME_OK
COPY ./agent/lock.txt /x"
refuse_runtime "non-canonical COPY path (/./ segment)" "$RUNTIME_OK
COPY agent/./lock.txt /x"
refuse_runtime "non-canonical COPY path (//)" "$RUNTIME_OK
COPY agent//lock.txt /x"

reset_fixture
printf 'ARG GLOBAL=1\n%s\n' "$(cat agent/templates/t/Dockerfile)" > agent/templates/t/Dockerfile; commit_all
expect_refused "global ARG before FROM"

reset_fixture
gsed -i 's/@sha256:[0-9a-f]*//' agent/templates/t/Dockerfile 2>/dev/null || sed -i 's/@sha256:[0-9a-f]*//' agent/templates/t/Dockerfile
commit_all
expect_refused "parent image not pinned by digest"

reset_fixture
printf 'FROM node:24-alpine@%s AS runtime\n%s\n' "$PARENT" "$RUNTIME_OK" > agent/templates/t/Dockerfile; commit_all
expect_refused "missing release marker"

# --- inputs must equal HEAD -------------------------------------------------------------
reset_fixture; printf 'lock-dirty\n' > agent/lock.txt
expect_refused "unstaged change to a COPY source"
reset_fixture; printf 'lock-staged\n' > agent/lock.txt; git add agent/lock.txt
expect_refused "staged change to a COPY source"
reset_fixture; printf 'x\n' > agent/supervisor/untracked.go
expect_refused "untracked file under a copied directory"
reset_fixture; mkdir -p agent/supervisor/build-out; printf 'x\n' > agent/supervisor/build-out/bin
expect_refused "ignored file under a copied directory"
reset_fixture; printf '# edit\n' >> agent/templates/t/Dockerfile
expect_refused "uncommitted Dockerfile edit"
reset_fixture; printf 'x\n' >> agent/templates/t/Dockerfile.dockerignore
expect_refused "uncommitted dockerignore edit"

reset_fixture
if "$SCRIPT" t linux/386 >/dev/null 2>&1; then bad "unknown platform accepted"; else ok "refused: unknown platform"; fi

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
