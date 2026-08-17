# PRD #18: Worker Templates (git-curated) + Devbox Tool Tiers + Agent Template Scopes

**GitLab Issue**: [#18](https://github.com/vtmocanu/uzi/-/issues/18)
**Status**: Complete (2026-07-10, MR !32)
**Priority**: Medium
**Created**: 2026-07-05

**Depends on**: PRD #4 (worker runtime, done) for the register/claim protocol and `agent/Dockerfile`; PRD #16 (agent skills) for the scope+allocation pattern and claim-payload delivery this PRD reuses for both tool profiles and agent template scopes; PRD #17 (lead template) for the claim plumbing precedent (`ClaimConfig` extension) and the ModelSelect UI component. Note #16 and #17 are both Draft — their schemas/components referenced here are design precedents, not existing code; **M6–M7 of this PRD are blocked on PRD #16's skills schema + allocation UI landing first** (or, if this PRD starts first, the shared shapes land here and #16 mirrors them — decide at `/prd-start`). See also the reconciler coupling with #17 in Technical Design §4.

## Problem

Three related gaps, all instances of "one size fits all":

1. **One worker image for everyone.** `agent/Dockerfile` is a single fixed image (node 22 + git + bash). A user whose repos need Java, kubectl, or terraform has no supported path — plan.md line 81 asks for exactly this ("one might need node tools, other might need java tools"). Users *could* hand-roll a derived image today (the server never verifies which image connects), but that path is undocumented, unauditable, and drifts.
2. **No per-repo toolchains.** Even with a better image, tools really belong to the *work*, not the person: two users' workers picking up the same issue should behave identically. There is no provisioning mechanism, and `settingSources: []` plus the guardrail posture mean any mechanism must be worker-controlled, not repo-controlled by default. Related: plan.md line 64 wants "command not found" surfaced — pointless until tools are configurable at all.
3. **Agent templates are flat.** `agent_templates` (migration `00011`) is a single global namespace: `name UNIQUE`, `is_builtin` flag, no `user_id`, no scope, no allocation. Every template is admin-managed and rides every claim for every user. plan.md line 44 asks for global/user scoping with per-agent allocation for *skills*; PRD #16 designs that — but the same need exists for the templates themselves, and today a user cannot define a private agent nor deselect a global one.

## Inspiration check

| Aspect | bottega | multica | dot-agent-deck | coder (+ myorg/coder) | uzi (this PRD) |
|---|---|---|---|---|---|
| Per-user/task tooling | None — fixed worker deps | None — host tools assumed | Ships its own `devbox.json` + `devbox.lock`/`flake.nix` for repo-local pinned tooling (prior art for the engine, not a product feature) | Whole product model: admin-curated **templates** (Terraform) provision full workspace images/VMs; user picks a template | Both: curated image templates in git (the coder-validated shape, minus Terraform) for heavy/system deps + devbox per-repo provisioning for CLI tools |
| Trust posture for repo-borne config | n/a | Materializes into workdir with native discovery ON (the weakness PRD #16 avoids) | Trusted-by-definition dev repo | Templates are admin-only; workspaces run arbitrary user code by design | Repo manifest is packages-only extraction, opt-in, provisioned in a secret-scrubbed env (Decision 3) |

coder's docker/kind-in-pod provisioning (plan.md lines 7, 11) is the eventual VM/pod story ("later: switch to VMs?") — its template-catalog UX is what Track 1 borrows at compose scale; revisit its provisioner properly when workers leave the laptop.

## Solution Overview

Three tracks sharing one mental model (the PRD #16 scope+allocation pattern):

1. **Worker templates in git**: curated Dockerfile variants under `agent/templates/<name>/Dockerfile`, all `FROM` the shared minimal base, reviewed like any code. Variants exist **only for heavy or system-level dependencies that devbox/nix provisioning handles poorly** (JVMs, docker CLI, system libs) — per-repo CLI tools are Track 2's job, so the two tracks don't solve the same problem twice. The user picks a template per worker in the UI (recorded at join-token issuance); `docker compose` selects it via a `WORKER_TEMPLATE` build arg; the worker reports its template name in the register call, so the server can show it per worker and badge drift from the declared choice. Soft verification only — the join token remains the trust anchor; template identity is observability, not security.
2. **Devbox tool tiers (per-repo provisioning)**: the worker gains one provisioning engine (devbox/nix, single-user install, store cached on the `agentdata` volume) fed by three manifest sources. Numbered by precedence — tier 1 wins over tier 2 wins over tier 3:
   - **Tier 1 — uzi-stored per-repo tool profile**: a plain package list (`["kubectl@1.31", "terraform"]`) stored in DB per (user, repo), validated against an admin-managed allowlist, delivered in the claim payload; the worker synthesizes a `devbox.json` *outside the clone* from it. No raw `devbox.json` accepted — `shell.init_hook`/`shell.scripts` are arbitrary shell.
   - **Tier 2 — repo manifest** (`devbox.json` in the clone): **per-repo opt-in, default off**, and even then the worker extracts **only the `packages` array** — `shell.init_hook`, `shell.scripts`, flake references, and every other key are ignored (same "extract the safe fields, drop the rest" discipline PRD #16 applies to repo-skill frontmatter). Merge is a package union with tier 1 winning version conflicts.
   - **Tier 3 — default**: no manifest → base toolset only (today's behavior).
3. **Agent template scopes + allocation**: `agent_templates` grows `scope` (`builtin`/`global`/`user`) + `user_id`, with the same partial unique indexes as PRD #16 skills (shared names unique across builtin+global; per-user names unique per user). An `agent_template_allocations` table (global defaults by admins + per-user overlay) decides which templates ride each claim. The lead's delegate list is already dynamic (`delegatesLine`, `agent/src/prompt.ts:152`), so the worker needs no routing changes.

## Design Decisions

1. **Tools belong with the work; images stay curated in git.** Long-term the toolchain follows the repo/task, not the person (what CI systems converged on), so per-repo devbox provisioning is the product path for CLI tools. The worker image stays minimal-by-default with a small set of git-curated, code-reviewed variants for what nix can't deliver well — not N unaudited private images. The derived-image escape hatch remains possible (unenforceable anyway) but undocumented as a product path. Among manifest sources, the **uzi-stored profile beats the repo manifest**: the profile is user-authored and allowlist-validated, the repo is semi-trusted input; the repo manifest also bypasses the admin allowlist (accepted — it is bounded by the opt-in and packages-only extraction instead).
2. **Tier 1 stores a package list, not devbox.json** (user, 2026-07-05). `devbox.json` permits `shell.init_hook` and `shell.scripts` — arbitrary shell at provision time. A declarative package list validated against an admin allowlist keeps the server-delivered tier safe to offer to non-admin users.
3. **Repo manifests are packages-only, and all provisioning runs secret-scrubbed** (review finding B1, 2026-07-05 — supersedes the earlier "full devbox expressiveness in tier 2"). Provisioning runs in the **worker**, before the SDK starts: outside the `agent/src/guardrails.ts` deny-hook and outside the scrubbed agent env, in a process that holds the decrypted forge PAT, the user's Anthropic token, and can read the join-token file. Honoring a repo's `init_hook` there would turn "install my repo's tools" into "read my credentials". Therefore: (a) tier 2 extracts only `packages`; (b) the provisioning subprocess (both tiers — nix build hooks are arbitrary code too) runs with an env scrubbed of the PAT, Anthropic token, and join token, mirroring the `buildSdkEnv` replacement-env discipline (`agent/src/sdk-env.ts:39`).
4. **Tier 2 is opt-in, default off** (user, 2026-07-05). A malicious repo must not choose what gets installed on the worker even packages-only. Mirrors PRD #16's repo-skills posture: explicit per-repo enable; never repo hooks/settings.
5. **Template choice is soft-verified, per worker** (user, 2026-07-05: "UI choice + compose var"; cardinality per review m2). The declared template is chosen at join-token issuance (a user legitimately runs a `base` and a `java` worker side by side), compared against what that worker self-reports at register; mismatch is surfaced (admin + owner visibility), not rejected. Hard rejection buys no security (a hostile worker can lie) and breaks legitimate local builds. The join token remains the sole authn boundary.
6. **Provisioned tools run inside the existing agent trust envelope.** Everything installed is reachable by the agent's Bash tool, so templates and allowlists must never include pre-authenticated credential-bearing CLIs (a logged-in `glab`, a kubeconfig) — that would bypass the "worker holds the PAT, agent doesn't" boundary. Guardrail deny-hook review is part of adding any tool that can push or mutate remotes.
7. **Agent template scopes mirror the PRD #16 skills design** (user, 2026-07-05: "global agents"). Same three server scopes, same partial-unique-index shape, same allocation overlay. One pattern for skills, templates, and tool profiles — one mental model, one UI shape. (Design-mirroring, not code reuse, until #16 lands — see Depends on.)
8. **User-scoped templates cannot weaken guardrails, and lead names are reserved** (review finding M-e). Template bodies are persona/workflow only; the worker's hardcoded guardrail append (`agent/src/prompt.ts:62-66`) and the PreToolUse deny-hook apply regardless of template content. Additionally, `scope='user'` templates may not match `LEAD_NAME_RE` (`/^(lead|orchestrator)$/i`, `agent/src/agents.ts:41`) — the partial uniques would otherwise allow a user-scoped `lead` to coexist with the builtin and win or lose main-thread routing by array order (`agents.ts:78` takes the first match). Enforced at create/rename; a worker-side test pins that a claim never carries two lead-matching templates.
9. **`is_builtin` migrates into `scope`.** `scope='builtin'` replaces the boolean (kept as a generated/compat column only if sqlc churn demands it); builtin seeding and `ResetAgentTemplate` semantics are unchanged in behavior, but the seeding query itself must change conflict target — see §4.

## Technical Design

### 1. Worker templates in git (agent + compose + docs)

- `agent/templates/<name>/Dockerfile`, each `FROM` the base image stage; `base` is today's `agent/Dockerfile` moved/aliased. Variants add heavy/system packages but never remove guardrail-relevant layers (non-root `uzi` user, tini, no secrets).
- `docker-compose.yml`: `agent.build.dockerfile` parameterized by `WORKER_TEMPLATE` (default `base`).
- `workers.template_declared` (set from the UI at join-token issuance) + `workers.template_reported` (from register). Register payload (`agent/src/client.ts:61-63` / `WorkerRegister`, `api/internal/handler/worker_protocol.go:19`): add optional `template`. Two compat notes: the handler currently decodes but deliberately ignores the sibling `name` field (`worker_protocol.go:25-33`) — `template` must be explicitly read and persisted; and `DecodeJSON` rejects unknown fields (`worker_protocol.go:30-33`), so the server must ship the widened decode struct **before** any worker sends the field, or old servers 400 new workers.
- Workers/admin pages show declared vs reported per worker with a drift badge. `docs/worker-setup.md` documents the choice + `WORKER_TEMPLATE`.

### 2. Devbox provisioning engine (agent)

- Base image (all templates) gains devbox + nix single-user; nix store relocated/cached under `UZI_DATA_DIR` so `agentdata` persists it across runs on the same worker (warm start after first provision).
- At claim time, before SDK start: resolve manifest per precedence → synthesize `devbox.json` in a per-run dir *outside the clone* → `devbox install` in a **secret-scrubbed subprocess env** (no PAT, no Anthropic token, no join-token path — Decision 3) → export the tool env via `devbox shellenv`, filtered through an **explicit variable allowlist** (`PATH` prepend, `NIX_SSL_CERT_FILE`, `LOCALE_ARCHIVE`, and whatever the chosen packages demonstrably need — never a blind merge) into the SDK env built by `buildSdkEnv` (`agent/src/sdk-env.ts:39`). Widening the deliberately-minimal agent env is guardrail-adjacent: each added var needs the same scrutiny as the existing ones, and none may carry worker secrets.
- Provision failures fail the run with a clear message (missing package, allowlist reject) rather than degrading silently.
- Claim payload (`api/internal/workersvc/claim.go:75` `ClaimConfig`): add `tool_packages []string` (resolved tier-1 list) and `repo_devbox_opt_in bool`.
- **New egress**: nix substituters (cache.nixos.org etc.). ARCHITECTURE.md's "outbound-only to `api`" worker description must be amended, and the substituter set documented (and ideally pinned) in the worker docs.

### 3. Tool profiles + allowlist (api + web)

- Tables: `tool_allowlist` (admin CRUD: package name pattern + optional pinned-version policy) and `repo_tool_profiles` (user_id, repo_id, packages JSONB, unique per pair). Validation at write time *and* at claim time (allowlist may have shrunk).
- Repo settings UI: package picker (allowlist-backed), plus the tier-2 "trust this repo's devbox.json packages" toggle (default off), living next to PRD #16's repo-skills toggle.

### 4. Agent template scopes + allocation (api + web + agent)

- Migration: `ALTER TABLE agent_templates ADD scope TEXT NOT NULL DEFAULT 'global' CHECK (scope IN ('builtin','global','user')), ADD user_id UUID REFERENCES users(id) ON DELETE CASCADE`; backfill `scope='builtin'` where `is_builtin`; drop the old unique on `name`, add partial uniques (shared: `ON (name) WHERE scope <> 'user'`; user: `ON (user_id, name) WHERE scope = 'user'`).
- **Reconciler coupling (shared surface with PRD #17)**: builtin seeding uses `INSERT ... ON CONFLICT (name) DO NOTHING` (`api/internal/store/agent_templates.sql.go:118`, source `agent_templates.sql:38-40`); after the partial-unique migration that conflict target is invalid and must become `ON CONFLICT (name) WHERE scope <> 'user'` **in the same milestone** or boot breaks. PRD #17 M1 edits the same reconciler path to seed the lead — whichever PRD lands second rebases its seeding change on the first; state the order at `/prd-start`.
- `agent_template_allocations` (same shape as PRD #16's `agent_skill_allocations`): global rows (admin) + per-user overlay rows (add/remove), resolved at claim time to the template set delivered in the payload. **No empty-means-all cliff** (review m5): the migration seeds explicit global allocation rows for all existing builtin/global templates, and creating a global template auto-seeds its allocation row (removable). Absence of a *user overlay* means "global default set", which is always explicit.
- Handlers: user CRUD for `scope='user'` templates (same validation path as admin CRUD — kebab-case name, tools allowlist, model aliases, plus the reserved-name check from Decision 8); visibility = builtin + global + own.
- Agents page: scope badges, "my agents" creation, allocation toggles per template; admin retains global/builtin management.
- Worker: no routing changes (`assembleAgents` consumes whatever the claim delivers); tests: (a) a user-scoped template cannot be named `lead`/`orchestrator` (API-level), (b) a claim payload never contains two `LEAD_NAME_RE` matches (worker-level pin).
- **Migration numbers**: drafted as `00023`–`00028` for this PRD (ledger of drafts: `00022` #17, `00030+` #5, `00036`–`00039` #19, `00040+` #6, `00041` #21 (landed; drafted `00040`, renumbered above PRD #16's `00040_skills`), `00050+` #16): workers template columns, tool_allowlist, repo_tool_profiles, agent_templates scope/user_id, agent_template_allocations. (Renumbered 2026-07-05 from `00030`–`00035`, which collided with PRD #5's `00030+` reservation.) **2026-07-05 convention change (CLAUDE.md)**: these are development drafts only — final numbers are assigned at merge time, next free above the live head. PRD #24 landed `00029` (above this PRD's draft range), and the boot runner is strict goose (no `allow-missing`), so landing a below-head version would brick upgraded instances at boot; reserved ranges cannot guarantee landing order, per-merge renumbering can.

### 5. Docs + specs

- `docs/worker-setup.md`: template choice + `WORKER_TEMPLATE`; new `docs/worker-tools.md` (audience: user): tiers, allowlist, repo opt-in trust warning (packages-only, what that does and doesn't protect against).
- `specs/ai.md`: new sections for worker templates, tool tiers, template scopes. `ARCHITECTURE.md`: worker image paragraph, worker egress (nix substituters), agent-templates paragraph.

## Milestones

- [x] **M1 — Worker template variants in git**: `agent/templates/` layout with `base` + at least one heavy-dep variant (e.g. `jvm`), compose `WORKER_TEMPLATE` plumbing, register-payload `template` field stored per worker. Docs updated.
- [x] **M2 — Template choice in UI + drift surfacing**: per-worker declared template at join-token issuance; workers page shows declared vs reported with mismatch badge.
- [x] **M3 — Devbox engine in the worker**: base image ships devbox/nix with store on `agentdata`; worker provisions from a claim-delivered package list in a secret-scrubbed env and exposes tools to the SDK env via the filtered shellenv allowlist; provision failure fails the run cleanly. (Tier 1 end-to-end with a hardcoded allowlist.)
- [x] **M4 — Tool profiles + admin allowlist (api + web)**: allowlist CRUD, per-(user,repo) package profiles, claim-time validation, repo settings UI.
- [x] **M5 — Tier 2 repo `devbox.json` packages opt-in**: per-repo trust toggle (default off), packages-only extraction, union-merge with tier-1 precedence, trust warning in UI + docs.
- [x] **M6 — Agent template scopes migration + user CRUD** *(blocked by PRD #16 schema landing, or explicitly takes over the shared shapes)*: scope/user_id migration, partial uniques, reconciler conflict-target fix, reserved-name validation, user-scoped CRUD.
- [x] **M7 — Template allocation + claim filtering** *(same blocker as M6)*: allocation table with explicit seeded defaults, overlay resolution, claim payload delivers only allocated templates, Agents page allocation UI.
- [x] **M8 — Tests, specs, docs complete**: e2e covering a provisioned-tool run (devbox stubbed in the isolated e2e stack — no substituter egress there; a separate opt-in integration test does a real `devbox install`) and a run using a user-scoped template; `specs/ai.md` + `ARCHITECTURE.md` + user docs updated.

Phase note: M1–M2 (worker templates) and M3–M5 (tooling) are independent tracks touching disjoint files and can run in parallel; M6–M7 (scopes) is sequenced against PRD #16/#17 per the notes above. M8 spans all.

### Progress + agent-validation ledger (2026-07-10, paused for resume)

Work runs on `feature/prd-18-worker-templates` (worktree `../prd-18-worker-templates`), agent-team workflow (coder + reviewer + auditor + tester). PRDs #16/#17 landed before this started, so the M6–M7 blockers are lifted; migrations were developed as `00044–00048` (live head was `00043`) and renumbered at landing to **`00045–00049`** after PRD #25 took `00044_slack` on main (per the merge-time renumbering convention); `specs/ai.md` sections likewise landed as §154–159 after #32/#25 took the 130s–140s–150s.

| Scope | Commits | Agent validation |
|---|---|---|
| M1+M2 templates + drift UI | `ca93b17` `a1dca44` `7baaece` | reviewer PASS, auditor PASS, tester PASS (images, compose default, e2e w/ 00044) |
| M3 devbox engine + hardening | `f8779d1` `84fa838` `13591c4` | reviewer PASS, auditor PASS (secret-scrub core confirmed) |
| M3 image packaging | `ef21dee` `989755f` | tester: LOGIC PASS at `ef21dee` (rootless uzi install, warm-start via `agentnix:/nix`, pinned devbox 0.17.5); the two build blockers it found (checksum-filename verify, hardcoded amd64/no TARGETARCH) are fixed in `989755f`. **`989755f` not yet agent-reviewed; still needs a real `docker build` of both templates (native amd64 ideally)** |
| M4 tool profiles + allowlist | `c846116` `342520b` | reviewer PASS, auditor PASS |
| M4 audit ride-alongs (denylist, caps, shared rules loader) | `a45a9fd` | **NOT yet agent-reviewed** |
| M5 tier-2 repo opt-in | `a0b7ba9` `ec12c96` | **NOT yet reviewed/audited/tested — first validation wave on resume** |

Day 2 (2026-07-10) completed the run — all milestones landed and validated:

| Scope | Commits | Agent validation |
|---|---|---|
| Day-1 validation debt (M4 ride-alongs, M5, Dockerfile fix) | `a45a9fd` `a0b7ba9` `ec12c96` `989755f` | reviewer PASS, auditor PASS; tester: real `docker build` of both templates (native arm64), uid-100 rootless provision smoke, cross-container warm-start, hostile tier-2 manifest inert, e2e 63/63 |
| main merge (PRD #28 drift) | `173b94b` | reviewer sanity PASS (empty combined diff, reviewed SHAs remain ancestors) |
| M6 scopes + user CRUD | `2ca62c0` | reviewer PASS; auditor found 1 Blocking (unfiltered claim query leaked private templates cross-user) → closed below |
| M7 allocations + claim filtering | `5d3d35d` `b3904c9` `f6aa609` `5416176` | reviewer PASS, auditor re-gate: Blocking CLOSED (owner-scoped claim SQL + shared-precedence drop + live-DB regression tests); tester: store-IT live-DB scenarios, e2e, image rebuilds all PASS |
| M8 e2e/docs/specs + fixes | `cafbcbd` `2d3e131` `da96e6a` `d04c731` `bcfc1bc` `bbb7c48` `31aeb08` | reviewer PASS, auditor SECURITY SIGN-OFF, fact-checker: all claims verified after 1 refuted egress-wording claim fixed in `31aeb08`; `specs/human.md` Feature #18 user-approved verbatim |

Closed during the run: tier-2 deliberately bypasses allowlist AND denylist (audit-probed, accepted — bounded by opt-in + packages-only + scrubbed provisioning env); shared-precedence claim drop for user/shared name collisions (deliberate divergence from skills' body precedence); reserved lead names blocked for global creates too. Known residual: native amd64 `docker build` unverified (arm64-only host; asset names + checksums verified against live releases).

## Success Criteria

- A user can pick the `jvm` template when issuing a join token, rebuild with `WORKER_TEMPLATE=jvm`, and see that template reported for that worker; a mismatch shows a badge, never a rejection.
- A run against a repo with a tier-1 profile has those tools on PATH inside the agent's Bash; the **same worker's second run** warm-starts from the nix cache.
- A repo's `devbox.json` contributes packages only after its owner flips the opt-in; its `shell.init_hook`/`shell.scripts` are never executed, verified by a worker test with a hostile manifest fixture.
- Provisioning subprocesses observe no PAT, Anthropic token, or join token in their env, verified by a worker test.
- A package outside the allowlist fails the profile save (and the claim, if grandfathered).
- A user can create a private agent template, allocate it to their runs, and no other user ever sees it or receives it in a claim; naming it `lead` is rejected.
- All guardrail layers unchanged: deny-hook, `settingSources: []`, PAT stays worker-side, `main` untouched.

## Risks

- **Nix store size/perf on laptops**: first provision is slow and the store grows; mitigated by the shared `agentdata` cache and allowlist-pinned versions, but needs an eviction note in docs. If devbox proves too heavy, `mise` is the fallback engine (same manifest-tier design holds).
- **Allowlist is an operability control, not a sandbox**: nix packages run build hooks; the allowlist bounds *what* is installed, not what installed code does. Acceptable because the agent already executes arbitrary repo code — but the secret-scrubbed provisioning env (Decision 3) is the actual security control and must not regress.
- **New worker egress** (nix substituters) weakens the clean "outbound-only to api" story; documented and ideally pinned, but it is a real surface change reviewers must see (ARCHITECTURE.md update is part of M3, not M8).
- **Cross-PRD collision on `agent_templates`**: #17 (lead seeding) and this PRD (partial uniques + conflict-target change) edit the same reconciler; whichever lands second rebases (Technical Design §4). Migration-number reservations prevent goose collisions.
- **UI surface creep**: three new admin/user surfaces (templates, allowlist, allocations); mitigated by reusing the PRD #16 skills UI shape.
