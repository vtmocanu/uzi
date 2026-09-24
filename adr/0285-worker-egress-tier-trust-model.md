# ADR-285: Worker egress is a two-tier trust boundary, and measurements must be tier-qualified

**Status**: Accepted (records the shipped model; standard-tier enforcement verification tracked in [vtmocanu/uzi#285](https://github.com/vtmocanu/uzi/issues/285))
**Date**: 2026-08-09
**Deciders**: Vlad + agent team (an architect review caught the wrong-tier misread that prompted writing this down)
**Origin**: the model was built across [#83](https://github.com/vtmocanu/uzi/issues/83) (the docker tier), [#50](https://github.com/vtmocanu/uzi/issues/50) (the egress-proxy residual), and [#123](https://github.com/vtmocanu/uzi/issues/123) (§1's tier table and its "state the tier" discipline). This ADR consolidates it after [#283](https://github.com/vtmocanu/uzi/issues/283) — a false-alarm "egress not enforced" issue built on a docker-tier measurement — showed the model was not written down anywhere a reader would find it before drawing the wrong conclusion.

## Decision (summary)

The hosted worker runs in one of two tiers whose **external egress is deliberately opposite**:

- **Restricted / standard** (`uzi-workers`): egress is a default-deny `NetworkPolicy` floor (`deploy/chart/templates/worker-networkpolicy.yaml`, `networkPolicy.enabled` default true) plus an Antrea FQDN allowlist ANNP (`worker-fqdn-egress.yaml`). Only the allowlist is reachable — `cache.nixos.org`, the forge, `*.anthropic.com`, the three Codex hosts `api.openai.com`, `chatgpt.com` and `auth.openai.com` (PRD #1106 D12, below), `search.devbox.sh` (PRD #808), `api.github.com` (allowed since #818), and the CNPG-chart pair `ghcr.io` + `pkg-containers.githubusercontent.com`. Off-allowlist hosts (e.g. `codeload.github.com`) are **blocked** (measured TIMEOUT; `.claude/rules/stack.md`, `ARCHITECTURE.md` hosted-worker section).
- **Docker** (`uzi-workers-docker`, PRD #83): egress is filtered by CIDR, not by name — `0.0.0.0/0` except in-cluster (`worker-docker-networkpolicy.yaml`). It reaches **arbitrary** internet hosts (`api.github.com` 200, `codeload.github.com` 301, `search.devbox.sh` 404). The `api.github.com` and `search.devbox.sh` responses no longer distinguish the tiers, since both hosts are now on the restricted allowlist too; the `codeload.github.com` 301 still does, because that host is off it. This broad reach is the accepted, not-yet-closed residual owned by [PRD #50](https://github.com/vtmocanu/uzi/issues/50)'s egress proxy — **not** a broken control.

Corollary, and the load-bearing operational rule: **an egress measurement is uninterpretable unless it names the worker tier.** A docker-tier reading is indistinguishable from a broken standard-tier allowlist. Before concluding anything about egress enforcement, run `uzi admin workers` and read the worker's `docker:` flag (true = docker tier = broad egress by design), then re-measure on a standard-tier worker. A completed HTTP response (`200`/`404`) to an **off-allowlist** host is itself evidence you are on the docker tier — the standard tier times out on those. A response from an allowlisted host (`cache.nixos.org`, `search.devbox.sh`, `api.github.com`, `ghcr.io`, `pkg-containers.githubusercontent.com`, the forge, `*.anthropic.com`, the Codex hosts) says nothing about tier.

## Context

The worker runs untrusted agent code and holds the decrypted forge PAT plus the user's Anthropic token and/or Codex credential, so its outbound reach *is* a trust boundary: a compromised or prompt-injected agent's exfiltration surface is exactly what it can egress to. The restricted tier's tight allowlist is that boundary. The docker tier was added (#83) to run Docker-in-Docker builds, which need broad registry/internet egress, so it deliberately trades the tight boundary for reach — an accepted residual pending #50's egress proxy.

Both namespaces exist at once on the cluster (docker tier enabled 2026-07-19), and the two give opposite answers to "can the worker reach GitHub?". So an egress probe against whichever worker happens to be up reads as "enforced" or "wide open" purely by the tier it landed on. This has produced two false alarms:

- an operator during #123, whose 200s were briefly read as falsifying the standard-tier premise — which is why #123 §1 mandates "M0 must state which tier every measurement came from";
- again 2026-08-09: a whole false-positive issue (#283) plus a wrong `prds/123` Decision Log entry, both from a docker-tier reading of `api.github.com` 200 / `search.devbox.sh` 404 (the docker-tier row #123 §1 already tabulates), caught by architect review before any code shipped.

## The decisions

### The tiers' egress is opposite, and that is correct
The restricted tier is the default trust boundary; the docker tier trades it for the reach DinD builds need, accepting #50's residual. Neither is a bug; the docker tier's broad egress is by design and is documented as #50's to close.

### Measurements must be tier-qualified
The single most repeatable mistake here is reading a docker-tier egress result as a standard-tier control failure — made at least twice. The mitigation is procedural and cheap: name the tier (`uzi admin workers` → `docker:`), re-measure on the intended tier, and treat a completed HTTP response to an off-allowlist host as evidence of the docker tier. The same rule lives in `ARCHITECTURE.md` (hosted-worker section) and `.claude/rules/stack.md` (which loads when an agent touches `deploy/**`), so the reader meets it before measuring.

### Codex endpoints on the restricted tier (PRD #1106 D12)
The restricted tier's allowlist gains three exact hosts, each on TCP 443 only: `api.openai.com`, `chatgpt.com` and `auth.openai.com` ([PRD #1106](../prds/1106-codex-harness-phase1.md) D12, confirmed by the user 2026-09-04; see also [ADR-1106](1106-codex-harness.md)).

- **Namespace-wide, on purpose.** The allowlist is one namespace-level FQDN policy, so every restricted-tier worker can reach them, including workers that only ever run Claude. Per-run egress is not expressible in a namespace FQDN policy, and a per-harness worker pool was rejected (it would reopen #229 D4).
- **Accepted exfiltration class.** An unauthenticated POST to `api.openai.com` is the same class as one to `*.anthropic.com`, which the tier already allows; the widening is accepted as symmetric with it.
- **Exact names only.** The `chatgpt.com` rule does not cover its subdomains, and there is no `*.openai.com`. The list grows only from a real provisioning failure that names a host (M6), never from prediction.
- **Evidence basis.** `api.openai.com` is the API-key endpoint: `agent/src/codex/config.ts` records that "API-key auth selects `https://api.openai.com/v1`, while `chatgptAuthTokens` selects the ChatGPT Codex backend"; `chatgpt.com` is carried for that backend on D12's stated need (the agent source does not name the host itself). `auth.openai.com` is carried on D12's stated need, not on a measured worker call: the worker's subscription token refresh currently routes through the api (`agent/src/client.ts` `refreshCodex`, `POST /worker/runs/:id/codex/refresh`), not to the provider directly.
- **Ownership.** These are WORKER egress rules. The api separately calls `chatgpt.com` (usage, `DefaultUsageBaseURL`) and `auth.openai.com` (OAuth refresh, `DefaultOAuthBaseURL`) from `api/internal/codexauth/codexauth.go`; the chart has no api egress policy (`deploy/chart/templates/api-networkpolicy.yaml` polices Ingress only), so nothing here governs those calls.
- **Overrides.** Deployment values that define their own `allowFQDNs` replace the chart default (Helm does not merge arrays), so they must carry the same three entries before live Codex verification.

### What this ADR does NOT decide
- Whether standard-tier enforcement is actually **realized** on the cluster (the values file admits "ENFORCEMENT is not proven; no packet has crossed") — that verification is [#285](https://github.com/vtmocanu/uzi/issues/285). The one legitimate tightening it also named (`github.com`→`ghcr.io` for the CNPG chart) is done: the allowlist's CNPG entries REPLACED the previous four GitHub-ish hosts, and `github.com` is gone (`deploy/chart/values.yaml`, `workers.fqdnEgress.allowFQDNs`).
- Closing the docker-tier residual — that is #50.
- Provisioning tools without github egress (tier-1 seed, tier-2 resolution) — that is #123.
- Compose (non-k8s) worker egress — different threat model (single-user laptop loop), out of scope.

## Consequences

- The tier-egress model is now stated where a reader finds it before measuring (this ADR, `ARCHITECTURE.md`, `.claude/rules/stack.md`).
- Any future change to worker egress policy, and any egress measurement, must respect the tier distinction — the invariant a silent change would break.
- #285 carries the outstanding standard-tier realization check (the `github.com`→`ghcr.io` tightening has landed); #50 owns the docker-tier residual.
- Every restricted-tier worker, including one that only runs Claude, can reach `api.openai.com`, `chatgpt.com` and `auth.openai.com` on TCP 443 (PRD #1106 D12). `scripts/assert-chart-render.sh` asserts exactly those three, TCP 443 only, with no wildcard or further OpenAI/ChatGPT host, in both the CI render and the chart defaults; a deployment that overrides `allowFQDNs` must carry the same three.
