# shellcheck shell=bash
# phase:    file-forge-issue
# title:    PRD #68: file a forge issue from a judge recommendation
# critical: no
# lane:     gitlab
# executor: any
# requires: J_RUN
# provides: F_REVIEW F_IID
# handoff:  -
# mutates:  -
# restores: -
# --- PRD #68: file a forge issue from a judge recommendation -------------------
# Filing a recommendation templates + sanitizes a draft server-side, creates a REAL
# issue on the fake forge labelled exactly [uzi] (never autopilot; PRD #764), persists the
# link, and enqueues NO run — filing an issue and spending tokens on a run stay
# separate human decisions.
#
# SETUP (PRD #97 M4): this phase used to consume the install_worker_tool/jq
# recommendation that the dropped #46 Phase B produced by planting a
# command-not-found and re-judging. It now SEEDS that coordinate directly on the review
# the funnel above already landed — the same direct-seed fixture pattern the harness
# uses for gauge rows. What #68 owns is everything DOWNSTREAM of a
# recommendation existing (draft → forge issue → labels → link → 409 → startable), and
# that is untouched; where the row came from was never this phase's property.
say "PRD #68: file a forge issue from a judge recommendation"
F_REVIEW="$(apiget "/api/runs/$J_RUN/review" | jq -r '.review.id')"
{ [ -n "$F_REVIEW" ] && [ "$F_REVIEW" != null ]; } || fail "PRD #68: no review on $J_RUN to seed a recommendation on"
# The id is never captured here — the row's `id uuid PRIMARY KEY DEFAULT
# gen_random_uuid()` (migration 00059) assigns it, and the read-back below takes the id
# from the API, which is the only one that proves anything.
db_psql "INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
         VALUES ('$F_REVIEW','install_worker_tool','jq','the reviewer hit jq: command not found in two iterations','high')" >/dev/null
# Read it back THROUGH the API (not from the INSERT) so the seed only counts if the
# review DTO actually surfaces the recommendation the filing routes will resolve.
#
# Assert EXACTLY ONE match. review_recommendations has no UNIQUE on
# (review_id, category, target) — only idx_review_recommendations_review (00059) — so
# nothing at the schema level stops a second row on this coordinate. Today that cannot
# happen (fallbackReview only emits install_worker_tool from signal.missing_tools, and
# nothing upstream plants one), but if it ever did, jq would emit two newline-joined
# uuids, the -n/!=null guard would pass, and the damage would surface far downstream as
# a malformed recommendation URL. Count first, then take the id — never `head -1`,
# which would hide the duplicate this check exists to name.
F_REC_IDS="$(apiget "/api/runs/$J_RUN/review" | jq -r '.review.recommendations[] | select(.category=="install_worker_tool" and .target=="jq") | .id')"
F_REC_N="$(printf '%s' "$F_REC_IDS" | grep -c . || true)"
[ "$F_REC_N" = 1 ] \
  || fail "PRD #68: expected exactly 1 install_worker_tool/jq recommendation on review $F_REVIEW, got $F_REC_N (0 = the seed never surfaced on the review DTO; >1 = the funnel's review already carried this coordinate and the seed duplicated it — the phase must file against an unambiguous row)"
F_REC="$F_REC_IDS"
[ "$F_REC" != null ] || fail "PRD #68: the install_worker_tool/jq recommendation has a null id"

# The draft GET is owner-scoped, templates the body, and carries the server-assembled
# uzi label (never autopilot, never from the request body; PRD #764).
F_DRAFT="$(apiget "/api/runs/$J_RUN/review/recommendations/$F_REC/issue-draft")"
echo "$F_DRAFT" | jq -e '.draft.labels == ["uzi"]' >/dev/null \
  || fail "PRD #68: the issue-draft must carry server-side labels [uzi] (got $(echo "$F_DRAFT" | jq -c '.draft.labels'))"
echo "$F_DRAFT" | jq -e '.draft.labels | index("autopilot") | not' >/dev/null \
  || fail "PRD #68: the draft must NEVER carry the autopilot label"
pass "issue-draft templated with the server-side uzi label (no autopilot)"

F_RUNS_BEFORE="$(db_psql "SELECT count(*) FROM runs")"

# File it against the caller's connected repo → 201 with the real created issue.
F_RESP="$(apipost "/api/runs/$J_RUN/review/recommendations/$F_REC/issue" \
  "{\"repo_id\":\"$REPO_ID\",\"title\":\"Install jq in the worker image\",\"description\":\"The reviewer hit jq command-not-found in two iterations.\"}")"
F_IID="$(echo "$F_RESP" | jq -r '.issue.iid')"
{ [ -n "$F_IID" ] && [ "$F_IID" != null ]; } || fail "PRD #68: filing did not return a created issue iid ($F_RESP)"
pass "filed issue #$F_IID on the forge from the recommendation"

# The FORGE truth: the bot-created issue carries exactly [uzi], never autopilot.
fake_state | jq -e --argjson iid "$F_IID" '.issues[] | select(.iid==$iid) | (.labels | sort) == ["uzi"]' >/dev/null \
  || fail "PRD #68: the filed forge issue #$F_IID must be labelled exactly [uzi] (got $(fake_state | jq -c --argjson iid "$F_IID" '.issues[] | select(.iid==$iid) | .labels'))"
fake_state | jq -e --argjson iid "$F_IID" '.issues[] | select(.iid==$iid) | (.labels | index("autopilot") | not)' >/dev/null \
  || fail "PRD #68: the filed forge issue #$F_IID must NOT carry autopilot"
pass "the filed forge issue #$F_IID is labelled exactly [uzi] (no autopilot)"

# Nothing auto-starts: filing enqueues NO run. No run row was added, and none exists for
# the filed issue — it is startable on the board, but only a human Start spends tokens.
F_RUNS_AFTER="$(db_psql "SELECT count(*) FROM runs")"
[ "$F_RUNS_BEFORE" = "$F_RUNS_AFTER" ] \
  || fail "PRD #68: filing enqueued a run ($F_RUNS_BEFORE -> $F_RUNS_AFTER) — filing must never start a run"
[ "$(db_psql "SELECT count(*) FROM runs WHERE repo_id='$REPO_ID' AND issue_iid=$F_IID")" = 0 ] \
  || fail "PRD #68: a run was enqueued for the filed issue #$F_IID — nothing must auto-start"
pass "filing enqueued NO run — the filed issue is startable, but nothing auto-started"

# The persisted link enforces one issue per coordinate: re-filing the same recommendation
# is a 409 (claim-first), and no second forge issue is created.
F_DUP_CODE="$(apipost_code "/api/runs/$J_RUN/review/recommendations/$F_REC/issue" \
  "{\"repo_id\":\"$REPO_ID\",\"title\":\"dup\",\"description\":\"dup\"}")"
[ "$F_DUP_CODE" = 409 ] || fail "PRD #68: re-filing the same coordinate must 409 (got $F_DUP_CODE)"
pass "re-filing the same recommendation is a 409 — one issue per coordinate (persisted link)"

# Headline success criterion (PRD #764): the filed issue carries the `uzi` label, so it
# is STARTABLE on the FIRST Start click — the single uzi_label gate passes and no PRD-file
# link is required, so createRun does NOT reject it (a 422 would make create_run return non-zero).
F_START_RUN="$(create_run "$REPO_ID" "$F_IID")" \
  || fail "PRD #68: the filed issue #$F_IID was NOT startable — createRun rejected it; a uzi-labelled issue must start on the first click"
{ [ -n "$F_START_RUN" ] && [ "$F_START_RUN" != null ]; } || fail "PRD #68: no run id returned for the filed issue #$F_IID"
pass "the filed uzi issue #$F_IID started a run ($F_START_RUN) on the first Start — no PRD-file link needed"

# Clean up: cancel this run so it does not hold worker capacity in the later PRD #42
# concurrency section (a normal issue run parks at the plan gate). Best-effort cancel +
# a soft wait for a terminal state (never hard-fail on the cleanup).
apipost "/api/runs/$F_START_RUN/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true
for _ in $(seq 1 60); do
  case "$(apiget "/api/runs/$F_START_RUN" | jq -r '.run.status // empty')" in
    cancelled|completed|failed) break ;;
  esac
  sleep 0.3
done
pass "cleaned up the filed-issue run (cancelled) so it frees worker capacity for later sections"

