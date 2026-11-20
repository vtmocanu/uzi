# PRD #4540: Organization-funded Claude provider keys with explicit recipient grants

**Issue**: [#4540](https://github.com/vtmocanu/uzi/issues/4540)
**Umbrella**: #4319
**Priority / area**: High / security
**Status**: Draft, split from #4324 by owner decision 2026-11-20
**Entry prerequisites**: #4324 M1 merged and #4505 merged with final owner-approved exact-key redaction/all-upload scan. Re-read merged helpers and evidence before M1. No implementation dependency on #4323, #4496 or #50. Preparation only; dispatch still requires the user's go.

## Problem and outcome

An organization may fund one Claude gateway credential for explicitly authorized users. #4324 supplies reusable shared definitions but requires every user to bring a key. This child adds an immutable `operator_key` credential mode: one instance-custodied key and explicit recipients, retaining source selection, frozen binding and next-claim revocation. No vendor or personal-key fallback is allowed.

Examples:

1. An admin saves an operator-key provider, acknowledges recipient trust, and grants B only. B sees/selects it; C cannot enumerate/get/select it and gets the existing opaque 404 behavior on crafted metadata requests. The source uses `claim.secrets.provider.credential` with no vendor credential, identical worker wire shape to a personal key.
2. Revocation committed before final claim authorization denies a new claim with `provider_not_granted`; after that decision an already-held claim keeps its authority under #4320 D9. The selected source is not cleared into vendor spend. A later re-grant does not silently auto-resume an old held run or replace its frozen key; an owner may explicitly resume only if the existing binding and lifecycle still permit it.
3. Set/replace/disable/delete of the organization key stales old frozen bindings. A queued old-key run refuses `provider_binding_stale`; it never receives replacement material. A gateway rotation is needed to revoke copies already obtained by trusted recipients.

## Owner decisions and residuals

The owner accepted on 2026-11-14 that granted users and code in their runs receive and can copy the organization key. Revoking a grant or disabling it in uzi cannot take back a copied key; recommend gateway rotation and per-key spend limits. Keep the key off workers only through a future #50 proxy, which is not an entry condition here.

**D6 (owner, 2026-11-20): redirect residual accepted for any supported auth style.** A redirecting gateway can re-send a selecting user's request body to the Location host and the key to a same-host sibling path, and cross-host under `x_api_key`. Bearer is recommended, not required. #4496 would remove this residual, but remains parked. The exact-base allowlist controls where requests start, not redirect confinement; probes still refuse redirects.

Display both residuals before saving, with a required `acknowledge_recipient_trust: true` handler field and a web regression. Explain also that one gateway key is a shared identity: gateway-side stored requests, logs or batch resources may expose recipients' work to one another. These are explicit recipient-trust conditions, not a promise of per-user isolation at the gateway. Restrict use to the deploying organization's authorized users per the umbrella's terms; the terms re-check is a maintainer obligation.

## Design

### Provider identity and grants

Extend #4324's NULL-owner shared definitions with `credential_mode='operator_key'`. Owner/scope/mode remain immutable after creation; switching funding mode requires a new provider, so old frozen runs cannot be reinterpreted. Reuse revisions and the starting URL allowlist. No user key is attached to or consumed by this mode; a granted user cannot mutate or Test the organization key through user routes. Shared definition and grant administration is cookie/CSRF admin-only. `uzi admin provider list|get` remains read-only; no new admin-write CLI authority.

Use `model_provider_grants(provider_id,user_id)` plus an explicit `all_users` flag. Default is no recipients. One predicate covers list/get, select, new/inherited creation and both assembly/final claim decisions. Preserve source references on revocation; the owner can clear/replace an unusable source without disclosure of now-private current metadata. Named grants are limited to 10,000 per provider; reuse shared-catalogue bounds and serialize cap/toggle decisions across admins. No new durable permission registry beyond these ordinary rows.

Creation/freeze and inherited rechecks go through existing `provider_source.go` callers, including preview/chat/overrides. Claim checks go through `assembleProviderBinding`, `lockedProviderRefusal` and `providerHoldRefusal`, never duplicated in each runner. If #4322 is merged, its judge/job/plan-and-code checker consumers reuse the same helper and lane settlement rules; otherwise those lanes remain unsupported. No dependency on unfinished #4322 is invented.

### Organization-key custody and frozen shape

Add one instance-secretbox table: stable `id`, unique `provider_id`, `ciphertext`, monotonically increasing `key_rev`, `disabled_at`, `set_by`, `set_at`. Seal/open with AAD `uzi/provider-operator-key/v1|<provider_id>`. No recipient vault or admin personal vault is involved. Set/replace/disable/delete increments revision; deletion clears the material while retaining an identity/revision tombstone, so revisions do not reset when a key is restored. The setter handles bounded plaintext input to seal it; reading stored material has only the two logical decrypt sinks below. Delivered worker credentials and bounded redaction registration follow the accepted recipient-trust boundary. No reveal endpoint, trace, response or log may return it.

Use the existing actual run columns: `model_provider_key_mode='operator_key'`, `model_provider_key_secret_id` contains this operator-key row's stable UUID, and `model_provider_key_rev` contains its revision. This column is a tagged key identity, not a foreign key to user_secrets. User-key mode retains its personal secret UUID/revision. Widen only `runs_model_provider_key_mode_check` with NOT VALID/revalidate migrations; retain the existing all-or-none `runs_model_provider_binding_check`. Dispatch key lookup by the frozen mode; never try the other table or vendor resolver on a miss. Unknown, partial, wrong-provider or disabled shapes refuse. Do not null the key ID as an alternate operator shape.

Runs keep no provider/key FK or delete cascade. Guarded Down refuses while any operator-key run exists; source/key schema rollback retains #4324's guards. Use online/additive migration patterns, short lock bounds and separate concurrent index work. A source schema rollback is a planned operation, not an image-only rollback; old API images cannot interpret queued operator-key bindings and may stale/refuse them. Document that hazard without rewriting them into vendor-bound runs.

### Shared authority fence and revocation timing

Admin grant/all-users/endpoint/key/retire/delete mutations serialize on the provider authority row for update. Recipient selection, creation freeze and final claim authorization take that row for share in the established secret-lock order; named-grant and operator-key rows are shared-locked for the decision too. A final recheck already holding run/lead locks uses bounded/FOR SHARE NOWAIT acquisition with existing retry/refusal handling, avoiding reverse lock-order waits. A missing grant is protected by the common provider-row fence, not by pretending to lock an absent row. No DB locks span network I/O.

Preliminary owner/grant checks precede decrypt; a denial already visible then decrypts and sends nothing. Assembly may open an allowed snapshot, but the final locked exact-claim recheck must compare that opened key/mode/revision and current grant before delivering. A revoke/rotation committed before the decision denies; one committed after affects the next actual claim. A later denial may follow a permitted decrypt, but no mixed authority is delivered. Test both orders and contention with an admin and two recipients. Preserve the accepted held-claim interval. Gateway-side key rotation revokes copied credentials. Cancelling a run stops that run through its existing cancellation path; it does not invalidate copies. uzi grant/key controls affect subsequent authority decisions under the stated next-claim contract.

### Exactly two logical decrypt sinks

1. Claim assembly after passing grant/frozen-binding checks and its final exact-claim fence.
2. A dedicated cookie/CSRF admin Test route. Granted users cannot Test operator material through the user Test route.

The probe captures a consistent immutable endpoint/auth/key identity/revision, rechecks before dial and stores evidence keyed to that tuple, separate from #4324's per-user results. Concurrent read/open/rotation cannot send new material to an old endpoint. An already-authorized old-key/old-endpoint request may finish. Preserve allowlist recheck, no redirects, bounded dial/body/rate/concurrency and response opacity; do not hold a database lock over the upstream request.

### Redaction, publication, audit and incident stop

Deliver the plaintext only as `claim.secrets.provider.credential`, with the existing exclusive provider object and no worker-visible custody change. Reuse #4505 exact-value output/error/log redaction and every GitHub/GitLab/Forgejo finalize/checkpoint/park/pause/shutdown exact-key scan unchanged. If the merged dependency lacks either, stop before implementation rather than assuming activation proves it. A hit/scanner failure blocks upload, retains local custody and the established typed refusal; no unrelated authority or vendor fallback is introduced. Newly enabled result/file publication must likewise reject exact-key bytes before upload, preserving that lane's terminal/optional-file semantics and never rewriting binary content or parking a job.

Structured API audit log lines record actor/provider IDs for shared create/edit/retire/delete, key set/replace/disable/delete, admin Test, grant add/remove and all-users toggle. A push-secret-blocked operator-key run records provider/run/user IDs, never the key or response body. Existing run fields attribute use to provider/mode/revision. No new audit table/service. Add operator keys and recipient/redirect residuals to vault-threat-model's Not protected by personal vault section. The admin UI exposes selected-user count, recipient status, disabled state and explicit incident-stop controls with the gateway-rotation warning.

## Out of scope

Codex shared providers, restricting users to shared-only sources, per-run/provider mixing, uzi-enforced organization spend budgets, preventing deliberate exfiltration by trusted key recipients, instantaneous network/claim revocation, the #50 proxy, the #4496 relay, widening CLI token authority and `.github/workflows/**`.

## Testing, milestone and landing

- LiveDB via `./e2e/run-store-it.sh`, owned Docker-capable DB, named PASS and zero SKIP, shared packages serial (`-p 1`): grant/visibility at selection/create/claim; two recipients/admin and both revoke/claim orders, all-users toggle, key rotate/disable/delete and NOWAIT contention; no fallback or source clearing; current and inherited frozen binding; per-user keys remain isolated; key revisions never repeat; tagged identity cannot open the wrong table/provider, including a harmless colliding UUID in the personal/operator tables; mode survives every freeze/inherit/promotion/snapshot conversion and chooses the table before lookup; AAD mismatch refuses; guarded Downs refuse live operator bindings. Otherwise report pending exact-head `test-api-store-it`.
- Handler/router: admin-only cookie/CSRF writes and Test reject `uza_` writes/non-admin/user calls; ungranted metadata is opaque 404; missing acknowledgment is refused; warning displayed and acknowledged; no reveal endpoint; user Test cannot spend the operator key; late probe result cannot overwrite current identity. Claim snapshot pins the exact credential wire shape and zero vendor release.
- Prefixless runtime-built key fixtures in success/error/log/metadata and uploaded objects: redaction and scan fail closed before any network write, including park/shutdown. Scanner failure also refuses. Clean controls pass. Judge/jobs/checkers, if enabled by #4322, obey its own lifecycle at both initial assembly and final recheck; jobs never park, checker failure never becomes a passing verdict.
- In-run: `task check:api` and affected Go tests `-race -count=1`, `task check:web` and touched UI/CLI tests; SQL generation/drift, migration/additivity/numbering, DTO, `task scan:secrets` with detected canaries, token-literals, docs sync/docs checks and UUID changelog. Agent changes also require `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent` plus focused tests. Watched red/green regressions at each bug seam. E2E sweep of changed reason/claim/grant fields; `task test:e2e-lib`, `task test:forge-fake`, named affected unrun phases.

### M1: One organization-funded provider serves only its explicitly granted recipients

**Blocked by**: merged #4324 and #4505. Deliver tagged custody/revision/grant fences, consistent admin probe, browser controls/read-only CLI, no-fallback publication controls and docs as one end-to-end authority outcome. No grant, mode or custody subsystem is shipped independently without the refusal checks.

Landing obligations outside M1: applicable exact-head API/web/repo/LiveDB and worker-UID checks; **full agent suite: exact-head PR CI** if agent touched. Linked hosted acceptance checks real gateway/grants/rotation/incident stop with no sweep/deadline; it does not replace offline authority proofs.

## Decision Log

- **D1. Split from #4324 (owner, 2026-11-20).** Shared definitions/personal keys ship first. Organization-funded keys introduce independent recipient authority and instance custody; this child depends on #4324 and preserves the committed feature scope.
- **D2. Explicit recipients and next-claim revocation.** Default no grants; owner/grant checks share one authority fence. Do not clear sources or fall back. Existing held claims remain under #4320 D9.
- **D3. Instance custody with provider-bound AAD and monotonic tombstone revisions.** No coupling to a personal vault; key mode and ID remain frozen, no cross-table or vendor retry. Existing all-or-none run shape is preserved by using the tagged operator row ID.
- **D4. Recipient trust accepted (owner, 2026-11-14).** Granted users/code can copy the key. That in-run exposure follows the existing credential-in-environment model; redirect exposure is separately D6. Gateway rotation revokes copies; uzi grant/disable controls stop later authority decisions. #50 remains a future mitigation, not a dependency.
- **D5. No worker-visible custody mode.** Reuse the same exclusive provider credential wire and #4505 safety gates; #4322's server-lane capability applies only to its enabled consumers. No Codex dependency.
- **D6. All supported auth-style redirect risk accepted (owner, 2026-11-20).** A gateway can re-send any selecting user's request body to Location, and the key to a same-host sibling path and cross-host under x_api_key. Bearer is recommended, not mandatory. #4496 would remove it. This accepts no accidental secret publication or grant bypass.
- **D7. Cookie-only administration and two logical decrypt sinks.** Existing admin-read CLI remains read-only. No plaintext reveal, personal-key probing through admin ownership, new audit service or implicit spending authority.
