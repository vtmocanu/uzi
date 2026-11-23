# ADR-4320: Custom model providers (Anthropic-compatible gateways)

**Status**: Accepted (PRD #4320, issue #4320). Activated 2026-11-19 without the loopback relay, accepting the redirect risk (owner decision, #4505); the 2026-11-18 decision to ship blocked is superseded. See [Known, accepted limit](#known-accepted-limit-the-cli-follows-redirects-activated-without-the-relay).
**Date**: 2026-11-18
**Deciders**: the maintainer + agent team; the owner decisions of 2026-11-18 are listed below.
**Related**: [PRD #4320](../prds/4320-custom-providers-core-anthropic.md) (the Decision Log carries D1-D14 and the per-milestone findings), [ADR-1106](1106-codex-harness.md) (the Codex binding freeze this extends to a destination), [ADR-1732](1732-disable-credentials.md) (disable/enable), [ADR-4201](4201-in-place-credential-switch.md) (the switch machinery providers opt out of), [docs/vault-threat-model.md](../docs/vault-threat-model.md#residual-risks-accepted).

## Decision (summary)

A user can define a **provider**: a named endpoint speaking the Anthropic Messages protocol, with its own key and model map, at a base URL the operator allowed. The user selects it as their Claude source; issue runs and chat then run through it with no Anthropic credential anywhere in the run. The destination is authorized by an operator allowlist of exact URLs, frozen onto each run at creation, and re-checked by the api at the next claim. The worker holds no copy of the list.

Shipped: the allowlist, provider and key storage, the Test probe, the Claude-source selection (`PUT /api/me/providers/claude-source`, `uzi provider use <name|id>|--none`, and a Settings control), the run-card provider display, the frozen per-run binding, the claim `provider` object, the worker env and spawn sites, model resolution, gateway-limit parking for issue runs, lane refusals, and `cost_status='unreported'`.

Not shipped (see the PRD for the milestone notes):

- Real-gateway and hosted-tier validation. Activation itself shipped on 2026-11-19 (`agent/src/worker.ts` advertises `claude_provider_v1`, pinned by `agent/test/worker-provider-capability.test.ts`), but nothing is validated against a real gateway or the hosted tier; that is owner acceptance, pending in #4473.
- A loopback relay that would stop redirects. Parked as #4496, the future mitigation.
- Server lanes (judge, jobs, cross-check) and the isolated research lane (PRD #4322), the Codex engine (PRD #4323), admin-shared providers (PRD #4324).

## Context

The Claude engine always called Anthropic, and the only Claude credential kind, `anthropic_token`, is a subscription token that the usage poller, auto-selection and limit-wait all treat as such. Storing a gateway key there would mix the two: the key would reach Anthropic or the subscription token a gateway. A gateway user also could not start a Claude run at all, because Claude usability was "the user holds an enabled Anthropic token".

Users, not admins, enter provider URLs and keys. A compromised account must not be able to point code and keys at an arbitrary host, and one tenant's path on a shared gateway must not be repointable to another tenant's.

## Decisions

### D1. The operator allowlist is an exact URL, path included

`UZI_PROVIDER_ALLOWED_BASE_URLS` is a comma list of exact `https` base URLs **with a path** (`api/internal/config`, a new parser: the forge parser is https-only and refuses paths). A provider URL is accepted only when it equals an entry after normalization (lowercase host, default port and one trailing slash dropped). There is no host, prefix or segment matching. A query, fragment, userinfo, dot segment, percent-encoding, `;`, `\` or non-ASCII character is refused. Empty or unset turns the feature off. Loopback hosts, and `http` for them, are accepted only with `UZI_PROVIDER_ALLOW_LOOPBACK`, so the probe cannot reach the api's own ports in production. The operator-facing grammar is in [docs/configuration.md](../docs/configuration.md#custom-model-providers-prd-4320).

The **api is the destination authority**; the worker validates only the binding's shape (https or the dev-flag loopback, no userinfo, exclusive with any vendor credential). A worker copy would need a controller change and a fleet roll on every edit and, on compose workers, would be user-controlled.

Enforcement points, all in the api:

1. **Save and edit** (`api/internal/handler/providers.go`): every save re-checks the URL it carries, a key save included, and stores the matching list entry.
2. **Probe** (`api/internal/providers/probe.go` through the handler): re-checked right before connecting.
3. **Run creation** (`api/internal/workersvc/provider_source.go`, inside the create transaction): the freeze refuses a provider whose URL is not allowlisted (`provider_source_unusable`).
4. **Claim**, for issue runs and chat (`api/internal/workersvc/provider_claim.go`, `chat.go`): a bound run whose frozen URL is no longer listed is parked `paused` with hold reason `provider_destination_not_allowed`. It never starts on the Anthropic token.

The worker namespace's `workers.fqdnEgress.allowFQDNs` is namespace-wide ([ADR-0285](0285-worker-egress-tier-trust-model.md)). Once an operator admits a gateway host, every restricted-tier run can reach it. That is network reach, not per-run authorization.

### D2. A separate key kind, frozen binding, retire versus revoke

The key is a new `user_secrets.kind`, `anthropic_compatible_key`, sealed through the existing vault path with the existing AAD `(user, kind)`. It is inserted only through an explicit provider-key route with `is_default=false` and `auto_eligible=false`, so it never enters the poller, the weekly reserve, rejection marks or pool selection. `user_secrets.provider_id` is set at insert and has no update path.

A run freezes its binding on the run row at creation, in the create transaction: provider id, revision, key mode (`user_key`), key secret id and the key's `enablement_rev` (the `model_provider_*` columns on `runs`). A revision is immutable; an edit inserts one. Revisions are retained while any run row references one (`PruneModelProviderRevisions` deletes only non-current revisions no run row references, in a statement after the edit commits).

- **Retire** stops new selections and fresh runs. Bound runs and inherited successors keep their frozen revision.
- **Revoke** is any of: key disabled, rotated or deleted (the `enablement_rev` no longer matches), the provider revision gone, or the URL removed from the list. A revocation refuses the next claim of a bound run with a typed hold: `provider_binding_stale` (final for that run; start a new run, which picks up the current key) or `provider_destination_not_allowed` (the promoter re-queues the run if the URL is allowed again). A disable then enable cycle also stales queued runs; that is accepted.
- Revocation applies at the **next actual claim**, never inside a held claim. A run waiting at the plan gate inside its claimed worker keeps its bound authority until that claim ends; cancel is the immediate stop.

### D3. No foreign key from runs to the provider tables

The provider-key uniqueness index is built in its own `NO TRANSACTION` migration with `CREATE UNIQUE INDEX CONCURRENTLY`. A partial index still scans existing `user_secrets` rows, so it must not share the column-add transaction's `ACCESS EXCLUSIVE` lock. The index migration drops its owned index before building, so retries replace an invalid concurrent build. Its Down retains uniqueness until the earlier guarded provider-column Down succeeds and removes the dependent index; a partial downgrade must not drop uniqueness and then refuse live keys.

Guarded provider-key and user-source ALTER Downs take `ACCESS EXCLUSIVE` before their guard reads, as the runs Downs do. Taking the final lock up front avoids a reader-then-writer transaction deadlocking against a later lock upgrade. The five-second lock acquisition bound and live-data refusals remain.

`runs` is a busy table. The `model_provider_*` binding columns carry only a CHECK that they are all null or all set (`runs_model_provider_binding_check`). There is deliberately no FK to `model_providers`, `model_provider_revisions` or `user_secrets`, so deleting a provider or key never cascades into runs and never takes a lock on `runs`. A binding that outlives its target is **detected at claim**, as `provider_binding_stale`, and is never nulled. Nulling would turn a revocation into a silent vendor fallback.

The same reason shapes selection: `users.claude_provider_id` has a composite FK to `model_providers` with `ON DELETE RESTRICT` (migration 00479), so deleting a selected provider is refused (`provider_in_use_as_source`) and the source is never auto-nulled. A set but unusable source (retired, URL removed, key missing or disabled) is a typed refusal, `provider_source_unusable`, before any same-engine or cross-engine fallback. A null source leaves the existing Claude ladder unchanged.

### D4. Route split: what a Bearer token may do

Under `/api/me/providers` (`api/internal/handler/routes_me.go`):

- **Bearer (`RequireUser`)**: list, get, create (keyless), patch of name and model map, test, retire, and `PUT /claude-source`.
- **Cookie only (`RequireAuth`)**: key PUT and DELETE, key enabled PATCH, endpoint PATCH (base URL, auth style) and DELETE of a provider.

Everything that handles the key or moves the destination is cookie-only for the same reason `/me/secrets` writes are: a stolen `uzc_` token must not replace a key or repoint where the key is sent. `use` is safe over Bearer because it reveals no key and cannot move a destination: it only selects among the caller's own providers, and is refused unless the provider is usable and allowlisted. Test is safe over Bearer because it sends the stored key to the stored, allowlisted destination and returns only two outcome words.

### D5. Inheritance of the binding

Same-run continuation (plan, implement, review, resume, recovery, restart, automatic rework) and that run's in-run model passes use the frozen binding, never the owner's current source. Successors split by whether they have a reliable source run:

- **Inherit**: `mr_rework`, task review, then-fix and chat copy the source run's binding verbatim after re-validating it (a revoked key still refuses with `provider_binding_stale`). A retired provider still serves an inherited successor.
- **Resolve fresh**: CI-fix and handoff tasks have no reliable source run, so they resolve against the owner's current source.

A schedule with a harness pin resolves the engine's source at fire time. A set provider source makes its engine ineligible for automatic harness choice and for creation overflow in either direction: unknown usage is never compared with vendor readings, and a provider user's run never moves to another engine's vendor credential.

Run-level credential overrides (`--token`, per-run or per-schedule overrides) and live switching (the ADR-4201 resolver, `set-token`, `approve --token`) are refused on provider runs, including a vendor run switched to `default` while the user's source is a provider. The switch machinery is vendor-token-shaped; extending it is out of scope.

### D6. Issue runs wait out a gateway limit on a fixed schedule; chat ends the turn

A gateway 429 or overload on an issue run reuses the existing non-terminal `limit_wait` with `rate_limit_type='provider_gateway_limit'` (migration 00488) and a fixed capped backoff as its resume time, keyed on `runs.model_provider_limit_streak` (the value before the park):

| Streak before the park | Retry time |
|---|---|
| 0 | now + 1 minute |
| 1 | now + 5 minutes |
| 2 | now + 15 minutes |
| 3 or more | owner wait: `retry_not_before` is NULL |

Owner Resume resets the streak. The vendor pool reads in `limitwait.go` (`anthropic_secret_id`, earliest pool reset) are skipped for provider runs, and no subscription reset time is invented. A run set not to wait out limits fails instead of parking, and jobs fail terminally.

**Chat ends the turn with no park.** This is the owner decision recorded in the PRD Decision Log as D15 ("Provider chat keeps chat's turn-ending behaviour on a gateway limit"): a chat turn that hits a gateway 429 or overload emits an error naming `provider_gateway_limit` and is not retried or parked (`agent/src/chat-executor.ts`).

Migration 00488's Down refuses only while a run is still `limit_wait` on `provider_gateway_limit`, and maps terminal or resumed rows to `unknown`.

### D7. The Test probe: direct, pinned, never follows a redirect

The probe (`api/internal/providers/probe.go`) is dialed from the api and does two steps against `<base_url>/v1/messages`: a `max_tokens: 1` request, then, if that passes, one forced single-tool request. Bounds: no redirects (`CheckRedirect` returns `http.ErrUseLastResponse`, proven by `TestProbeNeverFollowsACrossOriginRedirect`), 20 s per step, 4 KiB request, 64 KiB response read cap, nothing from a body echoed or logged. Neither result certifies full compatibility.

**Credential/destination snapshot.** The handler captures the provider revision, key identity and key enablement revision together, opens the key, then re-reads that snapshot before calling the probe. A changed revision, replaced/rotated/disabled key or retired/deleted provider returns 409 without sending. Key rotation keeps the secret ID and does not bump the provider revision, so both revisions are required. A later edit can leave an in-flight probe using the old key with its old endpoint, but cannot pair the new endpoint's key with the old endpoint. No database lock is held over the network request.

**Dialing and address policy.** The probe client has `Proxy: nil` (proxy environment variables are ignored), resolves the host itself, checks every answer, and connects only to the checked addresses. It always refuses unspecified, link-local (which includes cloud metadata `169.254.169.254`), `fd00:ec2::254`, multicast and broadcast. Loopback is refused unless `UZI_PROVIDER_ALLOW_LOOPBACK`. RFC 1918, IPv6 unique-local and CGNAT ranges are admitted. If any answer for a host is refused, the whole host is refused (mixed answers are not filtered).

This differs from the general fetcher's **public-only** policy (the forge SSRF guard) on purpose. That guard protects URLs an admin typed in blind. Here the operator has already approved the exact URL, and corporate gateways are routinely on private addresses, so admitting private ranges loses no protection the allowlist did not already give, while still refusing the classes (metadata, link-local, loopback) that reach the api's own surroundings.

**Limiter.** One probe in flight per owner, four instance-wide and 30 per owner per hour. The counters are **in memory per api process**, so they reset when the api restarts. The hourly slot is spent on admission, even if opening the stored key then fails.

**Private CA.** The probe's client trusts the system roots plus `UZI_PROVIDER_EXTRA_CA_FILE` (the chart mounts `workers.extraCA` into the api pod). Only the probe uses it.

### D8. Usage and cost are unknown

The SDK prices a run with the vendor table, so the mapped gateway model names have no valid basis. Provider runs record `cost_status='unreported'` (shown as unknown) and still record token counts. Run, time and concurrency limits apply.

### D9. Lanes that cannot serve a provider refuse

The judge, job and cross-check lanes refuse **any run of an owner whose source is set**, vendor runs included, with `provider_lane_unsupported` (the refused judge is a visible skip). The isolated research lane refuses provider runs. These lanes get providers in PRD #4322.

### D10. Residual: database write

An attacker with database write could repoint a binding or the unsealed `base_url`. Binding the provider id into the AAD would not close that, because the URL is unsealed. It is the same residual as the other secrets, recorded as residual 9 in [docs/vault-threat-model.md](../docs/vault-threat-model.md#residual-risks-accepted).

## Context window

The `CLAUDE_CODE_MAX_CONTEXT_TOKENS` key is process-wide, so it cannot differ between the lead and subagents.

- **Fallback.** For a provider credential (never OAuth) with no `context_window` in any mapped slot, the worker sets it to 200000 from `PROVIDER_FALLBACK_CONTEXT_WINDOW` in `agent/src/sdk-env.ts` (owner decision, 2026-11-22, #4611). At Claude Code 2.1.325 the CLI's own default for an unrecognized id is 1M, so without this a gateway model compacts far too late. Measured: the fallback compacts at 170k, for lead and subagent.
- **Process-wide rule.** The smallest configured `context_window` across all mapped models applies to the whole process. A partially specified map uses the smallest explicit window for every model: a documented limitation, not changed.
- **Measured exceptions (Claude Code 2.1.325).** `claude-corp-custom` and `corp-claude-x` honour a 50000 window (compaction at 20k). `claude-sonnet-4-5` and `us.anthropic.claude-sonnet-4-5` honour neither a configured window nor the 200k fallback (no compaction up to 605k reported tokens); the CLI logged no unrecognized-model warning for them. We record what was seen, not why. The web form and the `uzi provider` CLI warn for any model name containing "claude".

Evidence and history: the Decision Log in [PRD #4320](../prds/4320-custom-providers-core-anthropic.md), the 2026-11-17, 2026-11-18 and 2026-11-22 entries.

## Rollback hazards

- **Image-only api rollback.** Pause or cancel queued provider runs and gateway waits first. An older api does not know the `provider_gateway_limit` type, the provider holds or the claim `provider` object.
- **Migration Downs refuse live gateway waits.** 00488's Down raises while any run is `limit_wait` on `provider_gateway_limit`. Resume or cancel those runs, then downgrade.

## Known, accepted limit: the CLI follows redirects, activated without the relay

The pinned Claude CLI (SDK 0.3.319, CLI 2.1.319) follows HTTP redirects from the gateway. `agent/test/provider-exclusive-routing.integration.test.ts` characterizes it against a loopback fake gateway:

- A 307 to a same-origin sibling path is followed and the key travels in **both** auth styles.
- A 307 to another origin is followed: `Authorization` is dropped, but `x-api-key` is forwarded and the request body is re-sent.

So a gateway (or anyone who can make it answer with a redirect) can cause the key to be re-sent to another path on the same host under either auth style, and the request body (prompts, plus code and tool output the agent read) to be re-sent to any `Location` host it names. With `x_api_key` auth the key goes cross-host too; Bearer drops `Authorization` cross-origin. The test characterizes the cross-origin case with a second loopback origin, so "cross-host" here is a cross-origin result, not a measurement against a different real host. The PRD made activation depend on a test asserting that a redirect is not followed with the key, and the test shows the property does not hold.

### History: ship blocked (owner, 2026-11-18), superseded

On 2026-11-18 the owner decided to ship with activation blocked: workers did not advertise `claude_provider_v1`, so a provider-bound run stayed queued with the health reason "no worker that supports model providers is online; waiting for a worker roll". The PRD's Decision Log rejected "advertising the capability with the redirect leak documented" and deferred a loopback relay (a worker-side relay that holds the key and refuses redirects) to a separate issue. Ephemeral capability-gap provisioning also excluded provider-bound runs during that dormant phase. The decision below supersedes this.

### Decision (owner, 2026-11-19): activate without the relay, accept the redirect risk

Custom providers are activated for every install without the loopback relay; the relay (#4496) is parked as the future mitigation. The owner accepts the redirect risk stated above. Consequences of the decision:

- **The allowlist is the only control on where provider traffic starts.** The operator allowlist (`UZI_PROVIDER_ALLOWED_BASE_URLS`) cannot stop a redirect once the CLI is talking to an allowed gateway. Operators should allowlist only gateways they trust not to redirect, and users should prefer Bearer auth, which limits cross-host key exposure. The routing integration test pins the redirect behaviour, so an SDK bump that changes it turns red and prompts a review of this section.
- **Workers advertise `claude_provider_v1` by default**, with no switch. The worker still refuses a provider claim it did not advertise for, and the api's claim assembly keeps its Go-side backstop. Old workers never claim provider runs; the run queues with the reason above.
- **Ephemeral provisioning treats a provider-bound run as demand** (the temporary query exclusion is removed). An online ephemeral worker on an old image (no capability) bound to a provider run is reaped and re-provisioned. Bounded-concurrency re-provision churn is accepted until the worker image pin catches up; total cycles are not bounded while an old image override remains. Waking a hibernated worker for a provider chat requires the capability.
- **Rollout.** No new switch. The release helper autobumps the chart's worker image pin. Operator image overrides, Compose and separately run workers must be updated by their operator; old persistent workers leave provider runs queued.

### Decision (owner-approved, 2026-11-20): redact the provider key and block pushes that carry it

The redirect risk concerns where the key can go on the wire. Activation also makes a gateway key a secret the API's pattern-based scrubber cannot recognize, since an arbitrary gateway key has no known shape. Two worker-side controls cover the run's own outputs, both keyed on the exact key bound to the run:

- **Redaction.** The key and its encoded forms (raw, JSON-escaped, `\/`, HTML-safe `\u00XX`, percent-encoded) are redacted before any clamp from model-pass output, usage and errors and logs (review, intent, plan and PR-description summaries), run and chat messages and failure reasons, the lead PR summary, the plan gate, completion reports, MR thread replies, findings, memory, chat issue proposals, progress labels and milestone ids.
- **Exact-key push scan.** A worker-side scan blocks any forge upload whose objects carry the bound key in any encoded form: the finalize branch push on every forge, checkpoint packs (including park, pause, shutdown, capture and overlay) and held publication. A hit or a scanner failure blocks the upload (`push_secret_blocked` / `secret_remediation_pending`) and keeps the work local. Vendor runs are unaffected.

Limits of the scan: recovery-bundle uploads to the uzi API custody archive are not scanned (a private archive, not the forge). For the finalize branch push only (not checkpoint packs), objects reachable only from a stale remote-tracking ref (the branch was deleted or force-pushed on the forge) are excluded, since they came from the forge. A provider run whose new objects exceed the scan caps (128 MiB, 100k objects, 60 s) has its checkpoints withheld and its final push refused.

## Consequences

- Operators can allowlist gateway URLs and users can define, test, retire and select providers (Settings, `uzi provider use`, API).
- Provider runs start on workers that advertise `claude_provider_v1`, with the redirect risk above accepted (owner, 2026-11-19). Not validated against a real gateway or the hosted tier: owner acceptance is pending in #4473.
- Docs: [docs/providers.md](../docs/providers.md), [docs/configuration.md](../docs/configuration.md#custom-model-providers-prd-4320), [docs/run-limit-wait.md](../docs/run-limit-wait.md#a-custom-providers-gateway-rate-limit), [docs/cli.md](../docs/cli.md#model-providers-uzi-provider), and [ARCHITECTURE.md](../ARCHITECTURE.md#custom-model-providers-prd-4320).
