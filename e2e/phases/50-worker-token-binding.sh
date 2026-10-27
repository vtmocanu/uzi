# shellcheck shell=bash
# phase:    worker-token-binding
# title:    PRD #104: a worker's Anthropic binding reaches the claim payload; a rebind lands on the next claim
# critical: no
# lane:     gitlab
# executor: any
# requires: REPO_ID DUMMY_ANTHROPIC DUMMY_FORGE_PAT UZI_BIN
# provides: -
# handoff:  -
# mutates:  compose:agent(stopped); spare anthropic_token(console-key, deleted); binding-test worker and cancelled runs; isolated owner, connection/repos, vault credential, CLI token, persistent/ephemeral workers and run; cordon AUDIT snapshots intentionally retained
# restores: isolated owner resources cascade-delete after run terminalization via guarded EXIT cleanup (failure FAILs); cordon AUDIT snapshots intentionally retained; original agent stays stopped for 51, restart ENFORCED by 59-restart-agent providing SCHED_AGENT_UP for 60-62
# ---------------------------------------------------------------------------
# PRD #104: a worker's token binding reaches the CLAIM PAYLOAD, and a rebind takes
# effect on the very next claim with no restart.
#
# This is the one assertion no lower layer can make. The unit and live-DB tests prove
# the resolver picks the right secret id; what they cannot show is that the id turns
# into the right *plaintext* on the wire, through the real router, the real vault, and
# the real worker-Bearer auth — which is the whole product claim ("worker alpha spends
# console-key").
#
# It drives the claim endpoint with curl instead of the agent container,
# for two reasons that are not laziness:
#   - the claim payload is the only place the token is legible, and the agent
#     deliberately never writes it anywhere (the secret-hygiene phase above asserts
#     exactly that), so a container-side observation is impossible BY DESIGN;
#   - both claims must come from ONE worker with nothing restarted in between, which
#     is the property under test. A second container would test two workers instead.
# The live agent is stopped first: it shares the admin's queue and would otherwise
# claim these runs itself. Phase 51 also needs it stopped; the outbox and restart
# phases recreate it before later lifecycle scenarios.
#
# A new phase needing the real agent must recreate it or follow the explicit
# restart boundary. This phase itself keeps the agent stopped for the next
# synthetic-worker scenario.
say "PRD #104: a worker's Anthropic binding reaches the claim payload; a rebind lands on the next claim"
login
# ClaimRun still applies owner count < limit to these non-recovery-capable
# synthetic workers, but they add no custody. Wait while the real agent can settle
# its pending final inventories, before stopping it for the payload-only claims.
wait_custody_headroom 1
"${COMPOSE[@]}" stop agent >/dev/null 2>&1 || true

# A SECOND credential with a DISTINCT value — distinct is the whole test: the two
# claims below are told apart by which plaintext came back, so equal values would
# make both assertions pass vacuously.
DUMMY_ANTHROPIC_2="sk-ant-e2e-dummy-second-do-not-use-111111"
[ "$DUMMY_ANTHROPIC_2" != "$DUMMY_ANTHROPIC" ] || fail "the two e2e token fixtures must differ or the binding assertions are vacuous"
apipost /api/me/secrets/anthropic_token \
  "{\"token\":\"$DUMMY_ANTHROPIC_2\",\"label\":\"console-key\"}" >/dev/null \
  || fail "could not create the second named Anthropic token"
apiget /api/me/secrets \
  | jq -e '[.secrets[] | select(.kind == "anthropic_token")]
           | length == 2 and ([.[] | select(.is_default)] | length == 1)' >/dev/null \
  || fail "expected exactly two anthropic tokens with exactly one default after the create"
pass "the admin now holds two named tokens, exactly one of them default"

# A fresh worker, minted but never containerized. It authenticates with its join
# token like any worker; the api cannot tell the difference, which is the point.
BINDW="$(apipost /api/workers '{"name":"e2e-binding-worker"}')"
BINDW_ID="$(printf '%s' "$BINDW" | jq -r '.worker.id')"
BINDW_TOKEN="$(printf '%s' "$BINDW" | jq -r '.token')"
{ [ -n "$BINDW_ID" ] && [ "$BINDW_ID" != null ] && [ -n "$BINDW_TOKEN" ] && [ "$BINDW_TOKEN" != null ]; } \
  || fail "could not mint the binding-test worker"
# Register it advertising completion_interlock_v1: the completion interlock is on by
# default for Claude issue runs (issue #1626), and ClaimRun never hands an interlocked run
# to a worker whose registered protocol capabilities lack it.
curl -fsS -X POST "$BASE/api/worker/register" -H "Authorization: Bearer $BINDW_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"e2e-binding-worker","version":"0.0.0-e2e","protocol_capabilities":["completion_interlock_v1"]}' >/dev/null \
  || fail "could not register the binding-test worker"

# claim_token DESC — POST a claim as the binding-test worker and leave the delivered
# Anthropic plaintext in $CLAIM_TOKEN. Sets a global rather than printing, because
# `fail` inside a command substitution would exit only the SUBSHELL and its message
# would be captured instead of printed — the run would abort with no diagnosis.
#
# The token lives at .secrets.anthropic_oauth_token. The first version of this phase
# read it from the TOP level and failed the whole suite: ClaimPayload nests it under
# `secrets` (workersvc/claim.go:68), which a grep for the json tag alone does not
# show. Read the parent struct, not just the matching line.
#
# STATUS AND EXTRACTION ARE DELIBERATELY SEPARATE, and that split matters more than
# the path fix. The first version compared the extracted string against "" and
# reported an idle 204 whenever it was empty — but a 200 whose field sits at another
# path yields the identical empty string, so the failure message asserted a cause it
# had never measured and sent the reader hunting queue contention. Three distinct
# failures now: a non-200, a 200 of the wrong shape, and a 200 whose token is absent.
#
# The body carries a DECRYPTED forge PAT and Anthropic token. It never touches disk
# (a shell variable, piped through the printf BUILTIN so it never reaches an argv
# either) and no failure prints it: a shape mismatch reports KEY NAMES only, which
# is exactly enough to spot a wrong path and reveals no value. Do not widen that
# under debugging pressure.
CLAIM_TOKEN=""
claim_token() {
  local raw code body
  CLAIM_TOKEN=""
  # A run is created and claimed back-to-back here, so the create→claimable window
  # (the row becoming visible to the claim query's FOR UPDATE SKIP LOCKED + affinity/
  # quota predicates) is a real race — a legitimate 204 "queue idle" until it closes.
  # Bounded-retry ONLY the 204, mirroring create_run's retry idiom above: a 204 means
  # the claim mutated no row (workersvc returns nil,nil before any write), so retrying
  # is idempotent-safe and, because the claim is user-scoped with fixed ordering, still
  # lands on the same run a single call would. Fail loudly on any OTHER non-200 (a real
  # rejection) and on a persistent 204 — never blanket-swallow, that would mask a stuck
  # queue as green. e2e-only; the harness is not in CI (PRD #104 phase, issue #137).
  for _ in 1 2 3 4 5 6; do
    # No -f: a non-2xx must reach the checks below as a status, not abort the run.
    raw="$(curl -sS -w $'\n%{http_code}' -X POST "$BASE/api/worker/runs/claim" \
      -H "Authorization: Bearer $BINDW_TOKEN")"
    code="${raw##*$'\n'}"
    body="${raw%$'\n'*}"
    case "$code" in
      200) break ;;
      204) sleep 1; continue ;;  # run not yet claimable; the next attempt sees it
      *) fail "claim for '$1' returned HTTP $code, not 200" ;;
    esac
  done
  [ "$code" = 200 ] \
    || fail "claim for '$1' still returned 204 after 6 tries — the run never became claimable by this worker"
  printf '%s' "$body" | jq -e 'has("secrets")' >/dev/null 2>&1 \
    || fail "claim for '$1' returned 200 with no 'secrets' object — top-level keys: $(printf '%s' "$body" | jq -c 'keys' 2>/dev/null)"
  CLAIM_TOKEN="$(printf '%s' "$body" | jq -r '.secrets.anthropic_oauth_token // empty')"
  [ -n "$CLAIM_TOKEN" ] \
    || fail "claim for '$1' carried no anthropic_oauth_token — .secrets keys: $(printf '%s' "$body" | jq -c '.secrets | keys' 2>/dev/null)"
}
# queue_run DESC — an issue + a run for it, ready to be claimed. Prints the run id.
queue_run() {
  local iid
  iid="$(apipost "/api/repos/$REPO_ID/issues" \
    "{\"title\":\"E2E binding $1\",\"description\":\"implements prds/104-named-anthropic-tokens.md\"}" \
    | jq -r '.card.iid')"
  # A `local x="$(...)"` assignment swallows the substitution's exit status, so check
  # the value rather than trusting set -e to have aborted.
  case "$iid" in ''|null) echo "queue_run: could not create the '$1' issue" >&2; return 1 ;; esac
  create_run "$REPO_ID" "$iid"
}

# (a) UNBOUND → the owner's default token.
RUN_B1="$(queue_run unbound)" || fail "binding phase: could not queue the unbound-claim run"
claim_token "unbound (run $RUN_B1)"; GOT1="$CLAIM_TOKEN"
[ "$GOT1" = "$DUMMY_ANTHROPIC" ] \
  || fail "an UNBOUND worker's claim did not carry the default token (it carried some other credential)"
pass "unbound worker: the claim payload carries the owner's DEFAULT token"

# (b) REBIND, with nothing restarted. The worker is not a container here, so there is
# nothing to restart even in principle — which is exactly the property being asserted:
# the credential rides the claim, not the worker, so a server-side rebind is complete.
apipatch "/api/workers/$BINDW_ID" '{"anthropic_token":"console-key"}' \
  | jq -e '.worker.anthropic_secret_label == "console-key"' >/dev/null \
  || fail "PATCH /api/workers/{id} did not report the worker bound to console-key"
RUN_B2="$(queue_run bound)" || fail "binding phase: could not queue the bound-claim run"
claim_token "bound to console-key (run $RUN_B2)"; GOT2="$CLAIM_TOKEN"
[ "$GOT2" != "$GOT1" ] || fail "the claim payload did NOT change after the rebind — the binding never reached the claim"
[ "$GOT2" = "$DUMMY_ANTHROPIC_2" ] \
  || fail "a worker bound to 'console-key' did not receive that token's value"
pass "after the rebind the very next claim carries 'console-key' instead — no restart, no re-minted join token"

# (c) CLEAR → back to the default. The three-way field (absent / null / label) is what
# makes this expressible at all; null is the only spelling of "use my default again".
apipatch "/api/workers/$BINDW_ID" '{"anthropic_token":null}' \
  | jq -e '.worker.anthropic_secret_label == null' >/dev/null \
  || fail "PATCH with a null anthropic_token did not clear the worker's binding"
RUN_B3="$(queue_run cleared)" || fail "binding phase: could not queue the cleared-claim run"
claim_token "binding cleared (run $RUN_B3)"; GOT3="$CLAIM_TOKEN"
[ "$GOT3" = "$DUMMY_ANTHROPIC" ] \
  || fail "clearing the binding did not return the worker to the owner's default token"
pass "clearing the binding (anthropic_token: null) returns the next claim to the default token"

# (d) D5, live: deleting a bound token unbinds its workers instead of failing them.
# The composite FK's ON DELETE SET NULL is what does this, and getting the Postgres 15
# column-list syntax wrong would have nulled workers.user_id instead — so assert BOTH
# halves: the binding is gone AND the worker still belongs to its owner.
apipatch "/api/workers/$BINDW_ID" '{"anthropic_token":"console-key"}' >/dev/null \
  || fail "could not re-bind the worker before the delete-unbinds assertion"
CONSOLE_ID="$(apiget /api/me/secrets | jq -r '.secrets[] | select(.label == "console-key") | .id')"
[ -n "$CONSOLE_ID" ] || fail "could not resolve the console-key token id"
curl -fsS -b "$JAR" -X DELETE "$BASE/api/me/secrets/anthropic_token/$CONSOLE_ID" \
  -H "X-CSRF-Token: $(csrf)" >/dev/null || fail "deleting the bound token failed"
apiget /api/workers \
  | jq -e --arg id "$BINDW_ID" '.workers[] | select(.id == $id)
      | .anthropic_secret_id == null and .anthropic_secret_label == null' >/dev/null \
  || fail "deleting a bound token did not unbind its worker (D5)"
[ "$(db_psql "SELECT user_id IS NOT NULL FROM workers WHERE id = '$BINDW_ID'")" = t ] \
  || fail "deleting a bound token nulled workers.user_id — the composite FK's SET NULL is missing its column list"
pass "D5 live: deleting a bound token unbinds the worker and leaves workers.user_id intact"

# Binding-phase cleanup — terminate the three claimed-but-unfinished runs so they
# cannot leak into the next phase's queue. RUN_B1/B2/B3 were CLAIMED by the synthetic
# BINDW worker (claim_token reads the delivered token and stops there; it never drives
# them to a terminal state), and BINDW is not a container, so it runs no poller and
# they sit `claimed` on it. Once BINDW's last heartbeat ages past the stale window
# (E2E_WORKER_HEARTBEAT_STALE=15s), the sweeper's RequeueRunsOfStaleWorkers moves them
# back to `queued` (runtime.sql), where the very next phase's user-scoped worker can
# claim them. The PRD #108 M5 auto-stop phase below depends on "the two runs we create
# are the only queued rows for the admin" (see its header) and fails with a dirty-queue
# claim when a requeued B-run leaks in — the timing-dependent nightly failure this
# cleanup fixes.
#
# A `{"kind":"cancel"}` input (as the PRD #111 M6 phase uses) does NOT work here:
# SubmitInput cancels server-side only when the run has NO live poller
# (workersvc/service.go, the `!live` branch), and those M6 runs were still `queued`
# with no worker. BINDW's heartbeat is fresh at this point, so a cancel would merely
# enqueue a verdict BINDW never consumes. Write the terminal state directly (the
# harness's db_psql-write pattern) so the runs leave every requeue/claim predicate
# deterministically, regardless of BINDW's liveness.
CANCELLED_N="$(db_psql "WITH c AS (
  UPDATE runs SET status = 'cancelled', status_since = now(), finished_at = now(), updated_at = now()
  WHERE id IN ('$RUN_B1', '$RUN_B2', '$RUN_B3') AND status NOT IN ('completed', 'failed', 'cancelled')
  RETURNING 1
) SELECT count(*) FROM c")"
[ "$CANCELLED_N" = 3 ] \
  || fail "binding phase cleanup: expected to terminate 3 leftover claimed runs, cancelled $CANCELLED_N (B1=$RUN_B1 B2=$RUN_B2 B3=$RUN_B3)"
pass "binding-phase cleanup: the three claimed binding runs terminated so they cannot dirty the auto-stop queue"

# Manual intake is tested under a fresh owner: no admin agent or another phase's
# workers can enter the suitable-worker denominator. Keep the agent-stopped
# handoff above intact. This subshell gives the shared cookie helpers only our jar.
(
  M5_FIXTURE="$(db_psql 'SELECT gen_random_uuid()')"
  [[ "$M5_FIXTURE" =~ ^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$ ]] || fail "manual intake: invalid fixture UUID"
  M5_EMAIL="manual-$M5_FIXTURE@uzi.e2e"
  M5_LABEL="Manual owner $M5_FIXTURE"
  M5_REASON="e2e-manual-intake-$M5_FIXTURE"
  JAR="$RUNROOT/manual-$M5_FIXTURE.jar"
  M5_OWNER="" M5_CONNECTION="" M5_REPO="" M5_WORKER="" M5_EPHEMERAL=""
  M5_REG_ARMED=0
  M5_RUNS=()
  m5_uuid() { [[ "$1" =~ ^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$ ]]; }
  m5_cleanup() {
    local run rc=0 remaining deleted
    if [ -z "$M5_OWNER" ]; then
      [ "${M5_REG_ARMED:-0}" = 1 ] || return 0
      # Only an absent-before-POST, nonce-derived identity may be recovered.
      # Validate every interpolated fixture value before this single lookup.
      m5_uuid "$M5_FIXTURE" || return 1
      [ "$M5_EMAIL" = "manual-$M5_FIXTURE@uzi.e2e" ] || return 1
      [ "$M5_LABEL" = "Manual owner $M5_FIXTURE" ] || return 1
      M5_OWNER="$(db_psql "SELECT id FROM users WHERE email='$M5_EMAIL' AND display_name='$M5_LABEL' AND NOT is_admin")" || return 1
      if [ -z "$M5_OWNER" ]; then
        M5_REG_ARMED=0
        return 0
      fi
    fi
    m5_uuid "$M5_OWNER" || return 1
    # At most our one created run. Independent terminalizations are best-effort;
    # any failure prevents cascading deletion from concealing unfinished work.
    for run in "${M5_RUNS[@]}"; do
      db_psql "UPDATE runs SET status='cancelled', status_since=now(), finished_at=now(), updated_at=now()
        WHERE id='$run' AND user_id='$M5_OWNER' AND repo_id='$M5_REPO'
          AND status NOT IN ('completed','failed','cancelled')" >/dev/null || rc=1
    done
    [ "$rc" = 0 ] || return 1
    remaining="$(db_psql "SELECT count(*) FROM runs WHERE user_id='$M5_OWNER'
      AND status NOT IN ('completed','failed','cancelled')")" || return 1
    [ "$remaining" = 0 ] || return 1
    # No public DeleteUser exists. Only this newly registered identity is removed;
    # workers/runs, connection/repos and encrypted credential cascade from it.
    deleted="$(db_psql "DELETE FROM users WHERE id='$M5_OWNER' AND email='$M5_EMAIL' RETURNING id")" || return 1
    [ "$deleted" = "$M5_OWNER" ] || return 1
    [ "$(db_psql "SELECT (SELECT count(*) FROM workers WHERE user_id='$M5_OWNER')
      + (SELECT count(*) FROM runs WHERE user_id='$M5_OWNER')
      + (SELECT count(*) FROM forge_connections WHERE user_id='$M5_OWNER')
      + (SELECT count(*) FROM user_secrets WHERE user_id='$M5_OWNER')")" = 0 ] || return 1
    M5_OWNER=""
    M5_REG_ARMED=0
  }
  m5_exit() {
    local rc=$?
    if ! m5_cleanup; then
      echo "manual intake: owned fixture cleanup failed" >&2
      rc=1
    fi
    exit "$rc"
  }
  trap m5_exit EXIT
  # Registration unlock is best-effort in auth.go. Secret creation and the later
  # positive claim, rather than registration alone, establish vault eligibility.
  [ "$(db_psql "SELECT count(*) FROM users WHERE email='$M5_EMAIL'")" = 0 ] \
    || fail "manual intake: fixture email already exists or preflight failed; left untouched"
  M5_REG_ARMED=1
  M5_REG="$(curl -fsS -c "$JAR" -X POST "$BASE/api/auth/register" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$M5_EMAIL\",\"password\":\"e2e-manual-$M5_FIXTURE\",\"display_name\":\"$M5_LABEL\"}")"
  M5_OWNER="$(printf '%s' "$M5_REG" | jq -er '.user.id')"
  m5_uuid "$M5_OWNER" || fail "manual intake: registration returned no owner UUID"
  [ "$(db_psql "SELECT count(*) FROM users WHERE id='$M5_OWNER' AND email='$M5_EMAIL' AND NOT is_admin")" = 1 ] \
    || fail "manual intake: registration did not create the exact non-admin owner"
  M5_REG_ARMED=0
  [ -n "$(csrf)" ] || fail "manual intake: registration did not issue CSRF"
  M5_CONNECTION="$(printf '{"forge_type":"gitlab","base_url":"https://forge-fake.e2e","token":"%s"}' "$DUMMY_FORGE_PAT" \
    | curl -fsS -b "$JAR" -X POST "$BASE/api/forge/connections" -H 'Content-Type: application/json' \
      -H "X-CSRF-Token: $(csrf)" --data-binary @- | jq -er '.connection.id')"
  m5_uuid "$M5_CONNECTION" || fail "manual intake: connection returned no UUID"
  [ "$(db_psql "SELECT count(*) FROM forge_connections WHERE id='$M5_CONNECTION' AND user_id='$M5_OWNER'")" = 1 ] \
    || fail "manual intake: connection owner mismatch"
  M5_REPO="$(apiget "/api/forge/connections/$M5_CONNECTION/projects" \
    | jq -er '[.repos[] | select(.path_with_namespace == "group/repo")] | select(length == 1) | .[0].id')"
  m5_uuid "$M5_REPO" || fail "manual intake: projects returned no repo UUID"
  [ "$(db_psql "SELECT count(*) FROM repos WHERE id='$M5_REPO' AND connection_id='$M5_CONNECTION'")" = 1 ] \
    || fail "manual intake: repo connection mismatch"
  apiput "/api/repos/$M5_REPO" '{"enabled":true}' | jq -e --arg id "$M5_REPO" \
    '.repo.id == $id and .repo.enabled == true' >/dev/null || fail "manual intake: repo enable failed"
  M5_CREDENTIAL="sk-ant-""e2e-manual-$M5_FIXTURE"
  M5_SECRET="$(printf '{"token":"%s","label":"manual-intake"}' "$M5_CREDENTIAL" \
    | curl -fsS -b "$JAR" -X POST "$BASE/api/me/secrets/anthropic_token" \
      -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf)" --data-binary @- | jq -er '.secret.id')"
  m5_uuid "$M5_SECRET" || fail "manual intake: encrypted secret creation failed"
  [ "$(db_psql "SELECT count(*) FROM user_secrets WHERE id='$M5_SECRET' AND user_id='$M5_OWNER'
    AND kind='anthropic_token' AND sealed_with='dek' AND octet_length(ciphertext)>0")" = 1 ] \
    || fail "manual intake: secret was not encrypted for the new owner"
  UZI_TOKEN_VAL="$(apipost /api/me/cli-tokens "{\"name\":\"manual-$M5_FIXTURE\"}" | jq -er '.token')"
  [[ "$UZI_TOKEN_VAL" == uzc_* ]] || fail "manual intake: owner CLI token missing"

  M5_CREATED="$(apipost /api/workers "{\"name\":\"manual-$M5_FIXTURE\"}")"
  M5_WORKER="$(printf '%s' "$M5_CREATED" | jq -er '.worker.id')"
  m5_uuid "$M5_WORKER" || fail "manual intake: persistent worker UUID missing"
  M5_JOIN="$(printf '%s' "$M5_CREATED" | jq -er '.token')"
  [[ "$M5_JOIN" == uzw_* ]] || fail "manual intake: worker join token missing"
  [ "$(db_psql "SELECT count(*) FROM workers WHERE id='$M5_WORKER' AND user_id='$M5_OWNER' AND NOT ephemeral")" = 1 ] \
    || fail "manual intake: persistent worker ownership mismatch"
  curl -fsS -X POST "$BASE/api/worker/register" -H "Authorization: Bearer $M5_JOIN" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"manual-$M5_FIXTURE\",\"version\":\"0.0.0-e2e\",\"protocol_capabilities\":[\"completion_interlock_v1\"]}" >/dev/null
  m5_heartbeat() {
    curl -fsS --max-time 10 -X POST "$BASE/api/worker/heartbeat" -H "Authorization: Bearer $M5_JOIN" \
      -H 'Content-Type: application/json' -d '{}' >/dev/null
  }
  m5_heartbeat
  m5_worker() {
    apiget /api/workers | jq -ec --arg id "$M5_WORKER" '[.workers[] | select(.id == $id)] | select(length == 1) | .[0]'
  }
  m5_worker | jq -e '.status == "online" and .ephemeral == false and .manual_cordoned_at == null
    and .manual_cordoned_by == null and .manual_cordoned_by_label == null and .manual_cordon_reason == null' >/dev/null \
    || fail "manual intake: new worker was not online and manually clear"

  # Public CreateWorker has no ephemeral flag. Seed only this freshly API-created
  # worker as ephemeral, as manual_cordon_livedb_test.go does; never fake affinity.
  M5_EPHEMERAL="$(apipost /api/workers "{\"name\":\"ephemeral-$M5_FIXTURE\"}" | jq -er '.worker.id')"
  m5_uuid "$M5_EPHEMERAL" || fail "manual intake: ephemeral fixture UUID missing"
  [ "$(db_psql "UPDATE workers SET ephemeral=true WHERE id='$M5_EPHEMERAL' AND user_id='$M5_OWNER'
    AND NOT ephemeral RETURNING id")" = "$M5_EPHEMERAL" ] || fail "manual intake: ephemeral seed owner mismatch"
  m5_ephemeral_snapshot() {
    db_psql "SELECT jsonb_build_object(
      'tuple',jsonb_build_array(manual_cordoned_at,manual_cordoned_by,manual_cordoned_by_label,manual_cordon_reason),
      'events',(SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY e.occurred_at,e.id),'[]'::jsonb)
        FROM worker_cordon_events e WHERE e.worker_id=w.id))::text
      FROM workers w WHERE id='$M5_EPHEMERAL' AND user_id='$M5_OWNER'"
  }
  M5_EPHEMERAL_BEFORE="$(m5_ephemeral_snapshot)"
  [ -n "$M5_EPHEMERAL_BEFORE" ] || fail "manual intake: missing ephemeral snapshot"
  for M5_METHOD in POST DELETE; do
    M5_REJECTION="$(curl -sS -w $'\n%{http_code}' -b "$JAR" -X "$M5_METHOD" \
      "$BASE/api/workers/$M5_EPHEMERAL/cordon" -H 'Content-Type: application/json' \
      -H "X-CSRF-Token: $(csrf)" -d '{}')"
    [ "${M5_REJECTION##*$'\n'}" = 409 ] || fail "manual intake: ephemeral $M5_METHOD did not return 409"
    printf '%s' "${M5_REJECTION%$'\n'*}" | jq -e \
      '.error == "manual cordon is only supported for persistent workers"' >/dev/null \
      || fail "manual intake: ephemeral conflict reason mismatch"
    [ "$(m5_ephemeral_snapshot)" = "$M5_EPHEMERAL_BEFORE" ] \
      || fail "manual intake: ephemeral rejection changed manual tuple or audit events"
  done
  # Remove the unsuitable peer before health classification, leaving exactly one
  # persistent peer for this owner. No shared worker lifecycle is changed here.
  [ "$(db_psql "DELETE FROM workers WHERE id='$M5_EPHEMERAL' AND user_id='$M5_OWNER' AND ephemeral RETURNING id")" = "$M5_EPHEMERAL" ] \
    || fail "manual intake: ephemeral cleanup failed"
  M5_EPHEMERAL=""
  [ "$(db_psql "SELECT count(*) FROM workers WHERE user_id='$M5_OWNER'")" = 1 ] \
    || fail "manual intake: expected only the owned persistent worker"

  M5_HELD="$(uzi_cli worker cordon "$M5_WORKER" --reason "$M5_REASON" --json)"
  printf '%s' "$M5_HELD" | jq -e --arg id "$M5_WORKER" --arg actor "$M5_OWNER" \
    --arg label "$M5_LABEL" --arg reason "$M5_REASON" '.id == $id and .effective_cordoned == true
      and (.manual_cordoned_at | type == "string") and .manual_cordoned_by == $actor
      and .manual_cordoned_by_label == $label and .manual_cordon_reason == $reason' >/dev/null \
    || fail "manual intake: CLI cordon metadata mismatch"
  M5_AT="$(printf '%s' "$M5_HELD" | jq -er '.manual_cordoned_at')"
  # Compare the manual contract only: activity/heartbeat fields can change between
  # reads. CLI emits WorkerDTO directly; API mutation responses wrap .worker.
  m5_manual_tuple() {
    jq -Sc '{id,manual_cordoned_at,manual_cordoned_by,manual_cordoned_by_label,manual_cordon_reason,effective_cordoned}'
  }
  M5_FIRST="$(printf '%s' "$M5_HELD" | m5_manual_tuple)"
  M5_NOOP="$(apipost "/api/workers/$M5_WORKER/cordon" '{"reason":"must not replace first attributes"}' | jq -ec '.worker')"
  [ "$(printf '%s' "$M5_NOOP" | m5_manual_tuple)" = "$M5_FIRST" ] \
    || fail "manual intake: repeat cordon overwrote first attributes or API/CLI JSON diverged"
  [ "$(m5_worker | m5_manual_tuple)" = "$M5_FIRST" ] || fail "manual intake: owner list metadata differs"

  M5_IID="$(apipost "/api/repos/$M5_REPO/issues" \
    "{\"title\":\"E2E manual $M5_FIXTURE\",\"description\":\"implements prds/4-agent-runtime-workers.md\"}" | jq -er '.card.iid')"
  [[ "$M5_IID" =~ ^[0-9]+$ ]] || fail "manual intake: issue creation failed"
  M5_RUN="$(create_run "$M5_REPO" "$M5_IID")"
  m5_uuid "$M5_RUN" || fail "manual intake: queued run UUID missing"
  M5_RUNS+=("$M5_RUN")
  [ "$(db_psql "SELECT count(*) FROM runs WHERE id='$M5_RUN' AND user_id='$M5_OWNER'
    AND repo_id='$M5_REPO' AND worker_id IS NULL AND status='queued'")" = 1 ] \
    || fail "manual intake: run was not an owned unassigned queued run"
  m5_heartbeat
  # Claim bodies contain decrypted credentials: shell variables + builtin printf
  # only, never disk, external-command argv, or failure dumps.
  M5_RAW="$(curl -sS --max-time 10 -w $'\n%{http_code}' -X POST "$BASE/api/worker/runs/claim" -H "Authorization: Bearer $M5_JOIN")"
  [ "${M5_RAW##*$'\n'}" = 204 ] || fail "manual intake: cordoned worker admitted new intake"
  M5_DEADLINE=$((SECONDS + 90))
  M5_HEALTH_OK=0
  # Check a 90-second deadline between iterations, with each of the two HTTP
  # calls capped at ten seconds (at most 20 seconds in flight past the deadline).
  # An HTTP failure aborts immediately; no sibling work is hidden by this poll.
  while [ "$SECONDS" -lt "$M5_DEADLINE" ]; do
    m5_heartbeat
    M5_RUN_DTO="$(curl -fsS --max-time 10 -b "$JAR" "$BASE/api/runs/$M5_RUN")"
    printf '%s' "$M5_RUN_DTO" | jq -e --arg id "$M5_RUN" \
      '.run.id == $id and .run.worker_id == null and .run.status == "queued"' >/dev/null \
      || fail "manual intake: blocked run changed assignment or status"
    if printf '%s' "$M5_RUN_DTO" | jq -e --arg actor "$M5_OWNER" --arg label "$M5_LABEL" --arg reason "$M5_REASON" --arg at "${M5_AT:0:19}" \
      '.run.health_reason // "" | startswith("waiting for workers cordoned (manual):")
        and contains($actor) and contains($label) and contains($reason) and contains($at)' >/dev/null; then
      M5_HEALTH_OK=1
      break
    fi
    sleep 2
  done
  [ "$M5_HEALTH_OK" = 1 ] || fail "manual intake: RunDTO never reported attributed manual-cordon health"
  # Clear only the exact fixture's hold. A changed identity is never adopted.
  [ "$(m5_worker | m5_manual_tuple)" = "$M5_FIRST" ] || fail "manual intake: hold identity changed before uncordon"
  M5_CLEARED="$(uzi_cli worker uncordon "$M5_WORKER" --json)"
  printf '%s' "$M5_CLEARED" | jq -e --arg id "$M5_WORKER" '.id == $id and .manual_cordoned_at == null
    and .manual_cordoned_by == null and .manual_cordoned_by_label == null
    and .manual_cordon_reason == null and .effective_cordoned == false' >/dev/null \
    || fail "manual intake: explicit uncordon did not clear only manual metadata"
  m5_worker | jq -e '.manual_cordoned_at == null and .manual_cordoned_by == null
    and .manual_cordoned_by_label == null and .manual_cordon_reason == null' >/dev/null \
    || fail "manual intake: cleared metadata did not persist"
  # Positive control on the SAME unrelated new intake, after the health evidence:
  # the earlier 204 must not merely reflect an ineligible credential/repo fixture.
  for _ in 1 2 3 4 5 6; do
    m5_heartbeat
    M5_RAW="$(curl -sS --max-time 10 -w $'\n%{http_code}' -X POST "$BASE/api/worker/runs/claim" -H "Authorization: Bearer $M5_JOIN")"
    M5_CODE="${M5_RAW##*$'\n'}"
    case "$M5_CODE" in
      200) break ;;
      204) sleep 1 ;;
      *) fail "manual intake: positive claim returned HTTP $M5_CODE" ;;
    esac
  done
  [ "$M5_CODE" = 200 ] || fail "manual intake: uncordoned fixture remained ineligible after six attempts"
  printf '%s' "${M5_RAW%$'\n'*}" | jq -e --arg run "$M5_RUN" \
    '.run_id == $run and (.secrets.anthropic_oauth_token | type == "string" and length > 0)' >/dev/null \
    || fail "manual intake: positive claim identity or credential missing"
  M5_DELIVERED="$(printf '%s' "${M5_RAW%$'\n'*}" | jq -er '.secrets.anthropic_oauth_token')"
  [ "$M5_DELIVERED" = "$M5_CREDENTIAL" ] || fail "manual intake: positive claim delivered the wrong owner credential"
  [ "$(db_psql "SELECT count(*) FROM runs WHERE id='$M5_RUN' AND user_id='$M5_OWNER'
    AND repo_id='$M5_REPO' AND worker_id='$M5_WORKER' AND status='claimed'")" = 1 ] \
    || fail "manual intake: positive claim did not assign the exact owned run"
  # Like binding cleanup above, terminalize this owned synthetic claim before
  # deleting workers or their owner; no poller exists to consume cancellation.
  m5_cleanup || fail "manual intake: success cleanup failed"
  trap - EXIT
  pass "isolated owner: manual metadata/noop/JSON, ephemeral 409, new-intake 204, attributed health and positive uncordoned claim"
)
