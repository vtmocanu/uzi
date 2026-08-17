# PRD #174: Relevance-ranked cross-run memory — the READ path stops being "newest 20" (FTS now, pgvector behind it)

**GitLab Issue**: [#174](https://github.com/vtmocanu/uzi/-/issues/174)
**Status**: Not started (Later). Created 2026-07-27.
**Priority**: Low — a quality improvement on a shipped capability (PRD #90), not a gap and not a bug. Labelled `Later` deliberately: nothing is broken, the store just gets less useful as it fills.
**Depends on**: PRD #90 (the store, the write path, the inert nonce-fenced read path, the visibility/purge surfaces). This PRD changes **which** entries get injected and **how they are ranked**; it changes nothing about the trust model.

## Problem

PRD #90 shipped the memory loop. Its retrieval is `ORDER BY created_at DESC, id DESC` with no relevance term and no injection budget (`api/internal/store/queries/agent_memory.sql`, `ListAgentMemoryForUserRepo`), and the whole set is composed into the lead's plan prompt (`buildMemoryContext`, `agent/src/prompt.ts`). Two consequences, both of which get worse the more the feature is used:

1. **Irrelevant memory is always injected.** Every run on a repo gets every entry for that `(user, repo)`, up to `MemoryMaxPerUserRepo = 20` (`api/internal/workersvc/service.go:126`). A run on a CSS issue is handed a Go build-flag note. At 20 × 2048B that is up to ~40KB of untrusted advisory text in the plan prompt of every single run, regardless of what the run is about.
2. **Eviction discards value, not staleness.** `EvictAgentMemoryOverCap` keeps the newest 20 and deletes the rest. The 21st entry evicts the oldest, which on a long-lived repo is very often the *most* load-bearing learning (the setup quirk discovered on day one) rather than the least.

The cap and the newest-first order were the right v1 — they are simple, they bound the blast radius, and they were shippable without new infrastructure. They are also the reason the store gets noisier as it earns its keep.

## Solution

Rank memory against the **current run's issue** before injection, and bound what gets injected by count *and* bytes. Ranking is server-side at claim time (the API already holds both halves: the `(user, repo)` memory and the issue title/body in the `issues` cache), so the claim payload the worker receives (`agent/src/protocol.ts:518`) simply becomes an ordered, budgeted list instead of an unordered dump. **No new data flow, no worker-side change to the fence.**

Two retrieval tiers, deliberately split so the cheap one ships alone:

- **FTS (M1)** — `tsvector` over `title || body`, ranked with `ts_rank_cd` against the issue text. Runs on `postgres:17` today, needs no image change, no embedding provider, no egress, no per-user credential. This is the version that delivers most of the value for none of the cost.
- **pgvector (M4/M5)** — semantic recall for the cases lexical matching misses ("the linker flag" vs "build fails at link time"). Requires an image change and an embedding provider (see Open Questions — this is the decision that sizes the PRD). Hybrid-ranked with FTS via RRF, and **degrading to M1 when embeddings are unavailable is a requirement, not a fallback**: a memory read must never fail a run.

Plus the store-hygiene work that ranking makes possible: tags/TTL (closes PRD #90's OQ-D), relevance-aware eviction, and near-duplicate collapse on write.

## Security framing — what does NOT change, and the one thing that does

PRD #90's invariants are load-bearing and every one of them survives verbatim: identity stays server-derived from the run claim, every query still filters `(user_id, repo_id)`, the nonce fence and the untrusted-advisory framing stay exactly as `memoryFrame` writes them, the file-tool path guard is not touched, and per-entry purge stays. **A PR that touches any of those is out of scope for this PRD.**

Two honest notes, in the tone PRD #90 set:

- **Ranking does not reduce injection risk, and may sharpen targeting.** A poisoned entry can be keyword-stuffed to rank highly against any issue. Today it is injected anyway (everything is), so the *absolute* risk is unchanged — but "we now rank" must not be written up as a security improvement, because it is not one. The backstop stack is unchanged: deny-layer guardrails, per-`(user,repo)` scope, server caps, nonce fence, user purge.
- **Embedding is a NEW egress of untrusted content (M4 only).** Sending `title || body` to a third-party embedding API means attacker-influenceable text leaves the deployment, under whichever credential we choose. M1 (FTS) has no such surface, which is a second reason to ship it first and independently.

## Milestones

- [ ] **M1 — FTS-ranked, budgeted read path.** `tsvector` column + GIN index on `agent_memory`, backfill migration, a ranked query variant, and server-side selection at claim time against the run's issue title/body. Injection bounded by **top-K and a byte budget** (a single 2048B entry must not crowd out four relevant ones). Empty/absent issue text ⇒ fall back to today's newest-first, so nothing regresses.
- [ ] **M2 — Tags + TTL.** Closes PRD #90 OQ-D. `save_memory` gains an optional bounded `tags` field; entries gain an optional expiry with a sweep. Tags feed ranking (M1) and the user-facing list (M6).
- [ ] **M3 — Relevance-aware eviction + write-time dedup.** Replace pure oldest-eviction with a score that weighs recency, hit-rate (how often an entry ranked into an injection), and TTL. Collapse near-duplicates on write so five runs re-learning one fact do not consume five slots.
- [ ] **M4 — pgvector: infra + embedding provider.** Extension enabled, compose image moved off `postgres:17` (pinned by digest at `docker-compose.yml:12`) and the CNPG per-cluster image updated; embedding on write behind a config flag; **degrade to M1 ranking whenever embeddings are unavailable, never fail the read.** Blocked on OQ-A.
- [ ] **M5 — Hybrid rank (RRF over FTS + vector).** Fuse the two rankings. Ship with a way to compare the three modes (FTS-only / vector-only / hybrid) on real memory, so "hybrid is better" is measured rather than assumed.
- [ ] **M6 — Visibility.** Web UI + `uzi memory` show tags, expiry, and last-injected/hit-count; the CLI gains a way to preview what *would* be injected for a given issue. Per-entry purge unchanged. (Repo rule: new API ⇒ check `api/cmd/uzi/`.)
- [ ] **M7 — Tests + specs.** Live-DB tests for ranking, budget, eviction, dedup, and TTL sweep; scope-isolation tests re-run unchanged (no cross-user/cross-repo bleed through the new query paths); a regression test that the nonce fence and untrusted framing are byte-identical. `specs/ai.md` records the retrieval model and the FTS-vs-vector split.

### Phases / parallelism

| Phase | Milestones | Depends on | Files touched | Parallel? |
|---|---|---|---|---|
| 1 | **M1** | — | `store/migrations`, `store/queries`, `workersvc`, `agent/src/prompt.ts` | no (foundation) |
| 2 | **M2**, **M4** | M1 | M2: migrations/queries/`memory-tools.ts`; M4: `docker-compose.yml`, `deploy/`, new embed client | **yes** — disjoint files |
| 3 | **M3**, **M5** | M2 (M3), M4 (M5) | M3: `workersvc` eviction; M5: ranking query | **yes** — disjoint |
| 4 | **M6** | M2 | `web/`, `api/cmd/uzi/` | no |
| 5 | **M7** | all | tests, `specs/ai.md` | no |

**M1 is independently shippable and is the recommended stopping point if the OQ-A answer is unattractive.** M2/M3/M6 need no embedding provider either — the entire pgvector track (M4/M5) can be dropped without stranding the rest.

## Decision Log

- **2026-07-27 — Server-side ranking, not worker-side.** The API already holds the memory and the issue cache; ranking there keeps the worker's read path a dumb, ordered list and avoids shipping unranked memory across the claim boundary just to sort it. It also means the ranking never runs in an environment the user's repo can influence.
- **2026-07-27 — FTS first, pgvector second, and the split is the point.** FTS needs no image change, no provider, no credential, and no new egress of untrusted text; it captures most of the value. Sequencing it first means the expensive decision (OQ-A) blocks only the tail of the PRD, not the head.
- **2026-07-27 — Budget by bytes AND count, not count alone.** Top-K alone still lets one maximum-size entry dominate the injected block. The prompt-size problem is measured in bytes, so the budget must be too.
- **2026-07-27 — Ranking is explicitly NOT a security improvement.** Recorded so no later summary claims it as one. A poisoned entry can rank itself up; the risk profile is unchanged from PRD #90.
- **2026-07-27 — Rejected: adopting an off-the-shelf memory layer (agentmemory / mem0 / Zep / Cognee / Letta).** Surveyed 2026-07-27. All are single-tenant-local in shape: local SQLite or a sidecar daemon, hook-based auto-capture, and retrieval straight into context. uzi is multi-tenant with per-`(user, repo)` isolation as an invariant, its workers are ephemeral containers where nothing outside the worktree survives teardown, and auto-capture-into-context is precisely the surface PRD #90 fenced off. Adopting one would mean unwinding #90's security framing and adding a bundled native binary to an egress-restricted worker. The genuinely portable ideas — relevance ranking, tiering/decay, dedup — are in this PRD, implemented in the Postgres we already run.

## Open questions

- **OQ-A — Embedding provider (blocks M4/M5, blocks nothing else).** **Anthropic ships no embedding model** — verified 2026-07-27 at `platform.claude.com/docs/en/docs/build-with-claude/embeddings`, which recommends Voyage AI. Users connect an *Anthropic* token, so there is no existing credential to reuse. Three shapes, none free:
  1. **Server-side key we own** (Voyage or similar) — simplest UX, but we pay per embed and every user's untrusted memory text flows through our credential.
  2. **Per-user second credential** — matches the existing bring-your-own-token model and the vault, but is real onboarding friction for a Later-priority feature.
  3. **Local model in the api container** (e.g. an open-weight small embedder; `voyage-4-nano` is Apache-2.0 on HF) — no egress and no credential, at the cost of image size, CPU/RAM in the api pod, and a model to maintain.

  Decide before M4 starts. **If none of the three is attractive, M4/M5 are droppable and the PRD still delivers** — that is why the phases are cut this way.
- **OQ-B — Does the 20-entry store cap rise once injection is budgeted?** The cap partly exists *because* everything gets injected. With M1's budget the store cap and the injected-set size decouple, so the store could hold far more while the prompt gets less. Worth deciding alongside M3 — but note it raises the volume of retained attacker-influenceable text, so it is a security-relevant knob, not just a number.
- **OQ-C — FTS configuration.** `english` (stemming, stopwords) vs `simple` (exact tokens). Memory bodies are technical and dense with identifiers, flags, and paths, where stemming can hurt; needs a look at real entries before choosing.
- **OQ-D — What is the ranking query text?** Issue title + body is the obvious default. Title-only, or title + labels, may rank better on long issues where the body dilutes the signal. Measure in M1.
- **OQ-E — Does hit-count tracking (M3/M6) need its own privacy consideration?** Recording which memory was injected into which run is new metadata about user behaviour. Probably fine (it is the user's own data, in their own tenancy), but state it rather than assume it.
