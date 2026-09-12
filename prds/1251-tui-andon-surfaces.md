# PRD 1251: TUI andon surfaces — startup update prompt + vault-locked indicator

- **Issue**: #1251
- **Status**: Draft, ready for the Planned sweep
- **Priority**: Medium
- **Surfaces**: TUI only (`uzi tui`), the `api` Go module (`api/cmd/uzi/`). No web, no forge, no new trust boundary.
- **Guardrail**: TUI-only. Neither implementation nor validation touches `.github/workflows/**` (worker PAT lacks `workflow` scope; see `.claude/rules/prds.md`). Invariant at finalize: `git diff --name-only <base>..HEAD` shows zero entries under `.github/workflows/`.

> Two independent surfaces are grouped into one PRD because they share a single design
> contract (the TUI "andon" palette and render primitives) and are each small. They ship as
> separate milestones and can merge independently. Decision labels are D1..D11.

## Problem

The `uzi tui` board does not surface two actionable states the user needs at a glance:

1. **A newer uzi-cli release is available.** Today you only discover this by running `uzi version` (which already prints an `update X available` row, `api/cmd/uzi/version.go:165`) or by reading the release page. Codex prompts for an upgrade at startup; uzi does not.
2. **Your vault is locked.** A locked vault silently parks your queued runs. The web SPA already tells you ("waiting for vault unlock", derived from AuthContext `unlocked`, `api/internal/handler/auth.go:438-445`) and there is already a notification (`api/internal/schedsvc/vault_lock_notice.go`, PRD #890), but the **TUI shows nothing** beyond an owner-only health reason buried in the run detail. A user watching the board sees runs sit in `queued` with no explanation.

## Resolved facts (current code, no open-web lookup needed)

Everything this PRD relies on is already in the tree or on the wire; an offline worker can verify each by reading the cited file.

### M1 — update prompt

- The server runs an upstream release check by default: `DefaultReleaseCheckEnabled = "true"`, interval `6h` (`api/internal/settings/keys.go:362-364`), master gate `release_check_enabled`. When on, it persists `release_latest_tag` etc. from `github.com/vtmocanu/uzi` releases (`api/internal/releasecheck/`). **The api does the GitHub call; the derivation is read-time with zero CLI egress** (`api/internal/releasecheck/derive.go` header comment).
- The public, unauthenticated `GET /api/version` carries the release facts M1 needs: `BuildInfoDTO.Latest *LatestReleaseDTO` (`Version`, `Name`, `PublishedAt`, `NotesURL`, `Security bool`; inner fields at `api/internal/apitypes/buildinfo.go:101-113`), plus `UpdateAvailable *bool` and `FarBehind *bool` (`:85-93`). All are nil until a check has run and the feature is enabled (nil = unknown, distinct from false). **No backend work for M1.**
- **🔴 Axis warning — `UpdateAvailable` on the wire is the WRONG boolean to gate on.** It is derived *server-version-vs-latest* (over the api's own running version, `releasecheck`), not *CLI-version-vs-latest*. A current server (wire `update_available=false`) talking to a stale local CLI still needs the prompt. So use `Latest`/`UpdateAvailable` **only as a presence/nil signal** (nil ⇒ no check ran or feature disabled ⇒ show nothing), and **recompute the CLI axis locally**: `releasecheck.UpdateAvailable(cliVersion, Latest.Version)` — the semver compare, re-prefix + `IsValid`-guarded (`api/internal/releasecheck/derive.go:32`). `Latest` is populated whenever a check ran regardless of the server's own currency (`buildinfo.go:76-84`), so this always works. This is a **different axis** from the existing CLI-vs-server skew banner (`api/cmd/uzi/versioncheck.go`); keep both.
- The TUI already *fetches* `/api/version` but currently keeps only the version string: `fetchBuildInfoCmd` (`api/cmd/uzi/tui.go:492`) extracts `info.Version` into `buildInfoMsg{version, err}` (`tui.go:125-128`) and **discards `Latest`/`UpdateAvailable`/`Security`**. M1 must widen `buildInfoMsg` and the command to carry those fields — they are on the HTTP response, not yet on the message. The version string is already treated as a D7 untrusted field and sanitized via the local `cellText` in the footer skew readout (`api/cmd/uzi/tui_board.go:505-516`, `colorprofile.Ascii` check at `:516`); `Latest.Version` must get the same treatment.
- Existing gating machinery to reuse verbatim: `uzicli.IsStampedVersion(version)` (skips `go build` / source binaries, `api/internal/uzicli/versioncheck.go:46`), the `UZI_VERSION_CHECK=0` escape hatch, and the TTL cache `Store.CachedServerVersion` / `RecordServerVersion` with `VersionCheckTTL = time.Hour` (`api/internal/uzicli/versioncheck.go:133-154`). The same local store is where a per-version dismissal is persisted.
- uzi-cli installs via Homebrew and **builds from source**: `cd api && go build ... -X main.version=v#{version}` (`Formula/uzi-cli.rb:39-47`). So `brew upgrade uzi-cli` compiles Go (tens of seconds to a minute) and needs the `go` build dep. The tap is published by the same release CI as the GitHub release, so `Latest.Version` and the tap move together (no codex-style brew-cask lag). `brew upgrade` is non-interactive (no `-y` flag).

### M2/M3 — vault-locked indicator

- **Primary detector (reliable, NOT health-gated): `vault.unlocked` from `GET /api/auth/me`.** That endpoint is mounted `RequireUser`, so `uzi whoami` already reaches it over a Bearer token (`api/internal/handler/routes_auth.go:57-60`, comment: "GET /me is RequireUser so `uzi whoami` works"), and its `sessionPayload` already returns `"vault": {"unlocked": h.vaultUnlocked(user.ID), "exists": ...}` (`api/internal/handler/auth.go:444-447`, backed by `vault.Unlocked(userID)`, `auth.go:484-485`). The **CLI just does not decode it today**: `HTTPClient.Whoami` (`api/internal/uzicli/client_account.go:16-24`) hits `/api/auth/me` but reads only `{ user }`. So the whole feature is **backend-free**: decode the existing `vault.unlocked` (locked = `!unlocked`). Do NOT reuse the cookie-only web AuthContext path (`auth.go:445`) or the admin rate-limit DTO (`VaultLocked bool json:"vault_locked"`, `api/internal/apitypes/ratelimit.go:63`, `handler/ratelimits.go:120`); they read the same underlying state through paths the TUI must not take.
- **Escalation input (best-effort, for tier-2 only): parked-run count from `RunDTO`.** The board loads runs via `ListRuns` / `AdminListRuns` (`api/cmd/uzi/tui.go:428-430`) into owner/admin-scoped `apitypes.RunDTO`, whose `HealthReason *string` (`api/internal/apitypes/run.go:279`) **rides unconditionally** (`run.go:274-275`: "the owner-gating that the shared board applies is unnecessary"). This is NOT the shared-board `latestRunDTO` path that redacts owner-state reasons for non-owner viewers (`api/internal/handler/board.go:198-201`) — the TUI never calls that path, so the earlier "inherit board.go's redaction" framing does not apply here. Two consequences the implementer MUST handle:
  1. On a normal user's board every run is theirs; nothing is redacted.
  2. On the **admin board** (`a` toggles the factory view → `AdminListRuns`) health reasons for ALL users are present. A parked-run count MUST be scoped to the viewer's own runs (`OwnerEmail`, the only owner discriminator on admin `RunDTO`, `run.go:551`) — or the band suppressed on the admin/factory view — so it never counts or blames another user's locked vault the admin cannot act on.
- **The run-level vault reason is run-health-gated and has no stable enum, so it cannot be the primary signal.** `reasonVaultLocked = "your vault is locked, so this run can't start"` (`api/internal/workersvc/health.go:63`) is produced only by the run-health detector, which is off when `HealthEnabled` is false (default true, `api/internal/settings/keys.go:312`, `settings/settings_health.go:32`); and a vault-locked run maps to the **generic `health = "waiting_worker"`** value shared with ~8 other causes (seen at `api/cmd/uzi/tui_lanes.go:361`, `tui_render.go:208`), so there is no distinct `Health` value to match on. Hence: `vault.unlocked` is the detector; the parked count is a best-effort escalation input, and when run-health is disabled the indicator still shows (as the tier-1 hint) because whoami still reports the lock.
- `reasonVaultLocked` is **unexported** in `workersvc`, so `cmd/uzi` (package `main`) cannot import it. Match a lowercased substring like `"vault is locked"` on `HealthReason` and pin it with a regression test; a locked vault also causes a transient requeue at claim (`errVaultLocked`, `api/internal/workersvc/claim_assembly.go:485`), so the parked run is `queued`/`waiting_worker`.
- Existing copy to align with (do not invent new wording): title "Vault locked", body "Your vault is locked — unlock it so your queued and scheduled runs can proceed." (`api/internal/schedsvc/vault_lock_notice.go:102-103`).
- The board draws a single-line per-account rate-limit strip under the wordmark, `boardRateLimitStrip` (`api/cmd/uzi/tui_board_rows.go:82`, invoked `tui_board.go:332`). **D5: the vault indicator is a separate line and must not replace or hide this strip** — budget info stays visible while parked.

### Shared design language (both milestones)

- Palette + render primitives: `api/cmd/uzi/tui_render.go`. `newPalette(dark bool)` (`:161`) defines the andon tokens as `ld(light, dark)`: `tungsten` `ld(#7c5200,#c9a061)` (chrome / selection accent), `amber` `ld(#b45309,#ffb454)` (the one filled "needs-you" surface), `bandFg` `ld(#ffffff,#0e1016)` (near-bg ink on the amber band), `sage` `ld(#2f7d4f,#6fbf8f)` (ok/good), `faintC` `ld(#6c6c6c,#8a8a8a)` (ids/ages/chrome), `selBg` `ld(#f3ead8,#33302a)` (selection tint). Box style + `provenanceBox` at `:116,182-187`.
- Selection convention (any interactive row): bold `▸` cursor only, regular-weight `tungsten` label, on `selBg`, full-row fill — `api/cmd/uzi/tui_board_rows.go:242-244` (cursor bold, `:233-234` title tungsten, `:238-239` id tungsten).
- D7 untrusted rendering: any server-authored string (server version, health reason) MUST go through `m.renderer.Plain` (= `capCell(cellText(s))`), be added to `d7UntrustedFields`, and be covered by the hostile-value render test (`api/cmd/uzi/tui_model_test.go`, guard `tui_d7_guard_test.go`) — per `.claude/rules/tui.md`.
- Colorprofile downgrade: color alone cannot carry a signal. Under `colorprofile.Ascii` / `NO_COLOR` the amber fill and any tint are stripped, so every signal needs a text/glyph carrier that survives (`tui_board.go:516`).

## Solution

Two consistent, **steady** (never blinking) andon surfaces on the board, both built from the primitives above.

### M1 — startup update prompt (codex-style, uzi-native)

On `uzi tui` startup, if a newer uzi release exists, show a modal before the board:

```
▲ Update available
uzi 0.83.0  →  0.85.0          (latest in sage, current in faintC)
A newer release is available.

▸ Update now  (brew upgrade uzi-cli)
  Not now
  Don't remind me for 0.85.0

↑/↓ move · enter select · esc = not now
```

- **Detection, no CLI egress**: widen `buildInfoMsg` to carry `Latest` / `Security` (see Resolved facts); use the wire `Latest`/`UpdateAvailable` only as a **presence signal** (nil ⇒ show nothing) and **recompute the CLI axis locally** with `releasecheck.UpdateAvailable(cliVersion, Latest.Version)` — never gate on the wire `UpdateAvailable`, which is the server's axis. Degrade to nothing when `Latest` is nil (server unreachable, or release-check disabled).
- **Gating** (mirrors the skew warning): `IsStampedVersion` + `UZI_VERSION_CHECK != 0` + `Latest` present + locally-computed behind + not dismissed + TTL. Reuse the existing local-store cache.
- **Brew users** get the "Update now" action; **non-brew** (go-install / source) users get an info variant with the release-notes link and no button. Brew detection (`brew list uzi-cli`, or the binary path under `brew --prefix`) **shells out and MUST go through the same injection seam as the upgrade** so it is deterministic offline.
- **"Update now" = foreground exit** (D1): clear the screen, exit the TUI, run `brew upgrade uzi-cli` in the foreground so compile progress and failures are visible, then tell the user to rerun `uzi tui`. **Not** a background upgrade (the running process would stay stale, output hidden). The command is constructed and handed off on exit. **Seam exemplar**: a func-typed `Env` field like `Env.Git` / `Env.NewClient` / `Env.Getenv` (`api/cmd/uzi/root.go:119-122`) — NOT `env.CheckServerVersion`, which is a plain bool gate. The seam covers both brew detection and the upgrade so tests assert the argv without running brew.
- **Security releases escalate** (D2): when `Latest.Security` is true, render the prompt as the amber andon band (the one filled surface) instead of quiet tungsten ink, and word it as a security update. Reuse `Latest.Security`; no new field.

### M2 — vault-locked detection + tier-1 hint (foundational, backend-free)

Detect the lock from the reliable, always-available signal and show a quiet baseline indicator.

- **Detector**: decode `vault.unlocked` from `GET /api/auth/me` — extend `HTTPClient.Whoami`'s envelope (`api/internal/uzicli/client_account.go:16-24`, currently `{ user }` only), thread `locked = !unlocked` into the TUI model. No backend, not run-health-gated (works even when `HealthEnabled` is false).
- **Tier-1 render**: when locked, a quiet, faint footer hint beside the rate-limit strip (`🔒 vault locked`, or an ascii-safe equivalent) — informational, not an alarm; the strip stays (D5).
- **Steady, auto-clearing, non-dismissable** (D3): a lock is a standing blocker, not live motion — blink here means "in-progress" (milestone bars, crew-rail now-line), so reusing it would collide and is an accessibility hazard. It clears on its own when the vault unlocks; no dismiss (unlike the update prompt).
- **Glyph + text**, never color alone (D4): the word "vault locked" carries the signal under NO_COLOR/Ascii.

### M3 — escalate to the amber band when runs are parked (tier-2)

When locked **and** ≥1 of the viewer's own runs is parked on the vault, upgrade the tier-1 hint to a steady amber needs-you band, a **new line** above/around the board (the `boardRateLimitStrip` stays untouched, D5):

```
▌ VAULT LOCKED · 2 runs parked — unlock to resume (web · uzi vault unlock)
```

- **Escalation count (best-effort)**: count the viewer's own runs whose `HealthReason` matches a lowercased `"vault is locked"` substring (the reason string is unexported in `workersvc`, and there is no distinct `Health` enum — see Resolved facts). Pin the substring with a regression test.
- **Admin board**: on the `AdminListRuns` factory view, health reasons for ALL users are present, so scope the count to the viewer's own runs via `OwnerEmail` — or suppress the band on the factory view — never count another user's locked vault.
- **Graceful degrade**: when run-health is disabled or the count is 0, stay at the M2 tier-1 hint (the lock is still shown because whoami reports it); the band is purely additive.
- **Tell the fix in the band**: unlock is off-TUI (web / `uzi vault unlock`); copy aligns with `vault_lock_notice.go`. `HealthReason` renders through `m.renderer.Plain` + `d7UntrustedFields` + a hostile-render test (D7).

## Decision log

- **D1**: "Update now" exits and upgrades in the foreground, not a background upgrade — the formula builds from source, so hidden compile output/failures and a stale running process are worse than a brief exit-and-relaunch. (User-confirmed 2026-09-12.)
- **D2**: Severity uses `Latest.Security`; a security release renders as the amber andon band, a routine one as quiet tungsten/faint ink.
- **D3**: Vault indicator is **steady, not flashing**; auto-clears on unlock; non-dismissable. (User-confirmed 2026-09-12.)
- **D4**: Every andon signal has a text/glyph carrier that survives the colorprofile downgrade.
- **D5**: The vault band is a new line and never replaces/hides `boardRateLimitStrip`; the two are distinct signals. (User-confirmed 2026-09-12.)
- **D6**: The CLI does no egress for M1 — detection reads the wire the TUI already fetches; the api owns the GitHub call.
- **D7**: Server-authored strings (server/latest version, `HealthReason`) route through `m.renderer.Plain`, join `d7UntrustedFields`, and get a hostile-render test. Note the TUI reads owner/admin-scoped `RunDTO` where `HealthReason` rides **unconditionally** (`run.go:274-275`) — it is NOT the redacted shared-board path — so cross-tenant safety on the admin board is D11's scoping job, not inherited redaction.
- **D8**: Build from existing primitives (palette tokens, box/`provenanceBox`, `▸` selection convention) — no literal hex, no new ANSI, no parallel mock model. **Do not mandate the `--sketch` harness for this (unattended, screenless) worker**: a sketch's only value is visual iteration the worker cannot do, and no gate forbids a forgotten feature sketch merging to `main` (`sketch_test.go` asserts neither registry size nor "only template"). Build directly in the real `tuiModel` with deterministic seam tests. Finalize invariant: the sketch registry is unchanged (only `template` registered).
- **D9**: All milestones are backend-free. The self vault state is already served on the CLI-reachable `GET /api/auth/me` (`vault.unlocked`), so the vault work is a client-side decode of that existing field, not a new endpoint or DTO. Order: M1 (update prompt) and M2 (vault detection + tier-1 hint) first; M3 (tier-2 escalation band) builds on M2 and is optional/last.
- **D10**: The vault detector is `vault.unlocked` from whoami, NOT the run-level health reason. The reason is run-health-gated (`HealthEnabled`) and collapses into the generic `waiting_worker` value with no distinct enum, so it is only a best-effort escalation input for the tier-2 count, never the presence signal.
- **D11**: On the admin/factory board (`AdminListRuns`), the tier-2 parked count is scoped to the viewer's own runs via `OwnerEmail` (or the band suppressed there); the indicator never counts or blames another user's locked vault.

## Milestones

Each milestone's acceptance is its own behavior plus a green `task gate:api` (the gate, run once to a log then read it, `.claude/rules` Run economy) — deterministic `tui_render_test.go` / `tui_model_test.go` seam assertions (Update→msg / View→string, SGR + substring), never a screenshot.

- [ ] **M1a — Update-prompt render (real `tuiModel`, no sketch).** Modal overlay: box via `provenanceBox`, three choices with the `▸`/tungsten/`selBg` selection convention, latest in `sage`, security variant as the amber band. Deterministic View/Update assertions per state (normal / security-band / non-brew / selected-row) + the colorprofile-Ascii text fallback.
- [ ] **M1b — Detection, gating, dismissal.** Widen `buildInfoMsg`/`fetchBuildInfoCmd` to carry `Latest`/`Security`; compute the CLI axis locally with `releasecheck.UpdateAvailable(cliVersion, Latest.Version)` (never the wire bool, D6/axis warning); gate on `IsStampedVersion` + `UZI_VERSION_CHECK` + `Latest` present + behind + not-dismissed + TTL; persist per-version dismissal in the local store. `Latest.Version` sanitized (D7).
- [ ] **M1c — Brew detection + foreground-exit upgrade.** Detect brew and run `brew upgrade uzi-cli` both through a func-typed `Env` seam (exemplar `Env.Git`, `root.go:119-122`); "Update now" hands off on foreground exit; non-brew info variant with the release-notes link. Tests assert the constructed argv via the seam without running brew.
- [ ] **M2 — Vault detection + tier-1 hint (foundational, backend-free).** Decode `vault.unlocked` from `GET /api/auth/me` in `HTTPClient.Whoami` (envelope was `{user}` only), thread `locked` into the model; render the quiet faint footer hint beside the strip when locked; steady, auto-clears, glyph+text fallback. CLI decode test + View assertion.
- [ ] **M3 — Tier-2 escalation band (builds on M2, optional).** When locked and ≥1 of the viewer's OWN runs is parked on the vault (best-effort `"vault is locked"` substring on `HealthReason`, regression-pinned), upgrade to the steady amber band as a new line leaving `boardRateLimitStrip` intact; scope the count by `OwnerEmail` on the admin board (D11); degrade to the M2 hint when run-health is off or count is 0; `HealthReason` through `m.renderer.Plain` + `d7UntrustedFields` + hostile-render test.
- [ ] **M4 — Docs + CLI parity + finalize invariants.** Update the TUI/CLI docs (`docs/cli.md` and any TUI doc) for the startup prompt and vault indicator, run `task docs:sync` and commit the mirror (docs-only edits without the sync redden `gate:api`, `TestEmbeddedDocsMatchSource`). Confirm no other `api/cmd/uzi/` command needs a matching change (behavior is TUI-local). Finalize invariants: `git diff --name-only <base>..HEAD` shows zero `.github/workflows/**`, and the sketch registry is unchanged (only `template`, D8).

## Risks

- **R1 — brew detection false negative.** If brew detection misses, a brew user sees the info variant (no button) instead of the action. Non-fatal; the manual command is shown. Mitigation: two detectors (`brew list` and prefix path), prefer the cheaper one, fall back to info variant on doubt.
- **R2 — Offline worker cannot run brew or unlock a vault.** Both are runtime side effects, not implementation steps. Validate via injection seams (fake exec for brew; synthesized `HealthReason` / build-info fixtures for the band), never by actually invoking brew or a vault. This keeps the whole PRD implementable and gate-verifiable with no egress.
- **R3 — Tier-2 reason-string coupling.** The escalation count matches a `HealthReason` substring because `reasonVaultLocked` is unexported in `workersvc` and there is no distinct `Health` enum (`waiting_worker` is shared, D10). A reword upstream would break the count silently. Mitigation: match a lowercased `"vault is locked"` substring, pin it with a regression test; presence does not depend on it (it uses `vault.unlocked`), so a reword degrades only the tier-2 count, never whether the lock is shown.
- **R4 — Layout on narrow terminals.** Update prompt is transient; the vault indicator is persistent, alongside the existing strip. The band/hint must degrade gracefully at ~80 cols; only there may the vault hint yield priority to the strip.
- **R5 — Update prompt nag.** Mitigated by the TTL cache and per-version "Don't remind me", reusing the existing skew-cache store.
- **R6 — Run-health disabled hides the parked count.** When `HealthEnabled` is false, no parked run carries the vault reason, so the tier-2 count is 0. Mitigated by design (D10): presence comes from `vault.unlocked`, so the indicator still shows as the tier-1 hint; only the amber-band escalation is suppressed. A test with health-off fixtures asserts the tier-1 fallback still renders.

## Validation strategy

- **Deterministic (gates in CI)**: `tui_render_test.go` / `tui_model_test.go` seam tests with SGR-escape + substring assertions for: update-prompt states (normal / security-band / non-brew / selected-row styling); vault tier-1 hint present/absent by `vault.unlocked`; vault tier-2 band by the viewer's own parked-run count (incl. an admin-board scoping case and a health-off fallback-to-tier-1 case, R6); and the colorprofile-Ascii text fallback for each. D7 hostile-value render test extended for `Latest.Version` and `HealthReason`. Plus the `HTTPClient.Whoami` decode test (envelope now carries `vault.unlocked`) and the M1c seam argv assertions. `task gate:api` is the per-milestone gate line; `task scan:secrets` is not needed (no credential-shaped fixtures).
- **Visual (review, not a gate)**: render the new scenes to light+dark PNGs via the uxlab harness (`api/cmd/uzi/uxlab`, `.claude/rules/tui.md`) and review with the `tui-ux` agent.
- **Interactive smoke**: `go run ./cmd/uzi tui --demo` with seeded fixtures for a hands-on pass (maintainer, post-merge).

## Out of scope

- No change to the server release-check subsystem, the vault subsystem, or `vault_lock_notice.go`.
- No `.github/workflows/**` edits (guardrail above).
- No web changes (the web already surfaces both states).
- No automatic/silent upgrades — the update is always a per-invocation, explicit choice.
