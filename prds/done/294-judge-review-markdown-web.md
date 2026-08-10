# PRD #294: Render judge review markdown in the web run-review panel

**GitLab Issue**: [vtmocanu/uzi#294](https://gitlab.example.com/vtmocanu/uzi/-/issues/294)
**Status**: Done
**Priority**: Medium
**Created**: 2026-08-10
**Depends on**: none
**Related**: `web/src/pages/RunView.tsx`, `web/src/components/Markdown.tsx`, `web/src/components/MarkdownCore.tsx`, `web/src/lib/safeText.ts`, `docs/judge.md`

> Every file:line citation below was derived at **`81c98455`** (post-v0.26.0). A line
> number without a SHA is not a citation, re-derive before acting.

## Problem

The web run-review panel renders the judge's `summary_md` and each recommendation's
`rationale_md` as **escaped plaintext** wrapped in `whitespace-pre-wrap`, so all markdown
in them shows as literal source: `**bold**`, `- bullets`, `#` headings, `[label](url)`
links and multi-paragraph structure never format. Only fenced/inline code is visually
distinct, and only because monospace happens to read differently, the backticks are still
literal.

### Render sites (traced at `81c98455`)

- Summary: `web/src/pages/RunView.tsx:1781`
  ```tsx
  <p className="whitespace-pre-wrap text-sm text-muted">{stripUnsafeChars(review.summary_md)}</p>
  ```
- Per-recommendation rationale: `web/src/pages/RunView.tsx:1815`
  ```tsx
  <p className="mt-1.5 whitespace-pre-wrap text-sm text-muted">{stripUnsafeChars(rec.rationale_md)}</p>
  ```

The `_md` suffix on both fields (and on the judge's own prompt, which asks for markdown)
promises formatting the panel then throws away. The screenshot that prompted this PRD shows
a judge summary full of literal `**` and backticks.

### Why it was plaintext, and why that reason is now satisfiable

A comment block at `RunView.tsx:1767-1779` forbids a markdown renderer here on purpose:
these fields are **untrusted judge/worker output**, and the review-POST ingest scrub
(`ScrubSecrets` + control-strip) does **not** cover markdown/link injection. It also notes
issue #124: escaping alone is insufficient because Cf bidi overrides reorder what a human
reads, which is why `stripUnsafeChars` runs at the render site (`web/src/lib/safeText.ts`,
see its comment for why the strip lives at render and not at the API boundary).

That guard is already met elsewhere in the same file. `plan_md`, equally untrusted model
output, renders through the repo's hardened `<Markdown>` (`RunView.tsx:1151` and `:1259`
render `run.plan_md`; `:1220` renders a superseded plan version; and `:951` renders a user's
steering `feedback` — all untrusted text through the same component). `<Markdown>`
(`web/src/components/Markdown.tsx` → `MarkdownCore`) is built for exactly this threat:

- no `rehype-raw`, so raw HTML in the source stays inert text;
- react-markdown's `urlTransform` strips `javascript:` / `data:` / `file:` URLs to `""`
  before the overrides run, and `schemeIsDangerous` is applied again as independent
  defense-in-depth;
- links are forced external (`target="_blank" rel="noopener noreferrer"`), never rewritten
  to in-app routes, so a model cannot forge SPA navigation;
- images are size-capped so a remote/oversized `<img>` cannot blow up the box;
- `remark-gfm` gives tables/strikethrough/autolinks (`MarkdownCore.tsx`).

So the fix is to render `summary_md` and `rationale_md` through `<Markdown>` the way
`plan_md` already is, with `stripUnsafeChars` applied to the source **first** (the Cf/bidi
strip is orthogonal to markdown parsing and markdown syntax chars are not control chars, so
stripping first preserves `*`, `` ` ``, `#`, `-`, `[]()`). Client-side only, no server or
DB change.

## Non-collision with PRD #292

PRD #292 (Slack mrkdwn rendering, in flight at time of writing) is a **server-side Go**
change to the Slack bot's mrkdwn dialect (`api/internal/slacksvc/*`,
`api/internal/handler/judge_worker.go`, `api/internal/notifysvc/service.go`,
`web/src/lib/notifications.ts`). This PRD is a **client-side React** change to the web
panel (`web/src/pages/RunView.tsx`, reusing `web/src/components/Markdown.tsx`). No file
overlaps. The two consume the same `summary_md`/`rationale_md` fields but through
independent renderers, so they can land in either order and rebase clean. This PRD adds
**no** server change, so it never touches `judge_worker.go`, the one file #292 also edits.

## User journey

1. A run finishes and is judged.
2. The owner opens the run and reads the review panel.
3. **Today**: the summary and each rationale are a wall of `**`, backticks and literal `-`
   bullets; multi-paragraph reviews collapse into pre-wrapped source.
4. **After**: bold, italic, lists, headings, code and links render; links open safely in a
   new tab; nested/oversized/hostile content stays inert exactly as it does for `plan_md`.

## Scope

**In scope**
- `review.summary_md` and `rec.rationale_md` render through `<Markdown>` in
  `web/src/pages/RunView.tsx`.
- `stripUnsafeChars` continues to run on the source before rendering.
- The `RunView.tsx:1767-1779` comment is rewritten to state the new, still-hardened posture
  (and to keep the Cf-strip rationale), replacing the "never markdown/HTML" prohibition.
- Tests proving markdown renders and that the injection guards hold.

**Out of scope**
- Any server-side change (`judge_worker.go` scrubbers stay as-is; #292 owns that file).
- `rec.target` — it stays an inert `<code>` coordinate the page posts back
  (`RunView.tsx:1805`); it is not prose and must not become a link/markdown sink.
- The Slack bot rendering (PRD #292).
- Any change to what the judge is asked to produce.

## Technical approach

Replace the two `<p className="whitespace-pre-wrap …">{stripUnsafeChars(x)}</p>` sites with
`<Markdown content={stripUnsafeChars(x)} />`, wrapped in whatever container preserves the
current vertical rhythm (`<Markdown>` emits `.docs-prose`, so verify spacing against the
existing panel and adjust the wrapper, not `MarkdownCore`). Keep the `x.trim() !== ""`
guards so an empty summary/rationale still renders nothing.

Security note for the implementer and reviewer: the safety argument is "identical hardening
to `plan_md`, which is already untrusted model output rendered here." Do not add
`rehype-raw`, do not relax `schemeIsDangerous`, and do not drop `stripUnsafeChars`. The
auditor should confirm the `<Markdown>` path is byte-for-byte the same hardened component,
not a second markdown pipeline.

## Milestones

- [x] **M1 — Summary + rationale render as markdown.** Both sites in `RunView.tsx` route
  through `<Markdown>` with `stripUnsafeChars` applied first; empty-string guards retained;
  panel vertical rhythm preserved. Update the `1767-1779` comment to the new posture,
  **keeping** the note that `rec.target` (`:1804-1808`) stays inert plaintext (the comment
  covers three fields; only two change).
- [x] **M2 — Tests.** Two parts, because one existing test pins the OLD behaviour and will
  go red under M1: `RunView.test.tsx:538-559` ("renders review free text as escaped text,
  never HTML") asserts the literal `**not bold**` survives — once markdown renders it
  becomes `<strong>`, so that assertion must be **rewritten** to the new posture (its
  `querySelector("img")` inert-HTML check stays valid and should be kept). Then **add**
  cases proving (a) markdown in `summary_md`/`rationale_md` renders as elements
  (bold/list/link/code), (b) a `javascript:`/`data:` link is neutralized, (c) raw HTML
  stays inert, (d) an empty field renders nothing, (e) the #124 bidi-strip test
  (`RunView.test.tsx:566`) still passes. Reuse the assertion style the existing `plan_md`
  markdown tests use. **M1 alone leaves `task gate:web` red; M1+M2 land together.**
- [x] **M3 — Docs.** `docs/judge.md` (and any run-review UX note) reflects that the panel
  now renders the judge's markdown; note the shared hardened renderer so the security
  posture is discoverable.
- [x] **M4 — Gate green.** `task gate:web` passes (oxlint, knip, check-docs, typecheck,
  vitest). No new knip unused-export warnings from the change.

## Success criteria

1. In the run-review panel, a judge summary containing `**bold**`, a `- list`, a fenced
   code block and a `[label](https://…)` link renders as formatted HTML, not literal
   source. (Verifiable in mock mode with a fixture, or against a real judged run.)
2. A `summary_md`/`rationale_md` containing `[x](javascript:alert(1))`, a `data:` URL, or
   raw `<script>`/`<img onerror=…>` produces no active link, no script, and no navigation,
   identical to how `plan_md` handles the same input.
3. Cf/bidi override characters in the source are still stripped before render (issue #124
   behaviour preserved).
4. `rec.target` remains an inert code coordinate, unchanged.
5. `task gate:web` is green; no server, DB, or `judge_worker.go` change is introduced.

## Risks and mitigations

- **Risk: reintroducing an injection sink.** Mitigation: reuse the existing `<Markdown>`
  verbatim (no new pipeline, no `rehype-raw`); M2 tests assert the neutralization; auditor
  reviews the diff. The precedent (`plan_md` already renders this way) bounds the risk to
  "no worse than an already-shipped surface."
- **Risk: `stripUnsafeChars` ordering.** Mitigation: strip the source string, then pass to
  `<Markdown>`; a test asserts a bidi-override char does not survive.
- **Risk: layout regression** from `.docs-prose` inside the compact panel. This is more
  than spacing drift: `.docs-prose` (`web/src/index.css:206-276`) sets a base font size (no
  `text-sm`) and renders `h1` at `text-3xl`, `h2` at `text-xl` with a bottom border, so a
  judge summary containing a `#`/`##` heading would render large and bordered inside the
  compact card. The offending rules are descendant selectors (`.docs-prose h1/h2/p`), which
  a bare wrapper class cannot override, so the fix may need a scoped, more-specific CSS rule
  (a panel-local class that constrains heading sizes) rather than only a wrapper. Mitigation:
  visual check headings/lists/paragraphs specifically (mock mode or a real run); do not edit
  `MarkdownCore`.
- **Risk: no rich-markdown fixture to validate against.** The mock judge fixtures
  (`web/src/mocks/data.ts`) carry only inline code today — no bold, lists, headings, or
  links — so the mock-mode visual check in Validation requires **enriching one fixture**
  with `**bold**`, a `- list`, a fenced block and a `[label](https://…)` link first.
  (Low risk; noted so the visual step is not skipped for lack of input.)

## Validation strategy

- Unit: the M2 vitest cases in `RunView.test.tsx`.
- Visual: `VITE_UZI_MOCK=1` with a judge-review fixture carrying rich markdown — enrich a
  `web/src/mocks/data.ts` review fixture with `**bold**`, a `- list`, a fenced block and a
  `[label](https://…)` link first, since current fixtures only carry inline code (mock mode
  validates **rendering**, which is exactly this PRD's concern; it is not a data-population
  test, so the mock caveat in `.claude/rules/web.md` does not limit it here), or a real
  judged run in the live stack.
- Gate: `task gate:web`.
