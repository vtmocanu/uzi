#!/usr/bin/env bash
# hermetic-git-config.test.sh: tests never read host git config.
#
# On macOS a global `core.fsmonitor=true` spawns a detached `git fsmonitor--daemon`
# per fixture repo that outlives the test (hundreds accumulated on one laptop).
# Linux git has no daemon, so this suite plants a hostile global config whose
# core.fsmonitor is a RECORDER hook (hooks run on every platform) and checks:
#   1. control: the recorder fires for an unprotected repo, so the probe is not blind;
#   2. a fixture script that exports the pair keeps it silent;
#   3. a Task test command overrides an ALREADY-EXPORTED hostile GIT_CONFIG_GLOBAL
#      (fake `node` on PATH, so no script-level export can mask the Task prefix);
#   4. wiring: every repo-creating test script exports the pair (minus the listed
#      exemptions) and the prefixed targets still carry it under `task --dry`.
# Coverage boundary: 4 is a convention check over test-named shell/python files,
# not shell analysis; a git call through an unusual wrapper can evade it.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
mkdir -p .uzi/scratch
W="$(mktemp -d "$ROOT/.uzi/scratch/hermetic-git.XXXXXX")"
trap 'rm -rf "$W"' EXIT

FAILS=0
ok() { printf '  ok   %s\n' "$1"; }
bad() { FAILS=$((FAILS + 1)); printf '  FAIL %s\n' "$1"; }

# Attributable results: drop inherited config and repo-location overrides.
while read -r v; do unset "$v"; done < <(compgen -e | grep -E '^GIT_(CONFIG|DIR$|WORK_TREE$|INDEX_FILE$|COMMON_DIR$)' || true)

HOSTILE="$W/home"
mkdir -p "$HOSTILE"
REC="$W/fired"
cat > "$W/recorder" <<EOF
#!/bin/sh
echo "\$PWD" >> "$REC"
exit 1
EOF
chmod +x "$W/recorder"
printf '[core]\n\tfsmonitor = %s\n[user]\n\tname = t\n\temail = t@example.com\n' "$W/recorder" > "$HOSTILE/.gitconfig"
: > "$REC"
hostile() { HOME="$HOSTILE" XDG_CONFIG_HOME="$HOSTILE/.config" "$@"; }

# 1. Positive control.
r="$W/control"
hostile git init -q "$r"
echo x > "$r/f"
hostile git -C "$r" add f
hostile git -C "$r" status --porcelain > /dev/null
if [ -s "$REC" ]; then ok "control: a hostile global core.fsmonitor reaches an unprotected repo"
else bad "control: the recorder never fired, so the probes below prove nothing"; fi

# 2. A fixture script under the hostile HOME.
: > "$REC"
if hostile bash scripts/check-guidance-size.test.sh > "$W/script.log" 2>&1; then
  if [ -s "$REC" ]; then bad "check-guidance-size.test.sh read host git config (fired in $(head -1 "$REC"))"
  else ok "a fixture script ignores host git config"; fi
else
  bad "check-guidance-size.test.sh failed under a hostile HOME: $(tail -3 "$W/script.log")"
fi

# 3. A Task command beats an already-exported hostile GIT_CONFIG_GLOBAL.
: > "$REC"
mkdir -p "$W/bin"
cat > "$W/bin/node" <<EOF
#!/bin/sh
printf '%s\n' "\${GIT_CONFIG_GLOBAL-unset}" > "$W/seen"
d="\$(mktemp -d "$W/node.XXXXXX")"
git init -q "\$d" && git -C "\$d" status --porcelain > /dev/null
EOF
chmod +x "$W/bin/node"
if HOME="$HOSTILE" GIT_CONFIG_GLOBAL="$HOSTILE/.gitconfig" PATH="$W/bin:$PATH" \
    task test:forge-fake > "$W/task.log" 2>&1; then
  seen="$(cat "$W/seen" 2>/dev/null || echo missing)"
  if [ "$seen" = /dev/null ]; then ok "task test:forge-fake overrides an exported GIT_CONFIG_GLOBAL"
  else bad "task test:forge-fake ran its command with GIT_CONFIG_GLOBAL=$seen"; fi
  if [ -s "$REC" ]; then bad "task test:forge-fake's command read host git config"
  else ok "task test:forge-fake's command ignores host git config"; fi
else
  bad "task test:forge-fake failed with a fake node: $(tail -3 "$W/task.log")"
fi

# 4a. Every repo-creating test script exports the pair.
# Exempt: runs its repos inside a Linux container / drives a fake git / this file.
EXEMPT='^(\.agents/skills/uzi-lander/scripts/test-linux\.sh|scripts/devbox-update\.test\.sh|scripts/hermetic-git-config\.test\.sh)$'
# perl, not grep -P: BSD grep has no -P, and a matcher that errors matches nothing.
creates_repo() { perl -ne '$m = 1 if /^\s*[^#]*\bgit\w*\b.*\b(init|clone)\b/; END { exit($m ? 0 : 1) }' "$1"; }
missing=""
scanned=0
while read -r f; do
  [[ "$f" =~ $EXEMPT ]] && continue
  creates_repo "$f" || continue
  scanned=$((scanned + 1))
  { grep -q -E "GIT_CONFIG_GLOBAL=['\"]?/dev/null" "$f" &&
    grep -q -E "GIT_CONFIG_(SYSTEM=['\"]?/dev/null|NOSYSTEM=['\"]?1)" "$f"; } || missing="$missing $f"
done < <(git ls-files '*.sh' '*.py' | grep -E '(^|[/._-])test([._-]|$)')
# 24 repo-creating test scripts when this landed; far fewer means the matcher broke.
if [ "$scanned" -lt 20 ]; then bad "only $scanned repo-creating test scripts found; the matcher is broken"
elif [ -z "$missing" ]; then ok "all $scanned repo-creating test scripts isolate global and system git config"
else bad "repo-creating test scripts without global+system isolation:$missing"; fi

# 4b. Each prefixed Task command keeps the pair directly in front of it. A per-target
# count is not enough: test:agent echoes two prefixed commands, so losing one passes.
PAIR='GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null'
# `task --dry` still evaluates preconditions; where one is unmet on this host (CI's
# lint-repo job lacks a target's toolchain), read the target's own Taskfile block.
own_block() { awk -v t="  $1:" '$0 == t { f = 1; next } f && /^  [a-z][a-z0-9:-]+:$/ { exit } f' Taskfile.yml; }
while IFS='|' read -r t cmd; do
  if ! dry="$(task --dry "$t" 2>&1)"; then
    case "$dry" in
      *"precondition not met"*) dry="$(own_block "$t")" ;;
      *) bad "task --dry $t failed: $(printf '%s\n' "$dry" | tail -2)"; continue ;;
    esac
  fi
  # Capture first: `grep -q` closing the pipe early would SIGPIPE under pipefail.
  if printf '%s\n' "$dry" | grep -F "$PAIR $cmd" > /dev/null; then ok "$t: $cmd"
  else bad "$t: '$cmd' lost its $PAIR prefix"; fi
done <<'EOF'
test:api|go test -race -count=1
test:agent|../scripts/real-procfs-skip-check.sh ../scripts/tmpdir-leak-guard.sh npm test
test:codex-m4|node --import tsx --test --test-concurrency=1
check:codex-m4-receipts|node --import tsx ../e2e/codex-m4/run-receipts.ts
test:forge-fake|node --test e2e/forge-fake/issue-comments.test.mjs
test:forge-fake-contract|node e2e/forge-fake/operation-contracts.test.mjs
test:checkpoint-chain-bridge|go test -buildvcs=false -count=1 -race -v
EOF

[ "$FAILS" -eq 0 ] || { echo "hermetic-git-config: $FAILS failure(s)" >&2; exit 1; }
echo "hermetic-git-config: all checks passed"
