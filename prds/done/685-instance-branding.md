# PRD #685 — Instance Branding

**Issue**: [#685](https://github.com/vtmocanu/uzi/issues/685)
**Status**: Complete (2026-08-28)
**Priority**: Medium
**Author**: Vlad Mocanu

> Self-contained for an offline uzi worker. Every implementation anchor (file:line), the data model, the endpoint contracts, the CSP finding, and the shipped SVG asset content are stated inline — no open-web lookup is required. This PRD touches **no** `.github/workflows/**` file in either implementation or validation (worker-PAT workflow-scope guardrail respected), and needs **no** CSP/chart change (Decision D5). It **does** add one goose migration (Decision D7) — a data file, not a workflow file.
>
> Anchors below were verified against the tree during review; treat exact line numbers as "at/near" and re-derive at implementation time (the tree moves).

---

## Problem

uzi is a product that different orgs self-host, but its visual identity is hardcoded. The sidebar app mark (orange factory icon + `uzi` / `uzinele întunecate`) is baked into `AppShell.tsx` as inline SVG + string literals, so an operator cannot put their own mark on their instance, co-brand it, or white-label it. There is also no license/author attribution surfaced anywhere in the running app.

## Solution

A new **Admin → Branding** tab (its own tab, after *Instance*) that lets an admin, at runtime with no redeploy:

1. **Replace the app mark** (top-left) with a custom logo — either swap the icon only (keep the `uzi` name) or a full white-label (logo only).
2. **Add a "POWERED BY" brand** in the sidebar — either **text** (`POWERED BY <COMPANY>`) or a **logo**, placed below the wordmark (default) or top-right of the header.

Plus a **durable, build-time-constant** `MIT © Vlad Mocanu` credit rendered in the app chrome (Decision D3) — not admin-editable, so a rebrand cannot strip the license/author attribution.

Branding **config** (modes, flags, company text) lives in the existing `app_settings` KV store and is served to everyone (incl. signed-out) via a new public `GET /api/branding` that returns **only** the branding fields. Logo **bytes** live in a dedicated `branding_assets` table (Decision D7) and are served as cacheable images from `GET /api/branding/logo/{slot}` — never inlined in the JSON. A Metaminds "M" SVG preset ships in-repo as `web/public/brand-default.svg` so a mode can be enabled with one click, but **fresh installs are unbranded by default** (`app_logo_mode=default`, `brand_mode=none`).

Approved interactive design mock (visual reference; its concrete constants are baked into the milestones): the branding mock built during design review.

## Scope of the visual surfaces (white-label completeness)

The app mark renders in more than one place; `custom` app-logo (and the keep-name flag) must apply to **all** of them:

- **Sidebar header** — `web/src/components/AppShell.tsx:436-466` (inside `SidebarContent`), a `<Link to="/dashboard">` with the icon `<span>` (lines 445-447, `<FactoryIcon/>`) + name/subtitle literals (lines 450-455). `SidebarContent` mounts **twice**: desktop `<aside>` (`AppShell.tsx:942`, passes `collapsed`) and mobile drawer (`AppShell.tsx:983`, always expanded). Collapsed (lines 439-465) shows the icon only + a `title="uzi · uzinele întunecate"` tooltip.
- **Signed-out top bar** — `PublicShell`, `AppShell.tsx:703-708` (`uzi · uzinele întunecate`, uses `FactoryIcon`).
- **Mobile signed-in top bar** — `AppShell.tsx:964-966` (currently the divergent string `uzi · dark factory`, no icon). Reconcile it to the same branded source while here. **This is an intended default-mode change** (see M4 — it is *not* covered by "default render unchanged").

`FactoryIcon` is the inline component at `web/src/components/icons.tsx:293-298`. The default (unbranded) path keeps rendering `FactoryIcon` + literals exactly as today; only `custom` mode swaps them.

**Out of scope for v1** (follow-ups): browser-tab `<title>` (`web/index.html:6`) and favicon (`web/index.html:10-11` + the PRD #70 dynamic `useFavicon` status overlay). Deliberately excluded — the favicon carries a live status overlay and is a separate mechanism.

## Data model

### Config keys — `app_settings` KV (no migration for these)

All **non-secret** public config → they go in `Defaults` (NOT `SecretKeys`) in `api/internal/settings/settings.go`. Pattern: add to the `Key*` const block (`settings.go:37-194`), the `Defaults` map (`settings.go:333-407`, which auto-satisfies `Known()` at `settings.go:433-438`), and a per-key `case` in `Validate()` (`settings.go:1211-1259`). They round-trip through `PUT /admin/settings` and `AdminView` (`settings.go:1159-1184`) with no migration (absent row → default; `app_settings.value` is Postgres `TEXT`). No blobs live here (D7), so the settings cache stays small.

| Key | Values | Default | Notes |
|---|---|---|---|
| `app_logo_mode` | `default` \| `custom` | `default` | `default` = uzi FactoryIcon + literals |
| `app_logo_keep_name` | `true` \| `false` | `true` | keep `uzi`/`uzinele întunecate` next to a custom mark; `false` = full white-label |
| `brand_mode` | `none` \| `text` \| `logo` | `none` | the POWERED BY brand |
| `brand_company` | text (≤ 64 runes, may be `""`) | `""` | text mode → `POWERED BY <COMPANY>` |
| `brand_placement` | `below` \| `topright` | `below` | below-wordmark (carries the POWERED BY label) or top-right of the header (logo-only, ~96px, no label) |
| `brand_plaque` | `true` \| `false` | `false` | optional light plaque behind the POWERED BY logo (for dark-ink uploads) |

There is **no `app_logo`/`brand_logo` settings key** — logo bytes are not KV values (D7). Whether a custom logo exists is exposed as a derived boolean in the public read (`app_logo_present`, `brand_logo_present`).

**"Enable the M" path**: set `brand_mode=logo` (or `app_logo_mode=custom`) with **no** uploaded asset for that slot → the chrome falls back to the shipped `/brand-default.svg`. Uploading overrides the fallback; deleting the asset reverts to it.

### Logo bytes — `branding_assets` table (D7)

New goose migration (draft number `00164`; **assign the next free number above the live head at landing** per the goose-numbering convention — live head was `00163` at authoring). Strict goose, no `allow-missing`:

```sql
-- +goose Up
CREATE TABLE branding_assets (
    slot         TEXT PRIMARY KEY CHECK (slot IN ('app','brand')),
    content_type TEXT NOT NULL CHECK (content_type IN ('image/png','image/webp','image/svg+xml')),
    bytes        BYTEA NOT NULL,
    updated_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- +goose Down
DROP TABLE branding_assets;
```

At most two rows (`app`, `brand`). Read only by the image routes; never loaded into the settings cache. Store methods: `GetBrandingAsset(slot)`, `UpsertBrandingAsset(slot, contentType, bytes, userID)`, `DeleteBrandingAsset(slot)`.

### Validation (`Validate()` cases in `settings.go`)

The default `Validate` branch calls `ValidateLabel` — capped at **64 runes** (`const maxLabelLen = 64`, `settings.go:317`; validator body `settings.go:1549`) and it **rejects empty, whitespace, and commas**. So each key needs an explicit `case` (precedent: `validateAgentSourceCredential`, const cap at `settings.go:1266`, body `settings.go:1274`):

- `app_logo_mode` ∈ {`default`,`custom`}; `brand_mode` ∈ {`none`,`text`,`logo`}; `brand_placement` ∈ {`below`,`topright`}.
- `app_logo_keep_name`, `brand_plaque` ∈ {`true`,`false`}.
- `brand_company`: **dedicated** validator — allow `""`, allow commas, cap 64 runes, and **reject control / format runes** via `termsafe.Validate` (reject `unicode.IsControl(r) || unicode.In(r, unicode.Cf)`). It is admin-authored text rendered into every user's chrome incl. signed-out, so it is the "rendered to a principal other than the author" class that `.claude/rules/web.md` requires to pass `termsafe.Validate` (an RTL-override rune must not be able to mangle the chrome for everyone). Do **not** reuse `ValidateLabel` — it would 400 the `""` default and reject "Acme, Inc.".

Logo-byte validation lives in the **upload handler** (below), not in settings `Validate()`.

## Endpoints

### Public read — `GET /api/branding` (unauthenticated, allowlisted)

Modeled on `Version` (`api/internal/handler/handler.go:530-564`, route at `handler.go:593`, mounted outside every `RequireAuth` group with only `Recoverer`/`RequestID`, "unauthenticated by design"). **Build an explicit allowlisted struct from exactly the branding fields — do NOT reuse `Cache.All()` (`settings.go:1144`) or `AdminView.Values` (`settings.go:1168`)**, both of which range over all of `Defaults` and would leak the entire non-secret settings surface (base URLs, quotas, budgets, repo IDs…) to anonymous users (Risk R1).

The KV store is all-strings; this endpoint returns a **typed** JSON (bools coerced from `"true"`/`"false"` in the Go handler). `app_logo_present`/`brand_logo_present` are derived from `branding_assets` row existence:

```json
{
  "app_logo_mode": "default",
  "app_logo_present": false,
  "app_logo_keep_name": true,
  "brand_mode": "none",
  "brand_company": "",
  "brand_placement": "below",
  "brand_plaque": false,
  "brand_logo_present": false
}
```

### Public logo bytes — `GET /api/branding/logo/{slot}` (unauthenticated)

`slot` ∈ {`app`,`brand`}. Reads `branding_assets`; returns the bytes with the stored `Content-Type`, a strong `ETag` (hash of bytes or `updated_at`), and `Cache-Control: public, max-age=…` so repeat loads are cheap `304`s (mitigates the DoS/amplification tension of a large unauthenticated body — Risk R2). `404` when absent (chrome then uses the preset/none per mode). This is the reason logos are NOT in the `/api/branding` JSON.

### Admin write — settings + logo upload/delete (admin-gated)

- Config keys: existing `PUT /admin/settings` (map-based; adding the keys to `Key*`/`Defaults`/`Validate` makes them round-trip with no new Go struct field — verified: `settingsResponse`/`AdminView`/`updateSettingsRequest` are `map[string]string`).
- Logo bytes: new `PUT /api/admin/branding/logo/{slot}` (admin-gated, under the existing auth group) — accepts the image (raw body with `Content-Type`, or JSON `{content_type, base64}`), validates **type** ∈ {png,webp,svg+xml} and **size** (see cap below), and upserts `branding_assets`. `DELETE /api/admin/branding/logo/{slot}` clears it. Keeping bytes off `PUT /admin/settings` also avoids the 1 MiB JSON body cap (`httpx.maxBodyBytes`, `api/internal/httpx/respond.go:22`) that two base64 logos in one settings PUT would strain (Risk R4).

**Size cap (exact, for table-driven tests):** raw image ≤ **262144 bytes** (256 KiB). At-cap passes; `cap+1` rejected with a clear size error.

### Route-limiter pins (mandatory, not conditional)

`api/internal/handler/route_limiter_mounts_test.go` walks **every** mounted route (`chi.Walk`) and fails on any route absent from `wantRouteMounts`; a per-route count constant must also be bumped. Add rows and bump the count for all three new routes:
- `{"GET", "/api/branding", noLimiter}` (mirror `/api/version` at `:287`),
- `{"GET", "/api/branding/logo/{slot}", noLimiter}`,
- the admin `PUT`/`DELETE /api/admin/branding/logo/{slot}` with the same per-user limiter the other `/api/admin/*` writes carry.

### Client wiring (`web/`)

- `web/src/lib/api.ts`: add `branding: () => request<Branding>("GET", "/branding")` (next to `version`, `api.ts:2754`) + a `Branding` interface typed with **bools** for `app_logo_keep_name`/`brand_plaque` and the two `*_present` flags (while `AppSettings` keeps the six config keys as **strings**, `api.ts:652` — the admin page works in string-space via `getSettings`/`updateSettings`, the chrome in bool-space via `useBranding`; state this string↔bool split so the two consumers aren't confused). Add upload/delete client calls for the logo endpoints. Logo image URLs are just `/api/branding/logo/app` etc. used directly in `<img src>`.
- Memoize the `GET /api/branding` fetch at module scope exactly like `buildInfoPromise` (`AppShell.tsx:85`; hooks `useBuildInfo`/`useAppVersion` at `AppShell.tsx:130/177`) so both `SidebarContent` mounts share one unauthenticated GET. Add `useBranding()` alongside.

## Rendering rules (from the approved mock)

- **Custom logos always render via `<img>`** (never inlined into the DOM): `<img src="/api/branding/logo/{slot}">` for uploads, `<img src="/brand-default.svg">` for the preset fallback. This makes an uploaded SVG passive (scripts do not run in `<img>` context); with CSP `object-src 'none'` this is the XSS control (verified no `dangerouslySetInnerHTML`/`innerHTML` in `web/src`). The default uzi `FactoryIcon` stays an inline React component (trusted).
- **App mark, custom**: render the logo inside the existing 38px rounded-square frame (neutral bg for custom); `keep_name=false` hides the name/subtitle span on **all four** surfaces above.
- **POWERED BY, below-wordmark**: a row under the header with a faint uppercase `POWERED BY` label + text or logo. **top-right**: logo-only, ~96px max, no label, shares the header row.
- **Dim**: the POWERED BY logo renders at **opacity 0.8** (a CSS constant, not a setting). Logo heights ≈ 24px (below), 26px (top-right / app-square).
- **`brand_plaque=true`**: a light rounded plaque behind the POWERED BY logo (arbitrary value `bg-[#f6f6f8]` or an added token — must be a configured Tailwind token or arbitrary value so `web/scripts/check-styles.mjs` passes).
- **Credit line (D3, durable everywhere)**: render `MIT © Vlad Mocanu` (mono, ~10px, faint) **independently of the build-info fetch** so it shows during load and on fetch failure, on **both** the signed-in sidebar footer (on the version row at `AppShell.tsx:683-690`: refactor the trigger from `block w-full text-left` into a `flex items-center justify-between` row — version label left keeps the popover, credit span right) **and** the signed-out `PublicShell` (`AppShell.tsx:698-708`, which has no version row — add the credit to its footer). Hidden only when the sidebar is `collapsed`. The text is a build-time constant, not a setting.

## Shipped asset — `web/public/brand-default.svg`

`web/public/` is served at root (no `src/assets` import pattern exists); reference as `/brand-default.svg` (like `/favicon.svg`). Create it with exactly this content (the extracted, dimension-tightened Metaminds "M" — red `#ff3237`, wordmark dropped):

```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="1770 1873 712 506" role="img" aria-label="Metaminds">
  <path fill="#ff3237" d="M1978.41219,1877.95282l-205.47499,496.061v.00198h76.70398l205.47601-496.06299h-76.705ZM2090.55018,2374.01581h70.866v-496.06299h-70.866v496.06299ZM2196.8502,1877.95282l205.47498,496.06299h76.70599l-205.47699-496.06299h-76.70398Z"/>
</svg>
```

Ship it named as a neutral preset (`brand-default.svg`) so a fork can swap it; it happens to be the Metaminds M.

## Milestones

Dependency note: **M2, M3, M4 all depend on M1** (the keys, the `branding_assets` table + store methods, and the public endpoints). A worker must land M1 first.

- [x] **M1 — Backend: settings keys, asset table, public + admin endpoints.**
  - Add the 6 config keys to `settings.go` (`Key*`, `Defaults`, `Validate()` cases per the table; `brand_company` via a dedicated `termsafe`-based validator allowing `""`/commas).
  - Add the `branding_assets` migration + store methods (`Get/Upsert/DeleteBrandingAsset`).
  - Add `GET /api/branding` (unauthenticated, **allowlisted** struct — the six config keys coerced to typed JSON + the two derived `*_present` bools; no other settings key present).
  - Add `GET /api/branding/logo/{slot}` (unauthenticated image + ETag + Cache-Control).
  - Add admin `PUT`/`DELETE /api/admin/branding/logo/{slot}` (type ∈ {png,webp,svg+xml}, size ≤ 262144 bytes).
  - Add the three route-limiter rows + count bump.
  - **Go tests**: validator table (enums/bools ok; `brand_company` accepts `""`/"Acme, Inc.", rejects >64 runes and control/Cf runes); upload accepts png/webp/svg at/under cap, rejects other types and `cap+1`; `GET /api/branding` on a fresh DB returns the default JSON above **and contains only the branding keys** (assert no `public_base_url`/other settings key leaks — the R1 guard); logo GET returns bytes+ETag after upload, 404 before; a config-key set via `PUT /admin/settings` reflects on the next `GET /api/branding`.
  - *Success*: `task gate:api` green; the anonymous `GET /api/branding` exposes only branding fields; logos upload/serve/delete; oversized/invalid rejected with clear errors.

- [x] **M2 — Admin Branding tab.** New tab `{ to: "/admin/branding", label: "Branding" }` appended after Instance in `AdminShell.tsx:12-18`; route `{ path: "/admin/branding", element: <AdminBranding/>, guard: "admin" }` in `App.tsx:126-131`; page `web/src/pages/AdminBranding.tsx` shaped like `AdminSettings.tsx:221` (hooks + `getSettings`/`updateSettings` + the new logo upload/delete calls, wrapped in `<AdminShell>`). Controls: app-logo mode + upload/delete + keep-name; POWERED BY mode + company text + upload/delete + placement + plaque; a **live preview** of the sidebar chunk. Client size-check ≤ 262144 bytes before upload. **Web tests**: renders each mode; saving config issues the expected `updateSettings` payload; uploading calls the logo endpoint; over-cap is blocked client-side with a clear message.
  - *Success*: an admin can, from `/admin/branding`, set modes, upload/pick/clear logos, edit company text, save, and see the preview update; reload shows persisted state.

- [x] **M3a — Chrome: app mark + credit (all surfaces).** Add `useBranding()` (module-memoized). Render the app mark across all four surfaces (desktop aside, mobile drawer, `PublicShell`, mobile signed-in bar — reconciled), honoring `app_logo_mode`/`keep_name` and the collapsed state; custom logos via `<img>`. Render the durable `MIT © Vlad Mocanu` credit on the signed-in sidebar version row **and** the signed-out `PublicShell` footer, independent of the build fetch. **Web tests** (assert PROPERTIES on named channels, not snapshots): give the brand `<img>` a stable handle (`alt="brand logo"`/`data-testid`); the custom/white-label tests assert that exact query **finds** the `<img>` and reads `img.getAttribute("src")` (never `textContent`); the default test asserts the **same** query returns null AND that `FactoryIcon` + the literals render (target the HTML `<img>`, not `role="img"`, to avoid the inline-SVG collision). **Mode × surface matrix**: white-label (`keep_name=false`) asserts the name is absent on **each** of the four surfaces (the mobile drawer requires opening it to co-mount — see `AppShell.buildinfo.test.tsx:83`). **Adversarial**: full white-label settings → the `MIT © Vlad Mocanu` credit still renders (D3's whole point). Collapsed → credit hidden.
  - *Success*: defaults render `FactoryIcon` + literals with no brand `<img>` on any surface; custom/white-label swaps every surface; the credit shows signed-in and signed-out (and during load), hidden only when collapsed.

- [x] **M3b — Chrome: POWERED BY block + preset asset.** Ship `web/public/brand-default.svg`. Render the POWERED BY block: text mode (`POWERED BY <company>`), logo mode with **preset fallback** (no asset → `/brand-default.svg`) and **uploaded** (`/api/branding/logo/brand`), placement below vs top-right, plaque on/off, 0.8 dim. **Web tests**: text renders the company string (control/Cf already rejected server-side); logo mode with no asset shows the preset `<img src="/brand-default.svg">`, with an asset shows `<img src="/api/branding/logo/brand">`; **uploaded SVG renders as `<img>` and does NOT inject `<svg>` markup** (the XSS control, asserted positively); placement below shows the label, top-right hides it; plaque present/absent paired assertion.
  - *Success*: every POWERED BY state renders per the mock; the preset fallback works with zero uploads; uploaded logos never inline.

- [x] **M4 — No-regression + full gate.** `task gate:api` and `task gate:web` green (format, lint incl. ratchet, dead-code, typecheck, tests). Verify the **default** app-mark path is unchanged (FactoryIcon + literals, only gated) — **except** the intended mobile-top-bar reconcile (`uzi · dark factory` → the branded source) and the version-row credit span, both called out as intended diffs, not regressions. Confirm the existing version-row tests still pass and update them for the refactor: `web/src/components/AppShell.buildinfo.test.tsx` (asserts the version trigger button name + `aria-describedby` + single fetch for two mounts) and `BuildInfoPopover.test.tsx`.
  - *Success*: both gates exit clean; the named existing tests pass (updated for the credit-row refactor); the two intended default diffs are documented in the PR.

- [x] **M5 — Docs + CLI parity.** New user-facing `docs/branding.md` (frontmatter `title:`, `audience: user`, unique `order:` — matches `web/scripts/check-docs.mjs`) covering brand/white-label/unbrand and that the credit is fixed. Note the feature in `ARCHITECTURE.md`. Update `CHANGELOG.md` `[Unreleased]`. **CLI parity: no `api/cmd/uzi/` change is required — branding is configured entirely through the admin UI (`PUT /admin/settings` plus the logo upload/delete routes), which the CLI does not mirror.**
  - *Success*: `check-docs`/`check-changelog` pass; CHANGELOG updated; CLI-parity conclusion recorded.

## Testing strategy

- **Go** (M1): table-driven validators; handler tests for the three endpoints incl. the **R1 allowlist guard** (public JSON has only branding keys) and the size-cap boundary (262144 ok, +1 rejected). Live-DB store tests for `branding_assets` follow the repo's settings test patterns; `-race`/`-count=1` come from `task gate:api`.
- **Web** (M2/M3a/M3b): vitest, property assertions on **named channels** (brand `<img>` handle + `src`), the mode × surface matrix, the paired negative (default: same query returns null), the durable-credit adversarial (survives white-label), and the SVG-not-inlined positive. No pixel snapshots (the repo's Testing strategy forbids them).
- **Manual/visual**: the approved mock is the visual spec; the M2 live preview is the in-app equivalent.

## Risks & mitigations

- **R1 — public endpoint leaking the settings surface** → build an explicit allowlisted struct (never `Cache.All`/`AdminView`); M1 test asserts only branding keys present. *(Resolved in design.)*
- **R2 — unauthenticated large body / DoS amplification** → logos are **not** in the JSON; they're served from cacheable image routes (ETag + Cache-Control → cheap 304s). The JSON is small. *(Resolved in design.)*
- **R3 — blobs bloating the hot settings cache** → logos live in `branding_assets`, read only by the image routes; the settings cache never loads them (D7). *(Resolved in design.)*
- **R4 — 1 MiB PUT body cap** → logo bytes go through dedicated upload endpoints, one slot per request, not `PUT /admin/settings`. *(Resolved in design.)*
- **Stored-XSS via uploaded SVG** → always `<img>` (passive), type allowlist + size cap in the upload handler, CSP `object-src 'none'`; never inlined (verified no `dangerouslySetInnerHTML` in `web/src`). M3b asserts non-inlining positively.
- **Control/RTL runes in `brand_company`** → `termsafe.Validate` in the dedicated validator (admin text shown to all incl. signed-out).
- **CSP** → no change: `img-src 'self' data:` + `object-src 'none'` already present at `web/nginx.conf:20`, `web/nginx.mock.conf:19`, `deploy/chart/templates/web-configmap.yaml:29` (D5).
- **White-label misses a surface** → M3a's matrix asserts name-absence on each of the four surfaces.
- **Fork inherits Metaminds mark** → default unbranded; the M is only a preset a fork can replace.

## Dependencies

Internal only: `app_settings` KV + `settings` package; a new `branding_assets` table + store methods; the `Version`-style public-endpoint pattern; the `AdminShell`/route pattern; `termsafe`. Interacts with `httpx.maxBodyBytes` (1 MiB) — avoided by dedicated upload endpoints (R4). One goose migration (assign next free number at landing). No new third-party dep, no CSP/chart change, no `.github/workflows/**` change.

## Out of scope (v1)

Browser-tab `<title>` and favicon white-label (favicon has the PRD #70 status overlay); per-instance accent color/theme; making the credit admin-editable (deliberately fixed); a CLI branding command (unless M5's parity check finds a need).

## Decision log

- **D1** — Own **Branding** admin tab (after Instance), not a card in the Instance page. *(User.)*
- **D2** — Two independent surfaces: app mark (`app_logo_*`) and POWERED BY brand (`brand_*`). Co-brand and full white-label both expressible.
- **D3** — Credit `MIT © Vlad Mocanu` is a **build-time constant, rendered durably** on the signed-in sidebar **and** signed-out shell, independent of the build fetch (hidden only when collapsed) — so a rebrand cannot strip license/author attribution. Format follows the common OSS `MIT © <Author>` idiom (matches this repo's `README.md` `[MIT](LICENSE) © 2026 Vlad Mocanu`); `©` used, not `·`. *(User: durable-everywhere + `MIT © Vlad Mocanu`.)*
- **D4** — Default is **unbranded** (`app_logo_mode=default`, `brand_mode=none`); the Metaminds "M" ships as `web/public/brand-default.svg`, enable-with-one-click (mode set, no asset ⇒ preset fallback). *(User.)*
- **D5** — **SVG allowed** (raster too), rendered via `<img>` + type/size validation; no CSP change needed. Logo dimmed to opacity 0.8; optional light plaque for dark-ink uploads. *(User: "we will allow svgs also".)*
- **D6** — Placement default **below wordmark**; top-right kept as a tight logo-only option. *(User: "i like that POWERED BY under title".)*
- **D7** — Logo **bytes live in a dedicated `branding_assets` table** and are served from cacheable image routes; the `/api/branding` JSON returns only config + presence flags. Chosen over inlining base64 in `app_settings`/the JSON to resolve R1–R4 cleanly (keeps the anonymous body small, the hot settings cache clean, and the write off the 1 MiB PUT cap). Costs one migration. *(User: "lock in your recommendations" → reviewer-recommended Option B.)*
