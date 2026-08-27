# PRD #110: Checkpoint agent work mid-run — CLOSED (will not implement: unsafe on the primary runtime)

**GitLab Issue**: [#110](https://github.com/vtmocanu/uzi/-/issues/110) — **closed 2026-07-22, will not implement**
**Status**: 🔴 **Closed — will not implement.** Nothing was built. This file is the decision record for *why*, and the condition under which it could be revisited.
**Review**: adversarial review by a Fable agent, 2026-07-22, verified against `main`. It caught the load-bearing fact that reverses the whole proposal — the primary k8s runtime is **single-uid**, not uid-split — which is exactly why this is closed.
**Related**: [#105](https://github.com/vtmocanu/uzi/-/issues/105) (session lost on a different-worker requeue — the loss this would have reduced), [#51](https://github.com/vtmocanu/uzi/-/issues/51) (worker-uid split — compose-only, the safety lever that does **not** cover k8s), [#58](https://github.com/vtmocanu/uzi/-/issues/58) (single-uid non-root start — the k8s posture), [#42](https://github.com/vtmocanu/uzi/-/issues/42) (bounded concurrency — whose credential-exposure analysis this extends)

## Resolution

**Do not add a mid-run checkpoint push.** The idea — have the worker push the
agent's branch at each implement→review iteration boundary so an interrupted run
doesn't lose its committed work — is sound in its goal and small in its
mechanism, but it cannot be made safe on the runtime that matters (hosted k8s)
without either an unproven primitive or a disproportionately large re-architecture.
The security cost is the forge PAT (which can push to the user's repository); the
benefit is surviving an uncommon failure (a different worker re-claiming an
unfinished run) that is already partially mitigated. That trade is not worth it.

## Why it is not safe

The forge PAT is the system's strongest secret on the worker: the worker holds it
only in memory, from the run's claim response, and injects it into the `git push`
child's environment (`GIT_CONFIG_KEY/VALUE` extraHeader, off argv and off disk —
`agent/src/git.ts:214-236`). **The agent must never be able to read it.** Today
that guarantee is airtight, and it is airtight the *simplest possible way* —
**temporally**:

- The one push a run does happens **only after the agent's entire subprocess tree
  is reaped.** `executor.killAgentTree()` runs at `agent/src/runner.ts:325-331`
  (the "B1" audit invariant), and *then* `git.pushBranch(...)` runs at `:385`. At
  the instant the PAT touches a git child, **no agent process is alive** — nothing
  can read `/proc/<push>/environ`. This holds on every runtime, at default config.
  There is no PAT-disclosure window anywhere today.

A checkpoint push is, by definition, mid-run: the loop continues after it
(`agent/src/sdk-executor.ts:512-531`), so the agent is still alive and cannot be
reaped. It therefore **cannot use the temporal closure.** Its only alternative is
a *spatial* closure — the agent and the push child running under **different
uids**, so the agent cannot read the child's environ even while both are alive.
That is exactly what PRD #51's uid split provides… **and it exists only on the
root-started compose path.**

The primary runtime does not have it:

- Hosted **k8s runs single-uid** — `runAsUser: 10001`, PodSecurity `restricted`,
  no `CAP_SETUID` — because the restricted policy forbids the root-entry +
  `setpriv` drop the split requires. The chart says so in as many words:
  "PRD #51's A1 compose mechanism … **cannot run here**, and … v1 is
  single-container at `runAsUser 10001`" (`deploy/chart/templates/worker-namespace.yaml:48-54`;
  `docs/proc-hardening.md:107-136`). The k8s cross-container uid split is the (C)
  two-container model, **deferred to the remote-worker PRD**.
- k8s is the team's **primary runtime and test environment** (`CLAUDE.md`
  §Conventions: "We mostly test in k8s now").

Same uid, same pid namespace means a backgrounded agent process — trivially
planted during any Bash turn (`while true; do cat /proc/*/environ; done &`) and
surviving to the next boundary — reads the PAT out of **every** checkpoint push
window. So a default-on checkpoint push would **introduce a PAT-disclosure window,
at default config, on precisely the runtime where the feature is supposed to
help.** It would be trading the forge push credential for the convenience of not
re-doing some agent work. That is the wrong trade, and it is a regression from
today's zero-window posture.

## Why the safe variants aren't worth it (now)

Two designs would close the window on single-uid. Neither earns its cost here:

- **Reap only the agent's *background* subprocesses at each boundary, keep the
  quiescent SDK session, then push.** The B1 threat is tool-spawned background
  processes; if those are reaped while the SDK session process (which only acts
  when prompted, and we don't prompt it during the push) is spared, the window
  closes on single-uid too. But this needs a "reap descendants, spare the session
  pid" primitive that does not exist (`killAgentTree` reaps the whole tracked tree,
  `runner.ts:331`), and it rests on the SDK session process being **provably**
  inert between turns — an assumption we would be betting the PAT on. Building and
  proving that for this benefit is not justified.
- **Move the push server-side: the worker streams new objects to the API, the API
  pushes with its own PAT.** This genuinely closes the window on every runtime,
  present and future, because the PAT never enters a worker process the agent
  shares. But it is a large new worker→API object-transfer path plus an API-side
  push, and it duplicates push authority into a second component — disproportionate
  to surviving an uncommon mid-run worker death.

The **compose-only** fallback (guard checkpoints on `UZI_UID_SPLIT=1`, off on
k8s) is safe but pointless: it protects only the laptop dev loop and does nothing
for production k8s worker deaths — the case the feature exists for.

## Why the loss it would prevent is tolerable

The data loss is real but narrow and already partly mitigated:

- **Same-worker requeue within the affinity grace** (`WORKER_AFFINITY_GRACE`,
  default 2m) keeps the in-flight SDK session on disk and resumes it — no loss.
- **Work from a *completed* prior run** (or a human, or a prior self-improve
  cycle) is already pushed and is reused on the next claim via `priorCommits`
  (`git.ts:288-299`, `runner.ts:244`, `prompt.ts:167-176`).

The residual loss is specifically: a run interrupted mid-flight **and** re-claimed
by a **different** worker (or after a replaced volume / changed clone path), where
the session is gone (issue #105) — and only the *uncommitted-to-origin* portion,
i.e. this attempt's local commits. That is a genuine but uncommon gap, and closing
it is not worth opening a PAT-disclosure window on the primary runtime.

## When to revisit

Reopen (or fold into that work) **if the k8s two-container uid split lands** — the
deferred remote-worker design where the agent container and the credential-holding
worker container run under different uids with `shareProcessNamespace: false`
(`docs/proc-hardening.md:124-136`, `ARCHITECTURE.md:543`). Once the spatial
closure exists on k8s, a mid-run checkpoint push becomes safe by construction
there, exactly as it already is on compose, and the cost/benefit flips. Until
then, the once-at-the-end push after the agent is reaped stays the right design.

## Note for the next reader — the code docs are correct as-is

Because nothing here ships, the two in-code comments that assert a run "pushes
exactly once … requeued mid-flight left NOTHING behind" (`agent/src/git.ts:139-143`,
`agent/src/prompt.ts:157-159`) **remain true** and must NOT be changed. An earlier
draft of this PRD listed them for correction; that correction is void — do not
apply it.
