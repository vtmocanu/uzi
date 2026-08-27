# ADR-333: Incidental Findings — the off-task-bug capture seam

**Status**: Accepted (PRD #333 implemented)
**Date**: 2026-08-16
**Deciders**: Vlad + agent team (architect, coders, three PRD-review waves)
**PRD**: [prds/done/333-incidental-findings.md](../prds/done/333-incidental-findings.md) (GitLab issue [vtmocanu/uzi#333](https://github.com/vtmocanu/uzi/issues/333)) — the PRD carries the eight milestones, the full evidence base, and the Decision Log (D1–D12); this ADR carries only the durable seams a future change must respect, and the alternatives that were rejected, so a reader rebuilding from specs need not reread it.
**Numbering**: `0333` is the **PRD / issue** number, like `0065` and `0238`; it is not an ADR sequence number. A reader who assumes "ADR number == ADR count" will miscount.

## Decision (summary)

A worker mid-run can flag a bug it noticed **outside its task** without stopping or ending its turn. The finding is recorded, the user is told asynchronously (inbox + Slack), and it becomes a forge issue only on an explicit human file action, on the user's own connection — the same guardrail every forge write in uzi already honours: **the human gates every filing; the worker never writes to the forge.**

Four seams are durable and are the subject of this ADR:

1. **Capture is a plain, non-turn-ending MCP tool (`report_incidental_issue`), worker→api, never the forge** — mounted on the autonomous run lanes, deliberately not a signal and not the chat lane's `propose_issue`.
2. **A coordinate-keyed two-table store** — `findings` (per-run evidence) + `finding_dispositions` (cross-run `(user_id, repo_id, location)` lifecycle) — that reuses the judge backlog's *shape* but pushes dedup **into storage** instead of a read-time rollup.
3. **The content-hash re-open rule** — the anti-nag invariant: a report on an already-resolved coordinate re-surfaces iff its normalised content hash *differs*.
4. **Human-gated, claim-first filing** — a guarded `open→filing` UPDATE makes concurrent double-file produce exactly one issue; a stranded `filing` is reaped by a sweeper.

Everything else in the feature (the web surfaces, the CLI, the notification renderer, the label assembly) is conventional reuse of existing patterns and lives only in the PRD.

## Context

Headless uzi runs have no one watching the stream, so Claude Code's local move — *asking* "want me to file these as issues?" — is the wrong shape. The worker routinely reads code adjacent to its task and notices a real, unrelated bug; its only prior options were to smuggle an out-of-scope fix into the task MR (widening the diff and risking `main`-adjacent scope creep), bury it in prose nobody reads, or drop it. The feature is the headless equivalent of the local prompt: flag without blocking, tell the user out-of-band, let them file or dismiss on their own schedule.

Three existing structures set the constraints, and getting the seam right was mostly about **not** reaching for the nearest one:

- The **chat lane** already has `propose_issue` (`agent/src/uzi-tools.ts`), a user-directed issue drafter. It is the wrong home: findings are autonomous and off-task, not user-requested.
- The **run lane** already has signals (`agent/src/signals.ts`), captured out-of-band by `scanSignals`. A finding must *return* while the turn continues, so it is a working tool, not a signal.
- The **judge backlog** family (`review_recommendations` + `recommendation_dispositions` + `recommendation_filed_issues`) already solves "dedupe evidence seen in N runs onto a stable coordinate, triage once, survive re-discovery." Findings want that shape — but the judge dedups at *read* time, and findings need dedup in *storage*.

## The decisions

### D1 — Capture is a plain run-lane MCP tool, worker→api, never the forge

`report_incidental_issue` is a plain MCP working tool: it POSTs to the api, emits a `finding` stream card, returns a terse ack, and the turn continues. It is registered on **all** autonomous lanes (`issue`, `ci_fix`, `prompt`, `self_improve`) and consciously **not** behind the `isIssueRun` gate that wraps `report_progress`.

Three shape decisions, each with the alternative it rejects:

- **Not a signal.** Signals (`signals.ts`, drained by `scanSignals`) are captured *between* turns and can end a turn; a finding must not interrupt the task. The tool is therefore deliberately **absent from `isSignalToolName`/`scanSignals`** — structurally it can never be promoted to turn-ending.
- **Not the chat `propose_issue` path.** Overloading `propose_issue` was rejected: it is chat-scoped at the service (`workersvc/chat.go` rejects `run.Kind != RunKindChat`), user-directed, and would drag findings onto the wrong lane and the wrong table. Findings get their own tool, endpoint, and table so the chat path is untouched.
- **Implemented as its own in-process MCP server, not a tool bolted onto the run lane's `uzi` signal server.** The tool surfaces as `mcp__findings__report_incidental_issue` — a **distinct server key** (`agent/src/findings-tools.ts`, `FINDINGS_SERVER_NAME = "findings"`) from the `uzi` signal server, the `memory` server, and the `forge` read server. This is a refinement of the PRD's "plain tool on the run lane" wording: the lane is right, but a separate server keeps the finding tool off the signal-scanning path by construction rather than by a name-exclusion list that a future signal author could forget. The structure is `forge-tools.ts`'s token-less, run-scoped `{client, runId, log}` precedent **plus** an `emit` (new plumbing — `forge-tools.ts` emits nothing and `propose_issue`'s emit lives in the chat executor, so no single precedent was copied). The run id is a **closure**, never a tool parameter, so a subagent cannot report against another run.

**The worker holds no forge token.** Capture goes worker→api under `RequireWorker` exactly like proposals; the api derives `(user_id, repo_id)` from the claimed run (`GetRunByIDForUser`), never from a client-sent id. Auto-filing was rejected for v1 (deferred, D9 in the PRD): v1 is propose-then-file, matching every existing forge write. The schema leaves room — an auto-file mode is a create-time branch that inserts the disposition straight to `filing`, not a schema change.

### D2 — A coordinate-keyed two-table store that pushes dedup into storage

Two tables (`00129_incidental_findings.sql`), mirroring the judge backlog family:

- **`findings`** — per-run evidence, one row per report; two runs finding the same bug write two rows (the `review_recommendations` analogue). `run_id → runs ON DELETE CASCADE`.
- **`finding_dispositions`** — the cross-run coordinate lifecycle, `UNIQUE (user_id, repo_id, location)`, collapsing the judge's *two* side-tables into **one linear status machine**: `open → filing → filed`, or `open → dismissed` (`filing → open` on a retryable forge failure; `filed`/`dismissed → open` only on a content-hash mismatch, D3). It carries `filed_issue_iid`, `filed_issue_url`, the `filing_since` claim marker, `dismiss_reason`, `content_hash`, and a `last_title` snapshot. A CHECK ties `status='dismissed'` iff `dismiss_reason IS NOT NULL`.

Three rejected alternatives, each losing something load-bearing:

- **A single coordinate-keyed table** (upsert latest title/desc, inline status) loses the "seen in N runs" occurrence count the backlog UX and the anti-spam story depend on. The evidence table costs one migration; the count is worth it.
- **The judge's read-time rollup.** The judge dedups `(category, target)` at read time (`Handler.JudgeRecommendations`) and its disposition CASCADEs with the review. Findings instead dedup **in the storage coordinate** so a `filed`/`dismissed` coordinate survives even after its evidence rows are cascaded away with a deleted run. This is *why* `finding_dispositions` deliberately has **no run FK** and carries `last_title` — the backlog read is `FROM finding_dispositions LEFT JOIN findings`, disposition-driven, and must stay legible with zero evidence rows. Accepted residual: a fully-deleted run orphans its coordinate unboundedly; there is no run-deletion path today, and a prune is a later addition, not v1.
- **Columns on `issue_proposals`.** Rejected: `issue_proposals` is per-run (found by `run_id`, no coordinate, no backlog), also serves chat, and bolting a coordinate+disposition structure onto it couples two features. The judge's shape already fits; reuse the shape, not the table.

The store model/DTO is named **`IncidentalFinding`** (a `privcheck.Finding` type already exists and is unrelated); the `findings` table name is unambiguous.

### D3 — The content-hash re-open rule (the anti-nag invariant)

The `(user_id, repo_id, location)` coordinate has **no content discriminator** — `location` is canonicalised server-side to a fixed form (repo-root-relative, forward slashes, leading `./` dropped, lowercased, whitespace-stripped, capped, at most one symbol token; **line numbers excluded** because they drift and would defeat dedup). Two *different* bugs at the same `file.go#symbol` would therefore collide, and suppression would permanently hide the second.

So `finding_dispositions.content_hash` — the sha256 of the normalised title+description (the judge's `rationale_hash` idea reinstated) — decides re-surfacing:

- A report on a `filed`/`dismissed` coordinate whose hash **matches** records the evidence row but does **not** notify and does **not** re-enter the to-file bucket. **A dismissed bug stays gone across runs.**
- A report whose hash **differs materially** re-opens the coordinate (back to `open`, re-notify).

**This is the invariant a future change to the capture or dedup path must preserve.** If you change how `location` is canonicalised, how the coordinate is keyed, or how the hash is computed, you are changing whether a dismissed finding can nag again — and the anti-nag guarantee is the whole reason the feature is safe to leave on. The guarantee is pinned by a dedicated end-to-end scenario (report → dismiss → re-report same coordinate from a later run → no notification, absent from to-file; then a materially-different report re-opens and notifies).

The `open` insert is atomic via `UNIQUE(coordinate)` + `ON CONFLICT DO NOTHING`; the re-open is a *separate guarded UPDATE* on a hash mismatch, not part of the upsert. The suppression check is a benign read-then-notify: a report racing a concurrent file/dismiss of the same coordinate may emit one harmless extra ping — acceptable, not a correctness break.

### D4 — Human-gated, claim-first filing is the forge-write-safety seam

Filing (`POST /api/findings/{id}/issue`) is the only path that touches the forge, and it copies the judge's claim-first two-phase write (`ConfirmProposalForUser`):

- **Claim first.** `ClaimFindingForFiling` is a guarded `UPDATE … SET status='filing' WHERE coordinate AND status='open'` with a rows-affected check — *not* an insert-on-conflict. Of two concurrent files on the same coordinate, exactly one moves `open→filing`; the loser gets zero rows and a 409. So a double-file yields **exactly one `CreateIssue` and one `filed` disposition**, never two issues.
- **Server-assembled labels.** Filed labels = a server-mandated marker (`agent-found`, config-overridable) ∪ the user's sanitised, capped selection — never a client-trusted trigger label (the `review_issue_file.go` rule). The marker is `EnsureLabels`-ed first (Forgejo resolves label *names* to ids and needs it to pre-exist).
- **Text resolved from the stored, already-sanitised row**, never from the request body (D4 in the PRD). Agent-authored `title`/`description`/`location` are routed through the field-level sanitisers (`SanitizeTitle`, `FenceBlock`+`SanitizeFiledBody`, `SafeInlineCode`) at capture time — **not** `issuedraft.Render`, which is judge-hardcoded. If the user edits the draft, only their edited title/description are accepted and are re-run through the same passes.
- **Stranded-`filing` is reaped.** A claim that dies between `open→filing` and settle (crash, forge timeout) would strand the coordinate. `SweepStrandedFilingFindings` reverts a stale `filing` back to `open` so it can be re-filed — the claim marker is a lease, not a lock.

## Consequences

- **The capture seam is a fourth in-process MCP server** (`findings`), alongside `uzi` (signals), `memory`, and `forge`. A future run-lane tool that must return-and-continue (not signal) now has a precedent for living in its own server rather than being name-excluded from signal scanning.
- **`finding_dispositions` is the source of truth; the run-stream card is best-effort.** An old card may show *File* for a coordinate already filed or dismissed from the backlog; clicking gets the M5 409 / stale state, handled gracefully. The backlog — not the card — is authoritative. Any future surface must treat the disposition status as canonical.
- **The four guardrail layers are untouched.** The worker holds no forge token, capture is `RequireWorker` outbound-only, filing is human-gated on the user's own connection. Nothing here weakens the primary directive; findings never reach the forge except through a human file action.
- **Dedup is deterministic, never semantic.** The coordinate is an exact match and the hash is an exact match of normalised text — no fuzzy/AI matching. A future "smarter dedup" is a deliberate scope decision, not a bug fix, and must not silently erode the anti-nag guarantee (D3).
- **Auto-file (PRD D9) is the reserved extension**, drivable by the existing status set and label path with a create-time branch; it is out of v1 and must stay propose-then-file until a decision reopens it.
- **ARCHITECTURE.md** gains a Findings entry under the surfaces map, linking here — discharged by the milestone that lands this ADR, not by this file.
