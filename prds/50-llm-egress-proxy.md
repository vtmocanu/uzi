# PRD #50: LLM egress proxy — per-run opaque credential, Anthropic token never leaves the api

**GitLab Issue**: [#50](https://gitlab.example.com/vtmocanu/uzi/-/issues/50)
**Status**: Draft — reviewed 2026-07-12 by 3 agents (design, security, fact-check); all blocking/major findings folded in below (marked ↳review where the design changed). Fact-check: 25/25 claims verified.
**Priority**: Medium (k8s/remote-worker phase; see #36 "Later")
**Created**: 2026-07-12
**Depends on**: PRD #32 (vault, done), PRD #4 (runs/claim/sweeper, done). Synergy: PRD #40 (token usage reporting — the proxy sees every usage block on the wire).
**Umbrella**: issue [#36](https://gitlab.example.com/vtmocanu/uzi/-/issues/36) (protect worker credentials in use on k8s), option B. This PRD is the only control in #36 that protects the Anthropic token; the uid-split pod (option A) covers join token + PAT but explicitly not this credential.

## Problem

The worker fundamentally needs the plaintext Anthropic OAuth token to run the Claude
Agent SDK. Today the api unseals it per claim (`api/internal/workersvc/service.go:444-468`,
vault-gated) and ships it in the claim response (`agent/src/protocol.ts:162,277`); the
worker holds it heap-only and scrubbed (`agent/src/runner.ts:119,125,279`) but must put
it in the SDK child's env as `CLAUDE_CODE_OAUTH_TOKEN` (`agent/src/sdk-env.ts:60`, wired
at `agent/src/sdk-executor.ts:217,254`; chat lane `agent/src/chat-executor.ts:320`).

`/proc/<sdk-pid>/environ` therefore exposes the **long-lived, account-wide** token for
the whole SDK session (minutes to hours), on every run and every chat turn, to anyone
with container access. On the compose laptop that is only the owner; on k8s,
`pods/exec` / `pods/attach` / `pods/ephemeralcontainers` make it a multi-actor concern.

No in-container measure fixes this (architect panel, 2026-07-12, on #36):

- The SDK has **no programmatic auth option** — `Options` in
  `@anthropic-ai/claude-agent-sdk/sdk.d.ts` (1278-2010) exposes only `env` (:1407);
  auth is env-driven.
- `apiKeyHelper` (`Settings`, sdk.d.ts:4641, reachable inline without weakening
  `settingSources: []`) still lands the plaintext in the CLI child's memory/env — no
  net gain against a same-uid exec attacker.
- Memory-only tricks (env scrubbing, secret files) are equivalent-or-worse: `environ`
  is the exec-time kernel snapshot, and a same-uid-readable secret file is exactly as
  reachable — `docs/proc-hardening.md:83-94` already makes this point for the join
  token.
- The uid-split pod (`docs/proc-hardening.md:123-166`) closes join-token + PAT vectors
  (:146-147), but the SDK runs in the *agent* container (:137-140), so exec into it
  still reads the token — option A buys zero here.

The structural fix: **never give the agent container the real token.**

## Solution Overview

The uzi **api terminates an LLM egress proxy**. The real Anthropic token never leaves
the api process; the agent's SDK child gets:

- `ANTHROPIC_BASE_URL=<api>/llm-proxy`
- `ANTHROPIC_AUTH_TOKEN=uzi-run-<opaque>` — a per-run credential minted at claim

The proxy authenticates the opaque bearer (sha256 lookup, same pattern as join tokens),
resolves it to its run and *that run's owner*, checks the run is live and within
budget, and forwards to `api.anthropic.com` injecting `Authorization: Bearer <real
token>` and merging `oauth-2025-04-20` into the `anthropic-beta` header, streaming SSE
straight through.

A stolen credential is then **scoped to one run, budget- and rate-capped, revocable,
and dies with the run** — the same trust model the worker/PAT split already uses. This
is the accepted floor: the credential must exist where the agent uses it; we shrink
what it is worth, not whether it exists.

```
agent (SDK child)                    api                          Anthropic
  ANTHROPIC_BASE_URL ────────▶ /llm-proxy/v1/messages ──────▶ api.anthropic.com
  ANTHROPIC_AUTH_TOKEN=          sha256(bearer) → credential      Authorization:
    uzi-run-<opaque>             → run → owner → real token         Bearer <real>
                                 run live? budget? rate?          anthropic-beta:
                                 SSE passthrough ◀───────────       <cli betas>,
                                                                    oauth-2025-04-20
```

## Design Decisions

1. **Gateway mode via `ANTHROPIC_AUTH_TOKEN`, not `CLAUDE_CODE_OAUTH_TOKEN`.**
   Verified against the bundled SDK 0.3.201: the Anthropic client reads `baseURL` from
   `ANTHROPIC_BASE_URL` (sdk.mjs client ctor) and all relative calls (`/v1/messages`,
   `count_tokens`) plus streaming follow it. Setting the opaque credential as
   `CLAUDE_CODE_OAUTH_TOKEN` would engage the CLI's subscription-OAuth machinery
   (dozens of `/api/oauth/*` paths: roles, profile, limits) against our proxy;
   `ANTHROPIC_AUTH_TOKEN` is the documented gateway pattern and skips them.
   `ANTHROPIC_AUTH_TOKEN` is pinned `undefined` today (type `sdk-env.ts:42-43`,
   runtime assignment :64-65) and sits in `PROTECTED_ENV_KEYS` (:27-32) — the pin
   becomes a deliberate set; override protection stays, and a test asserts it still
   holds once the value is real (↳review).

   **`ANTHROPIC_BASE_URL` is itself load-bearing and must be override-protected**
   (↳review): a provisioned toolEnv var (PRD #18) or future drift that overrode it
   would repoint LLM traffic off the proxy. It joins `PROTECTED_ENV_KEYS` and the
   `SdkEnv` interface. In proxy mode `CLAUDE_CODE_OAUTH_TOKEN` is **unset**, which
   means `buildSdkEnv`'s shape and the `if (!oauthToken) throw` precondition
   (`sdk-env.ts:39`, `sdk-executor.ts:188-193`, `chat-executor.ts:315-316`) are
   reworked in M3 — the env builder takes either a real token (legacy) or a
   proxy credential, never both.

2. **The api is the proxy — no sidecar, no LiteLLM.** Rejected alternatives:
   - *Sidecar in the worker pod*: buys nothing — `kubectl exec -c proxy` reads the real
     token just as easily; `pods/exec` is pod-scoped.
   - *LiteLLM gateway*: works, but adds a stateful service, a DB↔virtual-key sync, and
     a second all-users-token store (new blast radius) for features the api already has
     (vault unseal at claim `service.go:444-468`, bearer mint/hash, run lifecycle +
     sweeper for revocation).

3. **Token retention at forward time: a run-scoped plaintext cache in the api, and a
   locked vault does not kill a live run** (↳review — this was the review's central
   blocking gap, flagged independently by both reviewers, and the PRD's original
   "no regression versus today's claim-time unsealing" line was simply wrong).

   The proxy needs the plaintext on *every* forward, minutes-to-hours after claim.
   Today `openAnthropic` (`service.go:444-468`) unseals once at claim and the plaintext
   is GC'd. Two options:
   - *Re-unseal per request*: hits the PRD #32 vault gate on every LLM call. If the
     owner re-locks mid-run, every subsequent call 401s and the run dies — a **behavior
     regression**: today a locked vault blocks new claims (`errVaultLocked` →
     `service.go:434,458`), it does not kill in-flight runs. Also an unseal per call.
   - *Run-scoped cache (chosen)*: unseal once at claim, hold the plaintext in a
     run-keyed in-memory map for the run's life, evict on the run leaving its live
     states (and on api restart, by construction).

   Chosen: the **run-scoped cache**, with the cost stated honestly rather than
   hidden. It means the api holds, in RAM, the plaintext Anthropic token of every user
   with an *active run* — bounded by concurrent runs, not by user count, and evicted at
   run end. Vault-lock semantics are thereby **preserved exactly as today**: locking
   blocks the next claim, in-flight runs finish. This is a real widening of the api's
   in-memory secret footprint versus today's transient unseal, and it is the price of
   removing the token from the agent container; the api is already the sole custodian
   of every secret (ARCHITECTURE trust boundaries), so it is a *longer-lived* exposure
   in an existing custodian, not a new one. Eviction on terminal/rotation is a
   correctness requirement, tested in M1.

4. **Credential liveness is derived from `runs.status`, not from a revoke-hook set**
   (↳review — the review counted ~13 transition paths that would each need a hook;
   missing one leaves a credential outliving its run). The proxy admits a request iff,
   in one query: `sha256(bearer)` matches a `run_llm_credentials` row **AND** its run's
   status is in the live set **AND** the credential is within budget/rate caps.

   - Live set: `claimed`, `running`, `awaiting_approval`. **`awaiting_approval` must
     stay live** (↳review): after approval the *same* worker resumes the *same* SDK
     session with the *same* in-memory credential — there is no re-claim and therefore
     no re-mint, so a credential dead at the gate means every post-approval call 401s.
     The gate can legitimately hold for up to `WORKER_PLAN_APPROVAL_TIMEOUT` (24h,
     `agent/src/config.ts:201`), which is exempt from `RUN_TIMEOUT` (2h,
     `config.go:332`; `SweepRunningTimeout` fires only on `status='running'`) — so a
     fixed TTL borrowed from `RUN_TIMEOUT` would have broken every human-gated run
     longer than two hours. Budget and rate caps (Decision 6) bound the exposure of
     that window instead of a clock.
   - `UNIQUE(run_id)` with **mint-as-upsert: a re-claim rotates the credential**
     (↳review). Requeue paths set `status='queued'` (non-terminal:
     `RequeueRunsOfStaleWorkers`, `SweepClaimedNeverStarted`, `RequeueClaimedRunToQueued`),
     so "revoke on terminal" alone would leave the pre-requeue bearer valid across a
     resume. Rotation gives a one-live-credential-per-run invariant for free: the old
     bearer's hash no longer matches. Stated behavior change: a transient
     heartbeat-stall requeue now also kills a still-alive SDK child's LLM access
     mid-stream (it was already going to be requeued; it now fails faster and louder).
   - Chat lane has **no `RUN_TIMEOUT`** (`SweepRunningTimeout … kind <> 'chat'`) and is
     resumable via `Continue`, so its liveness derives from the same status join plus
     its own terminal paths — `EndChat` (`handler.go:452`), `SweepIdleChatRuns`
     (`service.go:1438`), and `Continue` (mints a new run → new credential)
     (↳review). No borrowed clock.
   - An **absolute TTL backstop** still exists (`expires_at`, spanning
     `RUN_TIMEOUT + WORKER_PLAN_APPROVAL_TIMEOUT` with slack) purely so a row that
     somehow escapes the status join cannot live forever. It is a backstop, not the
     mechanism.
   - Table `run_llm_credentials` (draft migration `00057` — next free above live head
     `00056_oidc.sql`; renumbered at merge per convention): `run_id` (FK
     `ON DELETE CASCADE`), `token_sha256`, `expires_at`, budget/rate counters. The
     *migration* always applies; only the *behavior* is flag-gated (Decision 7)
     (↳review). Minted in `assembleClaim` (`service.go:471`) for both lanes; the
     plaintext is shown to the worker once in the claim response, exactly like join
     tokens.

5. **Proxy handler: strict ingress, streaming egress.** (↳review — the original
   "allowlist + 404 everything else" was under-specified enough to be an SSRF hole.)
   - **Per-request owner derivation is the isolation invariant**: bearer → credential
     row → `run_id` → `runs.user_id` → `openAnthropic(that user)`. The injected upstream
     credential is derived from the authenticated bearer's run-owner on **every**
     request, never from any handler- or package-level "current token" (the handler is
     concurrent across many runs and users). M2 carries an explicit cross-user
     isolation test: run A's bearer must never cause user B's token to be used.
   - **Path allowlist is exact-match, canonicalized, method-bound**: decode and
     `path.Clean` first, then exact-match `/v1/messages` and `/v1/messages/count_tokens`;
     **POST only**; everything else 404s. A prefix match or an un-normalized `..%2f`
     would ride the injected account-wide OAuth token to Anthropic's account-management
     surfaces. `httputil.ReverseProxy` forwards any method and any path by default —
     this must be an explicit gate in front of it, not a `Director` tweak.
   - **Authenticate before routing** so the response cannot become a credential oracle:
     bad credential → 401 regardless of path (generic, constant-time, matching
     `RequireWorker`'s pattern, `worker_auth.go:46-59`); valid credential + bad path →
     404.
   - **Inbound header scrub**: the SDK child is the client, and a prompt-injected or
     exec-present attacker controls its headers. Delete every client-supplied
     `Authorization`, `x-api-key`, `anthropic-beta`, `anthropic-version`,
     `anthropic-dangerous-*`, and all hop-by-hop/`Connection`/`Upgrade` headers (deny
     protocol upgrade outright); cap header size. Then inject ours.
   - **`anthropic-beta` is merged, not overwritten** (↳review): the CLI sends its own
     betas (prompt caching, token-efficient tools, 1M context, fine-grained streaming);
     replacing the header with a bare `oauth-2025-04-20` would silently strip them and
     degrade or break runs. Append to the CLI's list — from *our* allowlist of known
     beta values, since the header is client-controlled (see scrub above).
   - **Streaming**: `httputil.ReverseProxy` with `FlushInterval: -1` (SSE flushes
     immediately, buffers nothing), plus a per-request body size cap on the way in.
   - **Logging rules are part of the design, not an afterthought** (↳review): the proxy
     is the one component that sees full prompts and responses. It must **never** log
     request/response bodies (source code, untrusted issue/CI content, anything the
     agent surfaced), **never** log request or response headers (the injected real token,
     the inbound bearer), and never log the bearer itself. The 404 log line carries
     method + canonicalized path only, sanitized against CRLF log-forging.

6. **Spend is bounded before the plaintext token leaves the wire** (↳review — the
   original ordering removed the plaintext at cutover while the only v1 controls were a
   TTL and a request *count* cap, which does not bound cost: one `/v1/messages` can ask
   for a 200k-token context and max output).
   - M2 ships, at the same time as the handler: per-request **body size cap**, and
     per-credential **rate limit** (keyed on the credential/run — **not** the IP: the
     agent reaches the api directly at `http://api:8080`, `docker-compose.yml:210`,
     bypassing nginx, so an IP-keyed limiter is meaningless).
   - **Token budgeting lands before the cutover** (milestones reordered): the proxy
     parses `usage` from the SSE `message_delta` frames it already relays and enforces a
     per-run token budget. Constraint to honor: the tee that extracts usage counters
     must never stall the `FlushInterval: -1` passthrough, and must extract counters
     only — never buffer or log content. This is the same wire data PRD #40 wants, so
     the parse lands once and serves both.

7. **Flag-gated cutover; Phase 1 must already withhold the real token from the child
   env** (↳review — otherwise Phase 1 closes nothing).
   - *Phase 1*: the claim carries both the legacy plaintext token and the new
     base-URL/credential fields; with the flag on, the worker uses the proxy credential
     and **does not put the real token in the SDK child's env** (it stays in worker
     heap, redacted, unused). If both were set, the real token would still sit in
     `/proc/<sdk-pid>/environ` — the exact leak being closed — and the environ closure
     would not actually land until Phase 2.
   - *Phase 2*: the api stops sending the plaintext token in the claim at all.
   - *Phase 3* (k8s): NetworkPolicy makes the proxy mandatory rather than advisory.
   - The opaque bearer joins the agent redact set (`runner.ts:119,125`) so it is
     scrubbed from logs like every other secret.

8. **Credential namespaces stay disjoint** (↳review). The LLM bearer (`uzi-run-`,
   `run_llm_credentials`) and the worker join token (`uzw_`, `workers.token_hash`,
   `jointoken.go:25`) are separate stores, separate middleware, distinct prefixes:
   `RequireWorker` (`worker_auth.go:36`) must never accept an LLM bearer, and the proxy
   must never accept a join token. Without this, the SDK child — which now knows the
   api's URL by construction — could turn its bearer against `/api/worker/*`.

9. **Kill non-essential egress, and get the NetworkPolicies right** (↳review).
   `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` in the SDK env (telemetry/statsig/sentry
   ignore `ANTHROPIC_BASE_URL`). On k8s: the **agent** egress allowlist is api + forge +
   **DNS + the nix substituters** (`cache.nixos.org` and configured extras) — omitting
   the substituters would break PRD #18 tool provisioning, which is what makes a naive
   "api + forge only" policy a self-inflicted outage. The **api** gains a new outbound
   trust boundary of its own (`api.anthropic.com`) — today its only outbound is the
   forge — so the api's own NetworkPolicy and ARCHITECTURE's trust-boundary section
   both need it.

## Residual exposure & honest cons

- An attacker exec'ing mid-run reads the **opaque bearer** and can burn tokens through
  the proxy until the run leaves its live states, or the budget/rate cap trips.
  Accepted floor: the credential must exist where the agent uses it. What it is *not*:
  account-wide, long-lived, reusable after the run, or usable against any endpoint
  beyond `/v1/messages`.
- **The api holds every active run's plaintext token in RAM for the run's duration**
  (Decision 3) — a longer-lived in-memory secret footprint than today's transient
  claim-time unseal. This is the deliberate trade: concentrate the secret in the
  component that is already its sole custodian, so it is absent from the component an
  attacker can reach.
- The api becomes the LLM availability chokepoint: an api restart drops streams
  mid-turn (SDK retries) and clears the token cache (in-flight runs must re-claim).
  Negligible at current scale (a handful of concurrent runs); the handler moves
  unchanged to a dedicated deployment if that ever changes.
- Every forward costs a Postgres lookup (sha256 + status join). Fine at current scale;
  a future cache point.
- The path allowlist is a maintenance point as the CLI grows; 404s are logged
  server-side so the growth is visible rather than silent.
- OAuth refresh is a non-issue: uzi stores long-lived setup-token-style OAuth tokens;
  the refresh flow (`platform.claude.com/v1/oauth/token`) only applies to interactive
  `.credentials.json` logins. Nothing to intercept.

## Milestones

- [ ] **M1 — Credential store + liveness/rotation lifecycle.** `run_llm_credentials`
  migration (draft `00057`, FK `ON DELETE CASCADE`, `UNIQUE(run_id)`), mint-as-upsert in
  `assembleClaim` for run + chat lanes, run-scoped plaintext token cache with eviction.
  Tests: rotation on re-claim invalidates the prior bearer; liveness follows
  `runs.status` (live through `awaiting_approval`, dead on every terminal and requeue
  path, chat terminals included); cache evicts on run exit.
- [ ] **M2 — `/llm-proxy` handler in the api.** Auth-before-routing by hash;
  per-request bearer→run→owner token derivation; exact-match canonicalized POST-only
  allowlist; inbound header scrub + upgrade denial; `anthropic-beta` merge; body-size
  cap + per-credential rate limit; SSE streaming passthrough; the logging rules.
  Tests against a stub upstream: streaming intact, disallowed paths/methods 404,
  revoked/rotated/expired credentials 401, **run A's bearer cannot reach user B's
  token**, real token never echoed or logged, traversal payloads rejected.
- [ ] **M3 — Agent wiring behind a flag.** `buildSdkEnv` reshaped (proxy credential or
  legacy token, never both; `ANTHROPIC_BASE_URL` added to `SdkEnv` +
  `PROTECTED_ENV_KEYS`; `CLAUDE_CODE_OAUTH_TOKEN` unset in proxy mode; the
  `if (!oauthToken) throw` precondition reworked), both executors,
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, bearer in the redact set. Tests:
  override-protection still holds with a real value; with the flag on, the real token
  appears in no child env.
- [ ] **M4 — Token budget enforcement.** Parse `usage` from relayed `message_delta`
  frames without stalling the flush; enforce the per-run token budget; expose the
  counters (hand-off point to PRD #40). Lands **before** cutover so the plaintext token
  is never traded for an unbounded-spend credential.
- [ ] **M5 — Cutover: plaintext token leaves the wire.** Claim payload stops carrying
  `anthropic_oauth_token`. Positive proof (full run + chat turn through the proxy,
  streaming intact) is the M2 stub-upstream Go test — **not** e2e, which runs
  `UZI_EXECUTOR=stub` with no real SDK child and therefore cannot exercise the
  forward path (↳review). e2e's contribution is the negative assertion: no real-token
  shape in any agent-container process environ, mirroring the join-token check at
  `e2e/run-e2e.sh:712-721`.
- [ ] **M6 — k8s policy + docs + specs.** Agent NetworkPolicy (api + forge + DNS + nix
  substituters) and the new api→`api.anthropic.com` egress; `docs/proc-hardening.md`
  gains the proxy as the Anthropic-token close; ARCHITECTURE.md trust-boundary +
  run-lifecycle sections; `specs/ai.md` decision record; issue #36 updated (option B →
  delivered).

## Success criteria

- No plaintext Anthropic token in any env, file, or claim payload reachable from the
  agent/worker container (e2e-asserted).
- A leaked per-run credential is unusable once its run leaves the live states or is
  re-claimed (rotation), cannot exceed its token/rate/body caps while live, and cannot
  reach any Anthropic endpoint outside the two allowlisted paths or any other user's
  token.
- Runs and chats stream through the proxy with no user-visible latency change, and a
  human-gated run resumes correctly after an approval wait longer than `RUN_TIMEOUT`.

## References

- Issue #36 (umbrella) + architect panel and review-panel findings, 2026-07-12
- `docs/proc-hardening.md:83-94,123-166` (join-token residual, uid-split pod design)
- `api/internal/workersvc/service.go:444-468,471`, `claim.go:114`, `chat.go:111-116`
- `agent/src/sdk-env.ts:27-32,39,42-43,60,64-65`, `sdk-executor.ts:188-193,217,254`,
  `chat-executor.ts:315-316,320`, `runner.ts:119,125,279`, `protocol.ts:162,277`
- `api/internal/middleware/worker_auth.go:36,46-59`, `jointoken.go:25`
- specs/ai.md PRD #25/#32 (secrets-at-rest) — this is the secrets-in-use counterpart
