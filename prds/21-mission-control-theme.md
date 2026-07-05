# PRD #21: Mission-Control Theme — data-theme Port of the Ops-Console Identity

**GitLab Issue**: [vtmocanu/uzi#21](https://gitlab.example.com/vtmocanu/uzi/-/issues/21)
**Status**: Ready (2026-07-05: reviewed by 2 agents — adversarial fact-check + design review; all findings incorporated: ember slot inventory corrected, expectations reframed to what tokens deliver, queue-tone slot added per user decision, minimal branch named, M0 preservation split out, SC2 made satisfiable)
**Priority**: Medium
**Created**: 2026-07-05
**Depends on**: PRD #14 (done, merged `2efd83b`) — the ember token system this theme layers onto.

## Problem

PRD #14 shipped the ember design as the sole theme, dark-only, with all look-and-feel in CSS variables precisely so alternative identities could ship later as `data-theme` overrides. The mission-control ops-console identity — near-black ink, faint blueprint grid, six-tone status language, monospace-for-data accents — was one of the three evaluated prototypes and the user wants it selectable. Today it exists only as a **local-only branch**, one clone-loss away from disappearing, and uzi has no theme-switching mechanism at all: `web/src/index.css:13-14` pins `:root, [data-theme="ember"]` and nothing anywhere sets, switches, or persists the attribute (verified — no `data-theme`/`localStorage` handling outside the CSS).

## What this port delivers (expectations, verified against both codebases)

Tokens carry mission's **palette** (near-black ink chrome, neutral ramp), **status colors**, **font family**, **radius sharpening** (main derives all radii from one `--radius`, so mission's 0.25rem just works), **blueprint-grid background** (a background-image token on the existing `body` rule), and mono styling wherever main already applies `font-mono`. Rendered markdown re-themes for free (the prose block is token-backed).

Mission signatures that do **NOT** port (component-structure, not tokens — deliberately out of scope): the mono/uppercase `.kicker` section labels (no consumer in main), per-control padding density (main's spacing is hardcoded; only radius is tokenized), the gate-pulse box-shadow ring (a Tailwind keyframe + component class in the prototype, not a token; the ordinary opacity pulse on status dots re-themes fine since it rides `bg-current`), and the six per-agent lane hues (main's run stream has no lane coloring). Net: **mission's palette/type/radius/grid over ember's structure** — not a pixel-for-pixel prototype recreation.

## Source material

- **Mission prototype**: local branch `worktree-agent-ad57212a539147704` @ `4eec6dc` ("extract visual identity into design tokens"): CSS custom properties on `:root[data-theme="mission"]` in its `index.css` (`--uzi-*` naming: chrome palette, neutral ramp, status tones emerald/cyan/amber/rose/violet/slate + a seventh sky info tone, grid, font stacks, radii, density), `tailwind.config.js` reading variables only (plus the gate-pulse keyframe, which stays behind). Its `ux-review.md` documents the rationale.
- **Minimal prototype** (preserved alongside, not ported here): local branch `worktree-agent-a232851b9b5030e53` @ `1c5db2a` ("minimal Linear/Notion-class redesign"). Its `theme.ts` is a working data-theme stamper — the starting point for this PRD's theme module (note: that branch is light-default with a `[data-theme="dark"]` convention; selectors get renamed when/if that theme is ever ported).
- **Translation, not copy-paste**: the prototype's token schema was designed for the prototype's component tree, which is not what merged. The port maps mission's *values* onto main's ember slots — actual inventory in `web/src/index.css`: palette (`--bg/--raised/--edge/--fg/--muted/--faint/--brand/--ring`, Tailwind names `ink/surface/edge/...`, already semantic, not ember-specific) and exactly **four status tones** (`--ok/--warn/--danger/--info`). There is **no neutral status slot and no queue slot** — see Decision 3.

## Design Decisions

1. **Theme = tokens (plus one theme-agnostic tone slot, Decision 3).** The mission theme changes CSS variable values and nothing else per-theme. Mission's *structural* ideas — global status bar, Operate/Resources/Config nav grouping, telemetry strip, kicker labels, control density — stay out (a separate features PRD if ever wanted). **No component file may branch on the active theme** (no `if (theme === "mission")` anywhere) — that is the load-bearing rule; the surfaces a theme may touch are enumerated in Success Criterion 2.
2. **Theme infrastructure is the real feature.** Add a theme module (start from the minimal branch's `theme.ts`): typed registry (`ember` default, `mission`), `data-theme` on `<html>`, persisted in `localStorage` under `uzi.theme` (per-browser; theme deliberately does not follow the user across devices — same stance as the multica inspiration), value validated against the registry with fallback to `ember` on unknown/corrupt values, storage access wrapped in try/catch (private-mode safe), applied **pre-paint** via an inline `<head>` script in `web/index.html` (shared by real and mock builds — `Dockerfile.mock` copies `web/` wholesale, verified) and applied **live** on picker change (`documentElement.dataset.theme`), optional `storage`-event cross-tab sync. Picker lives in **Settings → Account & token** under an "Appearance" section header with a "saved on this device" helper line.
3. **Status tones: four exist, two get added — theme-agnostically** (user decision 2026-07-05). Ember today has `ok/warn/danger/info`; queued/stopped render via palette neutrals hardcoded in `runBadge.ts`/`ui.tsx`, which token values cannot reach. To carry mission's signature violet queue without per-theme component logic, add a **`queue`** status tone (and a **`neutral`** status slot for idle/stopped) consumed by the existing tone maps: `runBadge`/`RUN_STATUS_TONES` map `queued → "queue"` once, for all themes; **ember populates `--queue` with its current neutral gray (zero visual change)**, mission populates it violet. Mission's cyan-run and sky-info both flow through `--info` per theme. The `runBadge` tests update for the tone-name change only (same discipline as PRD #14's tone unification).
4. **Theme-scoped extras live in the token file.** The blueprint grid ships as background tokens consumed by the existing `body` rule (default themes set it to none); any `[data-theme="mission"]` CSS stays inside `index.css` next to the token blocks. Fix the default-selector brittleness while in there: the base block becomes `:root:not([data-theme]), [data-theme="ember"]` so overrides don't depend on source order. PRD #14's grep gate (zero raw palette classes outside `index.css`) must stay green.
5. **Contrast bar.** Every mission token pairing used for text meets the same readability bar ember set (web-ux validates in-browser); no known contrast regressions ship just because the prototype had them.
6. **Mock/demo parity.** The `VITE_UZI_MOCK=1` demo build gets the switcher for free (shared `index.html`/bootstrap) and is the primary preview vehicle for review.

## Out of Scope

- The minimal/Linear theme (same mechanism later; M0 preserves its branch).
- Light mode; per-account (server-side) theme preference; OS `prefers-color-scheme` detection.
- Mission layout/structure features (status bar, nav regrouping, telemetry strip, kicker labels, control density, agent-lane hues, gate-pulse ring).
- Backend/API changes of any kind.

## Milestones

- [ ] **M0 — Preservation** (prerequisite, independent): push `worktree-agent-ad57212a539147704` → `origin/prototype/mission-control` and `worktree-agent-a232851b9b5030e53` → `origin/prototype/minimal`. Two commands; do first — the branches are irreplaceable.
- [ ] **M1 — Theme infrastructure**: theme module per Decision 2 (registry, persistence, validation, pre-paint + live application), Settings Appearance picker; default `ember` with zero visual change when unswitched; smoke tests for persistence, validation fallback, and attribute application.
- [ ] **M2 — Tone slots + mission token block**: `queue`/`neutral` status slots added theme-agnostically (Decision 3, ember values = current grays, runBadge/StatusPill maps + tests updated once); mission values translated onto ember slots in `index.css` under `[data-theme="mission"]` (+ grid tokens); default-selector fix; grep gate zero-hit; typecheck + full vitest + real & mock builds green.
- [ ] **M3 — Validation**: web-ux browser pass over the golden flows in BOTH themes (live switching, persistence across reload, contrast spot-checks, no unthemed surfaces — the tell is any element identical in both themes when it shouldn't be, or vice versa); tests green.
- [ ] **M4 — Docs + review gate + merge**: `docs/theming.md` (frontmatter-compliant, check-docs-covered: how themes work, how to add one, what a theme may touch) + Settings doc mention; reviewer + fact-check pass; MR to `main`; specs synced (`human.md` addition gated on user confirmation); issue #21 closed.

## Success Criteria

1. Settings shows an Appearance picker; selecting **Mission control** restyles the app instantly and survives reload; default remains ember with zero visual change for existing users (including the `--queue`=gray no-op).
2. Theme-related changes touch ONLY: `web/src/index.css`, the theme module, `web/index.html` (pre-paint script), `web/src/main.tsx` (module import), `tailwind.config.js` (new slot names), the Settings picker, the one-time theme-agnostic tone-map change (`runBadge.ts`/`ui.tsx` + their tests), and docs. **No component file branches on the active theme.**
3. Both themes pass the web-ux browser validation (golden flows, contrast bar, no unthemed surfaces); mission renders its six-tone status language (violet queue included).
4. `origin/prototype/mission-control` and `origin/prototype/minimal` exist.
5. Adding a third theme that stays within the existing slot set requires only one registry entry + one CSS block (documented in `docs/theming.md`); themes needing new slots follow the Decision 3 pattern (theme-agnostic slot addition first).
