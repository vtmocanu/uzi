# PRD #4322: Custom model providers on the judge, jobs, cross-check and isolated research lanes (Claude engine)

**Issue**: [#4322](https://github.com/vtmocanu/uzi/issues/4322)
**Umbrella**: [#4319](https://github.com/vtmocanu/uzi/issues/4319)
**Priority**: High
**Status**: Draft
**Entry prerequisite**: PRD #4320 complete (whole PRD). No milestone here starts before it.

## Problem

After PRD #4320 a provider-sourced user's issue runs and chat go through their gateway, but the server-assembled lanes refuse them with `provider_lane_unsupported`: the judge (`api/internal/workersvc/judge.go`), jobs (`job_claim.go`) and cross-check (`cross_check_claim.go`). The isolated research lane (ADR-1906, `docs/isolated-research-lane.md`) refuses them too: its network admits exactly one model host by exact name (`workers.isolatedLane.modelHost`, default `api.anthropic.com`, rendered by `deploy/chart/templates/worker-isolated-model-egress.yaml`), so a repo-less research job bound to a site list cannot reach a gateway. Each lane has its own credential authority today:

- The judge credential is a property of the user, not the run (migration 00079; judge bind mode, default `auto`, 00197).
- A cross-check checker runs on the opposite engine from the lead it checks (ADR-2149).
- Jobs never park; a refusal is terminal (`job_claim.go`, `ARCHITECTURE.md` "Jobs").

## Outcome

Each of these lanes can use a provider while keeping its own authority rules:

- **Judge**: keeps the existing judge credential policy. Where that policy resolves to the user's Claude source and the source is a provider, the judge claim carries that provider's frozen binding. A judge pinned to a specific vendor credential keeps using it, even on a provider run's trace (the user chose it).
- **Cross-check**: a checker resolves its own engine's source at creation and freezes it. A Claude checker of a Codex lead uses the user's Claude source (provider or vendor). A Codex checker of a Claude provider lead uses the Codex source (vendor until PRD #4323).
- **Jobs**: a job resolves the owner's Claude source at creation and freezes it like a run. Any provider refusal fails the job terminally with the typed reason.
- **Isolated research lane**: the operator may list extra exact gateway hosts the lane can reach; a provider run or job bound to the lane is admitted only when its provider's host is on that list.

Acceptance examples:

1. User A's Claude source is "corp-gw"; judge bind mode `auto`. A finished provider run's judge claim carries the provider binding, no OAuth token. User B's judge is pinned to Anthropic token T; B's provider run is judged with T, as today.
2. User A has a Codex lead with plan cross-check on. The Claude checker child freezes "corp-gw" and runs through it. Reverse direction: a Claude provider lead's Codex checker uses the Codex vendor source; no Claude key reaches the Codex checker.
3. A job created by a provider-sourced user runs through the provider; removing the URL from the operator list before claim fails the job with `provider_destination_not_allowed`.
4. The operator sets `workers.isolatedLane.providerHosts: [gw.example.com]`. A site-list-bound research job from a user whose Claude source is a provider at `https://gw.example.com/anthropic` runs on the lane through the gateway, and its web content still arrives only through the fetcher. A provider at a host not on the list is refused with `provider_host_not_admitted_isolated`; vendor isolated runs keep reaching `api.anthropic.com` unchanged.

## Out of scope

- The Codex engine (PRD #4323). Changing judge policy itself. Wildcard or per-run lane egress.

## Modules and seams

- **Judge** (`judge.go`), mapping of the existing judge bind mode (00197):

  | judge bind mode | Claude source null | Claude source = provider |
  |---|---|---|
  | pinned | that vendor token | that vendor token (user's explicit choice) |
  | default | vendor default token | the provider |
  | auto | vendor pool | the provider (no pool over unknown usage) |

  When the mapping yields the provider, freeze `(provider_id, revision, key id, enablement_rev)` on the judge row and send the A1 `provider` claim. Pinned vendor judge credentials unchanged. The judge's model resolves through the bound map (`provider_model_unmapped` otherwise).
- **Cross-check** (`cross_check_claim.go`, `agent/src/cross-check-runner.ts`, `claude-cross-check.ts`): the checker child resolves its own engine's source in its own create transaction; the exclusivity check (`cross_check_claim.go`: a Claude checker carries Anthropic auth and no Codex auth, and the reverse) widens to "exactly one of: OAuth token, provider binding" for Claude checkers. **Same-model guard (additive):** opposite-engine selection, the existing credential resolution and checker confinement stay exactly as they are. Once both engines can bind providers, opposite engines no longer guarantee different models (an owner may point the Claude and Codex sources at the same self-hosted model, one through Messages and one through Responses). After resolving the checker, the server compares the frozen bindings' resolved model IDs (after the provider model map) together with the provider base URL host; a known match (same host and same resolved model ID) means no checker child is created, and the lead takes the existing human plan gate through its normal path with the typed reason `cross_check_same_model`, with no checker result fabricated. This detects known matches only; it cannot prove independence (different hosts or names can serve the same model), and the docs say so. A pairing is never treated as independent just because one side is a vendor credential and the other a provider.
- **Jobs** (`job_claim.go`, `agent/src/job-runner.ts`): the job freezes the owner's Claude source at creation; claim carries the provider binding; refusals are terminal.
- **Isolated research lane** (`deploy/chart`, `api/internal/workersvc`, ADR-1906): new value `workers.isolatedLane.providerHosts` (list of exact host names; new render validation in `_helpers.tpl` rejects `*`, a scheme, a port or a path, with render tests; today's `modelHost` check covers only presence and `*`), rendered into the same model-egress objects beside `modelHost` (Antrea: extra `fqdn` Allow rules after every Drop; OVN: extra `type: Allow, dnsName` entries before the final Deny), and into the api env (`api-deployment.yaml`, inside the same `uzi.isolatedLaneEnabled` guard as the fetcher-token env) so the api knows the admitted set; the api does not know `modelHost` today. The chart render guard `scripts/assert-chart-render.sh` (today: exactly one Antrea FQDN rule equal to `api.anthropic.com`, drop-before-allow checked for that one index) is extended: any number of FQDN Allows, each exactly one value from `modelHost` plus `providerHosts` on TCP 443, each after the last Drop; OVN equivalent; fixtures with and without `providerHosts`. `modelHost` stays and vendor isolated runs are unchanged. The lane's TCP 443-only invariant holds (both egress arms pin 443, `worker-isolated-model-egress.yaml`): no port is widened. Admission happens at job creation (where `api/internal/handler/v1_jobs.go` already refuses a missing lane with `isolated_lane_unavailable`) and again at claim (a chart upgrade can change the list): a lane-bound provider binding is admitted only when its base URL's effective port is 443 and its lowercase ASCII (punycode) host equals a normalized `providerHosts` entry (and still on `UZI_PROVIDER_ALLOWED_BASE_URLS`); otherwise `provider_host_not_admitted_isolated`, terminal for jobs, park for runs. Lane workers never take `uidSplit`, so `workers.extraCA` reaches their CLI for private-CA gateways. Lane gateways must resolve to public addresses: the lane drops every non-public and cluster range before the FQDN Allows (the anti-rebinding belt, kept), so an internal or in-cluster gateway times out; the api cannot detect this, so docs say it. On compose there is no lane (`IsolatedLaneEnabled()` false refuses site-list jobs at creation), so no lane-bound provider run exists. The new refusal reason is additive to `/api/v1`: add it to `api/openapi/v1.yaml` if reasons are enumerated and run `check:api-v1-compat`. ADR-1906 is amended, not only documented: its header decision (the lane reaches `api.anthropic.com` only and runs Claude only) and invariant 1 become "model destinations are exactly `modelHost` plus `providerHosts`, operator-listed, exact names, TCP 443"; a listed gateway is a model destination only; FQDN rules are per resolved address (ADR-0285), and a gateway that offers server-side web tools or proxies arbitrary URLs would bypass the fetcher, so docs tell operators to list only gateways without such features enabled. The ADR records this as a new residual: a gateway is an operator-trusted content channel, and the docs rule is advice, not enforcement (today's vendor residual, server-side web search, is narrower because the lane's fixed tool set never requests it). Changing `providerHosts` changes the lane policy objects and the api env (the api pod restarts), not worker pods (no fleet roll); during the rollout window a mismatch fails closed (timeout) or is refused.
- All four reuse A1's capability gate `claude_provider_v1`, claim builder, `buildSdkEnv` provider mode, revocation semantics and probe-free model resolution. Worker-side wiring already exists from A1; this PRD admits the claims.

## Testing decisions

- LiveDB: judge `auto` with a provider source, judge pinned to a vendor credential on a provider run, cross-check both directions (Claude provider checker of a Codex lead; Codex vendor checker of a Claude provider lead), the same-model guard (same host and same model: the lead parks at the human gate with `cross_check_same_model`; same host with different models and mapped aliases resolving to different IDs: checker created; mixed provider and vendor bindings: checker created only when the resolved models differ), and the new reason literal in the E2E sweep (all with synthetic provider bindings, so this PRD takes no implementation dependency on PRD #4323), job freeze and terminal refusal, capability gating per lane.
- Agent: the runners spawn with provider env and no OAuth token.
- Chart: render tests for `providerHosts` (both FQDN providers, render failure on wildcard, scheme or path, absent list renders today's objects byte-identical). Api: lane admission with listed and unlisted hosts, vendor lane runs unchanged.
- Validation contract as PRD #4320, including `task scan:secrets`, watched red/green regressions, and **full agent suite: exact-head PR CI** as a landing obligation.

## Milestones

### M1: Judge and jobs use the provider under their own authority rules
**Blocked by**: none (entry prerequisite above)
Judge branch and freeze; job freeze and claim; refusals; docs.
Acceptance: examples 1 and 3; `provider_lane_unsupported` no longer reachable for judge and jobs.

### M2: Cross-check checkers resolve and freeze their own engine's source
**Blocked by**: none within this PRD (sequenced after M1 for review size, not a code dependency)
Checker creation-time resolution and freeze; widened exclusivity check; both directions tested.
Acceptance: example 2; the same-model guard holds.

### M3: The isolated research lane admits listed gateway hosts
**Blocked by**: M1 (lane work is jobs, whose provider claim M1 adds)
`providerHosts` value, validation and render (both arms); extended render guard in `scripts/assert-chart-render.sh`; api env; creation-time and claim-time admission; v1 reason and compat check; ADR-1906 amendment (invariant 1 and the new residual); `docs/isolated-research-lane.md` (public-address requirement, web-feature warning) and docs sync.
Acceptance: example 4; `provider_lane_unsupported` no longer reachable for any Claude lane.

## Decision Log

- **D1: Judges keep independent authority.** Reason: the judge credential is the user's choice (00079), not the run's. Rejected: inheriting the judged run's binding (an earlier draft).
- **D2: Checkers resolve their own engine's source.** Reason: a checker runs on the opposite engine; inheriting the lead's binding is impossible across engines. Rejected: inheriting.
- **D3: Jobs freeze like runs and fail terminally.** Reason: jobs never park. Rejected: parking jobs.
- **D4: Isolated lane gets an operator list of extra exact gateway hosts.** Reason: keeps `modelHost` and vendor runs unchanged and keeps every lane destination an explicit operator choice. Rejected: swapping `modelHost` to the gateway (breaks vendor lane runs); deriving lane egress from the provider allowlist (would widen the lane whenever a provider URL is approved for ordinary runs).
