# shellcheck shell=bash
# phase:    skills-authz
# title:    PRD #16 skills authz: a non-admin cannot reach admin / other-user surfaces
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# --- PRD #16: skills authz (router glue only) ---------------------------------
# The reviewer's M2 ask was to pin the authz boundaries end to end. PRD #97 M4
# COLLAPSED this phase: the 403-vs-404 skills/agent-template matrix is proven at the
# handler layer by `api/internal/handler/skills_test.go` —
# TestAuthorizeSkillWrite (builtin/global are admin-only; a user skill is owner-only;
# a non-owner non-admin gets 404 with existence hidden, an admin who may not edit gets
# 403), TestResetSkillStatus (the same 404 existence-oracle on the reset path) and
# TestAllocatableRules (shared vs mine allocation). Those run in CI on every MR; what
# no lower layer proves is that the HTTP routes REACH those helpers, so one
# representative leg stays here as router glue.
#
# ⛔ TWO legs below are NOT part of that matrix and must NOT be "finished off" by a
# later cleanup — each is the ONLY assertion of its property anywhere in the tree:
#   1. non-admin `PUT /api/agent-templates/{id}/skills` → 403. This is the admin gate
#      at `skill_allocations.go:105-107` (shared half is admin-only). `TestAllocatableRules`
#      does NOT cover it — that pins WHICH skills are allocatable, a different property —
#      and `SetTemplateSkills` has no handler test at all. A discriminating unit test
#      would need a live pool (every non-403 path reaches `h.pool.Begin`), so this
#      stays here (PRD #97 M4: enumerated leg-by-leg, found uncovered, deliberately kept).
#   2. non-owner `PATCH /api/repos/{id}` → 404 — a *repos*-handler property, and there
#      is no `repos_test.go` anywhere under `api/` (PRD #97 M4, fable review).
# Uses the fresh non-admin user registered above (FRESHJAR).
say "PRD #16 skills authz: a non-admin cannot reach admin / other-user surfaces"
# $TID is also consumed by the PRD #16 skill-delivery phase further down — resolve it here.
TID="$(apiget /api/agent-templates | jq -r '.templates[0].id // empty')"
[ -n "$TID" ] || fail "no agent template to authorize against"

# Router glue: the live route reaches authorizeSkillWrite and returns its status.
C="$(fresh_code POST /api/skills '{"name":"e2e-nope","description":"x.","body":"b\n","scope":"global"}')"
[ "$C" = 403 ] || fail "non-admin POST /skills scope=global: expected 403, got $C"
pass "non-admin POST /skills scope=global ⇒ 403"

# KEEPER (1): the shared-allocation admin gate — its only assertion anywhere.
C="$(fresh_code PUT "/api/agent-templates/$TID/skills" '{"shared_skill_ids":[]}')"
[ "$C" = 403 ] || fail "non-admin shared allocation: expected 403, got $C"
pass "non-admin PUT shared allocation half ⇒ 403"

# KEEPER (2): a repos-handler property with no handler test in the tree.
C="$(fresh_code PATCH "/api/repos/$REPO_ID" '{"repo_skills_enabled":true}')"
[ "$C" = 404 ] || fail "non-owner repo PATCH: expected 404, got $C"
pass "non-owner non-admin PATCH /repos/{id} ⇒ 404"

