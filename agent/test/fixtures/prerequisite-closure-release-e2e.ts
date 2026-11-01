// Real WorkerClient against the real API handler and Postgres, launched by the Go proof
// TestPrerequisiteClosureReleaseE2E. The Go side owns the pinned closure and the loopback forge.
import assert from "node:assert/strict";
import { WorkerClient } from "../../src/client.js";
import type { Logger } from "../../src/log.js";

const cfg = JSON.parse(process.env.UZI_CLOSURE_RELEASE_FIXTURE ?? "") as { url: string; token: string };
const CLOSURE_FEATURE = "recovery_prerequisite_closure_v1";

const lines: string[] = [];
const log: Logger = {
  debug: (message, fields) => { lines.push(JSON.stringify({ message, fields })); },
  info: (message, fields) => { lines.push(JSON.stringify({ message, fields })); },
  warn: (message, fields) => { lines.push(JSON.stringify({ message, fields })); },
  error: (message, fields) => { lines.push(JSON.stringify({ message, fields })); },
  addSecret() {}, removeSecret() {}, child: () => log,
};

type Info = {
  run_id: string; capture_id: string; hold_id: string; repo_id: string; generation: number;
  source_sha: string; coverage_digest: string;
};
type Inspect = {
  listings: number; pin_present: boolean;
  hold: { state: string; release_evidence: string | null; final_disposition: string | null; final_closure_id: string | null; final_capture_id: string | null };
};

async function control<T>(action: string): Promise<T> {
  const response = await fetch(cfg.url + "/fixture/" + action, {
    method: "POST", headers: { Authorization: "Bearer " + cfg.token }, signal: AbortSignal.timeout(10_000),
  });
  assert.equal(response.ok, true, "fixture control " + action + ": " + await response.clone().text());
  return await response.json() as T;
}

try {
  const client = new WorkerClient(cfg.url, cfg.token, "0.1.0-test", log, {});
  const registration = await client.register("closure-release", undefined, 1, [], [
    "recovery_archive_v1", "recovery_inventory_v1", CLOSURE_FEATURE, "recovery_held_publication_v2",
  ]);
  // The server advertises the closure feature only because the knob, the port and the limiter are all wired.
  assert.equal(client.hasFeature(CLOSURE_FEATURE), true, "register response must advertise " + CLOSURE_FEATURE);
  assert.equal(client.hasFeature("recovery_inventory_v1"), true);

  const info = await control<Info>("info");
  const disposition = {
    kind: "archive" as const, capture_id: info.capture_id, source_sha: info.source_sha, coverage_digest: info.coverage_digest,
  };
  const before = await control<Inspect>("inspect");
  assert.equal(before.hold.state, "open");
  assert.equal(before.listings, 0);

  // (a) A thin guarded archive FINAL is released against the pinned closure with exactly one forge listing.
  const ack = await client.releaseRecoveryCustody(info.run_id, info.generation, undefined, disposition);
  assert.equal(ack.run_id, info.run_id);
  assert.equal(ack.released, true);
  assert.equal(ack.holds_released, 1);
  assert.equal(ack.generation, info.generation);
  assert.ok(!ack.retained, "closure release must not be retained");
  const released = await control<Inspect>("inspect");
  assert.equal(released.hold.state, "released");
  assert.equal(released.hold.release_evidence, "prerequisite_closure");
  assert.equal(released.hold.final_disposition, "archive");
  assert.equal(released.hold.final_capture_id, info.capture_id);
  assert.ok(released.hold.final_closure_id, "server row must name the closure that backed the release");
  assert.equal(released.listings, 1);

  // (b) Lost ACK: the same saved request is replayed after the pin is gone and gets the same ACK
  // with no further forge listing.
  const afterDelete = await control<Inspect>("delete-pin");
  assert.equal(afterDelete.pin_present, false);
  const replay = await client.releaseRecoveryCustody(info.run_id, info.generation, undefined, disposition);
  assert.deepEqual(replay, ack);
  const replayed = await control<Inspect>("inspect");
  assert.equal(replayed.listings, 1, "replay must not list the forge again");
  assert.deepEqual(replayed.hold, released.hold);

  // (c) The client's exact closed-hold observation accepts the closure-backed archive.
  const exact = await client.getExactRecoveryHold(info.run_id, info.generation, { holdId: info.hold_id, repoId: info.repo_id });
  assert.ok(exact, "exact closed-hold observation must be accepted");
  assert.equal(exact.state, "released");
  assert.equal(exact.release_evidence, "prerequisite_closure");
  assert.equal(exact.original_worker_id, registration.worker_id);
  assert.deepEqual(exact.final_receipt, disposition);
  assert.equal(exact.live_worker_id, undefined);
  assert.equal(exact.live_run_id, undefined);
  console.log("closure release: advertised -> FINAL ACK (prerequisite_closure) -> lost-ACK replay -> exact observation");
} catch (err) {
  console.error("fixture log:\n" + lines.join("\n"));
  throw err;
}
