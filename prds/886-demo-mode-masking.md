# PRD #886 — Demo mode: client-side masking of identifying values

**Issue**: [#886](https://github.com/vtmocanu/uzi/issues/886)
**Status**: Draft
**Priority**: Low
**Owner**: maintainer
**Scope**: `web/` only. No API, DB, migration, admin, or CLI changes.

---

## Problem

Demoing and screenshotting the running uzi web UI leaks the operator's real
identity and infrastructure: their email address, the repo owner/namespace, the
forge hostname, a CLI token's last-used IP, and the registration email-domain
allowlist. There is no way to hide these for clean, shareable screenshots without
either editing screenshots by hand or standing up a throwaway instance with fake
data.

## Solution

A **per-device** "Demo mode" toggle in the user's own settings that masks
identifying values **at the web render layer only**. When on, the current browser
shows deterministic fake values in place of the real ones; the real data in the
DB and API is untouched, and no other user is affected. Off by default.

This is a maintainer/demo convenience implemented entirely in the SPA — no server,
DB, admin, or CLI surface changes.

---

## Key design decisions (settled)

1. **Mask at the render layer, and ONLY at pure-display sites.** Masking is applied
   where a value is displayed as text, never by transforming DTOs as they arrive in
   `web/src/lib/api.ts`. Two hard rules follow, both load-bearing:
   - **Never mask a value that is also used as anything but display text** — a
     React `key`, an `href`/route, an `<input>`/`<select>` value, or the input to a
     dirty-check/submit/search/filter. Those must read the raw value. Masking one
     corrupts keys, links, forms, and search. (Confirmed hazards below: `repo_path`
     is a card key at `web/src/lib/api.ts:3931`; Slack `member_id`,
     `human_username`, `base_url`, and `public_base_url` are bound to editable
     inputs / dirty-checks; several repo paths feed forge `href`s.)
   - **Mask the display channel wherever it appears — including `title` and
     `aria-label` attributes, not just visible text.** Tooltips and a11y labels
     leak identity into screenshots and are the exact channel this repo's tests are
     blind to (see M5).

2. **Per-device, localStorage-backed.** State lives in `localStorage` under key
   `uzi_demo_mode` (mirrors the existing `uzi_mock_scenario` convention —
   `mockScenario()` in `web/src/mocks/mockApi.ts`). Not a per-user server
   preference and NOT an admin/global setting — the goal is clean screenshots on
   one machine, with zero backend work and zero impact on other users. Every
   `localStorage` access is wrapped in try/catch (private windows throw); a failed
   read means demo mode is off.

3. **Behavior-safe — verified.** Render-layer masking breaks no client logic
   *because it leaves every non-display use of a value raw.* Ownership ("is this
   mine") is a server-computed boolean keyed on **UUID**, not email:
   `IsMine: ownerID == viewerID` at `api/internal/handler/board.go:179`, surfaced as
   `is_mine` and read at `web/src/lib/runBadge.ts:580`. Email and repo path **are**
   used as client-side match keys in the shipped path —
   `web/src/pages/RunsList.tsx:490` (`r.owner_email !== user?.email`, the admin
   "factory runs" filter) and `web/src/lib/runGroups.ts:110`
   (`run.repo_path...includes(needle)`, repo search) — but both read the raw
   values, which render-layer masking preserves, so behavior is unaffected. The
   only `email ===` in an auth path is the mock backend
   (`web/src/mocks/mockApi.ts`). Consequence to accept, not fix: with demo mode on,
   **search/filter still operate on real values**, so typing a masked string
   ("demo/uzi") into repo search finds nothing. Demo mode is for screenshots, not
   for interactive filtering; state this in the docs.

4. **Deterministic, stable masks.** Same input maps to the same masked output
   everywhere and across screenshots, so a multi-screenshot demo stays coherent.
   No per-render randomness.

5. **Masked-value style.** People collapse to a **first name**
   (`vlad.mocanu@metaminds.com` → `Vlad`). Infrastructure uses
   **reserved/unmistakably-fake** values so a leaked one is obviously not real:
   `example.com`, `forge.example.com`, and a TEST-NET-3 IP (`203.0.113.x`).

6. **Not masked (deliberate):** issue/MR/PR **titles**, **branch names**, issue/MR
   **numbers** (`iid`/`mr_iid`), **worker names** (user-chosen labels), and the
   **brand/app name** (operator-set org name in header + `document.title` — the
   operator controls their own brand for a demo). **Slack `member_id`** is NOT
   masked: it is bound to an editable input and a dirty-check
   (`web/src/components/SlackNotifications.tsx` — value at `~:169`, dirty-check at
   `~:103`); masking it would corrupt the form (rule 1). Likewise
   `public_base_url` and the forge `allowed_base_urls` dropdown / connection input
   values are input-bound and stay raw.

7. **Avatars need nothing.** There are no user-avatar `<img>` elements; the only
   user glyph is an initials `<span>` in `AppShell` derived from name/email — so the
   initials MUST be recomputed from the **masked** name (`Vlad → V`), not the raw
   one. The `<img>` elements that do exist are the operator brand logo, deliberately
   unmasked (decision 6).

### Out of scope
- The browser **URL bar** (hostname + route) — the SPA cannot mask it; crop the
  screenshot or use a demo hostname.
- **`<a href>` hover** — decision 1 leaves hrefs raw (they are links), so hovering a
  card surfaces the real forge host+path in the browser status bar. Accepted
  consequence; don't hover during a screenshot.
- The **network tab / DevTools** — presentation-only masking; real values remain in
  API responses.
- Server-side / admin-global masking (possible future v2 for shared-audience
  demos; explicitly not this PRD).

---

## Masking rules (the registry)

Every mask function is the **identity function when demo mode is off**. Apply only
at pure-display sites (decision 1).

| Field(s) | Masked value |
|---|---|
| email / `owner_email` | first name from local-part, first token before `.`/`_`/`+`/`-`, capitalized (`vlad.mocanu@… → Vlad`) |
| `display_name` / `owner_name` / `author` | first whitespace token, capitalized (`Vlad Mocanu → Vlad`) |
| `repo_path` / `path_with_namespace` / `repo_name` / `path` | keep the last path segment (the repo), replace everything before it with `demo` (`vtmocanu/uzi → demo/uzi`; `group/sub/repo → demo/repo`) |
| forge `base_url` (display only) | replace host with `forge.example.com`, keep scheme (`https://gitlab.metaminds.com → https://forge.example.com`) |
| `human_username` (display row only) | `demo-user` |
| `bot_username` (display only) | `demo-bot` |
| `last_used_ip` | `203.0.113.7` (TEST-NET-3) |
| `allowed_email_domains` (joined display string only) | `example.com` |

**Register domains caveat:** in `web/src/pages/Register.tsx` the `domains` array is
both displayed (`domains.join(", ")` at `~:51` error string and `~:142` hint) and
passed to `emailDomainAllowed(email, domains)` (`~:50`). Mask the **joined display
string** at both display sites; never mask the `domains` array itself.

---

## Technical scope

1. **State + reactivity** — `web/src/lib/demoMode.ts`:
   - `isDemoMode(): boolean` — reads `localStorage["uzi_demo_mode"]`, try/catch,
     defaults false.
   - `setDemoMode(on)` — writes localStorage and **notifies local subscribers
     directly** (the `storage` event does NOT fire in the originating tab, so the
     toggling tab needs an explicit in-tab notify to re-render live).
   - `subscribeDemoMode(cb)` — registers local subscribers AND listens to the
     `storage` event for *other* tabs (cross-tab sync).
   - `useDemoMode(): boolean` — React hook via `useSyncExternalStore`; `getSnapshot`
     returns the plain boolean (stable primitive, no tearing) so consuming
     components re-render on toggle with no page reload.
   - Precedent: `web/src/lib/prefs.ts:8` explicitly skips `useSyncExternalStore`
     because "no two components watch one key" — the opposite of this case (many
     components, one key), so it is the correct tool here.

2. **Masking helpers** — `web/src/lib/demoMask.ts`: pure functions
   `maskEmail`, `maskName`, `maskRepoPath`, `maskHost`, `maskUsername(value, role)`,
   `maskIp`, `maskDomains`, each `(value, enabled) => string`, pure and testable.
   `enabled` comes from `useDemoMode()` at the call site.

3. **Toggle UI**:
   - Canonical toggle in `pages/Settings.tsx`. Note the theme control there is a
     **server-backed `<select>`** that "follows you across browsers"
     (`Settings.tsx:~100,~183`); place Demo mode with **deliberate visual
     separation** and the helper text "This device only. Masks emails, repo names,
     forge host, and other identifying info in what you see — for screenshots.
     Doesn't change your data or affect anyone else." so users don't assume it syncs.
   - Quick toggle in the user/avatar menu (`AppShell.tsx`) that also shows
     "Demo mode: On" as the state cue (a menu item, NOT a floating badge that would
     land in the screenshot).

4. **Apply masking at pure-display render sites** using `useDemoMode()` + the pure
   helpers, per the mandatory grep in M3/M4. Mask visible text AND `title`/
   `aria-label` attributes. Do not entangle masking with `stripUnsafeChars`.

---

## Resolved facts — render-site inventory (a STARTING list, not the whole set)

**⚠️ This inventory is known-incomplete and must not be trusted as exhaustive.**
An earlier draft's `repo_path` list was ~8 sites short and missed the attribute
channels. The reliable method is the **mandatory grep in M3/M4** — grep every
field name, mask every pure-display hit. The list below is orientation, not a
checklist. All paths under `web/src`; line numbers drift, match on field/component.

**User identity (email / `display_name` / `owner_email` / `author` / `owner_name`):**
`components/AppShell.tsx` — sidebar user cluster (initials + name + email) AND the
`title=` tooltip at `~:849` (attribute channel — easy to miss);
`pages/AdminUsers.tsx`, `pages/Settings.tsx`, `pages/CliAuth.tsx`,
`pages/Dashboard.tsx`, `pages/RunsList.tsx` (`owner_email`, incl. admin worker
rows), `pages/Notifications.tsx`, `pages/AdminBlockedRepos.tsx`,
`pages/AdminRateLimits.tsx`, `components/UsageCards.tsx`; authors at
`pages/IssueView.tsx` (`issue.author`), `pages/Board.tsx` (`card.author`), board
`owner_name`.

**Repo path / namespace (`repo_path` / `path_with_namespace` / `repo_name` /
`path`):** `pages/Board.tsx:~1043` (page `<h1>`), `components/AppShell.tsx:~742`
(sidebar repo-nav, always visible), `pages/Repos.tsx` (whole page + aria-labels),
`pages/RunView.tsx:~2876`, `components/RepoMultiSelect.tsx` (+aria-labels),
`components/OccurrenceFileIssue.tsx:~203`, `pages/AdminSettings.tsx:~2123`,
`pages/AdminBlockedRepos.tsx` (via field name **`path`**, not `repo_path` — a
by-field-name plan misses it unless the grep includes `\.path\b`),
`pages/Findings.tsx`, `pages/Dashboard.tsx`, `pages/RunsList.tsx`,
`pages/Schedules.tsx`, `components/DefaultJobs.tsx` (+aria-labels),
`components/ScheduleModal.tsx`, `components/ProposalCard.tsx`,
`components/RunRequestCard.tsx`, `components/Memory.tsx` (`repo_name`),
`components/ScheduleGroupRow.tsx`, `pages/Notifications.tsx`.

**Forge host / usernames (display sites only):** `pages/ForgeSettings.tsx`
(`c.base_url`, `c.human_username`, `c.bot_username` in the connection table — NOT
the editable draft inputs, and NOT the `allowed_base_urls` `<option>` dropdown at
`~:309`, which is input-bound), `pages/Repos.tsx` (`c.base_url`, `c.bot_username`
display).

**CLI last-used IP:** `components/CliTokens.tsx` (`last_used_ip`).

**Registration email-domain allowlist:** `pages/Register.tsx` (see caveat above).

**Explicitly NOT masked** (input-bound / non-display, per decision 1 & 6): Slack
`member_id`, `public_base_url`, forge `allowed_base_urls` dropdown, editable
connection draft inputs, brand `document.title`, forge `href`s, `iid`s. No
CSV/JSON export path exists (verified — nothing to mask there).

---

## Milestones

- [ ] **M1 — Demo-mode state + masking library.** `demoMode.ts`
  (localStorage/try-catch, same-tab local notify + cross-tab `storage` sync,
  `useDemoMode` via `useSyncExternalStore` returning a stable boolean) and
  `demoMask.ts` (all pure helpers per the registry), each deterministic and
  identity-when-off. Unit tests cover on/off for **every** helper (incl.
  multi-segment and subgroup repo paths, email with no `.` in local-part,
  single-word display name, empty/undefined pass-through). **Every export of both
  files must be consumed in this same MR** — the tests must import every
  `demoMode.ts` export too, not only the maskers — because `web/knip.jsonc` gates
  unused `exports`/`types` at **error**, so an export with no importer reddens
  `task gate:web`.
- [ ] **M2 — Toggle UI.** Settings toggle (visually separated from the server-backed
  theme control) + user-menu quick toggle with an "on" state cue. Toggling updates
  the UI **live in the same tab** (no reload), persists to localStorage, syncs to
  other tabs, and survives a refresh.
- [ ] **M3 — Mask user identity everywhere, grep-verified.** Mask all
  email/`display_name`/`owner_email`/`author`/`owner_name` sites INCLUDING the
  AppShell `title` tooltip and initials-from-masked-name. **Required step:**
  `git grep -nE 'owner_email|display_name|owner_name|\.email\b|\.author\b' -- 'web/src/**/*.tsx'`
  and confirm every pure-display hit routes through a masker (input/key/href sites
  stay raw). List the grep output in the MR.
- [ ] **M4 — Mask infra/forge fields, grep-verified.**
  `repo_path`/`path_with_namespace`/`repo_name`/`path`, forge `base_url`,
  `human_username`/`bot_username` (display rows), `last_used_ip`,
  `allowed_email_domains`. **Required step:**
  `git grep -nE 'path_with_namespace|repo_path|repo_name|\.path\b|base_url|human_username|bot_username|last_used_ip|allowed_email_domains' -- 'web/src/**/*.tsx'`
  and confirm every pure-display hit is masked and every input/select/href hit is
  left raw. List the grep output in the MR.
- [ ] **M5 — Tests green, attribute-aware.** Component tests proving a
  representative site in each channel — AppShell header **text**, the AppShell email
  **`title` tooltip**, a `repo_path` **`aria-label`**, ForgeSettings — shows the
  **real** value with demo off and the **masked** value with it on. Assertions MUST
  use `toHaveAttribute` / `getByTitle` / `getByLabelText` for attribute channels
  (`.claude/rules/web.md` documents that `textContent`/`queryByText` are blind to
  `title`/`aria-label` — a textContent-only test passes while the tooltip still
  leaks). For each, assert the **full real string is absent** with demo on (not
  merely that the mask is present — `maskEmail` and `maskName` both yield `Vlad`, so
  "Vlad present" doesn't prove which field masked). `task gate:web` passes. (Helper
  unit tests belong to M1; M5 owns component tests + gate.)
- [ ] **M6 — Docs.** A user-facing docs page (`docs/`, `audience: user`, valid
  frontmatter per `docs/README.md`) describing demo mode: what it masks, that it's
  per-device and screenshot-only, that **search/filter still use real values**, and
  the URL-bar / href-hover caveats.

## Testing / validation

- `task gate:web` (format, oxlint `-D correctness`, typecheck, knip dead-code,
  vitest) must pass. Keep every export consumed (knip error tier).
- Helper unit tests: assert both branches for each; cover the registry edge cases.
- Component tests: two-way (real-with-off, masked-with-on), attribute-aware per M5.
- Manual: toggle in the running SPA, confirm AppShell header + tooltip, RunsList
  attribution, Board h1, Repos page, ForgeSettings, CliTokens, and Register all
  mask live and un-mask on toggle-off, and that state survives a reload and syncs
  across two tabs.

## Risks & mitigations

- **A render site is missed → a real value leaks (highest-impact).** The static
  inventory is known-incomplete; the mitigation is the **mandatory field-name grep
  in M3/M4**, promoted to a required MR deliverable, not a manual afterthought.
- **Attribute-channel leak invisible to tests.** `title`/`aria-label` are blind to
  textContent assertions → M5 mandates `toHaveAttribute`/`getByTitle`.
- **Masking an input/key/href/search value → broken UI or form.** Decision 1 rule 1;
  the grep must leave those raw (member_id, public_base_url, draft inputs, hrefs).
- **knip ordering hazard.** Every export consumed in the same MR (M1).
- **Same-tab live toggle fails** if the design leans on `storage` (which doesn't
  fire in the originating tab). M1 mandates an explicit local notify.
- **`localStorage` throws** (private window) → try/catch, failure = off.

## Success criteria

1. A per-device toggle in user settings (and user menu) turns masking on/off live,
   persisted across reloads, synced across tabs, invisible to other users.
2. With demo mode on, none of these render their real value **anywhere, in text or
   in `title`/`aria-label`**: user email, display name, repo owner/namespace, forge
   host, forge usernames, CLI last-used IP, registration email-domain allowlist.
3. With demo mode off, all values render exactly as before (identity behavior);
   forms, keys, links, and search continue to operate on real values in both modes.
4. No API/DB/admin/CLI change; `task gate:web` green; works in real and mock builds.

## Decision log

- **D1**: Render-layer masking at pure-display sites only; never mask a
  key/href/input/search value; mask `title`/`aria-label` too.
- **D2**: Per-device localStorage, not per-user server pref, not admin-global.
- **D3**: Behavior-safe because non-display uses stay raw. Email/repo ARE
  client-side match keys (`RunsList.tsx:490`, `runGroups.ts:110`) — render-layer
  masking leaves them untouched. Search operates on real values (accepted).
- **D4**: Worker names, titles, branch names, issue numbers, brand name, and Slack
  `member_id` (input-bound) are deliberately NOT masked.
- **D5**: Infra fields mask to reserved/example values (`example.com`,
  `forge.example.com`, `203.0.113.x`).
- **D6**: Correctness rests on a mandatory field-name grep, not the static
  inventory, which is treated as incomplete by construction.
