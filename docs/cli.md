---
title: uzi CLI
order: 105
audience: user
---

# uzi CLI

`uzi` is the terminal control surface for the factory: it drives the same API
the web UI does, so anything you can do in a browser you can do headless —
list and follow runs, approve/reject/revise a plan gate, read the judge's review,
manage workers and repos, and (read-only) admin state. Built for humans
(tables on a TTY) and agents (`--json`, documented exit codes) alike.

## 1. Install

```sh
brew tap vtmocanu/tap
brew trust --formula vtmocanu/tap/uzi-cli   # one-time: Homebrew 6+ requires trusting third-party formulae
brew install vtmocanu/tap/uzi-cli
uzi version
```

`vtmocanu/tap` resolves to `github.com/vtmocanu/homebrew-tap` on its own (Homebrew maps
a bare `user/repo` tap name to `github.com/user/homebrew-repo`), so no explicit remote
is needed. `brew trust` is a one-time step: Homebrew 6+ refuses to load a third-party
formula until it is trusted, so trust just the `uzi-cli` formula rather than the whole
tap.

The formula builds `uzi` from source: `brew install` downloads the release source
tarball and runs `go build` (Homebrew installs Go as a build dependency). No access to
the product repo is required.

On first run the CLI also drops a Claude Code skill at
`~/.claude/skills/uzi-cli/SKILL.md` (generated from the binary's own command
tree, so it never drifts). No manual step is needed, but you can force a
refresh — e.g. right after upgrading — with `uzi skill install --force`, and
check it with `uzi skill status` (details under **Bundled skill and
session-start hook**, below).

## 2. Point it at your instance

Every command needs a base URL: pass `--url`, set `$UZI_URL`, or run
`uzi login` (below), which saves it for you. Only `https://` URLs are
accepted — plus plain `http://` on `127.0.0.1`/`localhost`, for a local
compose stack — so a credential is never sent in the clear.

## 3. Authenticate

Two paths, one credential.

**Human — `uzi login`.** Browser-brokered, no loopback listener, so it works
over SSH and in containers:

```sh
uzi login
```

It prints a one-time code and a URL, opens your browser to **Approve CLI
login**, and waits. Type the code from your terminal, pick a scope, and
approve — the CLI's token is saved to `~/.config/uzi/credentials.toml`
(0600) the moment you do.

**Agent or CI — a static token.** Mint one in **Settings → Access**, copy it
once (it's shown exactly once), and set it:

```sh
export UZI_URL=https://your-uzi-instance
export UZI_TOKEN=uzc_...
uzi run list --json
```

`UZI_URL`/`UZI_TOKEN` need no `$HOME`, no browser, no cookie — the whole
headless path. **In CI, keep `UZI_TOKEN` in a secret (e.g. a GitHub Actions
secret), never a plain variable.**

**Holding more than one credential?** Both `uzi login` and `uzi auth token`
can target a *named context* instead of overwriting the one you already have —
see [Named contexts](#named-contexts) below.

## Commands

```
uzi login | logout | auth token [--with-token] | auth status [--all] | whoami
uzi context list | current | use <name> | set <name> --url <url> | rm <name>
uzi run list | get <id> [--field <name> ...] | logs <id> [--follow] [--after <seq>]
uzi run wait <id> [--until <status,...>] [--interval <dur>] [--timeout <dur>] [--min-plan-seq <n>]
uzi run create --repo <id> --issue <iid> [--plan-file <path>]
                [--agent-source own|repo] [--exclude-agents a,b]
                [--planned-commit <sha>] [--require-base] [--force]
                [--harness claude|codex]
uzi run approve <id> [--agent-source own|repo] [--exclude-agents a,b]
                [--expected-gate-revision <n>]
uzi run reject <id> [--message <text>] [--expected-gate-revision <n>]
uzi run revise <id> [--message <text>] [--expected-gate-revision <n>]
uzi run cancel <id> [--discard-pending-outcome]
uzi run stop <id> [--message <text>]
uzi run scope <id> --through <n>
uzi run extend <id> --by <duration>
uzi run pause <id> [--now|--cancel]
uzi run resume <id>
uzi run resume-now <id>
uzi run decide <id> (--continue [--guidance <text>] | --partial <ids> --reason <text> | --accept <ids> --reason <text>)
uzi run follow-up <id> [--message <text>]
uzi run answer <id> [--message <text> ...]
uzi run inputs <id> [--json]
uzi run expedite <id> [--clear]
uzi run rework <id> [-m|--message <text>]
uzi run export <id> --output <path> [--capture <id>]
uzi run fetches <id> [--json]
uzi run recovery [<id>] [--json]
uzi run discard <id> --hold <hold-id> [--yes]
uzi schedule create --repo <id> [--repo <id> ...] (--issue <iid> | --sweep [--label <l> ...] [--create-missing-labels] | --prompt <text>)
                    (--at <rfc3339> | --cron <expr>) [--tz <iana>]
                    [--auto-approve[=false]] [--wait-on-limit[=false]]
                    [--max-issues <n>] [--guidance <text>]
                    [--output mr|issues]
                    [--model <alias|id>] [--apply-model-to-agents[=false]]
                    [--harness claude|codex]
uzi schedule list | get <id> | pause <id> | resume <id> | run-now <id> | delete <id>
uzi schedule pause-all --until <when> | resume-all | pause-status
uzi schedule edit <id> [--cron <expr> | --at <rfc3339>] [--tz <iana>]
                   [--prompt <text> | --label <l> ... [--create-missing-labels]]
                   [--auto-approve[=false]] [--wait-on-limit[=false]]
                   [--guidance <text> | --clear-guidance]
                   [--max-issues <n> | --clear-max-issues]
                   [--output mr|issues|""]
                   [--model <alias|id>] [--apply-model-to-agents[=false]] [--repo <id>]
                   [--harness claude|codex|""]
uzi schedule catalog list
uzi schedule catalog enable <slug> --repo <id> [--repo <id> ...] [--create-missing-labels]
uzi schedule reset <id>
uzi schedule clone <id> [--repo <id>]
uzi schedule add-repo <id> --repo <id>
uzi review show <id> | backlog [--bucket todo|filed|done|dismissed|all] [--category label,label]
uzi review resolve <id> <rec> | --category <c> --target <t>
uzi review dismiss <id> <rec> | --category <c> --target <t> --reason wont-do|not-an-issue
uzi review undo <id> <rec> | stats [--json]
uzi review file <id> <rec> [--repo <repo-id>]
uzi findings list [--repo <id>] [--bucket to_file|filed|done|dismissed|all] [--run <id>]
uzi findings draft <finding-id> [<finding-id>...] [--json]
uzi findings file <finding-id> [<finding-id>...]
uzi findings release <operation-id> --confirm-no-issue
uzi findings dismiss <finding-id> --reason wont-do|not-an-issue
uzi findings resolve <finding-id>
uzi findings undo <disposition-id>
uzi findings stats [--repo <id>] [--json]
uzi handoff -m <text> | -f <path> [--base <ref>] [--mr] [--review] [--then-fix] [--interactive] [--repo <id>]
uzi handoff rm <run-id> | review <run-id>
uzi token list
uzi rate-limits [--provider claude|codex]
uzi worker list | rm <id> | set-token <worker-id> <label> | set-token <worker-id> --default
uzi repo list | remove <id> [--force]
uzi project-sync status <repo> | resync <repo>
uzi pr list [--repo <id>] | checks <iid> [--repo <id>] [--watch]
uzi ci list [--repo <id>] [--limit <n>] | jobs <run-id> [--repo <id>] | fix <ref> [--repo <id>]
uzi admin users | runs | workers | usage | rate-limits | cli-tokens | products | guardrail-impact | blocked-repos
uzi admin health [--all] [--strict]
uzi admin agent-source get | status
uzi admin review backlog [--bucket todo|filed|done|dismissed|all] [--category label,label] | stats [--json]
uzi admin egress-profile list | show <name>
uzi skill status | install [--force] | install-hook | uninstall-hook
uzi docs list [--audience user|operator|design|contributor|all]
uzi docs show <slug>
uzi docs search <query> [--audience user|operator|design|contributor|all]
uzi tui [run-id]
uzi version
```

Global flags: `--json`, `--url <url>`, `--quiet`, `--no-color`,
`--context <name>`/`-c <name>`.

A few worth knowing:

- **`--harness` picks the run's execution engine; omit it to let the server
  resolve one.** `run create --harness claude|codex` and `schedule create
  --harness claude|codex` request a specific harness outright; omitting the
  flag leaves the server's own resolution in charge (your effective default,
  or whichever harness you have a usable credential for). `schedule edit
  --harness ""` clears a schedule's pin back to per-fire resolution; leaving
  the flag unset on edit leaves the stored pin unchanged. The resolved
  harness shows as a `HARNESS` row on `uzi run get`; on `uzi run list` it's
  a `HARNESS` column that shows `codex` for a Codex run and is left blank
  for a Claude run (the terse convention the CLI's list view uses
  throughout). A schedule's pin (or `implicit` when unset) shows on `uzi
  schedule get`.
  There's no CLI setter for your own default harness; that's a **Settings →
  Run defaults** web-only control (see [Worker model](./worker-model.md)).
- **`version` reports two coordinates, not one.** It always prints the CLI's own
  version (the `v*` release this binary was built from). When a server URL is
  configured it additionally reports that server's build info — version, full
  source commit, build time, commit count, founding date and uptime — because "what am I
  running" and "what is deployed" are different questions and the answers drift.
  The probe is best-effort with a short timeout, so `uzi version` exits 0 with or
  without a reachable server. Under `--json` the CLI's version stays top-level
  and the server's nests under `server`, leaving existing parsers untouched;
  fields the server did not stamp are omitted rather than sent empty, so an
  absent `server.commit` means "unknown", never "empty".
- **`uzi tui` also surfaces an available update, unprompted.** Where `version`
  only tells you on request, the TUI checks at every startup and shows a modal
  when this binary is behind the latest published release — see [Startup: an
  available update](#startup-an-available-update) below.
- **No `worker create` and no `admin` writes.** Minting a worker join token
  returns a credential that reads decrypted secrets, and every admin write
  stays cookie-only — both are web UI actions by design.
- **`token` is list-only, and `worker set-token` is the one write near it.**
  See [Anthropic tokens](#anthropic-tokens) below for why the split falls
  exactly there.
- **`uzi rate-limits [--provider claude|codex]`** — your own rate-limit
  meters, the terminal twin of the web sidebar meters. `--provider claude`
  (the default) lists your Anthropic tokens as `TOKEN`/`STATUS`/`5H%`/`7D%`;
  `--provider codex` lists your linked Codex accounts, one row per `(account,
  bucket)`, as `ACCOUNT`/`STATUS`/`BUCKET`/`PRIMARY`/`SECONDARY`/`RESET`. A
  window uzi has no reading for renders `—` (not `0`), so partial or unknown
  state is never confused with a genuine zero. `--json` returns the raw
  meters (`[]TokenRateLimitDTO` for claude, the nested
  `[]CodexAccountRateLimitDTO` for codex). Read-only; the cross-user view is
  `uzi admin rate-limits` below. See [Claude rate limits](rate-limits.md) and
  [Codex account limits](rate-limits.md#codex-account-limits) for what the
  numbers mean.
- **`run create --plan-file <path>` seeds the run with a plan you already
  wrote**, skipping the planning turn and the approval gate entirely — the
  worker implements it directly. Pass `-` to read the plan from stdin. See
  [Seeding a plan](./seeded-plans.md) for the full walkthrough, including the
  constraint that matters most: the plan must stand on its own, since a
  seeded run starts with no session and no memory of how it was written.
  `--agent-source`/`--exclude-agents` (below) and `--planned-commit`/
  `--require-base` (the base-commit staleness guard) are all optional and all
  require `--plan-file`; an empty or oversized plan is rejected at create
  time. A run created with no `--plan-file` is unchanged.
- **`run create --force` re-runs an issue that already has an open MR.** By
  default, creating a run for an issue whose last completed run still owns an
  open MR is refused (a 409, exit 5) — re-running would spend budget building
  onto a branch you have not merged yet. `--force` overrides that one refusal
  so the new run is created anyway; it bypasses only the open-MR guard, and a
  run already in progress for the issue is never bypassed.
- **`run approve` picks the subagent roster explicitly.** By default a run
  uses its own default roster; `--agent-source own|repo` overrides it
  (`own` = your template roster, `repo` = the agents the worker detected in
  the clone's `.claude/agents/` or `.codex/agents/`), and `--exclude-agents a,b`
  drops individual subagents from that source. `--exclude-agents` requires
  `--agent-source`. Detection reads one folder, never merges rosters, and prefers
  the run harness's native folder; a present preferred folder is final even if
  empty or invalid. See [Repo agents](repo-agents.md#which-folder-is-read).
  `run create --plan-file` takes the same two flags, for the seeded run's
  roster.
- **`run revise <id> -m "<feedback>"`** steers a plan at the approval gate
  without stopping the run: the agent re-plans from your feedback and returns
  to `awaiting_approval` for another decision, where `run reject` instead ends
  the run. Use it on a run parked at the gate; it needs a non-empty message
  (`-m`/`--message`, or piped on stdin). Revisions are capped by the run's
  revision limit, and an exhausted limit — or a run that has already finished —
  is a 409 (exit 5).
- **`run approve|reject|revise` bind to a plan-gate revision.** Every plan
  presented at the gate has a revision number, exposed as `gate_revision`
  (readable with `uzi run get <id> --field gate_revision`). Pass
  `--expected-gate-revision <n>` to make a verdict conditional on that exact
  revision still being current — the way to say "this is the plan I read."
  Without the flag, the CLI reads the run once, right before sending the
  verdict, and sends whatever revision it finds while the run is
  `awaiting_approval`; that only proves the verdict targets the gate current
  *at that moment*, not a plan you read earlier in a separate `run get` or in
  the web UI. A mismatch — the run has since shown a newer plan, or is no
  longer at the gate — is a 409 (exit 5) that names the current revision, and
  nothing is applied. `run approve --token <label>` switches the run's
  Anthropic token before approving; if the expected-revision check then
  refuses the approve, the token switch is NOT rolled back, and the error
  says so, so you know a bare re-run of `run approve` (no `--token`) is what's
  left to do.
- **`run stop <id>`** gracefully winds down a run. On an **interactive**
  task run (one created with `uzi handoff --interactive`; see [Interactive
  mode](./handoff.md#interactive-mode)) the current turn
  finishes, the branch is pushed, an MR opens iff `--mr` was set at handoff,
  and (via the server's `completed` transition) `--review` fires iff it was
  requested, then the run lands `completed` with a distinct stop disposition.
  Unlike `run cancel`, which aborts mid-turn, `stop` never discards in-flight
  work. If a run finished on its worker but its outcome never reached uzi (an
  api outage held it on the worker), `run cancel` refuses with a confirmation
  error unless you pass `--discard-pending-outcome`, which cancels the run and
  throws that finished result away. On a **milestone-structured issue run** (PRD #634), `stop` instead
  sets an operator scope ceiling at the already-completed milestone count:
  the run finalizes the committed slice (pushes the branch, opens the MR
  when requested) and starts no further milestone — the same graceful
  finalize, just capped at what's already done. Use `run scope --through N`
  below instead to let the run complete through a later milestone before
  finalizing. An optional `-m`/`--message` (or piped stdin) rides along with
  the stop. A foreign or unknown run id is exit 4; a run that has already
  finished is exit 5.
- **`run scope <id> --through N`** (PRD #634) sets an operator **scope
  ceiling** on an in-flight milestone-structured issue run: the run
  completes through milestone `N` (1-based, over the approved, frozen
  milestone list), then finalizes the committed slice (pushes the branch,
  opens the MR when requested) and starts no further milestone. `N` is
  clamped to `[already-completed, total]` — an out-of-range value is
  clamped and reported, never rejected. A later `run scope` or `run stop`
  **supersedes** an earlier one: both write the same underlying ceiling, so
  `scope --through 4` then `scope --through 5` finalizes at 5, not 4. The
  applied (clamped) value and its disposition surface via `run inputs`
  below, not in `scope`'s own output, since a read-back there would race
  the worker settling it. Owner-only; valid only on a milestone-structured
  issue run (409 otherwise).
- **`run extend <id> --by <duration>`** (PRD #1189) grants a non-terminal,
  time-limited run more wall-clock time on top of its frozen budget — see
  [Giving a run more time](./run-health.md#giving-a-run-more-time). `--by`
  is required and takes a duration in Go's `time.ParseDuration` syntax
  (`2h`, `90m`, `1h30m`) plus a `d` unit (`1d` = 24h); it must resolve to at
  least 60 seconds (the server's minimum), so `0`, anything under a minute, a
  negative value, or an unparseable one is a usage error (exit 2). Valid on any non-terminal run
  of a kind the sweep can time out (issue, task, prompt, self-improve,
  mr-rework, ci-fix), including a queued or parked one — a chat, judge, or
  interactive-task run never times out, so extending one is a 409 (exit 5).
  Extending past the admin's per-run allowance, or while an admin has
  turned extending off entirely, is also a 409, with the server's message
  (naming the remaining allowance) printed verbatim. On success it prints
  the new deadline and the running total against the allowance, e.g.
  `Extended <id> by 2h. 3h05m left, times out 17:20. Extensions on this
  run: 2h of 16h allowed.` Owner-only.
- **`run pause <id> [--now|--cancel]`** (PRD #1190) parks a running run on a
  pushed checkpoint until you resume it — see [Pausing and resuming a
  run](./run-pause.md) for the full picture. The default finishes the
  milestone (or turn) already in flight, pushes a checkpoint, then parks;
  `--now` drops the turn in flight and parks on the last checkpoint instead,
  discarding whatever changed since; `--cancel` withdraws a pending request
  and leaves the run running. `--now` and `--cancel` are mutually exclusive.
  None of the three is synchronous — the park lands on the worker's next
  report, so watch `uzi run get <id>`. Owner-only, and valid only on a
  running issue, non-interactive task, prompt or self-improve run: a chat
  run already parks between turns, an interactive task parks after every
  turn, judge/mr-rework/ci-fix runs finish on their own, and a run already
  at a gate or an involuntary park is refused too — all as a 409 (exit 5)
  naming the reason.
- **`run resume <id>`** resumes a run the owner paused: it moves the run
  from `paused` back to `queued`, keeps the worker pin, and preserves the
  remaining budget (the clock stopped while paused, so resuming does not
  reset it). The claim then continues the same SDK session if that worker
  is still alive, or recovers the branch from the checkpoint and re-plans
  the remaining milestones if it's gone. A run that isn't paused is a 409
  (exit 5). It posts to the same endpoint as `run resume-now` below, which
  now also resumes a paused run in addition to a pool-held one.
- **`run decide <id>`** (PRD #1226/#1227) records the owner's decision on a
  run the [structural completion interlock](./run-completion-hold.md) has
  blocked — either a live completion question or a run already parked in the
  completion hold. It is **owner-scoped**: a foreign run (including from a
  read-only admin token) is hidden as a 404 (exit 4). Exactly one decision is
  required (its absence, or more than one, is a usage error, exit 2):
  - `--continue [--guidance <text>]` resumes the run past the check; the
    optional `--guidance <text>` steers it. Answered live, the run resumes in
    place with no new claim; answered after it has parked, it resumes through
    `queued` like `run resume`.
  - `--partial <ids> --reason <text>` reduces scope to the comma-separated
    milestone ids to **keep**; the rest are deferred, and the run's closing
    pull/merge request does **not** close the issue (it lists the deferred
    ids). `--reason` is required.
  - `--accept <ids> --reason <text>` waives the comma-separated criterion ids
    as met; a closing pull/merge request then names them in a warning block.
    `--reason` is required.

  `--guidance` is valid only with `--continue`, and `--reason` only with
  `--partial`/`--accept`. A partial or accept first reads the run's **current**
  contract revision and sends it, which the server fences: a stale revision
  (someone has already decided) is a 409 (exit 5), as is a run that is not
  completion-blocked. `--json` emits the resumed run object.
- **`run answer <id>`** answers the clarifying question a run is parked on
  (`awaiting_input`) — see [Answering a
  question](./run-activity.md#answering-a-question). It reads the open
  question from the run's own feed rather than a dedicated field (no DTO
  field exists), so `run get` first to see what's actually being asked. Pass
  `-m`/`--message` once per question when the agent asked several (matched
  in order), or pipe a single answer on stdin. The answer names the question
  it answers, so one written against a question the agent has already moved
  past is rejected rather than applied to the current one, and calling it
  against a run that isn't currently parked fails outright instead of
  queuing.
  **The CLI's derivation is narrower than the web's**: it always answers the
  *newest* question message in the feed, where the web additionally checks
  whether a newer `answer` has already closed it. That gap is real for one
  short window — between you answering (on any surface) and the run
  reporting its next state — where the web has already hidden its composer
  but a `run answer` invoked in that window still finds a question to
  target, submits, and gets back a 409: the question it read was already
  answered. Re-run `run get` if that happens; it isn't a sign anything went
  wrong with your first answer.
- **`schedule` runs work on a clock.** A schedule starts run(s) at future
  time(s) through the *same* shared seam a manual `run create` uses, so the
  `uzi`-label gate, the forge issue fetch, active-run dedup and the usage-limit park all
  behave identically — a schedule can do nothing a manual start can't. `schedule
  create` takes exactly one **target** (`--issue <iid>` a pinned issue; `--sweep`
  every candidate issue matching the repeatable `--label` selector, defaulting to
  the `uzi` label; or `--prompt <text>` an issue-less repo→MR run) and exactly one
  **timing** (`--at <rfc3339>` fires once, or `--cron <expr>` recurring in
  `--tz`); either constraint violated is a usage error before any request.
  Repeat `--repo` to create the same schedule on **several repos at once**: a
  **client-side fan-out** of one independent create per `--repo` (each repo prints
  its own `created schedule …` line, and `--json` returns them all as an array; a
  single `--repo` is unchanged, dumping the one schedule object), and a mid-loop
  failure still reports the schedules that already landed before it exits non-zero.
  Creating on **N>1 repos** stamps the rows with one shared **display-only** group id
  (the web lists each as its own row); the rows stay fully independent
  (editing/pausing/removing one never touches a sibling), and a single-`--repo` create
  is standalone (no group). An **`--issue` target cannot span repos** (the issue
  number is repo-relative), so `--repo A --repo B ... --issue N` is a usage error
  (exit 2) before any create; `--sweep` and `--prompt` targets group freely.
  `--auto-approve` defaults **on** (an off-hours run should proceed past the plan
  gate); pass `--auto-approve=false` to keep the gate. `--wait-on-limit` also
  defaults **on** for a new schedule — a fired run parks until the Anthropic
  usage window reopens instead of failing — and this now takes effect even on
  the common auto-approve path (a schedule's own setting used to be silently
  ignored there); pass `--wait-on-limit=false` to fail on limit instead. A
  `--sweep` target defaults `--max-issues 10`, the cap on issues started per
  fire, oldest issue first (must be positive; `0` or negative is rejected);
  raise it with `--max-issues <n>`, or drop the cap entirely with
  `schedule edit <schedule-id> --clear-max-issues` (same pattern for
  `--clear-guidance`) — both are nullable fields, and clearing one from the
  CLI no longer requires the web modal; `create` itself still defaults
  `--max-issues 10`. For a `--sweep` target, `create` first checks each target
  repo's forge for the explicitly-named `--label` selectors and prints a
  `WARNING` (to stderr) for any that is missing — the schedule is still created,
  but the sweep will not match until the label exists; pass
  `--create-missing-labels` to create them on the forge first (an empty
  `--label`, which defaults server-side to the `uzi` label, is not checked). The
  guardrail is **purely advisory and never blocks the create — not even on its own
  forge errors**: a failed label check or a failed label create (with
  `--create-missing-labels`) prints a `WARNING` and proceeds, so a transient forge
  outage (expired token, rate limit, forge unreachable) cannot abort a create.
  `--guidance <text>` attaches
  optional owner steering ("always add a failing test first") to an `--issue`
  or `--sweep` target only (a `--prompt` target rejects it, since a prompt
  already carries its own text), injected into the run instruction as a
  section separate from the issue body; it does not change which issues are
  eligible to run, is capped at 8 KiB, and is truncated — never dropped — if a
  large issue body plus guidance would otherwise push the composed instruction
  over its size limit. `--output mr|issues` sets a **prompt-target** schedule's
  proposal output mode and is valid only with `--prompt` (an `--issue`/`--sweep`
  target rejects it, exit 2): `mr` writes an idea file and opens an
  MR, `issues` files the proposal as a `proposal::<slug>`-labelled forge issue
  server-side (never sweep-eligible until a human promotes it). It is three-way,
  like `--model`: omit it to inherit the job/catalog default, `--output issues`
  (or `mr`) to set it, and `--output ""` on `edit` to clear it back to inherit.
  Without catalog output, the fallback is `mr`; feature bingo and refactor
  scout ship with `issues`. Stored `mr`/`issues` modes remain in effect; an
  existing NULL mode inherits the new catalog value on its next fire, without
  migrating the row. Reset adopts the current catalog baseline (`issues` for
  those two jobs). Use `--output mr` for their idea-file and MR delivery.
  `--model <alias|id>` (valid on every target) pins the
  model a fired run uses; add `--apply-model-to-agents` (default off) to also
  apply that model to every subagent, overriding each agent's own model pin.
  Both `--max-issues` and `--wait-on-limit`'s new default
  apply at create time only — existing schedules keep their stored values.
  `schedule list`/`get` read them, `edit <schedule-id>` changes a schedule's
  mutable config in place — retime with `--cron`/`--at`, adjust `--tz`, or,
  scoped to the existing target, `--prompt`/`--label`/`--guidance`
  (`--clear-guidance`)/`--max-issues` (`--clear-max-issues`)/`--auto-approve`/
  `--wait-on-limit`/`--apply-model-to-agents` (toggle the subagent model
  override) — without churning the id or
  run history the way delete-and-recreate would; `edit` now accepts
  `--model <alias|id>` to change the run model in place, valid on every
  target and origin (an empty string clears it back to the Worker-model
  default), and it preserves the stored `--model` and `--apply-model-to-agents`
  across a partial edit that does not pass those flags — a plain retime no
  longer wipes the stored model. On a **default-origin** schedule only
  the catalog-editable fields (`--cron`, `--tz`, `--auto-approve`,
  `--wait-on-limit`, `--max-issues`, `--clear-max-issues`, `--model`,
  `--apply-model-to-agents`) may be edited — no clone needed — plus
  `--guidance`/`--clear-guidance` on a **prompt-target or sweep-target default**
  (owner steering is editable there — a partial edit restates the stored guidance
  so it is not wiped; on a sweep default the guidance is an **overlay** composed
  onto the read-only baked catalog guidance at fire time); a **Reset** restores
  `--apply-model-to-agents` to its catalog baseline of `false` alongside the
  other catalog fields. The catalog-owned fields (`--prompt`, `--label`,
  `--repo`, `--at`, and `--guidance` on an issue default) still require
  `uzi schedule clone` first — the subagent-model toggle is not among them.
  Changing a sweep schedule's `--label`
  selector runs the same advisory sweep-label guardrail as `create`/`catalog
  enable`: it `WARNING`s (to stderr) on any newly-set label missing on the
  schedule's repo, or creates it first with `--create-missing-labels`, and never
  blocks the edit (an edit that does not touch `--label` runs no guardrail). Any field you omit keeps its
  stored value, and editing config revives a terminal schedule (its status
  returns to active — a recurring one resumes, a fired one-shot needs a fresh
  future `--at`) but does not un-pause a paused schedule, while `pause`/`resume`
  flip firing without
  deleting, `run-now` fires one immediately without disturbing its cadence,
  and `delete` removes it (run history is preserved). Once a schedule has fired,
  `schedule get` also prints a **Last fire** block below the config: a summary
  line (`fired <time> · matched N · started M · skipped K`), one line per started
  run (`#<iid> → run <run-id>  <title>`, or a `prompt` marker for a prompt
  schedule), one line per skipped candidate with a human reason label
  (`not eligible`, `already running`, `description too large`, `fetch
  failed` — the raw wire reason for anything newer), and, when a capped fire
  reached nobody, a hint that newer eligible issues weren't reached because the candidates
  ahead of them were skipped (it no longer suggests raising `--max-issues`:
  the fix is to clear those skips, and a higher cap also raises how many runs
  one fire can start). A label sweep also prints how many open issues
  match its selector but aren't eligible (`ineligible_matched`), when known;
  a never-fired schedule reads `Last fire: never fired`, and `--json` carries
  the same detail under `.last_fire` (including `.last_fire.ineligible_matched`
  when present — absent means unknown, not zero). `run-now` prints the
  matching per-candidate breakdown inline — a `Started N run(s)` header with
  the created run id(s), a line per started run, then an `Examined N
  candidate(s), skipped K:` tally with a reason label per skip (`add the
  configured uzi label or assign the issue to uzi` for `not eligible`, with no
  `--max-issues` hint), and the same `ineligible_matched` line when a label
  sweep reports it — or `no run started from <id>: no eligible candidates`
  when the selector matched only ineligible issues, or the plain `no run
  started from <id> (a matching run may already be active)` when a benign
  dedup fired none; `--json` dumps the raw response (`created`, `run_ids`,
  `matched`, `capped`, `started`, `skips`, and `ineligible_matched` when
  known).
- **`schedule pause-all` / `resume-all` / `pause-status`** work on **every
  schedule you own, on every repo** — a user-level kill switch, distinct
  from the per-schedule `pause <id>`/`resume <id>` above, which toggles one
  row. `pause-all` requires `--until <when>`, so a bare `pause-all` is a
  usage error (exit 2) rather than silently meaning forever; `--until`
  accepts an RFC3339 timestamp, a Go duration (`24h`), `tomorrow[ HH:MM]`
  (bare `tomorrow` defaults to `09:00`), a weekday name (`monday`, `Mon`)
  `[ HH:MM]` defaulting to `09:00` and resolving to its next occurrence, or
  `never` for an indefinite pause — every relative form is resolved
  **client-side, in your local timezone**, and sent to the server as a
  plain RFC3339 stamp (or `null` for `never`). While paused, no schedule you
  own fires unattended on any repo: a recurring schedule due in the meantime
  records a benign `schedules_paused` skip and its cadence still advances,
  a one-time schedule waits and fires once the pause lifts, `run-now` still
  bypasses the switch, and no per-schedule `pause <id>`/`resume <id>` state
  is ever touched — `resume-all` (or an auto-resume `--until`) restores the
  exact set of schedules that were on before. `pause-status` prints the
  current state (paused, and until when, or not paused); `schedule list`'s
  `NEXT` column reads `paused (all)` (with the resume stamp, if any) for
  every row that would otherwise fire while a pause-all is active; a row
  whose own switch is off keeps `—`. `--json` works on all three,
  returning the same `{paused, until}` shape. See [Pausing everything at
  once](./scheduling.md#pausing-everything-at-once) for the full behaviour.
- **Default scheduled jobs (`schedule catalog`).** uzi ships a small catalog of
  builtin default schedules (docs hygiene, bug triage, a planned-work sweep, and
  so on). `schedule catalog list` shows them as a table (`SLUG`, `TARGET`, `CRON`,
  `ENABLED`, `NAME`) where `ENABLED` counts how many of your repos already run each
  default; `--json` adds the full per-repo enablement state. `schedule catalog
  enable <slug> --repo <id>` enables a default (by its `SLUG`) on a repo; repeat
  `--repo` to enable it on **several repos at once**. Multi-repo enablement is a
  **client-side fan-out** — the CLI issues one idempotent per-repo enable call per
  `--repo`, so a partial retry is safe and a repo already running the default is
  reported as `already enabled` rather than duplicated. Each repo prints its
  created/`already enabled` result with the backing schedule id; `--json` returns
  the per-repo results. For a **sweep** default (one that selects issues by a
  label), the enable first checks each target repo's forge for the selector label
  and prints a `WARNING` (to stderr) naming any that is missing — the schedule is
  still created, but the sweep will not match until the label exists. Pass
  `--create-missing-labels` to create the missing labels on each repo's forge
  first, then enable. The guardrail is **purely advisory and never blocks the
  enable either way — not even on its own forge errors**: a failed label check or
  create prints a `WARNING` and proceeds, so a transient forge outage cannot abort
  an enable (which otherwise reads nothing from the forge, computing the next fire
  from the catalog cron).
- **`schedule list`** prints `ID`, `TARGET`, `SOURCE`, `REPO`, `WHEN`, `NEXT`,
  `ON` and `HARNESS`. `SOURCE` is the catalog slug for a default schedule and
  `custom` for one you authored; `--json` carries the same facts as `origin`
  and `catalog_slug`.
- **`schedule reset <id>`** restores a **default** schedule's edited fields —
  cron, timezone, model, auto-approve, wait-on-limit, max issues and output
  mode — to the builtin catalog values, sets `apply-model-to-agents` back to
  `false`, and clears MR rework, guidance, the harness pin and the credential
  override back to inherit. It clears the customized flag and re-activates the
  schedule on its catalog cadence (a parked schedule fires again; a paused one
  stays paused). The catalog-owned prompt and labels are untouched. Only a
  default-origin schedule can be reset; a user-origin one is a `409`.
- **`schedule clone <id> [--repo <id>]`** copies a schedule into a new, fully
  editable schedule you own. Cloning a **default** schedule lifts its catalog
  prompt lock — the baked prompt (or a sweep's labels and guidance) is copied into
  the new row, which becomes an ordinary user schedule you can edit. Pass `--repo`
  to clone into a **different** repo you own (the replication path); omit it to
  clone into the source schedule's own repo.
- **`schedule add-repo <id> --repo <id>`** replicates an existing schedule you own
  onto **another** repo you own as a new **grouped sibling** — the new row is an
  independent, fully-editable copy of the source's current config, and both the source
  and the new row carry one shared **display-only** group id (the web Schedules tab
  renders siblings as independent rows —
  the CLI twin of the web "Add to another repo" action). `--repo` is
  required (the target repo id from `uzi repo list`). Only a **user** schedule can be
  added onto; a foreign source or target repo is a `404`. An **issue-target schedule
  cannot be added onto** (the issue number is repo-relative); that is a `422`, so use a
  fresh create against the other repo's own issue instead. If the schedule already has a
  sibling on that repo this is a clean **no-op** (exit 0). `--json` dumps the new
  sibling object.
- **`review show <id>`** (formerly `run review <id>`, still around as a
  hidden, deprecated alias) prints the judge's verdict, summary,
  recommendations, and triage tally for a run — see
  [Run judge](./judge.md#reading-a-review-from-the-cli) for the full `--json`
  contract. The rest of the `review` group (`backlog`/`resolve`/`dismiss`/
  `undo`/`stats`, plus `file` to turn a recommendation into a forge issue)
  triages recommendations, per run or across all of them — see
  [Reviewing and triaging from the CLI](#reviewing-and-triaging-from-the-cli)
  below. There's still no `rejudge` verb: re-running the judge spends the
  owner's Anthropic budget and stays a web action.
- **`run inputs <id>`** lists the steer queue — a table of `kind` / `body` /
  `state` / `age`, newest first — same states as the web pane, see [Run
  activity pane](./run-activity.md#steer-queue). `--json` prints the raw DTO
  instead. The `KIND` column distinguishes a follow-up from an operator
  **scope directive** (PRD #634, `uzi run stop`/`run scope` on a
  milestone-structured issue run): a follow-up's `STATE` is a receipt state
  (`queued`, `received`, `routed`, `included in a prompt`, or `not delivered` /
  `not confirmed (run finished)` once the run ends); a scope directive's `STATE` is its
  **disposition** instead — `active (scope ceiling set)` while pending,
  `applied (finalized at the ceiling)`, `declined (not acted on)`, or
  `superseded (a later directive replaced it)` — because a scope row is
  never consumed; its state *is* the disposition. **Chat caveat**: a chat
  run seeds every turn as a follow-up row, so `run inputs` against a chat
  run lists the whole conversation, not just steering messages; an issue or
  CI-fix run's queue starts empty and only ever holds what you actually sent
  mid-run.
- **`run fetches <id>`** prints a research run's source log
  ([PRD #1906](../prds/1906-official-sources-web-research.md)): one row per web
  fetch the run attempted, oldest first, with `STARTED`, `VERDICT` (`allowed` or
  `refused`), `REASON`, `HTTP` status, `BYTES`, `CONTENT TYPE`, `URL` and
  `FINAL URL` (after redirects). The command follows every page of the log; a read taken while the run is still fetching can miss a row that commits late.
  Owner-only: another user's run reads as not found (exit 4), and a run that
  never fetched, which is every run not bound to a site list, shows
  no rows. URLs are cut in the table; `--json` prints them whole with each file's
  sha256. The text cells are site- or agent-controlled and are printed with
  control characters stripped. See [Isolated research
  lane](isolated-research-lane.md#reading-the-source-log).
- **`run expedite <id>`** bumps a **queued** run to the front of the claim
  queue, so a worker picks it up ahead of the rest. It only matters before a
  run is claimed — ordering is fixed once a worker takes it — so a non-queued
  run is a conflict (exit 5) and a foreign/unknown run is not found (exit 4).
  `--clear` undoes the bump: it removes the manual override and returns the run
  to its kind default priority (it does **not** demote it below normal). It
  prints the updated run; `--json` emits the run object, whose `priority` reads
  `expedited` after a bump.
- **`run rework <id>`** starts ONE on-demand MR-rework cycle on a **completed**
  run whose MR is open, past the automatic cap — the way to rework an MR after the
  automatic watcher has stopped. It skips the cap, the quiet-period debounce, the
  head-SHA staleness check and the green-pipeline gate, but keeps the branch guard,
  the one-active-rework guard, the admin kill-switch, the owner token and the open-MR
  requirement; the cycle does **not** count against the automatic cap. A
  foreign/unknown run is not found (exit 4) and a refusal (disabled, not reworkable,
  already running, or nothing new to rework) is a conflict (exit 5). `-m`/`--message`
  carries optional guidance (or pipe it on stdin); an empty guidance is a valid
  trigger as long as there is a new review comment. It prints the created `mr_rework`
  run; `--json` emits the `{"run": ...}` envelope, like `run create`.
- **A message's content is under `payload`, not `body` or `content`.** Each
  `--json` line carries the text under `payload` (raw per-kind JSON); there is no
  `body`/`content` field, so reading either returns empty — indistinguishable from
  a message that genuinely has no content. Read `payload`.
- **`run logs <id>` names the invocation, not just the role.** The actor
  column reads `role[/<short id>][ · <task label>]`, so two `coder`
  subagents running in parallel are distinguishable instead of being two
  identical `coder` rows:

  ```
  #12   tool_use         coder/3v6ptu · API wiring          {"name":"Edit"}
  #13   tool_use         coder/2k9xqf · web gate UX         {"name":"Write"}
  #14   text             lead                               {"text":"delegating"}
  ```

  The short id is the **last** 6 characters of the invocation id, not the
  first — these ids share a constant prefix, so a leading slice would render
  every instance identically. A message with no invocation (the lead's own
  turns, infra frames, and anything from before this shipped) prints the bare
  role, with no `/id` and no `· label` — note the actor column itself is
  wider than it used to be, so the payload column of *every* line has moved.
  The cell is capped and the label truncated so the payload column stays
  aligned (single-width characters — a CJK label still occupies two terminal
  columns per rune, which this tool does not model).

  `--json` carries the stored invocation id and label in full, with no
  CLI-side truncation — but note the **server caps the label at 80 runes on
  write, and appends no ellipsis**, so a longer label was already shortened
  before the CLI ever saw it, with nothing in the value marking the cut. Both
  fields are always present in `--json`, `null` when absent:

  ```jsonc
  {"seq":12,"kind":"tool_use","agent":"coder",
   "agent_instance":"toolu_01AAAAAAAAAAAAAAAA3v6ptu","agent_label":"API wiring", ...}
  ```

  These two keys are the same per-invocation attribution the web pane draws
  its lanes from — see
  [Run activity pane](./run-activity.md#lanes-one-per-actor-not-one-per-turn).
- **`run logs` pages a large history transparently, and is all-or-nothing.**
  The server gzips the messages response and the CLI fetches it internally in
  bounded pages, reassembling them before printing anything — you never see a
  page boundary and never need to discover a run is large and resume manually
  with `--after` (that flag still works; it just sets the *starting* sequence,
  not a page). If any page fetch fails, a one-shot `run logs` prints
  **nothing** and exits non-zero rather than emitting a partial log (this
  whole-or-nothing guarantee is per fetch; under `--follow`, batches already
  streamed by earlier polls stay printed, but a failed poll still exits the
  session non-zero). Either way, **empty stdout only means "no messages" when
  the exit code is 0**; a non-zero exit means the fetch failed and stdout is
  not a reliable (or complete) transcript. A `--json` consumer should gate on
  the exit code before parsing NDJSON, not infer "empty run" from empty output
  alone.
- **`run logs --tail N` prints only the newest N messages** (in ascending
  sequence order), fetched in one request rather than the full-history walk.
  Combine it with `--follow` to tail-then-follow: the newest N are printed and
  then polling continues from the highest sequence just printed. `--tail` is
  mutually exclusive with `--after` (one selects a window, the other a starting
  point) — combining them is a usage error (exit 2).
- **`admin` needs an admin-scoped token.** A default (`uzc_`) token gets
  exit 3 with an actionable message; mint an `admin_ro` (`uza_`) token in
  Settings → Access to use it. `uzi whoami` over a `uzc_` token reports
  `is_admin: false` even for an admin — that's the credential's own
  authority, not your résumé.
- **`admin rate-limits` also takes `--provider claude|codex`** (default
  `claude`, so an existing invocation is byte-for-byte unchanged):
  `claude` is the existing per-user Anthropic view
  (`EMAIL`/`VAULT`/`TOKEN`/`STATUS`/`5H%`/`7D%`); `codex` is the per-user
  Codex view grouped by user, one row per `(account, bucket)`
  (`EMAIL`/`VAULT`/`ACCOUNT`/`STATUS`/`BUCKET`/`PRIMARY`/`SECONDARY`/`RESET`),
  with a no-reading window shown as `—`. `--json` returns the raw
  `[]CodexAdminRateLimitRowDTO` for codex, unchanged for claude.
- **`admin cli-tokens` is the factory-wide standing-credential inventory** —
  every CLI token, whoever owns it, with `OWNER`, `PREFIX`, `NAME`, `SCOPE`,
  `STATE`, `USED` (last-use, capped to once a minute) and `EXPIRES` (blank
  means never — the webui-minted `uzc_` the agent/CI path depends on). It
  never prints a token value or its hash: the value isn't stored anywhere
  after mint, and the hash is excluded by the query's own column projection,
  so it's absent from the Go type this reads, not merely dropped at render
  time. It **does** carry the same `last_used_ip` a user sees for their own
  tokens — factory-wide, so an `admin_ro` holder can see every user's source
  IPs. That's the deliberate point of `admin_ro` being a factory-wide read,
  not an oversight, but worth knowing before you mint or hand out one of
  these tokens. Read-only: there's no admin revoke here, the same write/read
  split as every other `admin` verb.
- **`admin products` lists the external products registered for product
  tokens** (PRD #1907), soft-deleted ones included, with `NAME`, `STATE`
  (`enabled`, `disabled` or `deleted`), `ACTIVE_TOKENS` (manual product
  tokens neither revoked nor expired; the access tokens of OAuth connections
  are not counted), `CONNECTIONS` (live OAuth connections) and `DESCRIPTION`.
  A disabled or deleted product's tokens and connections are refused on `/api/v1`, and a deleted product can never be
  re-enabled. Read-only: registering, editing, deleting a product and
  revoking one of its tokens are browser-only admin actions.
  `admin products skills <product>` (a product's name or id) shows one
  product's skill set read-only (PRD #1909): the source config (repo URL, ref,
  whether a clone token is set, never its value, and whether the instance has
  product skills enabled), the applied set (commit sha, when, skill names) and
  any staged snapshot waiting for approval with its diff and the skills the
  sync dropped and why. It works with a `uza_` token, like the rest of
  `uzi admin`; setting the source, syncing and approving stay browser-only. See
  [Product skills](./skills.md#product-skills).
  `admin products egress-profiles <product>` lists, read-only, the [site
  lists](egress-profiles.md) an admin has allowed that product's tokens to name
  on job create (PRD #1976): `NAME`, `ALLOWED` (when) and `DESCRIPTION`. It
  works with a `uza_` token; allowing and removing a list are browser-only
  admin actions on **Admin → Products**. See [Product
  tokens](product-tokens.md#site-lists-for-jobs).
  `admin products connections <product>` lists, read-only, the users who have
  connected that product through OAuth (PRD #1910): `USER`, `SCOPES`,
  `CONNECTED`, `LAST USED` and the connection `ID`. A connection is listed while
  it is live, whatever the state of its access tokens. `--json` prints
  `{"connections": [...], "truncated": bool}`; `truncated` is true when the
  server cut the list at its 1000-row cap. It works with a `uza_` token;
  revoking a connection is a browser-only admin action on **Admin → Products**.
  See [Connect a product](connect-a-product.md).
- **`admin guardrail-impact` is a live pre-flight count** (PRD #66) — how many
  enabled repos, factory-wide, the push/merge guardrail would refuse right now
  (the bot can push or merge to the default branch). It **persists nothing**: it
  re-checks the forge on each call rather than reading the stored privilege
  report, so it reflects the forge as it is now, not as of the last sweep. The
  table has `PATH`, `BLOCKED`, `UNEVALUABLE`, and a summary line
  `enabled=… blocked=… unevaluable=…`. `UNEVALUABLE` is counted apart from
  `BLOCKED` and is **not** safe: a forge error or a repo with no default branch
  means uzi could not tell — read it as unknown, never as zero affected.
- **`admin blocked-repos` is the cross-user allow/deny list** (PRD #66 D8) — every
  user's repos the guardrail refuses right now, plus any an admin has explicitly
  allowed. Unlike `guardrail-impact` it reads the **stored** privilege report
  (cheap, no forge call), so a repo whose connection was never checked
  (`UZI_PRIVILEGE_CHECK_INTERVAL=0`) is **invisible** here: a `note:` line then
  warns and the JSON `checks_unknown` is true, so an empty list means "unknown",
  not "none blocked". The table has `OWNER`, `PATH`, `BLOCKED`, `ALLOWED BY`
  (the admin who allowed it, or `—`). Allowing/revoking is done from the web UI.
- **`admin health` is the instance health document** (PRD #1484) — a closed registry
  of checks over what uzi knows about itself (worker rolls, queue and capacity, the
  controller report, background loops, the database, integrations, housekeeping), with an
  overall verdict and a per-severity tally. By default it prints only the checks needing
  attention (everything that is not `ok` and not `na`) as
  `SEVERITY CHECK SINCE SUMMARY`, then overall status,
  `blocking: true/false (instance-wide)` and the tally. The server's blocking flag
  means an instance-scoped check is danger; owner-only danger remains visible and still exits 8.
  `--all` lists every check. `--json` emits the endpoint's document unchanged.
  See [admin health](admin-health.md#severity) for scope and banner/episode routing.
  **Its exit code is a probe
  contract:** 0 unless the overall status is `danger`, then **8**; `--strict` also exits
  8 on `warn` or `unknown`. Exit 8 is a **success-path** exit — the HTTP call returned
  200 carrying an unhealthy verdict — so the full report prints first, and a transport or
  auth failure keeps its own code (3 for a 401, 6 for a 5xx or unreachable server). That
  lets a cron probe (`uzi admin health --strict` with a `uza_` token) tell "unhealthy"
  (8) from "could not ask" (3/6). `admin workers` gained the matching roll health per
  worker (`VERSION`, `UPGRADE`, `BLOCKING`), cross-user, so an admin sees which owner's
  fleet is stuck rolling and why. Read-only, `uza_`-token, same ceiling as every other
  `admin` verb — the banner snooze stays cookie-only in the web UI.
- **`admin agent-source get | status`** (PRD #602, update fields PRD #702
  M4) reads the [agent source](agent-source.md) config and sync status:
  `get` shows the repo URL, ref, folder (the repo-relative subfolder role
  files are read from, default `.claude/agents`), enabled flag, interval,
  and whether a credential is set (never its value); `status` shows the
  last sync/apply time and commit, the staged snapshot's counts, whether a
  snapshot is pending review, and the derived update signal —
  `update_available`, plus `latest_ref` and `update_checked_at` once a
  check has run. Those three are computed server-side from the last **Check
  for updates** result and the live config; the CLI itself does no egress
  to produce them. Read-only, same as every other `admin` verb here —
  setting up the source, and triggering **Sync now**, **Check for
  updates**, **Bump pin**, and **Approve & apply** stay web-only.
- **`admin review backlog` and `admin review stats` are the admin "All users"
  judge aggregate** (PRD #1184) — every user's judge recommendations deduped by
  `(category, target)` across the whole factory, with **attribution hidden**.
  `backlog` prints one line per group as `K users · M runs · N open` (how many
  distinct users hit the pattern, how many runs it recurs in, how many are still
  open) plus the rationale preview, and **no per-run or occurrence line**: no
  owner, run id or run title is shown, so a group tells you how widespread a
  recommendation is without saying whose it is. It takes `--bucket` and
  `--category` — forwarded verbatim and server-validated exactly like
  [`uzi review backlog`](#reviewing-and-triaging-from-the-cli), so an unknown
  value is a usage error (exit 2), not a silent empty list — but has **no
  `--run`** anchor, because an anchor names a run. `stats` is the all-users
  triage tally, the cross-user twin of `uzi review stats`. Both are read-only and
  need an `admin_ro` (`uza_`) token, same ceiling as every other `admin` verb;
  the cross-user Mark done and Undo stay cookie-only in the web UI.
- **`admin egress-profile list` and `admin egress-profile show <name>` read the
  [egress profiles](egress-profiles.md)** (PRD #1906), the named site lists for
  official-sources research. `list` prints `NAME`, `HOSTS` (entry count),
  `OVERRIDES` (multi-publisher entries admitted by an explicit override),
  `UPDATED` and `DESCRIPTION`. `show` prints the profile's fields, then one
  `HOST`/`OVERRIDE` row per entry, then a `warning:` line for each overridden
  multi-publisher host, for each stored multi-publisher entry that has no
  override (the built-in list grew; it matches nothing), and for each stored
  entry the current rules no longer accept (it matches nothing). An entry is an exact host or `*.base`, which
  matches proper subdomains of base but not base itself. An unknown name exits
  4; a name that is not a profile name (lowercase letters, digits and hyphens)
  exits 2 without a request. Names, descriptions and hosts go through the same
  sanitizing cell path as every other untrusted field; `show` prints a host or
  description whole, up to its 253- or 500-character maximum. Read-only, `uza_`-token, same ceiling as every other `admin`
  verb: creating, editing and deleting a profile are cookie-only admin writes.
- **`uzi repo remove <id>` deletes a single stale repo** — the surgical
  counterpart to deleting a whole forge connection. It only works on a
  **disabled** repo, so disable it first (`enabled` shows in `uzi repo list`);
  the server refuses an enabled repo, or one with a run still in flight, with a
  conflict. Removing a repo deletes its board and run history (the row and its
  cascade), so it is destructive: it prompts `[y/N]` on stdin unless you pass
  `--force`/`-f`. Note it is **not permanent for a repo the bot can still see** —
  the next projects refresh re-adds it as a disabled row, because the projects
  list reflects live membership. To keep a still-visible repo out for good,
  remove the bot's access on the forge first; `remove` is meant for a repo the
  bot no longer sees (a deleted/recreated project, the stale-duplicate case).
- **`uzi project-sync status <repo>` and `uzi project-sync resync <repo>`** are the
  CLI's read-and-fix-loop window onto a repo's GitHub Projects v2 sync. `<repo>` is a
  path-with-namespace (`org/repo`, matched against `uzi repo list`) or a raw repo id.
  `status` prints whether the repo is linked and, if so, the project number, whether the
  board is uzi-owned, the last sync time, the last error (health), the synced item count,
  any board columns with no matching Status option, and whether the synced field has no
  `Done` option to project a closed issue onto (see [Closed issues and the Done
  status](./github-project-sync.md#closed-issues-and-the-done-status)); a repo that is
  **not linked** is reported as normal output (`--json` returns `{"linked": false}`), not
  an error.
  `resync` re-seeds an already-linked board, picking up newly-added Status columns — the
  same operation the web panel's Resync button drives. Linking a repo to a project in the
  first place (**Adopt**) and creating a fresh uzi-owned board (**Provision**) stay
  **web-only** (D4): the CLI observes and re-seeds, it never mints a link or a project.
- **`uzi logout` is local-only.** It removes **the active context's** stored
  credential (its stored URL is left intact, so a later re-login needs no
  re-typed URL); it does **not** revoke it server-side (see
  [Managing tokens](#managing-tokens) below).

## uzi job: repo-less jobs

`uzi job` creates and inspects [jobs](./jobs.md), runs with no repository, over the stable `/api/v1/jobs` API with your CLI token (`uzc_`; a `uzp_` product token is refused). The only job type is `research`.

```
uzi job create --type research --prompt-file prompt.md --input notes.md=@notes.md
uzi job get <job-id>
uzi job result <job-id>
uzi job cancel <job-id>
uzi job list [--limit N] [--cursor C]
uzi job files <job-id>
uzi job file get <file-id> [-o path]
```

- **`create`** needs `--type` and exactly one of `--prompt <text>` or `--prompt-file <path>` (`-` reads stdin). `--input name=@file` attaches a named text input and repeats (at most 20, 1 MiB in total, UTF-8 regular files only). `--file <path>` attaches a file (PDF, PNG, JPEG, DOCX, XLSX or UTF-8 text; repeatable): each one is uploaded first, then attached to the job by id, so a rejected file stops the create before any job exists. `--title` overrides the title derived from the prompt; `--budget-seconds` sets the wall-clock limit (the server caps it at 8 hours). It prints the queued job.
- **`get`** shows the job's status; `REQUESTED_BY` is marked as reported by the product, not a verified identity.
- **`result`** prints the job status, then findings, then the report indented under its label. A job that has not reported prints `no result yet`; a finished one that never reported prints `no result`.
- **`cancel`** cancels a queued, running or waiting job; a running job may still read `running` for a moment. A finished job exits with a conflict error.
- **`list`** is newest first, 50 per page by default, and prints the `--cursor` for the next page.
- **`--json`** prints the raw API objects. Text from the server is sanitized for the terminal.
- **`files`** lists a job's input and output files (id, name, direction, size, short sha256, state, expiry and, for an output whose hash matches an allowed fetch in that job, its source URL), then any outputs that were refused and why. See [Job files](./jobs.md#job-files).
- **`file get`** downloads one file by its id (from `files`) to disk. It never prints the bytes and **never overwrites**: with no `-o` it writes to the file's content-derived storage name (`<sha256>.<ext>`) in the current directory, and it refuses if that path, or the `-o` path, already exists. The file appears at the target name only once the download is complete and verified, so a failed or interrupted download leaves no partial file there (at most a hidden `.uzi-download-*.tmp` file next to it). Placing the file uses a hard link, so the target directory must be on a filesystem that supports them (not FAT or exFAT). `-o` always names a path: `-o -` is refused rather than treated as standard output (use `-o ./-` for a file named `-`). An expired file is reported as expired (the server answers 410).

`uzi job` needs a `uzc_` user token: a `uzp_` product token is refused by the CLI and a `uza_` admin token is refused by `/api/v1`. Refusals print the server's error message (not the machine `reason` token): for example a 422 about no model credential (add an Anthropic credential in uzi) or a 429 when you reach the active-job cap (10 per user by default). A `job_type_not_allowed` 403 applies only to product tokens, so `uzi job` does not hit it.

## Recovering unpublished work: `uzi run export`

When a run commits useful work but then fails to publish its head (a workflow or
secret preflight, a rejected push, exhausted base alignment), uzi captures the run's
original committed history into a durable, owner-only recovery archive — a real Git
bundle that imports into a fresh clone even after the worker and its disk are gone.
`uzi run export` downloads that archive to a local file:

```
uzi run export <run-id> --output ./recovered.bundle
```

- **Owner-only.** You can export only your own runs; an admin viewing someone else's
  run is refused. The download reaches the API directly — no contact with the worker,
  which may no longer exist.
- **Explicit capture selection.** A run can retain more than one capture. When more
  than one is *available*, export refuses to guess: it prints a capture table (id, state,
  source, size, created) to stderr — even under `--quiet` — exits 2, and you re-run with
  `--capture <id>`. It never silently picks a different attempt. When exactly one is
  available it is used automatically. When captures exist but none is `available` (still
  preparing/uploading, `needs_action`, `expired` or discarded), the same table is printed
  and it exits 5; with no captures at all it exits 4. With `--capture <id>` naming a
  capture that isn't `available`, export refuses with its honest state rather than
  downloading.
- **Verified, atomic, no-clobber writes.** The download is streamed into a private
  `0600` temp file, its byte count and checksum are verified against the server manifest,
  and only then is the destination created — by an atomic link that **refuses to overwrite
  an existing file or symlink** at `--output`. The final file only ever appears
  fully-formed and verified.
- **Nonzero exit, no partial file, on failure.** An interrupted, corrupt or expired
  download exits nonzero and leaves **no file** at the destination, so a truncated
  bundle is never mistaken for a complete one.
- **Review before you publish.** The archive is the run's *original* committed history
  and may contain secrets. Review it before publishing anywhere; a real credential must
  be revoked/rotated and removed from the affected history. No raw bytes are ever printed
  to stdout — on success, `--json` prints the metadata result only (`capture_id`, `output`,
  `byte_size`, `checksum`, `source_sha`, `verified`). On the capture-choice refusal above,
  `--json` instead prints the listed captures (archive DTOs, each with a `hold_id`) as a
  JSON array on stdout, still exiting 2 or 5.

`uzi run get` also shows a metadata-only recovery summary for a terminal run that has
captures (the archive count, per-state tally, and each available capture's id) so you
know what `--capture` can fetch. It is metadata only and never claims an archive is
available when it is not.

### Reviewing and resolving held work: `uzi run recovery` / `uzi run discard`

A run's custody hold reserves owner capacity while its committed-but-unpublished work is
recovered. Holds are per claim generation and capped per owner, so unresolved holds can
eventually block new code runs. To find what held work you have, start here before choosing
a run to export or an exact hold to discard:

```
uzi run recovery [--json]
```

- The human view shows only open holds across your runs, oldest first by `created_at`.
  Its columns are `RUN ID`, `HOLD ID`, `GEN`, `DISPOSITION`, `ARCHIVE` (whether an
  available capture exists), `WORKER`, and `AGE`. Run and hold ids are shown in full.
  Below the table it prints the owner-wide `open_holds`, `custody_hold_limit`,
  `decision_needed`, and `blocked_runs` aggregate. For each `source_only` hold it prints
  `run <run-id> hold <hold-id>: no recovery archive; custody of worker <name>'s local source
  is retained (export unavailable; it may be the only copy)`. Then it prints hints: a
  `uzi run export` hint only when an open hold has an available archive (an `archive_ready`
  hold), and a `uzi run discard` hint when a hold awaits a decision (`source_only` or
  `needs_action`, which have no archive to export). With no open holds it says so and
  still prints the aggregate.
- Without a run id, `--json` returns the endpoint's `aggregate` and `holds` object,
  including settled holds. These hold rows have no `captures` array. Use the run id
  from this list for the detailed view and capture ids:

```
uzi run recovery <run-id> [--json]
```

- The per-run view shows each hold's exact id, claim generation, and its attention state — active
  protection, a capture in flight, or a ready archive. Legacy holds can release
  automatically on archive readiness; guarded holds await final inventory acknowledgment.
  The status also distinguishes a capture-less source that needs a decision and shows
  the latest capture state. A `source_only`
  hold prints `hold <hold-id>: no recovery archive; custody of worker <name>'s local source is
  retained (export unavailable; it may be the only copy)`, and the same export and discard
  hints as above follow. `--json` prints
  the raw rows for scripting, each hold with a `captures` array (id, state, source_sha,
  byte_size, created_at) whose ids `uzi run export --capture` takes; it's always `[]`
  rather than null, including when the run itself was deleted (a released hold outlives
  its run) or the server predates capture-hold linking. Owner-only.
- While the server retains the run's last published checkpoint on origin (PRD #1810), the
  human-readable output adds a line per hold naming where it lives: `hold <id> checkpoint:
  <ref> @ <12-char tip> (<state>)`, where `<ref>` is `refs/uzi-checkpoints/<branch>` or, once
  a newer run on the same branch superseded it, `refs/uzi-recovery/<run-id>`. `--json` carries
  the same information as `checkpoint_ref`, `checkpoint_tip`, and `checkpoint_state` on each
  hold. A hold with no retained checkpoint ref omits the line (and the `--json` fields are
  absent).

- A hold with `terminal_record_rejection: "mac_failure"` also shows the fixed diagnostic:
  “terminal record rejected after restart (MAC failure); completion is unverified; see run recovery for source custody”.
  JSON preserves that field and adds the fixed text as `terminal_rejection`, alongside the
  exact hold and generation. The diagnostic supplies no completion or replay authority and
  does not change attention or export availability. Export still requires an independently
  verified available capture. See [terminal record authentication failures](run-recovery.md#when-a-terminal-record-fails-authentication-after-restart)
  for negotiated reporting, retained source, and positive custody cleanup after settlement.

When a capture-less hold is genuinely not worth keeping, discard that one exact held
source:

```
uzi run discard <run-id> --hold <hold-id> --yes
```

- **Names one exact hold.** `--hold` is required; the discard settles only that hold and
  its non-ready captures. It never touches an *available* archive (delete that separately
  from the run view) and never a sibling hold or another generation.
- **Confirmation required, and it may be the only copy.** Without `--yes` it prompts
  interactively and refuses outright when stdin is not a TTY (so a script cannot discard
  unprompted); a declined prompt makes no change. Discarding a capture-less hold can
  destroy the last copy of that work — export anything you might need first.
- **Discarding a run's last open hold also deletes its retained checkpoint ref on the
  forge.** Once no hold of the run is left open, the server settles (CAS-deletes) whatever
  ref still carries the run's tip (`refs/uzi-checkpoints/<branch>`, or `refs/uzi-recovery/<run-id>`
  once a newer run superseded it) — see the `run recovery` checkpoint line above. Both the
  `run discard` help and its confirmation prompt name this; fetch the ref first if you
  might still need it.
- **Terminal and owner-scoped.** A discarded hold cannot be revived, and you can discard
  only your own runs' holds.

### A failed run's salvage copy: `refs/uzi-salvage/<run-id>`

Salvage is **off by default**. An operator turns it on per forge with
`UZI_SALVAGE_FORGES` (a comma list of `github`, `gitlab`, `forgejo`). When it is on,
uzi copies a failed run's last **published** checkpoint to a run-scoped ref,
`refs/uzi-salvage/<run-id>`, and removes that copy after `UZI_RECOVERY_READY_RETENTION`
(if the removal keeps failing, uzi stops trying and `SALVAGE_ERROR` names the ref that may
remain).
The copy is only what the run had checkpointed to the forge, so it may be behind the
run's final local work. Salvage never moves or deletes the branch's own checkpoint ref.

`uzi run get` on a failed run with a salvage record prints a `SALVAGE` block: the state
with a one-line explanation, then `SALVAGE_REF`, `SALVAGE_TIP` (short), `SALVAGE_EXPIRES`
and `SALVAGE_ERROR` when set. A promoted copy also prints the fetch command:

```
SALVAGE          promoted: checkpointed commits saved (last published checkpoint; may be behind the run's final local work)
SALVAGE_REF      refs/uzi-salvage/<run-id>
SALVAGE_TIP      89abcdef0123
SALVAGE_EXPIRES  2026-09-29T12:00:00Z
SALVAGE_FETCH    git fetch origin refs/uzi-salvage/<run-id>
```

The other states read `pending` (the copy is being made), `unavailable` (not saved: the
published checkpoint was no longer at its recorded tip on the forge, either gone or moved),
`refused` (the salvage ref already pointed at a different commit), `failed` (salvage stopped after
repeated attempts; the last error names any ref that may remain), `skipped_secret` (not saved: the run failed on a secret-scan block),
`expired` (the copy was removed, unless removal kept failing; the last error names any ref that may remain) and `disabled` (not saved: salvage was turned off for
that forge before a copy was made). `SALVAGE_REF` and `SALVAGE_FETCH` print only for a
ref of the exact form `refs/uzi-salvage/<run-id>` naming this run. The same values are the run's `salvage_state`,
`salvage_ref`, `salvage_tip`, `salvage_expires_at` and `salvage_last_error` fields
(`--json`, or `uzi run get <run-id> --field salvage_state`); all are null without a
salvage record.

While a failed run has a live salvage copy (made, or still being made), removing its repo
or forge connection is refused with a 409. The body names **at most 5** live rows total,
split between `salvage_refs` (each created `refs/uzi-salvage/<run-id>`) and
`salvage_pending_runs` (each run whose copy is still `pending`, by run id; a pending copy
may already have its ref); a run beyond that first 5 is only counted, in `salvage_count`
(the true total) and in the error text's trailing "and N more run(s)". The error text
otherwise names the same refs and runs as the body. The block lifts when the
copies expire, or when a pending copy settles (into `promoted`, which then holds until it
expires, or into a state that saved nothing); retry the removal after that.

## uzi handoff: ephemeral branch-scoped task runs

```sh
uzi handoff -m "<context>" [--file <path>] [--base <ref>] [--mr]
            [--review] [--then-fix] [--repo <repo-id>]
uzi handoff rm <run-id>
uzi handoff review <run-id>
```

`uzi handoff` (alias `uzi task`) hands a throwaway task to a worker without a
forge issue and without a PRD: no plan gate, no issue to file, no MR to
review, unless you ask for one. Think of it as renting a remote worktree —
you push it some work, watch it, pull the result, and throw the branch away.
See [Handoff: renting a remote worktree](./handoff.md) for the full mental
model; this section is the flag reference.

Run from inside a checkout with an `origin` remote:

```sh
uzi handoff -m "add input validation to the signup form"
```

This does three things, in order, and stops before the third if either of the
first two fails:

1. **Create** — a new `task` run, on the repo matched from your `origin`
   remote (`--repo <id>` overrides the auto-detection, see `uzi repo list`).
   The server names the branch: `uzi/task/<run-id>`.
2. **Push** — your local HEAD (or `--base <ref>`, if given) to that branch,
   with **your own** git credentials — the same push you'd type by hand.
   `--base` seeds the branch from a named ref instead of local HEAD.
3. **Dispatch** — only now can a worker claim the run. If the push in step 2
   fails, the run is left created but never dispatched — it has no seed
   content, so nothing will claim it — and the error tells you to cancel it
   with `uzi run cancel <id>`. If the dispatch itself fails, the outcome is
   ambiguous (the run may or may not have become claimable), so the error
   tells you to check with `uzi run get <id>` and either let the server
   expire it or cancel it once you've confirmed it's still undispatched. The
   server now automatically expires an undispatched task run past its setup
   deadline, so a stranded run no longer lingers forever.

The worker clones `uzi/task/<id>`, works your inline context (from `-m`, or
`-f <file>`/`-f -` for stdin, or piped bare stdin), commits, and pushes back
to the same branch. There's no forge issue and no MR by default — pull the
result yourself:

```sh
git fetch origin uzi/task/<run-id> && git switch uzi/task/<run-id>
```

Continuation is the same `uzi run follow-up <id>` you'd use on any other run;
watch it with `uzi run get`/`uzi run logs --follow`, or drop into
[`uzi tui`](#watching-runs-live-uzi-tui).

A few things worth knowing before you rely on this:

- **The push is non-forced, deliberately.** After your seed push, the worker
  is the sole writer to the branch. If you push more local commits to a
  *live* task branch mid-run, they're rejected non-fast-forward rather than
  clobbering the worker's history — a mid-run user push is out of scope for
  v1; use `uzi run follow-up <id>` to send the worker more context instead.
- **The seeded branch is already published, so ask for a merge, not a
  rebase.** Your seed push publishes `uzi/task/<id>` before the worker ever
  starts, so a task prompt should say "merge main into this branch," never
  "rebase onto main" — uzi can't force-push a rewrite of a branch it already
  published, so a rebase there gets bridged (or, if that's impossible, fails
  the run) at finalize instead of landing as asked.
- **A raw handoff has no forge record.** With no issue and no MR (no `--mr`),
  there's nothing durable on the forge — the run transcript and your inline
  context are still persisted in uzi (`uzi run get`/`uzi run logs`), but if
  you want a forge-visible artifact, pass `--mr` or escalate later by opening
  one yourself from the pulled branch.
- **`--mr`** has the worker open an MR for the branch once it finishes, the
  escalation path for a throwaway task that turns out to be keeper work. An
  MR-opened branch is exempt from `uzi handoff rm` — delete it via the MR
  instead.
- **`--review`** runs a diff-review once the task completes: a fresh review of
  `uzi/task/<id>` against its base, producing structured findings (file,
  symbol, line, severity, summary, rationale). Fetch them with:

  ```sh
  uzi handoff review <run-id>          # human table: [severity] file:line — summary
  uzi handoff review <run-id> --json   # the review DTO
  ```

  The findings are never committed to the branch — they're metadata you read,
  not a diff the worker writes. If the task hasn't finished yet, or wasn't
  launched with `--review`, this prints a hint instead of an error.
- **`--then-fix`** implies `--review`: once the review's findings land, a
  follow-on fix run auto-applies fixes for them and pushes to the same
  `uzi/task/<id>` branch. Use it when you want the whole loop — task, review,
  fix — without a manual step in between.
- **`--interactive`** keeps the task alive after `signal_done` instead of
  finalizing it: the run parks in `awaiting_followup` (session, clone and
  branch held open) until `uzi run follow-up <id>` wakes it for another turn
  or `uzi run stop <id>` winds it down. A forgotten park still finalizes on
  its own after `WORKER_TASK_IDLE_TIMEOUT` (30 minutes by default).
  `--review`/`--mr` compose at wind-down rather than at each park, and
  `--interactive --then-fix` is a usage error (exit 2) — see [Interactive
  mode](./handoff.md#interactive-mode) for the full loop.

Cleaning up:

```sh
uzi handoff rm <run-id>
```

Deletes the remote `uzi/task/<id>` branch with your own git credentials
(`git push origin --delete`). It only ever deletes inside the `uzi/task/*`
namespace, and refuses a run that opened an MR (delete it via the MR
instead) or one that isn't a `task` run at all. There's no server-side
auto-prune of stale task branches yet — `rm` is the v1 cleanup story; run it
once you've pulled what you need.

## Forge views from the CLI: `uzi pr` and `uzi ci`

`uzi` reads a repo's open PRs/MRs and its CI runs straight through the API,
which already holds the forge PAT — so you never install or authenticate `gh`,
and you never leave the terminal to check whether a PR's checks went green.

```sh
uzi pr list                       # open PRs/MRs on your enabled repo
uzi pr checks 1254                # one PR's checks, reviews, merge state
uzi pr checks 1254 --watch        # re-poll until no check is pending, then exit 0
uzi ci list --limit 20            # recent CI runs, newest first
uzi ci jobs 34677104577           # one run's jobs and steps
uzi ci fix agent/issue-1246       # queue a CI-fix run for a failed ref
```

Every command takes `--repo <id>`. Omit it and `uzi` defaults to your single
**enabled** repo; if several are enabled it exits `2` and names the choices, so
you always know which repo you're looking at. Get repo ids from `uzi repo list`.

- **`uzi pr list`** shows each open PR's iid, review decision, conflicts,
  branch, title, and the `↳ run` id when a uzi run opened it. It deliberately
  carries no per-PR check counts — checks are a drill-in, so use `pr checks`
  for those.
- **`uzi pr checks <iid>`** shows one PR's checks (name, state, description,
  elapsed), a reviews summary, and the merge blocked-reason. `--watch` re-fetches
  and re-prints on a fixed cadence and **exits 0 the moment no check is still
  pending** — the scriptable "wait for CI to settle" primitive. A transient
  rate-limit (`429`) or server blip during a watch prints one line to stderr,
  backs off (honouring the server's `Retry-After`), and keeps watching. Caveat:
  a PR with **no checks reported** counts as settled, so `--watch` exits 0
  immediately on a just-opened PR whose CI has not registered its first check
  yet; wait for that first check to appear before a script trusts the result.
- **`uzi ci list`** shows the repo's recent workflow/pipeline runs
  (`<name> #<number>`, event, branch, status, elapsed, title), newest first;
  `--limit <n>` bounds the page (server default 30, capped at 100). On a forge
  version that has no CI-runs endpoint it prints a one-line notice and exits 0.
- **`uzi ci jobs <run-id>`** shows one run's jobs, with GitHub Actions steps
  indented beneath each job.
- **`uzi ci fix <ref>`** queues a `ci_fix` run for a ref whose latest cached
  pipeline is failed. The server re-checks that precondition, so a ref that is
  not failed (or has no cached pipeline) is refused with a `409` (exit 5) and a
  reason; on success it prints the created run id.

All five support `--json` (a top-level array for the lists, one object for a
detail), for agents. The human table's PR/MR noun follows the repo's forge
(GitLab says MR).

## Watching runs live: `uzi tui`

```sh
uzi tui            # board — a live view of your own runs
uzi tui <run-id>   # jump straight into one run's lanes
```

A full-screen, keyboard-driven view of the factory: a board that updates
itself, a drill-in showing what each subagent is doing right now, and
in-place steering — all without leaving the keyboard. It needs an
interactive terminal; run it against a pipe or in CI and it exits with a
usage error pointing at `run list --json` and `run logs --follow` instead of
drawing escape codes into your log.

**This doesn't replace `run logs --follow`.** The TUI is for a human at a
keyboard; `--follow` (with `--json` for NDJSON, `--after <seq>` to resume,
and stop-on-terminal-status) stays the scriptable, single-run surface and is
also the TUI's own fallback when the live channel is unreachable (below).

### Startup: an available update

At startup, `uzi tui` checks for a newer release in the running CLI's channel
and, if one exists, shows a modal on top of the board. A stable
`uzi-cli` install checks stable releases; an `uzi-cli-rc` install checks the newer
of the latest stable and latest release candidate, while staying on its opt-in
formula. A stable install looks like this:

```
▲ Update available
uzi 0.83.0  →  0.85.0

A newer release is available.

▸ Update now  (brew upgrade uzi-cli)
  Not now
  Don't remind me for 0.85.0
```

- **A Homebrew install** gets the "Update now" action for the formula that owns
  the running binary: `brew upgrade uzi-cli` for stable or
  `brew upgrade uzi-cli-rc` for the RC channel, even when it currently runs a stable
  version. Choosing it exits the TUI and runs the command in the foreground, so the source-build output and any failure stay
  visible, then tells you to rerun
  `uzi tui`. It never upgrades silently in the background while the TUI keeps
  running. For an eligible **stamped** binary whose formula ownership cannot be
  proven, including a manually built binary, the prompt shows release notes
  without an upgrade action. It uses the binary's stamped `-rc.N` suffix to
  choose the information channel.
- **A security release** renders as the filled amber andon band and is
  worded as a security update; a routine release stays quiet.
- **Gating mirrors** [the CLI-vs-server skew warning](#when-your-cli-is-older-than-the-server):
  shown only for a stamped release build (a `go build`/`dev` binary never
  prompts), and it honours the same off-switches — `UZI_VERSION_CHECK=0` and
  `--quiet`. A stable install never offers a prerelease (`-rc.N`); an RC-channel
  install offers whichever valid stable or RC release is newest, when newer than
  the running binary. An unknown owner running an RC build uses the same selection
  but only gets release notes, never a Homebrew action.
- **"Don't remind me for `<version>`"** is remembered per release version (a
  later release re-prompts anyway); **"Not now"** (or `esc`) just closes the
  modal for this session, with nothing persisted, and it shows at most once
  per `uzi tui` invocation either way.
- **This is a third axis**, distinct from `version`'s own `update … available`
  row and from the CLI-vs-server skew banner above: it compares this CLI
  binary against the latest *published* release, not the CLI against the
  *server's* running version. The CLI makes no network call of its own for
  it — it reads the release facts already riding the same `/api/version`
  response the TUI fetches for the footer skew readout.

### Three views

- **Board** (the default). Your own runs, refreshed on a poll — a live list
  doesn't need a socket per row, so this is the one screen that doesn't use
  the live channel. `[a]` toggles the factory-wide admin board (needs a
  `uza_` token; a `uzc_` token gets refused inline and stays on your own
  runs, never a crash). **The admin board isn't your own-runs list widened —
  it's a different shape**: active runs only (nothing completed), capped at
  500, no judge-verdict or usage columns (cost included), titled
  `▚▚ uzi · active runs` on screen so it never promises a row it can't show.
  A milestone-structured run (one planned from a gated issue run — `uzi run
  create` with no `--plan-file`) shows a compact `▰▱` milestone micro-bar
  between AGE and TITLE (one `▰` per
  reported-complete milestone, `▱` for the rest, or `–/N` text when nothing
  has been reported complete yet); the micro-bar is hidden on a narrow
  terminal, and the full breakdown is on the run detail view. **The
  in-progress milestone's cell blinks** `▰`/`▱` in the tungsten colour (the
  same warm accent as the done fill) on a half-second tick — a static `▱`
  frame renders instead when the terminal isn't interactive (a piped or
  offline render) or with `UZI_TUI_NO_BLINK=1` set (a reduced-motion opt-out,
  read once at startup). The **selected row**
  additionally gains a second line naming the active lane's role, task
  label and age — the run's current activity, the same information [the run
  activity docs](./run-activity.md#milestones-and-the-now-line) describe for
  web and CLI. On your own board only, a right-aligned **COST** cell follows the micro-bar: `$N` in
  whole dollars (no cents, to keep the column width stable), `<$1` for a
  real sub-dollar cost, `—` for a subscription-auth run the SDK prices at
  $0, and a blank cell for a run with no recorded usage; it too is hidden on
  a narrow terminal, dropping right after the micro-bar does so the title is
  never squeezed. `[h]` hides finished runs (completed/failed/cancelled),
  leaving the active and needs-you runs. The rows are grouped into three
  triage bands (NEEDS YOU, then ON THE FLOOR, then DONE) rather than one
  flat table, and each row reads left to right as a `▌` andon strip and
  status glyph, the run id, the status WORD in its own colour (a stalled run
  reads `▲ stalled`, because health folds into the status token rather than
  a separate column), the AGE, the milestone micro-bar, the COST cell, and
  the TITLE; there are no STATUS/HEALTH/MILE column headers or full-width
  rules. The list windows to the terminal height so the wordmark and key
  legend stay on screen however many runs there are, with the visible run
  span (`lo–hi`) and a seven-day cost shown in the top-right summary
  cluster (`⚑ N · ✎ N · ➤ N · ⚿ N · ▲ N · $N 7d · T runs`). On your own
  board, `$N 7d` is the rounded `Last7Days.CostUSD` from the server's
  `GET /api/usage` response, covering the last seven days even when runs
  are outside the visible list window. A `+` after the dollar amount means
  subscription or unreported run costs are excluded from that amount;
  `$0+ 7d` can therefore appear. The cost segment is hidden while the
  initial fetch is pending, after a failed fetch, or when a fully metered
  total rounds to zero. It never appears on the admin/factory board. Each
  count segment is dropped when its count is zero, so a healthy factory
  reads simply `N runs`: `⚑` is the plan gate, `✎` a clarifying question,
  `➤` an interactive task waiting on your next follow-up, `⚿` a Codex run
  needing you to log in again (see [Codex account
  unavailable](run-recovery-wait.md#codex-account-unavailable)), and `▲` a
  stalled/looping/near-timeout run. The two-second board poll refreshes
  rows and their per-run COST cells, along with status, health, milestones,
  age and the judge verdict. The header's `7d` cost fetches separately on
  the 60-second side-fetch, manual refresh and board toggle. **A locked
  vault gets its own line** beside
  the per-token rate-limit strip under the wordmark — the strip is never
  hidden — matching the web SPA's "waiting for vault unlock": a quiet, faint
  `🔒 vault locked` hint (`[locked] vault locked` under NO_COLOR or a
  non-color terminal). When one or more of **your own** runs are actually
  parked on the lock, the hint escalates to a steady amber needs-you band
  instead: `▌ VAULT LOCKED · N runs parked — unlock in the web app to
  resume`. On the admin/factory board the count is scoped to your own
  runs only — it never counts or names another user's locked vault. Either
  form is steady, never blinking, and clears on its own within about a
  minute of the vault unlocking; there's no dismiss. Unlocking itself is done
  off-TUI, in the web app.
- **Run detail** (`[enter]` from the board, or `uzi tui <run-id>` directly).
  A left rail of agent lanes — the lead plus each live subagent, one lane per
  invocation, each with a status dot — beside the selected lane's transcript,
  rendered as markdown. Lanes are built from the same per-invocation
  `agent`/`agent_instance`/`agent_label` attribution `run logs` prints; see
  [Run activity pane](./run-activity.md#lanes-one-per-actor-not-one-per-turn)
  for what a lane's dot means. The header's status tag folds in the run's
  cost beside the elapsed time — e.g. `● running · 41m · $9.55` (with cents,
  since a single run's precision matters); `—` for a subscription-auth run
  the SDK prices at $0, and nothing shown for a run with no recorded usage.
  For a milestone-structured run the rail also shows a
  `MILESTONES {done}/{total}` block below the lanes, one row per approved
  milestone in order, marked `✓` reported complete, `○` not started, or —
  for a milestone in progress — a `◕` that blinks `◕`/`○` in the faint grey
  colour (the same colour as a not-started `○`; the row is told apart by the
  `◕` shape, its motion and a brighter title, never by colour), a static `◕`
  under `UZI_TUI_NO_BLINK=1` or a non-tty render. The rail's eyebrow also
  carries a compact `▰`/`▱` micro-bar that blinks one cell per milestone in
  progress in tungsten, the twin of the board's, and names the in-progress
  milestone(s) after the count (e.g. `· m1, m2`, capped at two then `+N`).
  The count reads "reported complete", not verified: uzi shows
  what the run reported and does not itself check the work. The
  in-progress row also carries a **now line** beneath it — `↳ <role> ·
  <age>` plus its task label — the crew rail's own current-activity read.
  When the lead has attributed agents to one or more in-progress
  milestones, every attributed row instead carries its own `↳ <role>` line
  (its task label beneath), with the live `· <age>` shown only on the lane
  that uniquely matches the run's current activity; an in-progress row the
  lead did not attribute keeps its plain mark with no now line; see
  [Milestones and the now line](./run-activity.md#milestones-and-the-now-line)
  for the full rule. Directly above
  the ACCOUNTS block, a **SPEND** block shows the run's total cost (same
  cents formatting) over a token breakdown — `in` (input plus
  cache-creation tokens), `out` (output tokens), and a `cache` line with the
  cached-read token count and its share of the total input (`in` + `cache`)
  as a percentage; it's
  omitted for a run with no recorded usage. The rail itself doesn't scroll —
  it's clamped to the transcript's height — so when the expanded roster plus
  MILESTONES, SPEND, and the run's own account meters wouldn't all fit, the
  rail folds the crew list by itself down to a count caret (`N ▸`, N being the
  lane count) plus the selected lane, keeping those blocks on screen; a
  roster that fits stays expanded (`▾`). Pressing `c` overrides the automatic
  call for that run — folding an expanded rail or unfolding a folded one —
  and the override sticks until the run is reopened, which returns it to the
  automatic behaviour.
- **Review overlay** (`[v]` from run detail). The judge's verdict, summary,
  and recommendations, with the same resolve/dismiss/undo triage described
  under [Reviewing and triaging from the CLI](#reviewing-and-triaging-from-the-cli).

### Workers and the floor fleet summary

Press `2` for the workers list. Rows put danger first, then warnings,
information, and workers with no attention item; names break ties. Selection
stays on the same worker when a refresh changes the order. `/` filters names
case-insensitively. Blank lines separate the column header and selected-worker
readout; at 120 columns the readout uses one clamped line, with separate items below
120. Press `?` for the disk and state legend.

At 80 columns the list shows name, state, kind, run occupancy, and attention.
At 120 columns, including in split view, it adds CPU, memory (used/total when
a limit is reported),
worst disk reading (label, usage bar and percentage), version, and heartbeat
age. Factory scope adds owner: `you` for your workers, otherwise the email
local part. Versions expand to the longest filtered value, capped at 18 cells
including an upgrade marker (`↑` outdated, `✕` failed). Offline resource
readings are faint and marked `~` as stale.

Floor and workers share a fleet summary on the title line's right edge:
worker count, online count, slots in use, unknown capacities, holds, drains,
cordons, and workers needing attention. It drops optional segments as space
shrinks; when its shortest form cannot fit beside the title, it uses a separate
line. Factory scope labels it `factory workers`. Occupancy is not spare
capacity: holding, draining, and cordoned workers may not take new work.
A `?` cap means unknown advertised slots. Only danger and warning items count
as needing attention; a lease or lone chat is information. Disk percentages
are visual cues; DinD and inode readings are display-only.

Press `enter` or `→` on a worker to open its full-screen detail. Below the tab
strip, `worker ›` introduces its name, state, and kind; faint uptime and
heartbeat share that line when they fit, otherwise appear below. Unknown
uptime and heartbeat values are omitted; offline workers with a heartbeat
show its age.
The lowercase sections are `attention`, `reported runs`, `resources`, and
`configuration`. Attention explains upgrade failures, unpublished-work holds,
outbox queues, and pending outcomes. Reported runs show issue/title and engine
when cached, otherwise a short run ID, followed by worker phase, cached run
stage, and claim generation. `enter` or `→` opens the selected reported run
even when it is not cached; an invisible run shows `run not visible`.

Resources show `?` for missing readings or a null limit; process samples cover
only the worker process. Data, nix, and DinD use usage bars and percentages;
reported inode percentages sit alongside them. Healthy readings use normal
ink, 75–89% uses warning colour, and 90% or more uses alarm colour. Offline
readings remain faint. DinD and inode readings are display-only. The largest
reported HOME appears on one line with its sample age and `≥` for a truncated
measurement (a lower bound). Configuration shows version and upgrade target,
capabilities (`none` when empty), declared template and any reported drift,
effective token mode,
kind, and ephemeral lease. Reported runs do not establish an ephemeral
worker's binding.

In run detail, uppercase `W` opens the run's worker; lowercase `w` keeps its
existing rework action. A run without a worker shows `no worker yet`.
`esc` from a reported run returns to its worker, and `esc` or `←` from the worker
returns to the original list or run. Cross-links retain one original return
target, including its originating pane, rather than a navigation history.
Returning to a run starts a fresh session: it fetches the current run and
newest transcript tail, opens a new stream, then fills older history in the
background. It does not restore loaded transcript state. The reported-run
preflight fetch supplies the initial run DTO without a second initial fetch.
Opening a PR from a run and returning also starts a fresh run session.

At 120 columns floor rows include a faint worker cell; below 120 it is omitted.
A missing name reads `no worker yet` on a non-terminal run and `—` on a
completed, failed, or cancelled run. The floor run summary (cost, count, and
visible row range) sits right-aligned with the account meters, or on its own
line when space is insufficient.

The floor summary, workers list and worker detail share one request chain,
polling every 5s while any is visible, including an unfocused top pane.
Failures back off to at most 60s and keep the last snapshot. Polling pauses
in run, PR and CI run detail and help; returning fetches immediately.
`r` refreshes immediately.
On the floor or workers list, `a` toggles their shared own/factory scope.
Factory scope needs a `uza_` admin token and adds owners and server-reported
disk pressure; a denied request returns both views to your own scope.

### The forge screens: `pulls` and `ci`

The tab strip reads `▚▚ uzi · floor  workers  pulls  ci`, without numeric
key hints. `?` help still lists the `1`–`4` shortcuts.
The two forge screens are both
read straight through the API's stored forge connection PAT: no `gh`, no
personal token, no leaving the terminal to see whether a PR went green.
In either layout, `tab` cycles floor → workers → pulls → ci → floor;
`shift+tab` reverses the order and `1`–`4` jump directly. Both forge lists scope to one repo at a time — `R` cycles
your enabled repos (hidden when only one is enabled), defaulting to the
repo of your newest run — and `/` filters a list by title, branch, author,
or workflow name.

- **`pulls`** lists every open PR/MR on the scoped repo, banded **NEEDS
  YOU** (changes requested, or a conflict), **IN FLIGHT** (a draft, or a
  review still pending), and **READY** (approved, or no review required,
  with no conflict). Each row carries a review cell, the branch, the title,
  and a `↳ <run>` link when a uzi run opened it; per-check counts live in the
  PR view, not the list row (the list read carries no per-check detail). Polls
  every 10s.
- **`ci`** lists the repo's CI/workflow runs, banded **RUNNING**,
  **FAILED**, and **RECENT**, each row carrying a `▰▱ done/total` jobs
  micro-bar while it's running. On a forge version with no Actions/CI-runs
  API it degrades to a one-line notice instead of an empty list. Polls
  every 10s.
- **PR view** (`enter`/`→` on a `pulls` row) drills into one PR: CHECKS
  (failing → pending → passed → skipped, each with its description and
  elapsed time — `↑↓` moves the cursor and shows the selected check's URL),
  REVIEWS (the latest state per reviewer), and MERGE (conflicts, required
  checks, the blocked reason). Re-polls live every 5s, with a `● live · 5s`
  header state and `re-polled Ns ago`.
- **CI run view** (`enter`/`→` on a `ci` row) drills into one run's JOBS,
  expanding the selected job's steps beneath it (GitHub Actions only —
  GitLab and Forgejo jobs carry no steps). Same 5s live re-poll.

A rate-limited forge read draws `~ rate-limited · retry in Ns` in the
header in place of an error and keeps polling once the wait is over — the
same per-connection budget `FORGE_INTERACTIVE_RATE_MAX` documents in
[Configuration](./configuration.md). `esc` (or `←`) backs a drill-in out
to the list it opened from; `esc` on a forge list returns to the floor. The
run detail view above gains its own cross-link into this: `m` opens the PR
view for that run's merge request, when it has one.

### Split view on a tall terminal

With the default `auto` setting, a terminal at least 80 columns wide and
tall enough shows the floor on top and the scoped repo's CI list below it.
The top pane can show floor or workers; the bottom pane can show pulls or CI.
The top title keeps `floor · workers` visible; the bottom separator keeps
`pulls · ci` visible and names the repo. The fleet summary stays on the title
line whichever top tab is selected. Account meters and the floor run summary
appear only with floor on top. The footer is shared.
The bracketed label (`[floor]`, `[workers]`, `[ci]`, or `[pulls]`)
shows which pane has focus. Both visible lists keep refreshing. Press `tab`
to cycle floor → workers → pulls → ci → floor, or `shift+tab` backwards.
`ctrl+w` switches focus between the selected top and bottom tabs;
`1`/`2` select floor/workers on top, and `3`/`4` select pulls/CI below.
`esc` from the bottom focuses the selected top tab. `R` cycles the scoped repo when
the bottom pane has focus. Run, worker, PR, and CI run detail open full-screen;
`enter` or `→` on a top-pane worker opens its detail, and `esc` or `←` returns to the
originating pane with its tab, selected worker and focus.

Press `s` on a list to collapse the split to its selected top tab (floor or
workers) for this session; press it again to restore the split when the
terminal is large enough. A resize that makes the terminal too small also
collapses to that top tab. Both tabs, cursors, filters, and the scoped repo
are kept for the next split, while the top tab takes focus after collapse.
The split enters at 43 rows (`minHeight + 2`) and leaves below 41 rows
(`minHeight`), avoiding a layout flip on a one-row resize. The 41-row minimum
includes the fleet summary and is derived from the
shared header, separator, footer, each pane's worst-case headings and
spacers, and eight actual list rows per pane; the layout changes only on
a resize. If the
terminal is too small, `s` shows `terminal too small to split` and keeps
the current layout. Split hints and size notes yield to the existing keys
and version readout when the footer is too narrow.

To keep the full-screen lists at every terminal size, set this in
`~/.config/uzi/config.toml`:

```toml
[tui]
split = "off"
```

`split = "auto"` is the default; `auto` and `off` are the supported values.
The `off` setting also disables `s`. `uzi tui --demo` and
`uzi tui --sketch` use `auto` regardless of this setting.

### Keybindings

```
←/→, h/l, tab detail: focus the crew rail / the transcript (h/← rail, l/→ transcript; tab cycles). Detail opens focused on the crew rail.
j/k, ↑/↓     move within the focused pane (board: row · detail: between agents on the rail, or scroll the transcript)
g            detail: follow live — re-attach and jump to the newest output (live runs only)
c            detail: fold / unfold the crew list; it also folds by itself when MILESTONES/SPEND/ACCOUNTS would not fit
enter / →    open the selected run (board), worker (workers), or reported run (worker detail)
←            worker detail: return to its originating list or run
W            open the run's worker (run detail; lowercase w still reworks)
/            filter the board or workers list
a            toggle shared own/factory scope (board or workers; admin token required)
h            hide finished runs — completed/failed/cancelled, keeps active + needs-you (board only; no-op on the admin board)
r            refresh
v            open/close the review overlay (detail)
f            start a follow-up (detail, owner only)
y/n          approve/reject, at a plan gate (detail, owner only)
x            cancel the run, asks to confirm (detail, owner only)
esc          back out / dismiss
?            this help
q            quit immediately; ctrl+c asks to confirm, and a second ctrl+c quits at once
```

The run detail view has **two focusable panes**: the crew rail (one lane per
agent) and the transcript. `←`/`→` (or `tab`) move focus between them; the
focused pane is drawn bright, the other dimmed. `↑`/`↓` then act *within* the
focused pane — moving the agent selection when the rail is focused, scrolling
the transcript when it is. A run opens focused on the crew rail.

The transcript **follows live** (tail -f): while a run is producing output the
transcript auto-tails the newest frame and shows a `⇣ following` badge.
Scrolling up detaches it — the badge becomes `⏸ N new · g ⇣` (N is how many
lines are below the fold) and the view holds still so you can read — and `g`
(or scrolling back to the bottom) re-attaches and jumps to the newest output.
Only a live run follows; a finished run's transcript is static.

Note what isn't here: there's no `[a]`-for-approve —
early drafts of this feature used it, but `[a]` doubling as admin-toggle
*and* approve would put "approve a plan" one keystroke from `[x]` cancel on
a live run, so approve/reject moved to `y`/`n` and `a` stayed admin-only.
`[q]` quits immediately; `ctrl+c` asks first (so a stray `ctrl+c` can't drop a
watched run), and a second `ctrl+c` is the escape hatch when the confirm
prompt itself is what's stuck.

**A run parked on a clarifying question (`awaiting_input`) has no in-TUI
composer** — it renders the same "blocked on a human" waiting treatment a
plan gate gets, but `y`/`n` don't apply to it. Answer from another terminal
with `uzi run answer <id>` (see [Commands](#commands)), from the web run
view, or from Slack; the TUI picks the change up on its next refresh.

The list screens and forge drill-ins (PR view, CI run view) use these
navigation bindings:

```
tab          floor → workers → pulls → ci → floor (both layouts)
shift+tab    cycle backwards through the same list order
ctrl+w       split: switch focus between the selected top and bottom tabs
1 / 2 / 3 / 4  focus floor / workers / pulls / ci
s            collapse or restore the split for this session (list screens)
R            cycle the scoped repo (forge list focused; hidden with one enabled repo)
enter / →    open the selected row (pulls → PR view · ci → CI run view)
←            back out of a drill-in (PR view, CI run view) to its list — the symmetric partner of →
↑ / ↓        move the cursor (PR view: over CHECKS · CI run view: over JOBS)
u            open the PR's linked uzi run (pulls row, PR view; shown only when linked)
w            rework the linked run (pulls row, PR view; shown only when linked)
f            fix ci: queue a CI-fix run for the branch (pulls row, PR view, CI run view)
m            detail: open the PR view for this run's merge request (when it has one)
esc          back: a drill-in returns to its list, a list returns to the floor
```

`f` means something different here than in the run detail table above — a
follow-up there, "fix ci" on the forge screens that bind it — but the two
views never overlap, so the key never carries two meanings at once. `u` and
`w` drop out of the legend on a PR with no linked run. A `ci` list row offers
no row actions (just `enter` to drill in); `f fix ci` lives on the CI run
view, where the run's branch is known.

### Steering is run-level, not per-agent

The steer bar sends `follow_up`, `approve_plan`, `reject_plan`, or `cancel`
to the **run** — there's no wire to whisper to one live subagent. The lane
rail is where you *watch* per-agent activity; the run (and its lead, who
then directs its own subagents) is what you *steer*. A queue indicator
above the bar (queued, received, routed, included in a prompt) reflects the same steer queue `uzi run inputs`
prints.

### Who can steer what

The steer bar only appears when you own the run — an admin who opens
someone else's run through `[a]` sees the transcript, lanes, and review
render normally, but the steer bar and queue indicator are replaced with a
one-line reason instead of controls that would 404. Ownership is checked by
asking the server (the same read the steer write itself scopes by), not by
comparing ids client-side, so the two can't drift apart.

**`uzi tui <chat-run>` opens and is read-only, on purpose.** Chat runs never
appear on the board — you can only reach one by id — and the TUI always
suppresses the steer bar for `kind=chat`: a chat follow-up is a
forge-minting action that belongs to the web's guarded, cookie-only chat
surface, and the plain run-input write doesn't know to keep a raw follow-up
out of it. Watching a chat run's transcript and lanes still works.

### The live channel and degradation

Run detail subscribes to the same `/api/ws` hub the web run view rides,
now reachable with a Bearer CLI token (`uzc_`/`uza_`) as well as a browser
session — that's the one backend change this feature needed; per-run
authorization (owner-or-admin) and the socket's origin check are both
unchanged. If the socket can't be opened or drops, the view falls back to a
plain 2s REST poll — the same cadence `run logs --follow` uses — and says so
on screen rather than freezing silently. A non-TTY stdout is refused up
front, before anything tries to draw (see above).

## Reviewing and triaging from the CLI

`uzi review` reads a run's judge output and sets the same **Mark done** /
**Dismiss** triage the [run judge](./judge.md#triage-resolve-dismiss-and-count)
page does, from the terminal:

```sh
uzi review show <run-id>                                    # verdict + recommendations + triage
uzi review resolve <run-id> <rec-id>                         # mark a recommendation done
uzi review dismiss <run-id> <rec-id> --reason wont-do        # valid, not worth doing
uzi review dismiss <run-id> <rec-id> --reason not-an-issue   # false positive
uzi review undo <run-id> <rec-id>                            # clear a disposition
uzi review stats [--json]                                    # your triage tally, across all runs
uzi review file <run-id> <rec-id>                            # file this recommendation as a forge issue
uzi review file <run-id> <rec-id> --repo <repo-id>           # file against a specific repo (ambiguous default)
```

`show` is one run. `backlog` is every recommendation across **all** your runs,
deduped by `(category, target)`, so a recommendation open in two of the five
runs it recurs in is a single row reading `2 open of 5 runs` (`5 runs, all
settled` once none are open) — the terminal form of the
[Judge menu](./judge-menu.md):

```sh
uzi review backlog                                           # what still needs triage
uzi review backlog --bucket all --json                       # settled groups too, for an agent
uzi review backlog --run <run-id>                            # only coordinates that recur in that run
uzi review backlog --category improve_uzi,install_worker_tool  # only these recommendation labels
uzi review resolve --category <c> --target <t>               # mark the whole group done
uzi review dismiss --category <c> --target <t> --reason wont-do
```

`--category` narrows `backlog` to one or more recommendation labels. It is
multi-value — pass a comma-separated list (`--category improve_uzi,install_worker_tool`)
to show groups in *any* of the named labels — and server-validated: an unknown
label is a usage error (exit 2), never a silently empty list, exactly like an
unknown `--bucket`. An empty or omitted `--category` means all labels. The valid
labels are `enable_tool`, `install_worker_tool`, `adjust_template`, `improve_agent`,
`add_agent`, `improve_uzi` and `cost_efficiency`. Like `--run` (and unlike `--bucket`), the label
predicate is applied *before* the server's row cap, so narrowing by label makes
truncation less likely to bite; it composes cleanly with `--bucket`. Note this
`backlog --category` is a **distinct** flag from the `--category` on
`resolve`/`dismiss`: there it is one literal group coordinate to act on, here it is a
multi-value label filter.

Three things to know before acting on a group action's output:

- **`updated` counts coordinates, not recommendations.** One review can carry
  the same `(category, target)` twice and both share a single disposition row,
  so dismissing a group of 5 can correctly report 4.
- **`updated: 0` succeeded and wrote nothing.** The `--json` field itself is
  `0` for three different causes, but the printed message no longer treats
  them as one answer: when the write's own re-read comes back untruncated and
  still holds the coordinate, it says **"that coordinate is already
  settled"** — that's your own data, so naming it leaks nothing. A coordinate
  that's misspelt and one belonging to another user still give the identical
  **"no open member of yours matched"** message; the server refuses to tell
  those two apart on purpose (distinguishing them would let you enumerate
  which coordinates exist for other users), and a truncated re-read folds
  "already settled" back into that same ambiguous message too, since a
  settled coordinate can simply have fallen outside the read window. Re-read
  `backlog` if the message doesn't already tell you which case you're in.
- **`truncated: true` means a missing group is unknown, not settled** — the row
  cap applies before grouping, so a surviving group's counts can be understated
  too. Narrow with **`--run <run-id>`**: the anchor is the only filter applied
  *before* the cap, so it is the only one that changes what gets cut. `--bucket`
  filters the surviving rows and cannot reach the missing ones. The `triage` tally is exempt: it's the canonical all-time aggregate and
  matches `uzi review stats` and the web nav badge exactly.
- **When the write's own re-read is truncated, the CLI prints the `--run`
  remedy for you** — one ready-to-paste `uzi review backlog --run <run-id>`
  line per run *this call actually settled* (read off the write's own record,
  never off the truncated re-read), not a placeholder to fill in yourself. An
  empty result from following one of those lines is the answer, not a dead
  end: nothing on that run is still un-triaged. If the follow-up read is
  itself cut, it prints its own truncation warning above the listing, so an
  empty result with no warning above it is complete. `--json` on the
  *original* write call is the only complete record of what that call did;
  neither `--bucket` nor a later re-read can reconstruct it.

Passing only one of `--category`/`--target` is a usage error (exit 2). An empty
half is a literal empty string, not a wildcard, so sending it would report a
successful no-op.

`uzi review file <run-id> <rec-id>` files one recommendation as a real forge
issue on **your own** connection. Title and description are server-templated
defaults from the same draft the web filing UI shows — the CLI files the
defaults; editing the draft before filing stays a web action. A successful
file records the issue under the review's `filed_issues` and moves the
recommendation to the `filed` bucket. `--repo <repo-id>` overrides the draft's
default repo; when the default is ambiguous and no `--repo` is given, the CLI
prints the server's picker note and exits with a usage error (exit 2) rather
than guessing. Exit 5 if the recommendation is already filed or mid-filing,
exit 4 if the run or recommendation is unknown or not yours. There is no
group form — filing is one issue per recommendation, matching the web; only
`resolve`/`dismiss` have a `--category`/`--target` group shape.

`<rec-id>` is the short, git-style id `show` prints as the first column of
each recommendation (or the full UUID from `--json`); an unambiguous prefix
resolves against the run's **current** review, so you can paste straight out
of `show`'s output. An ambiguous prefix is a usage error (exit 2, "use a
longer id"); an id that matches nothing is a not-found (exit 4) with a
refresh hint — the review may have changed under a re-judge.
`dismiss` requires `--reason wont-do` or `--reason not-an-issue`; anything
else is a usage error (exit 2), raised before any request is sent. `undo` on
a recommendation with no disposition is treated as already-undone (a
friendly line, exit 0), not a failure.

`uzi run review <id>` still works — it's a hidden, deprecated alias for
`uzi review show <id>`.

**The mutation verbs are owner-only, whatever the token.** A `uzc_` token
drives `resolve`/`dismiss`/`undo`/`stats` on its own runs, same as clicking
the buttons yourself. A read-only `uza_` token can `show` anyone's review
(the same admin reach other judge reads get) and `stats` always reports the
token owner's *own* tally, but the read-only ceiling still holds for writes:
the **per-run** `resolve`/`dismiss`/`undo` against **another** user's review is
refused (exit 4, not found), exactly as for a non-admin `uzc_` token. On its
**own** runs, though, a `uza_` token can triage same as any owner — the
ceiling blocks reaching into someone else's review, not every write
everywhere.

The **group** form is owner-only too, but it refuses *silently* rather than
with a 404, and the difference is a contract, not an oversight. Its unit is a
`(category, target)` coordinate, not an id, so there is nothing to report as
not-found: another user's coordinate simply resolves to zero of *your* rows and
comes back `200`, `updated: 0` — the same answer a misspelt coordinate gives
(an already-settled coordinate of your own is told apart by its message; see
above). That indistinguishability between "misspelt" and "someone else's" is
the point; a per-item outcome would rebuild exactly the existence oracle the
404-on-everything rule removes. Don't read `updated: 0` as an error, and don't
read it as success either.

## Incidental findings: `uzi findings`

While a worker implements a PRD it sometimes notices a bug **outside** the task at
hand — a leaked ticker in a sweeper it read on the way past, a retry that can never
succeed. Rather than smuggle an unrelated fix into the run's MR or drop the
observation, it flags an *incidental finding* without stopping its turn. Nothing
reaches the forge until you act: the findings collect into a per-repo backlog,
deduped by `(repo, location)` across runs, that you triage from the terminal the same
way as the [judge backlog](#reviewing-and-triaging-from-the-cli).

```sh
uzi findings list                                            # what still needs triage
uzi findings list --bucket all --json                        # filed, done and dismissed too, for an agent
uzi findings list --repo <repo-id>                           # one repo
uzi findings list --run <run-id>                             # coordinates that also occur in that run
uzi findings draft <finding-id>                               # preview one issue without filing
uzi findings draft <finding-id> <finding-id> ...              # preview one grouped issue
uzi findings file <finding-id>                               # file a forge issue from a coordinate
uzi findings file <finding-id> <finding-id> ...              # file ONE issue for several coordinates
uzi findings release <operation-id> --confirm-no-issue       # free a group filing that never confirmed
uzi findings dismiss <finding-id> --reason wont-do           # valid, not worth doing
uzi findings dismiss <finding-id> --reason not-an-issue      # false positive
uzi findings resolve <finding-id>                            # mark it done yourself
uzi findings undo <disposition-id>                           # undo a done or a dismissal
uzi findings stats [--repo <repo-id>]                        # your triage totals, across your repos
```

`list` prints one row per `(repo, location)` coordinate, grouped by repo, carrying
the actionable `finding_id`, the latest title, `seen in N runs`, and a state — a
dismissed row shows its reason (`Dismissed · Won't do` / `Dismissed · Not an issue`), a
coordinate you marked done yourself reads `done`, and one the issue-close sync settled
(below) reads `Done via #N`. The
`open_count` (what still needs triage) prints as a meta line and rides the `--json`
envelope. `--bucket` filters by disposition and defaults to `to_file`; `filed`,
`done`, `dismissed` and `all` show the rest. `--repo <repo-id>` (from
`uzi repo list`) and `--run <run-id>` narrow the list. As with `review backlog`, an
unknown `--bucket` is a usage error (exit 2), never a silently empty list, while a
well-formed but foreign or unknown `--repo`/`--run` returns an **empty list** — no
existence oracle — rather than a 404.

`draft` previews the server's issue draft without filing. Give it one finding id,
or several ids that collapse to one `(repo, location)` coordinate, for the single
finding draft. For multiple coordinates it fetches the group draft using their
distinct disposition UUIDs from one repo; the server orders group members by
disposition UUID. Older evidence ids resolve to the same coordinate and duplicates
count once; each id in a multi-id call needs a triage record. At most 50 distinct
ids can be previewed. The human output is the title, a blank line, then the body;
`--json` returns the exact server draft DTO, including its labels. A group
preview omits the filing-time operation marker, and filing may trim evidence at
a UTF-8-safe boundary to make room for it. Draft labels are suggestions: the
group draft includes assembled labels, including the mandatory marker label,
but unedited CLI filing sends only the explicit ids and the server applies its
mandatory marker label. Use explicit ids with `file`; there is no `--run` shortcut.

`file` turns one coordinate into a real forge issue on **your own** connection. The
title, description and labels are assembled server-side from the stored, sanitised
finding plus a mandatory marker label, so the CLI files the defaults — editing the
draft before filing is a web action. It prints the created issue's number and URL;
`--json` returns `{issue:{iid,web_url,title}, warning?}`, where a `warning` means the
issue was created but its local record could not settle (a success with a note, still
exit 0), not a retry signal. Filing a coordinate that is already filed or mid-filing
is a conflict (exit 5); an unknown or foreign `<finding-id>` is not-found (exit 4).

`file` also takes several ids of one repo and files **one** issue for all of them, with
each finding linked to it. One id takes the single-file path above. With two or
more ids (even the same id repeated), each distinct id's draft is fetched first and
resolved to its coordinate (older evidence ids included); duplicates count once. An id
with no triage record yet is a usage error (exit 2) and nothing is filed, even if it is
the only coordinate left; to file an untriaged coordinate, pass its id alone. Only when
every id resolves to one coordinate does the call fall back to the single-file filing.
At most 50 distinct ids per call. It is all or nothing: if any chosen
coordinate is already filed, dismissed or held, the call is a conflict (exit 5) and
stderr lists the `pending operation <op>` ids holding them. `--json` returns
`{operation_id, disposition_ids, phase, issue?, warning?}`, the issue plus the linked
disposition ids. If the filing cannot be confirmed (HTTP 202) the CLI prints the
operation id and a release hint and exits 5; the issue may still exist. uzi's repo sync
settles the group on its own once it finds the marked issue; that happens on the
periodic full (reconcile) sync and on a manual board Refresh. If it does not, check the
forge and, only once the operation's deadline has passed and no such issue exists, run
`uzi findings release <operation-id> --confirm-no-issue`. The flag is required: without
it the command is a usage error (exit 2), and an operation that cannot be released yet
or is unknown exits 5 or 4. Held rows show `pending group <op>` as their state in
`uzi findings list --bucket all`. Closing the filed issue on the forge marks every member
Done, as for a single finding; `undo` stays per finding.

`dismiss` triages a coordinate to `dismissed` so it stays gone and does not re-nag
across later runs (`not-an-issue` is a false positive, `wont-do` is valid-but-skip).
A missing or invalid `--reason` is a usage error (exit 2) raised before any request is
sent; a coordinate that is not dismissable (already filed, being filed, or already
dismissed) is a conflict (exit 5), and an unknown or foreign id is not-found (exit 4).

`resolve` is the twin of [`review resolve`](#reviewing-and-triaging-from-the-cli): it
marks a coordinate **done** yourself, the human counterpart to the issue-close sync
below. It works from `to_file`, `filed`, or `dismissed` (a done from `dismissed`
replaces the dismissal; resolving a coordinate the sync already set — `Done via #N`
— converts it to a human done rather than re-asserting the sync verdict). The only
refusal is a coordinate whose issue is currently being filed — a conflict (exit 5);
an unknown or foreign id is not-found (exit 4). It prints the disposition id `uzi
findings undo` takes; `--json` returns `{finding, status, disposition_id}` — no other
keys, so read the disposition id off there rather than off `list`'s table.

`undo` reopens a dismissed or done coordinate. From `dismissed` it goes back to
`open` (the To triage bucket); from a done it exposes `filed` if the coordinate has a
filed issue, otherwise `open` — it never restores a dismissal a done replaced. It keys on the
coordinate's `disposition_id`, **not** the `finding_id` the human `list` view and
`file`/`dismiss`/`resolve` use — `disposition_id` is always present (a dismissed or
done coordinate can outlive its own evidence, while `finding_id` goes nil once that
evidence is gone), so read it off `--json`, not off `list`'s table. A coordinate with
no disposition to undo — unknown, foreign, or never dismissed or done — is treated as
already-undone: a friendly line, exit 0, never a crash.

`stats` prints your Findings triage totals (total, to triage, filed, done, dismissed,
false positives) across every repo you own; `--repo <repo-id>` narrows it to one (a
foreign or unknown id is an all-zero tally, never a 404). `--json` emits the raw
totals object. It's the same number the web nav badge and the Findings tabs show for
the same repo scope.

`<finding-id>` is the id `list` prints as the first column of each coordinate; paste
it straight into `file`/`dismiss`/`resolve`. Treat `location`, the title and `repo_path` as
untrusted free text (they are agent-authored): render them as data, and branch only
on the `status`/`bucket` enums.

## Anthropic tokens

You can hold several named [Anthropic credentials](./anthropic-token.md) and
point individual workers at them. The CLI can **read** that set and **move a
worker between its members** — it cannot change the set itself:

```sh
uzi token list                                 # labels, default flag, pool opt-in, live eligibility, enabled state
uzi token test <label> [--kind anthropic|codex|openai-key]
uzi token pool console-key --on                # add it to the auto-selection pool
uzi token pool console-key --off               # take it back out
uzi worker set-token <worker-id> console-key   # bind a worker to a named token
uzi worker set-token <worker-id> --default     # clear the binding
uzi worker set-token <worker-id> --auto        # pick per claim, from the pool
```

`uzi token test` checks one enabled stored credential without exposing its
value. Labels are matched without regard to case. If the same label occurs
across kinds, pass `--kind`; use `uzi token list` to find the label. Anthropic
Test tries Usage first and may use a small Messages request when probing is
enabled; Codex Test reconciles a `staging` or `failed` login, while a `linked`
login gets a show-only usage read without refresh; OpenAI API key Test checks
`GET /v1/models`, which proves endpoint access alone, not inference or billing.
See [Anthropic tokens](./anthropic-token.md#good-to-know) and [Codex credential
testing](./codex-credentials.md#testing-a-credential).

The safe result has `status` (`ok`, `rejected`, `permission_denied`, or
`inconclusive`), optional `reason` (`generic`, `vault_locked`, or
`superseded`), and optional `display` (for example, `models endpoint
accessible`). `--json` also includes `kind`, `label`, and `id`; it carries no
credential value or provider response. An inconclusive result is not a
successful credential check. A result can go stale after the test.

`uzi token pool` is the one token **write** the CLI has, and it is here for
the same reason the others are not: it mints nothing and reveals nothing, it
only re-points spend among tokens you already hold. Adding, renaming,
re-defaulting and deleting stay web-only.

`uzi token list` prints two columns about the pool, and they answer different
questions:

| column | question |
|---|---|
| `POOL` | did you opt this token in? |
| `ELIGIBLE` | could auto-selection pick it *right now*? |

`ELIGIBLE` is `eligible` when it can, or `rejected` / `no_reading` /
`unmeasured` / `stale` / `below_threshold` when it cannot; `-` when the token is not pooled
(the `POOL` column beside it already says so), and `?` when the eligibility
read failed. **Check it after opting a token in**: a token uzi has never
managed to poll reads as not eligible for normal ranking while looking active —
though if it is pooled, the last-resort floor can still spend it when nothing
pooled has a usable reading.

Under `--json` the same answer is the `auto_status` field. It is always
present and is **`null` when it is not known** — which is not the same as
"not eligible", so branch on null before you branch on the value. An
un-pooled token reports `not_pooled` there rather than the table's `-`.

The last column, `STATE`, is `enabled` or `disabled since <date>` for a
credential you have [disabled](./anthropic-token.md#disabling-a-token) in
Settings; `--json` carries it as `enabled` (true/false) and `disabled_at`
(null while enabled). A disabled token keeps its `POOL` opt-in but reads `-`
under `ELIGIBLE`, since nothing picks it while it is disabled; under `--json`
its `auto_status` is `null`, because uzi reads no usage for it, so a script
checks `enabled` first and treats a `null` `auto_status` as "unknown" only for
an enabled token. `uzi token pool <name> --on` refuses a disabled token.
Disabling and enabling are
web-only; the CLI has no command for them.

`uzi worker list` carries a `TOKEN` column showing how each worker chooses:
the token's **name** when it is pinned, or `default` / `auto`. An `auto`
worker has no fixed answer, which is why it says `auto` rather than naming
whatever it happened to pick last.

`uzi run get` names the credential a run spent **and the mode that chose
it** — `console-key — auto, 62% headroom`, `console-key — pinned`,
`console-key — auto (pooled token, no fresh readings)`. See
[Anthropic tokens](./anthropic-token.md) for the full set and what each
reason means.

### Upgrade status

`uzi worker list` carries an `UPGRADE` column beside `VERSION`:

| value | meaning |
|---|---|
| `up to date` | the worker runs the release it is targeted at, or a newer one |
| `outdated` | it runs an older release, and nothing is currently rolling it |
| `upgrading` | a roll is in progress; expected and transient |
| `FAILED` | it tried to take a new release and could not — this is the one to act on |
| `-` | no usable version to compare (an unstamped local image, or a `dev` control plane) |

Two things worth knowing before acting on it. A worker's version is recorded **at
register only**, so a worker that is offline mid-roll still reports the release it was
running before — which is why `FAILED` comes from the controller watching the pod rather
than from the worker itself. And a **hosted** worker is compared against the tag the
controller is rolling to, which `values.yaml` may pin below the api's own release; the
Workers page states that divergence when it exists.

`-` is not a problem to fix. It is what a locally built image and an unstamped control
plane both look like, which is most of a development setup.

### Disk usage and checkpoint durability

`uzi worker list` and `uzi admin workers` (the cross-user list) both carry a
`LARGEST RUN` column (PRD #1809 M6, D8): the HOME size of the worker's
largest run, so a run growing toward filling the data volume is visible
before it does. It reads a compact size (e.g. `4.2GiB`), a trailing `+`
when the worker's size walk was truncated (the number is then a lower
bound), or `-` when the worker reports no run sizes at all (an older
worker, or no fresh report yet). The run id and cache bytes behind it ride
`--json` (the `run_disk` field) for scripting; this is the same figure the
`fleet.rundisk` [admin health](admin-health.md#the-checks) check's action
points an admin at.

The `RUNS` column of `uzi worker list` and `uzi admin workers` does not count
a run whose outcome the worker holds journaled but has not delivered to the api
(a pending outcome) as running. Those show as `N pending outcome(s) (oldest
<age>)` instead, so a worker stuck holding outcomes is visible at a glance.
`--json` carries the raw `terminal_pending` and `terminal_pending_since` fields
on each `reported_runs` entry. A worker over its pending-outcome cap lists
only some of its pending outcomes per heartbeat, so the count is then a lower
bound. A run whose outcome stays pending for 60 seconds
(with the default heartbeat) is flagged stalled; see
[run health](./run-health.md#what-the-flags-mean).

A worker that has latched a residue quarantine shows `(quarantined)` after its
status in `uzi worker list` and `uzi admin workers` (for example `online
(quarantined)`): it claims nothing until its container restarts, see
[Quarantined worker](worker-setup.md#quarantined-worker). The reported cause is
never put in the table; read `residue_quarantined_at` and
`residue_quarantine_cause` in `uzi admin workers --json`, or the worker view in
`uzi tui`.

`uzi run get` gains four rows, each emitted only when the server has
something to say:

| Row | When it appears | What it shows |
|---|---|---|
| `HOME` | the server has a fresh worker report of the run's size | the run's HOME and cache size, e.g. `4.2 GiB (cache 3.0 GiB)`; `at least` leads when the size walk was truncated (both numbers are then lower bounds) |
| `CHECKPOINT` | the run is parked (`limit_wait`, `recovery_wait`, or `paused`) and the worker reported it | whether the checkpoint published for this park contains the run's latest committed work: `contains the latest work`, or `does NOT contain the latest committed work (the worker keeps it)` |
| `DISK` | the run is parked with cause `data_volume_full` | the [waiting-for-disk-space sentence](run-recovery-wait.md#worker-data-volume-full), the next retry time, and the run's lifetime count of counted disk parks |
| `EST. TAIL` | a Claude run has usage no result frame covered (an interrupted session) | the [estimated tail](run-cost.md#estimated-tail-of-an-interrupted-session), kept apart from `COST`: `~$1.23 estimated (price-table version) · 1.2M in/34k out · partial: <reasons>`, `<$0.01 estimated` under half a cent, or `cost unknown (unpriced)`; hidden for the all-zero, complete tail |

`CHECKPOINT` shows while the run is actually parked (`limit_wait`,
`recovery_wait`, or `paused`); the API clears the underlying flag on every
claim and every `running` report, so a resumed run reports "not reported"
until its next park, rather than carrying a stale value forward. `uzi run
list` / `uzi admin runs` append `(waiting for disk space)` to the STATUS
cell for a `data_volume_full` park, the same pattern as `(waiting for
vault unlock)`.

A run that **fails** because its worker's data volume stayed full past the
disk-park cap gets a `FAIL_ORIGIN` row reading `data_volume_full (the
worker's data volume stayed full after N counted disk parks)` instead of
the bare enum — see [Worker data volume
full](run-recovery-wait.md#worker-data-volume-full) for the full park and
failure behavior, and [`UZI_RUN_DISK_PARK_MAX`](configuration.md#server-api)
for the cap.

`set-token` takes a **label** (the name from `token list`), not an id, and
takes effect on that worker's next claim — no restart and no re-minted join
token. Passing both a label and `--default`, or neither, is a usage error
(exit 2) rather than a guess; an unknown label is refused rather than stored.
A bound worker's **chat** runs still spend your default token: the binding
covers the run lane only.

**Adding, renaming, re-defaulting and deleting a token are web-only, and
that is a security boundary rather than an unfinished feature.** A CLI token
is a bearer credential — a stolen `uzc_` is meant to be able to read and to
drive runs, but never to *replace the credentials it runs on*. If token
writes were reachable over Bearer auth, an attacker holding a leaked `uzc_`
could swap a user's Anthropic credential for their own and quietly redirect
every future run's spend. That is exactly the escalation the split prevents,
and it is the same reasoning that keeps `worker create` out of the CLI.
`set-token` sits on the allowed side because it mints nothing and hands back
no credential: it re-points a worker between tokens the caller already owns.

**Driving the API directly?** The old kind-path routes still work as
deprecated aliases over your *default* token: `PUT
/api/me/secrets/anthropic_token` rotates-or-creates it, and `DELETE
/api/me/secrets/anthropic_token` removes it. The DELETE alias now answers
**409** once you hold more than one token, because "delete the anthropic
token" stopped naming one row — delete by id instead
(`DELETE /api/me/secrets/anthropic_token/{id}`). All of these are cookie-only,
for the reason above.

## Agents: `--json` and exit codes

Every command prints a human table by default; pass `--json` for a stable
document instead. A pipe does **not** auto-switch formats — that would be a
silent contract change — so an agent always passes `--json` explicitly.

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | generic error |
| 2 | usage error (bad flags/args) |
| 3 | auth required / invalid / wrong scope |
| 4 | not found |
| 5 | conflict (e.g. the run already finished) |
| 6 | server unreachable / 5xx |
| 7 | a `run wait --timeout` elapsed before any target state |
| 8 | `uzi admin health` overall status is `danger` (or `warn`/`unknown` under `--strict`) — a success-path exit (HTTP 200), so a transport/auth failure keeps its own 3/6 |

Branch on the exit code, not on stderr text — the wording is for humans and
can change. There's also no `--token` flag: a credential must never land on
`argv`, readable via `ps`/`/proc`. Use `$UZI_TOKEN`, or `uzi auth token`,
which reads a token from stdin.

### The `--json` envelope shape is per-verb

The `--json` wrapper is **not uniform** across the run verbs, so don't reuse one
verb's unwrapping for another:

| Verb | `--json` shape |
|---|---|
| `run create` | run nested under a top-level `run` key: `{"run": {…}}` |
| `run get` | the run object at the top level: `{…}` |
| `run list` | a top-level array: `[{…}, …]` |
| `run logs` | **NDJSON** — one JSON object per line, not a single document |

### Reading one scalar: `run get --field`

To read a single value — a status, an MR url — you do not need the whole object
or a JSON parser. `uzi run get <id> --field <name>` prints the named top-level
**scalar** field raw and unquoted, one per line; `--field` is repeatable and the
lines come out in the order you named them. `--field status` is the cheap poll a
loop wants, and it sidesteps a real footgun: piping `--json` through a shell that
re-interprets escapes (notably **zsh `echo`**, which turns the CLI's valid
`\uXXXX`-escaped control bytes back into raw bytes and breaks `jq`) mangles the
document. `--field` hands back the decoded value with nothing to re-mangle. If
you *do* parse `--json`, use `printf '%s'` (never `echo`) or write it to a file.

A `null` or absent field prints an empty line (so a nil array field is an empty
line, not an error). An unknown field, or a **non-scalar** one that is populated
— any array or object field (e.g. `milestones`, `own_agents`, `agent_exclusions`,
`usage`, `current_activity`), which you read with `--json` — is a usage error
(exit 2). `--field` and `--json` are mutually exclusive (two output modes).

The model a schedule froze onto a run is readable this way too:
`uzi run get <id> --field model` (the model alias/id, an empty line when the
schedule pinned none) and `uzi run get <id> --field override_subagent_model`
(the boolean literal `true`/`false` — whether that model was also applied to
every subagent).

A run's trigger provenance — what/how/who started it — is readable this way:
`uzi run get <id> --field trigger_source`. It is a NOT NULL server enum
(DEFAULT `manual`, so always set; historical rows carry a best-effort
backfilled value), one of: `manual`, `autopilot`, `schedule`, `self_improve`,
`ci_fix`, `mr_rework`, `chat`, `task`, `task_review`, `then_fix`, `judge`,
`judge_rerun`, `resume`, `cross_check`. The human `run get` view prints it as a `TRIGGER` row,
and `uzi admin runs` shows it as a `TRIGGER` column.

A run's plan-gate revision is readable the same way:
`uzi run get <id> --field gate_revision` (the number of the plan currently
presented at the gate, `0` for a run that has never gated under this
feature). See [Run activity pane](./run-activity.md#plan-approval-gate) and
`--expected-gate-revision` above.

### Plan cross-check evidence

`uzi whoami` shows your `PLAN CROSS-CHECK` consent value. Change it in
**Settings → Run defaults → Cross-check**; there is no CLI write verb for
this cookie-only consent setting.

`uzi run get <id>` shows `PLAN_CROSS_CHECK` for a current cross-check human
gate reason. Its `PLAN_CHECK_*` rows show the stored candidate's result,
reason, checker id, recorded model/effort, tokens/cost and findings. A human
revision changes the evidence prefix to `EARLIER_PLAN_CHECK_*` and adds a
warning that the check does not certify the current plan. Missing cost stays
unavailable; subscription usage is distinguished from metered spend.

The human view bounds findings to 20 items and sends displayed text through
`Plain` for terminal/control sanitization. Use `uzi run get <id> --json` for
the structured `plan_cross_check_required`, `plan_cross_check_gate_reason`
and optional `plan_cross_check_summary` fields, or
`uzi run get <id> --field plan_cross_check_required` for the run's snapshot.
A stored checker APPROVE and a current human gate can coexist: decide against
the displayed gate revision, not historical findings. See
[Cross-check](./cross-check.md) for fallbacks and terminal delivery failures.

A run's PRD-completion declaration is readable the same way:
`uzi run get <id> --field prd_done_path` (the repo-relative path the run
declared it moved a completed PRD to, e.g. `prds/done/72-x.md`) and <!-- check-docs:ignore-path: didactic example path, not a real PRD -->
`uzi run get <id> --field prd_patch_settled_at` (an RFC3339 timestamp once
the PRD-link patch lifecycle has settled, an empty line while still
pending). Both are emit-only-when-set on the human view too — `run get`
prints them as `PRD_MOVE` and `PRD_PATCH_SETTLED_AT` rows only when the run
has declared a move — and appear the same way under `--json`.

A running run's wall-clock stop time — the same clock the **near timeout**
[run-health](./run-health.md) flag (its `slow` value under `--json`) counts down
to — is readable the same way:
`uzi run get <id> --field deadline_at` (an RFC3339 timestamp, an empty line
when the run has none). It's set only while a non-`chat`, non-`judge`,
non-`interactive` run is actually `running`, so a queued run, a gated run, a
chat, and a finished run all print nothing. `run get`'s human view prints it
as a `DEADLINE` row right after `HEALTH`, folded to local time plus a
countdown (`15:20 · 1h05m left`, or `15:20 · stopping` once past) instead of
the raw timestamp, emit-only-when-set the same way `PRD_MOVE` is above. On a
run [extended](./run-health.md#giving-a-run-more-time) past its frozen
budget, the row gains a trailing `· +2h extended` clause naming the total
extra time granted so far.

`run get` also prints a `NOW` row right after the `MILESTONES` block: the
run's server-derived current activity, folded to
`<agent> · <agent_label> · <tool> <detail> · <age> ago` — the CLI twin of
the web run view's now line and the TUI's crew-rail/board second line (see
[Milestones and the now line](./run-activity.md#milestones-and-the-now-line)).
It's emit-only-when-set: absent for a terminal run or one with no recorded
activity, so a finished run and any pre-#1064 run print no such row.
When the lead has attributed agents to one or more in-progress milestones,
`run get` prints a `NOW <id>` row per attributed milestone instead of the
single global row — folded the same way, with live tool/age on only the
uniquely-matching lane and role · label alone on the rest; an in-progress
milestone the lead did not attribute gets no `NOW` row at all, just its
plain mark in the `MILESTONES` block. A run with no effective attribution
keeps the single global `NOW` row unchanged.
`--json` carries the full `current_activity` object; `current_activity` is
an object, so `--field current_activity` is the documented usage error
above (exit 2) even when the run has one — read it with `--json` instead.

`run get` also prints the [run summaries](./run-summaries.md), when they've
landed: an `INTENT` row (what the run will implement), a `PLAN SUMMARY` row
(what the proposed or approved plan will do), and one `DELTA` row per way
the plan diverged from the original ask. All three are emit-only-when-set —
a pre-feature run or one still queued prints none of them, and a seeded run
(one that skipped planning) prints its `INTENT` row but no `PLAN SUMMARY` or
`DELTA` rows. The scalar two are readable individually with
`--field summary_intent` / `--field summary_plan`; `summary_deltas` is an
array, so read it with `--json` instead.

`run get` also prints a run's inferred/hinted scheduling requirements (see
[Capability-aware scheduling](./capability-scheduling.md)), emit-only-when-set
like the rows above — a run predating the feature, or one whose plan-time
inference produced nothing, carries none of them: a `REQUIRED_CAPABILITIES`
row (the hard, closed-vocabulary set — today `docker`/`jvm` — a subset of
which the run's worker must have to claim and to clear the plan-approval
gate), a `REQUIRED_TOOLS` row (provisionable toolchains that will simply be
installed at run time, never a blocker), and a `SIZE_CLASS` row (`s`/`m`/`l`,
advisory only). All three are comma-joined where the value is a list, and
readable the same way under `--json`.

### Run status, and what `--follow` waits for

A run's `status` (on `run get` and `run list`) is one of exactly **thirteen** values:
`queued`, `claimed`, `running`, `awaiting_approval`, `awaiting_input`,
`awaiting_followup`, `limit_wait`, `pool_wait`, `recovery_wait`, `paused`, `completed`,
`failed`, `cancelled`. Only the last three are **terminal**, and `uzi run logs
--follow` returns **only** on those three. The seven non-terminal parks it will
*not* stop at:

- `awaiting_approval` — the plan gate;
- `awaiting_input` — a clarifying question, answered with `run answer`;
- `awaiting_followup` — an interactive task (`uzi handoff --interactive`)
  parked after a clean `signal_done`, awaiting your next `run follow-up`; it
  does **not** auto-resume on its own — wind it down explicitly with `run
  stop`, or let its worker-side idle timeout finalize it — see [Interactive
  mode](./handoff.md#interactive-mode);
- `limit_wait` — parked on a Claude or Codex subscription usage window, promoted
  back to `queued` once past its `retry_not_before` (reset plus jitter, or bounded
  fallback when no usable reset is known); see [Paused on a usage limit](run-limit-wait.md);
- `pool_wait` — an `auto` worker held because its Anthropic token pool is
  genuinely empty, resumed once a token is pooled — see [Letting uzi pick the
  token](anthropic-token.md#letting-uzi-pick-the-token-auto-selection);
- `recovery_wait` — parked to recover from a resumed turn that came back empty
  (no model activity) or hit a transient provider error; the sweep auto-resumes
  it on a capped backoff until it recovers or you cancel it — see [Recovering
  from a transient interruption](run-recovery-wait.md). A Codex credential
  refresh or release that found the owner's vault locked also parks here
  (cause `vault_locked`); it takes the same capped backoff and no lifetime
  cap. The owner's explicit successful vault unlock best-effort queues an
  already parked run promptly; the scheduled retry remains the fallback,
  and a worker starts it through the normal claim path — see
  [Vault locked](run-recovery-wait.md#vault-locked). A Codex subscription
  run can also park here because its account is quarantined or needs a fresh
  login (cause `codex_account_unavailable`); unlike the transient park it has
  no backoff or cap, and it resumes when the account is usable again, or fails
  if the run's credential binding changed — see [Codex
  account unavailable](run-recovery-wait.md#codex-account-unavailable).
- `paused`: an owner-requested hold (`uzi run pause`), resumed on demand from
  the run page or `uzi run resume <id>`. See [Pausing and resuming a
  run](run-pause.md). It does **not** auto-resume. A `paused` run can instead
  be a **completion-blocked hold** on a run whose [structural completion
  interlock](run-completion-hold.md) couldn't finish a frozen milestone —
  same status, a different reason, and a different resume path
  (`uzi run decide <id> --continue`, not `run resume`). `run get` prints the
  distinction as a `COMPLETION` row reading `Completion blocked`, alongside
  emit-only-when-set `COMPLETION_UNMET`/`COMPLETION_ATTEMPTS`/`HOLD_CONTEXT`
  rows (the same fields under `--json`: `completion_phase`, `hold_reason`,
  `completion_unmet`, `completion_attempts`, `hold_context`) — a plain owner
  pause carries none of them. Before it parks that way, an interlocked run
  also passes through two running-state `COMPLETION` labels of its own —
  **Checking completion** and **Reworking unmet milestones** — still `status:
  running` underneath, not a distinct CLI status value. A `paused` run can
  also be **waiting on a disabled credential** (`hold_reason:
  credential_disabled`): a token or Codex login it needs was [disabled in
  Settings](anthropic-token.md#work-that-needs-a-disabled-token-waits). It
  resumes on its own once that credential is enabled again. `run get` prints
  a `HOLD` row reading `credential disabled` with the next step (enable it in
  Settings, or `uzi run set-token` where the run accepts a token switch),
  `run list` shows `paused (credential disabled)`, and the TUI draws it as
  `⊘ cred disabled` in NEEDS YOU.

`limit_wait` and `recovery_wait` auto-resume on their own on a timer — nothing
to do but wait or cancel; `pool_wait` instead clears only when a token is
opted into the pool (or on demand with `uzi run resume-now`), so waiting alone
does not resume it.

So to wait for a plan gate or a clarification, use **`uzi run wait <id>`** (next
section) — leaning on `--follow` there blocks until the run truly finishes, which
may be never while it waits on a human. If you see a `status` outside this list, the server is newer
than this binary — upgrade rather than trusting it to mean "active". (The live
`/api/ws` stream and `uzi tui` rewrite an unrecognised status to `unknown`; plain
`run get`/`run list --json` pass it through as-is.)

A `running` run whose agent is still drafting its plan, pre-approval, reads
**planning** instead — in the STATUS column of `run list`/`run get`, in the
TUI board and detail header's status chip, and on `admin runs` — so you can
tell "still proposing work" apart from "actively implementing" at a glance.
It's still the same `running` value underneath, not an additional status.

### Waiting for a state: `uzi run wait`

`uzi run wait <id>` blocks until the run reaches a state you can act on — the
built-in primitive for driving a gated run headless, replacing the hand-rolled
`while … run get … sleep` poll loop. With no `--until` it stops on any
**actionable or terminal** state (`awaiting_approval`, `awaiting_input`,
`awaiting_followup`, `completed`, `failed`, `cancelled`) and waits through the
rest (`queued`/`claimed`/`running`/`limit_wait`/`pool_wait`/`recovery_wait`/`paused`):
limit and recovery waits retry on a timer, pool waits need an available pooled token,
and owner pauses need `uzi run resume`. A bare `run wait` means
"wait for the plan gate, a clarification, an interactive task's park, **or**
the end".

- It **exits 0** the moment a target state is reached — including if the run is
  already in one when you call it.
- It polls `GET /api/runs/:id` every `--interval` (default 3s) client-side (no
  server long-poll), printing each transition to **stderr**; `--json` prints the
  final run object (same shape as `run get --json`) to **stdout**.
- `--timeout <dur>` is opt-in and gives **exit 7** if it elapses before any
  target state. There is no default timeout: a healthy gated run stops at its
  gate, so a bare wait cannot hang.
- A single transient `6` (a server blip) is retried, not fatal; a `4` (not
  found) is immediate.
- `--until <a,b>` overrides the stop set, validated against the thirteen statuses.
- `--min-plan-seq <n>` is for waiting on a REVISED plan after `uzi run
  revise`: it makes the wait stop at `awaiting_approval` only once a plan
  message with seq greater than `<n>` exists, so it does not return on the
  stale pre-revise gate. It gates only the `awaiting_approval` stop; every
  other target still stops unconditionally. Default is off (`-1`); `0`
  means "wait for any plan" (a plan message's seq is always ≥ 1).

**Narrow the wait after approving.** A run lingers at `awaiting_approval` for a
beat after a successful `run approve` (the async flip to `running`), so the
second wait in a gated loop must exclude the gate it just cleared:

```
uzi run create --repo <id> --issue <iid> --json      # gated run
uzi run wait <id>                                     # returns at awaiting_approval
uzi run approve <id>
uzi run wait <id> --until completed,failed,cancelled  # narrowed, not a bare wait
uzi run get <id> --field mr_web_url                   # the MR, raw
```

## Bundled skill and session-start hook

**Two targets, one embedded body.** The CLI installs (and self-upgrades) the
same bundled skill into **two** harnesses: Claude Code
(`~/.claude/skills/uzi-cli/SKILL.md`) and Codex CLI
(`~/.agents/skills/uzi-cli/SKILL.md`, independent of `$CODEX_HOME`). Both
copies are generated from the binary's own command tree — neither ever drifts
from the CLI you actually have installed. Every executing non-skill `uzi`
command refreshes both, best-effort, before it runs (set
`UZI_SKILL_AUTO_UPGRADE=0` to disable that); the automatic path only ever
writes the Codex copy when a Codex config home already exists, so it never
litters a machine that doesn't run Codex.

All four `uzi skill` verbs (`status`, `install`, `install-hook`,
`uninstall-hook`) take `--target claude|codex|all`:

- **Omitted** (the default) — every auto-detected target: Claude always,
  Codex only when its config home (`$CODEX_HOME`, or `~/.codex` when unset)
  already exists as a directory.
- **`claude`** / **`codex`** — that target only. An explicit `codex` (or
  `all`) acts even when Codex was not auto-detected; `install`/`install-hook`
  then create the directories Codex needs, while `status`/`uninstall-hook`
  stay read-only/no-op over an absent directory.
- **`all`** — both targets, unconditionally.

`uzi skill install [--force] [--target ...]` refreshes the selected target(s)
explicitly — `--force` overwrites even a file you edited (your edit is
preserved to `SKILL.md.bak` first) — and `uzi skill status [--target ...]`
reports every selected target's path and whether it's installed and current.

**The session-start hook, opt-in.** The per-command refresh above only helps
once a `uzi` command has run — right after an upgrade, a fresh session can
still read the OLD skill before that happens. Run
`uzi skill install-hook [--target ...]` to narrow that window, for the
selected target(s):

- **Claude Code** — wires a `SessionStart` hook (matcher `startup`) into
  `~/.claude/settings.json` whose command is `uzi skill install --target
  claude`, so the skill is refreshed at session start rather than waiting for
  your next `uzi` command.
- **Codex CLI** — wires a `SessionStart` hook (matcher `startup|resume`) into
  `$CODEX_HOME/hooks.json` whose command is `uzi skill install --target
  codex`. Codex requires you to **review and trust the hook once via
  `/hooks`** before it runs — uzi never writes Codex trust state or
  `config.toml`, it only writes `hooks.json`.

The write is surgical and non-destructive, for either target:

- **Opt-in.** Nothing installs this for you; you run it yourself, once, per
  target.
- **Merged, not clobbered.** It adds just our one hook entry to the target's
  hook file, alongside any hooks other tools already put there — those are
  left untouched.
- **Backed up first.** The prior hook file is copied to a `.bak` sibling
  before the first write.
- **Abort on malformed JSON.** If the hook file exists but doesn't parse,
  `install-hook`/`uninstall-hook` refuse to touch it rather than risk
  clobbering a hand-maintained file.
- **Idempotent.** Running `uzi skill install-hook` again for the same
  target is a no-op — it detects the hook is already present.
- **Visible in status.** `uzi skill status` (and `--json`) reports every
  selected target, and per target whether its hook is installed and current,
  alongside the skill's own state.
- **Reversible.** `uzi skill uninstall-hook [--target ...]` removes the
  selected target's hook, leaving every sibling hook (and the other target's
  hook) intact.

The hook is best-effort and near-free to run: a failed refresh never blocks
session start, and `uzi skill install` is a version-gated no-op once the
skill is already current, so the hook costs almost nothing on a normal
session start.

**JSON output.** `uzi skill status|install|install-hook|uninstall-hook
--json` without `--target` keeps today's Claude-only top-level fields (for
compatibility with existing consumers) and adds a `targets` array covering
every attempted target; with `--target` (any value), the output is
`{targets}` only. `status` always reports every selected target, whichever
form the output takes.

## Product docs, offline: `uzi docs`

The CLI carries uzi's own conceptual and onboarding docs — the same pages the
web app renders at `/docs/:slug` — **embedded in the binary**, so you (or an
agent helping you get started) can read them with no server, no token, and no
network, exactly like `uzi version`. There is no `GET /api/docs` round-trip and
nothing to configure; the corpus ships inside `uzi` and is version-matched to
the release this binary was built from.

```sh
uzi docs list                       # the user-facing pages (slug · title · audience · order)
uzi docs list --audience all        # every page, incl. operator/design/contributor docs
uzi docs show getting-started       # print a page's markdown body
uzi docs search "connect a forge"   # find pages by a substring of the title or body
```

- **`list`** prints the docs for an audience, ordered the way the web index is.
  It defaults to `--audience user` (the onboarding-facing pages) and takes
  `--audience user|operator|design|contributor|all`. `--json` returns an array.
- **`show <slug>`** prints one page's raw markdown body (the slug is the
  filename without `.md`, e.g. `worker-setup`). An unknown slug exits 4 — with a
  "did you mean" suggestion when a similar slug exists; `--json` returns
  `{slug, meta, body}`.
- **`search <query>`** does a whole-query, case-insensitive substring match over
  every page's title and body (title matches rank first) and prints
  `slug · title · snippet`. It takes the same `--audience` filter and `--json`.
  This is the fastest way to find the page that answers a "how do I…" question.

All three read the embedded corpus directly, so they never contact a server —
they are exempt from the version-skew check for that reason, and work the same
whether or not a URL is configured. The single source of truth stays the
repo's `docs/` (see [the docs README](./README.md)); the CLI embeds a
drift-checked mirror, so a terminal answer and the in-app page cannot disagree.

## Config and credentials

`~/.config/uzi/config.toml` (URL, 0644) and `credentials.toml` (token, 0600 —
the CLI refuses to read it if it's group/world-readable). `$UZI_URL` and
`$UZI_TOKEN` override both files, which is why the headless path needs
neither.

⚠️ This path is fixed at `~/.config/uzi/` and does not honour
`$XDG_CONFIG_HOME` — deliberately: on at least one machine on this team that
variable points into a git-tracked, synced directory, and honouring it would
write a live token into version control.

### Named contexts

Both files hold a **map** of contexts, not a single slot — `config.toml` has
`[contexts.<name>]` (a URL) and `credentials.toml` has `[contexts.<name>]` (a
token), keyed by the same names. This is what lets the CLI hold several
credentials at once — say a `uzc_` owner token under `default` and a `uza_`
admin-read token under a second context named `admin` — instead of forcing you
to overwrite one with `uzi auth token` or juggle a `UZI_TOKEN=…` override per
invocation.

**Which context is active**, in order: the `--context`/`-c <name>` flag, then
`$UZI_CONTEXT`, then the sticky current context (set by `uzi context use`),
then `"default"`. An empty `--context`/`$UZI_CONTEXT` counts as unset. Only
after that is resolved do the per-invocation overrides from
[Authenticate](#3-authenticate) layer on top — `$UZI_TOKEN` still overrides
whatever token the context resolved to, and `$UZI_URL`/`--url` still override
the URL — so a headless job using plain `UZI_URL`+`UZI_TOKEN` behaves exactly
as before, whether or not contexts exist.

```
uzi context list                       # every stored context, its URL, token stored?, current
uzi context current                    # the sticky current context (or "default")
uzi context use <name>                 # set the sticky current context
uzi context set <name> --url <url>     # create/update a URL-only context
uzi context rm <name>                  # remove a context; resets current to "default" if it was current
```

`uzi auth token --context <name>` and `uzi login --context <name>` store the
credential under that context (an unknown name is **created** here — that's
the only way a context comes into being besides `context set`). `uzi auth
status --all` lists every stored context; a plain `uzi auth status` reports
just the active one. `uzi logout` removes only the active context's token,
leaving its URL in place.

**URL inheritance.** A context with no URL of its own inherits the `default`
context's URL (never its token), so the common case — two tokens against one
server — needs the URL stored just once, on `default`. **Multi-server
caution**: that inheritance is only right when every context talks to the same
server. A context aimed at a *different* server needs its own URL
(`uzi context set <name> --url <url>`) — otherwise it would send its token to
the wrong host.

**Security framing.** A context is pure client-side credential *selection* —
switching contexts never changes what a token can do. Authority is still the
token's server-enforced scope (a default `uzc_` token acts as your own user;
an admin-scoped `uza_` token is what `uzi admin …` needs — see
[Commands](#commands) above), so choosing the `admin` context above only works
because that token already carries `admin_ro`; a context never grants
capability it wasn't already given. The `0600` credentials-file rule and the
no-token-on-`argv` rule (above) hold for every context — they share the one
store.

## When your CLI is older than the server

A CLI that predates a server **silently drops response fields it does not know
about** — including under `--json`, where the field reads `null` while the
server holds a real value. That is a wrong answer rather than a missing
feature, and nothing used to say so. It happened for real: a `v0.11.8` binary
reported `anthropic_secret_label: null` for two runs whose labels the server
was serving perfectly well, and the web UI showed them correctly the whole time.

So every command now compares its own version against the server's and prints
one line to **stderr** when it is behind:

```
uzi: CLI v0.11.8 is behind server 0.14.0; some fields may be missing. Run: brew upgrade uzi-cli
```

A CLI owned by the RC formula names that formula even when it runs a stable
version. Ownership is resolved from the executable's Homebrew path without running
`brew`; if ownership is unknown, the stamped `-rc.N` suffix selects the RC remedy.
For example:

```
uzi: CLI v0.85.0-rc.2 is behind server 0.85.0-rc.3; some fields may be missing. Run: brew upgrade uzi-cli-rc
```

- **stderr, never stdout.** `--json` output stays byte-exact and parseable.
- **The exit code never changes.** A skew warning is not a failure.
- **Cached**, so it costs at most one short request per hour per server —
  recorded in `~/.config/uzi/version-check.json` (0644; it holds a version
  string and a hash of the server URL, no credentials). A failed probe is
  cached too, so an unreachable server does not slow every command down.
- **It clears the moment you upgrade.** The file stores the *server's* version,
  never a verdict, so the comparison is redone against your new binary on the
  very next command — there is no cache to wait out.
- **Silent when it cannot be sure.** A binary built from source normally reports
  `dev` rather than a release, and an unparseable version on either side means
  no warning at all. A manually stamped binary can also see this warning; its
  remedy follows its stamped release channel without probing Homebrew.
- **Not shown** when the CLI is *newer* than the server (nothing for you to do),
  under `--quiet`, or for `uzi logout`, `uzi auth token` and `uzi auth status`,
  which otherwise make no network call at all.

Set `UZI_VERSION_CHECK=0` to turn the check off entirely — for a test harness
that counts output lines, say. It is a poor substitute for upgrading.

## Managing tokens

> **A password change is NOT an incident-response control for CLI tokens. You
> must enumerate and revoke each one.**

If a laptop is lost, **Settings → Access → Revoke all** is the one-click
answer — it stops every `uzi` CLI and CI job using one of your tokens at
once. Since PRD #1907 it revokes your **product tokens** too (see below), in
the same step, so no CLI or product token of yours stays live (browser
sessions are not ended: sign out for that). If you'd rather keep some, the
token list gives you what you need to decide: `token_prefix`, `last_used_at`, and `last_used_ip`. Revoke anything
you don't recognise, and treat an unfamiliar `last_used_ip` as the signal to
revoke, not just a curiosity.

![Settings → Access, the CLI token list with the Revoke all button](img/cli-access-settings.png)

There is no per-request audit log for CLI tokens — `last_used_ip` (updated at
most once a minute) is the only detection control the design has, not a full
trail.

**Product tokens (`uzp_…`) are a separate credential.** A token you mint in
**Settings → Access → Product tokens** for an external product works only on
`/api/v1` and is not a CLI token: `uzi` cannot use one, so `UZI_TOKEN` must
hold a CLI token (`uzc_` or `uza_`), and the CLI refuses a `uzp_` value with an
error saying so. Like CLI tokens, product tokens are **not** revoked by a
password change or logout; Revoke all, revoking one token, an admin, disabling
or deleting the product, or deactivating the account does revoke them. Minting
and admin product management are browser-only; the CLI has just the read-only
`uzi admin products` (with a `JOB_TYPES` column showing the job types each product may start, and `CLIENT` and `SCOPES` columns for its [OAuth client](./oauth-clients.md) registration). See [Product tokens](./product-tokens.md).
