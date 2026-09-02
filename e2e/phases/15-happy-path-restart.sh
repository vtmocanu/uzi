# shellcheck shell=bash
# phase:    happy-path-restart
# title:    happy path: create a PRD issue and start a run
# critical: yes
# lane:     gitlab
# executor: any
# requires: UZI_WORKER_TOKEN
# provides: IID RUN MR_IID
# handoff:  -
# mutates:  -
# restores: -
# --- happy path with a mid-run restart ---------------------------------------
say "happy path: create a PRD issue and start a run"
IID="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E implement","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
RUN="$(create_run "$REPO_ID" "$IID")" || fail "happy-path run-create failed (non-transient; see stderr)"
[ -n "$RUN" ] && [ "$RUN" != null ] || fail "run was not created"
pass "issue #$IID created; run $RUN queued"

wait_status "$RUN" awaiting_approval
GATE="$(apiget "/api/runs/$RUN")"
[ "$(echo "$GATE" | jq -r '.run.plan_md // empty')" != "" ] || fail "awaiting_approval carried no plan"
pass "run reached the plan gate (awaiting_approval) with a plan"

# PRD #37 detect: the worker parsed the repo's .claude/agents/ after clone and
# reported the roster on the run (settingSources stays []; detection is
# executor-independent, so the stub path exercises it too).
echo "$GATE" | jq -e '.run.repo_agents | (type == "array") and (map(.name) | sort == ["repo-coder","repo-reviewer"])' >/dev/null \
  || fail "run did not report the seeded repo agents (got: $(echo "$GATE" | jq -c '.run.repo_agents'))"
pass "PRD #37: run detected + reported the repo's .claude/agents/ roster (repo-coder, repo-reviewer)"

say "restart-resilience: down/up (keep volumes) while parked at the gate"
"${COMPOSE[@]}" down                       # keeps the named volumes (pgdata, agentdata)
# The recreated worker re-registers into its EXISTING row using the same join token:
# UZI_WORKER_TOKEN reaches this (post-#966) subshell via phase 13's `provides` round-trip
# (see `requires: UZI_WORKER_TOKEN` above), so the `up` below re-sources the real token
# into the `worker_token` secret and overrides the --env-file placeholder — the entrypoint
# re-hardens it 0400 worker (PRD #51 M5). Without the provide the worker would send the
# placeholder token and the API would 401 it (invalid worker token) — issue #984.
"${COMPOSE[@]}" up -d --wait db api web forge-fake
wait_http
login
"${COMPOSE[@]}" up -d --wait agent
wait_worker_online
pass "stack restarted; worker back online"

wait_status "$RUN" awaiting_approval
pass "orphaned run was re-queued, re-claimed, and is back at the gate"

say "approve the plan with a repo-source selection (choose), excluding repo-reviewer"
# PRD #37 choose: approve with the repo source and one agent excluded. The server
# validates the selection against the run's real roster and writes the canonical
# body the worker reads; a bad selection would 400 here.
apipost "/api/runs/$RUN/inputs" \
  '{"kind":"approve_plan","body":"","selection":{"source":"repo","exclusions":["repo-reviewer"]}}' >/dev/null
wait_status "$RUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "run completed"

# =============================================================================
say "assertions"
FINAL="$(apiget "/api/runs/$RUN")"
[ "$(echo "$FINAL" | jq -r '.run.status')" = completed ] || fail "final status is not completed"
[ "$(echo "$FINAL" | jq -r '.run.branch')" = "agent/issue-$IID" ] || fail "run.branch is not agent/issue-$IID"
MR_IID="$(echo "$FINAL" | jq -r '.run.mr_iid')"
{ [ "$MR_IID" != null ] && [ "$MR_IID" -gt 0 ]; } || fail "run.mr_iid not set (got $MR_IID)"
pass "run row: completed, branch=agent/issue-$IID, mr_iid=$MR_IID"

# PRD #37 apply: the approved selection is persisted on the run — repo source, with
# repo-reviewer excluded — so the run view/board render which agents ran.
[ "$(echo "$FINAL" | jq -r '.run.agent_source')" = repo ] \
  || fail "run.agent_source is not 'repo' (got $(echo "$FINAL" | jq -c '.run.agent_source'))"
echo "$FINAL" | jq -e '.run.agent_exclusions == ["repo-reviewer"]' >/dev/null \
  || fail "run.agent_exclusions did not persist the choice (got: $(echo "$FINAL" | jq -c '.run.agent_exclusions'))"
pass "PRD #37: run persisted agent_source=repo + agent_exclusions=[repo-reviewer] (detect→choose→apply)"

git --git-dir="$RUNROOT/fakeremote/repo.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID" \
  || fail "branch agent/issue-$IID was not pushed to the remote"
pass "branch agent/issue-$IID present on the git remote"

STATE_JSON="$(fake_state)"
[ "$(echo "$STATE_JSON" | jq '.mrs | length')" -ge 1 ] || fail "fake GitLab recorded no merge request"
[ "$(echo "$STATE_JSON" | jq -r '.mrs[-1].source_branch')" = "agent/issue-$IID" ] \
  || fail "recorded MR source_branch mismatch"
[ "$(echo "$STATE_JSON" | jq -r '.mrs[-1].target_branch')" = "main" ] || fail "recorded MR target_branch is not main"
pass "fake GitLab recorded an MR from agent/issue-$IID into main"

# When the authenticated-remote variant ran, prove the git smart-HTTP endpoint
# actually GATES on the Basic credential (not a no-op that accepts anything): no
# credential must 401, the correct Basic header must 200. Probed from inside the
# agent, which resolves forge-fake.e2e and trusts its cert (NODE_EXTRA_CA_CERTS).
if [ -n "${E2E_GIT_SMART_HTTP:-}" ]; then
  refs_url="https://forge-fake.e2e/group/repo.git/info/refs?service=git-upload-pack"
  probe='const https=require("https");const u=new URL(process.argv[1]);const o={hostname:u.hostname,port:443,path:u.pathname+u.search,headers:{}};if(process.argv[2])o.headers.Authorization=process.argv[2];https.get(o,r=>{console.log(r.statusCode);r.resume();}).on("error",e=>{console.error(e.message);process.exit(2);});'
  auth="Basic $(printf 'uzi-bot:%s' "$DUMMY_FORGE_PAT" | base64 | tr -d '\r\n')"
  code_noauth="$("${COMPOSE[@]}" exec -T agent node -e "$probe" "$refs_url" | tr -d '\r\n')"
  [ "$code_noauth" = 401 ] || fail "git smart-HTTP without a credential should 401 (got '$code_noauth')"
  code_auth="$("${COMPOSE[@]}" exec -T agent node -e "$probe" "$refs_url" "$auth" | tr -d '\r\n')"
  [ "$code_auth" = 200 ] || fail "git smart-HTTP with the correct Basic credential should 200 (got '$code_auth')"
  pass "git smart-HTTP auth gate: no credential -> 401, correct Basic -> 200"
fi

MSGS="$(apiget "/api/runs/$RUN/messages")"
echo "$MSGS" | jq -e '.messages | (length > 0) and ([.[].seq] == [range(1; length+1)])' >/dev/null \
  || fail "run_messages seq is not a gapless 1..N sequence (across the restart)"
pass "run_messages seq is gapless 1..$(echo "$MSGS" | jq '.messages | length') across the restart"

# PRD #99: the LEGACY shape. This run is the ordinary (non-interleave) stub, which
# emits no subagent attribution at all — exactly what every pre-migration row looks
# like. Both columns must be PRESENT on the wire and explicitly `null`, never absent
# and never "": MessageDTO's tags are not omitempty precisely so the browser's
# RunMessage can require the fields, which is what makes deleting the carry in
# applyFrame a compile error instead of a silent loss of lane identity. Under By-agent
# such a run coalesces into one lane per ROLE — the intended Problem-1 fix.
echo "$MSGS" | jq -e '.messages | (length > 0) and all(has("agent_instance") and has("agent_label"))' >/dev/null \
  || fail "REST messages must always carry both attribution keys (they are not omitempty)"
echo "$MSGS" | jq -e '.messages | all(.agent_instance == null and .agent_label == null)' >/dev/null \
  || fail "a legacy (no-subagent) run must carry NULL for both attribution columns, never \"\""
pass "PRD #99: legacy run carries both attribution keys, both null -> role-coalesced lanes"

# PRD #40: the API folds the terminal result frame's usage onto the run, aggregates
# it into /api/usage, and keeps the per-agent row in the stream (the three surfaces
# M4 renders from). That PROPERTY is what PRD #40 owns and it holds under either
# executor; only the NUMBERS are the stub's. The stub emits a synthetic frame with
# fixed usage (21400/6100, and a coder message at 12000) + a per-agent coder message,
# standing in for the live SDK, so under the stub we assert the exact values — they
# also prove the frame was parsed, not merely non-empty.
#
# Under UZI_E2E_EXECUTOR=sdk the usage is whatever the real session actually spent, so
# assert the property instead. Asserting the stub's constants UNCONDITIONALLY is what
# made the documented capstone unrunnable: every one of these four fails under a live
# run, and the first one exits, so the harness reported failure after 24 PASS and a
# fully successful real run (observed 2026-07-16: 2229 in / 11171 out against a
# hardcoded 21400 — the run had cloned, planned, gated, implemented, pushed and opened
# an MR). e2e/README.md's "no milestone assertion depends on this path" was true of the
# milestones and false of this script.
RUNUSAGE="$(apiget "/api/runs/$RUN")"
RU_IN="$(echo "$RUNUSAGE" | jq -r '.run.usage.input_tokens // empty')"
RU_OUT="$(echo "$RUNUSAGE" | jq -r '.run.usage.output_tokens // empty')"
if [ "$EXECUTOR" = sdk ]; then
  # A live frame folded at all: non-empty and positive. The stub's exact-value
  # assertions below are the ones that prove parsing; here the run's own numbers are
  # unknowable in advance, so "> 0" is the strongest honest claim.
  echo "$RUNUSAGE" | jq -e '(.run.usage.input_tokens // 0) > 0 and (.run.usage.output_tokens // 0) > 0' >/dev/null \
    || fail "run.usage not folded from the live SDK result frame (got: $(echo "$RUNUSAGE" | jq -c '.run.usage'))"
  pass "PRD #40: run.usage folded the live SDK result frame ($RU_IN in / $RU_OUT out) via run_usage"
else
  [ "$RU_IN" = 21400 ] \
    || fail "run.usage.input_tokens is not 21400 (got: $(echo "$RUNUSAGE" | jq -c '.run.usage'))"
  [ "$RU_OUT" = 6100 ] \
    || fail "run.usage.output_tokens is not 6100 (got: $(echo "$RUNUSAGE" | jq -c '.run.usage'))"
  pass "PRD #40: run.usage folded the result frame (21400 in / 6100 out) via run_usage"
fi

# PRD #97 M4: the /api/usage ROLLUP leg was dropped here. `SelfUsage`'s aggregation is
# proven exactly (not with a `>=`) against a live Postgres by
# `api/internal/store/run_usage_integration_test.go` TestUsageRollupsLiveDB — per-run
# MAX-per-model (never a SUM of cumulative snapshots), lifetime vs 7-day windowing,
# run_count excluding pre-feature runs, and per-user isolation — and that test runs in
# CI on every MR (`test:api-store-it`), a stronger gate than this local-only harness.
# What stays below is the full-wire half no lower layer reaches: the worker's terminal
# result frame was actually parsed and folded onto the run.

if [ "$EXECUTOR" = sdk ]; then
  # The live run's agent names come from the cloned repo's roster (PRD #37 selected
  # agent_source=repo above), so "coder" is not guaranteed and the count is real —
  # assert only that SOME agent-attributed message carries usage.
  echo "$MSGS" | jq -e '[.messages[] | select((.payload.usage.input_tokens? // 0) > 0 and .agent != null)] | length >= 1' >/dev/null \
    || fail "no per-agent usage-bearing message in the run-view data"
  pass "PRD #40: per-agent usage message present in the run stream"
else
  echo "$MSGS" | jq -e '[.messages[] | select(.agent == "coder" and (.payload.usage.input_tokens? == 12000))] | length >= 1' >/dev/null \
    || fail "no per-agent (coder) usage-bearing message in the run-view data"
  pass "PRD #40: per-agent coder usage message present in the run stream (12000 in)"
fi

