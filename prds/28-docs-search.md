# PRD #28: Docs Search — Client-side Full-text Search on /docs

**GitLab Issue**: [vtmocanu/uzi#28](https://gitlab.example.com/vtmocanu/uzi/-/issues/28)
**Status**: In review (2026-07-09). M1-M3 done (commits 63b10e5, 2bba76f); review + audit + web-ux browser validation clean; M4 docs note + specs/ai.md done (d42504b, a564c97); specs/human.md proposal awaiting user approval; MR open.
**Priority**: Medium
**Created**: 2026-07-09
**Depends on**: PRD #7 (docs viewer, done). No API/DB/service changes.

## Problem

PRD #7 shipped the in-app `/docs` section and explicitly deferred search: "client-side filter is a follow-up if the page count grows." The page count grew — there are now 11 `audience: user` pages (getting-started, board, skills, autopilot, prdless, worker-setup, worker-model, admin-settings, …) and every feature PRD adds more. A user looking for "join token" or "PRDLESS" has to open pages one by one; titles and one-line summaries on the index don't surface body content.

## Solution Overview

A search box at the top of the `/docs` index that live-searches the **full text** of all `user` pages and shows ranked results with highlighted snippets. Everything needed is already in the browser: the docs viewer bundles every page body as a raw string at build time (`web/src/lib/docs.ts`), so search is pure client-side — **no API surface, no new service, no new dependency**.

Decisions fixed with Vlad (2026-07-09): full-text with snippets (not a title/summary filter); search box on the `/docs` index only (not on individual doc pages).

Inspiration audit (2026-07-09, submodule grep; corrected same day after fact-check):

| Concern | multica | bottega | dot-agent-deck | uzi will do |
|---|---|---|---|---|
| Search feature | Real docs search: Fumadocs/Orama via a **server-side API route** (`apps/docs/app/api/search/route.ts`, `createFromSource` + CJK/JP tokenizers) — closest prior art | `fuse.js` ^7 is a **declared-but-unused** dependency in its reference app (no import, no `new Fuse` anywhere) | Docusaurus site (search delegated to the framework) | Hand-rolled client-side tokenized substring search |
| Why / why not | Needs a server search endpoint + a docs framework; uzi's docs are already fully bundled client-side, and PRD #7 rejected a docs service/framework | Nothing to copy — no live search implementation | Second deployable; already rejected in PRD #7 | 11 short pages, all in memory; exact multi-token AND matching with transparent ranking beats a search dep at this scale |

Deliberately not copied: multica's server-route search (uzi has no docs backend by design) and a fuzzy-search dep like `fuse.js`. If the corpus grows past the point where substring search feels dumb (typos, stemming), swapping the pure search module for a library is a contained change — the UI consumes `searchDocs(query): SearchResult[]` either way.

## Technical Design

### Search core — `web/src/lib/docsearch.ts` (pure, unit-testable)

- **Corpus**: only `audience: user` docs (what the index lists — `listUserDocs()`). Each doc is indexed as `{ slug, title, headings, plainBody }`.
- **Markdown stripping** for `plainBody`: links/images reduced to their text (reuse the regex approach from `summarize()`), emphasis/backtick markers dropped, code fences kept as text (users search for commands like `docker compose --profile agent up`), tables kept as cell text, heading markers dropped but heading text retained (also collected separately for ranking).
- **Matching**: case-insensitive, multi-token AND — every whitespace-separated query token must appear as a substring somewhere in the doc (title, headings, or body). Query under 2 characters → no search (show the normal index). Matching and occurrence counting use **plain substring scanning (`indexOf` loops), never `new RegExp(token)`** — query tokens are user input full of regex metacharacters by design (`.env`, `--profile`, `c++`); a test pins this.
- **Ranking**: title match > heading match > body match; within a tier, more total token occurrences first; slug as stable tiebreak (same spirit as `sortDocsForIndex`).
- **Snippets**: for each result, a ~160-char window centered on the first body match, word-boundary trimmed with leading/trailing `…`, plus match ranges so the UI can `<mark>` the matched tokens. Title-only matches fall back to the doc's existing `summary`. Not every query token necessarily appears in the snippet — a token that matches only in the title/headings has no body range (test covers the mixed title-token + body-token case).
- **API**: `searchDocs(query: string): SearchResult[]` where `SearchResult = { doc: Doc, snippet: string, ranges: [start, end][] }`. Contract: `ranges` are **snippet-relative, sorted, and merged/non-overlapping** (overlapping tokens like `work` + `worker` collapse into one range), so the UI's split-and-mark is a straight fold — no nested `<mark>`s. The UI never touches the internals.

### UI — `web/src/pages/Docs.tsx`

- The repo's `Input` primitive (`web/src/components/ui.tsx`) with `type="search"` at the top of the index, labelled, with placeholder ("Search docs…") — not a bare `<input>`, so focus/border styling stays consistent. No debounce needed — 11 docs, synchronous, instantaneous.
- Empty/short query → the existing ordered card list, untouched. Active query → result cards (same `Card` visual language): title + snippet with `<mark>`-highlighted tokens. **`<mark>` must be explicitly styled** — there is no highlight token in the theme and the UA default is opaque yellow, unusable on the dark theme. Pin: reuse an existing accent at low alpha (e.g. `bg-warn/25 text-fg` or a new `--highlight` token), overriding the UA `mark` background/color.
- No-results state ("No docs match \"…\"") inside a `Card`, consistent with the existing empty state.
- Accessibility: the result region carries `aria-live="polite"` announcing the result count ("5 docs match") as it updates per keystroke; the input is labelled.
- Keyboard: `Escape` clears the query; `/` anywhere on the index focuses the box (skipped when focus is already in an input/textarea). Highlighting uses React elements (split text + `<mark>` nodes), **never** `dangerouslySetInnerHTML`.
- Responsive + dark-theme-consistent per the existing docs shell. No nginx/CSP change (no new asset types, no inline script).

### What this deliberately is not

- Not a global nav search, not on `/docs/:slug` pages (decision 2026-07-09).
- Not fuzzy/stemmed — exact substring AND matching.
- Not indexing `operator`/`design`/`contributor` pages — search surfaces exactly what the index lists.

## Milestones

- [x] **M1 — Search core**: `web/src/lib/docsearch.ts` with markdown stripping, tokenized AND matching (substring scanning, no `new RegExp(token)`), tier ranking, and snippet + merged-range extraction; unit tests (`docsearch.test.ts`) covering stripping edge cases (code fences, tables, links), ranking tiers, multi-token AND, snippet windowing, the short-query guard, regex-metachar tokens (`.env`, `--profile`), overlapping-token range merging, and the mixed title-token + body-token snippet case. *Files: `web/src/lib/docsearch.ts`, `web/src/lib/docsearch.test.ts`.*
- [x] **M2 — Index UI**: search box on `/docs` (via the `Input` primitive) with live results, `<mark>` highlighting (explicit dark-theme styling), no-results state, `aria-live` result count, `Escape`/`/` keyboard handling; empty query renders the existing card list byte-for-byte; jsdom component tests (`Docs.test.tsx`, existing vitest + testing-library infra) for query → results (body-only term from `worker-setup` found, snippet marked) → clear flow. **These component tests are the behavioral gate for search** — see M3. *Files: `web/src/pages/Docs.tsx`, `web/src/pages/Docs.test.tsx` (new). Depends on M1.*
- [x] **M3 — Whole-suite green + smoke**: `npm run typecheck`, `npm test`, `npm run build` (includes `check-docs.mjs`) green; compose smoke limited to what curl can assert — the built image serves `/docs` (HTML + JS bundle load). Search behavior itself is client-side JS and the repo has no browser automation, so the automated behavioral gate is M2's jsdom tests; a manual browser check of one body-only search is a nice-to-have, not a gate.
- [ ] **M4 — Docs & spec sync**: one-line note in `docs/README.md`'s add-a-page section that `user` pages are automatically searchable (nothing to register); `specs/ai.md` records the design decisions; PRD closed out, moved to `prds/done/`.

### Parallelization

Sequential — M1 → M2 → M3 → M4; single small surface (`web/` only), no cross-agent split worth the coordination.

## Success Criteria

- A term that appears only in a page body (not title/summary) is findable from `/docs` in one keystroke sequence, with a snippet showing the match in context — asserted by a jsdom component test against the real bundled corpus.
- Zero new dependencies, zero API/DB/service changes; `web` image builds as before.
- Empty-query behavior is pixel-identical to today's index.
- Adding a future docs page requires no search-related work — the corpus is derived from the same glob/frontmatter pipeline as the index.

## Risks & Mitigations

- **Markdown stripping distorts snippets** (tables/fences render oddly as plain text) → snippet tests pin the behavior on real bundled pages; snippets are plain text by design, not mini-markdown.
- **Search UX creep** (fuzzy, cross-page box, nav shortcut) → scope fixed by the 2026-07-09 decisions; follow-ups get their own issue.
- **Corpus grows enough to need real search** → the pure-module boundary (`searchDocs`) makes a library swap contained; noted in the inspiration audit.

## Out of Scope

- Search on individual `/docs/:slug` pages or in the global nav.
- Fuzzy matching, stemming, typo tolerance (bottega's `fuse.js` approach).
- Searching non-`user` (repo-only) docs.
- Search analytics, query history, deep-linking to a search state (`?q=`).

## Decision Log

- 2026-07-09 — Full-text + snippets over title/summary filter; `/docs` index only (Vlad). Body content is where the value is; a filter would miss it.
- 2026-07-09 — Hand-rolled tokenized search over a search dep: 11 short in-memory pages don't justify one; transparent ranking, contained swap path if that changes.
- 2026-07-09 — Review-wave fixes (reviewer + fact-checker agents): inspiration audit corrected (multica has a real Fumadocs/Orama **server-route** docs search, not chat search; bottega's `fuse.js` is declared but unused); M3 compose smoke rescoped to curl-assertable checks (search is client-side JS, repo has no browser automation — jsdom tests in M2 are the behavioral gate); pinned substring-not-regex matching, merged non-overlapping snippet ranges, explicit `<mark>` dark-theme styling, `aria-live` result count, `Input` primitive; "theming" example dropped (it is `audience: design`, not searchable).
