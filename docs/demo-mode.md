---
title: Demo mode
order: 74
audience: user
---

# Demo mode

Demo mode masks identifying values in the web UI — your email, your repo's
owner/namespace, your forge host, and a few other fields — so you can take
clean, shareable screenshots without editing them by hand afterward. It's a
**per-device** toggle, **off by default**, and changes nothing on the
server: the real data in the database and API is untouched, and no other
user is affected.

## Turning it on

1. Go to **Settings** and find the **Demo mode** card, kept visually
   separate from the theme picker above it — theme is a server setting that
   follows you across browsers, demo mode is not.
2. Or use the quick toggle in the sidebar, in the user area next to your
   name, which also shows "Demo mode: On/Off" as a state cue.

Either toggle updates the UI live (no reload needed), persists on this
device (`localStorage`), and syncs across every tab open in the same
browser. It does not follow you to another browser or device.

## What it masks

- **Email and display name** → a first name (e.g. `ada.lovelace@example.com`
  or `Ada Lovelace` → `Ada`). A handle-style email with no separator can't be
  told apart from a bare first name, so it collapses to the neutral `User`
  instead (e.g. `alovelace@example.com` → `User`)
- **Repo owner/namespace** → `demo/<repo>` (e.g. `vtmocanu/uzi` →
  `demo/uzi`)
- **Forge host / base URL** → `https://forge.example.com`
- **Forge usernames** (including issue and board authors) → `demo-user`
  (human) / `demo-bot` (bot)
- **CLI token last-used IP** → `203.0.113.7`
- **Registration email-domain allowlist** → `example.com`

Masking covers both visible text and tooltip/accessibility labels (`title`,
`aria-label`), so hovering for a tooltip or using a screen reader doesn't
leak the real value either.

## What stays real

Issue/MR/PR titles, branch names, issue/MR numbers, your worker names, and
the instance's brand/app name are never masked — the operator controls
those for a demo. Editable form fields (your public base URL, forge
connection inputs, and the like) also stay real, so forms and saves keep
working.

## Things to know before you screenshot

- **Search and filtering still use the real values, not the masked ones.**
  Demo mode only changes what's displayed, so typing a masked string (e.g.
  `demo/uzi`) into repo search finds nothing — search by the real name. It's
  for screenshots, not interactive filtering.
- **The browser's URL bar is never masked.** The SPA can't change the
  hostname or route shown there; crop it out of the screenshot or use a
  demo hostname.
- **Hovering a link can reveal the real forge host and path** in the
  browser's status bar — links keep their real target so they still work.
  Avoid hovering while you capture.
- **DevTools and the network tab still show real values.** Masking is
  presentation-only; API responses are never altered.
