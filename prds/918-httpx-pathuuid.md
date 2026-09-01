# PRD #918: httpx.PathUUID helper for the uuid.Parse(chi.URLParam) handler boilerplate

**GitHub Issue**: [#918](https://github.com/vtmocanu/uzi/issues/918)
**Status**: Draft (created 2026-09-01)
**Priority**: Medium
**Parent**: epic #915 (Batch 1, P2)
**Related**:
- `api/internal/httpx/respond.go` — exports JSON/Error/DecodeJSON/DecodeJSONLimited today; response, decode and auth-context concerns are already centralized (`UserFromContext` 124×). Path-UUID parsing is the one repeated concern that never was.
- Example site shape: `api/internal/handler/judge.go:135-138`. 78 non-test occurrences across ~37 handler files (verified count at `0fdec37`, fact-checked at `f8e3116`).

## Problem

78 handlers repeat the same 4-line pattern:

```go
id, err := uuid.Parse(chi.URLParam(r, "id"))
if err != nil {
    httpx.Error(w, http.StatusBadRequest, "invalid <thing> id")
    return
}
```

Every new handler copies it; every copy is a chance to drift the status code or forget the return. The epic #915 review found this the highest-count, lowest-risk dedup in the api module.

## Solution

One helper in `httpx`:

```go
// PathUUID parses the named chi URL param as a UUID. On failure it writes the
// 400 and returns ok=false; the caller just returns.
func PathUUID(w http.ResponseWriter, r *http.Request, param, label string) (uuid.UUID, bool)
```

Sites become `id, ok := httpx.PathUUID(w, r, "id", "run"); if !ok { return }`. The `label` preserves each site's existing "invalid X id" message text.

## Milestones

- [ ] **M1 — helper + unit test.** Add `PathUUID` to `api/internal/httpx` (this adds httpx's first `chi` + `uuid` imports — acceptable, see D1). Table test: valid UUID passes through; invalid writes exactly `{"error":"invalid <label> id"}` with 400 and returns ok=false (assert body AND code — the error shape must be byte-compatible with what the inline sites produce today, so diff one real site's rendered 400 against the helper's before migrating anything). `task gate:api` green.
- [ ] **M2 — migrate all 78 sites.** Mechanical migration across the ~37 handler files. Preserve each site's message text verbatim via `label`; a site whose message does not fit the `invalid <label> id` template keeps its custom text (pass the full message through a second helper variant or leave that site inline and note it — do not silently reword; see D2). Sweep: `git grep -F 'uuid.Parse(chi.URLParam' -- api/internal/handler/` returns zero non-test hits when done (calibrate the grep against a test-file hit first). Existing handler tests must stay green **unmodified** — they pin the 400 bodies and are the behavior-preservation proof. `task gate:api` green.

## Success criteria

1. Zero non-test `uuid.Parse(chi.URLParam` sites in `api/internal/handler`.
2. No handler test modified (green as-is), except tests that themselves construct the boilerplate as fixtures.
3. Every 400 body byte-identical to before (spot-diff at least 3 migrated endpoints' invalid-id responses against `main`).
4. `task gate:api` green; no `.github/workflows/**` in the branch diff.

## Decision Log

- **D1 — helper lives in httpx, accepting the new chi/uuid deps there.** httpx is already the handler-layer response toolbox; the alternative (a `handler`-package helper) works but leaves the next non-handler HTTP surface to re-invent it. chi and uuid are already direct deps of the module.
- **D2 — message text is preserved per-site, never normalized.** This PRD is a dedup, not a copy-edit: CLI and web both key off error strings in places, and a silent reword is a behavior change. Normalizing genuinely inconsistent messages is a separate, deliberate decision for a future PRD if anyone cares.
- **D3 — write-the-400 helper (w passed in) over a parse-only helper.** The repeated boilerplate IS the write+return dance; a parse-only helper would leave 78 copies of the 400 in place, deduplicating the cheap half.

## Risks & mitigations

- **Ratchet surfacing (`whole-files: true`).** ~37 touched handler files include `handler/schedules.go`, one of the two files the epic baseline names as carrying real pre-existing G115 gosec findings — touching it makes those gate. Budget for fixing the surfaced findings (usually a checked int conversion) or justifying a `//nolint`; run `task lint:api` early, not at the end.
- **A site with extra logic hiding in the boilerplate** (e.g. a custom status or a fallthrough). The migration is grep-driven but each site is read, not batch-sed'd; anything non-standard stays inline and is listed in the MR description.
- **Vacuous "zero hits" sweep.** Calibrate the final grep against a known-present test occurrence before trusting the zero (per the epic's evidence discipline).
