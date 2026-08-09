# ADR-285: Worker egress is a two-tier trust boundary, and measurements must be tier-qualified

**Status**: Accepted (records the shipped model; standard-tier enforcement verification tracked in [vtmocanu/uzi#285](https://gitlab.example.com/vtmocanu/uzi/-/issues/285))
**Date**: 2026-08-09
**Deciders**: Vlad + agent team (an architect review caught the wrong-tier misread that prompted writing this down)
**Origin**: the model was built across [#83](https://gitlab.example.com/vtmocanu/uzi/-/issues/83) (the docker tier), [#50](https://gitlab.example.com/vtmocanu/uzi/-/issues/50) (the egress-proxy residual), and [#123](https://gitlab.example.com/vtmocanu/uzi/-/issues/123) (§1's tier table and its "state the tier" discipline). This ADR consolidates it after [#283](https://gitlab.example.com/vtmocanu/uzi/-/issues/283) — a false-alarm "egress not enforced" issue built on a docker-tier measurement — showed the model was not written down anywhere a reader would find it before drawing the wrong conclusion.

## Decision (summary)

The hosted worker runs in one of two tiers whose **external egress is deliberately opposite**:

- **Restricted / standard** (`uzi-workers`): egress is a default-deny `NetworkPolicy` floor (`deploy/chart/templates/worker-networkpolicy.yaml`, `networkPolicy.enabled` default true) plus an Antrea FQDN allowlist ANNP (`worker-fqdn-egress.yaml`). Only the allowlist is reachable — `cache.nixos.org`, the forge, `*.anthropic.com`, and the CNPG-chart hosts. Off-allowlist hosts (e.g. `api.github.com`) are **blocked** (measured TIMEOUT, #123 §1).
- **Docker** (`uzi-workers-docker`, PRD #83): egress is filtered by CIDR, not by name — `0.0.0.0/0` except in-cluster (`worker-docker-networkpolicy.yaml`). It reaches **arbitrary** internet hosts (`api.github.com` 200, `codeload.github.com` 301, `search.devbox.sh` 404). This broad reach is the accepted, not-yet-closed residual owned by [PRD #50](https://gitlab.example.com/vtmocanu/uzi/-/issues/50)'s egress proxy — **not** a broken control.

Corollary, and the load-bearing operational rule: **an egress measurement is uninterpretable unless it names the worker tier.** A docker-tier reading is indistinguishable from a broken standard-tier allowlist. Before concluding anything about egress enforcement, run `uzi admin workers` and read the worker's `docker:` flag (true = docker tier = broad egress by design), then re-measure on a standard-tier worker. A completed HTTP response (`200`/`404`) is itself evidence you are on the docker tier — the standard tier times out.

## Context

The worker runs untrusted agent code and holds the decrypted forge PAT and the user's Anthropic token, so its outbound reach *is* a trust boundary: a compromised or prompt-injected agent's exfiltration surface is exactly what it can egress to. The restricted tier's tight allowlist is that boundary. The docker tier was added (#83) to run Docker-in-Docker builds, which need broad registry/internet egress, so it deliberately trades the tight boundary for reach — an accepted residual pending #50's egress proxy.

Both namespaces exist at once on dev-cluster (docker tier enabled 2026-07-19), and the two give opposite answers to "can the worker reach GitHub?". So an egress probe against whichever worker happens to be up reads as "enforced" or "wide open" purely by the tier it landed on. This has produced two false alarms:

- an operator during #123, whose 200s were briefly read as falsifying the standard-tier premise — which is why #123 §1 mandates "M0 must state which tier every measurement came from";
- again 2026-08-09: a whole false-positive issue (#283) plus a wrong `prds/123` Decision Log entry, both from a docker-tier reading of `api.github.com` 200 / `search.devbox.sh` 404 (the docker-tier row #123 §1 already tabulates), caught by architect review before any code shipped.

## The decisions

### The tiers' egress is opposite, and that is correct
The restricted tier is the default trust boundary; the docker tier trades it for the reach DinD builds need, accepting #50's residual. Neither is a bug; the docker tier's broad egress is by design and is documented as #50's to close.

### Measurements must be tier-qualified
The single most repeatable mistake here is reading a docker-tier egress result as a standard-tier control failure — made at least twice. The mitigation is procedural and cheap: name the tier (`uzi admin workers` → `docker:`), re-measure on the intended tier, and treat a completed HTTP response as evidence of the docker tier. The same rule lives in `ARCHITECTURE.md` (hosted-worker section) and `.claude/rules/stack.md` (which loads when an agent touches `deploy/**`), so the reader meets it before measuring.

### What this ADR does NOT decide
- Whether standard-tier enforcement is actually **realized** on dev-cluster (the values file admits "ENFORCEMENT is not proven; no packet has crossed") — that verification, plus the one legitimate tightening (`github.com`→`ghcr.io` for the CNPG chart), is [#285](https://gitlab.example.com/vtmocanu/uzi/-/issues/285).
- Closing the docker-tier residual — that is #50.
- Provisioning tools without github egress (tier-1 seed, tier-2 resolution) — that is #123.
- Compose (non-k8s) worker egress — different threat model (single-user laptop loop), out of scope.

## Consequences

- The tier-egress model is now stated where a reader finds it before measuring (this ADR, `ARCHITECTURE.md`, `.claude/rules/stack.md`).
- Any future change to worker egress policy, and any egress measurement, must respect the tier distinction — the invariant a silent change would break.
- #285 carries the outstanding standard-tier realization check and the `github.com`→`ghcr.io` tightening; #50 owns the docker-tier residual.
