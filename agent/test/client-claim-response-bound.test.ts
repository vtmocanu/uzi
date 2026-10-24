import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { CLAIM_RESPONSE_MAX_BYTES, WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

// Issue #2306: run and chat claim responses are read under a streamed-byte cap, never an
// unbounded res.json(). The cap is shared with the api through the fixture; Content-Length is
// forged small to prove streamed bytes are what is counted.
const MIB = 1024 * 1024;
const fixture = JSON.parse(
  fs.readFileSync(new URL("../../fixtures/claims/claim-response-limit.json", import.meta.url), "utf8"),
) as { claim_response_max_bytes: number };
const CAP = fixture.claim_response_max_bytes;

/** `{`, a reused 1 MiB block of spaces until `totalBytes` have been enqueued, then a closing
 *  `"run_id":"r1"}` (valid JSON). The trailing field is only visible after a full read. */
function paddedBody(totalBytes: number): Response {
  const block = new Uint8Array(MIB).fill(0x20);
  let sent = 0;
  const stream = new ReadableStream<Uint8Array>({
    pull(controller) {
      if (sent === 0) {
        controller.enqueue(new TextEncoder().encode("{"));
        sent = 1;
        return;
      }
      if (sent >= totalBytes) {
        controller.enqueue(new TextEncoder().encode('"run_id":"r1"}'));
        controller.close();
        return;
      }
      controller.enqueue(block);
      sent += block.byteLength;
    },
  });
  return new Response(stream, {
    status: 200,
    headers: { "Content-Length": "2", "Content-Type": "application/json" },
  });
}

function clientFor(totalBytes: number): WorkerClient {
  return new WorkerClient("http://example.test", "join-fixture", "test", nullLogger(), {
    fetch: async () => paddedBody(totalBytes),
  });
}

const isOverflow = (err: unknown): boolean =>
  err instanceof Error && err.name === "ResponseBodyOverflowError" && /exceeds/.test(err.message);

describe("claim response bound", () => {
  it("the client cap equals the shared api/worker fixture figure", () => {
    assert.equal(CLAIM_RESPONSE_MAX_BYTES, fixture.claim_response_max_bytes);
  });

  it("claimRun refuses a body past the cap", async () => {
    await assert.rejects(() => clientFor(CAP + MIB).claimRun(), isOverflow);
  });

  it("claimChat refuses a body past the cap", async () => {
    await assert.rejects(() => clientFor(CAP + MIB).claimChat(), isOverflow);
  });

  it("claimRun accepts a legitimate stock-maximum (~42 MiB) body", async () => {
    const claim = await clientFor(42 * MIB).claimRun();
    // The field follows ~42 MiB of padding, so a prefix or truncated read cannot produce it.
    assert.deepEqual(claim, { run_id: "r1" });
  });
});
