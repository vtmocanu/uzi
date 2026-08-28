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
has two modes:

- **Default** keeps uzi's own factory icon and wordmark, unchanged.
- **Custom** swaps in an uploaded logo. A **keep name** toggle picks how
  much of uzi's identity stays alongside it: on co-brands (your logo next
  to the `uzi` name), off is a full white-label (logo only, no uzi name
  anywhere).

The app mark renders everywhere uzi shows its identity: the signed-in
sidebar (desktop and the mobile drawer), the signed-out top bar, and the
mobile signed-in top bar.

## POWERED BY brand

An optional second mark in the sidebar, off by default:

- **Text** renders `POWERED BY <company>` with the company name you set.
- **Logo** renders an uploaded logo instead of text.

Placement is either **below** the wordmark (the default, and the only
placement that carries the `POWERED BY` label) or **top-right** of the
header (logo only, no label). For a dark-ink logo, turn on the **plaque**
option to render a light backing behind it. The logo itself is shown
slightly dimmed so it doesn't compete with uzi's own chrome.

## Uploading logos

There are two independent logo slots — the app mark and the POWERED BY
brand — each accepting **PNG, WebP, or SVG**, up to **256 KiB**. Uploading
replaces whatever was there; deleting reverts to the mode's default
(the shipped preset for a logo slot, or nothing for text). An uploaded SVG
is always rendered as an `<img>`, never inlined into the page, so it
cannot run a script.

## The Metaminds "M" preset

Turning on a custom or logo mode without uploading your own logo shows a
neutral preset mark (a Metaminds "M") until you upload one. **A fresh
install stays fully unbranded by default** — both modes start off, so
nobody sees the preset unless an admin deliberately turns branding on.

## The license credit is fixed

`MIT © Vlad Mocanu` is shown on the sidebar and on the signed-out footer.
It is not part of branding and can't be turned off, replaced, or hidden by
any combination of branding settings — including full white-label. Only
collapsing the sidebar hides it, the same as everything else in that row.

## Setting it up

1. Go to **Admin → Branding**.
2. Choose the app mark mode (Default/Custom) and, for Custom, the keep-name
   toggle; upload or clear a logo for that slot.
3. Choose the POWERED BY mode (None/Text/Logo), the company text for Text
   mode, placement, and the plaque option; upload or clear a logo for
   that slot.
4. Use the live preview to check the result before saving.
5. **Save.** Changes apply immediately, to every visitor.

See [Admin settings](./admin-settings.md) for the rest of the instance-wide
settings surface.
