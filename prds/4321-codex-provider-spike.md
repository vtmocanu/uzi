# PRD #4321: Characterize custom Responses providers on the production Codex path

**Issue**: [#4321](https://github.com/vtmocanu/uzi/issues/4321)
**Priority**: High
**Umbrella**: [#4319](https://github.com/vtmocanu/uzi/issues/4319)
**Dependencies**: None. Independent of PRD #4320; evidence gates PRD #4323.
**Disposition**: Commit and push for later; no dispatch or sweep enrollment.

## Problem

A user cannot select a custom Responses endpoint on the Codex engine. Configuration text already exists for test providers, but it does not prove that a custom endpoint works with production authentication, code-host callbacks, isolation and recovery. Shipping that assumption risks sending a credential or repository content to an unintended destination, or enabling a harness that cannot complete its assigned work.

Current-main facts (baseline 3c4a5ed7b363535bb91fbeb2a69b7c115eb69436; re-read at implementation, including later main a233883de):

- `agent/codex/codex-package.lock:21` pins 0.166.0 and `:24` pins upstream commit 6ff1e1ca3b30fc8b914720681fcf5fe62e95f856. Read the lock at execution time and record the actual pin; no runtime upgrade is authorized.
- `agent/src/codex/config.ts:134` emits a custom Responses-provider table using `base_url` and `env_key`, with OpenAI auth disabled, websockets disabled and zero retries. This is not the production configuration.
- `agent/src/codex/config.ts:158` and `:198` restrict production configuration to the built-in OpenAI provider. `agent/src/codex/appserver-auth.ts:342` supplies API-key or subscription auth through the login RPC. `agent/src/codex/launcher.ts:779` replaces the child environment and excludes provider credential env injection on managed-auth launches.
- A separate loopback test config supports custom provider identity plus login-RPC auth (`agent/src/codex/config.ts:217`). Its existence is not proof that every gateway auth style works.
- ADR-1106's Execution policy (`adr/1106-codex-harness.md:353`) requires native effectful execution/extensions disabled and worker-owned callbacks to remain the authority boundary. The generic test builder alone does not establish the production code-host path.
- The phase-one PRD already identifies custom Responses providers as a future auth-profile extension (`prds/done/1106-codex-harness-phase1.md:36`). Closed prior-work searches identified #1220 and runtime updates #3697/#4031, not a shipped custom-provider feature.

## Outcome

A repeatable, credential-free, offline characterization establishes which custom-provider configurations preserve the production Codex contract. Its checked-in matrix and Decision Log give PRD #4323 concrete supported configurations, failure behavior and acceptance requirements. A negative or unsupported result is useful evidence, not a reason to weaken the boundary.

Acceptance examples:

1. With the pinned Linux runtime and a local fake Responses server, the harness selects a distinct custom provider, sends a runtime-generated dummy credential only to that fake, receives a streamed response and records which auth/config path actually worked.
2. A fake response requests a worker-owned callback. The production code-host path admits the expected role/phase operation, refuses an unauthorized operation, and completes or cancels within its deadline with no native execution escape.
3. An unsupported auth style, redirect, auxiliary request or resume behavior is reported as unsupported or blocked with reproducible evidence. No live account, external endpoint or open-web lookup is needed to finish the spike.

## Out of scope

Production provider configuration, API/schema/UI/CLI changes, real gateway certification, real credentials, subscription forwarding to third parties, Chat-Completions transport, model mixing, runtime upgrades, new privileges and workflow changes. No files under `.github/workflows/` may be created or edited, including during validation.

## Modules and seams

- **Offline characterization runner.** Reuse the pinned-runtime fixture conventions in `e2e/codex-m4/` and the launcher/config/auth seams in `agent/src/codex/`. Exercise the production managed-auth and code-host composition, using a narrowly injected loopback provider only for the fixture. A test seam cannot become a production endpoint override. Generate credentials at runtime; never embed a complete provider-token literal.
- **Fake Responses server and destination observer.** Bind only 127.0.0.1 on a dynamically allocated port. Use synthetic prompts and fixed, finite response scripts. Limit each request body to 1 MiB, each scripted stream to 1 MiB, active connections to four, each case to 30 seconds and the complete characterization to five minutes. Abort and close owned children/sockets on timeout. A separate owned loopback receiver detects redirect credential delivery; it must receive none. External connectivity is not a prerequisite: deterministic request interception/config assertions must expose auxiliary destination attempts, and any unobservable destination behavior is reported unverified rather than inferred from fake-server silence. Never inspect unrelated host processes.
- **Auth/transport matrix.** Record configuration, pin, launch path, emitted request path/header names, streaming and callback result, cancellation, resume, auxiliary destinations, and secret-handling result. Compare env-key bearer auth and managed login-RPC auth; test alternate header styles only when the installed runtime accepts their configuration. Do not assume a schema key is supported because an old builder mentions it. No arbitrary user-shaped header dictionary is proposed. Raw secrets, request headers and full transcripts are not persisted; evidence stores boolean checks, safe names and bounded failure codes, at most 64 KiB per case and 1 MiB total.
- **Safety contract.** Existing synthetic-provider fixtures accept controlled local data. That does not justify production user-supplied destinations. This spike retains native suppression, explicit untrusted project state, environment replacement, run/role/phase callback admission and owned teardown. It separately measures credential placement because the current production login-RPC path deliberately keeps credentials out of the environment. If a candidate requires a broader secret surface, report the cost and hold that candidate for a design decision; do not silently adopt it.
- **Private-CA trust.** Identify which mechanism makes the pinned runtime trust an extra CA bundle for a custom provider (for example `SSL_CERT_FILE` or a Codex-specific variable), using fixture CAs generated at test time for the loopback fake (one trusted, one untrusted), with TLS verification always on, and whether the worker's sparse launcher environment can carry it. Record how a certificate failure and a stalled connection surface at the pin. Observation to re-check, not a guarantee: in a manual check on 2026-11-14 (codex-cli 0.167.2 via `codex exec`, not the pinned app-server path) against a private-CA Responses server, the CLI kept printing "Reconnecting... waiting for network" with no certificate error until the experiment stopped it, and worked once an extra CA bundle was provided (both `SSL_CERT_FILE` and `CODEX_CA_CERTIFICATE` were set, so which one mattered is unknown). Also record the runtime's context-window config key and its fallback-metadata warning for unknown model names.
- **Evidence document.** Record the matrix and PRD #4323 requirements in this PRD's Decision Log, promoting a durable decision into an ADR note if needed. New documentation paths are chosen at implementation time, without dangling path references in this draft.

## Testing decisions

Reuse `agent/test/codex-config.test.ts`, `agent/test/codex-launcher.test.ts`, the app-server auth tests and `e2e/codex-m4/` fixtures. Test real pinned-runtime behavior for provider/auth/stream/callback claims; mocks establish only fixture plumbing. Include discriminating negative controls: wrong credential, unsupported wire, redirect receiver, unauthorized callback, cancellation during a stream, and resume after changing the fake endpoint binding. A control must visibly change its expected result when its relevant guard is removed. Missing runtime or unavailable observation is a loud incomplete result, never a passing skip.

In-run: `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent`, touched/directly affected tests, `task test:codex-m4` for runtime-facing changes, and `task scan:secrets` with the canary evidence. Every bug fixed during implementation gets its own watched red/green regression. Use event-driven readiness and injected test timers; record before/after runtime of affected steps if added cases measurably grow them. No open-web lookup or package download at test time is required: run against the worker's already installed matching pinned runtime; an absent or mismatched runtime is an explicit prerequisite failure.

Landing obligations, outside milestones: **full agent suite: exact-head PR CI**; retain existing worker-UID and runtime conformance checks. Report focused checks separately from full-suite status.

## Milestones

- [ ] **M1: Observe a bounded custom-provider exchange through production authentication.**
  **Blocked by**: None.
  **Acceptance**: An offline runner verifies and records its runtime pin; drives the managed-auth production launch with a distinct loopback custom provider; proves a streamed response and expected credential recipient; and produces explicit supported/unsupported outcomes for the auth candidates. Wrong-key and redirect controls discriminate the result. No production routing behavior changes. Include its repeatable invocation and evidence schema in this PRD.
- [ ] **M2: Establish the production execution/recovery contract and publish PRD #4323 requirements.**
  **Blocked by**: M1.
  **Acceptance**: The same runner tests code-host callbacks with role/phase denial, native suppression, advice behavior, cancellation, owned teardown, session resume and auxiliary destination attempts. The matrix distinguishes observed support, observed failure and unverified cases. The Decision Log names the minimum supported auth style, transport, required metadata/capabilities, all remaining limitations and concrete PRD #4323 regression cases. If no candidate preserves the safety contract, mark PRD #4323 blocked with evidence rather than enabling an unsafe path.

## Decision Log

- **Independent evidence slice.** Reason: a protocol/auth matrix is valuable before the provider core exists. Rejected: coupling this investigation to PRD #4320 or shipping untested config as part of PRD #4323.
- **Exercise production auth/execution, not only a generic provider table.** Reason: production uses login RPCs and controlled code-host callbacks, while the generic builder does not prove that composition. Rejected: a text snapshot or a successful raw HTTP request as compatibility evidence.
- **Offline, pinned-runtime fake-server experiment.** Reason: the worker can reproduce it without live secrets or gateway availability. Rejected: online documentation research left to the worker, a runtime bump, or live contributor infrastructure.
- **Unsupported is a legitimate result.** Reason: endpoint compatibility must not weaken isolation or move credentials into an unreviewed surface. Rejected: making the experiment pass by enabling native tools, inheriting config/env, disabling checks or silently switching to a vendor credential.
- **Evidence to fill during implementation.** The matrix, measurements and resulting PRD #4323 contract belong here; this draft does not claim those experiments have run.
