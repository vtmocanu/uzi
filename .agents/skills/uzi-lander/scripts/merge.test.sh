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
  case "\$*" in
    *'-q .state'*) printf '%s\n' "\$st" ;;
    *-q*)  printf '%s\n' "\$oid" ;;   # the retry: gh -q '.mergeCommit.oid // empty' prints the oid
    *)     if [ -n "\$oid" ]; then
             echo '{"state":"'"\$st"'","headRefOid":"$HEAD","mergeStateStatus":"CLEAN","mergeable":"MERGEABLE","mergeCommit":{"oid":"'"\$oid"'"},"baseRefName":"main"}'
           else
             echo '{"state":"'"\$st"'","headRefOid":"$HEAD","mergeStateStatus":"BLOCKED","mergeable":"'"\${MERGEABLE:-MERGEABLE}"'","mergeCommit":null,"baseRefName":"main"}'
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
# MAIN_RUNS_PAGE overrides the raw slurped pages (malformed payloads).
if [ "\${1:-}" = api ] && case "\$*" in *actions/runs*) true;; *) false;; esac; then
  [ "\${RUNS_FAIL:-0}" = 1 ] && { echo 'HTTP 502' >&2; exit 1; }
  [ -n "\${MAIN_RUNS_PAGE:-}" ] && { printf '%s\n' "\$MAIN_RUNS_PAGE"; exit 0; }
  [ -n "\${MAIN_RUNS_PAGE_FILE:-}" ] && { cat "\$MAIN_RUNS_PAGE_FILE"; exit 0; }
  st=\$(printf '%s' "\$*" | sed -n 's/.*status=\\([a-z_]*\\).*/\\1/p')
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
chmod +x "$WORK/bin/gh" "$WORK/bin/sleep" "$WORK/bin/stat"
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
  rm -f "$WORK/merge.log"
  set +e
  bash "$SCRIPT" test/repo 42 --no-rework-check > "$WORK/m.$1" 2>&1
  rc=$?
  set -e
}
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
unset MAIN_RUNS_JSON
export RUNS_FAIL=1
merge_run runsfail
[ "$rc" -eq 2 ] || fail "an unreadable main run page did not refuse, rc=$rc: $(cat "$WORK/m.runsfail")"
[ ! -e "$WORK/merge.log" ] || fail "merged with an unreadable main run list"
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
# --confirm-only never reads the run list.
MERGE_STATE=MERGED; export MERGE_STATE RUNS_FAIL=1
seed_state
set +e; bash "$SCRIPT" test/repo 42 --confirm-only > "$WORK/confirm-runs.out" 2>&1; rc=$?; set -e
[ "$rc" -eq 0 ] || fail "--confirm-only read main's run list, rc=$rc: $(cat "$WORK/confirm-runs.out")"
bash "$HERE/claims.sh" release '#42' --purge > /dev/null
MERGE_STATE=OPEN; export MERGE_STATE
unset RUNS_FAIL CHECKS_JSON CHECKS_RC

echo "PASS merge: --confirm-only reconciles an out-of-band merge; empty/unreadable/partial/skipping-only/unregistered required checks refuse; a conflicting PR names the conflict; every unresolved thread (bots included), alerts and unacknowledged comments refuse; an in-flight release run on main refuses"
