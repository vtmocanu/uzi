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

## Feature #7 — In-app docs section

Tracked as GitLab issue vtmocanu/uzi#7; PRD at `prds/7-docs-section-webui.md`.

- A docs section on uzi with relevant howtos: how to create an agent/bot, how to do GitLab bots / give permissions, etc.
- Include screenshots; screenshots are provided by the user (ask the user for them).
- Skills howto is out of scope until a skills feature exists.

## Startup admin seed

- Seed an admin user from env at startup (`UZI_SEED_EMAIL` / `UZI_SEED_PASSWORD` / `UZI_SEED_NAME`) so the user survives DB wipes.
- Seeded user gets the admin role; never overwrite an existing user.

## Deferred (user, "later stuff")

- Auto-creating bot accounts / bot role enforcement (forge ships with user-managed bots).
- Forgejo driver (interface is forge-generic; GitLab implemented first).
- Enable/disable registration for users.
- SSO with Keycloak.
- Agent runtime/execution (spawn, file writes, Anthropic API calls) — PRD #4.
