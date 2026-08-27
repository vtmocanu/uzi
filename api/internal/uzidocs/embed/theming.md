---
title: Theming
audience: design
---

# Theming

uzi ships two themes today, **Ember** (the original dark-factory look) and
**Mission control** (an ops-console identity ported from a prototype
branch, PRD #21). A theme is **tokens only**: CSS custom properties keyed
off an `[data-theme="…"]` attribute on `<html>`. No component file may
branch on the active theme (`if (theme === "mission")` does not exist
anywhere) — that is the one load-bearing rule everything else here serves.

## How it works

Every color, radius, font, and the backdrop grid is a CSS variable, set once
per theme in `web/src/index.css`, under the default block (matched by
`:root` with no `data-theme` set, or `data-theme="ember"`) and under
`[data-theme="mission"]`. Components
only ever reference the Tailwind names those variables back (`bg-ink`,
`text-fg`, `border-edge`, `rounded-lg`, ...) via `tailwind.config.js` — never
a variable or a raw palette value directly. A theme that stays within the
existing token slots therefore never touches a component.

**Resolution chain**: a user's personal override wins; failing that, the
admin-set instance default; failing that, `ember`. This is computed
server-side (`theme.Resolve` in `api/internal/theme`) and mirrored in the web
theme module (`resolveTheme` in `web/src/lib/theme.ts`) so the SPA can
re-derive the same answer client-side without waiting on a round trip.

**Persistence is server-side**, not device-local: the user's override lives
in a nullable `users.theme` column, read and written through the existing
`GET/PUT /api/me/settings` (the same endpoint `default_model` uses; `theme:
null` means "use default"). The instance default is a `default_theme` key in
PRD #19's `app_settings`, edited from **Admin → Instance settings**. Both
write paths validate against the same Go registry (`api/internal/theme`), so
a bad value is rejected at write and can never fall back silently at render.
`GET /api/auth/me` carries all three theme fields — the resolved theme, the
user's raw override (nullable), and the instance default — because the
Appearance picker needs the override and the default separately (to render
"Use default (Mission control)" and to show the right selected option), not
just the already-resolved value.

Session bootstrap (`web/src/auth/AuthContext.tsx`) is where the theme is
applied: on every login/`me` refresh it re-resolves from the override and
default fields and stamps `<html data-theme>`, so a change (yours, or the
admin's default) restyles the page live, with no reload. The server also
sends an already-resolved `theme` field on the session payload; the client
does not read it and instead re-resolves itself, so that a server predating
these fields still degrades to `ember` rather than throwing on a missing
field. That's intentional, not dead code.

**Pre-paint cache.** Waiting for `me()` to resolve a theme would flash the
wrong one on every cold load. `web/public/theme-preinit.js` is a small,
dependency-free script that stamps `data-theme` from a `localStorage` cache
(`uzi.theme`) before first paint; `applyTheme()` in the web theme module
refreshes that cache every time the server-resolved value wins. It ships as
an **external, same-origin file** rather than an inline `<head>` script
because `web/nginx.conf`'s CSP is `script-src 'self'` with no
`'unsafe-inline'` — an inline script would simply never run in a built,
nginx-served image (it only worked under `vite dev`, which doesn't enforce
the CSP). Keeping it external keeps the "no inline scripts" posture literally
true with no CSP change, at the cost of the allowlist in that file needing
its own entry per theme (see below). A missing or blocked cache just leaves
`data-theme` unset, which renders `ember` — never a broken or half-themed
page.

## Picking a theme

- **Per-user**: Settings → Appearance → Theme. Options are "Use default
  (<instance default's name>)" plus each theme by name. Applies live and
  follows you across browsers (it's server-side, not a cookie/local
  setting).
- **Instance default**: Admin → Instance settings → Default theme. Applies
  to every user who has not picked a personal override; changing it restyles
  those sessions live on their next `me` refresh.
- With nothing set anywhere (no user overrides, no admin default), every
  session renders `ember`, pixel-identical to before this feature existed.

## What a theme may touch

A theme change is scoped to: the token block it owns in `web/src/index.css`,
its entry in the Go registry (`api/internal/theme`) and the web registry
(`web/src/lib/theme.ts`), its allowlist entry in
`web/public/theme-preinit.js`, and Tailwind slot names in
`tailwind.config.js` when a new slot is introduced. It never touches a
component, a handler, or a migration. PRD #14's convention (zero raw
palette values or ad hoc hex/rgb literals outside `index.css`) still holds
under this feature — a theme port that needs to touch a component file is a
sign the underlying feature belongs in tokens, not in the theme.

## Adding a theme

Adding a theme that stays within the existing token slots is exactly **four**
edits, no handler/component/migration changes:

1. **Go registry** — add the id to `registry` in `api/internal/theme/theme.go`.
   This is the canonical list both write surfaces (`default_theme` on
   `app_settings`, `theme` on `/api/me/settings`) validate against.
2. **Web registry** — add the id (and its picker label) to `THEMES` /
   `THEME_LABELS` in `web/src/lib/theme.ts`, the Go list's mirror.
3. **CSS block** — add a `[data-theme="<id>"]` block in `web/src/index.css`
   defining every token the base block defines (a theme block is a complete
   set, not a diff against ember).
4. **Pre-paint allowlist** — add the id to the literal string check in
   `web/public/theme-preinit.js`. This file is intentionally
   dependency-free (it runs before the app bundle loads), so it can't import
   the web registry and hardcodes the same list instead; a theme added
   without this edit still renders correctly once `me()` resolves, it just
   flashes `ember` for one frame on a cold load.

**A theme that needs a slot that doesn't exist yet is a different, two-step
change**: add the slot theme-agnostically first (every existing theme gets a
value, ember's chosen to be a no-op / visually identical to before), then
give the new theme its own value in step 3 above. This is how `queue` and
`neutral` were added for Mission control's violet queue tone: ember's
`queue`/`neutral` slots resolve to its existing solid gray pill (zero visual
change), and only Mission's block gives them a distinct color. Each of those
two slots is a **border/surface/fg triple**, not a single token — a single
hue-at-opacity token (the pattern the four original status tones use) can't
reproduce ember's solid `border-edge bg-raised text-muted` pill, so the
triple schema exists specifically to keep that pill pixel-identical while
still being themeable.

## Tailwind `neutral` caution

`tailwind.config.js` defines `neutral` as an object (`neutral.fg`,
`neutral.border`, `neutral.surface`, the token triple above) inside
Tailwind's default color palette, which already ships a `neutral-50`…
`neutral-950` gray scale. Tailwind merges the two: `bg-neutral-border` etc.
resolve to the token, but `bg-neutral-500` still resolves to stock Tailwind
gray, not a themed value. Nothing in `web/src` uses a bare `neutral-<number>`
class today, but a future edit reaching for one expecting it to be
theme-aware will silently get an untethered gray instead — use `queue-*` /
`neutral-{fg,border,surface}` explicitly.

## Good to know

- A theme-only change (a user's override, or the admin default) never
  triggers the label-driven board resync (`ForceReconcile`): it's
  presentation-only and carries no forge-visible effect.
- Mock mode (`VITE_UZI_MOCK=1`) mirrors both write surfaces in memory, so the
  full picker flow (user override and admin default) is exercised in the
  demo build without a real backend.
