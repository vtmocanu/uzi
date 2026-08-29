# PRD #780 — Branding: named logo presets + tighter POWERED BY / footer

**Issue**: [#780](https://github.com/vtmocanu/uzi/issues/780)
**Priority**: Medium
**Status**: Done

## Problem

The Metaminds "M" mark ships as `web/public/brand-default.svg` (literally
`aria-label="Metaminds"`, `#ff3237`) and today only appears as a **silent fallback**:
when App-logo *custom* mode is selected but nothing is uploaded, the chrome renders that
file and the admin copy reads "No logo uploaded — the preset is shown." The preset is
unnamed and unpickable, so it reads as an undocumented default (this is exactly how our
own instance currently shows the M: `app_logo_mode=custom`, no `app` asset uploaded).

Two secondary sidebar-polish items in the same surface:

- The POWERED BY *below* placement (`AppShell.tsx` `PoweredBy`) draws a `border-b`
  separator, stacks a `POWERED BY` label over the value, and sits with a large gap below
  the wordmark.
- The sidebar footer renders the version badge and the `MIT © Vlad Mocanu` credit on two
  separate rows.

## Solution

1. **Promote Metaminds to an explicit, named app-logo preset**, chosen from a visual
   picker rather than reached by uploading nothing. Build it on a small **code-side preset
   catalog** so future presets are additive (one catalog entry + one shipped asset, no
   schema or migration change).
2. **Keep every existing branding option** — App-logo default/custom + keep-name +
   upload; POWERED BY none/text/logo + company text + placement below/topright + plaque +
   upload. Nothing customizable is removed.
3. **Restyle the POWERED BY *below* placement**: drop the separator, collapse `powered by`
   + value onto **one right-aligned line**, tucked close under the wordmark.
4. **Footer on one line**: version badge left (still the interactive `BuildInfoPopover`
   trigger), license credit right, no separator dot.

This is a self-hosted single-instance change (ours is the only install), so the
"migration" is a one-time, idempotent settings flip rather than a fleet concern.

## Scope of the change (old → new)

| # | Knob | Today | Proposed | Verdict |
|---|------|-------|----------|---------|
| 1 | App-logo mode | `default` / `custom` | `default` / `preset` / `custom` | kept + added |
| 2 | Metaminds M | silent fallback of custom+no-upload | named `preset` tile | promoted |
| 3 | Keep app name | toggle (custom only) | toggle (preset + custom) | kept, widened |
| 4 | Custom upload | yes | yes | kept |
| 5 | POWERED BY mode | none / text / logo | none / text / logo | kept |
| 6 | Company text | yes (≤64) | yes (≤64) | kept |
| 7 | Placement | below / topright | below / topright | kept |
| 8 | Plaque toggle | yes | yes | kept |
| 9 | Brand-logo upload | yes | yes | kept |
| 10 | POWERED BY *below* look | separator, stacked, gap | no separator, tight, one right-aligned line | visual only |
| 11 | Footer | version + credit, two rows | one row | visual only |
| 12 | License credit | constant, unstrippable | constant, unstrippable | kept |
| 13 | Our instance's look | custom + no-upload → M via fallback | one-time flip to `preset=metaminds`, renders identically | one-time, trivial |

The **only** behavioural change is dropping the *app-slot* implicit `custom + no-upload →
brand-default.svg` fallback: in the new model, `custom` with no upload shows the stock uzi
mark (a neutral "upload a logo" placeholder), and the Metaminds M is reached by choosing
the preset. The **POWERED BY logo-slot** fallback to `brand-default.svg` is unchanged.

## Design decisions

- **D1 — Preset catalog lives in web, single source of truth.**
  `web/src/lib/brandPresets.ts` exports an ordered list
  `[{ slug, label, asset }]`, e.g. `{ slug: "metaminds", label: "Metaminds", asset:
  "/brand-presets/metaminds.svg" }`. The chrome and the admin picker both resolve a slug
  through this registry (a map lookup, never string-interpolated into a path), so an
  unknown/empty slug degrades to the stock uzi mark. **Adding a preset later = one entry
  here + one file in `web/public/brand-presets/`.** No backend, schema, or migration
  change. **Both** the chrome (`AppShell.appMarkImgSrc`) and the admin `BrandingPreview`
  import this registry — neither hardcodes a preset path — so the preview cannot drift
  from what the sidebar renders (today `AdminBranding.tsx:174-179` hardcodes its own
  `/brand-default.svg` fallback independent of `AppShell`; M3 replaces that with the
  shared registry).
- **D2 — Server stores a slug, validates only its *shape*.** Add setting key
  `app_logo_preset` (default `""`), and extend `app_logo_mode`'s enum to include
  `preset`. Server validation of `app_logo_preset` is a safe-slug format check
  (`^[a-z][a-z0-9-]{0,31}$`, empty allowed) — deliberately **not** an existence check
  against the web catalog, so the two never have to be kept in lockstep and adding a
  preset stays a web-only change. An unknown slug is a graceful degrade (stock uzi mark),
  never an error at render.
- **D3 — Asset.** Add `web/public/brand-presets/metaminds.svg` (the Metaminds mark; may be
  byte-identical to the existing `brand-default.svg`). Leave `brand-default.svg` in place
  for the unchanged POWERED BY logo-slot fallback. (Dedupe is out of scope.)
- **D4 — Keep-name widens to any non-default mode.** Today `app_logo_keep_name` only
  matters in `custom`; it now also governs `preset` mode (a preset is a co-brand by
  default with the name kept; turning keep-name off is a full white-label with the preset
  mark alone). `default` mode always shows the name, as today.
- **D5 — Picker UX.** The admin App-logo `<select>` (`default`/`custom`) becomes a row of
  selectable **tiles**: `uzi` (→ mode `default`), `Metaminds` (→ mode `preset`,
  `app_logo_preset=metaminds`), `Custom` (→ mode `custom`, reveals the upload). Future
  presets render as additional tiles between Metaminds and Custom. The confusing "the
  preset is shown" copy is removed; each tile is self-describing.
- **D6 — POWERED BY restyle is *below*-only.** The `topright` placement is unchanged.
  Text mode always renders below (existing Decision D6 from PRD #685). The restyle applies
  to the below block for both text and logo.
- **D7 — Our-instance transition is an idempotent data migration**, not a fleet migration
  (ours is the only install): flip `app_logo_mode` `custom`→`preset` and set
  `app_logo_preset=metaminds` **only** for an instance currently in custom mode with no
  uploaded `app` asset. `app_settings` is a KV store with **no seeded branding rows** (an
  absent row synthesizes to the compiled-in default — `settings.go:337-338`), so the flip
  is **two** statements: an `UPDATE app_settings SET value='preset' WHERE key='app_logo_mode'
  AND value='custom' AND NOT EXISTS (SELECT 1 FROM branding_assets WHERE slot='app')`, and
  an `INSERT ... (key,value) VALUES ('app_logo_preset','metaminds') ON CONFLICT (key) DO
  NOTHING` (the `ON CONFLICT` guard is what makes the claimed idempotency real — the
  `app_logo_preset` row does not exist by default, and a plain INSERT would violate the PK
  on any re-derivation). The migration carries a matching `-- +goose Down` (delete the
  `app_logo_preset` row, set mode back to `custom`). Both tables exist before `00172`
  (`00036`, `00169`), so there is no ordering hazard.
- **D8 — The below label is lowercased to `powered by`** (matching the approved mock),
  down from today's uppercase `POWERED BY` (`AppShell.tsx:355`). This is a copy change:
  the suite's case-sensitive matcher (`POWERED_BY_RE = /POWERED BY/`,
  `AppShell.branding.test.tsx:216`) must be repointed to the new casing. "All POWERED BY
  options preserved" (rows 5–9) means the **modes/placement/plaque behaviour**, not the
  literal label string.

## Technical scope (file anchors)

**Backend (api):**
- `api/internal/settings/settings.go` — add `KeyAppLogoPreset = "app_logo_preset"`, its
  `Defaults` entry (`""`), the `preset` value in the `app_logo_mode` `validateEnum`
  (:1425), a `validateBrandingSlug`-style format check for `app_logo_preset`, and
  `AppLogoPreset string` on `BrandingConfig` + its read in `Branding()`.
- `api/internal/handler/branding.go` — add `AppLogoPreset string
  \`json:"app_logo_preset"\`` to `brandingResponse` (:58) and populate it in `GetBranding`.
  The exact-key-set allowlist guard is `TestBrandingPublicReadDefaultsAndAllowlistLiveDB`
  (`api/internal/handler/branding_livedb_test.go:80`, its `wantKeys` at :84–88) — add the
  new key there. **It is `*LiveDB`-gated**, so it skips silently in `task gate:api` / the
  local pre-push gate and only runs in CI's `test:api-store-it` job; do not read a green
  local gate as proof this passed. (`TestBrandingPublicReadIsAllowlisted` is only a prose
  reference in a code comment, not a test.)
- `api/internal/store/migrations/00174_app_logo_preset.sql` *(number assigned at merge;
  head is 00172)* — the D7 idempotent flip against `app_settings`.

**Frontend (web):**
- `web/src/lib/api.ts` — add `app_logo_preset` to `AppSettings` (:673), `Branding` (:769),
  and `UpdateSettingsPayload` (:805). Update the stale `// "default" | "custom"` mode
  comments to include `preset` (`settings.go:216`, `api.ts:752`, `:770`).
- `web/src/lib/brandPresets.ts` *(new)* + `web/public/brand-presets/metaminds.svg` *(new)*
  — the D1 catalog and asset.
- `web/src/components/AppShell.tsx` — `appMarkImgSrc` resolves `preset` mode via the
  catalog (unknown → `null` → `FactoryIcon`); `appMarkShowName` widens to preset (D4);
  drop the app-slot `custom`+no-upload → `brand-default.svg` fallback; `PoweredBy` below
  block restyle (D6); footer version+credit one-row (`AppShell.tsx` ~:884–902), preserving
  collapsed-rail behaviour. The four surfaces that render through `AppMark` all get the
  preset; the favicon (`web/src/lib/favicon.ts`, hardcoded FactoryIcon paths) and the
  Landing hero (`web/src/pages/Landing.tsx:10`, direct `<FactoryIcon>`) do **not** read
  branding and stay the uzi factory icon — see Out of scope.
- `web/src/pages/AdminBranding.tsx` — replace the App-logo `<select>` with the tile picker
  (D5), wire `app_logo_mode` + `app_logo_preset`, keep the upload + keep-name toggle;
  remove the **app-slot** "No logo uploaded — the preset is shown." copy (:229). **Note the
  identical string is duplicated at :303 for the POWERED BY logo slot, whose fallback is
  unchanged — that copy STAYS.** Rewrite the inline `BrandingPreview` (:174-179, :345, :376)
  to resolve the mark through the shared `brandPresets` registry (not its own hardcoded
  fallback), and to mirror the restyled POWERED BY below + one-line footer.

**Docs:**
- `docs/branding.md` — rewrite the "App mark" and "Metaminds M preset" sections for the
  named-preset model; note presets are extensible; keep the license-credit section.

**Tests:**
- `api/internal/settings` + `api/internal/handler` — enum accepts `preset`, rejects a
  bad slug, `Branding()`/`brandingResponse` carry `app_logo_preset`, and the LiveDB
  allowlist `wantKeys` is updated (see Backend anchors — LiveDB-gated, CI store-it only).
- `web/src/components/AppShell.branding.test.tsx` — preset mode renders the catalog asset;
  unknown slug falls back to `FactoryIcon`; keep-name off in preset mode hides the name;
  **flip the existing "custom + not present → `/brand-default.svg`" assertion (:171-177)**
  to expect the stock uzi `FactoryIcon` (this is the one existing app-slot test the
  behaviour change invalidates). The `brandLogoImgSrc` fallback test (:228-234) stays valid
  — it justifies keeping `brand-default.svg` (D3). **Every existing POWERED BY
  mode/placement/plaque assertion still passes** (nothing lost), after repointing the
  case-sensitive `POWERED_BY_RE` matcher for D8.
- **jsdom has no layout engine**, so Success Criterion #4's "one line / right-aligned / one
  row" are asserted via **class proxies**: `border-b` is absent from the below block
  (`AppShell.tsx:354`) and a right-align/one-row class is present (`text-right` /
  `justify-end` / `ml-auto` / `justify-between`). A class-**absence** assertion is itself a
  negative assertion — pair it with a positive one so it can't go vacuous on a future class
  rename (`.claude/rules/web.md`).
- `web/src/pages/AdminBranding.test.tsx` — tile selection sets mode+preset; Custom reveals
  upload; preview reflects selections.
- **Copy sweep, not a blind removal:** "No logo uploaded — the preset is shown." is
  duplicated (app slot :229 removed, brand slot :303 kept). Grep the retired **app-slot**
  wording across the web test tree and repoint/remove only its negative assertions; the
  brand-slot copy still renders, so a `queryByText(/preset is shown/)` is NOT vacuous and
  must not be deleted (per `.claude/rules/web.md`).

## Milestones

- [x] **M1 — Backend: preset setting + mode enum + public field.** `app_logo_preset` key
      with default + slug-format validation; `preset` added to the `app_logo_mode` enum;
      `AppLogoPreset` on `BrandingConfig` and in the public `/api/branding` response
      (update the LiveDB allowlist `wantKeys`). `task gate:api` green — note the allowlist
      guard is LiveDB-gated and runs in CI's store-it job, not the local gate.
- [x] **M2 — Web catalog + chrome mark resolution.** `brandPresets.ts` catalog +
      `metaminds.svg` asset; `api.ts` types + stale-comment fixes; `AppShell.tsx` resolves
      `preset` mode via the catalog (unknown → uzi mark), widens keep-name to preset, and
      drops the app-slot implicit metaminds fallback (flip the `:171-177` test). Chrome
      renders the preset on all four surfaces (sidebar desktop + mobile, signed-out top bar,
      mobile signed-in top bar). **M2 and M5 ship in the same PR — never M2 alone**, or our
      live sidebar loses the M until the M5 flip.
- [x] **M3 — Admin picker + preview.** Replace the App-logo `<select>` with the tile
      picker wiring mode+preset; remove the confusing copy; keep upload + keep-name;
      `BrandingPreview` mirrors the new resolution.
- [x] **M4 — POWERED BY restyle + one-line footer.** Below placement: no separator, tight,
      one right-aligned `powered by` line (D8; chrome + preview), all modes/placement/plaque
      preserved (repoint the case-sensitive matcher); footer version + credit on one row,
      collapsed-rail behaviour intact. Layout asserted via class proxies (jsdom).
- [x] **M5 — Our-instance transition.** Idempotent two-statement `app_settings` migration
      (`UPDATE` mode + `INSERT ... ON CONFLICT DO NOTHING` preset) with a `-- +goose Down`,
      flipping custom+no-app-asset → `preset=metaminds`; verify the live sidebar renders
      identically.
- [x] **M6 — Docs + full gate.** Update `docs/branding.md`; `task gate` (api + web) green,
      including the copy-retirement grep sweep.

## Success criteria

1. An admin can pick Metaminds from a visible tile without uploading anything; the choice
   is explicit and named, and the **app-slot** "preset is shown" fallback copy is gone (the
   identical brand-slot copy at :303 stays, since that fallback is unchanged).
2. Adding a hypothetical second preset requires only a `brandPresets.ts` entry + a file in
   `web/public/brand-presets/` — no backend, schema, or migration change (demonstrated by
   the catalog shape, not shipped).
3. Every existing branding capability still works: default/custom/keep-name/upload for the
   app mark; none/text/logo + company + below/topright + plaque + upload for POWERED BY.
   The `AppShell.branding` suite proves each still renders.
4. POWERED BY *below* renders one right-aligned line with no separator, tucked under the
   wordmark; the footer is one row; `topright` and text-always-below behaviour unchanged.
   (Verified visually in a browser; jsdom tests assert the class proxies, not the layout.)
5. After deploy, our live instance shows the Metaminds M exactly as before (now via
   `preset` mode), with no visual disruption.
6. `MIT © Vlad Mocanu` remains unstrippable by any branding combination.

## Risks & mitigations

- **R1 — Dropping the app-slot fallback changes our live mark before the flip.** M2 (web
  bundle) and M5 (api migration, runs at boot) ship in one PR but are separate deploy
  artifacts, so a **brief cosmetic window** is possible during rollout in either order (new
  web + old row → uzi mark until the migration; new migration + old web bundle that doesn't
  understand `preset` → `appMarkImgSrc` returns null → uzi mark). Accepted: it is seconds,
  cosmetic, our single instance only, and self-corrects once both land; if it matters,
  hand-select the Metaminds tile in Admin → Branding post-deploy. "Byte-identical after
  deploy" is the steady state, not an atomic-cutover guarantee.
- **R2 — Server/web preset lists drift.** Avoided by design (D2): the server validates
  slug *shape* only; the web catalog is the single source; unknown slugs degrade
  gracefully.
- **R3 — Slug as a path.** The web resolves slug via a registry map, never by
  interpolating it into an asset URL, so a hostile slug cannot traverse; the server
  format check is defense-in-depth.
- **R4 — The "preset is shown" copy is DUPLICATED, so a blind retire-the-string sweep is
  itself the hazard** (`.claude/rules/web.md`). Only the app-slot copy (:229) is removed;
  the brand-slot copy (:303) stays and still renders, so a `queryByText(/preset is shown/)`
  does **not** go vacuous and must not be deleted. Mitigation: sweep for the app-slot
  wording specifically and repoint only its assertions.
- **R5 — Restyle regresses the collapsed rail or the signed-out shell.** Mitigated by
  keeping the collapsed-rail and PublicShell paths in the `AppShell.branding` suite.

## Out of scope

- **Branding the favicon and the Landing hero.** `web/src/lib/favicon.ts` and
  `web/src/pages/Landing.tsx:10` render the FactoryIcon directly, bypassing `AppMark`, so
  after this PRD the browser-tab favicon and the marketing hero stay the uzi factory icon
  while the sidebar shows the preset — a known, accepted inconsistency. Making those
  branding-aware is a separate, larger change (favicon needs runtime regeneration).
- Deduplicating `brand-default.svg` and the new `metaminds.svg`.
- Any new POWERED BY placement or mode.
- Making the license credit configurable (it stays fixed by design).
- A server-served preset catalog endpoint (the code-side web catalog is sufficient for the
  foreseeable preset count).

## Decision log

- Chose a **web-owned catalog + slug-shape server validation** over a server-enforced
  preset enum, so "add a preset" is a pure web change and the two sides never need
  lockstep updates. Trade-off accepted: the server cannot reject a slug the web doesn't
  know, but that degrades gracefully to the uzi mark.
- Chose an **idempotent data migration** over a manual admin-UI flip for the our-instance
  transition, to avoid any window where the M disappears between deploy and the flip.
- Kept the **POWERED BY logo-slot fallback** to `brand-default.svg` untouched; only the
  *app-slot* implicit fallback is retired, because that is the one that reads as a
  confusing default.
