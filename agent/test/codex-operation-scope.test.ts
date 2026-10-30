import { it } from "node:test";
import assert from "node:assert/strict";
import { acquireCodexOperationScope } from "../src/codex-operation-scope.js";
import { AdmissionClock, flushUntil } from "./operation-admission-clock.js";
import type { CodexOperationRequest } from "../src/protocol.js";

it("scope records final negotiation loss before a simultaneous source guard throws", async () => {
  const clock = new AdmissionClock();
  let supported = true;
  let epoch = 1;
  let reads = 0;
  let stale = false;
  const client = {
    hasFeature: () => supported,
    get codexOperationNegotiation() { return epoch; },
    checkCodexOperationAdmission: async (_run: string, request: CodexOperationRequest) => {
      reads++;
      return { decision: "admit" as const, operation_id: request.operation_id, claim_generation: request.claim_generation };
    },
  };
  const scope = acquireCodexOperationScope(client, "run", 7, { now: () => clock.now, sleep: clock.sleep });
  scope.bind((signal, check, kind = "root") => scope.controller.admit({
    operation_id: "00000000-0000-4000-8000-000000000001", kind,
  }, signal, check, async () => { check(); }));
  const abort = new AbortController();
  let successor: Promise<() => boolean | void> | undefined;
  try {
    const permit = await scope.prepare("root", abort.signal, () => { if (stale) throw new Error("source replaced"); });
    supported = false;
    stale = true;
    assert.throws(permit, /source replaced/);
    stale = false;
    supported = true;
    epoch++;
    successor = scope.prepare("child", abort.signal, () => {});
    await flushUntil(() => clock.sleepers.size === 1 || reads > 1);
    assert.equal(reads, 1, "replacement cannot bypass unsuccessful admission's shared deadline");
    clock.advance(14_999);
    assert.equal(reads, 1);
    clock.advance(1);
    const next = await successor;
    assert.equal(next(), true);
    assert.equal(reads, 2);
  } finally { abort.abort(); await successor?.catch(() => {}); scope.close(); }
});
