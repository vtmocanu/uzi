# PRD #56: Slack notifications UX — surface workspace state so users know why DMs can't send

**GitLab Issue**: [vtmocanu/uzi#56](https://gitlab.example.com/vtmocanu/uzi/-/issues/56)
**Status**: Draft — reviewed 2026-07-16, 3 agents (design, security, fact-check), all findings applied (marked ↳review). Design: 1 MAJOR (M4 missed the argo `targetRevision` MR) + wiring precision; security: neutral, health-oracle residual recorded; fact-check: 12/13 claims verified, 1 fixed (mockApi's actual role).
**Priority**: Medium
**Created**: 2026-07-16
**Mockup**: `prds/mockups/56-slack-notif-ux-mock.html` (4 of the 6 UI branches: unconfigured → connected-unlinked → pending → linked; `connecting`/`error` are copy variants of the unconfigured layout per Decision 3)
**Depends on**: PRD #25 (Slack integration, done — this extends its M3 self-service card).
**Related**: `docs/slack.md` §4 "Link your own account" (the user-facing page this changes).

## Problem

The Settings → Notifications card renders identically whether Slack is fully
working or was never configured by the admin. Found live on dev-cluster
(2026-07-16): the instance has no Slack tokens at all — `app_settings` holds
zero Slack keys and the api pod logs contain no `slack:` lines, so the Socket
Mode manager never connects, the email auto-match hook never runs, and every
user's card sits at "Not linked" forever with a silently disabled **Send test
DM** button (`SlackNotifications.tsx:152` disables it on `!link.resolved_id`,
with no visible reason). The user's mental model is "something is broken with
my account"; the truth is "this instance has no Slack yet, only an admin can
fix it" — and nothing on the page says so. The workspace connection state
exists server-side (`Manager.State()`), but it is exposed only on admin
surfaces (`GET /api/admin/slack/status`, the admin settings DTO); regular
users have no read at all.

## Solution Overview

Expose the workspace connection state as **one non-secret string** on the
existing self-service DTO (`GET /api/me/slack` and the two `PUT` siblings),
collapsed to four values so no admin detail leaks. The card then explains
itself per state: an "ask an admin" alert with everything disabled when
unconfigured, a why-disabled hint under the test-DM button when connected but
unlinked, a check-Slack helper when pending. No notification-path behavior
changes; the test-DM button was already correctly disabled — it just never
said why.

## Design Decisions

1. **`workspace` rides the existing link DTO — no new endpoint, no polling.**
   `writeSlackLink` (`handler/slack.go:50`) is the single renderer for the GET
   and both PUTs ("the GET and the PUTs never drift"), so the field lands in
   all three responses in one place. Precision (↳review 2): `writeSlackLink`
   is a package-level function with no receiver, so the value is threaded as a
   new `workspace string` parameter — a signature change touching the three
   call sites (`slack.go:76,103,164`), each passing the collapsed
   `h.slackState()`; kept a pure computed field, no DB/network read (↳review
   security 2). The card already loads the DTO once on mount; workspace state
   changes rarely and an admin fixing Slack will be noticed on the next page
   view. PUT responses carry the field for free — the component's
   `setLink(slack)` replaces the whole object. The admin chip's dedicated poll
   (`GET /api/admin/slack/status`) is unaffected (its accessor is wired at
   `cmd/server/main.go:448`, so the new field is live in prod, not stuck at
   `disabled`). A per-user poll or WS push for this was rejected as
   disproportionate.
2. **Collapse the manager states; never leak error class to non-admins.** The
   live states (`slacksvc/manager.go:16-21`) map as: `disabled` →
   `unconfigured`, `connecting` → `connecting`, `connected` → `connected`,
   `error:auth` / `error:connection` → `error`. Whether the admin's token was
   rejected vs. a network drop is an admin-only diagnostic (it stays on the
   admin DTO); users only need "Slack isn't available right now". Source is
   the existing `h.slackState()` (`handler/handler.go:129`) — nil-manager-safe,
   already returns `disabled` when Slack was never wired.
3. **UI states are derived from `workspace` × `link.state`, alert-first.**
   - `unconfigured`: info alert "Slack isn't connected on this uzi instance
     yet, so notifications can't be delivered. An admin can set it up under
     Admin Settings → Slack." All controls (notify toggle, override input,
     both buttons) disabled. The card stays visible — hiding it would make the
     feature undiscoverable and the alert is the answer to "why can't I…".
   - `connecting` / `error`: the alert slot shows softer copy ("Slack is
     reconnecting…" / "Slack is temporarily unavailable — an admin can check
     Admin Settings → Slack."); controls stay **enabled** (the backend accepts
     writes regardless; a transient socket blip must not lock a user out of
     flipping their toggle). Precedence with `link.state` is pinned (↳review
     4): the workspace alert renders **above** whatever link-state helper
     applies — the two axes compose, they don't replace each other (a
     confirmed user during a reconnect sees the reconnecting alert over an
     otherwise-normal card). "Send test DM" stays clickable per its normal
     link-state rule; during `error` it surfaces the backend's 502
     ("couldn't send the test DM — check the Slack connection",
     `slack.go:186-198`) through the existing error alert — accepted, and a
     named test branch in M2.
   - `connected` + `unlinked`: current layout plus one hint line under the
     disabled test-DM button: "Test DMs become available once uzi resolves
     your Slack account — by email match or the override above."
   - `connected` + `pending`: helper line "Check Slack for a confirmation DM
     from uzi — notifications start once you press Confirm." Test DM stays
     enabled (backend allows resolved-but-unconfirmed, `slack.go:182` — useful
     precisely when the confirmation DM went missing).
   - `connected` + `confirmed`: unchanged.
4. **Disabling in `unconfigured` is UI-only; the API contract is untouched.**
   `PUT /me/slack/notify` and `/me/slack/override` keep accepting writes on an
   unconfigured instance (they're plain DB writes; the confirmation DM send is
   already best-effort). Rationale: no new server-side coupling between the
   settings write path and the socket state, and a pre-set override simply
   activates when an admin later connects Slack. The UI disables the controls
   in `unconfigured` anyway because inviting input that can have no visible
   effect for possibly weeks is worse UX than a clear "not available yet".
5. **Existing tests pin the DTO; extend, don't fork — and mockApi is the demo
   backend, not the test harness (↳fact-check, ↳review 3).** The Go handler
   tests assert individual DTO fields via `decodeSlack` (not exact JSON), so
   the added field is non-breaking; they gain `workspace` assertions.
   `SlackNotifications.test.tsx` stubs `../lib/api` via `vi.mock` — the new
   state branches are exercised there with per-test stub responses (there is
   no scenario knob to add stories to; `mockScenario()` is OIDC-only).
   `web/src/mocks/mockApi.ts` is updated separately for typecheck + demo-mode
   parity: its persisted row type becomes
   `Omit<SlackLink, "state" | "workspace">` (workspace is server-derived,
   never persisted) with the value injected in `slackLinkResponse()`, and it
   reports `workspace` consistently with its admin-side
   `slack_status: "disabled"` (i.e. demo mode shows `unconfigured`, which
   conveniently demos the new alert).

## Technical Design

### API (api/)

- `slackLinkDTO` (`handler/slack.go`) gains `Workspace string`
  (`json:"workspace"`); `writeSlackLink` gains a `workspace string` parameter
  (signature change, three call sites at `slack.go:76,103,164` — ↳review 2),
  each passing `publicSlackState(h.slackState())` — a pure collapse helper
  (Decision 2), table-tested over all five manager states plus nil manager →
  `unconfigured`.
- No migration, no query change, no new route.

### Web (web/)

- `web/src/lib/api.ts`: `SlackLink` gains `workspace: "unconfigured" |
  "connecting" | "connected" | "error"` (required field — every literal site
  is in this PRD's file list, so typecheck flushes out stragglers).
- `web/src/components/SlackNotifications.tsx`: state derivation per Decision
  3; the `unconfigured` alert reuses the existing `Alert` component
  (`tone="info"`, supported at `ui.tsx:132-141`); hint lines use the existing
  muted/faint text classes. No new components.
- `web/src/mocks/mockApi.ts` + `SlackNotifications.test.tsx` +
  `Settings.test.tsx` updated per Decision 5.

### Docs + specs

- `docs/slack.md` §4 "Link your own account" (`docs/slack.md:97`): one short
  paragraph — what the card shows when the instance has no Slack configured,
  and that only an admin can connect it.
- `specs/ai.md`: Decisions 1–4 recorded, including the accepted health-oracle
  residual (see Accepted residuals).

## Milestones

- [ ] **M1 — API: `workspace` on the link DTO**: `publicSlackState()` helper +
  DTO field + wiring in `writeSlackLink`; table test over all five manager
  states (incl. nil manager → `unconfigured`); existing slack handler tests
  extended. Validation: `curl /api/me/slack` on a token-less stack returns
  `"workspace":"unconfigured"`.
- [ ] **M2 — Web: self-explaining card states**: Decision 3 rendering,
  `mockApi.ts` type + injection update (Decision 5), vitest via per-test
  `vi.mock` stubs for the state branches (alert shown; controls disabled only
  in `unconfigured`; workspace alert composes above the link-state helper;
  hint under disabled test-DM; pending helper; test-DM during `error`
  surfaces the 502 message); `npm run typecheck` green. Validation: the
  mock's four rendered states reproduced in demo mode / tests.
- [ ] **M3 — Docs + specs**: `docs/slack.md` paragraph (check-docs green),
  `specs/ai.md` decisions + residual. Validation: `/docs/slack` renders the
  new paragraph in-app.
- [ ] **M4 — Release: version bump + tag + argo deploy + verify on
  dev-cluster** (three steps, `deploy/README.md:53-85` — ↳review 1, MAJOR):
  (a) bump `deploy/chart/Chart.yaml` `version` **and** `appVersion` to the
  next release version (currently `0.2.0`) in the feature MR, merge; (b) tag
  `vX.Y.Z` on the merged commit — the tag pipeline asserts version equality
  and publishes images + OCI chart; (c) **second MR to `argo-apps`
  bumping `targetRevision` in `apps/uzi/app.uzi.yaml`** — deploy is an
  explicit git change, not latest-tracking; without it ArgoCD stays pinned to
  the old chart and nothing rolls. Validation: the live instance (still
  Slack-unconfigured) shows the state-A alert on Settings → Notifications.

## Milestone dependency / parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 (parallel) | M1 (api/), M2 (web/, against the four-value contract) | — | `handler/slack.go` + tests · `api.ts`, `SlackNotifications.tsx` + tests, `mockApi.ts` |
| 2 | M3 | M1–M2 | `docs/slack.md`, `specs/ai.md` |
| 3 | M4 | M1–M3 | `deploy/chart/Chart.yaml`, git tag, `argo-apps` `apps/uzi/app.uzi.yaml` (cross-repo) |

## Out of Scope

- Auto-linking or retrying auto-match from the card (the linker's cooldowned
  on-connect pass stays the only trigger).
- Exposing error detail (`error:auth` vs `error:connection`) to non-admins
  (Decision 2).
- Polling/WS-pushing workspace state to the settings page (Decision 1).
- Server-side rejection of settings writes on unconfigured instances
  (Decision 4).
- Any change to notification delivery, the linker, or the test-DM backend.

## Accepted residuals (named, per review)

- **Coarse health oracle for non-admins** (↳review security 1): this is the
  first non-admin exposure of any derivative of the live socket state — an
  authenticated user can poll `GET /api/me/slack` and watch
  `connected → error → connecting` transitions. Four values, no token/URL, no
  auth-vs-network class; consistent with the trusted-team posture already
  recorded for this route. Recorded in `specs/ai.md`.
- **UI copy names the admin surface ("Admin Settings → Slack") to non-admins**
  (↳review security 4): server-side `RequireAdmin` gates the page itself;
  naming it grants nothing and helps the user route the ask. Deliberate.
- **Test DM clickable during `error` returns a surfaced 502** (↳review 4):
  controls stay enabled outside `unconfigured` by design; the existing error
  alert is the feedback. Pinned by an M2 test, not redesigned.

## Success Criteria

- On a Slack-unconfigured instance, a user opening Settings → Notifications
  immediately sees that Slack isn't set up and who can fix it — no dead-end
  disabled button without explanation.
- On a configured instance, an unlinked user is told exactly what unlocks the
  test DM; a pending user is pointed at the confirmation DM.
- Linked users see a byte-identical card to today.
- No admin-only information (token validity, error class) is readable by
  regular users.
- The change ships as a tagged release (chart `version`/`appVersion` bumped,
  argo `targetRevision` bumped) and is verified live on dev-cluster.
