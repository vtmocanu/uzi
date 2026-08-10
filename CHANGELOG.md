# Changelog

Notable changes to uzi, loosely following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions are release git tags (`deploy/chart/Chart.yaml`'s `version`/`appVersion`, Model B) — this
file is not bumped per-commit; `[Unreleased]` collects everything since the last tag.

## [Unreleased]

### Changed

- Read-only validator agents keep the run worktree clean. The builtin `web-ux`
  and `fact-checker` prompts now direct transient scratch artifacts (screenshots,
  pulled-down bundles) so a clean delivery never hinges on a final `rm`, and
  `fact-checker` no longer tells the worker to write outside the worktree, which
  the file-access guardrail rejects. (from judge recommendations)
- Worker agents stop stumbling on `cd api && …` gate commands. The builtin
  `coder` and `lead` prompts now note that the worker shell's working directory
  is not reliable across separate Bash calls, and to use absolute paths (or `cd`
  from the worktree root fresh each command). (from judge recommendations)

## [0.25.0] - 2026-08-10

### Added

- **A run that hits a transient forge failure now retries instead of dying, and
  a run that fails for good raises a failure inbox notification.** Pushes and
  merge-request creates that fail for a transient reason (network blips, forge
  5xx) are retried on a bounded backoff (five attempts, 1/2/4/8/16s) rather than
  failing the run on the first error; a run that still cannot publish its work
  surfaces as a failure notification (Slack inbox badge) instead of quietly
  ending. The retry and publish paths stay credential-free — the worker holds
  the PAT, the agent never sees it. (#284)

- **Queued runs now spread across idle workers instead of piling onto one.**
  Fleet-aware claiming: while a queued run is still fresh (younger than
  `WORKER_SPREAD_GRACE`, default 3× the worker poll interval), a worker already
  running something defers it to a live, strictly-less-loaded peer that has a
  free slot, rather than taking a second run while that peer sits idle. Resume
  affinity is checked first and always wins, and past the grace window the run
  is claimable by any eligible worker, so a run is never stranded waiting for a
  peer that isn't there. The run-health signal now also distinguishes a
  saturated fleet from an idle queue. (#216)

### Changed

- **The built-in `lead` agent template now runs a per-unit, commit-anchored
  review lane, overlaps the quality gate with review, and splits work along
  seams.** Internal template tuning refreshed from accumulated judge
  recommendations; run guardrails are unchanged. (#215)

### Security

- **Hosted-worker deploy hardening: the CloudNativePG subchart is fetched from
  the ghcr.io OCI registry, and github.com is dropped from the restricted worker
  egress allowlist.** With the CNPG chart pulled via ghcr.io, the
  standard/restricted worker's FQDN allowlist no longer needs github.com,
  tightening the default-deny egress floor. (#285)

## [0.24.0] - 2026-08-09

### Added

- **Scheduled runs default to parking on a usage limit instead of failing,
  a label sweep now caps how many issues it starts per fire, and a schedule
  can carry optional owner guidance.** Three additive changes to the
  schedules feature (PRD #241): (1) a new schedule's `wait_on_limit` now
  defaults **on**, and — unlike before — that setting now actually takes
  effect on the common auto-approve path, so an unattended, typically
  off-hours fire parks on the Anthropic usage window rather than dying
  silently; (2) a new label-sweep schedule defaults a `max_issues` cap of
  **10**, applied oldest issue first, so one fire can't fan out across an
  entire label's backlog at once (clear it in the web modal for unlimited);
  (3) a pinned-issue or sweep schedule can carry optional `guidance` text
  (capped at 8 KiB, truncated rather than dropped if it would push a large
  issue over the run's size limit), injected into the run instruction as a
  section clearly separate from the issue body to steer *how* a run
  approaches its task without editing every issue. All three are
  create-time defaults, per-schedule overridable, and leave existing
  schedules untouched. CLI: `uzi schedule create` gains `--max-issues` and
  `--guidance`, and `--wait-on-limit` now defaults on. (#274)

- **A trusted repo's own conventions can now reach the agent.** The **Trusted
  repo** panel on the Repos page gains a **Repo instructions** toggle, a
  sibling of the existing **Repo skills** toggle, both independently
  revocable per repo. When on, the lead reads that repo's root `CLAUDE.md`
  as advisory context on every run against it — never as instructions it
  must follow, and never seen by subagents. The block is nonce-fenced and
  labelled UNTRUSTED/ADVISORY (the same framing PRD #90's cross-run memory
  uses) so a crafted file cannot forge trusted delimiters; the file is
  structurally sanitized before injection (root-only, symlinks never
  followed, `@`-import lines stripped, 64 KiB cap), never content-filtered.
  `settingSources` is untouched — the read goes through uzi's own channel,
  not the SDK's project loader — and the deny-hook, protected-branch
  guardrail, worker-held PAT, and human MR review are all unchanged: this
  opt-in grants context, not permissions. (#246)

- **Runs now show how long they have been going, on the Runs page, the board
  cards, and the CLI.** Each run carries a duration token (elapsed for an active
  or parked run, total for a finished one), so you can tell that a run has been
  working for 90 minutes or waiting on your approval for half an hour without
  opening it. Display only; no API, DTO, or schema change. (#256)

- **A run now publishes its committed work to origin on a time interval, not only
  at milestone boundaries.** A new `CHECKPOINT_INTERVAL` (a Go-style duration)
  periodically pushes the per-iteration checkpoint to origin, bounding how much
  work a worker's disk loss (a pod eviction or crash mid-milestone) can discard.
  The publish path is credential-free (auditor-confirmed), reusing the
  brokered-origin mechanism milestone checkpoints already use. (#267)

- **Pristine builtin agent templates now refresh automatically on boot.** When
  uzi ships an improved builtin role template, instances pick it up on the next
  boot for any builtin the user has not customized; customized templates are left
  untouched, so shipped improvements to the built-in roster propagate without a
  manual reseed and without clobbering local edits. (#275)

### Changed

- **The Judge's filter-chip counts now scope to the selected triage tab** instead
  of always counting the whole backlog, so each chip's number matches what the
  current tab is showing. (#270)

- **Builtin and dev-team agent templates refreshed from accumulated judge
  recommendations** (the `coder` and `lead` builtins, plus the repo's
  `coder`/`web-ux` roster). Internal template tuning; run guardrails are
  unchanged. (`737984b0`)

### Fixed

- **Returning to an already-finished multi-agent run now shows its agent lanes
  collapsed — the same as a live run — instead of auto-expanding every lane.**
  Opening or reloading a done multi-agent run previously sprang every lane open;
  it now opens collapsed, with "Expand all" as the one-click way to reveal them.
  This generalizes the earlier watched-while-finishing fix: a done run and a live
  run now share one code path, and expansion no longer branches on run state at
  all (single-actor runs still auto-expand). (#277)

- **An `auto` worker no longer immediately re-picks the Anthropic token that just
  hit its usage limit.** After a usage-limit park, token selection excludes the
  just-exhausted credential until its window resets, so a parked run resumes on an
  account with real headroom instead of bouncing straight back onto the exhausted
  one. (#217)

- **Slack chat answers are delivered to the thread you asked in, and every DM now
  renders with real Slack formatting.** Fixes two defects the Slack surface
  shipped in v0.23.0 (PRD #191): replies landing on the channel root instead of
  the originating thread, and direct messages rendered as raw text rather than
  Block Kit. (#268)

- **Costs of $1000 or more drop the cents in the web UI.** `formatCost` renders a
  whole-dollar amount (for example `$1119`) at or above $1000, where the cents are
  noise, and keeps the two-decimal form below that. (#269)

- **The label filter's "Clear" control no longer shifts the layout, and now reads
  as a button rather than a link.** Reserving its height stops the filter row from
  jumping as Clear appears or disappears, and the restyle brings it in line with
  the other actions. (#276)

- **A repo whose `devbox.json` is JSONC (carries comments) no longer silently
  provisions no tools.** Tier-2 worker devbox provisioning is now best-effort: a
  parse failure warns and skips rather than aborting the run, and tier-1 versus
  tier-2 provisioning failures are reported distinctly. (#278)

## [0.23.0] - 2026-08-09

### Added

- **Board membership and who can run what are now configurable label sets,
  not one hardcoded PRD label.** Two new admin-only instance settings replace
  what used to be a single gate: **run-eligible labels** (default `PRD,bug`)
  control which issues a human may click Start run on, and **board-extra
  labels** (admin default `bug`, overridable per user, per repo) control
  which non-primary issues a board shows by default. The board toolbar's
  **Issues** popover replaces the old "Show other issues" checkbox: a pinned
  always-shown primary row, one row per label with a count of how many cards
  it adds, a greyed `0` row for a configured default the repo has none of,
  the old "Show all other issues" escape hatch, and **Reset to default**. A
  third setting, `eligible_label_waives_prd_link` (default **on**), lets an
  issue eligible via a *non-primary* label (e.g. `bug`) start a run with no
  `prds/*.md` link — scoped to a human's own interactive Start click; it
  never applies to autopilot or to a scheduled/timer-fired run. The primary
  label (`prd_label`) itself is unchanged: still the label uzi writes to mark
  its own work, the only one boards fetch with, and the only one autopilot
  matches. (#196)

  **Operators — this changes default behavior on upgrade, in two halves.**
  *Visibility:* every board gains `bug` cards it didn't show before —
  cosmetic, and reversible per user from the Issues popover, or instance-wide
  by editing `board_extra_labels`. *Eligibility:* those `bug` cards also gain
  a working **Start run** button with no admin action, because `bug` ships in
  the eligible default (`PRD,bug`) and the PRD-link waiver defaults on — a
  `bug` issue with no `prds/*.md` link is startable where it wasn't before.
  **Autopilot is unaffected** — it still matches only the primary label and
  never receives the waiver — and neither is a scheduled or swept run: the
  waiver is scoped to an interactive, human-initiated Start click only, so
  nothing starts unattended that couldn't before. To keep the prior
  behavior, set `run_eligible_labels` to just the primary label, or turn
  `eligible_label_waives_prd_link` off, from Admin → Instance settings. See
  [docs/board.md](docs/board.md#which-issues-show-up) and
  [docs/admin-settings.md](docs/admin-settings.md#run-eligibility-and-board-membership).

- **Per-label counts on the Judge filter chips (#244).** Each chip now shows how
  many recommendation groups are in that category across your whole backlog, every
  bucket and triage state — sourced from a new server aggregate, never tallied off
  the on-screen list, so a chip reads correctly even when the truncation banner is
  showing. A zero-count chip stays visible, just dimmed.

- **Slack is now a two-way conversational surface: chat, run control, and status,
  from a DM.** A top-level DM to the uzi bot opens a real conversation, backed by
  the existing `runs.kind='chat'` machinery and streamed back into that DM's
  thread; a threaded reply continues it. The agent can propose filing an issue or
  starting a run, and both render as Block Kit cards with real Create/Dismiss or
  Confirm buttons — nothing fires from text alone, and only the confirmed-linked
  owner's click calls through. Run message content, previously withheld from
  Slack entirely ("content minimization"), is now streamed for `chat` runs only;
  every other run kind's notification lane is byte-for-byte unchanged. **This is a
  real widening of what reaches Slack, not just a new feature**: a chat's read
  tools are user-scoped but not kind-scoped, so a Slack DM can be asked to quote
  an `issue` run's saved content (plans, diffs, tool output) into the thread —
  `docs/slack.md` documents this plainly. No app re-install and no admin action
  needed on any workspace that already has uzi's Slack app connected; a spend
  limiter is shared with the web Chat page, so a heavy Slack day can rate-limit
  it too. (#191)

- **Runs can now be scheduled — one-time or recurring — instead of only starting
  on demand or via autopilot.** A new Schedules surface (web `/schedules` page +
  create/edit modal + an issue-view "Schedule…" action) and a `uzi schedule` CLI
  group manage a persisted, owner-scoped schedule against one of three targets: a
  pinned issue, a label sweep (every open issue on a repo matching a label
  selector, still gated by the normal run-creation rules), or an ad-hoc prompt — a
  new issue-less `prompt` run kind that opens an MR directly against a repo with
  no issue involved. A background scheduler goroutine (the same wake-ticker shape
  `selfimprove` already uses) claims due schedules and fires them as their owner,
  on the owner's own Anthropic token and forge PAT. Issue and sweep targets fire
  through the same run-creation seam autopilot uses, inheriting every existing
  gate (PRDLESS bypass, active-run dedup, the usage-limit park); the prompt target
  is the deliberate exception and bypasses the PRD-issue sanction gate by design.
  Auto-approve defaults on per schedule. See [docs/scheduling.md](docs/scheduling.md).
  (#241)

- **`uzi run wait` replaces the hand-rolled poll loop for driving a gated run
  headless.** `uzi run wait <id>` blocks until a run reaches an actionable or
  terminal state — a plan gate, a clarification park, or done — polling
  client-side and printing each transition to stderr; `--until` narrows the
  target set (after approving, wait again for only `completed,failed,cancelled`,
  since a run lingers briefly at `awaiting_approval` right after a successful
  approve), `--timeout` gives up with a new exit code 7, and a single transient
  server blip is retried rather than fatal. `uzi run get --field <name>`
  complements it: a single top-level scalar field printed raw and unquoted, one
  per line, sidestepping the footgun of piping `--json` through a shell that
  re-interprets escapes (notably zsh's `echo`, which mangles the CLI's
  `\uXXXX`-escaped control bytes and breaks `jq`). (#264)

### Changed

- **The admin rate-limits table no longer needs a horizontal scrollbar to see the
  status.** The two ~280px 5-hour/7-day window columns are now one stacked
  **Utilization** column (a mono `5h`/`7d` chip, meter, percent and reset
  countdown per row), and the "Updated" timestamp folds under the Status pill —
  six columns down to four, no data removed, fitting a normal laptop content
  width with no scrollbar and no clipped pill. (#240)

- **Six built-in agent roles picked up sharper review and gate-reading guidance,
  synced from the role library.** `auditor` gained lenses for uncapped
  amplification/resource-exhaustion reads, injection into a non-shell sink
  (terminal, log, a shared admin surface), and verifying that a security-gating
  check fails closed; `coder` gained a note that a directive or skip marker fires
  on any line carrying its literal text, including a comment warning against it;
  `researcher` gained the symlinked-directory blind spot (a recursive `grep`/`rg`
  sweep silently skips a symlinked corpus and reports a clean negative);
  `reviewer` gained "a fix at one call site is a claim about the whole sibling
  set" plus three checks specific to a status/health/authorization predicate;
  `tester` gained three gate-instrument traps (a shell pipeline misreporting its
  own exit code, a severity-staged tool's warn tier passing green with findings
  uncounted, and a stale result cache or an unstaged file silently excluded from
  a gate's scope); and `web-ux` gained a check that a mutating control is gated
  client-side on the same scope predicate the server enforces, not just hidden
  from an unauthorized viewer. **Operators:** on an already-seeded install these
  six templates badge as "differs from shipped" (issue #201's mechanism) until
  you open each and click Reset to default. (`9b930988`)

### Fixed

- **A completed run no longer shows `0/N` milestones when it actually finished
  them.** The tracker used to reconcile only from mid-run `report_progress`
  reports, so a run that jumped straight to its final turn with no progress
  report in between showed a misleading `0/N` at completion. The lead now
  declares which frozen milestones it finished on its `signal_done` call, the
  server unions that declaration into `milestones_completed` the same way it
  already unions mid-run reports, and the UI distinguishes "not reported"
  (neutral) from a genuine `0/N` (which now only means "reported and truly
  none"). (#265)

- **`save_memory` no longer lets an unverified claim become a self-reinforcing
  "fact."** A run had asserted, without ever testing it, that a builtin
  subagent lacked write tools, saved that to durable cross-run memory, and two
  later runs cited it as "the repo convention I noted" and over-serialized work
  onto the lead's own thread instead of delegating. `save_memory` now requires
  the writer to declare whether a claim was **observed** (a tool result,
  command output, or a `file:line`) or merely **inferred**, and a retrieved
  *inferred* entry is individually marked "re-verify before acting" rather than
  relying on the single blanket "memory is advisory" notice that had already
  failed once. Separately, the lead's per-turn roster now states each
  subagent's actual write capability instead of leaving it to guess — the
  specific trigger for the incident above. `save_memory` also nudges (never
  rejects) against saving a claim about the run's own runtime configuration, a
  class that goes stale and should be read live instead. See
  [docs/memory.md](docs/memory.md). (#266)

- **The judge's command-not-found pre-scan no longer flags generic output
  words as missing worker tools.** A low-confidence `X: not found` match (the
  dash/busybox form) — which a plain word like `key` or `foo` in unrelated
  output can trigger — is now corroborated against the commands the run
  actually invoked, and dropped unless the run really ran that command; the
  three high-confidence "command not found" forms are unaffected. (#263)

- **A worker's own gate run no longer false-flags a large pre-existing lint
  backlog in files it never touched.** The golangci-lint ratchet
  (`new-from-merge-base: origin/main`) computes its merge base against the
  runner clone's `origin/main`, but that ref was copied from the bare mirror's
  frozen snapshot at first clone and never advanced — so on an older mirror the
  merge base landed far enough back that the whole existing backlog read as
  branch-introduced. The clone now advances `origin/main` to the fresh default
  branch head before the gate runs, so the ratchet gates only what the branch
  actually introduced. (#262)

## [0.22.0] - 2026-08-08

### Added

- **GitHub forge support (#238).** A third forge driver (github.com, classic PAT)
  behind the forge-generic interface, at full parity with GitLab and Forgejo: board
  sync, runs, pull-request creation and watching, privilege guardrails, and the
  GitHub Actions CI-fix loop. Connect a GitHub bot PAT and your PRD-labeled issues
  populate the board; cards read "Pull Request"/"PR"/"#N". Ships dark behind the
  connect-form forge picker.

### Fixed

- **Milestone progress UI stayed blank on human-gated runs (#259).** Milestones are
  now frozen on the first running report (`milestones_frozen` is set), so the PRD #122
  progress UI populates on runs that pass through a plan gate.
- **Bug bundle: controller vulnerabilities, web papercuts, and a chat-cap bypass
  (#258).** A batch fix that landed alongside #238, also closing #221, #152, #163,
  #183, #185, #204, and #192.

## [0.21.0] - 2026-08-08

### Added

- **Milestone progress across the product (#122).** A run whose lead breaks its plan
  into milestones now shows what is done, in progress, and left everywhere the run
  appears: a checklist plus an M/N badge on the run page, and the candidate breakdown
  at the plan gate (M3); a milestone counter on the Slack root line and a threaded
  line as each milestone completes (M4); and the same state in `uzi run get` (M5). A
  run with no milestones is unchanged and keeps its iteration badge.
- **Per-worker uptime on the fleet UI and CLI (#251).** Each worker shows how long it
  has been online.

### Fixed

- **A working run no longer reads as stalled or idle (#193).** The wall-clock "slow"
  health flag was painting the actively-running lane amber as if it had stalled, and
  lane headers showed when a lane opened instead of its last activity. Both are fixed
  in the run activity view; the server health detector was already correct.
- **Version popover ordering (`91584071`).** Reordered so PRDs follow Commit and Uptime
  is last.

### Changed

- **uzi CLI skill documentation (#255).** Documented the seeded-plan budget tradeoff,
  the per-verb JSON envelope shapes, the full run-status enum (including `limit_wait`),
  and that `uzi run logs --follow` returns only on a terminal status; added a
  post-session "improve this skill" note.

## [0.20.2] - 2026-08-08

### Fixed

- **The checkpoint push broker no longer OOM-kills the api on a real repository.**
  M8's broker fetched the run branch's full history into an in-memory git store
  before pushing; on a real repo that unpacked the entire pack into RAM (~740 MB
  RSS for uzi's 130 MiB pack) and OOM-killed the 512Mi api pod at every milestone
  checkpoint. The base fetch is now **shallow (depth 1)** — it pulls only the tip
  snapshot the delta pack actually references (~190 MB peak, measured against the
  real forge) — and the api memory limit is raised 512Mi → 1Gi for concurrency
  headroom. This surfaced on the first real deploy of 0.20.1; the broker's unit
  tests use a one-commit local fixture where fetch depth is a no-op. (#122 M8)

## [0.20.1] - 2026-08-08

First **published** build of the 0.20.0 changeset. The `v0.20.0` tag was created
but its publish pipeline was skipped by a stray `[skip ci]` marker on the tagged
commit (the marker suppresses the tag's own publish pipeline too), so 0.20.0
produced no images or chart and was never deployed. 0.20.1 is identical in
shipping code — see the [0.20.0] section below for the actual changes.

## [0.20.0] - 2026-08-08

### Added

- **Runs now checkpoint committed work at milestone boundaries and recover it
  after a worker crash.** When the lead completes a milestone, the worker durably
  saves the run's branch to its data volume (surviving a pod kill and a
  same-worker restart), and — via a new server-side push broker — publishes it to
  a `refs/uzi-checkpoints/<branch>` ref on the forge, so a *different* worker that
  re-claims the run recovers the completed milestones instead of redoing them. The
  forge token is never exposed to a worker git process (the API holds it and
  performs the push), the push is never forced, and no CI pipeline fires on the
  checkpoint ref. The plan carries an optional milestone list (`submit_plan`), the
  run budget scales with it, and progress is reported over the existing run
  stream; the user-visible progress UI (web, Slack, CLI) is not part of this
  release. (#122 M1, M2, M6, M8)

- **The run page shows live in-flight token counts.** Usage on the run page
  updates from the first model call rather than only after the run records
  usage, so a running agent's token spend is visible as it happens. (#237)

- **The Runs menu item carries an in-progress count badge.** The nav badge shows
  how many runs are currently in progress at a glance. (#239)

- **The version popover and `uzi version` now show PRD roadmap progress.** A
  `PRDs  N done · M open` row (sidebar popover) and matching `prds  N done, M
  open` line (`uzi version`) count completed PRDs (`prds/done/*.md`) and active
  ones (`prds/*.md`) in the source tree the running image was built from, so a
  published instance's roadmap progress is visible without cloning the repo.
  Both counts are build stamps computed in CI and injected via ldflags, the
  same way the existing commit count is — the API's Docker build context has
  neither `.git` nor `prds/` to count from at runtime — so like that field
  they're simply absent (never shown as zero) on an unstamped dev build. (#245)

- **`save_memory` now steers agents away from saving numbers that will go stale.**
  The lead's prompt and the tool's own description ask agents to record the durable
  fact rather than today's count (test-pass tallies, version numbers, and the
  like), and a non-fatal warning fires when a saved memory's body looks like an
  obvious snapshot shape. (fcaecf57)

### Fixed

- **A subagent addressing "lead", "orchestrator", or "team-lead" no longer fails
  to send its message.** Those names now get transparently rewritten to `main` by
  a guardrail hook, defense in depth for repo-sourced and user-authored agent
  templates (builtins already said `main`). A repo that registers a real subagent
  literally named `lead` is unaffected: the rewrite only fires when no such
  subagent is registered. (61795aac)

- **The judge triage meter's "to do" segment is now amber.** It previously read
  as grey/"dismissed", making outstanding recommendations look already handled;
  amber distinguishes to-do from dismissed. (#243)

## [0.19.1] - 2026-08-07

First published cut of the 0.19.0 content: the `v0.19.0` tag was blocked by the release
changelog-coverage gate — a builtin-role refresh merged during the release cut and was not
yet cited — and `v*` tags are immutable here, so the identical code ships as 0.19.1.

### Added

- **Filter the Judge backlog by recommendation label.** A row of chips above the bucket
  tabs — one per label (enable a tool, install a worker tool, adjust a template, improve
  an agent, add an agent, improve uzi) — narrows the cross-run worklist to whichever you
  tick. It's multi-select (OR: tick two and see either), lives in the URL as a shareable
  `?category=`, and `uzi review backlog --category` does the same from the terminal. Like
  the existing `--run` anchor, the filter runs before the server's row cap, so narrowing
  by label makes the "backlog was truncated" banner less likely to bite, not more. (#235)

### Changed

- **Built-in agent roles refreshed from the role library.** The shipped agent roster
  (architect, coder, auditor, documenter, fact-checker, researcher, lead, web-ux) picked
  up judge-recommendation improvements and was re-synced from the role library, so new
  runs get sharper role instructions. (8199f8e8)

## [0.18.0] - 2026-08-07

### Added

- **Worker toolchain now ships `task` and `jq`.** The baked worker image includes
  the `task` runner, so agents can drive a repo's own `Taskfile` gate recipes
  instead of reconstructing them by hand, plus `jq`. Closes two
  `install_worker_tool` judge recommendations. (#233)

### Fixed

- **Judge recommendation backlog no longer fragments one recurring finding into
  separate rows.** Recommendations are deduped on a canonicalized
  `(category, target)` key, so a finding that recurs across runs collapses to a
  single row carrying its true "seen in N runs" frequency instead of splitting N
  ways. (#232)
- **Worker runner clones now carry a git author identity.** The clone the agent
  works in is pre-configured with `user.name`/`user.email`, so the agent's first
  `git commit` no longer fails with "Author identity unknown" (exit 128) and
  self-heals, which was burning an iteration on every commit-producing run. (#234)
- **Activity feed no longer springs every lane open when a run finishes while you
  watch it.** Auto-expand still applies when you open an already-finished run, but a
  live `running → completed` transition now preserves the collapsed view you were
  reading instead of discarding it.

## [0.17.0] - 2026-08-04

### Added

- **Seed a plan onto a run: `uzi run create --plan-file <plan.md>`.** A run can now
  be created with an externally-authored plan (written locally in Claude Code),
  skipping uzi's Phase-1 planning turn and the approval gate: the worker implements
  the supplied plan directly and opens an MR, so the human checkpoint moves from the
  plan gate to MR review. Optional `--agent-source` / `--exclude-agents` pick the
  subagent roster, and `--planned-commit` / `--require-base` guard against a stale
  base commit. The seeded plan is size-capped and secret-scrubbed; a run created
  with no `--plan-file` is unchanged. Web run pages render a seeded run's plan. See
  `docs/seeded-plans.md`. PRD #209.

### Changed

- **Contributor tooling: MR pipelines no longer wait for the full gate before
  they start building images.** Each `build:*` job now `needs:` only the
  checks for its own component (`build:api` waits on `validate:api`,
  `test:api` and `lint:api`, not the whole gate set) instead of every check
  across all four components, so it starts as soon as its own component is
  clean rather than after the slowest of all four. Publish jobs' `needs:`
  are untouched. Part of PRD #230. Developer-facing only: no change to how
  uzi behaves.

- **Contributor tooling: `test:api-store-it` no longer sits on the image-build
  critical path, and still gates every release exactly as before.** It's
  dropped from the validation-gate `needs:` list build jobs used to depend
  on, but stays in the publish-gate list, so it still runs on every ref and a
  tag pipeline still can't publish without it passing. Part of PRD #230.
  Developer-facing only: no change to how uzi behaves.

- **Contributor tooling: `e2e:kind-smoke` no longer rebuilds the api and web
  images from scratch on `main`.** `build:api`/`build:web` now emit a
  `--tarPath` image archive as an artifact on protected non-tag refs, and the
  e2e job loads that instead of running its own cold `docker build` inside
  DinD; a tag pipeline still self-builds its images, unchanged. `scripts/smoke.sh`
  is untouched. Part of PRD #230. Developer-facing only: no change to how uzi
  behaves.

- **Contributor tooling: an MR that doesn't touch a component now skips that
  component's image build.** `build:api`, `build:web` and `build:controller`
  gained a `changes:` filter scoped to each component's real Dockerfile
  inputs (`build:web`'s includes `docs/**`, which its Dockerfile copies in).
  `build:agent` is deliberately excluded and always builds, and no
  protected-ref pipeline skips a build, so `main` and tag pipelines are
  unaffected. Part of PRD #230. Developer-facing only: no change to how uzi
  behaves.

- **Contributor tooling: `golangci-lint` no longer compiles from source on a
  cold lint cache.** `task lint:api`/`lint:controller` now fetch a pinned,
  sha256-verified release binary (`scripts/golangci-lint.sh`) instead of
  `go run`-ing the module from source, with the cached binary re-verified
  against its own pin on every run rather than trusted on a cache hit alone.
  The pin is checked against both Go modules' `go` directive at gate time and
  fails loudly if a future linter release's minimum Go version outgrows them.
  Findings are unchanged: verified identical to the previous `go run` build,
  by diff, on both modules. Part of PRD #230. Developer-facing only: no
  change to how uzi behaves.

- **Contributor tooling: `test:api-store-it`'s throwaway Postgres now runs
  with durability off.** `fsync`, `full_page_writes` and `synchronous_commit`
  are disabled on the job's disposable `postgres:17` service, since its data
  is destroyed with the job either way; a mid-job crash could corrupt it,
  which is an accepted trade for data that was never going to outlive the
  job. Part of PRD #230. Developer-facing only: no change to how uzi behaves.

## [0.16.0] - 2026-08-04

### Changed

- **A worker no longer keeps a parked run's on-disk clone around.** When a run
  parks on a usage limit (or the worker shuts down), the worker used to preserve
  the run's clone directory on the theory a resume needed it. It never did: a
  resume re-clones unconditionally and the committed work is now fetched back into
  the worker's own repo (a tracking ref) and recovered from there instead,
  validated live on dev-cluster by a real worker eviction that recovered the
  committed work byte for byte. So the clone is now cleaned up on a park like on
  any other terminal path, freeing a full working tree per parked run for up to
  the 8-day park window; the plugin dir and the per-run session HOME are still
  preserved so the session resumes cleanly (issue #218).

## [0.15.0] - 2026-08-04

### Added

- **Agent templates now flag when a builtin has drifted from what this uzi
  version ships.** A **differs from shipped** badge appears on the Agents
  list and on a builtin's detail page whenever its stored description,
  model, tools, or prompt body no longer matches the shipped definition,
  whether the drift is your own edit or a shipped update you haven't picked
  up yet. Opening the template now shows the actual diff before you press
  **Reset to default**, which still replaces the whole body verbatim and
  still isn't automatic (issue #201). **Operators:** issue #210 rewrote ten
  of the eleven builtin templates' bodies to fix an unreachable report
  recipient (see Fixed, below); on any already-seeded install, those ten
  will badge as differing the moment you deploy this build. That's the new
  signal working as intended, not a defect: open each and click Reset to
  pick it up. See
  [docs/agent-templates.md](docs/agent-templates.md#resetting-a-builtin-template).
- **`uzi` now warns when the CLI you're running is older than the server it's
  talking to.** A CLI three minors behind was silently dropping fields it
  didn't know about — `run get --json` printed `null` for token attribution
  that a `curl` against the same endpoint returned fine — with nothing telling
  you why. The warning prints to stderr (never stdout, so `--json` output stays
  parseable) and costs at most one version probe an hour. Suppress it with
  `--quiet` or `UZI_VERSION_CHECK=0` (issue #144).

### Changed

- **Subagents no longer have the ordinary route to write files during a run's
  plan turn**, the phase before you approve a plan. Every subagent's `Edit`,
  `Write`, `MultiEdit` and `NotebookEdit` tools are removed for that turn
  only; the implement turn is unaffected, so a role that needs to write is
  exactly as able to once the plan is approved. This is a hygiene control,
  not a guarantee: the turn still runs under `Bash`, and nothing at the
  guardrail layer denies a shell-based write (`echo >`, `sed -i`, `tee`), so
  what changes is the tool a model reaches for by default and its awareness
  that one exists, on top of the prompt instruction that was previously the
  only thing saying not to. Actually catching a worktree change no matter how
  it was made is tracked separately as issue #212 (issue #203).

- **Worker names and CLI-token names are now validated on write, and reject
  terminal-unsafe characters that were accepted before.** `POST
  /api/workers`, the hosted-worker provisioning path, and both the
  CLI-token mint and device-start flows now return 400 for control
  characters, Unicode format characters (bidi overrides, zero-widths, the
  BOM, the soft hyphen), and invalid UTF-8; names are still trimmed of
  leading and trailing whitespace before that check, unchanged from before.
  These names are read back in cross-tenant admin listings beside a
  different user's account, so an unvalidated one was terminal control
  injection into another user's session, and an embedded newline could
  forge a whole table row in a listing an admin reads to make decisions.
  Existing stored names are untouched by this change and stay covered by
  the render-side fix instead (issue #180), which strips the same
  characters on the way out (issue #169).

- **Contributor tooling: dependency vulnerabilities are now scanned on every
  MR**, for the first time in this repo — `govulncheck` for both Go modules and
  `npm audit --audit-level=high` for both npm packages. They hang off the
  existing per-toolchain jobs (`lint:api`, `lint:controller`, `validate:web`,
  `validate:agent`) rather than adding new ones. Locally they are
  `task vulncheck` and are **deliberately not part of `task gate`**: their
  verdict is a function of a remote mutable database, so they can answer
  differently on two runs of one commit with nobody's diff in between, and a
  contributor's gate must be deterministic against the tree. All four host jobs
  are in `*publish_needs`, so this is release-blocking by inheritance — a CVE
  published on a Tuesday can redden a `v*` tag publish that contains nobody's
  diff. That is accepted, not overlooked. The npm half gates at `high` rather
  than zero because two moderate `react-router` advisories have no patched 6.x
  to move to; clearing them is a React Router 6 → 7 major through **runtime,
  shipped SPA routing code**, filed as issue #226. Part of PRD #103.
  Developer-facing only: no change to how uzi behaves.
  See [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: the gate now refuses environment variables that would
  silently narrow what it looks at.** Three tools turned out to have one — a
  substituted `GITLEAKS_CONFIG`, an `NPM_CONFIG_OMIT=dev` that drops the dev
  tree and prints "found 0 vulnerabilities" at exit 0, and a `GOPACKAGESDRIVER`
  stub or `GOFLAGS=-tags` that build-tags the vulnerable call out of the graph.
  Each produces output shaped exactly like a clean run. The scripts now refuse
  them, narrowly where a legitimate use exists (`GOFLAGS=-buildvcs=false` is a
  documented workflow here, so only `-tags` is rejected). Part of PRD #103.
  Developer-facing only: no change to how uzi behaves.
  See [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: `task gate:web` and `task gate:agent` now check that
  your `node_modules` matches the branch before running anything else.** A stale
  install does not make lint, knip, `tsc` and vitest *wrong* — it makes them
  answers about a different tree, all green. The check is two parts because
  `npm ls` compares against declared ranges only, so a transitive bump is
  invisible to it; the second part joins against the lockfile. Measured:
  `gate:agent` was green on a tree holding all five of the vulnerable versions
  this release bumps. Part of PRD #103. Developer-facing only: no change to how
  uzi behaves. See [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: `web` moved to vitest 4.1.10, pinned exactly.** A major,
  taken because the 2.x line is where the remaining high-severity `web`
  advisories lived. The pin is exact rather than a range, and so is
  `@vitest/coverage-v8`, which is an exact-version optional peer — the two move
  together or the install breaks. `environmentMatchGlobs` and `test.workspace`
  are both gone in 4.x. The upgrade's control was a count written down before the
  run and reproduced after (118 files / 1660 tests), because a green with a lower
  collected count is a silently narrowed suite that no exit code reveals. Part of
  PRD #103. Developer-facing only: no change to how uzi behaves.
  **Existing checkouts need `npm install --ignore-scripts` in `web/`.**

- **Contributor tooling: test coverage is now measured for `api`, `controller`
  and `web`, and shown on every MR.** `task test:api` and `task test:controller`
  write a profile and print the statement total; `task test:web` runs vitest with
  the v8 provider. GitLab reads the number off each job and the Cobertura reports
  annotate the MR diff. **There is deliberately no failing threshold** — a number
  picked before the current one was known is either vacuous or blocks unrelated
  work, so this milestone measures and a later one chooses. Two things worth
  knowing before reading the figures: the totals exclude packages with no tests of
  their own, and the single percentage GitLab puts on an MR is an unweighted mean
  across the three jobs rather than a repo-wide figure — read the per-job numbers.
  Part of PRD #103. Developer-facing only: no change to how uzi behaves.

- **Contributor tooling: `web` tests now get their environment from
  `vite.config.ts` instead of relying on a per-file docblock.** A test under
  `src/components` or `src/pages` that forgets `// @vitest-environment jsdom` used
  to run under node, which is usually a loud error and is not reliably one. Those
  directories now default to jsdom via `test.projects`, while `src/lib` and
  `src/mocks` — where every node-side test in the suite lives — stay on node. The
  docblock still wins over the config where a file carries one, measured on
  vitest 4 rather than assumed, so no existing file changed environment: the
  per-file census is identical before and after. Part of PRD #103.

- **Also in this release, smaller changes that merged alongside the above.** The
  builtin agent templates gained additional role guidance for several roles
  (auditor, coder, fact-checker, reviewer, tester, web-ux) (`e963f672`); on an
  already-seeded install these badge as "differs from shipped" until reset, like
  the #201 note above. The root `CLAUDE.md` was reorganised into path-scoped
  contributor rule files (`8fd735ba`, NEW-6). And a rejection-message wording in
  the secrets API handler was aligned with the project's lint rule (`10f7b6a5`).

### Fixed

- **A run that parks on an Anthropic usage limit, or whose worker is shut down
  or evicted, no longer loses the work the agent already committed.** Previously
  the committed branch lived only in the per-run clone; the next claim deleted
  that clone and re-seeded from the default branch, so a resume came back to an
  empty working tree while every other signal said it had resumed cleanly. The
  worker now fetches the agent's committed branch back into its own repository
  before cleanup, on both the park and the shutdown or eviction paths, and a
  resume rebuilds from that recovered work instead of silently rebasing onto a
  default branch that moved in the meantime. When work genuinely cannot be
  recovered, the run says so in its activity feed rather than re-treading it
  silently. The runner clone, plugin dir and per-run HOME are still preserved on
  a park exactly as before; removing the now-redundant clone-preservation step is
  a deferred follow-up (issues #218, #224).

- **Subagents in ten of the eleven builtin agent templates now reach the team
  lead when they report back.** Each template's "report via SendMessage"
  instruction named the recipient "the team lead," but the only address
  reachable from a builtin body is `main`: the lead's own template runs as the
  main thread, not as an invokable subagent, so a call addressed to "the team
  lead" (or a bare "lead") failed with "No agent named 'lead' is reachable."
  Measured across three real runs, 8 of 26 SendMessage reports failed this
  way. Five subagents recovered by resending to `main`, each costing a
  regenerated report of several kilobytes plus a few tool round-trips; the
  other three gave up on SendMessage entirely and returned their findings
  through a different channel, each prefixed with an apology about uzi's own
  plumbing that a human then read in the report. The thirteen recipient
  instructions across the ten templates now name `main` explicitly; the role
  is still called "the team lead" in surrounding prose, since a blanket
  rename there would misread as proposing action to the tool address rather
  than describing the role. **Operators:** the fix does not reach an install
  that has already booted, since builtin seeding never overwrites an existing
  template row. An admin must open each of the ten affected templates and
  click **Reset to default** to pick it up (issue #210).

- **Release tooling: the CHANGELOG coverage gate could block a good release,
  about once every dozen runs, and a retry made it go away.** It reported a
  merge as uncited while the section cited it on the line the check had just
  matched. Cause: `printf '%s' "$SECTION" | grep -q <pattern>` under
  `set -o pipefail`. `grep -q` exits the instant it matches and closes the pipe;
  bash's `printf` builtin writes line-buffered, so an 8.5 KB section leaves the
  shell as 72 separate writes and is still writing when grep goes; the next
  write takes SIGPIPE and `pipefail` reports 141 for a pipeline whose grep
  succeeded. Every `grep` in the script now reads a file, which has no writer to
  kill. Present since the gate was first added, on the issue-number lookup and
  on both escape hatches, not only on the short-SHA citation added the same day
  it was found (`19ad63c3` was the merge it flagged; job 134884 failed, retry
  134892 passed on the identical commit). Developer-facing only: no change to
  how uzi behaves.

- **A hostile server can no longer flood your terminal through `uzi version`.**
  The build-info strings uzi prints are now capped as well as stripped of
  terminal control characters: the stripping arrived with the shared render
  boundary (issue #180), but that boundary deliberately does not truncate, so a
  server returning a megabyte-long version string still printed all of it. The
  version line is bounded now (issue #144).

- **Hosted worker pods now declare an ephemeral-storage request (512Mi plain,
  4Gi docker-tier) on the worker container, and the Docker sidecar's data root
  moved off the pod's ephemeral storage onto its own PVC.** Every container in
  a worker pod previously requested zero ephemeral storage, so the scheduler
  placed pods with no account of their real disk footprint and kubelet ranked
  them first for eviction the moment a node ran low; an evicted pod's runs
  re-queue onto a stale local clone and lose every uncommitted local commit
  (one measured loss ran to 82 minutes of work, issue #209). **This lowers how
  often that happens; it does not close it.** A declared request changes
  kubelet's eviction ranking, it does not make a pod eviction-proof, and the
  fix for the underlying loss, fetching work back before an evicted worker's
  tree is discarded, is issue #218's, not this change's. **Operators:**
  rolling this out replaces every worker pod once, so every run in flight at
  deploy time loses its tree, the same accepted cost as any other
  worker-pod-spec change. It ships inside the chart, so merging to `main`
  alone does not deploy it: delivery needs a `v*` release tag published to
  Harbor plus a manual `targetRevision` bump in the `argo-apps`
  repo's `apps/uzi/app.uzi.yaml`. The new per-docker-worker dind-data PVC
  (20Gi, chart-overridable) is a third RWO volume per worker, widening the
  existing Multi-Attach window on reschedule by one; it is never
  garbage-collected, so `docker system prune` on the worker is the reclaim
  path, and the daemon's build cache now survives a pod restart instead of
  being wiped with the old emptyDir. Raising `workers.docker.dindDataSize` on
  a live cluster is a silent no-op for workers that already exist: the size
  isn't part of the pod spec hash and the PVC is never resized after
  creation, so only a newly-provisioned worker picks up a raised default
  (issue #224).

- **The worker controller now refuses to boot when a rendered worker PVC
  would exceed its tier's LimitRange storage ceiling, instead of retrying
  forever on a worker that provisions and never appears.** A preset's
  `/data`, `/nix`, or the dind data PVC could be sized above what the
  cluster's LimitRange allows and still pass `helm template` and the
  controller's own boot cleanly; the PVC create was then rejected on every
  reconcile tick with the reason visible only in the controller's own log,
  nothing reported back to the api, and no worker ever appeared. The boot
  check now names every offending claimant, its size, and its ceiling, and
  which value to lower, as a `CrashLoopBackOff` an operator will actually
  see. Existing worker pods and runs in flight are unaffected, since the
  api's run lanes never go through the controller. **This is a genuine new
  cost, not a pure improvement: provisioning, drift reconciliation, and
  teardown all run in the same reconcile loop, so a prolonged crash-loop
  also stops teardown, leaking PVCs and storage quota for workers the api
  has already dropped.** Before this change the same misconfiguration gave
  a silently stalled fleet with teardown still working; now it gives a loud
  failure with teardown stopped. **Operators:** recovery needs a chart
  values change plus an ArgoCD sync, not `kubectl set env` on the
  Deployment: `selfHeal: true` reverts that the moment it notices (issue
  #224).

### Security

- **The `uzi` CLI no longer renders server-supplied text on your terminal
  raw.** Every human-readable render path (tables, printed lines, the error
  line, login and version output) passed the server's strings straight
  through, so a hostile or compromised server the user had pointed `--url`
  at could clear the screen, rewrite the window title, or use a bidi
  override to make a name read as something other than what it says.
  Terminal control characters and Unicode format characters (the bidi
  overrides, zero-widths, the BOM) are now stripped at a shared render
  boundary before anything reaches stdout; `--json` output is deliberately
  untouched, since it already escapes those bytes and stays the lossless
  forensic channel. **The accepted cost:** a zero-width joiner is itself one
  of the stripped characters, so a multi-part emoji built from one (a family
  emoji) now renders as its separate members instead of the joined glyph; a
  single-codepoint emoji is unaffected (issue #180).

- Bumped `golang.org/x/text` v0.38.0 → v0.39.0, closing `GO-2026-5970` (infinite
  loop on invalid input), and `github.com/yuin/goldmark` v1.7.8 → v1.7.17,
  closing `GO-2026-5320` (XSS). Both were pre-existing on `main`, caught in an
  unrelated `govulncheck` scan and folded in here rather than filed separately.

## [0.14.0] - 2026-08-03

### Added

- **The run page now says when a judge is already on its way**, instead of
  offering a **Run judge** button whose only possible outcome was an error
  toast. A finished run enqueues a judge automatically, but the review panel
  used to show the same "not judged yet" state whether one was in flight or
  not, so the obvious click hit the one-active-judge-per-target index and came
  back a 409. The panel now reads **Judge scheduled** or **Judge running…**
  with the button disabled, keeps any existing verdict on screen while a
  re-judge runs, and swaps in the new one on its own. It is server truth, so it
  survives a reload and shows to every viewer of the run, not just the tab that
  started it. `uzi review` and its `--json` output carry the same distinction
  via a new `pending_judge` key (issue #119).

### Changed

- **Contributor tooling: the `lead` template's phrase pins are now scoped to the
  region of the prompt they belong to**, so moving a rule between the plan-turn
  paragraph and the post-implementation bullet fails the test instead of
  satisfying it from the wrong section (issue #205). Test-only: no change to how
  uzi behaves.
- **The plan you approve has now been read against the code first.** The `lead`
  must back its plan with citations — for every mechanism the plan asserts, the
  file that implements it and the line — and it collects them by sending the
  allocated read-only validators over the plan itself before submitting it for
  approval. That wave reports only; nothing in the worktree changes before you
  approve. Validators still fan out again over the diff after each
  implementation unit lands, which used to be the only time they ran, so a
  wrong plan was discovered only once it had been built (issue #197).
  **Operators: a shipped change to a builtin prompt does not reach an existing
  install** — an already-seeded template row is never overwritten. An **admin**
  must open the `lead` template and click **Reset to default** to pick this up
  (editing a builtin is admin-only; everyone else gets a 403). Reset re-applies
  the shipped body verbatim, so re-apply any local customization on top. See
  [docs/agent-templates.md](docs/agent-templates.md#resetting-a-builtin-template).
- **Contributor tooling: the `agent` suite's per-test timeout is 120000, and its
  largest test file was split into seven.** The cap was binding in CI and nowhere
  else — node isolates test files in a child process per file on the CI image but
  shares one process locally, so the same flag caps a whole *file* there and each
  *suite* here, and a file running 96s locally passed a 30s cap while failing it
  in CI. Splitting the file is what removed the knife edge; the raised cap only
  bought margin. Cut the whole agent suite from 112s to 46s locally
  (`37eefea6`, `32ff4bcf`). Developer-facing only: no change to how uzi behaves.

- **Contributor tooling: the three prior-art projects are no longer vendored as
  git submodules.** `./scripts/link-inspiration.sh` clones them once to a shared
  directory outside the repo and symlinks them into a gitignored `inspiration/`,
  so a fresh clone no longer drags the corpus along. Note `rg` and `grep -r` do
  not follow symlinked directories, so a repo-wide sweep silently returns nothing
  from it — search it by explicit path or with `-L` (`19ad63c3`). Developer-facing
  only: no change to how uzi behaves, and a worker container never had access to
  the corpus either way.

- **Contributor tooling: both Go modules are now `gofmt`-clean and a format check
  runs in the gate.** `task fmt-check` fails on any formatting drift and names the
  files; it also runs first inside `task gate:api` / `task gate:controller` and
  first in CI's `validate:api` / `validate:controller`, so drift is caught on every
  MR instead of accumulating. Part of the developer-loop quality-gate work in
  PRD #103, which also moved every gate recipe into the root `Taskfile.yml`.
  Developer-facing only: no change to how uzi behaves.
  See [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: linting now runs in the gate and on every MR**, for the
  first time in this repo. `task lint` covers all four components and each
  `task gate:<component>` runs its own. Go gets golangci-lint (`errcheck`,
  `staticcheck`, `ineffassign`, `unused`, `unparam`, and `nolintlint`, which
  lints the suppressions themselves so a bare or vacuous `//nolint` is a finding
  rather than a silent blanket exemption), ratcheted against
  `origin/main` so only findings a branch introduces block; `web` and `agent` get
  oxlint, configured so `react-hooks/rules-of-hooks` actually runs, which the
  default severity tier does not reach. The 16 pre-existing TypeScript findings
  were fixed rather than baselined. Part of PRD #103. Developer-facing only: no
  change to how uzi behaves. **Existing checkouts need
  `npm install --ignore-scripts` in *both* `web/` and `agent/` before
  `task gate:web` / `task gate:agent` will run** — oxlint is a new devDependency
  and the lint step fails closed with `oxlint: command not found` until it is
  installed. `--ignore-scripts` matters in `agent/`: without it, `agent-browser`'s
  postinstall rewrites the host's `agent-browser` binary. See
  [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: dead code is now detected in the gate and on every MR.**
  `task deadcode` covers all four components and each `task gate:<component>`
  runs its own. The Go modules use `golang.org/x/tools/cmd/deadcode` against a
  committed baseline that ships **empty**, so both are held at zero — the one
  unreachable function that existed was deleted rather than baselined. `web` and
  `agent` use knip, which gates unused files and dependencies at zero while
  reporting unused *exports* without failing the build; burning that tier down
  is tracked as issue #206. Neither tool sees a dead *branch*, which stays a review
  question. Part of PRD #103; no new CI jobs. Developer-facing only: no change to
  how uzi behaves. **Existing checkouts need `npm install --ignore-scripts` in
  *both* `web/` and `agent/`** before `task gate:web` / `task gate:agent` will
  run — knip is a new devDependency and the step fails closed with
  `knip: command not found` until it is installed, the same way oxlint did.
  See [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: shell scripts, YAML and the Homebrew formula are now
  checked in the gate and on every MR.** `task gate:repo` runs first inside
  `task gate` and covers the checks that belong to no single component:
  shellcheck over every tracked shell script (the git index unioned with a
  shebang scan, so `agent/templates/entrypoint.sh` and `agent/bin/agent-browser`
  — the worker container entrypoint and the `agent-browser` shim inside every
  worker pod, neither reachable by extension alone — are finally in scope),
  yamllint over every tracked YAML except the Helm templates (which are Go
  templates, not YAML), and a syntax check of `Formula/uzi-cli.rb`, which release
  CI copies verbatim into the shared tap on every tag. All three gate at zero: the
  four shellcheck warnings and three yamllint findings that existed were fixed
  rather than suppressed. One new CI job, `lint:repo`. `web/scripts/check-docs.mjs`
  also now validates relative links in `prds/*.md` and `adr/*.md`, which nothing
  checked before, and the three that had rotted are repaired. Part of PRD #103.
  Developer-facing only: no change to how uzi behaves. **None of the three tools
  is required locally** — a missing shellcheck, yamllint or Ruby ≥3.1 prints a
  loud skip and passes rather than blocking you (the formula check falls back to
  Homebrew's own bundled Ruby first, so it only skips outright without Homebrew
  either). **A shellcheck that IS present must be exactly 0.11.0**: the version
  is asserted and a mismatch exits 2 rather than quietly grading differently,
  because 0.10.0 does not report one of the diagnostics this repo relies on. CI
  always runs all three. **Adding an `ignore` key to `.yamllint` — top-level,
  `ignore-from-file`, or an indented per-rule `ignore` inside a `rules:`
  block — is refused at exit 2, before yamllint even runs**, because it would
  silently narrow what gets read while the gate's success line still reports
  the tracked-file count from its own separate query, so the two would quietly
  disagree. Disabling a *rule* is still a legitimate, deliberate suppression —
  it leaves that count truthful, only an ignore key breaks the agreement
  between what was counted and what was checked. The clean-run line now says
  files were **handed to yamllint** rather than **checked**, which is the
  claim the script can actually make good on. See
  [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: the repo is now scanned for secrets on every gate run
  and every MR**, which nothing did before. `task scan:secrets` runs gitleaks
  (pinned to v8.30.1) over every tracked file and joins `task gate:repo`, so it
  is part of `task gate` and of CI's `lint:repo` job. Eight fake-credential test
  fixtures that the first scan reported are now annotated individually with a
  written justification, rather than by excluding test files as a class. The
  seven `gitleaks:allow` annotations this repo already carried — written before
  anyone had run gitleaks — were each checked by removing it and re-scanning:
  five suppress a real finding and keep their line, two suppressed nothing and
  are gone. **The scanner's own configuration lives in the tree it scans**, so a
  four-line `.gitleaks.toml` could switch it off in the same merge request that
  adds a secret, and the job would report green. A committed canary closes that:
  `scripts/gitleaks-canary.txt` holds a fake token the scan must report on every
  run, and any allowlist broad enough to hide a real secret hides the canary too,
  so the check exits 2 (instrument broken) instead of 0. A run that finds nothing
  now prints the canary it did find. Part of PRD #103. Developer-facing only: no
  change to how uzi behaves. **gitleaks needs no separate install** — it arrives
  through `go run`, like the linters — so unlike the three checks above this one
  has no skip and always runs. See
  [docs/dev-conventions.md](docs/dev-conventions.md).

### Fixed

- **The run page's token counts were low, by 2.5x on output and up to 229x on
  input, and now match the board.** The run page read one field of each result
  frame while every rollup surface (the board, `uzi run list`, `GET /api/usage`,
  the admin totals) read another, and on the current Anthropic SDK those two
  fields no longer agree. Cost was low too, by whatever a model that dropped out
  of the run's last frame had spent. Both surfaces now fold the same field, per
  model, and a recorded fixture from a real run pins them to each other from both
  sides so they cannot drift apart again (issue #195). This also unblocks the
  live cost estimate in PRD #194.

- **Git repositories the worker creates no longer leave a background daemon
  watching their directory.** Any git command in a repo where `core.fsmonitor`
  is on spawns `git fsmonitor--daemon --detach`, which reparents to init and
  holds directory handles for as long as it lives, so the run's cleanup deleted
  every file and then could not remove the directory. Issue #127 removed a
  different detached child (`git maintenance run --auto`) and could not cover
  this one: a retry absorbs a lock held for milliseconds, not a watcher that
  never lets go. The worker's bare clone, the runner clone and the seed
  destination now set `core.fsmonitor=false`. **In a worker container this is a
  no-op** — the daemon dies with the container either way — so the effect is on
  local development, where one such daemon was found still alive 21 days after
  its repo was created (issue #127).

### Security

- **The web image's SPA build stage now runs `npm ci --ignore-scripts`.** No
  dependency in the current lockfile tree would execute a lifecycle script
  while producing the bundle served to users; the flag keeps that true across
  the next lockfile refresh, which is when nobody is looking. Hardening, not a
  fix: the image built both ways is byte-identical today. Deliberately not
  applied to the worker's base image, whose postinstall is what bakes in the
  `agent-browser` CLI.

## [0.13.0] - 2026-07-29

### Added

- **An agent can now stop mid-run and ask you a question**, instead of guessing or
  stalling on something it shouldn't decide alone. The run parks with a **needs
  your answer** badge, the question lands in the run feed with any suggested
  options, and you answer from the run view's composer, a reply in the run's
  Slack thread, or `uzi run answer <id>` — all three read the same open question
  off the feed, so no surface invents a question the others don't have. Left
  unanswered, the run fails closed ("clarification timed out") rather than
  hanging forever. Autopilot runs never park on a question — they proceed on
  their own best judgment and record the assumption in the feed instead
  (PRD #88). See [docs/run-activity.md](docs/run-activity.md).

- **The footer version badge is now a build-info popover.** Hover, focus or tap
  it to see the release version, the project's founding date, age, and process
  uptime, plus, on a release image built in CI, the source commit, build
  timestamp and commit count. `uzi version` now reports the server's build info
  alongside its own (PRD #175). See [docs/cli.md](docs/cli.md).

### Fixed

- **A run whose owner had already answered no longer keeps nudging them to
  approve a plan.** The health check flagged `approval_idle` purely on how long
  `runs.updated_at` had sat still, and a revise response deliberately never
  touches that column — so a user who requested changes was told for the rest
  of the wait that they were the one being waited on. Slack, the board, and the
  run view now report that the run is waiting on the worker instead (issue #182).

- **The plan-revision cap could be exceeded by two concurrent submissions.**
  Two revise requests arriving at the cap's last slot could both read the same
  pre-update count and both land, letting a run exceed `PLAN_MAX_REVISIONS`.
  The cap is now enforced atomically inside a single row update instead of a
  read-then-insert, closing the race for both the web and Slack paths
  (issue #106).

## [0.12.0] - 2026-07-28

### Added

- **An agent can now stop mid-run (or before planning) and ask you a
  question**, instead of guessing or stalling on something it shouldn't
  decide alone. The run parks with a **needs your answer** badge, the
  question lands in the run feed with any suggested options, and you answer
  from the run view's composer, a reply in the run's Slack thread, or `uzi
  run answer <id>` — all three read the same open question off the feed, so
  no surface invents a question the others don't have. Left unanswered, the run fails
  closed ("clarification timed out") rather than hanging forever; both the
  answer deadline and the per-run question cap are held in worker memory and
  reset on a worker-death requeue, so the honest worst case over a run's
  life is 48h and 10 questions on the defaults, not 24h and 5. Autopilot
  runs never park on a question — they proceed on their own best judgment
  and record the assumption in the feed instead (PRD #88).

- **The footer version badge is now a build-info popover.** Hover, focus or tap it to see the
  release version, the project's founding date, age (computed client-side, so it stays correct
  between releases), and process uptime, plus, on a release image built in CI, the source commit,
  build timestamp and commit count. `GET /api/version` keeps its `version` key unchanged, both
  name and value, and adds the rest of those fields alongside it; unstamped fields are omitted
  rather than zero-filled, so a local `docker compose` build reports only version, founding date
  and uptime, and the commit count is available only on a tagged release build. `uzi version`
  now reports the server's build info alongside its own: the CLI's own version stays top-level
  and the server's nests under a `server` key, so existing `--json` parsers are unaffected
  (PRD #175).

- **Manual drag-to-reorder for board cards, a sort switcher, and a keyboard
  equivalent.** Drag a card within its column to set the board's order by
  hand, or use the new per-card up/down buttons for the same reorder without
  a pointer: native HTML5 drag-and-drop has no keyboard path in any browser.
  A five-mode switcher (Manual, Issue number, Recent run activity, Last
  updated, Title) lets you view the board differently without discarding
  the manual order underneath it; Manual is the default and reproduces
  plain issue-number order on a board nobody has touched. A reorder is
  durable per owner and follows you across devices (PRD #102 M5).

- **Cards show their other GitLab labels as chips**, so `bug` or `security`
  is visible without opening the issue. The PRD label, the PRDLESS escape
  hatch, the autopilot label, and the card's own column label are excluded,
  since none of those tell you anything the card doesn't already show some
  other way. A card with many labels shows the first few plus a `+N` count
  with the rest on hover (PRD #102 M4).

- **A per-browser "Show other issues" toggle reveals a repo's open issues
  that don't carry the PRD label**, off by default so a board nobody
  touches looks exactly as it did before. Those cards render with a dashed
  border and offer **Promote to PRD** in place of Start run, since they
  can't start a run until they carry the label (PRD #102 M6).

- **Workers can be set to pick their Anthropic token automatically.** Settings → Workers gains
  an **auto (from pool)** option beside the existing per-token choices, and `uzi worker
  set-token <id> --auto` does the same from the CLI. An auto worker chooses per claim from the
  tokens you opted into the pool, so it needs no restart and no new join token to change what
  it spends. Existing workers are untouched: one with a pinned token stays pinned, one without
  stays on your default. A worker whose pinned token you later delete now says plainly that it
  uses your default, rather than showing a pin to a token that no longer exists. If you set a
  worker to auto while your pool is **empty**, the row says so rather than claiming it
  auto-selects: every claim would spend your default token, and finding that out from a
  finished run is too late (PRD #111 M3).

- **An opt-in pool for auto-selecting Anthropic tokens.** Settings → Anthropic tokens gains a
  per-token "Auto-select from this token" toggle, and `uzi token pool <label> --on|--off` does
  the same from the CLI. The pool is empty by default and opting a token in is deliberate: a
  pool that helped itself to every credential would spend the one you reserved for something
  else. Beside the toggle, each pooled token shows whether auto-selection could actually pick
  it right now — `in pool`, or `never polled` / `stale reading` / `no usage data` / `low
  headroom` when it could not. That is the point of the chip rather than decoration: a token
  uzi has never managed to read a usage figure for can never be picked, and without the chip it
  would sit there looking active. The status is computed server-side from the same rule the
  selector uses, so the page cannot promise something the selector will not do (PRD #111 M2).
  New operator knobs `UZI_AUTOSELECT_MIN_HEADROOM`,
  `UZI_AUTOSELECT_HEADROOM_TIE_PCT`, `UZI_AUTOSELECT_MAX_STALENESS` and
  `UZI_AUTOSELECT_INFLIGHT_PENALTY` — see
  [docs/configuration.md](docs/configuration.md).

- **uzi now picks the Anthropic token, per claim, by rate-limit headroom.** A worker set to
  auto spends whichever of your pooled tokens has the most room left — headroom being whichever
  of the 5-hour and 7-day windows is fuller, since both have to allow the run. Two tokens within
  a few points of each other are separated by which one **replenishes soonest**, and it is the
  reset of the window actually holding it back that counts: a 5-hour reset does not relieve a
  7-day cap. A small bias against tokens with runs already in flight keeps several claims
  arriving together from piling onto the same credential. Operators can tune all of it
  (`UZI_AUTOSELECT_*`); nobody has to.

  **It never fails a run**, and the ways it can decline to pick are worth knowing apart: nothing
  opted in, nothing with a current reading — which is also what a switched-off usage poller
  looks like — or a chosen token whose stored value would not decrypt. Each falls back to your
  default token and says which happened, in the run view and in `uzi run get`.

  🔴 **"Auto" does not mean "only my pool".** The fallback spends your **default** token, and
  that path does not consult the opt-in — so a token you deliberately kept *out* of the pool can
  still pay for a run if it happens to be your default. There is no third option, because
  failing the run would be worse; if you want a credential never spent by ordinary runs, it must
  not be your default either (PRD #111 M4).

- **Every run now names the Anthropic credential it spent, and why that one.** The credential
  is **recorded on all three lanes** (issue/ci_fix, judge and chat) and **shown in the run
  view** as a `token <label> — <mode>` chip and by `uzi run get` as an `ANTHROPIC_TOKEN` row.
  A chat conversation has its own view and does not show the chip yet, though its spend is
  recorded like any other run's. Until now a run recorded what it cost but not which account
  paid, which was a distinction without a difference while a user could hold one token and
  stopped being one the moment PRD #104 let them hold several. The label is a snapshot taken
  when the run was claimed, so it survives that token being renamed or deleted: a finished run
  keeps naming the account it billed even after the credential is gone, and says `(deleted)`
  so it is not mistaken for one you can still go and look at. Runs claimed before this landed
  show nothing rather than a guess.

  **The mode is there because the label alone cannot answer the question.** An automatic pick
  and a fallback to your default can name the *same* token, so the chip says which of them
  happened: `console-key — auto, 62% headroom`, `console-key — pinned`, `default — judge
  binding`. When an `auto` worker did *not* get an automatic pick, it says so and says why —
  `default (auto: no fresh usage readings)` — and links to Settings → Anthropic tokens, which
  is where that particular problem gets fixed. Those runs are the only ones that carry a
  warning colour: nothing failed, but your pool did no work, which is usually a setting you
  can change. A run that spent the least-consumed token of a pool where **every** token was
  nearly exhausted is called out separately, in blue rather than amber — it worked, but your
  pool is nearly out — and links to the same page. `uzi worker list` also gains a **TOKEN**
  column, so the three-way choice the CLI could already *set* is finally one it can *show*
  (PRD #111 M1 + M5).

- **A run that hits your Anthropic usage limit can now pause instead of failing.** Opt in
  per run from the run view, or set the default for every new run in Settings — off until
  you turn it on, and the Settings default is the **only** way to reach an autopilot,
  CI-fix, or self-improvement run, since none of those has a start button of its own. A
  paused run keeps its branch, its history, and an already-approved plan: once your 5-hour
  or 7-day window reopens it resumes on the same worker in the same session, without asking
  you to re-approve a plan it already had. It can pause more than once if the limit keeps
  recurring, backing off between attempts, up to a retry budget an operator can tune
  alongside how long any one pause may last (`RUN_LIMIT_MAX_WAITS`, `RUN_LIMIT_MAX_PARK` —
  see [docs/configuration.md](docs/configuration.md)). A run that stays opted out still
  fails the moment it hits the limit, exactly as before, but now says which window and when
  it resets instead of a bare `agent run failed: error_during_execution` (PRD #35).

### Changed

- **The board's implicit first column is now named Backlog, not Open.**
  Display only: it's still the lane for issues carrying none of the
  configured column labels (PRD #102 M1).

- **Newly connected repos now seed Planned, In Progress, Human Review,
  Later, with Planned first, replacing Upcoming.** Existing boards keep
  their Upcoming column as-is; see
  [docs/configuration.md](docs/configuration.md) for the manual rename
  procedure (PRD #102 M2).

- **Only PRD-labelled issues can start a run.** Start-run previously
  accepted any issue with a PRD link or the PRDLESS escape hatch, so a
  stranger's open issue that merely mentioned a `prds/*.md` path could
  satisfy both and become runnable, by hand or from autopilot. The label
  gate now runs first and applies uniformly to the manual handler, the
  poller, and the CLI (PRD #102 M6).

- **Anthropic token labels can no longer contain invisible formatting characters.** Zero-width
  spaces and joiners, bidirectional overrides, the byte-order mark and the soft hyphen are now
  rejected when a token is created or renamed, alongside the control characters that were
  already rejected. The reason is what a label is for: it is the string you read to answer
  "which account did this run bill?", and an invisible codepoint breaks exactly that — a
  right-to-left override makes a label *read* as a different account, and a zero-width space
  makes two genuinely different tokens draw identically in a browser while remaining distinct
  rows. **The accepted cost, stated because it is a real one:** the zero-width joiner is itself
  one of these characters, so multi-part emoji built from it (`👨‍👩‍👧`) can no longer be used in a
  label; a single-codepoint emoji (`🔑`) is unaffected. Rejecting only the bidirectional
  controls would have left the look-alike problem in place while appearing to fix it. Labels
  saved before this landed are untouched (PRD #111 M2).

- **A guardrail denial no longer reads as a failure in the run activity feed.** When a
  PreToolUse guardrail denies a call — a `git push`, an env or `/proc` read, an unassembled
  subagent — the agent recovers and the run carries on, but the feed painted the result with
  the same red "✗ error" badge as a genuine tool crash, so a healthy run looked broken to
  anyone watching. Such results now render a third, calm state: a neutral "⊘ blocked" chip
  (only the glyph is warn-tinted, because a full warn chip would collide with the
  "plan awaiting approval" and slow-duration tints) that stays collapsed rather than
  auto-expanding, since a handled and recovered-from denial does not want attention. A real
  failure is untouched — still red, still auto-expanded. The chat surface inherits it, as it
  renders through the same row. Detection is render-time, keyed off the `"denied by guardrail"`
  phrase all 15 deny reasons carry, so historical runs get the calm chip too with no persisted
  marker and no migration; `is_error` is deliberately left true on the stored frame, which keeps
  the record honest to what the SDK emitted and is what leaves every downstream reader — the
  judge's missing-tool prescan among them — seeing exactly what it saw before. The phrase must
  START a line of the result, not merely appear in it: matching anywhere would have meant a
  genuinely failing command whose output quotes the phrase rendering calm and collapsed, and this
  change created that case itself, since a red `npm test` in the agent prints test titles carrying
  the phrase. The coupling to the phrase is real and is pinned from the agent side, since the two
  are separate npm packages: `agent/test/guardrails.test.ts` now drives all 15 deny paths through
  the public API and also scans the reason declarations in source, so a future 16th reason added
  without the phrase — or written in a form the scan cannot read — fails there rather than
  silently turning its chip red again (issue #116).

### Fixed

- **A crash-looping hosted worker's diagnostics no longer vanish the moment its pod
  reports Ready.** A `settled` roll report overwrote `blocking_container`,
  `blocking_reason`, `restart_count` and `last_exit_code` with empties whether or not it
  had looked at them, so a worker with five restarts and exit 1 could read as pristine
  in the database at exactly the moment someone was reading the row to debug it. The
  four columns now move together and are only replaced by a report whose phase
  (`rolling`/`stuck`) means the controller actually measured them; the worker's own
  authenticated version move still clears them (issue #145).

- **The Workers page badge no longer flickers `upgrade failed` → nothing →
  `upgrade failed` while a container crash-loops.** The worker container has no
  readiness probe, so `Ready == True` means only that the process started, not that the
  agent works — a Ready-but-flapping container was being reported `settled`. A Ready pod
  is now withheld from `settled` while any container has 3+ restarts and its current
  instance has been up less than 10 minutes, and self-clears once the container stays
  up. A negative container uptime (clock skew between kubelet and controller) no longer
  reads as flapping either (issue #145).
  See [docs/worker-upgrades.md](docs/worker-upgrades.md).

- **The run-view usage tables' left-aligned cells (the Agent, Phase and Model columns)
  are legible again instead of uniformly dimmed.** Two Tailwind classes of equal
  specificity were both emitted on the same cell, and stylesheet order picked the
  muted one every time (issue #152).

- **The run-view usage tables are usable with a screen reader.** Column headers now
  carry `scope="col"`, each table has its own accessible name, and the decorative
  disclosure triangles next to each `<details>` no longer get announced as a second,
  contradictory reading of the same expand/collapse state.

### Security

- **Unicode "format" characters — bidi overrides like U+202E, zero-width
  spaces/joiners, the BOM — in untrusted text can no longer make it render in an
  order its bytes don't have (Trojan Source, CVE-2021-42574).** Stripped at
  render — in visible text and in attributes like tooltips and accessible names —
  everywhere judge output, run titles, forge- or agent-supplied issue, proposal and
  memory text, the worker fleet page, and a filed-issue draft seeded from judge text
  reach the UI; stripped at review ingest so a hostile character can no longer be
  stored in the first place; and stripped from the CLI's worker table. A worker's own
  name is covered too — the one case where the reader isn't the field's own owner: it
  could otherwise render, crafted, in an admin's fleet list next to a different user's
  email. Coverage is per-surface, not blanket: `agent_label` is a separate, still-open
  gap tracked as issue #164 (issue #124).

- **The judge no longer recommends installing a tool that policy permanently forbids.**
  A credential-bearing CLI such as `glab`, `gh`, `aws` or `az` is barred outright, even
  against an explicit admin allowlist, because a logged-in one reachable from the agent's
  shell would defeat the rule that the worker holds the forge credential and the agent does
  not. But nothing told either half of the judge that: the deterministic missing-tool scan
  keys on a `command not found` string rather than on whether the tool could ever be
  installed, and the judge model was never told the class exists. Both could produce an
  install-worker-tool recommendation nobody could action, and two such recommendations had
  reached the backlog. Seeing one of these absent is the policy working, so the scan now
  drops them and the judge prompt names the barred class, redirecting the model to report
  the wasted effort as a prompt or agent defect, which is actionable. Ordinary missing tools
  are unaffected and still reported. The
  suppression matches on the EXECUTABLE rather than the package name, which is the part
  that is easy to get wrong: the barred package is `awscli` while the command a shell
  reports is `aws`, so a name comparison would have covered `glab` and quietly missed the
  whole cloud-CLI half of the list.

- **Two password-manager CLIs an admin could allowlist are now barred, as was always
  intended.** The list of credential-bearing tools that may never be installed carried
  entries for `op` and `bw`, which are not real package names and therefore matched
  nothing — the 1Password and Bitwarden CLIs have been installable the whole time. The
  real names are now on the list. Nothing in the live allowlist or any repo's tool
  profile named either, so no existing configuration is affected. What changes is that an
  administrator can no longer add either one to the allowlist; a repository asking for one
  was already refused, simply for not being allowlisted rather than for being barred.

## [0.11.12] - 2026-07-27

### Fixed

- **`file` did not work in 0.11.11, despite that release announcing it.** The package was
  declared as bare `file`, and devbox resolved that to the package's `dev` output -- headers
  and libmagic, containing no `file` program at all. `devbox global install` reported success,
  so the image shipped with `file` simply absent from the toolchain, exactly the silent
  `command not found` the release was meant to remove. It is now declared `file.out`, the
  output that carries the program, and verified from the realized profile rather than from a
  shell that had the raw store path on its search path -- which is what made the original
  check look green. This is the same trap `openssl.bin` documents; the earlier note reasoned
  from a nixpkgs attribute that turns out not to govern what devbox installs, and that
  reasoning is now corrected in place. Found by the new guard below, on its first real run.

- **The worker image's toolchain guard had stopped covering the toolchain, and now cannot
  drift again.** The guard exists so a broken toolchain fails the image BUILD instead of
  shipping a silent `command not found` to every subagent at run time. It was a hardcoded
  line naming five binaries, and it never grew as the manifest did: `chromium`,
  `fontconfig` and `dejavu_fonts` arrived, then `file`, `perl`, `coreutils`,
  `kubernetes-helm` and `kubeconform` in 0.11.11, none of them guarded. By that release it
  was verifying 5 of 13 packages while reporting success, so a green `publish:agent` said
  nothing about whether `helm` resolved. Worse than an uncovered package is a green light
  nobody re-derives. The guard now checks its own coverage: it reads what `devbox global
  install` actually installed and fails the build if any package lacks a row in
  `agent/devbox-global/toolchain-guard.tsv`, so adding a package without guarding it is no
  longer possible. Each row also runs its tool rather than only resolving it, because a nix
  package can land its libraries and manual pages while its CLI never appears -- the
  `openssl.bin` bug this guard missed the first time. Packages that legitimately ship no
  binary are declared as such rather than guessed: `fontconfig` sounds like it provides
  `fc-cache` and does not.

## [0.11.11] - 2026-07-27

### Added

- **Five more tools every run can reach: `file`, `perl`, `fmt`, `helm` and `kubeconform`.**
  The first three close judge recommendations raised by runs that lost work to a missing
  executable — a fact-checker hex-dumped a PNG header to read its magic bytes because it had
  no `file`, a reviewer's mutation sweep exited 127 on `perl -0pi -e` and had to be rewritten
  as a Python heredoc, and a docs run hand-rewrapped the same paragraph twice for want of
  `fmt`. `helm` and `kubeconform` are new capability rather than repair: an agent editing
  `deploy/chart/` can now lint, render and schema-validate it in-worker instead of committing
  blind and learning from CI's `helm_chart` job minutes later. All five ride the same pinned
  nix/devbox path as the rest of the toolchain and are baked into the image at build time, so
  they cost no per-run provisioning. Two caveats are worth knowing rather than discovering:
  `coreutils`, which is where `fmt` comes from, places GNU versions of 82 busybox applets
  (`ls`, `cp`, `date`, `sort`, `stat`, …) ahead of the image's own on `PATH`; and the pinned
  nixpkgs revision carries helm 4 while CI gates on helm 3, so a local render is a smoke
  check and never the authority over a red `helm_chart` job.

### Changed

- **Hosted workers may now reach the CloudNativePG chart repository, at the cost of two
  general-purpose GitHub hosts.** Rendering uzi's own chart needs `helm dependency build` to
  fetch the `cluster` subchart, and that fetch redirects twice before it lands: from
  `cloudnative-pg.github.io` to `cloudnative-pg.io` for the index, then from `github.com` to
  `release-assets.githubusercontent.com` for the 63 KB tarball. All four names are now in
  `workers.fqdnEgress.allowFQDNs`, in both the chart default and the dev-cluster values —
  Helm replaces list values rather than merging them, so an entry added to only one of the
  two reaches no cluster. The last two names are by a distance the widest entries in that
  list (any repository, any release asset) on a fence whose stated purpose is the
  repository-exfiltration residual, so this is a deliberate trade and not an oversight. The
  same chart is also published as an OCI artifact on `ghcr.io`, which would narrow this to a
  registry carrying no repository or code access if the `Chart.yaml` change proves worth it.

### Fixed

- **The agent no longer reinstalls dependencies the worker has already provisioned.**
  0.11.9 added JS dependency provisioning before the agent's first turn, and it works: the
  first real run after it shipped installed both workspaces before the agent's first tool
  call, and hit zero `command not found` for a gate tool where earlier runs hit them
  routinely. But nothing told the agent any of that, so it planned an `npm ci` at plan time
  (when the background install genuinely has not finished yet) and then ran it. Since
  `npm ci` deletes `node_modules` before installing, that destroyed the provisioned tree and
  rebuilt it, costing exactly the time the feature exists to save. The agent is now told:
  the plan prompts state the mechanism without promising it will succeed, since the install
  can fail; the implement prompt carries the actual per directory results, reporting a
  failed directory as failed so the agent can react rather than trusting a claim that is
  false for that run. It also now says when discovery hit its directory bound, so a bounded
  scan cannot read as full coverage (issue #157).

- **Repo supplied directory names reaching the agent's prompt are contained.** Those names
  come from reading an untrusted cloned repo, and they land outside the fences that mark
  quoted content as data, which is the one position where instruction shaped text is what
  those fences exist to stop. A repo could commit a directory whose name reads as an
  instruction. Names are now filtered to a conservative character set, length bounded, and
  placed inside a nonce fence whose tag a repo cannot forge, since the tag is drawn at
  random during the run while the name was committed to git before the run existed. A name
  the filter had to alter is flagged as not being a usable path, with a pointer to locate
  the real directory, so honest names like `my project` or `café` do not silently become
  paths that cannot be found. This reduces the surface rather than closing it: the fence
  relabels the text as data, it does not remove it from the model's context (issue #157).

## [0.11.10] - 2026-07-27

### Fixed

- **The crossed-off steps in the dashboard's "Get the factory running" checklist are
  legible again.** The strikethrough was drawn in a border token (`--edge-strong`) sitting
  at roughly 1.9:1 against the card, while the struck text beside it is at roughly 6.9:1 —
  so the line marking a step done was all but invisible, in both the ember and mission
  themes. The decoration now inherits the muted text colour, which keeps it legible in
  every theme without adding a token (issue #60).

- **Two objects the chart declares were being deleted from the rendered manifest, so
  restricted-tier hosted workers could not be provisioned.** A Helm template comment
  ending `*/ -}}` immediately before a `---` trims the newline as well, gluing the
  document separator onto the previous value. The two objects on either side then merge
  into a single YAML document with duplicate keys, and a parser keeps only the last — so
  the `uzi-workers` ServiceAccount and the `InfisicalSecret` that materializes its Harbor
  pull secret silently vanished, taking the pull secret with them. The docker tier was
  unaffected only because its object happened to come last in each merged pair. Nothing
  cheap caught it: `helm lint` passed, `helm template` exited 0, the text still contained
  `kind: ServiceAccount`, and ArgoCD reported `Synced/Healthy` truthfully, being in sync
  with what the manifest declared once parsed. `scripts/assert-chart-render.sh` now
  asserts one `kind:` per document in CI (issue #149).

## [0.11.9] - 2026-07-27

### Added

- **JS dependency provisioning before the agent's first turn.** A run touching a JS repo (npm, pnpm, yarn, or bun, including monorepo workspaces) now gets its cloned repo's dependencies installed automatically: the worker detects the lockfile and kicks off a frozen, `--ignore-scripts` install as the run starts, overlapping it with the plan turn (and the approval wait, on human-gated runs) so it's ready before the agent's first implement turn, instead of the agent discovering a missing `node_modules` mid-task. Runs under the same runner-uid + scrubbed-env sandbox that already runs the repo's tests; an unrecognized layout or a failed install is skipped honestly, same as before (PRD #121).

- **A Model column in the run view's per-agent usage table.** The usage panel named the run's
  model only once, in the top strip — the main thread's. A run is multi-model (a subagent can
  pin its own model, PRD #37; the owner's default governs the lead, PRD #69), so the per-agent
  breakdown now shows which model each agent actually ran on: the model string normally,
  `<primary> +K` for an agent that spanned several, `—` for a run whose frames predate the
  feature. Tokens only — per-agent cost remains unavailable, and no cost surface changed
  (PRD #93).

### Fixed

- **A hosted worker that can never get a pod is now alerted on, and the alert no longer
  expires.** Two defects found by live cluster validation of 0.11.8, neither catchable
  before deploying. First, `deriveRollHealth` returned `rolling` unconditionally when a
  worker had zero pods, and every stuck-detection arm needs a pod, so a worker whose
  Deployment could not create one (a missing ServiceAccount, in the measured case) was
  reported as a healthy in-progress roll forever. It now reports `stuck` plus the
  Kubernetes reason when the Deployment carries `ReplicaFailure=True` (issue #148).
  Second, that alert then expired: the anti-suppression ceiling gated the `upgrade_failed`
  row as well as the `upgrading` one, and its clock starts when the api first believes a
  roll is in progress, which for a hosted worker is while it is still being provisioned.
  On the measured worker that left a 70-second window out of 45 minutes before the badge
  went quiet again, permanently. The ceiling now gates only the suppressing direction, so
  a worker the cluster keeps reporting as stuck keeps its badge until the pod recovers
  (issue #151). See [docs/worker-upgrades.md](docs/worker-upgrades.md).

- **A self-improve check could outlive its 15-minute cap and orphan a process.** The
  wall-clock cap was enforced by `execFile`'s own `timeout`, which kills from the worker
  uid, and under the runner-uid split that is `EPERM` against a process running as
  `runner`. Measured: a 2-second cap called back at 2008ms carrying `EPERM` while the
  runner's `sleep 120` was still alive six seconds later. Checks now run as a detached
  process group that is killed as a group (issue #153).

- **A check no longer runs against dependencies its own install failed to build.** The
  `node_modules` pre-flight treated a surviving directory as "deps ready", but a failed
  `npm ci` leaves the previous tree intact with the new dependency absent, so the install
  failed, the directory remained, the pre-flight passed, and the check reported a
  real-looking failure that was really a stale tree. The signal is now the gap between
  what the manifest declares and what the tree contains (issue #154).

- **The `/nix` guidance stopped flickering out of the upgrade detail strip.** A fast-failing
  `seed-nix` init container alternates between `CrashLoopBackOff` and `Error` as kubelet
  cycles it (a measured 71/29 split), and the explanation was gated on the first reason
  alone, so on roughly three polls in ten it vanished while the badge correctly stayed
  failed. The operator watched the explanation for an unchanged failure appear and
  disappear (issue #146).

- **The judge's missing-tool scan no longer flags a tool the run actually ran.** `scanCommandNotFound` used to text-match any `command not found` hit and report that tool as missing even when the same run later ran it successfully through an npm script (`node_modules/.bin/tsc`, `vitest`, `eslint`); it now checks up to a bounded window of the run's tool-invocation trace and drops a hit once that tool is seen running clean later in the run (PRD #121).

## [0.11.8] - 2026-07-26

### Added

- **Worker upgrade health.** Settings → Workers shows a per-worker upgrade badge, a Fleet
  upgrade summary, and a detail strip on a worker whose upgrade failed; the Workers nav item
  carries an alert-toned count of workers needing attention. `uzi worker list` gains an
  `UPGRADE` column. See [docs/worker-upgrades.md](docs/worker-upgrades.md) (PRD #113).

- **Builtin skills now ship allocated.** Each builtin carries a default shared allocation, seeded the first time uzi inserts the skill, so a fresh instance no longer needs an admin to click allocate before a builtin can reach a run. That gap was total rather than partial: allocations are what build the run's skill union, so an unallocated builtin reached nobody, not the subagents it was meant for and not the lead either. Seeding happens only on that first insert, so an allocation an admin removes afterwards stays removed; `ci-cd-norms` (whose row predates the mechanism everywhere, so its insert can never fire again) is backfilled onto existing instances by a migration, allocated to `coder` and `reviewer` (PRD #72 M2). See [docs/skills.md](docs/skills.md).

- **A `prd-lifecycle` builtin skill**, allocated to `lead` and `reviewer` by default: the playbook for updating the issue's linked `prds/*.md` file at the end of an issue run, ticking items only on direct evidence, and moving the file to `prds/done/` when (and only when) every item is complete. The lead's own instructions carry a short version of the same step so it does not depend on the skill being allocated, conditional in its wording on the issue actually linking a PRD, and the plan prompt now asks the submitted plan to name that step, so a human approving a plan sees that the repo's spec file changes too. Both layers are instructions to a model rather than enforcement: the merge request stays the check on whether a PRD update is honest (PRD #72 M3). See [docs/skills.md](docs/skills.md).

- **A run started with repo agents now carries the run's delivered skills.** Allocations key on your agent templates and a repo's `.claude/agents/` roster has none, so repo subagents previously received no delivered skill at all: a run started with agents from git silently lost every skill its owner had allocated. Each repo subagent now receives exactly the run's materialized skill union, which is the same set the lead already receives and no superset of it. Per-template scoping is unchanged on template runs. The trade, recorded rather than discovered later: a repo-authored subagent can now read every delivered skill body and could write it into the branch, accepted because skill bodies are user data and never secrets by product policy (PRD #72 M1). See [docs/repo-agents.md](docs/repo-agents.md).

- **The issue's own PRD link is corrected after the merge.** When an issue run reports that it moved its PRD to `prds/done/`, uzi rewrites that link in the issue description once the run's merge request has merged, so the link a human clicks still resolves against the default branch. It is edge-triggered (once, then never again, so it does not fight a later human edit) and only ever repoints a `prds/*.md` link the description already carried, matched on the moved file's name, so a link to a different PRD is untouched. The bound is that a run cannot introduce a link, not that it cannot pick among the ones already there: an issue linking several PRDs can have the wrong one repointed if the run declares a matching filename, which costs description integrity on that one issue and nothing wider. `ci_fix` and `self_improve` runs are excluded at both the write and the read side. Adds `UpdateIssueDescription` to the forge interface and both drivers (PRD #72 M5). See [docs/autopilot.md](docs/autopilot.md).

### Changed

- **`UZI_AGENT_VERSION` now carries a real build stamp instead of a frozen placeholder.** CI
  stamps the release into the agent image, and an unstamped image reports no version rather
  than the retired `0.1.0-m4` literal — so a hand-set value is no longer the only thing that
  variable could mean (PRD #113).

## [0.11.7] - 2026-07-26

### Added

- `uzi tui`: a full-screen terminal UI — a live board of your runs, a run detail view with a per-agent lane rail and live transcript, and in-place steering (follow-up, approve/reject, cancel) and judge-review triage, all without leaving the keyboard (PRD #112).

### Changed

- `/api/ws` now accepts a Bearer CLI token (`uzc_`/`uza_`) as well as a browser session cookie, so a headless client can subscribe to a run's live event stream; per-run authorization and the socket's origin check are unchanged (PRD #112 M1).

- **The `uzi` CLI strips more from untrusted text before printing it, which changes the output of existing commands** — not only the new `uzi tui`. `uzi run logs`, `uzi run get`, `uzi review show`, `uzi review backlog` and the disposition tables now also remove DEL (`0x7f`) and every Unicode format character (category `Cf`: the bidi overrides `U+202A`–`U+202E`, the isolates `U+2066`–`U+2069`, `U+200F`, zero-width spaces and joiners, the BOM, and the soft hyphen). Previously only C0 (except tab and newline) and C1 were removed, so a bidi override could visually reorder a judge's `target` or an agent's label into something it is not, and zero-width runes could silently consume a table column's width budget while drawing nothing. Printable text is unaffected, and `--json` output is byte-exact as before. If you script against the human tables, this removes characters that were previously passed through (PRD #112 M3).

- **Consequence of the above, stated because it is visible:** `U+200D` (zero-width joiner) is itself a format character, so **emoji ZWJ sequences decompose** in CLI output — a family emoji renders as its component people, a profession emoji as its parts. This affects all of the commands listed above, not only `uzi tui`. It is the accepted cost of rejecting the whole `Cf` category rather than an allowlist: an allowlist of "safe" format characters is a list somebody has to keep correct, and getting it wrong reopens the bidi-override spoof (PRD #112 M3).

### Fixed

- **Browser launches on hosted k8s workers get `--no-sandbox` again.** The worker's `CMD` is `npm run start`, and npm's run-script prepends `/app/node_modules/.bin` to `PATH` — so on the non-root (single-uid) start the real `agent-browser` CLI shadowed the PRD #87 shim, and every launch silently lost the flags the shim injects. Chromium then aborted on the setuid sandbox that the worker hardening makes impossible. The entrypoint now pins the runner PATH on both start modes, so the shim resolves first on k8s as it always did on compose. Runner children also stop resolving the worker's own `node_modules/.bin` (`tsx`, `tsc`, `esbuild`, …), which is the intended boundary (PRD #120, issue #120).

## [0.11.6] - 2026-07-25

### Added

- A run whose updates cannot be saved is now flagged **looping** with a reason that names the cause ("the agent's updates can't be saved, so it keeps resending them") instead of falling through to the 45-minute "taking longer than usual" wall clock. The signal is the API's own count of failed writes for that run, so unlike the existing loop detector it is not blind to a broken message stream (PRD #108).
- A confirmed per-run write loop is **stopped automatically**, in about a minute, under a conjunction of guards that must all hold — including "other runs on this instance are successfully saving messages", so an API-wide outage stops nothing. Such a run ends `failed` with the new stop kind `auto_stopped`, which `uzi run get` shows as a `STOP_KIND` row and the web styles as breakage rather than as a stop you asked for. Operators can turn the stop off with `UZI_AUTOSTOP_ENABLED=false`, which leaves the flag working (PRD #108). See [docs/run-auto-stopped.md](docs/run-auto-stopped.md).
- `GET /api/admin/cli-tokens` / `uzi admin cli-tokens` lists every CLI token in the factory with its owner — closing a gap `workers` hasn't had since PRD #42, where a token was visible to its own holder and nobody else. Read-only (no admin revoke) and never returns the token value or its hash, at four independent layers (query projection, generated row type, DTO, handler). The row does carry `last_used_ip`, so an `admin_ro` holder can see every user's source IPs — deliberate, since `admin_ro` is factory-wide read, but worth knowing before minting one (PRD #98). See [docs/cli.md](docs/cli.md#commands).
- The Slack health nudge now forks on cause: a persistence loop reads "⚠ This run's updates aren't being saved, so it keeps re-sending them", distinct from the pre-existing "⚠ This run looks like it's repeating the same step" (PRD #108).

### Changed

- `uzi worker list` gains a **VERSION** column. The operator page's first remedy for an auto-stopped run is "check the worker's version", and the web has rendered it since PRD #42 — so the page shipped a remedy one of its two audiences could not follow (PRD #108).

### Fixed

- The **docker worker tier** now gets the same values-gated egress allow to uzi's own in-cluster `web` Service that the restricted tier already had. PRD #87 M5 landed the rule in `worker-networkpolicy.yaml` only, so `web-ux` on a docker-capable worker had no target to drive — found by running PRD #87's M7 gate for real, where the browser launched correctly but every request to the UI timed out. Off by default; `dev-cluster` opts in (PRD #87 M5, MR !105).

### Known limitations

- Auto-stop needs at least one *other* run saving messages at the same time to act. On an instance running a single run at a time it will flag and notify but never stop — the correct behaviour on insufficient evidence, and the reason the flag, not the stop, is the value on that deployment shape (PRD #108).
- A run failing because the **worker itself** is sending malformed requests is flagged but never auto-stopped, because a worker defect affects every run that image touches and hiding them one at a time would hide the pattern. The remedy is to roll the worker image; the log line carries `failure_class=invalid` (PRD #108).

## [0.11.5] - 2026-07-25 [NOT RELEASED]

Version burned; nothing was published under it. The tag was pushed against the wrong commit (a bare-clone root's stale detached `HEAD`, carrying `Chart.yaml` 0.9.0). The tag pipeline's `publish:assert-version` job compares chart `version`/`appVersion` against the tag and blocks **all** pushes on a mismatch, so no image and no chart reached Harbor — the guard did exactly what it exists for. The tag is protected and could be neither deleted nor moved, so 0.11.5 is skipped rather than reused. Its intended payload shipped as 0.11.6.

## [0.11.4] - 2026-07-24

### Changed

- The agent worker's Claude Agent SDK moves 0.3.201 → 0.3.219, so `opus` resolves to **Opus 5** rather than the previous generation. The builtin agent templates pin `model: opus` (ten of the eleven roles; `documenter` is sonnet) and the alias is resolved by the `claude` binary the SDK bundles — so runs on a role that pins `opus` change model with this release, without any template edit.
- `openssl` is baked into the default worker toolchain instead of being a per-repo tier-2 package. It is a broadly useful base crypto/TLS CLI rather than a repo-specific extra, so every worker gets it without a repo opting in. (Specifically `openssl.bin` — the bare `openssl` attribute installs no CLI, which failed the image build guard with exit 127.)

## [0.11.3] - 2026-07-22

### Fixed

- The `Select` UI primitive no longer discards a caller's `className`, so the per-worker Anthropic token picker on the Workers page is styled like every other field instead of rendering as an unstyled native `<select>`. `Input` and `Textarea` already merged correctly; only `Select` was broken (issue #118).
- The your-usage card's "see per-run detail →" link no longer orphans its arrow onto a line of its own at narrow widths (issue #117).

## [0.11.2] - 2026-07-22

### Changed

- Meter colour thresholds move to **warn ≥40%**, **danger ≥85%** (from 80/95), so a rate-limit budget reads amber well before it is nearly gone. The status pill is decoupled from the bar: only a ≥95% window escalates it to "nearly out", and an 85–94% row keeps a green "Live" pill while the bar carries the red. A dedicated ≥95% screen-reader announcement keeps that escalation audible now that the danger tone steps at 85 (PRD #115). Busy workers now sit amber or red as their steady state — an accepted trade for the earlier warning.

## [0.11.1] - 2026-07-22

Re-ships the PRD #87 browser prebake + `web-ux` builtin (v0.11.0, rolled back to v0.10.1 after live testing on dev-cluster caught three cluster-only bugs). Fixes all three (issue #114).

### Fixed

- Docker-tier workers no longer CrashLoop at `seed-nix`: the browser build guard, running as root, created a `root:root 0700` directory in the prebaked Chromium nix closure that the non-root (uid 10001) seed tar could not read; `/nix` store permissions are now normalized after the guard in both worker Dockerfiles (BUG 1).
- The prebaked browser now launches under the hardened worker: the `agent-browser` shim's `XDG_CONFIG_HOME` is uid-scoped so the Crashpad database directory (previously baked `root:root` by the root build guard) is writable by uid 10001 at runtime. This resolves the `Chrome exited early without writing DevToolsActivePort` / Crashpad `recvmsg` reset failure, which is not caused by seccomp: a non-writable XDG is the sole determinant, confirmed on-cluster under both RuntimeDefault and Unconfined (BUG 2a and 2b, one root cause).
- The `uzi-hosted-workers-docker` ResourceQuota no longer over-counts storage: the controller now skips re-creating PVCs it already observes as present, ending the per-tick admitted-then-`AlreadyExists` creates that inflated `used.requests.storage` without decrement (k8s #119593) and pinned the quota at its limit, blocking new workers (BUG 3).

## [0.10.1] - 2026-07-22

### Fixed

- Worker message batches carrying a NUL byte, an unpaired UTF-16 surrogate, or invalid UTF-8 are sanitized server-side instead of wedging the run in a silent retry loop (PRD #108).
- A permanently-unstorable message batch now returns 400 (never retried) instead of 500 (retried forever); an oversize batch returns 413 and is split, never treated as poison (PRD #108).
- The worker's message batcher bounds batch size, backs off exponentially, and bisects a rejected batch down to the single poisoned message instead of growing the retry body past the API's 1 MiB cap (PRD #108).
- A poisoned message is tombstoned in place (visible in the run's history) rather than dropped, so the live run view no longer freezes at a permanent gap (PRD #108).
- `/api/worker/runs/:id/state` text fields (`failure_reason`, `session_id`, `plan_md`, `branch`, `mr_web_url`) are NUL-stripped, closing the same poison-pill class on the sibling route (PRD #108).
- `run_usage`'s `session_id`/`model` are length-capped before the upsert, closing a second poison-pill route (an oversized composite-key index entry, Postgres 54000) (PRD #108).
- Run HOME cleanup now restores write permission before removing a tree, so a Go module cache's read-only directories no longer strand disk on every Go-touching run; a one-off startup sweep (`UZI_HOME_RECLAIM`) reclaims HOMEs already stranded on existing volumes (PRD #108).

### Security

- Worker-side redaction now covers the `agent` and `kind` message fields, not just the payload and `agent_instance`/`agent_label`, closing a gap where a secret placed in either field reached the API, the WebSocket frame, the browser, and `uzi run logs` unscrubbed (PRD #108).
