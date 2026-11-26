# shellcheck shell=bash
# phase:    run-env-vars
# title:    PRD #4416 M2: repo run variables reach a run on both harnesses; the secret value never leaves the vault path (stub)
# critical: no
# lane:     gitlab
# executor: stub
# requires: REPO_ID
# provides: -
# handoff:  -
# mutates:  creates two repo run variables (one plain, one secret) on REPO_ID; adds then deletes a codex openai_api_key default; creates two issues+runs on REPO_ID
# restores: both run variables are deleted (a secret a run received a receipt for becomes pending_deletion, which is the product's own retire path); the openai_api_key is deleted; admin secrets end as found; both runs end completed
# =============================================================================
# PRD #4416 M2: per-repo run variables, proved end to end through the real api
# routes, the real claim assembly and the worker, offline (fake forge + stub executor).
#
# WHAT THIS COVERS (per harness: claude, then codex, same worker, same user):
#   - a plain variable (URL-shaped value) and a secret variable are created through
#     PUT /api/repos/{id}/env/{name};
#   - a run is dispatched, reaches the plan gate, is approved and completes;
#   - GET /api/runs/{id} lists injected_env NAMES (with scope repo and the secret flag)
#     and never a value;
#   - the secret value appears nowhere an egress path could carry it: the run's
#     messages, the pushed branch (every blob in the new commits), the MR recorded by
#     the forge fake, the worker's container logs;
#   - the secret value is not returned by the env list route.
#
# WHAT THIS DOES NOT COVER (be precise):
#   - The STUB executor (both harnesses share it under executor=stub) never spawns a
#     command, so it cannot execute a test command that requires both variables in
#     its environment. "The variable is present in the command env" is therefore NOT
#     proved here; it is proved by the agent unit tests over the lead-lane env builders
#     (agent/src, run-env-lane and sdk-env tests) and by the live capstone below.
#     What this phase proves is the receipt-backed delivery record: the api assembled
#     the claim with both variables and the worker accepted it (run_env_v1), which is
#     what injected_env is derived from.
#   - Because no real model or command ever sees the secret, the "secret never appears
#     in messages / pushed content / MR" legs are negative checks that guard the
#     plumbing (claim -> worker -> publish) against accidental leakage, with positive
#     controls (corpora non-empty); they are not proof that a hostile agent could not
#     print the value (the worker's exact-value scan has its own unit tests).
#   - Live Claude/Codex SDK runs (UZI_E2E_EXECUTOR=sdk) are out of scope and skipped.
say "PRD #4416 M2: repo run variables reach runs on both harnesses (stub)"
if [ "$EXECUTOR" != stub ]; then
  say "PRD #4416 run-env-vars scenario: SKIPPED (stub-only: asserts fixed stub completion shape and drives a Codex run through a dummy key; executor=$EXECUTOR)"
else

# Runtime-assembled fixtures: the secret is joined from fragments at run time so no
# source line carries a contiguous token shape (see 74-codex-activation.sh).
_re_sec_a="e2e-runenv-secret-"
_re_sec_b="$$-$RANDOM-donotuse"
RE_SECRET="${_re_sec_a}${_re_sec_b}"
RE_PLAIN="https://registry.e2e.example/simple/$$"
_re_oai_a="sk-"
_re_oai_b="e2e-runenv-codex-$$-donotuse"
RE_OPENAI="${_re_oai_a}${_re_oai_b}"
RE_PLAIN_NAME=E2E_REGISTRY_URL
RE_SECRET_NAME=E2E_REGISTRY_TOKEN

re_new_issue() {
  apipost "/api/repos/$REPO_ID/issues" \
    "$(jq -nc --arg t "$1" '{title:$t, description:"implements the repo run variables feature"}')" \
    | jq -r '.card.iid'
}

# re_create_run ISSUE_IID HARNESS -> run id; same transient-404 retry shape as create_run.
re_create_run() {
  local iid="$1" harness="$2" code out="$RUNROOT/.re-run-create.json" _
  for _ in 1 2 3 4 5 6; do
    code="$(curl -sS -b "$JAR" -o "$out" -w '%{http_code}' -X POST "$BASE/api/repos/$REPO_ID/runs" \
      -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf)" \
      -d "{\"issue_iid\":$iid,\"harness\":\"$harness\"}")"
    case "$code" in
      200|201) jq -r '.run.id' "$out"; return 0 ;;
      404)
        grep -q "issue not found on this repo's board" "$out" \
          || { echo "re_create_run: non-transient 404: $(cat "$out")" >&2; return 1; }
        sleep 1 ;;
      *) echo "re_create_run: HTTP $code (harness=$harness): $(cat "$out")" >&2; return 1 ;;
    esac
  done
  return 1
}

# re_delete_var NAME: 200 (deleted) or 202 (pending deletion, a receipt exists) both end the live row.
re_delete_var() {
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -b "$JAR" -X DELETE "$BASE/api/repos/$REPO_ID/env/$1" \
    -H "X-CSRF-Token: $(csrf)")"
  case "$code" in 200|202|204) return 0 ;; *) echo "re_delete_var $1: HTTP $code" >&2; return 1 ;; esac
}

# --- (1) create the variables through the real routes -------------------------------
apiput "/api/repos/$REPO_ID/env/$RE_PLAIN_NAME" \
  "$(jq -nc --arg v "$RE_PLAIN" '{value:$v, secret:false, description:"e2e registry url"}')" >/dev/null \
  || fail "could not create the plain run variable $RE_PLAIN_NAME"
apiput "/api/repos/$REPO_ID/env/$RE_SECRET_NAME" \
  "$(jq -nc --arg v "$RE_SECRET" '{value:$v, secret:true, description:"e2e registry token"}')" >/dev/null \
  || fail "could not create the secret run variable $RE_SECRET_NAME"
RE_LIST="$(apiget "/api/repos/$REPO_ID/env")"
echo "$RE_LIST" | jq -e --arg pn "$RE_PLAIN_NAME" --arg sn "$RE_SECRET_NAME" --arg pv "$RE_PLAIN" '
    ([.variables[] | select(.name == $pn and .secret == false and .state == "enabled")] | length == 1)
    and ([.variables[] | select(.name == $sn and .secret == true and .state == "enabled" and .value == null)] | length == 1)
    and ([.variables[] | select(.name == $pn and (.value == $pv or .value == null))] | length == 1)' >/dev/null \
  || fail "run variable list is not the expected plain+secret pair (secret value must be null): $(echo "$RE_LIST" | jq -c '[.variables[] | {name, secret, state, value_withheld}]')"
[[ "$RE_LIST" != *"$RE_SECRET"* ]] || fail "the env list route returned the secret value"
pass "plain ($RE_PLAIN_NAME) and secret ($RE_SECRET_NAME) variables created; list shows the secret with a null value"

# --- (2) a codex credential, so the second harness is dispatchable -------------------
# Created up front (before the claude run) is harmless: harness is pinned per run below.
RE_OPENAI_ID="$(apipost /api/me/secrets/openai_api_key \
  "$(jq -nc --arg t "$RE_OPENAI" '{token:$t, label:"e2e-runenv-codex", default:true}')" | jq -r '.secret.id')"
{ [ -n "$RE_OPENAI_ID" ] && [ "$RE_OPENAI_ID" != null ]; } || fail "could not save the dummy openai_api_key"

# --- (3) one run per harness ---------------------------------------------------------
for RE_H in claude codex; do
  RE_IID="$(re_new_issue "E2E run env vars ($RE_H)")"
  { [ -n "$RE_IID" ] && [ "$RE_IID" != null ]; } || fail "could not stage the $RE_H issue"
  RE_RUN="$(re_create_run "$RE_IID" "$RE_H")" || fail "$RE_H run-create failed"
  [ "$(apiget "/api/runs/$RE_RUN" | jq -r '.run.harness')" = "$RE_H" ] \
    || fail "run $RE_RUN did not read back harness=$RE_H"
  wait_status "$RE_RUN" awaiting_approval
  uzi_cli run approve "$RE_RUN" >/dev/null || fail "uzi run approve ($RE_H run) failed"
  wait_status "$RE_RUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
  pass "$RE_H run $RE_RUN completed with the repo run variables configured"

  # (a) the run succeeded and the run page lists the injected NAMES, scope and secret flag.
  RE_JSON="$(apiget "/api/runs/$RE_RUN")"
  echo "$RE_JSON" | jq -e --arg pn "$RE_PLAIN_NAME" --arg sn "$RE_SECRET_NAME" '
      .run.status == "completed"
      and ((.run.injected_env // []) | length == 2)
      and ([.run.injected_env[] | select(.name == $pn and .scope == "repo" and .secret == false)] | length == 1)
      and ([.run.injected_env[] | select(.name == $sn and .scope == "repo" and .secret == true)] | length == 1)
      and ([.run.injected_env[] | keys_unsorted | sort] | all(. == ["name","scope","secret"]))' >/dev/null \
    || fail "$RE_H run injected_env is not exactly the two repo variables by name: $(echo "$RE_JSON" | jq -c '.run.injected_env')"
  [[ "$RE_JSON" != *"$RE_SECRET"* ]] || fail "$RE_H run detail carries the secret value"
  pass "$RE_H run page: injected_env lists $RE_PLAIN_NAME and $RE_SECRET_NAME (names, scope repo, secret flag), no values"

  # (b) the secret value appears in no run message, pushed blob, MR record or worker log.
  RE_MSGS="$(apiget "/api/runs/$RE_RUN/messages")"
  echo "$RE_MSGS" | jq -e '.messages | length > 0' >/dev/null || fail "$RE_H run has no messages (empty corpus would pass vacuously)"
  [[ "$RE_MSGS" != *"$RE_SECRET"* ]] || fail "$RE_H run messages carry the secret value"

  RE_BR="$(echo "$RE_JSON" | jq -r '.run.branch // empty')"
  [ -n "$RE_BR" ] || fail "$RE_H run pushed no branch"
  RE_GIT=(git --git-dir="$RUNROOT/fakeremote/repo.git")
  "${RE_GIT[@]}" show-ref --verify --quiet "refs/heads/$RE_BR" || fail "branch $RE_BR missing on the git remote"
  RE_BLOBS="$("${RE_GIT[@]}" rev-list --objects "refs/heads/$RE_BR" --not "refs/heads/main" | awk 'NF>1{print $1}')"
  [ -n "$RE_BLOBS" ] || fail "$RE_H branch $RE_BR adds no objects over main (empty corpus)"
  RE_HIT=""
  while read -r RE_OBJ; do
    [ -n "$RE_OBJ" ] || continue
    if [ "$("${RE_GIT[@]}" cat-file -t "$RE_OBJ")" = blob ] || [ "$("${RE_GIT[@]}" cat-file -t "$RE_OBJ")" = commit ]; then
      "${RE_GIT[@]}" cat-file -p "$RE_OBJ" > "$RUNROOT/.re-obj" 2>/dev/null || true
      if grep -qF -- "$RE_SECRET" "$RUNROOT/.re-obj"; then RE_HIT="$RE_OBJ"; break; fi
    fi
  done <<<"$RE_BLOBS"
  [ -z "$RE_HIT" ] || fail "$RE_H pushed content carries the secret value (object $RE_HIT)"

  RE_STATE="$(fake_state)"
  echo "$RE_STATE" | jq -e '.mrs | length >= 1' >/dev/null || fail "forge fake recorded no MR"
  [[ "$RE_STATE" != *"$RE_SECRET"* ]] || fail "the forge fake's recorded state (MR title/body/notes) carries the secret value"
  pass "$RE_H run: secret value absent from run messages, pushed objects and the forge-fake MR state (corpora non-empty)"
done

RE_LOGS="$("${COMPOSE[@]}" logs --no-color 2>&1 || true)"
[ -n "$RE_LOGS" ] || fail "empty container-log corpus"
[[ "$RE_LOGS" != *"$RE_SECRET"* ]] || fail "the secret run variable value appears in container logs"
pass "secret value absent from all container logs"

# --- (4) teardown ----------------------------------------------------------------------
re_delete_var "$RE_PLAIN_NAME" || fail "could not delete $RE_PLAIN_NAME"
re_delete_var "$RE_SECRET_NAME" || fail "could not delete $RE_SECRET_NAME"
apiget "/api/repos/$REPO_ID/env" | jq -e --arg pn "$RE_PLAIN_NAME" --arg sn "$RE_SECRET_NAME" \
  '[.variables[] | select((.name == $pn or .name == $sn) and .state != "pending_deletion")] | length == 0' >/dev/null \
  || fail "run variables still live after teardown"
curl -fsS -b "$JAR" -X DELETE "$BASE/api/me/secrets/openai_api_key/$RE_OPENAI_ID" \
  -H "X-CSRF-Token: $(csrf)" >/dev/null \
  || fail "could not delete the dummy openai_api_key; a later phase would inherit it"
pass "teardown: run variables deleted (or pending deletion) and the dummy openai_api_key removed"

fi
