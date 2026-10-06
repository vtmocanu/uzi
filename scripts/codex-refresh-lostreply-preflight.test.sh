#!/usr/bin/env bash
# Exercise the real Task target, including its local includes, without Node,
# dependency installation or a database. Optional argument: an exported repo root
# (with Taskfile.yml and homebrew-tap.yml) for precondition mutation testing.
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
SOURCE="$(CDPATH='' cd -- "${1:-$ROOT}" && pwd)"
# Resolve before fake PATH shadows task: nested setup calls must hit the stub.
TASK="$(command -v task)"
[[ "$TASK" = /* && -x "$TASK" ]] || {
  echo "FAIL: task must resolve to an executable absolute path" >&2
  exit 1
}
mkdir -p "$ROOT/.uzi/scratch"
TMP="$(mktemp -d "$ROOT/.uzi/scratch/codex-lostreply-preflight.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
export TMPDIR="$TMP"
mkdir -p "$TMP/repo/bin" "$TMP/repo/scripts" "$TMP/repo/e2e" "$TMP/repo/agent" "$TMP/home"
cp "$SOURCE/Taskfile.yml" "$SOURCE/homebrew-tap.yml" "$TMP/repo/"

cat > "$TMP/repo/bin/uname" <<'STUB'
#!/bin/sh
[ "$*" = "-s" ] || exit 98
printf '%s\n' "$FAKE_OS"
STUB
cat > "$TMP/repo/bin/node" <<'STUB'
#!/bin/sh
printf 'node\n' >> "$SIDE_EFFECTS"
printf 'first-node-sentinel\n' > "$NODE_ARTIFACT"
echo 'first-node-sentinel' >&2
exit 97
STUB
cat > "$TMP/setup-stub" <<'STUB'
#!/bin/sh
printf '%s %s\n' "${0##*/}" "$*" >> "$SIDE_EFFECTS"
echo 'unexpected-setup-sentinel' >&2
exit 98
STUB
# Every later setup seam records and fails; none can invoke the real tools.
for tool in npm task mkdir docker go; do
  cp "$TMP/setup-stub" "$TMP/repo/bin/$tool"
done
cp "$TMP/setup-stub" "$TMP/repo/scripts/deps-check-gate.sh"
cp "$TMP/setup-stub" "$TMP/repo/e2e/run-store-it.sh"
chmod +x "$TMP/repo/bin/"* "$TMP/repo/scripts/deps-check-gate.sh" "$TMP/repo/e2e/run-store-it.sh"

target=test:codex-refresh-lostreply-e2e
failures=0
# Three independent cases, one invocation each, no retries. A failed assertion
# records the case failure and does not prevent the other OS cases from running.
for os in Darwin FreeBSD Linux; do
  effects="$TMP/$os.effects"
  artifact="$TMP/$os.node"
  output="$TMP/$os.output"
  : > "$effects"
  rc=0
  (
    cd "$TMP/repo"
    env -i PATH="$TMP/repo/bin:/usr/bin:/bin" HOME="$TMP/home" TMPDIR="$TMP" \
      FAKE_OS="$os" SIDE_EFFECTS="$effects" NODE_ARTIFACT="$artifact" \
      "$TASK" --taskfile "$TMP/repo/Taskfile.yml" "$target"
  ) > "$output" 2>&1 || rc=$?
  ok=1
  [[ "$rc" -ne 0 ]] || ok=0
  if [[ "$os" = Linux ]]; then
    [[ "$(cat "$effects")" = node ]] || ok=0
    [[ -f "$artifact" ]] && [[ "$(cat "$artifact")" = first-node-sentinel ]] || ok=0
    grep -Fq 'first-node-sentinel' "$output" || ok=0
    # Prove the Node version check was reached, not just command lookup.
    grep -Fq 'exit status 97' "$output" || ok=0
  else
    grep -Fq "$target is Linux-only" "$output" || ok=0
    grep -Fq 'Linux-only session-copy fixture' "$output" || ok=0
    [[ ! -s "$effects" && ! -e "$artifact" ]] || ok=0
  fi
  [[ ! -e "$TMP/repo/.uzi/scratch" ]] || ok=0
  if [[ "$ok" = 1 ]]; then
    printf 'PASS: %s preflight (Task exit %s)\n' "$os" "$rc"
  else
    printf 'FAIL: %s preflight (Task exit %s)\n' "$os" "$rc" >&2
    cat "$output" "$effects" >&2
    failures=$((failures + 1))
  fi
done
printf 'cases=3 passed=%s\n' "$((3 - failures))"
[[ "$failures" -eq 0 ]]
