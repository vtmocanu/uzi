# ADR-4173: The issue tracker seam

**Status**: Accepted (PRD #4173, M3 landed; M4–M6 pending)
**Date**: 2026-11-11
**Deciders**: the maintainer + agent team
**PRD**: [prds/4173-external-issue-trackers.md](../prds/4173-external-issue-trackers.md) — the PRD carries the milestones and the Decision Log; this ADR carries the durable shape of what M3 built and the invariants a later change must keep.

## Decision (summary)

Issue trackers other than the repo's forge sit behind a `Driver` in their own package, `api/internal/issuetracker`. A driver is `forge.IssueTracker` plus four connection operations. Connections live apart from forge connections (`issuetrackersvc`, table `issue_tracker_connections`) with their own base URL allowlist. The YouTrack driver adapts YouTrack's string ids inside itself, is built for exactly one project, and treats tags as user-owned objects chosen by id. Through M3 no repo can link to an issue tracker, so no behaviour changes for forges.

Not shipped yet: the repo link, `IssueTrackerForRepo` returning an issue tracker (M4), the board and run behaviour on YouTrack (M5), and the live pass against a real YouTrack with docs (M6).

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
- `issueIID` is YouTrack's `numberInProject`. Each per-issue call checks the issue reached is in the bound project and carries the number, so a moved issue reads as not found. YouTrack has no conditional write, so a move between that check and the write is not caught.
- `IssueNote.ID` is 0, not derived from YouTrack's string comment id. Account ids are 0, with the login kept as text.
- `ListStates` takes the project it reads, because states belong to a project.
- The nullable `issue_key` column is M4's.

### D14. The tag rules

- The eligibility tag is chosen by id. Its name resolves to the stored id whoever owns the tag, for reads and for writes.
- Any other name resolves only to a tag owned by the connection's account, which the driver learns from `users/me`. A missing one is created, owned by that account, with the eligibility tag's sharing settings. The eligibility tag is never created, and a drifted name never resolves to the eligibility id.
- Reads drop a tag that only shares the eligibility name.
- `ListTags` returns the tags the token can see, with owner and read/use sharing settings. `AccountCanApply` and `OthersCanApply` judge the use settings, and the owner counts as covered. A named group counts for others only with `UsersCount >= 2`; the anonymous response recorded from a real YouTrack (`testdata/youtrack_tags_recorded.json`) omits `usersCount`, so on such a response only the All Users group counts. This fails closed. Using these to refuse a link is M4.

## Invariants a future change must keep

- **S1. An issue tracker never gains code access.** `Driver` embeds `forge.IssueTracker`, not `forge.CodeHost`; a driver passed as a code host does not compile.
- **S2. Code-host checks stay on the forge.** The privilege check, the protected-branch check and the worker-held forge credential act on the repo's forge connection and fail closed. Nothing in M3 touches them, and no repo is linked yet to test the claim further.
- **S3. Credentials follow the forge rules.** Sealed token with AAD and no reveal path, https-only base URL on the separate empty-by-default allowlist re-checked in `DriverFor`, redirects refused, errors redacted. The redirect, oversized-body and token-in-error cases each have a test in `api/internal/issuetracker`.
- **S4. Issue tracker text is untrusted input.** The driver bounds what it reads (response bytes, listing pages and items, nested items, label name length). Fencing of titles, descriptions and comments is done by the existing callers once a repo can link in M4; it is not exercised on YouTrack text yet.
- **S5. The worker never holds an issue tracker token.** The allowlist does not feed worker egress, and the token is opened only inside `DriverFor` in the API. The worker read routes are M4.

## Open questions for M6's live pass

- Whether YouTrack accepts `readSharingSettings` and `tagSharingSettings` in the tag-create body. The fake accepts them; no real server has been asked.
- Whether an authenticated token receives `usersCount` on a tag's sharing groups (the anonymous recording has none).
- Whether the server query `tag: {name}` resolves every tag of that name, including ones owned by others. The client side filters by id, so a wrong answer here would drop issues, not mis-tag them.

## Consequences

- Forge drivers, the neutral types and every caller are unchanged. A second tracker (Asana) is the real test of the seam: YouTrack specifics (the id adapter, the tag rules) must stay inside its driver.
- Operators get one new empty setting. Until it is set no issue tracker connection can be created, and uzi boots as before.
- The `projectID` guard is one half of a binding. M4 owns the other half, the resolver.
