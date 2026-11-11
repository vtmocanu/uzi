# PRD #4173: External issue trackers: `IssueTracker` and `CodeHost` interfaces, with YouTrack as the first external issue tracker

**Issue:** #4173  
**Priority:** Medium  
**Status:** In progress, r3 2026-11-11. M1 delivered. r3 follows the maintainer's scope review: one real issue tracker instead of abstract identity work, and the comment, command and withdrawal behaviours leave this PRD.  
**Delivery:** one PR per milestone, each merged before the next starts. No implementation or validation step touches `.github/workflows/**`.  
**Builds on:**

- **The forge layer** (`api/internal/forge`): three drivers (GitLab, `adr/0065-forgejo-driver.md`, `adr/0238-github-driver.md`) and the shared fake `forgetest.BaseFake`.
- **The board column automation** (PRD #12, `api/internal/runlifecycle/`): moves a card by swapping labels when its run changes status.
- **Issue comment snapshots** (PRD #381): the issue's comments are saved and given to the agent when a run starts.

## Problem

uzi takes work only from forge issues. Teams that plan in an issue tracker (YouTrack, Jira, Asana) cannot use it without copying every task into the forge. An issue tracker cannot be plugged in today, because the forge layer does two jobs through one interface:

- **Code:** repos, merge requests, pipelines, branch protection, token and role checks.
- **Work items:** issues, labels, comments, issue state.

An issue tracker does only the second job. Adding one as a fourth forge would force it to pretend it has repos and merge requests, so the issue job has to come out of the forge first.

## Goal

1. `Forge` splits into `IssueTracker` (issues, labels, comments, state) and `CodeHost` (everything else), and stays as their union. Done in M1.
2. Every caller receives only the half it needs. M2.
3. A repo can take its issues from a YouTrack project while its code, merge requests and CI stay on its forge.
4. Feature parity: every issue-side feature that works today on GitLab, GitHub and Forgejo works on YouTrack, or is listed below as not offered on an issue tracker yet.
5. Backwards compatible: a repo with no issue tracker link behaves exactly as today. No backfill, no setting to change.

## Security invariants

These hold in every milestone. A change that weakens one is out of scope for this PRD.

- **S1. An issue tracker never gains code access.** An issue tracker driver implements `IssueTracker` only. Passing it where a `CodeHost` is required is a compile error.
- **S2. Code-host checks stay where they are.** The privilege check, the protected-branch check and the worker-held forge credential always act on the repo's forge connection, and they fail closed. Linking an issue tracker changes none of them.
- **S3. Issue tracker credentials follow the forge rules.** The token is sealed in `secretbox` with no reveal endpoint. The base URL is https only and must be on an allowlist that is separate from `FORGE_ALLOWED_BASE_URLS` and empty by default. Redirects are not followed. Every error passes the redactor.
- **S4. Issue tracker text is untrusted input.** Titles, descriptions, comments, tags and state names get the same bounds and fencing as forge issue text.
- **S5. The worker never holds an issue tracker token.** It reads issues through the existing worker API routes.

## Out of scope

- **The living status comment, retried comment writes, automatic withdrawal, comments that reach the agent and `/uzi` commands.** They change behaviour on all three forges and add trust boundaries, independently of issue trackers. They move to their own PRD(s). The design gaps found for them so far, to be carried into those PRDs:
  - *Withdrawal.* `FullSync` keeps every open issue cached regardless of label, so de-labeling an open issue does not evict it and nothing would fire. Eligibility is "uzi label OR assigned to the uzi bot", so removing the label must not cancel a still-eligible run. Cancel versus hold needs a reason, and a rule for the branch and unfinished work.
  - *Comments and commands.* Each comment needs an exact binding to the gate revision or question it answers, plus replay and ordering rules (`SubmitInput` already has `ExpectedGateRevision`; Slack already prevents a late answer to Q1 from answering Q2). `/uzi approve` cannot satisfy a semantic approval gate (`admitApproval` in `workersvc/approval_guard.go`), so it refuses there and points to the UI. Related: #1563 (untrusted issue text into prompts), #1420 (a follow-up shown delivered before acknowledgment).
  - *Outbox.* A per-run outbox cannot cover autopilot's refusal comments, which are written when no run exists. "Lands once" also assumes the marker can be read back before a retry.
- **Jira and Asana.** One PRD each, after YouTrack.
- **How the worker clones, pushes and opens the merge request.** The code host is unchanged.

## Current state (anchors from r1, verified on upstream `main` 6a76d09f7; line numbers drift, cite by symbol)

- **Where the issue side is used today.** Every place in uzi's code, tests excluded, that calls one of the eleven issue-side methods through the `Forge` interface (47 in all; calls a driver makes inside itself are not counted), listed from the type-checked code (`golang.org/x/tools/go/packages`) and cross-checked with `gopls call_hierarchy`:
  - *Sync:* `forgesvc` `FullSync`, `IncrementalSync` and `reconcileFindingGroupMarkers` (`ListIssues`, `api/internal/forgesvc/service.go:787`, `:795`, `:816`, `:911`, `:915`, `:926`, `:448`).
  - *Board:* `AutoMove` and `SetIssueLabel` (`EnsureLabels`, `UpdateIssueLabels`, `forgesvc/service.go:534-542`, `:600-607`); column setup and label checks in the handlers (`EnsureLabels`, `ListLabels`, `api/internal/handler/board.go:366`, `:432`, `:760`, `handler/labels.go:36`, `:120`); close and reopen (`SetIssueState`, `forgesvc/close_reopen.go:29`, `:62`).
  - *Run creation:* the issue's text and comments (`GetIssue`, `ListIssueComments`, `api/internal/issueinput/projection.go:158`, `:169`), reached from `workersvc` `CreateRun`, the scheduler (`api/internal/schedsvc/scheduler.go:594`, `:806`), autopilot (`api/internal/poller/autopilot.go:209`) and the composite flow (`api/internal/workersvc/composite.go:212`); a task continuing an issue (`GetIssue` through the helper `forge.GetIssueBounded`, `forge/response_limit.go:114`, called from `workersvc/task.go:100`); semantic drift (`GetIssue`, `workersvc/semantic_drift.go:75`).
  - *The agent:* the worker reads issues through the API, `GET /api/worker/runs/{id}/forge/issues`, `…/issues/{iid}` and `…/issues/{iid}/label-events` (`ListIssues`, `GetIssue`, `ListIssueComments`, `ListIssueLabelEvents`, `handler/worker_forge.go:138`, `:183`, `:265`, `:310`).
  - *The issue page:* `GetIssueDetail` (`GetIssue`, `handler/issues.go:216`).
  - *Issues uzi creates:* the board's new issue (`handler/issues.go:65`), findings and finding groups, which also ensure their labels first (`EnsureLabels`, `handler/findings_file.go:163`, `:169`, `handler/findings_group_file.go:203`, `:227`), the judge's admin issue (`handler/judge_admin_issue.go:269`), review issues (`handler/review_issue_file.go:159`), the self-improve tracking issue (`schedsvc/self_improve.go:269`) and proposals (`workersvc/composite.go:150`, `workersvc/proposal_filing.go:175`), all through `CreateIssue`; the scheduler's proposal digest and self-improve lookup read issues (`ListIssues`, `schedsvc/scheduler.go:1426`, `schedsvc/self_improve.go:252`).
  - *Issue description:* the PRD link patch (`GetIssue`, `UpdateIssueDescription`, `forgesvc/prd_link_patch.go:159`, `:180`).
  - *Autopilot:* label events (`ListIssueLabelEvents`, `poller/autopilot.go:140`).
  - *Comments out:* see below.
- **The privilege check is code-host only.** `VerifyToken`, `ProjectRole`, `DefaultBranchProtection` (`api/internal/privcheck/checker.go:77`, `:139`, `:147`; `privcheck/service.go:256-283` at claim), fail-closed.
- **Mixed callers.** Comment intake reads issue comments (`ListIssueComments`) and checks each author against the repository (`RepositoryAuthorEligibility`, `api/internal/issueinput/assessment.go:86`); merge request watchers read the MR from the code host (`forgesvc/mr_watch.go:60`) and move the card on the issue side (`AutoMove`, `mr_watch.go:227`).
- **Issue identity is a number.** `issues.forge_issue_iid bigint` (`api/internal/store/migrations/00002_forge.sql:47-50`), `runs.issue_iid`, and one active run per issue on `(repo_id, coalesce(issue_iid, continued_issue_iid))` for issue runs and tasks continuing an issue (`00351_task_continued_issue.sql:11-14`).
- **Board columns are labels.** `board_columns.label_name` (`00002_forge.sql:39`); moving a card swaps labels through `UpdateIssueLabels` (`api/internal/forgesvc/service.go:512-542`), forge-first, with the `move_pending_since` marker (`api/internal/runlifecycle/lifecycle.go:1-17`).
- **Autopilot.** Maps the label adder to a uzi user through `forge_connections.human_username` (`00037_autopilot_mapping.sql:6-20`, `poller/autopilot.go:194`).
- **Constraints a text key meets.** `issues.forge_issue_iid` is `bigint NOT NULL` with `UNIQUE (repo_id, forge_issue_iid)` (`00002_forge.sql:50`, `:59`), and `runs_kind_shape` requires `issue_iid IS NOT NULL` for `kind = 'issue'` (`00314_cross_check_claude_checker.sql`). An added `issue_key` column alone does not make an issue tracker issue storable.
- **Comment authors are checked against the repository.** Comment intake keeps a comment only when its author has repository access (`RepositoryAuthorEligibility`, a `CodeHost` method taking a forge user ID). An issue tracker comment's author is an issue tracker account, which that check cannot evaluate.

## Decisions

- **D1. The split.** `IssueTracker` is the eleven issue-side methods. `CodeHost` is the other 29. `RepositoryAuthorEligibility` and `UserExists` stay on `CodeHost`: they ask the forge about its accounts and permissions.
- **D2. `Forge` is the embedded union of the two.** The three forge drivers and `forgetest.BaseFake` implement `Forge`, so they satisfy both halves.
- **D3. Narrowest interface per caller.** A caller that needs both halves takes two parameters (`ch forge.CodeHost, tr forge.IssueTracker`), never the union.
- **D4. What enforces the split.** The interface declarations do: a method belongs to the half that declares it, and passing an `IssueTracker` where a `CodeHost` is required does not compile. The per-driver `var _ IssueTracker` / `var _ CodeHost` lines are readable markers only; implementing `Forge` already implies them. No reflection test in M1 or M2.
- **D5. A repo resolves its two sides.** `IssueTrackerForRepo` and `CodeHostForRepo` (`forgesvc`). Through M2 both return the repo's forge driver. From M4, `IssueTrackerForRepo` returns the linked issue tracker's driver. `CodeHostForRepo` always returns the forge driver (S2).
- **D6. Issue tracker drivers live in their own package**, `api/internal/issuetracker`, not in `forge`. A driver is `forge.IssueTracker` plus what a connection needs: verify the token, list projects, list states. A new issue tracker is one kind constant, one registry entry, one driver and its fake.
- **D7. Every driver ships with a fake and contract tests.** The fake follows `forgetest.BaseFake` (loud `ErrNotStubbed` defaults). One contract suite runs against every `IssueTracker` implementation, the three forge drivers' fakes included, so parity is tested, not asserted.
- **D8. Issue tracker connections are separate from forge connections.** Per user, kind and base URL, with the rules in S3. A repo links to one connection and one issue tracker project. NULL means the forge is the issue source.
- **D9. Moving a card is an `IssueTracker` method.** `MoveToColumn`. The forge drivers implement it with one shared helper that does today's label swap in one `UpdateIssueLabels` call. YouTrack maps a column to a value of the project's State field. The column-to-state mapping is a side table, so relinking a repo never has to clear a column other code reads.
- **D10. Issue identity is designed in M4, against YouTrack.** The candidate, taken from a working prototype in our fork: the numeric identity is YouTrack's `numberInProject`, which satisfies the `NOT NULL`, uniqueness and `runs_kind_shape` constraints above with no schema change to them; a nullable `issue_key text` carries the readable id (`ABC-12`) for display and references, NULL for forge issues. An issue moved to another YouTrack project gets a new number, which is treated as the issue leaving the repo. The decision is recorded in the Decision Log in M4, before code.
- **D11. Issue tracker comments are withheld until their author is proven.** An issue tracker comment reaches a run only when its author maps to a uzi user who owns or can access the repo. Until that mapping exists, issue tracker comments are withheld exactly like a comment whose author lookup failed. How an issue tracker account is proven to belong to a uzi user is designed in M4 and recorded in the Decision Log before code; the fail-closed default holds until then.
- **D12. Autopilot is not offered on a repo linked to an issue tracker in this PRD.** It maps the label adder to a uzi user through the forge identity, which an issue tracker account does not have. It stays unchanged on forge-backed repos.
- **D13. An issue number belongs to one issue source, and a repo changes source only when idle.** A forge issue 12 and a YouTrack issue 12 are different issues, and so are number 12 in two YouTrack projects. Each run and each cached issue records the issue source it came from (the issue tracker connection and project, or the forge when NULL). Linking, relinking or unlinking a repo is refused while it has a run that is not terminal. When the source changes, the repo's cached issues from the old source are removed and schedules pinned to an issue number are disabled until their owner pins them again. Lifecycle moves, comments and merge request watcher updates act on a run's issue only when the run's recorded source is the repo's current source; otherwise they are skipped and logged, never applied to the same number in another source. A source change is fenced against operations already in flight: each repo carries an issue-source generation, bumped in the same transaction that changes the link, clears the cache and disables the pinned schedules. A sync records the generation when it starts and its results are stored only if the generation is unchanged, in the same statement as the write. An operation that writes to an issue (lifecycle move, comment, merge request watcher update) holds a per-repo shared lock from resolving the source until its external call returns, and the source change takes that lock exclusively, so it waits for writes in flight and no write resolves one source and lands on another.

## Feature parity on a repo linked to an issue tracker

- Bring tagged issues into uzi: the `uzi` tag in the linked project. Milestone M4.
- Give the run the issue's title, body and comments: yes, comments subject to D11. M4.
- Let the agent read an issue through the worker API: yes, same routes. M4.
- Show the board and move cards: yes, through the State field (D9). M5.
- Close or reopen an issue: yes, through the State field. M5.
- Create an issue (board, findings, judge, review, self-improve, proposals): yes, in the linked project. M5.
- Write comments on the issue (today's automatic templates): yes. M5.
- PRD link patch in the issue description: yes. M5.
- Eligibility by assigning the issue to the uzi bot: not offered in this PRD; the tag is the only signal.
- Autopilot: not offered (D12).
- Privilege checks, claim and settle, pipeline sync, merge request watchers, clone and push: unchanged, on the forge (S2).

## Milestones

Progress (one PR per milestone, each merged before the next starts):

- [x] **M1:** the `IssueTracker` and `CodeHost` interfaces
- [ ] **M2:** every caller on its half
- [ ] **M3:** issue tracker connections and the YouTrack driver
- [ ] **M4:** issue identity, the repo link and issue intake from YouTrack
- [ ] **M5:** the board and issue writes on YouTrack
- [ ] **M6:** web and CLI surfaces, docs, ADR and live acceptance

### M1: the `IssueTracker` and `CodeHost` interfaces

D1, D2 and D4. One file per interface with the types only it uses (`issuetracker.go`, `codehost.go`); `forge.go` keeps the shared pieces.

- **Acceptance:** pure moves (`git diff -M` shows no changed lines in moved code); no caller changes; every existing test passes unchanged.

### M2: every caller on its half

D3 and D5.

- **Blocked by:** M1.
- **Acceptance:** all 47 issue-side uses go through `IssueTracker`; `privcheck` cannot be handed a value that has only the issue side; existing tests change only in two mechanical ways (a fake's parameter type, and passing the same fake as both halves).

### M3: issue tracker connections and the YouTrack driver

D6, D7, D8 and S3. The `api/internal/issuetracker` package, the YouTrack driver for every `IssueTracker` method plus verify, list projects and list states, its fake, the contract suite, the connection table and the allowlist. No repo can link to an issue tracker yet, so no behaviour changes.

- **Blocked by:** M2.
- **Acceptance:** the contract suite passes on the YouTrack fake and on the three forge fakes; with the allowlist empty no connection can be created and uzi still boots; a redirect, an oversized body and an error carrying the token are each refused or scrubbed in a test.

### M4: issue identity, the repo link and issue intake from YouTrack

D5, D10, D11, D13 and S2. The identity decision (recorded in the Decision Log before code), the repo link to a connection and project with the link and relink rules of D13, `IssueTrackerForRepo` returning the issue tracker, sync on the poll cadence, run creation from an issue tracker issue.

- **Blocked by:** M3.
- **Acceptance:** a tagged YouTrack issue appears on the board and starts a run that receives its title and body; an issue tracker comment from an unmapped author is withheld; the privilege check and claim still run against the forge and fail closed with the issue tracker unreachable; a repo with no link is byte-for-byte unchanged in its forge calls (recorded-call test); linking is refused while a run is active; after a link or relink, a finished run and a pinned schedule from the old source cause no read or write on the same issue number in the new source; a sync that started before a relink stores nothing after it, and a watcher update racing a relink either completes on the old source before the change or is skipped after it (D13).

### M5: the board and issue writes on YouTrack

D9. `MoveToColumn` on all four drivers, the column-to-state mapping, close and reopen, issues uzi creates, today's comment templates and the PRD link patch.

- **Blocked by:** M4.
- **Acceptance:** the recorded-call test shows identical label calls on the three forges; a card move changes the YouTrack State and a failed move snaps back; closing never lands on a state that means "won't fix".

### M6: web and CLI surfaces, docs, ADR and live acceptance

Settings → Issue trackers, the CLI commands, a setup page under `docs/`, an ADR for the issue tracker seam.

- **Blocked by:** M5.
- **Acceptance (maintainer-owned, live):** connect, link, tag, run, board move and close verified on a real YouTrack, and a regression pass on GitHub, GitLab and Forgejo.

## Testing decisions

- **Every milestone's gate:** `task gate:api` and `task scan:secrets`, plus `task gate:web` when web changes, `check:changelog-entry`, and `docs:sync` with `check-docs:web` when docs change. Token-shaped test strings are assembled at runtime, and the run's evidence includes the canaries-detected line of `scan:secrets`.
- **No change in behaviour on forge-backed repos:** the existing API, web, agent and E2E suites pass; M2's two mechanical test edits are the only ones allowed without a finding.
- **Parity is the contract suite (D7)**, not a per-driver checklist.

## Risks

- **Size of M2.** About 57 production and 31 test files, mechanical. A type-checked inventory of forge calls is the guard.
- **Drift from `main` between milestones.** Each milestone's branch is rebased on `main` before its PR.
- **YouTrack specifics leak into the seam.** The contract suite runs on all four implementations, and the second issue tracker (Asana) is the real test of the seam.

## Decision Log

- **2026-11-05: the interface is named `IssueTracker`.**
- **2026-11-10: one file per interface.** `issuetracker.go` and `codehost.go`; r1 had both interfaces in `forge.go`.
- **2026-11-10: no reflection test.** The interface declarations enforce the split (D4).
- **2026-11-10: M2 test edits.** Fakes may change a parameter type, and mixed callers' test call sites pass the same fake twice. Nothing else in a test changes.
- **2026-11-10: scope after the maintainer's review.** YouTrack becomes part of this PRD and drives the identity design. The living comment, the outbox, withdrawal, comments to the agent and `/uzi` commands move out. Asana follows in its own PRD.
- **2026-11-11: link and relink policy (D13).** Review of the M1 PR found that project-local numbers let an old run or pinned schedule resolve to an unrelated issue after a repo changes its issue source. D13 scopes every number to its source and M4's acceptance covers the transition. A second pass added the fencing against in-flight syncs and writes (generation check and per-repo lock).
