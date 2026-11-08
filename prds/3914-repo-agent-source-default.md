# PRD #3914: Per-repo default agent source, applied before the first plan turn

**Issue:** #3914
**Status:** Draft r2, 2026-11-04 (r1 reviewed by an architect pass and the Codex peer; findings folded). Source anchors checked against origin/main `9cc6ab5a1`. Line numbers drift; cite by symbol.
**Priority:** Medium (`priority::normal`).
**Blocked by:** M3 (planning) is blocked by #3913 (pin the repo-agent roster to a trusted default-branch revision), because M3 is the first change that runs repo agents before a human plan gate. M1, M2 and M4 do not depend on it.
**Related:** PRD #37 (`prds/done/37-run-agent-selection.md`, Decisions 4, 5, 6, 10), #3912 (evidence-audit wording of the repo-agent passage), #3250 (per-run model at start), #602 (instance-wide agent-source sync), #2242.
**Delivery:** one PRD, four milestones, implemented by an external contributor one PR per milestone and reviewed by the maintainers. No implementation or validation step touches `.github/workflows/**`.
**Mock:** none. The PRD text is the presentation spec; every new control reuses an existing component (the repo setup toggles, the plan-gate agent picker, the Slack gate buttons).

## Problem

A run's planning turn always uses the run owner's uzi templates. The owner's choice between the repo's own agents (`.claude/agents/`, `.codex/agents/`) and uzi templates takes effect only after plan approval (PRD #37 Decision 5). Three consequences:

1. **A repo-specific plan check cannot live in the repo.** The workaround in use is a user-scope ("Mine") template: only that user's runs get it, and template allocation has no repo dimension, so it also rides that user's runs on every other repo. An admin could ship a global template instead, which also has no repo dimension and reaches every user's runs on every repo.
2. **Callers that never choose cannot get repo agents for planning.** An external dispatcher that creates runs and approves with no selection gets the repo roster for implementation (the absent-selection rule picks `repo` whenever a roster exists) but never for planning. `run create` accepts `agent_selection` only together with `plan_md`, so there is no earlier choice.
3. **An owner who prefers their own templates on a repo that ships agents gets the repo roster anyway** whenever approval carries no selection, and on every autopilot run.

## Verified baseline (origin/main 9cc6ab5a1)

| Seam | Anchor | Current behaviour |
|---|---|---|
| Selection resolution | `resolveAgentSelection(parse, repoAvailable)`, `agent/src/protocol.ts` | `ok` passes through (`repo` with no roster degrades to `own` plus a note); `invalid` forces `own` plus a note; `absent` gives `repoAvailable ? "repo" : "own"`. No input for a default. Callers: the Claude gate path (`sdk-executor.ts`), the Codex gate path (`codex-executor.ts`, "ONCE on every path"), and the runner's autopilot branch (`runner.ts`, "The executor re-resolves the SAME absent-parse"). |
| Persisted selection | `seedAgentSelection`, `protocol.ts`; `persistedSelection`, `api/internal/workersvc/agent_selection.go` | A persisted `runs.agent_source` reaches the claim as `AgentSelection`; NULL sends none and the worker falls to the absent rule. |
| Writers of `runs.agent_source` | `submitApproval` (`workersvc/submit.go`) via the approve query in `runtime.sql`; the autopilot `running` report (`COALESCE(sqlc.narg('agent_source'), agent_source)` in `runtime.sql`, handled in `workersvc/service.go` only when the run is autopilot); a seeded plan in `createRun` (`service.go`); milestone reconciliation (`workersvc/milestone_reconciliation.go`, `agent_source = CASE WHEN $4::jsonb IS NULL ...`) | `submitApproval` runs only when the approval carries a selection, so a no-selection approval leaves it NULL and nothing records what implementation used. Migration `00052_run_agent_selection.sql` documents it as a record of what ran; a requeue or resume that re-enters the human plan gate overwrites it. |
| Plan turn, Claude | `sdk-executor.ts` (comment "plan turn runs with the OWN subagents (PRD #37 Decision 5)"); `planTurnSubagents`, `agent/src/agents.ts` | Owner templates only, write tools stripped, agents left with only write tools dropped. `subagentWriteCapabilities` is derived from `assembled.subagents` (PRD #266). The implement path builds its roster with `selectSubagents`, then `shapeGuardedGrants` (#2471), then the #305 model override. |
| Plan turn, Codex | `buildRunRequest` `phase === "plan"`, `agent/src/codex/codex-executor.ts` | `ownSubagentTemplates(ctx.agents)` only. No `planTurnSubagents` strip; the broker refuses planning file-write requests and the plan prompt says "All planning dispatches are read-only"; the shell remains. The implement phase uses `implementTemplates(ctx, selection)`, which maps repo agents with `toHarnessAgent` and passes `repoSkills`. |
| Untrusted framing | `REPO_SUBAGENT_UNTRUSTED_APPEND`, `agent/src/prompt.ts`; `buildLeadSystemPrompt` `opts.repoSourced` | Applied to the implement prompt when `selection.source === "repo"` on both harnesses. The plan phase passes `repoSourced = false`. |
| Roster detection | `detectRepoAgents`, `agent/src/repoagents.ts`; runner preflight (`parseRepoAgents`, `agent/src/runner.ts`) | Every claim, before plan and gate, on the working clone. Reported to the server as `repo_agents` on a `running` state. The server knows no roster before the first claim's preflight. #3913 changes the source to git objects at a pinned default-branch revision. |
| Autopilot exposure today | `runner.ts` autopilot branch | An autopilot run with a detected roster already implements with write-enabled repo agents and no human review. |
| Run create | `Handler.CreateRun` request struct, `api/internal/handler/runs_lifecycle.go`; `createRun`, `service.go`; INSERT `CreateRun` in `api/internal/store/queries/runtime.sql` | `agent_selection` without `plan_md` returns 400 "agent_selection requires plan_md". A seeded selection is validated shape-only (no roster exists at create). The INSERT copies `required_capabilities` from the repo row. Manual, CLI, chat `start_run`, schedules and autopilot issue runs all go through `createRun`; other kinds (`ci_fix`, `mr_rework`, `self_improve`, `prompt`, `task`, jobs) have their own INSERTs. |
| Worker protocol gate | standalone clauses in the `ClaimRun` query (`runtime.sql`), e.g. `r.completion_contract_version IS NULL OR 'completion_interlock_v1' = ANY(@worker_protocol_caps)` (PRD #1226 M1) | A run that needs newer worker behaviour is claimable only by a worker whose self-reported `workers.protocol_capabilities` contains the token. These clauses sit outside `required_capabilities` and the `capability_aware` kill switch, so no owner override or admin setting can bypass them. (`required_capabilities` is the closed tooling set in `api/internal/capability`, not a protocol gate.) |
| CLI | `uzi run create --agent-source/--exclude-agents` (`api/cmd/uzi/run_lifecycle.go`, error text in `run.go`); `uzi run approve --agent-source` (`run.go`, `approveSelection`) | Create flags require `--plan-file`. Approve with no flag sends no selection. |
| Web gate | `PlanPanel.tsx` (`web/src/pages/runView/`), `AgentPicker.tsx` | Pre-selects `repoDetected ? "repo" : "own"` and always submits an explicit selection. |
| Slack gate | `gateBlocks`, `api/internal/slacksvc/gate.go`; mapping in `gatekeeper.go` | With a roster: "Approve · repo agents (N)" (primary style) and "Approve · my templates"; without: one "Approve" that sends explicit `own`. Every Slack approval is explicit. |
| Per-repo settings | `PatchRepo`, `api/internal/handler/forge.go` (exclusive settings groups, owner via `...ForUser` queries, admin via unscoped queries, cookie auth + CSRF); `RepoSetupChip.tsx`; `api.ts` | Per-user per-repo toggles (`repo_skills_enabled`, `repo_claudemd_enabled`, `repo_devbox_opt_in`, ...) live on the `repos` row. `repos` is `UNIQUE (connection_id, forge_project_id)`: one row per user connection. |
| Run DTO | `apitypes/run.go` (`RepoAgents`, `AgentSource`, `AgentExclusions`); web `apiTypes.ts`; `RunView.tsx` | Shows the implementation source; nothing records which roster planned the run or why a source was chosen. |
| E2E | `e2e/phases/29-autopilot-lifecycle.sh`, `e2e/phases/15-happy-path-restart.sh` | Phase 29 asserts an autopilot run with no default resolves to `agent_source = repo`; phase 15 asserts an explicit repo choice survives a restart. |

## Outcome

Each user can set, per repo they have connected, a **default agent source**: `own`, `repo`, or unset (today's behaviour). It is frozen onto each issue run they create and governs both phases unless a run carries an explicit choice.

- **Approval with no selection** (CLI, API, an external dispatcher) implements with the requested source, and the run records it.
- **Autopilot** implements with the requested source.
- **Web and Slack** pre-select the requested source; an explicit choice at approval still wins.
- **Planning:** with a requested source of `repo` and a detected roster, the plan turn uses the repo's agents under today's plan-phase restrictions (write tools stripped on Claude, file-write requests refused by the broker on Codex; the shell remains on both, so shell commands can still change files and this is not a read-only sandbox), with the same untrusted framing as implementation.
- **Per-run choice:** `uzi run create --agent-source` and the API's `agent_selection` work without a plan file and override the default for that run.
- **Visibility:** the run shows "Planned with X (repo default / this run)" and "Implemented with Y (as requested / changed at approval)".
- **Unset** keeps today's behaviour exactly.

### Consent and trade-off

Only the run owner can opt their runs into `repo`: the setting changes only runs that the same user creates on that repo connection, on their credential and worker. An admin may clear the setting or set `own` on any row, never `repo`. The repo supplies the shared agent definition through git; each teammate opts in once. Nothing enforces one team-wide value, and a new teammate must opt in themselves. A shared, enforced project default is out of scope.

The new exposure is limited: autopilot runs already implement with write-enabled repo agents without review whenever a roster exists. M3 adds repo agents to the **plan turn of human-gated runs**, report-only, for owners who opted in.

### Acceptance examples

1. User A sets `repo` on repo R, which ships `.claude/agents/site-plan-check.md`. An external dispatcher on A's account creates an issue run and approves it with no selection. The plan turn dispatches `site-plan-check` (Claude and Codex), the plan prompt carries the untrusted framing, implementation uses the repo roster, and the run page shows "Planned with: repo agents (repo default) · Implemented with: repo agents (as requested)".
2. User A sets `own` on repo R. A run approved with no selection, and an autopilot run, both implement with A's templates although R ships agents. The web gate pre-selects "my templates" and the Slack card makes "Approve · my templates" the primary button.
3. With A's default `repo`, an explicit approval with `own` implements with A's templates and shows "Implemented with: my templates (changed at approval)". If the run later re-plans, the plan turn uses `own`.
4. With A's default `repo`, `uzi run create --repo R --issue 12 --agent-source own` plans and implements with A's templates and shows "(this run)".
5. User B, connected to the same project, has the default unset. B's runs behave as today, whatever A set.
6. A's default is `repo` but R has no agents: the run plans and implements with A's templates and shows the existing "no repo agents found" note.

## Out of scope

- **A shared or enforced project-level default** (one value across users, editor authority, forge-role checks). uzi stores no human forge identity today, so "users the forge reports as maintainers" has no data source. Revisit only if a team needs one enforced value.
- **Run kinds outside `createRun`** (`ci_fix`, `mr_rework`, `self_improve`, `prompt`, `task`, jobs, cross-check): they keep today's selection behaviour.
- **Exclusions chosen at create without a plan file.** Exclusions stay an approval-time or seeded-plan choice.
- **Other per-repo run defaults** (lead model, plan cross-check, memory). Per-run model is #3250.
- **A write policy for `.claude/agents/**`** and the roster pin itself (#3913).
- **CLI write of the setting.** `PatchRepo` is cookie-only, like the other repo setup toggles; the CLI shows the value read-only.
- **The evidence-audit wording** of the repo-agent passage (#3912); this PRD only applies the existing passage to the plan prompt.

## Modules and seams

### 1. Per-repo setting: `repos.agent_source_default`, `PatchRepo`, repo setup UI

- **Column:** `repos.agent_source_default text NULL CHECK (agent_source_default IN ('own','repo'))`. NULL means unset.
- **Write:** a new exclusive `PatchRepo` settings group `agent_source_default` (JSON `"own" | "repo" | null`). The owner path (`...ForUser`, scoped by `user_id`) may set any value. The admin path may set only `own` or `null`; an admin request for `repo` returns 403. Unknown values return 400. Cookie auth and CSRF as today.
- **Read:** the repo DTO (`agent_source_default`), `uzi repo list --json`, and a row in the repo setup UI next to the repo-skills toggle: "Default agents for my runs on this repo: uzi templates / repo agents / not set (today's behaviour)". Choosing `repo` shows: "Repo agents can run shell commands while planning, before you review the plan."
- **Invariant:** the value affects only runs whose `repo_id` is this row, i.e. this user's connection.

### 2. Frozen request on the run: `createRun`

- **Columns:** `runs.agent_source_requested text NULL` (`own` | `repo`) and `runs.agent_source_requested_basis text NULL` (`explicit` | `repo_default`). Added `NOT VALID` with named CHECKs and validated in a sibling migration, following `00204`/`00205`.
- **Write at create, in `createRun` only, inside the create transaction:** an explicit create-time source (M4, or a seeded plan's source) writes basis `explicit`; otherwise a non-NULL `repos.agent_source_default` on the run's own repo row writes basis `repo_default`; otherwise both stay NULL. A later settings change never alters an existing run.
- **Worker gate:** a new standalone `ClaimRun` clause `r.agent_source_requested IS NULL OR 'agent_source_request_v1' = ANY(@worker_protocol_caps)`, following the `completion_interlock_v1` precedent. Workers that implement module 4 (and, from M3, module 5) self-report the token. An old worker never claims a requested run, so no worker can ignore the request. Unset runs are unaffected.
- **Kept separate from `runs.agent_source`**, which stays "what implementation used" with its existing overwrite semantics. No writer of `agent_source` (including milestone reconciliation) touches the new columns, and approval never changes them.
- **Claim:** `assembleClaim` (`workersvc/claim_assembly.go`) adds `requested_agent_source` (`"own" | "repo" | null`).

### 3. Server-side approval materialization: `SubmitInput` / `submitApproval`

- When an `approve_plan` arrives with no selection and `agent_source_requested` is set, the server treats it as an explicit selection `{source: requested, exclusions: []}` and sends it through `submitApproval`. If `repo` fails validation against the reported roster, it degrades to `own`, matching the worker's degrade rule.
- This writes `runs.agent_source`, so "Implemented with" is recorded for the external-dispatcher case, and every worker receives an explicit selection.
- An approval that carries a selection is unchanged and wins.

### 4. Worker resolution: one requested input on every call site

- `resolveAgentSelection(parse, repoAvailable, requested)`: the `absent` arm becomes `requested ?? (repoAvailable ? "repo" : "own")`, then the existing `repo`-without-roster degrade applies. `ok` and `invalid` are unchanged.
- The Claude gate path, the Codex gate path and the runner's autopilot branch all pass `claim.requested_agent_source`. No ordering change is needed: resolution is pure in (requested, roster). One table test pins that all three call sites resolve identically.

### 5. Planning with the requested source (M3): `sdk-executor.ts`, `codex-executor.ts`, `prompt.ts`

- **Plan source:** `planAgentSource(persisted, requested, repoAvailable)` returns `repo` only when `requested === "repo"`, a roster was detected, and the persisted `runs.agent_source` is not `own`; otherwise `own`. A persisted value can only narrow the source, never widen it: `runs.agent_source` also records automatic choices (autopilot, materialization), so it is never read as consent. An explicit gate choice of `own` therefore wins on a replan, and an unset request always plans with owner templates.
- **Claude:** a repo-sourced plan turn builds its roster through the implement-path pipeline: `selectSubagents("repo", ...)`, then `planTurnSubagents`, then `shapeGuardedGrants` (#2471), then the #305 model override. `subagentWriteCapabilities` is derived from the pre-strip repo definitions. The Agent-guard allow set and the "Available subagents" line come from the result.
- **Codex:** the plan phase maps repo agents as `implementTemplates` does (`toHarnessAgent`, plus `repoSkills`), and every planning dispatch keeps plan-phase grants, so the broker refuses its file-write requests (`write_denied_in_plan`); the shell remains. A test proves the broker refuses a repo planning agent's file write.
- **Prompt:** `buildLeadSystemPrompt` receives `repoSourced = true` for a repo-sourced plan turn, so the plan prompt carries `REPO_SUBAGENT_UNTRUSTED_APPEND` plus "Planning subagents report findings; they do not change files."
- **Record:** the worker reports `plan_agent_source` (`own` | `repo`) on the `running` state that starts the plan turn; the server stores it in `runs.plan_agent_source` (overwritten on replan). A seeded run skips the plan turn, so it stays NULL and the run page shows "—".
- **Roster provenance:** planning reads the roster #3913 pins; this PRD adds no second detection path.

### 6. Approval surfaces (M2)

- **Web:** `PlanPanel` pre-selects `run.agent_source_requested` when set and the roster supports it, else today's `repoDetected ? "repo" : "own"`.
- **Slack:** `gateBlocks` takes the requested source; with `own` requested, "Approve · my templates" gets the primary style and comes first. Action ids and their mapping are unchanged (PRD #37 Decision 10).

### 7. Per-run choice at create (M4): `CreateRun`, `uzi run create`

- Without `plan_md`, `agent_selection.source` is shape-validated and written as `agent_source_requested` with basis `explicit`. A selection with non-empty `exclusions` and no `plan_md` returns 400 ("exclusions require a plan file or an approval").
- `uzi run create --agent-source` no longer requires `--plan-file`; `--exclude-agents` still does.
- Chat `start_run`, schedules and autopilot pass no explicit choice; they get the repo default through module 2.
- Until M3 lands, a create-time `repo` affects implementation only; planning stays on owner templates.

### 8. Run DTO and run page

- `RunDTO` and the web `Run` type add `agent_source_requested`, `agent_source_requested_basis` and (M3) `plan_agent_source`; contract fixtures (`fixtures/api-contract/run.*.json`) updated; `TestRunSummaryItemDTOKeys` placement decided per field.
- `RunView` and `uzi run get` show "Planned with X (repo default / this run)" from `plan_agent_source` and the requested basis, and "Implemented with Y (as requested / changed at approval)", where "changed at approval" means `agent_source` differs from `agent_source_requested`.

## Testing decisions

- **Pure resolution (agent):** table tests for `resolveAgentSelection` and `planAgentSource` over persisted × requested × roster × parse status; one test pins the three call sites to the same result. Extend the existing selection cases in `protocol.test.ts`.
- **Create freeze (api, live DB):** `createRun` writes basis `repo_default` from the run's own repo row, `explicit` for a seeded or M4 source, NULL when unset; another user's row on the same project is never read; a later settings change leaves the run unchanged; the run's `required_capabilities` are unchanged.
- **Worker gate (live DB):** a worker without `agent_source_request_v1` cannot claim a requested run even with `capability_aware` off; an unset run is claimable as today.
- **Materialization:** a no-selection approval on a requested run writes `agent_source` (degrading `repo` to `own` with no roster); an explicit approval wins; the requested columns never change.
- **PatchRepo:** owner can set and clear; admin can set `own` or clear but gets 403 for `repo`; another user gets 404; invalid value 400; combined groups 400.
- **Plan turn (agent, both harnesses):** with requested `repo` and a roster, the plan request carries the repo agents through the implement-path pipeline (Claude write tools stripped, correct write-capability labels; Codex broker refuses file-write requests) and the untrusted append; with unset, today's owner templates and no append; with a persisted `own` on a replan, owner templates; with the request unset and a persisted `repo` from autopilot, a re-entered plan still uses owner templates. Watch each red on today's code.
- **Slack:** `gateBlocks` primary button follows the requested source; action ids unchanged (`gate_test.go`).
- **Web:** `PlanPanel` pre-selection and the repo setting control (Vitest).
- **CLI:** `run create --agent-source` without `--plan-file` sends the source; `--exclude-agents` without `--plan-file` still errors; `repo list --json` shows the setting.
- **E2E:** phases 29 (autopilot resolves to `repo` with no default) and 15 (explicit repo choice survives restart) are the unset-behaviour guards; grep `e2e/` and `e2e/forge-fake/` for "agent_selection requires plan_md" and the gate button literals, update stale expectations, run `task test:e2e-lib` and `task test:forge-fake`, and name the phases left unrun in each PR.

## Milestones

### M1: the per-repo default governs implementation, approval and autopilot

Modules 1, 2, 3 and 4, and module 8 without `plan_agent_source`; migrations; docs (`docs/repo-agents.md`, `docs/cli.md`); changelog fragment; `specs/human.md` entry.

- **Blocked by:** none.
- **Acceptance:** examples 2 (approval and autopilot parts), 3 (implementation part), 5 and 6 pass; a no-selection approval on a repo whose default is `own` implements with owner templates and records `agent_source = own` (fails on today's code); an old worker cannot claim a requested run; unset leaves every existing selection test and E2E phases 15/29 unchanged; full agent suite: exact-head PR CI.

### M2: web and Slack pre-select the requested source

Module 6, docs and changelog.

- **Blocked by:** M1.
- **Acceptance:** example 2's web and Slack parts pass; with unset, today's pre-selection and button order are unchanged.

### M3: planning uses the requested source

Module 5, `plan_agent_source` in module 8, docs and changelog.

- **Blocked by:** M1 and #3913.
- **Acceptance:** examples 1 and 6 pass on Claude and Codex; example 3's replan part passes; the broker refuses a Codex repo planning agent's file write; the plan prompt carries the untrusted append only for a repo-sourced plan; full agent suite: exact-head PR CI.

### M4: explicit per-run choice at create

Module 7, docs and changelog.

- **Blocked by:** M1.
- **Acceptance:** example 4 passes for implementation (and for planning once M3 is in); `agent_selection` without `plan_md` returns 201 with basis `explicit`; exclusions without `plan_md` return 400; seeded create unchanged.

## Risks

- **Pre-gate execution of repo code (M3).** Repo-authored agents with a shell run before the owner reviews the plan. Mitigations: owner-only opt-in per repo connection, the #3913 roster pin, write tools stripped / broker file-write refusal (the shell can still change files), the untrusted framing, and the setting's warning line. Autopilot already carries a larger exposure today.
- **Codex plan-phase roster mapping** is new code on the plan-phase file-write boundary; its file-write refusal test is mandatory in M3.
- **Worker roll.** Requested runs wait for a worker reporting `agent_source_request_v1`; until the fleet rolls they stay queued, and the queued-run health reason must name the missing worker version (extend it if it does not). Unset runs are unaffected.
- **Two fields for a run's agents** (`agent_source_requested` vs `agent_source`): the DTO names and run-page labels keep "requested" and "implemented with" distinct.

## Decision Log

- **D1 (2026-11-04): per-user, per-repo default, not a shared project record.** The setting decides whether the owner's own runs execute repo agents before their plan gate, so the consent belongs to the run owner. A shared record needs editor authority and a forge-role check uzi has no data for (no stored human forge identity). The external dispatcher runs on one account, so that account's setting covers every dispatched run, and the shared artefact (the agent itself) is shared through the repo. Rejected: a shared project record edited by admins or forge maintainers (consent and authority problems, new table, new forge lookup). Agreed with the reporter in review.
- **D2: admins may clear or set `own`, never `repo`.** Setting `repo` opts someone's runs into pre-gate repo code; only the owner may consent to that.
- **D3: freeze the request at create** in new columns rather than reading the repo row at claim time. A settings change must not alter queued or running runs, and resume must be deterministic. Rejected: claim-time read (non-deterministic across claims); writing into `runs.agent_source` (it records what implementation used and is overwritten on replan).
- **D4: materialize no-selection approvals on the server.** It makes every worker follow the request and records what implementation used. Rejected: worker-only resolution (an old worker would pick `repo` for an `own` owner, and nothing would record the result).
- **D5: gate requested runs with a worker protocol token** (`agent_source_request_v1`, a standalone `ClaimRun` clause like `completion_interlock_v1`) so autopilot cannot run on a worker that ignores the request. Rejected: `required_capabilities` (a closed tooling set with an owner override and an admin kill switch); an accepted race with a version floor.
- **D6: on replan, a persisted source can only narrow planning to `own`.** `runs.agent_source` mixes explicit gate choices with automatic ones (autopilot, materialization), so it cannot carry consent; only the owner's request can enable repo planning. An explicit `own` at the gate is honoured on replan. Rejected: persisted source wins outright (an unset autopilot run would plan with repo agents on replan); a new column distinguishing explicit from automatic choices (more state for no extra safe behaviour).
- **D7: only `createRun` applies the default.** That covers manual, CLI, chat, schedules and autopilot issue runs. Other kinds keep today's behaviour until a concrete need.
- **D8: unset keeps today's behaviour everywhere.**
- **D9: create-time exclusions need a plan file.** A source-only request has no roster to validate exclusions against, and a silently dropped exclusion would run an agent the owner excluded.
- **D10: Slack keeps its closed action-id set** (PRD #37 Decision 10); only the primary style and order follow the request.
- **D11: only M3 waits for #3913**, because only M3 runs repo agents before a human plan gate.
