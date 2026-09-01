# PRD #917: Forge driver internals: shared wrapErr, bounded rawGet, pagination helper

**GitHub Issue**: [#917](https://github.com/vtmocanu/uzi/issues/917)
**Status**: Complete (created 2026-09-01; M1+M2+M3 landed 2026-09-01)
**Priority**: High (M2 closes epic #915's only graded security finding)
**Parent**: epic #915 (Batch 1, P1)
**Related**:
- `api/internal/forge/github.go:123-137` — the `wrapErr(op, err)` helper (nil passthrough + rate-limit classification via `errors.As` + redact wrap) this PRD copies to the other two drivers.
- `api/internal/forge/forgejo.go:1238-1284` — `rawGet` (unbounded `io.ReadAll` at :1249) beside its byte-bounded twin `rawGetLimited` (:1265-1284), whose own comment states the threat: "A hostile forge streaming a multi-GB log body therefore cannot OOM the api."
- `api/internal/forge/pagination.go` — caps + error builder already extracted; the loop scaffolding is not.
- Line refs at `0fdec3791dad53d28f44193290f04a139e8a0719`, independently fact-checked at `f8e3116`.

## Problem

Three internal-consistency defects inside `api/internal/forge`, all verified by the epic #915 review wave:

1. **The error-wrap idiom exists in three shapes.** github.go extracted `wrapErr`; gitlab.go inlines `g.redact.error(fmt.Errorf("gitlab: <op>: %w", err))` **43×** and forgejo.go the forgejo-prefixed form **54×**. Beyond those: github.go carries **8** real residual inline sites in its graphql path (:1303-1352; its other 3 anchor hits are wrapErr's own body — not migration targets), plus forgejo_pipelines.go (8), github_pipelines.go (1), projectsync.go (1). 118 anchor hits package-wide at `601ea91`, of which 115 are inline call sites. Every inline site is one forgotten wrap away from a PAT-leak-class defect; the helper makes the wrap structural.
2. **`forgejo.rawGet` is the ONLY unbounded response-body read in the forge package** (github.go:1318, github_pipelines.go:214, gitlab.go:731 and forgejo's own `rawGetLimited` all wrap `io.LimitReader`). Callers: `ListIssueLabelEvents` (issue-timeline pagination, reached by the poller via `poller/autopilot.go:138`) and `TokenInfo` (:1183). The item/page caps bound counts computed *after* the read; the 15s timeout bounds seconds, not bytes. A hostile or compromised forge can OOM the api. **Epic finding S1, MEDIUM.**
3. **Pagination loop scaffolding is copy-pasted ~25×** across the three drivers (item-cap checks: gitlab 7 / forgejo 8 / github 10; page-cap likewise); e.g. gitlab.go:209-249 vs :251-279 are identical but for the client call + mapping.

## Solution

Three milestones, strictly inside `api/internal/forge` (no interface change, no consumer change):

1. Add `wrapErr` to the gitlab and forgejo receivers (same semantics as github's: nil passthrough, rate-limit classification, redact wrap, driver prefix) and mechanically migrate the ~97 inline sites.
2. Merge `rawGet` into the `rawGetLimited` shape (one helper taking a limit), bounding the two `rawGet` call paths.
3. (Stretch) Generic `paginate[T]` helper in pagination.go; each list method supplies a fetch + map closure.

## Milestones

- [x] **M1 — wrapErr for all inline sites in the package.** Add the helper to the gitlab and forgejo receivers, mirroring github.go:123-137 exactly (nil passthrough; rate-limit classification; `redact.error` wrap; `"gitlab: "`/`"forgejo: "` prefix). Migrate ALL inline call sites in the package: gitlab.go (43), forgejo.go (54), github.go's 8 residual inline sites in the graphql path (:1303-1352 — it has the helper but does not use it everywhere), forgejo_pipelines.go (8), github_pipelines.go (1), projectsync.go (1). Total 115 at the fact-check tip `601ea91`; counts drift, the site list is the contract. **Sweep criterion (fact-checked satisfiable form):** after migration, `git grep -nF '.redact.error(fmt.Errorf(' -- api/internal/forge/` returns hits ONLY inside the three `wrapErr` function bodies (the helper itself legitimately contains the idiom, ~3 hits per driver — a package-wide zero is IMPOSSIBLE by construction, do not chase it); read the residue and confirm every remaining hit is within a wrapErr definition. Never anchor on a receiver name (gitlab uses `g`, forgejo `f`). **Behavior-preservation proof: the existing suite is NOT it** — the forge tests pin redaction (PAT-absent), not the driver-prefixed op strings, so a silently dropped op-context would pass green. Verify byte-identical error strings directly: for a sample of migrated sites per file, render the pre- and post-migration error (small throwaway test or golden diff) and compare. Existing tests must also stay green (that proves redaction held). `task gate:api` green.
- [x] **M2 — bound rawGet (closes S1).** Fold `rawGet` and `rawGetLimited` into one bounded helper (keep the `rawGetLimited` limit semantics; pick the limit for the former `rawGet` callers to comfortably cover a real timeline page / token-info response — mirror the limits the sibling drivers use for comparable payloads, and record the chosen value + rationale in the Decision Log on landing). Test ships here: a test with a fake forge server streaming an oversized body asserts the read stops at the cap and returns the same error shape `rawGetLimited` produces today, plus a positive control that a normal-size body still parses. This is deliberately behavior-affecting (a byte cap where none existed); the PRD flags it and the cap must be generous enough that no legitimate response hits it. `task gate:api` green.
- [x] **M3 (stretch, own MR-sized commit; skip if it fights the ratchet) — pagination helper.** `paginate[T](ctx, fetch func(page int) (items []T, next int, err error)) ([]T, error)` in pagination.go, using the existing caps/error builder; migrate the ~25 loops. Medium risk (touches every list path in all three drivers): each migrated method keeps its existing tests green unmodified, and the fake-server pagination tests must include one path that exercises the item-cap and one the page-cap (positive controls that the caps still fire through the helper). `task gate:api` green.

## Success criteria

1. Every `.redact.error(fmt.Errorf(` hit in the forge package lives inside a `wrapErr` function body; zero inline call sites remain. (A package-wide zero is unreachable — the helpers themselves contain the idiom.)
2. Zero unbounded response-body reads in the forge package (`git grep -F 'io.ReadAll(resp.Body)' -- api/internal/forge/` returns only comments, if anything).
3. Existing driver tests pass unmodified in M1/M3 (they prove redaction held — NOT that error text held; that proof is M1's direct string diff); M2 adds the cap test with its positive control.
4. `task gate:api` green; no `.github/workflows/**` in the branch diff.

## Decision Log

- **D1 — copy the helper into each driver rather than a shared free function.** `wrapErr` closes over the driver's redactor and prefix; three tiny receiver methods mirror the existing github.go shape and keep the drivers structurally parallel (the property the epic's architect verified as clean). A shared generic would need the redactor + prefix threaded through every call.
- **D2 — M2 is behavior-affecting and stays in this PRD anyway.** The epic's rule is that hardening is flagged, not smuggled: the byte cap is the entire point of M2, the fix is literally a dedup with the bounded twin, and splitting it out would leave the security finding unfixed for no risk reduction.
- **D3 — M3 is a stretch milestone.** It is the one medium-risk piece; if it drags or the golangci ratchet surfaces a large pre-existing backlog in the touched files (`whole-files: true` — findings in files you merely touch gate), land M1+M2 and file M3 as its own follow-up rather than holding the security fix hostage.

### Landing decisions (2026-09-01)

- **D4 — gitlab/forgejo `wrapErr` OMIT the rate-limit classification branch.** github's `wrapErr` classifies `*gh.RateLimitError`/`*gh.AbuseRateLimitError` via `errors.As` (R7). gitlab and forgejo have no such classification today, and their SDKs (go-gitlab, gitea) never produce `gh.RateLimitError`, so copying that branch verbatim would be dead code — and any real classification would change error strings for rate-limited cases, breaking M1's byte-identical requirement. So the two new helpers carry only the default path (nil passthrough + `redact.error` wrap + driver prefix). This is a deliberate, minor deviation from the milestone's "mirror github.go exactly" wording, in favour of byte-identity and no dead code. Real per-SDK rate-limit classification, if ever wanted, is a separate behavior-changing follow-up.
- **D4b — three single-segment "ceiling" guards stay inline, NOT routed through `wrapErr`.** gitlab.go / forgejo_pipelines.go / github_pipelines.go each carry a `"<driver>: job %d ... exceeds the %d-byte ceiling"` fail-closed message. These have no `op: detail` colon boundary, so `wrapErr`'s `"<driver>: <op>: <err>"` form would insert a second colon and change the bytes. They already go through `redact.error` (PAT-safe) and are commented in place. Success criterion 1's residue is therefore the three `wrapErr` bodies plus these three documented exceptions — not a package-wide zero (which is impossible, since the helpers themselves contain the idiom).
- **D5 — M2 byte cap is `maxTraceBytes+1` (16 MiB).** `rawGet` was deleted and both callers (`ListIssueLabelEvents`, `TokenInfo`) now use `rawGetLimited(ctx, path, maxTraceBytes+1)`. 16 MiB mirrors every sibling bounded read in the package (the `maxTraceBytes` ceiling at gitlab.go:29) and is ~1000× any legitimate single timeline/token JSON page (page size ≤ `forgejoPerPage` = 50), so no real response hits it; the failure mode is a loud redacted error, never silent truncation. The `+1` lets an at-limit body be detected as over-limit, matching the sibling drivers.
- **D6 — M3 migrated the 15 byte-identical-fitting loops; 12 SPECIAL + 2 borderline stay inline.** The generic `paginate[T](wrap, fetch)` keeps two counters (iteration count for the page-cap, forge page for the request) so it stays byte-identical even under a forge that jumps `NextPage`, and preserves the exact check order. The 12 loops with special structure (`ListIssues`' `opts.Limit` mid-page early-return; forgejo `TokenInfo` match-counting, `issueLabelNames` dual-map dedup, and the raw-GET timeline loop; the cumulative-cap `ListMergeRequestComments` loops; the GraphQL cursor loops in github `reviewThread*` and projectsync) are deliberately left inline — folding them in would change behavior. The 2 first-loops of multi-loop MR-comment methods are left inline to avoid fragmenting a method whose sibling loops are SPECIAL. Net −110 lines; existing pagination and per-method tests pass unmodified.

## Risks & mitigations

- **Ratchet surfacing (`whole-files: true`).** gitlab.go/forgejo.go are heavily touched; pre-existing findings in them will gate. Budget for fixing or justifying (`//nolint:<linter> // <why>`) whatever surfaces; `task lint:api` before pushing.
- **A legitimate large response hits the M2 cap.** Mitigated by choosing the cap against the sibling drivers' precedent for comparable endpoints and by the normal-size positive control; the failure mode is a loud, redacted error, not silent truncation.
- **Error-string drift in M1 is NOT caught by the existing suite** (fact-checked: the 35 error-content assertions pin redaction and a handful of substrings, not the per-site op strings). The migration must produce byte-identical strings, and M1's direct rendered-error diff is the only instrument that proves it — do not skip it because the suite is green.
