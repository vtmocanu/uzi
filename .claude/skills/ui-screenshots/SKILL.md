---
name: ui-screenshots
description: "Captures clean, demo-masked screenshots of the uzi web UI for the blog, docs, and README, fast. Fronts a logged-in browser window through Orca computer-use, retries until a Retina (2x) capture lands, and auto-crops the browser chrome (sidebar, toolbar, rounded window border) with no hardcoded coordinates. Takes the uzi instance URL as an argument and never hardcodes it, so it is safe in this public repo. Use when capturing or refreshing uzi UI screenshots for the blog, docs, or README. Triggers include uzi screenshots, blog screenshots, readme screenshots, refresh the screenshots, screenshot the uzi UI, capture the UI."
---

# ui-screenshots — capture clean uzi web UI screenshots

Turn the live uzi web UI into publication-ready screenshots: capture a browser
window at Retina resolution, then crop away all browser chrome so only the uzi UI
remains. Two bundled scripts do the deterministic work; you drive the browser
between shots.

**The instance URL is always an argument, never hardcoded.** This repo is public,
so no concrete instance host (for example a company dev URL) belongs in this skill,
a script, or a commit. Ask for the URL if it was not given, or read `UZI_URL` /
`uzi auth status`.

## Prerequisites

- **macOS with Orca computer-use** available (`orca` on PATH, or `$ORCA_CLI_COMMAND`).
  Load the `computer-use` skill for selector discovery and window commands.
- **A desktop browser logged in to the uzi instance.** The scripts screenshot an
  existing window; they do not log in.
- **Demo mode ON in that browser** (see below). Non-negotiable for anything public.
- **ImageMagick** (`magick`) for cropping.

## Step 1 — turn on demo mode (masks identifying data)

Demo mode is a per-device toggle stored in the browser's `localStorage`, off by
default, server-side data untouched. Turn it on in the target browser: **Settings
-> Demo mode**, or the quick toggle in the sidebar user area. It masks email and
display name to a first name, repo owner/namespace to `demo/<repo>`, the forge host
to a placeholder, and forge usernames to `demo-user` / `demo-bot`.

It is per-browser and per-device, so it does not follow a login to another browser.
**Always confirm the masking is visible in the captured image before saving it** (no
real email, repo owner, or host). If a shot leaks real data, demo mode was off in
that browser.

## Step 2 — find the browser window

```
orca computer list-apps --json
orca computer list-windows --app <bundle-id> --json
```

Common bundle ids: `app.zen-browser.zen`, `com.google.Chrome`, `com.apple.Safari`,
`com.microsoft.edgemac`. Navigate that window to the uzi view you want (see
*Navigating* below).

## Step 3 — capture at Retina

One call can navigate, resize, capture, and crop:

```
<this skill's directory>/scripts/capture.sh --app <bundle-id> <out.png> \
  --url <url> --size 1728x1080 --crop --width 1800
```

- `--url <url>` opens the uzi URL in a **new tab** (leaves your other tabs alone), so
  the script does not depend on what the window happened to be showing. Pass the
  instance URL here; it is never hardcoded.
- `--size WxH` resizes the window (via osascript) to fixed points for reproducible
  framing. **It also tends to restore Retina**: a stale window often captures soft
  (scale < 1), and the resize rebuilds the window surface so the next capture comes
  back at scale 2. If a shot is soft, re-run with `--size`.
- `--crop` chains `crop-ui.sh` (below) so one call gives a finished image; `--width
  1800` downscales for the blog.

`capture.sh` fronts the window and retries until Orca returns a real window capture
(scale >= 1.5) rather than the soft low-resolution desktop-region fallback.

**When a capture stays soft (scale < 1):** re-run with `--size` first. Failing that,
the window is in **native fullscreen** (use a normal window), on a **non-Retina
display** (move it to the built-in), or **occluded / screensaver active** (keep it
visible, dismiss the screensaver). On exit 1 the script saved the best (soft) shot and
printed which condition to fix. During a batch, keep the window stable and unoccluded.

## Step 4 — crop the browser chrome

```
<this skill's directory>/scripts/crop-ui.sh <in.png> <out.png> [--width 1800]
```

`crop-ui.sh` finds the uzi UI (one large dark rectangle) inside a full-window
screenshot and crops out the browser sidebar, toolbar, and rounded window border,
with no hardcoded coordinates, at any window size or resolution. `--width N`
downscales the result (blog default 1800, which is 2x for a 900px column).

It assumes a **dark-themed UI**. On a light theme, raise `--darkmax` or crop by hand.
If it reports "no dark UI region", the window was not showing the uzi UI, or the
center of the shot was not dark; re-check the window.

## Navigating between views

Firefox-based browsers (Zen) expose only browser chrome to the accessibility tree,
not the web app, so you cannot click uzi's in-page nav by element index there. Drive
it one of two ways:

- **By coordinate.** Screenshot, read the sidebar item's pixel position, convert to a
  window-local click: `click_x = screenshot_pixel_x / scale` (the screenshot reports
  `scale`, typically 2 on Retina). Click the uzi sidebar entries (Overview, Boards,
  Runs, Schedules, and so on).
- **By URL.** Focus the address bar and set `<url>/<route>` (Chrome and Safari also
  expose page elements, so element-index clicks work there).

## Views worth capturing

Capture into the caller's image directory (the blog uses `static/images/uzi/`; docs
and README have their own). Suggested slugs, kept stable across refreshes:

| Slug | View |
|------|------|
| `dashboard` | Overview / factory-at-a-glance |
| `board` | a repo board (kanban of issues) |
| `schedules` | Schedules, default-jobs catalogue |
| `run-cost` | a finished run's cost and token panel |
| `run-judge` | a finished run's judge review |
| `activity` | a run's per-agent activity feed |
| `lead-transcript` | an expanded agent transcript |
| `milestones` | a run's milestone checklist |
| `run-plan-gate` | a run parked at the approval gate |
| `mobile-*` | a phone-width window (narrow the window or use responsive mode) |

`mobile-*` and terminal (`uzi tui`) shots are not handled by these scripts: resize
the window narrow for mobile, or screenshot the terminal directly for the TUI.

## Judgment — do not replace a shot with a worse one

Some views only look right in a **live run state** (a plan gate needs a run parked at
approval; the activity feed and in-progress milestones need a run in flight). If no
such run exists, a fresh capture shows an empty or all-complete state that reads worse
than the existing image. When refreshing, compare each new shot against the current
one and **keep the better image** rather than always overwriting. A board with several
populated columns beats one with two, and so on.
