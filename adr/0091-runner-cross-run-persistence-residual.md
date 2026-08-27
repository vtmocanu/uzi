# ADR-91: Runner cross-run (and cross-repo) executable persistence is an accepted residual until ephemeral workers land

**Status**: Accepted (records the accepted residual and its bound). PRD #529's exit condition (ephemeral-per-run workers) has now shipped M1–M8 on this branch, structurally closing this residual **for ephemeral usage**; it remains the accepted (bounded) residual for the standing shared-per-user worker path (ephemeral off), and for the #79 shared-nix-store optimization caveat below. This ADR and #529 record that closure; forge issue #91 is to be closed citing this ADR at merge.
**Date**: 2026-08-22
**Deciders**: Vlad + agent team
**Origin**: surfaced during the PRD #90 (agent memory) review as a separate pre-existing residual, filed as [#91](https://github.com/vtmocanu/uzi/issues/91) and verified in code by re-derivation. This ADR is written in place of keeping #91 open: the decision is made, and the live follow-up it would remind us of is already owned by #529.

## Decision (summary)

A hosted worker is **per-user, long-lived, and spans repos**. Two runner-writable paths persist for the worker's whole lifetime rather than being reset per run:

- `/nix` (on `UZI_RUNNER_PATH`, runner-group-writable), and
- `/data`'s shared provisioning state — the nix/devbox provisioning HOME and root are **shared worker-lifetime** by design (`agent/src/main.ts`, Decision 5: `provisionHomeDir: sdkHomeRoot`). Only `agent-home/<runId>` is torn down per run.

The `runner` uid is the untrusted execution surface (it runs the run's prompt-injectable repo content). So a prior run's runner can plant **executable** state (a config the provisioning subprocess sources, devbox/nix profile state) that a **later** run's runner on the same worker executes. Because one worker spans repos, this is **cross-repo within one user**: a run on hostile repo A can persist state that tampers with the same user's later run on trusted repo B.

**We accept this residual for now, with the bound below, and close it structurally when ephemeral-per-run workers (#529) ship** — a fresh worker per run has its own fresh `/nix` and provisioning HOME, so there is no shared state to plant into. Paying a per-run cold-start tax to reset that state on the current shared worker (the alternative, "per-run provisioning isolation") would degrade every run's latency, which is exactly the warmth Decision 5 chose to keep, to close a bounded and rarely-triggered gap. That trade is not worth it ahead of the ephemeral fix.

## Context

The class of "a shared writable path is planted by one uid and executed by another" is one this design is already aware of and already mitigates for the surface that matters most — the **credential-holding worker**. The entrypoint is explicit that a persisted `/nix` could carry a runner-planted trojan (`agent/templates/entrypoint.sh`, PRD #51 M4): root resolves binaries by absolute image-baked path (never from `/nix`/`/data`), and the worker is handed a **separate** stripped PATH while the full image PATH goes to the runner via `UZI_RUNNER_PATH` at the drop. So a planted trojan **cannot reach the worker or the PAT/join token**.

The gap #91 records is the part that mitigation does **not** cover: runner-context integrity **across a user's runs on one worker**. There is no isolation, and until this ADR no explicit record, for it.

## The decision

### Accept the residual, bounded and stated honestly
- **Not** a secret-exfil or worker-escalation path — the worker is isolated from `/nix`/`/data` (above), and egress is NetworkPolicy-fenced ([ADR-285](0285-worker-egress-tier-trust-model.md)).
- **Not** a cross-user path — workers are per-user.
- It **is** a cross-repo **output-integrity** risk on a shared-per-user worker: hostile repo A poisoning the user's trusted repo B run's output/MR. Distinct from PRD #90, which persists **inert data** read into the lead; this is **executable** persistence in the runner context.
- **Low** for single-repo or ephemeral-worker usage, and low in absolute terms for the mostly-single-user private-factory model this product targets: the trigger requires a user to run both a hostile repo and a trusted repo on the same worker.

### The exit condition is ephemeral-per-run workers, not per-run provisioning reset
The clean fix is #529's ephemeral workers: a run-bound worker that boots, serves one run, and is torn down. #529 has now shipped: each ephemeral worker (unique id) gets its own `-nix` and `-data` PVCs, keyed by worker id (Decision 7; `controller/internal/kube/render.go` `RenderPVCs`/`dataPVCName`/`nixPVCName`), so the shared-path vector **does not exist** for a run-bound worker — there is no shared `/nix` or provisioning HOME left for a prior run's runner to have planted into. This is now asserted by a controller test verifying the per-worker-PVC property. **Caveat for the #79 cost path:** if a later optimization shares one nix store across ephemeral workers to cut cold-start (the tradeoff issue #79 weighs), the `/nix` half of this vector partially returns — content-addressed `/nix/store` paths are hash-immutable (planting one needs a hash preimage), but the mutable profile/gcroots would again be shared. So a shared-nix-store variant must re-check this residual rather than assume ephemerality closed it. The middle option (fresh provision HOME + fresh nix profile per run on the shared worker) is explicitly **not** chosen: it pays Decision 5's cold-start cost on every run and was superseded by #529.

## What this ADR does NOT decide
- Whether to build ephemeral workers, or when — that is [PRD #529](https://github.com/vtmocanu/uzi/blob/main/prds/done/529-ephemeral-workers.md) (shipped M1–M8, opt-in and off by default) and the cost tradeoff in [issue #79](https://github.com/vtmocanu/uzi/issues/79).
- The worker's egress trust boundary — that is [ADR-285](0285-worker-egress-tier-trust-model.md).
- The worker-side write-then-execute hardening (git config-key pins, absolute-path binary resolution) — shipped, PRD #51; this ADR only records the residual that hardening deliberately leaves on the runner side.

## Consequences
- The residual is now recorded with its bound, so a future reader can tell "known and accepted" from "missed."
- #529 has shipped M1–M8; its M8 verified and (via a controller test) asserted the per-worker-PVC property that closes this residual for ephemeral usage. Forge issue #91 is to be closed citing this ADR at merge; this ADR remains its durable record.
- If usage shifts toward users routinely mixing hostile and trusted repos on one worker before #529 lands, revisit: the cheaper-than-ephemeral hardening would be to scope workers per-repo (they are per-`user_id` today, spanning repos — `api/internal/workersvc`), which removes the cross-repo case while keeping warm-start.
