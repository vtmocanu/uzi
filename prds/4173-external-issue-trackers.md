# PRD #4173: External issue trackers: `IssueTracker` and `CodeHost` interfaces, with YouTrack as the first external issue tracker

**Issue:** #4173

**Priority:** Medium

**Status:** In progress, r5 2026-11-11. M1 and M2 delivered. r3 followed the maintainer's scope review: one real issue tracker instead of abstract identity work, and the comment, command and withdrawal behaviours leave this PRD. r4 follows the maintainer's review of the M1 PR: the YouTrack id, tag and author decisions are made before the driver, and the issue source of a repo no longer changes.

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
2. Every caller receives only the half it needs. Done in M2.
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

## Current state (anchors refreshed in M2's PR on upstream `main` f19bc0739 with M2 applied; cited by file and symbol, because line numbers drift)

- **Where the issue side is used.** Every place in uzi's code, tests excluded, that calls one of the eleven issue-side methods (45 calls, all through `IssueTracker` since M2; calls a driver makes inside itself are not counted), listed from the type-checked code (`golang.org/x/tools/go/packages`). r1 counted 47 through `Forge` on an older `main`.
  - *Sync:* `FullSync`, `IncrementalSync` and `reconcileFindingGroupMarkers` (`ListIssues`, seven calls, `api/internal/forgesvc/service.go`).
  - *Board:* `AutoMove` and `SetIssueLabel` (`EnsureLabels`, `UpdateIssueLabels`, `forgesvc/service.go`); column setup and label checks in the handlers (`EnsureLabels` in `ConfigureColumns` and `ensureHumanReviewColumn`, `api/internal/handler/board.go`, and in `ensureRepoLabels`, `handler/labels.go`; `ListLabels` in `CheckRepoLabels`, `handler/labels.go`); close and reopen (`SetIssueState` in `CloseIssue` and `ReopenIssue`, `forgesvc/close_reopen.go`).
  - *Run creation:* the issue's text and comments (`GetIssue`, `ListIssueComments` in `FetchWithTimeout`, `api/internal/issueinput/projection.go`), reached from `workersvc` `CreateRun`, the scheduler, autopilot and the composite flow; a task continuing an issue (`GetIssue` through the helper `forge.GetIssueBounded`); semantic drift (`GetIssue` in `readCurrentSemanticIssue`, `api/internal/workersvc/semantic_drift.go`).
  - *The agent:* the worker reads issues through the API, `GET /api/worker/runs/{id}/forge/issues`, `…/issues/{iid}` and `…/issues/{iid}/label-events` (`ListIssues` in `WorkerForgeListIssues`, `GetIssue` and `ListIssueComments` in `WorkerForgeGetIssue`, `ListIssueLabelEvents` in `WorkerForgeListIssueLabelEvents`, `handler/worker_forge.go`).
  - *The issue page:* `GetIssueDetail` (`GetIssue`, `handler/issues.go`).
  - *Issues uzi creates* (`CreateIssue`, eight calls): the board's new issue (`CreateIssue`, `handler/issues.go`), findings and finding groups, which also ensure their labels first (`FileFinding`, `handler/findings_file.go`; `FileFindingGroup`, `handler/findings_group_file.go`), the judge's admin issue (`AdminFileJudgeIssue`, `handler/judge_admin_issue.go`), review issues (`FileIssue`, `handler/review_issue_file.go`), the self-improve tracking issue (`ensureTrackingIssue`, `api/internal/schedsvc/self_improve.go`) and proposals (`ConfirmProposalForUser`, `workersvc/composite.go`; `maybeFileProposal`, `workersvc/proposal_filing.go`). The scheduler's proposal digest and the self-improve lookup read issues (`ListIssues` in `proposalDigestSection`, `schedsvc/scheduler.go`, and in `ensureTrackingIssue`).
  - *Issue description:* the PRD link patch (`GetIssue`, `UpdateIssueDescription` in `patchOnePRDLink`, `forgesvc/prd_link_patch.go`).
  - *Autopilot:* label events (`ListIssueLabelEvents` in `detectOne`, `api/internal/poller/autopilot.go`).
  - *Comments out* (`CreateIssueNote`, five calls, all discarding the returned note): autopilot's refusal comment (`recordThenComment`, `poller/autopilot.go`), CI autofix start and halt comments (`detectOne`, `poller/ci_autofix.go`), the merge request rework halt comment (`detectOne`, `poller/mr_review_watch.go`) and the terminal comment (`maybeTerminalComment`, `api/internal/runlifecycle/lifecycle.go`).
- **No call goes through the union.** The same inventory shows 66 code-host calls, all through `CodeHost`, and none of either kind through `forge.Forge`. The connection factories still return `forge.Forge`; each caller narrows the value before its first call.
- **The privilege check is code-host only.** `VerifyToken`, `ProjectRole`, `DefaultBranchProtection` (`Check`, `CheckToken` and `checkRepo`, `api/internal/privcheck/checker.go`, all taking `forge.CodeHost`; `privcheck/service.go` at claim), fail-closed.
- **Mixed callers.** Comment intake reads issue comments (`ListIssueComments`) and checks each author against the repository (`RepositoryAuthorEligibility`, in `Assessment.Author`, `api/internal/issueinput/assessment.go`); merge request watchers read the MR from the code host and move the card on the issue side (`AutoMove`), both in `forgesvc/mr_watch.go`.
- **Issue identity is a number.** `issues.forge_issue_iid bigint` (`api/internal/store/migrations/00002_forge.sql:47-50`), `runs.issue_iid`, and one active run per issue on `(repo_id, coalesce(issue_iid, continued_issue_iid))` for issue runs and tasks continuing an issue (`00351_task_continued_issue.sql:11-14`).
- **Board columns are labels.** `board_columns.label_name` (`00002_forge.sql:39`); moving a card swaps labels through `UpdateIssueLabels` (`AutoMove`, `api/internal/forgesvc/service.go`), forge-first, with the `move_pending_since` marker (`api/internal/runlifecycle/lifecycle.go`).
- **Autopilot.** Maps the label adder to a uzi user through `forge_connections.human_username` (`00037_autopilot_mapping.sql:6-20`, `detectOne` in `poller/autopilot.go`).
- **Constraints a text key meets.** `issues.forge_issue_iid` is `bigint NOT NULL` with `UNIQUE (repo_id, forge_issue_iid)` (`00002_forge.sql:50`, `:59`), and `runs_kind_shape` requires `issue_iid IS NOT NULL` for `kind = 'issue'` (`00314_cross_check_claude_checker.sql`). An added `issue_key` column alone does not make an issue tracker issue storable.
- **The issue author is checked the same way.** Run creation assesses `Issue.AuthorForgeUserID` through `RepositoryAuthorEligibility` (`Assessment.Author`, `api/internal/issueinput/assessment.go`). An author that cannot be assessed gives the reason `permission_unknown`, and a scheduled start with that reason is not auto-approved (`createRun`, `api/internal/workersvc/service.go`).
- **The forge allowlist is deployment configuration.** `FORGE_ALLOWED_BASE_URLS` is rendered from the Helm value `forge.allowedBaseURLs` (`deploy/chart/values.yaml`, `deploy/chart/templates/api-configmap.yaml`) and passed through `docker-compose.yml`.
- **Disabling a repo drains nothing.** It flips `repos.enabled`; a sync or a run already in flight finishes.
- **Comment authors are checked against the repository.** Comment intake keeps a comment only when its author has repository access (`RepositoryAuthorEligibility`, a `CodeHost` method taking a forge user ID). An issue tracker comment's author is an issue tracker account, which that check cannot evaluate.

## Decisions

- **D1. The split.** `IssueTracker` is the eleven issue-side methods. `CodeHost` is the other 29. `RepositoryAuthorEligibility` and `UserExists` stay on `CodeHost`: they ask the forge about its accounts and permissions.
- **D2. `Forge` is the embedded union of the two.** The three forge drivers and `forgetest.BaseFake` implement `Forge`, so they satisfy both halves.
- **D3. Narrowest interface per caller.** A caller that needs both halves takes two parameters (`ch forge.CodeHost, tr forge.IssueTracker`), never the union.
- **D4. What enforces the split.** The interface declarations do: a method belongs to the half that declares it, and passing an `IssueTracker` where a `CodeHost` is required does not compile. The per-driver `var _ IssueTracker` / `var _ CodeHost` lines are readable markers only; implementing `Forge` already implies them. No reflection test in M1 or M2.
- **D5. A repo resolves its two sides.** `IssueTrackerForRepo` and `CodeHostForRepo` (`forgesvc`). Through M2 both return the repo's forge driver. From M4, `IssueTrackerForRepo` returns the linked issue tracker's driver. `CodeHostForRepo` always returns the forge driver (S2).
- **D6. Issue tracker drivers live in their own package**, `api/internal/issuetracker`, not in `forge`. A driver is `forge.IssueTracker` plus what a connection needs: verify the token, list projects, list tags and, for a named project, list its states. A new issue tracker is one kind constant, one registry entry, one driver and its fake.
- **D7. Every driver ships with a fake and contract tests, and the contract suite runs against the real drivers.** One contract suite covers every `IssueTracker` implementation. Each one is built by its production constructor and talks HTTP to a fake server: the YouTrack driver to an in-process HTTPS fake, the three forge drivers to `e2e/forge-fake`, as `api/internal/forge/fake_contract_test.go` already does for forge operations. An in-memory fake in the style of `forgetest.BaseFake` (loud `ErrNotStubbed` defaults) also ships, for the tests of callers; passing the suite on it is not evidence of parity.
- **D8. Issue tracker connections are separate from forge connections.** Per user, kind and base URL, with the rules in S3. A repo links to one connection and one issue tracker project. NULL means the forge is the issue source.
- **D9. Moving a card is an `IssueTracker` method.** `MoveToColumn`. The forge drivers implement it with one shared helper that does today's label swap in one `UpdateIssueLabels` call. YouTrack maps a column to a value of the project's State field. The column-to-state mapping is a side table, so `board_columns.label_name` keeps its meaning for the code that reads it.
- **D10. YouTrack ids are adapted inside the driver; the neutral types do not change.** `IssueTracker` takes `projectID int64` and `issueIID int64` and returns `IssueNote.ID int64`, while YouTrack project, comment and account ids are strings. Changing the neutral types would cost the three forge drivers and every caller and give the forges nothing, so the driver adapts, and M3 implements exactly this:
  - *Project.* A YouTrack driver is built for one project. It holds that project's string id and short name, and the one `projectID int64` value it answers to: the repo's forge project id, which is the number every caller already passes for that repo. An `IssueTracker` method called with a different `projectID` is refused before any request. This catches mismatched forge project numbers; the owner-scoped repo resolver must also bind the driver to the correct repo and issue tracker connection, including when two connections expose the same forge project number. M4 tests that collision. The `IssueTracker` methods of a driver built without a project return an error.
  - *Connection operations.* Verify the token, list projects and list tags need no project, so a connection can be set up before one is chosen. Listing states is project-scoped, because states belong to a project: it takes the project it reads, picked from the list of projects, and cannot be called without one.
  - *Issue.* `issueIID` is YouTrack's `numberInProject`. It satisfies the `NOT NULL`, uniqueness and `runs_kind_shape` constraints above with no change to them.
  - *Comment.* The driver does not derive a number from a YouTrack comment id. YouTrack documents these ids as strings and guarantees no grammar for them. `CreateIssueNote` returns `IssueNote.ID` 0 on YouTrack, and no caller reads that field today: all five call sites discard the returned note.
  - *Accounts.* The numeric author id of an issue or comment is 0, which the existing code already reads as unresolved; the login is carried as text. D11 says what an unresolved author may do.
  - *Every read and write checks where it landed.* The issue reached must be in the driver's project and carry the requested number; otherwise the driver returns issue-not-found. An issue moved to another YouTrack project gets a new number there, so in the old project it is not found, even where YouTrack still resolves the old readable id.
  - *A live run whose issue was moved away.* Its later issue writes (card move, comment, description patch) are skipped and logged. The code-host side of the run (branch, merge request, CI, settle) continues. The next sync removes the issue from the repo's cache.
  - *Storage, decided in M4.* A nullable `issue_key text` carries the readable id (`ABC-12`) for display and references, NULL for forge issues. M4 records the column and its constraints in the Decision Log before code.
- **D11. An issue tracker author is unproven until mapped, for comments and for the issue itself.** The forge's `RepositoryAuthorEligibility` is never asked about an issue tracker account. An issue tracker author counts as eligible only when the account maps to a uzi user who owns or can access the repo. Until then:
  - *Comments* are withheld from the run, exactly like a comment whose author lookup failed.
  - *The issue author* is assessed as `permission_unknown`. A manual start proceeds, because a uzi user chose that issue. A scheduled start is created but never auto-approved: its plan waits for a person. M4 tests the manual and the scheduled start separately.

  How an issue tracker account is proven to belong to a uzi user is designed in M4 and recorded in the Decision Log before code; the fail-closed default holds until then.
- **D12. Autopilot is not offered on a repo linked to an issue tracker in this PRD.** It maps the label adder to a uzi user through the forge identity, which an issue tracker account does not have. It stays unchanged on forge-backed repos.
- **D13. A repo's issue source is chosen before the repo is first enabled, and does not change.** Linking is accepted only for a repo that has never been enabled. Such a repo has had no sync and no run, so no stored reference and no operation in flight can meet the change. M4 verifies in code that neither can start on a repo that is not enabled, and refuses the link when anything else already stores an issue number for the repo, such as a schedule pinned to an issue. Relinking and unlinking are out of scope for this PRD, and so is linking a repo that is already in use; moving a repo in use to an issue tracker is left for a later PRD. A repo with no link is unaffected (Goal 5).
  - *"Never enabled" is a stored fact, not a reading of `repos.enabled`,* because disabling drains nothing. M4 adds a marker set in the transaction that first enables a repo and never cleared. Its migration marks every repo that exists at that moment as previously enabled, because current rows cannot prove otherwise: a repo that was used, then disabled, can be left with no cached issue, run or schedule. Only a repo created after the migration can be linked.
  - *The link and the first enable take the same repo row lock,* so one of them sees the other: either the link lands first and the repo starts on YouTrack, or the enable lands first and the link is refused.
  - *An issue tracker connection with a linked repo cannot be deleted.*
  - A forge issue 12 and a YouTrack issue 12 can therefore never belong to the same repo.
- **D14. YouTrack tags are not project labels, and the eligibility tag is chosen by id.** A YouTrack tag belongs to the user who created it, is private unless shared, and can share its name with another user's tag. So:
  - *Choice.* When a repo is linked (M4), the admin picks the eligibility tag from the tags the connection's token can see. uzi stores the tag's id with the link, and its name for display only.
  - *Who must be able to use the tag.* YouTrack separates who can see a tag from who can apply it (`readSharingSettings` and `tagSharingSettings` on the tag). Two parties need the right to apply it: the connection's account, because uzi puts the tag on issues it creates, and the people who start work by tagging an issue. uzi cannot know who those people are, so the admin decides it in YouTrack and confirms it at link time.
  - *Verification at link time, by reading only.* uzi reads the tag and its two sharing settings; no issue is modified. The link is refused, with what to change in YouTrack, unless the connection's account can see the tag and is covered by its use settings (it owns the tag, is named in them, or they cover all users), and the use settings cover someone other than the connection's account. The link screen then shows who can see and who can use the tag, and the admin confirms that this is who should start work.
  - *Matching.* Intake selects issues by the stored tag id. Another tag with the same name is ignored.
  - *Other tags uzi writes* (on issues it creates, such as findings). `EnsureLabels` resolves a name only to a tag owned by the connection's account and creates it there when missing, with the sharing settings of the eligibility tag. It never resolves a name to another user's tag and never creates the eligibility tag. When uzi writes the eligibility label itself, on an issue it creates or moves, that name resolves to the stored tag id, whoever owns the tag, and never to a same-named tag of the connection's account.
  - *`ListLabels`* returns the tags the token can see. Board columns are not tags on YouTrack (D9).
  - *In the contract suite* the shared rule is "a label is addressed by name within the project the driver serves". The YouTrack subject is seeded with an eligibility tag owned by another user and shared, and a second tag of the same name owned by the connection's account, and the suite shows that reads, `EnsureLabels` and `CreateIssue` only ever use the first.

## Feature parity on a repo linked to an issue tracker

- Bring tagged issues into uzi: the eligibility tag chosen at link time (D14), in the linked project. Milestone M4.
- Give the run the issue's title, body and comments: yes, comments subject to D11. M4.
- Auto-approve a scheduled run's plan: only when the issue author is mapped (D11).
- Let the agent read an issue through the worker API: yes, same routes. M4.
- Show the board and move cards: yes, through the State field (D9). M5.
- Close or reopen an issue: yes, through the State field. M5.
- Create an issue (board, findings, judge, review, self-improve, proposals): yes, in the linked project. M5.
- Write comments on the issue (today's automatic templates): yes. M5.
- PRD link patch in the issue description: yes. M5.
- Eligibility by assigning the issue to the uzi bot: not offered in this PRD; the tag is the only signal.
- Autopilot: not offered (D12).
- Change a repo's issue source after it was first enabled: not offered (D13).
- Privilege checks, claim and settle, pipeline sync, merge request watchers, clone and push: unchanged, on the forge (S2).

## Milestones

Progress (one PR per milestone, each merged before the next starts):

- [x] **M1:** the `IssueTracker` and `CodeHost` interfaces
- [x] **M2:** every caller on its half. The type-checked inventory shows 45 issue-side calls through `IssueTracker`, 66 code-host calls through `CodeHost` and none through `forge.Forge`, and handing `privcheck` an `IssueTracker`-only value fails to compile.
- [ ] **M3:** issue tracker connections and the YouTrack driver
- [ ] **M4:** issue identity, the repo link and issue intake from YouTrack
- [ ] **M5:** the board and issue writes on YouTrack
- [ ] **M6:** web and CLI surfaces, docs and live acceptance

### M1: the `IssueTracker` and `CodeHost` interfaces

D1, D2 and D4. One file per interface with the types only it uses (`issuetracker.go`, `codehost.go`); `forge.go` keeps the shared pieces.

- **Acceptance:** pure moves (`git diff -M` shows no changed lines in moved code); no caller changes; every existing test passes unchanged.

### M2: every caller on its half

D3 and D5.

- **Blocked by:** M1.
- **Acceptance:** every issue-side use in the refreshed inventory (45 calls, 47 at r1) goes through `IssueTracker`; `privcheck` cannot be handed a value that has only the issue side; existing tests change only in two mechanical ways (a fake's parameter type, and passing the same fake as both halves).

### M3: issue tracker connections and the YouTrack driver

D6, D7, D8, D10 (the id adapter), D14 (the tag rules inside the driver) and S3. The `api/internal/issuetracker` package, the YouTrack driver for every `IssueTracker` method plus verify, list projects, list tags and list a project's states, its fakes, the contract suite, the connection table and the allowlist. No repo can link to an issue tracker yet, so no behaviour changes.

- **Deployment wiring for S3.** The issue tracker allowlist gets a Helm value rendered into the API ConfigMap, with the same https-only validation as `forge.allowedBaseURLs`, and a pass-through in `docker-compose.yml`. It is empty by default in both. It does not feed the worker egress list, because the worker never calls an issue tracker (S5).
- **ADR.** The ADR for the issue tracker seam ships with this milestone, where the package (D6) and the connections (D8) become real: adr/4173-issue-tracker-seam.md.
- **Blocked by:** M2.
- **Acceptance:** the contract suite passes on the real YouTrack driver against its HTTPS fake and on the three real forge drivers against `e2e/forge-fake`; the same-name tag case of D14 passes; a call with a `projectID` other than the driver's and an issue found in another project are each an error; the tag's sharing settings are parsed from a response recorded from a real YouTrack; with the allowlist empty no connection can be created and uzi still boots; the chart renders the allowlist and refuses a non-https entry; a redirect, an oversized body and an error carrying the token are each refused or scrubbed in a test.

### M4: issue identity, the repo link and issue intake from YouTrack

D5, D10 (storage), D11, D13, D14 (choice and verification of the tag) and S2. The storage decision and the author mapping (both recorded in the Decision Log before code), the never-enabled marker, the repo link to a connection, a project and an eligibility tag, `IssueTrackerForRepo` returning the issue tracker, sync on the poll cadence, run creation from an issue tracker issue.

- **Blocked by:** M3.
- **Acceptance:**
  - a YouTrack issue carrying the eligibility tag appears on the board and starts a run that receives its title and body, and an issue carrying only a same-named tag of another user does not;
  - an issue tracker comment from an unmapped author is withheld;
  - with an unmapped issue author, a manual start proceeds and a scheduled start is created without auto-approval, each in its own test;
  - two repos on different forge connections that expose the same forge project number, one linked to YouTrack and one not, each resolve to their own issue source: a call for one never reaches the other's driver (D10);
  - the privilege check and claim still run against the forge and fail closed with the issue tracker unreachable;
  - a repo with no link is byte-for-byte unchanged in its forge calls (recorded-call test);
  - linking is refused for every repo that existed before the migration and for a repo enabled since, including one that is disabled now; it is refused for a tag the connection's account cannot apply or that nobody else can apply, and the check modifies no issue; a link racing the first enable ends in exactly one of the two outcomes of D13; a connection with a linked repo cannot be deleted;
  - when a run's issue is moved to another YouTrack project, its issue writes are skipped and its merge request still completes (D10).

### M5: the board and issue writes on YouTrack

D9. `MoveToColumn` on all four drivers, the column-to-state mapping, close and reopen, issues uzi creates, today's comment templates and the PRD link patch.

- **Blocked by:** M4.
- **Acceptance:** the recorded-call test shows identical label calls on the three forges; a card move changes the YouTrack State and a failed move snaps back; closing never lands on a state that means "won't fix".

### M6: web and CLI surfaces, docs and live acceptance

Settings → Issue trackers, the CLI commands, a setup page under `docs/`.

- **Blocked by:** M5.
- **Acceptance (maintainer-owned, live):** connect, link, tag, run, board move and close verified on a real YouTrack from a hosted Kubernetes install, with the allowlist set through the chart, and a regression pass on GitHub, GitLab and Forgejo.

## Testing decisions

- **In a uzi run, the in-run boundary of `AGENTS.md` "Run economy" applies:** `task check:api` plus `go test -race -count=1` on the affected packages, with each focused regression test watched red then green, `task check:web` plus the touched tests when web changes, and the companion checks for the files changed (`check:changelog-entry`, the migration checks, `docs:sync` with `check-docs:web`, `check:token-literals`). M3 to M5 touch credentials and untrusted input, so `task scan:secrets` also runs in the run and its canaries-detected line is part of the evidence. Token-shaped test strings are assembled at runtime. The full `gate:*` targets are the PR's CI on the exact head.
- **The E2E harness follows every changed issue or forge call in the same PR** (`AGENTS.md`, "A product contract change updates the E2E harness in the same PR"): the old identifiers are searched across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`, stale expectations and fake routes are updated, `task test:e2e-lib` and `task test:forge-fake` run, and the PR names the affected phases left unrun.
- **No change in behaviour on forge-backed repos:** the existing API, web, agent and E2E suites pass; M2's two mechanical test edits are the only ones allowed without a finding.
- **Parity is the contract suite on the real drivers (D7)**, not a per-driver checklist and not a run against in-memory fakes.
- **Hosted Kubernetes is verified, not only compose:** the chart test in M3 and the live acceptance in M6.

## Risks

- **Size of M2.** 54 production and 31 test files, mechanical. A type-checked inventory of forge calls was the guard.
- **Drift from `main` between milestones.** Each milestone's branch is rebased on `main` before its PR.
- **YouTrack specifics leak into the seam.** The id adapter (D10) and the tag rules (D14) stay inside the driver, the contract suite runs on all four implementations, and the second issue tracker (Asana) is the real test of the seam.

## Decision Log

- **2026-11-05: the interface is named `IssueTracker`.**
- **2026-11-10: one file per interface.** `issuetracker.go` and `codehost.go`; r1 had both interfaces in `forge.go`.
- **2026-11-10: no reflection test.** The interface declarations enforce the split (D4).
- **2026-11-10: M2 test edits.** Fakes may change a parameter type, and mixed callers' test call sites pass the same fake twice. Nothing else in a test changes.
- **2026-11-10: M2 wires `IssueTrackerForRepo`/`CodeHostForRepo` (D5) where a repo is in hand without new test fakes.** They are `forgesvc.(*Service)` methods in `forgesvc/repo_sides.go` taking `(ctx, repoID, forgeType, baseURL, tokenCiphertext)` (the caller's own owner-scoped lookup, never the unscoped `GetRepoByID`), used by the repo-scoped handlers and the merged-predecessor check. `workersvc`, `schedsvc`, `runlifecycle`, `privcheck`, `provisionsvc` and `seed` keep their own `ForgeForConnection` factory interfaces (adding the resolvers there would add methods to about 30 test fakes) and narrow at the first use; M4 routes them through the resolvers. Connection-scoped handlers, the run-scoped worker forge proxy (`resolveForgeRun`), `ProjectSyncService`'s own factory and the poll loop build one driver and narrow it at each use or pass it as both halves.
- **2026-11-10: the PRD link patch takes both halves.** D3 placed it on `IssueTracker`, but it also reads the merge request (`GetMergeRequest`), so `SyncPRDLinkPatches` takes `(ch forge.CodeHost, tr forge.IssueTracker)`. Comment intake (`issueinput.Fetch`), the autopilot, CI-autofix and MR-rework detectors, the MR state watcher, self-improve and the worker get-issue route are mixed the same way.
- **2026-11-10: "`privcheck` compiles against `CodeHost` only" means every privcheck parameter, local and forge call is `CodeHost`.** Its `ForgeBuilder` factory interface still returns `forge.Forge`, converted to `CodeHost` straight after the build; handing privcheck an `IssueTracker`-only value is a compile error.
- **2026-11-10: scope after the maintainer's review.** YouTrack becomes part of this PRD and drives the identity design. The living comment, the outbox, withdrawal, comments to the agent and `/uzi` commands move out. Asana follows in its own PRD.
- **2026-11-11: the issue source is fixed before first enable (D13).** Review of the M1 PR found that project-local numbers let an old run or pinned schedule resolve to an unrelated issue after a repo changes its issue source. The first answer was a per-repo fence over run creation, sync and every issue write. The maintainer proposed the simpler policy: choose the source before the repo is first enabled, and leave relinking out. It holds, because a never-enabled repo has had no sync and no run, provided "never enabled" is stored rather than read from `repos.enabled` and the link and the first enable share the repo row lock. The fence is dropped.
- **2026-11-11: id adapter instead of new neutral types (D10).** The driver is built for one project, `issueIID` is `numberInProject`, the driver answers only to the repo's forge project id, `IssueNote.ID` is not derived and account ids are 0. Decided before the M3 driver; only the `issue_key` column waits for M4.
- **2026-11-11: the eligibility tag is chosen by id (D14)**, because YouTrack tags are user-owned, private by default and not unique by name.
- **2026-11-11: an unmapped issue author is `permission_unknown` (D11).** Manual start proceeds, scheduled start is never auto-approved.
- **2026-11-11: the contract suite runs on the real drivers (D7), the allowlist is wired through the chart and compose in M3, and the ADR moves from M6 to M3.**
- **2026-11-11: second review pass.** Every repo that exists at the M4 migration counts as previously enabled (D13). The driver refuses a `projectID` other than its own, and listing states is project-scoped (D10). The eligibility tag is checked through its see and use settings, read-only (D14). No number is derived from a comment id (D10).
- **2026-11-11: the `projectID` guard is not the whole binding (D10).** `repos` is unique on `(connection_id, forge_project_id)`, so two connections can expose the same forge project number. The guard catches a mismatched number; the owner-scoped repo resolver binds the driver to the right repo and issue tracker connection, and M4 tests the collision.
