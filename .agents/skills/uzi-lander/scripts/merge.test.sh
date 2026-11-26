#!/usr/bin/env bash
# Hermetic regression: merge.sh --confirm-only reconciles a merge that landed OUT OF BAND.
# On PR #1510 the classifier refused the in-script `gh pr merge --admin`; the user ran it via
# a `!` line, so merge.sh's own confirm/trail block never executed and NO MERGE_SHA or trail
# was ever produced. --confirm-only writes that owed terminal evidence from the one place that
# knows the true merge SHA. The merge releases the live claim but preserves the trail through
# post-merge CI, so the final status line still contains the complete landing history.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/merge.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

# API listing caps cannot establish completeness, even when the response is valid JSON.
# Keep these tests hermetic and avoid placing a large JSON fixture in the environment.
. "$HERE/lib/migrations.sh"
if (gh() { jq -cn '[range(0;3000)|{filename:"README.md",status:"modified"}]'; }; migration_pr_files test/repo 42 >/dev/null); then
  fail "capped PR files were accepted as complete"
fi
base_limit=$(jq -cn '[range(0;1000)|{name:"00001_base.sql"}]')
if migration_report '[]' "$base_limit" >/dev/null; then fail "capped base directory was accepted as complete"; fi
unset base_limit
# A rename or copy introduces its destination version just as an added file does.
for status in renamed copied; do
  files=$(jq -cn --arg s "$status" '[{filename:"api/internal/store/migrations/00371_vault.sql",status:$s,previous_filename:"api/internal/store/migrations/99999_source.sql"}]')
  report=$(migration_report "$files" '[{"name":"00372_base.sql"}]')
  [ "$(printf '%s' "$report" | jq '.collisions|length')" -eq 1 ] || fail "$status destination bypassed migration check"
done
unset files report

HEAD=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
MSHA=abcabcabcabcabcabcabcabcabcabcabcabcabca

REAL_STAT=$(command -v stat) || { echo "BROKEN: no stat on PATH" >&2; exit 2; }
export REAL_STAT
mkdir -p "$WORK/bin" "$WORK/outer-bin" "$WORK/state"
cat > "$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
cat > "$WORK/bin/stat" <<'STUB'
#!/usr/bin/env bash
set -eu
# Model GNU stat: `-c %Y` is the numeric mtime interface, while BSD-first `-f %m`
# succeeds with filesystem-formatted text and therefore is not a portable feature probe.
if [ "${1:-}" = -c ]; then
  last="${!#}"
  if [ "$(uname -s)" = Darwin ]; then "$REAL_STAT" -f %m "$last"; else "$REAL_STAT" -c %Y "$last"; fi
  exit 0
fi
if [ "${1:-}" = -f ]; then
  echo '  File: "%m"'
  echo '    ID: deadbeef Namelen: 255 Type: ext2/ext3'
  exit 0
fi
exec "$REAL_STAT" "$@"
STUB
export OUTER_GH_LOG="$WORK/outer-gh.log"
cat > "$WORK/outer-bin/gh" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$OUTER_GH_LOG"
exit 1
STUB
chmod +x "$WORK/outer-bin/gh"
export PATH="$WORK/outer-bin:$PATH"
cat > "$WORK/bin/gh" <<STUB
#!/usr/bin/env bash
set -eu
if [ "\${1:-}" = pr ] && [ "\${2:-}" = view ]; then
  # Resolve (state, oid) for this call. merged_lagging returns state=MERGED but a null oid on
  # the FIRST view, then the real oid — GitHub's eventual-consistency lag. merged_no_oid never
  # populates it.
  st=MERGED; oid='$MSHA'
  case "\$MERGE_STATE" in
    OPEN) st=OPEN; oid= ;;
    merged_no_oid) oid= ;;
    merged_lagging)
      c=0; [ -f "$WORK/calls" ] && c=\$(cat "$WORK/calls"); c=\$((c+1)); echo "\$c" > "$WORK/calls"
      [ "\$c" -ge 2 ] || oid= ;;
  esac
  if [ "\${CONFIRM_AFTER_MERGE:-0}" = 1 ] && [ -s "$WORK/merge.log" ]; then st=MERGED; oid='$MSHA'; fi
  case "\$*" in
    *closingIssuesReferences*)
      [ "\${CLOSES_FAIL:-0}" = 1 ] && exit 1
      cj_now=\${CLOSES_JSON:-[]}
      # CLOSES_SEQ_FILE counts metadata reads; from the second read on CLOSES_JSON_LATER is returned.
      if [ -n "\${CLOSES_SEQ_FILE:-}" ]; then
        sq=0; [ -f "\$CLOSES_SEQ_FILE" ] && sq=\$(cat "\$CLOSES_SEQ_FILE"); sq=\$((sq+1)); echo "\$sq" > "\$CLOSES_SEQ_FILE"
        [ "\$sq" -lt 2 ] || cj_now=\${CLOSES_JSON_LATER:-[]}
      fi
      [ -z "\${PR_META_JSON:-}" ] || { printf '%s\n' "\$PR_META_JSON"; exit 0; }
      printf '{"closingIssuesReferences":%s,"title":%s}\n' "\$cj_now" "\$(jq -n --arg t "\${PR_TITLE:-fix(x): y}" '\$t')"; exit 0 ;;
  esac
  case "\$*" in
    *'-q .state'*) printf '%s\n' "\$st" ;;
    *-q*)  printf '%s\n' "\$oid" ;;   # the retry: gh -q '.mergeCommit.oid // empty' prints the oid
    *)     if [ -n "\$oid" ]; then
             echo '{"state":"'"\$st"'","headRefOid":"$HEAD","headRefName":"agent/issue-42","mergeStateStatus":"CLEAN","mergeable":"MERGEABLE","mergeCommit":{"oid":"'"\$oid"'"},"baseRefName":"main"}'
           else
             echo '{"state":"'"\$st"'","headRefOid":"$HEAD","headRefName":"agent/issue-42","mergeStateStatus":"BLOCKED","mergeable":"'"\${MERGEABLE:-MERGEABLE}"'","mergeCommit":null,"baseRefName":"main"}'
           fi ;;
  esac
  exit 0
fi
# The base branch's required contexts as gh api --paginate --slurp returns them (pages).
# RULES_JSON = one page; RULES_FAIL=1 = unreadable. Default: none required.
# The every-author blockers: review threads (THREADS_JSON nodes), code-scanning alerts
# (CS_MODE alert|broken), issue comments (COMMENTS_FILE) and reviews (none). Default: clear.
if [ "\${1:-}" = api ]; then
  case "\$*" in
    *'/commits/deadbee '*) [ "\${EXPECT_AMBIGUOUS:-0}" = 1 ] && exit 1; echo '$HEAD'; exit 0 ;;
    *'/commits/abcdef0 '*) echo 'abcdef0123456789abcdef0123456789abcdef01'; exit 0 ;;
    *'/contents/.github/workflows/'*)
      [ "\${WF_FAIL:-0}" = 1 ] && exit 1
      printf '{"content":"%s"}\n' "\$(base64 < "\${WF_BODY_FILE:?}" | tr -d '\n')"
      exit 0 ;;
    *'/pulls/42/files'*)
      [ "\${PR_FILES_FAIL:-0}" = 1 ] && { echo '[]'; exit 1; }
      if [ -n "\${PR_FILES_RAW:-}" ]; then printf '%s\n' "\$PR_FILES_RAW"; else printf '%s\n' "\${PR_FILES_JSON:-[]}"; fi
      exit 0 ;;
    *'/contents/api/internal/store/migrations?'*)
      [ -f "\$UZI_LANDER_STATE_DIR/locks/merge/uuid" ] || { echo 'base lookup outside merge lock' >&2; exit 99; }
      [ "\${BASE_MIG_FAIL:-0}" = 1 ] && exit 1
      printf '%s\n' "\${BASE_MIG_JSON:-[]}"
      exit 0 ;;
    *'/pulls/42/commits'*) [ "\${CLOSES_FAIL:-0}" = 1 ] && exit 1
      [ -z "\${COMMIT_PAGES_JSON:-}" ] || { printf '%s\n' "\$COMMIT_PAGES_JSON"; exit 0; }
      printf '[%s]\n' "\${COMMITS_JSON:-[]}"; exit 0 ;;
    *'/pulls/42 --jq .commits'*) printf '%s\n' "\${COMMIT_COUNT:-\$(printf '%s' "\${COMMITS_JSON:-[]}" | jq length)}"; exit 0 ;;
    *'/protection/required_status_checks'*)
      printf 'HTTP/2.0 %s Test\r\n\r\n' "\${CLASSIC_HTTP:-404}"
      if [ -n "\${CLASSIC_BODY:-}" ]; then printf '%s' "\$CLASSIC_BODY"; else printf '{"message":"Branch not protected"}'; fi
      exit "\${CLASSIC_RC:-1}" ;;
    *graphql*) printf '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":%s,"pageInfo":{"hasNextPage":false}}}}}}\n' "\${THREADS_JSON:-[]}"; exit 0 ;;
    *'/code-scanning/alerts'*)
      [ "\${CS_MODE:-}" = broken ] && { echo 'HTTP 502: Bad Gateway' >&2; exit 1; }
      [ "\${CS_MODE:-}" = malformed404 ] && { echo '[{"number":1,"tool":{"name":"CodeQL"},"rule":{"id":"bug"},"most_recent_instance":{"message":{"text":"known open alert"}}}]'; printf '{'; echo 'gh: later page failed (HTTP 404)' >&2; exit 1; }
      [ "\${CS_MODE:-}" = partial404 ] && { echo '[{"number":1,"tool":{"name":"CodeQL"},"rule":{"id":"bug"},"most_recent_instance":{"message":{"text":"live finding"}}}]'; echo 'gh: later page failed (HTTP 404)' >&2; exit 1; }
      if [ "\${CS_MODE:-}" = alert ]; then echo '[{"number":52,"tool":{"name":"CodeQL"},"rule":{"id":"js/shell-command-injection-from-environment"},"most_recent_instance":{"location":{"path":"agent/src/js-deps.ts","start_line":576},"message":{"text":"Shell command built from environment values."}}}]'; else echo '[]'; fi
      exit 0 ;;
    *'/issues/42/comments'*) if [ -n "\${COMMENTS_FILE:-}" ]; then cat "\$COMMENTS_FILE"; else echo '[]'; fi; exit 0 ;;
    *'/pulls/42/reviews'*) echo '[]'; exit 0 ;;
    *'/git/ref/tags/'*) [ -n "\${TAG_REF_JSON:-}" ] || { echo 'HTTP 404' >&2; exit 1; }; printf '%s\n' "\$TAG_REF_JSON"; exit 0 ;;
    *'/git/tags/'*) [ -n "\${TAG_OBJ_JSON:-}" ] || { echo 'HTTP 404' >&2; exit 1; }; printf '%s\n' "\$TAG_OBJ_JSON"; exit 0 ;;
  esac
  case "\$*" in *'/rules/branches/main'*)
    [ "\${RULES_FAIL:-0}" = 1 ] && exit 1
    if [ -n "\${RULES_JSON:-}" ]; then printf '[%s]\n' "\$RULES_JSON"; else echo '[[]]'; fi
    exit 0 ;;
  esac
fi
if [ "\${1:-}" = pr ] && [ "\${2:-}" = checks ]; then
  if [ "\${CHECKS_REQUIRED_EMPTY:-0}" = 1 ] && [[ " \$* " == *' --required '* ]]; then echo '[]'; exit 0; fi
  printf '%s\n' "\${CHECKS_JSON:-}"; exit "\${CHECKS_RC:-0}"
fi
# main's workflow runs via the Actions API, one paginated query per status: MAIN_RUNS_JSON is
# the full run list (Actions shape); the stub filters by status and splits it into pages of 2,
# so a release run past any recency window is still reached. RUNS_FAIL=1 = an unreadable page.
# MAIN_RUNS_PAGE overrides the raw slurped pages (malformed payloads). Every call, failed ones
# included, appends its status to runs-calls.log. MAIN_RUNS_SEQ_DIR holds files <status>.<n>:
# the raw stdout for the n-th call of that status (no file = the generation below).
# MAIN_RUNS_EMPTY=1 prints nothing and exits 0.
if [ "\${1:-}" = api ] && case "\$*" in *actions/runs*) true;; *) false;; esac; then
  st=\$(printf '%s' "\$*" | sed -n 's/.*status=\\([a-z_]*\\).*/\\1/p')
  echo "\$st" >> "$WORK/runs-calls.log"
  [ "\${RUNS_FAIL:-0}" = 1 ] && { echo 'HTTP 502' >&2; exit 1; }
  [ "\${MAIN_RUNS_EMPTY:-0}" = 1 ] && exit 0
  if [ -n "\${MAIN_RUNS_SEQ_DIR:-}" ]; then
    n=\$(grep -cFx -- "\$st" "$WORK/runs-calls.log" || true)
    [ -f "\$MAIN_RUNS_SEQ_DIR/\$st.\$n" ] && { cat "\$MAIN_RUNS_SEQ_DIR/\$st.\$n"; exit 0; }
  fi
  [ -n "\${MAIN_RUNS_PAGE:-}" ] && { printf '%s\n' "\$MAIN_RUNS_PAGE"; exit 0; }
  [ -n "\${MAIN_RUNS_PAGE_FILE:-}" ] && { cat "\$MAIN_RUNS_PAGE_FILE"; exit 0; }
  printf '%s' "\${MAIN_RUNS_JSON:-[]}" | jq -c --arg s "\$st" '[.[]|select(.status==\$s)] as \$r
    | (\$r|length) as \$n | if \$n==0 then [{total_count:0,workflow_runs:[]}] else [range(0; \$n; 2) as \$i | {total_count:\$n,workflow_runs: \$r[\$i:\$i+2]}] end'
  exit 0
fi
# The same data through the older gh run list --limit N lookup, so this case also proves the
# window and malformed-record regressions against a recency-capped implementation.
if [ "\${1:-}" = run ] && [ "\${2:-}" = list ]; then
  [ "\${RUNS_FAIL:-0}" = 1 ] && exit 1
  lim=20; prev=; for a in "\$@"; do [ "\$prev" = --limit ] && lim=\$a; prev=\$a; done
  if [ -n "\${MAIN_RUNS_PAGE:-}" ]; then
    printf '%s' "\$MAIN_RUNS_PAGE" | jq -c '[.[]?|.workflow_runs[]?|with_entries(.key |= ({display_title:"displayTitle",head_sha:"headSha",id:"databaseId"}[.] // .))]' 2>/dev/null || echo '[]'
  else
    printf '%s' "\${MAIN_RUNS_JSON:-[]}" | jq -c --argjson n "\$lim" '.[:\$n]|map({status,displayTitle:.display_title,headSha:.head_sha,name,databaseId:.id})'
  fi
  exit 0
fi
if [ "\${1:-}" = pr ] && [ "\${2:-}" = merge ]; then echo "\$*" >> "$WORK/merge.log"; exit 0; fi
echo "unexpected gh call: \$*" >&2
exit 1
STUB
[ ! -s "$OUTER_GH_LOG" ] || fail "outer gh ran while creating the gh stub: $(cat "$OUTER_GH_LOG")"
# python3 stand-in for scripts/release-ci-coverage.py only (COV_RC = its verdict); every other
# python3 call (lib/state.sh's peer registry) goes to the real interpreter.
REAL_PY=$(command -v python3) || { echo "BROKEN: no python3 on PATH" >&2; exit 2; }
export REAL_PY
cat > "$WORK/bin/python3" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  *release-ci-coverage.py*) echo "$*" >> "$COV_LOG"; echo "coverage stub rc=${COV_RC:-1}"; exit "${COV_RC:-1}" ;;
esac
# The session-peers registry, only when a case supplies one (REG_JSON); each call is counted.
if [ -n "${REG_JSON:-}" ] && [ "$*" = "${SESSION_PEERS_PY:-} list --json" ]; then
  echo x >> "$REG_LOG"; printf '%s\n' "$REG_JSON"; exit 0
fi
exec "$REAL_PY" "$@"
STUB
export COV_LOG="$WORK/cov.log"
chmod +x "$WORK/bin/gh" "$WORK/bin/sleep" "$WORK/bin/stat" "$WORK/bin/python3"
export PATH="$WORK/bin:$PATH"
export UZI_LANDER_STATE_DIR="$WORK/state"

CL="$WORK/state/claims"; TR="$WORK/state/trail"
mkdir -p "$CL" "$TR"
seed_state() {  # a live claim + a pre-merge trail for #42, so the terminal side effects are observable
  printf '{"key":"#42","repo":"test/repo","pr":42,"owner":"tester","owner_uuid":"unknown-test","state":"pushed"}\n' > "$CL/#42.json"
  printf 'pr opened\nci green\n' > "$TR/#42.trail"
}

# 1. Already MERGED out of band → --confirm-only emits the owed terminal evidence (MERGED, the
#    true MERGE_SHA, the trail's admin-merged line) THEN releases the claim but preserves the
#    trail until the post-merge result is appended, printed and explicitly purged.
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
set -eu
case "$*" in
  'repo list --json') jq -cn --arg repo "${HELPER_REPO:-test/repo}" '[{id:"repo",path_with_namespace:$repo}]' ;;
  'run list --json')
    n=0; [ ! -f "$HELPER_CALLS" ] || n=$(cat "$HELPER_CALLS")
    n=$((n+1)); echo "$n" > "$HELPER_CALLS"
    [ "${HELPER_RUNS_RC:-0}" -eq 0 ] || exit "$HELPER_RUNS_RC"
    if [ "${HELPER_LATE:-0}" = 1 ] && [ "$n" -eq 1 ]; then echo '[]'
    else printf '%s\n' "$HELPER_RUNS"; fi ;;
  # cobra prints the parent help (rc 0) for any subcommand; the verb line exists only on a new CLI.
  'admin --help')
    printf 'Admin commands\n\nUsage:\n  uzi admin [command]\n\nAvailable Commands:\n  review      Review backlog\n'
    [ -z "${ADMIN_RW:-}" ] || printf '  run-writers List active ci_fix and mr_rework writers\n'
    printf '\nFlags:\n  -h, --help   help for admin\n' ;;
  'admin run-writers '*)
    echo "$*" >> "$WRITERS_ARGS"
    n=0; [ ! -f "$WRITERS_CALLS" ] || n=$(cat "$WRITERS_CALLS")
    n=$((n+1)); echo "$n" > "$WRITERS_CALLS"
    if [ "${WRITERS_LATE:-0}" = 1 ] && [ "$n" -eq 1 ]; then echo '{"writers":[],"complete":true}'; exit 0; fi
    [ "${WRITERS_RC:-0}" -eq 0 ] || { echo "HTTP ${WRITERS_HTTP:-500}" >&2; exit "$WRITERS_RC"; }
    printf '%s\n' "$WRITERS_OUT" ;;
  *) exit 1 ;;
esac
STUB
chmod +x "$WORK/bin/uzi"

if [ "${UZI_MERGE_POLICY_ONLY:-0}" != 1 ]; then
MERGE_STATE=MERGED; export MERGE_STATE
seed_state
set +e
bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/confirm.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--confirm-only on a merged PR returned rc=$rc: $(cat "$WORK/confirm.out")"
grep -q '^MERGED #42$' "$WORK/confirm.out" || fail "--confirm-only did not confirm MERGED: $(cat "$WORK/confirm.out")"
grep -q "^MERGE_SHA=$MSHA$" "$WORK/confirm.out" || fail "--confirm-only did not print the true MERGE_SHA: $(cat "$WORK/confirm.out")"
# trail.sh prints the whole trail to stdout and preserves it for post-merge CI, so a deleted
# trail.sh call (the #1510 regression) would drop this line.
grep -q "admin-merged ${MSHA:0:8}" "$WORK/confirm.out" || fail "--confirm-only did not write the terminal trail line: $(cat "$WORK/confirm.out")"
# The live claim is gone, but the trail survives for the post-merge CI result.
[ ! -e "$CL/#42.json" ] || fail "--confirm-only did not release the claim"
[ -e "$TR/#42.trail" ] || fail "--confirm-only purged the trail before post-merge CI"
final=$(bash "$HERE/trail.sh" '#42' 'main ci green')
[ "$final" = "#42: pr opened → ci green → admin-merged ${MSHA:0:8} → main ci green" ] \
  || fail "post-merge CI lost the earlier trail: $final"
bash "$HERE/claims.sh" release '#42' --purge > /dev/null
[ ! -e "$TR/#42.trail" ] || fail "final cleanup did not purge the completed trail"

# If the landing session dies before explicit cleanup, a fresh orphan trail survives reap while
# a stale one is collected after the shared stale-owner TTL. This prevents both callback-time
# history loss and permanent state leakage.
printf 'admin-merged deadbeef\n' > "$TR/#fresh.trail"
printf 'admin-merged deadbeef\n' > "$TR/#stale.trail"
touch -t 200001010000 "$TR/#stale.trail"
bash "$HERE/claims.sh" reap > "$WORK/reap.out"
[ -e "$TR/#fresh.trail" ] || fail "reap removed a fresh post-merge trail"
[ ! -e "$TR/#stale.trail" ] || fail "reap left a stale orphan trail behind"
grep -q 'reaped #stale (orphan trail stale ' "$WORK/reap.out" || fail "reap did not report stale orphan cleanup: $(cat "$WORK/reap.out")"
rm -f "$TR/#fresh.trail"

# A concurrent reap can observe MERGED before merge.sh releases the claim. It must remove the
# terminal claim without deleting the fresh trail the post-merge watcher still needs.
seed_state
bash "$HERE/claims.sh" reap > "$WORK/reap-merged.out"
[ ! -e "$CL/#42.json" ] || fail "reap left a merged claim on the live board"
[ -e "$TR/#42.trail" ] || fail "reap deleted the merged PR trail before post-merge CI"
grep -q 'reaped #42 (PR MERGED; trail preserved)' "$WORK/reap-merged.out" \
  || fail "reap did not report terminal claim-only cleanup: $(cat "$WORK/reap-merged.out")"
bash "$HERE/claims.sh" release '#42' --purge > /dev/null

# A terminal claim can carry a trail older than the orphan TTL after a long review wait.
# Terminal cleanup refreshes its mtime, so neither that reap nor the next immediate reap
# can delete the history before post-merge CI appends its result.
seed_state
touch -t 200001010000 "$TR/#42.trail"
bash "$HERE/claims.sh" reap > "$WORK/reap-stale-terminal.out"
[ ! -e "$CL/#42.json" ] || fail "reap left the stale terminal claim on the live board"
[ -e "$TR/#42.trail" ] || fail "terminal reap deleted an old trail instead of refreshing it"
bash "$HERE/claims.sh" reap > "$WORK/reap-stale-terminal-again.out"
[ -e "$TR/#42.trail" ] || fail "the next immediate reap deleted the refreshed terminal trail"
bash "$HERE/claims.sh" release '#42' --purge > /dev/null

# 2. state=MERGED but mergeCommit not populated yet (GitHub lag) → re-read recovers the oid.
MERGE_STATE=merged_lagging; export MERGE_STATE
rm -f "$WORK/calls"; seed_state
set +e
bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/lag.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--confirm-only did not recover a lagging mergeCommit, rc=$rc: $(cat "$WORK/lag.out")"
grep -q "^MERGE_SHA=$MSHA$" "$WORK/lag.out" || fail "--confirm-only did not re-read the populated oid: $(cat "$WORK/lag.out")"

# 3. state=MERGED but mergeCommit never populates → refuse (exit 9): NEVER an empty MERGE_SHA
#    or a bare admin-merged trail, and the claim stays.
MERGE_STATE=merged_no_oid; export MERGE_STATE
seed_state
set +e
bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/nooid.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 9 ] || fail "--confirm-only emitted evidence with no merge commit, rc=$rc: $(cat "$WORK/nooid.out")"
if grep -qE '^MERGE_SHA=$|admin-merged $' "$WORK/nooid.out"; then fail "--confirm-only wrote empty terminal evidence: $(cat "$WORK/nooid.out")"; fi
[ -e "$CL/#42.json" ] || fail "--confirm-only purged the claim without confirming the merge"

# 4. Still OPEN → refuse (exit 9): never a merge, never a purge of a live claim/trail.
MERGE_STATE=OPEN; export MERGE_STATE
seed_state
set +e
bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/open.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 9 ] || fail "--confirm-only on an OPEN PR returned rc=$rc, want 9: $(cat "$WORK/open.out")"
grep -q 'not MERGED' "$WORK/open.out" || fail "--confirm-only OPEN did not report not-merged: $(cat "$WORK/open.out")"
{ [ -e "$CL/#42.json" ] && [ -e "$TR/#42.trail" ]; } || fail "--confirm-only OPEN mutated the claim/trail: $(cat "$WORK/open.out")"

# 5. The guarded merge's required-checks gate is fail-closed. An EMPTY list (a head with no
#    check runs: CI skipped, a missed dispatch) and an unreadable reply both refuse with
#    exit 2 and no merge call; a failing check is read from the array gh prints alongside
#    its non-zero exit; a pass with an unexplained non-zero exit (partial read) and a
#    skipping-only list refuse; pass+skipping and pass reach the merge.
MERGE_STATE=OPEN; export MERGE_STATE
merge_run() { # label -> rc, output in $WORK/m.<label>
  rm -f "$WORK/merge.log" "$WORK/runs-calls.log"
  set +e
  bash "$SCRIPT" test/repo 42 --no-rework-check ${MERGE_ARGS[@]+"${MERGE_ARGS[@]}"} > "$WORK/m.$1" 2>&1
  rc=$?
  set -e
}
# Exact-head review accepts a unique short SHA, resolves it before comparing,
# and still pins the full head server-side. Mismatch diagnostics never truncate.
CHECKS_JSON=$(jq -cn '[{name:"ci",bucket:"pass"}]')
CHECKS_RC=0; export CHECKS_JSON CHECKS_RC
MERGE_ARGS=(--expect-head deadbee)
CONFIRM_AFTER_MERGE=1; export CONFIRM_AFTER_MERGE
merge_run short_head
[ "$rc" -eq 0 ] || fail "matching seven-character head rc=$rc: $(cat "$WORK/m.short_head")"
[ -e "$WORK/merge.log" ] || fail "matching seven-character head refused: $(cat "$WORK/m.short_head")"
grep -qF -- "--match-head-commit $HEAD" "$WORK/merge.log" || fail "short head weakened the server-side full-head lock"
# Merge-lock contention reads the session-peers registry once: self_identity runs in a
# $(...) subshell, so without a warm fetch in the parent the holder's is_live refetched.
mkdir -p "$WORK/state/locks/merge"; printf uuid-other > "$WORK/state/locks/merge/uuid"; printf other > "$WORK/state/locks/merge/name"
: > "$WORK/peers.py"; rm -f "$WORK/reg.log"
(
  unset SESSION_PEERS_NAME SESSION_PEERS_UUID SESSION_PEERS_KIND CODEX_THREAD_ID OPENCODE_SESSION_ID
  export SESSION_PEERS_PY="$WORK/peers.py" CLAUDE_CODE_MESSAGING_SOCKET=/x REG_LOG="$WORK/reg.log"
  export REG_JSON='{"claude":[{"sessionId":"uuid-me","name":"me","socket":"/x"},{"sessionId":"uuid-other","name":"other","socket":"/y"}],"codex":[],"codex_schema_recognised":true}'
  merge_run lock_held; echo "$rc" > "$WORK/lock_held.rc"
)
[ "$(cat "$WORK/lock_held.rc")" -eq 7 ] || fail "live lock holder rc=$(cat "$WORK/lock_held.rc"), want 7: $(cat "$WORK/m.lock_held")"
grep -qF 'MERGE_LOCK_HELD_BY=other (uuid-other' "$WORK/m.lock_held" || fail "lock holder not named: $(cat "$WORK/m.lock_held")"
[ ! -e "$WORK/merge.log" ] || fail "a held merge lock reached the merge"
[ "$(wc -l < "$WORK/reg.log" | tr -d ' ')" -eq 1 ] || fail "merge-lock contention fetched the registry $(wc -l < "$WORK/reg.log" | tr -d ' ') times, want 1"
rm -rf "$WORK/state/locks/merge" "$WORK/peers.py"
MERGE_ARGS=(--expect-head abcdef0)
merge_run short_mismatch
[ "$rc" -eq 8 ] || fail "short mismatch rc=$rc, want 8"
grep -qF -- "current $HEAD, expected abcdef0123456789abcdef0123456789abcdef01" "$WORK/m.short_mismatch" || fail "mismatch must show both full SHAs: $(cat "$WORK/m.short_mismatch")"
[ ! -e "$WORK/merge.log" ] || fail "short mismatch reached merge"
MERGE_ARGS=(--expect-head deadbeef00000000000000000000000000000000)
merge_run shared_display_prefix
grep -qF -- "current $HEAD, expected deadbeef00000000000000000000000000000000" "$WORK/m.shared_display_prefix" || fail "same displayed prefix hid full mismatch: $(cat "$WORK/m.shared_display_prefix")"
for bad in deadbe 'deadbee.*'; do
  MERGE_ARGS=(--expect-head "$bad")
  merge_run invalid_head
  [ "$rc" -eq 2 ] || fail "invalid head '$bad' rc=$rc, want 2"
  [ ! -e "$WORK/merge.log" ] || fail "invalid head reached merge"
done
MERGE_ARGS=(--expect-head deadbee); EXPECT_AMBIGUOUS=1; export EXPECT_AMBIGUOUS
merge_run ambiguous_head
[ "$rc" -eq 3 ] || fail "ambiguous/unresolvable prefix rc=$rc, want 3"
[ ! -e "$WORK/merge.log" ] || fail "ambiguous prefix reached merge"
unset MERGE_ARGS EXPECT_AMBIGUOUS CONFIRM_AFTER_MERGE
CHECKS_JSON='[]' CHECKS_RC=0; export CHECKS_JSON CHECKS_RC
export RULES_JSON='[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}]}}]'
merge_run empty
[ "$rc" -eq 2 ] || fail "an empty required-checks list returned rc=$rc, want 2: $(cat "$WORK/m.empty")"
grep -q "no required checks reported on ${HEAD:0:8}; not merging" "$WORK/m.empty" || fail "empty list not named: $(cat "$WORK/m.empty")"
[ ! -e "$WORK/merge.log" ] || fail "merged with no required checks reported"
CHECKS_JSON='' CHECKS_RC=1
merge_run unreadable
[ "$rc" -eq 2 ] || fail "a failed gh pr checks returned rc=$rc, want 2: $(cat "$WORK/m.unreadable")"
grep -q 'cannot read the required checks for #42 (gh exit 1)' "$WORK/m.unreadable" || fail "gh failure not named: $(cat "$WORK/m.unreadable")"
[ ! -e "$WORK/merge.log" ] || fail "merged with unreadable required checks"
CHECKS_JSON='[{"name":"ci","bucket":"pass"},{"name":"other","bucket":"fail"}]' CHECKS_RC=1
merge_run failing
[ "$rc" -eq 1 ] || fail "a failing required check returned rc=$rc, want 1: $(cat "$WORK/m.failing")"
[ ! -e "$WORK/merge.log" ] || fail "merged with a failing required check"
# A pass with a NON-zero gh exit is a partial read (API failure), not a verdict.
CHECKS_JSON='[{"name":"ci","bucket":"pass"}]' CHECKS_RC=1
merge_run partial
[ "$rc" -eq 2 ] || fail "a pass list with gh exit 1 returned rc=$rc, want 2: $(cat "$WORK/m.partial")"
grep -q 'gh pr checks exited 1 with no failing or pending check' "$WORK/m.partial" || fail "partial read not named: $(cat "$WORK/m.partial")"
[ ! -e "$WORK/merge.log" ] || fail "merged on a partial required-checks read"
# Only skipping checks: no required gate ran.
CHECKS_JSON='[{"name":"ci","bucket":"skipping"},{"name":"other","bucket":"skipping"}]' CHECKS_RC=0
merge_run skipping
[ "$rc" -eq 2 ] || fail "skipping-only returned rc=$rc, want 2: $(cat "$WORK/m.skipping")"
grep -q 'no required check passed' "$WORK/m.skipping" || fail "skipping-only not named: $(cat "$WORK/m.skipping")"
[ ! -e "$WORK/merge.log" ] || fail "merged with only skipping required checks"
# Path-filtered checks skip next to a passing one: that still merges.
CHECKS_JSON='[{"name":"ci","bucket":"pass"},{"name":"other","bucket":"skipping"}]' CHECKS_RC=0
merge_run passskip
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "pass+skipping did not reach the merge: $(cat "$WORK/m.passskip")"
CHECKS_JSON='[{"name":"ci","bucket":"pass"}]' CHECKS_RC=0
merge_run green
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "a green required-checks list did not reach the merge: $(cat "$WORK/m.green")"
# A green head is still owned by an active helper. The second lookup models a
# helper appearing while the other merge preflights run; no real factory is used.
export HELPER_CALLS="$WORK/helper.calls" WRITERS_CALLS="$WORK/writers.calls" WRITERS_ARGS="$WORK/writers.args" CONFIRM_AFTER_MERGE=1
helper_merge() {
  rm -f "$WORK/merge.log" "$HELPER_CALLS" "$WRITERS_CALLS" "$WRITERS_ARGS"
  set +e
  bash "$SCRIPT" test/repo 42 > "$WORK/m.helper" 2>&1
  rc=$?
  set -e
  [ "$rc" -eq "$1" ] || fail "helper guard rc=$rc, want $1: $(cat "$WORK/m.helper")"
  if [ "$1" -ne 0 ]; then [ ! -e "$WORK/merge.log" ] || fail 'helper refusal merged'; fi
}
export HELPER_RUNS='[{"id":"fix-active","kind":"ci_fix","repo_id":"repo","pipeline_ref":"agent/issue-42","status":"running"}]'
helper_merge 14
grep -qF 'fix-active' "$WORK/m.helper" || fail 'active ci_fix id missing'
export HELPER_LATE=1
helper_merge 14
[ "$(cat "$HELPER_CALLS")" -eq 2 ] || fail 'merge did not refresh ownership'
unset HELPER_LATE
export HELPER_RUNS='[{"id":"rework-active","kind":"mr_rework","repo_id":"repo","mr_iid":42,"status":"awaiting_approval"}]'
helper_merge 14
grep -qF 'wait-mrrework.sh' "$WORK/m.helper" || fail 'rework wait guidance missing'
export HELPER_RUNS='not-json'
helper_merge 4
export HELPER_RUNS='[{"id":"bad","kind":"ci_fix","repo_id":"repo","pipeline_ref":"agent/issue-42","status":null}]'
helper_merge 4
export HELPER_RUNS='[{"id":"unrelated","kind":"ci_fix","repo_id":"repo","pipeline_ref":"agent/issue-420","status":"running"}]'
helper_merge 0
[ -s "$WORK/merge.log" ] || fail 'unrelated branch prevented merge'
# Authorized all-owners read (uzi admin run-writers). Owner list is empty or unparseable in every
# case below, proving the decision comes from the authorized read and never downgrades.
export ADMIN_RW=1 HELPER_RUNS='not-json'
wrow() { jq -nc --arg id "$1" --arg kind "$2" --arg st "$3" --arg ref "$4" --argjson mr "$5" \
  '{run_id:$id,kind:$kind,status:$st,owner_id:"other-owner",repo_id:"repo",pipeline_ref:(if $ref=="" then null else $ref end),mr_iid:$mr,created_at:"2026-01-01T00:00:00Z"}'; }
wout() { jq -nc --argjson w "$1" --argjson c "${2:-true}" '{writers:$w,complete:$c}'; }
export WRITERS_OUT; WRITERS_OUT=$(wout "[$(wrow foreign-fix ci_fix running agent/issue-42 null)]")
helper_merge 14
grep -qF 'ACTIVE_HELPER=foreign-fix kind=ci_fix' "$WORK/m.helper" || fail 'foreign ci_fix not named'
grep -qF 'VISIBILITY=all_owners' "$WORK/m.helper" || fail 'all_owners visibility missing'
grep -qF -- '--repo repo --branch agent/issue-42 --mr 42 --json' "$WORK/writers.args" || fail "run-writers args: $(cat "$WORK/writers.args")"
WRITERS_OUT=$(wout "[$(wrow foreign-rw mr_rework awaiting_approval '' 42)]")
helper_merge 14
grep -qF 'ACTIVE_HELPER=foreign-rw kind=mr_rework' "$WORK/m.helper" || fail 'authorized mr_rework not named'
# A writer that appears between the preflight and the final refresh stops the merge (no gh pr merge).
export WRITERS_LATE=1
WRITERS_OUT=$(wout "[$(wrow late-fix ci_fix queued agent/issue-42 null)]")
helper_merge 14
[ "$(cat "$WRITERS_CALLS")" -eq 2 ] || fail 'authorized read was not refreshed before the merge'
grep -qF 'late-fix' "$WORK/m.helper" || fail 'late writer not named'
unset WRITERS_LATE
# Complete empty result proceeds, and the merge still pins the head.
WRITERS_OUT=$(wout '[]')
helper_merge 0
grep -qF 'VISIBILITY=all_owners' "$WORK/m.helper" || fail 'empty authorized read lacks visibility line'
grep -q -- '--match-head-commit' "$WORK/merge.log" || fail 'merge lost --match-head-commit'
# Anything but a clean complete result refuses (exit 4), never a downgrade to the owner list.
export HELPER_RUNS='[]'
refuse_case() { # name rc out
  WRITERS_RC=$2 WRITERS_OUT=$3 helper_merge 4
  grep -qF 'cannot rule out active helpers' "$WORK/m.helper" || fail "$1: refusal not explained: $(cat "$WORK/m.helper")"
  [ ! -s "$HELPER_CALLS" ] || fail "$1: downgraded to the owner run list"
}
refuse_case malformed 0 'not-json'
refuse_case incomplete-rc0 0 "$(wout '[]' false)"
refuse_case incomplete-rc1 1 "$(wout '[]' false)"
refuse_case rc1 1 ''
refuse_case rc2 2 ''
refuse_case rc6 6 ''
refuse_case rc5 5 "$(wout '[]')"
refuse_case terminal-entry 0 "$(wout "[$(wrow finished ci_fix completed agent/issue-42 null)]")"
refuse_case bad-id 0 "$(wout "[$(wrow 'a b' ci_fix running agent/issue-42 null)]")"
refuse_case bad-kind 0 "$(wout "[$(wrow k issue running agent/issue-42 null)]")"
refuse_case ci_fix-other-branch 0 "$(wout "[$(wrow k ci_fix running agent/issue-43 null)]")"
refuse_case rework-other-mr 0 "$(wout "[$(wrow k mr_rework running '' 43)]")"
refuse_case writers-not-array 0 '{"writers":{},"complete":true}'
# 401/403 (CLI rc 3) and 404 (rc 4) fall back to the owner-scoped list; owner-only is labelled.
for pair in 401:3 403:3 404:4; do
  export WRITERS_HTTP=${pair%%:*} WRITERS_RC=${pair##*:}
  export HELPER_RUNS='[{"id":"own-fix","kind":"ci_fix","repo_id":"repo","pipeline_ref":"agent/issue-42","status":"running"}]'
  helper_merge 14
  grep -qF 'ACTIVE_HELPER=own-fix' "$WORK/m.helper" || fail "$pair: owner writer not refused"
  export HELPER_RUNS='[]'
  helper_merge 0
  grep -qF 'VISIBILITY=owner_only' "$WORK/m.helper" || fail "$pair: owner_only not printed"
  grep -qxF 'note: owner-only visibility; foreign or older writers are not excluded' "$WORK/m.helper" || fail "$pair: owner-only note missing"
done
unset WRITERS_HTTP WRITERS_RC
# A failed or malformed fallback refuses, whatever row is malformed (even a non-matching one).
export HELPER_RUNS='[]'; WRITERS_RC=3 HELPER_RUNS_RC=1 helper_merge 4
for bad in '[{}]' '[1]' '[{"id":"x","kind":"mr_rework","repo_id":"repo","mr_iid":"7","status":"running"}]' \
  '[{"id":"o","kind":"issue","repo_id":"elsewhere","status":"RUNNING"}]' '[{"id":"o","kind":"issue","repo_id":7,"status":"running"}]'; do
  WRITERS_RC=4 HELPER_RUNS=$bad helper_merge 4
done
# An older CLI (no run-writers line in admin help) falls back without calling the verb.
unset ADMIN_RW WRITERS_RC
export HELPER_RUNS='[{"id":"old-fix","kind":"ci_fix","repo_id":"repo","pipeline_ref":"agent/issue-42","status":"running"}]'
helper_merge 14
[ ! -e "$WRITERS_ARGS" ] || fail 'older CLI still called run-writers'
unset HELPER_RUNS HELPER_CALLS WRITERS_CALLS WRITERS_ARGS WRITERS_OUT CONFIRM_AFTER_MERGE
# A required context the base branch's rules name but the head has not reported is pending:
# right after a push only the fast required checks have registered.
export RULES_JSON='[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"},{"context":"slow"}]}}]'
CHECKS_JSON='[{"name":"ci","bucket":"pass"}]' CHECKS_RC=0
merge_run unregistered
[ "$rc" -eq 2 ] || fail "an unregistered required check returned rc=$rc, want 2: $(cat "$WORK/m.unregistered")"
grep -q "1 required check(s) not yet reported on ${HEAD:0:8}; not merging" "$WORK/m.unregistered" || fail "unregistered check not named: $(cat "$WORK/m.unregistered")"
[ ! -e "$WORK/merge.log" ] || fail "merged before every required check reported"
CHECKS_JSON='[{"name":"ci","bucket":"pass"},{"name":"slow","bucket":"pass"}]'
merge_run allreported
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "every required check passed yet no merge: $(cat "$WORK/m.allreported")"
RULES_FAIL=1; export RULES_FAIL
merge_run rulesunreadable
[ "$rc" -eq 2 ] || fail "unreadable required-check rules returned rc=$rc, want 2: $(cat "$WORK/m.rulesunreadable")"
grep -q 'cannot read the required checks of main; not merging' "$WORK/m.rulesunreadable" || fail "unreadable rules not named: $(cat "$WORK/m.rulesunreadable")"
[ ! -e "$WORK/merge.log" ] || fail "merged with unreadable required-check rules"
unset RULES_FAIL
# A required-check rule with no list is malformed: refuse, never read it as none required.
export RULES_JSON='[{"type":"required_status_checks","parameters":{}}]'
merge_run rulesmalformed
[ "$rc" -eq 2 ] || fail "a malformed required-check rule returned rc=$rc, want 2: $(cat "$WORK/m.rulesmalformed")"
[ ! -e "$WORK/merge.log" ] || fail "merged on a malformed required-check rule"
unset CHECKS_JSON CHECKS_RC RULES_JSON

# Readable empty requirements gate ALL checks, including optional failures/skips.
export CHECKS_REQUIRED_EMPTY=1 CHECKS_RC=0
export CHECKS_JSON='[{"name":"ci","bucket":"pass"},{"name":"optional","bucket":"skipping"}]'
merge_run noreq-green
grep -q -- '--match-head-commit' "$WORK/merge.log" || fail "no-required green did not merge: $(cat "$WORK/m.noreq-green")"
CHECKS_JSON='[{"name":"ci","bucket":"skipping"}]'; merge_run noreq-skipping
grep -q -- '--match-head-commit' "$WORK/merge.log" || fail "no-required skipping did not merge"
for bucket in pending cancel mystery; do
  CHECKS_JSON="[{\"name\":\"ci\",\"bucket\":\"$bucket\"}]"; merge_run "noreq-$bucket"
  [ "$rc" -eq 2 ] && [ ! -e "$WORK/merge.log" ] || fail "no-required $bucket was merge-ready"
done
CHECKS_JSON='[{"name":"ci","bucket":"fail"}]'; merge_run noreq-fail
[ "$rc" -eq 1 ] && [ ! -e "$WORK/merge.log" ] || fail "no-required failed check was merge-ready"
CHECKS_JSON='[]'; merge_run noreq-empty
[ "$rc" -eq 2 ] && [ ! -e "$WORK/merge.log" ] || fail "no-required zero checks was merge-ready"
unset CHECKS_REQUIRED_EMPTY CHECKS_JSON CHECKS_RC

export CLASSIC_HTTP=200 CLASSIC_RC=0 CLASSIC_BODY='{"contexts":["slow"],"checks":[]}'
export CHECKS_REQUIRED_EMPTY=1 CHECKS_JSON='[{"name":"fast","bucket":"pass"}]' CHECKS_RC=0
merge_run classic-unregistered
[ "$rc" -eq 2 ] && [ ! -e "$WORK/merge.log" ] || fail "merged before classic slow registered"
unset CHECKS_REQUIRED_EMPTY
CLASSIC_BODY='{"contexts":["ci","slow"],"checks":[]}'
CHECKS_JSON='[{"name":"ci","bucket":"pass"}]'; merge_run classic-partial
[ "$rc" -eq 2 ] && [ ! -e "$WORK/merge.log" ] || fail "merged without all classic contexts"
CLASSIC_HTTP=403; CLASSIC_RC=1; CLASSIC_BODY='{"message":"Forbidden"}'; merge_run classic-forbidden
[ "$rc" -eq 2 ] && [ ! -e "$WORK/merge.log" ] || fail "unreadable classic protection was none"
export RULES_JSON='[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}]}}]'
merge_run rules-present-classic-forbidden
grep -q -- '--match-head-commit' "$WORK/merge.log" || fail "ruleset repo lost legacy readiness without classic read rights"
unset RULES_JSON
CLASSIC_HTTP=404; CLASSIC_BODY='{"message":"Required status checks not enabled"}'; merge_run classic-disabled
grep -q -- '--match-head-commit' "$WORK/merge.log" || fail "explicit classic-disabled reply not accepted"
unset CLASSIC_HTTP CLASSIC_RC CLASSIC_BODY CHECKS_JSON CHECKS_RC

# 6. A PR that conflicts with its base gets no pull_request CI, so gh reports no required
#    checks and exits 1. Name the conflict (exit 3, land-prep), not "cannot read the checks".
export MERGEABLE=CONFLICTING CHECKS_JSON='' CHECKS_RC=1
merge_run conflict
[ "$rc" -eq 3 ] || fail "a conflicting PR returned rc=$rc, want 3: $(cat "$WORK/m.conflict")"
grep -q 'conflicts with its base (mergeable=CONFLICTING' "$WORK/m.conflict" || fail "conflict not named: $(cat "$WORK/m.conflict")"
grep -q 'land-prep.sh' "$WORK/m.conflict" || fail "land-prep not pointed to: $(cat "$WORK/m.conflict")"
grep -q 'cannot read the required checks' "$WORK/m.conflict" && fail "a conflict was reported as unreadable checks"
[ ! -e "$WORK/merge.log" ] || fail "merged a conflicting PR"
# mergeable=UNKNOWN (GitHub computing) is not a conflict: the checks gate still decides.
export MERGEABLE=UNKNOWN
merge_run computing
[ "$rc" -eq 2 ] || fail "mergeable=UNKNOWN returned rc=$rc, want 2: $(cat "$WORK/m.computing")"
grep -q 'conflicts with its base' "$WORK/m.computing" && fail "mergeable=UNKNOWN read as a conflict"
unset MERGEABLE CHECKS_JSON CHECKS_RC

# 7. Every-author blockers refuse a green PR (#1817): a CodeQL thread, an open code-scanning
#    alert, an unacknowledged human comment (exit 5, listed as UNTRUSTED rows, no merge); an
#    unreadable alert lookup refuses too (exit 2). Acking the comment lets the merge through.
export CHECKS_JSON='[{"name":"ci","bucket":"pass"}]' CHECKS_RC=0
export THREADS_JSON='[{"isResolved":false,"isOutdated":false,"comments":{"nodes":[{"databaseId":4116110339,"author":{"login":"github-advanced-security"},"body":"## CodeQL / Improper code sanitization","path":"agent/test/r.test.ts","line":372,"originalLine":372}],"pageInfo":{"hasNextPage":false}}}]'
merge_run codeql
[ "$rc" -eq 5 ] || fail "an unresolved CodeQL thread did not refuse the merge, rc=$rc: $(cat "$WORK/m.codeql")"
grep -qF '  UNTRUSTED [thread t4116110339] author=github-advanced-security' "$WORK/m.codeql" || fail "CodeQL thread not listed: $(cat "$WORK/m.codeql")"
grep -q 'BLOCKED: open_threads=1 code_scanning=0 unacknowledged=0' "$WORK/m.codeql" || fail "blocker counts not named: $(cat "$WORK/m.codeql")"
[ ! -e "$WORK/merge.log" ] || fail "merged over an unresolved CodeQL thread"
# Every unresolved bot thread refuses too, whatever reviewer lane was used, and a look-alike
# login is just another author: only resolving (or outdating) the thread clears it.
for author in coderabbitai greptile-apps coderabbitai-mallory; do
  THREADS_JSON=$(jq -nc --arg u "$author" '[{isResolved:false,isOutdated:false,comments:{nodes:[{databaseId:88,author:{login:$u},body:"DO NOT MERGE",path:"x.go",line:3,originalLine:3}],pageInfo:{hasNextPage:false}}}]'); export THREADS_JSON
  merge_run "thread-$author"
  [ "$rc" -eq 5 ] || fail "an unresolved $author thread did not refuse the merge, rc=$rc: $(cat "$WORK/m.thread-$author")"
  [ ! -e "$WORK/merge.log" ] || fail "merged over an unresolved $author thread"
done
# A resolved or outdated thread does not block.
export THREADS_JSON='[{"isResolved":true,"isOutdated":false,"comments":{"nodes":[{"databaseId":89,"author":{"login":"coderabbitai"},"body":"x","path":"x.go","line":3,"originalLine":3}],"pageInfo":{"hasNextPage":false}}},{"isResolved":false,"isOutdated":true,"comments":{"nodes":[{"databaseId":90,"author":{"login":"greptile-apps"},"body":"y","path":"x.go","line":null,"originalLine":3}],"pageInfo":{"hasNextPage":false}}}]'
merge_run settled-threads
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "resolved/outdated threads blocked the merge: $(cat "$WORK/m.settled-threads")"
unset THREADS_JSON
export CS_MODE=alert
merge_run alert
[ "$rc" -eq 5 ] || fail "an open code-scanning alert did not refuse the merge, rc=$rc: $(cat "$WORK/m.alert")"
[ ! -e "$WORK/merge.log" ] || fail "merged over an open code-scanning alert"
for m in broken partial404 malformed404; do
  export CS_MODE=$m
  merge_run "cs$m"
  [ "$rc" -eq 2 ] || fail "an unreadable alert lookup ($m) did not refuse, rc=$rc: $(cat "$WORK/m.cs$m")"
  [ ! -e "$WORK/merge.log" ] || fail "merged with unreadable code-scanning alerts ($m)"
done
unset CS_MODE
export COMMENTS_FILE="$WORK/comments.json"
jq -n '[{id:777,user:{login:"alice"},created_at:"2026-09-27T17:00:00Z",updated_at:"2026-09-27T17:00:00Z",body:"Do not merge before the migration lands."}]' > "$COMMENTS_FILE"
merge_run unacked
[ "$rc" -eq 5 ] || fail "an unacknowledged comment did not refuse the merge, rc=$rc: $(cat "$WORK/m.unacked")"
grep -qF '  UNTRUSTED [comment c777] author=alice at=- | Do not merge before the migration lands.' "$WORK/m.unacked" || fail "comment not listed: $(cat "$WORK/m.unacked")"
[ ! -e "$WORK/merge.log" ] || fail "merged over an unacknowledged comment"
# --confirm-only still reconciles an out-of-band merge while a comment is unacknowledged.
MERGE_STATE=MERGED; export MERGE_STATE
seed_state
set +e; bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/confirm-blocked.out" 2>&1; rc=$?; set -e
[ "$rc" -eq 0 ] || fail "--confirm-only was blocked by an unacknowledged comment, rc=$rc: $(cat "$WORK/confirm-blocked.out")"
bash "$HERE/claims.sh" release '#42' --purge > /dev/null
MERGE_STATE=OPEN; export MERGE_STATE
d777=$(bash "$HERE/ack-comments.sh" test/repo 42 --show c777 | grep -F '[comment c777@' | sed -E 's/.*c777@([0-9a-f]+)\].*/\1/')
bash "$HERE/ack-comments.sh" test/repo 42 "c777@$d777" > "$WORK/ack.out" 2>&1 || fail "ack failed: $(cat "$WORK/ack.out")"
merge_run acked
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "an acknowledged comment still blocked the merge: $(cat "$WORK/m.acked")"
unset COMMENTS_FILE

# 8. A release cut waiting on main CI (#2191 merged over v0.85.1's run and cancelled it):
#    an in-flight `chore(release):` run on main refuses (exit 10), however many runs precede it;
#    ordinary in-flight main CI does not; an unreadable page or record refuses (exit 2).
run() { jq -nc --arg s "$1" --arg t "$2" --argjson id "$3" '{status:$s,display_title:$t,head_sha:"5f9145cbaaaa0000",name:"CI",id:$id}'; }
MAIN_RUNS_JSON="[$(for i in $(seq 1 60); do run in_progress "fix(x): y" "$i"; printf ','; done)$(run in_progress 'chore(release): v0.85.1' 900)]"
export MAIN_RUNS_JSON
merge_run release
[ "$rc" -eq 10 ] || fail "an in-flight release run (61st of 61) did not refuse, rc=$rc: $(cat "$WORK/m.release")"
grep -q 'a release cut is waiting on main CI' "$WORK/m.release" || fail "release refusal not explained: $(cat "$WORK/m.release")"
[ ! -e "$WORK/merge.log" ] || fail "merged over an in-flight release run"
MAIN_RUNS_JSON="[$(run completed 'chore(release): v0.85.1' 101),$(run in_progress 'fix(x): y (#2186)' 102),$(run queued 'docs: z' 103)]"
export MAIN_RUNS_JSON
merge_run ordinary
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "ordinary in-flight main CI blocked the merge: $(cat "$WORK/m.ordinary")"
# 8b. A release run whose publication does not need it (rc.12's metadata-only release commit
#     blocked every merge for ~15 min). It is allowed ONLY when the release tag is already on
#     origin at that exact commit AND release-ci-coverage.py (read-only, --wait 0, no --tag so
#     the unverified-hotfix exception never counts) reports the tag's coverage satisfied without
#     this run. Everything else keeps exit 10.
REL=$(git -C "$HERE" rev-parse HEAD)
rel_run() { jq -nc --arg t "$1" --arg sha "$REL" '{status:"in_progress",display_title:$t,head_sha:$sha,name:"CI",id:901}'; }
MAIN_RUNS_JSON="[$(rel_run 'chore(release): v0.86.0-rc.12')]"; export MAIN_RUNS_JSON
TAGOK=$(jq -nc --arg s "$REL" '{object:{type:"commit",sha:$s}}')
rel_case() { # label want-rc : runs merge.sh against the current TAG_*/COV_RC env
  : > "$COV_LOG"; merge_run "$1"
  # want 0 = proceeds to the merge command (this stub never reaches MERGED, so rc is 9 there)
  if [ "$2" -eq 0 ]; then [ "$rc" -ne 10 ] || fail "release case $1: refused with 10: $(cat "$WORK/m.$1")"
  else [ "$rc" -eq "$2" ] || fail "release case $1: rc=$rc, want $2: $(cat "$WORK/m.$1")"; fi
}
export TAG_REF_JSON="$TAGOK" COV_RC=0
rel_case relok 0
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "tag on origin + coverage satisfied did not merge: $(cat "$WORK/m.relok")"
grep -q 'release run .* allowed' "$WORK/m.relok" || fail "allow rule not printed: $(cat "$WORK/m.relok")"
grep -q -- "release-ci-coverage.py coverage $REL .*--wait 0" "$COV_LOG" || fail "coverage not invoked read-only: $(cat "$COV_LOG")"
if grep -q -- '--tag' "$COV_LOG"; then fail "coverage must not be invoked with --tag (the hotfix exception is not evidence)"; fi
# annotated tag: the ref names a tag object that peels to the release commit
TAG_OBJ_JSON=$(jq -nc --arg s "$REL" '{object:{type:"commit",sha:$s}}')
export TAG_REF_JSON='{"object":{"type":"tag","sha":"aaaa1111"}}' TAG_OBJ_JSON
rel_case relann 0
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "annotated tag peeling to the release commit did not merge: $(cat "$WORK/m.relann")"
unset TAG_OBJ_JSON TAG_REF_JSON
# refusals, each exit 10 with the rule named and nothing merged
rel_case reltagless 10
grep -q 'release run .* refused' "$WORK/m.reltagless" || fail "refusal rule not printed: $(cat "$WORK/m.reltagless")"
[ ! -e "$WORK/merge.log" ] || fail "merged although the release tag is not on origin (cut awaiting its CI)"
TAG_REF_JSON=$(jq -nc '{object:{type:"commit",sha:"0123456789abcdef0123456789abcdef01234567"}}'); export TAG_REF_JSON
rel_case relother 10
[ ! -e "$WORK/merge.log" ] || fail "merged although the tag points at another commit"
export TAG_REF_JSON="$TAGOK" COV_RC=1
rel_case relnocov 10
[ ! -e "$WORK/merge.log" ] || fail "merged although release-ci-coverage did not report coverage"
export COV_RC=0
MAIN_RUNS_JSON="[$(rel_run 'chore(release): bump things')]"; export MAIN_RUNS_JSON
rel_case reltitle 10
[ ! -e "$WORK/merge.log" ] || fail "merged on a release title carrying no tag"
# one allowed run does not excuse a second one that is not
MAIN_RUNS_JSON="[$(rel_run 'chore(release): v0.86.0-rc.12'),$(jq -nc '{status:"queued",display_title:"chore(release): v0.86.0-rc.13",head_sha:"5f9145cbaaaa0000",name:"CI",id:902}')]"; export MAIN_RUNS_JSON
rel_case relmixed 10
[ ! -e "$WORK/merge.log" ] || fail "merged over a second, uncovered release run"
unset TAG_REF_JSON COV_RC
unset MAIN_RUNS_JSON
# 8c. Since #4107 a merge's new main run no longer cancels a RUNNING ci.yml / kind-smoke.yml
#     main run (the incoming run's cancel-in-progress decides). A release run already in
#     progress there is allowed when live main's file has the main-finishes line ACTIVE and the
#     PR does not touch that file. Queued runs, other workflows, a cancelling or commented-out
#     policy, a PR editing the workflow, or an unreadable file keep exit 10.
WF_OK="$WORK/wf-ok.yml" WF_TRUE="$WORK/wf-true.yml" WF_COMMENT="$WORK/wf-comment.yml"
cat > "$WF_OK" <<'YAML'
concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: ${{ github.ref != 'refs/heads/main' }}
YAML
cat > "$WF_TRUE" <<'YAML'
concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true
YAML
cat > "$WF_COMMENT" <<'YAML'
concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true
  # cancel-in-progress: ${{ github.ref != 'refs/heads/main' }}
YAML
export WF_BODY_FILE="$WF_OK"
prun() { jq -nc --arg s "$1" --arg p "$2" --argjson id "$3" '{status:$s,display_title:"chore(release): v0.86.0-rc.30",head_sha:"5f9145cbaaaa0000",name:"CI",path:$p,id:$id}'; }
relrefuse() { # label: expect exit 10 and no merge
  merge_run "$1"
  [ "$rc" -eq 10 ] || fail "release case $1 did not refuse, rc=$rc: $(cat "$WORK/m.$1")"
  [ ! -e "$WORK/merge.log" ] || fail "release case $1 merged"
}
MAIN_RUNS_JSON="[$(prun in_progress .github/workflows/ci.yml 911),$(prun in_progress '.github/workflows/kind-smoke.yml@refs/heads/main' 912)]"; export MAIN_RUNS_JSON
merge_run relrunning
[ "$rc" -ne 10 ] || fail "a running release run in a no-cancel workflow still refused: $(cat "$WORK/m.relrunning")"
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "running no-cancel release run did not merge: $(cat "$WORK/m.relrunning")"
grep -q 'whose main runs finish rather than cancel' "$WORK/m.relrunning" || fail "finish rule not printed: $(cat "$WORK/m.relrunning")"
export WF_BODY_FILE="$WF_TRUE"; relrefuse relcancelling
export WF_BODY_FILE="$WF_COMMENT"; relrefuse relcommented
export WF_BODY_FILE="$WF_OK" WF_FAIL=1; relrefuse relwfunread; unset WF_FAIL
export PR_FILES_JSON='[{"filename":".github/workflows/ci.yml","status":"modified"}]'; relrefuse relprtouch; unset PR_FILES_JSON
MAIN_RUNS_JSON="[$(prun queued .github/workflows/ci.yml 913)]"; export MAIN_RUNS_JSON
relrefuse relqueued
MAIN_RUNS_JSON="[$(prun in_progress .github/workflows/codeql.yml 914)]"; export MAIN_RUNS_JSON
relrefuse relcodeql
MAIN_RUNS_JSON="[$(prun in_progress .github/workflows/ci.yml 915),$(prun queued .github/workflows/kind-smoke.yml 916)]"; export MAIN_RUNS_JSON
relrefuse relhalf
# A tab inside the title must not shift the tab-joined row: the title below would, if split,
# read as status=in_progress and path=ci.yml (the trailing @ is what running_run_finishes cuts
# the rest of a split path at), which relrunning shows is allowed. It is a queued run: refuse.
TABTITLE=$(printf 'chore(release): v0.86.0-rc.30\tin_progress\t.github/workflows/ci.yml@')
MAIN_RUNS_JSON="[$(jq -nc --arg t "$TABTITLE" '{status:"queued",display_title:$t,head_sha:"5f9145cbaaaa0000",name:"CI",path:".github/workflows/other.yml",id:917}')]"; export MAIN_RUNS_JSON
relrefuse reltabtitle
unset WF_BODY_FILE
unset MAIN_RUNS_JSON
export RUNS_FAIL=1
merge_run runsfail
[ "$rc" -eq 2 ] || fail "an unreadable main run page did not refuse, rc=$rc: $(cat "$WORK/m.runsfail")"
[ ! -e "$WORK/merge.log" ] || fail "merged with an unreadable main run list"
[ "$(wc -l < "$WORK/runs-calls.log")" -eq 1 ] || fail "runsfail: a failed gh call was retried: $(cat "$WORK/runs-calls.log")"
unset RUNS_FAIL
for bad in '{}' '[]' '[{}]' '[{"total_count":1,"workflow_runs":[{}]}]' '[{"total_count":1,"workflow_runs":[{"status":"queued","display_title":"x","head_sha":"a"}]}]' '[{"total_count":0,"workflow_runs":{}}]' \
    '[{"workflow_runs":[]}]' '[{"total_count":3,"workflow_runs":[]}]' '[{"total_count":1,"workflow_runs":[]},{"total_count":2,"workflow_runs":[]}]'; do
  export MAIN_RUNS_PAGE="$bad"
  merge_run malformed
  [ "$rc" -eq 2 ] || fail "malformed run page $bad did not refuse, rc=$rc: $(cat "$WORK/m.malformed")"
  [ ! -e "$WORK/merge.log" ] || fail "merged on malformed run page $bad"
done
unset MAIN_RUNS_PAGE
# GitHub returns at most 1,000 results for a filtered run search: 1,000 fetched records of a
# total_count of 1001 is an incomplete listing, never "no release running".
jq -n '[range(0;10) as $p | {total_count:1001,workflow_runs:[range($p*100;($p+1)*100)|{status:"in_progress",display_title:"ordinary",head_sha:"5f9145cbaaaa0000",name:"CI",id:(.+1)}]}]' > "$WORK/capped-pages.json"
export MAIN_RUNS_PAGE_FILE="$WORK/capped-pages.json"
merge_run capped
[ "$rc" -eq 2 ] || fail "1,000 of total_count 1001 did not refuse, rc=$rc: $(cat "$WORK/m.capped")"
[ ! -e "$WORK/merge.log" ] || fail "merged on a capped run listing"
unset MAIN_RUNS_PAGE_FILE
# A total_count disagreement can be a listing that changed between page reads: re-read per
# status (first read + up to 3 re-queries). Hard failures (unreadable, empty, several values,
# malformed, non-integer or over-cap counts) are never retried. MAIN_RUNS_SEQ_DIR scripts the
# n-th stdout per status; runs-calls.log records every call.
runs_calls() { # status -> number of actions/runs calls for it
  { grep -cFx -- "$1" "$WORK/runs-calls.log" 2>/dev/null || true; }
}
runs_total() { wc -l < "$WORK/runs-calls.log" | tr -d ' '; }
seq_put() { mkdir -p "$WORK/seq"; printf '%s' "$3" > "$WORK/seq/$1.$2"; } # status n raw-stdout
seq_reset() { rm -rf "$WORK/seq"; mkdir -p "$WORK/seq"; export MAIN_RUNS_SEQ_DIR="$WORK/seq"; }
ordrun() { jq -nc --argjson id "$1" '{status:"in_progress",display_title:"ordinary",head_sha:"5f9145cbaaaa0000",name:"CI",id:$id}'; }
EMPTYPG='[{"total_count":0,"workflow_runs":[]}]'
MISPG="[{\"total_count\":2,\"workflow_runs\":[$(ordrun 1),$(ordrun 2)]},{\"total_count\":3,\"workflow_runs\":[$(ordrun 3)]}]"
# 1. pages that disagree once, then agree: the guard passes after one re-read
seq_reset; seq_put in_progress 1 "$MISPG"; seq_put in_progress 2 "$EMPTYPG"
merge_run retrypages
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "case retrypages: merge stub not reached: $(cat "$WORK/m.retrypages")"
[ "$(runs_calls in_progress)" -eq 2 ] || fail "case retrypages: in_progress calls $(runs_calls in_progress), want 2"
grep -qF "main's in_progress run listing changed while paging (total_count mismatch); re-reading (retry 1/3)" "$WORK/m.retrypages" || fail "case retrypages: retry line missing: $(cat "$WORK/m.retrypages")"
# 2. fewer records than the total, then consistent
seq_reset; seq_put in_progress 1 "[{\"total_count\":3,\"workflow_runs\":[$(ordrun 1),$(ordrun 2)]},{\"total_count\":3,\"workflow_runs\":[]}]"; seq_put in_progress 2 "$EMPTYPG"
merge_run retryshort
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "case retryshort: merge stub not reached: $(cat "$WORK/m.retryshort")"
[ "$(runs_calls in_progress)" -eq 2 ] || fail "case retryshort: in_progress calls $(runs_calls in_progress), want 2"
# 3. the retry finds a release run in a cancelling workflow
seq_reset; seq_put in_progress 1 "$MISPG"
seq_put in_progress 2 "[{\"total_count\":1,\"workflow_runs\":[$(prun in_progress .github/workflows/ci.yml 921)]}]"
export WF_BODY_FILE="$WF_TRUE"
merge_run retryrelease
unset WF_BODY_FILE
[ "$rc" -eq 10 ] || fail "case retryrelease: release run found on the re-read did not refuse, rc=$rc: $(cat "$WORK/m.retryrelease")"
[ ! -e "$WORK/merge.log" ] || fail "case retryrelease: merged over a release run seen on the re-read"
[ "$(runs_calls in_progress)" -eq 2 ] || fail "case retryrelease: in_progress calls $(runs_calls in_progress), want 2"
# 4. a mismatch on every read refuses after exactly 4 reads
seq_reset; for n in 1 2 3 4 5; do seq_put in_progress "$n" "$MISPG"; done
merge_run retrypersist
[ "$rc" -eq 2 ] || fail "case retrypersist: persistent mismatch rc=$rc: $(cat "$WORK/m.retrypersist")"
[ ! -e "$WORK/merge.log" ] || fail "case retrypersist: merged on a persistent mismatch"
[ "$(runs_calls in_progress)" -eq 4 ] || fail "case retrypersist: in_progress calls $(runs_calls in_progress), want 4"
grep -qF "main's in_progress run listing changed while paging (total_count mismatch); re-reading (retry 3/3)" "$WORK/m.retrypersist" || fail "case retrypersist: retry 3/3 line missing: $(cat "$WORK/m.retrypersist")"
grep -qF "cannot parse main's in_progress workflow runs (total_count mismatch persisted across 4 reads); not merging" "$WORK/m.retrypersist" || fail "case retrypersist: persistence message missing: $(cat "$WORK/m.retrypersist")"
# 5. empty stdout with rc 0 is a parse failure, not "no release run" (green-merge setup)
unset MAIN_RUNS_SEQ_DIR
# CONFIRM_AFTER_MERGE=1 makes a merge that reaches the merge command confirm as MERGED, so the
# pre-fix behaviour (no release run found, merge proceeds) exits 0 here instead of 9.
PRIOR_CAM="${CONFIRM_AFTER_MERGE-__unset__}"; export CONFIRM_AFTER_MERGE=1
export MAIN_RUNS_EMPTY=1
merge_run emptyout
unset MAIN_RUNS_EMPTY
[ "$rc" -eq 2 ] || fail "case emptyout: empty run listing rc=$rc: $(cat "$WORK/m.emptyout")"
[ ! -e "$WORK/merge.log" ] || fail "case emptyout: merged on an empty run listing"
[ "$(runs_total)" -eq 1 ] || fail "case emptyout: calls $(runs_total), want 1"
grep -qF "cannot parse main's queued workflow runs; not merging" "$WORK/m.emptyout" || fail "case emptyout: parse refusal missing: $(cat "$WORK/m.emptyout")"
# 6. two concatenated, individually valid page arrays are not one page set
export MAIN_RUNS_PAGE="${EMPTYPG}${EMPTYPG}"
merge_run concatpages
unset MAIN_RUNS_PAGE
[ "$rc" -eq 2 ] || fail "case concatpages: concatenated page arrays rc=$rc: $(cat "$WORK/m.concatpages")"
[ ! -e "$WORK/merge.log" ] || fail "case concatpages: merged on concatenated page arrays"
[ "$(runs_total)" -eq 1 ] || fail "case concatpages: calls $(runs_total), want 1"
if [ "$PRIOR_CAM" = __unset__ ]; then unset CONFIRM_AFTER_MERGE; else export CONFIRM_AFTER_MERGE="$PRIOR_CAM"; fi
# Mutation controls: hard failures are never retried (1 call, exit 2, no merge).
ctl() { # label status-1 raw: exit 2 after exactly one call
  seq_reset; seq_put queued 1 "$2"
  merge_run "$1"
  [ "$rc" -eq 2 ] || fail "control $1: rc=$rc, want 2: $(cat "$WORK/m.$1")"
  [ ! -e "$WORK/merge.log" ] || fail "control $1: merged"
  [ "$(runs_total)" -eq 1 ] || fail "control $1: calls $(runs_total), want 1"
}
ctl ctlnonarray '[7]'
ctl ctlbadrec "[{\"total_count\":2,\"workflow_runs\":[{}]},{\"total_count\":3,\"workflow_runs\":[]}]"
ctl ctlbadpath "[{\"total_count\":2,\"workflow_runs\":[{\"status\":\"queued\",\"display_title\":\"x\",\"head_sha\":\"a\",\"id\":1,\"path\":{}}]},{\"total_count\":3,\"workflow_runs\":[]}]"
ctl ctlcap "$(cat "$WORK/capped-pages.json")"
ctl ctlnegative '[{"total_count":-1,"workflow_runs":[]}]'
ctl ctlfraction '[{"total_count":1.5,"workflow_runs":[]}]'
unset MAIN_RUNS_SEQ_DIR
# --expect-closes: GitHub's closingIssuesReferences counts negated prose too (PR #2629's
# "this PR does not close #1912" closed #1912 on merge). A mismatch refuses before the merge.
export CLOSES_JSON='[{"number":1912}]'
MERGE_ARGS=(--expect-closes none)
merge_run closes_negated
[ "$rc" -eq 11 ] || fail "a Refs-only PR that GitHub would close #1912 with did not refuse, rc=$rc: $(cat "$WORK/m.closes_negated")"
grep -q 'merging would close \[1912\], expected \[none\]' "$WORK/m.closes_negated" || fail "closing mismatch not named: $(cat "$WORK/m.closes_negated")"
[ ! -e "$WORK/merge.log" ] || fail "merged although it would close an issue meant to stay open"
MERGE_ARGS=(--expect-closes '#1912')
merge_run closes_match
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "matching closing references blocked the merge: $(cat "$WORK/m.closes_match")"
export CLOSES_JSON='[{"number":7},{"number":12}]'
MERGE_ARGS=(--expect-closes '12,7')
merge_run closes_set
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "an equal set in another order blocked the merge: $(cat "$WORK/m.closes_set")"
MERGE_ARGS=(--expect-closes 7)
merge_run closes_extra
[ "$rc" -eq 11 ] || fail "an extra closing reference did not refuse, rc=$rc: $(cat "$WORK/m.closes_extra")"
[ ! -e "$WORK/merge.log" ] || fail "merged with an unexpected closing reference"
for bad in 'seven' '1#2' '7,' ',7' '7,,12' '##7' '7 12' ''; do
  MERGE_ARGS=(--expect-closes "$bad")
  merge_run closes_bad
  [ "$rc" -eq 2 ] || fail "malformed --expect-closes '$bad' did not refuse, rc=$rc: $(cat "$WORK/m.closes_bad")"
  [ ! -e "$WORK/merge.log" ] || fail "merged on malformed --expect-closes '$bad'"
done
MERGE_ARGS=(--expect-closes ' #12 , 7 ')
merge_run closes_blanks
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "surrounding blanks and a leading # blocked a matching set: $(cat "$WORK/m.closes_blanks")"
export CLOSES_FAIL=1
MERGE_ARGS=(--expect-closes none)
merge_run closes_unreadable
[ "$rc" -eq 2 ] || fail "unreadable closing references did not refuse, rc=$rc: $(cat "$WORK/m.closes_unreadable")"
[ ! -e "$WORK/merge.log" ] || fail "merged with unreadable closing references"
MERGE_ARGS=()
merge_run closes_noflag
grep -q 'CLOSES=unknown' "$WORK/m.closes_noflag" || fail "no-flag run did not report unknown closing references: $(cat "$WORK/m.closes_noflag")"
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "without --expect-closes an unreadable lookup must stay informational: $(cat "$WORK/m.closes_noflag")"
unset CLOSES_FAIL
# Commit messages and the PR title reach main in the squash commit and close issues there,
# outside closingIssuesReferences (PR #2651's squash commit 4d64d9a4 quoted the negated phrase).
export CLOSES_JSON='[]'
export COMMITS_JSON='[{"commit":{"message":"skills(x): y\n\nGitHub parses \"this PR does not close #1912\" as a keyword."}}]'
MERGE_ARGS=(--expect-closes none)
merge_run closes_commit
[ "$rc" -eq 11 ] || fail "a closing keyword in a commit message did not refuse, rc=$rc: $(cat "$WORK/m.closes_commit")"
grep -q 'merging would close \[1912\]' "$WORK/m.closes_commit" || fail "commit-message closing ref not named: $(cat "$WORK/m.closes_commit")"
[ ! -e "$WORK/merge.log" ] || fail "merged although a commit message would close #1912"
export COMMITS_JSON='[{"commit":{"message":"Fixes: test/repo#7\nRESOLVED #12\nRefs #99, prefix#5 not a ref"}}]'
MERGE_ARGS=(--expect-closes '7,12')
merge_run closes_grammar
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "keyword grammar (colon, same-repo OWNER/REPO#, case) not parsed as 7,12: $(cat "$WORK/m.closes_grammar")"
export CLOSES_JSON='[{"number":12}]'
merge_run closes_union
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "body and commit references were not unioned: $(cat "$WORK/m.closes_union")"
# Another repository's issue is a different issue: it keeps its identity and never matches N.
export CLOSES_JSON='[]' COMMITS_JSON='[{"commit":{"message":"Fixes other/repo#7"}}]'
MERGE_ARGS=(--expect-closes 7)
merge_run closes_foreign
[ "$rc" -eq 11 ] || fail "a foreign other/repo#7 matched --expect-closes 7, rc=$rc: $(cat "$WORK/m.closes_foreign")"
grep -q 'merging would close \[other/repo#7\]' "$WORK/m.closes_foreign" || fail "foreign ref identity not kept: $(cat "$WORK/m.closes_foreign")"
[ ! -e "$WORK/merge.log" ] || fail "merged with a foreign closing reference"
# A commit list shorter than the PR's commit count (REST caps at 250) fails closed.
export COMMITS_JSON='[{"commit":{"message":"x"}}]' COMMIT_COUNT=3
MERGE_ARGS=(--expect-closes none)
merge_run closes_truncated
[ "$rc" -eq 2 ] || fail "an incomplete commit list did not refuse, rc=$rc: $(cat "$WORK/m.closes_truncated")"
[ ! -e "$WORK/merge.log" ] || fail "merged on an incomplete commit list"
unset COMMIT_COUNT
# The PR title alone can close an issue.
export COMMITS_JSON='[]' PR_TITLE='Fixes #44 in the lander'
merge_run closes_title
[ "$rc" -eq 11 ] || fail "a closing keyword in the PR title did not refuse, rc=$rc: $(cat "$WORK/m.closes_title")"
grep -q 'merging would close \[44\]' "$WORK/m.closes_title" || fail "title closing ref not named: $(cat "$WORK/m.closes_title")"
unset PR_TITLE
# A keyword must end at a word boundary before its separator: an owner named fixes/ or
# closed/ in a Refs line is not a keyword (it was parsed as "fix" + "es/repo#7").
export COMMITS_JSON='[{"commit":{"message":"Refs fixes/repo#7 and closed/repo#8\nfixes #10, close:#11, Resolved\t#12, fix#13"}}]'
MERGE_ARGS=(--expect-closes '10,11,12,13')
merge_run closes_owner_names
grep -q -- '--match-head-commit' "$WORK/merge.log" 2>/dev/null || fail "owner names fixes/ closed/ parsed as keywords, or a separator form missed: $(cat "$WORK/m.closes_owner_names")"
grep -q 'CLOSES=10,11,12,13' "$WORK/m.closes_owner_names" || fail "unexpected parse: $(cat "$WORK/m.closes_owner_names")"
unset CLOSES_JSON COMMITS_JSON; MERGE_ARGS=()
# Last-moment migration checks use live base max, even when all CI is green.
export CONFIRM_AFTER_MERGE=1
export BASE_MIG_JSON='[{"name":"00372_retained.sql"}]'
for version in 00372 00371; do
  export PR_FILES_JSON="[{\"filename\":\"api/internal/store/migrations/${version}_vault.sql\",\"status\":\"added\"}]"
  merge_run "migration_$version"
  [ "$rc" -eq 13 ] || fail "migration $version: want 13, got $rc: $(cat "$WORK/m.migration_$version")"
  [ ! -e "$WORK/merge.log" ] || fail "colliding migration reached gh pr merge"
  grep -qF 'MIGRATION_COLLISION: renumber above 372 with land-prep.sh' "$WORK/m.migration_$version" || fail "migration remediation missing"
done
export PR_FILES_JSON='[{"filename":"api/internal/store/migrations/00373_vault.sql","status":"added"}]'
merge_run migration_above
[ "$rc" -eq 0 ] && [ -s "$WORK/merge.log" ] || fail "migration above head did not merge: $(cat "$WORK/m.migration_above")"
for status in renamed copied; do
  export PR_FILES_JSON="[{\"filename\":\"api/internal/store/migrations/00371_vault.sql\",\"status\":\"$status\",\"previous_filename\":\"api/internal/store/migrations/99999_source.sql\"}]"
  merge_run "migration_${status}_below"
  [ "$rc" -eq 13 ] || fail "$status below head: want 13, got $rc: $(cat "$WORK/m.migration_${status}_below")"
  [ ! -e "$WORK/merge.log" ] || fail "$status below head reached gh pr merge"
  export PR_FILES_JSON="[{\"filename\":\"api/internal/store/migrations/00373_vault.sql\",\"status\":\"$status\",\"previous_filename\":\"api/internal/store/migrations/00372_source.sql\"}]"
  merge_run "migration_${status}_above"
  [ "$rc" -eq 0 ] && [ -s "$WORK/merge.log" ] || fail "$status above head did not merge: $(cat "$WORK/m.migration_${status}_above")"
done
# An existing migration edited in the PR is not an added migration.
export PR_FILES_JSON='[{"filename":"api/internal/store/migrations/00372_retained.sql","status":"modified"}]'
merge_run migration_modified
[ "$rc" -eq 0 ] || fail "modified migration misclassified as added"
export PR_FILES_JSON='[{"filename":"api/internal/store/migrations/00373_vault.sql","status":"added"}]'
for scenario in files_failed files_malformed files_entry_malformed base_failed base_malformed; do
  case "$scenario" in
    files_failed) export PR_FILES_FAIL=1 ;;
    files_malformed) export PR_FILES_RAW='{}' ;;
    files_entry_malformed) export PR_FILES_RAW='[{"filename":"api/internal/store/migrations/00373_vault.sql"}]' ;;
    base_failed) export BASE_MIG_FAIL=1 ;;
    base_malformed) export BASE_MIG_JSON='{}' ;;
  esac
  merge_run "migration_$scenario"
  [ "$rc" -eq 2 ] || fail "$scenario: want unreadable=2, got $rc: $(cat "$WORK/m.migration_$scenario")"
  [ ! -e "$WORK/merge.log" ] || fail "$scenario reached gh pr merge"
  unset PR_FILES_FAIL PR_FILES_RAW BASE_MIG_FAIL
  export BASE_MIG_JSON='[{"name":"00372_retained.sql"}]'
done
unset PR_FILES_JSON BASE_MIG_JSON CONFIRM_AFTER_MERGE
# --confirm-only never reads the run list.
MERGE_STATE=MERGED; export MERGE_STATE RUNS_FAIL=1
seed_state
set +e; bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/confirm-runs.out" 2>&1; rc=$?; set -e
[ "$rc" -eq 0 ] || fail "--confirm-only read main's run list, rc=$rc: $(cat "$WORK/confirm-runs.out")"
bash "$HERE/claims.sh" release '#42' --purge > /dev/null
MERGE_STATE=OPEN; export MERGE_STATE
unset RUNS_FAIL CHECKS_JSON CHECKS_RC
fi

# Real local history plus the existing fake forge: all CI is green, but only the
# policy delta since this branch's own merge-base decides freshness (#3550).
REAL_GIT=$(command -v git)
export REAL_GIT POLICY_ROOT="$WORK/policy" POLICY_REMOTE="$WORK/policy-origin.git"
export POLICY_SCRIPT_DIR="$HERE" POLICY_BASE_GH="$WORK/bin/gh" POLICY_GIT_LOG="$WORK/policy-git.log" POLICY_GH_LOG="$WORK/policy-gh.log"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_ALLOW_PROTOCOL=file
export SESSION_PEERS_NAME=policy-fixture SESSION_PEERS_UUID=policy-fixture SESSION_PEERS_KIND=codex
mkdir -p "$WORK/policy-bin" "$POLICY_ROOT"
"$REAL_GIT" -c core.fsmonitor=false init -q -b main "$POLICY_ROOT"
"$REAL_GIT" -C "$POLICY_ROOT" config user.name fixture
"$REAL_GIT" -C "$POLICY_ROOT" config user.email fixture@example.invalid
mkdir -p "$POLICY_ROOT/scripts/lib" "$POLICY_ROOT/api"
printf 'base\n' > "$POLICY_ROOT/README.md"
printf 'base\n' > "$POLICY_ROOT/api/doc.md"
# Intentionally independent of merge.sh's array: deleting a production path must fail.
POLICIES=(scripts/check-changelog-entry.sh scripts/lib/shipping-paths.sh scripts/lib/dependency-bump.sh
  scripts/lib/changelog-fragments.sh scripts/lib/changelog-fragments.awk scripts/assert-changelog-covers-release.sh)
for path in "${POLICIES[@]}"; do printf 'old policy\n' > "$POLICY_ROOT/$path"; done
"$REAL_GIT" -C "$POLICY_ROOT" add .
"$REAL_GIT" -C "$POLICY_ROOT" commit -qm base
"$REAL_GIT" clone -q --bare "$POLICY_ROOT" "$POLICY_REMOTE"
"$REAL_GIT" -C "$POLICY_ROOT" remote add origin https://github.com/vtmocanu/uzi.git
cat > "$WORK/policy-bin/git" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = -C ] && [ "${2:-}" = "$POLICY_SCRIPT_DIR" ]; then
  shift 2; set -- -C "$POLICY_ROOT" "$@"
fi
case " $* " in
  *' config --get remote.origin.url '*)
    if [ "${POLICY_ORIGIN_MISMATCH:-0}" = 1 ]; then echo https://github.com/other/repo.git; exit 0; fi ;;
  *' fetch '*)
    if [ "${POLICY_LOCK_STATE_FAIL:-0}" = 1 ]; then
      [ ! -f "$UZI_LANDER_STATE_DIR/locks/merge/uuid" ] || { echo 'expected unavailable lock state' >&2; exit 99; }
    else
      [ -f "$UZI_LANDER_STATE_DIR/locks/merge/uuid" ] || { echo 'fetch outside merge lock' >&2; exit 99; }
    fi
    printf '%s\n' "$*" >> "$POLICY_GIT_LOG"
    [ "${POLICY_FETCH_FAIL:-0}" != 1 ] || exit 88 ;;
  *' diff '*)
    [ "${POLICY_DIFF_FAIL:-0}" != 1 ] || exit 89
    # Fails only the fragment listing (its pathspec is changelog.d), not the policy diff.
    if [ "${POLICY_FRAGMENT_DIFF_FAIL:-0}" = 1 ] && [[ " $* " == *' -- changelog.d '* ]]; then exit 90; fi ;;
  *' cat-file blob '*) [ "${POLICY_FRAGMENT_BLOB_FAIL:-0}" != 1 ] || exit 91 ;;
esac
exec "$REAL_GIT" -c core.fsmonitor=false -c "url.${POLICY_REMOTE}.insteadOf=https://github.com/vtmocanu/uzi.git" "$@"
STUB
cat > "$WORK/policy-bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$POLICY_GH_LOG"
# POLICY_BASE_REF serves another base branch (its rules answer as main's); POLICY_BASE_JSON
# sets a raw baseRefName value (a non-string).
args=()
for a in "$@"; do
  if [ -n "${POLICY_BASE_REF:-}" ] && [[ "$a" == */rules/branches/"$POLICY_BASE_REF" ]]; then a=${a%/*}/main; fi
  args+=("$a")
done
if [ "${1:-}" = pr ] && [ "${2:-}" = view ] && [[ "$*" == *headRefOid* ]]; then
  "$POLICY_BASE_GH" "${args[@]}" | jq --arg h "$POLICY_HEAD" --argjson b "${POLICY_BASE_JSON:-null}" --arg r "${POLICY_BASE_REF:-}" \
    '.headRefOid=$h | if $b != null then .baseRefName=$b elif $r != "" then .baseRefName=$r else . end' |
    if [[ "$*" == *statusCheckRollup* ]]; then
      jq --argjson labels "${POLICY_LABELS:-[]}" --argjson checks "${POLICY_ROLLUP:-[]}" \
        --arg head "${ELIGIBILITY_HEAD:-$POLICY_HEAD}" '. + {headRefOid:$head,labels:$labels,statusCheckRollup:$checks}'
    else cat; fi
else
  exec "$POLICY_BASE_GH" "${args[@]}"
fi
STUB
REAL_GREP=$(command -v grep); export REAL_GREP
cat > "$WORK/policy-bin/grep" <<'STUB'
#!/usr/bin/env bash
# POLICY_FRAGMENT_GREP_FAIL fails only the fragment path filter (its uuid pattern), as a grep error.
if [ "${POLICY_FRAGMENT_GREP_FAIL:-0}" = 1 ]; then
  for a in "$@"; do case "$a" in *'changelog\.d/[0-9a-f]{8}'*) exit 2 ;; esac; done
fi
exec "$REAL_GREP" "$@"
STUB
chmod +x "$WORK/policy-bin/git" "$WORK/policy-bin/gh" "$WORK/policy-bin/grep"
export PATH="$WORK/policy-bin:$PATH" CONFIRM_AFTER_MERGE=1 MERGE_STATE=OPEN
export CHECKS_JSON='[{"name":"ci","bucket":"pass"}]' CHECKS_RC=0
export RULES_JSON='[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}]}}]'
policy_run() {
  local repo=${1:-vtmocanu/uzi}
  if [ "$#" -gt 0 ]; then shift; fi
  rm -f "$WORK/merge.log"
  set +e
  if [ "${POLICY_REWORK:-0}" = 1 ]; then
    bash "$SCRIPT" "$repo" 42 --expect-head "$POLICY_HEAD" "$@" > "$WORK/policy.out" 2>&1
  else
    bash "$SCRIPT" "$repo" 42 --expect-head "$POLICY_HEAD" --no-rework-check "$@" > "$WORK/policy.out" 2>&1
  fi
  rc=$?
  set -e
}
for path in "${POLICIES[@]}"; do
  git -C "$POLICY_ROOT" checkout -q -B topic main
  printf '%s\n' "$path" > "$POLICY_ROOT/api/doc.md"
  git -C "$POLICY_ROOT" commit -qam 'docs: shipping mirror without a fragment'
  POLICY_HEAD=$(git -C "$POLICY_ROOT" rev-parse HEAD); export POLICY_HEAD
  git -C "$POLICY_ROOT" checkout -q main
  printf 'new policy\n' > "$POLICY_ROOT/$path"
  git -C "$POLICY_ROOT" commit -qam 'tighten policy'
  git -C "$POLICY_ROOT" push -q origin main
  policy_run
  [ "$rc" -eq 12 ] || fail "policy-stale '$path': want 12, got $rc: $(cat "$WORK/policy.out")"
  [ ! -e "$WORK/merge.log" ] || fail "stale-policy merge reached the forge"
  grep -qF "$path" "$WORK/policy.out" || fail "stale-policy refusal omitted $path"
  grep -qF 'fresh exact-head CI' "$WORK/policy.out" || fail "stale-policy refusal omitted recovery"
done
for repo in Vtmocanu/Uzi github.com/vtmocanu/uzi https://github.com/vtmocanu/uzi; do
  policy_run "$repo"
  [ "$rc" -eq 12 ] || fail "policy repository spelling '$repo' bypassed the guard: $(cat "$WORK/policy.out")"
done
for repo in otherhost/vtmocanu/uzi git@github.com:vtmocanu/uzi vtmocanu/uzi/ vtmocanu/uzi.git vtmocanu/..; do
  rm -f "$POLICY_GH_LOG"
  policy_run "$repo"
  [ "$rc" -eq 2 ] || fail "malformed repository '$repo' did not refuse before forge reads"
  [ ! -s "$POLICY_GH_LOG" ] || fail 'malformed repository reached the forge'
done
policy_run vtmocanu/uzi --no-admin
[ "$rc" -eq 12 ] || fail "policy-no-admin bypassed the guard: $(cat "$WORK/policy.out")"
[ ! -e "$WORK/merge.log" ] || fail 'stale policy reached a non-admin merge'
POLICY_STALE_HEAD=$POLICY_HEAD
git -C "$POLICY_ROOT" checkout -q topic
git -C "$POLICY_ROOT" rebase -q origin/main
POLICY_HEAD=$(git -C "$POLICY_ROOT" rev-parse HEAD); export POLICY_HEAD
policy_run
[ "$rc" -eq 0 ] || fail "policy-rebased: $(cat "$WORK/policy.out")"
[ -s "$WORK/merge.log" ] || fail "rebased policy did not reach merge"
grep -qF -- "--match-head-commit $POLICY_HEAD" "$WORK/merge.log" || fail 'policy check lost exact-head merge binding'
git -C "$POLICY_ROOT" checkout -q main
printf 'unrelated\n' >> "$POLICY_ROOT/README.md"
git -C "$POLICY_ROOT" commit -qam 'unrelated main change'
git -C "$POLICY_ROOT" push -q origin main
policy_run
[ "$rc" -eq 0 ] || fail "policy-unrelated-main: $(cat "$WORK/policy.out")"
[ -s "$WORK/merge.log" ] || fail 'unrelated main change prevented merging'
export POLICY_FETCH_FAIL=1
policy_run
[ "$rc" -eq 2 ] || fail "policy-fetch-error: expected instrument refusal, got $rc"
[ ! -e "$WORK/merge.log" ] || fail 'merged after policy fetch failed'
unset POLICY_FETCH_FAIL
export POLICY_DIFF_FAIL=1
policy_run
[ "$rc" -eq 2 ] || fail "policy-diff-error: expected instrument refusal, got $rc"
[ ! -e "$WORK/merge.log" ] || fail 'merged after policy diff failed'
unset POLICY_DIFF_FAIL
export POLICY_ORIGIN_MISMATCH=1
policy_run
[ "$rc" -eq 2 ] || fail "policy-origin-mismatch: expected refusal, got $rc"
[ ! -e "$WORK/merge.log" ] || fail 'merged using policy from another repository'
unset POLICY_ORIGIN_MISMATCH
# A PR may propose a policy edit itself: compare main's delta, not head..main.
git -C "$POLICY_ROOT" checkout -q -B policy-edit main
printf 'branch policy improvement\n' >> "$POLICY_ROOT/scripts/check-changelog-entry.sh"
git -C "$POLICY_ROOT" commit -qam 'propose a policy edit'
POLICY_HEAD=$(git -C "$POLICY_ROOT" rev-parse HEAD); export POLICY_HEAD
policy_run
[ "$rc" -eq 0 ] || fail "policy-own-edit: $(cat "$WORK/policy.out")"
[ -s "$WORK/merge.log" ] || fail 'branch-owned policy edit was refused'

# The actual head exists only on the bare origin, under its pull ref. This proves
# a real missing-object fetch, not a mocked cat-file failure in the same clone.
git clone -q --no-hardlinks "$POLICY_REMOTE" "$WORK/pr-only"
git -C "$WORK/pr-only" config user.name fixture
git -C "$WORK/pr-only" config user.email fixture@example.invalid
printf 'remote-only PR\n' > "$WORK/pr-only/api/doc.md"
git -C "$WORK/pr-only" commit -qam 'remote-only PR head'
POLICY_HEAD=$(git -C "$WORK/pr-only" rev-parse HEAD); export POLICY_HEAD
git -C "$WORK/pr-only" push -q origin HEAD:refs/pull/42/head
if git -C "$POLICY_ROOT" cat-file -e "$POLICY_HEAD^{commit}" 2>/dev/null; then fail 'remote-only head was already local'; fi
policy_run
[ "$rc" -eq 0 ] || fail "policy-remote-head: $(cat "$WORK/policy.out")"
grep -qF "origin $POLICY_HEAD" "$POLICY_GIT_LOG" || fail 'remote-only head did not exercise by-SHA fetch'
git -C "$POLICY_ROOT" cat-file -e "$POLICY_HEAD^{commit}" || fail 'exact remote head was not imported'

# A persistent state-dir failure already blocks acknowledgement reads. Model a
# failure only at lock acquisition (second state_dir call), preserving that path.
REAL_MKDIR=$(command -v mkdir); export REAL_MKDIR POLICY_STATE_CALLS="$WORK/policy-state-calls"
cat > "$WORK/policy-bin/mkdir" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${POLICY_LOCK_STATE_FAIL:-0}" = 1 ] && [ "${1:-}" = -p ] && [ "${2:-}" = "$UZI_LANDER_STATE_DIR/trail" ]; then
  count=0; if [ -f "$POLICY_STATE_CALLS" ]; then count=$(cat "$POLICY_STATE_CALLS"); fi
  count=$((count+1)); printf '%s\n' "$count" > "$POLICY_STATE_CALLS"
  [ "$count" -ne 2 ] || exit 1
fi
exec "$REAL_MKDIR" "$@"
STUB
chmod +x "$WORK/policy-bin/mkdir"
POLICY_FRESH_HEAD=$POLICY_HEAD
export POLICY_LOCK_STATE_FAIL=1
rm -f "$POLICY_STATE_CALLS"
policy_run
[ "$rc" -eq 0 ] || fail "policy-no-lock-fresh: $(cat "$WORK/policy.out")"
[ -s "$WORK/merge.log" ] || fail 'fresh policy was refused when the lock state directory was unavailable'
POLICY_HEAD=$POLICY_STALE_HEAD; export POLICY_HEAD
rm -f "$POLICY_STATE_CALLS"
policy_run
[ "$rc" -eq 12 ] || fail "policy-no-lock-stale: $(cat "$WORK/policy.out")"
[ ! -e "$WORK/merge.log" ] || fail 'missing lock skipped stale-policy protection'
unset POLICY_LOCK_STATE_FAIL
POLICY_HEAD=$POLICY_FRESH_HEAD; export POLICY_HEAD
fetches_before=$(wc -l < "$POLICY_GIT_LOG")
export MERGE_STATE=MERGED POLICY_FETCH_FAIL=1 POLICY_HEAD=$POLICY_STALE_HEAD
rm -f "$WORK/merge.log"
set +e
bash "$SCRIPT" vtmocanu/uzi 42 --confirm-only > "$WORK/policy-confirm.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "policy-confirm-only: $(cat "$WORK/policy-confirm.out")"
[ ! -e "$WORK/merge.log" ] || fail 'confirm-only attempted another merge'
[ "$(wc -l < "$POLICY_GIT_LOG")" = "$fetches_before" ] || fail 'confirm-only fetched policy after an out-of-band merge'
unset POLICY_FETCH_FAIL
[ -s "$POLICY_GIT_LOG" ] || fail 'policy freshness never fetched under the merge lock'

# Changelog fragment citation guard (#4669): two or more fragments added by the PR need one
# headline citing the PR number or a same-repo issue the merge closes. PR is #42.
export MERGE_STATE=OPEN
pol_reset() {
  unset CLOSES_JSON CLOSES_JSON_LATER CLOSES_SEQ_FILE CLOSES_FAIL COMMITS_JSON COMMIT_COUNT COMMIT_PAGES_JSON PR_META_JSON \
    PR_TITLE FRAG_DESC POLICY_BASE_REF POLICY_BASE_JSON POLICY_FRAGMENT_DIFF_FAIL POLICY_FRAGMENT_BLOB_FAIL POLICY_FRAGMENT_GREP_FAIL
  MERGE_ARGS=()
}
pol_commits() { # [message of the first commit]: a stub commit list matching the topic branch
  local n; n=$(git -C "$POLICY_ROOT" rev-list --count main..HEAD)
  COMMITS_JSON=$(jq -cn --argjson n "$n" --arg m "${1:-docs: fixture}" '[range(0;$n) | {commit:{message:(if . == 0 then $m else "docs: fixture" end)}}]')
  COMMIT_COUNT=$n; export COMMITS_JSON COMMIT_COUNT
}
pol_pr() { # branch, then N=headline per fragment (one commit each); none = a docs-only commit
  local name=$1 spec n; shift
  git -C "$POLICY_ROOT" checkout -q -B "$name" main
  mkdir -p "$POLICY_ROOT/changelog.d"
  for spec in "$@"; do
    n=${spec%%=*}
    printf '### Fixed\n\n- **%s**\n  %s\n' "${spec#*=}" "${FRAG_DESC:-Description line.}" > "$POLICY_ROOT/changelog.d/$(printf '%08x-1111-4111-8111-%012x' "$n" "$n").md"
    git -C "$POLICY_ROOT" add changelog.d
    git -C "$POLICY_ROOT" commit -qm "docs: fragment $n"
  done
  if [ "$#" -eq 0 ]; then printf '%s\n' "$name" > "$POLICY_ROOT/api/doc.md"; git -C "$POLICY_ROOT" commit -qam 'docs: no fragment'; fi
  POLICY_HEAD=$(git -C "$POLICY_ROOT" rev-parse HEAD); export POLICY_HEAD
  pol_commits
}
pol_want() { # label, rc, 1 = merged | 0 = not merged
  [ "$rc" -eq "$2" ] || fail "fragment guard $1: want rc $2, got $rc: $(cat "$WORK/policy.out")"
  if [ "$3" = 1 ]; then
    [ -s "$WORK/merge.log" ] || fail "fragment guard $1: did not merge: $(cat "$WORK/policy.out")"
  else
    [ ! -e "$WORK/merge.log" ] || fail "fragment guard $1: reached the forge merge"
  fi
}
pol_same_repo() { printf '[{"number":%s,"repository":{"owner":{"login":"vtmocanu"},"name":"uzi"}}]' "$1"; }
F1=$(printf 'changelog.d/%08x-1111-4111-8111-%012x.md' 1 1); F2=$(printf 'changelog.d/%08x-1111-4111-8111-%012x.md' 2 2)

# 1. Regression: two fragments citing only the issue, nothing closed, no PR number.
pol_reset; pol_pr frag-1 '1=Fix a (#4626).' '2=Fix b (#4626).'
policy_run; pol_want uncited 15 0
grep -qF "$F1" "$WORK/policy.out" && grep -qF "$F2" "$WORK/policy.out" || fail "uncited refusal omitted a fragment path: $(cat "$WORK/policy.out")"
grep -qF 'CHANGELOG_FRAGMENTS_UNCITED' "$WORK/policy.out" || fail 'uncited refusal lacks its marker'
grep -qF 'add the PR number to one headline' "$WORK/policy.out" || grep -qF 'Add the PR number to one headline' "$WORK/policy.out" || fail 'uncited refusal lacks the fix'
grep -qF '(#<issue>, #42)' "$WORK/policy.out" || fail 'uncited refusal lacks the example'
# 2./3. The PR number, bare or as this repository's owner/repo#N, in any one headline.
for cite in '(#4626, #42)' 'vtmocanu/uzi#42' 'Vtmocanu/Uzi#42' '(#042)' '#42.' '[#42]'; do
  pol_reset; pol_pr frag-2 "1=Fix a $cite" '2=Fix b (#4626).'
  policy_run; pol_want "cite $cite" 0 1
done
# 4. A same-repo issue the merge closes (body reference or commit-message keyword) counts.
pol_reset; pol_pr frag-4 '1=Fix a (#4626).' '2=Fix b (#4626).'
export CLOSES_JSON; CLOSES_JSON=$(pol_same_repo 4626)
policy_run vtmocanu/uzi --expect-closes 4626; pol_want 'closed same-repo issue' 0 1
pol_reset; pol_pr frag-4b '1=Fix a (#4626).' '2=Fix b (#4626).'; pol_commits 'fix(x): y

Closes #4626'
policy_run vtmocanu/uzi --expect-closes 4626; pol_want 'closed by a commit keyword' 0 1
# 5. Another repository's reference never counts, as a citation or as a closing reference.
pol_reset; pol_pr frag-5 '1=Fix a (other/repo#42).' '2=Fix b (#4626).'
policy_run; pol_want 'foreign cite' 15 0
pol_reset; pol_pr frag-5b '1=Fix a (#4626).' '2=Fix b (#4626).'
export CLOSES_JSON; CLOSES_JSON=$(pol_same_repo 4626 | sed 's/"vtmocanu"/"other"/; s/"uzi"/"repo"/')
policy_run; pol_want 'foreign closing reference' 15 0
# 6. Whole references only.
for cite in '#420' '#42suffix' '#42_name' '#42é' 'café#42' 'abc#42' 'a/b/vtmocanu/uzi#42' '#46260'; do
  pol_reset; pol_pr frag-6 "1=Fix a ($cite)." '2=Fix b (#9999).'
  if [ "$cite" = '#46260' ]; then export CLOSES_JSON; CLOSES_JSON=$(pol_same_repo 4626); fi
  policy_run; pol_want "partial cite $cite" 15 0
done
# 7. Only the headline counts, not the description.
pol_reset; FRAG_DESC='Related to #42.' pol_pr frag-7 '1=Fix a (#4626).' '2=Fix b (#4626).'
policy_run; pol_want 'description-only cite' 15 0
# 8. Zero or one fragment is never checked.
pol_reset; pol_pr frag-8 '1=Fix a (#4626).'
policy_run; pol_want 'one fragment' 0 1
pol_reset; pol_pr frag-8b
policy_run; pol_want 'no fragment' 0 1
# 9. Fragments that main gained after the branch point are not the PR's.
pol_reset; pol_pr frag-9
git -C "$POLICY_ROOT" checkout -q main
for n in 3 4; do
  printf '### Fixed\n\n- **Main fix %s (#4626).**\n  Description line.\n' "$n" > "$POLICY_ROOT/changelog.d/$(printf '%08x-1111-4111-8111-%012x' "$n" "$n").md"
done
git -C "$POLICY_ROOT" add changelog.d; git -C "$POLICY_ROOT" commit -qm 'docs: main fragments'; git -C "$POLICY_ROOT" push -q origin main
POLICY_HEAD=$(git -C "$POLICY_ROOT" rev-parse frag-9); export POLICY_HEAD
policy_run; pol_want 'fragments added on main' 0 1
# 10. Other repositories and other base branches are out of scope.
pol_reset; pol_pr frag-10 '1=Fix a (#4626).' '2=Fix b (#4626).'
policy_run vtmocanu/other; pol_want 'another repository' 0 1
export POLICY_BASE_REF=release
policy_run; pol_want 'non-main base' 0 1
# 11. An unreadable fragment list or blob refuses.
pol_reset; pol_pr frag-11 '1=Fix a (#42).' '2=Fix b (#4626).'
export POLICY_FRAGMENT_DIFF_FAIL=1; policy_run; pol_want 'fragment list unreadable' 2 0
unset POLICY_FRAGMENT_DIFF_FAIL
export POLICY_FRAGMENT_BLOB_FAIL=1; policy_run; pol_want 'fragment blob unreadable' 2 0
unset POLICY_FRAGMENT_BLOB_FAIL
export POLICY_FRAGMENT_GREP_FAIL=1; policy_run; pol_want 'fragment filter failed' 2 0
grep -qF 'cannot filter the changelog fragments' "$WORK/policy.out" || fail "fragment filter failure: wrong refusal: $(cat "$WORK/policy.out")"
unset POLICY_FRAGMENT_GREP_FAIL
# 12. Unreadable closing metadata refuses, cited or not.
export CLOSES_FAIL=1
policy_run; pol_want 'closes unreadable, cited' 2 0
pol_reset; pol_pr frag-12 '1=Fix a (#4626).' '2=Fix b (#4626).'
export CLOSES_FAIL=1
policy_run; pol_want 'closes unreadable, uncited' 2 0
# 13./14. Metadata the lenient CLOSES= read tolerates is incomplete for the guard (even when
# the PR is cited), while a one-fragment PR keeps the lenient behaviour.
pol_fixture() { # name -> exports that fixture's environment (after pol_reset)
  case "$1" in
    a) export COMMITS_JSON='[{}]' COMMIT_COUNT=1 ;;
    b) export COMMITS_JSON='[{"commit":{"message":null}}]' COMMIT_COUNT=1 ;;
    c) export PR_META_JSON='{"closingIssuesReferences":[],"title":null}' ;;
    d) export COMMIT_PAGES_JSON='[null,[{"commit":{"message":"x"}}]]' COMMIT_COUNT=1 ;;
    e) export CLOSES_JSON='[{"number":4626}]' ;;
    f) export COMMIT_PAGES_JSON='[[]]' COMMIT_COUNT=0 ;;
    g) export COMMIT_PAGES_JSON='[[{"commit":{"message":"x"}}]] {"extra":1}' COMMIT_COUNT=1 ;;
  esac
}
for fx in a b c d e f g; do
  cite='(#42)'; [ "$fx" = e ] && cite='(#4626)'
  pol_reset; pol_pr "frag-13$fx" "1=Fix a $cite." "2=Fix b $cite."; pol_fixture "$fx"
  policy_run; pol_want "incomplete metadata $fx" 2 0
  grep -qF 'closing metadata incomplete for the changelog fragment check' "$WORK/policy.out" || fail "incomplete metadata $fx: wrong refusal: $(cat "$WORK/policy.out")"
  want=none; [ "$fx" = e ] && want=4626
  pol_reset; pol_pr "frag-14$fx" "1=Fix a $cite."; pol_fixture "$fx"
  policy_run vtmocanu/uzi --expect-closes "$want"; pol_want "one fragment $fx with flag" 0 1
  grep -qxF "CLOSES=$want" "$WORK/policy.out" || fail "one fragment $fx: CLOSES= line changed: $(cat "$WORK/policy.out")"
  policy_run; pol_want "one fragment $fx without flag" 0 1
  grep -qxF "CLOSES=$want" "$WORK/policy.out" || fail "one fragment $fx without flag: CLOSES= line changed: $(cat "$WORK/policy.out")"
done
# 15. A non-string base is no base: refused before any merge.
pol_reset; pol_pr frag-15 '1=Fix a (#4626).' '2=Fix b (#4626).'
export POLICY_BASE_JSON=true; policy_run; pol_want 'non-string base' 2 0
# The old `jq -r` read stringified it ("of true"); the new read has no base at all.
grep -qF 'cannot read the required checks of the base branch' "$WORK/policy.out" \
  || fail "non-string base was read as a branch name: $(cat "$WORK/policy.out")"
# 15b. Only added changelog.d/<uuid>.md paths count: a new README.md beside one fragment is one.
pol_reset; pol_pr frag-17 '1=Fix a (#4626).'
printf 'notes\n' > "$POLICY_ROOT/changelog.d/README.md"
git -C "$POLICY_ROOT" add changelog.d; git -C "$POLICY_ROOT" commit -qm 'docs: changelog readme'
POLICY_HEAD=$(git -C "$POLICY_ROOT" rev-parse HEAD); export POLICY_HEAD; pol_commits
policy_run; pol_want 'non-uuid added path' 0 1
# 15c. Editing two inherited fragments (main's 3 and 4 from case 9) adds none.
pol_reset; git -C "$POLICY_ROOT" checkout -q -B frag-18 main
for n in 3 4; do
  printf '### Fixed\n\n- **Main fix %s reworded (#4626).**\n  Description line.\n' "$n" > "$POLICY_ROOT/changelog.d/$(printf '%08x-1111-4111-8111-%012x' "$n" "$n").md"
done
git -C "$POLICY_ROOT" commit -qam 'docs: reword inherited fragments'
POLICY_HEAD=$(git -C "$POLICY_ROOT" rev-parse HEAD); export POLICY_HEAD; pol_commits
policy_run; pol_want 'modified inherited fragments' 0 1
# 16. One captured read: a later forge answer cannot change the guard's closing set.
pol_reset; pol_pr frag-16 '1=Fix a (#4626).' '2=Fix b (#4626).'
export CLOSES_SEQ_FILE="$WORK/closes-seq" CLOSES_JSON='[]' CLOSES_JSON_LATER; CLOSES_JSON_LATER=$(pol_same_repo 4626)
rm -f "$CLOSES_SEQ_FILE"
policy_run vtmocanu/uzi --expect-closes none; pol_want 'single closing read' 15 0
[ "$(cat "$CLOSES_SEQ_FILE")" = 1 ] || fail "closing metadata read $(cat "$CLOSES_SEQ_FILE") times, want 1"
pol_reset
# Fast main CI is an opt-in for the reviewed full head and low-risk files.
pol_reset; pol_pr main-ci-gate
export POLICY_REWORK=1 HELPER_REPO=vtmocanu/uzi ADMIN_RW=1
export HELPER_CALLS="$WORK/helper.calls" WRITERS_CALLS="$WORK/writers.calls" WRITERS_ARGS="$WORK/writers.args"
export WRITERS_OUT='{"writers":[],"complete":true}'
export PR_FILES_JSON='[{"filename":".agents/skills/uzi-lander/references/batch.md","status":"modified"}]'
CHECKS_JSON='[]' policy_run vtmocanu/uzi --main-ci-gate
pol_want 'main CI gate on unregistered checks' 0 1
grep -qF "MAIN_CI_GATE: PR #42 at $POLICY_HEAD" "$WORK/policy.out" || fail 'main CI waiver not logged'
for status in added removed modified; do
  PR_FILES_JSON="[{\"filename\":\"api/internal/store/migrations/00003_new.sql\",\"status\":\"$status\"}]" \
    policy_run vtmocanu/uzi --main-ci-gate
  pol_want "main CI gate refuses a $status migration" 2 0
  grep -qF 'MAIN_CI_GATE_REFUSED: migration' "$WORK/policy.out" || fail 'migration was refused for another reason'
done
PR_FILES_JSON='[{"filename":"tmp/archive.sql","status":"renamed","previous_filename":"api/internal/store/migrations/00003_new.sql"}]' \
  policy_run vtmocanu/uzi --main-ci-gate
pol_want 'main CI gate refuses a renamed-away migration' 2 0
PR_FILES_JSON='[{"filename":"agent/src/guardrails.ts","status":"modified"}]' policy_run vtmocanu/uzi --main-ci-gate
pol_want 'main CI gate refuses a trust-boundary path' 2 0
POLICY_LABELS='[{"name":"area::security"}]' policy_run vtmocanu/uzi --main-ci-gate
pol_want 'main CI gate refuses a trust-boundary marker' 2 0
POLICY_ROLLUP='[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE"}]' \
  policy_run vtmocanu/uzi --main-ci-gate
pol_want 'main CI gate refuses a known red' 1 0
POLICY_ROLLUP='null' policy_run vtmocanu/uzi --main-ci-gate
pol_want 'main CI gate refuses unreadable checks' 2 0
ELIGIBILITY_HEAD="$HEAD" policy_run vtmocanu/uzi --main-ci-gate
pol_want 'main CI gate refuses a head that moved during preflight' 2 0
policy_run vtmocanu/uzi --main-ci-gate --expect-head deadbee
pol_want 'main CI gate refuses a short SHA' 2 0
policy_run vtmocanu/uzi --main-ci-gate --no-rework-check
pol_want 'main CI gate refuses a writer-check bypass' 2 0
rm -f "$WRITERS_CALLS"
WRITERS_LATE=1 WRITERS_OUT='{"writers":[{"run_id":"late-fix","kind":"ci_fix","status":"queued","pipeline_ref":"agent/issue-42"}],"complete":true}' \
  policy_run vtmocanu/uzi --main-ci-gate
pol_want 'main CI gate refuses a pending writer at the final recheck' 14 0
[ "$(cat "$WRITERS_CALLS")" = 2 ] || fail 'main CI gate skipped the final writer read'
unset POLICY_REWORK HELPER_REPO ADMIN_RW HELPER_CALLS WRITERS_CALLS WRITERS_ARGS WRITERS_OUT PR_FILES_JSON
echo 'PASS main-ci-gate: exact-head waiver does not bypass migration, trust, known red or writer refusal'

echo 'PASS merge-policy: six changes/admin modes/repo spellings refuse stale policy; rebase/unrelated-main/own-policy-edit/remote-head accept; Git errors refuse; optional lock still checks; confirm-only skips; fragment guard refuses uncited multi-fragment PRs (15) and incomplete metadata (2), counting only added <uuid>.md fragments'
if [ "${UZI_MERGE_POLICY_ONLY:-0}" = 1 ]; then exit 0; fi

echo "PASS merge: --confirm-only reconciles an out-of-band merge; empty/unreadable/partial/skipping-only/unregistered required checks refuse; a conflicting PR names the conflict; every unresolved thread (bots included), alerts and unacknowledged comments refuse; an in-flight release run on main refuses unless it is already running in a no-cancel workflow (#4107) or its tag is on origin and coverage holds without it; a run-listing total_count mismatch is re-read up to 3 times while empty, malformed or unreadable listings refuse without retry; --expect-closes refuses a closing-reference mismatch"
