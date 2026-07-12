# PRD #39: In-App uzi Chat Agent — chat with uzi about itself, its runs, and its work

**GitLab Issue**: [vtmocanu/uzi#39](https://gitlab.example.com/vtmocanu/uzi/-/issues/39)
**Status**: Draft — reviewed 2026-07-10 by 3 agents (design, security, fact-check); all blocking/major findings folded in below (marked ↳review where the design changed).
**Priority**: Medium
**Created**: 2026-07-10
**Depends on**: PRD #3 (per-user Anthropic token, done), PRD #4 (worker runtime + run machinery, done), PRD #6 (`ci_fix` — the second-run-kind precedent and the untrusted-evidence framing, done), PRD #19 (autopilot — `Forge.CreateIssue` already exists, `forge.go:225`, done), PRD #32 (vault — claim gating covers chat automatically, §139, done).
**Related decision**: specs/ai.md §168 — worker availability (always-on on compose, operator-spawned pods on k8s). §168 already names chat as the motivating case; chat must work identically under both shapes.

## Problem

uzi has no conversational surface. A user who wants to know what uzi can do, how a feature is implemented, or why run #57 failed has to read the docs, read the source, or eyeball the activity feed themselves. Turning a half-formed idea into a `PRD`-labeled GitLab issue means leaving uzi for GitLab. The platform runs agents all day but you cannot talk to it.

## Solution Overview

A **Chat** page in the web UI where the user converses with an agent that:

1. runs on the user's **existing worker** as a third run kind (`chat`) — billed to the user's own Anthropic token, streamed through the existing `run_messages` → `/api/ws` pipeline; the user never knows a worker is involved (except an honest "worker offline" state when none is connected);
2. knows **uzi's own source**, baked read-only into the worker image at build time (no clone, no PAT, version-matched to the deployed build);
3. can **investigate the user's runs** via read-only, API-mediated tools (list runs, run detail, message history) — "why did this fail?" answered with evidence;
4. can **create GitLab issues** through a two-step propose → user-confirms-in-UI → forge-first flow using the user's existing bot connection — the agent proposes, the human clicks, the API writes;
5. changes **no guardrail**: `settingSources: []`, deny-hooks, no forge PAT anywhere near the agent (chat claims omit it entirely), primary directive untouched.

## Design Decisions

1. **Chat rides the run machinery as `runs.kind = 'chat'` — but honestly costed (↳review, design verdict).** What is genuinely reused with zero or trivial change: per-claim delivery of the decrypted Anthropic token, vault claim-gating (§139 is a single user-scoped gate before `ClaimRun` — covers chat automatically), persisted-first gapless `run_messages` + `/api/ws` with owner-or-admin subscribe authz and REST replay (`?after=<seq>`), the steering input wire, heartbeat liveness, per-user rate-limit middleware, session-id persistence for resume (§51). What chat *inverts* from the `ci_fix` precedent and therefore must build (each covered by a decision below): nullable `repo_id` + a forked repo-less claim assembly (Decision 12), a claim-lane split and a second concurrent executor (Decision 4), a `ChatRunner` instead of `RunRunner` (Decision 13), sweeper carve-outs (Decision 3). A dedicated chat service was weighed against this now-visible cost and still loses: it would rebuild token custody, message persistence/replay, liveness, vault gating, and authz from scratch, and would add a fourth service + trust boundary. Riding runs is also the k8s-portability guarantee (§168): chat targets the deployment-agnostic claim/poll protocol, so compose (always-on worker) and k8s (operator-spawned pods) serve it identically.
2. **One chat = one run; turns are `follow_up` inputs on the existing wire.** `POST /api/chats` creates a queued `chat` run whose first message is the initial prompt; subsequent user messages ride `run_user_inputs` (`kind = 'follow_up'` is already in its CHECK, `00020_workers_runs.sql:83-90`; ↳review facts: table is `run_user_inputs`, not "run_inputs") through `workersvc.SubmitInput`'s ownership check and `steering.ts` delivery. New worker-side primitive required (↳review D9): `steering.ts` today offers only a non-blocking `pullFollowUp` — chat's park-between-turns loop needs an await-next-follow-up wakeup; named here so it isn't assumed free. Latency floor stated honestly (↳review D8): input pickup is bounded by the worker's poll interval; chat polls faster than runs (`WORKER_CHAT_POLL_MS`, default 1000ms) so a turn starts ≤1s after send, plus model latency. WS wakeup for workers stays out of scope (ARCHITECTURE "Not yet in scope").
3. **Chat gets its own lifecycle clocks — idle-bounded, turn-capped, AND per-turn wall-clocked (↳review D5).** The sweeper's 2h `RUN_TIMEOUT` fail is skipped for `chat` kind. In its place, three independent bounds: (a) **idle**: the worker completes a chat run after `WORKER_CHAT_IDLE_TIMEOUT` (default 60m) with no user message, with a server-side idle sweep (last-message age > `CHAT_IDLE_TIMEOUT`, default 70m) as the not-trusting-the-worker backstop — both raised from an earlier 15/20m draft because idle-death discards the conversation (see Decision 11); (b) **turns**: `CHAT_MAX_TURNS` (default 50) — enforced worker-side per turn AND server-side (counted from persisted user inputs) so a compromised worker can't burn spend past it (↳review S7); (c) **per-turn wall-clock**: `WORKER_CHAT_TURN_TIMEOUT` (default 10m) aborts a single runaway turn — necessary because the idle timer re-arms on every SDK message (`sdk-executor.ts:389`), so a busy tool loop inside one turn never goes idle and the turn counter never increments mid-turn; for issue runs the 2h wall-clock is exactly this backstop, and chat must not drop it without a replacement. An explicit "End chat" in the UI cancels cleanly. `RUN_MAX_ITERATIONS` doesn't apply (no implement loop). Honest note (↳review D14): `RUN_MAX_REQUEUES` (default 1) means one worker crash mid-conversation likely ends that chat run; the Continue affordance (Decision 11) is the recovery path.
4. **A second claim lane — a real refactor, budgeted as such (↳review D3).** `ClaimRun` today claims the oldest queued run of ANY kind with no kind predicate (`runtime.sql:154-170`), and the worker runs ONE claim loop that awaits terminal state before the next claim (`worker.ts:58-73`). Chat requires: (a) a kind split of the claim query — the run lane excludes `chat`, a new chat lane selects only `chat` (parameterized or two queries; `claim_wire_contract_test` updated); (b) a second, independent claim loop in `worker.ts`, i.e. the worker executes a run and up to `WORKER_CHAT_SESSIONS` (default 1) chat sessions **concurrently — a property it has never had**. Concurrency audit is explicit M2 work: per-instance executor state is safe (`spawnedPids` is instance-scoped) but shared collaborators are not free — `Logger.addSecret` mutates shared redactor state, and `WorkerClient` is shared; both get thread-safety review + tests. `WORKER_CHAT_SESSIONS=1` also means one live conversation per user-worker; a second chat queues until the first ends (↳review D10 — acceptable MVP, stated). Affinity grace applies as for runs, so a re-queued chat prefers the worker holding its SDK session.
5. **uzi source is baked into the worker image at build time — never cloned — via a dedicated build-context change (↳review S3/D2/F8: the original premise here was wrong).** The agent image builds from `context: ./agent` (`docker-compose.yml:156`), NOT the repo root — only `web` builds from the root, and the root `.dockerignore` is web-tuned: it deliberately EXCLUDES `api/`, `agent/`, `e2e/` (exactly the source chat must know) while being the only place `.env*` is excluded (`agent/.dockerignore` doesn't list `.env*` at all). The fix is explicit scope: switch the agent service's build context to the repo root and give the agent Dockerfile its **own BuildKit per-Dockerfile ignore file** (`Dockerfile.dockerignore` next to each template Dockerfile — one ignore per context otherwise) that includes `api/ web/ agent/ docs/ specs/ prds/ *.md` source and hard-excludes `.env`, `.env.*`, `**/.env*`, `.git`, `inspiration/`, `node_modules`, `dist`. (`pgdata` needs no exclusion — it is a named volume, not a tree path; the original claim was wrong.) `.git` is already excluded from both existing contexts, so no committed-history secret leak either way. A `BUILD_INFO` file (git SHA, build date) is stamped beside the source and named in the system prompt. This still beats a clone on every axis — the snapshot matches the *deployed* version exactly, needs no PAT and no network — and the §54 docs-bundling precedent applies with the honest caveat that §54 widened the *web* context and this PRD re-does that widening for the agent image. M2 carries these as named line items (↳review D13).
6. **Chat's tool surface is a real deny-by-default via the SDK `tools` option — not `allowedTools` (↳review S1: the original mechanism was a no-op).** `allowedTools` is only the auto-approve list and confines nothing under `bypassPermissions` (`sdk.d.ts:1324-1331`: "To restrict which tools are available, use the `tools` option instead"). Chat therefore sets **`options.tools: ['Read','Grep','Glob', <uzi MCP tool names>]`** — the base-set restriction that holds in any permission mode — with `disallowedTools` (async-deferral + `Agent`) and `settingSources: []` retained verbatim. No Bash, no Write/Edit, no WebFetch/WebSearch, no subagents. **And the path-guard hook is wired, which is what makes the confinement true (↳review S2)**: `buildPathGuardHook` (`guardrails.ts:601`) with root `/opt/uzi-src` and the worker's join-token secret file in `extraSecretPaths` — without it, Read/Grep/Glob escape to worker-owned paths, and a prompt-injected chat could read the join token (`docs/proc-hardening.md:84-94`) and escalate to the full worker protocol, whose *run* claims DO carry the PAT. The path guard also carries the `/proc` deny (`guardrails.ts:527`) that closes `Read /proc/self/environ`, the one non-Bash egress for `CLAUDE_CODE_OAUTH_TOKEN`. M5 pins all of this with tests: a Bash `tool_use` is rejected in a chat session; a Read of the token path is denied; a Read outside `/opt/uzi-src` is denied. **Only with this decision landed does the "no egress channel" claim hold** — stated as the reward for doing it correctly, not as already true (↳review S9).
7. **uzi tools are an SDK MCP server in the worker (the `signals.ts` precedent, wired like `sdk-executor.ts:229`), backed by worker-authenticated, user-scoped API endpoints (↳review S5).** New `agent/src/uzi-tools.ts` (`createSdkMcpServer`) exposes:
   - `list_runs` / `get_run` / `get_run_messages` — read-only, calling new `GET /api/worker/chat/runs*` endpoints. These deliberately widen the worker's read surface from "the run I hold" (`GetRunOwnedByWorker`) to "runs of the user I belong to" — so they use **new queries filtered by the authenticated worker's `user_id`** (from `RequireWorker`), never a bare run_id lookup; a compromised worker still reads only its own user's runs. An admin's chat likewise sees only their own runs — reading other users' runs from chat is out of scope.
   - `propose_issue` — takes repo, title, description, labels; **never writes to the forge**. It POSTs a proposal row via `POST /api/worker/runs/:id/proposals`, which verifies the target run is owned by the worker's user AND `kind='chat'`. A per-worker proposal-creation cap bounds spam (↳review S6 — a prompt-injected loop can otherwise mass-create inert proposal rows).
   - **All forge- or model-derived text returned to the model is wrapped in the `ci_fix` untrusted-evidence framing** — run messages, and equally `get_run`'s issue titles and failure reasons (attacker-influenceable through the forge; ↳review D11).
8. **Issue creation is structurally human-gated, not model-promised.** `propose_issue` persists an `issue_proposals` row (chat run id, repo, title, description, labels, status `pending`); the proposal streams to the chat UI as a distinct `run_messages` kind rendered as a card with the draft + **Create issue** / **Dismiss** buttons. (No message-kind migration needed: `run_messages.kind` carries no DB CHECK and `AppendMessages` rejects only empty kinds — stated so no reviewer expects one; ↳review D12.) Only the browser's authenticated, CSRF-protected `POST /api/chats/:id/proposals/:pid/confirm` executes `Forge.CreateIssue` (`forge.go:225`, GitLab driver `gitlab.go:241`) via the user's own connection — forge-first, PAT-redacted, behind the existing per-user forge limiter. The agent can *ask* nicely all it wants; the write path simply does not exist without the click. This gate is genuinely structural (the chat agent holds no PAT and no forge tool) — as long as Decision 12 keeps the PAT out of the chat claim. Result (issue URL) is appended to the conversation so the model can reference it.
9. **Chat claims omit the forge PAT — as designed policy with wire-level type changes, not a free flag flip (↳review S4/F4).** The claim payload today delivers both secrets unconditionally (`claim.go:111-115`) and the agent types `forge_pat` as required (`protocol.ts:143`) with `runner.ts` consuming it unconditionally. Truly omitting it means: Go `ClaimSecrets.ForgePAT` becomes omit-empty/pointer for chat claims, TS makes it optional, and only `ChatRunner` (Decision 13) tolerates its absence. A wire test asserts the chat claim JSON contains no `forge_pat`/`forge_username` key. Narrowest-possible-credentials, one tier narrower than runs.
10. **Skills and agent templates do not ride chat claims (MVP).** The chat system prompt is worker-built (uzi purpose, baked-source location + BUILD_INFO, tool descriptions, honesty rules); templates/skills allocation is a run concept and adds nothing until chat grows roles. Revisit if users want persona control.
11. **Idle-completed conversations get an explicit Continue, because terminal runs refuse inputs (↳review D6).** `SubmitInput` rejects terminal runs (`ErrRunTerminal`), so once idle-completion lands, the conversation cannot be silently continued. The Chat page shows ended conversations with a **Continue** button: it creates a NEW queued `chat` run carrying `resume_of_run_id` (new nullable column) pointing at the ended run; the claiming worker best-effort resumes the persisted SDK `session_id` (works when affinity lands it on the worker whose disk holds the session — same mechanism as run resume §51) and says so honestly in the feed when it cannot ("continuing without prior context"). Raised idle windows (Decision 3) make this the exception, not the norm. Cross-run context *injection* (replaying prior transcript into a fresh session) stays out of scope.
12. **`runs.repo_id` becomes nullable and claim assembly forks — the most load-bearing change in this PRD, named as such (↳review D1/S4/F5).** `repo_id` is `NOT NULL` today (`00020_workers_runs.sql:32`) and `assembleClaim` → `GetRunClaimContext` INNER-JOINs `runs→repos→forge_connections` and unconditionally decrypts the bot PAT (`runtime.sql:172-188`, `service.go:388-392`) — a repo-less run would return zero rows and idle-loop forever. The migration relaxes `repo_id` to nullable with the kind-shape CHECK enforcing `kind='chat' ⇒ repo_id IS NULL` (and non-chat ⇒ NOT NULL, preserving today's invariant for every existing kind); claim assembly gains an explicit chat branch that (a) requires no repo and **no forge connection at all** — a user with an Anthropic token but no forge configured can still chat; (b) omits PAT/username per Decision 9; (c) opens only the Anthropic token. This touches the most security-sensitive assembly path in the codebase; M1 carries it as a named line item with the claim-context queries and their tests, and every `runs` query/DTO consumer is audited for a NULL `repo_id` (issue_iid/branch are already nullable since `ci_fix`, so the blast radius is repo_id alone).
13. **A `ChatRunner`, not `RunRunner` (↳review D4).** `RunRunner.execute` hardwires ensureClone → worktree → executor → push → MR and reads `clone_url`/`forge_pat` unconditionally — the Executor seam isolates only the SDK turn, not the git/MR orchestration around it. Chat gets its own slim runner (claim → session loop → complete; no clone, no worktree, no push, no MR) sharing the batcher/client/redaction collaborators. Named work item in M2, not a footnote.
14. **Admin visibility of chats: visible, documented, revisit flagged (↳review D7).** Chat rides `runs`, and run view/messages/WS are owner-or-admin — so an admin can open any user's chat conversation, which is more sensitive than a work run. MVP keeps the uniform rule (admins can read the DB anyway — the same honesty the skills read-authz applies) and `docs/chat.md` states it plainly ("admins can view chat conversations, like all runs"). A per-kind admin exemption is recorded as a revisit candidate, not silently ignored.
15. **Worker liveness is surfaced honestly.** The Chat page checks worker status (heartbeats already track it): no online worker → an explicit "No worker connected — chat needs your worker running" state with a link to the workers doc, instead of a message silently queueing forever. A queued chat claimed later still works (the machinery allows it); the banner is UX, not a gate.

## Technical Design

### API (api/)

- **Migration (draft `00065` — renumber at landing per CLAUDE.md; live head `00051`, PRD #37 holds draft `00061`)**:
  - `runs.repo_id` DROP NOT NULL; `runs.kind` CHECK gains `'chat'`; `runs_kind_shape` extended: `chat` ⇒ `repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL`; existing kinds ⇒ `repo_id IS NOT NULL` (Decision 12).
  - `runs.title text` (nullable, chat conversation title, first-message derived) and `runs.resume_of_run_id uuid` (nullable FK, Decision 11).
  - `issue_proposals` (id, run_id FK, repo_id FK, title, description, labels jsonb, status CHECK `pending/confirmed/dismissed`, created_issue_iid, created_at, resolved_at).
  - No `run_messages` change (kind is un-CHECKed by design; Decision 8).
- **Claim path**: kind-split of `ClaimRun` (run lane excludes chat; chat lane selects only chat), chat branch of `assembleClaim` (no repo/connection join, Anthropic-token-only, PAT omitted at the type level — Go omit-empty + TS optional), `claim_wire_contract_test` extended with the no-PAT assertion (Decisions 4/9/12).
- **Browser endpoints** (session + CSRF, owner-scoped): `POST /api/chats`, `GET /api/chats`, `POST /api/chats/:id/messages` (wraps `SubmitInput` `follow_up`), `POST /api/chats/:id/end`, `POST /api/chats/:id/continue` (Decision 11), `POST /api/chats/:id/proposals/:pid/confirm|dismiss`. Live view rides `/api/ws` + REST replay untouched.
- **Worker endpoints** (Bearer join token): chat claim lane; `GET /api/worker/chat/runs*` — new user_id-scoped queries, never run_id-trusting (Decision 7); `POST /api/worker/runs/:id/proposals` (owner + kind=chat verified, per-worker creation cap).
- **Sweeper**: skip `RUN_TIMEOUT` for `chat`; idle sweep + server-side turn ceiling (Decision 3). Requeue/affinity logic unchanged.
- **Rate limiting**: proposal confirm rides the existing per-user forge limiter; chat create/messages ride a per-user chat limiter (`CHAT_RATE_LIMIT_MAX`/`_WINDOW`).

### Worker (agent/)

- **Image**: agent build context switched to repo root + per-Dockerfile `Dockerfile.dockerignore` per template (include source, hard-exclude `.env*`/`.git`/`inspiration/`/`node_modules`), bake stage → `/opt/uzi-src` + `BUILD_INFO` (Decision 5).
- **`ChatRunner`** (new `agent/src/chat-runner.ts`, Decision 13) + **chat executor** (new `agent/src/chat-executor.ts`): SDK session with `cwd: /opt/uzi-src`, `tools` base-set restriction + `disallowedTools` + `settingSources: []` + Bash deny-hook + **path-guard hook rooted at `/opt/uzi-src` with the join-token path in `extraSecretPaths`** (Decision 6), `mcpServers: { uzi: buildUziToolsServer(ctx) }`; session id persisted for resume (§51); turn counter, per-turn wall-clock, idle timer (Decision 3).
- **Loop**: second claim loop for the chat lane running concurrently with the run slot; concurrency audit of shared collaborators (`Logger.addSecret` redactor state, shared `WorkerClient`) with tests (Decision 4); new blocking await-follow-up primitive in `steering.ts` (Decision 2); chat-lane poll at `WORKER_CHAT_POLL_MS` (default 1000ms).
- **`agent/src/uzi-tools.ts`**: the MCP server (Decision 7), untrusted-evidence wrapping for ALL forge/model-derived fields.

### Web (web/)

- **Chat page** (`web/src/pages/Chat.tsx` + nav entry): conversation list (chat-kind runs, ended ones with Continue) + conversation view. Reuses the run-view streaming machinery (WS subscribe, REST replay, markdown rendering with the §61 trust policy — model prose renders as markdown, tool/evidence content stays escaped) and the follow behavior (§62), styled as chat bubbles.
- **Composer**: send box → `POST /api/chats/:id/messages`; worker-offline banner (Decision 15); End chat; turn-cap notice; one-live-conversation note when a second chat queues (Decision 4).
- **Proposal card**: distinct rendering for proposal messages with Create/Dismiss wired to the confirm endpoints; proposal text renders as plain inert text (no clickable markdown from model output — same rule PRD #37 applies to repo-agent descriptions); created-issue link rendered on success.

### Docs + specs

- New `docs/chat.md` (`audience: user`): what chat can see (your runs, uzi's own source at the deployed version), what it can do (propose issues you confirm), what it can never do (touch repos, push, spend without your token), the worker-offline state, admin visibility (Decision 14), the idle/Continue behavior.
- `specs/ai.md`: Decisions 1–15 recorded. `specs/human.md`: the user-stated requirement set (chat in the web UI on the user's token, transparent worker use, baked source over clone, user-confirmed issue creation, works on docker now / k8s later) — needs user approval to edit.
- ARCHITECTURE.md: chat surface section (fifth surface) + the no-new-trust-boundary argument.

## Milestones

Progress log (team run, `feature/prd-39-chat-agent`; 3 coders in parallel worktrees, per-branch
review + security audit, all blocking/major findings resolved before merge):

**2026-07-10 — Phases 1, 2, 3-agent COMPLETE and merged into the integration branch:**
- Phase 1: M1 api (chat run kind, nullable `repo_id`, forked no-PAT claim) ✅; M2 agent phase-1 slice
  (baked `/opt/uzi-src` + confined chat executor) ✅; M4 web shell vs mocked API ✅.
- Phase 2: worker claim-lane split + chat steering (structural idle-race fix) + `user_message`
  emission (merge `bd5693c`) ✅; M3-api worker chat read endpoints + proposal creation + claim-first
  confirm + stuck-confirming sweep + boot-clamped timeout invariant + `repo_path` resolution
  (merges `22a3af6` … final api tip `e01fc74`) ✅.
- Phase 3-agent: `uzi-tools.ts` MCP (list_runs/get_run/get_run_messages/propose_issue) + per-call
  CSPRNG-nonce untrusted-evidence framing + the proposal-card run_message emission (merge `325ec3a`,
  tip `040b9e3`) ✅.

**Landed design deviations (all reviewed, better-than-spec):** chat claim is a SEPARATE
`ChatClaimPayload` with no PAT field at all (stronger than omit-empty); initial prompt travels as an
atomically-seeded `run_user_inputs` follow_up row (claim carries no prompt text); chat runs excluded
from runs/board/admin lists; `GET /api/chats` returns `{chats:[chatListDTO], max_turns}`;
`propose_issue` sends `repo_path` (server resolves user-scoped; internal UUIDs stay off the worker);
the proposal-card is a worker-emitted `proposal`-kind run_message keyed on `id`. Authoritative
Phase-3 wire catalog: `.claude/agent-team-tasks/prd39-phase3-wire-catalog.md`.

**STOPPED 2026-07-10 EOD. To resume tomorrow — do these in order (see "Resume plan" below the
milestones):** (1) review + merge P3-web; (2) M5 security validation pass; (3) M6 docs/specs/e2e;
(4) `/prd-done` up to MR creation.

- [x] **M1 — API: chat run kind, nullable repo_id, forked claim path**: migration (draft `00065`, incl. `repo_id` DROP NOT NULL + kind-shape rework + `resume_of_run_id`), NULL-repo_id audit across runs queries/DTOs, claim kind-split + chat assembly branch (no repo/connection required, PAT type-level omitted), chat CRUD/message/continue endpoints, sweeper idle handling + server turn ceiling, chat rate limiter; Go tests (kind-shape matrix, claim-lane isolation, chat claim with no forge connection succeeds, **no `forge_pat` key on the chat claim wire**, idle sweep, server turn cap). Validation: a chat run created, claimed by a stub worker, messaged, idle-completed, continued.
- [x] **M2 — Worker: baked source + ChatRunner/executor + concurrency** *(DONE — phase-1 slice `bd5693c`-merged + Phase-2 worker wiring `18019d3`-merged; reviewed + audited)*: build-context switch + per-Dockerfile ignores + bake stage + BUILD_INFO (image-content test: `/opt/uzi-src/api` exists, no `.env*` anywhere in the image), `chat-runner.ts`/`chat-executor.ts` (session, turns, per-turn wall-clock, idle timer, resume-of), second claim loop + shared-collaborator thread-safety audit/tests, `steering.ts` await-follow-up; agent tests (no clone attempted, turn caps, idle completion, concurrent run+chat session). Validation: live chat answers a question about uzi's source citing a real file while an issue run is executing on the same worker — live leg deferred to M6 e2e.
- [x] **M3 — uzi tools + issue proposals** *(DONE — api half `e01fc74` + agent half `040b9e3`, both merged; validated review + audit incl. adversarial evidence-fence attack)*: `uzi-tools.ts` MCP server, user_id-scoped worker read endpoints, untrusted-evidence framing (messages + titles + failure reasons), `issue_proposals` flow end to end (propose → card → confirm → `Forge.CreateIssue` → link back into chat), per-worker proposal cap; tests both sides (a worker can never read another user's runs; confirm is the only forge write path; dismissed proposal never writes; proposal spam capped). Validation: "why did run X fail?" answered from real messages; a confirmed proposal opens a real GitLab issue (`vtmocanu/uzi-e2e-scratch`) — end-to-end live validation deferred to M6 e2e.
- [x] **M4 — Web chat UI** *(DONE — phase-1 shell `6b6f0cd` + Phase-3 real-endpoint wiring `99df248` + `chatFromRun` optimistic-meta `7122191`, all merged; reviewed)*: Chat page, streaming view, composer, worker-offline banner, proposal cards (inert-link vitest), End chat/Continue, turn-cap notice; typecheck green.
- [x] **M5 — Security validation pass** *(DONE — auditor consolidated pass 2026-07-12, PASS, no regression; the live red-team leg landed in the M6 e2e)*: guardrail regression checks — `tools` base-set restriction actually rejects a Bash `tool_use` in a chat session; path guard denies reads outside `/opt/uzi-src`, of the join-token path, and of `/proc/self/environ`; `settingSources` still `[]`; no PAT on the chat claim wire; proposal confirm requires session+CSRF; injection attempt via a poisoned run message / issue title does not yield tool misuse (scripted red-team scenario in e2e with the stub executor where feasible, manual live check otherwise).
- [x] **M6 — Docs + specs + e2e** *(DONE — docs/specs `0f6fe52`/`07a4ee3`, config knobs `db191cb`; chat stub-executor `984e6db` + image-check full-mode fix `da1fcb0`; e2e `7dbcec6`)*: `docs/chat.md` (frontmatter per `docs/README.md`), specs/ai.md + human.md (user-approved) + ARCHITECTURE.md updates, e2e scenario (create chat → message → tool call → proposal → confirm → idle-complete → continue) on the isolated stack. **e2e ran GREEN: 17 chat legs incl. the live injection red-team (poison quoted in the nonce fence, no tool action), lane coexistence, propose→confirm→real issue, dismiss→no-write, idle-complete + gapless seq, continue.**

## Status: COMPLETE (2026-07-12) — ready for MR review

All six milestones done, merged, reviewed + security-audited on `feature/prd-39-chat-agent`; M5 security
PASS; M6 e2e PASS. Remaining is landing-only:
- **Migration renumber at landing** (CLAUDE.md convention): drafts `00065` (chat runs) + `00066` (proposal
  `confirming`) must be renamed to the next free numbers above the live head on `main` at merge time.
- **Optional cleanup deferred** (non-blocking, tracked): #18 drop `repo_id` from the worker-facing
  proposal-create DTO (Decision-7 purity; non-exploitable — the worker's own repo id); the three inert
  `created_issue_*`/`resolved_at` null fields on the emitted proposal payload (harmless extra JSON).

## Resume plan (2026-07-11, now HISTORICAL — see Status above) — pick up here

Integration branch `feature/prd-39-chat-agent` (worktree `../prd-39-chat-agent`). All merged work
is reviewed + audited with zero open blocking/major findings.

**Progressed after the team hit the account session-limit (lead solo, 2026-07-11 early AM):**
- P3-web MERGED (merge `255bf8d`; the `chatFromRun` follow-up `7122191` was lead-reviewed — small,
  UI-only, tested, gate green 393/393 — since the reviewer was capped; re-review optional tomorrow).
  So **M1/M2/M3/M4 are all complete and integrated.**
- M6 docs DONE (commit after `255bf8d`): `docs/chat.md`, ARCHITECTURE.md "Chat with uzi (the fifth
  surface)", `specs/ai.md` §169–175. check-docs passes. Carry-overs #7 (untracked-secrets operator
  note) + #10 (PRD-label suggestion) folded into the docs.
- **Environment note:** `NODE_OPTIONS` carries a broken cmux `--require=…restore-node-options.cjs`
  preload (temp file cleaned) that crashes every node invocation incl. git hooks — run node/npm/git
  commands with `NODE_OPTIONS="--max-old-space-size=4096"` until the cmux shim restores it.

**Remaining (needs the team at reset, or lead solo):**
1. **M5 — Security validation pass** (agents: **auditor** lead, **tester** for the live/e2e legs).
   Most sub-checks were already verified per-branch during review; M5 is the consolidated end-to-end
   proof + the deferred live legs. Fold in these tracked carry-overs: **#6** (image-content check must
   assert `/opt/uzi-src` is root-owned + unwritable by the agent user — add `find /opt/uzi-src ! -user
   root` / write-as-uzi to the check); **#9** already done (verify in the live pass that the real
   worker constructs `ChatExecutor` with `secretPaths=[workerTokenFile]`); **#18** (optional Decision-7
   purity — drop `repo_id` from the worker-facing proposal-create DTO). Run the M5 checklist in the
   milestone below against the merged integration branch; the injection red-team leans on the
   evidence-fence (already adversarially audited) + the no-egress tool surface.

2. **M6 — the docs are DONE; e2e + specs/human.md remain.** `docs/chat.md`, ARCHITECTURE.md, and
   `specs/ai.md` §169–175 are written and committed (carry-overs #7 + #10 folded in). STILL TODO:
   (a) **`specs/human.md` Feature #39 entry — NEEDS USER APPROVAL** (draft prepared; the spec contract
   forbids editing human.md without the user's sign-off); (b) the **e2e scenario** (tester) — create chat
   → message → tool call → proposal → confirm → idle-complete → continue — which requires **#15** first
   (the chat lane runs the real ChatExecutor even under `UZI_EXECUTOR=stub`; add a chat stub path so the
   isolated stack runs without a live Anthropic token).

3. **`/prd-done` up to MR creation** (per the `/prd-full` flow — stop at MR, do not merge/close).

Migration renumber reminder (landing): drafts `00065` (chat runs) + `00066` (proposal `confirming`)
must be renamed to the next free numbers above the live head at rebase time (CLAUDE.md convention).

Team roster to re-spawn tomorrow (all opus except documenter=sonnet): coder-m1 (api), coder-m2
(agent), coder-m4 (web), reviewer, auditor — plus tester + documenter + spec-keeper for M5/M6.
Full team task list persisted in the session; briefs live in `.claude/agent-team-tasks/`.

## Milestone dependency / parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 (parallel) | M1 (api/), M2 image+bake+executor skeleton (agent/ + compose), M4 UI shell against mocked API (web/) | — | `api/internal/store/migrations/`, `queries/runtime.sql`, `workersvc/`, `handler/` · `docker-compose.yml`, `agent/templates/*/Dockerfile*`, `agent/src/chat-{runner,executor}.ts` · `web/src/pages/Chat.tsx` |
| 2 | M2 claim-loop split + steering, M3 (agent/ + api/) | M1 wire shapes | `agent/src/worker.ts`, `steering.ts`, `uzi-tools.ts` · `api/internal/workersvc/`, forge call site |
| 3 | M4 wiring (web/) | M1+M3 | web api client + Chat page |
| 4 | M5, M6 | M2+M3+M4 | e2e, docs, specs |

Note: `worker.ts`/`steering.ts` and `workersvc/service.go` churn — check other in-flight PRDs (#37 touches `sdk-executor.ts`/`steering.ts`) before starting Phase 2.

## Out of Scope

- Auto-spawning workers when chat is opened — always-on worker on compose, operator-spawned pods on the k8s phase (decided 2026-07-10, specs/ai.md §168).
- Bash/shell, file mutation, subagents, or skills/templates in chat sessions (Decisions 6/10) — revisit with demand.
- Chat reading *other users'* runs, even for an admin's chat (Decision 7). Admin *human* visibility of chat runs stays (Decision 14; exemption is a recorded revisit candidate).
- Cross-run context injection (transcript replay) — Continue resumes the SDK session when the disk still has it, nothing more (Decision 11).
- Any forge write beyond confirmed issue creation (no label moves, no comments, no MR actions from chat).
- Editing uzi's own code from chat ("uzi fixes itself") — that is a future issue-run on the uzi repo, not a chat capability.
- WS wakeup for worker input pickup (poll floor stated in Decision 2); a second execution provider.

## Accepted residuals (named, per review)

- **Prompt injection via investigated evidence** (poisoned run message, attacker-authored issue title): with Decisions 6/7/8 landed, the worst case is misleading prose to the user or a plausible-looking proposal card — still human-confirmed, no egress, no mutation. The size of this residual is *contingent* on the `tools` restriction + path guard landing exactly as specified; without them it would be token exfiltration, which is why M5 gates the PRD.
- **`CLAUDE_CODE_OAUTH_TOKEN` sits in the chat agent's env** (as for runs, `sdk-env.ts`): chat's no-Bash/no-network/no-`/proc` surface is strictly stronger than the run surface, but the token's presence itself is unchanged — the PRD #37 Decision 2 residual, narrowed not removed.
- **A compromised worker binary** can read its own user's runs via the new chat read endpoints (scoped to that user, never others) and spam inert proposal rows up to the cap. Same trust level the worker already holds for that user's claims.

## Success Criteria

- A user with a connected worker opens Chat, asks "how does the plan-approval gate work?", and gets an answer citing real files from the deployed version's baked source — while a normal issue run executes concurrently on the same worker.
- "Why did my last run fail?" is answered from the actual run messages via the tools, not hallucinated.
- An issue idea discussed in chat becomes a real GitLab issue only after the user clicks Create on the proposal card; dismissing it provably writes nothing to the forge.
- A user with an Anthropic token but **no forge connection** can still chat (investigation/proposal tools degrade gracefully).
- Chat streams live over the existing WS with replay-on-reconnect; a second browser tab shows the same conversation; an idle-ended conversation continues via Continue.
- With no worker online, the Chat page says so plainly instead of hanging.
- No guardrail regression (M5 list), and the chat claim payload demonstrably contains no forge PAT key.
- Everything works identically under compose today and requires no chat-side change for the k8s worker phase (claim/poll protocol only).
