# PRD #16: Agent Skills (builtin / global / user / repo) + first CICD skill

**GitLab Issue**: [vtmocanu/uzi#16](https://github.com/vtmocanu/uzi/-/issues/16)
**Status**: Complete (2026-07-05, MR !22 merged; all milestones M1–M7 done, final audit clean)
**Priority**: High
**Created**: 2026-07-05
**Depends on**: PRD #3 (agent templates, done) for the storage/reconciler pattern and the templates the skills attach to; PRD #4 (agent runtime + workers, done) for the claim payload, SDK executor, and guardrail layers this extends; PRD #17 (runs first) for the decoupled builtins convention — `api/internal/*/builtins/` as single source of truth, parse/validity tests instead of golden byte-match vs `.claude/`, which this PRD's `skilltmpl` adopts from day one.

## Problem

plan.md line 44: "we should be able to define skills global skills and also each user should be able to define skill for his aagent, and he should be able to associate/allocate global or user skills to each agent (pod/vm)". Today an agent's entire knowledge is its template prompt plus whatever it reads in the clone. There is no way to give agents reusable, curated domain knowledge — playbooks like "how CI/CD works at example" — without bloating every template's system prompt with content only some runs need. Skills are the SDK-native answer: named markdown playbooks whose name+description sit cheaply in context and whose body loads only when relevant (progressive disclosure).

## Solution Overview

Skills stored in DB with three server scopes (**builtin** — shipped with uzi, seeded like builtin agent templates; **global** — admin-defined, visible to all; **user** — self-service, visible to the owner), allocatable to agent templates (shared allocations by admins, per-user overlay allocations by each user), delivered to the worker inside the claim payload, and loaded by the worker through a **local SDK plugin directory it synthesizes outside the clone** — because `settingSources: []` (the repo-borne prompt-injection defense) means filesystem skill discovery is off and must stay off. A fourth, opt-in source: **repo skills** (`.claude/skills/*/SKILL.md` inside the cloned repo), loaded only when the repo's owner has explicitly enabled it — skills only, never the repo's hooks/settings/commands.

Ships with the first builtin skill, **`ci-cd-norms`** (authored in this PRD, source of truth at `api/internal/skilltmpl/builtins/ci-cd-norms/SKILL.md` — the single home, per PRD #17's decoupled convention; no `.claude/skills/` mirror): the example CI/CD norm (`shared-pipelines` includes + ArgoCD GitOps via `argo-apps`), how to detect repos that deviate, and example-app as the worked exception.

### SDK mechanics (verified against the installed `@anthropic-ai/claude-agent-sdk`, 2026-07-05)

- `settingSources: []` blocks all filesystem skill discovery (`~/.claude/skills`, `<cwd>/.claude/skills`) — intentional, unchanged (`SettingSource` covers only user/project/local settings, sdk.d.ts:6281).
- `plugins: [{ type: 'local', path }]` loads a plugin's skills/agents/hooks (sdk.d.ts:1714) — our delivery channel. Independence from `settingSources` is an inference from the type model (plugins are a separate top-level option outside `SettingSource`; plugin customizations are explicitly exempt from the lockdown knob, sdk.d.ts:5125), confirmed behaviorally by the M4 test, not assumed.
- Top-level `skills: string[] | 'all'` is an explicit enable-list (context filter; field at sdk.d.ts:1889), plugin skills addressable as `plugin:skill` (doc comment, sdk.d.ts:1876). **Omitting it is not "skills off"** (sdk.d.ts:1872-1873) — so the worker always passes an explicit list, never omits.
- `AgentDefinition.skills?: string[]` exists (sdk.d.ts:67) — per-subagent allocation maps 1:1 onto the SDK. The SDK docs are ambiguous on whether the top-level filter also gates subagents (sdk.d.ts:1879 vs :3297) — resolved by the M4 integration test, see Worker §2.

### Inspiration check (per plan.md, audited 2026-07-05)

| Concern | bottega does | multica does | dot-agent-deck does | uzi will do |
|---|---|---|---|---|
| Skills storage | Nothing | **Structured skills**: workspace-scoped `skill` + `skill_file` (multi-file) + `agent_skill` join (`server/migrations/008_structured_skills.up.sql`); SKILL.md open standard; hub imports | Repo-local `.claude/skills` for its own tooling only — not a product feature | DB `skills` with builtin/global/user scopes + allocation overlay; builtins Go-embedded from `skilltmpl/builtins/` with parse/validity tests (PRD #17's decoupled convention) |
| Delivery to agent | N/A | Materializes into `{workDir}/.claude/skills/` etc. per provider for **native discovery** (`server/internal/daemon/execenv/context.go:31-48`) — requires project-settings loading to be ON | N/A | Synthesized **local plugin dir outside the clone** + explicit `skills` enable-list — works with `settingSources: []`, so the injection defense never loosens; native-discovery would have forced us to weaken it (the multica weakness to avoid) |
| Repo-borne skills | N/A | Workdir materialization only (server-pushed); repo's own skills load implicitly wherever native discovery sees them | Its whole model — but it's a dev-tool repo, trusted by definition | Explicit per-repo opt-in, default off, skills-only (hooks/settings/commands from the repo never load) |
| Per-agent allocation | N/A | `agent_skill` many-to-many | N/A | Same shape + per-user overlay (multica is workspace-flat; uzi runs belong to users with private skills) |

## Technical Design

### Storage (migration drafted as `00050`+ — final numbers assigned at merge time, next free above the live head, per the CLAUDE.md convention; draft ledger in PRD #18 §Technical Design)

```sql
skills (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name        TEXT NOT NULL,        -- kebab-case ^[a-z0-9][a-z0-9-]{0,63}$; immutable after creation (skill identity)
  description TEXT NOT NULL,        -- one line; always in context — this is what the model routes on
  body        TEXT NOT NULL,        -- SKILL.md markdown body (frontmatter synthesized at delivery, never stored)
  scope       TEXT NOT NULL CHECK (scope IN ('builtin','global','user')),
  user_id     UUID REFERENCES users (id) ON DELETE CASCADE,
  updated_by  UUID REFERENCES users (id) ON DELETE SET NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK ((scope = 'user') = (user_id IS NOT NULL))
);
CREATE UNIQUE INDEX uq_skills_shared_name ON skills (name) WHERE scope <> 'user';
CREATE UNIQUE INDEX uq_skills_user_name   ON skills (user_id, name) WHERE scope = 'user';

agent_skill_allocations (
  template_id UUID NOT NULL REFERENCES agent_templates (id) ON DELETE CASCADE,
  skill_id    UUID NOT NULL REFERENCES skills (id) ON DELETE CASCADE,
  user_id     UUID REFERENCES users (id) ON DELETE CASCADE,  -- NULL = shared (admin-managed); else that user's private overlay
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- intentionally no surrogate PK: uq_allocations is the row identity
CREATE UNIQUE INDEX uq_allocations ON agent_skill_allocations
  (template_id, skill_id, COALESCE(user_id, '00000000-0000-0000-0000-000000000000'));
```

- Builtin rows: seeded/repaired by the same startup-reconciler semantics as agent templates (PRD #3) — editable, resettable, never deletable. Source of truth: `api/internal/skilltmpl/builtins/<name>/SKILL.md`, Go-embedded, the **single** home — PRD #17 (done first) dissolved the builtins ↔ `.claude/` mirror convention for `agenttmpl`, and skills adopt the decoupled convention from day one: no `.claude/skills/` copy (a product skill there would load into the dev team's own Claude Code sessions — the same masquerade #17 evicted `lead.md` for), no golden byte-match test. Instead: parse/validity tests over the embedded files (frontmatter parses, name matches the regex, description non-empty single-line, names unique). Deliberate divergence from `agent_templates.is_builtin`: builtin-ness is a `scope` value here, the reconciler keys on `(name, scope='builtin')`, and `uq_skills_shared_name` means a builtin and a global can never share a name — intended, do not "fix".
- Allocation rules, enforced server-side: shared rows (`user_id NULL`) are admin-only and may reference only builtin/global skills; user rows are owner-only and may reference builtin/global skills or the owner's own user skills. `repos` gains `repo_skills_enabled BOOLEAN NOT NULL DEFAULT false` in the same migration range.

### API

**Read authz is NOT the agent-templates pattern** (templates are all-shared; skills are not — copying that handler verbatim would leak private skills): every read returns builtin ∪ global ∪ **the caller's own** user skills, nothing else. Admins see all scopes (they administer the system and can read the DB anyway — honest, not a leak). Allocation reads follow the same rule: `GET` on a template's skills returns the shared rows plus the caller's overlay rows only — never another user's overlay rows nor the names behind them. Pinned by authz tests (Success Criteria).

- `GET/POST /api/skills`, `GET/PUT/DELETE /api/skills/{id}`, `POST /api/skills/{id}/reset` (builtin only). Create/edit of `global` scope = admin; `user` scope = owner. `name` immutable after creation; server validates the name regex, single-line description, and `SKILL_MAX_BYTES` on body (per-skill checks only — the per-run count cap cannot be enforced at save time, see Claim assembly).
- `PUT /api/agent-templates/{id}/skills` — replace-set semantics, split into shared (admin) and mine (any user) halves.
- `PATCH /api/repos/{id}` gains `repo_skills_enabled` (repo owner or admin).

### Claim assembly (`api/internal/workersvc`)

For the claiming run's user: per template, allocated skills = shared rows ∪ that user's overlay rows. `ClaimPayload` gains `skills: [{name, description, body}]` (the union, deduplicated) and each `ClaimAgent` gains `skills: [names]` (that template's allocation). `ClaimRepo` gains `skills_enabled bool`; `ClaimConfig` gains `skill_max_bytes` and `skills_max_per_run` so the worker enforces the same caps the server configured (no hardcoded drift). **Name-collision precedence** at assembly: user > global > builtin (a shadowed shared skill is dropped from the union); repo skills are resolved worker-side and rank below everything (collision ⇒ repo skill skipped). **`SKILLS_MAX_PER_RUN` is enforced here, at assembly** — the per-run union spans all templates (every template ships in every claim, `service.go:310`), so no single allocation save can see the whole picture; overflow is dropped lowest-precedence-first. All assembly-time drops (shadowed + overflow) ride the claim as `skills_dropped: [{name, reason}]` and the **worker** emits the corresponding run-message log lines — the worker owns the gapless per-run `seq` (ARCHITECTURE.md), so the server never writes `run_messages` itself. Skills re-assemble on every claim including resume (a skill deleted between claim and resume disappears from the resumed session even if the approved plan references it — accepted, one log line). Both directions of the wire shape land under a cross-side contract test (the PRD #4 M1+M2 lenient-fakes lesson).

### Worker (`agent/src`)

1. Materialize a plugin dir **outside the clone** (sibling of the worktree, never inside it): `.claude-plugin/plugin.json` (`{"name": "uzi"}`) + `skills/<name>/SKILL.md` per claim skill. The `name:`/`description:` frontmatter is synthesized as **quoted, fully-escaped YAML scalars** — not just newline-stripped: `:`,`#`,`|`,`>`,`&`,`*`,`!`, leading spaces, and `---` sequences must all be inert (frontmatter-injection guard; the M4 test covers each metacharacter class, not only the newline/`---` cases). The dir is rebuilt from the claim on **every** claim, including resume — the M4 test also asserts that a resumed SDK session (`options.resume`) re-applies `plugins`/`skills` rather than baking them into the original session.
2. `query()` options gain `plugins: [{type: 'local', path}]` and an **always-explicit** top-level `skills:` list — never omitted (omission is not "skills off" either: the CLI's own defaults still apply, sdk.d.ts:1872-1873) and never `'all'` (which is what enables every discovered skill, sdk.d.ts:1874). After PRD #17 (runs first) a `lead` builtin exists and `assembleAgents` (`agent/src/agents.ts`) routes it to the **main thread** — the lead is the main session, not a subagent, so its allocation has no `AgentDefinition.skills` slot to map onto. The top-level list is the **full claim union**: correct under either reading of the SDK's subagent-gating ambiguity (below) and it gives the orchestrator visibility. Consequence, documented in `docs/skills.md`: allocating a skill specifically to `lead` is subsumed by the union (it only affects union membership, not lead-exclusive scoping). Per-template scoping for subagents happens via each `AgentDefinition.skills`. The SDK docs conflict on whether the top-level filter also gates subagents (sdk.d.ts:1879 "rejected by the Skill tool" vs :3297 "main session only") — passing the full union is correct under either reading; the M4 integration test pins the actual behavior, including that a **tools-restricted subagent** (reviewer/tester allowlists exclude many tools) can still load its allocated skill's body — if skill expansion requires a `Skill` tool grant, the assembler must add it for skill-bearing templates rather than silently shipping listing-only skills. **RESOLVED NOT NEEDED** — see Decision Log 2026-07-05 (SDK-verified): `sdk.d.ts:44` marks a tools-`'Skill'` grant deprecated; `AgentDefinition.skills` is the single enable switch, no allowlist was widened.
3. **Repo skills** (only when `repo.skills_enabled`): after checkout, enumerate `<clone>/.claude/skills/*/SKILL.md`, parse only `name` + `description` from the frontmatter, **drop every other frontmatter key** (`allowed-tools` and friends grant capabilities — stripping them is the security point; server-stored skills get the identical treatment structurally, since the DB stores body-only), re-synthesize escaped frontmatter, apply the same name regex + caps, and place at lowest precedence (collisions skipped + logged into run messages). Nothing else under the repo's `.claude/` is ever read for loading: no hooks, no settings, no commands, no CLAUDE.md. Repo skills are enabled for **all** templates in the run (they carry no allocation). Composition with the `Skill`-grant rule (§2): if skill expansion requires a `Skill` tool grant, enabling repo skills widens **every** template's allowlist for that run, including the read-only ones (reviewer/tester) — intended, stated here so it isn't rediscovered as a surprise. **MOOT** for the same reason as §2 above — see Decision Log 2026-07-05.
4. Config: `SKILL_MAX_BYTES` (default 65536), `SKILLS_MAX_PER_RUN` (default 32) — server env, delivered to the worker via `ClaimConfig`; the server applies the size check at save and the count cap at claim assembly; the worker applies the size check to repo skills and re-enforces `SKILLS_MAX_PER_RUN` over the **combined** delivered ∪ repo set (delivered DB skills count against the cap; repo skills, lowest precedence, drop first) — a run can never exceed the cap, not reach 2× it.

### Trust model (why repo skills are opt-in, default off)

Repo content splits into data (source files the agent reads — already handled as untrusted) and config (things the harness auto-loads with privileged framing). `settingSources: []` is the guardrail layer keeping the config class closed; skills are the mildest member of that class but still get trusted-affordance framing the model is trained to invoke. Repo skills are trustworthy exactly when the repo's MR review discipline is — which uzi cannot know for arbitrary connected repos. So the repo owner asserts it per repo, uzi loads skills-only through its own controlled channel, and the other three guardrail layers stay untouched. This does not weaken any existing layer: a hostile repo skill still cannot push (worker holds the PAT), still hits the `PreToolUse` deny-hook, and still cannot load hooks/settings.

### First builtin skill: `ci-cd-norms`

Authored with this PRD at `api/internal/skilltmpl/builtins/ci-cd-norms/SKILL.md` (researched 2026-07-05 from the internal KB `ci-pipeline.md`, `deployments.md`, and the example-app repo + its `argo-apps` app). Structure: the **default norm** (thin `.gitlab-ci.yml` including a bundle from private `shared-pipelines`; lint→build→audit→push→cleanup with `SKIP_*` toggles; Harbor `registry.example.com` for images + OCI charts, sha/version/latest tags; **CI never deploys** — ArgoCD app-of-apps in `argo-apps` does; secrets via Infisical operator), the **exception-detection rule** (no `include:` of `shared-pipelines` ⇒ the repo is an exception; follow its local convention, never "normalize" it unasked), **example-app as the worked exception** (hand-rolled DAG pipeline, kaniko with protected-ref-only cache writes, tag-only publishing of 4 artifacts, chart-in-repo consumed as Harbor OCI via multi-source ArgoCD app, fully manual `targetRevision` release ritual), and a **verify-live list** for facts the KB doesn't pin (bundle contents, pinned tool versions, push-credential var names).

### Web UI

1. **Skills page** (`/skills`): three groups (Builtin / Global / Mine); create/edit (markdown body editor, name locked after create); builtin rows show Edit + Reset, never Delete; admin sees Global create, everyone sees Mine.
2. **Agent template detail** (`AgentDetail.tsx`): allocation panel — shared allocations (admin-editable) and "my skills for this agent" (self-service), rendered as the union the user's runs will actually get.
3. **Repos page**: "Load repo skills" toggle per repo with explicit warning copy (what it trusts, what it still never loads).
4. Responsive, component tests, same as everything since PRD #2.

## User Journey

1. Admin opens Skills, sees `ci-cd-norms` under Builtin, and allocates it (shared) to the `coder` and `reviewer` templates.
2. A run starts on a repo whose pipeline must be extended. The lead's coder subagent sees the skill's one-line description in context, pulls the body, and adds the new job by editing the thin `.gitlab-ci.yml` include-vars — instead of hand-rolling a pipeline that fights `shared-pipelines`.
3. The same team later works on example-app: the skill's exception rule fires (no `include:`), the agent follows example-app's kaniko/tag-only conventions and leaves the release ritual manual, as documented.
4. A user writes a private `qdrant-kb` skill for their own agent, allocates it to their `coder` overlay — other users' runs never see it.
5. A user flips "Load repo skills" on their own well-reviewed repo; its `.claude/skills/deploy-notes/SKILL.md` starts appearing in runs on that repo only. On every other repo the flag stays off and a hostile `.claude/` keeps loading nothing.

## Milestones

- [ ] **M1 — Store + builtin bundling**: migration `00050`+ (`skills`, `agent_skill_allocations`, `repos.repo_skills_enabled`); `api/internal/skilltmpl` (embed + parse + parse/validity tests over the embedded files — no `.claude/` mirror, per PRD #17); startup reconciler seeding `ci-cd-norms`; sqlc queries; the skill file itself lands here (already drafted with this PRD at `.claude/skills/ci-cd-norms/SKILL.md` — M1 **moves** it into `skilltmpl/builtins/`, deleting the `.claude/skills/` copy).
- [ ] **M2 — API: CRUD + allocations**: skills endpoints with scope authz (admin/global, owner/user, builtin reset-not-delete); template allocation endpoint (shared vs mine halves); repo flag PATCH; validation (name regex, single-line description, size caps); handler tests.
- [ ] **M3 — Claim assembly + wire contract**: union/dedup/precedence logic; `ClaimPayload.skills` + `ClaimPayload.skills_dropped`, `ClaimAgent.skills`, `ClaimRepo.skills_enabled`; cross-side contract test pinning the shapes.
- [ ] **M4 — Worker: plugin delivery**: plugin-dir synthesis with frontmatter-injection guard (full YAML-metacharacter matrix); `plugins` + explicit top-level `skills` + per-`AgentDefinition.skills` wiring; caps from `ClaimConfig`; guardrail suite extended (skill bodies are data, `settingSources` still `[]`); **one integration test carrying four load-bearing assertions**: plugin-qualifier naming, plugin skills actually load under `settingSources: []`, a tools-restricted subagent can expand its allocated skill, and resume re-applies `plugins`/`skills`. To the extent the fake-`queryFn` seam can't prove live loading in CI, ship a manual/opt-in live check and record the residual in the risk register — not silently.
- [ ] **M5 — Web UI**: Skills page, allocation panel, repo toggle with warning copy; vitest coverage. *(Parallel-safe with M3–M4 once M2 merges.)*
- [ ] **M6 — Repo skills opt-in, end to end**: worker enumeration/parsing/caps/precedence; hostile-repo guardrail test (repo ships malicious `.claude/settings.json` + hooks + skills: flag off ⇒ nothing loads; flag on ⇒ only skills load, hooks/settings never).
- [ ] **M7 — Docs + E2E + audit**: `docs/skills.md` (audience: user — creating, allocating, repo opt-in trade-off, authoring guidance); ARCHITECTURE.md skills section; e2e scenario through stub executor (skill delivered, plugin dir synthesized, repo-skill opt-in path); auditor pass focused on the frontmatter-injection guard and the repo-skills trust boundary.

### Phasing & parallel-safety

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 | M1 | — | migrations, `skilltmpl/` (incl. embedded skill file), store |
| 2 | M2 → M3 | M1 | `handler/`, `workersvc/` |
| 3 (parallel) | M4 ∥ M5 | M3 / M2 | `agent/src/` ∥ `web/src/` — disjoint trees, two agents can run concurrently |
| 4 | M6 | M4 | `agent/src/`, small server bits |
| 5 | M7 | all | `docs/`, `e2e/` |

Merge points with PRD #17 (runs first): `api/internal/workersvc/claim.go` `ClaimConfig` (#17 adds `default_model`; M3 adds `skill_max_bytes`/`skills_max_per_run`) and `agent/src/sdk-executor.ts` + `protocol.ts` (#17 rewrites the model resolution and the ClaimConfig type; M4 adds `plugins`/top-level `skills`/per-`AgentDefinition.skills` wiring). M3/M4 rebase onto #17's landed shapes — pure additions, but the same files.

## Success Criteria

- A skill allocated to a template shows up in that template's subagent sessions (and only those), verified by an integration test asserting the SDK receives the right `skills` lists and the plugin dir contains exactly the claim's skills.
- `settingSources` remains `[]` in every configuration, including with repo skills enabled — asserted by the guardrail suite, not by review.
- The hostile-repo test proves: flag off ⇒ zero repo `.claude/` influence; flag on ⇒ skills only, with caps and lowest precedence.
- A user's private skill never appears in another user's run, skill listing, or allocation view (authz + assembly tests — reads are builtin ∪ global ∪ own, never the templates-style all-shared read).
- Parse/validity tests cover every embedded builtin skill (frontmatter parses, name regex, single-line description, unique names); `api/internal/skilltmpl/builtins/` is the only home — nothing lands in `.claude/skills/`.
- Wire shapes covered by a cross-side contract test, not two lenient fakes.
- `ci-cd-norms` contains no invented facts: every claim traces to the internal KB pages or the example-app/argo-apps repos, and known-unverifiable items sit in its verify-live section.

## Risks

- **Frontmatter injection via description/body**: a crafted description could try to break out of the synthesized YAML frontmatter. Mitigation: single-line validation server-side + quoted-and-escaped emission worker-side (full metacharacter matrix, not just newlines) + dedicated tests; body is below the frontmatter, so it cannot redefine it.
- **Plugin-qualifier drift / live-load proof gap**: the exact `plugin:skill` naming and the fact that plugin skills load at all under `settingSources: []` are pinned by the M4 integration test rather than assumed from docs; an SDK upgrade that changes either fails loudly. What CI's fake-`queryFn` seam cannot prove (a real SDK session loading the skill — the testing-credentials policy forbids live sessions in CI) is covered by a documented manual/opt-in check; accepted residual.
- **Context bloat**: each skill costs its name+description in every session. Bounded by `SKILLS_MAX_PER_RUN` and by allocation being per-template (skills aren't global-on by default).
- **Repo-skill social engineering**: a repo owner enabling the flag on a repo whose review discipline is weaker than they think. Mitigated by default-off, loud warning copy, skills-only loading, and the unchanged outer guardrail layers; residual risk documented in `docs/skills.md`.
- **Stale skill content** (`ci-cd-norms` describes infrastructure that will drift): the skill's verify-live section instructs agents to confirm volatile facts; builtin skills are editable in place by admins without a uzi release.

## Out of scope (deferred)

Multi-file skills (multica's `skill_file`; v1 is SKILL.md-only — revisit when a skill actually needs helper scripts); skill import from hubs / the Anthropic skills repo; a skills catalog/marketplace (plan.md later-stuff has the same idea for agents); per-run ad-hoc skill selection (allocation is per-template); auto-detecting which skills a run *should* have had (belongs to plan.md's session-analysis/LLM-judge item); forgejo parity.

## Decision Log

- 2026-07-05 (user): create the skills PRD (plan.md line 44) + author the first builtin skill about example CICD, researched from the internal KB and the example-app repos; example-app is the exception that proves the need for default-vs-exceptions structure in the skill.
- 2026-07-05 (user + AI): repo-borne skills included — user asked "skills can sit in repo and worker detects them, no?"; agreed design is per-repo opt-in, default off, skills-only, lowest precedence, because repo `.claude/` is exactly the config class `settingSources: []` exists to block; the opt-in is the repo owner vouching for the repo's review discipline.
- 2026-07-05 (AI, verified against installed SDK): delivery via local plugin dir + explicit `skills` enable-list + `AgentDefinition.skills` — the only channel that works under `settingSources: []`; multica's native-discovery materialization was rejected because it would require loosening that isolation.
- 2026-07-05 (research): the internal KB documents the norm (`shared-pipelines` includes, Harbor, `argo-apps` app-of-apps; some legacy Argo repos are deprecated per `cnpg.md:35`); example-app's actual deviations verified in-repo (hand-rolled CI, kaniko + protected-ref cache boundary, tag-only 4-artifact publish incl. homebrew tap, manual `targetRevision` ritual, coexisting manual Taskfile deploy path); two initial assumptions corrected — example-app pushes to Harbor (GitLab's container-registry advertisement is unused), and multi-source OCI-chart ArgoCD apps are within the org's spectrum (a sibling app uses the same shape), so the skill frames example-app's CI shape and release ritual as the exception, not its registry or Argo wiring.
- 2026-07-05 (AI, defaults chosen — **revisit on review**): SKILL.md-only v1; per-template allocation with shared + per-user overlay rows in one table; name-collision precedence user > global > builtin > repo with shadowed skills dropped-and-logged; caps 64 KiB / 32 skills; builtin reconciler semantics copied verbatim from agent templates (editable, resettable, not deletable); migration range `00050+` reserved.
- 2026-07-05 (user + AI, ordering vs PRD #17): #17 runs first, so its Decision 1 (dissolve the builtins ↔ `.claude/` mirror; `api/internal/agenttmpl/builtins/` becomes the single source of truth with parse/validity tests instead of golden byte-match) is inherited here from the start: `ci-cd-norms` lives only at `api/internal/skilltmpl/builtins/ci-cd-norms/SKILL.md`, no `.claude/skills/` copy, no golden drift test. Same rationale transfers directly — `.claude/skills/` is a live Claude Code directory for this repo's dev team, so a product skill there would masquerade as (and actually load as) dev tooling. Supersedes the earlier golden-drift wording in this PRD.
- 2026-07-05 (AI, post-draft review wave — design reviewer + fact-checker; fact-check: zero wrong claims, four citation-precision nuances corrected). **Blocker fixed**: skills read authz rewritten — it is NOT the agent-templates all-shared read (copying that handler would leak private user skills); reads return builtin ∪ global ∪ caller's own, allocation reads never expose other users' overlays. **Should-fixes applied**: top-level `skills` is always an explicit list, never omitted (omission silently enables everything, sdk.d.ts:1871) — full claim union at top level (correct under both readings of the conflicting sdk.d.ts:1878/:3297 subagent-gating docs, and necessary since no lead template exists in the builtin set), per-template scoping via `AgentDefinition.skills`; `SKILLS_MAX_PER_RUN` moved from save-time (can't see the cross-template union) to claim assembly, drop-lowest-precedence-and-log; frontmatter guard upgraded from single-line-only to quoted-escaped YAML with a full metacharacter test matrix; caps ride `ClaimConfig` (no server/worker drift); M4 integration test now carries four pinned assertions (qualifier, live-load under `settingSources: []`, tools-restricted subagent can expand its skill — adding a `Skill` tool grant to skill-bearing allowlists if needed, resume re-applies plugins/skills), with the CI-unprovable live-load part as a documented manual check + accepted residual; repo-skill processing restated as parse-name+description / drop-all-other-frontmatter-keys (stripping capability-granting keys like `allowed-tools` is the point, not "body only"); resume-vs-deleted-skill behavior documented as accepted. Builtin-vs-global name exclusivity called out as intended.
- 2026-07-05 (AI, post-decoupling review wave — design review of the #17-aligned draft). **Blocker fixed**: the "no lead template in the builtin set" premise was invalidated by PRD #17 shipping `lead` as the eighth builtin (and #17 runs first) — Worker §2 reframed: the lead is the main thread (`assembleAgents` routes it there), so its allocation has no `AgentDefinition.skills` slot; top-level `skills` stays the full claim union (correct under either SDK subagent-gating reading + orchestrator visibility), with the documented consequence that allocating a skill specifically to `lead` is subsumed by the union. **Should-fixes**: assembly-time drops now ride the claim as `skills_dropped` and the *worker* emits the run-message lines (worker owns the gapless `seq`; the server never writes `run_messages`); `SKILLS_MAX_PER_RUN` re-enforced worker-side over the combined delivered ∪ repo set (was up-to-2×-cap as written); #17 merge points flagged (`ClaimConfig` in `claim.go`, `sdk-executor.ts`/`protocol.ts`). **Nits**: repo-skills-on ⇒ universal `Skill`-grant widening stated as intended; allocations table's no-surrogate-PK noted in the schema; migration-ledger pointer added (clear of #17's `00022`).
- 2026-07-05 (AI, M4 build — SDK-verified, reviewer + auditor). §Worker point 2's open contingency ("if skill expansion requires a `Skill` tool grant, the assembler must add it...") is **resolved as not needed**: the installed `@anthropic-ai/claude-agent-sdk`'s `sdk.d.ts:44` marks a tools-`'Skill'` grant deprecated, and `AgentDefinition.skills` is confirmed the single enable switch by the M4 integration test — a tools-restricted subagent (reviewer/tester) is wired to expand its allocated skill (wiring test: tools allowlist untouched, skills list correct; live body expansion is covered separately by the documented manual check, not this test) with no allowlist change. No allowlist was widened anywhere in the landed code.
- 2026-07-05 (AI, M6 build). §Worker point 3's composition note ("enabling repo skills widens every template's allowlist") is **moot** for the same reason as the point above — there is no `Skill`-grant to widen. Separately, the M6 blocker it was adjacent to is fixed: repo-borne skills carry no allocation, so `assembleAgents` (`agent/src/agents.ts`) now appends the materialized repo-survivor names to **every** subagent's `AgentDefinition.skills`, not just the lead — the top-level `skills` union only covers the main-thread lead session (sdk.d.ts main-session semantics), so without this a repo skill never reached a subagent at all. Delivered (allocated) skills stay per-template; only repo skills are all-templates. Pinned by the hostile-repo test's both-directions assertions (flag on: every subagent lists the valid repo skill, tools allowlists unchanged; flag off: none do) plus the cap-eviction case.
