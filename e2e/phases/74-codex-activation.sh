# shellcheck shell=bash
# phase:    codex-activation
# title:    PRD #1429 M6: the joined Codex public activation, proved offline (stub harness-neutral seam)
# critical: no
# lane:     gitlab
# executor: stub
# requires: REPO_ID UZI_BIN UZI_TOKEN_VAL UZI_WORKER_TOKEN ADMIN_ID
# provides: -
# handoff:  -
# mutates:  admin secrets (adds then deletes a codex openai_api_key default; temporarily deletes+recreates the default anthropic_token for the codex-only sub-scenario, which cascades its anthropic_rate_limits gauge row); creates a fire-once prompt schedule pinned harness=codex (deleted before this phase ends); creates several issues+runs on REPO_ID
# restores: admin secrets end exactly as found (one default anthropic_token = $DUMMY_ANTHROPIC, zero codex-kind secrets); the anthropic_rate_limits gauge is reseeded 55/12 (phase 47's values) under the recreated token id; an EXIT-trap fail-safe recreates the default anthropic_token if a mid-scenario assertion fails, mirroring 34-vault.sh, so a fail-soft driver never strands the admin Codex-only; every run this phase creates ends completed or cancelled (no handoff, nothing for the quarantine sweep)
# =============================================================================
# PRD #1429 M6 — the last milestone of the atomic Codex public-activation PRD: prove
# the WHOLE joined change (M1-M5) end to end, offline, through the fake forge + the
# STUB executor + dummy credentials. `executor: stub` because every numeric assertion
# below (the exact stub usage/cost figures) is written for the stub and would need
# rework under `UZI_E2E_EXECUTOR=sdk` (mirrors 15-happy-path-restart.sh's own stub/sdk
# fork, which this phase does not need — M6 is explicitly the OFFLINE proof).
#
# Everything below runs on the ONE seed admin account so the "same worker, same user"
# comparisons (the Codex run vs. the Claude control) are genuinely apples-to-apples,
# and so this phase needs no new forge connection/repo-provisioning machinery (repos
# are owner-scoped; the admin's REPO_ID is the only one this suite ever discovers). The
# admin's credential set is toggled through the sequence below and restored to its
# exact starting shape at the end — the same "toggle on one account, then restore"
# idiom 48-auto-selection.sh and 34-vault.sh already use for D11/vault-state coverage.
say "PRD #1429 M6: joined Codex public activation, offline (stub harness-neutral seam)"
if [ "$EXECUTOR" != stub ]; then
  say "PRD #1429 M6 codex-activation scenario: SKIPPED (stub-only — every assertion below is a fixed stub token/cost figure and drives a Codex run through a dummy key; executor=$EXECUTOR)"
else

# --- runtime-assembled dummy openai_api_key -----------------------------------
# NEVER a complete provider-token-shaped literal in this source (task scan:secrets +
# check-token-literals.sh + GitHub Push Protection all scan for exactly that shape,
# .claude/rules/prds.md "A PRD whose tests need secret-shaped strings"): the "sk-"
# prefix and the body are two separate shell variables, joined only at expansion time,
# so no line here carries a contiguous token shape. This is the SAME value used for
# every scenario below (D3: "an OpenAI API key is usable by existence when it is the
# one Codex default"); validateOpenAIAPIKey (secrets.go) accepts any non-empty,
# whitespace/control-free string, no provider-specific shape required.
_cx_oai_prefix="sk-"
_cx_oai_body="e2e-codex-fixture-$$-donotuse"
DUMMY_OPENAI="${_cx_oai_prefix}${_cx_oai_body}"

# cx_new_issue TITLE -> issue iid (board card), same shape every other phase uses.
cx_new_issue() {
  apipost "/api/repos/$REPO_ID/issues" \
    "$(jq -nc --arg t "$1" '{title:$t, description:"implements prds/1429-codex-public-activation.md"}')" \
    | jq -r '.card.iid'
}

# cx_create_run REPO ISSUE_IID [HARNESS] -> prints the created run id on success.
# create_run()'s exact retry shape (lib.sh), widened with an optional explicit harness
# field so a 422 no_credential_for_harness is never confused with the transient
# board-reconcile 404 create_run() already defends against.
cx_create_run() {
  local repo="$1" iid="$2" harness="${3:-}" code out="$RUNROOT/.cx-run-create.json" body
  if [ -n "$harness" ]; then body="{\"issue_iid\":$iid,\"harness\":\"$harness\"}"; else body="{\"issue_iid\":$iid}"; fi
  for _ in 1 2 3 4 5 6; do
    code="$(curl -sS -b "$JAR" -o "$out" -w '%{http_code}' -X POST "$BASE/api/repos/$repo/runs" \
      -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf)" -d "$body")"
    case "$code" in
      200|201) jq -r '.run.id' "$out"; return 0 ;;
      404)
        grep -q "issue not found on this repo's board" "$out" \
          || { echo "cx_create_run: non-transient 404 for issue #$iid: $(cat "$out")" >&2; return 1; }
        sleep 1 ;;
      *) echo "cx_create_run: HTTP $code creating a run (harness=${harness:-<implicit>}) for issue #$iid: $(cat "$out")" >&2; return 1 ;;
    esac
  done
  echo "cx_create_run: still transient-404 after 6 tries (issue #$iid)" >&2
  return 1
}

# --- (1) explicit-unavailable: harness=codex with NO codex credential -> 422 -------
# Admin's ONLY credential right now is the seeded default anthropic_token (Codex has
# not been added yet), so an EXPLICIT codex request must be refused — D11 rule 1 never
# falls back to the other harness. Run BEFORE the openai_api_key add below, which is
# what makes this a real negative rather than a vacuous one.
CX_UNAVAIL_IID="$(cx_new_issue "E2E codex explicit-unavailable")"
{ [ -n "$CX_UNAVAIL_IID" ] && [ "$CX_UNAVAIL_IID" != null ]; } || fail "could not stage the explicit-unavailable issue"
CX_UNAVAIL_BODY="$RUNROOT/.cx-unavail.json"
CX_UNAVAIL_CODE=""
for _ in 1 2 3 4 5 6; do
  CX_UNAVAIL_CODE="$(curl -sS -b "$JAR" -o "$CX_UNAVAIL_BODY" -w '%{http_code}' -X POST "$BASE/api/repos/$REPO_ID/runs" \
    -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf)" \
    -d "{\"issue_iid\":$CX_UNAVAIL_IID,\"harness\":\"codex\"}")"
  if [ "$CX_UNAVAIL_CODE" = 404 ] && grep -q "issue not found on this repo's board" "$CX_UNAVAIL_BODY"; then
    sleep 1; continue
  fi
  break
done
[ "$CX_UNAVAIL_CODE" = 422 ] \
  || fail "explicit harness=codex with no codex credential: expected 422, got $CX_UNAVAIL_CODE ($(cat "$CX_UNAVAIL_BODY"))"
jq -e '.code == "no_credential_for_harness"' "$CX_UNAVAIL_BODY" >/dev/null \
  || fail "422 body missing the stable no_credential_for_harness classification: $(cat "$CX_UNAVAIL_BODY")"
assert_no_run_for_issue "$CX_UNAVAIL_IID" 2
pass "explicit harness=codex with no codex credential: 422 no_credential_for_harness, no run created"

# --- (2) save a runtime-assembled dummy openai_api_key through the boot-unlocked vault --
# The admin session's vault was boot-unlocked at login (PRD #32); PUT/POST secrets seal
# under its DEK exactly like the anthropic_token save in 34-vault.sh. This is the user's
# FIRST codex credential, so it is forced default regardless of the explicit flag.
CX_OPENAI_ID="$(apipost /api/me/secrets/openai_api_key \
  "$(jq -nc --arg t "$DUMMY_OPENAI" '{token:$t, label:"e2e-codex-key", default:true}')" | jq -r '.secret.id')"
{ [ -n "$CX_OPENAI_ID" ] && [ "$CX_OPENAI_ID" != null ]; } || fail "could not save the dummy openai_api_key"
pass "saved a runtime-assembled dummy openai_api_key through the boot-unlocked vault ($CX_OPENAI_ID, default)"

# --- (3) an EXPLICITLY Codex-selected issue run, start to MR -----------------------
# Through the public API (harness:"codex" — PRD #1429 M2/D2's `uzi run create --harness
# codex` is the same wire field, CLI-vs-API is the PRD's own "either" — cx_create_run
# hardens against the SAME transient board race create_run() defends against).
CX_IID="$(cx_new_issue "E2E codex explicit activation")"
{ [ -n "$CX_IID" ] && [ "$CX_IID" != null ]; } || fail "could not stage the explicit-codex issue"
CX_RUN="$(cx_create_run "$REPO_ID" "$CX_IID" codex)" || fail "explicit codex run-create failed (non-transient; see stderr)"
[ "$(apiget "/api/runs/$CX_RUN" | jq -r '.run.harness')" = codex ] \
  || fail "a run created with harness:\"codex\" does not read back harness=codex"
pass "explicit harness:\"codex\" run $CX_RUN created; observed harness=codex on the wire"

wait_status "$CX_RUN" awaiting_approval
[ "$(uzi_cli run get "$CX_RUN" --field harness)" = codex ] \
  || fail "uzi run get --field harness disagrees with the API at the plan gate (CLI/API parity, D4)"
pass "codex run $CX_RUN reached the plan gate (awaiting_approval); CLI and API agree harness=codex"

uzi_cli run approve "$CX_RUN" >/dev/null || fail "uzi run approve (codex run) failed (exit $?)"
wait_status "$CX_RUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
CX_RUN_JSON="$(apiget "/api/runs/$CX_RUN")"
[ "$(echo "$CX_RUN_JSON" | jq -r '.run.harness')" = codex ] \
  || fail "codex run $CX_RUN drifted off harness=codex by completion"
CX_BR="$(echo "$CX_RUN_JSON" | jq -r '.run.branch // empty')"
[ -n "$CX_BR" ] || fail "the codex run pushed no branch: $(echo "$CX_RUN_JSON" | jq -c '.run.branch')"
CX_MR="$(echo "$CX_RUN_JSON" | jq -r '.run.mr_iid')"
{ [ "$CX_MR" != null ] && [ "$CX_MR" -gt 0 ]; } || fail "the codex run opened no MR (got $CX_MR)"
pass "codex run $CX_RUN completed through the stub harness-neutral seam (branch + MR !$CX_MR); harness stayed codex through completion"

# Honest cost-status rendering (D7) for a Codex claim under the stub: the stub's
# synthetic result frame carries NO per-model costStatus marker at all (it is
# harness-neutral, agent/src/executor.ts STUB_RESULT_MODEL_USAGE), and deriveUsageCost
# (usage_fold.go) maps a MISSING marker on a Codex row to 'unreported', cost_usd=0 --
# never a metered $0 and never a fabricated dollar figure. Tokens still fold
# unconditionally (nonNegTokens), so the exact stub counts still show.
echo "$CX_RUN_JSON" | jq -e '.run.usage.cost_status == "unreported" and .run.usage.cost_usd == 0
    and .run.usage.input_tokens == 21400 and .run.usage.output_tokens == 6100' >/dev/null \
  || fail "codex run cost-status not honestly rendered (want unreported/\$0 with 21400 in/6100 out tokens still folded): $(echo "$CX_RUN_JSON" | jq -c '.run.usage')"
pass "codex run cost_status=unreported, cost_usd=0, tokens folded honestly (21400 in / 6100 out) -- never a metered \$0"

# D4: a Codex claim never opens/records an Anthropic credential. codex_secret_id/auth
# mode ARE bound; anthropic_secret_id stays NULL; no run_credential_epochs row (that
# journal is Anthropic-only, PRD #1247).
[ "$(db_psql "SELECT (codex_secret_id IS NOT NULL) FROM runs WHERE id='$CX_RUN'")" = t ] \
  || fail "codex run $CX_RUN has no frozen codex_secret_id"
[ "$(db_psql "SELECT codex_auth_mode FROM runs WHERE id='$CX_RUN'")" = api_key ] \
  || fail "codex run $CX_RUN codex_auth_mode is not 'api_key'"
[ "$(db_psql "SELECT (anthropic_secret_id IS NULL) FROM runs WHERE id='$CX_RUN'")" = t ] \
  || fail "codex run $CX_RUN carries a non-null anthropic_secret_id -- Anthropic-before-harness regression"
[ "$(db_psql "SELECT count(*) FROM run_credential_epochs WHERE run_id='$CX_RUN'")" = 0 ] \
  || fail "codex run $CX_RUN wrote an Anthropic run_credential_epochs row"
pass "codex run $CX_RUN: bound codex_secret_id/api_key, NO anthropic_secret_id, NO run_credential_epochs row"

# --- (4) a Claude CONTROL run, same worker, same (now dual-credential) admin -------
# Proves no fallback / no cross-contamination the OTHER direction: adding a Codex
# default must not flip an explicit Claude request, and the Claude run must carry no
# codex binding.
CL_IID="$(cx_new_issue "E2E claude control")"
{ [ -n "$CL_IID" ] && [ "$CL_IID" != null ]; } || fail "could not stage the claude-control issue"
CL_RUN="$(cx_create_run "$REPO_ID" "$CL_IID" claude)" || fail "claude control run-create failed (non-transient; see stderr)"
[ "$(apiget "/api/runs/$CL_RUN" | jq -r '.run.harness')" = claude ] \
  || fail "a run created with harness:\"claude\" does not read back harness=claude"
wait_status "$CL_RUN" awaiting_approval
uzi_cli run approve "$CL_RUN" >/dev/null || fail "uzi run approve (claude control run) failed (exit $?)"
wait_status "$CL_RUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
CL_RUN_JSON="$(apiget "/api/runs/$CL_RUN")"
[ "$(echo "$CL_RUN_JSON" | jq -r '.run.harness')" = claude ] || fail "claude control run $CL_RUN drifted off harness=claude"
CL_BR="$(echo "$CL_RUN_JSON" | jq -r '.run.branch // empty')"
[ -n "$CL_BR" ] || fail "the claude control run pushed no branch"
CL_MR="$(echo "$CL_RUN_JSON" | jq -r '.run.mr_iid')"
{ [ "$CL_MR" != null ] && [ "$CL_MR" -gt 0 ]; } || fail "the claude control run opened no MR (got $CL_MR)"
# Claude ignores the (absent) marker entirely and stays 'metered' under the stub's
# fixed provider-reported cost (D5 backward-compat rule) -- the OTHER honest value
# from the same closed enum the codex run above exercised.
echo "$CL_RUN_JSON" | jq -e '.run.usage.cost_status == "metered" and .run.usage.cost_usd == 0.24
    and .run.usage.input_tokens == 21400 and .run.usage.output_tokens == 6100' >/dev/null \
  || fail "claude control run cost-status not as expected (want metered/\$0.24, 21400 in/6100 out): $(echo "$CL_RUN_JSON" | jq -c '.run.usage')"
[ "$(db_psql "SELECT (anthropic_secret_id IS NOT NULL) FROM runs WHERE id='$CL_RUN'")" = t ] \
  || fail "claude control run $CL_RUN has no frozen anthropic_secret_id"
[ "$(db_psql "SELECT (codex_secret_id IS NULL) FROM runs WHERE id='$CL_RUN'")" = t ] \
  || fail "claude control run $CL_RUN carries a non-null codex_secret_id -- cross-contamination"
pass "claude control run $CL_RUN completed (branch + MR !$CL_MR); harness stayed claude, metered/\$0.24, no codex binding -- no fallback, no cross-contamination either direction"

# --- (5) a PINNED Codex schedule fire (D2/D11 rule 1: explicit/pinned never falls back) --
# Both harnesses are usable for the admin right now (steps 3-4 proved it), so an
# UNPINNED schedule would resolve Claude (D11 rule 4, both usable + no default). The
# pin must override that and force codex anyway -- a meaningful, non-vacuous proof of
# pin authority, not just "codex happens to be the only option".
CX_AT="$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v+1H +%Y-%m-%dT%H:%M:%SZ)"
[ -n "$CX_AT" ] || fail "could not compute an RFC3339 --at instant for the pinned-codex schedule"
CX_SID="$(uzi_cli schedule create --repo "$REPO_ID" --prompt 'e2e codex-pinned prompt run' --at "$CX_AT" --harness codex --json | jq -r '.id')"
{ [ -n "$CX_SID" ] && [ "$CX_SID" != null ]; } || fail "schedule create --harness codex returned no id"
CX_RN="$(uzi_cli schedule run-now "$CX_SID" --json)" || fail "pinned-codex schedule run-now failed (exit $?)"
echo "$CX_RN" | jq -e '(.started | length) == 1' >/dev/null \
  || fail "pinned-codex schedule run-now should start exactly one run: $CX_RN"
CX_SCHED_RUN="$(echo "$CX_RN" | jq -r '.started[0].run_id')"
{ [ -n "$CX_SCHED_RUN" ] && [ "$CX_SCHED_RUN" != null ]; } || fail "pinned-codex schedule run-now returned no run_id: $CX_RN"
[ "$(apiget "/api/runs/$CX_SCHED_RUN" | jq -r '.run.harness')" = codex ] \
  || fail "a schedule pinned harness=codex fired a run whose harness is not codex"
# --auto-approve defaults true (61-schedules-prompt.sh), so this skips the gate.
wait_status "$CX_SCHED_RUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
CX_SCHED_JSON="$(apiget "/api/runs/$CX_SCHED_RUN")"
[ "$(echo "$CX_SCHED_JSON" | jq -r '.run.harness')" = codex ] || fail "pinned-codex schedule's run drifted off harness=codex"
CX_SCHED_MR="$(echo "$CX_SCHED_JSON" | jq -r '.run.mr_iid')"
{ [ "$CX_SCHED_MR" != null ] && [ "$CX_SCHED_MR" -gt 0 ]; } || fail "the pinned-codex scheduled run opened no MR"
[ "$(db_psql "SELECT (anthropic_secret_id IS NULL) FROM runs WHERE id='$CX_SCHED_RUN'")" = t ] \
  || fail "pinned-codex scheduled run $CX_SCHED_RUN carries a non-null anthropic_secret_id"
pass "a schedule pinned harness=codex fired a codex run (overriding D11's both-usable->claude default) $CX_SCHED_RUN, completed, MR !$CX_SCHED_MR, no anthropic binding"
uzi_cli schedule delete "$CX_SID" >/dev/null || fail "could not delete the pinned-codex schedule"

# --- (6) Codex-only implicit resolution (D11 rule 3: the sole usable harness) ------
# Requires a moment where Claude is genuinely NOT usable for the admin: usability is a
# raw "does an anthropic_token row exist" check (UserHasAnthropicToken), so the admin's
# single seeded default token must be deleted (D6/D14: deleting the LAST token, even
# though it is the default, is allowed -- it returns the user to the token-less
# state). An EXIT-trap fail-safe recreates it immediately if any assertion below dies,
# mirroring 34-vault.sh's lock/unlock fail-safe, so a fail-soft driver can never
# strand the admin Codex-only for the rest of a (possible future) run.
CX_CUR_ANTH_ID="$(apiget /api/me/secrets | jq -r '[.secrets[]|select(.kind=="anthropic_token" and .is_default)][0].id // empty')"
{ [ -n "$CX_CUR_ANTH_ID" ] && [ "$CX_CUR_ANTH_ID" != null ]; } || fail "admin has no default anthropic_token to remove for the codex-only scenario"
trap 'apiput /api/me/secrets/anthropic_token "{\"token\":\"$DUMMY_ANTHROPIC\"}" >/dev/null 2>&1 || true' EXIT
curl -fsS -b "$JAR" -X DELETE "$BASE/api/me/secrets/anthropic_token/$CX_CUR_ANTH_ID" \
  -H "X-CSRF-Token: $(csrf)" >/dev/null \
  || fail "could not delete the admin's default anthropic_token for the codex-only scenario"
[ "$(apiget /api/me/rate-limits | jq -r '.tokens | length')" = 0 ] \
  || fail "admin still shows an anthropic_token in /api/me/rate-limits after deleting the default -- deletion did not take"
pass "admin now holds no anthropic_token (rate-limits reports zero per-token rows; Claude genuinely unusable)"

CX_ONLY_IID="$(cx_new_issue "E2E codex-only implicit")"
{ [ -n "$CX_ONLY_IID" ] && [ "$CX_ONLY_IID" != null ]; } || fail "could not stage the codex-only-implicit issue"
CX_ONLY_RUN="$(create_run "$REPO_ID" "$CX_ONLY_IID")" || fail "codex-only implicit run-create failed (non-transient; see stderr)"
wait_status "$CX_ONLY_RUN" awaiting_approval
[ "$(apiget "/api/runs/$CX_ONLY_RUN" | jq -r '.run.harness')" = codex ] \
  || fail "a Codex-only user's PLAIN (no --harness) run did not implicitly resolve to codex (D11 rule 3)"
pass "Codex-only user's plain run $CX_ONLY_RUN implicitly resolved to codex (D11 rule 3: the sole usable harness)"
# A live-worker cancel at the gate is poller-consumed (SteeringChannel still polls at
# awaiting_approval) and converges on 'cancelled' regardless of path (PRD #503 M1, the
# same idiom 22-steer-queue-delivery.sh uses for its own live-worker cancel-at-gate:
# fire the cancel, then wait_status ... cancelled). Waiting here (rather than firing and
# moving straight to teardown) settles the run to a terminal state before the phase
# ends, so it can never still be mid-cancel when driver.sh's end-of-phase leak-sweep
# quarantine scan runs (a hard fail under E2E_STRICT_LEAKS=1).
apipost "/api/runs/$CX_ONLY_RUN/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true
wait_status "$CX_ONLY_RUN" cancelled

# Explicit restore (not just the trap fail-safe): recreate the default anthropic_token
# with the SAME seeded literal via the upsert-shaped PUT (34-vault.sh's own restore
# route -- idempotent, so a retry or a trap/explicit double-fire converges on ONE
# default row rather than risking a second), then reseed its rate-limit gauge back to
# phase 47's 55/12 (the delete above CASCADEs the old gauge row via its FK).
CX_NEW_ANTH_ID="$(apiput /api/me/secrets/anthropic_token "{\"token\":\"$DUMMY_ANTHROPIC\"}" | jq -r '.secret.id')"
{ [ -n "$CX_NEW_ANTH_ID" ] && [ "$CX_NEW_ANTH_ID" != null ]; } || fail "could not recreate the admin's default anthropic_token"
trap - EXIT  # explicit restore confirmed; drop the fail-safe
db_psql "INSERT INTO anthropic_rate_limits
           (user_secret_id, user_id, five_hour_pct, five_hour_resets_at, seven_day_pct, seven_day_resets_at, source, synced_at)
         VALUES ('$CX_NEW_ANTH_ID', '$ADMIN_ID', 55, now() + interval '2 hours', 12, now() + interval '3 days', 'usage_endpoint', now())
         ON CONFLICT (user_secret_id) DO UPDATE SET
           five_hour_pct = 55, seven_day_pct = 12, source = 'usage_endpoint', synced_at = now()" >/dev/null
pass "restored the admin's default anthropic_token ($CX_NEW_ANTH_ID) and its 55/12 rate-limit gauge (phase 47's values)"

# --- (7) teardown: leave admin secrets exactly as this phase found them -----------
curl -fsS -b "$JAR" -X DELETE "$BASE/api/me/secrets/openai_api_key/$CX_OPENAI_ID" \
  -H "X-CSRF-Token: $(csrf)" >/dev/null \
  || fail "could not delete the dummy openai_api_key; a later phase (or a repeat run) would inherit it"
apiget /api/me/secrets | jq -e '
    ((.secrets | map(select(.kind == "anthropic_token")) | length) == 1)
    and ((.secrets | map(select(.kind != "anthropic_token")) | length) == 0)
  ' >/dev/null \
  || fail "the codex-activation phase leaked a secret; admin should hold exactly one anthropic_token and zero codex-kind secrets: $(apiget /api/me/secrets | jq -c .)"
pass "teardown: admin secrets end exactly as found (one anthropic_token, zero codex-kind secrets)"

# --- (8) no dummy credential leak, in container logs or on the worker's disk -------
# Same idiom as 23-secret-hygiene.sh: a positive control BEFORE the absence check (an
# empty/short-read corpus would make the scan pass vacuously), and bash pattern
# matching -- never a `grep -q` at the END of a pipe over a big corpus (SIGPIPE can
# disarm a real match, see that phase's header). The per-claim mint-time `capability`
# secret (codexauthz.go mintCodexCapability) is a SEPARATE value from this dummy key:
# it is crypto/rand-minted per claim and ONLY its sha256 hash is ever persisted
# (codex_cap_hash) -- by design there is no plaintext anywhere, including here, to
# assert a literal-value absence against. Its non-leakage is the STRUCTURAL guarantee
# this PRD's M6 stub seam (agent/src/main.ts buildRunExecutor) proves instead: the
# stub never reads `selection.binding` (which carries both access_token and
# capability) at all, so nothing Codex-secret-shaped is ever handed to it -- and the
# codex run's own cost_status=unreported above is the same-run evidence that no real
# provider call (and so no live capability use) ever happened. For the api_key auth
# mode this run actually used, `access_token` IS the plaintext DUMMY_OPENAI (the
# claim opens the user's own openai_api_key verbatim -- claim_assembly.go), so the
# scan below already covers that half of the codex claim's secret material by value.
CX_LOGS_RC=0
CX_LOGS="$("${COMPOSE[@]}" logs --no-color 2>&1)" || CX_LOGS_RC=$?
[[ "$CX_LOGS" == *"database system is ready to accept connections"* ]] \
  || fail "positive control: compose-logs corpus looks incomplete (no db boot banner) -- the leak scan below would be vacuous (rc=$CX_LOGS_RC bytes=${#CX_LOGS})"
if [[ "$CX_LOGS" == *"$DUMMY_OPENAI"* ]]; then
  fail "the assembled dummy openai_api_key leaked into container logs"
fi
pass "no dummy openai_api_key in any container log (corpus vouched for by the db boot banner)"

"${COMPOSE[@]}" exec -T agent sh -c 'find /data -type f 2>/dev/null | head -1' | grep -q . \
  || fail "positive control: the worker's /data has no files to scan -- the leak scan below would be vacuous"
if "${COMPOSE[@]}" exec -T agent sh -c "grep -rlF '$DUMMY_OPENAI' /data 2>/dev/null | head -1" | grep -q .; then
  fail "the assembled dummy openai_api_key is present on the worker's /data disk"
fi
pass "no dummy openai_api_key on the worker's /data (bare clone cache, worktrees, sessions; corpus non-empty)"

fi
