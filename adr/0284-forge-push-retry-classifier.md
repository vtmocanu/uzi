# ADR-284: The worker's push/MR retry classifier is permanent-first and fail-closed

**Status**: Accepted (PRD #284 M1/M2 merged — Layer A only; Layer B is a deferred follow-up, see Consequences)
**Date**: 2026-08-09
**Deciders**: architect (design review that scoped the PRD to Layer A), coders, reviewers
**PRD**: [prds/284-forge-push-retry.md](../prds/284-forge-push-retry.md) (GitLab issue [vtmocanu/uzi#284](https://github.com/vtmocanu/uzi/issues/284)) — the PRD carries the milestones, the two-layer split, and the decision log; this ADR carries the one invariant a future edit to the pattern list could silently break.

## Decision (summary)

The worker's final `git push` and its whole `createMergeRequest` call are wrapped
in a bounded backoff loop (`agent/src/forge-retry.ts` `withForgeRetry`,
`[1s,2s,4s,8s,16s]`, ~30s total) that classifies the error **before** deciding to
retry. The classifier (`classifyForgeError`) is **permanent-first**: it checks the
permanent patterns before the transient ones, so an error string containing both
wins as permanent, and an error matching **neither** list also defaults to
permanent. **A PERMANENT forge rejection — auth failure, a protected-branch
guardrail, a non-fast-forward `[rejected]`, 401/403/404/422 — is never retried.**
This is the load-bearing guarantee; the backoff loop around it is the easy part.

## Context

Observed live on issue #216 (run `8fc2fa47`, 2026-08-09): the agent finished, its
diff was already fetched into the worker's bare repo, and a single dropped
HTTP/2 stream on the final `git push` failed the whole run terminally, discarding
completed work that could only be recovered by a fresh run re-spending the
owner's Anthropic budget from scratch:

```
git push origin refs/uzi-runner/agent/issue-216:refs/heads/agent/issue-216 failed:
fatal: unable to access 'https://github.com/vtmocanu/uzi.git/':
HTTP/2 stream 1 reset by server (error 0x2 INTERNAL_ERROR)
```

`RepoCache.pushBranch` had no retry loop at all: a transient stream reset and a
permanent auth/protected-branch rejection were handled identically — one
attempt, then `failed`. The requeue machinery (`requeue_count`,
`RUN_MAX_REQUEUES`) does not help here; it only fires on worker death or a stale
heartbeat, and a failed push comes from a live, heartbeating worker.

## The decision

### D9 — Permanent-first, fail-closed on the unmatched case

The one non-negotiable this PRD exists to protect: **retrying a permanent forge
rejection would be wrong.** A protected-branch or auth rejection is not a
condition that clears on attempt 2 — retrying it does nothing but delay a failure
that should be immediate, and a design that classified loosely enough to retry it
would, in effect, weaken a guardrail by giving a rejected push five more chances
to somehow succeed. So the classifier is built two ways to make that failure mode
structurally hard to introduce:

1. **Permanent patterns are checked first, and a match wins**, even when the same
   error string also contains a transient-looking substring. Git's generic
   `Could not read from remote repository` trailer is deliberately **absent**
   from the transient list for the same reason: it also trails an auth/permission
   denial, so on its own it must not be classified transient. (`PERMANENT_PATTERNS`
   includes `authentication failed`, `protected branch`,
   `pre-receive hook declined`, `remote rejected`, `[rejected]`,
   `non-fast-forward`, `fetch first`, and the 401/403/404 status codes; forge-side,
   a `ForgeError` with any status outside `{0, 429, 408, >=500}` is permanent by
   the same rule.)
2. **An error that matches neither list defaults to permanent** — fail fast is
   the safe default, not "assume transient and retry." A new git/forge error
   string this classifier has never seen is far more likely to be a rejection the
   author didn't think to enumerate than a genuinely transient one; retrying it
   silently would be the wrong failure mode to pick by default.

The push itself is idempotent on retry (non-forced, same commits →
"Everything up-to-date"), and `createMergeRequest` was already idempotent before
this PRD (adopt-existing on a duplicate-MR response, all three drivers) — so
retrying the transient half costs nothing extra even when the first attempt
partially succeeded. The retry loop wraps the **entire** `createMergeRequest`
call, not just its final thrown status: a duplicate POST followed by a transient
`findOpenMr` GET surfaces as a 409/422, which the classifier reads as permanent —
so classifying only the thrown status would fail a run whose MR actually exists.
`agent/src/forge.ts`'s shared `request()` now surfaces a transient 5xx as a
`ForgeError` at the single transport point precisely so this case is caught
uniformly rather than per-callsite.

Both loops are tested against the discriminating cases this invariant depends on
(`agent/test/forge-retry.test.ts`): the #216 stream reset retries; auth failure,
`protected branch`, and non-fast-forward/`[rejected]` each fail fast; a stderr
string containing both a transient and a permanent substring is classified
permanent (precedence); and the bare `Could not read from remote repository`
trailer alone is classified permanent, not transient.

### Why a second classifier, not a shared one

`agent/src/client.ts` already has a transient-vs-permanent decision for the
worker→**API** status-callback hop: `isTransient()` (`status >= 500 || 408 || 429`,
plus network/timeout) driving `DEFAULT_TERMINAL_RETRY_SCHEDULE`
(`[1s,2s,4s,8s,16s]`). `classifyForgeError`/`FORGE_RETRY_SCHEDULE` is a
**deliberate second implementation** of that same shape for the worker→**forge**
hop, which the API classifier never covered (it has no reason to see git stderr).
A shared classifier was rejected because the two hops classify different input —
one an HTTP status the worker's own client produced, the other git subprocess
stderr plus a forge HTTP status — and forcing them through one function would
either leak forge-specific patterns into the API path or lose the pattern
matching the forge hop needs.

Two implementations of one decision is real duplication risk, so drift is guarded
structurally rather than by convention: a differential test
(`agent/test/forge-retry.test.ts`) asserts
`FORGE_RETRY_SCHEDULE` is `deepStrictEqual` to `DEFAULT_TERMINAL_RETRY_SCHEDULE`
imported directly from `client.ts`. Changing one schedule constant without the
other reds this test immediately, rather than the two backoff shapes silently
drifting apart over time.

### The locale pin

`gitEnv` (`agent/src/git.ts`) pins `LANG=C` / `LC_ALL=C` on every worker-side git
subprocess. The classifier matches English-language substrings
(`stream .* reset`, `Connection reset`, `protected branch`, …) against git's
stderr; a localized git build or a non-English `LANG` in the worker's container
would translate that stderr and silently defeat every pattern in both lists —
worse, an unmatched translated string still falls to the permanent default
(fail-closed), so a translated *transient* error would incorrectly stop retrying
rather than incorrectly retry a permanent one. Pinning the locale is what keeps
the classifier's pattern tables meaningful at all, independent of the worker's
host locale.

## Consequences

**Layer A alone fixes #216 and ships here.** A single stream reset clears on
attempt 2 within seconds, so this retry loop — with no other change — would have
saved run `8fc2fa47`. It needs no new `ClaimConfig` field: the schedule is a
worker-side constant, not server-plumbed configuration.

**Layer B — surviving a *sustained* forge outage by parking the run rather than
retrying inline — is a deliberate follow-up, not shipped here.** Layer A's
backoff is bounded (~30s total); once it is exhausted the run still fails today,
same as before this PRD. Review found that a naive `running`-status park cannot
safely absorb a longer outage: `SweepRunningTimeout` fails any `running`
non-chat run whose `started_at` exceeds `RUN_TIMEOUT` (default 2h),
heartbeat-independent, and its clock runs from the run's start rather than from
when the push begins — so an unguarded park would be killed by the wall sweep
mid-retry, with a wasted judge pass queued behind it and a possible late MR
landing on an already-`failed` row. The right guard (a distinct non-terminal
status modeled on `limit_wait`, versus a lighter marker+budget bound) is a real
design choice with tradeoffs on both sides and no urgency behind it — Layer A
already prevents the data loss that motivated this PRD — so it is deferred to a
follow-up PRD rather than rushed in alongside the incident fix. Until that
follow-up ships, a forge outage longer than ~30s still fails the run exactly as
it did before this ADR.

**This invariant is the thing to check before "simplifying" the pattern lists.**
Adding a new transient pattern, reordering the permanent/transient checks, or
changing the unmatched-case default from permanent to transient would each look
like a harmless cleanup and would each risk retrying a guardrail rejection. The
permanent-first ordering and the fail-closed default are both asserted directly
by the discriminating test cases above, not just documented in a comment.
