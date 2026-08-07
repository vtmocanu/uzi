# ADR-122: Checkpoint pushes are brokered through the api, not run on the worker

**Status**: Accepted (design decision; PRD #122 M8 is deferred, so this records the
decision ahead of implementation). Supersedes the "no credentialed mid-run push"
conclusion of PRD #110 for the checkpoint case, by a route #110 did not consider.
**Date**: 2026-08-07
**Deciders**: a three-agent, code-grounded security review (researcher + auditor +
architect) that reached a unanimous recommendation; team lead; Vlad (maintainer),
who requested per-checkpoint publish "as secure as possible".
**PRD**: [PRD #122](../prds/122-milestone-structured-runs.md), milestone M8 and
Decision 14 — this ADR carries only the decision, its security invariant, and the
alternatives, because each alternative reads as attractive again to anyone who has
not walked the single-uid k8s attack.
**Numbering**: `0122` is a PRD number (`prds/122-*.md`), like `0035`/`0042`/`0065`;
`0106` is an issue number. ADRs are numbered by their tracking item.

## Decision (summary)

M8 publishes the run's branch to origin at each milestone checkpoint so work
survives a **different** worker re-claiming the run (which M6's fetch-back cannot:
its `refs/uzi-runner/<branch>` tracking ref lives on one worker's PVC — #218 R1).

The checkpoint push is **brokered through the api**, not run on the worker:

1. After M6's reap + `fetchAgentBranch`, the worker already holds the agent's
   objects in its own bare as `refs/uzi-runner/<branch>` with **no credential**.
2. The worker ships the delta pack (`origin/<branch>..refs/uzi-runner/<branch>`) to
   a new, **authorization-scoped** `POST /api/worker/runs/{id}/publish` — scoped to
   the run's own repo and branch via the worker join token that already scopes
   claims. A worker can never ask the api to push an arbitrary repo or ref.
3. **The api pushes to origin with the PAT it already decrypts** (`secretbox` lives
   in the api), targeting `refs/heads/<branch>`, never forced, CI suppressed
   (`ci.skip`), through a pure-Go smart-HTTP client (go-git), reusing the existing
   `FORGE_ALLOWED_BASE_URLS` SSRF allowlist.

No PAT-bearing git child ever runs under the agent-reachable uid.

## Context

On hosted k8s (PRD #58, the primary runtime) the worker is single-uid: **agent uid
== worker uid == 10001**, no `CAP_SETUID`. The PAT reaches the worker in the claim
and enters a process environ only when the worker spawns a git child for a push
(injected as `GIT_CONFIG_VALUE_n = "Authorization: Basic …"`). Any same-uid process
can read that child's `/proc/<pid>/environ` with a plain `open()`/`read()` — seccomp
`RuntimeDefault` + `Drop ALL` blocks `ptrace`/`process_vm_readv` (the heap) but not
a same-uid environ read.

PRD #110 closed mid-run push on exactly this ground. Decision 8 argued that reaping
the agent tree before the push restores "temporal closure". The 2026-08-07 review
**demonstrated that the reap is escapable**: `killAgentTree` process-group-kills
only the SDK CLI pid, and a `setsid`/new-session child (writable as an unscanned
script file, then `setsid bash loot.sh &`, both ALLOWED by the guardrail) escapes
the group kill, survives across checkpoints, and reads the next push child's
environ. M8 would turn today's single end-of-run push window into **N windows
interleaved with a re-spawning agent** — the exposure multiplied, not merely
repeated.

uzi's architecture already names the api as "the sole holder of secrets/keys", with
workers outbound-only to the api. That is the boundary this decision reuses.

## The security invariant

> No forge PAT may be present in the environment or memory of any process the
> untrusted agent can read (same uid + pid-namespace) while any agent-controlled
> process is alive.

On single-uid k8s this is satisfiable two ways: **temporal** closure (prove no agent
process is alive at the push instant) or **spatial** closure (keep the PAT out of
the worker's process space entirely). Reap-then-push attempts the temporal form and
fails it, because it depends on `killAgentTree` catching a `setsid` escapee at each
of N windows. The broker achieves the **spatial** form — the PAT never enters the
worker for the push — which does not depend on the reap being complete, and is
therefore strictly stronger.

## Consequences

- **The api gains its first git capability and its first outbound git push.** It has
  only ever spoken forge REST; the broker adds a pure-Go smart-HTTP push client
  (go-git) and a binary/pack upload endpoint (the existing worker→api channel is
  JSON-only, `DisallowUnknownFields`, 1 MiB cap). New surface on the
  secret-sensitive component — but the api already holds every PAT, so this adds no
  new *secret* exposure, only new code to audit (pin + vendor + govulncheck).
- **The api image stays distroless-static.** A 2026-08-07 source check confirmed
  go-git's `PushOptions.Options []string` carries git push-options, so `ci.skip` is
  expressible with no git binary. Adding a git binary was considered and rejected: it
  would fatten the secrets-holder's base image and CVE surface for no capability the
  pure-Go path lacks. (The git-binary cost is image posture, not a new PAT-leak
  vector — the api has no untrusted co-resident, unlike the worker.)
- **`main` is still never touched**, the bot's Developer role is unchanged (it can
  already push non-protected branches, which is how the worker pushes today), and the
  end-of-run push + MR path is unchanged. This is not a privilege change.
- **The uid split (#51/#58) is no longer a dependency** but remains the primitive
  that would let M8 retire the broker and publish with a plain worker `git push`,
  safe by construction — #110's named revisit condition.
- **Fallback**: if a real-forge go-git send-pack proves fragile, Option A
  (worker reap-then-push) is the validated fallback, but only hardened with
  `hidepid=2` on the worker's `/proc` and a cgroup-based reap that catches the
  `setsid` escape.

## Alternatives considered

- **Worker-side reap-then-push (Decision 8 / Option A).** Rejected as primary: the
  reap is escapable (demonstrated), so it does not meet the invariant for N
  interleaved windows. Retained as the hardened fallback.
- **Infisical agent-vault (forward-MITM credential proxy).** Rejected: its own docs
  require the vault on a separate host/uid, which single-uid k8s cannot provide;
  it needs a network egress lock uzi lacks (the agent can `unset HTTPS_PROXY`; egress
  is CIDR-open, the tracked #50 residual); MITM needs the git client to trust a
  planted CA; git-push-over-HTTPS injection is undocumented; and it moves push
  capability *to* the agent (placeholder credential), weakening the SDK deny-`git
  push` guardrail. Strictly more attack surface than the api boundary uzi already
  has.
- **Short-lived / scoped push tokens** (GitLab project-access or job tokens).
  Rejected as infeasible under the deliberately-minimal Developer bot: minting
  project-access tokens needs Maintainer/Owner, PAT minimum expiry is 1 day (not
  per-checkpoint), and CI job tokens cannot `git push`. Elevating the bot to mint
  them is a net security loss.
- **Forge REST Commits API** (`POST …/repository/commits` with `actions[]`),
  server-side. Rejected: squashes each checkpoint into one synthetic commit (loses
  agent history), collides non-fast-forward with the end-of-run real `git push`, and
  needs a Forge-interface change across both drivers and five test fakes. The
  pack-shipping broker preserves real history and stays fast-forward-compatible with
  the final push.
