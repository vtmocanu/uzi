---
title: Appearance
order: 43
audience: user
---

# Appearance

**Settings → Appearance** controls how uzi looks for you: a light/dark
mode, a theme for each polarity, and a typeface. Every choice is
server-side — it persists to your account and follows you across browsers
and devices, the same as your Anthropic tokens. A pick applies immediately;
if the save fails, it reverts and shows why.

## Mode

- **System** — follows your OS's light/dark setting, live: flipping your
  OS between light and dark restyles uzi immediately, with no reload.
- **Lights on** — always paints your chosen light theme.
- **Lights off** — always paints your chosen dark theme.

You hold one theme for **Lights on** and one for **Lights off**, so
switching mode — including to **System**, which follows the OS — never asks
you to re-pick: it just decides which of the two you've already chosen
paints right now.

## Theme

Pick a **Lights on theme** and a **Lights off theme**; whichever one isn't
currently painting shows dimmed, not hidden, so you can still change it
ahead of time.

Two dark themes: **Ember** (uzi's original look) and **Mission control**
(an ops-console identity). Three light themes, all sharing the same light
palette and differing only in where the dark "factory" still shows through:

| Theme | Where it stays dark |
|---|---|
| **Dawn** | Only machine surfaces — the run activity feed and code/log/CLI blocks. Everything else is light. |
| **Hall** | Dawn, plus the sidebar and mobile top bar. |
| **Shadow** | Dawn, plus a board card, run row, or run header while that run is actively working (claimed, running, or planning). A run waiting on you — awaiting approval, in review — stays light with a rust accent, so your decision surface is never inside a dark card. |

## Typeface

**System** uses your platform's default UI font — nothing downloaded.
**IBM Plex** is bundled with uzi, so switching to it fetches no external
font either; the files ship with the app.

## Use instance defaults

**Use instance defaults** clears every personal choice above, so you go
back to tracking whatever your admin has set instance-wide for mode, both
theme slots, and typeface. See
[Admin settings](./admin-settings.md#default-appearance) for what an admin
controls there.
