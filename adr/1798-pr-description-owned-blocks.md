# ADR-1798: Two owned PR-description blocks, a fenced artifact, and a closing-directive scan that never trusts model or lead text

**Status**: Accepted (PRD #1798, M1-M7 implemented; M8 docs/spec/ADR)
**Date**: 2026-09-28
**PRD**: [prds/1798-plain-english-pr-descriptions.md](../prds/1798-plain-english-pr-descriptions.md) — carries the full milestone breakdown and Decision Log D1-D17; this ADR restates the invariants a future edit to the renderer, the publisher or the completion interlock would otherwise break silently.
**Related**: [ADR-1225](1225-structural-completion-interlock-invariants.md) (the completion interlock this PRD's renderer and publisher sit beside; ADR-1225's I4 is amended by this PRD, see its addendum).

## Decision (summary)

uzi opens PRs with a plain-English description a human can read in under a minute: what changed, a computed size line, what was verified, and how the delivery differs from the ask. That text can be model-generated or lead-written, both untrusted, so five mechanisms keep it from ever doing something only uzi's own deterministic code should do:

1. **Exactly two owned, marked blocks** in the PR body — a description region and a completion block — with a fixed precedence when they conflict with a human edit or with preserved text.
2. **A persisted, versioned artifact**, staged before the PR exists, bound to it once it does, and acknowledged only once the forge write is confirmed, fenced by the run's claim generation throughout.
3. **A nominal (branded) class** for the only publishable text: the api's sanitized fields, checked at runtime with `is()`, never a plain object or a shallow copy of one.
4. **A structural rule that generated or lead-written text can never close an issue** — only uzi's own renderer ever writes `Closes #N`. For an interlocked run that add happens only after the PR head is independently verified; a legacy (non-interlocked: seeded, kill-switched, or pre-rollout) run renders `Closes #N` at creation with no head read, as before.
5. **A whole-body closing-directive scan** with an explicitly bounded threat model, run whenever a completion must not close its issue.

## Context

A PR body historically carried little more than "Closes #N" and a fixed sentence; PRD #1798 replaces that with a short, generated-or-lead-written description. The moment any of that text is untrusted (attacker-shaped issue/PRD/diff content, or a lead's own self-report), it becomes a candidate for exactly the kind of forgery the completion interlock (ADR-1225) was built to prevent: a body that closes an issue it should not, on a PR a human never really reviewed as complete. This ADR records the seams that keep the new description machinery from becoming a second way into that hole.

## The invariants

### I1 — exactly two owned blocks, and a fixed precedence (D10)

uzi owns exactly two marked regions of a PR body: the **description region** (`<!-- uzi:description:start v1 -->` … `<!-- uzi:description:end -->`) and the **completion block** (`<!-- uzi:completion:start v1 -->` … `<!-- uzi:completion:end -->`). Everything outside both is preserved byte-for-byte on an ordinary refresh — a review bot's appended summary, a maintainer's notes, a repo PR template's filled sections. A handful of named cases rewrite the whole body instead — the closing-directive case is rule 1 below, and a completion block still missing when the interlock's own reconcile reads the PR gets the same treatment — see [run-summaries.md](../docs/run-summaries.md#pull-request-description) for the full, plain-English list: no uzi markers yet, malformed markers on an interlocked run (the interlock's reconcile rewrites them; a legacy run's publisher only skips), a completion block still missing when the reconcile reads it, a closing directive found outside the completion block, or a blind rewrite of an unreadable PR. (A merely-missing completion block that the publisher can still repair in place, by appending a fresh one, is not one of these — see run-summaries.md.) When these three concerns conflict, precedence is fixed, highest first:

1. **The completion interlock.** The completion block is always rewritten. On every `own`-mode publication of an interlocked issue run whose completion block does not close — which is every one of them before its head is verified, not only a hold or a non-closing delivery (I5's exact scope) — it scans the *entire resulting body*, region and preserved text included, for a closing directive aimed at the run's issue. If one exists anywhere uzi does not write, uzi first tries to remove it by rewriting the whole body non-closing; it fails closed only when that rewrite itself cannot be confirmed to have removed the directive — the write is refused, or it is written but a read-back shows it did not land. This is the same fail-closed shape the Amends section below states for the `Closes`-add strip, applied here to the non-closing rewrite instead. On an interlocked run, a blind non-closing rewrite of a PR uzi could not even read back is still accepted: it carries no closing directive by construction, even though nothing confirmed it landed. Human-edit protection and preservation both yield to this rule.
2. **Human-edit protection**, for the description region only: before writing, uzi hashes the region as it currently reads on the forge and compares it against the hash of the last version it published. A mismatch means a human edited it; uzi leaves the region alone (`skipped_human_edit`), records why, and still rewrites the completion block.
3. **Preservation** of everything else.

A PR with no uzi markers at all — an adopted legacy PR — is rewritten whole, which is the interlock's pre-existing behaviour for that case.

### I2 — a persisted, versioned, claim-fenced artifact with exactly six ack outcomes (D9, D11)

The description is not composed at write time from scratch; it is staged, bound, and acknowledged as a durable artifact, every call fenced by the run's live claim generation so a stale worker after a reclaim gets a 409 rather than writing over newer state:

1. **Stage** — before the PR is created, the worker posts the raw (unsanitized) fields and the exact snapshot (`base_sha`, `head_sha`, `target_branch`) they describe; the api sanitizes and stores a `pending` version keyed by the run.
2. **Bind** — once the PR exists (created or adopted), the version is bound to `(repo_id, mr_iid)`.
3. **Publish ack** — after the forge write, the worker compare-and-swaps the per-PR row on `lock_version`: the outcome is recorded whether or not the write is what ends up published.
4. **Lost-ack recovery** — if the forge write succeeded but the ack was lost, the next writer hashes what it reads back and, on a match, acknowledges the pending version before proceeding.

`last_outcome` is exactly one of six values: `published`, `skipped_human_edit`, `skipped_no_region`, `skipped_malformed`, `skipped_snapshot_moved`, `write_failed`. Every publication attempt lands on one of these; nothing is left ambiguous between "we don't know" and "it published."

### I3 — only api-sanitized text is publishable, enforced by a nominal class plus a runtime check (D7)

Issue text, PRD text, the plan, the lead's own claims, and diff content are all attacker-shapeable. The worker never publishes what it generated locally: it POSTs the raw fields to the api, which validates and sanitizes them (stripping raw HTML and comments, images and link targets, breaking `@mentions`, neutralizing closing keywords for all three forges' syntaxes), and only the api's *returned* fields are renderable. The renderer (`agent/src/pr-description.ts`) enforces this at the type level and at runtime: it accepts fields only as a `SanitizedPrDescriptionFields` instance, checked with `SanitizedPrDescriptionFields.is()`, so a plain object, `Object.assign({}, s, raw)`, `structuredClone(s)`, or any other shallow copy that merely has the right shape is rejected and the region falls back to its deterministic, fields-free rendering. This is deliberately stronger than a TypeScript type alone, which a plain object satisfies structurally.

### I4 — generated or lead-written text can never close an issue (D5, D8)

`Closes #N` and every other deterministic sentence of the completion block (the partial and accepted-unmet-criteria warnings, the completion-unverified banner, the gates-unverified section, the history-bridge sentence, the agents line, the footer) is written by the renderer alone, from run data, never from model or lead output. The description region — generated, lead-written, or the deterministic-only fallback — never contains a closing directive: the api's sanitizer neutralizes one if the model or the lead wrote it, and the interlock's whole-body scan (I5) is the backstop that catches what neither generation nor sanitization prevented, including one a human added before uzi's last write (an adopted PR that already carried `Closes #N`, for example). A directive a human adds *after* uzi's last write is out of the scan's reach — see I5.

### I5 — the whole-body closing-directive scan: its scope and its threat model, stated once (D10 amended, and this PRD's own hardening)

`closingDirectiveFor` (`agent/src/pr-description.ts`) is the scan the interlock runs, through `closingDirectiveOutsideCompletion`, on **every `own`-mode publication of an interlocked issue run whose completion block does not close** — which is every one of them before its head is verified, not only an owner-partial delivery or a run scope-capped mid-flight (those are the non-closing cases for a legacy run, which has no completion hold to be a third one). It is a port of the api sanitizer's own closing pattern, extended to GitLab's reference lists, run over several views of the body (raw, a rendered view mirroring the api's normalization, a decoded-raw view, a tag-stripped view, and their composition) so a directive split by markup or an HTML entity is still caught. It detects, at write time: plainly written directives, the api sanitizer's normalized forms, and common markup and entity splits (a keyword split by an entity, a tag, a comment, a processing instruction, CDATA, a declaration, or an empty link).

**It is not a boundary against deliberate obfuscation by someone with edit rights on the PR description.** Anyone who can edit the PR body can add a plain `Closes #N` at any time after uzi's last write, and the scan cannot prevent that — it is a write-time check, not a standing guard on the forge. Anyone *without* edit rights reaches the body only through the api sanitizer (model and lead text) and the renderer's own escaping (deterministic interpolations), which is where I3 and I4 do the actual work. The documented, accepted gaps in the scan's own pattern matching (a link destination with balanced parentheses or a quoted closing paren, a custom tag with a quoted `>` in an attribute, and a marker pair a forge would read as fenced code but the block parser would still adopt) are pinned in the module header of `agent/src/pr-description.ts` and in its hardening tests, and are accepted rather than hidden.

### I6 — a refresh never authors closing semantics (D17)

An `mr_rework` run, or a `ci_fix` that adopts an existing agent branch rather than opening a fresh one, refreshes the description region against the whole PR diff (not only its own commits) but never touches the completion block's closing decision: it replaces only the staleness line inside the existing completion block. **A refresh never falls back to a whole-body rewrite**, unlike an `own`-mode publication under I1: a PR with no uzi markers is left untouched (`legacyUntouched`), malformed markers skip the write entirely, and a missing completion block simply stays missing — none of these compose a replacement body. A refresh run therefore cannot turn a non-closing PR into a closing one, or vice versa, by the act of refreshing it.

## Amends ADR-1225

ADR-1225's I4 states: "If the add itself fails the run **holds** rather than reporting completion." This PRD narrows that: if the `Closes #N` add cannot be *confirmed* — the write fails, the confirming re-read fails, or the PR's head moves between the write and the re-read — uzi strips `Closes` back out. The run holds when that strip is *confirmed*, or when the MR was unreadable and the strip fell back to writing the non-closing body *blind* (no re-read, but no closing directive by construction). It fails closed rather than holding only when the strip write is refused, or is written but a confirming re-read shows it did not land. See ADR-1225's own addendum for the amendment note pointing back here.

## Consequences

- A future third owned block (for example, a dedicated review-bot handshake region) must state its own precedence against I1's three rules rather than assume it inherits either human-edit protection or the interlock's override; neither is generic to "a marked block," they are specific to the two blocks named here.
- Widening `closingDirectiveFor`'s view set (a new markup-splitting technique) extends I5's detected set; it must not be read as closing the gap I5 explicitly declines to close (obfuscation by an editor with legitimate write access).
- A new forge's driver must implement `getMergeRequest` under the same byte cap (`MR_DETAIL_MAX_BYTES`, 6 MiB + 64 KiB) the existing three do, and must treat an over-cap read as unreadable rather than truncating it, since every fail-closed path here assumes "unreadable" is a possible, handled outcome.
