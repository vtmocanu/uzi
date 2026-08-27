# PRD #602: Admin-configurable agents repo — sync builtin roles from git at runtime

**GitHub Issue**: [#602](https://github.com/vtmocanu/uzi/issues/602)
**Status**: Complete (created 2026-08-22, all milestones M0–M6 shipped 2026-08-23)
**Priority**: Medium
**Drive**: Auto mode (uzi-watcher) — plan reviewed and steered at the approval gate, NOT the unattended sweep. This is a trust-boundary feature; the plan gate is the human checkpoint and the MR review is the security sign-off.
**Related**:
- PRD #85 (`prds/done/85-agenttmpl-role-library-sync.md`) — keeps the *shipped defaults* honest by version-stamping builtins and detecting library drift at build time. This PRD is its runtime superset; #85's vendored manifest generalizes to the synced snapshot here.
- PRD #275 — `RefreshPristineBuiltin` / the `customized` flag: boot re-applies the embedded body to pristine builtin rows. **This PRD extends that discriminator** (see Decision 3).
- PRD #122 M8 — `api/internal/pushbroker/pushbroker.go`, the api's pure-Go go-git client (a push pipeline). The sync reuses the `go-git/v5` dependency but adds a new clone-and-read path (Decision 5).
- Issue #201 — the `differs_from_builtin` admin drift badge. Becomes source-aware here (Decision 11).
- `api/internal/forge` — the `FORGE_ALLOWED_BASE_URLS` SSRF allowlist *pattern*; the source-repo URL uses a **separate** list, `AGENT_SOURCE_ALLOWED_BASE_URLS` (Threat Model).
- `agent/src/repoagents.ts` — the frontmatter/tools/description parser contract the new api-side parser must match (Decision 6).

## Problem

The 12 builtin roles are `go:embed`'d into the api binary and hand-ported from the upstream role library (`vtmocanu/skills` `agent-team/roles.yaml`). #85 makes drift *detectable*, but a human still ports each change, and a self-hoster cannot point uzi at their own role set without editing each template in the admin UI or dropping `.claude/agents/*.md` into every worked repo.

Goal: let an admin configure a git repo that uzi syncs agent role definitions from at runtime — a reconcile loop (configurable interval, ~1h default) plus a manual "Sync now" button plus visible sync status — feeding the existing `agent_templates` store, without weakening any guardrail and without breaking offline/hermetic fresh installs.

## Solution Overview

1. **A configurable source** — an admin setting: repo URL + ref (+ enable toggle + interval), the credential (for a private repo) sealed in `secretbox`, the URL constrained by an SSRF allowlist.
2. **A reconcile job in the api** (new clone-and-read go-git path): clone/fetch the pinned ref → parse the `.md` role files → validate/sanitize each → **stage** a snapshot (fetched SHA + parsed set + computed diff), which an admin then approves to apply. Applying is a provenance-aware upsert that never clobbers an admin-edited row. One idempotent reconcile function, two triggers (interval timer + "Sync now"); apply is a separate, gated step.
3. **A provenance state** extending #275's `customized` so boot-reconcile never fights the sync (Decision 3), and so the #201 drift badge can say *which* source a row differs from.
4. **An approve-before-apply gate** — synced role changes are staged and an admin approves before they reach runs (the primary supply-chain control), never silently applied at reconcile.
5. **Admin UI** — config section, sync-status panel, "Sync now", source-aware provenance/drift badge with a clear "reset to / differs from *which* source" answer.

Explicitly **not** in this PRD: replacing the `go:embed` builtins (they stay as the shipped default + offline fallback, Decision 1); making the embedded builtins *generated* from the repo (Decision 12, a later optimization); the scheduled upstream-refresh CI bot (that is the `local` sibling #601, a `.github/workflows` file).

## Design Decisions

1. **Additive source layer, not replace-the-builtins.** The `go:embed` builtins remain the shipped default and the bootstrap/offline fallback. A fresh install boots with working agents and zero external dependency; the sync is opt-in on top.

2. **Default mode = pinned/embedded, sync OFF, URL EMPTY — no canonical repo is pre-filled (corrected per ADR-0602, 2026-08-23).** A fresh install runs the embedded builtins with `agent_source_enabled=false` and `agent_source_repo_url` **empty**; no canonical product-agents repository exists yet, so this section's original draft text — "the source-repo setting is pre-populated with our canonical product-agents repo, so an admin who wants to follow us flips one toggle" — described something not implementable at ADR-writing time and does not match what M2 built. Pre-filling a URL, once such a repo exists, is a future, separate change, not something M2 does. See [ADR-0602](../../adr/0602-agent-source-repo-sync.md#default-off-empty-url--no-canonical-repo-is-pre-filled) for the full correction. Rationale is otherwise unchanged: preserve offline/hermetic fresh installs, avoid a default-on remote-execution trust posture (role bodies are system prompts for agents that run bash), keep runs reproducible. (Live-follow-by-default was considered and rejected as the default; it remains a per-instance choice via the toggle.)

3. **Provenance is a scope-aware THREE-state extension of #275's `customized`, and this is the load-bearing change — not "just reuse `customized`".** `ReconcileBuiltinTemplates` runs `RefreshPristineBuiltin` at every boot, re-applying the `go:embed`'d body to `scope='builtin' AND customized=false` rows (`api/internal/store/agent_templates_builtins.go`). If a synced body were stored as a pristine builtin row, the next boot would clobber it back to embed — ping-pong. The binary `customized` cannot express "not admin-edited, but owned by the sync, not by embed." So add an explicit **`origin`** to `agent_templates`.

   **`origin` is not builtin-only (corrected per ADR-0602, 2026-08-23) — `synced` is also legal on `scope='global'`.** `agent_templates` already carries `scope` (`builtin`/`global`/`user`, tied to `is_builtin` by a CHECK) and `customized`; `origin` is a *fourth* discriminator that must not fight the other three. This section's original draft table described `origin` as meaningful "ONLY for `scope='builtin'` rows" with every `global`/`user` row `n/a`/unused — that undersold the shape M1 actually built: a synced-only role with no same-named builtin has no embedded default to reset to, so it is stored as a **deletable** `scope='global'` row, and that row needs a legal `origin='synced'` too, not just NULL. Legal combinations:

   | scope | origin | meaning | refreshed by boot? |
   |---|---|---|---|
   | builtin | `embedded` | shipped default, not admin-edited | **yes** (only this) |
   | builtin | `synced` | body overrides the same-named builtin, sourced from sync | no (sync owns it) |
   | builtin | `admin` | admin-edited (was `customized=true`) | no |
   | global | `synced` | a synced-only role with no same-named builtin (deletable) | no |
   | global | `NULL` | an admin-authored global template | no |
   | user | `NULL` | always — provenance tracking never applies to a personal template | no |

   Boot-reconcile refreshes **only `scope='builtin' AND origin='embedded' AND customized=false`** — the `scope='builtin'` predicate the live query already carries stays. The CHECK enforces `origin` non-null on `scope='builtin'`, and additionally permits `origin='synced'` on `scope='global'` (see the corrected M1 milestone below for the exact predicate). This modifies #275 boot machinery deliberately; it is M1.

4. **Precedence: a synced role OVERRIDES the same-named embedded builtin.** This is what makes the feature useful — our own `coder`/`tester` are deliberately adapted, and an admin pointing at their roster expects it to win over the shipped default. Override is what forces the origin work in Decision 3.

5. **Host = the api, reusing the go-git dependency.** `go-git/v5` is already a direct dep in `api/go.mod` (`pushbroker.go`, PRD #122 M8), and the api owns `agent_templates`, holds `secretbox`, is distroless-static (so go-git, not exec), and runs the reconcile path — so the api is the coherent host. Be honest that this is a **new clone-and-read go-git path**: `pushbroker` is a *push* pipeline, not clone-read, and its "the ONE place go-git is used" comment goes stale on landing and must be updated. The dependency is reused; the usage is new.

6. **A NEW api-side parser for external role files, matching the worker's contract exactly (this is a security-relevant parser, not a tweak).** Today two parsers exist: `api/internal/agenttmpl` (Go, strict single-line frontmatter parser — rejects unknown keys, no tools handling, "only ever fed the embedded builtin files") and `agent/src/repoagents.ts` (TS, lenient — the parser uzi already applies to a *user's* cloned `.claude/agents/*.md`). External role files must be parsed by the **same contract `repoagents.ts` uses**, because that is the format the ecosystem already produces. Do not bend the strict builtin parser; add a **new synced-file parse path** in the api that reproduces `repoagents.ts`'s behavior, and **pin the two together with a differential test** (same inputs → same accepted keys, same tool set, same sanitization). Get the control right, precisely as the worker does it:
   - **Tools is a narrow DENYLIST that STRIPS, not an allowlist that rejects.** `repoagents.ts` removes denied tools (`SendMessage`, task tools, etc.) from the role's list and keeps the role; it fails the role only if *every* tool is denied. Reproduce that, not a fail-closed allowlist.
   - **Sanitize the description the same way.** `repoagents.ts` rejects/strips Unicode Cf and bidi (RTL-override) characters in the description. This is load-bearing here: the description feeds Decision 8's admin approval dialog, so an unsanitized bidi payload could disguise what the admin is approving. Omitting this sanitization is a hole in the primary control.

7. **Config = repo URL + ref, default a PINNED tag/SHA.** A floating branch is an explicit opt-in, not the default: a floating default would let an upstream force-push flow into every run at the next reconcile, the weakest posture for a source of agent system prompts.

8. **Approve-before-apply is the primary security control, not the tools line.** Most roles carry `Bash`, and `repoagents.ts` already notes that denying network tools is "theatre" because Bash is full egress — so "a synced role can only narrow capability" is largely moot. The real supply-chain risk is **prompt content steering a Bash-capable agent (injection)**. Therefore a synced change is **staged** and an admin approves it before it reaches any run; it is never applied silently at reconcile. See Threat Model.

9. **Transport = plain git (go-git), never the `npx skills` CLI.** That CLI is a dev-laptop tool with the wrong runtime model and documented footguns; the runtime need is "clone a repo and read files," which go-git already does.

10. **Sync the roster you RUN, not the generic ancestor.** Adapted roles (`coder`, and especially `tester`, ~89% uzi-own) mean syncing from the generic upstream `roles.yaml` would clobber the adaptations. The configured repo holds the bodies to actually run. It is **not** `.claude/agents/` (the dev-team roster, decoupled by design and never a product source).

11. **#201's drift badge becomes source-aware.** `differs_from_builtin` currently compares the stored row to the single embedded `BuiltinByName` def (`api/internal/handler/agent_templates.go`). With a second source the question splits: differs-from-embedded vs differs-from-synced. The computation and the badge/reset UI must name which source. This is a semantic change to a shipped feature, scoped in M5.

12. **Embedded builtins stay hand-maintained (for now).** They are not generated from the repo in this PRD; #85 keeps them honest against upstream. Generating them from the source repo (which would turn #85's drift-*check* into a drift-*fix*) is a deliberate later step, out of scope here.

13. **Fail per-role, gracefully; never fatal at boot.** Unreachable repo / malformed frontmatter / a denied-tool role fails *that role* with a visible status error and leaves the previous good state intact. The reconcile runs pre-listen-safe: a failure never crashes the api, and an unreachable source falls back to last-good (or embedded).

14. **CLI parity (per repo convention) — READ-ONLY (corrected per ADR-0602, 2026-08-23).** A new admin surface implies an `api/cmd/uzi/` check, but the existing `uzi admin` namespace is **read-only by established convention** — `docs/cli.md` documents every other `admin` verb (`users`, `runs`, `workers`, `usage`, `rate-limits`, `cli-tokens`, `guardrail-impact`, `blocked-repos`) as read-only, and states the convention explicitly: every admin *write* stays cookie-only, a web UI action by design. This section's original draft text — `uzi admin agent-source {get,set,sync,status}`, which included two writes (`set`, `sync`) — broke that convention. The corrected shape is `uzi admin agent-source get|status` (read-only); enabling sync, editing the source config, and triggering "Sync now" or an approve-and-apply stay web-only. See [ADR-0602](../../adr/0602-agent-source-repo-sync.md#consequences) for the full correction. Confirmed in M6.

15. **Source format = a repo of `.md` role files, not `roles.yaml`.** The sync source is a git repo of individual role files with `.claude/agents`-style frontmatter — the exact contract `repoagents.ts` parses (Decision 6) — one file per role. It is **not** the upstream library's single `roles.yaml` YAML file: that file is the *ancestor* the shipped builtins are hand-ported *from* (#85's concern), whereas the sync source is the *roster you run* (Decision 10). Keeping the source in the `.md`-per-role shape means one parser contract across per-repo agents, the synced source, and (as snapshots) the builtins.

## Threat Model (security review lands with the M0 ADR + the MR review)

- **Asset**: the role body is a system prompt for an agent with `Bash` + file-edit. Compromise = arbitrary instructions to a code-editing bot on the user's repos.
- **Primary control**: **approve-before-apply** (Decision 8) — no synced change reaches a run without an admin approving the diff. Auto-approve sweeps do not bypass this; it is a separate gate on the *template source*, not on a run's plan.
- **Network**: the source URL is constrained by an **SSRF allowlist** following the `forge` package's pattern but a **separate list** (`AGENT_SOURCE_ALLOWED_BASE_URLS`, https-only) — do not reuse `FORGE_ALLOWED_BASE_URLS`, which would couple the forge host set to the role-source host set. The clone fetch targets the pinned ref; PRD #702 later adds one further egress path — a lightweight ref-advertisement (`git ls-remote`-equivalent, no full clone) to resolve the latest tag / detect updates — behind these same controls (see prds/done/702-agent-source-follow-skills-harness-lift.md).
- **Credential**: a private-repo token is sealed in `secretbox` (distinct from the forge push PAT); it is read-only, scoped to clone.
- **Pinning**: default pinned tag/SHA (Decision 7) so upstream force-pushes do not auto-flow; floating branch is an explicit, logged opt-in.
- **Capability**: the api parser enforces the tools allowlist (Decision 6), fail-closed; a role requesting a denied tool fails validation and is not staged.
- **Blast radius**: a bad sync affects only `origin='synced'` templates; `admin` and `embedded` rows are untouched, and Reset-to-embedded is always available.
- **No guardrail weakened**: the worker still holds the PAT (not the agent), the deny-hook and `settingSources: []` are unchanged. This PRD adds a source of *prompts*, not a source of *tools or credentials*.

## Touchpoints

| Area | File(s) | Change |
|---|---|---|
| Schema | `api/internal/store/migrations/` (new) | add `origin` to `agent_templates` (scope-aware CHECK: non-null on `scope='builtin'`, also permits `synced` on `scope='global'` — Decision 3); backfill builtin rows `admin`/`embedded` by `customized`, non-builtin rows NULL; new `agent_source_staged` table |
| Reconcile | `api/internal/store/agent_templates_builtins.go` | `RefreshPristineBuiltin` guarded to `scope='builtin' AND origin='embedded' AND customized=false` |
| Sync job | `api/internal/agentsource/` (new) | reconcile fn: clone/fetch → parse → validate → stage snapshot; one fn, two triggers. Apply-on-approval is a separate handler path |
| Git | new clone-and-read go-git helper | clone/fetch the source repo; update `pushbroker`'s "ONE place go-git is used" comment |
| Parser | `api/internal/agentsource` (new synced parser) + `agent/src/repoagents.ts` | reproduce the `repoagents.ts` contract in Go (denylist-strip, Cf/bidi description sanitization); a **differential test** pins the two together. `api/internal/agenttmpl` (embedded path) stays strict, unchanged |
| Settings/secrets | admin settings + `api/internal/secretbox` | repo URL + ref + enable + interval; credential sealed + referenced by secret id; `AGENT_SOURCE_ALLOWED_BASE_URLS` SSRF check |
| Handlers | `api/internal/handler/agent_templates.go` + routes | "Sync now", sync status, approve-before-apply, source-aware `differs_from_builtin` |
| Web | `web/src/…` admin templates | config section, status panel, "Sync now", source-aware badge + "reset to/differs from which source" |
| CLI | `api/cmd/uzi/` | `uzi admin agent-source get\|status` — read-only (Decision 14) |
| Docs | `docs/` | admin-settings + a new source-sync page; threat model reference |
| ADR | `adr/0602-agent-source-repo-sync.md` (new) | trust seam + the resolved decisions above |

## Milestones

Dependency: **M0 (ADR) → M1 (origin) → M2/M3 → M4 → M5 → M6.** M1 unblocks everything (no correct upsert without provenance). M2 and M3 can overlap once M1's `origin` lands. Each milestone ships as its own MR.

- [x] **M0 — ADR + threat model.** `adr/0602-agent-source-repo-sync.md` records the trust seam and Decisions 1-14. The security review is this ADR's MR review. No code.
  - Landed: `adr/0602-agent-source-repo-sync.md`, including the two corrections carried into Decisions 2 and 14 above.
- [x] **M1 — `origin` provenance (scope-aware).** Migration adds `origin` to `agent_templates`, **backfilled by scope**: `scope='builtin'` rows → `customized=true ? 'admin' : 'embedded'`; `scope IN ('global','user')` rows → NULL (origin does not apply). **A CHECK enforces `origin` non-null on `scope='builtin'`, and additionally permits `origin='synced'` on `scope='global'`** — not the "non-null iff `scope='builtin'`" shape this bullet originally described (corrected per ADR-0602: a synced-only role with no same-named builtin is stored as a *deletable* global row, not an undeletable builtin, so `global` needs a legal `synced` origin too). `RefreshPristineBuiltin`/`ReconcileBuiltinTemplates` keep their existing `scope='builtin'` predicate and add `AND origin='embedded'` (still `AND customized=false`). Tests: a `synced` builtin row and an `admin` builtin row both survive a boot reconcile; an `embedded` pristine builtin row still refreshes; a `global`/`user` row is untouched and its `origin` stays NULL.
  - Landed: `api/internal/store/migrations/00159_agent_template_origin.sql` with the exact widened predicate above; `api/internal/store/agent_templates_origin_livedb_test.go` (`TestAgentTemplateOriginLiveDB`).
- [x] **M2 — Source config + secrets + SSRF.** Admin setting (URL + ref + enable + interval); the private-repo credential sealed in `secretbox` and referenced by a secret id on the settings row (the pattern other uzi secrets use); URL validated against the **separate** `AGENT_SOURCE_ALLOWED_BASE_URLS` list (https-only). Pinned-ref (tag/SHA) default. No sync yet — config only.
  - Landed: `agent_source_{repo_url,ref,enabled,interval,credential}` keys in `api/internal/settings/settings.go` (URL/enabled default empty/false — no canonical repo pre-filled, per Decision 2 above); `Config.AgentSourceAllowedBaseURLs` / `AgentSourceBaseURLAllowed` in `api/internal/config/config.go`, enforced in the generic settings PUT handler.
- [x] **M3 — The reconcile job (fetch + STAGE only; no apply).** `api/internal/agentsource`: new clone-and-read go-git usage (following the pattern `pushbroker` established — note `pushbroker` is a *push* pipeline, not clone-read, so this is new go-git surface, and its "ONE place go-git is used" comment must be updated) → clone/fetch the pinned ref → parse each `.md` role file via the new synced-file parser (Decision 6, with the differential test) → compute the diff against current templates → **persist a staging snapshot** (fetched SHA + parsed role set + computed diff) in a new table, e.g. `agent_source_staged`. It does **not** write `agent_templates` — apply is M4. Per-role graceful failure (a bad role is recorded in the snapshot with its error, others proceed); unreachable source → keep last-good, never fatal at boot. One idempotent reconcile fn; interval-timer trigger.
  - Landed: `api/internal/agentsource/{git.go,parser.go,reconcile.go}` + `api/internal/store/migrations/00160_agent_source_staged.sql`; redirect re-check against the allowlist and clone wire-size bound added in the M3b hardening pass (see the ADR's Threat model).
- [x] **M4 — "Sync now" + status + approve-and-apply gate.** The manual trigger (same reconcile fn as M3), a status surface (last-synced time/SHA, ok/error, N roles staged/changed/failed), and the **approve-and-apply** step: an admin reviews the staged diff and approves, and only then does the **provenance-aware upsert** run — a synced role that shares a name with a builtin **UPDATEs that `scope='builtin'` row** (sets `origin='synced'`, never a second INSERT that would hit the unique-name index); a synced-only **new** role is inserted **and given an `agent_template_allocations` row**, because allocation — not table presence — is what makes a template actually run. Nothing reaches a run before approval.
  - Landed: `api/internal/agentsource/apply.go` (`Reconciler.Apply` — re-reads templates at apply time and re-classifies via the shared `computeDiff`, executes the four cases + de-provisioning in ONE transaction, records `agent_source_last_applied_{at,sha}`); new store queries `ApplySyncedOverrideBuiltin`/`InsertSyncedGlobalTemplate`/`UpdateSyncedGlobalTemplate`/`DeleteSyncedGlobalTemplate` (sqlc regenerated); endpoints `GET /admin/agent-source` (RequireAdminRO), `POST /admin/agent-source/{sync,apply}` (cookie-only RequireAdmin) in `api/internal/handler/agent_source.go`; the staged-role body is display-sanitized in the DTO while Apply writes the raw body; `*agentsource.Reconciler` wired into the handler in `cmd/server/main.go`. Applied-tracking via the `agent_source_last_applied_sha` engine setting (no new migration). Store/handler `...LiveDB` apply tests + `planApply` unit tests cover all four cases + de-provisioning + the sanitization asymmetry.
- [x] **M5 — Admin UI + source-aware drift.** Config section, status panel, "Sync now", provenance badge (`embedded`/`synced`/`admin`), and `differs_from_builtin` made source-aware with a clear "reset to / differs from which source". `web` build (check-docs) green.
  - Landed: `web/src/pages/AdminSettings.tsx` (`AgentSourceSettingsCard` + `AgentSourceStagedReview` — config form, sync-status panel, "Sync now", staged-diff review with an always-visible "hidden formatting characters were removed" honesty flag when the server's display-sanitization changed a role body, and **Approve & apply**); `web/src/lib/agentTemplates.ts` (`provenanceBadgeKind`/`templateOrigin`/`SYNCED_BADGE_*` — a `synced` chip shown on `Agents.tsx`/`AgentDetail.tsx` in place of the drift chip for a synced-origin row, so `embedded`/`admin` origins carry no distinct chip of their own beyond the existing "differs from shipped" drift badge).
- [x] **M6 — CLI + docs.** `uzi admin agent-source get|status` (read-only, per the corrected Decision 14 above); `docs/` admin-settings + a source-sync page + the threat-model reference; CLI-parity check per convention.
  - Landed (CLI): `newAdminAgentSourceCmd` in `api/cmd/uzi/admin.go` (a container group with two read-only leaves, `get`/`status`, no write verb); `Client.AdminAgentSource` / `apitypes.AgentSourceDTO` (+ nested config/status/staged types) in `api/internal/uzicli/client.go` and `api/internal/apitypes/agent_source.go`; `FakeClient.AgentSourceV` in `api/internal/uzicli/fake.go`; documented in `api/internal/uzicli/skill/SKILL.md` (command-ref block + prose bullet, satisfying `skill_drift_test.go`) and in `docs/cli.md`. Covered by `TestAdminAgentSourceGetAndStatus` (`api/cmd/uzi/commands_test.go`) — `--- PASS`.
  - Landed (docs): `docs/agent-source.md` (new page), `docs/admin-settings.md` and `docs/agent-templates.md` extended, `docs/configuration.md` gets the `AGENT_SOURCE_ALLOWED_BASE_URLS` operator entry, `docs/cli.md` documents the two new leaves, `ARCHITECTURE.md` gets a pointer to `api/internal/agentsource` — all linking `adr/0602-agent-source-repo-sync.md` for the trust model. `node web/scripts/check-docs.mjs` exits 0.

## Out of Scope

- Replacing the `go:embed` builtins (Decision 1). They stay as default + fallback.
- Generating the embedded builtins from the source repo (Decision 12) — a later step.
- The scheduled upstream-refresh CI bot — that is the `local` sibling **#601** (a `.github/workflows` file the worker PAT cannot push).
- Any change under `.github/workflows/**` — see Scope Guard.

## Scope Guard (this PRD goes to uzi)

Neither the implementation nor the validation may create, modify, or commit any file under `.github/workflows/**` (the worker PAT lacks `workflow` scope; such a diff is an atomic push rejection that loses the branch — `.claude/rules/prds.md`). The reconcile job is exercised with a fixture/stub source repo (an in-tree temp git repo or an in-memory go-git remote), never a real workflow file. Before finalize, `git diff --name-only <base>..HEAD` must show zero `.github/workflows/**` entries.

## Validation

- `cd api && go test ./...` with `UZI_TEST_DATABASE_URL` set for the store tests; the M1 reconcile-provenance tests must appear as `--- PASS` with `RUN > 0`, not skipped.
- M1: a `synced` and an `admin` row both survive a boot `ReconcileBuiltinTemplates`; an `embedded` pristine row still refreshes to the shipped body.
- M3: reconcile against a fixture source repo (temp/in-memory) stages a role; a role with one denied tool has that tool **stripped** and is kept; a role with **all** tools denied is skipped (fail-closed); a description carrying a Cf/bidi character is sanitized; the differential test shows the api parser and `repoagents.ts` agree on the same inputs; an unreachable source keeps last-good without crashing.
- M4: "Sync now" and the interval trigger call the same reconcile fn and produce identical status/staging; approving a staged snapshot applies it (builtin-name override UPDATEs the row to `origin='synced'`; a new role also gets an allocation); an unapproved staged change never reaches a run.
- M5: `cd web && npm run build` (check-docs) green; the badge names the source.
- Guardrails unchanged: worker holds the PAT, deny-hook and `settingSources: []` intact.
