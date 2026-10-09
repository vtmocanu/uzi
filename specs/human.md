# uzi — Human Requirements (Contract)

Product requirements and decisions. This is the contract: every item
here must hold in any rebuild. AI has full authority over this file and needs no
approval to edit it, form and substance alike: adding a requirement, rewording
one, or changing what one means (`CLAUDE.md`, "Specs contract"). Keep entries
terse and tag each AI edit `(AI-synced YYYY-MM-DD)`.

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

## Codex bounded Read (#296)

- Text Read accepts path/file_path and optional one-based positive safe integer offset (default 1), limit 1–2000 (default 200); excerpts preserve UTF-8/BOM and LF/CRLF, cap at 64 KiB, and report size/content/offset/linesReturned/partialLastLine/truncated without duplicate base64 or a cursor. Strict helper validation retains the 1 MiB file ceiling and neutral errors. Invalid UTF-8 or NUL returns exact base64 only without explicit ranges; ranged binary is refused. Root and child replies must fit the existing 4 MiB transport cap. Full contract: [ADR 0296](../adr/0296-codex-bounded-text-reads.md). (AI-synced 2026-10-05)

## Claude SDK spill Read (#2332)

- On the Claude run lane the file-tool path guard lets `Read` (only) open a direct-child regular file of the run's own SDK tool-result spill directory (`<run HOME>/.claude/projects/<P>/<session_id>/tool-results/`), keyed on the SDK-supplied session_id/transcript_path and the worker-supplied HOME; other sessions' or runs' directories, symlinks and nested paths stay denied, and the /proc, secret and `.git` denies are unchanged. Full contract: [ADR 2332](../adr/2332-sdk-spill-read-allowance.md). (AI-synced 2026-10-06)

## MVP / infrastructure

- Initial MVP is a local laptop demo via docker-compose.
- PostgreSQL database.
- Persistent storage (data survives restarts).

## Feature #1 — Simple WebUI with user registration

Tracked as GitLab issue vtmocanu/uzi#1; PRD at `prds/done/1-simple-webui-user-registration.md`.

- Simple web UI with user support and registration.
- Plain user/password registration stored in the DB.
- No email (no OTP, no verification, no reset for now).
- No SSO/OAuth. (This concerns logging in to uzi and was superseded by #45; since #1910 uzi acts as an OAuth server for registered products. (AI-synced 2026-10-01))
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
- Board/agents work only issues carrying the `PRD` label, sanity-checked to contain a link to the PRD file. [superseded — see Feature #22: run-eligibility is now the single `uzi` label and the PRD link is optional (PRD #764) (AI-synced 2026-08-29)]

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
- A blocked repo's owner can request an instance-admin guardrail exception (with a reason) after a waivable Enable refusal; only an admin approves, and approval sets the existing per-repo override rather than enabling — the owner retries Enable so the live guard re-runs. An unverifiable refusal (unreadable branch protection) is never waivable, so no request is offered for it. Extends #66's admin-only override reachability; still only an instance admin may allow a repo through the guardrail. (AI-synced 2026-09-19)

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
- A Claude run whose skills plugin the SDK reports as failed to load (issue #1888) never works without its selected skills: a run with selected skills fails with the `skills_plugin_load_failed` fail origin (never judged, not retried automatically), even when the reported error detail is malformed; a run with no selected skills gets a warning status line (once per start or resume) and continues. The SDK's error text is redacted, stripped of control characters and bounded before it is shown. (AI-synced 2026-09-29)

## Feature #17 — Builtin lead template (opus) + worker model selection

Tracked as GitLab issue vtmocanu/uzi#17; PRD at `prds/done/17-lead-template-and-model-selection.md`.

- Ship `lead` as a builtin agent template with a real orchestrator prompt, on model `opus`; editable and resettable in the UI like the other builtins.
- Builtin templates are the single source of truth; `.claude/agents/` is the dev team's own roster only. Decouples the former 1:1 mirror. [user, 2026-07-05, supersedes the earlier "both dirs" choice]
- Per-user worker-model defaults are retained separately for Claude and Codex and saved together with the default harness in Settings. (AI-synced 2026-09-23)
- Both worker-model defaults offer curated models plus a custom model ID; Codex custom IDs apply to the worker root and the dedicated Plan cross-check checker pin, not schedule or role pins. The checker exception does not enable Codex-lead cross-checks. (AI-synced 2026-10-08)
- Precedence: the user's default for the run's selected harness wins over the lead template's model (unset = inherit the lead template's model, opus by default). (AI-synced 2026-09-23)
- Sequence this PRD before PRD #16 so #16 inherits the decoupled-builtins convention.
- Switching the default harness never clears either saved worker-model choice. [PRD #1551] (AI-synced 2026-09-23)
- Task review uses its own built-in model (Codex: `gpt-6-sol`) independent of these saved defaults; there is no setting to choose the task-review model in this release. [PRD #1551] (AI-synced 2026-09-23)
- The Codex worker fallback is `gpt-6.1-sol`, included in the curated picker; explicit model choices remain authoritative. (AI-synced 2026-10-01)
- Claude and Codex keep independent reasoning-effort preferences, both inheriting medium. Splitting preserves every existing explicit preference in both lanes; queued and resumed runs resolve preferences at claim time. (AI-synced 2026-10-01)
- Codex runs wait for a worker with the current runtime/catalog and correct effort handling; an older worker must not silently substitute a model or ignore the requested effort. (AI-synced 2026-10-01)

## Feature #18 — Worker templates, per-repo tools & agent scopes

Tracked as GitLab issue vtmocanu/uzi#18; PRD at `prds/done/18-worker-templates-and-agent-scopes.md`.

- Curated worker image templates in git so different workers can carry different heavy toolchains (e.g. node tools vs java tools); the user picks one per worker.
- Per-repo CLI tools installed on demand (so "command not found" stops being a dead end), from a user tool profile bounded by an admin allowlist, plus an opt-in for a repo's own devbox.json packages.
- Agent templates gain global and per-user (private) scopes with per-user allocation, so a user can define a private agent and choose which agents ride their runs; admins manage the shared defaults.

## Feature #19 — Admin settings & autopilot label

Tracked as GitLab issue vtmocanu/uzi#19; PRD at `prds/done/19-admin-settings-and-autopilot.md`.

- Generic admin-only instance-settings infrastructure; the PRD label and the autopilot label are its first two configurable keys.
- Admins can change the PRD label and the autopilot label; the board reflects the new label set after a resync (no code fork).
- Autopilot: adding the autopilot label (alongside the run-eligibility label, default `uzi`) to an issue in GitLab normally runs it end to end without uzi interaction. With Plan cross-check enabled, a Claude lead proceeds only after Codex approves the latest exact plan and the handoff is acknowledged. Eligible REVISE automatically returns fenced untrusted checker advice to a round-capable Claude lead within the server-counted budget; exhaustion, BLOCK, timeout and other check failures retain their human fallback. Codex leads park as unsupported. Irrecoverable preparation receipts or human-presentation ACK loss fails terminally; unresolved preparation ACKs after three attempts and an acknowledged forced human gate also fail. Historical findings do not certify a human revision. See [Cross-check](../docs/cross-check.md). (AI-synced 2026-10-08)
- Automatic Plan cross-check revisions use `PLAN_CROSS_CHECK_MAX_REVISIONS` (default 2, integer 0–4), not human `runs.revise_count` or `PLAN_MAX_REVISIONS`. Superseded attempts consume the global limit + 1 candidate budget across claim generations. First-candidate enabled/limit snapshots are immutable and copied to subsequent rounds; configuration changes affect new checks only. `cross_check_rounds_v1` gates automatic leads and later-round children; older workers retain the REVISE human gate, while capable zero-budget workers park as exhausted. Each candidate gets a fresh deadline. Before an automatic revision, validated preparation/usage/reconciliation must release the actual reservation successfully, then release and await the checked-state barrier successfully; this path creates no human presentation or `awaiting_approval` report. Human inputs cannot supply automatic provenance. (AI-synced 2026-10-08)
- Recovery decision **2026-10-07: preserve decided fallback**. Before an established human gate or durably approved plan, fresh-round recovery follows this matrix, within the same server budget. The first custody-invalidating transition is authoritative: direct writers use the database transaction clock; frozen writers use the server-provided `now` for that same transition. Persisted `interrupted_at`/decision evidence controls eligibility, not a later sweep clock. Equality or ambiguous legacy evidence fails closed to timeout. An eligible attempt without remaining budget parks with `plan cross-check: revisions exhausted`; a decided non-revisable outcome retains its own reason even when budget is spent. PRD #2149 D7 terminal delivery failures and D16 established human presentation/revision context remain unchanged, with no adoption or fresh check from an established human gate. (AI-synced 2026-10-08)

  | Latest attempt at reclaim | Disposition |
  |---|---|
  | Pending interrupted strictly before deadline | Fresh candidate/check within budget; refuse stale child verdict/storage. |
  | Lifecycle-settled failed/superseded with proven pre-deadline pending origin | Same fresh-round eligibility. |
  | Decided REVISE | Fresh round within budget. |
  | APPROVE never durably stored, marked `approved_not_stored` | Fresh round within budget, requiring a new exact-plan APPROVE. |
  | BLOCK, timeout or other check failure | Retain fallback; no fresh row or child. |
  | D7 terminal delivery loss | Retain terminal failure. |
  | Established D16 human gate | Retain human context/revision path; no adoption or fresh check. |
  | Durably approved plan | Existing approved-plan resume path. |

- (AI-synced 2026-08-29) PRD #764 removed the `prd_label` setting (and its special-casing) and added a configurable `uzi_label` (default `uzi`) as the single run-eligibility key; autopilot now rides alongside `uzi`, not `PRD`. The generic admin-settings infrastructure and the configurable `autopilot_label` are unchanged.
- Progress is visible via the existing board label moves; an opted-in Plan cross-check fallback can require a human gate decision in uzi, the CLI or Slack. (AI-synced 2026-10-06)
- Plan cross-check has separate worker capacity (default one checker slot), so a compatible worker can check its own Claude lead on Codex while the lead holds its run slot. Own-worker preference, cordoned completion and ephemeral own-pod checking retain maintenance fences; explicit zero disables the lane without legacy run-slot fallback. Worker capacity shows runs and cross-checks separately. Hosted memory headroom and acceptance remain pending; Codex-lead and Code cross-check are outside this change. See [Cross-check slots](../docs/worker-setup.md#cross-check-slots). (AI-synced 2026-10-08)
- Unchanged tracked symlinks no longer block Plan cross-check, and when the planning diff is refused its reason is shown with the gate reason (run page, CLI, Slack). (AI-synced 2026-10-07)
- Plan cross-check has independent hard model and effort pins per family in Run defaults; Default follows that family's worker default. Claim-time delivery freezes values and independent sources; findings retain immutable provenance, with legacy sources unknown. Pins do not substitute, clamp or retry on defaults. The Claude cell is editable but inactive ("Used once Codex-lead runs are cross-checked"); only Codex checkers for Claude leads execute. (AI-synced 2026-10-08)
- Local syntax/family, effort and worker-capability checks precede checker credential delivery; capability races requeue and queue time counts toward the verdict deadline. Pinned Codex cells require `cross_check_pins_v1`; custom resolved models separately require `codex_custom_model_v1`. Unpinned checks remain eligible on older `cross_check_v1` workers. Account model availability is discovered at authenticated startup: recognized pinned-model rejection fails the child with `plan cross-check: checker unavailable` and forces the human plan gate; other startup failures keep existing handling. (AI-synced 2026-10-08)
- `uzi settings get` reads stored pins, worker defaults, resolved values, sources and inactive status; JSON returns the decoded account settings DTO. CLI setters, admin/per-run overrides, Code cross-check and Claude-checker execution remain outside this pin release. (AI-synced 2026-10-08)
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

## Feature #22 — PRDLESS label (RETIRED by PRD #764)

Tracked as GitLab issue vtmocanu/uzi#22; PRD at `prds/done/22-prdless-label.md`.

- RETIRED: the `PRDLESS` escape-hatch label and the whole PRD-link requirement it bypassed were removed end-to-end by PRD #764. (AI-synced 2026-08-29)
- New binding reality (PRD #764): run-eligibility is the single configurable `uzi` label (default `uzi`) — label an issue `uzi` and it is runnable. (AI-synced 2026-08-29)
- A `prds/*.md` link is OPTIONAL — still auto-detected, implemented when present, and shown as a PRD-presence badge, but never required to start a run. (AI-synced 2026-08-29)
- Also removed with `PRDLESS`: the `eligible_label_waives_prd_link` waiver, the `run_eligible_labels`/`board_extra_labels` sets, and the `prd_label` special-casing. `Planned`/`bug` stay sweep selectors that fire only when the issue also carries `uzi`; `autopilot` is unchanged. (AI-synced 2026-08-29)
- (AI-synced 2026-08-29) An issue is ALSO runnable when assigned to the uzi-bot account (matched on the connection's numeric bot user id, rename-safe) — a second, equivalent expression of the same single eligibility concept, additive to the `uzi` label (PRD #767). Assignment grants eligibility ONLY; it never auto-runs (unattended execution still needs `autopilot` or an enabled sweep). uzi only reads assignees, never auto-assigns. New `assigned-sweep` default schedule fires the oldest few bot-assigned issues (auto-approve ON, like the other default sweeps).
- (AI-synced 2026-09-24) A label sweep reaches eligible issues (`uzi`-labelled or bot-assigned) no matter how many ineligible selector matches precede them: ineligible matches never consume the per-fire scan window, and the fire reports them as one aggregate count rather than per-issue skips (#1543).

## Feature #23 — Web UX polish: live dashboard, collapsible sidebar, hide empty board columns

Tracked as GitLab issue vtmocanu/uzi#23; PRD at `prds/done/23-web-ux-live-dashboard-sidebar-board.md`.

- Dashboard updates live: a run reaching `awaiting_approval` must show without a manual refresh.
- Desktop sidebar is collapsible.
  - The collapse control must not consume a full sidebar row. [user 2026-08-14]
- Empty board columns can be hidden.
  - Hiding empty columns is the DEFAULT for a board the user has not configured; an explicit "show empty" choice still wins. [AI-synced 2026-09-08 (#1208)]
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
- The repo roster may also come from `.codex/agents/*.toml`; the run's native folder is preferred, never merged (issue #2085). (AI-synced 2026-10-02)
- Show whether repo agents were detected and which ones. [user 2026-07-10]
- Default to the detected repo agents; if the user does not want them, they can choose their own templates instead. [user 2026-07-10]
- Repo agents run with the tools and model their files declare (honored as they would be under Claude Code), still subject to uzi's guardrails. [user 2026-07-10; a review-round proposal to deny WebFetch/WebSearch and clamp the model to aliases was rejected by the user the same day — `Agent`/nested-spawn and the async-deferral tools stay denied]
- Codex 0.159.3 permits native nonblocking async questions and read-only UTC on root start/resume and worker-created children; these grant no worker effect or workflow signal. The historical Claude async-deferral denial above remains. (AI-synced 2026-10-04) [AI-synced, #1566]
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
- One Usage card has a shared All time / Last 7 days toggle, defaulting to seven days and remembered per viewer in the browser. Personal figures are shown to everyone; factory figures and the embedded per-user breakdown are admin-only. Personal, factory and per-user figures follow the shared toggle's selected window; failure recency stays lifetime in both windows. [AI-synced 2026-10-07]
- The dashboard highlights metered cost, failed-run rate and tokens for the selected window. Metered $0 is shown as $0.00; subscription and unreported runs are disclosed separately from cost (#1429 D7). [AI-synced 2026-10-07]
- Failed and cancelled runs still count their spend. [user]
- Chat runs are out of scope (not counted). [user]
- Shipped surfaces validated against the approved mock (+ addendum). [user 2026-07-12]
- A Claude run's usage not covered by any SDK result (an interrupted session's tail) is recorded and shown apart from the metered total, labelled estimated with its price provenance, as unknown cost when unpriced (never $0), with a coverage indicator. (AI-synced 2026-10-03)

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
- The judge runs on the run owner's own credential for the reviewed run's harness
  (Anthropic token for a Claude run, Codex credential for a Codex run), never a
  shared one. [user 2026-07-12] (AI-synced 2026-09-24)
- Per-user opt-in/out; admin can toggle the feature globally and force-disable
  per user (existing admin settings). [user 2026-07-12]
- Recommendations show on the Judge page and the run page (users see their own)
  and go out as a Slack DM. [user 2026-07-12] (AI-synced 2026-09-25: the
  inbox/notifications surface and its admin all-users view were retired, #1650)
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
- One PRD covers both, phased: judge first, job second (shared settings
  plumbing). [user 2026-07-12] (AI-synced 2026-09-25: inbox retired, #1650)
- Token for the job: each user can enable the job (on a repo they own) using their
  own token; the design also accommodates a general/instance token for later, when/if
  one is implemented (plan.md:69). [user 2026-07-12; user 2026-08-23: enablement
  generalized from admin to per-user, PRD #590]
- Judge recommends; only the job acts. Judge never auto-creates MRs.
  [user 2026-07-12]
- **File a recommendation as a forge issue** (vtmocanu/uzi#68): each recommendation
  gets a File-issue button; the human picks which one to file, reviews an
  API-templated editable draft (no extra LLM call, no Anthropic token), and files
  it. The issue is labelled to be runnable (the `uzi` label — was `PRD`+`PRDLESS`
  before PRD #764) but **never auto-starts** (no `autopilot`) — filing an issue and
  spending tokens on a run stay separate human decisions. [user 2026-07-17] (AI-synced 2026-08-29: PRDLESS retired by PRD #764)
- **Admin may file** another user's recommendation (kept, not restricted to the
  owner), conditioned on **prominent provenance** showing whose worker produced
  the (attacker-influencable) text. [user 2026-07-17]
- **Admin may read the judge backlog aggregated across all users** with
  attribution hidden (no owner, no run identity — a distinct-user count instead),
  and may **mark a recommendation done across users** (each owner sees it was
  done by an admin, and can undo); dismissing another user's recommendation stays
  the owner's decision. [user 2026-09-07]
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
- **Judge and Findings share one triage row and one vocabulary** (File issue · Mark done · Dismiss ▾ at equal weight; the open state is "To triage" everywhere; filed is "Filed #N"); Findings shows each coordinate's evidence and the runs it was seen in, counted tabs, multi-select with undo, and a filed finding becomes Done when its issue closes on the forge; a person can also mark a finding done (from To triage, Filed or Dismissed) with the judge's disposition semantics, and Undo exposes Filed or To triage. [user 2026-09-07] (AI-synced 2026-09-26)

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
- The "slow" health flag is a near-timeout warning at a share of the run's wall-clock budget, not a bare timer. [user, #1170]
- A lead tool call in flight past an admin-set threshold (default 20 minutes, 0 disables) on a quiet run is flagged stalled with a fixed reason (the outbox-queued reason during an api outage); an open delegation to a subagent is never aged. (AI-synced 2026-10-01, #2046)
- A run's owner can extend its wall-clock budget from the web or CLI, up to an admin-set allowance; the frozen budget itself never changes. [user, #1189]
- A run that runs out of time is parked and its owner is asked to extend or stop; it is never failed for the clock alone, the park never expires, and this is not configurable. [user, #1497]
- Run defaults are 6h base wall time and 3 stale-worker requeues. RUN_WALL_CEILING defaults to min(max(24h, RUN_TIMEOUT), 72h), with empty/unset deriving the default so an existing larger base still boots. An explicit value must be at least 1s and the base timeout, and at most 72h; it caps scaled, non-interactive handoff and caller-requested job walls server-side. Explicit operator values win. Existing frozen budgets and the separate extension allowance stay unchanged. (AI-synced 2026-10-07, #2279)

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
- Alert the user (opt-in, default on; Slack DM only) whenever their Anthropic 7-day window clears early — on ANY early clear, not only after their runs were blocked on that window; a user without Slack linked sees the reset only on the rate-limit meters. [user, #1114] (AI-synced 2026-09-25: inbox copy retired, #1650)

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
  - `l`'s RAM limit was raised to **12Gi** (request was 4Gi; then 8Gi, #1341; superseded below by #2127), raised to stop runtime OOMKills from multi-agent runs (parallel subagent waves plus the web-ux browser). [user, #131]
  - Per-size RAM raised: `s` 2–4Gi, `m` 4–8Gi, `l` 8–12Gi — each request lifted above that size's measured per-run peak (all still Burstable). Stops kubelet node-memory-pressure eviction of workers that sat over their request, incl. mid-run. [user, #1341]
  - `l` RAM raised again to request **14Gi** / limit **20Gi** (#2127): a persistent `l` worker running two concurrent runs was OOMKilled twice at 12Gi while both runs were in whole-program Go analyzer gates; owner-selected sizing, not a measured capacity guarantee; reduced node placement capacity accepted. `m` likewise raised to request **8Gi** / limit **12Gi** (a single run's `deadcode` hit 7.3-7.5 GiB at the old 8Gi limit and was OOMKilled twice). [user, #2127] (AI-synced 2026-10-07)
  - Hosted worker pods now carry a default `priorityClassName` (a modest cluster-scoped PriorityClass, value 1000, `globalDefault: false`), so under node memory pressure other lower-priority pods are evicted before ours. `preemptionPolicy: PreemptLowerPriority` (owner's choice): a worker that cannot be scheduled for lack of room also preempts lower-priority pods to get placed. Operators can set `Never` to drop that scheduling-time preemption. [user, #1341]
- New ephemeral hosted workers default to `m`, configurable through chart `workers.ephemeralDefaultSize` (`s`, `m`, `l`, invalid values refuse rendering); non-chart API defaults and invalid-value fallback use `m`. The persistent picker stays at `l`; existing workers keep their stored sizes. `m` matches the pre-#2127 `l`: 1/4 CPU, 8Gi/12Gi memory, 25Gi persistent data. Ephemeral data retains its separate 20Gi default; existing persistent `m` PVCs keep 10Gi until reprovisioned. [user, #2412] (AI-synced 2026-10-07)
- Three sizes stay, and the picker displays what each size buys. [user 2026-07-17]
- Deleting a hosted worker requires a confirmation (it destroys the worker's volumes); deleting an external worker stays one click. [user 2026-07-16]
- Hosted k8s gains an opt-in uid-split worker profile for Codex (default off; while on, the kube-native worker namespace's PodSecurity admission drops from `restricted` to `baseline`, while the separate Docker-capable tier keeps its own `privileged` namespace); Landlock is optional via a mode knob (`required` fails closed, `best-effort` runs unconfined on a kernel without it, relying on the uid split alone). A worker without the split, or without usable Landlock under `required`, stops advertising Codex — those runs (including tool-less Codex advice) simply queue instead of being claimed and then failing. (AI-synced 2026-09-20)
- Codex advice has zero worker callbacks; native async questions and read-only UTC remain permitted. Advice ignores text from items whose delivery is exactly `"async"`; no questions are surfaced or answered, and the native capability stays available. This resolves the earlier filtering deferral under #2239. Other native effectful surfaces stay disabled, except the authority-free run code host. (AI-synced 2026-10-05) [AI-synced, #1566]
- Landlock for Codex commands is off by default (a third mode, `off`, is the default everywhere: worker, controller, chart); `required` and `best-effort` remain opt-ins. User decision after repeated Landlock-caused run failures (#1769, #1863, #1598). (AI-synced 2026-09-28)
- Restricted-tier hosted workers may reach `api.openai.com`, `chatgpt.com` and `auth.openai.com` on 443, fleet-wide for the tier (Claude-only workers included), per PRD #1106 D12. (AI-synced 2026-09-24)

## Feature #64 — uzi CLI: terminal control for humans and agents

Tracked as GitLab issue vtmocanu/uzi#64; PRD at `prds/done/64-uzi-cli.md`.

- A `uzi` CLI shipped in this repo, installed via the existing `vtmocanu/homebrew-tap`.
- Driven identically by humans (tables on a TTY) and agents/CI (`--json`, documented exit codes).
- Headless requirement: an agent drives uzi with a Bearer token in `UZI_TOKEN` — no browser, no cookie, no `$HOME`. [Success Criterion 1]
- `uzi login` works on a password-only stack AND an OIDC-backed instance with no IdP configuration change. [Success Criterion 2]
- Admin gets read-only verbs over the CLI; every admin write stays a webui action. [user override, PRD #64 Decision 5]

## Feature #71 — Automatic CI-fix for failed pipelines

Tracked as GitLab issue vtmocanu/uzi#71; PRD at `prds/done/71-ci-autofix.md`.

- Automatic CI-fix is ON by default for every user (per-user tri-state opt-out, NULL=inherit=on), behind an admin instance-wide kill-switch (default on); admin can force-toggle any user. [user, PRD #914 — supersedes the earlier default-off from #71]
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
- DinD state (containers, named/anonymous volumes, networks, images, build cache) is scratch; deliverables use git/checkpoints/capture. Persistent hosted Docker workers use distinct fresh byte-or-inode pressure epochs and a terminal-only, atomic claim fence plus zero local activity/fresh matching custody clearance for optional anonymous-volume prune and DinD-only PVC recycle; parked/paused/approval/input/follow-up waits block cleanup while own parked resumes may finish during pending drain. Recycle preserves nix/data, UUID and join Secret but loses DinD state and run-workdir emptyDir; default-on upgrades require an explicit warning and pre-stop opt-out, no forced park/deadline override, no rollback. Legacy disk_pressure remains nix/data-only; ordinary legacy overrides and compose manual cleanup are unchanged. Supersedes the cache-only/never-volumes decision of 2026-09-27 per maintainer decision 2026-10-04; see ADR-1759 for the strict gate, freshness, lifecycle, capability and observation boundaries. (AI-synced 2026-10-05)

## Feature #95 — Run activity pane v2: crew roster, opt-in follow, steer-queue delivery

Tracked as GitLab issue vtmocanu/uzi#95; PRD at `prds/done/95-activity-pane-v2.md`. Rebuilds Feature #38 (activity feed); the follow behavior amends Feature #11.

- Three user-reported UX problems to fix:
  - The activity pane must not auto-scroll / jerk to the bottom on every incoming frame — watch a live run without being dragged along. [user 2026-07-20]
  - Show a real "who's alive": glance at the pane and see each agent's state — working / waiting / done / blocked. [user 2026-07-20]
  - A follow-up must not vanish silently — show that it exists and whether the worker has picked it up. [user 2026-07-20]
  - The steer queue distinguishes received by the worker, routed by steering, and included in a prompt; it never says "delivered" for a follow-up no prompt included, and never claims the agent acted on it. Owner follow-ups reach the lead on both harnesses. (AI-synced 2026-10-01, issue #1800)
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
- Worker authentication returns 401 for a missing, unknown, or mismatched Bearer token, and 503 for a store lookup failure so the worker can retry. (AI-synced 2026-09-30)
- Session and CLI-token authentication return 401 for a refused credential and 503 for a store lookup failure, so a database outage does not sign users out or reject a good CLI token; on initial load the web app shows a transient can't-reach-server state that retries instead of treating the user as signed out. (AI-synced 2026-10-02, #1991)

## Feature #102 — Board v2: column rename, label chips, manual order, non-PRD issues

Tracked as GitLab issue vtmocanu/uzi#102; PRD at `prds/done/102-board-v2.md`.

- The implicit no-label column is called `Backlog`, not `Open`. Display only; it is not a forge label. [user 2026-07-20]
- The seeded `Upcoming` column label is renamed `Planned` and seeds first, before In Progress: Backlog | Planned | In Progress | Human Review | Later. Existing boards are not migrated automatically. [user 2026-07-20]
- Cards show their other labels (e.g. `bug`) as chips. The autopilot label and the card's own column label are not shown; the `uzi` run-eligibility label IS shown, hoisted ahead of the other chips and highlighted as the runnable marker. [user 2026-07-20] (AI-synced 2026-08-29: PRD #764 makes `uzi` the run-eligibility marker, shown and highlighted rather than hidden; `PRDLESS` retired. Chip exclusions are autopilot + column labels only.)
- Cards can be hand-ordered within a column. The order is shared between users, and is uzi's own, not stored on the forge. [user 2026-07-20]
- A per-user toggle shows open issues that lack the `PRD` label, so the board can be used to see untriaged work. Off by default. [user 2026-07-20]
  - Non-PRD cards are visually distinct and cannot start runs.
  - They move between columns like any other card (including into In Progress).
  - A `Promote` action adds the `PRD` label, making the card a normal one.
- Authorized behavior change: the board can also DISPLAY open issues without the `PRD` label (opt-in, off by default); agents still work only `PRD`-labeled issues. [user 2026-07-20, narrows the Feature #2 line "Board/agents work only issues carrying the `PRD` label"] [superseded by PRD #764: agents work issues carrying the single `uzi` label; the board still shows all open issues and the toggle/Promote now key on `uzi` (AI-synced 2026-08-29)]

## Feature #113 — Worker upgrade & version health

Tracked as GitLab issue vtmocanu/uzi#113; PRD at `prds/done/113-worker-upgrade-status.md`.
Mock at `prds/mockups/113-worker-upgrade-status-mock.html`.

- A worker's reported version must be the release it is actually running — no more a frozen informational string. [user, accepted from the mock 2026-07-22]
- Workers needing attention are those that **failed to upgrade** or are **behind**; a worker mid-upgrade is informational and must not raise an alert. [user 2026-07-22]
- Worker-roll waiting is informational until its overlap with the current suitable-worker drain reaches 24h; fleet.capacity then reports danger for an overdue wait while workers upgrade. (AI-synced 2026-10-03)
- Diagnostics are **read-only** in v1: no restart, retry, or auto-rollback of a failed upgrade. [user 2026-07-22]
- Dark-only, matching the product's two dark themes; no light variant. [user 2026-07-22] [superseded by PRD #1167: the panel is token-driven and now renders in every theme, light included; the light themes were approved in that PRD's decision log 2026-09-07 (AI-synced 2026-09-08)]
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
- A run's owner can choose which of their Anthropic tokens a run spends — at run start, while parked, at the plan-approval gate, or mid-run — overriding the worker's binding for that run; an `auto` run parked on an exhausted account also fails over by itself to a pooled account with headroom, while a pinned or default choice is never moved automatically. [user, #1247]

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
- When a run parked on a usage limit resumes, the owner gets a Slack message in the run's thread. [user 2026-09-05, PRD #1116]
- A run's displayed duration (runs list, board card, run and issue views, `uzi run list`, the TUI) spans from its first start to now or to its finish, parks included, even after a resume that gives it a fresh `RUN_TIMEOUT` wall; the timeout budget stays measured per resumed leg, and each resume path keeps its existing `started_at` handling. For a run started before this shipped, the duration counts from its latest start before the upgrade, or, if `started_at` was NULL at the upgrade, from its first start after it, so its earlier legs are not counted. (AI-synced 2026-10-01, #2004)

## Feature #1190 — Pause and resume a run on demand

Tracked as GitHub issue vtmocanu/uzi#1190; PRD at `prds/1190-run-pause-resume.md`.

- A run's owner can pause it (after the current milestone, or at once) and resume it later; the run parks on a pushed checkpoint, spends nothing while paused, and its budget clock stops. [user, #1190]

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
- A long implementation turn's committed work must have a best-effort opportunity
  to reach a worker-independent checkpoint within a bounded interval, without
  waiting for a milestone. A busy clone, a blocked or untrusted secret scan, or a
  failed publish can prevent that checkpoint, and the work is then never reported
  as durable. [user, #1597] (AI-synced 2026-09-24)
- When the checkpoint is not published, the shutdown feed must name a bounded
  reason class. [user, #1597] (AI-synced 2026-09-24)

## Feature #1392 — Park a run on a transient forge failure at clone or fetch

Tracked as GitHub issue vtmocanu/uzi#1392; PRD at `prds/1392-forge-unreachable-preclone-park.md`.

- A transient forge failure (DNS/connect/5xx) at clone or fetch parks the run and auto-resumes it, rather than failing it; it fails only after a bounded number of forge parks (`RUN_FORGE_UNREACHABLE_MAX_PARKS`). A permanent forge error (401/403/404) still fails at once. [user 2026-09-15, PRD #1392]

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
- The warning's remedy names the Homebrew formula owning the executable, resolved without a `brew` subprocess; unknown ownership falls back to the stamped channel (`-rc.N` selects `uzi-cli-rc`, otherwise `uzi-cli`). (AI-synced 2026-10-03, #2180)

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
- The run detail opens on the run and its newest messages first and fills older history in the background, so a slow link never sits on an empty pane; refetches continue from what is already held. [user, #1137]
- The run view's crew rail folds by itself when the expanded roster would push MILESTONES, SPEND or the run's own account meters off the rail; a roster that fits stays open; `c` overrides for that run. [user 2026-09-12, #1257]

## Feature #1093 — Pause all schedules

Tracked as GitHub issue vtmocanu/uzi#1093; PRD at `prds/done/1093-pause-all-schedules.md`.

- A user-level switch pauses every schedule the user owns, on every repo, catalog defaults and user-authored alike, with an optional auto-resume instant. [user]
- An expired auto-resume time resumes on its own, with no background job. [user]
- While paused: a recurring schedule keeps its cadence and records a "paused" skip, so nothing replays on resume. [user]
- While paused: a one-time schedule that came due waits and fires once after the pause ends. [user]
- `Run now` still works while paused; runs already in flight are not stopped. [user]
- Per-schedule toggles are left untouched, so resuming restores the exact prior set. [user]
- Reachable from the Schedules page, the CLI (`uzi schedule pause-all --until <when>` / `resume-all` / `pause-status`), and shown in the schedule's last-fire record. [user]

## Feature #1140 — Anthropic bind mode defaults to auto for every new worker and for the judge lane

Tracked as GitHub issue vtmocanu/uzi#1140; PRD at `prds/done/1140-bind-mode-auto-defaults.md`.
Extends Feature #111 (auto-select) and issue #804 (ephemeral default).

- Every new worker — external (join-token mint) or hosted (provisioned) — defaults its Anthropic bind mode to auto-select when the owner has a pooled token, else the default token: the SAME rule ephemeral/throwaway workers already use (#804). A worker pinned to a named token stays pinned; existing workers are not retroactively changed. [user 2026-09-05]
- The judge lane (run retrospectives and self-improvement runs) gets that same auto mode as its DEFAULT, spreading retrospectives across the owner's pooled tokens instead of always billing one fixed account. On an empty pool the judge spends the default token (it does not hold). [user 2026-09-05]

## Feature #1167 — Lights on: light themes, system-follow appearance, typeface

Tracked as GitHub issue vtmocanu/uzi#1167; PRD at `prds/done/1167-lights-on-themes.md`.
Mock at `prds/mockups/1167-lights-on-themes-mock.html`.

- Three light themes — Dawn, Hall, Shadow — a light option for a dark-first tool, each keeping the dark factory somewhere on screen; ship all three, Hall the recommended light default. [user 2026-09-07]
- Theme ids and labels are English (dawn/hall/shadow), no Romanian anywhere. [user 2026-09-07]
- Appearance mode System / Lights on / Lights off: System follows the OS; the other two hold one preferred theme per polarity, so the user picks one light theme and one dark theme. [user 2026-09-07]
- Settings vocabulary is "Lights on" / "Lights off"; Appearance is its own Settings tab, second after Account & tokens, and also holds the per-device Demo mode toggle. [user 2026-09-07; AI-synced 2026-09-08 (#1208): promoted from a card inside Account & tokens to a dedicated tab, and Demo mode moved onto it, per the user's 2026-09-08 review]
- Admin sets the instance defaults (mode plus a theme per polarity plus typeface); a user override wins — extends Feature #21's server-side theme default. [user 2026-09-07]
- Per-user typeface — System or IBM Plex — independent of theme; bundled, no external font fetch. [user 2026-09-07]

## Feature #1202 — On-demand MR rework

- An owner can start one MR rework cycle on demand from a completed run's page or with uzi run rework, with optional guidance, even after the automatic cap; on-demand cycles never count against the cap. [user, #1202]

## Feature #1226 — Structural completion interlock

Tracked as GitHub issue vtmocanu/uzi#1226 (parent epic #1225); PRD at `prds/done/1226-structural-completion-interlock.md`.

- An issue run may open a closing PR only after every in-scope approved milestone is declared complete against a frozen contract for the exact final head, or the owner records an explicit later decision. An incomplete attempt returns to the same lead and otherwise holds without discarding its work. [user, #1226]
- (AI-synced 2026-09-25) The structural completion interlock applies by default to new, unseeded issue runs on both Claude and Codex. A Codex interlocked run needs a worker advertising `codex_completion_interlock_v1` in addition to the shared completion protocol; unmet milestones return to the same Codex thread before a recoverable hold. [AI-synced, #1226]
- (AI-synced 2026-09-13) That explicit later decision is delivered by #1227 as three bounded, revisioned, owner-only decisions — continue, partial (reduce scope by exact milestone ids → a `scope_reduced` delivery that never closes the issue) and accept (waive exact named criteria with a required reason, named in the closing PR); each creates a new contract revision and invalidates every prior permit, and the lead has no route to the decision authority. PRD at `prds/done/1227-completion-owner-decisions.md`. [AI-synced, #1227]
- (AI-synced 2026-10-04) An owner `scope N` / `stop` directive authorizes a count-capped slice of the immutable frozen milestone list through an explicit capped permit: a `scope_capped` delivery never closes the issue; reaching the full frozen total remains a full delivery eligible to close the issue. [AI-synced, #2080]

## Feature #1265 — RC-first release train

Tracked as GitHub issue vtmocanu/uzi#1265; PRD at `prds/1265-rc-release-train.md`.

- Releases are cut as release candidates by default; a stable release is promoted from the candidate's own commit. Stable-facing update surfaces (the `uzi-cli` formula, the GitHub Release marked latest, and the TUI update prompt for stable installs) never surface a candidate; the `uzi-cli-rc` formula and its TUI prompt follow the newer of stable and RC releases without changing formula ownership. Unknown-owner RC builds get the same selection with release notes only. Tap writes skip equal/older versions and refuse unreadable or malformed current versions. (AI-synced 2026-10-03, #2180)

## Feature #1349 — Recovery custody hardening

Tracked as GitHub issue vtmocanu/uzi#1349; PRD at `prds/1349-recovery-custody-hardening.md`.

- An owner can see retained unpublished committed work, recover an available archive, and explicitly discard one exact held source only after a warning distinguishes recoverable work from a possible only copy. [AI-synced 2026-09-14, #1349]
- An older generation's custody hold adopted by a same-worker resume is released automatically on server-proven ancestry of its candidate commits against the completed run's published branch head (or, as before, once its own capture is durably archived); otherwise it is retained. [AI-synced 2026-09-24, #1582] For inventory-guarded holds, an archive with prerequisites does not authorize final custody release; the source remains retained until a verified final disposition or an explicit owner discard. (AI-synced 2026-10-08, #2476)
- The fixed per-owner custody admission limit is 8. Total `open_holds` stays unchanged; `admission_counted_holds` excludes at most one non-decision hold per run backing an exact-owner/run/live-run, unreleased current-generation claim on the matching same-owner existing worker with a nonnull heartbeat inclusively at or after the configured WorkerHeartbeatStale cutoff, in claimed/running/awaiting_approval/awaiting_input/awaiting_followup. Unknown, mismatched, stale, released, terminal, out-of-allowlist and old/future-generation claims remain counted, as do needs_action/source_only decision holds; attention=active alone is not eligibility. A requeued previously claimed run bypasses owner admission with 1–7 own total open holds, losing that exemption at 8. Accounting releases/discards nothing; cleanup and release safeguards still use any relevant open holds. The STABLE statement-snapshot gate allows concurrent-claim and later-staleness overshoot without serialization. Capacity surfaces use admission count and show total custody separately; clients fall back to total only when the new API field is absent, preserving explicit zero. Approved #2445 plan amends #1296 admission policy and retains #1751's continuation bound; see ADR-2445. (AI-synced 2026-10-07, #2445)
- An older generation's hold adopted by a same-worker resume can also be released while the resumed run is still live, on server-proven ancestry against the checkpoint (or task branch) the run published; a cross-worker predecessor stays held. [AI-synced 2026-09-26, #1751]

## Feature #1810 — A finished run's checkpoint ref is retained, not deleted, while custody is open

Tracked as GitHub issue vtmocanu/uzi#1810; PRD at `prds/1810-retain-failed-run-checkpoint-ref.md`.

- A finished run's last published checkpoint ref stays on the forge while any of its custody holds is open — a failed or cancelled run, but also a completed run that still has an older generation's open hold — instead of being deleted at the terminal transition; a new run on the same branch moves it to a per-run recovery ref rather than being blocked by it, and it is deleted only once the run's last hold is released or discarded. [AI-synced 2026-09-27, #1810]

## Feature #1867 — A failed run's last published checkpoint gets a bounded, run-scoped salvage copy

Tracked as GitHub issue vtmocanu/uzi#1867; PRD at `prds/1867-failed-run-salvage-ref.md`.

- On a forge listed in `UZI_SALVAGE_FORGES` (default empty: off), a failed, checkpoint-eligible run's last published checkpoint is copied to a run-scoped `refs/uzi-salvage/<run-id>`, only when it is still verified live under the branch checkpoint ref or its recovery ref, and expires after `UZI_RECOVERY_READY_RETENTION`; a `push_secret_blocked` failure is never salvaged, and salvage never deletes or moves the branch checkpoint ref or a recovery ref, which stay #1810's to manage. (AI-synced 2026-09-28)

## Feature #1907 — Product tokens and a stable `/api/v1`

Tracked as GitHub issue vtmocanu/uzi#1907; PRD at `prds/1907-product-tokens-api-v1.md`.

- An admin registers an external product; a user then mints a `uzp_` product token for it in Settings → Access, which acts as that user (once job endpoints exist, on the user's own worker and model credential) but only on the stable, versioned `/api/v1` and never with admin authority, and is refused on every other route exactly like an unknown token. A user holds at most 10 active manually created tokens per product (connections made through OAuth, #1910, are counted apart), chooses an expiry (30 days, 90 days by default, 1 year or never), and can revoke one token or use the existing Revoke all, which now covers product tokens too; an admin can revoke one product token or disable or delete a product, which cuts off all its tokens on their next request. A password change and logout do not revoke them. `/api/v1` changes are additive only, with a deprecation window of at least two minor releases and 90 days. Only `GET /api/v1/whoami` exists so far; job endpoints come later. (AI-synced 2026-09-29)
- `/api/v1` now serves `whoami` and the jobs endpoints of Feature #1908. (AI-synced 2026-09-30)
- `/api/v1` returns 401 for a refused token and 503 `auth_unavailable` when the token store cannot be read, so a caller can retry instead of dropping a good token. (AI-synced 2026-10-02)

## Feature #1908 — Repo-less jobs over `/api/v1`

Tracked as GitHub issue vtmocanu/uzi#1908; PRD at `prds/1908-repo-less-jobs-api.md`.

- A `uzc_` or `uzp_` caller can create a repo-less `research` job over `/api/v1/jobs` with a prompt and inline text inputs, and can follow, list, cancel and read its structured result (a report plus findings). The job runs as the token's user on that user's own Claude credential, with no plan gate. (AI-synced 2026-09-30)
- A job never touches a repo, a branch, a forge or `main`. It has no web or network tool unless it is bound to a site list its caller may use; then its only network tool is the uzi fetch tool, on the isolated lane. The api refuses forge, memory, publish and review worker routes for it. (AI-synced 2026-10-01)
- Docker-tier workers and worker images without the job runner never claim a job. (AI-synced 2026-09-30)
- A `uzp_` token creates only the job types its product's admin-set allow-list names (empty allows none) and sees only its own product's jobs. A `uzc_` token may create any type and sees all of its user's jobs. (AI-synced 2026-09-30)
- A `uzp_` token may bind a job only to the site lists an admin allowed its product (none by default; refused otherwise, with no job created); a `uzc_` token may name any existing list. Removing an allowance affects only jobs created afterwards. Admins grant and remove allowances in the web UI (cookie-only writes); the CLI only lists them. (AI-synced 2026-10-01)
- A job naming a site list is refused at create (503 `isolated_lane_unavailable`) when the instance has no isolated lane enabled, rather than queued; no lane worker is provisioned while the lane is off. (AI-synced 2026-10-01)
- Revoking the creating product token, disabling or deleting its product, or deactivating the owner cancels the job. Token expiry alone never does. (AI-synced 2026-09-30)
- A job fails rather than waits: a usage limit, its time budget, a disabled credential or an ephemeral worker that cannot serve it ends it `failed`. (AI-synced 2026-09-30)
- A new job with no caller wall budget freezes min(max(12h, RUN_TIMEOUT), max(RUN_WALL_CEILING, RUN_TIMEOUT)) at creation. An omitted wall preserves the existing base even above the derived ceiling; positive caller values, including shorter values, are capped at RUN_WALL_CEILING. Jobs remain non-extendable. (AI-synced 2026-10-07, #2279)
- Each user has a cap on active jobs (default 10, admin-tunable) and a per-user `/api/v1` rate limit. (AI-synced 2026-09-30)
- Job runs appear in the web runs list and run detail with their report and findings. `uzi job create|get|result|cancel|list` mirrors the API. (AI-synced 2026-09-30)
- `/api/v1` breaking changes fail `gate:repo`. (AI-synced 2026-09-30)

## Feature #1909 — Job files and product skill sets

Tracked as GitHub issue vtmocanu/uzi#1909; PRD at `prds/1909-job-files-product-skills.md`.

- A `uzc_` or `uzp_` caller can upload input files for a job and download the job's input and output files; a caller sees only files of jobs it can see, and a product only its own uploads. (AI-synced 2026-09-30)
- File types are checked by content, not name; files are stored sealed and served only as downloads (attachment, `nosniff`, `application/octet-stream`). (AI-synced 2026-09-30)
- Per-file, per-job, per-owner and instance caps apply, and job files and run recovery archives share one hard stored-file budget; recovery wins by reclaiming expired then oldest finished-job files, never those of a live job. (AI-synced 2026-09-30)
- An output over a cap or quota is refused and listed; the job still completes. (AI-synced 2026-09-30)
- Worker-produced output files are read through pinned no-follow directory descriptors and one regular, single-link file handle for hashing and upload. A symlink component or unavailable descriptor anchoring refuses the file visibly; it never redirects a read outside the workspace. (AI-synced 2026-10-01)
- A result's `source_url` is set only when the file's hash matches a page the same job fetched; nothing the agent claims sets it. (AI-synced 2026-09-30)
- An admin can give a product a skills repo (allowlisted base URLs, write-only clone token); synced skills reach that product's jobs only after the admin approves the exact commit, and no other run ever receives a product skill. (AI-synced 2026-09-30)
- New jobs run only on workers that advertise `job_files_v1`. (AI-synced 2026-09-30)

## Feature #1910 — Connect uzi (OAuth for external products)

Tracked as GitHub issue vtmocanu/uzi#1910; PRD at `prds/done/1910-connect-uzi-oauth.md`.

- An admin registers a product as a confidential OAuth client: exact redirect URIs, the scopes it may request and a `uzs_` client secret that is shown once. A product missing any of these is not a client; pasted `uzp_` tokens keep working for every product. (AI-synced 2026-10-01)
- A product sends the user to uzi (authorization code with PKCE S256); uzi always shows an explicit consent page, and the user approves or denies. No public clients, no implicit or password grant, no OpenID Connect provider features. (AI-synced 2026-10-01)
- The product gets a one-hour access token that goes through the same `/api/v1` enforcement as a pasted token, and a non-rotating refresh token valid 30 days idle and 90 days from the user's latest consent. (AI-synced 2026-10-01)
- Users see and revoke their connections in Settings → Access → Connected products; admins see and revoke a product's connections on its card; the CLI lists them read-only. Revoke all includes connections. (AI-synced 2026-10-01)
- Revoking a connection or one of its tokens cancels the jobs it created. A password change and logout do not revoke a connection. Disabling a product refuses its connections' access until it is re-enabled; deactivating a user refuses access too, and a product may then drop the connection, so reactivation restores only the connections a product kept. (AI-synced 2026-10-01)
- Narrowing or clearing a product's registration leaves existing access tokens with their scopes until they expire (at most one hour); disable the product to cut access at once. (AI-synced 2026-10-01)

## Feature #1390 — Api outage does not disturb a run on a still-live worker

Tracked as GitHub issue vtmocanu/uzi#1390; PRD at `prds/1390-outage-requeue-readoption.md`.

- An api outage does not disturb a run executing on a still-live worker: within one worker heartbeat of the api's return each such run is restored to the exact status it held (running, or waiting at its plan/question gate), without spending its re-queue budget or opening a new custody hold. [user, #1390]
- A worker that has genuinely died still has its runs re-queued after the stale window, and failed only after a second window. [user, #1390]
- Each worker reports which runs it is executing and in which phase, visible in `uzi worker list` / `uzi admin workers`. [user, #1390]

## Feature #1393 — A finished run's outcome survives an api outage

Tracked as GitHub issue vtmocanu/uzi#1393; PRD at `prds/1391-worker-outbox-durable-reports.md`.

- A run that finishes while the api is unreachable has its completed/failed outcome recorded durably on its worker before the first send, and that outcome — with its merge request — lands once the api returns and the run's own message backlog has replayed; it is never redone and never falsely marked failed by the outage. (AI-synced 2026-09-17)
- While a worker still holds an unsent outcome for a run, no second execution of that run ever starts. (AI-synced 2026-09-17)
- An outcome the api permanently refuses is shown on the run as held on the worker, and is resolved only by the owner explicitly discarding it, never on a timer. (AI-synced 2026-09-17)

## Feature #1742: A worker restart after the agent finished does not fail the run

Tracked as GitHub issue vtmocanu/uzi#1742; ADR at `adr/1742-finalize-resume-allowance.md`.

- In initial episode 0 only, a worker restart after the agent finished but before the outcome was durably recorded gets the once-per-run finalize-resume allowance with a positive `RUN_MAX_REQUEUES`, exhausted episode allowance, unused lifetime `finalize_resume_generation` and valid exact-generation attested proof. Completion remains through the normal later-generation completion path (including the completion permit where interlocked). Owner-started episodes cannot exceed the configured automatic cap or renew that marker. (AI-synced 2026-10-07; approved #1742/#2394 qualification)
- The residual window stays: before the finalize record is durable, or after a slow restart without timely attested proof, ordinary disposition applies: requeue under the episode budget; at exhaustion, hold with recorded recovery evidence, unresolved custody or uncertainty, otherwise `worker_lost` without proving absence of unrecorded worker work. (AI-synced 2026-10-07; approved #1742/#2394 qualification)
- The extra allowance is once per run and disabled at `RUN_MAX_REQUEUES=0`, which grants no automatic requeues. Owner Resume of an exhaustion hold still queues one explicit attempt at 0. Ordinary under-budget requeues do not stamp or overwrite the lifetime marker. (AI-synced 2026-10-07; approved #1742/#2394 qualification)
- After such a restart, `uzi run recovery` is honest about the source. A finalization-pinned head verifiable in the local repository and not yet on the default branch can produce an exportable archive (`uzi run export`). Ordinary unguarded archive-backed holds become `archive_ready`; the narrow exception is an OPEN hold for `recovery_wait` / `worker_requeue_exhausted`, classified `needs_action` after failed latest capture, otherwise `source_only`, before archive readiness or capture progress. An available archive remains exportable independently of attention; earlier archives may omit latest worker-local work, and preparing/uploading captures are not yet downloadable. Without an archive, retained source may be the only copy and export is unavailable. Inventory-guarded warnings and safeguards remain: an inventory-guarded hold stays `source_only` (custody open) even while an archive is downloadable, whether coverage is incomplete or recovery requires external commits, and availability and byte verification do not establish independent recovery. (AI-synced 2026-10-08, #2476) (AI-synced 2026-10-08; approved #1742/#2394/#2445 qualification)
- Early-cut limit: a restart before fetch-back and the finalization pin gives generation G's own hold no archive. A resumed claim can capture predecessor work under G+1 only if the source clone survives. At exhaustion, unresolved source custody holds for owner Resume with `source_only` and no archive; no recorded recovery evidence or unresolved custody keeps `worker_lost`, without proving absence of unrecorded worker work. On a Docker-lane worker the clone does not survive pod loss. (AI-synced 2026-10-07; approved #1742/#2394 qualification)

## Feature #2394 — Owner-resumed worker-death recovery episodes

Accepted plan approved 2026-10-07; qualifies #1742 above. See [ADR-1742 amendment](../adr/1742-finalize-resume-allowance.md#amendment-2026-10-07--2394-owner-resumed-recovery-episodes).

- Worker-death exhaustion is decided from recorded server state under the existing transition locks, without a forge lookup. A persisted checkpoint_tip or available capture, pending publication/capture, unresolved source custody, or unknown evidence holds the run for explicit owner resume. No recorded recovery evidence or unresolved custody keeps worker_lost; this does not prove that no unrecorded work survives on the worker.
- Each owner resume of an exhaustion hold starts a recovery episode with at most RUN_MAX_REQUEUES automatic re-queues; 0 grants none. The lifetime charged re-queue counter and generation-proven readoption refunds are preserved. Timer, credential, vault and ordinary input events do not release this hold; there is no innocent-sibling exemption.
- The once-per-run finalize-resume allowance applies only in the initial episode, before an owner-started worker-death recovery episode. It never exceeds the configured automatic allowance in an owner-started episode.
- Custody attention and capture availability are independent at exhaustion: an available archive or a latest preparing/uploading capture does not remove the owner decision or prove latest-work coverage. Historical evidence remains until owner Resume or Cancel; capture expiry neither fails nor promotes the hold, custody is never implicitly released, and account/extend/approval/follow-up/pause events do not release or renew the allowance. Owner Resume queues one explicit attempt, including at 0, through normal claim-generation and released-incarnation fences. Existing wall budget is preserved; held time is banked only for an already-started clock and old approval/input waits are banked at park. (AI-synced 2026-10-08, #2394/#2445)
- With N = RUN_MAX_REQUEUES and no other fresh executor execution, worker-death retries contribute N+1 attempts (initial N+2 only with the once-per-run extra allowance and positive cap), with the same conditional multipliers for QUESTION_MAX and QUESTION_TIMEOUT_SECONDS. Ordinary transient/limit/credential redispatch can create fresh execute() within the same episode, resetting worker-memory question budgets without changing the episode or charged requeue_count. Charged automatic worker-death retries remain bounded by N per owner-started episode; the extra is initial-only. RUN_MAX_REQUEUES gives no unconditional per-episode or lifetime question/attempt ceiling; this clarifies the approved design and introduces no runtime budget. (AI-synced 2026-10-07)
- CLI get shows the limit, episode used/remaining, lifetime charged count and historical evidence/uncertainty; JSON has typed `worker_recovery`. Default wait stops on `worker_requeue_exhausted`, explicit `--until` keeps its status set, logs `--follow` notices cause changes within the same status, and the TUI marks NEEDS YOU. The run page enables Resume/Cancel only after the existing owner-only GET inputs returns 200; pending/non-owner views are inert. Shared board LatestRun has no cause/count metadata and directs readers to open the run. No self-retry countdown applies to this cause, even with a due retry timestamp.
- Checkpoint copy: `The server recorded checkpoint <tip> before this hold. Current availability and the latest local edits are not verified.` Capture copy: `A recovery capture was recorded as available at <time>. It may later expire or be discarded; it may not contain the latest local edits.` Pending publication/capture, retained source custody or unavailable server evidence gives no archive/export guarantee or promise that a local clone survives.

## Feature #1995 — A failed recovery upload is retried without a worker restart

Tracked as GitHub issue vtmocanu/uzi#1995; ADR at `adr/1296-durable-run-recovery.md` (amendment 2026-10-01).

- A recovery bundle whose upload failed is retried while the worker stays up, once the api is reachable again, with bounded exponential backoff; no worker restart is needed. (AI-synced 2026-10-01)
- An upload the api permanently refuses (ownership lost, route refused, over the size cap) or whose local bytes no longer match what was journaled leaves the worker's record `needs_action` with its bundle and source pin kept, and is not retried automatically. (AI-synced 2026-10-01)
- A credential rejection is retried only after the worker authenticates again. (AI-synced 2026-10-01)
- A run the worker is executing is not selected for the retry. (AI-synced 2026-10-01)

## Feature #1293 — Failed-run rate on the dashboard (global and per user)

- The dashboard shows a failed-run percentage: global for admins, per user for everyone, and per user in the admin table. [user 2026-09-12]
- In the admin per-user table the Failed and Fail rate columns come right after Runs, and Cost sits last before Share. [user 2026-09-12]

## Feature #1418 — "Needs landing" bucket for failed runs whose work is human-landable

- A failed run whose committed work is still human-landable surfaces a secondary "needs landing" presentation bucket (`landing_state`), derived server-side from the run's `fail_origin` and whether its work is recoverable (a preserved diff or an available durable-recovery archive). The four human-landable origins are the publish-time failures `finalize_base_align_conflict`, `workflow_scope_missing`, `push_secret_blocked`, and `history_rewritten`. [AI-synced 2026-09-19, #1418]
- The per-run bucket renders in the web run list and run page, the TUI, `uzi run list` / `uzi run get`, and Slack run-finished copy. [AI-synced 2026-10-07]
- The dashboard keeps one failed bar segment and shows a per-window "failed runs with recoverable work" note, qualified because work may already have been landed; CLI usage summaries call the subset "recoverable". [AI-synced 2026-10-07]
- These runs still count as failures — they extend, not amend, the #1293 failed-run rate (the factory did not publish its output). [AI-synced 2026-09-19, #1418]
- The judge skips retrospecting the environment-caused subset (`finalize_base_align_conflict`, `workflow_scope_missing`, `push_secret_blocked`); `history_rewritten` stays judge-eligible as an agent defect. [AI-synced 2026-09-19, #1418]

## Feature #1484 — Admin in-app health

Tracked as GitHub issue vtmocanu/uzi#1484; PRD at `prds/1484-admin-health-tab.md`.

- An admin gets a read-only, closed registry of checks over what uzi knows about itself (worker rolls, queue and capacity, controller liveness, background loops, the database, integrations, housekeeping). Health, Overview, overall status/counts, attention pips, history and `uzi admin health` retain all checks; the app-wide Danger banner, snooze, episodes and admin notices follow the server's separate `blocking` field for instance danger. Owner-only danger remains visible and exits CLI code 8 without an episode, admin DM or banner. [AI-synced 2026-10-05, #2293]
- Health is the **last** admin tab, not the first; the sidebar pip and the Overview card are the entry points. [AI-synced 2026-10-03, #1484]
  - Admin tab order: Users, Rate limits, Tool allowlist, Blocked repos, Instance, Branding, Site lists, Products, Health. Site lists and Products follow Branding, with Health still last. (AI-synced 2026-10-03)
- The Danger banner carries a "Snooze 1 h", per admin, per open episode; a new episode shows the banner again. [AI-synced 2026-09-20, #1484]
- A non-admin gets a platform line on Overview instead of the admin card, derived only from their own runs and workers, so they can tell a platform problem from a problem with their own run. [AI-synced 2026-09-20, #1484]
- In-app health never reads the Kubernetes API; the api holds no kube credential, and no action (restart, retry, rollback, cordon) is offered — diagnosis only. [AI-synced 2026-09-20, #1484]

## Issue #2271 — Findings stay in the backlog; admin notices cover instance danger

- Findings remain captured, stored, listed and available in stream cards and the backlog for filing and dispositions; they send no Slack DMs and create no new notification latch rows. [AI-synced 2026-10-05, #2271]
- #2293 supersedes #2271's literal-ID selection: the server registry gives each check `scope: instance` or `scope: owner`; `db`, `controller.report`, `loops`, `fleet.roll` are instance (including a single owner's fleet), the rest owner. Every document emits `blocking`, true exactly when any instance check is danger; admin notices select instance-danger checks by scope. Owner run-health DMs retain their existing routing. [AI-synced 2026-10-05, #2293]
- #2293 explicitly supersedes #2271's accepted owner-bridged episode timing: `blocking` opens/holds episodes; the opening tick sends no notice, the next still-blocking tick claims one per admin. Clearing instance danger closes and rearms even while owner danger remains; owner-only danger cannot open/hold an episode. [AI-synced 2026-10-05, #2293]
- The accepted #2293 follow-up replaces the originally deferred mixed-version fallback: web honors present `blocking` true/false exactly with scoped count/cause and no client ID map; absent `blocking` conservatively uses legacy status/danger count/first danger, including snooze expiry. Api-before-web upgrade / web-before-api rollback is preferred to avoid legacy owner false positives, not required to prevent suppressed banners. Owner-only danger reads "N checks need attention; no instance-wide blocker detected" (singular for one); warn/unknown copy makes no work-flow claim. CLI adds `blocking: true/false (instance-wide)` without changing overall exit 8, strict, transport or malformed handling. [AI-synced 2026-10-05, #2293]
- `queue.waiting` evaluates the full waiting population, excluding only waits with the exact stored `workersvc.ReasonWorkersUpgrading` value confirmed with finite, nonzero, nonfuture wait/drain times and eligibility `DrainingEligible > 0`, `NonDrainingEligible == 0`, `SuitableOwnDraining == 0`. Capacity-first shared confirmation memoizes by run, caps at 200 calls, 2s per call and a lazy shared 4s budget; failures/elapsed deadlines/exhaustion/invalid inputs leave genuine waits. Genuine waits warn at 10m, danger at 30m; unknown age yields unknown unless a valid wait establishes danger. Confirmed drains stay excluded even overdue; capacity retains D18's >=24h later-wait/drain overlap, independent of controller deadlines. Evidence caps at five sanitized run/owner/wait/reason rows plus omissions; severity uses the full population. [AI-synced 2026-10-05, #2293]

## Feature #1594 — Codex provider-rejection surfaces as re-login required

Tracked as GitHub issue vtmocanu/uzi#1594.

- A Codex login the provider rejects (an allowlisted refresh-token rejection code, on a 400/401) surfaces to its owner as re-login required with the reason that the provider rejected it; any other refresh failure stays ambiguous and never releases or re-spends a token. (AI-synced 2026-09-24)
- The documented way to add a Codex login is a login dedicated to uzi, isolated from the owner's everyday Codex CLI; a re-paste after a failure comes from a new isolated login. (AI-synced 2026-09-24)

## Issue #1593 — A prose-only plan turn parks for the owner instead of failing

Tracked as GitHub issue vtmocanu/uzi#1593.

- A gated plan (or plan-revision) turn that ends with prose only — no `submit_plan`, no `ask_user`, just lead text — gets exactly one corrective nudge, resuming the same session with fixed text; no second nudge is given on the same planning attempt. [user, #1593]
  - A planning attempt is one call of the plan or revision turn. The nudge and plan-missing-park budget is not persisted: anything that runs that turn again (a requeue, a recovery-wait or limit-wait resume, a wall-park extend, or a credential-switch give-up re-running the first plan turn) starts a fresh attempt with its own budget. How many attempts a run can get is therefore set by those re-drive mechanisms, not by this flow. The clarification-park budget is likewise not persisted and restarts whenever the executor is re-entered. [AI-synced 2026-09-24, #1593]
- If the nudged turn is still prose only, an attended run falls back to an attended park: a fixed, uzi-authored question ("Plan missing"), emitted by the worker, marked worker-authored, not the lead's. [user, #1593]
- The lead's final message is never put in the question, its header, its options, or the prompt that resumes planning after guidance — it appears only in a separate, bounded, escaped, secret-scrubbed field on a feed status card. [user, #1593]
- The owner's only choices at that park are to give guidance (which resumes planning in the same session) or to cancel the run — there is no "revise the PRD and retry" option, because an answer cannot refresh the checked-out PRD or branch; changing the PRD means cancelling and re-dispatching. [user, #1593]
- An answer is never plan approval: after guidance, planning still has to produce a plan, and that plan still goes through the ordinary approval gate. [user, #1593]
- The existing `awaiting_input` answer deadline (`QUESTION_TIMEOUT_SECONDS`) applies unchanged, and an unanswered attended run timing out is the intended outcome here, not a special case. [user, #1593]
- An auto-approved (autopilot) run, or any run with no one to ask, never parks: after the nudge it fails closed with the distinct fail_origin `plan_missing` and a fixed `failure_reason`. [user, #1593]
- Nothing is ever inferred from the lead's prose — it never becomes a plan, a question, or part of a later prompt. [user, #1593]
- (AI-synced 2026-10-06) Explicit draft captures are advisory activity only, never submission, approval, prompt recovery or automatic adoption; retries require fresh review. [user, #2323]

## Feature #1598 — Codex command storage no longer accumulates in the writable layer

Tracked as GitHub issue vtmocanu/uzi#1598.

- (AI-synced 2026-09-24) A Codex model-authorized command's per-command tmp still lives in `/tmp` while the command runs, and a run's per-run Codex build/module cache lives on k8s in a worker-only emptyDir, on compose in the container layer; but both are now removed (fail-closed and fd-safe: a dedicated no-follow tree-removal primitive, not `os.RemoveAll`) after the command drains or at run end, with leftovers reaped at worker startup (gated on a kernel process-table proof rather than an unlocked-name guess), so neither accumulates the way it used to. The cache is a storage/performance boundary only, never a trust boundary. The hosted-worker measurement needed to raise the worker ephemeral-storage requests for it is pending, not delivered by this issue.

## Feature #1590 — Hold a Codex run on an unavailable subscription account

Tracked as GitHub issue vtmocanu/uzi#1590; PRD at `prds/1590-codex-quarantine-claim-hold.md`; ADR at `adr/1590-codex-binding-same-identity-readmission.md`.

- A Codex subscription run whose account is quarantined or needs a re-login is held (`recovery_wait`), never failed for that; chat and judge runs keep failing. (AI-synced 2026-09-24)
- The hold has no automatic expiry; it ends by resuming, by owner cancel, or by a binding change. (AI-synced 2026-09-24)
- A held run resumes by itself once the account is usable, including after a re-login on the same credential that resolves to the same Codex identity. (AI-synced 2026-09-24)
- A binding change (the re-login resolved to a different identity, the credential was deleted, or the account credential revision or auth mode no longer matches) fails the run `credential_unavailable`. (AI-synced 2026-09-24)
- The owner sees why the run is held and what to do next (reconciling, re-log in the named credential, verifying the new login, resuming) on the web, CLI and TUI. (AI-synced 2026-09-24)

## Bug #1624 — ephemeral worker cap

Tracked as GitHub issue vtmocanu/uzi#1624.

- (AI-synced 2026-09-24) An ephemeral (run-bound) worker reports `max_concurrent_runs` 1 and never counts as a free slot for another run; persistent workers report their advertised cap.
- (AI-synced 2026-10-01) Qualifier: a leased-idle ephemeral worker (one that finished its run and is held for its lease) may take a same-owner, same-repo, same-branch follow-up run during the lease (PRD #2006); it still never counts as a free slot for any other run.

## Feature #1650 — Retire the Notifications inbox tab

Tracked as GitHub issue vtmocanu/uzi#1650; PRD at `prds/done/1650-retire-notifications-inbox.md`.

- The web Notifications inbox (tab, bell, unread badge) is retired; actionable signals use the page that owns the thing and, where their routing provides it, Slack DM (when linked). Findings use stream cards and the backlog without DMs (#2271). [user 2026-09-25, #1650] (AI-synced 2026-10-05, #2271)
- "Settings → Notifications" (Slack linking) is not the inbox and stays. [user 2026-09-25, #1650] (AI-synced 2026-09-25)
- CI auto-fix / MR rework halt DMs are delivered at-least-once: retried until posted or the owner has no Slack link (capped at about 24h); the forge halt comment stays once-only. A rare duplicate DM is accepted. (AI-synced 2026-10-01, #1675)

## Feature #1695 — Review-bot control commands never trigger an MR rework

Tracked as GitHub issue vtmocanu/uzi#1695.

- A top-level MR comment consisting solely of an allowlisted review-bot control command (CodeRabbit's `@coderabbitai review` family; Greptile's `@greptileai review` / `@greptile review`) is not actionable review feedback and never starts an mr_rework; any added prose, or an inline comment, still counts. (AI-synced 2026-09-25)

## Feature #2347 — Author eligibility for MR review comments

Tracked as GitHub issue vtmocanu/uzi#2347; design rationale in `adr/2347-review-comment-author-trust.md`.

- An MR review comment reaches an mr_rework run, and can trigger one, only when its author has repository access or is an allowlisted review bot; comments from anyone else, and from authors whose access cannot be verified in time, are withheld (omitted, with counts shown to the agent). Unknown never triggers. (AI-synced 2026-10-07)
- The instance setting `mr_review_trusted_bots` (admin-only, default empty) lists trusted bots as `<base_url>#<forge_user_id>`; a bot matches by forge instance plus numeric user id, never login. Allowlisting only permits ingestion: bot text stays untrusted, and a trusted bot's summary or walkthrough comment never triggers. (AI-synced 2026-10-07)
- Reply/resolve is allowed only on a thread with an included (eligible) comment in the run's snapshot; a wholly withheld thread is refused 403. Snapshots from before this change are replayed empty and authorize no thread. (AI-synced 2026-10-07)
- An outsider flood does not suppress an eligible finding unless the pending set (one entry per unverified author, accumulated across fires; near the cap, when superseding could not be guaranteed to fit, an author's older id is kept so an author may briefly hold two entries rather than risk losing both) exceeds 10,000 entries, but may delay it, bounded only conditionally; this departs from the issue's zero-delay criterion and the maintainer accepted it on 2026-10-07. (AI-synced 2026-10-07)

## Feature #1732 — Disable and re-enable account credentials

Tracked as GitHub issue vtmocanu/uzi#1732; PRD at `prds/done/1732-disable-account-credentials.md`.

- A user can disable any Anthropic token or OpenAI/Codex credential and re-enable it on demand. Disabled means kept (value, name, preferences) but not polled, not refreshed in the background, not selectable, hidden from the sidebar and pickers, and absent from the admin Rate limits page (no row, no count). Past runs and spend stay in history. (AI-synced 2026-09-26)
- Work pinned or bound to a disabled credential waits and resumes on re-enable; uzi never silently spends a different credential instead. A run already holding the credential finishes. (AI-synced 2026-09-26)
- Every default is enabled: disabling the default requires choosing an enabled replacement; disabling the last credential of a kind leaves that kind with no default. The Judge keeps Feature #1140's empty-pool fallback to the (enabled) default. (AI-synced 2026-09-26)
- Disabled credentials collapse into a "Disabled (n)" section, collapsed by default. Enable/disable is web-only; the CLI only shows the state. (AI-synced 2026-09-26)

## Feature #1604 — Plan-gate verdicts survive interruptions

Tracked as GitHub issue vtmocanu/uzi#1604; decision record `adr/1604-plan-gate-verdict-durability.md`.

- An approve, reject or request-changes sent at the plan gate is never lost silently across a credential switch, worker restart or resumed run (an empty change request carries nothing); it takes effect once its result is saved, and otherwise carries over to the resumed run. A carried-over verdict uzi cannot match to a plan is ignored with a feed note asking to re-send it; on Codex runs every carried-over verdict (approve, reject or request-changes) is ignored that way, and a fresh plan is shown. (AI-synced 2026-09-26)
- A verdict is never applied to a plan the owner did not see: one sent before the current plan was shown, or one uzi cannot match to a plan, is ignored, and an ignored approve never counts as approval. (AI-synced 2026-09-26)
- A resumed run with an unapproved plan reads the owner's pending verdicts before it shows a plan. On Claude runs a pending cancel, reject or request-changes acts first (changes revise the submitted plan rather than re-offering it); a pending approve waits for the gate, where it is judged like any replayed verdict. On Codex runs every pending approve, reject or request-changes is ignored with a note asking to re-send it, and a fresh plan is shown. (AI-synced 2026-09-26)
- Every ignored verdict is explained in the run feed, except an approve replaced by a newer verdict, a repeat approve after the plan was approved, and an empty change request. (AI-synced 2026-09-26)

## Feature #1783 — Confirmed run quiescence and attempt-unique clone paths

Tracked as GitHub issue vtmocanu/uzi#1783; decision record `adr/1783-run-quiescence-and-attempt-clone-paths.md`.

- Destructive clone cleanup and any credentialed sink (checkpoint publish, finalize, pause, retire, graceful shutdown) require confirmed quiescence of the run's clone first, wherever the process reaper or the Codex supervisor boundary actually runs (see the Codex gap below); an unproven state fails closed rather than proceeding. On a Claude or stub run on a Linux worker, every pause that reaches its park boundary kills the run's own background processes as part of that proof; clone-bound container teardown runs alongside it but is best-effort cleanup, never itself proof, so it never gates the pause. On a Codex run, pause, wall park, terminal retire, completion hold and the recovery and credential-switch captures do not run this process reap (the credentialed publishes among them run inside the Codex supervisor boundary) — a disclosed gap, not yet closed. The Claude/stub reap runs even when the pause cannot durably checkpoint and the run keeps running. (AI-synced 2026-09-27)
- On a Docker-wired worker, a run's clone path is never reused by a later execution attempt of the same run; each attempt seeds its own path. (AI-synced 2026-09-27)
- Foreign residue found at the CANONICAL clone path is quarantined beside it rather than left to wedge the worker; a run that cannot prove its canonical clone path clear fails with the `worker_residue_blocked` fail origin. This holds at the canonical path only: a Docker-wired worker's per-attempt path is never reused by a later attempt, and a pre-existing fresh attempt path (one that should not yet exist) fails the run closed instead of being quarantined. (AI-synced 2026-09-27)
- A resume or re-claim on a Docker-wired worker starts a fresh model session (Claude session or Codex thread); its earlier committed work still carries forward through the branch, tracking ref and checkpoints, but the conversation itself is not resumed. Unwired workers keep full same-path resume. (AI-synced 2026-09-27)
- Under the uid split, an unreadable, unattributed runner-uid process is killed in any scan mode only when no other claim or attempt is in flight, its start time still matches immediately before SIGKILL, and a rescan confirms its exit. Scanner descendants are excluded. A recorded live root is exempt by matching pid and start time. On single-uid workers or with another claim or attempt in flight, the process remains `unverified` and fails closed. No process is cleared as unrelated merely because ancestry is absent. This relies on runner-uid processes being agent-controlled code, recorded worker roots and descendants, readable worker-marked fixed-command spawns, or the scanner. (AI-synced 2026-09-28)
- On a Claude run on a Linux worker, that quiescence proof also requires a complete reap of every process carrying the run's HOME or working in its trees: a process left alive, or a reap that cannot finish, blocks the credentialed sink like any other unproven state. A re-claimed run reaps its HOME the same way before its clone fetch and fails `worker_residue_blocked` if it cannot. (AI-synced 2026-10-03, #1828)
- On a single-uid Linux worker, a runner-uid process whose environment or working directory is unreadable and that nothing ties to another live attempt latches a process-wide quarantine, released only by a container restart. While latched the worker claims nothing, starts no forge-credentialed git child and no new Claude or Codex provider turn; a turn already in flight runs to its boundary and is not killed. Runs then fail `worker_residue_blocked` with the clone, generation hold and recovery pins kept; a run stopped by its own blocking quiescence check fails with a plain residue-blocked reason (usually naming the process) and gets no archive, while a run stopped because the latch refused its turn, credentialed git or claim fails with a "this worker is quarantined" reason and, when the capture succeeds, a verified credential-free local archive of the committed work already in the worker bare; nothing is uploaded or released except a completed run's custody release and a pre-clone park's hold release. The latch is not set under the uid split, and no other unproven state latches it. (AI-synced 2026-10-06, #2213)
- A recovery capture whose quiescence proof keeps blocking is retried only a bounded number of times, then the run fails `worker_residue_blocked` with the clone kept. The failure names the unreadable process's pid and program name so an operator can stop it. (AI-synced 2026-09-28)

## Feature #1795 — Plan-gate verdicts bound to the gate revision they were sent against

Tracked as GitHub issue vtmocanu/uzi#1795; decision record `adr/1795-gate-revision-bound-verdicts.md`.

- A plan-gate approve, reject or request-changes applies only to the plan revision it was sent against; a client that shows a plan (web, CLI, Slack) sends back the revision it displayed, and a stale one is refused rather than applied to a different plan. (AI-synced 2026-09-27)

## Feature #1798 — Plain-English PR descriptions

Tracked as GitHub issue vtmocanu/uzi#1798; PRD at `prds/1798-plain-english-pr-descriptions.md`.

- Every PR uzi opens gets a plain-English description plus a computed size
  line; a human can see what it does and how big it is from the description
  alone. (AI-synced 2026-09-28)
- A Claude run and a Codex run produce the same layout. (AI-synced 2026-09-28)
- The PR body shows the size as a file count plus a small table (one row per
  category, a Total row); the web and CLI keep the one-line form. (AI-synced 2026-10-01, #2061)
- An editor-grounded multi-component or order-dependent code change may add one
  structured diagram inside the description region; uzi renders Mermaid, omits
  the diagram for zero-code or uncertain changes, and drops it before the size
  table on size limits. Web and CLI show its text outline only when the bound
  region was published with the diagram. (AI-synced 2026-10-02, #1840)
- Generated or lead-written text can never close an issue. (AI-synced 2026-09-28)
- Text outside uzi's own blocks is preserved on an ordinary refresh; a few
  named cases (no uzi markers yet, malformed markers — the publisher skips
  such a PR and the interlock's own reconcile rewrites it whole — a closing
  directive found outside the completion block, a blind rewrite of an
  unreadable PR) rewrite the whole body instead. A missing completion block
  is repaired in place instead: the publisher appends a fresh one and keeps
  everything else, and only a completion block still missing when the
  interlock's reconcile reads the PR gets the whole-body rewrite.
  (AI-synced 2026-09-28)
- The footer wording ("Opened by uzi from `<branch>`. A human reviews and
  merges; uzi never merges.") was approved by the maintainer on 2026-09-27.
  (AI-synced 2026-09-28)

## Feature #1809 — Worker disk safety for long runs

Tracked as GitHub issue vtmocanu/uzi#1809; PRD at `prds/1809-worker-disk-safety.md`; ADR at `adr/1809-per-run-cache-bounds.md`.

- A run's rebuildable caches (Go build and module cache, npm cache) stay per run, never shared between runs, and are bounded while the run lives. (AI-synced 2026-09-28)
- A park that ends a Claude run's process drops those caches and keeps everything a resume needs (session, config, unknown files); a gate-parked run keeps its gate, though its rebuildable caches may be dropped in place at the hard disk threshold, and the park itself leaves Codex runs' caches alone. (AI-synced 2026-10-01)
- The worker's periodic reclaim drops the same caches from any worker-owned HOME of a run parked with its process ended, and removes terminal runs' leftovers. (AI-synced 2026-09-28)
- A full data volume at clone/fetch or at the claim/resume check, the cache cap (uncounted) and the hard disk stop (counted) park the run in `recovery_wait` with cause `data_volume_full`. Admission and hard thresholds use the higher valid byte or inode used fraction; periodic cache-byte selection cannot guarantee this failing run parks before failure. (AI-synced 2026-10-03)
- Approved #1829 review supersedes the standing decision that later disk-full execution errors fail immediately: an issue run's direct, otherwise-untyped executor rejection may instead take one counted disk park after settlement and one bounded reclaim wait, without replaying execution, when exact-generation running ownership and a fresh full-volume sample on the actual worktree/HOME device are confirmed. Strict D6 write classification and safe worker-owned retry stay separate and unchanged. Setup, finalize and settlement failures, protected outcomes, recognizable typed, wrapped or trusted security/guardrail failures, including admission, launcher and plan-wiring refusals (pinned by the exclusion fixtures in `agent/test/runner-terminal-disk-deferral.test.ts`), unknown accounting, old APIs and approval revisions keep existing handling. (AI-synced 2026-10-03)
- This policy bounds capture to three nonblocked attempts or five consecutive blocked proofs (at most 15 alternating capture calls plus five final proof attempts); degraded parking requires affirmative quiescence and retains the original clone, journal, HOME, session and custody without claiming verified or published latest work. Same-worker resume captures dirty work before fresh execution; cross-worker recovery can lose that newer work. ACK reconciliation is separate and does not reset capture budgets; existing terminal/cancel/stale/cap cleanup still applies. (AI-synced 2026-10-03)
- Current fullness does not attribute the error: an unrelated opaque failure can be deferred. Preserved trusted types and `cause`/`interruption` wrappers remain excluded. Complete legacy reasons are recognized directly and through leading `<context>: <reason>` envelopes with a literal colon and space. Contexts may contain apostrophes; double-quoted or multiline contexts, alternate separators, quoted reason diagnostics and producer-domain near-misses are not legacy refusal envelopes. For actual trusted refusals, only completely erased-origin opaque failures remain eligible under this accepted coincidence residual. Ordinary opaque failures keep their existing handling. The lifetime cap bounds repetition, not misclassification; `UZI_RUN_DISK_PARK_MAX=0` remains unlimited. This approved correction must land before the next release; acceptance still pending in the PRD is not implied complete. (AI-synced 2026-10-03)
- Completed size samples show live or parked runs' HOME and cache bytes: `uzi run get`, each worker's largest measured run in worker lists, and the run page's park panels; admin health warns on a large run (`fleet.rundisk`). Periodic samples do not guarantee warning before exhaustion. (AI-synced 2026-10-03)
- The in-run cache cap, the hard disk stop, the periodic reclaim and the admission stop each have an off switch; the park-time cache drop and the claim/resume disk check have no off switch. (AI-synced 2026-09-28; reworded 2026-10-03)
- A park or the periodic reclaim drops a run's caches only once no process carrying the run's HOME is left alive (a complete reap or scan); otherwise the caches stay and a later reclaim pass retries. (AI-synced 2026-10-03, #1828)

## Bug #1864 — Codex delegation still open when the lead's turn ends

Tracked as GitHub issue vtmocanu/uzi#1864.

- A Codex delegation still open when the lead's turn ends is cancelled and its work settled before the next checkpoint; a boundary that still cannot settle fails closed, and the failure reason and worker log name the stage, the checkpoint and the unsettled work. (AI-synced 2026-09-28)
- A Codex run's finalize publish runs under its own finite boundary deadline sized for the full publish (push, PR description, merge request), not the 30 s checkpoint deadline; a finalize deadline failure names the finalize step that was running when it fired, and the worker logs each finalize step's duration. (AI-synced 2026-09-29)

## Issue #1932 — Pre-exit secret-scan remediation before publishing

- Before a non-interactive run's done checkpoint, the worker scans the branch's unpublished commits locally. A trusted finding entirely above the checkpoint floor returns to the lead for a history rewrite, at most 2 times; a finding at or below the floor, an exhausted cap, or an untrusted rescan after a finding fails the run `push_secret_blocked` with no push and no preserved patch (a durable-recovery archive stays exportable). A commit the mid-turn checkpoint scan flagged fails the run the same way while it is still in the pushed history. While a finding is live, checkpoint uploads are held at the shared upload seam (#1964); the park, shutdown, pause, capture and completion-hold sinks stay unscanned (#1597). The local-scan failure reason never claims GH013 or GitHub Push Protection. Limits: a secret already published by an earlier GitHub milestone checkpoint is at or below the floor and is not remediated; a token-shaped fixture on a merged public non-default branch counts as a finding. (AI-synced 2026-10-07)
- #1964 supersedes the checkpoint-body-only hold: every checkpoint upload (usage-limit, shutdown, pause, credentialed/free recovery, credential switch, completion-hold/wall and body overlay/pinned publishes) holds nonempty current-flight known/blocked findings, overflow or a retained flagged SHA ancestor; unknown ancestry or errors hold too. Boundary (`reap:true`) publishes after `everKnown` require the source `cleanTip`; `reap:false` retains its exemption from that equality check. The actual overlay candidate is guarded before `pack-objects`; a hold starts no pack producer or remote attempt. Local captures still run and are verified, with no origin durability from a held upload. A fresh pause fails and continues the run; the already-durable shortcut is unchanged. (AI-synced 2026-10-07)
- #1964 adds no scans: the unscanned sinks remain **UNSCANNED**. Unknown, never-flagged content and workflow trees copied from non-parent commits are outside remembered-SHA ancestry containment. Remediation memory is process-local `RunFlight` state, lost on reclaim/new flight, including same-worker reclaim; surviving local commits may ship in a later unscanned publish before another finding is recorded. Persistence and mandatory resume scanning remain separate work under the approved plan's explicit residual boundaries. (AI-synced 2026-10-07)

## PRD #1906 — Official-sources web research

Tracked as GitHub issue vtmocanu/uzi#1906; design in `prds/1906-official-sources-web-research.md`.

- A run bound to a site list may read web content only from hosts on that list, fetched through a uzi fetch service; it runs in a worker lane with no internet, and nothing (kill-switch, cleared requirements, self-reported capability, old agent) can place it outside that lane. (AI-synced 2026-09-29)
- Site lists are named and admin-managed; a request may name a list (a job's `egress_profile`) but never supply hosts. Admins create and edit them in the web UI (cookie-only writes), the CLI can list and show them. (AI-synced 2026-10-01)
- Every fetch attempt, allowed or refused, is in the run's source log, readable by the run owner (`uzi run fetches <run>`); a fetch that cannot be logged does not happen. (AI-synced 2026-09-29)
- The lane is off by default; existing worker tiers are unchanged, and no new image or workflow change is needed. (AI-synced 2026-09-29)

## Bug #2099 — A transient Codex provider failure is retried, not failed

Tracked as GitHub issue vtmocanu/uzi#2099.

- A Codex run turn that fails with a transient provider error (overload, internal-server or flex-capacity failure, or a transport failure with a 408/429/5xx status or none) is retried in place a bounded number of times; if it persists, the run is parked in `recovery_wait` instead of failed. A transport failure with a permanent status, authentication and other non-transport failures behave as before, except for the usage-limit requirement superseded by #2360 below. `rateLimitExceeded` and `sessionBudgetExceeded` behavior is unchanged. Cancel, pause and the wall budget keep precedence over the retry. (AI-synced 2026-10-06)
- #2360 supersedes only #2099’s usage-limit requirement: a subscription Codex turn ending in `usageLimitExceeded` parks at `limit_wait` when accepted structured `account/rateLimits/updated` evidence from that turn identifies a window limit and the existing `wait_on_limit` preference (default on) and budgets permit it. Use the latest reset among exhausted windows, or highest-used ties for `rate_limit_reached` rounding; a missing selected reset or past reset uses bounded fallback. No reset is inferred from prose, account reads or polling. Limits classified as non-window (missing/unaccepted evidence, API-key or undefined authentication, spend-control, credit/quota/plan or unknown rejection evidence) fail as typed `rate_limited` without a reset promise; opt-out reasons are server-composed with the provider and reset when known. Resume keeps the frozen Codex account, without Anthropic pooling, gauge updates or token switching; an unavailable account is refused at claim and held by Sweep at `recovery_wait` / `codex_account_unavailable`. Successful WIP recovery reuses approval; failed capture retains the source clone without promising latest-work durability, and total tree loss keeps the existing re-gating policy. A resolvable non-Docker thread resumes; Docker attempt paths keep fresh-thread lineage breaks with work restored when available. The per-park 8-day default can hold the issue lock and run-bound hosted worker/PVC; fallback and the wait-count budget can exhaust before a weekly reset. See [PRD #2360](../prds/done/2360-codex-usage-limit-park.md); persistent `rateLimitExceeded` remains follow-up #2361. (AI-synced 2026-10-06)

## Startup admin seed

- Seed an admin user from env at startup (`UZI_SEED_EMAIL` / `UZI_SEED_PASSWORD` / `UZI_SEED_NAME`) so the user survives DB wipes.
- Seeded user gets the admin role; never overwrite an existing user.

## Feature #2083 — Decisions memo for MR rework (experiment)

Tracked as GitHub issue vtmocanu/uzi#2083; an experiment to decide the next step of PRD #1214.

- An admin setting `decisions_memo_enabled` (default off) lets a Claude run that publishes its merge request save a private decisions memo (up to 8 KiB), stored with the run, owner-scoped, and never in the PR description; uzi never logs the stored memo or the tool call that saves it (the lead can still quote an injected memo in its own messages). (AI-synced 2026-10-02)
- Only a later MR rework on the same lineage (same owner, repo, branch and MR) receives the latest memo, as untrusted, advisory context; an absent memo or any fetch problem means a normal fresh rework. (AI-synced 2026-10-02)
- A failed, held or unpublished round never replaces the prior memo; turning the setting off stops writes and injection but keeps stored memos; Codex runs neither write nor receive one. (AI-synced 2026-10-02)

## Feature #2012 — Lead rework triage

Tracked as GitHub issue vtmocanu/uzi#2012.

- The lead reworks only mandatory items: blocking findings, security findings graded Medium or above, and demonstrated correctness, acceptance-criterion, data-integrity or safety-invariant violations even when labelled non-blocking; a trust-boundary class alone does not make a note mandatory, and a demonstrated defect is never deferred. (AI-synced 2026-10-02)
- Reworks use the smallest fixing change with no new design (a mandatory item needing one is re-planned as a material change); re-validation is scoped to the committed fix range, with a full wave when impact is uncertain. (AI-synced 2026-10-02)
- Non-blocking notes not reworked are recorded as grouped `deferred` scope notes in `signal_done`, never as incidental findings. (AI-synced 2026-10-02)

## Proposal agent output modes

- Feature-bingo and refactor-scout default to issues (supersedes PRD #929 D5); global fallback stays mr; stored mr/issues modes remain authoritative, NULL inherits the catalog default, and reset adopts that default; no migration. (AI-synced 2026-10-02)

## Feature #2278 — Docker-capable ephemeral workers

- Add a per-user persisted **Docker-capable** checkbox beside **Auto-provision on demand**, on one row with one shared paragraph; hide it without the instance Docker tier (including an old API omitting the flag), keep it usable and retain its value while auto-provision is off, save `{docker}` separately from `{enabled}`, disable competing writes while pending, update auth on success, and restore the confirmed value with a local error on rejection (including an old API's unknown-field error). The preference adds Docker on capability-gap and saturation provisioning only for ordinary runs whose non-null repository is explicitly in the admin's `docker_repo_allowlist`; jobs, isolated-lane and repo-less runs including judges are excluded. Failed allowlist reads discard returned values and mean empty membership; capability-driven Docker and the independent claim fence remain unchanged. Plain warm leases step aside and the capability-free gap widens under the same policy, preserving plain reuse when inapplicable and existing cap/early-eviction rules. Use exactly “Docker-capable includes Docker for repositories your admin allows, at extra CPU and storage.” only when the checkbox is visible; remove experimental/cold-start-size framing and rootless promises from the shipped worker sections while retaining the accurate rootless-by-default Docker documentation. [user 2026-10-05, #2278] (AI-synced 2026-10-05)

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
