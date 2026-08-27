# ADR-602: Agent-source repo sync — an additive, provenance-scoped layer over the embedded builtins, gated by approve-before-apply

**Status**: Accepted (PRD #602, milestones M0–M6 landed)
**Date**: 2026-08-23
**Deciders**: Vlad (maintainer) + agent team (architect, security review — this ADR's MR review stands in for the reviewer/auditor wave the PRD names)
**PRD**: [prds/done/602-agent-source-repo-sync.md](../prds/done/602-agent-source-repo-sync.md) (GitHub issue [vtmocanu/uzi#602](https://github.com/vtmocanu/uzi/issues/602)) — the PRD carries the milestones, the full Decision list (1-15), and the touchpoints; this ADR carries the trust seam and the decisions load-bearing enough that a future change must respect them without rereading the PRD.

## Decision (summary)

uzi gains an **opt-in**, admin-configurable git-backed sync for agent role
definitions — the system prompts that drive `Bash`-capable, file-editing
agents — layered **additively** on top of the existing `go:embed` builtins,
never replacing them. Sync is **off by default with no repo URL pre-filled**;
turning it on and pointing it at a source is a deliberate per-instance choice.
Provenance is tracked by a new scope-aware `origin` column on
`agent_templates` so boot-time reconciliation (`RefreshPristineBuiltin`) never
fights a synced or admin-edited row. A synced role **overrides** a
same-named embedded builtin; a synced-only role lands as a deletable
`scope='global'` template. Every synced change is **staged** and requires an
explicit admin approval before it can reach any run — the primary control,
because most role bodies carry `Bash`, which is full egress, so a
tools-allowlist is not where the real risk is contained. The api hosts the
sync (a new clone-and-read `go-git` path, reusing the dependency `pushbroker`
already pulled in) with a brand-new Go parser that reproduces the worker's
`agent/src/repoagents.ts` tolerant-frontmatter contract, pinned to it by a
differential test.

## Context

uzi ships 12 builtin agent roles, `go:embed`'d into the api binary and
hand-ported from the upstream role library (`vtmocanu/skills`
`agent-team/roles.yaml`). PRD #85 makes that porting *drift-detectable* at
build time, but a human still has to read the upstream diff and re-port each
change by hand — and a self-hoster who wants their own roster has no path
short of editing every template through the admin UI, or dropping
`.claude/agents/*.md` into every repo uzi is asked to work.

The role body is not incidental prose: it is the **system prompt for an
agent that runs `Bash` and edits files** in the user's own repos. Whatever
lets uzi pull that prompt from somewhere other than the binary is, by
definition, a supply-chain trust boundary — the asset a compromise there
would hand over is "arbitrary instructions to a code-editing bot," not a
config value. Every decision below is shaped by treating that boundary as
the central design constraint, not an afterthought bolted onto a sync
feature.

## The decisions

### An additive source layer, never a replacement

The `go:embed` builtins remain the shipped default and the
bootstrap/offline fallback. A fresh install boots with a full, working
agent roster and zero external dependency, exactly as it does today; the
sync is strictly opt-in on top. This is what makes every decision below
possible to reason about independently: nothing about sync being broken,
misconfigured, or disabled can leave an instance without agents to run.

### Default off, empty URL — no canonical repo is pre-filled

A fresh install runs the embedded builtins with `agent_source_enabled =
false` and `agent_source_repo_url` **empty**. This ADR does **not**
hardcode a canonical product-agents repository URL into that default: no
such repo exists yet, so pre-filling one would point a toggle at nothing.
Following an upstream roster is a one-toggle, one-URL opt-in per instance,
made available once that canonical repo exists — not before.

> **Note — this corrects the PRD's Decision 2 draft text.** PRD #602
> Decision 2, as drafted 2026-08-22, describes the source-repo setting as
> "pre-populated with our canonical product-agents repo" so an admin need
> only flip one toggle to follow it. At ADR-writing time no such repo
> exists (a repo-wide grep for a hardcoded URL, `agent_source_repo_url`, or
> any `product-agents` reference returns nothing outside the PRD's own
> prose), and pre-filling a URL for a repo that does not yet exist is not
> implementable. This ADR's default — off, URL empty, nothing pre-filled —
> is the one M2 must build to; pre-filling a canonical URL, if and when that
> repo exists, is a future, separate change, not part of M2. The PRD's
> Decision 2 text should be updated to match once this ADR merges.

The rejected alternative — live-follow-by-default — was rejected on the
same grounds Decision 1 protects: a default-on remote-execution trust
posture for a source of Bash-agent system prompts, and a fresh install
that is no longer offline/hermetic by default. It remains available as a
per-instance opt-in, never the shipped default.

### Provenance is a scope-aware `origin` column extending PRD #275's `customized`

PRD #275 already tracks whether a builtin row has been admin-edited via a
boolean `customized`; `ReconcileBuiltinTemplates` runs
`RefreshPristineBuiltin` at every boot, re-applying the embedded body to
`scope='builtin' AND customized=false` rows
(`api/internal/store/agent_templates_builtins.go`). That boolean cannot
express a third state: "not admin-edited, but not owned by the embedded
default either." Without a third state, a synced builtin row stored as
merely "not customized" would be clobbered back to the embedded body on
the very next boot — sync and boot-reconcile fighting each other forever.

So `agent_templates` gains an explicit `origin` column, and the exact CHECK
predicate is load-bearing — the migration and this ADR must agree on it
verbatim:

```sql
CHECK (
     (scope = 'builtin' AND origin IS NOT NULL AND origin IN ('embedded','synced','admin'))
  OR (scope = 'global'  AND (origin IS NULL OR origin = 'synced'))
  OR (scope = 'user'    AND origin IS NULL)
)
```

The explicit `origin IS NOT NULL` in the builtin branch is load-bearing, not
redundant: a SQL `CHECK` passes when its expression evaluates to NULL (SQL
three-valued logic), and `origin IN ('embedded','synced','admin')` is NULL —
not FALSE — when `origin` is NULL. Without the guard a `scope='builtin'` row
with a NULL origin would be *accepted* (the whole predicate evaluates to NULL),
defeating the very invariant this constraint exists to enforce — a builtin must
always carry a concrete provenance. The guard forces that branch to FALSE for a
NULL origin so the row is rejected.

- **`embedded` and `admin` are builtin-only.** A `scope='builtin'` row is
  either the shipped default (`embedded`) or admin-edited
  (`admin`, the successor to `customized=true`).
- **`synced` is allowed on `scope='builtin'` OR `scope='global'`** — two
  different shapes of "this row's body came from the sync source," covered
  in the apply decision below.
- **`scope='user'` rows are always `origin IS NULL`** — provenance tracking
  is a product/admin-template concern; a personal template was never a
  candidate for sync or boot-reconcile.

Boot-reconcile refreshes **only `scope='builtin' AND origin='embedded' AND
customized=false`** — the existing `scope='builtin'` predicate the live
`RefreshPristineBuiltin` query already carries stays, and `AND
origin='embedded'` is added to it. A synced or admin-edited builtin row is
therefore structurally unreachable by the boot refresh; it is never
clobbered, regardless of how many boots pass between syncs.

**Why `synced` is also legal on `scope='global'`, not builtin-only:** a
`scope='builtin'` row is undeletable by design (`DeleteAgentTemplate`
answers 409 for any `is_builtin` row — the "reset, never delete" contract
PRD #4 relies on) and has no embedded default to reset to if the source
stops shipping it. A sync source is dynamic: roles come and go as the
upstream roster changes, and a de-provisioned role must be genuinely
**removable**, not stuck forever as an un-deletable row with nothing to
reset to. So a synced-**only** role — one with no same-named embedded
builtin — is stored as a deletable `scope='global'` row. A synced role
that instead **overrides** a shipped builtin stays a `scope='builtin'`
row, because it keeps something a synced-only role does not have: an
embedded body to reset to if the admin ever wants the shipped default
back.

### Precedence: a synced role overrides the same-named embedded builtin

This is what makes the feature worth having: an admin who points uzi at
their own roster expects their `coder` or `tester` to win over the shipped
one, not sit beside it unused. Concretely, a synced role sharing a
builtin's name **updates that `scope='builtin'` row** — never a second,
shadow row — setting `origin='synced'`.

### Applying a sync is a four-case, provenance-aware upsert, gated by approve-before-apply

Reconcile (fetch + parse + diff) and apply (write `agent_templates`) are
deliberately separate steps — see the threat-model control below. When an
admin approves a staged snapshot, the upsert resolves each synced role
into exactly one of four cases:

| # | Case | Effect |
|---|---|---|
| 1 | Synced role shares a name with an existing builtin | **UPDATE** that `scope='builtin'` row: body from the source, `origin='synced'`. Still resettable back to the embedded body. |
| 2 | Synced role's name is new (no builtin or admin global template by that name) | **INSERT** `scope='global', is_builtin=false, origin='synced'`, plus a seeded `agent_template_allocations` row — allocation, not table presence, is what makes a template actually eligible to run. Deletable/de-provisionable. |
| 3 | Synced role's name collides with an existing **admin-authored** `scope='global'` template | **Never** overwritten. Stage a visible error and skip; an admin's own global row is never silently clobbered by a sync. |
| 4 | A previously-synced role is now **absent** from the source (de-provisioning) | For a synced-only global row (`scope='global' AND origin='synced'`): **DELETE** it. For an overridden builtin (`scope='builtin' AND origin='synced'`): **RESET** it to the embedded body (`origin='embedded'`). An admin-edited (`origin='admin'`) or untouched-embedded row is **never** de-provisioned by an absence in the source — the source only ever acts on rows it itself owns. |

A role also fails per-role, gracefully — an unreachable source, malformed
frontmatter, or an all-tools-denied role fails that one role with a
visible status error, and every other role and the previous good state are
left untouched. Reconcile never crashes api boot; an unreachable source at
any point falls back to the last-good staged snapshot, or to the embedded
default if there has never been a good sync.

### Host = the api, reusing the existing pure-Go `go-git` client

`go-git/v5` is already a direct dependency of `api/go.mod`
(`v5.19.2`), pulled in for `api/internal/pushbroker` (PRD #122 M8, the
checkpoint-push broker). The api is the coherent host for the sync too: it
already owns `agent_templates`, already holds `secretbox` for sealed
credentials, is built distroless-static (so a pure-Go git client, not a
shelled-out `git` binary, is the only option that does not fatten the
image), and is where the reconcile loop and boot-reconcile already live.

Be precise about what is reused and what is new: the **dependency** is
reused, but the **usage** is not. `pushbroker.go` is, by its own header
comment, "the ONE place go-git is used, deliberately isolated" — a **push**
pipeline (worker deltas → the api → origin). The sync adds a genuinely new
**clone-and-read** path (source repo → the api → parse → stage), the
opposite direction and a different `go-git` surface (worktree/clone APIs,
not `PushOptions`/pack construction). Landing this feature makes
`pushbroker`'s "ONE place" comment stale; it must be corrected in the same
change that adds the sync's git helper, not left to rot.

Transport stays plain `go-git` for the same reason it is the whole
dependency's reason to exist here: the runtime need is "clone a repo and
read files," which `go-git` already does. The `npx skills` CLI (used
elsewhere in this project's own tooling to sync `.claude/agents/`) is
explicitly **not** the transport — it is a dev-laptop tool with the wrong
runtime model and documented footguns, not something the api process
should be shelling out to.

### A new api-side parser, reproducing `agent/src/repoagents.ts`'s contract, pinned by a differential test

Two role-file parsers exist today. `api/internal/agenttmpl` is strict: it
rejects unknown frontmatter keys, records `tools` verbatim with no
denylist/security filtering, and today is only ever fed the embedded
builtin `.md` files uzi ships itself. `agent/src/
repoagents.ts` is lenient: it is the parser uzi already applies to a
user's own cloned `.claude/agents/*.md`, and its tolerant contract —
unknown-key-ignoring frontmatter, BOM/CRLF normalization, dropped block
scalars, inline/`[..]`/`-`-list tool syntax, quote stripping — is what the
ecosystem's role files actually look like.

External sync-source files must be parsed by the **same contract**
`repoagents.ts` uses, because that is the format such a repo will actually
contain. The strict embedded-builtin parser is left unchanged — it is not
bent to also accept this looser shape, since that would weaken what it
guarantees for uzi's own shipped files. Instead a **new** synced-file
parser is added on the api side that reproduces `repoagents.ts`'s
behavior, and the two are pinned together by a **differential test**: the
same input files must produce the same accepted keys, the same resulting
tool set, and the same sanitization outcome from both implementations.

Two specific behaviors of that contract are security-relevant enough to
call out on their own, because getting them subtly wrong reopens exactly
the risk the approval gate exists to contain:

- **Tools is a narrow denylist that strips, not an allowlist that
  rejects.** `repoagents.ts` removes denied tools (`Agent`, `ScheduleWakeup`,
  `CronCreate`, with `Task` canonicalized to `Agent`) from a role's tool list
  and keeps the role; it only fails the role outright if *every* tool ends
  up denied. The new parser reproduces exactly that — a denylist-strip, not
  a fail-closed allowlist.
- **The description is sanitized; the body is not.** `repoagents.ts`
  rejects (not merely strips) a description containing Unicode Cc/Cf
  control or bidi (RTL-override) characters — load-bearing because the
  description is what an admin reads in the approval dialog (the primary
  control below), and an unsanitized bidi payload there could make the
  approved text look like something other than what it actually is. The
  role **body** is deliberately left unsanitized, matching the worker: it
  is prose for a model to read, not UI chrome an admin is asked to trust
  at a glance. Because the body is kept raw, the **M4 approval render
  boundary** — not the parser — must neutralize control/bidi in the body
  (and re-sanitize the description/model) before an admin reads the staged
  diff; the parser's description rejection is necessary but not sufficient
  on the larger field.

Two places the api twin is deliberately **stricter** than `repoagents.ts`,
because it writes to a Postgres UTF-8 column and an admin approval/status
surface the worker parser never touches. Both are documented divergences
(the differential test pins parity on every *other* input; these two are
tested Go-side only):

- **Non-UTF-8 input is rejected, not lossily decoded.** `repoagents.ts`
  reads a file as UTF-8 with lossy replacement (an invalid byte becomes
  U+FFFD). The twin instead rejects a non-UTF-8 file per-role (`invalid`).
  Reproducing the lossy decode by ranging runes would let a lone C1 control
  byte (e.g. `0x9B`) survive as a raw byte in the stored description — past
  the Cc/Cf check, and into a Postgres `text` column that rejects invalid
  UTF-8 anyway. Rejecting up front keeps the write boundary clean, matching
  `termsafe.Validate`'s own invalid-UTF-8 rejection.
- **A Cf character in the `model` is rejected (→ `model_ignored`), not
  honored.** `repoagents.ts`'s `isValidModel` permits a Unicode Cf char in
  the model token; uzi's own `agenttmpl.ValidateModel` rejects it, with an
  explicit note that the model is echoed onto the admin cross-owner status
  surface where a zero-width/bidi-padded token is a spoofing vector — the
  same surface the description rejection protects. The twin follows uzi's
  doctrine (reject Cf) rather than the worker's (honor it); the clamp is
  fail-safe (the role inherits the run default, it is not skipped).

### Source format: a repo of individual `.md` role files, not `roles.yaml`, not `.claude/agents/`

The sync source is a git repo of one `.md` file per role, each carrying
`.claude/agents`-style frontmatter — the exact shape the new parser above
(and `repoagents.ts`) already parses. It is deliberately **not** the
upstream role library's single `roles.yaml`: that YAML file is the
*generic ancestor* the shipped builtins are hand-ported *from* (PRD #85's
concern), and uzi's own roles (`coder`, and especially `tester`, ~89%
adapted from the ancestor) would be clobbered back toward the generic if
sync pulled from it. The sync source is the **roster you actually run**,
which is a different artifact from the ancestor it may have started as.

It is equally deliberately **not** `.claude/agents/` — this repo's own
dev-team roster. That directory is decoupled by design (see the
`judge-triage` skill's three-copy table: upstream library, product
builtins, repo agents are three separately-maintained targets today), and
this feature does not change that: the sync source is a product-agents
roster an admin points uzi at, never `.claude/agents/` itself.

Keeping the source in the same `.md`-per-role shape as `.claude/agents/`
files means one parser contract covers three surfaces — a repo's own
agents, a synced source, and (as point-in-time snapshots) the builtins —
rather than a fourth bespoke format uzi would need to teach a parser about.

## Threat model

- **Asset.** The role body is a system prompt for an agent with `Bash` and
  file-edit access. A compromised source hands over arbitrary instructions
  to a code-editing bot running against the user's own repos.
- **Primary control: approve-before-apply.** No synced change reaches a
  run without an admin explicitly approving the staged diff — this is a
  separate gate on the *template source*, and auto-approve run sweeps do
  not bypass it. This is the primary control, not the tools line, because
  most role bodies carry `Bash`, and `repoagents.ts` itself already notes
  that denying network-shaped tools is "theatre" when `Bash` is full
  egress regardless. The real supply-chain risk is **prompt content
  steering a Bash-capable agent** (injection), not a missing tool
  restriction — so the control that matters is a human reading the diff
  before it can run anywhere, not a capability list.
- **Network: a separate SSRF allowlist.** The source URL is constrained by
  `AGENT_SOURCE_ALLOWED_BASE_URLS` (https-only), enforced in the generic
  admin-settings PUT handler **and re-checked at the clone seam itself**
  (a TOCTOU guard — the setting could change between validation and the
  reconcile that actually dials out). This is a **separate** list from
  `FORGE_ALLOWED_BASE_URLS`; reusing the forge's list would couple the
  forge host set to the role-source host set for no reason the two
  concerns share. **Redirects are re-checked against the same allowlist
  (M3b hardening).** go-git's default transport would follow the initial
  redirect without re-validating the resolved target, so an allowlisted
  host answering `302 Location: http://169.254.169.254/…` would be dialed.
  The clone therefore runs through a per-operation `http.Client` whose
  `CheckRedirect` refuses any redirect whose target does not still pass
  `AgentSourceBaseURLAllowed` (a same-host `http→https` or `/repo→/repo.git`
  hop stays allowed; a cross-host/internal hop is refused). That client is
  scoped to the agentsource clone alone — it is never installed into
  go-git's process-global protocol registry — so `pushbroker`'s push
  pipeline, which uses the default client, is unaffected.

  > **Update (PRD #702).** This clone/redirect path is no longer the only
  > egress: PRD #702 adds a second, lightweight path — a ref-advertisement
  > (`git ls-remote`-equivalent, no full clone) used to resolve the latest
  > tag / detect an available update. Its "preset resolve" variant can dial
  > an **unsaved, browser-supplied (typed) URL**, before an admin ever
  > Saves it — a trust property this ADR's clone path did not have. Both
  > variants run behind the SAME controls described above: the SSRF
  > allowlist re-check, https-only, the cookie-only admin gate, the guarded
  > redirect-checking transport, and PAT-scrubbing on error. See
  > `prds/done/702-agent-source-follow-skills-harness-lift.md`.
- **Clone resource bounds (memory / inflate-bomb).** The `readRoleFiles`
  caps (16 files, 64KB/file, 1MiB total) run only AFTER go-git has decoded
  the whole tip snapshot into an in-memory storer, so they cannot bound the
  memory a hostile tip forces. `Depth:1`+`SingleBranch` bound history and the
  60s clone timeout bounds wall-clock, but neither bounds bytes. The clone's
  scoped `http.Client` adds a **cumulative wire-size cap** (`maxCloneWireBytes`,
  tens of MiB — generous for 16×64KB role files plus pack/protocol overhead)
  that errors the clone cleanly, stopping the plain giant-blob vector (a tip
  carrying one multi-GB blob) before it can OOM the api. **Residual, accepted
  and documented:** the wire cap bounds COMPRESSED bytes, not the
  RECONSTRUCTED/inflated size, so a zlib decompression-bomb pack (small on the
  wire, huge inflated) is not yet bounded on the CLONE path. Closing that half
  needs a reconstructed-size pre-scan of the fetched pack analogous to
  `pushbroker`'s `scanPackBudget` (which solves the same problem for the inbound
  checkpoint pack); it is a deliberate follow-up, not implemented here. The
  residual is acceptable because the mitigating preconditions are strong: the
  source must be on the admin-configured allowlist AND the feature explicitly
  enabled (both off by default), reconcile is single-flight on one goroutine,
  and the 60s timeout bounds the inflation wall-clock. An admin who allowlists
  and enables a malicious source has already handed it the far larger asset this
  ADR is about (Bash-agent system prompts).
- **Credential.** A private-repo token is sealed in `secretbox`, distinct
  from the forge push PAT, read-only, and scoped to clone only — it can
  never push.
- **Pinning.** The default is a pinned tag/SHA (Decision 7); a floating
  branch is an explicit, logged opt-in. A default-floating source would
  let an upstream force-push flow into every instance's next reconcile
  with no gate at all — the weakest posture available for a source of
  agent system prompts.
- **Blast radius.** A bad sync only ever affects `origin='synced'` rows.
  `admin`- and `embedded`-origin rows are structurally untouched by both
  reconcile and apply, and reset-to-embedded is always available for an
  overridden builtin.
- **No guardrail weakened.** The worker still holds the forge PAT, never
  the agent; the `PreToolUse` deny-hook and `settingSources: []` are
  unchanged. This feature adds a new **source of prompts**; it adds no new
  source of tools or credentials, and no existing guardrail's posture
  moves because of it.

## Consequences

- PRD #85's build-time drift check remains the honesty check for the
  embedded builtins; embedded builtins stay hand-maintained. Generating
  them *from* a synced source (turning #85's drift-check into a
  drift-*fix*) is a deliberate, later, separate step — not something this
  feature does as a side effect.
- Issue #201's `differs_from_builtin` drift badge becomes source-aware,
  but only for the override case (Decision/apply-case 1): "differs from
  embedded" and "differs from synced" are now two different questions with
  two different answers, and the badge/reset UI must name which source it
  means. A synced-only global template (apply case 2) has no embedded
  counterpart to diff against at all.
- A new admin surface implies a CLI check, and the existing `uzi admin`
  namespace is **read-only by established convention** — `docs/cli.md`
  documents that every other `admin` verb (`users`, `runs`, `workers`,
  `usage`, `rate-limits`, `cli-tokens`, `guardrail-impact`,
  `blocked-repos`) is read-only, and states the convention explicitly: "No
  `admin` writes... every admin write stays cookie-only — both are web UI
  actions by design." This surface follows that convention rather than
  breaking it: `uzi admin agent-source get | status` (read-only),
  mirroring the shape of `uzi admin guardrail-impact` /
  `uzi admin blocked-repos`. Enabling sync, editing the source config, and
  triggering "Sync now" stay web-only, like every other admin write in
  this CLI.

  > **Note — this corrects the PRD's Decision 14 / M6 draft text.** PRD
  > #602 Decision 14 and milestone M6, as drafted 2026-08-22, describe
  > `uzi admin agent-source {get,set,sync,status}` — a shape that includes
  > two writes (`set`, `sync`). That breaks the `uzi admin` read-only
  > convention `docs/cli.md` already documents and enforces for every
  > sibling `admin` command. This ADR's CLI-parity shape is read-only
  > (`get`, `status`); the PRD's Decision 14 / M6 text should be updated to
  > match once this ADR merges.

- `PatchRepo`-style "at least one of N related flags in one atomic
  round-trip" is not reused here — the source config (URL, ref, enable,
  interval) is a settings row, not a per-repo flag pair — but the same
  atomicity discipline applies to it: partial writes to source config must
  not leave enable-on with an empty/invalid URL, or vice versa.
