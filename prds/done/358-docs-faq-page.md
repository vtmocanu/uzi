# PRD #358 — Docs FAQ page (uzi demo)

**Issue**: #358
**Priority**: Low
**Status**: Complete (2026-08-18)

## Purpose

A deliberately tiny, self-contained feature whose real job is to **exercise a uzi
worker end to end**: plan → approve → implement ⇄ validate → branch + MR. Three
milestones, each ~2 minutes of work, all touching one component (`docs/`) and one
gate (`web/scripts/check-docs.mjs`). Nothing here needs the open internet — every
fact the worker needs is stated in this file.

## Problem

uzi's docs are good but scattered: the answer to "what is uzi?", "which forges does
it support?", and "does it ever push to `main`?" each lives in a different page. A
first-time reader has no single FAQ to skim.

## Solution

Add one `audience: user` docs page, `docs/faq.md`, with a few plain-language Q&As,
and link it from `docs/getting-started.md`. That's it.

## Offline contract (worker has no open-web egress — read this, do not research)

The docs system is validated by `web/scripts/check-docs.mjs` (also the first step of
`npm run build` in `web/`). The rules that matter here, taken from `docs/README.md`:

- **Leading-fence frontmatter at byte 0**: the file must start with a `---\n` fence.
  Required fields for a user page: `title`, `order`, `audience`.
- **`audience`** must be one of `user | operator | design | contributor`. This page
  is `audience: user` (renders in-app at `/docs/faq`, searchable, no extra
  registration).
- **`order`** must be **unique among `audience: user` pages** (user and operator are
  separate order namespaces). The current highest user `order` is **107**
  (`docs/findings.md`), so **use `order: 108`**. If the validator reports a collision
  (another page landed at 108 meanwhile), bump to the next free integer.
- **Links must be inline** (`[text](target)`) — reference-style links fail the
  validator. Doc-to-doc links use `./<file>.md`.
- **Body budget**: ≤ 60 body lines per user page (the build *warns* past that, does
  not fail). Keep the FAQ well under 60 lines.
- **Validate offline** with: `node web/scripts/check-docs.mjs` (run from repo root).
  It must exit 0 with no errors. This is the milestone's done-check — no network
  needed.

All Q&A answer text is **given below** and is already accurate against the repo
(`ARCHITECTURE.md`, `CLAUDE.md`). Do not invent new facts or look anything up; write
exactly these answers (light wording polish is fine).

## Milestones

### M1 — Create the FAQ page with its first entry (~2 min)

- [x] Create `docs/faq.md` starting with this exact frontmatter:
  ```
  ---
  title: FAQ
  order: 108
  audience: user
  ---
  ```
- [x] Add a short intro line and the first Q&A:
  - **What is uzi?** uzi ("Uzinele Întunecate") is an AI dark factory: agents pick
    up `PRD`-labeled issues on your connected forge and work them end to end (plan →
    approval gate → implement ⇄ review → branch + merge request), never touching
    `main`.
- [x] Run `node web/scripts/check-docs.mjs`; it must exit 0.

### M2 — Add two more Q&As (~2 min)

- [x] Append two more Q&As to `docs/faq.md`, keeping the body ≤ 60 lines:
  - **Which forges does uzi support?** GitLab, GitHub, and Forgejo/Gitea — one forge
    driver each. You connect one forge per account.
  - **Does uzi ever push to `main`?** No. Four independent guardrail layers keep
    `main` untouched: a forge Developer role on a protected branch, the worker (not
    the agent) holding the PAT, an SDK deny-hook that blocks `git push` and history
    rewrites, and `settingSources: []` so a cloned repo's own `.claude/` is never
    loaded. Agents only open merge requests from `agent/*` branches.
- [x] Re-run `node web/scripts/check-docs.mjs`; it must exit 0.

### M3 — Link the FAQ from Getting started (~2 min)

- [x] In `docs/getting-started.md`, add one inline doc-to-doc link to the new page,
  e.g. a short line such as: `See the [FAQ](./faq.md) for quick answers.` Place it
  where it reads naturally (a "Next steps" / closing spot is fine); do not restructure
  the page.
- [x] Re-run `node web/scripts/check-docs.mjs`; it must exit 0 (this proves the
  doc-to-doc link resolves).

## Success criteria

- `docs/faq.md` exists, is `audience: user`, has valid unique-`order` frontmatter, and
  three Q&As with the answers above.
- `docs/getting-started.md` links to it with a working inline relative link.
- `node web/scripts/check-docs.mjs` exits 0 (no errors; a line-budget warning would be
  acceptable but the page should stay well under 60 lines).
- A merge request is opened from an `agent/*` branch. `main` is untouched.

## Out of scope

- Screenshots / images.
- Any code, API, CLI, or web-component change.
- Rewording or restructuring existing docs beyond the single link line in M3.

## Notes / risks

- **Only real risk**: an `order` collision at 108 if another user page lands first —
  the validator catches it; bump to the next free integer.
- No new dependencies. No network. Diff is confined to `docs/faq.md` (new) and one
  line in `docs/getting-started.md`.
