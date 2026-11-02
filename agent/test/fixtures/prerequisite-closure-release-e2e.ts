// Real WorkerClient against the real API handler and Postgres, launched by the Go proof
// TestPrerequisiteClosureReleaseE2E. The Go side owns the pinned closure and the loopback forge.
import assert from "node:assert/strict";
import { RequestError, WorkerClient } from "../../src/client.js";
import type { Logger } from "../../src/log.js";

const cfg = JSON.parse(process.env.UZI_CLOSURE_RELEASE_FIXTURE ?? "") as { url: string; token: string; retain_pin?: boolean; expected_refusal?: boolean };
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
  source_sha: string; coverage_digest: string; worker_id?: string; closure_id?: string;
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
  for (const key of ["UZI_TOKEN", "UZI_URL", "UZI_CONTEXT", "UZI_RESTORE_SENTINEL"]) {
    assert.equal(process.env[key], undefined, "inherited fixture isolation: " + key);
  }
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
  if (info.worker_id) assert.equal(registration.worker_id, info.worker_id);

  if (cfg.expected_refusal) {
    await assert.rejects(client.releaseRecoveryCustody(info.run_id, info.generation, undefined, disposition),
      (err: unknown) => err instanceof RequestError && err.status === 409 && JSON.parse(err.body).reason === "manifest_conflict");
    const refused = await control<Inspect>("inspect");
    assert.deepEqual(refused.hold, before.hold);
    assert.equal(refused.listings, before.listings);
    assert.equal(refused.pin_present, false);
    console.log("closure refusal: FINAL refused; hold unchanged");
    process.exit(0);
  }

  // (a) A thin guarded archive FINAL is released against the pinned closure with exactly one forge listing.
  const ack = await client.releaseRecoveryCustody(info.run_id, info.generation, undefined, disposition);
  assert.equal(ack.run_id, info.run_id);
  assert.equal(ack.released, true);
  assert.equal(ack.holds_released, 1);
  assert.equal(ack.generation, info.generation);
  assert.ok(!ack.retained, "closure release must not be retained");
  const released = await control<Inspect>("inspect");
  assert.equal(released.hold.state, "released", "committed closure release state");
  assert.equal(released.hold.release_evidence, "prerequisite_closure");
  assert.equal(released.hold.final_disposition, "archive");
  assert.equal(released.hold.final_capture_id, info.capture_id);
  assert.ok(released.hold.final_closure_id, "server row must name the closure that backed the release");
  if (info.closure_id) assert.equal(released.hold.final_closure_id, info.closure_id);
  assert.equal(released.listings - before.listings, 1);

  // (b) Lost ACK: the same saved request is replayed after the pin is gone and gets the same ACK
  // with no further forge listing.
  const afterDelete = await control<Inspect>(cfg.retain_pin ? "inspect" : "delete-pin");
  assert.equal(afterDelete.pin_present, cfg.retain_pin === true);
  const replay = await client.releaseRecoveryCustody(info.run_id, info.generation, undefined, disposition);
  assert.deepEqual(replay, ack);
  const replayed = await control<Inspect>("inspect");
  assert.equal(replayed.listings, released.listings, "replay must not list the forge again");
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
  console.error("fixture log entries=" + lines.length);
  if (err instanceof assert.AssertionError && err.message.startsWith("committed closure release state") && err.actual === "open" && err.expected === "released") {
    console.error("committed closure release state actual=open expected=released");
  } else {
    console.error("fixture assertion failed", err instanceof Error ? err.name : "unknown");
  }
  process.exitCode = 1;
}
