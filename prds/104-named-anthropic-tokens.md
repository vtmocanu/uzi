# PRD #104: Named Anthropic tokens — multiple credentials per user, bound to workers and the judge lane

**GitLab Issue**: [#104](https://gitlab.example.com/vtmocanu/uzi/-/issues/104)
**Status**: Draft (created 2026-07-21; revised 2026-07-21 after a citation-verification review)
**Priority**: Medium
**Related**: [#53](https://gitlab.example.com/vtmocanu/uzi/-/issues/53) (rate limits — this PRD repoints its table), [#32](https://gitlab.example.com/vtmocanu/uzi/-/issues/32) (user vault — whose rewrap path this PRD must fix before it can store a second token), [#50](https://gitlab.example.com/vtmocanu/uzi/-/issues/50) (LLM egress proxy — must learn which token to inject), [#69](https://gitlab.example.com/vtmocanu/uzi/-/issues/69) (judge mode + per-user model — draft; overlapping settings surface)

Seven milestones. M1 is the enabling migration and unblocks four independent
tracks (M2, M3, M4, M5); M6 (web) needs the API shapes from M2/M3/M4/M5; M7
(docs + specs) lands last.

## Problem

uzi assumes **exactly one Anthropic credential per user**. The assumption is
baked into the schema:

```sql
-- api/internal/store/migrations/00010_user_secrets.sql
kind TEXT NOT NULL CHECK (kind IN ('anthropic_token')),
UNIQUE (user_id, kind)
```

and into every read path, all of which resolve "the user's token" by
`(user_id, kind)` rather than by identity:

- `api/internal/workersvc/service.go:682` — `openAnthropic`, the **single**
  function shared by all three lanes: the run lane (`service.go:723`), the
  judge lane (`judge.go:139`), and the chat lane (`chat.go:175`).
- `api/internal/workersvc/judge_enqueue.go:76-79`, `judge_read.go:128-131` —
  the "does this user even have a token?" gates.
- `api/internal/usagepoller/engine.go:143,175` — the rate-limit poller.
- `api/internal/store/queries/anthropic_rate_limits.sql` —
  `ListUsersWithAnthropicToken:8`, the admin join at `:48`, and
  `UserHasAnthropicToken:61-66` (which feeds the `no_token` status).
- `api/internal/store/queries/autopilot.sql:30-31` (`has_anthropic_token`),
  consumed by `api/internal/poller/autopilot.go:293`.
- `api/internal/seed/anthropic.go:79,95` — `UZI_SEED_ANTHROPIC_TOKEN`.
- `api/internal/handler/secrets.go:103,119,147` — the REST surface
  (`PUT`/`DELETE /api/me/secrets/anthropic_token`), path-hardcoded to the kind.

And, most consequentially, into the **write** paths — see "The blocker M1 must
clear" below.

This costs real capability today:

**1. You cannot spread burn across accounts.** Anthropic's 5-hour and 7-day
windows are per-credential. A user with a Max subscription *and* a console API
key (or two subscriptions) can only ever wire one of them into uzi, so the
second sits idle while the first throttles every worker the user owns.

**2. You cannot separate implementation burn from review burn.** The judge lane
calls the same `openAnthropic` as the run lane (`judge.go:139` vs
`service.go:723`). There is no way to bill retrospectives to a cheap console key
while runs go against a subscription.

**3. Rotation is destructive.** `docs/anthropic-token.md:45` documents rotation
as "paste a new token and click **Save new token**; it overwrites the old one" —
enforced by `UNIQUE (user_id, kind)`. There is no way to stage a replacement,
verify it on one worker, then cut over. A bad paste takes every worker down at
once, and (per the same doc) uzi does not validate tokens at save time, so the
failure surfaces only on the next run.

**4. Attribution is guesswork.** `run_usage` (PRD #40) records what a run spent,
but not which credential paid. With one token per user that was tautological;
the moment a user wants two, it stops being. (See D3/R6 — this PRD improves it
but does not fully solve it.)

The user-facing ask: **give each token a name, let a worker be pointed at a
token by name, and let that pointer be changed later without re-provisioning
the worker.**

## Why this is cheap on the worker side

**Workers never hold the Anthropic token.** The API decrypts it and ships it
inside each claim response — `api/internal/workersvc/claim.go:134`
(`anthropic_oauth_token`), mirrored in `chat.go:115`. The agent consumes it as
an env var it never persists (`agent/src/sdk-env.ts:75`,
`CLAUDE_CODE_OAUTH_TOKEN`), and `agent/src/protocol.ts:214` (run lane, optional)
and `:408` (chat lane, required) already type the field as a plain string.

Three consequences:

- **No worker protocol change.** The wire field keeps its name, type, and
  meaning. `agent/` is untouched by this PRD.
- **No re-provisioning.** Rebinding a worker to another token takes effect on
  that worker's **next claim** — seconds, not a container restart.
- **Hosted workers come along for free.** `hostedsvc`/`controller` provision
  worker pods that claim through the same path; M3's binding covers them with no
  controller change.

The change is therefore concentrated in **which row the API selects**, plus the
surfaces that name and display those rows — *once M1 clears the blocker below.*

## The blocker M1 must clear (do not skip this)

Storing a second `anthropic_token` row for a user is **not safe today**. Three
write paths are keyed by `(user_id, kind)` and silently assume that pair
identifies at most one row:

**1. The vault rewrap destroys sibling tokens.** `RewrapUserSecret`
(`api/internal/store/queries/user_secrets.sql:63-68`) is:

```sql
UPDATE user_secrets
SET ciphertext = @ciphertext, sealed_with = 'dek', updated_at = now()
WHERE user_id = @user_id AND kind = @kind AND sealed_with = 'master';
```

`rewrapMasterSecrets` (`api/internal/vault/vault.go:216-241`) loops the rows from
`ListMasterSealedSecrets` (`user_secrets.sql:45-46`, which selects `kind,
ciphertext` — **no id**) and issues one such UPDATE per row. With N
master-sealed rows of the same kind, the **first** iteration's UPDATE matches
**all N** and overwrites every sibling with token 1's resealed bytes; the
remaining iterations then find nothing. Tokens 2..N are gone.

This is reachable in a supported configuration: a vault-nil deployment seals
everything `'master'` (`handler/secrets.go:26-33`), and PRD #32's documented
upgrade is to enable the vault later — at which point the owner's first unlock
runs exactly this loop.

**2. `UpsertUserSecret` requires the constraint M1 drops.**
`ON CONFLICT (user_id, kind)` (`user_secrets.sql:6-11`) names a unique index by
its exact column list; dropping that index makes the statement fail at runtime.
It is the write path behind both the REST save and the seed.

**3. `DeleteUserSecret` deletes every row of the kind.**
`DELETE FROM user_secrets WHERE user_id = $1 AND kind = $2`
(`user_secrets.sql:27-28`) — one call would wipe a user's whole token set.

M1 is therefore not "add two columns". It is **re-key the secret write paths on
`user_secrets.id`** (D10), and it is the reason M1 must land alone before any
milestone that can create a second row.

## Solution

1. `user_secrets` gains `label` and `is_default`; the `UNIQUE (user_id, kind)`
   constraint is replaced by a unique expression index on
   `(user_id, kind, lower(label))` plus a partial unique index enforcing at most
   one default per `(user_id, kind)`.
2. The secret write queries become id-keyed (D10), and `secretopen` gains a
   by-id open alongside the by-kind open.
3. `workers` gains `anthropic_secret_id`, with ownership enforced by a composite
   FK (D11). NULL means "use my default token".
4. `users` gains `judge_anthropic_secret_id` with the same semantics for the
   judge/self-improve lane.
5. `anthropic_rate_limits` is repointed from `PRIMARY KEY (user_id)` to
   `PRIMARY KEY (user_secret_id)`, and `usagepoller` polls every token.
6. Settings grows a token **list** (add / rename / set-default / delete), the
   worker rows grow a token picker, the judge settings grow a token picker, and
   `RateLimitMeters` renders one meter pair per token.

### Decisions

**D1 — Binding reaches workers and the judge lane, not repos or runs.**
A worker names a token; the judge lane names a token; everything else (chat,
autopilot, CI-fix, which rides the run lane) resolves the user's default.

*Amended during M4: there are **three** resolution rules, not two.* A
`self_improve` run is repo-ful and therefore rides the **ordinary run lane**
(`assembleClaim`, not `assembleJudgeClaim`, which forks only on
`run.Kind == RunKindJudge`), so "self-improve follows the judge binding" — stated
in M4 below as though it were free — required an explicit branch. Without it a
self-improve run would have followed the *claiming worker's* binding while
appearing handled, and no test would have asked. All three rules live in
`claimSecretID` so R4's "resolution lives in one place" still holds. The original
two-item phrasing named neither self-improve nor CI-fix explicitly; a future rule
must be added there and nowhere else.
Per-repo and per-run pinning are deliberately excluded: they multiply the
resolution matrix without a demonstrated need, and per-run pinning in particular
would have to survive requeue-to-another-worker, which conflicts with the
worker-affinity rule in `runs.worker_id` (`AffinityCutoff` in the claim queries).

**D2 — An explicit `is_default` flag, not "oldest wins".**
Exactly one row per `(user_id, kind)` carries `is_default = true`, enforced by a
partial unique index. The existing token migrates to `label = 'default'`,
`is_default = true`. Rationale: "oldest wins" changes silently when you delete a
token, which is exactly the moment a user is least able to notice that every
unbound worker just moved to a different account. See D12 for what the index
does *not* enforce.

**D3 — Auto-failover on rate limit is OUT OF SCOPE.**
When a worker's bound token is exhausted, its runs throttle; uzi does not
silently move them to another credential. Failover needs its own policy design
(which token, chosen per-run or per-worker, what happens mid-run, and how
`run_usage` attributes the spend across a switch). It gets its own PRD if
wanted; this one makes it *possible* by giving it a set of named tokens to
choose from.

**D4 — Rate limits become per-token, table and UI together.**
`anthropic_rate_limits` keeps a `user_id` column (for the admin cross-user view
and the cascade) but keys on `user_secret_id`. `GET /api/me/rate-limits` returns
an array of `{secret_id, label, is_default, limits}` instead of a single
reading — a **breaking response-shape change**, so the web client, the CLI
(`api/cmd/uzi/admin.go:118-149`, `api/internal/uzicli/client.go:532-536`,
`api/internal/apitypes/ratelimit.go`), and the e2e assertions move in the same
MR. Alternative rejected: polling only the default token — the meters would keep
rendering confidently while describing an account that most of the user's
workers are no longer spending against, which is worse than no meter.

**D5 — Deleting a bound token unbinds, it does not cascade-delete workers.**
`ON DELETE SET NULL` on both `workers.anthropic_secret_id` and
`users.judge_anthropic_secret_id`; affected workers silently fall back to the
default. The UI must name the affected workers in the delete confirmation (quiet
fallback is acceptable behavior, not acceptable UX).

**D6 — The default token cannot be deleted while other tokens exist.**
Promote another token first. This keeps "there is always a default while any
token exists" a true statement the resolution path can rely on, instead of a
runtime branch every consumer has to re-derive — and it is what preserves the
existing judge/autopilot gates unchanged (see "Gates that survive" below).
Deleting the *last* token is allowed and returns the user to the current
token-less state. Enforcement is transactional, not schema-level (D12).

**D7 — Labels are user-scoped, case-insensitively unique, and not secret.**
A unique index on `(user_id, kind, lower(label))`. Labels appear in the UI, the
CLI, admin views, and logs; the token value continues to appear nowhere.
Validation mirrors `maxTokenBytes`'s spirit in `handler/secrets.go:37`: bounded
length (64), no control characters, no leading/trailing whitespace.

**D8 — The token-CRUD routes stay cookie-only; worker rebinding is `RequireUser`.**

*Amended during M2: this decision and M2's stated CLI scope contradicted each
other, and D8 wins.* M2's milestone text listed
`uzi token list|add|rename|set-default|rm` while the same sentence kept write
paths cookie-only. Those are incompatible — the CLI authenticates only with
`Authorization: Bearer <uzc_>`, which a `RequireAuth` route rejects, so every
write command would 401. **Only `uzi token list` exists**, mirroring the `uzi
worker` precedent (no `worker create`, for exactly this reason), and
`TestTokenHasOnlyList` pins the absence so nobody adds one by reflex. The
command's help says the writes are web-only and why.

`GET /api/me/secrets` moved from `RequireAuth` to `RequireUser` to make `token
list` reachable — safe, since it returns labels, ids and flags, never a value.

Relaxing D8 to allow CLI writes would make token create/rotate/delete
Bearer-reachable, so a stolen `uzc_` could replace a user's credentials rather
than merely read metadata. That is the precise escalation D8 exists to prevent,
and it is not taken.
`/api/me/secrets/*` is `RequireAuth` today (`handler/handler.go:320`) and stays
that way: minting and replacing credentials is the CLI's exclusion zone,
consistent with `POST /workers` being cookie-only because its join token yields a
decrypted credential (`handler.go:615-619`). Rebinding a worker between two of
the *user's own* tokens grants no credential the caller lacks, so the new
`PATCH /api/workers/{id}` is `RequireUser` and reachable from
`uzi worker set-token` — **provided** the ownership check in D11 holds.

**D9 — Kind stays `anthropic_token`.**
The `kind` column's CHECK is unchanged and the table stays generic. Labels are a
new axis, not a replacement for kinds — a future `openai_token` kind should get
the same label treatment for free.

**D10 — The secret write paths are re-keyed on `user_secrets.id`.**
`RewrapUserSecret` takes an id and `ListMasterSealedSecrets` returns one, so the
rewrap loop updates exactly the row it opened (fixing the clobber above);
`UpsertUserSecret` splits into an explicit `InsertUserSecret` plus an
id-targeted `RotateUserSecret`, retiring the `ON CONFLICT (user_id, kind)`
target; `DeleteUserSecret` takes an id. `GetUserSecretCiphertext` keeps its
by-kind form (it becomes "the default", see D14) and gains a by-id sibling.
This is a **correctness fix to PRD #32's existing code**, not new feature work,
and it is the single reason M1 cannot be merged as part of a larger milestone.

**D11 — Cross-user binding is blocked by a composite FK, not just a handler check.**
A plain `workers.anthropic_secret_id REFERENCES user_secrets (id)` guarantees the
row exists, not that the caller owns it: a crafted `PATCH /api/workers/{id}`
would otherwise point a worker at another user's credential and spend their
account. `user_secrets` therefore gains `UNIQUE (user_id, id)` so `workers` can
carry `FOREIGN KEY (user_id, anthropic_secret_id) REFERENCES user_secrets (user_id, id)
ON DELETE SET NULL`. The handler validates ownership too (for a 404 rather than a
constraint violation), and `OpenByID` re-checks `(user_id, kind)` — defense in
depth, with the schema as the backstop. Same treatment for
`users.judge_anthropic_secret_id`.

**D12 — The default invariant is serialized by an ADVISORY LOCK, not `FOR UPDATE`.**

*Corrected during M2. The original text below specified `SELECT ... FOR UPDATE`,
which is the wrong primitive and would not have closed the races this decision
names.* `FOR UPDATE` locks **existing rows**: it cannot block a concurrent
`INSERT` of a new row, and it locks nothing at all when the set is empty — which
are exactly races (a) and (b). Every mutation instead takes
`pg_advisory_xact_lock(SecretMutationLockClass, objid(user_id))` as its
transaction's first statement, matching the hosted-provision quota lock already
in this codebase (`store/migrate.go`).

Proven, not asserted: with the lock removed,
`TestConcurrentDeleteDefaultVsCreateLiveDB` fails with
`user has 1 tokens but 0 defaults — the delete-vs-create race left a no-default
state` — precisely the state the kind-path alias 500s on. With the lock, 25
interleavings pass.

**Which test proves what** (recorded because the two are not equivalent — and
this paragraph was itself wrong once, see below):

Both tests fail without the lock, for *different* reasons, and both are earning
their place:

| test | lock removed | what breaks |
|---|---|---|
| `TestConcurrentDeleteDefaultVsCreateLiveDB` | FAIL — `1 tokens but 0 defaults` | **state corrupts** |
| `TestConcurrentFirstTokenCreatesLiveDB` | FAIL — 7× `concurrent create returned 500` | **contract breaks** |

The distinction: **the partial unique index protects the data; the lock protects
the user-visible outcome.** Without the lock, concurrent first-token creates are
*safe but ugly* — the index still prevents a second default, so the invariant
holds, but the losing goroutines hit `duplicate key … user_secrets_one_default_key`
which the handler maps to a **500** instead of a clean 201/409. Only the
delete-vs-create test shows absence of the lock corrupting *state*.

*Corrected 2026-07-21.* This paragraph originally claimed
`TestConcurrentFirstTokenCreatesLiveDB` **passes** without the lock, on the
implementer's report. The reviewer removed the lock and ran both tests: it fails.
The wrong version was the dangerous one — "test B passes without the lock" reads
as "test B is decorative" and invites deleting a test that genuinely guards the
response contract.

*Original text, retained for provenance:*

**D12 — The default invariant is transactional; the index alone is not enough.**
The partial unique index enforces **at most** one default. "Exactly one while any
token exists" is not schema-enforceable, and three races exist: two concurrent
first-token creates both claiming default; a delete-default that passes its
"no other tokens" check while a second token is being inserted (leaving tokens
with no default, so every fallback consumer hits `ErrNoSecret`); and set-default,
which is a two-statement clear-then-set swap. All three mutations therefore run
in one transaction that first takes `SELECT ... FROM user_secrets WHERE user_id
= $1 AND kind = $2 FOR UPDATE`. M2 owns this; its tests must include the
concurrent-create and concurrent-delete cases.

**D13 — This PRD takes goose numbers `00077`–`00080`.**
Live head is `00074_plan_revision.sql`. `00075` is held by the unmerged prd-98
branch and `00076` by prd-99, so this PRD starts above both: **M1 = `00077`**
(landed), M3 = `00078`, M4 = `00079`, M5 = `00080`. M2 adds no migration — its
columns landed in M1.

*Superseded:* this decision originally reserved `00104`–`00106` "to match the PRD
number". That was a drafting error with no basis in the repo's convention, which
is to renumber to the next free number above the live head at merge (recorded in
`00065_anthropic_rate_limits.sql`). Reserving a block three tens above the head
would have collided with nothing but signalled a numbering scheme that does not
exist. Corrected 2026-07-21 during M1.

Other outstanding drafts held elsewhere, for whoever merges next: #50 → `00085`,
#35 → `00095`, #99 → `00074` (already collides with the live head — that PRD's
problem, noted here so the next reader does not inherit it silently).

**D14 — The kind-path routes stay as compatibility aliases over the default.**
`PUT /api/me/secrets/anthropic_token` rotates the default (or creates the first
token, labelled `default`). `DELETE` on the same path deletes the default —
which, by D6, means it **409s for a user with more than one token**, breaking its
current unconditional-success contract. That is deliberate and must be
documented in `docs/cli.md` and the OpenAPI/docs surface: a multi-token user
deletes by id. Both aliases are marked deprecated in the same MR that adds their
replacements.

### Gates that survive unchanged (verified)

`judge_enqueue.go:76-79`, `judge_read.go:128-131`, `autopilot.sql:30-31`, and
`UserHasAnthropicToken` (`anthropic_rate_limits.sql:61-66`) all ask
"does a row of this kind exist for this user?". Under D6, presence-of-any-row
implies presence-of-a-default, so presence-of-any ≡ presence-of-resolvable —
these gates keep working with no change. **This is a load-bearing consequence of
D6**: relax D6 and all four gates become wrong (they would green-light a run whose
resolution then fails), so any future PRD that revisits D6 must revisit them.

Two of the four are load-bearing on D6 in a stronger sense than the others, and
M1 is what makes them so. `judge_enqueue.go` and `judge_read.go` reach the row
through `GetUserSecretCiphertext`, which M1 narrows to "the default" (it must, so
the single-token read paths keep resolving exactly one row once the unique
constraint is gone). Their question therefore changes from *"any row exists"* to
*"a default exists"* — still equivalent under D6, but resting on D6 rather than on
any-row semantics. `autopilot.sql:30-31` and `UserHasAnthropicToken` are
independent `EXISTS` queries and keep true any-row semantics. If a future change
relaxes D6, the judge pair breaks first and silently.

### Open question (not blocking M1)

**PRD #50 (LLM egress proxy) intersects this.** That design has the API inject
the real token server-side at `/llm-proxy/v1/messages`, keyed by the run. Once a
run's credential depends on the claiming worker's binding, the proxy's token
lookup must key off the same resolution. Whichever of #50 and #104 lands second
owns the reconciliation; M1's `OpenByID` is the shared primitive either way.

## Milestones

Phase 1 (enabling, blocking — must merge alone):

- [ ] **M1 — Re-key the secret paths, then add labels.** Two halves in one MR,
      in this order. **(a) The correctness fix (D10):** `ListMasterSealedSecrets`
      returns `id`, `RewrapUserSecret` targets it, `UpsertUserSecret` splits into
      insert + id-targeted rotate, `DeleteUserSecret` takes an id; `vault.go`'s
      rewrap loop updated accordingly. Prove it with a test that rewraps **two**
      master-sealed rows of the same kind and asserts both survive with distinct
      plaintexts — a test that fails on today's code. **(b) The feature:**
      `label` (NOT NULL) and `is_default` (NOT NULL DEFAULT false); drop
      `UNIQUE (user_id, kind)`; add the unique expression index on
      `(user_id, kind, lower(label))`, the partial unique index on
      `(user_id, kind) WHERE is_default`, and `UNIQUE (user_id, id)` for D11;
      backfill the existing row to `label='default'`, `is_default=true`;
      `secretopen.OpenByID` with the same
      `ErrNoSecret`/`ErrUndecryptable`/`ErrVaultLocked` sentinels;
      `seed/anthropic.go` seeds label `default`.
      **Also lands the shared resolver shape** so M3 and M4 stay file-disjoint:
      `openAnthropic` (`service.go:682`) gains a binding-else-default helper that
      all three lanes call, rather than M3 and M4 each restructuring it.
      Acceptance: existing api tests green, no behavior change visible to any
      user with one token.

Phase 2 (parallel, all depend only on M1):

- [ ] **M2 — Token CRUD, API + CLI.** No migration (M1 landed the columns).
      `GET /api/me/secrets` returns labels, default flag, and per-token
      timestamps; new `POST /api/me/secrets/anthropic_token` (create with label),
      `PATCH /api/me/secrets/anthropic_token/{id}` (rename, set-default, rotate
      value), `DELETE .../{id}` (D5/D6 rules). D14's aliases kept and deprecated.
      **D12's transaction + `FOR UPDATE` is this milestone's core risk** —
      concurrent-create and concurrent-delete-default tests are acceptance
      criteria, not extras.
      **Three defects M1 deliberately left live for M2 to close** (found by the
      M1 audit, verified against Postgres 17; they are unreachable in M1 because
      no second token can exist yet, and become reachable the instant this
      milestone ships): (i) a user in the "tokens exist, none is default" state
      gets a raw **500** from `PutAnthropicToken` — the alias INSERT finds no
      arbiter conflict and collides on the label index instead
      (`user_secrets_user_kind_label_key`); D12's `FOR UPDATE` transaction is
      what makes that state unreachable, so no handler branch is wanted, only the
      transaction. (ii) `DELETE /api/me/secrets/anthropic_token` deletes the
      default unconditionally instead of D14's 409 for a multi-token user.
      (iii) `RotateUserSecret` exists with no caller until this milestone wires
      the id-keyed rotate route — it is not dead code. `uzi token list|add|rename|set-default|rm`, read
      paths `RequireUser`, write paths per D8. Every create/rotate pokes the
      usage poller (`handler/secrets.go:128-132` today pokes by user id;
      coordinate the new poke identity with M5 — whichever lands first defines
      it). Metadata only — no reveal endpoint, ever.
- [ ] **M3 — Worker binding.** Migration: `workers.anthropic_secret_id` with the
      D11 composite FK. The M1 helper resolves the worker's binding first and the
      user's default second; `PATCH /api/workers/{id}` (`RequireUser`, D8, with
      the ownership check) sets/clears it; `POST /api/workers` accepts an
      optional label at mint time; `uzi worker set-token <worker-id> <label|--default>`.
      Tests: (a) flipping the binding between two claims changes the payload with
      no worker restart; (b) a PATCH naming another user's secret id is refused
      by both the handler and, with the handler check bypassed, the FK.
      **Also investigate and record R6's session-resume question** before this is
      called done.
- [ ] **M4 — Judge-lane binding.** Migration: `users.judge_anthropic_secret_id`
      (D11 treatment). `judge.go:139` resolves it via the M1 helper, falling back
      to the default. **`PUT /api/me/judge` currently carries only `enabled`**
      (`handler/judge.go:28-49`) and `judge_model` is a *global admin* setting
      (`settings.KeyJudgeModel`, `AdminSettings.tsx:392-428`) — per-user judge
      model is PRD #69, still Draft. So M4 **extends** `PUT /api/me/judge` with a
      token field; it must not assume #69's surface exists, and if #69 lands
      first the two settings merge there. Self-improve runs follow the judge
      binding.
- [ ] **M5 — Per-token rate limits.** Migration: `anthropic_rate_limits`
      repointed to `PRIMARY KEY (user_secret_id)`, retaining `user_id`.
      `usagepoller/engine.go` sweeps every token per tick instead of one per
      user, preserving the locked-vault skip and fail-closed reading rules.
      `GET /api/me/rate-limits` and `GET /api/admin/rate-limits` return per-token
      arrays (D4) — **and this MR carries its own consumers**: `apitypes/ratelimit.go`,
      `uzicli/client.go:532-536`, `cmd/uzi/admin.go:118-149`, and the e2e seed at
      `e2e/run-e2e.sh:3527-3535` (whose `ON CONFLICT (user_id)` is invalid after
      the repoint — e2e is the local pre-merge gate, so it cannot wait for M7).
      Poll cost scales with token count: measure a 3-token user against
      `UsagePollInterval` before calling this done.

Phase 3 (needs M2/M3/M4/M5 API shapes):

> **Carry-forward from the M3/M4 audit (LOW):** `workerDTOFromWorker` renders a
> bound worker with an id and a *null* label unless the caller passes the
> just-resolved label. Every current caller does, so it is a sharp edge, not a
> defect — but M6's picker must render from a source that always carries the
> label (the list path's joined `workerDTOFromRow` does; the create/rebind
> response needs the label threaded, or the UI must re-fetch). Do not render a
> binding as "spends: (none)" when it is actually bound.

- [ ] **M6 — Web UI.** Settings → Anthropic token becomes a token **list**
      (label, set date, default badge, per-token meters, add/rename/set-default/
      delete with the D5 affected-workers warning); `WorkersSettings.tsx` grows a
      per-worker token picker showing the effective token when unbound; the judge
      settings grow a token picker (**M4's control — without it the success
      criterion "the judge lane can burn a different token" is unreachable from
      the UI**); `RateLimitMeters.tsx`, `web/src/lib/rateLimits.ts`, and
      `Dashboard.tsx` (`:115` hasToken) render one meter pair per token;
      `AdminRateLimits.tsx` groups by user then token; `lib/api.ts` types and the
      `mocks/mockApi.ts:760-787` + `mocks/data.ts:392` fixtures the tests read.
      `IssueView.tsx:56` and `Board.tsx:160` hasToken checks are verified
      unaffected (any-row semantics) — no change expected, assert it.

Phase 4 (last):

> **Carry-forward from the M3/M4 audit (LOW):** the judge lane's two writes — the
> `enabled` flag and the token binding — are deliberately non-transactional
> (independent settings, both user-visible and re-doable). It is the one place in
> the M3/M4 diff where a half-applied pair is observable if a request fails
> between the two statements. M7's `docs/judge.md` should state that enabling the
> judge and choosing its token are separate saves, so a partial failure leaves a
> visible, correctable state rather than a silent one.

- [ ] **M7 — Docs and specs.** `docs/anthropic-token.md` rewritten around
      multiple named tokens (its line 45, "it overwrites the old one", is *made
      false* by this PRD and must change in the same MR); `docs/rate-limits.md`,
      `docs/worker-setup.md`, `docs/judge.md`, `docs/cli.md` (incl. D14's alias
      deprecation and the 409), `docs/configuration.md` + `.env.example:197-206`
      (`UZI_SEED_ANTHROPIC_TOKEN` now seeds label `default`),
      `docs/vault-threat-model.md` (D10 changed the rewrap path it describes);
      `ARCHITECTURE.md` §Secrets (line 294); `specs/ai.md` (the rebuild-from-specs
      contract); the uzi-cli SKILL.md gains the new commands. Plus the e2e
      **binding** scenario (two tokens, rebind, assert the claim payload changed)
      — distinct from M5's e2e *fix*, which is a prerequisite, not this.

## Success criteria

- A user can store three Anthropic tokens with distinct labels; Settings shows
  three rows and three meter pairs, and exactly one carries the default badge.
- Pointing worker `alpha` at label `console-key` changes the credential in
  `alpha`'s **next** claim payload, with no container restart and no re-mint of
  its join token.
- A vault unlock with three master-sealed tokens leaves all three openable with
  their original plaintexts (the M1(a) regression test).
- A `PATCH` binding a worker to another user's secret id is refused, and is still
  refused with the handler check stubbed out.
- Deleting a bound token unbinds its workers (they fall back to the default) and
  the delete confirmation named those workers before it happened.
- Deleting the default while another token exists is refused with a message that
  says to promote another first — including under a concurrent insert (D12).
- The judge lane can burn a different token from the run lane, set from the web
  UI, verified by two meters moving independently.
- A user who never touches this feature sees no change: one token, labelled
  `default`, one meter pair, every existing route and CLI command behaving as
  before.
- The token value still appears in no response, log, or error — the existing
  redaction tests (`api/internal/handler/ci_fix_scrub_test.go`,
  `agent/src/redact.ts`) extended to cover labels being present while values are
  not.

## Risks

- **R1 — M1(a) is a live-data correctness fix, and its failure mode is silent.**
  A wrong id-keyed rewrap does not error; it destroys tokens. The two-row rewrap
  test is the gate, and it must be written to fail against pre-M1 code first.
- **R2 — The rate-limit table repoint discards existing rows.** The reading is a
  disposable gauge (PRD #53 D4: no history — see the comment in `00065`), and
  absent rows render `unavailable` rather than erroring
  (`handler/ratelimits.go:29-47`), so the migration drops and lets the poller
  refill on the next tick. **Caveat to state in the migration comment**: a
  deployment with the poller disabled (`UZI_USAGE_POLL_INTERVAL=0`, which is the
  e2e overlay) never refills, so meters read `unavailable` there permanently.
- **R3 — Poll cost multiplies.** `usagepoller` currently does one Anthropic call
  per user per tick; per-token makes it N. Keep the fail-closed and locked-vault
  skips, and measure before M5 is done.
- **R4 — Resolution-order bugs are silent and expensive.** A wrong fallback
  spends the wrong account and nothing errors. Mitigation, as *built* through M4
  (refined from the original "one function" wording, which the M3/M4 review
  showed was imprecise): the credential **open** is genuinely one function
  (`openAnthropic`, three call sites — run `service.go:802`, judge `judge.go:172`,
  chat `chat.go:177` with explicit nil). The binding **selection** is not one
  function — it is `claimSecretID` (worker rule + self_improve→judge rule) plus
  `assembleJudgeClaim`'s direct `judgeSecretID` call plus chat's nil — but the
  guarantee that matters holds: **each of the three selection rules is expressed
  exactly once, and self_improve and judge share `judgeSecretID` so they cannot
  drift apart.** That is R4's real property (no divergent copies of a rule), not
  "resolution in a single function". Backed by table-driven tests over the
  (worker bound? judge bound? default exists?) matrix and the resolved label
  logged (label, never value) on every claim.

  *Note on symmetry, from the review:* "a failed read of the binding fails the
  claim" is strictly true only for the **judge** lane (`judgeSecretID` at
  `judge.go:140` propagates a lookup error, pinned by
  `TestJudgeBindingLookupErrorFailsClaim`). The **worker** lane has no such read
  to fail — `workerSecretID` reads `AnthropicSecretID` off the already-claimed
  worker row, so there is no separate lookup. Not a gap; the two lanes are
  asymmetric by construction, and a future reader should not expect an M3
  read-failure path that cannot exist.
- **R5 — D14's alias behavior change.** `DELETE /api/me/secrets/anthropic_token`
  starts returning 409 for multi-token users. Low blast radius (the web UI moves
  to the id routes in M6) but it is a contract change and belongs in the docs
  diff, not just the code.
- **R6 — Requeue is an unacknowledged mid-run rebind. RESOLVED during M3: resume
  is NOT at risk; attribution is, and rides as a documented limitation.**

  *Investigated 2026-07-21.* The original premise — that resuming an SDK session
  under a different account's token might fail — is **wrong**. Session state is a
  local JSONL transcript under `$HOME/.claude/projects/<encoded-cwd>/<session-id>.jsonl`,
  not server-side account-bound state; `agent/src/sdk-env.ts:19-21` says so
  explicitly ("HOME is pinned onto the persistent data volume … so the SDK's
  session transcripts … survive a container restart (resume)"). Resume replays
  that local file and re-sends the history; the OAuth token is the credential on
  the next API call, not part of session identity. **So pinning the resolved
  secret id on the run row would protect nothing, and is not done.**

  What remains is the attribution half: `run_usage` cannot say which credential
  paid across a requeue that changed the binding. That is real but small, and
  pinning is deliberately still rejected — it would introduce a second source of
  truth for "which token" that must agree with the binding forever after, on a
  PRD whose top risk (R4) is precisely resolution-order divergence. M7 documents
  it as a limitation.

  *Unverified residual, flagged not designed around:* the chat lane uses a shared
  `sdkHomeRoot` (`agent/src/main.ts:146`, deliberate — a Continue is a new run id
  resuming the same session), so one `$HOME/.claude` can see token A then token B.
  If the SDK caches account-scoped metadata there, behavior is unknown. Chat is
  unbindable under D1, so this is reachable only via a default flip, never a
  worker rebind.

  *Separate pre-existing bug found while investigating, filed as its own issue:*
  the run lane's `agent-home/<runId>` (`main.ts:103`) lives on the **claiming
  worker's** data volume, so a requeued run re-claimed by a different worker once
  the affinity grace lapses has no transcript to resume — today, with one token
  per user, unrelated to this PRD.

  *Original text, retained for provenance:* The token is delivered only at claim (no later
  re-delivery in `chat.go`, `judge.go`, or the steering path), but the stale-run
  sweeper requeues and the affinity grace re-claims — so a rebind or default flip
  between claims switches credentials mid-run. Two consequences: Problem-4's
  attribution is *improved but still not exact* across a requeue, and resuming an
  SDK session (`chat.go:186-190`, plus the run-lane resume) under a **different
  account's** token may not work at all — Anthropic session state may not
  transfer. M3 must test this and record the answer; if resume breaks, the fix is
  to pin the resolved secret id on the run row at first claim, which is a scope
  addition, not a redesign.
- **R7 — "One worker, two accounts" is a real user-visible subtlety.** Under D1,
  a bound worker's *chat* runs still spend the default, because chat is not
  bound. "Worker alpha spends console-key" is therefore true of the run lane
  only. M7's docs must say this in plain words rather than leaving users to infer
  it from a meter.
- **R8 — PRD #50 collision.** See the open question; the two PRDs must not both
  invent a token-resolution path.
- **R9 — M2-before-M5 opens a gauge race, if this PRD is ever split across MRs.**
  `ListUsersWithAnthropicToken` (`anthropic_rate_limits.sql:8`) selects every
  `anthropic_token` row with no `is_default` filter and no id, and
  `UpsertRateLimits` is `ON CONFLICT (user_id)`. M2 is what first creates a second
  row; M5 is what repoints the gauge to `user_secret_id`. In a deployment where M2
  has landed and M5 has not, a multi-token user's tokens race for one gauge row
  every tick and the meters flip between accounts with no indication. **This PRD
  is being delivered as one branch and one PR, so all seven milestones land
  together and the window never opens.** The risk is recorded for whoever later
  splits it: if you do, either merge M5 before M2 or gate M2's create path on M5.
- **R10 — multi-token narrows the DEK AAD's integrity guarantee.**
  `vault.secretAAD` (`vault.go:364-370`) binds a sealed secret to `user_id||kind`,
  which `secretbox.go` and `docs/vault-threat-model.md` sell as "a DB-write
  operator cannot swap a ciphertext onto a different owner/kind". That was a
  per-ROW binding only because `UNIQUE (user_id, kind)` made kind identify the
  row. With N named tokens the AAD is identical across all of them, so a DB-write
  operator can move token A's ciphertext onto the row labelled `console-key` and
  it authenticates cleanly — the bound worker then spends account A while the
  label UI says otherwise. **Decision: document, do not fix.** The adversary
  required is DB-write, strictly stronger than the passive-read adversary the
  vault targets, and putting the row id in the AAD would need a versioned AAD
  scheme or a rewrap migration of every existing ciphertext. M1 states the
  narrowing in its migration comment and corrects the stale `secretAAD` comment;
  M7 carries it into `docs/vault-threat-model.md` as a residual risk.

## Parallel execution plan

| Phase | Milestones | Depends on | Migration | Files touched | Parallel? |
|---|---|---|---|---|---|
| 1 | M1 | — | yes | `store/queries/user_secrets.sql`, `vault/vault.go`, `store/migrations/`, `secretopen/`, `seed/anthropic.go`, `workersvc/service.go` | no (blocking, merges alone) |
| 2 | M2 | M1 | no | `handler/secrets.go`, `handler/handler.go`, `store/queries/user_secrets.sql`, `uzicli/`, `cmd/uzi/` | yes |
| 2 | M3 | M1 | yes | `store/migrations/`, `workersvc/service.go`, `handler/workers.go`, `uzicli/`, `cmd/uzi/` | yes |
| 2 | M4 | M1 | yes | `store/migrations/`, `workersvc/judge.go`, `handler/judge.go`, `store/queries/users.sql` | yes |
| 2 | M5 | M1 | yes | `store/migrations/`, `usagepoller/engine.go`, `handler/ratelimits.go`, `apitypes/ratelimit.go`, `uzicli/client.go`, `cmd/uzi/admin.go`, `e2e/run-e2e.sh` | yes |
| 3 | M6 | M2, M3, M4, M5 | no | `web/src/pages/Settings.tsx`, `WorkersSettings.tsx`, `AdminRateLimits.tsx`, `Dashboard.tsx`, `components/RateLimitMeters.tsx`, `lib/rateLimits.ts`, `lib/api.ts`, `mocks/` | no |
| 4 | M7 | all | no | `docs/`, `.env.example`, `ARCHITECTURE.md`, `specs/ai.md`, `e2e/`, `uzicli/skill/SKILL.md` | no |

M2 touches `store/queries/user_secrets.sql` after M1 rewrote it — the only
Phase-2 file overlap, and it is additive (new CRUD queries beside M1's re-keyed
ones). M3 and M4 both touch `workersvc` but different files, which holds **only
because M1 lands the shared resolver helper**; if M1 ships without it, M3 and M4
collide in `service.go` and must serialize.
