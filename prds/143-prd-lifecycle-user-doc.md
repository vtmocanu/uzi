# PRD #143: a user-facing page for the `prd-lifecycle` skill

**GitLab Issue**: [#143](https://gitlab.example.com/vtmocanu/uzi/-/issues/143)
**Status**: Draft
**Priority**: Low

## Problem

PRD #72 shipped the `prd-lifecycle` builtin skill, which is what lets a run update
its own PRD and move it to `prds/done/` when the work is finished. `docs/skills.md`
mentions it in passing, but there is no page a user can read to understand what the
run will actually do to their PRD file, or when it will decline to move it.

That matters more than a normal doc gap: the run **edits the user's spec file** and
commits the change into the MR. Somebody approving that MR should be able to look up
the rule beforehand rather than inferring it from a diff.

## Solution Overview

One short user-facing docs page, linked from `docs/skills.md`.

## Milestones

- [ ] **M1 — the page**: create `docs/prd-lifecycle.md` with leading-fence
      frontmatter (`title`, `order`, `audience: user`). Content: what the skill does
      (scan unchecked items, tick only on direct evidence, move to `prds/done/` only
      when every box is checked), what it deliberately does **not** do (no mid-run
      push; a partially-complete PRD stays put; a PRD already under `done/` is a
      no-op), and that it is an instruction to a model rather than an enforced
      guarantee. **Verified**: `cd web && npm run check-docs` passes, and the page
      has a unique `order`.

- [ ] **M2 — link it**: add a pointer from `docs/skills.md`'s `prd-lifecycle`
      section to the new page. **Verified**: the relative link resolves under
      `check-docs`, which fails the build on a broken one.

- [ ] **M3 — confirm it renders in-app**: load `/docs/prd-lifecycle` on the deployed
      instance (`uzi.example.com`) and confirm it appears in the docs nav at
      its `order` and renders. **Verified**: a human loads the page.

      **This milestone cannot be completed by the run that implements M1 and M2.**
      It requires the change to be merged, released, and deployed first. A run that
      implements M1 and M2 must leave this box unchecked and leave this PRD in
      `prds/`.

## Success Criteria

- A user approving a PRD-touching MR can look up the rule the run followed.
- The page states plainly that the behaviour is prompt-level, not enforced.

## Out of Scope

- Changing any of the `prd-lifecycle` behaviour. This is documentation only.
- The mandated manual acceptance run for PRD #72 M3 — tracked on #72.
