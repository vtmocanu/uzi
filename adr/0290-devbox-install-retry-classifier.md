# ADR-0290: The worker's `devbox install` retry classifier is permanent-first and fail-closed

**Status**: Accepted (issue #290 — worker-only "Layer A"; a sustained-outage "Layer B" requeue/park is a deferred follow-up, see Consequences)
**Date**: 2026-08-10
**Deciders**: architect, coders, reviewers
**Issue**: GitLab issue [vtmocanu/uzi#290](https://github.com/vtmocanu/uzi/issues/290)

This is the provisioning twin of [ADR-0284](0284-forge-push-retry-classifier.md): the same permanent-first, fail-closed retry shape, applied to the worker's run-time `devbox install` instead of its final push/MR-create.

## Decision (summary)

The worker's run-time `devbox install` invocation (`agent/src/provision.ts`, in `provisionTools`) is wrapped in a bounded backoff loop (`agent/src/forge-retry.ts` `withRetry`, `DEVBOX_RETRY_SCHEDULE = [1s,4s,10s]`, 3 retries ⇒ 4 attempts) that classifies the error **before** deciding to retry, via a **new** `classifyDevboxError`. The classifier is **permanent-first and fail-closed**: only narrowly-scoped network/timeout phrases are transient, everything else defaults to permanent, and a worker-timeout SIGTERM kill is permanent and checked first. The adjacent, local, no-network `devbox shellenv` step is deliberately **not** wrapped.

## Context

`provisionTools` (`agent/src/provision.ts`) runs a `devbox install` BEFORE the agent starts, to provision the run's tool packages. A single transient network/timeout error there failed the whole run with `tool provisioning failed`, losing the queued attempt with no work produced — the agent never got to run.

The motivating case (judge recommendation on run `c926fa20`, review `18dec207`) was `Timeout was reached (28)` fetching nixpkgs metadata from `api.github.com` — a one-shot metadata fetch that a later attempt clears in seconds. This is the provisioning twin of the forge-push incident #216 that [ADR-0284](0284-forge-push-retry-classifier.md) fixed: a transient blip on a one-attempt path terminating a run whose real work would otherwise have succeeded.

## The decision

Wrap ONLY the `devbox install` invocation — the inner `run("devbox", ["install"], …)` call inside the existing `try`/`catch` — not the local, no-network `devbox shellenv` step. Wrapping at that inner call means the classifier sees the raw `execFileAsync` rejection (its `.stderr` / `.code` / `.killed` / `.signal`), the richest input available; the existing catch still wraps the final failure as `tool provisioning failed (devbox install): …`, unchanged. It composes with the existing tier-2 best-effort degradation in `provision-run.ts` with no change there.

### The load-bearing invariants (mirroring ADR-0284's D9)

1. **Permanent-first, fail-closed.** Only narrowly-scoped network/timeout **phrases** are classified transient (`DEVBOX_TRANSIENT_PATTERNS`): curl codes like `(28)` / `(6)` / `(7)`, `Timeout was reached`, `unable to download`, DNS / connection-reset / connection-refused, TLS handshake, and 5xx from the binary cache or github. Everything else — an unknown package, a malformed manifest, a resolver error — defaults to **permanent** (no retry). The patterns are deliberately phrases, not bare tokens, so a deterministic error like `package 'openssl' not found` is NOT mis-read as transient off a bare `ssl`/`tls` match. This is the direct analogue of ADR-0284's `Could not read from remote repository` caveat: a substring that also appears in a permanent failure must not, on its own, mark an error retryable.

2. **A worker-timeout SIGTERM kill is PERMANENT and checked FIRST.** The install already carries a 10-minute `execFileAsync` timeout; a genuinely hung install is SIGTERM-killed (`killed === true`, `signal` in `{SIGTERM, SIGKILL}`, `code === null`). Retrying that would give up to 4×10min of dead wall-clock, so it is classified permanent up front — and because the kill check runs **before** the pattern match, even a transient-looking stderr on a killed process stays permanent. This also keeps total retry wall-clock far under `RUN_TIMEOUT` (2h).

Both invariants are pinned by the discriminating cases in `agent/test/forge-retry.test.ts` (the `classifyDevboxError` table) and `agent/test/provision.test.ts` (retry-in-`provisionTools` cases).

### Why a separate classifier and a separate schedule

Mirroring ADR-0284's "why a second classifier": the input differs. `classifyDevboxError` inspects nix/curl/devbox subprocess stderr, not the git/forge stderr or HTTP status that `classifyForgeError` reads — so `classifyForgeError` is deliberately **not** extended to cover this hop.

And `DEVBOX_RETRY_SCHEDULE` is a **separate constant**, not a reuse of `FORGE_RETRY_SCHEDULE`. `FORGE_RETRY_SCHEDULE` is pinned by a differential test to `DEFAULT_TERMINAL_RETRY_SCHEDULE` (`client.ts`) — a semantic link to the worker→API terminal-callback hop that the devbox retry does not share; borrowing that constant would couple two unrelated decisions. What IS reused is the backoff **loop**: the generic `withRetry` extracted from `withForgeRetry` (which is now a thin wrapper supplying the forge defaults).

## Consequences

**A single transient nixpkgs-metadata timeout now clears on a later attempt** within seconds and the run proceeds, instead of failing before the agent starts — the fix that would have saved run `c926fa20`. The backoff is bounded (~15s total across the schedule). It needs no new `ClaimConfig` field: the schedule is a worker-side constant, not server-plumbed configuration.

**A sustained outage past the schedule still fails the run exactly as before.** Once the bounded retries are exhausted, the existing catch fires and the run fails as it did today. Surviving a longer outage by requeueing/parking the run is the deferred "Layer B" seam that [ADR-0284](0284-forge-push-retry-classifier.md) already scoped — explicitly out of scope here. Also out of scope: a shared/offline nix cache, and the npm `installJsDeps` path.

**This invariant is the thing to check before "simplifying" the pattern list.** Broadening a transient phrase to a bare token, reordering the SIGTERM-kill check after the pattern match, or flipping the unmatched-case default from permanent to transient would each look like a harmless cleanup and would each risk retrying a deterministic failure or a hung install. The permanent-first ordering, the kill-first precedence, and the fail-closed default are all asserted directly by the discriminating test cases above, not just documented here.
