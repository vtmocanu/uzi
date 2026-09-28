#!/usr/bin/env bash
# Offline fixture test of scripts/refresh-role-manifest.sh (#1851). Builds a temp
# repo and a temp upstream checkout per case, with a stub sync.py and a stub parity
# command, and drives the real script. Prints `N passed, N failed`; exits 1 on any
# failure. Needs bash, git, jq, yq and python3 (no PyYAML: the sync.py is a stub).
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/refresh-role-manifest.sh"
pass=0
fail=0
tmp_root="$(mktemp -d)"
trap 'rm -rf "$tmp_root"' EXIT

ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1${2:+: $2}"; }

# stub sync.py: `check` prints $STUB_DIR/check.txt and exits with $STUB_DIR/check.rc;
# `apply r...` backs each .claude/agents/<r>.md up to .pre-sync and replaces it with
# $STUB_DIR/new/<r>.md, like the real apply (tail handling is the real script's job).
write_stub_sync() {
  cat > "$1/sync.py" <<'PY'
import os, shutil, sys
stub = os.environ["STUB_DIR"]
args = sys.argv[1:]
agents = args[args.index("--agents") + 1]
cmd = [a for a in args if a in ("check", "apply")][0]
if cmd == "check":
    print(open(os.path.join(stub, "check.txt")).read(), end="")
    sys.exit(int(open(os.path.join(stub, "check.rc")).read().strip() or 0))
if os.path.exists(os.path.join(stub, "apply.fail")):
    open(os.path.join(agents, "coder.md"), "a").write("half-written\n")
    sys.exit(1)
for role in args[args.index("apply") + 1:]:
    path = os.path.join(agents, role + ".md")
    shutil.copy(path, path + ".pre-sync")
    shutil.copy(os.path.join(stub, "new", role + ".md"), path)
PY
}

# stub parity: match when every builtin equals the upstream product file.
write_stub_parity() {
  cat > "$1/parity.sh" <<'SH'
#!/usr/bin/env bash
for f in api/internal/agenttmpl/builtins/*.md; do
  n="$(basename "$f")"; [ "$n" = lead.md ] && continue
  cmp -s "$f" "$1/$n" || { echo "DIFFERS  ${n%.md}"; exit 0; }
done
echo "builtins match upstream (stub)"
SH
  chmod +x "$1/parity.sh"
}

# new_case NAME: a fresh repo (cwd) + upstream + stub dir, all at manifest v1, pin old.
new_case() {
  case_dir="$tmp_root/$1"
  repo="$case_dir/repo"; up="$case_dir/up"; stub="$case_dir/stub"
  mkdir -p "$repo/api/internal/agenttmpl/library" "$repo/api/internal/agenttmpl/builtins" \
    "$repo/.claude/agents" "$up/skills/agent-kit/agent-team" "$up/product-agents" "$stub/new"
  printf '{"upstream_sha":"old","synced":"2026-01-01","roles":{"coder":1,"reviewer":1}}\n' \
    > "$repo/api/internal/agenttmpl/library/manifest.json"
  for r in coder reviewer lead; do printf 'body %s v1\n' "$r" > "$repo/api/internal/agenttmpl/builtins/$r.md"; done
  for r in coder reviewer; do printf 'body %s v1\n' "$r" > "$up/product-agents/$r.md"; done
  printf 'agent coder old\n## For this repo\ntail\n' > "$repo/.claude/agents/coder.md"
  printf 'roles:\n  - name: coder\n    version: 1\n  - name: reviewer\n    version: 1\n' \
    > "$up/skills/agent-kit/agent-team/roles.yaml"
  printf 'coder         1          tail 5B    ok\n' > "$stub/check.txt"
  echo 0 > "$stub/check.rc"
  : > "$case_dir/allow.tsv"
  write_stub_sync "$stub"
  write_stub_parity "$stub"
  (cd "$repo" && git init -q && git add -A && git -c user.email=t@t -c user.name=t commit -qm init)
}

run_case() { # run_case SHA -> sets out (stderr+stdout) and GITHUB_OUTPUT file ghout
  ghout="$case_dir/ghout"; : > "$ghout"
  out="$(cd "$repo" && STUB_DIR="$stub" ROLE_SYNC_PY="$stub/sync.py" ROLE_PARITY_CMD="$stub/parity.sh" \
    ROLE_SYNC_ALLOWLIST="$case_dir/allow.tsv" GITHUB_OUTPUT="$ghout" "$script" "$up" "$1" 2>&1)" || {
    bad "$case_name" "script exited non-zero: $out"; return 1; }
}
outv() { grep -E "^$1=" "$ghout" | head -1 | cut -d= -f2; }
clean() { [ -z "$(cd "$repo" && git status --porcelain)" ]; }
man() { jq -r "$1" "$repo/api/internal/agenttmpl/library/manifest.json"; }

# 1. forward bump: body copied, version bumped, pin moved, parity ok.
case_name="forward bump copies the body and pins"; new_case bump
sed -i.bak 's/version: 1/version: 2/' "$up/skills/agent-kit/agent-team/roles.yaml" 2>/dev/null
printf 'body coder v2\n' > "$up/product-agents/coder.md"
if run_case newsha; then
  if [ "$(outv product_changed)" = true ] && [ "$(man .roles.coder)" = 2 ] && [ "$(man .upstream_sha)" = newsha ] \
     && cmp -s "$up/product-agents/coder.md" "$repo/api/internal/agenttmpl/builtins/coder.md" \
     && [ "$(outv parity_ok)" = true ]; then ok "$case_name"; else bad "$case_name" "$out"; fi
fi

# 2. backward version: nothing written, even with another role moving forward.
case_name="backward version writes nothing"; new_case backward
printf '{"upstream_sha":"old","synced":"x","roles":{"coder":3,"reviewer":1}}\n' > "$repo/api/internal/agenttmpl/library/manifest.json"
(cd "$repo" && git -c user.email=t@t -c user.name=t commit -qam m3)
printf 'roles:\n  - name: coder\n    version: 2\n  - name: reviewer\n    version: 5\n' > "$up/skills/agent-kit/agent-team/roles.yaml"
printf 'body reviewer v5\n' > "$up/product-agents/reviewer.md"
if run_case newsha; then
  if [ "$(outv warned)" = true ] && [ "$(outv changed)" = false ] && clean; then ok "$case_name"; else bad "$case_name" "$out"; fi
fi

# 3. a role missing upstream (no product file): nothing written.
case_name="missing upstream role writes nothing"; new_case missing
rm "$up/product-agents/reviewer.md"
sed -i.bak 's/version: 1/version: 2/' "$up/skills/agent-kit/agent-team/roles.yaml"
if run_case newsha; then
  if [ "$(outv warned)" = true ] && clean && grep -q 'no upstream product-agents/reviewer.md' <<<"$out"; then ok "$case_name"; else bad "$case_name" "$out"; fi
fi

# 4. same-version body change is a change.
case_name="same-version body change is a change"; new_case samever
printf 'body coder v1 reworded\n' > "$up/product-agents/coder.md"
if run_case old; then
  if [ "$(outv product_changed)" = true ] && grep -q 'bodies copied: coder' <<<"$out"; then ok "$case_name"; else bad "$case_name" "$out"; fi
fi

# 5. a new release commit alone moves the pin.
case_name="new release commit alone moves the pin"; new_case pinonly
if run_case newsha; then
  if [ "$(outv product_changed)" = true ] && [ "$(man .upstream_sha)" = newsha ]; then ok "$case_name"; else bad "$case_name" "$out"; fi
fi

# 6. nothing new: no change.
case_name="nothing new is no change"; new_case nochange
if run_case old; then
  if [ "$(outv changed)" = false ] && clean; then ok "$case_name"; else bad "$case_name" "$out"; fi
fi

# 7. roster-only drift: applied, dropped lines listed, backup removed, product untouched.
case_name="roster-only drift syncs and lists dropped lines"; new_case roster
printf 'coder         1 -> 2     tail 5B    LEGACY    body -1/+1 vs library\n' > "$stub/check.txt"; echo 1 > "$stub/check.rc"
printf 'agent coder new\n## For this repo\ntail\n' > "$stub/new/coder.md"
if run_case old; then
  if [ "$(outv roster_changed)" = true ] && [ "$(outv product_changed)" = false ] \
     && grep -q 'agent coder old' "$ghout" && [ ! -e "$repo/.claude/agents/coder.md.pre-sync" ]; then ok "$case_name"; else bad "$case_name" "$out"; fi
fi

# 8. allowlisted tester model is silent; another MODIFIED is reported.
case_name="allowlisted MODIFIED is silent, others reported"; new_case allow
printf 'tester\tmodel sonnet vs library opus\n' > "$case_dir/allow.tsv"
printf 'tester        14         tail 9B    MODIFIED  model sonnet vs library opus\nreviewer      2          tail 9B    MODIFIED  body -1/+1 vs library\n' > "$stub/check.txt"; echo 1 > "$stub/check.rc"
if run_case old; then
  if ! grep -q -- '- tester:' "$ghout" && grep -q -- '- reviewer: body' "$ghout"; then ok "$case_name"; else bad "$case_name" "$(cat "$ghout")"; fi
fi

# 9. tester with a body difference beyond the allowlisted model is still reported.
case_name="allowlist matches only the exact detail"; new_case allow2
printf 'tester\tmodel sonnet vs library opus\n' > "$case_dir/allow.tsv"
printf 'tester        14         tail 9B    MODIFIED  body -1/+1 vs library; model sonnet vs library opus\n' > "$stub/check.txt"; echo 1 > "$stub/check.rc"
if run_case old; then
  if grep -q -- '- tester: body' "$ghout"; then ok "$case_name"; else bad "$case_name" "$(cat "$ghout")"; fi
fi

# 10. BAD-FM stops the roster half.
case_name="BAD-FM stops the roster half"; new_case badfm
printf 'coder         1 -> 2     tail 5B    BAD-FM    unquoted colon\n' > "$stub/check.txt"; echo 1 > "$stub/check.rc"
if run_case old; then
  if [ "$(outv roster_changed)" = false ] && grep -q 'Roster sync stopped' "$ghout"; then ok "$case_name"; else bad "$case_name" "$out"; fi
fi

# 11. a failed apply restores the roster.
case_name="failed apply restores the roster"; new_case applyfail
printf 'coder         1 -> 2     tail 5B    STALE\n' > "$stub/check.txt"; echo 1 > "$stub/check.rc"; : > "$stub/apply.fail"
if run_case old; then
  if [ "$(outv roster_changed)" = false ] && clean && grep -q 'apply failed' "$ghout"; then ok "$case_name"; else bad "$case_name" "$out"; fi
fi

# 12. latest stable tag ignores prereleases and sorts by version.
case_name="latest stable tag ignores prereleases"; new_case tags
(cd "$up" && git init -q && git -c user.email=t@t -c user.name=t commit -q --allow-empty -m t \
  && git tag v0.9.0 && git tag v0.10.0 && git tag v0.11.0-rc.1 && git tag vnext)
got="$("$script" --latest-stable-tag "$up")"
if [ "$got" = v0.10.0 ]; then ok "$case_name"; else bad "$case_name" "got $got"; fi

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
