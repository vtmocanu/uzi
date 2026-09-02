# shellcheck shell=bash
# phase:    skills-templates-tools
# title:    PRD #16: skill delivery (builtin allocated -> claim -> synthesized plugin dir)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  allocates the builtin prd-lifecycle skill (shared) to template TID; flips repo_skills_enabled=true on REPO_ID; creates + allocates a user-scoped template (e2e-mine); sets then clears a tier-1 tool profile on REPO_ID
# restores: repo_skills_enabled=false on REPO_ID + tool profile cleared to [], both at the end (the builtin/user-template allocations are left in place — no later phase is perturbed by them)
# =============================================================================
# PRD #16 — skill delivery + repo-skill opt-in, end to end. The stub executor
# synthesizes the plugin dir the SAME way the SDK executor does (shared
# prepareSkillPlugin) and reports the skill dirs it materialized on disk, so
# delivery is observable without a live Anthropic session.
say "PRD #16: skill delivery (builtin allocated → claim → synthesized plugin dir)"

# Allocate the builtin prd-lifecycle (shared) to a template. Claim assembly unions
# every template's allocations for the run's user, so this reaches every run.
SKILL_BUILTIN_ID="$(apiget /api/skills | jq -r '.skills[] | select(.name=="prd-lifecycle") | .id')"
[ -n "$SKILL_BUILTIN_ID" ] && [ "$SKILL_BUILTIN_ID" != null ] || fail "builtin prd-lifecycle skill was not seeded"
apiput "/api/agent-templates/$TID/skills" "{\"shared_skill_ids\":[\"$SKILL_BUILTIN_ID\"]}" >/dev/null
pass "allocated builtin prd-lifecycle (shared) to template $TID"

# plugin_skills RUN — the flattened plugin_skills arrays the stub reported (exactly
# the skill dirs materialized under the synthesized plugin dir).
plugin_skills() { apiget "/api/runs/$1/messages" | jq -c '[.messages[].payload.plugin_skills // empty] | flatten'; }

# skill_run TITLE — new issue+run, park at the gate (the stub reports skills at run
# START, before the gate), approve, and drive to completed (freeing the worker).
skill_run() {
  local iid run
  iid="$(apipost "/api/repos/$REPO_ID/issues" "{\"title\":\"$1\",\"description\":\"skill e2e — prds/16-agent-skills.md\"}" | jq -r '.card.iid')"
  run="$(create_run "$REPO_ID" "$iid")" || fail "skill_run: run-create failed for '$1' (non-transient; see stderr)" >&2
  wait_status "$run" awaiting_approval
  echo "$run"
}

# Repo skills OFF (default): the delivered builtin is materialized; the repo skill
# (seeded in the clone's .claude/skills) is NOT.
RUN_S1="$(skill_run 'E2E skill delivery (repo off)')"
PS1="$(plugin_skills "$RUN_S1")"
echo "$PS1" | jq -e 'index("prd-lifecycle") != null' >/dev/null \
  || fail "delivered builtin absent from the synthesized plugin dir: $PS1"
echo "$PS1" | jq -e 'index("e2e-repo-skill") == null' >/dev/null \
  || fail "repo skill loaded while the opt-in flag is OFF: $PS1"
apipost "/api/runs/$RUN_S1/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_S1" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "flag OFF: plugin dir has prd-lifecycle, NOT the repo skill ($PS1)"

# Flip the repo-skills opt-in ON (repo owner = the seed admin) and confirm it stuck.
apipatch "/api/repos/$REPO_ID" '{"repo_skills_enabled":true}' | jq -e '.repo.repo_skills_enabled == true' >/dev/null \
  || fail "PATCH repo_skills_enabled=true did not stick"
pass "repo owner enabled repo skills"

# Repo skills ON: the repo skill now loads too, at lowest precedence, alongside the
# delivered builtin.
RUN_S2="$(skill_run 'E2E skill delivery (repo on)')"
PS2="$(plugin_skills "$RUN_S2")"
echo "$PS2" | jq -e '(index("prd-lifecycle") != null) and (index("e2e-repo-skill") != null)' >/dev/null \
  || fail "repo skill not loaded at lowest precedence after opt-in: $PS2"
apipost "/api/runs/$RUN_S2/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_S2" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "flag ON: plugin dir has BOTH prd-lifecycle and e2e-repo-skill ($PS2)"

# =============================================================================
# PRD #18 — agent template scopes/allocation + tier-1 tool provisioning, end to
# end. The stub executor reports the claim's delivered template set
# (payload.agents) and runs the SAME provisioning path as the SDK executor
# (against the stubbed devbox), so both are observable with no live Anthropic
# session and no substituter egress.
say "PRD #18: user-scoped template + allocation → claim delivers only the owner's set"

# delivered_agents RUN — the flattened, deduped agent names the stub reported.
delivered_agents() { apiget "/api/runs/$1/messages" | jq -c '[.messages[].payload.agents // empty] | flatten | unique'; }
# run_texts RUN — every status/text line, newline-joined (fixed-string greppable).
run_texts() { apiget "/api/runs/$1/messages" | jq -r '.messages[].payload.text // empty'; }

# Create a private (scope=user) template as the seed admin, then allocate it to the
# admin's own runs (my_overrides enabled). A uniquely-named user template rides the
# owner's claim; another user would never see it (proven at the SQL layer).
UT_ID="$(apipost /api/agent-templates '{"name":"e2e-mine","description":"a private e2e helper.","prompt_body":"You help with e2e things.\n","model":null,"tools":null,"scope":"user"}' | jq -r '.template.id')"
{ [ -n "$UT_ID" ] && [ "$UT_ID" != null ]; } || fail "user-scoped template create failed"
apiput /api/agent-templates/allocations "{\"my_overrides\":[{\"template_id\":\"$UT_ID\",\"enabled\":true}]}" >/dev/null
pass "created + allocated a user-scoped template (e2e-mine)"

# A reserved lead name is refused for a user template (Decision 8, the no-two-leads pin).
C="$(fresh_code POST /api/agent-templates '{"name":"orchestrator","description":"x.","prompt_body":"b\n","model":null,"tools":null,"scope":"user"}')"
[ "$C" = 400 ] || fail "reserved lead name (orchestrator) should be 400, got $C"
pass "reserved lead name refused for a user template (400)"

RUN_UT="$(skill_run 'E2E user-template delivery')"
DA="$(delivered_agents "$RUN_UT")"
echo "$DA" | jq -e 'index("e2e-mine") != null' >/dev/null \
  || fail "allocated user template absent from the delivered claim: $DA"
echo "$DA" | jq -e '(index("lead") != null) and (index("coder") != null)' >/dev/null \
  || fail "builtin lead/coder missing from the delivered claim: $DA"
apipost "/api/runs/$RUN_UT/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_UT" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "user template e2e-mine delivered to its owner's run alongside the builtins ($DA)"

say "PRD #18: tier-1 tool provisioning → claim carries tool_packages → worker provisions (devbox stubbed)"

# Set an allowlisted package as the repo's tier-1 tool profile (the M4 seed allowlist).
PKG="$(apiget /api/tool-allowlist | jq -r '.allowlist[0].name // empty')"
{ [ -n "$PKG" ] && [ "$PKG" != null ]; } || fail "tool allowlist was not seeded"
apiput "/api/repos/$REPO_ID/tool-profile" "{\"packages\":[\"$PKG\"]}" \
  | jq -e --arg p "$PKG" '.packages | index($p) != null' >/dev/null \
  || fail "repo tool profile did not save $PKG"
pass "set repo tier-1 tool profile: [$PKG]"

RUN_TP="$(skill_run 'E2E tool provisioning')"
# The claim carried tool_packages=[$PKG]; the worker provisions against the stubbed
# devbox (install no-op, shellenv one PATH line — no substituter egress).
TP_TEXTS="$(run_texts "$RUN_TP")"
echo "$TP_TEXTS" | grep -qF "provisioning 1 tool(s): $PKG" \
  || fail "claim did not carry tool_packages / provisioning not started for $PKG"
echo "$TP_TEXTS" | grep -qxF "tools provisioned" \
  || fail "provisioning path not exercised (no 'tools provisioned' message)"
apipost "/api/runs/$RUN_TP/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_TP" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "tier-1 tool [$PKG] provisioned against the stubbed devbox, run completed"
# Clear the profile so later scenarios' runs aren't perturbed by provisioning.
apiput "/api/repos/$REPO_ID/tool-profile" '{"packages":[]}' >/dev/null
# Restore repo_skills_enabled=false on REPO_ID: no later gitlab-lane phase (29-51) reads
# this flag, but leaving it true would make the mutation outlive its phase, so undo it to
# keep the phase self-contained (mirrors the flip-on at the PATCH above).
apipatch "/api/repos/$REPO_ID" '{"repo_skills_enabled":false}' >/dev/null

