# PRD #4324: Admin-shared model providers (Claude engine)

**Issue**: [#4324](https://github.com/vtmocanu/uzi/issues/4324)
**Umbrella**: [#4319](https://github.com/vtmocanu/uzi/issues/4319)
**Priority**: High
**Status**: Draft
**Entry prerequisite**: PRD #4320 complete (whole PRD). No milestone here starts before it.

## Problem

After PRD #4320 every user defines their own provider. An organization that runs one gateway for its people wants to define it once, and some deployments fund the gateway themselves (one key for their own authorized users) so users have no key to bring. Today each user must re-enter the same URL and model map, and an organization key would have to be copied into every user's settings.

## Outcome

An admin can create a **shared provider** on the Claude engine, visible to the users it is granted to. Each shared provider has one credential mode:

- `per_user_key`: the admin sets URL, protocol, auth style and model map; each user attaches their own key, owned and sealed per user exactly as in PRD #4320.
- `operator_key`: one organization key, used for every granted user's runs. Who may use it is an explicit grant (all users, or named users), checked at selection, at run creation and at claim.

A user selects a shared provider as their Claude source (`users.claude_provider_id`, PRD #4320) like their own.

Acceptance examples:

1. Admin creates shared provider "org-gw" (`per_user_key`). User B sees it, attaches key K_B, tests it, selects it; B's runs carry K_B. User C, who attached nothing, cannot select it (`provider_credential_missing`). B's and C's keys for the same provider never mix (one key per (user, provider), each readable only by its owner).
2. Admin creates shared provider "org-funded" (`operator_key`, granted to user B only). B can select it; C neither sees nor selects it; a crafted API call by C naming it is refused (`provider_not_granted`). Revoking B's grant while B's run is queued makes its next claim refuse with that reason under each lane's lifecycle (a run parks, a job fails terminally); a claim already held keeps its authority until it ends (PRD #4320 D9).
3. The admin replaces the operator key: a queued run frozen on the old key revision is refused with `provider_binding_stale`, never sent the new key. No settings or API route reveals the key.

## Out of scope

- Codex-engine shared providers: a follow-up after PRD #4323 and this PRD (umbrella).
- Restricting users to shared providers only; spend caps or quotas on operator keys (gateway's job).
- Keeping the operator key off workers: that needs the #50 proxy (umbrella: deferred).

## Modules and seams

- **Delete and retire.** PRD #4320's rule holds: deleting a shared provider any user has selected as a source is refused (`provider_in_use_as_source`); retiring it is allowed and selected users' next runs refuse with `provider_source_unusable`, never falling back to a vendor credential. The source FK is single-column (no owner component), so it can reference a shared provider with a NULL owner.
- **Ownership.** `model_providers.owner_user_id` becomes nullable with a `scope` column (`user` | `shared`) and, for shared providers, `credential_mode`. Same revisions, retire/revoke semantics, operator allowlist, capability gate and probe as PRD #4320.
- **Read paths.** A provider is visible to a user when they own it or it is a shared provider granted to them (`per_user_key` providers are granted to all users unless restricted). Key ownership is separate from visibility: a `per_user_key` key row is owned and readable only by its user; owner-equality checks in A1 become "owner, or granted shared provider" for provider reads and "owner" for key reads.
- **Grants.** `model_provider_grants(provider_id, user_id)` or an `all_users` flag; admin-only. Enforced in the resolver (usability), in the create transaction, and at claim (re-read). Revocation applies at the next actual claim. Grant changes are logged as structured api log lines (no new audit subsystem).
- **Operator key custody.** Not a user-vault secret: the user vault seals per user under that user's policy (`api/internal/vault/policy.go`, `vault.go`), so an admin's vault would couple availability to every granted user. The key is sealed with the instance `secretbox` (`UZI_SECRET_KEY`, the custody used for forge PATs) with a monotonically increasing key revision. A run freezes it in PRD #4320's `provider_key_rev` with `provider_key_mode='operator_key'` and no `provider_key_secret_id`; after replacement, later claims of runs frozen on the old revision are refused with `provider_binding_stale`. Exactly two decrypt sinks: claim assembly after the grant check, and the admin-authorized probe (allowlist re-checked, A1 probe bounds, response scrubbed, never revealed). Deleted with the provider. Rotating `UZI_SECRET_KEY` invalidates it like every other instance secret.
- **Recipient trust (user decision, 2026-11-14).** A granted user's runs receive the operator key in their run environment, as every Claude credential is today. Code executed in those runs, including an agent following a prompt injection, can read it and use it outside uzi; revoking a grant does not take back a copied key. Granted users and their runs are therefore trusted with the key. The only real revocation is rotating the key at the gateway; per-key spend limits at the gateway are recommended. The admin page and docs state this before an operator key is saved. Keeping the key off workers is the #50 proxy's job.
- **Terms.** Within Anthropic's terms only for the deploying organization's own authorized users (umbrella "Terms of use"); the docs say so.
- **Surfaces.** Admin "Shared providers" page and `uzi admin provider ...` CLI; the user Providers section lists granted shared providers with their mode.
- **Bounds.** At most 50 shared providers; grant lists at most 10 000 rows per provider; A1's probe and field bounds apply.

## Testing decisions

- LiveDB: visibility vs key ownership; grant enforcement at selection, creation and claim; revocation while queued and while a claim is held; per-user key isolation on one shared provider; operator-key revision freeze and `provider_binding_stale` after replacement; the claim sink decrypts only after a passing grant check; the probe sink is admin-only.
- Handler: admin-only routes reject non-admins; the trust warning is shown before saving an operator key; mock fidelity; CLI tests.
- Validation contract as PRD #4320, including `task scan:secrets`, watched red/green regressions, **full agent suite: exact-head PR CI** as a landing obligation, live checks in a linked `acceptance` issue.

## Milestones

### M1: Admin-shared providers with per-user keys
**Blocked by**: none (entry prerequisite above)
Scope column, read paths, admin create, edit and retire, users attach their own key and select it, admin UI and CLI, docs.
Acceptance: example 1.

### M2: Organization-funded keys with enforced grants
**Blocked by**: M1
`operator_key` mode with instance-secretbox custody and key revisions, grants, enforcement at selection, creation and claim, admin probe, the recipient-trust warning, docs including terms of use and blast radius.
Acceptance: examples 2 and 3.

## Decision Log

- **D1: Shared providers reuse PRD #4320's object, revisions, allowlist and source selection.** Reason: one model; roles can reference either scope later. Rejected: a separate admin "gateway" setting.
- **D2: Operator keys require an explicit grant.** Reason: one key spends for many users. Rejected: implicitly available to every user.
- **D3: Operator key in the instance secretbox with its own revision.** Reason: no coupling to one admin's vault; replacement must not reach older bindings. Rejected: sealing under the creating admin's vault.
- **D4: Ship operator keys with explicit recipient trust (user decision).** Reason: lets organizations fund a gateway now; the exposure equals today's credential-in-env model, documented and bounded by gateway rotation and spend limits. Rejected: deferring operator keys until the #50 proxy (the reviewers' recommendation).
- **D5: Claude engine only here.** Reason: no milestone may wait on unfinished PRD #4323. Rejected: a hidden Codex milestone.
