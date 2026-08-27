# PRD #212: Surface plan-turn worktree changes at the approval gate

**GitHub Issue**: [vtmocanu/uzi#212](https://github.com/vtmocanu/uzi/issues/212)
**Status**: Complete (2026-08-23) — all milestones landed on `agent/issue-212`
**Priority**: High
**Created**: 2026-08-22
**Scope**: agent (worker) + api + DB migration + web + CLI. **No** forge-driver change,
**no** new HTTP route, **no** `.github/workflows/**` change.
**Anchors**: all file:line references below are against `main` at **`335136c0a`**.
Re-derive them at implementation time (`grep -n` the quoted symbol/string) rather
than trusting the numbers — these files change often. Every symbol name is stable;
the line numbers are the perishable part.

## Problem

uzi's primary directive is that a human approves exactly what an agent is about to
do. The approval gate delivers on that by showing the human the written plan
(`plan_md`) and **nothing else**. But the plan turn runs under
`permissionMode: "bypassPermissions"` with full `Bash`, and `agent/src/guardrails.ts`
has **no filesystem-write rule of any kind** — the guardrails comment says so
itself and names this issue as the fix (`agent/src/guardrails.ts:122-128`):

> "…this file has NO filesystem-write rule of any kind — `echo 'x' > f`, `sed -i`,
> `tee` and `git apply` are all allowed. … The integrity property (a `git status`
> check at the gate, which catches an `Edit` and an `echo >` identically) is issue #212."

So during the plan turn an agent (the lead or any subagent) can write files into the
runner worktree. Those writes are visible as tool-call rows on the run feed, but they
are **never surfaced as a change at the approval gate**, and the first implement
commit typically sweeps them in (`git.ts` topology: the worker "NEVER runs
`worktree add`/checkout, `status`, or a content diff", `agent/src/git.ts:38-39`;
nothing resets the tree between phases). The human approves what looks like "just a
plan" and gets uncommitted planning-turn edits bundled into the branch unseen.

Nothing today prevents **or reveals** this. This PRD makes it **visible at the moment
of approval**. That is the integrity property the gate actually needs and, per the
issue's own analysis (and #203's design wave), the property **no tool allowlist can
deliver** — `Bash` stays granted, and the write surface (`echo >`, `sed -i`, `tee`,
`git apply`, `python3 -c open(...,'w')`, even the SDK `REPL`/`NotebookEdit` tools) is
not enumerable from templates. A gate-side `git status` catches every write path
identically.

## Solution Overview

Immediately before the `awaiting_approval` gate report is sent, run
`git status --porcelain` **in the runner clone, as the runner uid**, and carry the
resulting changed-file list as a new additive-optional field on the report. The api
persists it on the `runs` row and returns it on the run-detail response, exactly like
`plan_md`/`required_tools` do today. The web approval panel and the `uzi run get` CLI
each render a "Files changed during planning" section beside the plan.

This is **surface-only**: it makes the writes visible, it does not prevent them and
does not block approval. (Block-on-dirty is a deliberate, separate future decision —
see Non-goals.)

The new wire field is named **`plan_changed_files`** everywhere (JSON key),
`PlanChangedFiles` (Go), `plan_changed_files` (TS). It is a `[]string` of
`git status --porcelain` lines (status code + path, e.g. ` M src/app.ts`,
`?? notes.md`, `A  x.go`), sanitized and capped.

### The load-bearing safety constraint (do not get this wrong)

`git status` touches the working tree, which can fire `.gitattributes`
`filter.<name>.clean` / `diff.<name>.command` drivers whose command is
attacker-chosen and **cannot be blanket-pinned** (`agent/src/git.ts:97-105`). Those
drivers execute as **whatever uid runs the command**. The whole PRD #51 M0
write→worker-execute vector is: a process that writes a config source a
**worker-uid** (PAT holder) git reads can get code-exec as the PAT holder. Today the
worker never checks out or runs `status`, so those drivers never reach it
(`git.ts:28-34`, `git.ts:97-105`).

Therefore the gate `git status` **MUST run as the runner uid**, via the existing
`runGitAsRunner` primitive (`agent/src/git.ts:1219-1236`, which reuids to the cap-less
`runner` user through `setpriv`). A runner-uid `git status` in the runner-owned clone
is "the untrusted uid exec'ing in its OWN tree — not a boundary crossing"
(`git.ts:100`). A **worker-uid** `git status` in that clone would make that sentence
false and re-open code-exec as the PAT holder. This is the single most important
constraint in the PRD; every reviewer and the implementer must confirm it.

## Design Decisions

1. **Ride the authoritative `reportState` report, not the `onAwaitingApproval`
   side-channel.** The changed-file list is integrity data the human must see, so it
   belongs on the bounded-retry state report that already persists
   `plan_md`/`milestones`/toolchain fields (`agent/src/runner.ts:2438-2446`), mirroring
   the `milestones` (`:2441`) and `toolchainReportFields` (`:2445`) additive-optional
   precedent. The `onAwaitingApproval` plan-summary hook (`runner.ts:2455-2470`) is a
   best-effort side POST explicitly designed to be silently droppable and only carries
   `planMd` — wrong channel. Leave it untouched.

2. **Thread the runner clone path into `gatePlan`.** `gatePlan`
   (`agent/src/runner.ts:2345`) does **not** currently receive the clone path. Add a
   parameter and pass `worktreePath` from the `RunContext.gatePlan` closure
   (`runner.ts:894-906`), where it is in scope as `worktreePath` (= `runnerClone.path`,
   assigned `runner.ts:553`, also passed into RunContext at `runner.ts:811`).

   🔴 **The status call MUST be best-effort — a visibility feature may never block the
   gate on its own failure.** `runGitAsRunner` throws on non-zero exit and on the 10-min
   `GIT_TIMEOUT_MS` (`git.ts:1226-1235`), and the `awaiting_approval` `reportState` at
   `runner.ts:2438` is `await`ed with **no** `.catch`, so a throwing `git status` (a
   benign git error, or a pathological/hostile tree that hangs) would propagate and
   **abort the run instead of parking it at the gate**. Wrap the call `try/catch → []`,
   matching every sibling git helper in this file (`changedFiles` returns `null` on
   error at `git.ts:788`, `commitsAheadOfDefault` returns `0`, `trackingTip` returns
   `null`) and the api side's "additive-optional, never fails the report" contract. A
   failed status renders as "no changes" — acceptable; blocking the gate is not.

3. **Always send the field on the awaiting_approval report (empty array when clean),
   NOT conditionally-spread like `milestones`.** This is the one place the naive
   "match the `milestones` precedent" is wrong. A run can revise (gate reopens for a
   round 2). Nothing resets the worktree between rounds, and the agent could revert a
   plan-turn write between rounds; a conditional spread + COALESCE-preserve would then
   show a **stale** round-1 file list at the round-2 gate. Sending the current list
   every round (empty when clean) makes each gate reflect that round's actual tree.
   Concretely: the worker includes `plan_changed_files: [...]` on **every**
   awaiting_approval report; `[]` when the plan turn wrote nothing.

4. **Back-compat via the pointer/COALESCE tri-state — model on `repo_agents`, NOT
   `required_tools`.** On ingest, `StateRequest.PlanChangedFiles` is a `*[]string`
   (`api/internal/workersvc/service.go`). A **pre-#212 worker** never sends the key →
   nil pointer → the SQL `COALESCE(sqlc.narg('plan_changed_files')::text[],
   plan_changed_files)` preserves the column (stays NULL). A **#212 worker** always
   sends a non-null array (possibly empty `{}`) → COALESCE picks it → the column is
   replaced with the current round's set. So COALESCE gives *both* back-compat (nil
   preserves) *and* per-round replace (empty-but-present replaces), because an empty SQL
   array is not NULL — the codebase says so itself: "a non-nil empty slice (`{}`) would
   WIPE a prior tool set" (`service.go:3387`).

   🔴 **The only StateRequest field with the semantics we need is `repo_agents`, and
   the two fields the earlier draft pointed at (`required_tools` /
   `inferredRequirementParams`, `preserved_patch` / `clampWirePreservedPatch`) do the
   OPPOSITE.** Both of those collapse an empty result to nil/NULL on purpose (a
   `len(filtered) > 0` guard at `service.go:3396-3400`; "a value that sanitizes to empty
   maps to NULL" at `:5566-5578`), so COALESCE **preserves** — which would reintroduce
   the stale-list bug on the revert-between-rounds path. `repo_agents` is the field that
   already implements "present-`[]` replaces / absent preserves": Go builds the param
   whenever the pointer is non-nil with **no `len>0` collapse** (`if req.RepoAgents !=
   nil { … }`, `service.go:3437-3446`); SQL is `repo_agents =
   COALESCE(sqlc.narg('repo_agents')::jsonb, repo_agents)` (`runtime.sql:831`); the wire
   note is "always sent once detection ran, possibly `[]`; an absent field/NULL column
   means a pre-feature run." Cite `repo_agents` for both the Go ingest shape and the SQL
   shape. Use the per-element sanitizer (Decision 5) **only** for char/length clamping of
   each line — never let it collapse the whole list to nil when it happens to be empty.

5. **Sanitize and cap the list server-side; the worker text is untrusted.** Paths
   originate in a cloned repo an attacker may control. Apply a per-line control-char
   strip + length clamp (use the bounded-string sanitizer `termsafe.SanitizeBounded`,
   the char/length primitive `clampWirePreservedPatch` itself calls — take the clamp,
   **not** its empty→NULL collapse, per Decision 4) and a max element count with a
   truncation marker (e.g. cap 200 lines; if exceeded, keep the first N and append one
   `… (+K more)` marker line). The marker is a **synthetic non-porcelain line**, so any
   renderer that keys on porcelain status codes (` M `, `??`, `A `) must tolerate a line
   that has none — display it verbatim, don't parse it. The web/CLI renderers
   additionally route every path through their existing TTY/DOM sanitizers
   (`sanitizeTTY`/`cellText` in the CLI, `stripUnsafeChars`-style handling in web) —
   never print a raw repo-controlled path.

6. **`git status --porcelain` (v1, the stable default) honors `.gitignore`.** Without
   `--ignored`, gitignored paths (node_modules, build output) are excluded, so the list
   is bounded to real tracked-modifications + non-ignored untracked files — exactly the
   plan-turn writes worth surfacing. Do **not** pass `--ignored`. **Scope caveat:** the
   visibility guarantee therefore covers non-ignored paths only — a plan turn could
   write to an already-ignored path and stay off the list. This is largely
   self-backstopping (an ignored file is also not swept into the branch commit, and
   editing `.gitignore` itself surfaces as ` M .gitignore`), so it is not a blocker, but
   state the guarantee as "non-ignored paths," not "all writes."

7. **`text[]` column, storing porcelain lines (status + path), not bare paths.** The
   status code (` M`, `??`, `A `, `R  old -> new`) tells the human *how* a file
   changed, which is the useful signal at a gate. Model the column on `ci_config_paths`
   (`api/internal/store/migrations/00116_run_ci_config_paths.sql`), a `text[]`.

8. **Response field emits `[]`, never `omitempty`.** Follow `required_tools`
   (`apitypes/run.go:385`, mapped via `capsOrEmpty`), not the pointer `plan_md`. A
   pre-feature run's NULL column maps to `[]`; renderers show the section only when the
   list is non-empty, so a clean plan turn and an old run both render nothing. This also
   means the wire-key pin test must gain the new key (`apitypes/wire_test.go:69`
   `runDTOKeys`) — a useful forcing function.

9. **Web: gate panel only, not the seeded read-only twin.** Render the section in
   `PlanPanel` (`web/src/pages/RunView.tsx:1246`, beside the `plan_md` block at
   `:1542-1548`). Do **NOT** add it to `SeededPlanPanel` (`RunView.tsx:1637`): the
   seeded path has a human-authored plan and no adversarial plan turn, so the property
   is not meaningful there. Explicit decision, called out so a reviewer does not file
   the omission as a gap.

10. **CLI: a new rendered block (there is nothing to hang it beside).** Unlike the web
    gate, `renderRunDetail` (`api/cmd/uzi/run.go:1234`) does **not** print the plan body
    today — only the plan *summary*. So "surface at the gate" in the CLI means a new
    block, modeled on the below-the-table `ReportMd` free-text tail
    (`run.go:1348-1359`). The array cannot ride `--field` (scalar-only, rejected at
    `run.go:1204`); `--json` carries it, and `uzi run get --field plan_changed_files`
    intentionally errors toward `--json` (existing contract,
    `run_field_test.go:145`).

11. **No forge change.** `git status --porcelain` is worker-local worktree state
    shipped as report data (like `plan_md`/`preserved_patch`); the `Forge` interface
    (`api/internal/forge/forge.go:375`) is untouched, so the gitlab/github/forgejo
    drivers and their six test fakes are out of scope.

## Implementation anchors (current `main` @ `335136c0a`)

### A. agent worker (`agent/`)

- Wire type: add `plan_changed_files?: string[]` to `StateRequest` in
  `agent/src/protocol.ts` (near `milestones?` / `required_tools?`, ~`:1343`), with the
  "OMITTED when the worker predates the feature; `[]` when the plan turn was clean"
  note.
- Gate call site: `agent/src/runner.ts:2438-2446` (`reportState({ status:
  "awaiting_approval", ... })`) inside `gatePlan` (`runner.ts:2345`). Add
  `plan_changed_files: changed` to the object (always present, per Decision 3).
- New status call: just before `:2438`, `const changed = await
  this.runGitAsRunner(worktreePath, ["status", "--porcelain"])` then split/trim/cap
  into `string[]`. `runGitAsRunner` returns stdout as a string
  (`agent/src/git.ts:1219-1236`). Note there is **no** existing `git status` via this
  primitive today — this is a genuinely new call.
- Thread the path: add a `worktreePath: string` param to `this.gatePlan`
  (`runner.ts:2345-2369`) and pass it from the closure at `runner.ts:894-906`.
- Do not touch `onAwaitingApproval` (`runner.ts:2455-2470`).

### B. api + DB (`api/`)

1. **Migration** (assign the next free prefix above the live head at merge — head is
   `00149_github_project_link_seeding.sql` today, so draft as `00150_*`; numbers are
   assigned at merge time and `gate:repo`'s `check:migration-numbering` enforces
   uniqueness): `ALTER TABLE runs ADD COLUMN plan_changed_files text[];` — model on
   `api/internal/store/migrations/00116_run_ci_config_paths.sql`, which the implementer
   must copy **including its `-- +goose Down … DROP COLUMN plan_changed_files;`** clause
   (the `text[]` model file carries a Down; don't ship Up-only).
2. **SQL**: in `SetRunAwaitingApproval`
   (`api/internal/store/queries/runtime.sql:1004`) add `plan_changed_files =
   COALESCE(sqlc.narg('plan_changed_files')::text[], plan_changed_files)` (absent-safe,
   model on `required_tools` at `:1070`). Run `sqlc generate` (regenerates
   `SetRunAwaitingApprovalParams` in `runtime.sql.go` and the `store.Run` model). The
   two run-detail loaders `GetRunByID`/`GetRunByIDForUser` are `SELECT *`
   (`runtime.sql:400`, `:377`), so the new column auto-flows into `store.Run` — no
   query edit there.
3. **Ingest DTO**: add `PlanChangedFiles *[]string \`json:"plan_changed_files"\`` to
   `StateRequest` (`api/internal/workersvc/service.go:2998`, near `PlanMd` at `:3000` /
   `RequiredTools` at `:3121`).
4. **Ingest arm**: in the `case "awaiting_approval":` arm (`service.go:3174-3195`),
   build the param with the **`repo_agents` shape** (`if req.PlanChangedFiles != nil {
   … }`, no `len>0` collapse — see Decision 4; model on `service.go:3437-3446`), apply
   the per-element char/length clamp from Decision 5, and set it on
   `SetRunAwaitingApprovalParams` beside `PlanMd` (`:3189`). Rejected/oversized input is
   clamped, never fails the report (the "additive-optional, never fails the report"
   contract, `:3424-3427`). **Do NOT** model the ingest on `inferredRequirementParams`
   (`:3392`) or `clampWirePreservedPatch` (`:5573`) — both collapse empty→NULL and would
   break the empty-clears semantics (Decision 4).
5. **Response DTO**: add `PlanChangedFiles []string \`json:"plan_changed_files"\``
   (no omitempty) to `apitypes.RunDTO` (`api/internal/apitypes/run.go`, near `PlanMd`
   at `:168` / `RequiredTools` at `:385`). `RunListItemDTO` embeds `RunDTO`
   (`run.go:392`) so it inherits automatically.
6. **Wire-key pin test**: add `"plan_changed_files"` to `runDTOKeys`
   (`api/internal/apitypes/wire_test.go:69`), or `TestRunDTOTags` (`:159`) fails.
7. **Mapper**: add `PlanChangedFiles: capsOrEmpty(r.PlanChangedFiles)` to
   `runToDTO` (`api/internal/handler/workers.go:358`) — beside the other `capsOrEmpty`
   `text[]` mappings, grouped at `workers.go:428-429` (`RequiredTools`/`RequiredCapabilities`),
   not next to `PlanMd`; `capsOrEmpty` at `api/internal/handler/forge.go:148`.
   **`runToDTO` is the single chokepoint for every run-detail response** — GetRun, the
   run list (`RunListItemDTO{RunDTO: runToDTO(...)}`), chat, ci_fix, judge-accept,
   priority, waitonlimit all route through it, so this one line covers every consumer
   (they harmlessly carry the field as `[]`). `board.go`'s `latestRunDTO` and
   `judge.go`'s `JudgeRunDTO` are separate DTOs that do not embed `RunDTO` and correctly
   need no change.

### C. web (`web/`)

- Type: add `plan_changed_files?: string[];` to `interface Run`
  (`web/src/lib/api.ts:1318`, after `plan_md` at `:1422`; optional `?` for
  api-pod rollout skew, per the `plan_source` convention). `RunListItem extends Run`
  (`api.ts:1601`) inherits it.
- Render: a "Files changed during planning" section in `PlanPanel`
  (`web/src/pages/RunView.tsx:1246`), inserted after the `plan_md` block
  (`:1542-1548`, before the request-changes composer at `:1550`). Render **only when
  the list is non-empty**. Sanitize each path for display.
- Do NOT touch `SeededPlanPanel` (`RunView.tsx:1637`) — Decision 9.

### D. CLI (`api/cmd/uzi/`)

- A new block in `renderRunDetail` (`api/cmd/uzi/run.go:1234`), modeled on the
  `ReportMd` below-the-table tail (`run.go:1348-1359`); render only when non-empty;
  route each path through `sanitizeTTY`/`cellText`. No `--field` enum edit (derived);
  the array intentionally errors toward `--json`.

## Milestones

Ordering note: **M1 fixes the wire contract** (field name, shape, SQL semantics).
**M2 (agent) and M3 (web) parallelize with M1** — each declares its own field in its own
module (`protocol.ts`, web `interface Run`), so they need only the pinned *name*, not
M1's code; end-to-end verification of M3 needs M1 landed. **M4 (CLI) must FOLLOW M1, not
parallelize with it** — the CLI is in the **same Go module** as M1, and its
`render_test.go` renders an `apitypes.RunDTO` with `PlanChangedFiles` populated, so the
branch does not **compile** until M1's `RunDTO` field (B.5) exists. A parallel M4 would
either fail to build or duplicate-and-conflict M1's DTO edit. For a single sweep worker
doing the whole branch, do them in order M1 → M2 → M3 → M4 → M5.

- [x] **M1 — api + DB carry `plan_changed_files` end to end.** Migration adds the
      `runs.plan_changed_files text[]` column (with a goose Down); `SetRunAwaitingApproval`
      persists it (COALESCE absent-safe); `StateRequest` accepts it with the `repo_agents`
      non-nil-replaces shape (Decision 4) and per-element clamp/cap (Decision 5); `RunDTO`
      returns it via `capsOrEmpty`; `runDTOKeys` updated. `sqlc generate` run and
      committed. **The store test must cover all three tri-state transitions —
      nil-preserves, non-empty-replaces, AND empty-clears-a-previously-non-empty** (the
      third is the one that fails if the implementer used the `required_tools` empty→nil
      collapse; a test that only checks nil-preserve + non-empty-replace passes over the
      bug). `task gate:api` green (incl. the live-DB store tests and `wire_test`).

- [x] **M2 — the worker computes and sends the list at the gate, as the runner uid.**
      `gatePlan` receives `worktreePath`; a new **best-effort** `runGitAsRunner(worktreePath,
      ["status", "--porcelain"])` (wrapped `try/catch → []`, Decision 2) runs before the
      awaiting_approval report; the parsed, capped list rides the report on **every**
      round (empty when clean). `protocol.ts` declares the field.
      **The safety test is a METHOD-ROUTING spy, not an argv/uid inspection.** In the
      unit harness `uidSplitActive()` is false (`runner-uid.test.ts:32`), so
      `runnerCommand` is a passthrough and a runner-uid call and a worker-uid call spawn
      **identical** `git status` argv — an "assert the argv is setpriv-wrapped" test is
      **vacuous** (setpriv-wrapping is already covered under `UZI_UID_SPLIT=1` by
      `runner-uid.test.ts:44-59`). Instead spy on the shared `GitCache` singleton
      (harness precedent: `git.changedFiles = …` override at `runner-harness.ts:215`) and
      assert the gate invoked **`runGitAsRunner`** and **not** the worker-uid helpers
      (`runGit` `git.ts:1161`, `tryGit`, `tryGitStdout`, all built on `gitEnv(pat,…)`).
      Note `runGitAsRunner` is private (cast at the spy site), and the override must be
      **selective** — delegate the many other runner-uid calls (clone/config/checkout)
      and capture only the `["status","--porcelain"]` call, or the rest of the run
      breaks. `task gate:agent` green.

- [x] **M3 — web approval gate shows "Files changed during planning".** `PlanPanel`
      renders the section beside the plan body when the list is non-empty; `interface
      Run` carries the optional field; `SeededPlanPanel` is unchanged. `RunView.test.tsx`
      asserts: the section renders given a fixture with a non-empty list, renders nothing
      when empty/absent, and paths are sanitized for display. Run the
      `.claude/rules/web.md` retired-string / negative-assertion sweep (expected no-op —
      this adds copy, retires none). `task gate:web` green.

- [x] **M4 — `uzi run get` shows the list.** A new `renderRunDetail` block prints the
      changed-file list (only when non-empty), sanitized; `--json` carries it;
      `--field plan_changed_files` errors toward `--json` (existing contract).
      `render_test.go` gains `TestRenderRunDetailChangedFiles` (present + sanitized
      cases). `task gate:api` green (the CLI is in the api module).

- [x] **M5 — docs + specs reflect the new gate surface.** Update the gate/approval
      documentation and `specs/ai.md`'s existing plan-turn-write harm statement to note
      the property is now **surfaced** at the gate (not merely a known gap). No new
      user-facing doc page is required if none exists for the gate; if a gate/approval
      doc page exists under `docs/`, add one paragraph. Confirm `web/scripts/check-docs.mjs`
      passes if any `docs/*.md` is touched.

## Success Criteria

1. When the plan turn writes to the worktree (via `Edit`, `echo >`, `sed -i`,
   `git apply`, or any other path), the approving human sees a "Files changed during
   planning" list at the gate in **both** the web UI and `uzi run get` — the same list,
   from one server field.
2. The `git status` goes through **`runGitAsRunner`** (runner uid), never a worker-uid
   git helper — verified by M2's method-routing spy (asserting `runGitAsRunner` is
   invoked and `runGit`/`tryGit` are not), **not** by an argv/setpriv inspection, which
   is vacuous in the single-uid unit harness (M2). The `.gitattributes` clean-filter
   vector stays closed.
3. A clean plan turn, and any pre-#212 run, render **no** such section (empty/NULL →
   `[]` → hidden) — no false "changes" signal, no old-run breakage.
4. A revision round shows that round's actual tree, not a stale earlier round's list
   (Decision 3).
5. `task gate:api`, `task gate:agent`, `task gate:web` all green; `wire_test`'s
   `runDTOKeys` includes the new key; no forge driver, HTTP route, or
   `.github/workflows/**` file changed.

## Risks & Mitigations

- **Worker-uid `git status` re-opens code-exec as the PAT holder** (the M0
  write→worker-execute vector). *Mitigation*: the status call goes through
  `runGitAsRunner` only; M2 carries an explicit method-routing test asserting the
  runner-uid path. This is the PRD's top risk and top review item.
- **The status call throws and aborts the gate** (git error, or a hostile tree that
  hangs to `GIT_TIMEOUT_MS`; the `reportState` await has no `.catch`). A visibility
  feature must never wedge approval. *Mitigation*: best-effort `try/catch → []`
  (Decision 2), matching the file's other git helpers; M2 requires it.
- **The `repo_agents` empty-clears semantics is silently downgraded** to the
  `required_tools` empty→preserve collapse, reintroducing a stale round-1 list on a
  revert-between-rounds. *Mitigation*: Decision 4 pins `repo_agents` as the only correct
  precedent and names the two wrong ones; M1's store test asserts the empty-clears
  transition specifically.
- **Stale list across revision rounds.** *Mitigation*: always-send + COALESCE-replace
  (Decision 3/4); Success Criterion 4 tests it.
- **Untrusted repo-controlled paths injected into the terminal/DOM.** *Mitigation*:
  server clamp/cap + control-char strip (Decision 5), plus each renderer's existing
  `sanitizeTTY`/`cellText` (CLI) and safe-text handling (web).
- **A huge plan turn (hundreds of touched files) bloats the report/row/UI.**
  *Mitigation*: server-side element cap with a truncation marker (Decision 5).
- **Migration-number collision at merge** (parallel PRDs reserve overlapping ranges).
  *Mitigation*: rename to the next free prefix above the live head at landing;
  `gate:repo`'s `check:migration-numbering` catches a duplicate before boot.
- **Vacuous web negative assertion** (a `queryBy(...).toBeNull()` that can never
  render passes forever). *Mitigation*: M3's empty/absent case asserts against a
  fixture that *would* render if the guard were wrong; run the `.claude/rules/web.md`
  sweep.
- **Scope creep into block-on-dirty.** *Mitigation*: explicitly a Non-goal;
  surface-only ships first.

## Non-goals

- **Does not prevent the write.** A tool allowlist cannot (Bash stays granted; the
  write surface is not enumerable — issue #212 and #203's design wave). This PRD makes
  the write *visible*; prevention, if ever wanted, is a `PreToolUse` deny on the main
  thread, recorded in #203 as the upgrade path and deliberately not built here.
- **Does not block approval when the tree is dirty.** Surface-only. Block-on-dirty is a
  behavior change needing its own decision (which changes are benign? does it wedge
  legitimate runs?) — a future issue, not this PRD.
- **No `SeededPlanPanel` change** (Decision 9).
- **Autopilot / auto-approved runs are not covered.** They short-circuit before the
  gate report (`runner.ts:2423`, ahead of `:2438`), so there is no human gate to surface
  the list at — plan-turn writes on an auto-approved run are still swept in unseen. That
  is inherent to autopilot (the user opted out of the gate), not a gap this PRD closes;
  the Problem statement's "the human approves what looks like just a plan" is about
  gated runs.
- **No forge-driver / `Forge` interface change** (Decision 11).
- **No new HTTP route** — the field rides the existing `awaiting_approval` report and
  the existing run-detail GET (`api/internal/handler/workers.go:1073`,
  `GetRun`).
- **No `.github/workflows/**` change.** The worker PAT lacks `workflow` scope; a
  workflow-file touch in the branch diff is an atomic push rejection that loses the
  whole branch (`.claude/rules/prds.md`). Nothing in this PRD needs one.

## Validation strategy

- **Gates**: `task gate:api` (Go: live-DB store tests, `wire_test`, `-race`),
  `task gate:agent` (node --test), `task gate:web` (jsdom/vitest — no browser). All
  three are runnable by an offline sweep worker.
- **The safety assertion is code-level, not browser-level**: M2's runner-uid test and
  M1's store round-trip fully cover the security-load-bearing behavior without a
  rendering engine.
- **Single-file iteration**: `cd agent && node --import tsx --test
  --test-timeout=120000 test/<file>.test.ts`; `cd web && npx vitest run
  src/pages/RunView.test.tsx`; the api store/render tests via their `task` slices.
- **Manual visual confirmation is optional and post-merge** (a human viewing a real
  gated run's PlanPanel); it is **not** a worker task, because the section's presence,
  emptiness handling, and sanitization are all asserted in jsdom.

## Offline-worker notes (sweep handoff)

This PRD is fully **internet-independent**: every fact is read from this repo's own
source at `335136c0a`. No milestone needs the open web, an external API, or
`WebFetch`/`WebSearch`, so it is safe for a restricted-egress uzi sweep worker. The
prior-art convention (bottega / dot-agent-deck) is **not load-bearing** here
— a gate-integrity `git status` check is uzi-specific — and is skippable offline.

**Internet-independent and browser-independent both hold.** All three gates are
headless (Go test, node --test, jsdom/vitest); the only browser-shaped step (a human
eyeballing the rendered PlanPanel) is explicitly optional and post-merge.

## Decision Log

- **2026-08-22**: PRD created from issue #212 after verifying the gap is still live on
  `main` @ `335136c0a`: `guardrails.ts:122-128` still declares no filesystem-write rule
  and names #212; the gate report (`runner.ts:2438-2446`) still emits `plan_md` + the
  two additive-optional siblings and no changed-file list; `runGitAsRunner`
  (`git.ts:1219-1236`) still exists as the safe runner-uid primitive. Field named
  `plan_changed_files` uniformly across agent/api/web/CLI. Chose ride-the-report over
  the `onAwaitingApproval` side-channel (Decision 1), always-send + COALESCE for
  revision-round correctness (Decisions 3/4), `text[]` of porcelain lines (Decision 7),
  and surface-only scope (block-on-dirty deferred to a future issue).
- **2026-08-22 (review pass, two parallel reviewers)**: ~35 anchors spot-checked against
  `335136c0a` — **zero wrong** (offsets within ±4, the stated tolerance). Five fixes
  applied from the review: (1) **critical** — retargeted the ingest precedent from
  `inferredRequirementParams`/`clampWirePreservedPatch` (both collapse empty→NULL →
  *preserve*, which reintroduces the stale-list bug) to **`repo_agents`** (non-nil
  replaces, no `len>0` collapse), the only StateRequest field with the needed
  empty-clears semantics; M1's store test must assert the empty-clears transition.
  (2) worker-side status call made **best-effort** (`try/catch → []`) so it can't abort
  the gate. (3) M2 safety test respecified as a **method-routing spy** (assert
  `runGitAsRunner` invoked, not worker-uid `runGit`) — an argv/setpriv check is vacuous
  in the single-uid unit harness. (4) **M4 cannot parallelize with M1** (same Go module,
  compile-time dependency); only M2/M3 do. (5) added the `.gitignore`-evasion scope
  caveat, the autopilot carve-out, the truncation-marker-tolerance note, and the goose
  Down clause. Mapper-line location corrected to the `capsOrEmpty` group
  (`workers.go:428-429`).
- **2026-08-23 (implemented, M1–M5 landed on `agent/issue-212`)**: all five milestones
  committed and each reviewed clean (reviewer + auditor per unit). Three corrections the
  implementation made against the PRD's stated anchors, each discovered by testing rather
  than review:
  1. **Migration number is `00151`, not `00150`.** The live head had advanced to
     `00150_github_project_link_done_option.sql` since the PRD was written, so the new
     column landed as `00151_run_plan_changed_files.sql` (numbers are assigned at merge;
     `gate:repo`'s `check:migration-numbering` enforces uniqueness).
  2. **The M1 per-line clamp is NOT `termsafe.SanitizeBounded`.** Decision 5 named it, but
     `SanitizeBounded` `TrimSpace`s its input, which strips the LEADING space of the
     porcelain XY status code (" M path" → "M  path"), silently rewriting the change kind —
     defeating Decision 7 (store porcelain lines *with* their status). A unit test caught
     it; the ingest now uses a dedicated `sanitizePlanChangedLine` that strips control/bidi
     via `termsafe.Unsafe` (which also drops `\n`/`\t`, a persist-time defense against
     embedded-newline row-forgery) and byte-clamps **without** trimming.
  3. **The M4 CLI renderer uses `sanitizeTTY`, NOT `cellText`.** Decision 10 said
     "`sanitizeTTY`/`cellText`", but `termsafe.CellText` ends in `TrimSpace` — the same
     leading-status-space corruption as (2). The below-table free-text block uses
     `sanitizeTTY` (preserves the leading space, still strips control/bidi); the server
     already stripped `\n`/`\t` per line, and an off-table `Println` block is not a
     tabwriter rail an embedded newline could forge a row in.
  Runner-uid discipline (Decision/Risk #1) verified closed by the M2 method-routing spy
  and an auditor pass: `GitCache.planChangedFiles` is the only `git status` in the agent
  tree and routes exclusively through the cap-less runner-uid `runGitAsRunner`. Gates
  green: `task gate:api`, `task gate:agent`, `task gate:web`. Docs half: `docs/run-activity.md`
  "Plan approval gate" section gained the "Files changed during planning" paragraph, a
  CHANGELOG `[Unreleased]` entry was added, and `specs/ai.md`'s plan-turn-write harm
  statement was amended to note the property is now surfaced (surface-only). PRD moved to
  `prds/done/`.
