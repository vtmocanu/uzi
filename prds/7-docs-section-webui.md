# PRD #7: In-app Docs Section — Terse Howtos with Screenshots

**GitLab Issue**: [vtmocanu/uzi#7](https://gitlab.example.com/vtmocanu/uzi/-/issues/7)
**Status**: In progress (2026-07-04: M1–M4 done on `prd-7-docs-section`; 4-agent review wave — reviewer/auditor/fact-checker/tester — passed with all findings fixed; M5 + spec sync pending; real-screenshot swap pending as the single final commit)
**Priority**: Medium
**Created**: 2026-07-04
**Depends on**: PRD #1 (web shell, done). Content covers PRD #2/#3 features (done). No dependency on PRD #4/#5/#6; new runtime docs land with those PRDs.

## Problem

plan.md requires "a docs section on uzi with relevant howtos (how to create an agent/bot/skill, how to do gitlab bots/give permissions, etc, include screenshots)". Today the howtos exist only as repo markdown (`docs/*.md`) — invisible from inside the webui, where users actually hit the moments that need them (connecting a forge, pasting an Anthropic token, editing an agent template). They are also written longer than their audience will read: humans with small attention spans need scannable steps, not design essays.

## Solution Overview

A public `/docs` section in the SPA that renders a curated set of **terse** howto pages, bundled at build time from the repo's `docs/` directory — one source of truth, zero new services, zero API/DB changes. Screenshots (captured by Vlad, requested explicitly in M3) live in `docs/img/` and ship in the same bundle.

Inspiration audit (2026-07-04, submodule grep):

| Concern | multica | bottega | dot-agent-deck | uzi will do |
|---|---|---|---|---|
| In-app docs section | None | None | None in-app; ships a **standalone Docusaurus site** (`site/`) | `/docs` index + per-page routes, public |
| Markdown rendering | `react-markdown` + `remark-gfm` + `rehype-raw`+`rehype-sanitize` (+ katex/math/breaks; hardened, for *model-generated* chat output) | `react-markdown` + `remark-gfm` in its reference app, for chat messages/editor — no docs route | None (Rust TUI) | `react-markdown` + `remark-gfm`, **no `rehype-raw`** — content is repo-authored, so raw HTML simply stays inert instead of needing a sanitizer |
| Docs source of truth | n/a | n/a | Separate `site/` tree (drift risk vs the TUI it documents) | Repo `docs/*.md`, bundled at build time (no drift between repo docs and in-app docs) |

Pattern copied: multica's and bottega's choice of `react-markdown`, whose core builds a React element tree rather than injecting HTML (multica's own code-highlighting components do use `dangerouslySetInnerHTML`; we adopt the renderer, not those). Deliberately not copied: multica's `rehype-raw` + sanitize pipeline — needed for untrusted LLM output, dead weight for trusted, repo-reviewed markdown — and dot-agent-deck's standalone Docusaurus site: a second deployable and a second source tree is overkill for five howto pages whose readers are already inside the webui.

## Technical Design

### Single source of truth: repo `docs/`, bundled at build time

- The web build imports `docs/*.md` (excluding `README.md`) as raw strings via Vite glob (`import.meta.glob('../../../docs/*.md', { query: '?raw', import: 'default', eager: true })` from `web/src/lib/docs.ts`) and `docs/img/*` via a `?url` glob (hashed asset URLs).
- **Glob paths resolve relative to the source file, so the image build must preserve the `web/`↔`docs/` sibling layout**: `docker-compose.yml` `web.build` becomes `{ context: ., dockerfile: web/Dockerfile }`; the Dockerfile copies `COPY web/ /app/web/` + `COPY docs/ /app/docs/` with `WORKDIR /app/web` (not today's flattened `COPY . .` into `/app`, which would put `docs.ts` at `/app/src/lib/` and break the `../../../docs` depth). `web/scripts/check-docs.mjs` reaches docs at a *different* depth (`../../docs` from `web/scripts/`) — both only work if the sibling layout is preserved.
- **Root `.dockerignore` details are load-bearing**: it must exclude `.git`, `inspiration/`, `api/`, `web/node_modules`, `web/dist` (the existing `web/.dockerignore` stops applying once the context is the repo root) and must **not** blanket-exclude `*.md` the way `web/.dockerignore` does today — that would strip the very files this feature bundles.
- Vite dev mode needs `server.fs.allow` to include the repo root (raw imports are read from disk in dev; the production build via rollup is unaffected).

### Frontmatter contract (which pages appear in-app)

Every file in `docs/` gets minimal YAML frontmatter:

```yaml
---
title: GitLab bot setup
order: 20
audience: user        # user | operator | design | contributor
---
```

Only `audience: user` pages are listed and routable in-app, ordered by `order`. `operator` (installation, configuration), `design` (auth-design), and `contributor` (dev/testing conventions, e.g. the E2E-test-bot section trimmed from gitlab-bot-setup) pages stay repo-only — the audience inside the webui already has a running instance. `order` is required (and duplicate-checked) only for `user` pages. The slug is the filename (`gitlab-bot-setup.md` → `/docs/gitlab-bot-setup`); `docs/README.md` is excluded from the glob and from frontmatter validation. Frontmatter is parsed by a ~15-line hand-rolled parser (the format is ours and constrained; `gray-matter` drags Buffer polyfills into the browser bundle) that consumes only a **leading** `---` fence starting at byte 0 — `---` later in a body (e.g. inside agent-templates.md's embedded code fence) is content. A file with no frontmatter renders as `audience: design` (repo-only) rather than erroring, so a pre-M2 build stays green.

### Viewer

- Routes: `/docs` (index: title list with one-line summaries) and `/docs/:slug`. **Public, no auth** — bot setup and token howtos are exactly what a user needs *before* they can do anything in uzi, and nothing in `docs/` is secret (the stack is loopback-only; the same files are world-readable in the repo). An unknown slug renders a not-found state inside the docs shell (with a link back to `/docs`), not the App-level catch-all redirect to `/`.
- Renderer: `react-markdown` + `remark-gfm` — two new deps, added (with lockfile) as an explicit M1 step. The existing docs use GFM tables. Raw HTML is not rendered (no `rehype-raw`) — inert by default. External links get `rel="noopener noreferrer"`. No nginx CSP change needed (same-origin `img-src 'self'`, class-based Tailwind, no inline scripts — verified against current `web/nginx.conf`).
- Link rewriting: relative `*.md` links between docs resolve to `/docs/:slug` when the target is a bundled `user` page; links to repo-only files (`../plan.md`, `auth-design.md`) rewrite to the pinned GitLab blob base `https://gitlab.example.com/vtmocanu/uzi/-/blob/main/` + repo-relative path. `#anchor` fragments are preserved in both cases (the existing docs lean on them, e.g. `ARCHITECTURE.md#forge-integration`). Relative `img/*` sources resolve through the asset-URL map.
- Styling: Tailwind prose-style rules consistent with the existing dark theme; responsive (mobile-first, per plan.md's adaptive-width requirement). Nav gets a "Docs" link in both authenticated and logged-out states (`web/src/components/Layout.tsx`).

### Content plan — terse or it didn't happen

House style for every `user` page: task-titled, numbered steps, one screenshot per major step, no design rationale (link to design docs instead), target **≤ 60 lines** per page (a target enforced as a build warning, not a hard gate — see M4).

| Slug | State | Work |
|---|---|---|
| `getting-started` | new | The golden path: register → create bot → connect forge → enable repo → board → paste Anthropic token. Mostly links into the other pages. |
| `gitlab-bot-setup` | exists (82 ln) | Trim to steps; move the why-a-bot rationale to a one-liner + ARCHITECTURE link. |
| `anthropic-token` | exists (85 ln) | Trim; keep the two-credential table, cut the security essay (link auth-design/ARCHITECTURE). |
| `agent-templates` | exists (116 ln) | Trim to what a user does in the UI; builtin table stays, renderer internals move to design docs or get cut. |
| `board` | new | Kanban howto: what columns are, how moves write labels back, why only `PRD`-labeled issues with a PRD link appear. |

`installation.md`, `configuration.md` → `audience: operator`; `auth-design.md` → `audience: design`. Content cut from user pages that is still worth keeping moves to an explicit destination, not into the void — M2 must record the mapping. Two known cases up front: gitlab-bot-setup's E2E-test-bot section (dev/testing convention) → a `contributor` page; agent-templates' renderer/validation/API internals → ARCHITECTURE.md's Agent templates section (already covers the renderer) or cut where it duplicates it.

### Screenshots

- Stored in `docs/img/`, kebab-named after page + step (`board-move-card.png`), PNG, each ≤ 300 KB (bundle ships to every visitor), meaningful alt text.
- Captured by **Vlad on the running stack** — M3 starts with a checklist of requested shots (register form, forge connect + verify, repo enable, board with cards, token settings, agent template editor). Pages ship with **placeholder images** (clearly-marked generated PNGs in `docs/img/`, alt text describing the intended shot) until Vlad delivers the real captures; swapping placeholders for real screenshots is a **single final commit** after all other milestones land (decision 2026-07-04).

### Build-time validation

`web/scripts/check-docs.mjs`, run as part of `npm run build` (and standalone): fails the build on missing/invalid frontmatter (README.md exempt), duplicate `order` among `user` pages, broken relative links (doc→doc and doc→img), and warns (not fails) on any `user` page over the line budget. This is the guard that keeps in-app docs from silently rotting — there is no CI yet (plan.md: later), so the image build is the gate.

## Milestones

- [x] **M1 — Docs viewer infrastructure**: deps added (`react-markdown`, `remark-gfm`); `/docs` + `/docs/:slug` routes render bundled markdown with GFM tables, images, rewritten links, and an in-shell 404; nav link present logged-in and logged-out; responsive + dark-theme-consistent; compose context/Dockerfile/root-`.dockerignore` reworked per the sibling-layout spec above; `docker compose build web` green **against pre-M2 docs** (frontmatterless files default to repo-only, and one existing doc gets seed frontmatter as the M1 rendering/link-rewrite fixture). *Files: `web/*`, `docker-compose.yml`, `.dockerignore`, + frontmatter on one doc.*
- [x] **M2 — Content curation**: frontmatter on all `docs/*.md`; the five `user` pages exist, terse (target ≤ 60 lines), house style; cut material relocated per the recorded mapping (`operator`/`design`/`contributor`); existing repo links (README, ARCHITECTURE) still resolve. *Files: `docs/*` only — runs in parallel with M1; the schema contract is fixed in this PRD, and M1 does not depend on M2's content thanks to the graceful-no-frontmatter rule.*
- [x] **M3 — Screenshots**: shot checklist presented to Vlad; **placeholder images** wired into pages with alt text now (not gating anything); real captures replace them in a single final commit after all other milestones. *Depends on M1 + M2.* (Placeholders shipped — 7 PNGs in `docs/img/`; real-capture swap pending.)
- [x] **M4 — Validation gate**: `check-docs.mjs` wired into the build and failing on frontmatter/link rot; whole suite green (`npm run build`, `go test ./...` untouched-but-verified, compose smoke: `/docs` and one page render end-to-end).
- [ ] **M5 — Meta-docs & handoff**: "how to add a docs page" section (frontmatter contract, line budget, screenshot conventions) in `docs/README.md`; ARCHITECTURE.md gets a short Docs section; PRD closed out.

### Parallelization

| Phase | Milestones | Depends on | Files |
|---|---|---|---|
| 1 (parallel) | M1, M2 | — (contract fixed in this PRD) | `web/*` vs `docs/*` — disjoint |
| 2 | M3, M4 | M1 + M2 | `docs/img/*`, `web/scripts/*` |
| 3 | M5 | M3 + M4 | `docs/README.md`, `ARCHITECTURE.md` |

## Success Criteria

- A logged-out visitor can read every howto needed to go from nothing to a working board without leaving the webui.
- Every `user` page targets ≤ 60 lines (build warns when over), numbered steps, at least one screenshot.
- In-app docs can never drift from repo docs (same files, one build).
- No new API surface, DB table, or service; `web` image still builds reproducibly.

## Risks & Mitigations

- **Content drift** → single source + build-time link/frontmatter validation (M4).
- **Heavy screenshots** → images are emitted as hashed assets fetched lazily by `<img>` (not inlined into the JS bundle), so the cost is per-page download, bounded by the per-image size budget `check-docs.mjs` enforces.
- **Trimming loses information** → cut text moves to `operator`/`design`/`contributor` pages per an explicit mapping recorded in M2, never deleted outright; reviewer verifies in M2.
- **Screenshot staleness as UI evolves** → accepted for MVP; shots are named per page/step so re-capture is a checklist, not archaeology.
- **Docker context widening slows builds** → root `.dockerignore` excludes `.git`, `inspiration/`, `api/`; context stays a few MB.

## Out of Scope

- Docs search (client-side filter is a follow-up if the page count grows).
- Skills howto (no skills feature yet — plan.md; the page lands with that PRD).
- Runtime/agent-run howtos (PRD #4 documents its own surface; this PRD only builds the shelf it goes on).
- Serving docs from the API/DB, editing docs from the UI, versioned docs.

## Decision Log

- 2026-07-04 — Build-time bundling from repo `docs/` over an API endpoint or duplicated `web/src/docs/`: single source of truth, no runtime moving parts; cost is the compose build-context widening, judged cheap.
- 2026-07-04 — `/docs` public (unauthenticated): onboarding docs are needed pre-registration; content is non-secret and repo-public anyway.
- 2026-07-04 — `react-markdown` without `rehype-raw`: content is trusted (repo-reviewed), so raw HTML stays inert rather than sanitized; smallest safe pipeline.
- 2026-07-04 — Frontmatter `audience` field over a hardcoded page list in `web/`: docs stay self-describing; adding a page never touches web code.
- 2026-07-04 — Screenshots via placeholders first (Vlad): implementation never blocks on captures; real screenshots land as a single final commit after all other milestones.
