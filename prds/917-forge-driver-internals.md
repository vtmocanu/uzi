# PRD #917: Forge driver internals: shared wrapErr, bounded rawGet, pagination helper

**GitHub Issue**: [#917](https://github.com/vtmocanu/uzi/issues/917)
**Status**: Draft (created 2026-09-01)
**Priority**: High (M2 closes epic #915's only graded security finding)
**Parent**: epic #915 (Batch 1, P1)
**Related**:
- `api/internal/forge/github.go:123-137` — the `wrapErr(op, err)` helper (nil passthrough + rate-limit classification via `errors.As` + redact wrap) this PRD copies to the other two drivers.
- `api/internal/forge/forgejo.go:1238-1284` — `rawGet` (unbounded `io.ReadAll` at :1249) beside its byte-bounded twin `rawGetLimited` (:1265-1284), whose own comment states the threat: "A hostile forge streaming a multi-GB log body therefore cannot OOM the api."
- `api/internal/forge/pagination.go` — caps + error builder already extracted; the loop scaffolding is not.
- Line refs at `0fdec3791dad53d28f44193290f04a139e8a0719`, independently fact-checked at `f8e3116`.

## Problem

Three internal-consistency defects inside `api/internal/forge`, all verified by the epic #915 review wave:

1. **The error-wrap idiom exists in three shapes.** github.go extracted `wrapErr`; gitlab.go inlines `g.redact.error(fmt.Errorf("gitlab: <op>: %w", err))` **43×** and forgejo.go the forgejo-prefixed form **54×**. Every inline site is one forgotten wrap away from a PAT-leak-class defect; the helper makes the wrap structural.
2. **`forgejo.rawGet` is the ONLY unbounded response-body read in the forge package** (github.go:1318, github_pipelines.go:214, gitlab.go:731 and forgejo's own `rawGetLimited` all wrap `io.LimitReader`). Callers: `ListIssueLabelEvents` (issue-timeline pagination, reached by the poller via `poller/autopilot.go:138`) and `TokenInfo` (:1183). The item/page caps bound counts computed *after* the read; the 15s timeout bounds seconds, not bytes. A hostile or compromised forge can OOM the api. **Epic finding S1, MEDIUM.**
3. **Pagination loop scaffolding is copy-pasted ~25×** across the three drivers (item-cap checks: gitlab 7 / forgejo 8 / github 10; page-cap likewise); e.g. gitlab.go:209-249 vs :251-279 are identical but for the client call + mapping.

## Solution

Three milestones, strictly inside `api/internal/forge` (no interface change, no consumer change):

1. Add `wrapErr` to the gitlab and forgejo receivers (same semantics as github's: nil passthrough, rate-limit classification, redact wrap, driver prefix) and mechanically migrate the ~97 inline sites.
2. Merge `rawGet` into the `rawGetLimited` shape (one helper taking a limit), bounding the two `rawGet` call paths.
3. (Stretch) Generic `paginate[T]` helper in pagination.go; each list method supplies a fetch + map closure.

## Milestones

- [ ] **M1 — wrapErr for gitlab.go and forgejo.go.** Add the helper to each receiver, mirroring github.go:123-137 exactly (nil passthrough; rate-limit classification; `redact.error` wrap; `"gitlab: "`/`"forgejo: "` prefix). Migrate all inline sites. Sweep anchor: `git grep -F '.redact.error(fmt.Errorf(' -- api/internal/forge/` must return **zero** production hits when done (receivers differ — gitlab uses `g`, forgejo `f` — so never anchor on a receiver name). Behavior-preserving: identical error strings and classification. Existing driver tests must stay green unmodified (they pin the error text). `task gate:api` green.
- [ ] **M2 — bound rawGet (closes S1).** Fold `rawGet` and `rawGetLimited` into one bounded helper (keep the `rawGetLimited` limit semantics; pick the limit for the former `rawGet` callers to comfortably cover a real timeline page / token-info response — mirror the limits the sibling drivers use for comparable payloads, and record the chosen value + rationale in the Decision Log on landing). Test ships here: a test with a fake forge server streaming an oversized body asserts the read stops at the cap and returns the same error shape `rawGetLimited` produces today, plus a positive control that a normal-size body still parses. This is deliberately behavior-affecting (a byte cap where none existed); the PRD flags it and the cap must be generous enough that no legitimate response hits it. `task gate:api` green.
- [ ] **M3 (stretch, own MR-sized commit; skip if it fights the ratchet) — pagination helper.** `paginate[T](ctx, fetch func(page int) (items []T, next int, err error)) ([]T, error)` in pagination.go, using the existing caps/error builder; migrate the ~25 loops. Medium risk (touches every list path in all three drivers): each migrated method keeps its existing tests green unmodified, and the fake-server pagination tests must include one path that exercises the item-cap and one the page-cap (positive controls that the caps still fire through the helper). `task gate:api` green.

## Success criteria

1. Zero inline `redact.error(fmt.Errorf(` production sites in the forge package; all three drivers wrap through a `wrapErr`.
2. Zero unbounded response-body reads in the forge package (`git grep -F 'io.ReadAll(resp.Body)' -- api/internal/forge/` returns only comments, if anything).
3. Existing driver tests pass unmodified in M1/M3 (they are the behavior-preservation proof); M2 adds the cap test with its positive control.
4. `task gate:api` green; no `.github/workflows/**` in the branch diff.

## Decision Log

- **D1 — copy the helper into each driver rather than a shared free function.** `wrapErr` closes over the driver's redactor and prefix; three tiny receiver methods mirror the existing github.go shape and keep the drivers structurally parallel (the property the epic's architect verified as clean). A shared generic would need the redactor + prefix threaded through every call.
- **D2 — M2 is behavior-affecting and stays in this PRD anyway.** The epic's rule is that hardening is flagged, not smuggled: the byte cap is the entire point of M2, the fix is literally a dedup with the bounded twin, and splitting it out would leave the security finding unfixed for no risk reduction.
- **D3 — M3 is a stretch milestone.** It is the one medium-risk piece; if it drags or the golangci ratchet surfaces a large pre-existing backlog in the touched files (`whole-files: true` — findings in files you merely touch gate), land M1+M2 and file M3 as its own follow-up rather than holding the security fix hostage.

## Risks & mitigations

- **Ratchet surfacing (`whole-files: true`).** gitlab.go/forgejo.go are heavily touched; pre-existing findings in them will gate. Budget for fixing or justifying (`//nolint:<linter> // <why>`) whatever surfaces; `task lint:api` before pushing.
- **A legitimate large response hits the M2 cap.** Mitigated by choosing the cap against the sibling drivers' precedent for comparable endpoints and by the normal-size positive control; the failure mode is a loud, redacted error, not silent truncation.
- **Error-string drift in M1 breaks a test pinning exact text.** That is the desired signal: the migration must produce byte-identical strings; a red test means a site was migrated wrong, not that the test needs updating.
