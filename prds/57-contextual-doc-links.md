# PRD #57: Contextual docs links — UI surfaces link to their in-app guide

**GitLab Issue**: [vtmocanu/uzi#57](https://github.com/vtmocanu/uzi/-/issues/57)
**Status**: Draft
**Priority**: Low
**Created**: 2026-07-16
**Depends on**: PRD #7 (in-app docs, done), PRD #28 (docs search, done).
**Coordinate with**: PRD #56 (Slack notifications UX) — both touch
`web/src/components/SlackNotifications.tsx` and `Settings.test.tsx`; sequence
the merges.
**Related**: `docs/README.md` (frontmatter contract), `web/src/lib/docs.ts` (`listUserDocs`).

## Problem

uzi ships a full in-app docs section (`/docs`, bundled at build time from
`docs/*.md` with `audience: user`) that explains every setup flow — bot PAT
creation, worker join tokens, Anthropic tokens, Slack linking. But the UI
surfaces where users actually get stuck almost never link to it. Found
2026-07-16 on the Settings → Forge tab: the card asks for a bot PAT with the
`api` scope, yet the step-by-step `gitlab-bot-setup` guide is linked **only
from the over-privileged-token error card** (`ForgeSettings.tsx:173`) — a
first-time user doing it right never discovers the guide. A repo-wide grep
shows exactly one `/docs/<slug>` reference in the whole SPA outside the docs
pages themselves; the only other doc link (`Settings.tsx:21` `DOC_URL`) sends
the user to the GitLab **blob URL** of `anthropic-token.md` in a new tab
instead of the same guide bundled in-app. The docs are one router hop away
and effectively invisible; users fall back to asking an admin or guessing.

## Solution Overview

A tiny reusable `DocLink` component plus a central slug registry with a
slug-validity test, then wire each settings/admin/management surface to its
matching `audience: user` guide with an always-visible "see the … guide"
sentence in the card intro (not only in error states). No API or backend
changes; this is a web-only PRD.

## Surface → guide mapping

| Surface (page) | Doc slug | Guide title |
| --- | --- | --- |
| Settings → Forge (`ForgeSettings.tsx`) — **the original ask** | `gitlab-bot-setup` | GitLab bot setup |
| Settings → Workers (`WorkersSettings.tsx`) | `worker-setup` | Worker setup |
| Settings → Account & token (`Settings.tsx`) | `anthropic-token` | Anthropic token |
| Settings → Slack card (`components/SlackNotifications.tsx`, rendered in `Settings.tsx`) | `slack` | Slack notifications |
| Repos / boards home (`Repos.tsx`) | `repo-agents` | Repo agents |
| Agents (`Agents.tsx`, `AgentNew.tsx`) | `agent-templates` | Agent templates |
| Skills (`Skills.tsx`) | `skills` | Agent skills |
| Admin → Tool allowlist (`ToolAllowlist.tsx`, admin-gated) | `worker-tools` | Per-repo tools |
| Admin settings (`AdminSettings.tsx`) | `admin-settings` | Admin settings |
| Admin rate limits (`AdminRateLimits.tsx`) | `rate-limits` | Claude rate limits |

All ten slugs are already `audience: user` (verified against frontmatter,
2026-07-16), so every target renders in-app today — no doc changes needed.
`/docs` routes are public (`App.tsx:59-61`, unauthenticated), so links from
protected/admin pages never hit a login wall; the flip side is that the
admin-oriented guides (`admin-settings`, `rate-limits`) are already publicly
readable, and linking them from admin pages creates no new exposure.

## Design Decisions

1. **One `DocLink` component, one central slug registry.** A
   `web/src/lib/doclinks.ts` exports named slug constants (e.g.
   `DOC_BOT_SETUP = "gitlab-bot-setup"`); a `DocLink` component
   (`web/src/components/DocLink.tsx`) takes a slug + children and renders the
   react-router `Link` to `/docs/<slug>` with the existing
   `text-brand hover:underline` idiom. Pages never hand-write `/docs/…`
   strings. Two existing links migrate to it: the ForgeSettings error-card
   link (local `BOT_SETUP_DOC` constant) and the Settings.tsx `DOC_URL`
   external GitLab-blob link to `anthropic-token.md` — the in-app route is
   the same content, works offline, and drops the `target="_blank"` hop.
   (Checked `inspiration/` per convention: no comparable bundled-docs SPA
   link registry exists in bottega/multica/dot-agent-deck — this is a
   uzi-specific design.)
2. **Slug validity is a unit test, not a convention.** `check-docs.mjs`
   validates the docs side (frontmatter, orders, relative links) but nothing
   validates SPA → docs references; a renamed doc file would leave a stale
   link landing on DocPage's soft not-found (`DocPage.tsx:17`). A vitest test
   asserts every registry constant is a slug returned by `listUserDocs()` —
   renaming or de-`user`-ing a doc breaks `npm test` instead of a live link.
   The test must also assert `listUserDocs()` is non-empty (it is the first
   test to depend on the `import.meta.glob` doc map populating under vitest;
   an empty map must not pass vacuously).
3. **Always-visible placement, one sentence, in the card intro.** The link
   belongs in the descriptive paragraph the user reads before acting
   ("Step-by-step instructions are in the **GitLab bot setup** guide."), not
   behind an icon, tooltip, or error state. Error/help states may add a
   second, more targeted link (the over-privileged card keeps its own), but
   the happy path must carry one too — that was the gap. Accessibility falls
   out of the pattern: the guide title is the link text (never "click here"),
   same-tab react-router navigation, no `target="_blank"`.
4. **In-app links only, no deep anchors.** Targets are `/docs/<slug>` routes
   (client-side navigation, works offline with the bundled docs). Linking to
   `#section` anchors inside a guide is out of scope until DocPage heading
   anchors are a proven need.

## Milestones

- [ ] **M1 — Link infrastructure**: `DocLink` component + `doclinks.ts` slug
  registry + slug-validity vitest (incl. the non-empty guard); ForgeSettings'
  existing error-card link migrated to it. `npm run typecheck` and
  `npm test` green.
- [ ] **M2 — Settings surfaces** (the original ask): always-visible guide
  links on the Forge, Workers, Account & token, and Slack cards, per the
  mapping table — Account & token and Slack both live in `Settings.tsx`
  (the Slack card is the `SlackNotifications` child component), and the
  existing `DOC_URL` external-blob link is replaced by the in-app DocLink;
  page tests updated to assert the links render.
- [ ] **M3 — Management surfaces**: Repos/boards home, Agents, Skills.
- [ ] **M4 — Admin surfaces**: Tool allowlist, Admin settings, Admin rate
  limits.
- [ ] **M5 — Convention recorded + shipped**: `docs/dev-conventions.md`
  documents the DocLink registry + test contract (new UI surface ⇒ consider
  a guide link); full `npm run build` (check-docs + typecheck + vite) green.
  Ships with the next tagged release (Model B, per `deploy/README.md`);
  verify one link live on dev-cluster after it rolls out.

## Success criteria

- Every surface in the mapping table shows its guide link in the default
  (non-error) state.
- Zero hand-written doc-link strings in pages — neither `/docs/…` routes nor
  external GitLab-blob URLs to `docs/*.md` (registry only; grep-clean outside
  `doclinks.ts`, the docs pages themselves, and tests).
- Renaming a linked doc file fails `npm test` before it can ship a 404.

## Out of scope

- Board, Chat, and run-view contextual help (those pages are task surfaces,
  not setup surfaces; revisit if users ask).
- Deep-linking to sections within a guide (needs DocPage heading anchors).
- Any backend/API change, and any docs content change.
