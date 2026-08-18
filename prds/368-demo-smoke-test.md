# PRD #368 — Demo: docs smoke test for uzi

**Issue**: [#368](https://github.com/vtmocanu/uzi/issues/368)
**Priority**: Low
**Status**: Not started

## Problem

We want to confirm uzi drives an issue end to end — plan, implement, review, open an
MR, never touch `main` — with a task small enough that a flaky code gate (`-race`,
the Go lint ratchet, vitest contention) can't make the test inconclusive.

## Solution

A **docs-only** demo, built in three roughly-2-minute milestones. Docs-only means
zero blast radius, no `-race`/lint-ratchet exposure, and the only gate that matters
is the docs validator `web/scripts/check-docs.mjs` (wired into `npm run build` and
the web gate). The change is entirely internal to this repo, so it needs **no open
internet** — an offline uzi worker can complete and self-verify it end to end.

Each milestone is independently checkable by running the validator; there is no
external dependency to resolve.

## Scope

**In scope**: one new user-facing doc page (`docs/demo.md`) and one cross-link to it
from `README.md`.

**Out of scope**: any Go, TypeScript, SQL, or config change; any new route, DTO, or
CLI surface. This is a throwaway smoke test — the resulting MR is demo cruft, safe to
revert after the run is validated.

## Facts the implementer needs (all offline-verifiable)

- **Docs contract** (`docs/README.md`): every page needs a leading `---` frontmatter
  fence at byte 0 with `title` and `audience` (required), and `order` (required and
  **unique within its audience** for `audience: user` pages). Audiences are
  `user | operator | design | contributor`; only `user`/`operator` render in-app at
  `/docs/<slug>`, where `<slug>` is the filename without `.md`.
- **Free `order` value**: the highest `order` currently used among `audience: user`
  pages is **107** (`docs/findings.md`). **108** is free — use it. (Re-confirm before
  writing with: `grep -h '^order:' docs/*.md | sort -n | tail`.)
- **Link rules** (`web/scripts/check-docs.mjs`): use **inline** links only —
  `[text](target)`. **Reference-style** definitions (`[label]: target`) FAIL the
  build. A relative link whose target file does not exist FAILS the build. Doc→doc
  links are resolved relative to `docs/`, so a link to another doc is just its
  filename, e.g. `[Getting started](getting-started.md)`.
- **House-style budget**: `audience: user` pages target **≤ 60 body lines**
  (frontmatter not counted); over budget only **warns**, it does not fail. The demo
  page stays far under this.
- **README anchor for the cross-link**: `README.md` has a golden-path docs list of
  `- [Title](docs/<slug>.md)` bullets near the bottom (it currently ends with
  `- [Developer conventions](docs/dev-conventions.md)`). Append the demo entry there.

## Milestones

### M1 — Create the demo doc page (~2 min)

Create `docs/demo.md` with valid frontmatter and a short intro:

```
---
title: Demo
order: 108
audience: user
---

# Demo

A throwaway page created to smoke-test uzi's end-to-end run pipeline
(plan → implement → review → MR). It carries no product meaning and is safe
to delete.
```

**Done when**: `node web/scripts/check-docs.mjs` exits 0, and the page has a valid
frontmatter fence at byte 0 (`title`, `order: 108`, `audience: user`).

### M2 — Add a section with a working inline relative link (~2 min)

Append a short section to `docs/demo.md` with two or three numbered steps and exactly
one **inline relative link** to an existing user doc, for example:

```
## What uzi does

1. You label an issue `PRD` and start a run.
2. uzi plans, you approve, then it implements and opens an MR — see
   [Getting started](getting-started.md).
3. uzi never touches `main`.
```

Use an inline `[text](target)` link only (no reference-style `[label]: target`), and
point it at a file that exists under `docs/` (`getting-started.md` exists).

**Done when**: `node web/scripts/check-docs.mjs` exits 0 (its broken-relative-link
and reference-style-link checks both pass).

### M3 — Cross-link the page from README (~2 min)

In `README.md`, append `- [Demo](docs/demo.md)` to the golden-path docs bullet list
(the one that currently ends with `- [Developer conventions](docs/dev-conventions.md)`).
Confirm `docs/demo.md`'s body stays ≤ 60 lines (it will).

**Done when**: `node web/scripts/check-docs.mjs` exits 0, `README.md` links to
`docs/demo.md`, and the page would render in-app at `/docs/demo`.

## Success criteria

1. `docs/demo.md` exists, `audience: user`, `order: 108`, valid frontmatter.
2. The page has a body section with one working inline relative link.
3. `README.md` links to the page.
4. `node web/scripts/check-docs.mjs` exits 0 (equivalently, the web gate / `npm run
   build` docs check passes). No other gate is touched because no code changed.
5. An MR is open against a branch; `main` is untouched.

## Risks & mitigations

- **Duplicate `order`** → build fails. Mitigated: 108 is confirmed free; re-check with
  the `grep` above before writing.
- **Reference-style or broken link** → build fails. Mitigated: use inline links to an
  existing target only.
- **Merging demo cruft into `main`** → the page and README bullet are harmless and
  clearly labeled a throwaway; revert the MR after the smoke test if unwanted.

## Dependencies

None. No internet, no other PRD, no schema/migration, no code change.

## Milestone dependency graph

Sequential — M1 → M2 → M3 all touch `docs/demo.md` (M3 also `README.md`). No
parallelization; each step's `check-docs.mjs` pass gates the next.
