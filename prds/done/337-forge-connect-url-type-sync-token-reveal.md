# PRD #337: Forge connect — base-URL ⇄ forge-type sync + token reveal toggle

**Issue**: [#337](https://github.com/vtmocanu/uzi/-/issues/337)
**Priority**: Medium (a UX papercut on the connect form; no correctness or security defect)
**Status**: Done (shipped on branch agent/issue-337)
**Anchor**: code references below are against `main` @ `dd95ecef`.

## Problem

Two small friction points on **Settings → Forge → "Connect a bot PAT"** (`web/src/pages/ForgeSettings.tsx`):

1. **Base URL and forge type drift apart.** The "Forge base URL" `<Select>` (`:268-276`) and the "Forge type" `<Select>` (`:281-291`, shown only when the API advertises more than one type) are fully independent choices. A user can pick `https://github.com` while the type still reads GitLab; the mismatch is only surfaced later, when `VerifyToken` rejects the token. The code says so deliberately: *"Base URL and type are independent choices (D11a): a mismatch is caught by VerifyToken"* (`:277-280`). That is safe but not helpful — the form knows the host and could keep the pair consistent.

2. **The PAT field is always masked with no reveal.** The token `<Input type="password">` (`:292-300`) has no way to un-mask, so a user cannot eyeball a pasted token for a bad copy/paste before hitting Connect. Since the token is never shown again after saving (stored encrypted), the connect form is the *only* moment they can verify it, and right now they can't.

## Current behavior (references)

- `ForgeSettings.tsx`
  - Base-URL select over the allowlist `allowedUrls` (`api.forgeConfig().allowed_base_urls`): `:268-276`.
  - Forge-type select over `forgeTypes` (`api.forgeConfig().forge_types`), rendered only when `forgeTypes.length > 1`: `:281-291`. Defaults to `forgeTypes[0]` / `"gitlab"` (`:112`).
  - Token input, `type="password"`, no reveal: `:292-300`.
  - `connectHints(forgeType)` already makes the scope label, placeholder, and role clause follow the selected type (`:45-74`) — so a type change already has a visible effect; sync just makes the type change happen for you.
- `web/src/components/ui.tsx`: `Input` (`:159-164`), `Field` (`:119-152`) — the primitives to build on.
- `web/src/components/icons.tsx`: no eye/eye-off glyph yet — needs adding.
- `web/src/lib/forgeNoun.ts`: the existing single per-forge mapping site (PRD #65 D2). Its acceptance test pins "exactly one literal of the GitLab noun", so the new host-inference logic lives in a **sibling** module, not here, to avoid muddying that invariant.
- Fixtures: `web/src/mocks/data.ts` already has `mockForgeConfigAllForges` (`:885-897`) advertising `["gitlab","forgejo","github"]` with `https://github.com` in the allowlist — the ready-made multi-forge test config.

## Solution (Decision)

Two independent, frontend-only changes. No API, DB, or worker change; `VerifyToken` stays exactly as the backstop it is today.

### Feature A — two-way base-URL ⇄ forge-type sync

Infer the forge type from the base URL's **host**, and keep the two selects consistent, in both directions, **only when the host is recognized**:

- **URL → type.** On a base-URL change, infer the type from the host. If the inferred type is recognized *and* is advertised in `forgeTypes` *and* differs from the current type, switch the type to it. If the host is unrecognized, leave the type on the user's manual pick.
- **type → URL.** On a forge-type change, only move the base URL when the *current* URL's host is a **recognized** forge of a **different** type. Then look up `defaultUrlForType(chosenType, allowedUrls)`:
  - if it returns a URL (an allowlist entry whose inferred type matches), switch to it;
  - **if it returns `null`** (the chosen type is advertised but the allowlist has no *recognized* URL for it — e.g. the only Forgejo host in the list is a self-hosted `git.example.com` that infers to `null`), **keep the current URL and change nothing.** The pair may then be a recognized-and-mismatched pair; that is the exact situation `VerifyToken` still backstops, and it is strictly better than moving the URL to a wrong or absent target.
  - If the current host is unrecognized/self-hosted, keep it (the user is annotating what that host runs).

**Recognition rule** (`inferForgeType`): parse the host (`new URL(x).hostname`, already lowercased, so ports and case are handled); case-insensitive substring match — host contains `github` → `github`, `gitlab` → `gitlab`, `forgejo` → `forgejo`; else a small known-alias map matched by host equality/suffix (`codeberg.org` and `*.codeberg.org` → `forgejo`) for canonical public hosts whose name does not advertise the software; else `null` ("unrecognized → user chooses"). The helper returns a type **only if it is in the advertised `forgeTypes`**, so it can never select a forge the instance did not enable (e.g. a `github.com` allowlist entry on a gitlab-only instance is left alone). Substring matching accepts a small, deliberate false-positive risk (a self-hosted host like `git.gitlab-migration.example.com` misclassifies) — gated by the advertised-`forgeTypes` check and `VerifyToken`, so blast radius is a wrong pre-fill the user can override, never a broken connect. See D3 for the alias-map scope and D4 for why `null` is the safe default.

Sync fires on **user change events only, not on mount** — the landing pair stays exactly as today (D5). The auto-changed field gets a brief visual highlight so the coupling is legible (mirrors the mock).

This is the behavior visualized and approved in the pre-PRD mock (forge-connect prototype): recognized hosts (`github.com`, `gitlab.com`, `forgejo.example.com`) sync both ways; an unrecognized host (`git.example.com`, and note the existing `forge.example.com` fixture entry) stays under full manual control.

### Feature B — reveal (eye) toggle on the PAT input

Build a **reusable** reveal-capable secret input rather than a one-off in ForgeSettings, because `type="password"` appears in **7 components** today (`ForgeSettings`, `Login`, `Register`, `AdminSettings`, `AnthropicTokens` ×2, `VaultControls` ×3) and they should converge on one accessible affordance:

- Add `EyeIcon` / `EyeOffIcon` to `icons.tsx` (lucide-style, matching the existing stroke set).
- Add a `PasswordInput` primitive to `ui.tsx`: the existing `Input` in a relative wrapper with a trailing icon-button that toggles `type` between `password` and `text`. Defaults to **masked**. The button is `type="button"` (never submits the form), carries a toggling `aria-label` ("Show token" / "Hide token"), `aria-pressed`, and a visible focus ring. Takes an `id` and forwards it to the inner input so the field's label associates with the input, not the composite (see the `Field` note below).
- **Re-mask on external clear.** Reveal state is internal `useState`, so an empty `value` prop does not flip it back on its own; a revealed field would otherwise show the *next* pasted token in clear. Add a `useEffect` that resets reveal→masked when `value` becomes empty, so clearing the token after a successful connect (`setToken("")`) re-masks it.
- **`Field` association.** The forge PAT `Field` currently passes no `htmlFor` (`ForgeSettings.tsx:292`), so `Field` wraps a `<label>` around its child — and `Field`'s own doc (`ui.tsx:128-132`) warns that wrapping a composite child (here: input **+** toggle `<button>`) pollutes the accessible name / puts two controls under one label. Switch the PAT field to the `htmlFor`/`id` form of `Field`, with `PasswordInput` carrying the matching `id`.
- Apply it to the forge PAT field (`:292-300`) — the requested surface.

**Security note (for the reviewer):** this reveals only what the user is *currently typing* into the connect form. It does **not** touch the secretbox no-reveal invariant, which is about *stored* tokens (there are no reveal endpoints, and stored PATs are never returned). Nothing here reads or exposes a persisted secret.

### Scope boundaries

- **In scope:** the two behaviors above, applied to the forge connect form; the reusable `PasswordInput`; the pure inference helpers; tests; the doc/spec updates.
- **Out of scope (follow-up):** rolling `PasswordInput` out to the other six `type="password"` sites (its own small PRD once this lands), and inferring type from a *free-text* URL field (base URL is a fixed allowlist `<Select>` today; if it ever becomes free text, `inferForgeType` already takes an arbitrary host).

## Milestones

Sequential; single component (`web`). Four milestones, each behavioral one landing with its own tests so it is validatable at its boundary — count reflects the change's size, not padding.

- [x] **M1 — Host→type inference helpers (pure, unit-tested).** New `web/src/lib/forgeInfer.ts`:
  - `inferForgeType(baseUrl: string, forgeTypes: string[]): string | null` — parse host, substring/alias match, return the type only if advertised, else `null`.
  - `defaultUrlForType(forgeType: string, allowedUrls: string[]): string | null` — first allowlist URL whose inferred type equals `forgeType`, else `null`.
  - `web/src/lib/forgeInfer.test.ts`: cover github/gitlab/forgejo recognition, a `gitlab.example.com` subdomain, a host with a port, the `codeberg.org`/`*.codeberg.org` alias, the unrecognized host → `null` case, the "inferred type not advertised → `null`" guard, `defaultUrlForType` returning `null` when no recognized URL matches an advertised type, and a malformed URL (no throw). These pure tests are the primary regression guard for the rule and must fail against a stub that ignores the host.
- [x] **M2 — Wire two-way sync into the connect form + tests + doc/decision.** In `ForgeSettings.tsx`, extend the base-URL and forge-type `onChange` handlers per Feature A (URL→type; type→URL only when the current host is recognized-and-different **and** `defaultUrlForType` is non-null, else keep the URL; unrecognized-host opt-out; advertised-type guard). The auto-changed field gets a **CSS-only** highlight (a class toggle re-triggering a keyframe via `animationend`/reflow, as in the mock — no `setTimeout`, so no unmount/`act()` cleanup hazard); it is cosmetic and not behaviorally asserted. **Fix the now-stale comment** at `:277-280` (repo fix-the-doc rule): reword from "independent choices, reconciled by VerifyToken" to describe the sync, noting VerifyToken remains the backstop for a recognized-mismatched or self-hosted pair. **Add a generic multi-forge fixture** to `web/src/mocks/data.ts` using `example.com`-style hosts (`https://github.com`, `https://gitlab.example.com`, `https://forgejo.example.com`, `https://git.example.com`) so tests assert against no new `*.example.com` literal (sanitization). Tests in `ForgeSettings.test.tsx`: selecting the github.com URL switches type→GitHub (and `connectHints` copy/placeholder follows); selecting the GitHub type moves the URL→github.com; selecting a recognized forgejo URL and the forgejo type round-trips; the **null-target** case (advertise a type whose only allowlist URL is the unrecognized `git.example.com` → changing to that type keeps the current URL); selecting `git.example.com` leaves the type manual, and then changing the type does **not** move that URL. Record the Decision Log entries below in this PRD.
- [x] **M3 — Reveal-token primitive + tests + applied.** Add `EyeIcon`/`EyeOffIcon` (`icons.tsx`) and `PasswordInput` (`ui.tsx`) per Feature B (masked default; `type="button"` toggle; toggling `aria-label`/`aria-pressed`; visible focus ring; `useEffect` re-mask on empty `value`; forwards `id` to the inner input). Swap the forge PAT field to `PasswordInput` and its `Field` to the `htmlFor`/`id` form. Keep the rendered input `type="password"` on mount so existing `input[type="password"]` selectors are unaffected until toggled. Tests in `ForgeSettings.test.tsx`: the reveal button flips the input to `type="text"` and back with `aria-label`/`aria-pressed` toggling; the field stays masked (`type="password"`) on mount and after a `setToken("")` clear; confirm the existing password-selector tests (`:206`, `:227`, `:238`) still pass.
- [x] **M4 — Full gate + specs.** `task gate:web` green (lint, deadcode, typecheck, all tests). Record the sync rule and the reveal affordance in `specs/ai.md` (AI design decisions; auto-applied). No user-facing doc page changes (the bot-setup guides describe token scopes, not form chrome); confirm `web/scripts/check-docs.mjs` still passes via the build.

## Success criteria

1. On a multi-forge config, selecting a **recognized** base URL sets the matching forge type (and vice-versa), and the `connectHints` copy tracks the resulting type.
2. Selecting an **unrecognized/self-hosted** host never changes the forge type, and changing the type never overwrites an unrecognized URL — both directions stay manual there. When a chosen type has **no recognized allowlist URL** (`defaultUrlForType` → `null`), the current URL is kept rather than blanked or mis-set.
3. Inference never selects a forge type the instance did not advertise in `forgeTypes`.
4. The forge PAT field can be revealed and re-masked via an accessible eye button; it starts masked and never submits the form when clicked.
5. No change to what gets sent to the API: `createConnection(baseUrl, token, forgeType)` still carries the three values; `VerifyToken` remains the backstop for a genuinely wrong pair.
6. `task gate:web` green (lint, deadcode, typecheck, tests), and the existing single-type "picker hidden, sends gitlab" test is unaffected.

## Risks and mitigations

- **Changing established "independent choices" behavior.** Confirmed with the user (this PRD's origin) and de-risked in the browser mock before writing. The change only *proposes* a consistent pair; VerifyToken still rejects a wrong token, so no new failure mode is introduced. Reviewer must confirm the sync is change-only and the advertised-type guard holds.
- **Clobbering a self-hosted URL/type.** Explicitly designed against: an unrecognized host is left untouched in both directions (Success criterion 2, tested in M4).
- **Breaking existing password-input test selectors.** The input renders `type="password"` on mount, so `document.querySelector('input[type="password"]')` still resolves everywhere until a user toggles; M4 verifies.
- **Mistaking this for a secret-reveal endpoint.** It reveals only the in-progress form value, not a stored secret; called out for the reviewer so it is not flagged against the secretbox no-reveal invariant.
- **Revealed state leaking across tokens.** A `PasswordInput` that keeps reveal state internally would show the next pasted token in clear after a clear+re-type. Mitigated by the `useEffect` re-mask on empty `value` (M3), tested by the "masked after `setToken('')`" assertion.
- **Highlight cleanup / `act()` warnings.** The auto-change highlight is CSS-only (class toggle + keyframe), with no timer to leak on unmount, so it avoids the classic "update on unmounted component" test warning. It carries no behavioral assertion.
- **Substring false-positives.** A self-hosted host containing a rival forge's name is misclassified; accepted deliberately — gated by the advertised-`forgeTypes` check and VerifyToken, so the worst case is a wrong pre-fill the user overrides.
- **Sanitization (repo is going public).** New tests/fixtures use a generic fixture with `github.com` / `gitlab.example.com` / `forgejo.example.com` / `git.example.com`, adding no new `*.example.com` literal in test assertions.

## Testing strategy

- **Pure helpers (M1)** carry the load: `forgeInfer.test.ts` is the discriminating regression guard for the recognition rule and the advertised-type guard; it fails against a host-ignoring stub.
- **Integration (M2/M3)** in `ForgeSettings.test.tsx` drives the real selects via `fireEvent.change` on a new generic multi-forge fixture and asserts both sync directions, the null-target keep-URL case, the unrecognized-host opt-out, and the reveal toggle — the same interactions the mock demonstrated.
- No backend/live-DB test: this PRD changes only client-side form behavior; the wire contract is unchanged.

## Decision Log

- **D1 — Two-way sync, host-inferred, recognized-only.** Keep the pair consistent for recognized hosts in both directions; leave unrecognized hosts fully manual. Chosen over a one-way (URL→type only) sync because the user asked for both directions, and over always-forcing because that would clobber self-hosted instances whose hostname does not advertise the forge.
- **D2 — Inference lives in a new `web/src/lib/forgeInfer.ts`, not in `forgeNoun.ts`.** `forgeNoun.ts` has a strict "exactly one GitLab-noun literal" acceptance test (PRD #65 D2); adding host-matching there would muddy that invariant. A sibling pure module keeps both testable in isolation.
- **D3 — Recognition = substring match + a tiny alias map.** Core rule is the user's stated one: host contains `github`/`gitlab`/`forgejo`. Add only `codeberg.org` / `*.codeberg.org` → `forgejo` (the canonical public Forgejo whose host hides the software), matched by host equality/suffix so `git.codeberg.org` also resolves. The map is deliberately minimal and easy to extend; broader host intelligence is not worth it. Substring matching accepts a bounded false-positive (a self-hosted host carrying a rival's name), gated by D4's advertised-type check and VerifyToken. *(Open to dropping the alias map entirely if the reviewer prefers the pure substring rule — codeberg would then fall to "user chooses", which is still safe.)*
- **D4 — `null` (unrecognized) is the safe default, and inference is gated on `forgeTypes`.** Never guess a type for an unknown host, and never select a forge the instance did not advertise. Both directions treat `null` as "do nothing", which is what makes self-hosted hosts safe.
- **D8 — type→URL keeps the current URL when there is no recognized target.** If the chosen type is advertised but no allowlist URL infers to it (`defaultUrlForType` → `null`), do not blank or mis-set the URL — keep it and let VerifyToken backstop the possibly-mismatched pair. Moving the URL to a wrong or absent target would be worse than the mismatch the feature is trying to avoid, and this case is directly reachable (a Forgejo instance whose only host is a self-hosted `git.example.com`). Tested in M2.
- **D9 — The auto-change highlight is CSS-only and untested.** A class-toggle keyframe (no `setTimeout`) keeps the coupling legible without a timer to leak on unmount; it is cosmetic, so it carries no behavioral assertion and does not appear in the success criteria.
- **D5 — Sync fires on user change events only, not on mount.** The landing pair stays byte-identical to today (important for the single-type "sends gitlab" test and to avoid surprising the user at page load). Initial-load reconciliation is a deliberate non-goal.
- **D6 — Reveal is a reusable `PasswordInput`, applied only to the forge PAT now.** Building the primitive right (accessible, `type="button"`, toggling `aria-label`) costs the same as a one-off but lets the six other secret fields adopt it later. Rolling it out to them is out of scope here to keep this PRD focused.
- **D7 — Reveal is client-only and does not touch secretbox.** It un-masks the value the user is typing, not a stored secret; the no-reveal-endpoint invariant is untouched.
