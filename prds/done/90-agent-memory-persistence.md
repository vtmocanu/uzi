# PRD #90: Cross-run agent memory — a sanctioned, inert, per-user+per-repo store (the READ path is the new surface)

**GitLab Issue**: [#90](https://gitlab.example.com/vtmocanu/uzi/-/issues/90)
**Status**: Complete (2026-07-20). Shipped via MR !78 (uzi-authored, human-reviewed + merged); all six milestones landed — DB store + server-derived identity, `save_memory` custom tool with the file-guard regression test, inert nonce-fenced read path, server-enforced caps, deterministic mechanism + live-DB tests, and the web + `uzi memory` visibility/purge surfaces. Design/decision record in `specs/ai.md` §317–322. Issue #90 auto-closed on merge.
**Priority**: Medium (a capability gap + a small trust-boundary change on the READ path; not a bug or an active security hole).
**Depends on**: PRD #51 (worker/runner uid split; the file-tool path guard this PRD does NOT change). PRD #42/#58 (the per-run SDK `$HOME = agent-home/<runId>` model + the accepted out-of-worktree-Bash residual). Surfaced by the judge on run `e2d7427b` (PRD #86).
**Related**: An existing residual this PRD does NOT introduce but should be tracked separately — runner-group-writable `/data/provision` + `/nix` are a cross-run *executable* persistence channel today (future runs' provisioning consumes that state). That is strictly worse than anything here; flag as its own issue.

## Problem

Runs re-discover the same operational facts every time; the agent has no **sanctioned, structured, user-visible** way to carry a learning to a future run. Note precisely what is and is NOT true (re-derived from code; the first draft got this wrong):

1. **The file-tool guard blocks agent memory writes via Write/Edit.** `agent/src/guardrails.ts` gives the file tools (Read/Edit/Write/MultiEdit/NotebookEdit/Glob/Grep, `PATH_TOOLS` :152) a path guard that denies anything resolving outside the run worktree (`REASON_OUTSIDE_WORKTREE` :109), authoritative under `bypassPermissions`. So the lead on run `e2d7427b` was denied because it used the **Write tool**.
2. **But a durable cross-run write path ALREADY exists via Bash.** The Bash screener is a deny-LIST (push / config / env / `/proc` / secrets / docker-redirect) with **no path containment** — `echo x > /data/agent-home/foo` passes it. The runner uid can write there, and only `agent-home/<runId>` is torn down on terminal (runner.ts) — a sibling file survives indefinitely. `git.ts:14-18` records this explicitly: "out-of-worktree writes are the accepted PRD #42 residual." So persistence is not the missing capability.
3. **What's actually missing** is (a) a *sanctioned, structured, bounded, user-visible* place for a learning, and (b) the deliberate **read-back** of it into a future run — which no mechanism does today. Stray Bash-written files are never re-read into context; PRD #90 creates that loop on purpose. **The read path is therefore the only genuinely new trust surface, and the design centers on it.**

Concrete loss on `e2d7427b`: the lead learned a build-flag fact and had no sanctioned way to save it; the next run re-learns it. (That exact fact is now moot — 0.8.3 bakes gcc — but the class recurs.)

## Solution

A durable **per-(user, repo)** memory store, written through an **in-process SDK custom tool** and read back into a future run as **inert, nonce-fenced, untrusted** context. No change to the file-tool path guard.

- **Store**: a DB table `agent_memory` keyed `(user_id, repo_id)`, FK → `users(id)` / `repos(id)` `ON DELETE CASCADE`, with provenance (`run_id`, `created_at`) and server-enforced caps. DB (not a file/volume) means the READ path never touches a file tool and the store is in the CNPG backup set.
- **Write path** — a `save_memory(title, body)` **SDK custom tool** (MCP/custom-tool seam the chat executor already uses), not a file write. The worker validates shape + caps and POSTs to the API. **The API derives `(user_id, repo_id)` from the run claim (`run_id`) server-side and NEVER accepts them as caller parameters** — the worker's join token is not user-scoped, so a compromised worker must not be able to write arbitrary users' memory. No guard carve-out is needed, and the file guard stays a hard "deny everything outside the worktree," unchanged.
- **Read path** (the new surface) — the worker composes the run's `(user, repo)` memory into the **lead's prompt at claim time**, as prompt text (never via a file tool). It is wrapped in a per-run **nonce fence** (`<untrusted_memory_${nonce}>…</untrusted_memory>`, the pattern `judge-runner.ts:324` uses) and prefaced as untrusted, advisory data the agent may weigh but must never follow as instructions.
- **User control (v1, not optional)** — memory is **visible and per-entry deletable** in both the web UI and the CLI (`api/cmd/uzi` — the repo's "new API ⇒ check the CLI" rule). The distinctive risk of memory is a poisoned entry outliving the repo injection that planted it (attacker cleans the repo; the payload lives on), so the owner's ability to see and purge is a security control, not a nicety.

## Security framing (the crux — the READ path)

Writing bytes across runs is already possible (Problem #2); what's new is **reading persisted, attacker-influenceable content back into a future run's context**. Bounds:

- **Inert/advisory is prompt-level for a tool-bearing lead — stated honestly.** The judge's "data-not-commands" containment is *structural* (a tool-less reader, `judge-runner.ts:5`); the lead has full tools, so for memory the label + nonce fence are prompt-level only. A poisoned memory CAN still injection-*shape* a tool-bearing lead. The real backstop stack is: (a) the deny-layer guardrails that bound what any shaped lead can do (no push, no secret read, no outside-worktree file write, docker-redirect denied…); (b) the per-(user, repo) scope; (c) server-enforced caps + structure; (d) the nonce fence; (e) user-visible purge. With those, the residual ≈ the injection risk the lead already carries just by reading the repo it runs on.
- **Per-user + per-repo scope confines poisoning.** Only your runs on a repo write that `(you, repo)` memory, and it is read only by your future runs on that same repo — no cross-user, no cross-repo bleed. "Your own repo is trusted-enough" holds *because memory adds persistence, not authority*: a future run on that repo already ingests its (possibly hostile) content.
- **The file guard is not touched.** No carve-out (which would also carry a TOCTOU on the runner-group-writable `agent-home` parent). A regression test asserts the guard still denies every outside-worktree file-tool write.

Out of scope / rejected: making memory *trusted* (cross-run injection persistence); cross-user/cross-repo sharing (blast-radius expansion); a file-based store (needs a guard change + file-parsing of untrusted content).

## Milestones

- [ ] **M1 — DB store + API + server-side identity.** `agent_memory` table (keyed `(user_id, repo_id)`, FK cascade, `run_id`/`created_at` provenance, caps as columns/constraints), migration (draft number per repo convention), and read/write API routes that **derive `(user_id, repo_id)` from the run claim, never from the body**.
- [ ] **M2 — `save_memory` SDK custom tool (no guard change).** Worker-side custom tool that validates `{title, body}` shape + caps and POSTs to the API. Plus a **regression test that the file-tool path guard is UNCHANGED** — still denies all outside-worktree writes.
- [ ] **M3 — Read path: inert, nonce-fenced context.** Worker composes `(user, repo)` memory into the lead's prompt at claim time, nonce-fenced + prefaced untrusted-advisory. Prove a future run *sees* a prior learning as data.
- [ ] **M4 — Structure + caps enforced server-side.** Per-entry size, per-`(user,repo)` count (oldest-eviction), and a per-run write cap (spam bound within one run). Enforced at the API + tool schema, not client-trusted.
- [ ] **M5 — Deterministic mechanism tests (+ optional manual eval).** CI-able: caps enforced, fencing/escaping applied, scope isolation (no cross-user/cross-repo bleed), provenance recorded, guard unchanged (M2). NOT a live LLM-behavioral "does the model obey" assertion (non-deterministic + a no-op under the stub); leave that to an optional manual/periodic eval.
- [ ] **M6 — User-visible + deletable + specs.** Web UI list + per-entry delete AND a `api/cmd/uzi` command (v1, required per S1). `specs/ai.md`: the memory model, the inert/advisory + read-path-is-the-surface framing, the per-(user,repo) scope, and that the PRD #51 file guard is unchanged.

Dependencies: M1 → M2/M3/M4 (need the store + API). M2/M3 are the two halves of the loop. M5 after M1-M4. M6 last (UI/CLI + specs). The file-guard regression test in M2 is independent.

## Decision Log

- **2026-07-19 — Premise corrected (fable review).** "No cross-run persistence path" was false: Bash already writes durably outside the worktree (accepted PRD #42 residual, `git.ts:14-18`). The new surface is the READ path, and the PRD is centered there. Verified in code by the lead before accepting.
- **2026-07-19 — Trust model = INERT / ADVISORY, stated as prompt-level for a tool-bearing lead (owner delegated).** Trusted memory = cross-run injection persistence; rejected. Inert keeps the value while removing auto-execution. Honest caveat: labelling is prompt-level here (the reader has tools), so the real backstops are the deny-layer + scope + caps + nonce fence + user purge.
- **2026-07-19 — Scope = per-user + per-repo (owner delegated).** Smallest blast radius; matches the run model. FK cascade + stable forge project-ids resolve the repo-rename/transfer/delete edges.
- **2026-07-19 — OQ-A resolved: DB store + `save_memory` custom tool, NOT a file + guard carve-out (fable B2).** Deletes the guard change entirely ("don't touch the containment control" beats "amend it"), enforces structure/caps/provenance server-side, and avoids the TOCTOU a runner-group-writable file memory path would carry.
- **2026-07-19 — Server-side identity derivation is mandatory (fable B3).** The worker's join token is not user-scoped; the API derives `(user_id, repo_id)` from the run claim, never the request body.
- **2026-07-19 — User-visible + deletable is v1, not conditional (fable S1).** A poisoned entry can outlive the repo injection that planted it; seeing + purging it is the owner's only recourse.

## Open questions

- **OQ-B — Write policy: auto vs curated.** With inert memory, auto-write (lead decides) is acceptable (a saved entry can't auto-execute later). Human-curation is a heavier follow-up, not v1. Leaning auto-write, **lead-only** (one writer, clean provenance).
- **OQ-C — Concrete caps.** Pick numbers before M1: entries per `(user,repo)`, bytes per entry, writes per run. (Suggest small, e.g. ~20 × ~2KB, ~5/run.)
- **OQ-D — Entry shape.** `{title, body}` bounded; anything richer (tags, ttl) deferred.

## Reviews

- **Fable-model independent review (2026-07-19): approve-with-changes.** Re-derived all code claims; **refuted the first draft's premise** (Bash already persists out-of-worktree) and showed the READ path is the only new surface (B1); recommended DB + custom tool to delete the guard carve-out entirely (B2); flagged mandatory server-side identity derivation (B3); required user-visible + deletable memory in v1 (S1); demanded honesty that "inert" is prompt-level for a tool-bearing lead, with nonce-fencing (S2); reframed the adversarial test as deterministic mechanism tests (S3); and specified schema/FK/caps (S4). Also surfaced an unrelated existing residual (runner-writable `/data/provision`+`/nix` executable persistence) worth its own issue. All folded into this revision; the lead independently verified B1 in code before accepting.
