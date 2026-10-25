# ADR-3396: Run-lane claim size contract

Status: accepted, #3396.

## Decision

The api bounds the run-lane claim in aggregate. Inside the exact-claim finish
transaction, after every projection onto the payload (the task checkpoint recovery
fields) and before commit and delivery, it measures the final encoded body
(`len(json.Marshal(payload))+1`, the `httpx.WriteJSON` body). If that exceeds the worker's claim read cap (168 MiB,
176160768 bytes), the claim is not delivered and the run fails terminally through the
fenced exact-claim finish with `fail_origin` `provisioning_failed` and a reason
starting `claim too large:`. A minted Codex capability is revoked with its identity
preserved.

Rejected: per-field caps chosen so their sum provably stays under the worker cap. The
aggregate also depends on per-run content (plan, comments, completion projections,
memory) and on future fields, so per-field caps would need new settings and leave the
next field unbounded.

## Surfacing

The failure is a typed run failure, not a write-time error. A template or skill-setting
write cannot know future claim sizes, so there is no write-time validation, and no boot
clamp on `SKILLS_MAX_PER_RUN` or `SKILL_MAX_BYTES`.

## One figure

The cap lives in `fixtures/claims/claim-response-limit.json`, pinned by
`api/internal/workersvc/claim_size_test.go` and
`agent/test/client-claim-response-bound.test.ts`. To change it, edit the fixture, the
workersvc `claimResponseMaxBytes` and the agent `CLAIM_RESPONSE_MAX_BYTES` together; both
pin tests redden otherwise. The budget derivation is in the `claimResponseMaxBytes` doc
comment (stock worst case ~41.3 MiB, rounded up 4x to 168 MiB, #2306).

## Boundaries

- Chat: `ChatClaimPayload` carries no templates, skills or issue content (title capped at
  80 runes, ids and scalars, `MemoryPolicy`, one token bounded by the 1 MiB request
  limit), so it is bounded by construction and is not checked.
- `cross_check` claims are exempt: `MarshalCrossCheckClaim` applies its own tighter
  envelope.
- A marshal error is returned unwrapped, the existing non-terminal path: the run stays
  claimed until `SweepClaimedNeverStarted`.
- The claim is encoded once to measure and again by the handler. Reusing the measured
  bytes, as the `cross_check` path does, was deferred to keep the handler unchanged.
- A resumed run failing this way follows the same terminal-assembly path as the existing
  `provisioning_failed` and `credential_unavailable` origins: the current generation's
  unadopted hold is released and checkpoint retention depends on surviving holds
  (`SettleRetainedCheckpoint`); otherwise recovery depends on existing verified
  archive/publication sources.
- Rollout skew: workers older than #3392 read claims uncapped, so the api may now fail a
  non-stock claim over 168 MiB that an old worker could have parsed.
