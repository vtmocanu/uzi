import { test } from "node:test";
import assert from "node:assert/strict";

import { isRecoveryPrerequisiteClosureStatus } from "../src/protocol.js";
import type {
  RecoveryReserveRequest,
  RecoveryReserveResponse,
  RecoveryUploadManifest,
  RecoveryCaptureStatusResponse,
  RecoveryReleaseResponse,
} from "../src/protocol.js";

// The worker side of the PRD #1296 M1 durable-recovery RPC contract. M1 freezes these
// request/response shapes (TYPES ONLY — the client methods land in M3), so this test pins
// them across the language boundary: each type is exercised in a value position, so a
// rename/removal in protocol.ts fails `npm run typecheck`, and the shapes are asserted to
// match the Go worker-facing DTOs in api/internal/apitypes/recovery.go (their tag sets are
// pinned by api/internal/apitypes/wire_test.go's TestRecoveryWorkerRPCTags). It also keeps
// the frozen-but-not-yet-consumed types out of the knip dead-code gate until M3 wires them.

test("recovery RPC request/response shapes are frozen (PRD #1296 M1)", () => {
  const reserve: RecoveryReserveRequest = {
    run_id: "run-1",
    idempotency_key: "k1",
    source_sha: "H0",
    attempted_head_sha: "Hprime",
  };
  assert.equal(reserve.run_id, "run-1");
  assert.equal(reserve.attempted_head_sha, "Hprime");
  // attempted_head_sha is optional (omitted when no publish was attempted).
  const reserveNoAttempt: RecoveryReserveRequest = {
    run_id: "run-1",
    idempotency_key: "k1",
    source_sha: "H0",
  };
  assert.equal(reserveNoAttempt.attempted_head_sha, undefined);

  const reserveResp: RecoveryReserveResponse = { capture_id: "cap-1", state: "preparing" };
  assert.equal(reserveResp.state, "preparing");

  const manifest: RecoveryUploadManifest = {
    byte_size: 4096,
    checksum: "sha256:abc",
    chunk_count: 2,
    prerequisite_shas: ["base1"],
  };
  assert.equal(manifest.chunk_count, 2);
  // prerequisite_shas is optional.
  const manifestNoPrereq: RecoveryUploadManifest = {
    byte_size: 1,
    checksum: "x",
    chunk_count: 1,
  };
  assert.equal(manifestNoPrereq.prerequisite_shas, undefined);

  const status: RecoveryCaptureStatusResponse = {
    capture_id: "cap-1",
    state: "available",
    manifest_bound: true,
    byte_size: 4096,
    checksum: "sha256:abc",
    reason: "",
    expires_at: "2026-01-02T03:04:05Z",
  };
  assert.equal(status.manifest_bound, true);
  // The optional fields may all be absent (a preparing capture with no manifest yet).
  const statusMinimal: RecoveryCaptureStatusResponse = {
    capture_id: "cap-1",
    state: "preparing",
    manifest_bound: false,
  };
  assert.equal(statusMinimal.byte_size, undefined);
  assert.equal(statusMinimal.prerequisite_closure, undefined);

  // The optional C1b closure object mirrors Go apitypes.RecoveryPrerequisiteClosureStatus
  // (tags pinned by TestRecoveryWorkerRPCTags): state, reason, prerequisite_shas, checksum.
  const sha = "a".repeat(40);
  const checksum = "b".repeat(64);
  const withClosure: RecoveryCaptureStatusResponse = {
    ...status,
    prerequisite_closure: { state: "pinned", prerequisite_shas: [sha], checksum },
  };
  assert.equal(isRecoveryPrerequisiteClosureStatus(withClosure.prerequisite_closure), true);
  assert.equal(isRecoveryPrerequisiteClosureStatus({ state: "refused", reason: "pin_refused" }), true);
  assert.equal(isRecoveryPrerequisiteClosureStatus({ state: "pending", reason: "" }), true);
  for (const bad of [undefined, null, [], "pinned", {}, { state: "available" }, { state: "pinned", reason: "nope" },
    { state: "pinned", extra: 1 }, { state: "pinned", prerequisite_shas: ["short"] },
    { state: "pinned", prerequisite_shas: "x" }, { state: "pinned", checksum: "short" }, { state: 1 }]) {
    assert.equal(isRecoveryPrerequisiteClosureStatus(bad), false, JSON.stringify(bad));
  }

  const release: RecoveryReleaseResponse = {
    run_id: "run-1",
    released: true,
    holds_released: 1,
  };
  assert.equal(release.released, true);
  assert.equal(release.holds_released, 1);
});
