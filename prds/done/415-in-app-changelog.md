# PRD #415: In-app changelog / release notes panel

**Issue**: #415
**Priority**: Medium
**Status**: Complete — all milestones landed on branch `agent/issue-415` (M1 `63eca22f`, M2 `02268f28`, M4 `19d43b89`, M3 `8ee7829d`, M5 docs/specs/architecture). `task gate:web` green (incl. the new `check-changelog` version-match gate and the parity test over every emitted version); a `VITE_UZI_MOCK=1` browser pass confirmed the drawer opens from the popover, the running-version guard behaves, and all close paths restore focus.

> **MAINTAINER FOLLOW-UP:** add a `task check-changelog:web` step to the `validate-web` job in `.github/workflows/ci.yml` so the version-match gate runs in CI. The worker's git token lacks `workflow` scope, so this run could not edit workflow files; the change is safe and one line. The **parity** gate needs no follow-up — it already runs in CI via the existing `test-web` job's `task test:web`.
>
> **PROPOSED `specs/human.md` line (needs owner approval — not added):** a one-line requirement that the web UI shows the changelog in-app from the version popover and marks the running release. `specs/human.md` edits require explicit user approval.

## Problem

uzi maintains a curated `CHANGELOG.md` (Keep-a-Changelog) and, since PR #413, publishes automated GitHub Releases whose body is each version's changelog section. But a user running the app has no way to see what changed: the sidebar version badge shows the running version and a build-info popover, yet reading the release notes means leaving the app for GitHub. Operators upgrading an instance, and users noticing the version moved, both have to go off-app to learn what is new.

## Solution

Add a **Changelog** button to the version build-info popover. Clicking it opens a **left-side, scrollable overlay** of release notes, newest-first, rendered from `CHANGELOG.md` bundled into the web app **at build time** (reusing the exact `?raw` mechanism the docs viewer already uses: no runtime service, no new API route, no committed second copy). The running version is marked **"You're running this"** and any newer versions are flagged **"Newer"**, both derived from the same `GET /api/version` build-info the badge already reads. The design was prototyped and approved as a browser mock; this PRD implements that mock faithfully.

Because the changelog is authored in exactly one place and the panel is *derived* from it, the two cannot drift by construction. Two CI gates make the remaining coupling deterministic: a **version-match** gate (the newest released section equals `Chart.yaml`'s version) and a **parity** gate (the web parser's per-version raw body is byte-identical to what the release tooling's `scripts/changelog-section.sh` extracts).

This PRD is written to be implemented by an offline worker: every load-bearing fact is stated below as a resolved code fact, and each was verified against the tree during authoring.

## Background: current behaviour (code facts)

**Docs viewer = the template to mirror.** `web/src/lib/docs.ts` bundles repo docs at build time with `import.meta.glob("../../../docs/*.md", { query: "?raw", eager: true })` ("one source of truth, no runtime service"). It has a tiny hand-rolled frontmatter parser (no `gray-matter`), a `summarize()` helper, a graceful fallback (a malformed file does not throw), and a pinned `REPO_BLOB_BASE = "https://github.com/vtmocanu/uzi/blob/main/"` **hardcoded constant** used for links to repo-only files. `web/scripts/check-docs.mjs` is its build-time gate; it **re-implements** the same `parseFrontmatter` (it does not import it from `docs.ts`, because `docs.ts`'s top-level `import.meta.glob` cannot execute under plain node). It runs in `npm run build`, `task check-docs:web`, and the GitHub Actions `validate-web` job.

**Relative path.** `web/src/lib/docs.ts` is three levels under `web/`, so `../../../docs/` resolves to repo-root `docs/`. Repo-root `CHANGELOG.md` is `../../../CHANGELOG.md` from `web/src/lib/` (verified). A script at `web/scripts/` reaches the repo root at `../..` (a *different* depth: `check-docs.mjs` uses `path.resolve(scriptDir, "..", "..")`).

**Dev file access already allowed.** `web/vite.config.ts` sets `server.fs: { allow: [".."] }` so the docs raw-glob can read repo-root files in dev. Repo-root `CHANGELOG.md` is covered; no vite-config change is needed for dev.

**Docker context — the packaging facts.** `web/Dockerfile` builds from the repo-root context in `node:24-alpine` and does `COPY web/ ./` + `COPY docs/ /app/docs/` (web at `/app/web`, docs at `/app/docs`). It does **not** copy `CHANGELOG.md`, and it does **not** copy `deploy/chart/Chart.yaml`, `scripts/`, or `.git`. The image has **no bash** (alpine ships busybox `/bin/sh` only). The root `.dockerignore` does **not** blanket-exclude `*.md`. Consequences: (a) bundling the changelog needs `COPY CHANGELOG.md /app/CHANGELOG.md` so `../../../CHANGELOG.md` resolves at `/app/web/src/lib/`; (b) anything reading `Chart.yaml`/`scripts/` or requiring bash **cannot** run during the image's `npm run build`. `check-docs.mjs` already handles this by gating its outside-`docs/` work behind `fullCheckout = existsSync(repoRoot/.git)` and self-skipping when false.

**Build-info + running version.** `GET /api/version` returns `apitypes.BuildInfoDTO` (`api/internal/handler/handler.go`; type `BuildInfo` in `web/src/lib/api.ts`, `api.version()`), always carrying `version`. `web/src/components/AppShell.tsx` fetches it once (`useBuildInfoSnapshot()` / `useBuildInfo()`, a module-scope memoised promise) and renders `BuildInfoPopover` inside `SidebarContent` at **two** mounts (desktop rail and mobile drawer). `BuildInfoPopover.tsx` (`role="tooltip"`, opened on hover/focus, closed by `onBlur`/`onMouseLeave` on the trigger) shows `uzi vX.Y.Z`, an age/commits subtitle, and a `Founded/Built/Commit/PRDs/Uptime` definition list. **`version` is `"dev"` on every local compose stack and can be `"demo"`; `displayVersion()` keeps a non-numeric version as-is** — so the running version is not always semver.

**Reusable overlay already exists.** `web/src/components/Modal.tsx` is the shared overlay: focus-in on open, focus-trap with wrap, Esc close, focus-restore on close, `aria-modal="true"` backdrop, and a `className` override (default centres a card). It exists "so every overlay shares ONE correct implementation instead of re-deriving it."

**Release/version coupling (Model B).** `deploy/chart/Chart.yaml` sets `version` == `appVersion` == the release git tag (stated in its own comment). So the newest **released** changelog section equals `Chart.yaml`'s version on every `main` commit. (Read the file for the current number; it moves every release and must not be pinned in this PRD.)

**`[Unreleased]` and `[NOT RELEASED]` both exist.** The file's working head is `## [Unreleased]`, and it also contains staged-but-never-released sections tagged `## [X.Y.Z] - <date> [NOT RELEASED]`. Both must be treated as "not the running/released version" by the gate and the markers, and the heading parser must tolerate trailing text after the date.

**Release tooling to stay parity with.** `scripts/changelog-section.sh body <ver>` extracts the content between `^## \[<ver>\]` and the next `^## \[` (or EOF), drops the `<!-- release-title: … -->` marker line, and trims leading/trailing blank lines. **For the OLDEST version there is no next heading, so the EOF compare-link footer block (`[X.Y.Z]:` / `[Unreleased]:` reference-definition lines) is swept into that section's body** — a latent quirk that only ever bit the oldest section, which is never re-released. `scripts/changelog-links.sh` maintains those footers and linkifies bare `#N` PR/issue refs via a safe-by-default allow-list: only `(#N)` parentheticals and `issue/PR #N` labels are linked; `PRD #N` and cross-repo refs stay plain (the file already carries inline `[#N](…)` markdown links). Its repo URL is origin-derived with a `UZI_CHANGELOG_REPO_URL` override — but **there is no git remote at web build time** (rollup; `.git` excluded from the image), so the web side cannot derive it and must use a hardcoded default like `REPO_BLOB_BASE`. Pre-baked inline PR links are frozen absolute URLs from each release's `changelog-links.sh` run.

**Markdown rendering already exists.** `web/src/components/{Markdown,MarkdownCore,DocMarkdown}.tsx` render markdown; `MarkdownCore` uses react-markdown with no `rehype-raw`, so an HTML comment such as the `release-title` marker stays inert. The changelog is trusted repo content (DocMarkdown-style rendering is appropriate; no bidi strip needed).

**PRD refs.** A PRD's number IS its originating issue/PR number (repo convention). So `PRD #N` links robustly to `<repo>/issues/N` with no filesystem lookup — chosen deliberately over a `prds/<N>-slug.md` deep-link, because `prds/` is **not** copied into the web image, so a build-time path glob would resolve empty and every ref would silently fall back. GitHub redirects `/issues/N` to `/pull/N` (and vice versa), so the link resolves whether the originator was an issue or a PR.

**No DB change.** The running version already flows to the client via `GET /api/version`; nothing here needs a schema change or a goose migration.

**Web has no semver comparator** (`web/src/lib` has none; worker-upgrade classification is server-side Go). M3 must add a small `X.Y.Z` comparator; a full semver library is unnecessary and undesirable for the bundle. The shared `\d+\.\d+\.\d+` shape does not match `-rc.N` prereleases; uzi cuts none today (plain-semver Model B), so that is an accepted, documented limitation.

## Milestones & execution plan

| Phase | Milestone | Depends on | Files touched (primary) |
|---|---|---|---|
| 1 | M1 parser + bundling | — | `web/src/lib/changelog.ts`, `web/Dockerfile` |
| 2 | M2 drawer + popover button | M1 | `web/src/components/{ChangelogDrawer,BuildInfoPopover,AppShell}.tsx` |
| 2 | M4 sync gates | M1 | `web/scripts/check-changelog.mjs`, `web/src/lib/changelog.parity.test.ts`, `Taskfile.yml`, `.github/workflows/ci.yml`, `web/package.json` |
| 3 | M3 release-notes rendering | M1, M2 | `web/src/components/ChangelogDrawer.tsx` (+ a `web/src/lib` semver helper) |
| 4 | M5 docs / specs / integration | M1-M4 | `docs/*.md`, `specs/ai.md`, `ARCHITECTURE.md` |

M2 and M4 are file-disjoint and run in parallel. M3 depends on M2 (it enriches the same drawer component), so it is **not** parallel with M2.

### M1 - Changelog parser + build-time bundling [no deps]

- New `web/src/lib/changelog.ts`, mirroring `docs.ts`: a **framework-agnostic** parse function plus the browser-only `?raw` import of `../../../CHANGELOG.md`. (See M4 for why the parser is authored so it is reusable by a test without dragging in `import.meta`.)
- `Release { version, date, titleMarker?, body, groups }`:
  - **`body`** is the raw section slice produced by the **same algorithm** as `scripts/changelog-section.sh body`: content between `^## \[<ver>\]` and the next `^## \[` (or EOF), drop the `^<!--\s*release-title:.*-->\s*$` marker line, trim leading/trailing blank lines. Do **not** strip the compare-link footer — the shell keeps it for the oldest section, so keeping it here makes parity exact by construction. `body` is the **parity surface**.
  - **`groups`** is the **render surface**: split each `### <Category>` subsection into its `- ` bullets (bullets keep inline `[#N](…)` markdown so PR links render for free), ignoring `[x]:` footer reference-definition lines (which render as nothing anyway). `[Unreleased]` / `[NOT RELEASED]` sections are marked so M3 excludes them from markers; an empty `[Unreleased]` is omitted from the emitted list entirely.
- Add `COPY CHANGELOG.md /app/CHANGELOG.md` to `web/Dockerfile` so the raw import resolves in the image; confirm `npm run build` bundles it and the image builds.
- Unit tests: section boundaries; marker extraction; `body` matches the shell slice for a representative middle version **and** the oldest (footer retained); category/bullet split; inline links preserved; **empty `[Unreleased]` dropped while a populated one is emitted** (pair the absence assertion with a positive one, per `.claude/rules/web.md`); graceful fallback on a malformed file (no throw).

### M2 - Left-overlay drawer + popover Changelog button [deps: M1]

- New `ChangelogDrawer` **built on `web/src/components/Modal.tsx`** (focus-trap, Esc, focus-restore, `aria-modal="true"` — `aria-modal` matters because a left overlay covers the primary nav), with a `className` override for the left slide-in + backdrop and independent body scroll; honors `prefers-reduced-motion`. Mounts **once** at `AppShell` level (there are two `SidebarContent` mounts, so the drawer cannot live inside the popover); a small app-level open state is raised by the button.
- Add a **Changelog** button to `BuildInfoPopover.tsx`. **Fix the popover's close semantics first**: today `onBlur`/`onMouseLeave` close the `role="tooltip"` panel, which makes any control inside it keyboard-unreachable (blur fires before the button gets focus) and is an ARIA anti-pattern. Change to focus-within (keep open while `relatedTarget`/pointer is inside the host) so the button is reachable, or move the trigger into the normal focus flow. Verify keyboard reachability, not just mouse.
- Make the popover's **PRDs** row a link to the `prds/` directory (approved in the mock).
- To keep M2's scroll/focus tests non-vacuous before M3's rich rendering exists, the drawer renders a **minimal list** from `parseChangelog` (version headings at least); M3 upgrades this in place.
- Component tests: opens from the popover button **via keyboard and mouse**; closes on Esc, backdrop, and ✕; focus returns to the trigger; body scrolls; running version passed through.

### M3 - Release-notes rendering: markers, links, category groups [deps: M1, M2]

- Render releases newest-first: `vX.Y.Z` heading linking to `<repo>/releases/tag/vX.Y.Z`, date, optional one-line title marker, then category groups each with a status-tone dot (Added=ok, Changed=info, Fixed=warn, Security=danger, Dependencies=faint), bullets as inline markdown.
- **current/newer markers**, derived from the BuildInfo `version` via a small `X.Y.Z` comparator added to `web/src/lib`: the matching section gets "You're running this"; strictly-greater sections get "Newer"; a banner reads "This instance runs vX · vY available". `[Unreleased]`/`[NOT RELEASED]` are **never** eligible to be "current" or the "vY available" target. **When `version` is non-semver or absent (`dev`/`demo`), render no markers and no banner** (neutral state); guard the parse so it never throws or marks everything "Newer".
- **PRD #N linkify** is a web-only post-parse transform to `<repoBase>/issues/N` (repoBase = a hardcoded `vtmocanu/uzi` default, overridable via a `VITE_UZI_CHANGELOG_REPO_URL` build-time env), rendered as a visually distinct, quieter link than PR refs. This does not modify `CHANGELOG.md` (its PRD-plain policy exists for GitHub's Release-body autolinker) and does not touch M4's parity, which compares the raw `body`, not rendered HTML.
- Tests: marker derivation (current / newer / none-for-`dev` / none-for-`[NOT RELEASED]`), semver ordering, PRD `#N -> /issues/N`, category dot mapping.

### M4 - Deterministic-sync gates [deps: M1]

Two gates, split by what can actually run where:

- **Version-match** — `web/scripts/check-changelog.mjs`, pure text (no parser): find the newest heading that is neither `[Unreleased]` nor `[NOT RELEASED]`, assert its `X.Y.Z` equals `deploy/chart/Chart.yaml`'s `version`. Gate its `Chart.yaml` read behind the same `fullCheckout = existsSync(repoRoot/.git)` self-skip `check-docs.mjs` uses, so it is a no-op in the image build and only enforces in a full checkout. Wire into `task check-changelog:web` and the GitHub Actions `validate-web` job — **not** unconditionally into `npm run build`.
- **Parity** — a **vitest test** `web/src/lib/changelog.parity.test.ts` (vitest imports TS natively and can `spawnSync` + `fs`-read): for **every version `parseChangelog` emits** (so a populated `[Unreleased]` is handled and empties are skipped), spawn `bash scripts/changelog-section.sh body <ver>` with `cwd = repoRoot` (or `UZI_CHANGELOG_FILE=<repoRoot>/CHANGELOG.md`) and assert it equals `Release.body`. It runs inside `task test:web` / `gate:web` and CI, where bash + a full checkout exist; skip cleanly if bash/script are absent.
- `check-changelog:web` is added to `gate:web`'s recipe so SC6 holds locally.
- Calibration: a deliberately mismatched fixture reddens each gate; the committed tree passes both. State that "the same parser is shared" is achieved by the **test importing the web parser** (not by a node `.mjs` importing a `.ts`), and that version-match uses no parser at all.

### M5 - Docs, specs, CLI check, architecture note [deps: M1-M4]

- Short user-facing note that release notes are available in-app from the version badge (extend an existing UX/help `docs/*.md` page or add one with valid frontmatter; `check-docs` gates it).
- Record the design decisions in `specs/ai.md`. **Flag to the owner** that this owner-approved user-facing feature may also warrant a one-line `specs/human.md` requirement; `human.md` edits need explicit user approval, so propose it rather than adding it.
- **CLI check** (repo convention): state explicitly that no `api/cmd/uzi/` change is required — this renders a repo file already visible via git/GitHub; a `uzi changelog` verb is noted as possible future scope, not built here.
- One-line addition to `ARCHITECTURE.md`'s surfaces map: the in-app changelog is a build-time-bundled read of `CHANGELOG.md` (no new service, no new trust boundary).
- Full `task gate:web` green; a browser pass under `VITE_UZI_MOCK=1` confirming the drawer opens and the running-version marker renders (the marker reads the mocked `GET /api/version`).

## Success criteria

1. Clicking **Changelog** in the version popover (by keyboard or mouse) opens a left-side, scrollable overlay of release notes; Esc / backdrop / ✕ close it; focus returns to the trigger.
2. The running version is marked "You're running this" and strictly-newer released versions are flagged, derived solely from `GET /api/version`; a non-semver running version (`dev`/`demo`) renders a neutral panel with no markers. No new wire field, no migration.
3. **Parity gate green**: for every version `parseChangelog` emits, `Release.body` is byte-identical to `scripts/changelog-section.sh body <ver>` (including the oldest section's retained footer).
4. **Version-match gate green**: the newest non-`Unreleased`/non-`[NOT RELEASED]` section equals `Chart.yaml`'s `version`.
5. The panel is a pure function of `CHANGELOG.md` — no committed second copy exists (guaranteed by the single build-time `?raw` import, and evidenced by the parity gate). Editing `CHANGELOG.md` and rebuilding updates the panel.
6. `task gate:web` is green (lint, deadcode, check-docs, **check-changelog**, check-styles, typecheck, test-incl-parity).

## Decision Log

1. **Delivery = build-time `?raw` bundle, not a runtime endpoint or a Vite plugin.** Reuse `docs.ts`'s proven `import.meta.glob(..., {query:"?raw"})` pattern. Zero duplication, deterministic (the bundle is a pure function of `CHANGELOG.md`), no new route or trust boundary, and `server.fs.allow:[".."]` already permits reading the repo root in dev.
2. **Panel opens on the left** (owner decision). Trade-off recorded: a right overlay keeps the sidebar/nav and the clicked badge visible (the conventional side for a detail slide-over); left was chosen for proximity to the bottom-left trigger and owner preference. `aria-modal` (via `Modal`) keeps a screen reader out of the nav the left overlay covers.
3. **`PRD #N` links to the originating issue `<repo>/issues/N`** (owner decision), not a `prds/` file deep-link. `prds/` is not in the web image, so a build-time path glob would resolve empty and silently fall back; the issue link always resolves and needs no image change. `CHANGELOG.md` keeps `PRD #N` plain (a bare `#N` in a GitHub Release body would autolink to the wrong issue); the web renderer adds the link only in rendered output, so parity (on raw `body`) is unaffected.
4. **Sync is guaranteed by two gates, split by runtime.** Version-match is a pure-text `.mjs` (no parser) that self-skips outside a full checkout; parity is a **vitest test** that imports the web parser and spawns `changelog-section.sh`. This is deliberately not "one pure parser imported by a node `.mjs`" — plain node cannot import the `.ts` parser (no `tsx`) and importing `changelog.ts` would drag in the browser-only `?raw`. The test importing the real parser is what genuinely pins the web output to the release tooling.
5. **`Release` carries a raw `body` for parity and structured `groups` for rendering.** Reconstructing byte-identical text from `groups` is lossy (inter-group blanks, multi-line bullet continuations); a retained raw slice produced by the shell's own algorithm makes parity exact.
6. **current/newer from existing BuildInfo, guarded for non-semver.** The running version reaches the client via `GET /api/version`; markers are derived client-side (no endpoint/field/migration) and degrade to a neutral panel when the version is `dev`/`demo`.
7. **Reuse `Modal.tsx`; fix the popover close semantics.** The drawer's focus/Esc/restore/`aria-modal` come from the shared component rather than being re-derived; the `BuildInfoPopover` tooltip is changed to focus-within so an interactive control inside it is keyboard-reachable.

## Risks and mitigations

- **Parity red on the oldest section** (footer swept in by the shell). Mitigated by Decision 5: `body` uses the identical slice, footer retained, so it matches by construction. A parity calibration must include the oldest version.
- **Gate wired where it cannot run** (image `npm run build` lacks bash / `Chart.yaml` / `scripts/`). Mitigated by M4's split: version-match self-skips outside a full checkout; parity is a vitest test that never runs in the image build.
- **Docker context misses `CHANGELOG.md`** → empty/absent import. Mitigated by M1's `COPY` + image-build verification; the parser fails safe (empty → "no release notes yet", never a throw), mirroring `docs.ts`.
- **Non-semver / absent running version** (`dev`/`demo`, the common local case). Mitigated by M3's neutral state + guarded compare.
- **`[NOT RELEASED]` staged sections** already exist. Mitigated by excluding them (like `[Unreleased]`) from the gate and markers, and tolerating trailing heading text.
- **Interactive control in a `role="tooltip"`** → keyboard-unreachable. Mitigated by Decision 7 (focus-within + `Modal`).
- **Fork repo URL** — no git remote at web build. Mitigated by a hardcoded `vtmocanu/uzi` default + optional `VITE_UZI_CHANGELOG_REPO_URL`; pre-baked PR links are frozen at each release's `changelog-links.sh` run (an accepted limitation).
- **Changelog bundle size** (~200 KB, `eager` in the initial chunk). Acceptable (docs already do this); if initial-load budget matters, the raw import + parse can move behind drawer-open. Noted, not pre-optimised.
- **Two popover mounts, one drawer.** Single app-level drawer instance; the button only raises shared open state; `useId` already prevents duplicate ids.

## Out of scope (explicitly)

- A `uzi changelog` CLI verb (possible future; no CLI change needed now).
- Any change to `CHANGELOG.md`'s content policy or the PR #413 GitHub-Release tooling.
- Fixing the latent `changelog-section.sh` oldest-section footer quirk (parity accommodates it rather than changing the shipped tooling).
- Prerelease/`-rc.N` version support (Model B is plain semver; documented limitation).
- "What's new since you last visited" tracking / an unread badge on the version.
- Deep-linking the drawer via a route or URL query param.
- A mobile-specific redesign beyond the existing responsive sidebar behaviour.
