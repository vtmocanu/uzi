#!/usr/bin/env bash
# Hermetic regression: owned-gates.sh reports a ci_fix parked at its plan gate on a claimed,
# watcher-less PR. A ci_fix on a parked PR (matched only by its "Fix CI: <branch>" title)
# waited 11 hours unseen because nothing polled claimed PRs that had no watcher.
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/owned-gates.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  "repo list --json") cat "$WORK/repos.json" ;;
  "run list --json") [ -f "$WORK/uzi-down" ] && exit 1; cat "$WORK/runs.json" ;;
  "run get "*" --json") id="${3}"; jq -e --arg id "$id" '.[] | select(.id == $id)' "$WORK/runs.json" || { echo '{"error":"not found"}'; exit 4; } ;;
  *) echo "unexpected uzi call: $*" >&2; exit 99 ;;
esac
STUB
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  "pr view 42 --repo test/repo --json headRefName --jq .headRefName") echo "agent/issue-7" ;;
  "pr view 43 --repo test/repo --json headRefName --jq .headRefName") echo "agent/issue-8" ;;
  *) echo "unexpected gh call: $*" >&2; exit 99 ;;
esac
STUB
cat > "$WORK/claims" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  whoami) echo "SESSION_NAME=me"; echo "SESSION_UUID=u-me"; echo "SESSION_KIND=claude" ;;
  list) cat "$WORK/claims.json" ;;
  *) exit 99 ;;
esac
STUB
chmod +x "$WORK/bin/uzi" "$WORK/bin/gh" "$WORK/claims"
export PATH="$WORK/bin:$PATH" WORK OWNED_GATES_CLAIMS="$WORK/claims"
echo '[{"id":"r1","path_with_namespace":"test/repo"},{"id":"r2","path_with_namespace":"test/other"}]' > "$WORK/repos.json"

jq -n '[
  {key:"#42", repo:"test/repo", owner_uuid:"u-me", claimed_at:"2026-01-05T00:00:00Z"},
  {key:"run-r9", repo:"test/repo", owner_uuid:"u-me"},
  {key:"#43", repo:"test/repo", owner_uuid:"u-other"}]' > "$WORK/claims.json"

runs() { jq -n "$1" > "$WORK/runs.json"; }
run_it() { set +e; out="$("$SCRIPT" test/repo 2>&1)"; rc=$?; set -e; }

# 1. The incident: a ci_fix linked only by its title, parked at a plan gate on claimed #42.
runs '[{id:"cf1",repo_id:"r1",kind:"ci_fix",status:"awaiting_approval",branch:null,mr_iid:null,
        issue_title:"Fix CI: agent/issue-7 pipeline #123",created_at:"2026-01-01T00:00:00Z"},
       {id:"r9",repo_id:"r1",kind:"issue",status:"running",created_at:"t"}]'
run_it
[ "$rc" -eq 1 ] || fail "ci_fix gate: rc=$rc out=$out"
grep -qF 'GATE key=#42 run=cf1 kind=ci_fix status=awaiting_approval' <<<"$out" || fail "ci_fix gate line missing: $out"

# 2. mr_rework by mr_iid, and a claimed run key; other sessions' PRs and terminal runs ignored.
runs '[{id:"mr1",repo_id:"r1",kind:"mr_rework",status:"awaiting_input",mr_iid:42,created_at:"t"},
       {id:"r9",repo_id:"r1",kind:"issue",status:"paused",created_at:"t"},
       {id:"x1",repo_id:"r1",kind:"ci_fix",status:"awaiting_approval",issue_title:"Fix CI: agent/issue-8 pipeline #1",created_at:"t"},
       {id:"d1",repo_id:"r1",kind:"ci_fix",status:"completed",issue_title:"Fix CI: agent/issue-7 pipeline #2",created_at:"t"}]'
run_it
[ "$rc" -eq 1 ] || fail "mixed: rc=$rc out=$out"
grep -qF 'run=mr1 kind=mr_rework status=awaiting_input' <<<"$out" || fail "mr_rework missing: $out"
grep -qF 'key=run-r9 run=r9' <<<"$out" || fail "run key missing: $out"
! grep -qE 'run=(x1|d1)' <<<"$out" || fail "matched another owner's PR or a terminal run: $out"
grep -qF 'COUNT=2' <<<"$out" || fail "count: $out"

# 3. A prefix branch must not match (agent/issue-7 vs agent/issue-70).
runs '[{id:"p1",repo_id:"r1",kind:"ci_fix",status:"awaiting_approval",issue_title:"Fix CI: agent/issue-70 pipeline #9",created_at:"t"},
       {id:"r9",repo_id:"r1",kind:"issue",status:"running",created_at:"t"}]'
run_it
[ "$rc" -eq 0 ] || fail "prefix branch matched: rc=$rc out=$out"
grep -qF 'RESULT=none' <<<"$out" || fail "none: $out"

# 4. Inferred PR matches require this repo's exact id: a repo-less or other-repo run on the
#    same branch name is not ours.
runs '[{id:"n1",kind:"ci_fix",status:"awaiting_approval",issue_title:"Fix CI: agent/issue-7 pipeline #3",created_at:"t"},
       {id:"o1",repo_id:"r2",kind:"mr_rework",status:"awaiting_input",mr_iid:42,created_at:"t"},
       {id:"r9",repo_id:"r1",kind:"issue",status:"running",created_at:"t"}]'
run_it
[ "$rc" -eq 0 ] || fail "repo-less/other-repo matched: rc=$rc out=$out"

# 5b. Malformed lookups are unknown, never none: a run row without status breaks the matcher,
#     a claimed run without status, and a sole repo row with a null id.
runs '[{id:"m1",repo_id:"r1",kind:"ci_fix",status:{"x":1},mr_iid:42,created_at:"t"},
       {id:"r9",repo_id:"r1",kind:"issue",status:"running",created_at:"t"}]'
run_it
[ "$rc" -eq 3 ] || fail "malformed row: rc=$rc out=$out"
runs '[{id:"r9",repo_id:"r1",kind:"issue",created_at:"t"}]'
run_it
[ "$rc" -eq 3 ] || fail "run claim without status: rc=$rc out=$out"
echo '[{"id":null,"path_with_namespace":"test/repo"}]' > "$WORK/repos.json"
runs '[{id:"r9",repo_id:"r1",kind:"issue",status:"running",created_at:"t"}]'
run_it
[ "$rc" -eq 3 ] || fail "null repo id: rc=$rc out=$out"
echo '[{"id":"r1","path_with_namespace":"test/repo"}]' > "$WORK/repos.json"

# 6. An ambiguous repo lookup fails closed.
echo '[{"id":"r1","path_with_namespace":"test/repo"},{"id":"r3","path_with_namespace":"test/repo"}]' > "$WORK/repos.json"
run_it
[ "$rc" -eq 3 ] || fail "ambiguous repo: rc=$rc out=$out"
echo '[{"id":"r1","path_with_namespace":"test/repo"}]' > "$WORK/repos.json"

# 7. No claims for this repo: none, without touching uzi.
jq -n '[{key:"#43", repo:"test/repo", owner_uuid:"u-other"}]' > "$WORK/claims.json"
touch "$WORK/uzi-down"
run_it
{ [ "$rc" -eq 0 ] && grep -qF 'RESULT=none' <<<"$out"; } || fail "no claims: rc=$rc out=$out"
rm -f "$WORK/uzi-down"
jq -n '[{key:"#42", repo:"test/repo", owner_uuid:"u-me", claimed_at:"2026-01-05T00:00:00Z"},{key:"run-r9", repo:"test/repo", owner_uuid:"u-me"}]' > "$WORK/claims.json"

# 8. A failed lookup is exit 3, never "none".
touch "$WORK/uzi-down"
run_it
[ "$rc" -eq 3 ] || fail "uzi down: rc=$rc out=$out"
! grep -qF 'RESULT=none' <<<"$out" || fail "uzi down read as none: $out"
rm -f "$WORK/uzi-down"

echo "owned-gates.test.sh: ok"
