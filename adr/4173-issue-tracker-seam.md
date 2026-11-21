# ADR-4173: The issue tracker seam

**Status**: Accepted (PRD #4173, M5 landed; M6 pending)
**Date**: 2026-11-11 (updated 2026-11-18)
**Deciders**: the maintainer + agent team
**PRD**: [prds/4173-external-issue-trackers.md](../prds/4173-external-issue-trackers.md) — the PRD carries the milestones and the Decision Log; this ADR carries the durable shape of what M3 to M5 built and the invariants a later change must keep.

## Decision (summary)

Issue trackers other than the repo's forge sit behind a `Driver` in their own package, `api/internal/issuetracker`. A driver is `forge.IssueTracker` plus four connection operations. Connections live apart from forge connections (`issuetrackersvc`, table `issue_tracker_connections`) with their own base URL allowlist. The YouTrack driver adapts YouTrack's string ids inside itself, is built for exactly one project, and treats tags as user-owned objects chosen by id. From M4 a repo created after the M4 migration can link to one before its first enable (D13); a repo with no link behaves as before.

Shipped in M4: the repo link and `IssueTrackerForRepo` returning the linked issue tracker, with intake and runs from YouTrack issues. Shipped in M5: the board and issue writes on YouTrack. A card move goes through `MoveToColumn` and writes the issue's YouTrack State, through a per-repo column-to-State mapping set with `GET` and `PUT /api/repos/{id}/issue-tracker-link/states` (session-only, no UI); an unmapped column is never guessed (`ErrColumnNotMapped`, a 409 on a board drag). Close and reopen write the stored closed, open or mapped column States, and never land on a won't-fix State: closability is an allowlist of done-style names, checked when the mapping is saved and again against the live bundle before every State write. A column move refuses an issue YouTrack already shows as resolved unless it is the explicit reopen of a closed card (`ColumnMove.Reopen`), so an automatic move or a board drag never reopens an issue a person resolved there. A linked card's board column is read from its State and cached in `issues.issue_tracker_column`. Issues uzi creates on a linked repo are created in the linked project and carry the eligibility tag where the forge path would carry the uzi label. Comments and the PRD link patch reach the issue. The one issue write still refused is removing the eligibility tag (`ErrIssueWriteNotOffered`); `EnsureRepoLabels` is refused with a 409 before any request. Not shipped yet: the web and CLI settings surfaces (the mapping has no UI), docs, the live pass against a real YouTrack, and the issue detail page's column (all M6). There is no close-on-merge: a linked MR body is non-closing, so the issue stays in its column after the merge until a person closes it.

## Context

`forge.IssueTracker` was written for forge issues: `projectID int64`, `issueIID int64`, `IssueNote.ID int64`. YouTrack ids for projects, comments and accounts are strings, and a YouTrack tag is not a project label: it belongs to a user, is private unless shared, and can share its name with another user's tag. The three forge drivers and every caller depend on the neutral types, so the seam had to take YouTrack without changing them.

## Decisions

### D6. The package and the `Driver` seam

- `Driver` is `forge.IssueTracker` plus `VerifyToken`, `ListProjects`, `ListTags` and `ListStates(project)`. It never embeds `forge.CodeHost`, so passing a driver where a code host is required is a compile error (S1).
- A new issue tracker is one `Kind` constant, one entry in the `constructors` map in `registry.go` and one driver. There is no init-time registration.
- The package ships one shared in-memory fake, `issuetrackertest.BaseFake` (loud not-stubbed defaults, for the tests of callers), and each driver ships its HTTP fake (YouTrack: `youtracktest`, an in-process HTTPS server).
- `issuetrackertest.RunContract` is the one contract suite. It runs on the real YouTrack driver against its HTTPS fake, and on the three real forge drivers against `e2e/forge-fake` in contract mode. Passing it on an in-memory fake is not evidence of parity.

### D8. Separate connections

- The `issuetrackersvc` package and the `issue_tracker_connections` table hold connections per user, kind and base URL, apart from forge connections.
- The token is sealed with `secretbox` using the user, kind and base URL as AAD. The connection type the service returns has no token or ciphertext field, and there is no reveal endpoint.
- The base URL is https, scheme and host only (`strictBaseURL`).
- The allowlist `ISSUE_TRACKER_ALLOWED_BASE_URLS` is separate from `FORGE_ALLOWED_BASE_URLS` and empty by default, so no connection can be created until an operator sets it. It is wired through the Helm value `issueTracker.allowedBaseURLs` (setting both the value and `api.config.ISSUE_TRACKER_ALLOWED_BASE_URLS` fails the render, and a non-https, comma or whitespace entry is refused) and through a pass-through in `docker-compose.yml`. It never feeds worker egress, because the worker never calls an issue tracker (S5).
- `DriverFor` re-checks the strict URL form and the allowlist against the stored row before opening the token, so removing a host stops use of existing rows.
- Redirects are refused (`ErrRedirectRefused`), response bodies and listings are bounded, and every error passes the redactor.

### D10. The id adapter

- The driver is built for one project through `issuetracker.Binding{Project, ProjectID, EligibilityTag}`: all three set or none (`ValidBinding`). A driver built with none serves only the connection operations.
- Every `IssueTracker` method refuses a `projectID` other than the bound one with `ErrProjectMismatch` before any request. This catches a mismatched number only. Two connections can expose the same forge project number, so the owner-scoped repo resolver in M4 must still bind the right repo and connection, and M4 tests that collision.
- `issueIID` is YouTrack's `numberInProject`. Each per-issue call checks the issue reached is in the bound project and carries the number, so an issue moved before an operation reads as not found and is not written. YouTrack has no conditional write, so an issue moved after that check can still be written, and `UpdateIssueLabels` can send several tag writes after one check. This window is documented, not closed.
- `IssueNote.ID` is 0, not derived from YouTrack's string comment id. Account ids are 0, with the login kept as text.
- `ListStates` takes the project it reads, because states belong to a project.
- The readable id is `issues.issue_key` (`<SHORT>-<n>`), nullable with a CHECK, written only by the linked sync and NULL for forge issues.

### D14. The tag rules

- The eligibility tag is chosen by id. Its name resolves to the stored id whoever owns the tag, for reads and for writes. A rename in YouTrack changes nothing for a caller: no request carries a tag name (intake lists the tag's issues by id, below), and reads report the tag under the stored name.
- Any other name resolves only to a tag owned by the connection's account, which the driver learns from `users/me`. A missing one is created, owned by that account, with the eligibility tag's sharing settings plus that tag's owner as a named user. The eligibility tag is never created, and a drifted name never resolves to the eligibility id.
- Reads follow the same rules as writes: an issue's labels and label events report the eligibility tag and account-owned tags whose name differs from the stored eligibility name. They leave out all other tags, including an account-owned tag that shares the eligibility name.
- `ListTags` returns the tags the token can see, with owner and read/use sharing settings. `AccountCanApply` and `OthersCanApply` judge the use settings, and the owner counts as covered. A named group counts for others only with `UsersCount >= 2`; the anonymous response recorded from a real YouTrack (`testdata/youtrack_tags_recorded.json`) omits `usersCount`, so on such a response only the All Users group counts. This fails closed. The link step uses these to refuse a link (M4). Both are facts about tag sharing only: they do not prove that anyone can edit issues in the linked project, which uzi does not read. That is a limitation the admin confirms at link time.
- Intake lists the eligibility tag's issues by id (`GET /api/tags/{id}/issues`), filtered on the client by the stored tag id, and `ListLabels` returns only the tags the driver can use: the stored eligibility name plus tags the connection's account owns.

Port note (2026-11-17): M4 was rebased onto the final M3. Of the M3 review changes only two are replaced, both by the maintainer's M4 preconditions: the driver does not read the eligibility tag's live name for a server query (intake lists the tag's issues by id), and `ListLabels` returns only the usable tags. The rest of the review stands, including the token-holding account refusal, the context-path base URL refusal, the owner added to a created tag's sharing and label reads that follow the write rules. The PRD Decision Log entry of the same date has the detail.

## Invariants a future change must keep

- **S1. An issue tracker never gains code access.** `Driver` embeds `forge.IssueTracker`, not `forge.CodeHost`; a driver passed as a code host does not compile.
- **S2. Code-host checks stay on the forge.** The privilege check, the protected-branch check and the worker-held forge credential act on the repo's forge connection and fail closed. M4 tests that a linked repo keeps them on the forge and that they fail closed with the issue tracker unreachable.
- **S3. Credentials follow the forge rules.** Sealed token with AAD and no reveal path, https-only base URL on the separate empty-by-default allowlist re-checked in `DriverFor`, redirects refused, errors redacted. The redirect, oversized-body and token-in-error cases each have a test in `api/internal/issuetracker`.
- **S4. Issue tracker text is untrusted input.** The driver bounds what it reads (response bytes, listing pages and items, nested items, label name length). YouTrack titles, descriptions and comments go through the same `issueinput` fencing as forge text; an oversized description is refused and an oversized comment by the connection's own account is cut to the same bound as on a forge, while an unmapped author's comment is withheld; M4 tests each.
- **S5. The worker never holds an issue tracker token.** The allowlist does not feed worker egress, and the token is opened only inside `DriverFor` in the API. The worker read routes go through the API's resolver, so the worker never holds the token.
- **S6. Every YouTrack State write re-checks the live bundle.** The open state and every column state must be unresolved and not archived, the closed state must pass `issuetracker.ClosableState` (an allowlist on the State's `name` alone, never `localizedName`), and all mapped states must be distinct. All of this runs when the mapping is saved (a 400); before every write the target State is checked again in its role, failing with `ErrStateNotAllowed`; an unmapped column fails with `ErrColumnNotMapped` before any request. Do not turn the allowlist into a denylist.
- **S7. The eligibility tag is never stripped.** The write guard refuses any label write on a linked repo that removes the link's eligibility name, and remove-label-on-dispatch compares the selector with the eligibility label. Every other issue write is lifted one operation at a time and passes through `markNotFound`, so a moved-away issue stays skippable.
- **S8. `issues.issue_tracker_column` has three writers.** The linked `UpsertIssueIfChanged`, `SetIssueTrackerColumn` (after a successful move) and `ClearIssueTrackerColumnsNotIn` (a linked `ConfigureColumns`, in the transaction that prunes the mapping). Every reader goes through `board.CardColumnLabels`, keyed on the repo being linked, which leaves an unlinked repo's labels unchanged.
- **S9. `forge.MoveByLabels` keeps the forge label calls identical.** `EnsureLabels` only when `Ensure` is non-empty, then one `UpdateIssueLabels` with the slices passed through. `forgetest.Recorder` logs `MoveToColumn` in a separate `Moves()` log so the `Calls()` goldens do not change. The YouTrack driver ignores the label fields, because columns are not tags.
- **S10. A linked run's MR-state moves happen in the board-free MR-watch lane.** It moves cards only on the close and reopen edges and only for the issue's latest issue-lane run; the board lane excludes linked repos so the two stay disjoint.

## Open questions for M6's live pass

- Whether YouTrack accepts `readSharingSettings` and `tagSharingSettings` in the tag-create body. The fake accepts them; no real server has been asked.
- Whether an authenticated token receives `usersCount` on a tag's sharing groups (the anonymous recording has none).
- Whether the id-scoped listing `GET /api/tags/{id}/issues` behaves on a real instance as the fake and YouTrack's documented shape say. The earlier question, whether `tag: {name}` resolves every tag of that name, is settled: intake sends no tag name in a search query (PRD Decision Log 2026-11-12). If the listing proves unusable, the documented fallback walks the whole project and filters by stored id, bounded by the same listing ceilings.
- Whether the real YouTrack State write (the custom field update on an issue) behaves as the fake and the documented shape say.
- Whether the bounded `State` custom field read with each issue behaves on a real instance, including the cap that fails a listing when the field list is over it.
- Whether the done-name allowlist matches real bundles. A done state named only in another language is refused, so a real project may need its State renamed or the list extended.
- Whether a non-admin token can read `admin/projects/{id}/customFields`, which the live bundle check uses before every State write.
- The window between the bundle check and the State write (as in D10): a bundle edited inside it can still be landed on.
- The duplicate-on-retry risk of a create: YouTrack's create has no rollback, so if the POST lands but attaching the tag or the read-back fails, the untagged issue stays in YouTrack and a retry can file a duplicate.

## Consequences

- Forge drivers and the neutral types are unchanged; callers resolve their issue side through the owner-scoped resolvers, and on a repo with no link they behave as before. A second issue tracker (Asana) is the real test of the seam: YouTrack specifics (the id adapter, the tag rules) must stay inside its driver.
- Operators get one new empty setting. Until it is set no issue tracker connection can be created, and uzi boots as before.
- The `projectID` guard is one half of a binding. M4 added the other half, the resolver (`IssueTrackerForRepo`, owner-scoped).
