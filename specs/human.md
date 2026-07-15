# uzi — Human Requirements (Contract)

Requirements and decisions stated by the user. This is the contract: every item
here must hold in any rebuild. Do not edit without user approval.

## Project

- Name: "Uzinele Întunecate" (uzi) — an AI dark factory.

## Inspiration (prior art)

- Three inspiration projects vendored as git submodules under `inspiration/`:
  `bottega`, `multica`, `dot-agent-deck`.
- Before implementing anything, check them for prior art on the same/similar feature.
- Prefer the better implementation; beat them where possible. Best-practice bar.
- Some scope may be deferred to later.

## MVP / infrastructure

- Initial MVP is a local laptop demo via docker-compose.
- PostgreSQL database.
- Persistent storage (data survives restarts).

## Feature #1 — Simple WebUI with user registration

Tracked as GitLab issue vtmocanu/uzi#1; PRD at `prds/1-simple-webui-user-registration.md`.

- Simple web UI with user support and registration.
- Plain user/password registration stored in the DB.
- No email (no OTP, no verification, no reset for now).
- No SSO/OAuth.
- Auth flow: password + revocation.
- Stack: Go API + React/Vite SPA.
- Minimal-shell scope: landing, register/login, protected dashboard, admin user list.

## Feature #2 — Forge integration & label-synced kanban

Tracked as GitLab issue vtmocanu/uzi#2; PRD at `prds/2-forge-integration-kanban.md`.

- Forge-generic design: GitLab first, Forgejo support later.
- Each user creates their own GitLab bot account + PAT, adds it as Developer to the projects they choose.
- uzi only sees issues the bot has rights to — no shared/ambient identity.
- Repo list + picker in the UI.
- Per-repo kanban board, columns = GitLab labels, kept in two-way sync between uzi and GitLab.
  - Reference: example-app board (label-as-column example); kan.bn (UI style).
- Board/agents work only issues carrying the `PRD` label, sanity-checked to contain a link to the PRD file.

## Feature #3 — Agent templates & per-user Anthropic token

Tracked as GitLab issue vtmocanu/uzi#3; PRD at `prds/3-agent-templates-anthropic-tokens.md`.

- Agent templates stored in the DB, editable via the UI (the agents themselves sit with the code).
- Admin-only template writes; all authenticated users can read/preview. [AI-proposed, user-confirmed 2026-07-03]
- Each user stores their own Anthropic OAuth token via the webui, encrypted in the DB.
- A doc explaining how to obtain the token.
- Scope: templates + token storage only. Agent runtime/execution deferred to PRD #4 (no spawning, no file writes, no Anthropic API calls).
- Built in parallel with PRD #2, on a separate worktree/branch.

## Feature #4 — Agent runtime: workers, job queue & live run view

Tracked as GitLab issue vtmocanu/uzi#4; PRD at `prds/4-agent-runtime-workers.md`.

- Implement the agent-runtime/workers PRD: act on a PRD card — run an agent, watch it live, correct it, land an MR.
- Agent execution uses the Claude Agent SDK. NOT agent-deck's real-Claude-Code/PTY approach (deferred, maybe later).
- Create a PRD issue in GitLab from uzi web and work on it from uzi web (no CLI after worker setup).
- PRIMARY DIRECTIVE: agents may only ever create MRs; never write to main (or mutate other resources). Verify this holds.
- Model credential: OAuth subscription tokens only (`claude setup-token`); no Anthropic API keys available to the team.
- Testing-credentials policy: never mint an OAuth token for tests/CI/dev; tests use dummy creds + stub executor; optional live validation only with the user's EXISTING token, provided at that moment, removed after — live tests must never be REQUIRED to prove a milestone.
- Live run visibility: which agents are live/idle (+ best-effort "next"), the messages, plan-approval gate, stop, follow-up corrections; admins see all agents/runs, users see their own.
- Each user runs their own worker (container), connected outbound to the server; worker↔server link should be encrypted. (MVP: join-token auth now; transport TLS deferred to remote-worker/ingress — deferral accepted by user 2026-07-04.)

## Feature #5 — Access control: registration restrictions & PAT least-privilege

Tracked as GitLab issue vtmocanu/uzi#5; PRD at `prds/5-access-control-pat-hardening.md`.

- Registration restricted to a configurable email-domain allowlist (plan.md: "allow registration only from @example.com - configurable"; empty = all domains).
- Registration can be enabled/disabled (kill-switch).
- uzi verifies the bot PAT has no more permissions than needed for MRs, per repo, at save time and periodically afterwards (plan.md line 48).
- Serves the primary directive: agents must not be able to modify main.
- Scope (option A) chosen to run parallel to PRD #4.

## Feature #6 — CI status integration & CI-fix agent

Tracked as GitLab issue vtmocanu/uzi#6; PRD at `prds/6-ci-status-integration.md`.

- Check and display GitLab CI/pipeline status in uzi (repo view, board header, per-card).
- When CI is broken, spin up an agent to review what happened and fix it if it can.
- If the code was bad, uzi verifies its own fix (the fix's pipeline must pass).
- Source: plan.md line 52.

## uzi's own CI (dummy, kept)

- uzi's repo ships a minimal dummy `.gitlab-ci.yml`, merged to main and kept (not throwaway), so the CI feature is demonstrable against real pipelines. [user, 2026-07-06]

## Feature #7 — In-app docs section

Tracked as GitLab issue vtmocanu/uzi#7; PRD at `prds/7-docs-section-webui.md`.

- A docs section on uzi with relevant howtos: how to create an agent/bot, how to do GitLab bots / give permissions, etc.
- Include screenshots; screenshots are provided by the user (ask the user for them).
- Skills howto now in scope and shipped (`docs/skills.md`) — Feature #16 added the skills feature.

## Feature #28 — Docs search

Tracked as GitLab issue vtmocanu/uzi#28; PRD at `prds/28-docs-search.md`.

- A search box for the docs page.
- Full-text search with snippets (not a title/summary filter).
- Search box on the `/docs` index only (not on individual doc pages).

## Feature #11 — Run view UX: markdown plan, boxed activity, terse events

Tracked as GitLab issue vtmocanu/uzi#11; PRD at `prds/11-run-view-ux.md`.

- Plan (and agent prose) renders as formatted markdown, not raw `#`/`-`/backticks.
- Activity feed is a bounded, scrollable box that auto-scrolls (follows) live runs; scrolling up pauses following without fighting the user; a "{n} new ↓" affordance resumes it in one click.
- No raw JSON anywhere in the run view, for any event kind.
- Long tool calls visibly show as running (spinner + elapsed) — no frozen-feed dead air.
- Raw run frames go to the worker's docker logs behind `UZI_LOG_LEVEL=debug`, not the browser.
- No "show raw JSON" toggle in the web UI. [explicit user rejection]
- Reuse PRD #7's markdown renderer rather than building a parallel one. [user coordination]

## Feature #38 — Activity feed redesign

Tracked as GitLab issue vtmocanu/uzi#38; PRD at `prds/38-activity-feed-redesign.md`.

- The run Activity feed is polished/redesigned per the approved mock.
- Bash commands render as highlighted code, not plain text; no command content is lost in the UI.
- Agent output is collapsible per agent (collapse lead/worker output).
- The approved mock is the design contract; the implemented UI must match it. Mock committed at `prds/mockups/38-activity-feed-mock.html`.

## Feature #12 — Board–run lifecycle integration

Tracked as GitLab issue vtmocanu/uzi#12; PRD at `prds/12-board-run-lifecycle.md`.

- Issues move board columns automatically with the run lifecycle: In Progress while agents work, a review column once the MR is open. Hand-dragging is the defect being fixed.
- The board shows that runs happened/are happening: run badges + MR link on the card; the board updates itself (no manual Refresh).
- Clicking an issue stays in-platform: an in-app issue view with its runs and a start-run action. GitLab remains reachable via an explicit icon.
- Failed forge column moves are retried via reconciliation — not dropped, not a persisted move queue. (2026-07-04 user review; the "no retry, next lifecycle event heals it" stance was rejected because `completed` is terminal.)

## Feature #14 — UI redesign: multica-inspired "ember" theme

Tracked as GitLab issue vtmocanu/uzi#14; PRD at `prds/done/14-multica-ui-redesign.md`.

- Three UI redesign prototypes built and evaluated live in-browser; the multica-inspired "ember" design was selected as the real UI.
- Board nav entries carry the GitLab logo; fall back to a generic git icon when no GitLab icon applies.
- Forge sits only under Settings — no standalone Forge nav item.
- Run status colors unified across all surfaces (board card, runs list, run view, issue history) — extends PRD #12's run badge tones.
- A themed focus-visible ring (keyboard/AT focus indicator).
- E2E testing includes a real-GitLab leg, scoped to the dedicated scratch project `vtmocanu/uzi-e2e-scratch` (created for this); the live-Anthropic capstone is skipped.

## Feature #16 — Agent skills (global / user / builtin / repo)

Tracked as GitLab issue vtmocanu/uzi#16; PRD at `prds/16-agent-skills.md`.

- Skills exist at global scope and per-user scope (plan.md line 44).
- Users allocate global skills or their own skills to each agent.
- First builtin skill: `ci-cd-norms`, researched from internal-kb and the example-app repos — the example CI/CD norm, with example-app as the worked exception.
- Repos may carry skills the worker detects. Per-repo opt-in, default off. [capability: user; opt-in/default-off shape AI-proposed, user-accepted]
- Builtin skills ship with uzi; editable and resettable like builtin agent templates.

## Feature #17 — Builtin lead template (opus) + worker model selection

Tracked as GitLab issue vtmocanu/uzi#17; PRD at `prds/17-lead-template-and-model-selection.md`.

- Ship `lead` as a builtin agent template with a real orchestrator prompt, on model `opus`; editable and resettable in the UI like the other builtins.
- Builtin templates are the single source of truth; `.claude/agents/` is the dev team's own roster only. Decouples the former 1:1 mirror. [user, 2026-07-05, supersedes the earlier "both dirs" choice]
- Per-user default worker model: each user can pick the model for their own runs (Settings), stored per user.
- Model choices offered as curated aliases plus a custom free-text escape hatch for any model ID.
- Precedence: a user's default model wins over the lead template's model (unset = inherit the lead template's model, opus by default).
- Sequence this PRD before PRD #16 so #16 inherits the decoupled-builtins convention.

## Feature #18 — Worker templates, per-repo tools & agent scopes

Tracked as GitLab issue vtmocanu/uzi#18; PRD at `prds/18-worker-templates-and-agent-scopes.md`.

- Curated worker image templates in git so different workers can carry different heavy toolchains (e.g. node tools vs java tools); the user picks one per worker.
- Per-repo CLI tools installed on demand (so "command not found" stops being a dead end), from a user tool profile bounded by an admin allowlist, plus an opt-in for a repo's own devbox.json packages.
- Agent templates gain global and per-user (private) scopes with per-user allocation, so a user can define a private agent and choose which agents ride their runs; admins manage the shared defaults.

## Feature #19 — Admin settings & autopilot label

Tracked as GitLab issue vtmocanu/uzi#19; PRD at `prds/19-admin-settings-and-autopilot.md`.

- Generic admin-only instance-settings infrastructure; the PRD label and the autopilot label are its first two configurable keys.
- Admins can change the PRD label and the autopilot label; the board reflects the new label set after a resync (no code fork).
- Autopilot: adding the autopilot label (alongside the PRD label) to an issue in GitLab runs it end to end with zero uzi interaction.
- Progress is visible via the existing board label moves; the user need never open uzi.
- Outcome returns as one GitLab issue comment: MR link on success; on failure, one comment with a run link.
- Consent is per-user opt-in, default off — a third party must never be able to spend your Anthropic tokens without your opt-in.
- Each user self-declares their own forge (human) username on their connection (the mapping autopilot attributes runs to).
- Attribution order: label adder first, issue author fallback.

## Feature #21 — Mission-control theme (second selectable theme)

Tracked as GitLab issue vtmocanu/uzi#21; PRD at `prds/21-mission-control-theme.md`.

- Mission-control theme (one of three evaluated prototypes) must be selectable in the product.
- Prototype branches preserved on origin: `prototype/mission-control` and `prototype/minimal`.
- Theme preference is server-side with an admin-set instance default. Resolution: user override > admin default > ember. [user 2026-07-05, supersedes a device-local draft]
- Mission's six-tone status language, incl. the violet queue tone, added theme-agnostically. [user 2026-07-05]
- Theme settings tenant into PRD #19's `app_settings` — no parallel settings table. [user-approved 2026-07-05, "update 21, 19 is in flight"]
- Mock demo persists settings across reload (settings-only). [user approved 2026-07-05]

## Feature #22 — PRDLESS label: run an issue without a PRD link

Tracked as GitLab issue vtmocanu/uzi#22; PRD at `prds/22-prdless-label.md`.

- An escape-hatch label lets an issue run without a `prds/*.md` link.
- Label name is configurable; the feature can be toggled on/off — both in admin settings.
- Enabled by default (available out of the box).
- Default label name: `PRDLESS`.
- The label can be added/removed directly from the uzi web UI, not only in GitLab.

## Feature #23 — Web UX polish: live dashboard, collapsible sidebar, hide empty board columns

Tracked as GitLab issue vtmocanu/uzi#23; PRD at `prds/23-web-ux-live-dashboard-sidebar-board.md`.

- Dashboard updates live: a run reaching `awaiting_approval` must show without a manual refresh.
- Desktop sidebar is collapsible.
- Empty board columns can be hidden.
- Web-only; no API/schema/agent changes.
- "Board columns should auto refresh" — already satisfied by existing polling; no change shipped.

## Feature #24 — MR closed without merging → card back to In Progress

Tracked as GitLab issue vtmocanu/uzi#24; PRD at `prds/done/24-mr-close-rework.md`.

- When a reviewer closes an agent's MR without merging, move the board card back from Human Review to In Progress (the "rework needed" signal).
- Target column is In Progress — user's explicit choice, over Open/backlog.

## Feature #25 — Slack integration: run notifications, approve from Slack, reply-from-Slack

Tracked as GitLab issue vtmocanu/uzi#25; PRD at `prds/25-slack-integration.md`.

- Per-user Slack DMs for run state (started, awaiting approval, completed + MR link, failed, cancelled).
- Approve/reject the plan-approval gate from Slack: buttons + threaded reject reason.
- Reply from Slack to steer a live run (thread reply becomes a follow-up correction).
- Socket Mode only — outbound-only; no inbound HTTP, no public URL. [user 2026-07-06]
- User mapping: email auto-match + manual Slack member-ID override. [user 2026-07-06]
- Per-user notifications toggle, default ON; default-ON initiates a link-confirmation DM, run content flows only after Confirm. [user 2026-07-06, amended by security review]
- Slack tokens configurable from ENV or the admin webui; sealed at rest, never echoed back; ENV wins.

## Feature #32 — Per-user vault: password-wrapped secrets

Tracked as GitLab issue vtmocanu/uzi#32; PRD at `prds/32-user-vault-password-wrapped-secrets.md`.

- Threat: a k8s operator can read env/Infisical/etcd (master key) plus the DB and decrypt every user's Anthropic token. etcd encryption at rest is not an option. [user-stated threat model]
- Goal is to make token theft materially harder, not impossible — no decryption key at rest anywhere an operator can read. [user accepts residual risks: memory dump, trojaned image]
- Each user's secrets are encrypted with a key derived from their own login password (vault); the server stores only the wrapped key.
- Vault unlocks automatically at login and the key is kept in server memory until pod restart or an explicit "Lock vault" action — no per-session re-entry, so overnight/autopilot runs keep working while unlocked. [user choice over session-TTL caching]
- UI shows an unlocked/locked vault status; when locked (e.g. after a deploy), runs queue as "waiting for vault unlock" and a password prompt unlocks without full re-login.
- Forgotten password ⇒ vault contents unrecoverable by design; user re-enters tokens.

## Feature #37 — Per-run agent selection: repo `.claude/agents` detection with plan-gate choice

Tracked as GitLab issue vtmocanu/uzi#37; PRD at `prds/37-run-agent-selection.md`.

- At the plan-approval gate, the user chooses which agents the run uses: the repo's own agents (detected from `.claude/agents/`) or the user's uzi templates. [user 2026-07-10]
- Show whether repo agents were detected and which ones. [user 2026-07-10]
- Default to the detected repo agents; if the user does not want them, they can choose their own templates instead. [user 2026-07-10]
- Repo agents run with the tools and model their files declare (honored as they would be under Claude Code), still subject to uzi's guardrails. [user 2026-07-10; a review-round proposal to deny WebFetch/WebSearch and clamp the model to aliases was rejected by the user the same day — `Agent`/nested-spawn and the async-deferral tools stay denied]
- Either/or source with per-agent exclusions; no mixing the two sources in one run. [user 2026-07-10]
- Autopilot runs apply the default automatically (repo agents if detected, else the user's templates) and record which roster they used, with no human interaction. [user 2026-07-10]
- The Slack plan-approval gate offers the same source choice (two Approve buttons: repo agents / my templates); excluding individual agents is done in the web UI. [user 2026-07-10]
- The shipped picker is validated visually against the approved mock (`prds/mockups/37-agent-picker-mock.html`). [user 2026-07-10]

## Feature #40 — Token usage & cost reporting (per run / per user / factory-wide)

Tracked as GitLab issue vtmocanu/uzi#40; PRD at `prds/40-token-usage-reporting.md`.

- Report token usage and cost per run, per user, and factory-wide. [user]
- Run view shows the run's usage, broken down per phase and per agent ("coder used 800k"). [user 2026-07-12]
- Every user sees their own "Your usage" (lifetime + last-7-days); the factory total and per-user breakdown are admin-only. [user]
- Tokens are the headline figure; cost is a secondary estimate — a $0 cost (subscription-auth runs) renders as "—", not "$0.00". [user]
- Failed and cancelled runs still count their spend. [user]
- Chat runs are out of scope (not counted). [user]
- Shipped surfaces validated against the approved mock (+ addendum). [user 2026-07-12]

## Feature — Run retrospective (LLM judge) & self-improvement job

Tracked as GitLab issue vtmocanu/uzi#46; PRD at `prds/46-run-judge-self-improvement.md`
(supersedes plan.md:64/69/91).

- **Run retrospective (LLM judge)**: after a run finishes, an LLM reviews the run
  trace — agents, tools, prompts, plan quality, review cycles, how the run
  progressed and delivered — and produces a verdict + recommendations. v1 judges
  finished runs only, automatically when enabled; mid-run judging deferred. Judge
  model configurable, cheap default. [user 2026-07-12]
- Verdicts: "all good/ideal", or concrete suggestions — enable an existing
  tool/skill for an agent, install a missing tool on the worker, adjust an agent
  template/prompt, improve existing agents (including repo agents living in git)
  or propose missing agents that should be added to a repo, or change uzi itself
  (recommendation only, never code). [user 2026-07-12]
- The judge runs on the run owner's own Anthropic token, never a shared one.
  [user 2026-07-12]
- Per-user opt-in/out; admin can toggle the feature globally and force-disable
  per user (existing admin settings). [user 2026-07-12]
- Recommendations land in an inbox/notifications surface — users see their own,
  admins see all; also visible on the run page — and go out via the existing
  Slack notifications too. [user 2026-07-12]
- The deterministic "command not found" scan feeds the judge as an input signal.
  [user 2026-07-12; plan.md:64]
- **Self-improvement scheduled job (admin-only)**: configurable interval (2-day
  default), admin-toggled; reviews uzi's own codebase plus accumulated judge
  recommendations and picks **one top thing** per iteration — bug, feature, or
  whole refactor — so uzi iterates and self-improves. [user 2026-07-12]
- The job spins up an agent team that implements the pick and creates an MR
  (normal guardrails: never main, MR only). It runs autonomously — no approval
  gate blocks it — but the plan it worked from must be inspectable. If a
  self-improvement MR is already open, it reuses/extends that MR so everything
  is tested together. [user 2026-07-12]
- One PRD covers both, phased: judge first, job second (shared settings/inbox
  plumbing). [user 2026-07-12]
- Token for the job: each admin can enable the job using their own token; the
  design also accommodates a general/instance token for later, when/if one is
  implemented (plan.md:69). [user 2026-07-12]
- Judge recommends; only the job acts. Judge never auto-creates MRs.
  [user 2026-07-12]

## Feature #45 — OIDC SSO login (Keycloak / Pocket ID)

Tracked as GitLab issue vtmocanu/uzi#45; PRD at `prds/done/45-oidc-sso-login.md`.

- SSO login against a single external OIDC provider — Keycloak (work) and Pocket ID (homelab) are the two supported targets. [user 2026-07-12]
- One provider, env-configured (not multi-provider, no in-app provider config). [user 2026-07-12]
- Coexists with email+password login; a `UZI_PASSWORD_LOGIN_ENABLED` kill-switch lets operators go SSO-only. [user 2026-07-12]
- JIT provisioning: first SSO login auto-creates the user; an existing account is linked by verified email. [user 2026-07-12]
- Admin stays uzi-managed (existing first-user-is-admin rule); no groups/roles-claim mapping this iteration. [user 2026-07-12]
- OIDC-created users have no password, so they set a dedicated vault passphrase for the PRD #32 vault. [user 2026-07-12]
- Operator docs include step-by-step walkthroughs for BOTH Keycloak and Pocket ID. [user 2026-07-12]
- Supersedes the earlier "SSO with Keycloak" deferral in the Deferred list. [user 2026-07-12]

## Feature #47 — Loop/hang detection

Tracked as GitLab issue vtmocanu/uzi#47; PRD at `prds/47-loop-hang-detection.md`.

- Detect runs that are taking too long or seem stuck, and flag them. (plan.md line 68)
- Flags surface in the web UI and on Slack.
- Flags are non-terminal and self-clearing: a flag never kills, requeues, or times out a
  run — early-warning only; existing watchdogs keep the kill job.
  [AI-proposed surface, user-ratified via PRD approval 2026-07-12]

## Feature #49 — Worker resource stats (live per-worker CPU/memory)

Tracked as GitLab issue vtmocanu/uzi#49; PRD at `prds/49-worker-resource-stats.md`.

- Live per-worker CPU and memory visibility in the uzi web UI ("worker resource stats"). [user 2026-07-14]
- Per-worker granularity is sufficient; no per-run attribution. [user-confirmed]
- Must work the same under docker-compose today and k8s later (portability). [user]

## Feature #53 — Per-user Claude rate-limit visibility

Tracked as GitLab issue vtmocanu/uzi#53; PRD at `prds/53-rate-limits.md`.

- Show each user's Anthropic account rate-limit headroom (5-hour and 7-day windows) in the uzi web UI.
- Users see their own meters; admins see every user's, on one page (mirrors the PRD #40 usage split).
- Server polls with the user's own token; the token never leaves the api container — SPA sees only percentages.
- The header-probe fallback spends ~1 token/interval of the user's own quota; operators can disable the probe (`UZI_USAGE_PROBE=false`) or the whole poller (`UZI_USAGE_POLL_INTERVAL=0`).
- No Anthropic token ever appears in a log line, API response, or the SPA.

## Startup admin seed

- Seed an admin user from env at startup (`UZI_SEED_EMAIL` / `UZI_SEED_PASSWORD` / `UZI_SEED_NAME`) so the user survives DB wipes.
- Seeded user gets the admin role; never overwrite an existing user.

## Deferred (user, "later stuff")

- On-demand worker spawning: on compose the worker simply runs always-on (idle is
  cheap); server-provisioned per-user workers are deferred to the k8s/remote-worker
  phase, where a dedicated operator (never the api, which must never hold
  container-runtime credentials like `docker.sock`) spawns worker pods when queued
  work appears — e.g. a future in-app chat agent, whose users chat in the web UI
  without knowing a worker serves them. [user, 2026-07-10; design detail in
  specs/ai.md §168]
- Auto-creating bot accounts / bot role enforcement (forge ships with user-managed bots).
- Forgejo driver (interface is forge-generic; GitLab implemented first).
- Agent runtime/execution (spawn, file writes, Anthropic API calls) — PRD #4.
