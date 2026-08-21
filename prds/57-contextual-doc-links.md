# PRD #57: Contextual docs links — UI surfaces link to their in-app guide

**GitHub Issue**: [vtmocanu/uzi#57](https://github.com/vtmocanu/uzi/issues/57)
**Status**: Implemented (M1–M4 landed on `agent/issue-57`); M5 in-branch parts
done, its release cut + live dev-cluster verification pending post-merge
**Priority**: Low
**Created**: 2026-07-16
**Refreshed**: 2026-08-21 — reconciled against a repo that has since gone
multi-forge (GitLab / Forgejo / GitHub drivers) and migrated its own remote
from GitLab to GitHub; the surface mapping, the two links to migrate, and the
line references below were all restated against current `main`.
**Depends on**: PRD #7 (in-app docs, done), PRD #28 (docs search, done).
**Related**: `docs/README.md` (frontmatter contract), `web/src/lib/docs.ts`
(`listUserDocs`, `getDoc`, `REPO_BLOB_BASE`).

## Problem

uzi ships a full in-app docs section (`/docs`, bundled at build time from
`docs/*.md` with `audience: user`) that explains every setup flow — bot PAT
creation, worker join tokens, Anthropic tokens, Slack linking. But the UI
surfaces where users actually get stuck almost never link to it. Found
2026-07-16 on the Settings → Forge tab and still true on `main`: the card asks
for a bot PAT with the write scope for the connecting forge, yet the
step-by-step bot-setup guide is linked **only from the over-privileged-token
error card** (`ForgeSettings.tsx:249`, `botSetupDoc(forgeType)`) — a first-time
user doing it right never discovers the guide. Outside the docs pages
themselves the SPA carries only two doc references: that forge-aware error-card
link, and one external link (`AnthropicTokens.tsx:17`, `DOC_URL`) that sends the
user to the **GitHub blob URL** of `anthropic-token.md` in a new tab instead of
the same guide bundled in-app. The docs are one router hop away and effectively
invisible; users fall back to asking an admin or guessing.

Note the forge context: since 2026-07-16 uzi has grown Forgejo and GitHub
drivers alongside GitLab, and this repo migrated its own remote to GitHub, so
"the bot-setup guide" is now three docs (`gitlab-bot-setup`, `forgejo-bot-setup`,
`github-bot-setup`) selected by the connected forge type — the Forge surface
below is wired forge-aware, not to a single GitLab slug.

## Solution Overview

A tiny reusable `DocLink` component plus a central slug registry with a
slug-validity test, then wire each settings/admin/management surface to its
matching `audience: user` guide with an always-visible "see the … guide"
sentence in the card intro (not only in error states). No API or backend
changes; this is a web-only PRD.

## Surface → guide mapping

| Surface (page) | Doc slug | Guide title |
| --- | --- | --- |
| Settings → Forge (`ForgeSettings.tsx`) — **the original ask** | forge-aware: `gitlab-bot-setup` / `forgejo-bot-setup` / `github-bot-setup` | Bot setup (for the connected forge) |
| Settings → Workers (`WorkersSettings.tsx`) | `worker-setup` | Worker setup |
| Settings → Account & token (`components/AnthropicTokens.tsx`, rendered in `Settings.tsx`) | `anthropic-token` | Anthropic token |
| Settings → Slack card (`components/SlackNotifications.tsx`, rendered in `Settings.tsx`) | `slack` | Slack notifications |
| Repos / boards home (`Repos.tsx`) | `repo-agents` | Repo agents |
| Agents (`Agents.tsx`, `AgentNew.tsx`) | `agent-templates` | Agent templates |
| Skills (`Skills.tsx`) | `skills` | Agent skills |
| Admin → Tool allowlist (`ToolAllowlist.tsx`, admin-gated) | `worker-tools` | Per-repo tools |
| Admin settings (`AdminSettings.tsx`) | `admin-settings` | Admin settings |
| Admin rate limits (`AdminRateLimits.tsx`) | `rate-limits` | Claude rate limits |

All twelve target slugs (the three forge-setup guides plus the nine others) are
already `audience: user`, so every target renders in-app today — no doc changes
needed. The Forge surface does **not** take a fixed slug: it reuses the existing
`botSetupDoc(forgeType)` helper (`ForgeSettings.tsx:19-23`, PRD #65 M6b), so the
registry must expose those three forge-setup constants rather than one, and the
Forge DocLink resolves the slug from the connection's forge type.
`/docs` routes are public (`App.tsx:74-75`, unauthenticated), so links from
protected/admin pages never hit a login wall; the flip side is that the
admin-oriented guides (`admin-settings`, `rate-limits`) are already publicly
readable, and linking them from admin pages creates no new exposure.

## Design Decisions

1. **One `DocLink` component, one central slug registry.** A
   `web/src/lib/doclinks.ts` exports named slug constants (e.g.
   `DOC_WORKER_SETUP = "worker-setup"`, plus the three forge-setup slugs
   `DOC_BOT_SETUP_GITLAB` / `_FORGEJO` / `_GITHUB` consumed by the existing
   `botSetupDoc(forgeType)` helper); a `DocLink` component
   (`web/src/components/DocLink.tsx`) takes a slug + children and renders the
   react-router `Link` to `/docs/<slug>` with the existing
   `text-brand hover:underline` idiom. Pages never hand-write `/docs/…`
   strings. Two existing links migrate to it: the ForgeSettings error-card
   link (`botSetupDoc(forgeType)` at `ForgeSettings.tsx:249`, forge-aware —
   keep it forge-aware, sourcing its slugs from the registry) and the
   `AnthropicTokens.tsx` `DOC_URL` external **GitHub**-blob link to
   `anthropic-token.md` (`AnthropicTokens.tsx:17`) — the in-app route is the
   same content, works offline, and drops the `target="_blank"` hop.
   (Checked `inspiration/` per convention: no comparable bundled-docs SPA
   link registry exists in bottega/multica/dot-agent-deck — this is a
   uzi-specific design.)
2. **Slug validity is a unit test, not a convention.** `check-docs.mjs`
   validates the docs side (frontmatter, orders, relative links) but nothing
   validates SPA → docs references; a renamed doc file would leave a stale
   link landing on DocPage's soft not-found (`DocPage.tsx:21`). A vitest test
   asserts every registry constant is a slug returned by `listUserDocs()` —
   renaming or de-`user`-ing a doc breaks `npm test` instead of a live link.
   The test must also assert `listUserDocs()` is non-empty (it is the first
   test to depend on the `import.meta.glob` doc map populating under vitest;
   an empty map must not pass vacuously).
3. **Always-visible placement, one sentence, in the card intro.** The link
   belongs in the descriptive paragraph the user reads before acting
   ("Step-by-step instructions are in the **bot setup** guide." — the guide
   for the connected forge), not behind an icon, tooltip, or error state. Error/help states may add a
   second, more targeted link (the over-privileged card keeps its own), but
   the happy path must carry one too — that was the gap. Accessibility falls
   out of the pattern: the guide title is the link text (never "click here"),
   same-tab react-router navigation, no `target="_blank"`.
4. **In-app links only, no deep anchors.** Targets are `/docs/<slug>` routes
   (client-side navigation, works offline with the bundled docs). Linking to
   `#section` anchors inside a guide is out of scope until DocPage heading
   anchors are a proven need.

## Milestones

- [x] **M1 — Link infrastructure**: `DocLink` component + `doclinks.ts` slug
  registry (including the three forge-setup constants) + slug-validity vitest
  (incl. the non-empty guard); ForgeSettings' existing forge-aware error-card
  link migrated to source its slug from the registry, staying forge-aware.
  `task typecheck:web` and `task test:web` green.
- [x] **M2 — Settings surfaces** (the original ask): always-visible guide
  links on the Forge (forge-aware), Workers, Account & token, and Slack cards,
  per the mapping table — Account & token and Slack are child components
  rendered in `Settings.tsx` (`AnthropicTokens` and `SlackNotifications`
  respectively), and the existing `DOC_URL` external GitHub-blob link in
  `AnthropicTokens.tsx` is replaced by the in-app DocLink; page tests
  (`Settings.test.tsx`, `ForgeSettings.test.tsx`) updated to assert the links
  render.
- [x] **M3 — Management surfaces**: Repos/boards home, Agents, AgentNew, Skills
  (AgentNew gained a minimal test, as it had none).
- [x] **M4 — Admin surfaces**: Tool allowlist, Admin settings, Admin rate
  limits (via widening `AdminShell`'s `description` prop to `ReactNode`).
- [ ] **M5 — Convention recorded + shipped**: `docs/dev-conventions.md`
  documents the DocLink registry + test contract (new UI surface ⇒ consider
  a guide link); full `cd web && npm run build` (check-docs + typecheck +
  vite) green. Ships with the next tagged release (Model B, cut per
  `.claude/agents/release.md`); verify one link live on dev-cluster after it
  rolls out. **In-branch parts done** (dev-conventions.md section added; full
  web build green); the checkbox stays open for the post-merge release cut and
  the live dev-cluster verification, which happen after this branch merges.

## Success criteria

- Every surface in the mapping table shows its guide link in the default
  (non-error) state.
- Zero hand-written doc-link strings in pages — neither `/docs/…` routes nor
  external GitHub-blob URLs to `docs/*.md` (registry only; grep-clean outside
  `doclinks.ts`, the docs pages themselves, and tests).
- Renaming a linked doc file fails `npm test` before it can ship a 404.

## Out of scope

- Board, Chat, and run-view contextual help (those pages are task surfaces,
  not setup surfaces; revisit if users ask).
- Deep-linking to sections within a guide (needs DocPage heading anchors).
- Any backend/API change, and any docs content change.
