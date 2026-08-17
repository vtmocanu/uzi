# PRD #345: Surface repo-enable guardrail violations in the UI

**GitHub Issue**: [#345](https://github.com/vtmocanu/uzi/issues/345)
**Status**: Draft (created 2026-08-17)
**Priority**: Medium
**Related**:
- [#66](https://github.com/vtmocanu/uzi/issues/66) / [#238](https://github.com/vtmocanu/uzi/issues/238) — the repo-enable guardrail (privcheck) that produces the `violations`. This PRD does not touch the guard; it renders what the guard already returns.
- `ForgeSettings.connect` (`web/src/pages/ForgeSettings.tsx:144-151`, `:237-244`) — the existing, working in-repo pattern for the **same** `422 { error, violations[] }` contract. This PRD copies it onto the repo-enable path.

## Problem

When enabling a repository fails the enable guardrail (privcheck), the reason is computed and returned, but the web UI throws it away and shows only a generic headline. The user is told the enable failed but not *why*; the specific reasons are visible only in the browser's Network tab.

Observed live 2026-08-17 enabling `github.com/vtmocanu/uzi`: the enable was refused with `violations:["could not read default-branch protection on this repo"]` (privcheck code `protection_unreadable`), present in the API response but never rendered in-app.

The server contract is already correct and tested. `PUT /repos/{id}` → `Handler.SetRepoEnabled` (`api/internal/handler/forge.go:621`) runs `GuardRepo` only on the enable path (`forge.go:642-684`) and, on a block, returns **HTTP 422** with body `{ "error": "<headline>", "violations": ["<human message>", ...] }` (`forge.go:673-681`; `violations` built by `blockFindingMessages` at `forge.go:608-616`). The live-DB test `api/internal/handler/repo_enable_guardrail_livedb_test.go:227-252` already pins 422 + non-empty `violations` + repo-stays-disabled.

The reasons even reach the browser: `api.setRepoEnabled` (`web/src/lib/api.ts:2670-2671`) surfaces a failure as `ApiError`, and `request()` (`api.ts:2365-2376`) stores the full parsed body — `violations` included — on `ApiError.body` (documented at `web/src/lib/apiError.ts:9-11`). The data is on the client; it is simply ignored.

The bug is one line. `Repos.tsx` `toggle` (`web/src/pages/Repos.tsx:108-119`) catches with:

```
setError(err instanceof ApiError ? err.message : "Update failed");   // Repos.tsx:115
```

It reads only `err.message` (the generic headline at `forge.go:679`) and never touches `err.body.violations`. There is no `422` branch. So the page-level `Alert` (`Repos.tsx:332`) shows the headline and the actual reasons are lost.

## Solution

A web-only fix that renders the violations the server already returns, matching the existing `ForgeSettings.connect` pattern for the identical contract. **No server change** — the `{error, violations[]}` DTO is already emitted and tested.

1. **Read the violations on a refused enable.** In `Repos.tsx` `toggle`, add a `422` branch that reads `(err.body as { violations?: string[] }).violations ?? []`, mirroring `ForgeSettings.tsx:144-151`.
2. **Render them.** Add an `enableViolations` state (cleared at the top of `toggle` alongside `setError("")` at `Repos.tsx:109` and on success), and render a danger-toned card + `<ul>` of reasons right after the page `Alert` (`Repos.tsx:332`), copying the block at `ForgeSettings.tsx:237-244`. Keep the existing `Alert` for the headline; the card adds the itemized reasons.
3. **Close the mock-mode gap** so the fixed UX is reproducible in mock/demo mode and browser passes. `mockApi.setRepoEnabled` (`web/src/mocks/mockApi.ts:2292-2297`) always succeeds today; make it throw `ApiError(422, headline, { violations })` when a `guardrail_blocked` repo is enabled, sourcing the strings from the existing `mockBlockedRepoMeta` (`web/src/mocks/data.ts:1166-1188`; repos already carry `guardrail_blocked` at `data.ts:1126`). No new mock scenario is needed.

**Scope note — human strings, not codes.** `blockFindingMessages` ships only the human `.Message`, not the machine `Code` (`protection_unreadable`, etc., which live in `privcheck.Finding` at `api/internal/privcheck/report.go:134-138`). Rendering codes would require a server DTO change and is out of scope; the human messages are already actionable.

## Milestones

### Committed in this PRD

- [ ] **M1 — Surface violations on a refused enable (web), with its test.** `Repos.tsx` `toggle` gains a `422` branch reading the violations off `err.body`; a new `enableViolations` state is cleared on entry/success and populated on block; a danger card + `<ul>` renders after the page `Alert`, matching `ForgeSettings.tsx:237-244`. **The test ships in this milestone, not a later one** (a ~30-line branch must not land untested): a `Repos.test.tsx` case `mockApi.setRepoEnabled.mockRejectedValue(new ApiError(422, "<headline>", { violations: [...] }))`, click Enable, assert **each** violation string renders AND the repo stays disabled (paired positive/negative, per the vacuous-negative-assertion caution in `.claude/rules/web.md`), plus clear-on-success. This test mocks at the api boundary, so it does **not** depend on M2. Use the `err.body as { violations?: string[] } | null` + `body?.violations ?? []` form from `ForgeSettings.tsx:146-148` (not a non-null cast). `task gate:web` green.
- [ ] **M2 — Mock-mode parity (for demo/browser passes).** Make `mockApi.setRepoEnabled` throw `ApiError(422, headline, { violations })` (strings from `mockBlockedRepoMeta`) **only when `enabled === true` and the repo is `guardrail_blocked`** — a *disable* is never gated, matching the server which guards the enable path only. **This is not "one throw":** today no fixture is both `enabled:false` AND `guardrail_blocked` (`repo-payments` is blocked-but-enabled, `data.ts:1114`; `repo-www` is disabled-but-unblocked, `data.ts:1128`), so a one-click "Enable a blocked repo" demo does not exist yet. **Prefer adding a disabled+blocked fixture row** (+ its `mockBlockedRepoMeta` key) over flipping an existing one, and mind the ripples: `mockApi.listRepos` filters on `r.enabled` (`mockApi.ts:2291`), and `repo-payments` being enabled is what makes the Boards "runs blocked" badge and the admin Allow-anyway demo reachable (`data.ts:1109-1113`).

### Optional / follow-up (only if the owner wants them)

- [ ] **M3 — Actionable copy + a11y.** Add short guidance ("fix branch protection on the forge, then retry") and a link to the bot-setup doc, mirroring `ForgeSettings.tsx:245-251`; ensure the reasons are announced (`role="alert"` via the danger `Alert`). Low effort, higher polish.
- [ ] **M4 — Handler-layer contract test.** A characterization test pinning `{error, violations[]}` for `SetRepoEnabled` at the handler layer (belt-and-braces over the existing live-DB coverage at `repo_enable_guardrail_livedb_test.go`), so the web contract cannot silently drift. Server-side, independent.

## Success criteria

1. A refused repo-enable shows the specific violation message(s) in the UI (not only the generic headline, not only in the Network tab).
2. The repo remains disabled after a refused enable (unchanged behavior; asserted alongside 1 so the test is not vacuous).
3. Mock/demo mode reproduces a blocked enable, so the UX is verifiable without a live forge.
4. No server behavior change: the `422 { error, violations[] }` contract and its existing tests are untouched.

## Decision Log

- **D1 — Fix is web-only; do not touch the guard or its DTO.** The server already returns the reasons (`forge.go:608-616`, `:673-681`) and pins them in a live-DB test. The gap is entirely `Repos.tsx` ignoring `err.body.violations`. Adding a server change (e.g. shipping codes) would widen scope for no user benefit here — the human messages are already actionable. Codes stay out of scope (see Solution scope note).
- **D2 — Copy `ForgeSettings.connect`, do not invent a pattern.** The same `422 { error, violations[] }` contract is already handled and rendered in `ForgeSettings.tsx:144-151` / `:237-244`. Reusing it keeps the two enable-ish surfaces visually and behaviorally consistent and minimizes review surface. The in-file findings box at `Repos.tsx:816-824` (the Allow-anyway modal) is a second precedent for the same idiom.
- **D3 — Ship the mock-parity fix in the same PRD, and budget for the fixture gap.** Without it, mock/demo mode (`mockApi.setRepoEnabled` always succeeds) cannot exercise the new UX, so a browser/demo validation pass would show nothing. But it is **not** "one throw": no current fixture is both disabled and `guardrail_blocked`, so a natural one-click blocked-enable demo requires adding a disabled+blocked repo row (preferred) or flipping one, minding the `listRepos` `enabled` filter and the badges that depend on `repo-payments` staying enabled (M2). The mock must gate on `enabled === true` so it never blocks a disable.

## Risks & mitigations

- **Vacuous negative test.** A test that only checks "no error thrown" would pass without rendering anything. Mitigated by M3 asserting each violation **string** is present AND the repo stays disabled (positive + negative), per `.claude/rules/web.md`.
- **Stale reasons lingering after a later success.** Mitigated by clearing `enableViolations` at the top of `toggle` and on success, exactly as `resetMessages` clears `connectViolations` in `ForgeSettings.tsx:132`.
- **Mock strings drifting from real ones.** Low: `mockBlockedRepoMeta` already carries realistic block messages; they need only be plausible, not identical to server output.
- **Page-level card is ambiguous on a multi-repo list.** `Repos.tsx` is a list; a single card after the page `Alert` does not name which row was refused. Consistent with the existing page-level `error` Alert (not a regression), but if it reads unclear in review, scope the violations to the acted-on repo by naming it in the card heading. Noted, not required.

## Out of scope

- Any change to the privcheck guard, its findings, severities, or the `{error, violations[]}` wire shape.
- Shipping machine `code`s to the client (would need a server DTO change).
- A CLI change: `api/cmd/uzi/` has no repo-enable command, so the "check the CLI too" convention does not apply here.

## Open decisions (for the user)

1. **Include the optional milestones (M3 copy/a11y, M4 handler test)?** Recommend M3 (cheap polish, mirrors `ForgeSettings`); M4 is belt-and-braces over existing live-DB coverage and can be skipped.

---

*Drafted 2026-08-17 against code at current `main` HEAD. File anchors from a codebase investigation this session; cite by symbol (`toggle`, `SetRepoEnabled`, `blockFindingMessages`, `ForgeSettings.connect`/`connectViolations`) if the tree moves, since line numbers drift. No open-web dependency: the fix reuses an in-repo pattern, existing components, and the already-shipped server contract, so it is safe for an offline uzi worker.*
