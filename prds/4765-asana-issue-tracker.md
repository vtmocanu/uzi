# PRD #4765: Asana as an external issue tracker

**Issue:** #4765

**Priority:** Medium

**Status:** Draft r3.3, 2026-11-27. Checked against the Asana API reference, fact-checked against the code at #4173 M6 (PR #4757, `d77731b2b`), and approved by an architecture review after one fix. Not yet reviewed by the maintainer. Three Asana facts the reference does not settle are marked *(live check)* and are confirmed on a real workspace before the code that relies on them.

**Delivery:** one PR per milestone, each merged before the next starts. No implementation or validation step touches `.github/workflows/**`.

**Builds on:** PRD #4173 (external issue trackers, YouTrack first), M1 to M6. #4173 names Asana as the second issue tracker and "the real test of the seam".

## Problem

Teams that plan in Asana cannot hand that work to uzi without copying every task into the forge. #4173 made an issue tracker pluggable, but only YouTrack is plugged in, and several parts of the seam turn out to be YouTrack-shaped:

- **No issue number.** An Asana task is a `gid` string, global across Asana, with no per-project number and no readable key. `IssueTracker` takes `issueIID int64`, and `issues.issue_key` is `<SHORT>-<n>` with `n = forge_issue_iid`.
- **Ids in another grammar.** The kind-neutral validators (`ValidProjectID`, `ValidTagID`) accept only YouTrack's `0-1` form (`entityIDRE` in `youtrack.go`), so an Asana project or tag gid is refused at link, in `DriverFor` and in the States routes.
- **No State field.** A task is open or completed (a boolean), and a board column is a *section* of the project.
- **Tags belong to a workspace**, not to a user, while the link applies YouTrack's tag sharing rules (`AccountCanApply`, `OthersCanApply` in `tag.go`) to every kind.
- **Tasks can live in several projects**, in a different section in each.
- **No public inbound URL.** uzi has none, so Asana webhooks are out; intake polls, as it already does for YouTrack.

## Goal

1. A repo can take its issues from an Asana project, while its code, merge requests and CI stay on its forge.
2. Feature parity with a YouTrack-linked repo (the #4173 parity list), or each gap is listed below as not offered on Asana yet.
3. No change to the neutral `IssueTracker` method signatures or to `forge.Issue` / `IssueNote`, and no behaviour change for forge-backed or YouTrack-linked repos.
4. The YouTrack-shaped parts of the seam become kind-neutral in a first milestone (A1), with YouTrack unchanged, before any Asana code.

## Security invariants

#4173's S1 to S5 hold unchanged: the Asana driver implements `IssueTracker` only, code-host checks stay on the forge, the token is sealed in `secretbox` with no reveal endpoint, issue tracker text is untrusted input, and the worker never holds the Asana token. In addition:

- **SA1. Asana is opt-in like any other issue tracker.** Asana has one public host, `https://app.asana.com`, and it must still be on `ISSUE_TRACKER_ALLOWED_BASE_URLS`, which is empty by default. Redirects are not followed (`issuetracker/httpclient.go`).
- **SA2. A connection is bound to one workspace.** A personal access token acts as its user, with no scope of its own; OAuth scopes exist, but personal tokens are what uzi accepts. The workspace is stored on the connection (A4) and every driver call, connection-level ones included, is confined to it: a project, tag or task outside it is refused. A reconnect that would change the workspace of a connection with a linked repo is refused. The docs recommend a dedicated Asana account that belongs only to the projects uzi should see.
- **SA3. Asana-supplied URLs are untrusted.** `permalink_url` is stored and shown only after the same checks as a forge issue's `web_url` (https, the Asana host, no credentials, no control characters).

## Out of scope

- Everything #4173 left out: the living status comment, retried comment writes, withdrawal, comments that reach the agent, `/uzi` commands, autopilot on a linked repo (D12), relinking a repo in use (D13).
- **Asana webhooks and the events API.** Webhooks need a public callback. The events API is pollable, but its sync tokens expire after 24 hours and it does not fit the existing full-listing sync; it can be a later optimisation.
- **Subtasks**, unless the subtask is itself a member of the linked project.
- **Status in a custom field.** Sections and completion only.
- **More than one Asana workspace per user.** `issue_tracker_connections` is unique on `(user_id, kind, base_url)`, and Asana has one base URL, so a user has at most one Asana connection, hence one workspace.
- **The Asana permalink in merge request bodies.** It would need a new claim field and an agent change gated like `issue_key_v1`. On Asana, uzi's lifecycle comments on the task carry the merge request link instead.
- **The PRD link patch in the task description** (A11).
- **Trusted Asana text**, and with it scheduled auto-approval on Asana (A10): it needs a provenance record a story deletion cannot erase.

## Current state (at `d77731b2b`)

- `issuetracker/kind.go`: `KindYouTrack` is the only kind; `SupportedKinds` drives the connect UI. `registry.go`: `Constructor func(Options) (Driver, error)`; a driver is built per call (`IssueTrackerForRepo`, `issuetrackersvc.DriverFor`), so it keeps nothing between requests.
- `issuetracker/driver.go`: `Driver` is `forge.IssueTracker` plus `VerifyToken`, `ListProjects`, `ListTags`, `ListStates(ctx, projectID string)` and `CheckStateField(ctx, ProjectRef)`. The linked lane also needs three optional interfaces found by type assertion: `issuesource.IssueStateLister` (`ListIssuesWithState`; the sync fails without it) and `issueinput.AuthorMapper` / `IssueTextMapper` (`MapsAuthor`, `IssueTextMapped`) (`forgesvc/issue_write_guard.go`).
- `issuetracker/options.go`: `Options` and `Binding` carry the project id, short name and tag; no workspace, no store access. Connection-level calls (`ListProjects`, `ListTags`, the tag read in `LinkRepo`) use an empty `Binding`. The package imports only `forge`, `httptransport` and `issuesource`.
- Migration `00424`: `issue_tracker_connections.kind CHECK (kind IN ('youtrack'))`, `UNIQUE (user_id, kind, base_url)`, no workspace column. `connect.go`: `errReconnectChangesAccount` refuses a reconnect that changes the account of a connection with a linked repo.
- Migration `00474`: `repos.issue_tracker_project_short` is 1 to 44 characters, `^[A-Za-z][A-Za-z0-9_]*$`; `issues.issue_key` is at most 64 characters, `^[A-Za-z][A-Za-z0-9_]*-[1-9][0-9]*$`, and its suffix must equal `forge_issue_iid` (so numbers start at 1). Nothing makes `repos.issue_tracker_project_id` unique across repos.
- `issuetrackersvc/link.go`: `LinkRepo(ctx, userID, repoID, connID, projectID, tagID)` is owner-scoped, takes the short name from `ListProjects`, and refuses a tag unless `AccountCanApply` and `OthersCanApply`.
- `issuetrackersvc/driver.go`: `checkStateField` turns `ErrStateMachineField` and `ErrStateFieldUnreadable` into 409s with YouTrack-worded remedies; any other error is a 502.
- `forgesvc/linked_issue_sync.go`: every pass lists the eligibility tag's issues with no `UpdatedAfter`, caches resolved ones as closed, and evicts every cached row the listing did not return (`DeleteIssuesNotIn`). `ListIssues` is also called outside intake: the worker route (`handler/worker_forge.go`), the proposal digest (`schedsvc/scheduler.go`) and findings dedup (`forgesvc/service.go`).
- `forgesvc/prd_link_patch.go`: the PRD link is a find-and-replace through `UpdateIssueDescription`; `IssueWriteSkippable` already settles `ErrIssueWriteNotOffered`.
- `issuetracker/issuetrackertest/contract.go`: the contract suite covers update, create, move, state, comments and label events, with per-subject skips.
- `issuetracker/redact.go`: a 429 is classed `ErrorClassRateLimited`; nothing reads `Retry-After`. `pagination.go`: `paginate` is skip/top with item, page and byte ceilings.
- State mapping: `repo_issue_tracker_states` and `repo_issue_tracker_column_states`, by name, `UNIQUE (repo_id, state_name)`; `"completed"` is in `closableNames` (`state_rules.go`). Close is `SetIssueState`; reopen is `MoveToColumn` with `ColumnMove.Reopen`; a non-reopen move of a resolved issue is refused (`ErrIssueResolved`).
- The agent renders a linked run's reference as `Related to ABC-12.` with no closing keyword (`agent/src/pr-description.ts`, `issueArm`).
- `FORGE_POLL_INTERVAL` is one global interval for every repo.

## Decisions

- **A1. Task numbers: allocated by uzi per Asana project, adapted inside the driver.** Asana has no number to adapt, and the reference describes `gid` only as a string id.
  - *Why the driver adapts.* Changing the neutral id type would touch `runs.issue_iid`, `issues.forge_issue_iid`, schedules, the worker routes, the agent mirror, the CLI and all three forge drivers. Allocating outside the driver does not work either: the sync is not the only source of numbers, since `CreateIssue` callers and the worker route also hand numbers on to code that stores them. So the translation sits where tasks are surfaced, as #4173 D10 did for YouTrack.
  - *Keyed by the tracker, not the repo.* A table `issue_tracker_task_refs (connection_id, project_id, iid, external_id)`, with `UNIQUE (connection_id, project_id, iid)`, `UNIQUE (connection_id, project_id, external_id)`, `iid >= 1`, and `ON DELETE CASCADE` from the connection. It is Asana's missing `numberInProject`: two repos linked to one project see one number per task, and a repo removed and listed again keeps its numbers. It stores no repo issue number, so it needs no D13 trigger and takes no lock on `repos`.
  - *Allocation.* `INSERT … ON CONFLICT (connection_id, project_id, external_id) DO NOTHING` with `iid = max + 1`, retried on a conflict on `iid`, in batches.
  - *The injected contract.* `Options` gains an optional `TaskRefs` with three methods: `Numbers(ctx, gids) (map[gid]iid, error)` allocates; `Lookup(ctx, gids) (map[gid]iid, error)` resolves known gids without allocating and leaves unknown ones out; `External(ctx, iid) (gid, error)` resolves a number to its gid. `DriverFor` builds it from the store for the connection and project; it is nil for YouTrack. An Asana driver built with a project binding and nil refs refuses construction.
  - *Where numbers are allocated: exactly two places.* `ListIssuesWithState`, after the bound-project filter, and `CreateIssue`, after the task exists. Every other listing (`ListIssues`, reached from the worker route, the digest and findings dedup) resolves numbers through `Lookup` only and omits tasks that have none, so agent input never creates rows. Per-task methods only resolve; an unknown number is issue-not-found before any request.
  - *The `CreateIssue` window.* If the allocation fails after Asana created the task, the call returns an error and the task stays in Asana. If it carries the eligibility tag, the next sync numbers it. A caller that retries the create files a duplicate. This window is documented, not closed, as #4173 D10's moved-issue window is. It adds no reconciliation gap of its own: the finding-group marker reconciliation (`reconcileFindingGroupMarkers`) runs only inside `FullSync`, and a linked repo never runs `FullSync` (`SyncIssues` sends it to `syncLinkedIssues`), so on YouTrack today an in-flight or uncertain finding create is not matched by marker either. Closing that for linked repos is out of scope here, and the PRD for it would cover both trackers.
  - `issue_key` keeps its CHECK: `<SHORT>-<n>`, `n = iid`.
  - *Rejected:* parsing the gid as an `int64`. It fits today, but Asana guarantees no grammar for it, the key would read `ATL-1208765432109876`, and #4173 D10 already declined to derive numbers from string ids without a grammar.
- **A2. The key prefix comes from the project name.** Asana projects have no short name. The driver's `ListProjects` reports a `ShortName` made from the project name: its ASCII letters and digits, upper-cased, first character a letter, at most 10 characters, `TASK` when nothing is left. `LinkRepo` stays as it is. Nobody in Asana sees `ATL-12`, so the issue page and the board link the key to the task's `permalink_url` (SA3).
- **A3. References.** The merge request body keeps `Related to ATL-12.` with no closing keyword; the agent code is unchanged. uzi's lifecycle comments land on the task as stories, so the people working in Asana see the merge request link there.
- **A4. The workspace belongs to the connection.** A new nullable `issue_tracker_connections.workspace_id`, with a CHECK that an Asana row carries one (`kind <> 'asana' OR workspace_id IS NOT NULL`); `DriverFor` puts it in `Options`, so connection-level calls are confined to it too. `Binding` and `ValidBinding` are unchanged.
  - *Choosing it at connect.* `VerifyToken` reads the account's workspaces from `users/me`. With one, it is bound. With several, the connect request must name one of them by gid; there is no listing before the token is stored.
  - *Reconnect.* As `errReconnectChangesAccount`: a reconnect that changes the workspace of a connection with a linked repo is refused.
  - *Per call.* As #4173 D10, the driver answers only to the repo's forge project id. Every per-task read and write first checks that the task is a member of the bound project (`memberships`); a task removed from the project is not found, and a live run's later issue writes are skipped and logged.
- **A5. Board columns are sections; closed is completion.** The State mapping tables and rules are reused with no schema change:
  - `ListStates` returns the project's sections as openable States and one synthetic State `Completed`, resolved and closable; it passes `ClosableState` as is. This is a driver-internal adaptation under #4173 D10, not a seam type.
  - The open State and every column map to a section; the closed State is `Completed`. A task's State is read from its membership in the bound project only, never from another project's section.
  - A column move is `POST /sections/{gid}/addTask`, which removes the task from its other sections in that project. `SetIssueState` closed sets `completed: true`. A reopen (`ColumnMove.Reopen`) sets `completed: false` and adds the task to the target section. A non-reopen move of a completed task is refused with `ErrIssueResolved`, as on YouTrack.
  - Sections are matched by name. The reference does not say whether two sections of a project can share a name *(live check)*, so `CheckStateField` refuses a project with duplicate section names, or with a section named `Completed`, through a new sentinel `ErrStateNamesAmbiguous`, answered 409 with the project, the clashing name and the fix (rename the section). Nothing is written.
- **A6. Tags.** Asana tags are created in one workspace and stay there.
  - *Eligibility tag, by gid.* The owner picks it from the workspace's tags at link time; uzi stores its gid, and its name for display only. Intake, writes and reads of the eligibility tag use the gid.
  - *The link check is the driver's verdict.* A `Tag` carries `AccountCanApply` and `OthersCanApply` booleans that the driver computes (milestone A1). YouTrack fills them from today's rules in `tag.go`. Asana sets both for a workspace tag, after the live check confirms that another workspace member can apply it *(live check)*; if the check fails, the Asana driver reports `OthersCanApply` false and the link refuses the tag.
  - *Other tags* (findings, judge, review) resolve by name in the workspace and are created when missing, as forge labels are. YouTrack's owner rule (#4173 D14) existed because YouTrack tags are private to a user; Asana's are not.
  - *`ListLabels`, `EnsureLabels`:* the eligibility tag under its stored name plus the workspace tags uzi writes by name. *`ListIssueLabelEvents`:* not offered on Asana (only autopilot reads it, and autopilot is out of scope); the contract suite skips it.
- **A7. Intake keeps the existing full listing.** As #4173's linked sync, every pass lists the eligibility tag's tasks with no modified-since filter, caches completed ones as closed and evicts what the listing did not return. The Asana driver's `ListIssuesWithState` reads `GET /tags/{tag_gid}/tasks` with minimal `opt_fields`, keeps the members of the bound project, and numbers new ones (A1).
  - A task that loses the tag is evicted; its number is kept, so re-tagging it gives the same key. A completed task is cached as closed. A task that leaves the project is filtered out, then evicted.
  - *Limit.* The tag listing covers the whole workspace and all of its history, and the item, page and byte ceilings count before the project filter. Once a tag's history passes the item ceiling, every pass fails closed and intake stops. The docs recommend one eligibility tag per project.
  - *`ListIssues`* lists the bound project's tasks carrying the requested tag names (the eligibility tag when none is given), resolve-only (A1).
- **A8. Paging and rate limits, in the Asana driver.**
  - Paging: a cursor helper beside `paginate` follows `next_page.offset` with `limit=100` and the same ceilings. An expired offset fails the pass; the next pass lists again.
  - Rate limits: 150 requests per minute per token on a free workspace, 1,500 on a paid one, at most 50 concurrent reads and 15 concurrent writes; a 429 carries `Retry-After`, and rejected requests still count. The Asana driver honours `Retry-After` up to a short cap bounded by the request's `ctx`, because it runs inside handler requests such as a board drag; past the cap the call fails as rate-limited. The shared client and YouTrack's 429 handling are unchanged.
  - The poller, handlers and worker routes share one token concurrently. uzi does not throttle per token; it relies on the 429 handling above. `FORGE_POLL_INTERVAL` is global, so the docs give the request cost per pass.
- **A9. Comments are stories.** `ListIssueNotes` reads the task's stories with `created_by` in `opt_fields` and keeps `comment_added` ones; `CreateIssueNote` posts one. `IssueNote.ID` is 0 and the account id is 0, as on YouTrack; the author is `created_by.gid` as text.
- **A10. Author mapping, fail closed.**
  - `MapsAuthor`: an author maps exactly when it is the connection's account (`users/me` gid, read once per driver). A missing or null `created_by` (forms, rules, integrations) is unmapped. This gates comments (D11), as on YouTrack.
  - `IssueTextMapped` always reports `permission_unknown` on Asana. A task's story listing cannot prove who wrote its current title and body: Asana consolidates some stories, and an author can delete their own stories (`DELETE /stories/{gid}`), so a third party's edit can leave no visible trace even when every story shown maps to the connection's account. A manual start proceeds as on YouTrack; a scheduled start may plan, and its plan always waits for a person.
  - Trusted Asana text needs a provenance record that cannot be erased this way. Designing one is out of scope; until then scheduled auto-approval is not offered on Asana.
- **A11. uzi never writes the task body.** The driver reads `notes` (plain text), never `html_notes`. The reference does not say what writing `notes` does to rich text *(live check)*, so `UpdateIssueDescription` returns `ErrIssueWriteNotOffered` on Asana. `IssueWriteSkippable` already settles it; its log text ("the linked issue is gone") is reworded for this case. The contract suite gains a skip for it. The PRD link patch is therefore not offered on Asana.
- **A12. Creating a task.** `POST /tasks` with `projects: [project gid]` and the tags, then `addTask` to the open section, then the number (A1). The Tasks reference describes `memberships` as create-only, which allows setting the section at creation; A3 evaluates that single-call form against the two-call sequence, which works either way.

## Feature parity on a repo linked to Asana

| Feature | Asana | Milestone |
|---|---|---|
| Bring tagged tasks into uzi | Yes, by the eligibility tag's gid (A6, A7) | A2 |
| Give the run the title, body and comments | Yes, body as plain text, comments subject to D11 (A9, A10) | A2 |
| Auto-approve a scheduled run's plan | Not offered: the title and body are always `permission_unknown`, so the plan waits for a person (A10) | — |
| Agent reads the issue through the worker API | Yes, same routes | A2 |
| Issue key and link on the board and issue page | Key `ATL-12`, linked to the permalink (A2) | A2 |
| Board and card moves | Yes, through sections (A5) | A3 |
| Close and reopen | Yes, through completion (A5) | A3 |
| Create an issue (board, findings, judge, review, proposals) | Yes, in the linked project and its open section (A12) | A3 |
| uzi's comments on the issue | Yes, as stories, with the merge request link (A3) | A3 |
| PRD link in the description | Not offered (A11) | — |
| Asana link in the merge request body | Not offered (out of scope) | — |
| Eligibility by assigning to the uzi account | Not offered; the tag is the only signal, as on YouTrack | — |
| Autopilot | Not offered (#4173 D12) | — |
| Change the issue source after first enable | Not offered (#4173 D13) | — |
| Privilege checks, claim, settle, CI, merge requests, clone and push | Unchanged, on the forge | — |

## Milestones

### A1: the seam made kind-neutral (no Asana yet)

Only the changes a second kind needs before its first line of code. YouTrack's behaviour is unchanged, and its tests and the contract suite stay green.

- Id validation by kind: the registry entry carries the kind's project, tag and entity id validators, and `ValidProjectID`, `ValidTagID` and `ValidBinding` call them. YouTrack keeps `entityIDRE`.
- Tag verdicts computed by the driver: `Tag` gains `AccountCanApply` and `OthersCanApply`, filled by the YouTrack driver from the rules in `tag.go`; `link.go` and `handler/issue_trackers.go` read the booleans.
- Kind-neutral refusals: the 409 remedies (`StateMachineRefusal`, `StateFieldUnreadableRefusal`) come from the driver's error, not from YouTrack text in `issuetrackersvc`; YouTrack's wording is unchanged.
- **Acceptance:** no behaviour change on YouTrack or the forges; a test per seam change (the validators by kind, the tag verdict booleans, the remedy carried by the error).

### A2: connect, link and intake on Asana

- `KindAsana`, the registry entry, and one migration: the `kind` CHECK widened, `issue_tracker_connections.workspace_id`, and `issue_tracker_task_refs` (A1, A4).
- The driver's read side in `api/internal/issuetracker` (`asana*.go`): `VerifyToken` with the workspace choice (A4), projects with the derived short name (A2), tags (A6), sections as States and `CheckStateField` with `ErrStateNamesAmbiguous` (A5), `ListIssuesWithState` and `ListIssues` (A7), `GetIssue`, `ListIssueNotes` (A9), `MapsAuthor` and `IssueTextMapped` (A10), the cursor paging and `Retry-After` (A8).
- `TaskRefs`: the store-backed one, and an in-memory one for the contract suite.
- `asanatest`: an in-process HTTPS fake of the endpoints used, with cursor paging, 429 and `Retry-After`, multi-homed tasks, duplicate section names and a section named `Completed`.
- Link and intake: linking a never-enabled repo to a project and eligibility tag in the connection's workspace, with #4173 D13 unchanged; the linked sync; the key and permalink on the board and issue page.
- **Before code:** the three live checks (another member can apply a workspace tag, two sections can share a name, writing `notes` against rich text) on a real free workspace, by whoever holds one (the maintainer by default), recorded in the Decision Log.
- A2 is exercised through the API, the CLI and tests; the web connect form arrives in A3. The contract suite is A3's proof, because its read cases seed their data through `CreateIssue` and `EnsureLabels`.
- **Acceptance:**
  - `asanatest`-backed unit tests for every read method, the paging and `Retry-After`;
  - a differential test of the two `TaskRefs`: the same gid is idempotent, distinct gids get distinct numbers, numbering starts at 1, an unknown number is not found, `Lookup` never allocates and leaves an unknown gid out, and concurrent first sightings allocate exactly one number (on Postgres for the store one);
  - LiveDB tests for intake, eviction, a task removed from the project, a multi-homed task in another project's section, an untagged task, an unmapped author on a manual and on a scheduled start, a scheduled start on a task whose creator maps and whose every visible non-comment story maps (or which has none) that still does not auto-approve (A10), and a `ListIssues` call that allocates nothing.

### A3: board and writes, surfaces, docs and live acceptance

- `MoveToColumn`, `SetIssueState`, reopen, `CreateIssue` with its number, `CreateIssueNote`, and `UpdateIssueDescription` not offered (A5, A9, A11, A12).
- The full contract suite on the Asana driver. `caseUpdate` is split into a label update and a description update, so the label assertion still runs on Asana. The Asana subject skips exactly: the description update (A11), label events (A6), and the owner-tag cases `caseSameNameTag`, `caseRenamedEligibilityTag` and `caseListLabelsOnlyUsableTags` (`Subject.SameName` nil), because Asana resolves other tags by name (A6).
- Web: the connect form offers Asana, with the workspace choice when the account has several; the States panel shows sections and the fixed `Completed`. The CLI needs no new verb (`uzi issue-tracker list` and `uzi repo issue-tracker-states` are kind-neutral); `api/cmd/uzi` is checked for anything YouTrack-worded.
- `docs/issue-trackers.md`: an Asana section with the dedicated account (SA2), the allowlist entry (SA1), one eligibility tag per project (A7), the request cost per pass (A8), scheduled auto-approval not offered (A10) and the parity table.
- **Acceptance (maintainer-owned, live):** connect, link, tag, run, board move, close and reopen on a real Asana workspace from a hosted Kubernetes install, plus a regression pass on a YouTrack-linked repo and on GitHub, GitLab and Forgejo.

## Testing decisions

- The #4173 contract suite is the parity proof; the Asana fake ships with it (#4173 D7). Its skips are named and listed in A3.
- Every refusal (another project, another workspace, a task outside the project, an unknown number, duplicate section names, a section named `Completed`, a non-reopen move of a completed task, a reconnect changing the workspace, a driver built without refs) has a test that fails without its guard.
- A1's changes are tested on YouTrack first, so a seam regression shows before any Asana code exists.

## Risks

- **The seam still leaks YouTrack.** A1 holds what the checks found; the contract suite on a second driver may find more. Each one is fixed in the seam and recorded, never special-cased in callers.
- **A long-lived eligibility tag stops intake.** A7's ceiling fails closed. The docs recommend one tag per project; a later PRD can filter the listing server-side if teams need more.
- **Rate limits on a free workspace.** The tag listing keeps a pass small, and 429s are honoured; the live acceptance measures a real pass.
- **The key is uzi-only.** It is linked to the task wherever uzi shows it; the merge request body is the gap.

## Open questions for the maintainer

1. Should the owner choose the key prefix at link time (a new link field), or is the derived one (A2) enough?
2. Sections plus completion (A5), or should the first version also support an enum custom field as the status?
3. Is "PRD link not offered" on Asana (A11) acceptable, or should the driver patch `html_notes` with a sanitizer?
4. One Asana workspace per user (out of scope): acceptable for now?

## Decision Log

- **2026-11-27: draft r1.** Written after #4173 M6 (PR #4757) from the `issuetracker` seam and the #4173 decisions.
- **2026-11-27: r2 after two checks.** The Asana API reference and a fact-check against `d77731b2b` found r1 wrong in eleven places. r2 added a seam milestone (id validators by kind, the `kind` CHECK, a workspace, a kind-aware tag check, a kind-neutral 409, `Retry-After`, cursor paging, an injected `TaskRefs`), kept the existing full listing, derived the key prefix in the driver, and dropped the permalink from merge request bodies.
- **2026-11-27: r3 after an architecture review.**
  - The workspace moves from `Binding` to the connection and `Options`, so connection-level calls are confined too; it is chosen at connect from `users/me`, and a reconnect cannot change it under a linked repo.
  - Task numbers are keyed by connection and Asana project instead of repo, so two repos on one project agree and a re-listed repo keeps its numbers. This also removes the D13 trigger and the `repos` lock. `TaskRefs` takes `ctx`, works in batches, and is required when a project is bound.
  - Numbers are allocated only by `ListIssuesWithState` and `CreateIssue`; every other listing resolves only, so agent-driven reads never write.
  - `UpdateIssueDescription` is not offered on Asana instead of posting the PRD link as a comment, which contradicted the contract suite and needed a caller change.
  - Other tags resolve by name, as forge labels do. Tag sharing becomes driver verdicts instead of a `WorkspaceWide` flag, and the 409 remedies come from the driver's error.
  - `Retry-After` is scoped to the Asana driver and bounded by `ctx`. The claim that the driver sends one request at a time is dropped: drivers are built per call and cannot enforce it.
  - The milestones are re-sliced vertically: A1 seam prefactors, A2 connect, link and intake, A3 writes, surfaces, docs and live acceptance.
  - Also added: the tag listing's workspace-wide limit, the State read from the bound project's membership, the fail-closed edges of the text mapping, the differential `TaskRefs` test, label events as not offered, and an owner for the live checks.
- **2026-11-27: r3.1 after the re-review (approved with one blocking item, now fixed).** A2 no longer claims the contract suite's read cases: they seed through `CreateIssue` and `EnsureLabels`, which are A3. A3 lists the exact skip set and splits `caseUpdate`. An Asana connection must carry a workspace (CHECK). A2 is exercised through the API, the CLI and tests.
- **2026-11-27: r3.2 after CodeRabbit on #4766.** `TaskRefs` gains the read-only `Lookup`, which `ListIssues` needs to resolve without allocating. The `CreateIssue` window now states that finding-group marker reconciliation does not run on linked repos at all (`FullSync` only), so the window adds no new gap.
- **2026-11-27: r3.3 after the maintainer's review of #4766.** A10: a story listing cannot prove who wrote the text (Asana consolidates stories and authors can delete theirs), so `IssueTextMapped` is always `permission_unknown` on Asana and scheduled auto-approval is not offered; A2 gains the acceptance case where every visible story maps and auto-approval still stays off. A12: `memberships` is create-only, not read-only; A3 evaluates the single-call create.
