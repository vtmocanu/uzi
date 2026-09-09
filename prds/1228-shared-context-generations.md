# PRD #1228: Shared encrypted provider-context generations

**Issue:** [#1228](https://github.com/vtmocanu/uzi/issues/1228)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Consumers:** Completion holds in #1229 and retained PR sessions in #1214.
**Status:** Planned, blocked on the one-at-a-time sequence after #1227.
**Priority:** High.
**Execution:** Keep `Planned` without `uzi`. Add `uzi` only after #1227 merges and main CI is green.

This child creates a dark, neutral storage/codec primitive with no active lifecycle consumer. That is intentional: two independent features need the same security-sensitive transport, and implementing it once prevents divergent schemas and allowlists. No implementation or validation may modify `.github/workflows/**`.

## Problem and outcome

Today's Git checkpoints preserve code, not a provider conversation. #1229 needs a durable `run_hold` generation before releasing a completion-blocked worker; #1214 needs a durable `pr_session` generation between terminal implementation/rework attempts. Separate implementations would duplicate encryption, upload fencing, unsafe-path validation, provider codecs and GC while making cross-feature bugs likely.

Build one bounded, encrypted, claim-fenced context-generation service and provider-neutral agent codec/transfer interface. The generation declares `owner_kind`, but this child activates neither `run_hold` nor `pr_session`; consumers must prove their own authorization and lifetime later.

## Included

- Neutral immutable generation and chunk storage with `owner_kind in {run_hold, pr_session}`.
- Staged upload, manifest validation, atomic complete-generation commit and current-generation CAS.
- Claim-generation fencing, owner/repository/run authorization and idempotency.
- Compression before API-held `secretbox` encryption, decoded-byte bounds and integrity checks.
- Streaming upload/read routes and a provider-neutral agent transfer/codec interface.
- A Claude codec based on the pinned installed SDK artifacts and #1214's verified allowlist.
- Explicit unsupported-provider outcomes, including Codex `unavailable(codec_missing)` until merged runtime evidence proves a production allowlist.
- Expiry/quota/GC primitives and metadata-only DTOs.

## Excluded

- Capturing or restoring a completion hold, delivered by #1229.
- PR-session authorization, activation, owner retention settings, forget or MR rework, delivered by #1214.
- Keeping workers alive, copying an entire HOME or retaining credentials/settings.
- Codex production continuation without #1171's merged routing, packaged artifact characterization and rework-provider prerequisites.
- Web owner controls beyond bounded diagnostic metadata.

## Resolved facts

The offline implementer must use these facts rather than open-web research:

- #1214 locally records the installed Claude SDK behavior and allowlist: the producing session's `projects/<project>/<session-id>.jsonl` plus its same-ID sidecar subtree; sibling sessions, credential files, `settings*.json`, `todos/`, `shell-snapshots/`, `debug/` and `statsig/` are excluded.
- Claude session storage can report `mirror_error`; a successful model result or mirror callback is not proof of a complete generation. Finalization needs an explicit complete manifest acknowledgement.
- Resume restores conversation context; Git/workspace state remains a separate channel.
- Compaction means retained context is resumable context, not guaranteed unlimited verbatim history.
- Codex has a provisional local session-copy path in `agent/src/codex/session-state.ts`, but #1171 is not merged and provisional copying is not production continuation evidence.
- Context may contain source or secret material in model/tool output even when credential files are excluded. Encrypt the whole artifact and never expose content in ordinary DTOs/logs.

## Design decisions

### D1: Separate transport from lifecycle policy

Generations are immutable encrypted artifact sets. Each records owner kind/id, user, repository, producing run/claim fence, provider/codec versions, session/workspace identity, published or captured head, previous generation, manifest, byte counts, integrity hash and state `staged|complete|released`.

The shared service knows how to authenticate a current worker upload/read against a server-issued handoff. It does not decide whether a run may park or a PR session may retain data. #1229/#1214 own those transitions.

### D2: Publish only complete, validated generations

Upload bounded chunks under an idempotency identity and expected part count. Validate every manifest path and decoded size before committing. Partial uploads are never current/resumable. Finalize performs compare-and-swap against the expected previous generation and returns the same committed generation on an admitted retry.

Reject traversal, absolute paths, symlinks, special files, duplicate/ambiguous paths, decompression bombs, unsupported provider/codec versions, cross-owner reads and stale claims. Content hashes detect corruption; authenticated encryption and server binding provide confidentiality/provenance.

### D3: Encrypt and bound database storage

Use PostgreSQL and API-held `secretbox`, compressing before encryption while bounding both encoded and streaming decoded bytes. Define per-generation, per-owner and global quotas plus upload expiry. Store only bounded metadata in run/list responses. Raw transcripts, storage keys and arbitrary failure strings never enter logs, metrics, notifications or Git.

### D4: Keep the harness provider-neutral

Expose versioned capture/validate/materialize/restore operations without Go transport importing SDK-specific types. A codec must return a positive resume signal in its harness test; copying a file is insufficient.

Claude uses only the verified allowlist above and includes compacted/subagent artifacts where required. Unsupported Codex returns a typed `codec_missing` state. Do not infer support from a development branch, file copy, or local unit test without the packaged runtime and production route.

### D5: Prepare consumer-specific authorization

The API requires an opaque server handoff identifying owner kind and allowed operation. A `run_hold` handoff cannot read a `pr_session`; a PR-session attempt cannot read an active-run generation. Claims, ownership, repository and generation identity are rechecked on every chunk/finalize/read request.

GC uses consumer-provided references and policies; it cannot delete a generation currently staged/finalizing/restoring. `released` means unavailable to new restores even if physical deletion retries.

## Milestones and dependency plan

Six mostly sequential milestones; M2 and M3 can proceed in parallel after M1 freezes the manifest/routes.

| Phase | Milestone | Dependencies | Primary areas | Outcome |
|---|---|---|---|---|
| 1 | M1: generation schema and wire contract | Decisions D1-D5 | migrations, store/service, worker DTOs | Immutable identities, states, bounds and handoffs are frozen. |
| 2 (parallel) | M2: encrypted transfer and authorization | M1 | API routes/service/LiveDB | Partial/stale/foreign transfers cannot publish or read. |
| 2 (parallel) | M3: agent codec/transfer interface and Claude codec | M1 | harness, codec/client modules, agent tests | Real Claude artifacts round-trip with a positive resume signal. |
| 3 | M4: atomic finalization, CAS and idempotency | M2-M3 | store/service/client | Only complete valid generations become current. |
| 4 | M5: quotas, expiry, release and GC | M4 | store/sweeper/metrics | Storage growth and races are bounded. |
| 5 | M6: docs, ADR, consumer contracts and adversarial proof | M1-M5 | docs/specs/ADR, tests, #1214 PRD | Both consumers can integrate without redefining the primitive. |

- [ ] **M1: generation schema and wire contract.** Define additive tables, manifest v1, owner-kind/id identities, handoff DTOs, rejection enums, chunk/decoded-byte/part/path bounds and provider/codec metadata. Freeze routes and idempotency/CAS semantics before parallel work. Assign migration numbers at landing. Gate: `task gate:api`, relevant LiveDB schema tests.
- [ ] **M2: encrypted transfer and authorization.** Implement claim-fenced chunk upload, finalize/read and server handoffs using secretbox encryption and streaming decoded bounds. LiveDB tests cover wrong user/repo/run/claim/owner kind, stale fence, duplicate/partial upload, corrupt ciphertext/hash, decompression limit, concurrent finalize and cross-owner reads. Gate: `task gate:api`, `task scan:secrets`.
- [ ] **M3: agent interface and Claude codec.** Add neutral capture/validate/materialize/restore operations and transfer client. Implement the verified Claude allowlist with compacted context and subagent sidecars, `mirror_error`/missing-final-batch handling, safe path/type/mode validation and no credential/config capture. Use pinned SDK fixtures and define the offline positive resume signal as the installed SDK session store plus `inspectSession` recognizing the restored session identity; no live model turn or network call is required. Codex returns `codec_missing`; do not fabricate coverage. Gate: `task gate:agent`, `task scan:secrets`.
- [ ] **M4: atomic finalization, CAS and idempotency.** Commit only complete validated manifests, advance expected-current with CAS, return the same generation on retries and prevent an older producer from superseding a newer one. Exercise interrupted upload/finalize response loss, retry, generation race, corrupt materialization and release/finalize ordering across API and client. Gates: `task gate:api`, `task gate:agent`, LiveDB.
- [ ] **M5: quotas, expiry, release and GC.** Add bounded per-generation/owner/global storage, incomplete-upload expiry and reference-safe reclamation. Content-free metrics report bytes, age and outcomes without high-cardinality identities. Test quota/finalize/restore/GC races, secret-key rotation and physical-delete retry; rotation yields typed unavailable, not corrupted restore. Gate: `task gate:api`.
- [ ] **M6: docs, ADR, consumer contracts and adversarial proof.** Add an ADR for the encrypted generation boundary, complete-manifest fence, owner-kind isolation and provider codec contract. Document no-content DTOs and operational storage impact. Verify the 2026-09-09 amendment to `prds/1214-retained-pr-sessions.md` against the landed primitive and refresh any package/route/codec names; its M1/M2 consume #1228 while its PR-session lifecycle decisions remain. Record #1229's hook contract. Run `task docs:sync` for any `docs/` edits and all relevant gates once to logs.

## Acceptance criteria

- No staged, partial, corrupt, oversized, stale-claim or foreign-owner generation becomes resumable.
- Complete-generation publication is CAS-protected and idempotent.
- Raw context is compressed, encrypted and absent from public/ordinary DTOs and logs.
- Claude round-trips a real supported artifact with a positive session-resume signal.
- Credential/config files and unsafe filesystem objects never enter a valid manifest.
- Unsupported Codex is explicitly `codec_missing`; the feature does not claim production support.
- `run_hold` and `pr_session` authorization cannot cross.
- Quota, expiry, release and GC are race-safe and bounded.
- The child introduces no active capture/restore consumer and states that dark status honestly.

## Review record

- 2026-09-09: Split from #1225 after both peer reviews found a combined interlock plus context system too large. This child is the single foundation consumed by #1229 and #1214.
