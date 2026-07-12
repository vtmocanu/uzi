# PRD #25: Slack Integration — per-user run notifications, approval buttons, reply-from-Slack

**GitLab Issue**: [vtmocanu/uzi#25](https://gitlab.example.com/vtmocanu/uzi/-/issues/25)
**Status**: Complete (2026-07-10, MR !31)
**Priority**: Medium
**Created**: 2026-07-06
**Depends on**: nothing pending — rides merged machinery only: the run lifecycle + steering inputs (PRD #4), admin `app_settings` (PRD #19), per-user columns on `users`, `secretbox`. No forge-layer changes.

## Problem

Users only learn that an agent run finished, failed, or — worst — is parked at the plan-approval gate by keeping the webui open. `awaiting_approval` is a human-latency bottleneck by design (the primary directive's gate), but today the human isn't told they're needed: a run can sit blocked for hours because nobody had the tab open. There is no push channel of any kind.

## Solution Overview

A Slack bot owned entirely by the `api` service:

1. **Socket Mode, outbound-only** (`slack-go/slack` + `socketmode`). The api container opens a WebSocket *out* to Slack; no inbound HTTP, no public URL, no signing-secret endpoint — the loopback-only trust posture keeps zero inbound surface. (Caveat honestly recorded below: enabling Slack does export run *status metadata* to Slack's cloud — see Security.)
2. **Per-user DMs about their runs**: state transitions of runs a user owns (started, awaiting approval, completed with MR link, failed, cancelled) arrive as Slack DMs. One root message per run, updated in place with the current status; subsequent events thread under it. Every message deep-links to the run view in the webui. Messages carry **status + issue title + link only** — the plan body stays behind the deep link (content-minimization, see Security).
3. **Unblock from Slack**: the `awaiting_approval` message carries **Approve** / **Reject** buttons (Block Kit) plus an **Open in uzi** link. Approve submits `approve_plan` through `workersvc.SubmitInput` — the same ownership-checked path the webui uses. Reject prompts for a threaded reason (the webui reject also takes a reason, `RunView.tsx` `submit("reject_plan", reason)`): the reply becomes the `reject_plan` content, with a "Reject without reason" escape hatch. **Threaded replies** on a live (non-gate) run become `follow_up` inputs.
4. **Config from ENV or webui**: `SLACK_BOT_TOKEN` / `SLACK_APP_TOKEN` env vars *or* admin-settings fields (sealed at rest with `secretbox`, write-only — never echoed back; this requires teaching the settings registry a structural secret-key class, scoped as its own milestone). ENV wins; when a value comes from ENV the webui field renders greyed out (disabled, "set from environment" hint), enforced server-side. Same precedence pattern for the new `public_base_url` used to build deep links.
5. **User mapping, default ON with confirmed linking**: once the admin wires the workspace, uzi auto-matches each user by account email (`users.lookupByEmail`) and sends a one-time **link-confirmation DM** ("uzi wants to send you run notifications — Confirm / Not me"). Run content flows only after Confirm; any user can disable in their settings at any time. Manual Slack member-ID override for email mismatches, plus a "send test DM" button. (uzi emails are unverified at registration, so the confirmation round-trip — not the email — is what makes the link trustworthy; see Security.)

### Inspiration check (audited 2026-07-06; corrected after fact-check — multica already uses Socket Mode)

| Concern | bottega | multica | dot-agent-deck | uzi will do |
|---|---|---|---|---|
| Slack integration | None (one doc mention) | **Full chat bridge** (`server/internal/integrations/slack/`): Socket Mode transport (`slack_channel.go` `socketmode.New` + `RunContext`, Events API payloads delivered over the socket), slash commands, channel↔session binding, mrkdwn conversion, threading, typing indicator, BYO app install validated via `StartSocketModeContext` (slack-go v0.26.0) | None (its notification PRDs explicitly keep Slack out) | Different *shape*, same transport: a **notification/approval bot**, not a chat channel. Reuse multica's proven pieces: slack-go, the socket supervisor pattern (per-connection cancellable context, reconnect), ACK-envelope-first inbound handling, base64-sealed BYO tokens, validate-token-on-save |
| Human gate from chat | — | Agent conversations happen *in* Slack, but no approve/reject affordance on a plan gate | — | Block Kit buttons + threaded reject-reason wired to the existing `approve_plan`/`reject_plan`/`follow_up` steering inputs — none of the three has this |

## Technical Design

### Settings: secret keys + ENV overlay (net-new registry work, milestone M1)

The current settings machinery is plaintext-only and would echo or reject tokens as-is: `Cache.All()` feeds `GET /api/admin/settings` verbatim, and `Validate`'s default case is `ValidateLabel` (≤64 runes, no commas), which rejects any real `xoxb-`/`xapp-` token. Two structural additions, both new (verified against `api/internal/settings/settings.go`, `handler/settings.go`, `config.go` — no `source` concept or ENV-over-DB merge exists today):

1. **Secret-key class in the registry**: a declared set of secret keys (`slack_bot_token`, `slack_app_token`) that (a) get sealed with `secretbox` and base64-encoded before `UpsertAppSetting` (`app_settings.value` is `TEXT`; multica's `byo_install.go` uses the same base64-of-sealed encoding), (b) are **structurally excluded** from `All()`/`GetSettings` value output — the API emits only `configured: true|false`, so the handler *cannot* forget to redact, (c) get dedicated `Validate` branches, and (d) are readable only through a new decrypt accessor used by `slacksvc`. A test asserts the admin GET response never contains token bytes (sealed or plain). Saving validates live: `AuthTest` for the bot token, a Socket Mode handshake for the app token (multica's BYO pattern) — validation errors must not echo the submitted token.
2. **ENV-source overlay**: `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN`, `UZI_PUBLIC_BASE_URL` in `config.go`. Per key, ENV wins over DB; `GET /api/admin/settings` gains a per-key `source: "env"|"db"|"default"`, and `PUT` rejects writes to env-sourced keys (409) so the webui greying reflects enforced policy. This means threading `config.Config` into the settings handler — new plumbing, called out as such.

Non-secret new keys: `slack_enabled` (`"true"|"false"`, default `"false"`) and `public_base_url` (default `http://127.0.0.1:8080`; validated `http(s)`-only server-side — it becomes a button URL in every DM, so no other schemes). On a laptop the default base URL only resolves for the laptop's own user; that's honest and documented, and a Tailscale/LAN URL can be set here.

### Persistence (migration drafted as `00042`+ — final number assigned at merge time per the CLAUDE.md convention)

Per-user fields follow the established pattern of columns on `users` (like `default_model` 00031, `autopilot_enabled` 00037, `theme` 00041 — there is no `user_settings` table):

```sql
ALTER TABLE users
  ADD COLUMN slack_member_id text,              -- manual override; NULL = email auto-match
  ADD COLUMN slack_notify boolean NOT NULL DEFAULT true,   -- per-user kill switch; default ON
  ADD COLUMN slack_resolved_id text,            -- effective linked Slack id (cached lookupByEmail result or the override)
  ADD COLUMN slack_link_confirmed_at timestamptz;  -- set when the user hits Confirm in the link DM; NULL = no content flows

-- exactly-one-user-per-Slack-identity: the inbound authz join must never be ambiguous
CREATE UNIQUE INDEX users_slack_resolved_id_key ON users (slack_resolved_id) WHERE slack_resolved_id IS NOT NULL;

CREATE TABLE slack_run_messages (               -- one row per notified run: threading + interactivity anchor
  run_id uuid PRIMARY KEY REFERENCES runs ON DELETE CASCADE,
  channel_id text NOT NULL,                     -- the DM channel
  root_ts text NOT NULL,                        -- root message; status edits target this, events thread under it
  gate_ts text,                                 -- ts of the live awaiting_approval message (buttons), NULL when no open gate
  gate_state text,                              -- NULL | 'open' | 'reject_pending' (Reject pressed, awaiting threaded reason)
  updated_at timestamptz NOT NULL
);
```

Linking rules: setting a manual override that collides with another user's `slack_resolved_id` is rejected (the unique index is the backstop); override writes go only through the self-scoped user-settings path (keyed by `UserFromContext`), so no user can touch another's mapping. Email changes clear `slack_resolved_id` + `slack_link_confirmed_at`. On inbound, the Slack-id→user lookup must resolve to **exactly one confirmed row** or the event is refused with an ephemeral error — never a "first row wins" guess.

### Slack service (`api/internal/slacksvc`, new package)

- **Manager goroutine** started from `main`: polls the settings cache (the cache is TTL + `Invalidate()` only — there is no watch/pubsub) and diffs; when enabled + both tokens present it runs a `socketmode.Client` with exponential-backoff reconnect (multica's supervisor pattern: cancellable per-connection context + `RunContext`), otherwise idles. Settings changes hot-restart the socket — no api reboot. Connection state (`disabled | connecting | connected | error:<class>`) is exposed on the admin settings DTO for the webui status chip. slack-go debug logging stays **off** and the socket connection URL (`wss://…?ticket=…` — a credential the token-pattern redaction would miss) is never logged.
- **Outbound notifier**: subscribes to run transitions at the existing seam — `workersvc.SetBroadcaster` gains a trivial fan-out wrapper (`multiBroadcaster{hub, slack}`). `Broadcaster.PublishState` runs on the request path and **must never block** (documented contract, `service.go`), so the slacksvc implementation enqueues to a channel and returns; run/owner loads and Slack calls happen on the notifier's own goroutine. Slack failures are logged (redacted) and never affect the run lifecycle — strictly best-effort.
- **Sweeper coverage (a real gap the seam alone doesn't fill)**: the sweeper's bulk transitions (`SweepClaimedNeverStarted`, `SweepRunningTimeout`, `FailRunsOfStaleWorkersOverCap`, `RequeueRunsOfStaleWorkers`) currently run as bulk SQL returning row counts only and **never touch the Broadcaster** — so timeout/worker-loss failures, exactly the "failed" DMs users most need, would be silently missed. M3 changes these sweep queries to `RETURNING id, status` (owner joined on publish) and has `Sweep` publish each transition through the same fan-out (the WS hub gains these events too — a strict improvement; today's webui also misses live sweeper transitions).
- **Inbound router** (Socket Mode events): `block_actions` (Approve/Reject/Confirm-link) and `message.im` thread replies. **ACK the envelope first, process async** (Slack retries un-ACKed envelopes in ~3s; multica ACKs before any DB work). Every action resolves the envelope's authenticated Slack user id (never a payload value blob) → confirmed uzi user via the exactly-one rule above, then goes through `workersvc.SubmitInput` (ownership-checked). Note honestly: `SubmitInput` enforces ownership and rejects terminal runs, but does **not** verify `awaiting_approval` for `approve_plan` — so slacksvc reads `run.Status` itself for stale-click handling ("already handled in {state}") before submitting. Unlinked Slack users get an ephemeral "this Slack account isn't linked to uzi", coalesced (not re-sent per event). Per-Slack-user inbound rate limit guards against event floods.
- **Redaction**: a slacksvc redactor (same approach as `forge/redact.go` — redaction is per-package here, there is no central middleware) scrubs `xoxb-`/`xapp-` from logs/errors. **Outbound scrub pass**: everything sent to Slack additionally passes a secret-pattern scrub (`sk-ant-`, `glpat-`, `xoxb-`, `xapp-`) as defense-in-depth. Tokens live only in api memory; never in worker claims, run messages, or agent context — the guardrail layering is unchanged.

### Message design (content-minimized)

- **Root message** (first notified transition): `"[uzi] run on {repo}#{iid} «title» — {status}"` + Open-in-uzi link button. Status edits (`chat.update`) keep the root current (▶ running, ⏸ needs you, ✅ MR !42, ❌ failed: reason). No plan/diff content — status, issue title, MR URL, failure reason only.
- **Gate message** (`awaiting_approval`): posted in-thread, records `gate_ts`/`gate_state='open'`. **No plan excerpt** — "Plan ready for review" + **Approve** (primary, native confirm dialog) / **Reject** / **Open in uzi** (read the plan there).
  - **Content-minimization update (PRD #37 M7)**: when the run detected a repo roster in `.claude/agents/`, the gate shows two approve buttons (`Approve · repo agents (N)` / `Approve · my templates`) and lists the repo agent **NAMES** in the body. This is a deliberate, bounded relaxation of the minimization posture: the names (≤16, kebab-case, ≤64 chars, `IsValidName`-validated, mrkdwn-escaped) egress to Slack's cloud so the user can pick the roster; the **descriptions** (1024 chars of repo-authored free text) are never sent — the SQL that feeds the message extracts names only. NULL and `[]` rosters render the single-approve shape identically.
  - Approve → stale-check → `approve_plan`. Message edited to the outcome, buttons removed.
  - Reject → `gate_state='reject_pending'`; message edits to "Reply in this thread with the rejection reason — or:" with a **Reject without reason** button. A thread reply in this state submits `reject_plan` with the reply as content (mirroring the webui's reasoned reject); the button submits it reason-less.
  - Resolution from *either* surface (webui or Slack) edits the Slack message and clears `gate_ts`/`gate_state`, making button handling idempotent; a stale click gets an ephemeral "already handled in {state}".
- **Thread replies**: while a gate is open (not reject-pending), a bare reply gets an ephemeral "the plan gate takes Approve/Reject; press Reject to use a reply as the reason, or open in uzi" — `follow_up` is *not* submitted during a gate (the worker is parked on the gate; whether it would consume a `follow_up` there is unverified — M4 includes verifying this, and if the worker does consume it as plan feedback, bare replies get wired to `follow_up` then). On a live run outside a gate, replies become `follow_up` inputs; on a finished run, an ephemeral "run already finished". Every accepted reply gets a ✅ reaction as the ack.
- **Completed/failed**: threaded event message with MR URL (completed) or failure reason (failed), root updated. Note: a live-run cancel surfaces as `failed` with `failure_reason="run cancelled"` (true `cancelled` only on the no-live-poller path) — the notifier special-cases that reason to render 🚫 cancelled.

### Web UI

- **Admin → Settings, new "Slack" card**: enable toggle, bot/app token inputs (write-only password fields showing `configured ✓`), public base URL, live connection-status chip, and the ENV-greying behavior (disabled inputs + hint when `source: "env"`).
- **User settings, new "Notifications" section**: enable/disable toggle (default on), link status (unlinked / pending confirmation / confirmed, with Slack display name when resolvable), manual member-ID override, **Send test DM** button. Clear error state when neither auto-match nor override resolves.

### Testing

- `slacksvc` unit tests against a fake Slack API (slack-go's `slacktest` or an httptest stub): notifier threading/edit sequences, ACK-first inbound, exactly-one-row linking (collision refusal), unlinked-user coalescing, stale-click idempotency, reject-pending flow, outbound scrub, redaction.
- Settings tests: secret keys never appear in `All()`/admin GET (byte-level assertion), token validation branches, `source` reporting, PUT-409 on env-sourced keys.
- Sweeper fan-out tests: swept runs publish transitions with correct ids/owners.
- Web: vitest for greyed fields, link-status states, user-settings section.
- e2e: stack boots green with Slack disabled (default) — the integration must be a strict no-op when unconfigured.

## Milestones

- [x] **M1 — Settings foundation**: secret-key class in the registry (sealed+base64 storage, structural exclusion from reads, `configured` flags, decrypt accessor, validate-on-save) + ENV-source overlay (`source` field, PUT-409, config threading) + `slack_enabled`/`public_base_url`; admin webui Slack card with env-greyed fields (status chip stubbed "disabled"). *(Done: commits 4d5fb20, 3383ab6, 1c8173a — reviewed + audited clean.)*
- [x] **M2 — Socket manager**: `slacksvc` manager (connect, backoff reconnect, hot-reload by polling+diffing settings, connection state on the admin DTO + live status chip), debug-log/ticket-URL hygiene, redactor. *(Done: commits 51f4ad5, 98eea02, edb4999 — reviewed + audited clean.)*
- [x] **M3 — User linking + run notifications**: migration (users columns + unique index + `slack_run_messages`), email auto-match, link-confirmation DM round-trip, manual override with collision refusal, test-DM; lifecycle fan-out (`multiBroadcaster`, non-blocking enqueue) **including the sweeper `RETURNING` fan-out**; root/thread/edit message flow with deep links; user-settings UI. *(Implemented: commits 31231ad, 295b0e2, d342849, 8b49978, cee0985, 2d4f817. Parts 1+2 reviewed + audited — the audit's one blocking finding, mrkdwn/link injection, fixed in 2d4f817. Parts 3+4 + the fix (d342849…2d4f817): **both verdicts IN.** Reviewer: APPROVE, no blocking. Auditor: injection fix verified RESOLVED; **one open Blocking (Medium) — unbounded DM endpoints**: `PUT /me/slack/override` + `POST /me/slack/test-dm` are RequireAuth-only with no rate limit, giving an authenticated user an arbitrary-member-DM-spam primitive plus a member-id enumeration oracle (test-dm 200/502 and override 409 both leak validity). No content/authz compromise (anti-squat holds; templates fixed). **FIXED in `1707d04`** (per-user limiter on both DM routes + 30s per-target cooldown, cooldown armed even on failed sends so the 502 oracle is throttled too) — **auditor verdict: finding CLOSED.** Accepted residuals (lead ruling, trusted-team model): the semantic oracles (test-dm 200/502, override 409) stay as PRD-specified UX; ~30/min varying-target DM spam bounded by the forge budget. One Low optional follow-up: a dedicated tighter budget (~5/min) for the two DM routes — queued for an M4/M5 boundary.)*
- [ ] **M4 — Approval gate from Slack**: gate message + Approve/Reject/Open, ACK-first `block_actions` → slacksvc gate-state check → `approve_plan`/reasoned `reject_plan` via `SubmitInput`, cross-surface idempotency (webui resolution edits the Slack message), stale-click ephemerals; verify worker behavior for `follow_up`-during-gate and wire bare replies accordingly.
- [ ] **M5 — Reply-from-Slack**: thread replies → reject-pending reason / `follow_up` on live runs, ✅ ack reactions, unlinked/finished-run ephemerals (coalesced), per-user inbound rate limit . *(The `/me/slack` DM-endpoint rate limit originally penciled in here was promoted to an M3 blocking fast-follow — see the progress checkpoint.)*
- [ ] **M6 — Tests passing**: the suites above green (`go test ./...`, `npm test`, e2e unchanged with Slack off).
- [x] **M7 — Docs & specs**: `docs/slack.md` (audience: user) with the Slack app manifest YAML (scopes as VERIFIED against the implemented code, 2026-07-10: `chat:write`, `im:write`, `im:history`, `users:read.email`, `reactions:write` — the draft's `users:read` proved unneeded, and `reactions:write` was missing for the M5 ✅ ack; Socket Mode + interactivity + `message.im` event) + setup steps + a privacy note that `users:read.email` lets uzi resolve workspace members' emails and that enabling Slack sends run status metadata to Slack's cloud; ARCHITECTURE.md trust-model addendum; `specs/ai.md` decision record.

### Progress checkpoint (final — 2026-07-10, agent-team sessions 1+2)

**ALL MILESTONES COMPLETE (M1-M7) and agent-verified** — every commit reviewed + audited (approve / no-blocking), docs fact-checked, specs recorded (ai.md §142-153 after the landing renumber; human.md Feature #25 user-approved). Landing merge of origin/main (PRD #28/#32/#33 drift) at `51e7636`, all gates green on the merged tip (api go test; web 311 tests + build; agent 270; live-DB store ITs incl. seal-at-rest and is_active proofs; e2e strict-no-op with Slack off). Status: in-review — MR open, awaiting human review/merge.

Original session-1 resume note (historical): branch `prd-25-slack-integration`, tip `2d4f817`, all gates green (`go build -buildvcs=false ./... && go test ./...` + `-race` on slacksvc/handler; web typecheck + 283 tests + build; gitleaks per commit). Worktree: `../prd-25-slack-integration`.

- **Agent-review status**: ALL landed work is now agent-verified — M1, M2, M3 (all parts + injection fix) reviewed AND audited; verdicts approve/no-blocking EXCEPT one open Blocking from the final audit (unbounded `/me/slack` DM endpoints, above). The whole team was shut down cleanly at EOD 2026-07-09; spawn a fresh team on resume.
- **Remaining, in order**: (0) the `/me/slack` rate-limit blocking fast-follow; then M4 (approval gate), M5 (reply-from-Slack), M6 (e2e + the auditor-recommended raw-DB token byte-check assertion), M7 (docs/specs), then MR creation. M4's inbound-router foundation already exists (`routeInteractive` seam built in M3 to the PRD's inbound rules — M4 only adds action kinds).
- **Deferred small items** (non-blocking): backoff-reset-on-flap gating (M2 reviewer nit a); requeue-swept runs render as "queued" in DMs (consider "↻ requeued" or suppress); redundant `OpenDM` on non-first transitions; rare duplicate-root if anchor upsert fails after post; Slack display-name resolution in `GET /me/slack` (kept pure-DB deliberately); worker-side `failure_reason` sanitization at source (notifier now bounds to 500 runes + escapes).
- **Working docs**: `.claude/agent-team-tasks/prd-25-brief.md` (team rules), `prd-25-m3-checkpoint.md` (coder handoff for parts 3-4, includes design + traps).

### Phasing & parallel-safety

M1 → M2 → M3 → M4 → M5 is a dependency chain; M6 grows with each milestone rather than trailing; M7 can start after M4. One sequential track — parallelism is *across* PRDs, not within this one. Files touched: `api/internal/slacksvc` (new), `settings`, `config`, `handler`, `workersvc` (fan-out seam + sweeper `RETURNING` queries), one migration, `web/src` settings pages, `docs/`.

## Success Criteria

- A run hitting `awaiting_approval` produces a Slack DM with working Approve/Reject within seconds; approving from Slack resumes the run with no webui involvement; rejecting with a threaded reason lands that reason as the `reject_plan` content.
- A run failed by the sweeper (timeout, worker loss) produces a failure DM — not just worker-reported failures.
- With `SLACK_BOT_TOKEN`/`SLACK_APP_TOKEN` exported, the webui Slack fields are visibly disabled and server-side writes to them are rejected.
- No inbound Slack action can affect a run whose owner isn't the *confirmed-linked* Slack user sending it; an ambiguous or unconfirmed link refuses rather than guesses.
- With Slack unconfigured, uzi behaves exactly as today (e2e green untouched).
- No Slack token (or socket ticket URL) ever appears in logs, API responses, run messages, worker claims, or agent context.

## Security posture (from the security review)

- **Identity mapping is the authz primitive** and uzi emails are unverified at registration — hence: confirmation round-trip before any run content flows, unique index on the effective Slack id, exactly-one-row inbound resolution, self-scoped override writes. Account-squatting an email then routes *nothing* anywhere until the squatter's Slack target explicitly confirms a link DM that names the uzi account.
- **Primary directive unaffected**: the plan gate is a latency/authorization control, not a `main`-write capability; a wrongful approval can at worst produce a branch + MR. All four guardrail layers untouched; Slack tokens never leave api memory.
- **Content minimization**: run *content* (plans, diffs) never goes to Slack — status, titles, MR links, failure reasons only, plus an outbound secret-pattern scrub. Enabling Slack still exports status metadata off-box; documented in ARCHITECTURE.md.
- Ownership on approve/reject rides `SubmitInput` → `GetRunByIDForUser` (user-scoped); gate-state checks are slacksvc's job (SubmitInput doesn't check `awaiting_approval` for approves).

## Risks & Mitigations

- **Slack app creation friction** — paste-ready app manifest in `docs/slack.md`.
- **Email mismatch** (uzi email ≠ Slack email) — manual member-ID override + test DM.
- **Deep links unreachable off-laptop** — `public_base_url` setting (http/https-only); default documented as loopback-only.
- **Socket flapping / Slack outage** — backoff reconnect; notifications best-effort, never block or fail runs; webui remains source of truth.
- **Inbound floods** — ACK-first + per-user rate limit + coalesced error ephemerals.
- **Token leakage** — secretbox at rest, structural no-read settings class, log redaction incl. ticket URLs, api-only custody.

## Out of Scope (deliberate)

- Channel/broadcast notifications (DM-only; a team channel digest is a natural follow-up).
- Events API / public-URL mode, slash commands, Slack as a full chat surface (multica's shape).
- Multiple Slack workspaces; non-Slack providers (the notifier sits behind the fan-out seam, so a future Telegram/webhook notifier slots in without rework).
- Email verification at registration (would strengthen auto-match; tracked as a possible follow-up PRD, not required given the confirmation round-trip).

## Decision Log

- **2026-07-06 — Socket Mode only** (user-confirmed): outbound-only fits the loopback deployment; Events API rejected, "support both" rejected as premature surface. (Corrected after fact-check: this matches, not differs from, multica — multica's transport is also Socket Mode; uzi's differentiator is the notification/approval shape.)
- **2026-07-06 — Email auto-match + manual override** (user-confirmed) over manual-only or silent-match-only.
- **2026-07-06 — Per-user toggle defaults ON** (user-directed), amended by security review: default-ON means uzi *initiates a link-confirmation DM* automatically; run content flows only after the user confirms in Slack (one click, no webui needed). Unverified registration emails make an unconfirmed auto-match unsafe as an authz join.
- **2026-07-06 — Buttons + threaded reply** (user-confirmed) over buttons-only or button+modal. Refined after design review: `reject_plan` is terminal, so feedback can't follow it — Reject enters a `reject_pending` state whose threaded reply *is* the reasoned reject (webui parity); bare replies during a gate are nudged, not guessed at.
- **2026-07-06 — Tokens sealed in `app_settings` via secretbox (base64), behind a new structural secret-key registry class**: review showed the existing registry is plaintext-only (would echo values, and `ValidateLabel` rejects token-length strings), so this is net-new registry work, scoped as M1 — not "reuse as-is".
- **2026-07-06 — ENV-over-DB enforced server-side** (`source` field + PUT 409): also net-new (no ENV/settings merge exists today); the webui greying is a reflection of policy, not the policy itself.
- **2026-07-06 — Sweeper transitions must be published** (fact-check finding): the Broadcaster seam alone misses sweeper-driven failures/requeues; M3 adds `RETURNING`-shaped sweep queries + publish, which also fixes the webui's live view for these transitions.
- **2026-07-06 — No plan excerpts in Slack** (security review): content minimization; the plan is one click away behind the deep link.
- **2026-07-09 — Sweeper fan-out also restores board columns** (AI decision, implementation): `publishSwept` publishes through BOTH the broadcaster (WS hub + Slack) and the board-lifecycle seam — before this, a timed-out/worker-lost run's board column stayed stuck in "In Progress" forever. Judged a latent-gap bugfix within the PRD's "publish each transition through the same fan-out"; reviewer flagged the broadening, lead accepted.
- **2026-07-09 — ENV overlay + decrypt accessors live on the settings cache** (AI decision, diverges from the PRD's "threading config into the settings handler"): a one-shot `ConfigureSecrets(box, env)` keeps `settings.New` unchanged and puts env-over-db-over-default precedence in ONE place for all three readers (GET source-reporting, decrypt accessors, slacksvc token reads). Reviewer ruled it superior to handler-side placement.
- **2026-07-09 — Manual override also sends the Confirm/Not-me DM** (AI decision, defect fix in the minimal design): auto-match skips override users and `SetUserSlackOverride` resets `confirmed_at`, so without an override-triggered confirmation DM an override user could never confirm — a dead end. Anti-squat invariant unchanged (no content before Confirm). Pending reviewer scrutiny (incl. DM-spam-via-override-hammering question).
- **2026-07-09 — mrkdwn injection hardening** (audit finding, fixed 2d4f817): forge/worker-controlled fields (issue title, repo path, failure reason, MR URL, confirm-DM label) are per-field escaped (`EscapeMrkdwn`) before interpolation into DM text; deep-link markup stays raw; `failure_reason` bounded to 500 runes. Escaping is the standing rule for every future outbound Slack surface (M4/M5 gate + reply messages).
- **2026-07-09 — `GET /me/slack` stays pure-DB** (AI decision): shows the resolved member id, no live `users.info` display-name call, so a settings GET never degrades with Slack down. PRD's "display name when resolvable" deferred as a follow-up.
