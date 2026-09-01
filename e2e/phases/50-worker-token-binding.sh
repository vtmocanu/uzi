# shellcheck shell=bash
# phase:    worker-token-binding
# title:    PRD #104: a worker's Anthropic binding reaches the claim payload; a rebind lands on the next claim
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
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
# It runs LAST and drives the claim endpoint with curl instead of the agent container,
# for two reasons that are not laziness:
#   - the claim payload is the only place the token is legible, and the agent
#     deliberately never writes it anywhere (the secret-hygiene phase above asserts
#     exactly that), so a container-side observation is impossible BY DESIGN;
#   - both claims must come from ONE worker with nothing restarted in between, which
#     is the property under test. A second container would test two workers instead.
# The live agent is stopped first: it shares the admin's queue and would otherwise
# claim these runs itself. Nothing follows this phase, so the stop is free.
#
# 🔴 THAT LAST SENTENCE IS A CONSTRAINT ON EVERY FUTURE PHASE, not a description. The
# `stop agent` below is never undone, so ANY phase appended after this one runs against
# a stack with no worker: its runs are created fine, sit in `queued`, and time out at
# whatever it waits for. PRD #35 M6 was written here first and died exactly that way
# (2026-07-28). Append below only what needs no worker — otherwise put the phase ahead
# of this one, or restart the agent yourself.
say "PRD #104: a worker's Anthropic binding reaches the claim payload; a rebind lands on the next claim"
login
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

