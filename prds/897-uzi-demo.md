# PRD #897 — uzi demo (three 2-minute milestones)

**Issue**: [#897](https://github.com/vtmocanu/uzi/issues/897)
**Priority**: Low
**Status**: Planned

## Purpose

A throwaway PRD whose only job is to exercise a full uzi run: plan gate -> implement -> gate -> branch + MR. It is NOT a real feature. Three milestones, each independent, purely additive, gate-safe, and roughly two minutes of work. All facts needed are stated here; nothing requires the open web.

## Constraints (read first)

- **Purely additive, no behaviour change.** Each milestone creates or appends; none edits existing logic. There is no user-facing runtime behaviour to change, so `specs/human.md` needs no edit.
- **No workflow files.** The branch diff must not touch `.github/workflows/**` (the worker PAT lacks `workflow` scope; any such change is an atomic push rejection that loses the branch). None of these milestones go near CI workflow files. Verify before finalizing: `git log --name-only origin/main..HEAD -- .github/workflows/` must come back empty.
- **Gate before done.** Run `task gate` (or at minimum the touched component gates: `gate:web` for M1, `gate:repo` is unaffected, `gate:api` for M3) and confirm green before opening the MR. Component gates run serially by design.

## Milestones

### M1 — Add a user-facing docs page (`web` surface)

Create `docs/demo.md` with valid leading-fence frontmatter and a short body.

- Frontmatter keys required by `web/scripts/check-docs.mjs` (runs inside `npm run build`): `title`, `order`, `audience`.
- Use `audience: user` so the page renders in-app at `/docs/demo`.
- **`order: 110`** — the current maximum `order` across `docs/*.md` is `109` (`docs/changelog.md`), and `order` must be unique, so `110` is the next free slot. Confirm offline in the clone with `awk -F': *' '/^order:/{print $2}' docs/*.md | sort -n | tail -1` before committing.
- Body: two or three sentences stating this is a uzi demo page created to exercise the docs pipeline. No relative links (the checker fails on broken ones); if you add one, point it at an existing doc.

Suggested content:

```markdown
---
title: Demo
order: 110
audience: user
---

# Demo

This page exists to exercise uzi end to end: it was created by a uzi worker
implementing PRD #897. It carries no product meaning and can be removed at any time.
```

**Done when**: `docs/demo.md` exists with valid frontmatter and `task gate:web` (which runs `check-docs.mjs`) is green.

### M2 — Add a CHANGELOG entry (repo surface)

Append one bullet under `## [Unreleased]` -> `### Added` in `CHANGELOG.md` (that section already exists).

- Follow the file's format exactly (stated in its own header): a **bold title on its own physical line**, then the description on the **next physical line with no blank line between**, the description on **one physical line** (no mid-description newlines), indented two spaces.
- Reference the issue: `([#897](https://github.com/vtmocanu/uzi/issues/897))`.

Suggested bullet (place it as the first item under `### Added`):

```markdown
- **A demo docs page and helper landed to exercise a full uzi run ([#897](https://github.com/vtmocanu/uzi/issues/897)).**
  Adds `docs/demo.md` and an `api/internal/demo` helper with a unit test; the change is additive, carries no product behaviour, and exists only to drive a uzi run end to end.
```

**Done when**: the bullet is present and correctly formatted; no gate covers CHANGELOG prose, so a visual check against the format rule suffices.

### M3 — Add a tiny Go helper with a unit test (`api` surface)

Create a new self-contained package `api/internal/demo/` (it does not exist today) with one exported function and a test that calls it.

- `api/internal/demo/demo.go`:

```go
package demo

// Greeting returns a fixed demo string. It exists only to exercise a uzi run
// (PRD #897) and is intentionally trivial.
func Greeting() string {
	return "hello from uzi demo"
}
```

- `api/internal/demo/demo_test.go`:

```go
package demo

import "testing"

func TestGreeting(t *testing.T) {
	if got, want := Greeting(), "hello from uzi demo"; got != want {
		t.Fatalf("Greeting() = %q, want %q", got, want)
	}
}
```

Gate notes (why this is safe):
- golangci-lint `unused` reports only *unexported* dead symbols; `Greeting` is exported, so it is not flagged.
- `deadcode -test ./...` (the Go dead-code gate) treats `Test*` functions as entry points, so `Greeting` is reachable through `TestGreeting` and the dead-code gate stays at zero.
- Run `gofmt` (tabs, as shown) so the format check passes.

**Done when**: `task gate:api` is green (fmt, lint, dead code, and `go test` including `TestGreeting`).

## Validation

- `task gate` green (or each touched component gate: `gate:web`, `gate:api`).
- `git log --name-only origin/main..HEAD -- .github/workflows/` is empty (no workflow-scope rejection).
- MR opened against `main`; `main` itself untouched (uzi's standing guardrail).
