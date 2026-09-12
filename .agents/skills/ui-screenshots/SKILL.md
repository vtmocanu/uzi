---
name: ui-screenshots
description: "Captures clean, demo-masked uzi web UI screenshots in BOTH light (Dawn) and dark (Ember) themes for the blog and README, fast and deterministically. Drives a logged-in Chrome over CDP - ensures demo mode on, flips instance branding to vanilla (built-in uzi mark, no powered-by) then restores it, forces each theme, leak-scans, and captures the app viewport at 2x with no browser chrome. shots.json is the manifest of every screenshot and where it lands. Use when capturing or refreshing uzi UI screenshots for the blog or README. Triggers include uzi screenshots, blog screenshots, readme screenshots, refresh the screenshots, screenshot the uzi UI, regenerate screenshots."
---

# ui-screenshots — capture uzi web UI screenshots (light + dark)

Regenerate publication-ready uzi UI screenshots in both themes, straight from a
logged-in Chrome over CDP. The engine captures the app **viewport only** (no browser
chrome, no crop) at deviceScaleFactor 2, so light and dark both come out crisp.
`shots.json` is the single source of truth: every screenshot, how to capture it, and
whether it lands in the README, the blog, or both.

**The instance URL is always an argument** (or read from `uzi auth status`), never
hardcoded — this repo is public, so no instance host belongs in a script or commit.

## Prerequisites

- **macOS + Google Chrome** at `/Applications/Google Chrome.app`.
- **node** on PATH (run.sh self-provisions `playwright-core` into a cache dir `~/.cache/uzi-ui-screenshots`, kept out of the skill tree).
- **`uzi` CLI** logged in (`uzi auth status`) — used only to resolve the instance URL.
- **Admin on the instance** — the branding vanilla-flip uses `PUT /api/admin/settings`. Without admin, pass `--no-branding` and set branding by hand first.

## Flow

```
launch-chrome.sh <url>          # app-mode Chrome + CDP + persistent profile; log in ONCE
  -> run.sh [--only …] [--repo …] [--run …]   # stages <slug>-light.png / <slug>-dark.png, leak-scans
  -> review every staged PNG (Read it): layout right, nothing sensitive leaked
  -> publish-shots.mjs           # copies reviewed pairs to README + blog dirs
  -> swap-markup.mjs (first time only)   # README <img> -> <picture>, blog html.dark swap
```

1. **Launch + log in once:**
   ```
   <skill dir>/scripts/launch-chrome.sh "$(uzi auth status | awk '/^URL/{print $2}')"
   ```
   Log in in that window if the SSO page shows; the profile persists the session.

2. **Capture (stages both themes, leak-scans):**
   ```
   <skill dir>/scripts/run.sh --stage /tmp/uzi-ui-shots --only dashboard,schedules,mobile-overview,mobile-runs,mobile-nav
   ```
   Omit `--only` to shoot every `auto` shot plus any `repo-board`/`run-detail` whose
   id you pass. run.sh resolves the URL, ensures demo mode on, flips branding to
   vanilla and restores it, and prints a leak-scan summary.

3. **Review — mandatory.** `Read` each staged PNG. Confirm demo masking (no real
   email, repo owner, or forge host), vanilla branding (uzi factory mark, no "powered
   by"), correct theme, and clean layout. Any `⚠` from the leak scan must be resolved
   before publishing.

4. **Publish the reviewed pairs:**
   ```
   node <skill dir>/scripts/publish-shots.mjs --manifest <skill dir>/shots.json --stage /tmp/uzi-ui-shots --only <slugs>
   ```
   `--blog-dir` defaults to `~/stuff/gitrepos/wxs/wxs/hai/static/images/uzi`; README
   dir is `<repo>/.github/readme`. `--dry-run` previews.

## The manifest (`shots.json`)

Each row: `slug`, `kind`, `route`, `viewport`, `themed`, `targets`, and (for
non-`auto`) `needs` + `state`. Add a row and it flows to both consumers. `kind`:

- **auto** — deterministic route, captured with no extra input (dashboard, schedules, mobile-*).
- **repo-board** — needs `--repo <id>` (`/repos/<id>/board`).
- **run-detail** — needs `--run <id>` **and a live run in the right state**; several also need scrolling to a panel or expanding a transcript (the `arrange` field). These are operator judgment (see below).
- **terminal** — `uzi tui`, captured by hand; not a web shot, not theme-swapped.

Themes are `light: dawn` / `dark: ember`. `denylist` drives the leak scan (the
instance host is added automatically); extend per-run with `--deny a,b,c`.

## Run-detail + board shots (operator judgment)

`run-cost`, `run-judge`, `run-plan-gate`, `milestones`, `activity`, and
`lead-transcript` are panels/states of one run page, and `board` needs a repo. They
depend on live data: a plan gate needs a run parked at approval; the activity feed
and in-progress milestones need a run in flight. Pick a run that shows the panel
well, pass `--run <id>` (and `--repo <id>` for the board), and **compare each new
shot against the current published image — keep the better one** (a board with
several populated columns beats one with two). Because a theme switch reloads the
page, arrange the panel (scroll/expand) *after* the theme is applied, per theme.

## Themes on the two consumers

- **README** (`.github/readme/`): `<picture>` with a `prefers-color-scheme: dark` source and the light PNG as the `<img>` fallback. This is GitHub's theme signal.
- **Blog** (`static/images/uzi/`): the blog theme is a manual toggle (`html.dark`), not `prefers-color-scheme`, so swap with CSS keyed off `html.dark` (mirrors the existing archify diagram). `swap-markup.mjs` sets this up once; after that, regens reuse the same filenames and need no markup change.

## Known gap

Demo mode does not mask the run **working-dir path** (`github.com+<owner>+<repo>+issue-N`)
shown briefly in the recent-runs list before the summary streams in. The capture
settles 1.6s to avoid the flash and the leak scan catches it; a proper fix is to mask
that field in demo mode (uzi web).
