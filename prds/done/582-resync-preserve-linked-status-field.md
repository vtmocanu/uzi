# PRD #582 — Resync must re-read the LINKED Status field, not re-resolve by name

**Issue**: [#582](https://github.com/vtmocanu/uzi/issues/582)
**Status**: Done — shipped on branch `agent/issue-582` (M1–M4 complete)
**Priority**: High (data-integrity: silently mis-points a working sync)
**Owner**: uzi agent

> Fixes a bug in PRD #576 (GitHub Projects v2 sync, adopt-first + auto-create). Read `api/internal/forgesvc/projectsync.go` first.

> **Scope guard for the uzi worker**: touches **no** `.github/workflows/**` file (worker PAT lacks `workflow` scope — an atomic push rejection loses the branch, `.claude/rules/prds.md`). All changes are `api/`, `docs/`, and tests.

---

## Problem

A repo can be synced through **either** GitHub's built-in `Status` field (when Adopt matched it) **or** uzi's own `uzi Status` field (created by **Provision** or by **auto-create the missing columns**, PRD #576 M6). Which field a link uses is recorded in `github_project_links.status_field_id`.

**Resync ignores that stored field and re-resolves by NAME**, so on a board that has *both* fields it always switches to the built-in `Status`:

- `Resync` → `resyncPrepare` → `prepareSeedLink` (`api/internal/forgesvc/projectsync.go:552`).
- `prepareSeedLink` resolves the field by name: `ProjectV2StatusFieldByName(projectID, statusFieldName)` where `statusFieldName = "Status"` (`projectsync.go:38`), falling back to `uziStatusFieldName = "uzi Status"` (`:39`) **only if the `"Status"` lookup fails** (`projectsync.go:553-560`).
- It then **re-persists** `StatusFieldID: field.ID` into the link (`projectsync.go:603-611`), overwriting whatever the link pointed at.

A GitHub Projects v2 board created the ordinary way has a built-in field named `Status` (it can be renamed or deleted, which is why the fallback exists at all). Whenever both a `Status` field and a `uzi Status` field are present — the normal state after auto-create or Provision — the `"Status"` lookup succeeds, the `uzi Status` fallback is never reached, and a Resync on a `uzi Status`-linked board silently re-points to the built-in `Status`.

**Reproduced live on `vtmocanu/uzi` (Project #2), 2026-08-22.** Before Resync the link's `status_field_id` was the `uzi Status` field (`PVTSSF_…hgGC00`) with all five board columns mapped and `unmatched_columns` empty. After one Resync it was the built-in `Status` field (`PVTSSF_…hgE-Fo`) with only `{"In Progress": …}` mapped and `unmatched_columns = {Planned, bug, Human Review, Later}` — the panel then showed "These board columns have no matching Status option and won't sync". The board *display* was unaffected (its view groups by `uzi Status`, an independent GitHub view setting), but sync now writes to the wrong field, so a subsequent card move would not appear on the board.

This defeats the whole point of `uzi Status`: the two safe paths (Provision, auto-create) that PRD #576 built specifically to avoid editing the user's field are undone by the first Resync.

## Why the by-name resolution exists (and where it is still correct)

Adopt legitimately resolves by name — at first Adopt there is no stored field yet, and matching the user's board columns against a named field's options is exactly the job (`adoptPrepare` → `prepareSeedLink`). AutoCreateColumns already treats the field as identity-by-ID and deliberately does **not** call `prepareSeedLink` for this very reason — its own doc says: *"that resolves the field BY NAME, and the built-in 'Status' still exists, so it would re-resolve the WRONG (old) field"* (`projectsync.go:747-750`). Resync is the missing case: it must behave like AutoCreateColumns (identity-by-ID), not like Adopt (by-name). `prepareSeedLink` has exactly two production callers — `adoptPrepare` and `resyncPrepare` — so a resolution split that changes only the Resync path cannot affect Adopt or AutoCreateColumns.

## Resolved facts (baked in — the worker has no open-web access)

> The uzi worker runs with restricted egress (forge + `*.anthropic.com` + package caches; no open web). Everything below is from this repo's code and a live measurement; the worker re-verifies by reading the cited code, all in-repo.

- **F-1 — GitHub Projects v2 field identity is a node id.** A single-select field is addressed by its `PVTSSF_…` node id; its options are `{ id name }`. Reading a field by id uses GraphQL `node(id: $fieldId) { ... on ProjectV2SingleSelectField { id name options { id name } } }`. This is a normal forge call (the forge host is on the worker egress allowlist), not open-web.
- **F-2 — Field names are unique within a project.** GitHub rejects a second field with an existing name, so `Status` and `uzi Status` are distinct fields and a name maps to at most one field. (This is why the current fallback can never reach `uzi Status` when `Status` exists.)
- **F-3 — The interface today has only a by-NAME reader.** `ProjectBoardSyncer.ProjectV2StatusFieldByName(ctx, projectID, fieldName)` (`api/internal/forge/projectsync.go:140`, github impl `:291`). There is **no** by-id reader — this PRD adds one. `ProjectBoardSyncer` is the GitHub-only sub-interface, so the change does **not** touch the 3-driver `Forge` interface (blast radius: the github driver + the project-syncer test fake).
- **F-4 — The link already stores the field id.** `github_project_links.status_field_id` (migration `00140`) holds the linked field's node id, set by Adopt/Provision/AutoCreate. No migration is needed for the primary fix.

## Milestones

### M1 — Add a field-by-ID reader to the ProjectBoardSyncer sub-interface
- Add `ProjectV2StatusFieldByID(ctx, projectID, fieldID string) (ProjectV2StatusField, error)` to `ProjectBoardSyncer` (`api/internal/forge/projectsync.go`), implement it in the github driver mirroring `ProjectV2StatusFieldByName`'s GraphQL pattern (`node(id:)` per F-1), and add it to the project-syncer test fake. GitHub-only sub-interface, so the 3-driver `Forge` and its fakes are untouched.
- **Success criteria** (offline): a Go unit test drives the github method against a canned GraphQL response (or the fake) and asserts it returns the field's name + options for a given id; the fake returns a scripted field by id.

### M2 — Resync re-reads the LINKED field by stored id (the fix)
- Split the field-resolution seam so **Resync reads `link.StatusFieldID` via `ProjectV2StatusFieldByID`** (F-4/F-1), re-maps the board columns against *that* field's current options, and re-persists the **same** `status_field_id`. Keep re-reading the options (the point of Resync — newly-added options on the linked field are picked up), but never switch fields.
- **Change ONLY the field lookup, preserve everything else `prepareSeedLink` does.** Do not inline a fresh Resync body that drops behavior: the by-id variant must still recompute `unmatched` against the linked field's options (`projectsync.go:574-582`), still best-effort re-link the project into the repo's Projects tab via `LinkProjectV2ToRepository` (`:589-593`), and still pass through the link's `ProjectNumber` and `OwnedByUzi` (`resyncPrepare`, `:705`). The clean shape is to pass a field-resolver function into the shared `prepareSeedLink` (by-name for `adoptPrepare`, by-id for `resyncPrepare`) rather than duplicating its ~40-line map/unmatched/persist body.
- **Adopt keeps by-name** (`adoptPrepare` → `prepareSeedLink` as-is): there is no stored field at first Adopt. AutoCreateColumns is already correct. So only the Resync path changes.
- **Repair the existing test that encodes the old behavior.** `TestResyncReseedsAndRepersists` (`api/internal/forgesvc/projectsync_test.go:1142`) stores a link with `StatusFieldID` unset (`""`) and asserts the re-persisted `StatusFieldID == "PVTSSF_NEW"` — i.e. it pins the by-name re-resolve this fix removes. After the fix it will fail because Resync reads the (empty) stored id. **Fix the fixture, do not revert the behavior to make it green**: set the fixture link's `StatusFieldID` to the uzi field's id, have the fake return that field by id, and keep the reseed/re-persist assertions. Flag this in the PR so the red is not misread.
- **Success criteria** (offline, regression test that reproduces this bug): a fixture project has BOTH a `Status` field (options `Todo, In Progress, Done`) and a `uzi Status` field (options = all 5 board columns), and the link's `status_field_id` points at `uzi Status`. Assert that after `Resync` the link's `status_field_id` is **still** the `uzi Status` field, `unmatched_columns` is **empty**, and the Projects-tab re-link still fired. Prove the key assertion non-vacuous with a mutation at the call site: revert Resync to the by-name resolve and confirm the test reddens (field id flips to `Status`, `unmatched` becomes `{Planned, bug, Human Review, Later}`) — see `.claude/agent-team.md` mutation sections. The fake already models fields by name (`fakeProjectSyncer.fields`); add a by-id read (index the same values by `.ID`), matching M1.

### M3 — Docs + recovery note
- `docs/github-project-sync.md`: state that **Resync re-reads the field the link already uses** (it never switches fields), so "add the option in GitHub, then Resync" is safe on a `uzi Status` board. Remove/qualify any wording implying Resync re-matches by name.
- Add a short **recovery** note for a link already mis-pointed by the old behavior (the fix is forward-looking and does not migrate an already-wrong `status_field_id`): re-run **auto-create** (org: Provision) to re-establish the `uzi Status` link, **or** temporarily rename the built-in `Status` field in GitHub and Resync once so the old by-name path lands on `uzi Status`.
- **Success criteria**: `web/scripts/check-docs.mjs` passes (frontmatter/links); the doc no longer claims Resync resolves by name.

### M4 — Gate + CLI check
- `task gate:api` and `task gate:web` green (`cd web && npm run build` for the docs check). The CLI `uzi project-sync resync` behavior is unchanged from the caller's view (same endpoint, same 200), so no CLI surface change is expected — **verify** there is none needed (`api/cmd/uzi/`, `docs/cli.md`) rather than assuming.
- **Heads-up (repo gotcha, not a defect):** the Go lint ratchet is `whole-files: true` (`.golangci.yml`), so editing `api/internal/forge/projectsync.go` and `api/internal/forgesvc/projectsync.go` makes any *pre-existing* golangci-lint findings in those whole files gate too, not just your new lines. If the gate reddens on lines this PRD didn't touch, that's the ratchet, not your change — read `.claude/rules/go.md` on it before treating it as a regression.
- **Success criteria**: gates green; a note in the PR stating the CLI needed no change (or the change, if one is found).

## Risks & mitigations

- **R1 — Adopt regression.** Changing shared resolution could break first-Adopt's by-name matching. **Mitigation**: only the Resync path changes; Adopt and AutoCreateColumns keep their current resolution. The M2 test asserts Resync; existing adopt tests guard Adopt.
- **R2 — A field the link points at was deleted on GitHub.** `ProjectV2StatusFieldByID` returns not-found. **Mitigation**: surface it as the sync's `last_error` (the existing error path), not a panic; the user re-adopts. State this in M2.
- **R3 — Already-mis-pointed links are not auto-healed.** The fix prevents future re-points but does not rewrite an existing wrong `status_field_id`. **Mitigation**: M3's recovery note; this is intentionally not a data migration.

## Non-goals

- Editing or deleting existing GitHub Status options (still never done, PRD #576 D3).
- Auto-healing already-mis-pointed links (recovery is documented, not automated).
- Column rename/remove propagation (still manual, PRD #364 D5).
- Any `.github/workflows/**` change.

## Dependencies

- PRD #576 (auto-create / `uzi Status` field), PRD #364 (core sync). No new external services or trust boundaries. No migration needed (F-4).

## Decision log

- **D1 — Resync resolves by stored field id, not by name.** By-id is rename-proof and matches how AutoCreateColumns already treats field identity; by-name is kept only for first Adopt, where no stored field exists. The unique-name fact (F-2) means a stored-name lookup would also work, but id survives a field rename and is the more correct identity.
- **D2 — Forward-fix only.** The change stops future re-points; existing mis-pointed links recover via the documented steps (M3), not a migration.
