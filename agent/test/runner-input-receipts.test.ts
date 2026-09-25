// Issue #1673: the runner holds a resume report behind the applied receipt of the input that
// caused it. When that receipt keeps failing on the active claim, the run must fail cleanly
// rather than wait forever or send the resume as if the input were applied.
import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import { Outbox } from "../src/outbox.js";
import { StubExecutor } from "../src/executor.js";
import { api, fakeGitlab, gitlabClaim, input, installHarness, runner } from "./runner-harness.js";

installHarness();

const tmpRoots: string[] = [];
after(() => {
  for (const dir of tmpRoots) fs.rmSync(dir, { recursive: true, force: true });
});

async function mkOutbox(): Promise<Outbox> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "runner-input-receipts-"));
  tmpRoots.push(dir);
  const outbox = new Outbox({
    root: path.join(dir, "outbox"),
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  });
  await outbox.init();
  return outbox;
}

describe("RunRunner — input receipts (issue #1673)", () => {
  it("fails the run, without a resume report, when the approval's APPLIED keeps failing", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1673);
    // The owner approves once the gate is reported; that approval's applied receipt then fails.
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_approval") api.setInputs(claim.run_id, [input("approve_plan")]);
    });
    api.failInputReceipts("applied", 503);
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab).execute(claim);

    const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    const statuses = states.map((s) => s.status);
    const gate = statuses.indexOf("awaiting_approval");
    assert.ok(gate >= 0, `the run reached the plan gate: ${statuses.join(",")}`);
    assert.deepStrictEqual(statuses.slice(gate + 1), ["failed"], "no resume report after the gate, then failed");
    assert.match(states.at(-1)?.failure_reason ?? "", /could not confirm 1 applied operator input/);
    assert.ok(
      api.inputReceiptCalls.filter((c) => c.runId === claim.run_id && c.kind === "applied").length >= 30,
      "the applied receipt was retried up to the active-claim bound",
    );
  });

  it("stamps the claim's generation on every receipt through a full run (strict generations)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1674, { claim_generation: 5 });
    api.strictReceiptGenerations = true;
    api.setInputClaimGeneration(claim.run_id, 5);
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_approval") api.setInputs(claim.run_id, [input("approve_plan")]);
    });
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.strictEqual(statuses.at(-1), "completed", statuses.join(","));
    const receipts = api.inputReceiptCalls.filter((c) => c.runId === claim.run_id);
    assert.deepStrictEqual(receipts.map((c) => [c.kind, c.generation]), [["ack", 5], ["applied", 5]]);
  });

  it("a flight fenced by a receipt stops quietly: no terminal report and no terminal journal", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1675, { claim_generation: 5 });
    const outbox = await mkOutbox();
    api.strictReceiptGenerations = true;
    api.setInputClaimGeneration(claim.run_id, 5);
    // At the gate the claim is released server-side (e.g. superseded by a reclaim): the approval's
    // ACK comes back 409 with reason "released", which ends this flight.
    api.onState(claim.run_id, (body) => {
      if (body.status !== "awaiting_approval") return;
      api.setInputClaimGeneration(claim.run_id, 6);
      api.setInputFenceReason(claim.run_id, "released");
      api.setInputs(claim.run_id, [input("approve_plan")]);
    });
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    const gate = statuses.indexOf("awaiting_approval");
    assert.ok(gate >= 0, statuses.join(","));
    assert.deepStrictEqual(statuses.slice(gate + 1), [], "no report after the fence, terminal or otherwise");
    assert.deepStrictEqual(outbox.listPendingTerminals(), [], "no terminal journal left behind");
    assert.ok(
      !api.messages(claim.run_id).some((m) => m.kind === "error"),
      "no failure surfaced on the run feed",
    );
  });
});
