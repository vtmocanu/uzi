# PRD #4320: Custom model providers, core and Anthropic-compatible endpoints for issue runs and chat

**Issue**: [#4320](https://github.com/vtmocanu/uzi/issues/4320)
**Umbrella**: [#4319](https://github.com/vtmocanu/uzi/issues/4319)
**Priority**: High
**Status**: Draft

## Problem

A user whose organization fronts Claude with an Anthropic-compatible gateway (a LiteLLM-style proxy, a corporate egress gateway, a cloud-hosted Messages endpoint) cannot use it from uzi:

- The Claude engine always calls Anthropic. `agent/src/sdk-env.ts` `buildSdkEnv` sets only `CLAUDE_CODE_OAUTH_TOKEN` and pins `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` to `undefined`; no base URL is configurable.
- The only Claude credential kind is `anthropic_token` (`api/internal/store/enums.go`), a subscription token. Storing a gateway key there would make the token test, the usage poller, auto-selection and limit-wait treat it as an Anthropic subscription, and would send it to Anthropic.
- Claude usability is "the user holds an enabled Anthropic token" (`api/internal/workersvc/harness_resolver.go`, `resolveRunHarnessQ`), so a gateway-only user cannot start a Claude run.

## Outcome

A user can define their own **provider** (a named, engine-bound endpoint with its own key and model map, at a base URL the operator approved), test it, and select it as their Claude source. Issue runs and chat then run through it, with no Anthropic credential anywhere in that run. The core (provider object, key binding, operator allowlist, selection, per-run binding, probe) is engine-neutral, so PRD #4323 adds the Codex engine and PRD #4322 the server lanes without reshaping it.

Acceptance examples:

1. Operator sets `UZI_PROVIDER_ALLOWED_BASE_URLS=https://gw.example.com/anthropic`. User A creates provider "corp-gw" (engine `claude`, protocol `anthropic_messages`, base URL `https://gw.example.com/anthropic`, auth style `bearer`, model map `opus=corp-opus, sonnet=corp-sonnet, haiku=corp-haiku`) with key K and presses Test: "authenticated, Messages protocol OK" and, separately, "tool use: passed". Saving `https://gw.example.com/other`, `https://gw.example.com/anthropic/../other` or `https://gw.example.com.evil.test/anthropic` is refused with `provider_base_url_not_allowed`.
2. User A selects "corp-gw" as their Claude source and starts an issue run. The worker's Claude processes (main, subagents, in-run advice and eval passes) get `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN=K`, the mapped `ANTHROPIC_DEFAULT_*_MODEL` and `ANTHROPIC_SMALL_FAST_MODEL`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, and no `CLAUDE_CODE_OAUTH_TOKEN`, although user A also holds an Anthropic token. The run card shows the provider and usage and cost "unknown". User A's Anthropic token keeps its own default and is untouched.
3. The operator removes the URL from the list while a provider run is queued. Its next claim is refused with `provider_destination_not_allowed` and the run parks for the owner; it never starts on the Anthropic token. A run whose claim is already held (for example waiting at the plan gate inside the claimed worker) keeps its bound authority until that claim ends; cancelling is the immediate stop (D9).

## Out of scope

- Server lanes (judge, jobs, cross-check): PRD #4322. Until then they refuse a provider-sourced user with `provider_lane_unsupported`.
- The isolated research lane: refuses provider runs until PRD #4322 M3 (its network admits one exact model host today).
- The Codex engine (PRD #4323), admin-shared providers (PRD #4324), per-run or per-schedule provider choice, per-role mixing, usage budgets, the #50 proxy.
- Custom extra headers, mTLS client certificates.
- Private-CA trust for the Claude CLI under `workers.uidSplit`: the existing `workers.extraCA` (#4236) reaches only the worker process there, not the CLI uid (`deploy/chart/values.yaml` comment on `extraCA`). Documented as a limitation; isolated-lane and non-split workers are covered.

## Modules and seams

**1. Operator allowlist (`api/internal/config`)**
- `UZI_PROVIDER_ALLOWED_BASE_URLS`: comma list of exact base URLs. A new path-aware parser (the forge one, `normalizeAllowlistEntry`, is https-only and refuses paths, `api/internal/config/config.go`). Normalization: lowercase host, default port elided, single trailing slash removed; refused outright: query, fragment, userinfo, dot segments, `%2e` / `%2f` encodings, `;`, `\`. A provider URL matches only an equal entry; no prefix or segment matching, so one tenant's path on a shared gateway cannot be repointed to another's.
- https only. Loopback is accepted only when the explicit dev flag `UZI_PROVIDER_ALLOW_LOOPBACK=true` is set (compose dev), never by default, so the probe cannot reach the api's own local ports in production.
- Empty or unset: the feature is off; creation is refused and no provider claim is issued.
- Checked by the api at save, Test, run creation and claim. The api is the destination authority; the worker validates the binding's shape (https or the dev-flag loopback, no userinfo, exclusive with any vendor credential) but holds no copy of the list. A copy would need a controller change and a fleet roll on every edit (`controller/internal/kube/render.go`), and on compose workers the env is user-controlled anyway.
- The worker namespace's `workers.fqdnEgress.allowFQDNs` is namespace-wide (ADR-0285): once an operator admits a gateway host, every restricted-tier run can reach it. It is network reach, not per-run authorization; docs say so.

**2. Provider object (`api/internal/store` + new `api/internal/providers`)**
- Tables `model_providers` (id, owner_user_id, name unique per owner, engine, current_revision, retired_at) and `model_provider_revisions` (provider_id, revision, protocol, base_url, auth_style, model_map jsonb, created_at). A revision is immutable; an edit inserts one. Revisions are retained while any non-terminal run binds one; unbound non-current revisions are pruned.
- Engine and protocol enums ship `claude` + `anthropic_messages`; CHECKs widen additively in PRD #4323.
- `auth_style`: `bearer` (`ANTHROPIC_AUTH_TOKEN`) or `x_api_key` (`ANTHROPIC_API_KEY`).
- Model map keys `opus`, `sonnet`, `haiku`, `fable`; `haiku` is required because the SDK's background calls use the small/fast slot (`ANTHROPIC_SMALL_FAST_MODEL` is set from it). The env names `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU,FABLE}_MODEL`, `ANTHROPIC_SMALL_FAST_MODEL`, `CLAUDE_CODE_SUBAGENT_MODEL`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` were found in the bundled `claude` binary of SDK 0.3.284; the pin is in `agent/package.json`. M1 re-verifies them at the pin offline (`strings <platform package>/claude | grep -E '...'`) and records the result; a missing name blocks M2.
- **Retire vs revoke.** Retiring stops new selections and runs; bound runs keep their frozen revision. Revocation (key disabled, rotated or deleted, URL removed, provider deleted) refuses every later claim of a bound run with a typed reason, preserving each lane's lifecycle: a park-eligible run parks for the owner, a job fails terminally.
- Resource bounds (untrusted input): at most 20 providers per user (enforced in the create transaction under a per-owner lock), name 1..64 chars, base URL at most 2048 bytes, model map at most 8 entries, values 1..256 chars of `[A-Za-z0-9._:/@-]`, key at most 4 KiB.

**3. Provider key (vault)**
- New `user_secrets.kind` `anthropic_compatible_key` (one CHECK-widening migration plus a `store/enums.go` constant, as ADR-1106 did), sealed through the existing vault write path (`api/internal/handler/secrets.go`) with the existing AAD `(user, kind)`. Read `docs/vault-threat-model.md` first.
- `provider_id` column, set at insert, no UPDATE path (tested); one key per (user, provider); read access only by the owner. Residual: a database-write adversary could re-point the association or the unsealed `base_url`; recorded in the threat model (adding provider id to the AAD would not close it, since the URL is unsealed).
- **Material is frozen per run.** A run freezes the key's `enablement_rev` (bumped by rotation and by disable/enable, `api/internal/store/queries/user_secrets.sql`); a claim whose frozen rev no longer matches is refused with `provider_binding_stale`. So a new key is never sent to an old run's frozen destination, and a disable/enable cycle also stales queued runs (accepted). The Codex-only `material_revision` (`OpenByIDOfKindAtRevision`, `api/internal/secretopen/secretopen.go`) is not used here: it is NULL for non-Codex kinds.
- Inserted only through an explicit provider-key route with `is_default=false` and `auto_eligible=false`, never through the first-of-kind auto-default insert (`user_secrets.sql`).
- `auto_eligible` is false; the key never enters the poller, the weekly reserve, rejection marks or pool selection. Disable/enable follows ADR-1732.

**4. Selection**
- New per-engine source setting `users.claude_provider_id` (nullable, on `users` beside `default_harness` (migration 00226); PRD #4323 adds `codex_provider_id`). Single-column FK to `model_providers` with `ON DELETE RESTRICT`: deleting a provider that any user has selected is refused (`provider_in_use_as_source`); retiring is allowed and leaves the source set, so the next run refuses with `provider_source_unusable`. A source is never silently nulled (the `ON DELETE SET NULL` pattern of 00078/00079 would turn a deletion into a vendor fallback). Null leaves the entire existing Claude ladder unchanged (`anthropic_token` default, worker and judge bind modes, auto-selection). Set: Claude runs use that provider. `default_harness` still picks the engine; explicit `--harness claude` uses the Claude source.
- A set but unusable source (retired, URL not allowlisted, key missing or disabled) is a typed refusal (`provider_source_unusable`) before any same-engine or cross-engine fallback. Claude counts as usable for harness resolution when the source is set and usable, or when it is null and today's rule holds.
- Resolution runs inside `createRunAtomic` -> `resolveRunHarnessQ` with tx-bound queries and freezes the binding on the run row: `provider_id`, `provider_revision`, `provider_key_mode` (`user_key` here; PRD #4324 adds `operator_key`), `provider_key_secret_id` (nullable, set for `user_key`) and `provider_key_rev` (the key's `enablement_rev`, or PRD #4324's operator-key revision). The resolver's non-transactional Claude path is fail-closed for provider sources.
- **Inheritance.** Same-run continuation (plan, implement, review, resume, recovery successors, automatic rework, restarts) and that run's in-run model passes use the frozen binding, never the owner's current source. Judges and cross-check checkers keep independent authority (PRD #4322). A schedule with a harness pin resolves the engine's source at fire time.
- **Automatic choice and overflow.** A set provider source makes its engine ineligible for automatic harness choice (`automaticHarnessEligible`, `harness_resolver.go`) and for creation overflow in either direction (`evaluateCreationOverflow`, `harness_overflow.go`): unknown usage is never compared with vendor weekly readings, and a provider user's run never moves to the other engine's vendor credential.
- **Overrides and switching refused.** A run-level credential override (`--token`, a per-run or per-schedule pinned/auto/default override, `api/internal/workersvc/credential_override.go`) on a provider-sourced run is rejected before creation; live switching is refused at switch admission and apply in the #4201 resolver, `set-token`, and `approve --token`, including a vendor run switched to `default` while the user's source is a provider.

**5. Claim binding and worker capability (`api/internal/workersvc`, `agent/src/protocol.ts`)**
- A provider run's claim carries one `provider` object `{provider_id, revision, engine, protocol, base_url, auth_style, credential, model_map}` and no `anthropic_oauth_token`. The builder refuses a payload with both; the agent decoder refuses it too. On vendor claims the field is omitted (not null, not empty), so they stay byte-identical (snapshot-tested).
- Workers advertise capability `claude_provider_v1` (precedent `cross_check_gated_v1`, `api/internal/workersvc/claim.go`). Only advertising workers can claim a provider run; otherwise health reports "no capable worker". Rollout: api first is safe (provider runs wait for a capable worker); workers first is safe (the capability is unused until the api ships). The hosted fleet is pinned separately from the app tier, so the release notes say provider runs need the next worker roll.
- Provider claims ignore the worker's `anthropic_bind_mode` and never write `runs.anthropic_secret_id` or `anthropic_rejected_at`.
- Issue runs (`claim_assembly.go`, `claim_recovery.go`) and chat (`chat.go`) carry the binding. Judge, jobs and cross-check refuse a provider-sourced user with `provider_lane_unsupported` until PRD #4322.
- **Gateway limits.** A gateway 429 or overload on a provider run reuses the existing non-terminal `limit_wait` status (migrations 00204/00206) with a provider reason detail (`provider_gateway_limit`) and a fixed capped backoff as its resume time (1, 5, 15 minutes; after the third, it waits for the owner's Resume). Readers stay the existing ones: the sweeper resumes at the resume time, the Resume handler, and web, CLI and Slack render it as a usage-limit park naming the provider. No subscription reset time is invented, and the vendor pool reads in `limitwait.go` (`runs.anthropic_secret_id`, earliest pool reset) are skipped for provider runs. Jobs fail terminally instead (they never park). The E2E sweep covers the new reason literal.

**6. Worker env (`agent/src/sdk-env.ts`) and every spawn site**
- `buildSdkEnv` takes a discriminated binding: `{kind: "oauth", token}` or `{kind: "provider", ...}`. Provider mode sets `ANTHROPIC_BASE_URL`, exactly one of `ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_API_KEY`, the mapped model keys including `ANTHROPIC_SMALL_FAST_MODEL`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, and pins `CLAUDE_CODE_OAUTH_TOKEN` and the other auth key to `undefined`. All these keys join the protected set a provisioned tool env cannot override; the key joins the redaction set.
- Every worker-side spawn and consumer passes the binding through `buildSdkEnv`: the files consuming `anthropicOAuthToken` today (`runner.ts`, `chat-runner.ts`, `review-runner.ts`, `summary-runner.ts`, `model-pass.ts`, `now-summary.ts`, `diagram-diagnostic.ts`, `claude-cross-check.ts`, `cross-check-runner.ts`, `job-runner.ts`, `judge-runner.ts`, `isolated-runner.ts`) and the two that read `env.CLAUDE_CODE_OAUTH_TOKEN` directly (`code-cross-check-replay.ts`, `pr-description-eval-cli.ts`). Lanes A2 enables are wired here but unreachable until A2 admits their claims.

**7. Model resolution and exclusive routing**
- Every model source resolves through the bound map before content is sent: the lead and every enabled template at creation; summary, classifier, advice and later agent-selection models at their spawn. An unmapped alias or a full vendor model ID is a typed refusal naming it (`provider_model_unmapped`); nothing silently rewrites a role's model.
- An offline test runs the pinned SDK in provider mode against a loopback fake gateway (dev loopback flag on) and asserts every outbound request (main, subagent, auxiliary) reaches only the bound base URL, a redirect is not followed with the key, and nonessential traffic is off. Activation is blocked on this test; a destination it cannot observe is reported as unverified in the PR.

**8. Test probe**
- Two steps against `<base_url>/v1/messages` with the provider's auth style: (a) a `max_tokens: 1` request: 2xx "authenticated, protocol OK", 401/403 "rejected", anything else (other status, redirect, timeout, transport error, non-Messages body) "inconclusive"; (b) after (a) passes, one forced single-tool request: a well-formed `tool_use` "tool use: passed", a well-formed response without it "not supported", timeout, redirect or malformed "inconclusive". Results are stored per revision (latest only) and shown separately; the UI says neither certifies full compatibility.
- Bounds: no redirects, 20 s per step, request body at most 4 KiB, response read capped at 64 KiB, nothing from the body echoed or logged. One probe in flight per owner, four instance-wide, 30 per owner per hour (per owner, so create/retire cycles do not reset it). Dialed from the api; docs say the api must reach the gateway.
- **Private-CA gateways.** The probe's HTTP client trusts the system roots plus the operator's `workers.extraCA` bundle (#4236), mounted into the api pod by the chart when set (same `api.tls.enabled` precondition the value already documents), and used only by this client (not for the api's other outbound calls). Compose has no equivalent: there the probe uses system roots. Without it a private-CA gateway always probes "inconclusive" while runs reach it. Chart change under `deploy/chart/` (no workflow files).

**9. Usage, cost, surfaces**
- Provider runs show usage and cost "unknown" (the SDK prices with the vendor table; mapped names have no valid basis). Token counts still record; run, time and concurrency limits apply.
- Web "Providers" section in user settings (create, edit, test, retire, select as Claude source); run card shows the bound provider. CLI `uzi provider list|create|edit|test|retire|use` (`docs/cli.md`). Docs: a user page, the operator variables in `docs/configuration.md`, the egress note, then `task docs:sync`. An ADR records the allowlist and per-run binding invariants (number assigned when written).

**Reused safety stories**
- The forge allowlist protects admin-entered URLs; here users enter them, so the list is exact with path, checked at four api points, and the probe follows no redirect.
- The Codex binding freeze (ADR-1106) assumed vendor endpoints; here the frozen value also covers the destination and the key revision.

## Testing decisions

- Go unit: parser (look-alike hosts, trailing slash, dot segments, encodings, userinfo, loopback with and without the flag); model-map validation; probe classification against `httptest` (2xx, 401, 403, 404, 500, 302, timeout, oversized, malformed tool response).
- Go LiveDB: source precedence (null keeps the vendor ladder byte-identical; set and unusable refuses with no same- or cross-engine fallback); create-tx freeze; claim exclusivity; capability gating; refusal after URL removal, key disable and key rotation (`provider_binding_stale`); override and switch refusals including the #4201 apply path; a provider run never writes `anthropic_secret_id`; gateway 429 park and retry cap; jobs fail terminally on refusal.
- Agent: `buildSdkEnv` both modes with exact key sets and protected keys; decoder refuses both bindings; every spawn site's env in provider mode; redaction; the exclusive-routing test.
- Web and CLI tests; mock fidelity. Secret-shaped fixtures assembled at runtime (`.claude/rules/prds.md`).
- E2E contract: sweep changed claim, state and reason literals across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh`, `e2e/forge-fake/`; update expectations; run `task test:e2e-lib` and `task test:forge-fake`; name affected unrun phases.

**Validation contract (per milestone, in-run)**: `task check:api` plus `go test -race -count=1` on affected packages; `task check:web` plus touched Vitest files; `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent` plus touched and directly affected agent tests; migration checks; `task docs:sync` + `task check-docs:web`; `check:changelog-entry`; `check:token-literals`; `task scan:secrets` with the canary line in the evidence. Every bug fixed ships a regression watched red then green. Landing obligations outside milestones: **full agent suite: exact-head PR CI**; full `test:api`, `test:web`, `gate:repo` pending exact-head PR CI. Live behaviour (hosted restricted tier, a real gateway) goes in a linked `acceptance` issue owned by the maintainer, not blocking merge.

## Milestones

### M1: A user defines and tests an Anthropic-compatible provider
**Blocked by**: none
Parser, env and dev flag; tables and migration; `anthropic_compatible_key` with immutable association; CRUD with bounds; the probe; web Providers section; CLI `provider list|create|edit|test|retire`; SDK env-name verification at the pin; docs. No run uses a provider yet.
Acceptance: example 1; feature off when the variable is unset; a user cannot read, test or select another user's provider.

### M2: Issue runs and chat run through the user's Claude source
**Blocked by**: M1
`claude_provider_id` and resolver; create-tx freeze; claim binding with capability `claude_provider_v1`; `buildSdkEnv` provider mode at every spawn site; model resolution; exclusive-routing test; override and switch refusals; gateway-limit parking; `provider_lane_unsupported` for judge, jobs and cross-check; isolated-lane refusal (until PRD #4322 M3); usage and cost unknown; CLI `provider use`; ADR.
Acceptance: examples 2 and 3; a rotated key is not sent to an old destination; same-run continuation inherits the frozen binding; a user with a null source sees today's behaviour byte-for-byte (env and claim snapshots).

## Decision Log

- **D1: Provider is a first-class object with immutable revisions.** Reason: per-run binding must freeze the destination, and roles must be able to reference providers later. Rejected: an instance-wide gateway setting.
- **D2: New `anthropic_compatible_key` kind with an immutable provider association; AAD unchanged.** Reason: a subscription token must never reach a gateway, nor a gateway key Anthropic; provider id in the AAD would not stop a database-write adversary who can edit the unsealed URL. Rejected: reusing `anthropic_token`; provider-bound AAD.
- **D3: Operator allowlist in env/Helm, exact URL with path, api-enforced; worker checks shape only.** Reason: a compromised account must not redirect code and keys; shared hosts serve many tenants; a worker copy costs a controller change and fleet rolls without independent authority. Rejected: in-app list; wildcards; a worker-side copy.
- **D4: Claim carries the binding; the env builder is a second layer; workers advertise a capability.** Reason: a server-side exclusion holds for later lanes; old workers must not claim provider runs. Rejected: env-only enforcement with a broadcast gateway config.
- **D5: Per-engine source setting; vendor defaults untouched.** Reason: widening the `is_default` groups breaks a dozen readers (per-kind default index, compatibility upsert, poller, reserve, worker and judge bind ladders) and cannot hold a secretless shared-provider selection. Rejected: making a provider key a member of the vendor default group (an earlier draft).
- **D6: Unmapped model is a refusal, not a rewrite; `haiku` required.** Reason: silent model swaps erase role choices; the SDK's background slot must not leak vendor IDs. Rejected: forcing one model into every slot.
- **D7: Probe splits auth/protocol from tool use and never certifies.** Reason: a gateway can answer and still lack features a run needs. Rejected: one pass/fail.
- **D8: No live credential switching or overrides on provider runs in v1.** Reason: the switch machinery (#4201, credential epochs) is vendor-token-shaped. Rejected: extending it now.
- **D9: Revocation applies at the next actual claim.** Reason: continuation can proceed inside a held claim; cancel is the immediate stop. Rejected: a new pre-continuation admission check in v1.
- **D10: Key material frozen by `enablement_rev`.** Reason: a replaced key must not reach an old run's destination; the Codex `material_revision` is NULL for this kind. Rejected: freezing only the secret id.
- **D11: Gateway limits reuse `limit_wait` with a capped backoff.** Reason: no reset time is known; the existing park already has its readers. Rejected: vendor pool promotion; a new status.
- **D12: Selected providers cannot be deleted; sources are never auto-nulled.** Reason: nulling a source would silently fall back to the vendor ladder. Rejected: `ON DELETE SET NULL`.
- **D13: Provider sources are excluded from automatic harness choice and overflow.** Reason: unknown usage cannot be compared with vendor readings, and overflow would spend the other engine's vendor credential. Rejected: treating unknown as headroom.
