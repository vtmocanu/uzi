#!/usr/bin/env bash
# lib/greptile-race.fixture.sh — the PR #1698 race as hermetic `gh api` data, shared by the
# watch-pr, pr-findings and takeover test stubs so the three scripts are judged on the same
# facts. Source it from a stub; `race_api "$@"` answers one `gh api ...` call for a `pushrace*`
# $MODE (rc 1 on an unexpected call).
#
# Timeline (live #1698, 2026-09-25, UTC): trigger 16:13:24; head committed 16:14:20 (PREV
# 16:11:38); Greptile's run on PREV started 16:15:23 and completed 16:18:40, 6 s after its
# PR-body edit at 16:18:34 named the head; a second trigger at 16:24:25 started a run on the
# head at 16:24:51.

RACE_HEAD=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
RACE_PREV=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

# race_trigger AT [TYPE] — one `@greptileai review` issue comment.
race_trigger() { printf '{"user":{"login":"lander","type":"%s"},"created_at":"%s","body":"@greptileai review"}' "${2:-User}" "$1"; }
# race_snapshot SHA — a body snapshot carrying Greptile's block naming SHA.
race_snapshot() { printf 'Intro\n\n<!-- greptile_comment -->\n\n<!-- greptile_confidence_score:5 -->\n\n<sub>Reviews (1) · Last reviewed commit: ["fix: x"](https://github.com/test/repo/commit/%s)</sub>\n\n<!-- /greptile_comment -->' "$1"; }
# race_edit AT LOGIN DIFF — one userContentEdits node (DIFF is raw text, or the word null).
race_edit() {
  if [ "$3" = null ]; then jq -nc --arg a "$1" --arg l "$2" '{editedAt:$a, editor:{login:$l}, diff:null}'
  else jq -nc --arg a "$1" --arg l "$2" --arg d "$3" '{editedAt:$a, editor:{login:$l}, diff:$d}'; fi
}
# race_run ID STATUS STARTED COMPLETED M — one Greptile Review check-run.
race_run() {
  jq -nc --argjson id "$1" --arg st "$2" --arg s "$3" --arg c "$4" --arg m "$5" '
    {id:$id, app:{slug:"greptile-apps"}, name:"Greptile Review", status:$st, started_at:$s}
    + (if $st=="completed" then {conclusion:"success", completed_at:$c,
         output:{summary:("Greptile has reviewed the Pull Request.\n\n10 files reviewed, " + $m + " comments added.")}}
       else {conclusion:null, completed_at:null, output:{summary:""}} end)'
}

race_api() {
  local nodes more=false
  case "$*" in
    *userContentEdits*)
      # Oldest first on purpose: the newest must be chosen by editedAt, not node order.
      nodes="$(race_edit 2026-09-25T16:05:00Z greptile-apps "$(race_snapshot "$RACE_PREV")"),$(race_edit 2026-09-25T16:12:23Z maintainer 'Intro')"
      case "$MODE" in
        pushrace_othersha) nodes="$nodes,$(race_edit 2026-09-25T16:18:34Z greptile-apps "$(race_snapshot "$RACE_PREV")")" ;;
        # A user wrote Greptile's block naming the head, at the very moment Greptile's run
        # finished; Greptile itself never did. Only the editor check stands in the way.
        pushrace_forged) nodes="$(race_edit 2026-09-25T16:18:34Z maintainer "$(race_snapshot "$RACE_HEAD")")" ;;
        pushrace_pending_first) ;;
        pushrace_malformed) nodes="$nodes,$(race_edit 2026-09-25T16:18:34Z greptile-apps "<!-- /greptile_comment -->
Last reviewed commit: [x](https://github.com/test/repo/commit/$RACE_HEAD)
<!-- greptile_comment -->")" ;;
        pushrace_short) nodes="$nodes,$(race_edit 2026-09-25T16:18:34Z greptile-apps "$(race_snapshot deadbeef)")" ;;
        pushrace_nodiff) nodes="$nodes,$(race_edit 2026-09-25T16:18:34Z greptile-apps null)" ;;
        pushrace_truncated) nodes="$nodes,$(race_edit 2026-09-25T16:18:34Z greptile-apps "$(race_snapshot "$RACE_HEAD" | head -c 120)")" ;;
        pushrace_early) nodes="$nodes,$(race_edit 2026-09-25T16:13:00Z greptile-apps "$(race_snapshot "$RACE_HEAD")")" ;;
        *) nodes="$nodes,$(race_edit 2026-09-25T16:18:34Z greptile-apps "$(race_snapshot "$RACE_HEAD")")" ;;
      esac
      [ "$MODE" = pushrace_more_pages ] && more=true
      printf '{"data":{"repository":{"pullRequest":{"userContentEdits":{"pageInfo":{"hasNextPage":%s},"nodes":[%s]}}}}}\n' "$more" "$nodes" ;;
    *graphql*) echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}' ;;
    *"/commits/$RACE_HEAD/status"*) echo '{"statuses":[]}' ;;
    *'/pulls/42/reviews'*)
      case "$MODE" in
        pushrace_review_findings) printf '[{"id":7,"user":{"login":"greptile-apps[bot]"},"commit_id":"%s","state":"COMMENTED","body":"","submitted_at":"2026-09-25T16:18:37Z"}]\n' "$RACE_HEAD" ;;
        *) echo '[]' ;;
      esac ;;
    *'/issues/42/comments'*)
      case "$MODE" in
        pushrace_notrigger) echo '[]' ;;
        pushrace_new_trigger|pushrace_new_trigger_absent) printf '[%s,%s]\n' "$(race_trigger 2026-09-25T16:13:24Z)" "$(race_trigger 2026-09-25T16:24:25Z)" ;;
        # A bot quoting the trigger phrase later is not a trigger.
        *) printf '[%s,%s]\n' "$(race_trigger 2026-09-25T16:13:24Z)" "$(race_trigger 2026-09-25T16:30:00Z Bot)" ;;
      esac ;;
    *'/pulls/42/comments'*)
      case "$MODE" in
        pushrace_review_findings) echo '[{"user":{"login":"greptile-apps[bot]"},"path":"in.go","line":3,"body":"<img alt=\"P2\"> inline finding","pull_request_review_id":7}]' ;;
        *) echo '[]' ;;
      esac ;;
    *'/pulls/42/commits'*)
      printf '[{"sha":"%s","commit":{"committer":{"date":"2026-09-25T16:11:38Z"}}},{"sha":"%s","commit":{"committer":{"date":"2026-09-25T16:14:20Z"}}}]\n' "$RACE_PREV" "$RACE_HEAD" ;;
    *"/commits/$RACE_PREV/check-runs"*)
      case "$MODE" in
        pushrace_norun) echo '{"check_runs":[{"id":5,"app":{"slug":"github-actions"},"name":"CI","status":"completed","conclusion":"success","completed_at":"2026-09-25T16:18:40Z","output":{"summary":""}}]}' ;;
        pushrace_nowindow) printf '{"check_runs":[%s]}\n' "$(race_run 3 completed 2026-09-25T16:15:23Z 2026-09-25T16:25:00Z 0)" ;;
        pushrace_early) printf '{"check_runs":[%s]}\n' "$(race_run 3 completed 2026-09-25T16:12:00Z 2026-09-25T16:13:30Z 0)" ;;
        pushrace_review_findings|pushrace_findings_noreview) printf '{"check_runs":[%s]}\n' "$(race_run 3 completed 2026-09-25T16:15:23Z 2026-09-25T16:18:40Z 1)" ;;
        pushrace_two_runs) printf '{"check_runs":[%s,%s]}\n' "$(race_run 6 completed 2026-09-25T16:16:00Z 2026-09-25T16:19:30Z 2)" "$(race_run 3 completed 2026-09-25T16:15:23Z 2026-09-25T16:18:40Z 0)" ;;
        *) printf '{"check_runs":[%s]}\n' "$(race_run 3 completed 2026-09-25T16:15:23Z 2026-09-25T16:18:40Z 0)" ;;
      esac ;;
    *"/commits/$RACE_HEAD/check-runs"*)
      case "$MODE" in
        pushrace_stale_dup|pushrace_new_trigger|pushrace_pending_first) printf '{"check_runs":[%s]}\n' "$(race_run 4 in_progress 2026-09-25T16:24:51Z '' '')" ;;
        *) echo '{"check_runs":[]}' ;;
      esac ;;
    *'/pulls/42/files'*|*'/contents/'*) echo '[]' ;;
    *'/rules/branches/'*) echo '[[]]' ;;
    *) echo "unexpected gh api in race fixture: $*" >&2; return 1 ;;
  esac
}
