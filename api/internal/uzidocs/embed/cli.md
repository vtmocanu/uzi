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
brew trust vtmocanu/tap   # one-time: Homebrew 6+ requires trusting third-party taps
brew install vtmocanu/tap/uzi-cli
uzi version
```

`vtmocanu/tap` resolves to `github.com/vtmocanu/homebrew-tap` on its own (Homebrew maps
a bare `user/repo` tap name to `github.com/user/homebrew-repo`), so no explicit remote
is needed. `brew trust` is a one-time step: Homebrew 6+ refuses to load formulae from a
third-party tap until it is trusted.

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
                [--planned-commit <sha>] [--require-base]
uzi run approve <id> [--agent-source own|repo] [--exclude-agents a,b]
uzi run reject <id> [--message <text>]
uzi run revise <id> [--message <text>]
uzi run cancel <id>
uzi run stop <id> [--message <text>]
uzi run scope <id> --through <n>
uzi run follow-up <id> [--message <text>]
uzi run answer <id> [--message <text> ...]
uzi run inputs <id> [--json]
uzi run expedite <id> [--clear]
uzi schedule create --repo <id> [--repo <id> ...] (--issue <iid> | --sweep [--label <l> ...] [--create-missing-labels] | --prompt <text>)
                    (--at <rfc3339> | --cron <expr>) [--tz <iana>]
                    [--auto-approve[=false]] [--wait-on-limit[=false]]
                    [--max-issues <n>] [--guidance <text>]
                    [--model <alias|id>] [--apply-model-to-agents]
uzi schedule list | get <id> | pause <id> | resume <id> | run-now <id> | delete <id>
uzi schedule edit <id> [--cron <expr> | --at <rfc3339>] [--tz <iana>]
                   [--prompt <text> | --label <l> ... [--create-missing-labels]]
                   [--auto-approve[=false]] [--wait-on-limit[=false]]
                   [--guidance <text> | --clear-guidance]
                   [--max-issues <n> | --clear-max-issues]
                   [--model <alias|id>] [--apply-model-to-agents] [--repo <id>]
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
uzi findings list [--repo <id>] [--bucket to_file|filed|dismissed|all] [--run <id>]
uzi findings file <finding-id>
uzi findings dismiss <finding-id> --reason wont-do|not-an-issue
uzi handoff -m <text> | -f <path> [--base <ref>] [--mr] [--review] [--then-fix] [--interactive] [--repo <id>]
uzi handoff rm <run-id> | review <run-id>
uzi token list
uzi worker list | rm <id> | set-token <worker-id> <label> | set-token <worker-id> --default
uzi repo list | remove <id> [--force]
uzi project-sync status <repo> | resync <repo>
uzi admin users | runs | workers | usage | rate-limits | cli-tokens | guardrail-impact | blocked-repos
uzi admin agent-source get | status
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
- **No `worker create` and no `admin` writes.** Minting a worker join token
  returns a credential that reads decrypted secrets, and every admin write
  stays cookie-only — both are web UI actions by design.
- **`token` is list-only, and `worker set-token` is the one write near it.**
  See [Anthropic tokens](#anthropic-tokens) below for why the split falls
  exactly there.
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
- **`run approve` picks the subagent roster explicitly.** By default a run
  uses its own default roster; `--agent-source own|repo` overrides it
  (`own` = your template roster, `repo` = the agents the worker detected in
  the clone's `.claude/agents/`), and `--exclude-agents a,b` drops individual
  subagents from that source. `--exclude-agents` requires `--agent-source`.
  `run create --plan-file` takes the same two flags, for the seeded run's
  roster.
- **`run revise <id> -m "<feedback>"`** steers a plan at the approval gate
  without stopping the run: the agent re-plans from your feedback and returns
  to `awaiting_approval` for another decision, where `run reject` instead ends
  the run. Use it on a run parked at the gate; it needs a non-empty message
  (`-m`/`--message`, or piped on stdin). Revisions are capped by the run's
  revision limit, and an exhausted limit — or a run that has already finished —
  is a 409 (exit 5).
- **`run stop <id>`** gracefully winds down a run. On an **interactive**
  task run (one created with `uzi handoff --interactive`; see [Interactive
  mode](./handoff.md#interactive-mode)) the current turn
  finishes, the branch is pushed, an MR opens iff `--mr` was set at handoff,
  and (via the server's `completed` transition) `--review` fires iff it was
  requested, then the run lands `completed` with a distinct stop disposition.
  Unlike `run cancel`, which aborts mid-turn, `stop` never discards in-flight
  work. On a **milestone-structured issue run** (PRD #634), `stop` instead
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
  so the web renders them as one expandable group; the rows stay fully independent
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
  over its size limit. `--model <alias|id>` (valid on every target) pins the
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
  reached nobody, a hint to raise `--max-issues` or add the `uzi` label; a
  never-fired schedule reads `Last fire: never fired`, and `--json` carries the
  same detail under `.last_fire`. `run-now` prints the matching per-candidate
  breakdown inline — a `Started N run(s)` header with the created run id(s), a
  line per started run, then a `Matched N candidate(s), skipped K:` tally with a
  reason label per skip (and a `# add the uzi label, or raise
  --max-issues` hint for `not eligible`) — or `no run started` when a benign
  dedup fired none; `--json` dumps the raw response (`created`, `run_ids`,
  `matched`, `capped`, `started`, `skips`).
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
- **`schedule reset <id>`** restores a **default** schedule's edited fields (cron,
  timezone, model, apply-model-to-agents, auto-approve, wait-on-limit,
  max-issues) to the builtin catalog values — `apply-model-to-agents` resets to
  `false` — and clears its customized flag. Only a default-origin schedule can be
  reset; a user-origin one is a `409`.
- **`schedule clone <id> [--repo <id>]`** copies a schedule into a new, fully
  editable schedule you own. Cloning a **default** schedule lifts its catalog
  prompt lock — the baked prompt (or a sweep's labels and guidance) is copied into
  the new row, which becomes an ordinary user schedule you can edit. Pass `--repo`
  to clone into a **different** repo you own (the replication path); omit it to
  clone into the source schedule's own repo.
- **`schedule add-repo <id> --repo <id>`** replicates an existing schedule you own
  onto **another** repo you own as a new **grouped sibling** — the new row is an
  independent, fully-editable copy of the source's current config, and both the source
  and the new row carry one shared **display-only** group id so they render as one
  expandable group (the CLI twin of the web "Add another repo" action). `--repo` is
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
  milestone-structured issue run): a follow-up's `STATE` is a delivery state
  (queued/delivered, as before); a scope directive's `STATE` is its
  **disposition** instead — `active (scope ceiling set)` while pending,
  `applied (finalized at the ceiling)`, `declined (not acted on)`, or
  `superseded (a later directive replaced it)` — because a scope row is
  never consumed; its state *is* the disposition. **Chat caveat**: a chat
  run seeds every turn as a follow-up row, so `run inputs` against a chat
  run lists the whole conversation, not just steering messages; an issue or
  CI-fix run's queue starts empty and only ever holds what you actually sent
  mid-run.
- **`run expedite <id>`** bumps a **queued** run to the front of the claim
  queue, so a worker picks it up ahead of the rest. It only matters before a
  run is claimed — ordering is fixed once a worker takes it — so a non-queued
  run is a conflict (exit 5) and a foreign/unknown run is not found (exit 4).
  `--clear` undoes the bump: it removes the manual override and returns the run
  to its kind default priority (it does **not** demote it below normal). It
  prints the updated run; `--json` emits the run object, whose `priority` reads
  `expedited` after a bump.
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
- **`admin` needs an admin-scoped token.** A default (`uzc_`) token gets
  exit 3 with an actionable message; mint an `admin_ro` (`uza_`) token in
  Settings → Access to use it. `uzi whoami` over a `uzc_` token reports
  `is_admin: false` even for an admin — that's the credential's own
  authority, not your résumé.
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
   content, so nothing will claim it — and the error tells you to clean it up
   with `uzi handoff rm <id>`.

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
  terminal, and the full breakdown is on the run detail view. On your own
  board only, a right-aligned **COST** cell follows the micro-bar: `$N` in
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
  span (`lo–hi`) and a rounded floor total shown in the top-right summary
  cluster (`⚑ N · ✎ N · ▲ N · $N · T runs`) — the total is the sum of every
  usage-bearing run's cost, rounded from the raw sum rather than added up
  from the visible per-row cells (so the two won't always visibly agree —
  the total is the accurate figure), and it's dropped when zero. The whole
  board refreshes on the poll, so status, health, milestones, age, cost and
  the judge verdict stay live.
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
  milestone in order, marked `✓` reported complete, `◐` in progress, or `○`
  not started. The count reads "reported complete", not verified: uzi shows
  what the run reported and does not itself check the work. Directly above
  the ACCOUNTS block, a **SPEND** block shows the run's total cost (same
  cents formatting) over a token breakdown — `in` (input plus
  cache-creation tokens), `out` (output tokens), and a `cache` line with the
  cached-read token count and its share of the total input (`in` + `cache`)
  as a percentage; it's
  omitted for a run with no recorded usage.
- **Review overlay** (`[v]` from run detail). The judge's verdict, summary,
  and recommendations, with the same resolve/dismiss/undo triage described
  under [Reviewing and triaging from the CLI](#reviewing-and-triaging-from-the-cli).

### Keybindings

```
←/→, h/l, tab detail: focus the crew rail / the transcript (h/← rail, l/→ transcript; tab cycles). Detail opens focused on the crew rail.
j/k, ↑/↓     move within the focused pane (board: row · detail: between agents on the rail, or scroll the transcript)
g            detail: follow live — re-attach and jump to the newest output (live runs only)
enter        open the selected run (board)
/            filter the board
a            toggle the factory-wide admin board (board only)
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

### Steering is run-level, not per-agent

The steer bar sends `follow_up`, `approve_plan`, `reject_plan`, or `cancel`
to the **run** — there's no wire to whisper to one live subagent. The lane
rail is where you *watch* per-agent activity; the run (and its lead, who
then directs its own subagents) is what you *steer*. A queued/delivered
indicator above the bar reflects the same steer queue `uzi run inputs`
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
deduped by `(category, target)`, so one that recurs in five runs is a single
row reading `seen in 5 runs` — the terminal form of the
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
uzi findings list                                            # what still needs filing
uzi findings list --bucket all --json                        # filed + dismissed too, for an agent
uzi findings list --repo <repo-id>                           # one repo
uzi findings list --run <run-id>                             # coordinates that also occur in that run
uzi findings file <finding-id>                               # file a forge issue from a coordinate
uzi findings dismiss <finding-id> --reason wont-do           # valid, not worth doing
uzi findings dismiss <finding-id> --reason not-an-issue      # false positive
```

`list` prints one row per `(repo, location)` coordinate, grouped by repo, carrying
the actionable `finding_id`, the latest title, `seen in N runs`, and a status; the
`open_count` (what still needs filing) prints as a meta line and rides the `--json`
envelope. `--bucket` filters by disposition and defaults to `to_file`; `filed`,
`dismissed` and `all` show the settled coordinates. `--repo <repo-id>` (from
`uzi repo list`) and `--run <run-id>` narrow the list. As with `review backlog`, an
unknown `--bucket` is a usage error (exit 2), never a silently empty list, while a
well-formed but foreign or unknown `--repo`/`--run` returns an **empty list** — no
existence oracle — rather than a 404.

`file` turns one coordinate into a real forge issue on **your own** connection. The
title, description and labels are assembled server-side from the stored, sanitised
finding plus a mandatory marker label, so the CLI files the defaults — editing the
draft before filing is a web action. It prints the created issue's number and URL;
`--json` returns `{issue:{iid,web_url,title}, warning?}`, where a `warning` means the
issue was created but its local record could not settle (a success with a note, still
exit 0), not a retry signal. Filing a coordinate that is already filed or mid-filing
is a conflict (exit 5); an unknown or foreign `<finding-id>` is not-found (exit 4).

`dismiss` triages a coordinate to `dismissed` so it stays gone and does not re-nag
across later runs (`not-an-issue` is a false positive, `wont-do` is valid-but-skip).
A missing or invalid `--reason` is a usage error (exit 2) raised before any request is
sent; a coordinate that is not dismissable (already filed, being filed, or already
dismissed) is a conflict (exit 5), and an unknown or foreign id is not-found (exit 4).

`<finding-id>` is the id `list` prints as the first column of each coordinate; paste
it straight into `file`/`dismiss`. Treat `location`, the title and `repo_path` as
untrusted free text (they are agent-authored): render them as data, and branch only
on the `status`/`bucket` enums.

## Anthropic tokens

You can hold several named [Anthropic credentials](./anthropic-token.md) and
point individual workers at them. The CLI can **read** that set and **move a
worker between its members** — it cannot change the set itself:

```sh
uzi token list                                 # labels, default flag, pool opt-in, live eligibility
uzi token pool console-key --on                # add it to the auto-selection pool
uzi token pool console-key --off               # take it back out
uzi worker set-token <worker-id> console-key   # bind a worker to a named token
uzi worker set-token <worker-id> --default     # clear the binding
uzi worker set-token <worker-id> --auto        # pick per claim, from the pool
```

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

`ELIGIBLE` is `eligible` when it can, or `no_reading` / `unmeasured` /
`stale` / `below_threshold` when it cannot; `-` when the token is not pooled
(the `POOL` column beside it already says so), and `?` when the eligibility
read failed. **Check it after opting a token in**: a token uzi has never
managed to poll reads as not eligible for normal ranking while looking active —
though if it is pooled, the last-resort floor can still spend it when nothing
pooled has a usable reading.

Under `--json` the same answer is the `auto_status` field. It is always
present and is **`null` when it is not known** — which is not the same as
"not eligible", so branch on null before you branch on the value. An
un-pooled token reports `not_pooled` there rather than the table's `-`.

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
`usage`), which you read with `--json` — is a usage error (exit 2). `--field` and
`--json` are mutually exclusive (two output modes).

The model a schedule froze onto a run is readable this way too:
`uzi run get <id> --field model` (the model alias/id, an empty line when the
schedule pinned none) and `uzi run get <id> --field override_subagent_model`
(the boolean literal `true`/`false` — whether that model was also applied to
every subagent).

A run's PRD-completion declaration is readable the same way:
`uzi run get <id> --field prd_done_path` (the repo-relative path the run
declared it moved a completed PRD to, e.g. `prds/done/72-x.md`) and <!-- check-docs:ignore-path: didactic example path, not a real PRD -->
`uzi run get <id> --field prd_patch_settled_at` (an RFC3339 timestamp once
the PRD-link patch lifecycle has settled, an empty line while still
pending). Both are emit-only-when-set on the human view too — `run get`
prints them as `PRD_MOVE` and `PRD_PATCH_SETTLED_AT` rows only when the run
has declared a move — and appear the same way under `--json`.

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

A run's `status` (on `run get` and `run list`) is one of exactly **ten** values:
`queued`, `claimed`, `running`, `awaiting_approval`, `awaiting_input`,
`awaiting_followup`, `limit_wait`, `completed`, `failed`, `cancelled`. Only the
last three are **terminal**, and `uzi run logs --follow` returns **only** on
those three. The four non-terminal parks it will *not* stop at:

- `awaiting_approval` — the plan gate;
- `awaiting_input` — a clarifying question, answered with `run answer`;
- `awaiting_followup` — an interactive task (`uzi handoff --interactive`)
  parked after a clean `signal_done`, awaiting your next `run follow-up`; it
  does **not** auto-resume on its own — wind it down explicitly with `run
  stop`, or let its worker-side idle timeout finalize it — see [Interactive
  mode](./handoff.md#interactive-mode);
- `limit_wait` — parked while an Anthropic usage limit resets, promoted back to
  `queued` once past its `retry_not_before`.

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
It's still the same `running` value underneath, not an eleventh status.

### Waiting for a state: `uzi run wait`

`uzi run wait <id>` blocks until the run reaches a state you can act on — the
built-in primitive for driving a gated run headless, replacing the hand-rolled
`while … run get … sleep` poll loop. With no `--until` it stops on any
**actionable or terminal** state (`awaiting_approval`, `awaiting_input`,
`awaiting_followup`, `completed`, `failed`, `cancelled`) and waits through the
rest (`queued`/`claimed`/`running`/`limit_wait`), so a bare `run wait` means
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
- `--until <a,b>` overrides the stop set, validated against the ten statuses.
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

**The skill itself.** The CLI installs (and self-upgrades)
`~/.claude/skills/uzi-cli/SKILL.md` on first run, generated from the binary's
own command tree — it never drifts from the CLI you actually have installed.
Every `uzi` command refreshes it best-effort before it runs (set
`UZI_SKILL_AUTO_UPGRADE=0` to disable that). `uzi skill install [--force]`
refreshes it explicitly — `--force` overwrites even a file you edited (your
edit is preserved to `SKILL.md.bak` first) — and `uzi skill status` reports
its path and whether it's installed and current.

**The session-start hook, opt-in.** The per-command refresh above only helps
once a `uzi` command has run — right after `brew upgrade uzi-cli`, a fresh
Claude Code session can still read the OLD skill before that happens. Run
`uzi skill install-hook` to narrow that window: it wires a Claude Code
`SessionStart` hook into `~/.claude/settings.json` whose command is
`uzi skill install`, so the skill is refreshed at session start rather than
waiting for your next `uzi` command.

The write is surgical and non-destructive:

- **Opt-in.** Nothing installs this for you; you run it yourself, once.
- **Merged, not clobbered.** It adds just our one hook entry to
  `~/.claude/settings.json`, alongside any hooks other tools already put
  there — those are left untouched.
- **Backed up first.** The prior file is copied to `settings.json.bak` before
  the first write.
- **Abort on malformed JSON.** If `settings.json` exists but doesn't parse,
  `install-hook`/`uninstall-hook` refuse to touch it rather than risk
  clobbering a hand-maintained file.
- **Idempotent.** Running `uzi skill install-hook` again is a no-op — it
  detects the hook is already present.
- **Visible in status.** `uzi skill status` (and `--json`) reports whether
  the hook is installed and current, alongside the skill's own state.
- **Reversible.** `uzi skill uninstall-hook` removes it, leaving every
  sibling hook intact.

The hook is best-effort and near-free to run: a failed refresh never blocks
session start, and `uzi skill install` is a version-gated no-op once the
skill is already current, so the hook costs almost nothing on a normal
session start.

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
  filename without `.md`, e.g. `worker-setup`). An unknown slug exits 4 with a
  "did you mean" suggestion; `--json` returns `{slug, meta, body}`.
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

- **stderr, never stdout.** `--json` output stays byte-exact and parseable.
- **The exit code never changes.** A skew warning is not a failure.
- **Cached**, so it costs at most one short request per hour per server —
  recorded in `~/.config/uzi/version-check.json` (0644; it holds a version
  string and a hash of the server URL, no credentials). A failed probe is
  cached too, so an unreachable server does not slow every command down.
- **It clears the moment you upgrade.** The file stores the *server's* version,
  never a verdict, so the comparison is redone against your new binary on the
  very next command — there is no cache to wait out.
- **Silent when it cannot be sure.** A binary built from source reports `dev`
  rather than a release, and an unparseable version on either side means no
  warning at all. That also means the remedy is always the right one: only a
  `brew`-installed CLI can ever see this message.
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
once. If you'd rather keep some, the token list gives you what you need to
decide: `token_prefix`, `last_used_at`, and `last_used_ip`. Revoke anything
you don't recognise, and treat an unfamiliar `last_used_ip` as the signal to
revoke, not just a curiosity.

![Settings → Access, the CLI token list with the Revoke all button](img/cli-access-settings.png)

There is no per-request audit log for CLI tokens — `last_used_ip` (updated at
most once a minute) is the only detection control the design has, not a full
trail.
