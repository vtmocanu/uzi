---
title: In-app changelog
order: 109
audience: user
---

# In-app changelog

uzi shows its own release notes without leaving the app.

## Opening it

Click the version badge in the sidebar footer (bottom-left) to open the
build-info popover, then click **Changelog**. A drawer slides in from the
left with the full release history, newest release first, scrollable
independently of the rest of the app.

## Reading a release

Each entry is a version, its release date, and its changes grouped by
category (Added, Changed, Fixed, Security, Dependencies, and so on). Two markers help
you place a release relative to what you're running:

- **You're running this** — the release matching your instance's version.
- **Newer** — a release that shipped after your instance's version.

These markers (and the summary banner at the top of the drawer) only appear
when your instance is running a normal `X.Y.Z` release; a `dev` or `demo`
build shows the same release history with no markers, since there is no
released version to compare against.

## Where the version comes from

The drawer doesn't ask for anything new — it reuses the same version your
instance already reports in the build-info popover, so the badge and the
drawer are always talking about the same running build.
