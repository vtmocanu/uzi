# docs/

This directory is the single source of truth for uzi's howtos and design
docs. `audience: user` pages are also bundled at build time into the
in-app `/docs` section (PRD #7); everything else stays repo-only. There is
no separate docs site or duplicated content: the same file you edit here is
what both a browser and a `git blame` see.

This file itself is exempt from the frontmatter contract below and from
`check-docs.mjs`'s validation (it is the index/meta page, not a doc).

## Adding a page

1. Create `docs/<slug>.md`. The slug is the filename; it becomes the route
   (`/docs/<slug>`) if the page is `audience: user`.
2. Add the frontmatter (a leading `---` fence, at byte 0):

   ```yaml
   ---
   title: GitLab bot setup
   order: 20
   audience: user        # user | operator | design | contributor
   ---
   ```

   - `title` and `audience` are required on every page (this file excepted).
   - `order` is required, and must be unique, only for `audience: user`
     pages; it decides the page's position in the in-app index. Non-`user`
     pages may omit it.
   - A missing or malformed frontmatter fence doesn't fail the build: the
     page silently falls back to `audience: design` (repo-only). That's a
     safety net for a half-written page, not something to rely on.
3. Write the content (house style below), run the validator, and link the
   page from wherever a reader would land on it (another doc, `README.md`,
   or `ARCHITECTURE.md`).

## Audiences

| Audience | Who it's for | Where it lives |
|---|---|---|
| `user` | Someone using the running app | In-app `/docs` index + repo |
| `operator` | Someone deploying/configuring the stack | Repo only |
| `design` | Someone reviewing why something was built this way | Repo only |
| `contributor` | Someone changing uzi's own code or tests | Repo only |

## House style for `user` pages

Task-titled, numbered steps, no design essays (link to a `design`/
`operator` page or `ARCHITECTURE.md` instead of re-explaining the
mechanism). Target **≤ 60 body lines** (frontmatter not counted); the
build warns, but does not fail, past that.

## Screenshots

One per major step, in `docs/img/`, named `<page>-<step>.png` (e.g.
`board-move-card.png`), referenced with real, descriptive alt text:

```markdown
![Dragging a card between board columns, relabeling the underlying GitLab issue](img/board-move-card.png)
```

Each image must stay under 300 KB (`check-docs.mjs` fails the build past
that; images ship to every visitor, not just docs readers). It's fine to
reference an image before it exists: placeholders land first, real
captures replace them later in one dedicated commit, and a missing file
only breaks the build once the reference itself is broken (a typo'd
path), not while the file is merely a placeholder.

## Links

- Doc-to-doc: `./<file>.md`, preserving any `#anchor`.
- Repo-root files (`../ARCHITECTURE.md`, `../plan.md`): the in-app viewer
  rewrites these to the pinned GitLab blob URL, since they aren't bundled.
- **Inline links only** (`[text](target)`, `![alt](target)`): reference-style
  links and images (`[text][ref]`, `[label]: target`) fail the validator,
  since they're invisible to its link-existence check.

## Validating

```sh
node web/scripts/check-docs.mjs
```

Also runs as the first step of `npm run build` inside `web/`. It fails on
missing/invalid frontmatter, a duplicate `order` among `user` pages, a
broken relative link (doc→doc or doc→img), reference-style links, and an
oversized image; it warns (without failing) on a `user` page over the line
budget.
