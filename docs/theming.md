---
title: Theming
audience: design
---

# Theming

uzi ships five themes, each with a fixed **polarity**: **Ember** and
**Mission control** are dark; **Dawn**, **Hall**, and **Shadow** are light
(PRD #1167, "Lights on"). A theme is **tokens only**: CSS custom properties
keyed off a `[data-theme="…"]` attribute on `<html>`. No component file may
branch on the active theme (`if (theme === "mission")` does not exist
anywhere) — that is the one load-bearing rule everything else here serves.

Polarity is a theme's own fixed property (Go `theme.Theme.Polarity`,
mirrored by `THEME_POLARITY` in `web/src/lib/theme.ts`), not a setting a
user picks directly: it is what the **appearance mode** (System / Lights on
/ Lights off) selects between when deciding which of a user's two chosen
themes — one light, one dark — actually paints. See
[Appearance](./appearance.md) for the user-facing mode/theme/typeface
picker; this page stays on how a theme itself is built.

## How it works

Every color, radius, font, and the backdrop grid is a CSS variable, set once
per theme block in `web/src/index.css`: the default block (matched by
`:root` with no `data-theme` set, or `data-theme="ember"`), `[data-theme="mission"]`,
and one comma-list block shared by all three light themes,
`[data-theme="dawn"], [data-theme="hall"], [data-theme="shadow"]` — declared
once so the three can never drift apart (see "Light themes: one token set,
three scopes" below for where they differ). Components only ever reference
the Tailwind names those variables back (`bg-ink`, `text-fg`, `border-edge`,
`rounded-lg`, ...) via `tailwind.config.js` — never a variable or a raw
palette value directly. A theme that stays within the existing token slots
therefore never touches a component.

**Resolution is per field.** Each of the four appearance fields — mode,
light-slot theme, dark-slot theme, typeface — resolves independently: a
valid user value wins, else a valid instance default, else a compiled
fallback (`mode=dark`, `light=hall`, `dark=ember`, `typeface=system`). This
is `theme.ResolveAppearance` in `api/internal/theme`, mirrored by
`resolveAppearance` in `web/src/lib/theme.ts` so the SPA can re-derive the
same answer client-side without a round trip. The legacy single-theme
`theme.Resolve` (user override, else instance `default_theme`, else `ember`)
still exists as the pre-appearance helper but no longer drives resolution. See
[Appearance](./appearance.md) for the user-facing picker and
[Admin settings](./admin-settings.md#default-appearance) for the four
instance-default keys.

**Persistence is server-side**, not device-local: the four fields live in
nullable `users.appearance_mode` / `light_theme` / `dark_theme` / `typeface`
columns, read and written through `GET/PUT /api/me/settings` (a field left
`null` means "use the instance default"). The four instance defaults are
`app_settings` keys, edited from **Admin → Instance settings**. Every write
path validates against the same Go registry, polarity-aware for the two
theme slots (`ValidateFor("light", …)` / `ValidateFor("dark", …)`), so a dark
id handed to the light slot is rejected rather than silently accepted.
`GET /api/auth/me` carries the fully-resolved `appearance` object plus the
raw nullable `overrides` and the instance `defaults` separately, because the
Appearance picker needs to render "Use default (<name>)" and the correct
selected option, not just the already-resolved value.

Session bootstrap (`web/src/auth/AuthContext.tsx`) is where the appearance
is applied: on every login/`me` refresh it calls `applyAppearance`, which
stamps `<html data-theme>` and `<html data-font>`, so a change (yours, or
the admin's default) restyles the page live, with no reload. While the mode
is `system`, `applyAppearance` also arms a `prefers-color-scheme` media
query listener that re-stamps `data-theme` the moment the OS flips, and
tears the listener down when the mode changes away from `system`.

**Pre-paint cache.** Waiting for `me()` to resolve an appearance would flash
the wrong theme (and briefly the wrong OS polarity, in `system` mode) on
every cold load. `web/public/theme-preinit.js` is a small, dependency-free
script that stamps `data-theme` and `data-font` from a `localStorage` cache
(`uzi.appearance`, a JSON `{mode, light, dark, typeface}`) before first
paint; `applyAppearance()` refreshes that cache every time the
server-resolved value wins. It ships as an **external, same-origin file**
rather than an inline `<head>` script because `web/nginx.conf`'s CSP is
`script-src 'self'` with no `'unsafe-inline'` — an inline script would
simply never run in a built, nginx-served image (it only worked under
`vite dev`, which doesn't enforce the CSP). Keeping it external keeps the
"no inline scripts" posture literally true with no CSP change, at the cost
of the allowlist in that file needing its own entry per theme (see below).
With no cache at all (first visit, cleared storage) the script does not
leave `data-theme` unset: it evaluates the OS media query itself and stamps
the compiled fallback for that polarity (`hall` on an OS-light system,
`ember` on OS-dark), so a `system`-mode user never sees a wrong-polarity
flash before `me()` resolves the exact theme.

## Picking a theme

- **Per-user**: Settings → Appearance — mode, a light theme, a dark theme,
  and a typeface. See [Appearance](./appearance.md) for the walkthrough.
- **Instance default**: Admin → Instance settings → the four appearance
  defaults. See [Admin settings](./admin-settings.md#default-appearance).
- With nothing set anywhere (no user overrides, no admin defaults), every
  session renders `ember`, pixel-identical to before PRD #1167.

## Light themes: one token set, three scopes

Dawn, Hall, and Shadow are one light token set, declared once on the shared
`[data-theme="dawn"], [data-theme="hall"], [data-theme="shadow"]` block —
they cannot drift apart because there is only one place to edit. They differ
only in **where the dark factory still shows**, each expressed as a scope: a
selector that remaps the palette variables back to ember's own dark
channel triples (plus `color-scheme: dark`) so nested `bg-ink` /
`text-muted` / `border-edge` utilities keep working unchanged. All three
scopes share the exact same remap rule (`web/src/index.css`), which is what
makes "a machine surface looks like ember on every theme" a provable claim
rather than a visual approximation:

- **`console`** — code/log/CLI panes and the run activity feed. Applied to
  the opaque console surfaces (`.console` class) and to `.docs-prose pre`
  (the docs viewer's code blocks, which get no class to add). Present on all
  three light themes; it is what makes Dawn (the plainest of the three) keep
  machine output dark while every chrome surface is light.
  `[data-theme="dawn"] .console, [data-theme="hall"] .console,
  [data-theme="shadow"] .console`.
- **`frame`** — Hall's sidebar and mobile top bar. `[data-theme="hall"]
  .frame` rides the identical remap rule as `console`, so the frame is
  ember's brand tile, active item, badges, and rate-limit meters, token for
  token.
- **Shadow's live/attention attributes** — `data-live` and `data-attention`,
  rendered on `IssueCard`, run rows, and the `RunView` run header on
  *every* theme (a pure function of run status, `shadowSignal` in
  `runBadge.ts`), but only styled under Shadow. `[data-theme="shadow"]
  [data-live]` rides the same ember remap plus a solid `--surface`
  background, so a `claimed`/`running`/`planning` card, row, or header reads
  as a dark card. `[data-theme="shadow"] [data-attention]`
  (`awaiting_approval`, revising, or in review with an open PR) deliberately
  does **not** join the dark remap: it stays light with a rust inset rail
  and a 6%-rust tint over white, so a human's decision surface never sits
  inside a dark card.

## What a theme may touch

A theme change is scoped to: the token block it owns in `web/src/index.css`,
its entry in the Go registry (`api/internal/theme`) and the web registry
(`web/src/lib/theme.ts`), its allowlist entry in
`web/public/theme-preinit.js`, and Tailwind slot names in
`tailwind.config.js` when a new slot is introduced. It never touches a
component, a handler, or a migration. PRD #14's convention (zero raw
palette values or ad hoc hex/rgb literals outside `index.css`) still holds
under this feature — a theme port that needs to touch a component file is a
sign the underlying feature belongs in tokens, not in the theme. The
typeface (`--font-*`) lives in its own layer after every theme block (see
"Adding a theme" below) — it is a separate, orthogonal axis from a theme's
token block, never something a theme edit touches.

## Adding a theme

Adding a theme that stays within the existing token slots is exactly **five**
edits, no handler/component/migration changes (the contrast oracle needs no
edit at all — see "Guardrail tests" below):

1. **Go registry** — add the id, label, and **polarity** to `registry` in
   `api/internal/theme/theme.go` (`Theme{ID, Label, Polarity}`, `Polarity ∈
   {light, dark}`). This is the canonical list every write surface (the four
   `app_settings` appearance defaults, `PUT /api/me/settings`) validates
   against, polarity-aware for the two theme slots.
2. **Web registry** — add the id, its picker label, and its polarity to
   `THEMES` / `THEME_LABELS` / `THEME_POLARITY` in `web/src/lib/theme.ts`,
   the Go list's mirror. `LIGHT_THEMES` / `DARK_THEMES` are derived from
   `THEME_POLARITY`, never hand-listed, so stating the polarity here is the
   only place it needs to be said on the web side.
3. **CSS block** — add a `[data-theme="<id>"]` block in `web/src/index.css`
   defining every token the base block defines (a theme block is a complete
   set, not a diff against ember). A light theme that wants to share Dawn's
   token set can instead join the shared comma-list selector rather than
   duplicating it — see "Light themes: one token set, three scopes" above.
   The typeface layer (`--font-*`) is a **separate** block placed after
   every theme block, shared by all themes — a theme block never defines
   `--font-*` and never needs to.
4. **Pre-paint polarity map** — add the id to the literal `LIGHT` or `DARK`
   object in `web/public/theme-preinit.js`. This file is intentionally
   dependency-free (it runs before the app bundle loads), so it can't import
   the web registry and hardcodes the same map instead; a theme added
   without this edit still renders correctly once `me()` resolves, but the
   pre-paint script won't recognize it as a valid slot value for its
   polarity and falls back to the compiled default (`hall`/`ember`) for one
   frame on a cold load.
5. **Registry test** — add the id (and its polarity, for `TestPolarity`) to
   the literal lists `TestValid` and `TestPolarity` iterate in
   `api/internal/theme/theme_test.go` (they do not read `registry`, so a new
   id is otherwise untested there).

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

## Guardrail tests

Three parse-level tests keep a theme edit honest, none of them a manual
review:

- **The contrast oracle** (`web/src/lib/contrast.test.ts`) reads
  `index.css` from disk, auto-discovers every `[data-theme="…"]` selector,
  resolves each theme's `var()` aliases, and asserts WCAG 2.2 AA (≥ 4.5:1)
  for every text token against every surface, and for each status tone
  against its own 10% pill tint — for every theme, so a new or edited theme
  is covered automatically, no new test data required.
- **The console/frame parse guard** (`web/src/lib/console-scope.test.ts`)
  resolves the shared dark-remap rule (`console`, Hall's `frame`, Shadow's
  `[data-live]`) and asserts every one of its tokens, base palette plus the
  queue/neutral/syn/tool-rail aliases, equals ember's own value — the
  measurable form of "a machine surface looks like ember on every theme."
  It also guards a subtler CSS-inheritance trap: an alias declared only on
  the `[data-theme]` ancestor inherits its already-*resolved* light value
  into a nested scope, so the aliases must be re-declared inside the scope
  itself, not just the base palette.
- **An ember/mission colour snapshot**, parsing the two original dark
  blocks and asserting every token's channel triple is unchanged except the
  `--font-*` lines (which move to the typeface layer) — is the validation
  milestone's (M8) planned last guardrail, proving ember and mission render
  pixel-identical to before this feature. Not yet landed as of the m1–m6
  work merged so far; `console-scope.test.ts`'s ember-equality checks above
  are today's partial substitute.

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
