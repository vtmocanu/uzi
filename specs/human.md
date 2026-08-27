# uzi — Human Requirements (Contract)

Requirements and decisions stated by the user. This is the contract: every item
here must hold in any rebuild. Do not edit without user approval.

## Project

- Name: "Uzinele Întunecate" (uzi) — an AI dark factory.

## Inspiration (prior art)

- Two inspiration projects: `bottega`, `dot-agent-deck`.
  They were vendored as git submodules under `inspiration/` from the start of
  the project until **2026-08-03**, when the user had them removed from the
  tree and kept as ordinary clones outside the repo, symlinked back into a
  gitignored `inspiration/` for local work. The prior-art requirement below is
  unchanged by that; only where the code lives changed.
- Before implementing anything, check them for prior art on the same/similar feature.
- Prefer the better implementation; beat them where possible. Best-practice bar.
- Some scope may be deferred to later.

## MVP / infrastructure

- Initial MVP is a local laptop demo via docker-compose.
- PostgreSQL database.
- Persistent storage (data survives restarts).

## Feature #1 — Simple WebUI with user registration

Tracked as GitLab issue vtmocanu/uzi#1; PRD at `prds/done/1-simple-webui-user-registration.md`.

- Simple web UI with user support and registration.
- Plain user/password registration stored in the DB.
- No email (no OTP, no verification, no reset for now).
- No SSO/OAuth.
- Auth flow: password + revocation.
- Stack: Go API + React/Vite SPA.
- Minimal-shell scope: landing, register/login, protected dashboard, admin user list.

## Feature #2 — Forge integration & label-synced kanban

Tracked as GitLab issue vtmocanu/uzi#2; PRD at `prds/done/2-forge-integration-kanban.md`.

- Forge-generic design: GitLab and Forgejo both supported at full parity (Forgejo added by PRD #65, 2026-07-17).
- Each user creates their own GitLab bot account + PAT, adds it as Developer to the projects they choose.
- uzi only sees issues the bot has rights to — no shared/ambient identity.
- Repo list + picker in the UI.
- Per-repo kanban board, columns = GitLab labels, kept in two-way sync between uzi and GitLab.
  - Reference: an internal board (label-as-column example); kan.bn (UI style).
- Board/agents work only issues carrying the `PRD` label, sanity-checked to contain a link to the PRD file.

## Feature #3 — Agent templates & per-user Anthropic token

Tracked as GitLab issue vtmocanu/uzi#3; PRD at `prds/done/3-agent-templates-anthropic-tokens.md`.

- Agent templates stored in the DB, editable via the UI (the agents themselves sit with the code).
- Admin-only template writes; all authenticated users can read/preview. [AI-proposed, user-confirmed 2026-07-03]
- Each user stores their own Anthropic OAuth token via the webui, encrypted in the DB.
- A doc explaining how to obtain the token.
- Scope: templates + token storage only. Agent runtime/execution deferred to PRD #4 (no spawning, no file writes, no Anthropic API calls).
- Built in parallel with PRD #2, on a separate worktree/branch.

## Feature #4 — Agent runtime: workers, job queue & live run view

Tracked as GitLab issue vtmocanu/uzi#4; PRD at `prds/done/4-agent-runtime-workers.md`.

- Implement the agent-runtime/workers PRD: act on a PRD card — run an agent, watch it live, correct it, land an MR.
- Agent execution uses the Claude Agent SDK. NOT agent-deck's real-Claude-Code/PTY approach (deferred, maybe later).
- Create a PRD issue in GitLab from uzi web and work on it from uzi web (no CLI after worker setup).
- PRIMARY DIRECTIVE: agents may only ever create MRs; never write to main (or mutate other resources). Verify this holds.
- Model credential: OAuth subscription tokens only (`claude setup-token`); no Anthropic API keys available to the team.
- Testing-credentials policy: never mint an OAuth token for tests/CI/dev; tests use dummy creds + stub executor; optional live validation only with the user's EXISTING token, provided at that moment, removed after — live tests must never be REQUIRED to prove a milestone.
- Live run visibility: which agents are live/idle (+ best-effort "next"), the messages, plan-approval gate, stop, follow-up corrections; admins see all agents/runs, users see their own.
- Each user runs their own worker (container), connected outbound to the server; worker↔server link should be encrypted. (MVP: join-token auth now; transport TLS deferred to remote-worker/ingress — deferral accepted by user 2026-07-04.)

## Feature #5 — Access control: registration restrictions & PAT least-privilege

Tracked as GitLab issue vtmocanu/uzi#5; PRD at `prds/done/5-access-control-pat-hardening.md`.

- Registration restricted to a configurable email-domain allowlist (plan.md: "allow registration only from @example.com - configurable"; empty = all domains).
- Registration can be enabled/disabled (kill-switch).
- uzi verifies the bot PAT has no more permissions than needed for MRs, per repo, at save time and periodically afterwards (plan.md line 48).
- Serves the primary directive: agents must not be able to modify main.
- Scope (option A) chosen to run parallel to PRD #4.

## Feature #6 — CI status integration & CI-fix agent

Tracked as GitLab issue vtmocanu/uzi#6; PRD at `prds/done/6-ci-status-integration.md`.

- Check and display GitLab CI/pipeline status in uzi (repo view, board header, per-card).
- When CI is broken, spin up an agent to review what happened and fix it if it can.
- If the code was bad, uzi verifies its own fix (the fix's pipeline must pass).
- Source: plan.md line 52.

## uzi CI + GitLab demo / self-host example

- uzi's ci-autofix feature is demonstrable against a real GitLab pipeline via the `ai-team/uzi` GitLab mirror; the repo ships a minimal example `examples/gitlab/.gitlab-ci.yml` as a self-hosting starting point. (The repo's own CI is GitHub Actions since 2026-08-18, so a root `.gitlab-ci.yml` would be inert.) [user, 2026-07-06; updated 2026-08-26]

## Feature #7 — In-app docs section

Tracked as GitLab issue vtmocanu/uzi#7; PRD at `prds/done/7-docs-section-webui.md`.

- A docs section on uzi with relevant howtos: how to create an agent/bot, how to do GitLab bots / give permissions, etc.
- Include screenshots; screenshots are provided by the user (ask the user for them).
- Skills howto now in scope and shipped (`docs/skills.md`) — Feature #16 added the skills feature.

## Feature #28 — Docs search

Tracked as GitLab issue vtmocanu/uzi#28; PRD at `prds/done/28-docs-search.md`.

- A search box for the docs page.
- Full-text search with snippets (not a title/summary filter).
- Search box on the `/docs` index only (not on individual doc pages).

## Feature #11 — Run view UX: markdown plan, boxed activity, terse events

Tracked as GitLab issue vtmocanu/uzi#11; PRD at `prds/done/11-run-view-ux.md`.

- Plan (and agent prose) renders as formatted markdown, not raw `#`/`-`/backticks.
- Activity feed is a bounded, scrollable box that auto-scrolls (follows) live runs; scrolling up pauses following without fighting the user; a "{n} new ↓" affordance resumes it in one click.
- No raw JSON anywhere in the run view, for any event kind.
- Long tool calls visibly show as running (spinner + elapsed) — no frozen-feed dead air.
- Raw run frames go to the worker's docker logs behind `UZI_LOG_LEVEL=debug`, not the browser.
- No "show raw JSON" toggle in the web UI. [explicit user rejection]
- Reuse PRD #7's markdown renderer rather than building a parallel one. [user coordination]

## Feature #38 — Activity feed redesign

Tracked as GitLab issue vtmocanu/uzi#38; PRD at `prds/done/38-activity-feed-redesign.md`.

- The run Activity feed is polished/redesigned per the approved mock.
- Bash commands render as highlighted code, not plain text; no command content is lost in the UI.
- Agent output is collapsible per agent (collapse lead/worker output).
- The approved mock is the design contract; the implemented UI must match it. Mock committed at `prds/mockups/38-activity-feed-mock.html`.

## Feature #12 — Board–run lifecycle integration

Tracked as GitLab issue vtmocanu/uzi#12; PRD at `prds/done/12-board-run-lifecycle.md`.

- Issues move board columns automatically with the run lifecycle: In Progress while agents work, a review column once the MR is open. Hand-dragging is the defect being fixed.
- The board shows that runs happened/are happening: run badges + MR link on the card; the board updates itself (no manual Refresh).
- Clicking an issue stays in-platform: an in-app issue view with its runs and a start-run action. GitLab remains reachable via an explicit icon.
- Failed forge column moves are retried via reconciliation — not dropped, not a persisted move queue. (2026-07-04 user review; the "no retry, next lifecycle event heals it" stance was rejected because `completed` is terminal.)

## Feature #14 — UI redesign: "ember" theme

Tracked as GitLab issue vtmocanu/uzi#14; PRD at `prds/done/14-ember-ui-redesign.md`.

- Three UI redesign prototypes built and evaluated live in-browser; the "ember" design was selected as the real UI.
- Board nav entries carry the GitLab logo; fall back to a generic git icon when no GitLab icon applies.
- Forge sits only under Settings — no standalone Forge nav item.
- Run status colors unified across all surfaces (board card, runs list, run view, issue history) — extends PRD #12's run badge tones.
- A themed focus-visible ring (keyboard/AT focus indicator).
- E2E testing includes a real-GitLab leg, scoped to the dedicated scratch project `vtmocanu/uzi-e2e-scratch` (created for this); the live-Anthropic capstone is skipped.

## Feature #16 — Agent skills (global / user / builtin / repo)

Tracked as GitLab issue vtmocanu/uzi#16; PRD at `prds/done/16-agent-skills.md`.

- Skills exist at global scope and per-user scope (plan.md line 44).
- Users allocate global skills or their own skills to each agent.
- First builtin skill: `ci-cd-norms`, researched from an internal knowledge base and reference repos — an organization's CI/CD norm, with a reference app as the worked exception.
- Repos may carry skills the worker detects. Per-repo opt-in, default off. [capability: user; opt-in/default-off shape AI-proposed, user-accepted]
- Builtin skills ship with uzi; editable and resettable like builtin agent templates.

## Feature #17 — Builtin lead template (opus) + worker model selection

Tracked as GitLab issue vtmocanu/uzi#17; PRD at `prds/done/17-lead-template-and-model-selection.md`.

- Ship `lead` as a builtin agent template with a real orchestrator prompt, on model `opus`; editable and resettable in the UI like the other builtins.
- Builtin templates are the single source of truth; `.claude/agents/` is the dev team's own roster only. Decouples the former 1:1 mirror. [user, 2026-07-05, supersedes the earlier "both dirs" choice]
- Per-user default worker model: each user can pick the model for their own runs (Settings), stored per user.
- Model choices offered as curated aliases plus a custom free-text escape hatch for any model ID.
- Precedence: a user's default model wins over the lead template's model (unset = inherit the lead template's model, opus by default).
- Sequence this PRD before PRD #16 so #16 inherits the decoupled-builtins convention.

## Feature #18 — Worker templates, per-repo tools & agent scopes

Tracked as GitLab issue vtmocanu/uzi#18; PRD at `prds/done/18-worker-templates-and-agent-scopes.md`.

- Curated worker image templates in git so different workers can carry different heavy toolchains (e.g. node tools vs java tools); the user picks one per worker.
- Per-repo CLI tools installed on demand (so "command not found" stops being a dead end), from a user tool profile bounded by an admin allowlist, plus an opt-in for a repo's own devbox.json packages.
- Agent templates gain global and per-user (private) scopes with per-user allocation, so a user can define a private agent and choose which agents ride their runs; admins manage the shared defaults.

## Feature #19 — Admin settings & autopilot label

Tracked as GitLab issue vtmocanu/uzi#19; PRD at `prds/done/19-admin-settings-and-autopilot.md`.

- Generic admin-only instance-settings infrastructure; the PRD label and the autopilot label are its first two configurable keys.
- Admins can change the PRD label and the autopilot label; the board reflects the new label set after a resync (no code fork).
- Autopilot: adding the autopilot label (alongside the PRD label) to an issue in GitLab runs it end to end with zero uzi interaction.
- Progress is visible via the existing board label moves; the user need never open uzi.
- Outcome returns as one GitLab issue comment: MR link on success; on failure, one comment with a run link.
- Consent is per-user opt-in, default off — a third party must never be able to spend your Anthropic tokens without your opt-in.
- Each user self-declares their own forge (human) username on their connection (the mapping autopilot attributes runs to).
- Attribution order: label adder first, issue author fallback.

## Feature #21 — Mission-control theme (second selectable theme)

Tracked as GitLab issue vtmocanu/uzi#21; PRD at `prds/done/21-mission-control-theme.md`.

- Mission-control theme (one of three evaluated prototypes) must be selectable in the product.
- Prototype branches preserved on origin: `prototype/mission-control` and `prototype/minimal`.
- Theme preference is server-side with an admin-set instance default. Resolution: user override > admin default > ember. [user 2026-07-05, supersedes a device-local draft]
- Mission's six-tone status language, incl. the violet queue tone, added theme-agnostically. [user 2026-07-05]
- Theme settings tenant into PRD #19's `app_settings` — no parallel settings table. [user-approved 2026-07-05, "update 21, 19 is in flight"]
- Mock demo persists settings across reload (settings-only). [user approved 2026-07-05]

## Feature #22 — PRDLESS label: run an issue without a PRD link

Tracked as GitLab issue vtmocanu/uzi#22; PRD at `prds/done/22-prdless-label.md`.

- An escape-hatch label lets an issue run without a `prds/*.md` link.
- Label name is configurable; the feature can be toggled on/off — both in admin settings.
- Enabled by default (available out of the box).
- Default label name: `PRDLESS`.
- The label can be added/removed directly from the uzi web UI, not only in GitLab.

## Feature #23 — Web UX polish: live dashboard, collapsible sidebar, hide empty board columns

Tracked as GitLab issue vtmocanu/uzi#23; PRD at `prds/done/23-web-ux-live-dashboard-sidebar-board.md`.

- Dashboard updates live: a run reaching `awaiting_approval` must show without a manual refresh.
- Desktop sidebar is collapsible.
  - The collapse control must not consume a full sidebar row. [user 2026-08-14]
- Empty board columns can be hidden.
- Web-only; no API/schema/agent changes.
- "Board columns should auto refresh" — already satisfied by existing polling; no change shipped.

## Feature — Runs page IA (live console + past-runs archive)

Web-only; no API/schema/agent changes. [user 2026-08-14]

- Past runs live on a separate tab from active runs.
- Past runs are searchable: by title, repo, issue number (#iid), worker, and status.
- Past runs are grouped by date: by day within the current week, by week within the current month, by month beyond.
- Past runs reveal progressively ("show next 50"), like the board.
- Admin Factory status shows only other users' runs — the admin's own runs are not repeated there.
- Alongside (same batch): the Schedules "Last fire" caret must render correctly.

## Feature #24 — MR closed without merging → card back to In Progress

Tracked as GitLab issue vtmocanu/uzi#24; PRD at `prds/done/24-mr-close-rework.md`.

- When a reviewer closes an agent's MR without merging, move the board card back from Human Review to In Progress (the "rework needed" signal).
- Target column is In Progress — user's explicit choice, over Open/backlog.

## Feature #25 — Slack integration: run notifications, approve from Slack, reply-from-Slack

Tracked as GitLab issue vtmocanu/uzi#25; PRD at `prds/done/25-slack-integration.md`.

- Per-user Slack DMs for run state (started, awaiting approval, completed + MR link, failed, cancelled).
- Approve/reject the plan-approval gate from Slack: buttons + threaded reject reason.
- Reply from Slack to steer a live run (thread reply becomes a follow-up correction).
- Socket Mode only — outbound-only; no inbound HTTP, no public URL. [user 2026-07-06]
- User mapping: email auto-match + manual Slack member-ID override. [user 2026-07-06]
- Per-user notifications toggle, default ON; default-ON initiates a link-confirmation DM, run content flows only after Confirm. [user 2026-07-06, amended by security review]
- Slack tokens configurable from ENV or the admin webui; sealed at rest, never echoed back; ENV wins.

## Feature #32 — Per-user vault: password-wrapped secrets

Tracked as GitLab issue vtmocanu/uzi#32; PRD at `prds/done/32-user-vault-password-wrapped-secrets.md`.

- Threat: a k8s operator can read env/Infisical/etcd (master key) plus the DB and decrypt every user's Anthropic token. etcd encryption at rest is not an option. [user-stated threat model]
- Goal is to make token theft materially harder, not impossible — no decryption key at rest anywhere an operator can read. [user accepts residual risks: memory dump, trojaned image]
- Each user's secrets are encrypted with a key derived from their own login password (vault); the server stores only the wrapped key.
- Vault unlocks automatically at login and the key is kept in server memory until pod restart or an explicit "Lock vault" action — no per-session re-entry, so overnight/autopilot runs keep working while unlocked. [user choice over session-TTL caching]
- UI shows an unlocked/locked vault status; when locked (e.g. after a deploy), runs queue as "waiting for vault unlock" and a password prompt unlocks without full re-login.
- Forgotten password ⇒ vault contents unrecoverable by design; user re-enters tokens.

## Feature #37 — Per-run agent selection: repo `.claude/agents` detection with plan-gate choice

Tracked as GitLab issue vtmocanu/uzi#37; PRD at `prds/done/37-run-agent-selection.md`.

- At the plan-approval gate, the user chooses which agents the run uses: the repo's own agents (detected from `.claude/agents/`) or the user's uzi templates. [user 2026-07-10]
- Show whether repo agents were detected and which ones. [user 2026-07-10]
- Default to the detected repo agents; if the user does not want them, they can choose their own templates instead. [user 2026-07-10]
- Repo agents run with the tools and model their files declare (honored as they would be under Claude Code), still subject to uzi's guardrails. [user 2026-07-10; a review-round proposal to deny WebFetch/WebSearch and clamp the model to aliases was rejected by the user the same day — `Agent`/nested-spawn and the async-deferral tools stay denied]
- Either/or source with per-agent exclusions; no mixing the two sources in one run. [user 2026-07-10]
- Autopilot runs apply the default automatically (repo agents if detected, else the user's templates) and record which roster they used, with no human interaction. [user 2026-07-10]
- The Slack plan-approval gate offers the same source choice (two Approve buttons: repo agents / my templates); excluding individual agents is done in the web UI. [user 2026-07-10]
- The shipped picker is validated visually against the approved mock (`prds/mockups/37-agent-picker-mock.html`). [user 2026-07-10]

## Feature #41 — Plan revision at the approval gate

Tracked as GitLab issue vtmocanu/uzi#41; PRD at `prds/done/41-plan-revision-gate.md`.

- At the plan gate the user can request changes (bounded rounds) to steer the plan without killing the run.

## Feature #40 — Token usage & cost reporting (per run / per user / factory-wide)

Tracked as GitLab issue vtmocanu/uzi#40; PRD at `prds/done/40-token-usage-reporting.md`.

- Report token usage and cost per run, per user, and factory-wide. [user]
- Run view shows the run's usage, broken down per phase and per agent ("coder used 800k"). [user 2026-07-12]
- Every user sees their own "Your usage" (lifetime + last-7-days); the factory total and per-user breakdown are admin-only. [user]
- Tokens are the headline figure; cost is a secondary estimate — a $0 cost (subscription-auth runs) renders as "—", not "$0.00". [user]
- Failed and cancelled runs still count their spend. [user]
- Chat runs are out of scope (not counted). [user]
- Shipped surfaces validated against the approved mock (+ addendum). [user 2026-07-12]

## Feature — Run retrospective (LLM judge) & self-improvement job

Tracked as GitLab issue vtmocanu/uzi#46; PRD at `prds/done/46-run-judge-self-improvement.md`
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
- **Self-improvement scheduled job (per-user; any repo the owner enables it on)**:
  configurable interval (2-day default), per-user enabled; reviews uzi's own codebase
  plus accumulated judge recommendations and picks **one top thing** per iteration —
  bug, feature, or whole refactor — so uzi iterates and self-improves. [user 2026-07-12;
  user 2026-08-23: generalized from admin-only to per-user, PRD #590]
- The job spins up an agent team that implements the pick and creates an MR
  (normal guardrails: never main, MR only). It runs autonomously — no approval
  gate blocks it — but the plan it worked from must be inspectable. If a
  self-improvement MR is already open, it reuses/extends that MR so everything
  is tested together. [user 2026-07-12]
- One PRD covers both, phased: judge first, job second (shared settings/inbox
  plumbing). [user 2026-07-12]
- Token for the job: each user can enable the job (on a repo they own) using their
  own token; the design also accommodates a general/instance token for later, when/if
  one is implemented (plan.md:69). [user 2026-07-12; user 2026-08-23: enablement
  generalized from admin to per-user, PRD #590]
- Judge recommends; only the job acts. Judge never auto-creates MRs.
  [user 2026-07-12]
- **File a recommendation as a forge issue** (vtmocanu/uzi#68): each recommendation
  gets a File-issue button; the human picks which one to file, reviews an
  API-templated editable draft (no extra LLM call, no Anthropic token), and files
  it. The issue is labelled to land on the board (`PRD`+`PRDLESS`) but **never
  auto-starts** (no `autopilot`) — filing an issue and spending tokens on a run
  stay separate human decisions. [user 2026-07-17]
- **Admin may file** another user's recommendation (kept, not restricted to the
  owner), conditioned on **prominent provenance** showing whose worker produced
  the (attacker-influencable) text. [user 2026-07-17]
- Works on **every existing recommendation**, with no backfill and no re-judge.
  [user 2026-07-17]
- **When a backlog read is truncated the page says so**, in a plain warning
  banner naming the two consequences (understated counts, missing groups) and
  the two remedies — **no dismiss control and no warning icon**. A banner that
  says the screen is not the truth must not be silenceable. [user 2026-07-25]
- After a bulk disposition on a truncated backlog, the CLI prints **one runnable
  `uzi review backlog --run <id>` line per settled run**, not a single line
  carrying a `<run-id>` placeholder — naming every affected run beats making the
  user guess one. A write that settled nothing prints no command.
  [user 2026-07-25]
- Truncation is **reachable in demo mode** through the existing demo-scenario
  mechanism (`?mock=truncated-backlog` / `uzi_mock_scenario`), never a build flag
  and never by accident: it is the one state where the screen is not the truth,
  so a person needs to be able to see it. [user 2026-07-25]
- A dedicated `cost_efficiency` judge recommendation category: surface quality-first
  cost-efficiency findings — recommend cost cuts only where they don't reduce
  correctness, verification depth, or code quality. [user 2026-08-15]
- `cost_efficiency` is triage-only: it does NOT feed the self-improvement job.
  [user 2026-08-15]

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

Tracked as GitLab issue vtmocanu/uzi#47; PRD at `prds/done/47-loop-hang-detection.md`.

- Detect runs that are taking too long or seem stuck, and flag them. (plan.md line 68)
- Flags surface in the web UI and on Slack.
- Flags are non-terminal and self-clearing: a flag never kills, requeues, or times out a
  run — early-warning only; existing watchdogs keep the kill job.
  [AI-proposed surface, user-ratified via PRD approval 2026-07-12]
  - **The clause "existing watchdogs keep the kill job" stopped being true on 2026-07-25**
    (PRD #108 Phase 2). The rest of the line still holds exactly as written: the FLAG still
    never kills, requeues or times out anything, and the stop is a separate mechanism with
    its own off switch that deliberately does not ride the run-health toggle. What changed
    is that there is now a NEW watchdog, and it kills. Recorded here rather than rewritten,
    because a requirement that changed is not the same artifact as one that was wrong.
    [AI-proposed; NEEDS USER RATIFICATION]

## Feature #49 — Worker resource stats (live per-worker CPU/memory)

Tracked as GitLab issue vtmocanu/uzi#49; PRD at `prds/done/49-worker-resource-stats.md`.

- Live per-worker CPU and memory visibility in the uzi web UI ("worker resource stats"). [user 2026-07-14]
- Per-worker granularity is sufficient; no per-run attribution. [user-confirmed]
- Must work the same under docker-compose today and k8s later (portability). [user]

## Feature #52 — CI/CD: real pipeline, tag releases, ArgoCD deploy to dev-cluster

Tracked as GitLab issue vtmocanu/uzi#52; PRD at `prds/done/52-cicd-argocd-deploy.md`.

- Real CI/CD: a working pipeline, tag-driven versioning, and ArgoCD deploy to the dev cluster. [user 2026-07-13]
- The ArgoCD wiring lands via an MR to the `argo-apps` repo — never a direct push to that repo's main. [user]

## Feature #53 — Per-user Claude rate-limit visibility

Tracked as GitLab issue vtmocanu/uzi#53; PRD at `prds/done/53-rate-limits.md`.

- Show each user's Anthropic account rate-limit headroom (5-hour and 7-day windows) in the uzi web UI.
- Users see their own meters; admins see every user's, on one page (mirrors the PRD #40 usage split).
- Server polls with the user's own token; the token never leaves the api container — SPA sees only percentages.
- The header-probe fallback spends ~1 token/interval of the user's own quota; operators can disable the probe (`UZI_USAGE_PROBE=false`) or the whole poller (`UZI_USAGE_POLL_INTERVAL=0`).
- No Anthropic token ever appears in a log line, API response, or the SPA.

## Feature #55 — OIDC group → role/access mapping (Keycloak / Pocket ID)

Tracked as GitLab issue vtmocanu/uzi#55; PRD at `prds/done/55-oidc-group-mapping.md`. Builds on PRD #45 (OIDC SSO).

- On a shared/team deployment the IdP owns who is admin (and optionally who may log in), replacing the first-login-race / env-seed model. [user 2026-07-16]
- Two configurable comma-separated group lists: an admin-groups list (membership ⇒ is_admin) and an allowed-groups list (membership required to SSO-login / JIT-provision at all); empty = that feature off. [user 2026-07-16]
- Authoritative sync on EVERY OIDC login: groups both grant AND demote — leaving the admin group demotes on next login, leaving the allowed group blocks the next login. No sticky roles. [user 2026-07-16]
- An absent/unparseable groups claim is treated as IdP misconfig, NOT removal (fail-safe): existing users keep their role and pass the gate; a brand-new JIT user is still refused when the allowlist is set. [user 2026-07-16]
- When admin-groups is configured, first-OIDC-user-becomes-admin is disabled (the group decides). `UZI_SEED_EMAIL` stays as break-glass, exempt from group demotion; with groups configured, seeding is optional (only for break-glass password login + credential seeding). [user 2026-07-16]
- OIDC-only scope: groups apply only at OIDC login; password-login users (incl. the seed admin) keep their stored `is_admin`; password first-user-admin is untouched. [user 2026-07-16]
- Must work with BOTH Keycloak and Pocket ID. [user 2026-07-16]

## Feature #58 — Hosted k8s workers (self-service worker provisioning)

Tracked as GitLab issue vtmocanu/uzi#58 (closed); PRD at `prds/done/58-hosted-k8s-workers.md` (moved to `done/` 2026-07-25). Partially delivers the Deferred "on-demand worker spawning" item below — spawn-on-queued-work is NOT in scope here.

- Self-service: any user provisions their own worker from the web UI, bounded by an admin-set per-user quota. [user 2026-07-16]
- The user picks two things: a worker type (image template) and a size (S/M/L). [user 2026-07-16]
- k8s only; docker-compose/laptop users keep the manual worker flow. [user 2026-07-16]
- A dedicated controller — never the api — holds the cluster credentials and creates the worker pods. [user 2026-07-16]
- The worker→api hop is TLS in v1. [user 2026-07-16]
- Trimmed v1 surface: sizes are built-in constants (no preset CRUD), no restart endpoint, heartbeat-only status. [user 2026-07-16]
- Sizes are Burstable (requests < limits): `s` 250m–1 CPU / 1–2Gi RAM; `m` 500m–2 / 2–4Gi; `l` 1–4 / 4–8Gi; `/data` 5/10/20Gi; `/nix` a flat 4Gi. [user 2026-07-17]
  - `/nix` is now a flat **20Gi** — raised for PRD #87's prebaked Chromium closure. [user, PRD #87]
  - `l`'s RAM limit is now **12Gi** (request stays 4Gi), raised to stop runtime OOMKills from multi-agent runs (parallel subagent waves plus the web-ux browser). [user, #131]
- Default size is `m`, not `s`. [user 2026-07-16]
- Three sizes stay, and the picker displays what each size buys. [user 2026-07-17]
- Deleting a hosted worker requires a confirmation (it destroys the worker's volumes); deleting an external worker stays one click. [user 2026-07-16]

## Feature #64 — uzi CLI: terminal control for humans and agents

Tracked as GitLab issue vtmocanu/uzi#64; PRD at `prds/done/64-uzi-cli.md`.

- A `uzi` CLI shipped in this repo, installed via the existing `vtmocanu/homebrew-tap`.
- Driven identically by humans (tables on a TTY) and agents/CI (`--json`, documented exit codes).
- Headless requirement: an agent drives uzi with a Bearer token in `UZI_TOKEN` — no browser, no cookie, no `$HOME`. [Success Criterion 1]
- `uzi login` works on a password-only stack AND an OIDC-backed instance with no IdP configuration change. [Success Criterion 2]
- Admin gets read-only verbs over the CLI; every admin write stays a webui action. [user override, PRD #64 Decision 5]

## Feature #71 — Automatic CI-fix for failed pipelines

Tracked as GitLab issue vtmocanu/uzi#71; PRD at `prds/done/71-ci-autofix.md`.

- Opt-in per-user automatic CI-fix, default OFF (mirrors judge/autopilot); admin can force-toggle. [user 2026-07-17]
- Fires only on agent-owned MR-branch pipelines (`agent/issue-N`); `main`/protected branches are never auto-touched — a fix still lands on the MR branch and a human still merges (primary directive). [user 2026-07-17]
- Loop-guarded: max 2 automatic attempts per branch + an early stop when the failure hasn't changed; on giving up, uzi comments + notifies and stops (the manual Fix CI button remains). [user 2026-07-17]
- "Usually we fix the code, not CI itself, but if CI is really at fault we can add a CI fix in the MR" — code fixes push automatically; a fix that edits the CI config passes the approval gate (human-approved before it pushes). [user 2026-07-17]

## Feature #72 — PRD lifecycle inside the run

Tracked as GitLab issue vtmocanu/uzi#72; PRD at `prds/done/72-prd-lifecycle-in-run.md`.

- Bundle the relevant PRD skills so a run can update its own PRD. [user, the originating ask on #72]
- After the MR merges, uzi patches the issue's own PRD link to follow the moved file. [user 2026-07-25]
- Autopilot does this unattended — a run may move a completed PRD to `prds/done/` with no human in the loop. [user 2026-07-25]
- Accepted exposure: a repo-source autopilot run may move a PRD to done with no uzi-controlled component checking it — *"allow it, we review the MR by human anyway"*. [user 2026-07-25, ratified verbatim]

## Feature #83 — Docker-capable worker

Tracked as GitLab issue vtmocanu/uzi#83; PRD at `prds/done/83-docker-capable-worker.md`.

- Workers must be able to run Docker/Compose projects (uzi's own e2e/smoke need `docker compose up`). [user]
- The default worker has docker + python + go available. [user]
- Trust model: trust the USER who owns the worker, not the repo code the agent runs (prompt-injectable). Security compromises allowed to cut complexity; agent-facing defenses stay load-bearing. [user]
- k8s is the first-class test/runtime environment (not the deferred track). [user]
- k8s docker posture: a dedicated privileged-tier namespace running the rootless-DinD sidecar. [user, Q-B owner decision]

## Feature #95 — Run activity pane v2: crew roster, opt-in follow, steer-queue delivery

Tracked as GitLab issue vtmocanu/uzi#95; PRD at `prds/done/95-activity-pane-v2.md`. Rebuilds Feature #38 (activity feed); the follow behavior amends Feature #11.

- Three user-reported UX problems to fix:
  - The activity pane must not auto-scroll / jerk to the bottom on every incoming frame — watch a live run without being dragged along. [user 2026-07-20]
  - Show a real "who's alive": glance at the pane and see each agent's state — working / waiting / done / blocked. [user 2026-07-20]
  - A follow-up must not vanish silently — show that it exists and whether the worker has picked it up. [user 2026-07-20]
- Authorized behavior change: collapse-by-default logs + an opt-in "Follow live" toggle REPLACE the global auto-scroll. [user 2026-07-20, supersedes the Feature #11 default "activity feed auto-scrolls (follows) live runs"]

## Feature #108 — Worker retry loop: stop losing runs to unsaveable messages

Tracked as GitLab issue vtmocanu/uzi#108; PRD at `prds/done/108-worker-retry-loop-autostop.md`.

**Every bullet below is [AI-proposed; NEEDS USER RATIFICATION].** The user's direct input on
this work was "finish PRD 108" and "use the team"; the requirements here were derived by the
team from the PRD and are recorded so they can be accepted or rejected on sight, not so they
can be assumed.

- A run whose updates the server cannot save must not spin silently until `RUN_TIMEOUT`.
- The user is flagged with the cause, and DM'd on Slack, before anything is stopped.
- uzi may stop such a run automatically. A stopped run is presented as **breakage**, not as a
  stop the user asked for.
- If uzi cannot tell "this run is poisoned" from "the database is down", it flags and does
  **not** stop — permanently, with no fallback and no timeout into stopping.
- uzi only stops for a failure a correct worker could have hit through no fault of its own. A
  malformed request means the worker *build* is broken; that is flagged, never stopped, and
  the remedy is rolling the image.
- Operators can disable the automatic stop without losing the flag.

## Feature #102 — Board v2: column rename, label chips, manual order, non-PRD issues

Tracked as GitLab issue vtmocanu/uzi#102; PRD at `prds/done/102-board-v2.md`.

- The implicit no-label column is called `Backlog`, not `Open`. Display only; it is not a forge label. [user 2026-07-20]
- The seeded `Upcoming` column label is renamed `Planned` and seeds first, before In Progress: Backlog | Planned | In Progress | Human Review | Later. Existing boards are not migrated automatically. [user 2026-07-20]
- Cards show their other labels (e.g. `bug`) as chips. Workflow labels (`PRD`, `PRDLESS`, autopilot) and the card's own column label are not shown. [user 2026-07-20]
- Cards can be hand-ordered within a column. The order is shared between users, and is uzi's own, not stored on the forge. [user 2026-07-20]
- A per-user toggle shows open issues that lack the `PRD` label, so the board can be used to see untriaged work. Off by default. [user 2026-07-20]
  - Non-PRD cards are visually distinct and cannot start runs.
  - They move between columns like any other card (including into In Progress).
  - A `Promote` action adds the `PRD` label, making the card a normal one.
- Authorized behavior change: the board can also DISPLAY open issues without the `PRD` label (opt-in, off by default); agents still work only `PRD`-labeled issues. [user 2026-07-20, narrows the Feature #2 line "Board/agents work only issues carrying the `PRD` label"]

## Feature #113 — Worker upgrade & version health

Tracked as GitLab issue vtmocanu/uzi#113; PRD at `prds/done/113-worker-upgrade-status.md`.
Mock at `prds/mockups/113-worker-upgrade-status-mock.html`.

- A worker's reported version must be the release it is actually running — no more a frozen informational string. [user, accepted from the mock 2026-07-22]
- Workers needing attention are those that **failed to upgrade** or are **behind**; a worker mid-upgrade is informational and must not raise an alert. [user 2026-07-22]
- Diagnostics are **read-only** in v1: no restart, retry, or auto-rollback of a failed upgrade. [user 2026-07-22]
- Dark-only, matching the product's two dark themes; no light variant. [user 2026-07-22]
- The mock is the accepted design for the fleet panel, the per-worker badges, the failed-worker detail strip, and the Workers-menu alert badge. [user 2026-07-22]

**Deviations from the accepted mock, taken by the team during implementation — ratified [user 2026-07-26]:**

- The raw pod-log pane is **dropped** and `pods/log` is refused: worker logs carry agent output over a user's cloned private repo, so granting it would make the controller a channel for customer source.
- **"View pod events"** is a copy-the-`kubectl`-command affordance, not live in-app events (no `events: list` grant). Restorable later for one RBAC line plus a handler.
- The **"1 release behind"** ordinal is dropped as not derivable (uzi knows two version strings, not the release sequence). Both versions are rendered instead.
- **Mute** shipped as storage only — there is no way to set a mute from the UI in v1.

## Feature #121 — Pre-provision a cloned repo's JS dependencies

Tracked as GitLab issue vtmocanu/uzi#121; PRD at `prds/done/121-clone-js-deps-provision.md`.

**Ratified [user 2026-07-27].** Derived by the team during implementation and put to the
owner, rather than stated up front: the original trust-posture premise was found false
during the work, and the constraint below is what replaced it.

- A run's dependency install must never execute code the cloned repo authored: it
  runs pre-approval, so it has to clear the same bar as `repo_devbox_opt_in` (per-repo
  opt-in, default OFF, a repo's devbox scripts never executed).
- `--ignore-scripts` is NOT sufficient on its own to hold that line (measured for yarn
  and pnpm), so the per-manager mitigations that do hold it are part of the contract,
  not an implementation detail.
- If uzi ever adopts a full-scripts install, auto-provisioning must become opt-in.
  Crossing that tradeoff is the owner's decision, never a milestone's.

## Feature #111 — Auto-select the Anthropic token per run

Tracked as GitLab issue vtmocanu/uzi#111; PRD at `prds/done/111-auto-select-anthropic-token.md`.

- A worker can choose its Anthropic token automatically from a pool the user opts in, preferring the account with the most headroom. [user, the originating ask on #111]
- Every run shows which token it actually used. [user, same ask]
- Scope is a per-worker third bind mode (default / pinned / auto), not a per-user global toggle: some workers stay pinned while the rest auto-balance. [user 2026-07-22]
- The candidate set is an opt-in pool, per token, default OFF — auto must never spend a token the user reserved for other work. [user 2026-07-22]
- Ranking is least-consumed first; a within-threshold tie goes to the account that resets soonest. [user 2026-07-22]
- The dev-cluster k8s validation (a PRD success criterion) is deferred to a follow-up issue, not dropped. [user 2026-07-27]

## Feature #35 — Retry after an Anthropic usage limit

Tracked as GitLab issue vtmocanu/uzi#35; PRD at `prds/done/35-run-limit-retry.md`;
ADR at `adr/0035-run-limit-retry.md`.

- When a run hits the Anthropic usage limit, retry after a delay — back off until
  the limit resets, instead of failing the run. [user, the originating ask on #35]
- Two opt-in scopes: a per-user default in Settings, and a per-run choice.
  [user, same ask]
- The per-run scope is a toggle on the RUN VIEW while the run is non-terminal,
  plus `wait_on_limit` on run creation for CLI/API callers; starting a run stays
  one click and inherits the user default. [user 2026-07-27 — confirmed
  reinterpretation of the per-run clause above: the run-start modal the PRD assumed
  does not exist, and a toggle also reaches autopilot, `ci_fix` and `self_improve`
  runs, which have no start affordance at all]
- `RUN_LIMIT_MAX_WAITS` stays at its default of 5 — a retry budget, not a
  credential-count budget; a large-pool operator raises it via env. [user 2026-07-27]

## Feature #218 — A park or shutdown must not lose the agent's committed work

Tracked as GitLab issue vtmocanu/uzi#218; PRD at `prds/done/218-park-resume-work-loss.md`.

- When a run parks on a usage limit, or its worker is shut down or evicted, the
  work the agent has already committed must survive: a resume finds those commits,
  not an empty tree. [user 2026-08-04 — PRD #218, the originating bug]
- A resume must not silently rebase onto a default branch that moved while the run
  was interrupted; the run's own recovered work is preferred when it is available.
  [user 2026-08-04]
- When prior work genuinely cannot be recovered, the run says so in the feed rather
  than silently re-treading it. [user 2026-08-04]

## Feature #88 — Ask-user clarification: the agent can ask the human a question

Tracked as GitLab issue vtmocanu/uzi#88; PRD at `prds/done/88-ask-user-clarification.md`.

- An agent can ask the user a clarifying question and wait for the answer, before
  and during a run. [user, the originating ask on #88]
- One PRD owns the whole mechanism — pre-run and mid-run both. [user 2026-07-19]
- PRD #84 only *emits* pre-run spec questions into this mechanism; it does not build
  the surface. [user 2026-07-19]

## Feature #175 — Build info in the UI: version badge popover, endpoint and CLI parity

Tracked as GitLab issue vtmocanu/uzi#175; PRD at `prds/done/175-build-info-popover.md`.

**Ratified [user 2026-07-28].** The user asked for the feature and stated no design
requirement; the team decided its shape. The two below were put to the owner because
they are durable product constraints rather than implementation choices. Every other
decision on this feature is the team's and lives in `specs/ai.md` §448-§454.

- `GET /api/version` publishes `uptime_seconds` on an unauthenticated, unrate-limited,
  ingress-reachable endpoint. Uptime is accepted as public; severity Low.
  [user 2026-07-28]
- `uzi version --json` gains a `server` key carrying the server's build info, with the
  CLI's own `version` unchanged at the top level. A CLI output-contract change.
  [user 2026-07-28]

## Feature #201 — Builtin agent-template drift signal

Tracked as GitLab issue vtmocanu/uzi#201; PRD at `prds/201-builtin-drift-signal.md`.
Issue #201's body plus its note_22449 comment (2026-08-03) are the settled design
for the whole of #201; this milestone (M4a) is the drift signal only.

- Implement issue #201. [user 2026-08-03]
- Ship M4a (the drift signal) on its own, first. M4b (auto-update) does not start
  until M4a is reviewed. [user 2026-08-03]
- The API shape for serving the shipped definition is delegated to the team under a
  best-practice bar; breaking API changes are affordable while uzi has a single
  user. Scoped to this decision, NOT a project-wide constraint. [user 2026-08-03]

Every other decision on this feature is the team's and lives in `specs/ai.md`
§476-§478.

## Feature #224 — Worker pods declare no ephemeral-storage request

Tracked as GitLab issue vtmocanu/uzi#224; PRD at `prds/done/224-worker-ephemeral-storage.md`.

- Fix the defect: a worker pod evicted for node ephemeral-storage pressure
  destroys every in-flight run's work, silently. [user, the originating ask]
- Ship a conservative, chart-tunable default now; measure on the fleet after.
  [user 2026-08-04]
- Ship it and accept ONE loss event: rolling this kills every in-flight run once.
  Not sequenced behind #218, not gated on a drained fleet. The change lowers how
  often the loss happens; it does not close it. [user 2026-08-04, chosen from
  four options with the loss stated plainly]
- All four design follow-ups land in this session rather than being filed —
  "cant we fix them all now, in this session?" [user 2026-08-04]
- Evicted-pod cleanup is a manual one-off deletion by exact name. No
  controller-side reaper: a new reconcile responsibility plus an RBAC delete verb
  plus its own tests, for a cosmetic problem. [user 2026-08-04]
- Quotas are raised to match the advertised fleet, rather than the advertised
  fleet lowered to match the quotas. [user 2026-08-04]
- Build the boot-time PVC-ceiling check now. [user 2026-08-04]
- The PVC resize path is dropped; the one legacy worker gets
  delete-and-reprovision. [user 2026-08-04]
- The imagefs / image-accumulation defect is FILED, not fixed — issue #225.
  [user 2026-08-04]

Every other decision on this feature is the team's and lives in `specs/ai.md`
§479-§481.

## Feature #144 (item 1) — Warn when the CLI is behind the server

Tracked as GitLab issue vtmocanu/uzi#144 (item 1). Scoped MR, no PRD [user 2026-08-03].
Completes Feature #64/#175: `uzi version` reported both versions and never compared them.

- Every uzi command warns when the CLI is older than the server it talks to — not
  just `uzi version`, except commands that make no network call of their own.
  [user 2026-08-03, chosen from three placements]
- The warning goes to stderr. stdout and the exit code are unchanged. [user 2026-08-03]
- The server's version is probed on a cache, never once per command. [user 2026-08-03]
- The remedy offered is `brew upgrade uzi-cli`. [user 2026-08-03]

## Feature #325 — TUI redesign ("factory shift board")

Tracked as GitLab issue vtmocanu/uzi#325; PRD at `prds/325-tui-redesign.md`.
Redesigns the shipped `uzi tui` (PRD #112). TUI/CLI-only.

- The shipped TUI looked bad; redesign it to convey run status and support live-following legibly. [user 2026-08-15]
- Direction is a "factory shift board": a colour-coded status board. [user]
- Agents can review the TUI's look on both light and dark themes. [user]
- Detail nav is a focusable-pane model: up/down navigate agents, left/right select the pane; default focus is the crew rail. [user]
- One-line keybinding footer. [user]
- Keep the health words visible on the board (not colour-only). [user]
- The interactive demo is rebuilt on the shipped views (not retired, not a separate prototype). [user, D1]

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
  - PARTIALLY delivered by PRD #58 (2026-07-17), k8s only: the dedicated operator
    exists (a `controller` component, never the api) and provisions per-user workers
    **on user request** (Settings → Workers; opt-in, quota-bounded). NOT delivered,
    still deferred: the **when-queued-work-appears trigger**, autoscaling,
    scale-to-zero, and the chat-agent case — a #58 worker is persistent and runs
    until deleted. [design detail in specs/ai.md §264-275]
- Auto-creating bot accounts / bot role enforcement (forge ships with user-managed bots).
- Forgejo driver — DELIVERED by PRD #65 (2026-07-17): full-parity second driver behind the forge-generic interface; no longer deferred.
- Agent runtime/execution (spawn, file writes, Anthropic API calls) — PRD #4.
