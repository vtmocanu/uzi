---
title: Branding
order: 72
audience: user
---

# Branding

**Admin → Branding** lets an admin put an org's own mark on a self-hosted
uzi instance, at runtime, with no redeploy. It's its own tab, after
*Instance*. Only an admin can change it, but the result is visible to
**everyone** who loads the app, including a signed-out visitor.

## App mark

The top-left mark — the icon and `uzi` / `uzinele întunecate` wordmark —
is chosen from a **tile picker**, not a dropdown. Click a tile to switch:

- **uzi** (the default) keeps uzi's own factory icon and wordmark,
  unchanged.
- **A named preset**, e.g. **Metaminds** (the red "M") — its own tile,
  picked directly. No upload needed.
- **Custom** uploads your own logo.

For a preset or Custom, a **keep name** toggle picks how much of uzi's
identity stays alongside the mark: on co-brands (the mark next to the
`uzi` name), off is a full white-label (mark only, no uzi name
anywhere). The default `uzi` tile always shows the name.

The app mark renders everywhere uzi shows its identity: the signed-in
sidebar (desktop and the mobile drawer), the signed-out top bar, and the
mobile signed-in top bar.

## POWERED BY brand

An optional second mark in the sidebar, off by default:

- **Text** renders the company name you set.
- **Logo** renders an uploaded logo instead of text.

Placement is either **below** the wordmark (the default) or **top-right**
of the header (logo only, no label). Below renders as one tight,
right-aligned line tucked under the wordmark, with a faint lowercase
`powered by` label inline before the company text or logo, and no
separator rule above it. For a dark-ink logo, turn on the **plaque**
option to render a light backing behind it. The logo itself is shown
slightly dimmed so it doesn't compete with uzi's own chrome.

## Uploading logos

There are two independent logo slots — the app mark's Custom tile and the
POWERED BY brand — each accepting **PNG, WebP, or SVG**, up to
**256 KiB**. Uploading replaces whatever was there. Deleting the app mark's
Custom logo falls back to the stock uzi factory icon, a neutral
placeholder — it does not show a preset. The POWERED BY logo slot falls
back to a shipped default logo when nothing is uploaded. An uploaded SVG
is always rendered as an `<img>`, never inlined into the page, so it
cannot run a script.

## Named presets

Presets are built-in marks you pick directly from the app-mark tile
picker — no upload required. Today the catalog has one, **Metaminds**
(a red "M"); it's an explicit, named tile, not something that appears
only when a custom slot is left empty. Presets are extensible: they come
from a small built-in catalog, so more can be added over time, each as
its own tile.

**A fresh install is fully unbranded by default** — the app mark starts
on `uzi` and POWERED BY starts off, so nobody sees a preset or a
co-brand unless an admin deliberately turns branding on.

## The license credit is fixed

`MIT © Vlad Mocanu` is shown on the sidebar footer row (alongside the
build/version badge) and on the signed-out footer. It is not part of
branding and can't be turned off, replaced, or hidden by any combination
of branding settings — including full white-label. Only collapsing the
sidebar hides it, the same as everything else in that row.

## Setting it up

1. Go to **Admin → Branding**.
2. Pick an app-mark tile — `uzi`, a named preset like Metaminds, or
   Custom. For Custom, upload or clear a logo; for a preset or Custom,
   set the keep-name toggle.
3. Choose the POWERED BY mode (None/Text/Logo), the company text for Text
   mode, placement, and the plaque option; upload or clear a logo for
   that slot.
4. Use the live preview to check the result before saving.
5. **Save.** Changes apply immediately, to every visitor.

See [Admin settings](./admin-settings.md) for the rest of the instance-wide
settings surface.
