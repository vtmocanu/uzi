import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { makeScratchRoot } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

const run = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
function latch() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { resolve, promise };
}
for (const observer of ["admission", "recovery", "observation"] as const) {
  it("R1 ordinary install temporary is not retained as a phantom first writer by " + observer, async t => {
    const dir = makeScratchRoot("r1-install-observation-", await fs.realpath(os.tmpdir()));
    t.after(() => fs.rm(dir, { recursive: true, force: true }));
    const root = path.join(dir, "outbox");
    const entered = latch(), resume = latch();
    let tempName = "";
    const box = new Outbox({
      root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1,
      rawWrite: async (write, context) => {
        await write();
        if (context.kind === "terminal" && context.path.endsWith(".tmp")) {
          tempName = path.basename(context.path);
          entered.resolve();
          await resume.promise;
        }
      },
    });
    await box.init();
    const writing = box.journalTerminal(run, 7, "running", 0, { status: "completed" });
    try {
      await entered.promise;
      if (observer === "admission") await box.hasPendingPolicyOutcome(run);
      else if (observer === "observation")
        assert.equal((await box.observeTerminalAuthentication(run, 7, tempName)).kind, "malformed");
      else {
        let page = await box.scanTerminalObservationsPage();
        for (let pages = 0; !page.passComplete && pages < 10; pages++) page = await box.scanTerminalObservationsPage();
        assert.equal(page.passComplete, true);
        await box.closeTerminalObservationScan();
      }
    } finally {
      resume.resolve();
      assert.equal((await writing).journaled, true);
      await box.closeTerminalObservationScan();
    }
    assert.equal(await box.hasPendingPolicyOutcome(run), false,
      "an unfinished owned installation must not become a permanent unknown source after its normal commit");
    assert.equal((await box.readTerminalJournal(run, 7))?.body.status, "completed");
  });
}

for (const name of [
  "terminal-0007.json.aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.tmp",
  "terminal-7.json.00000000-0000-0000-0000-000000000000.tmp",
]) {
  it("R1 noncanonical install lookalike remains protected: " + name, async t => {
    const dir = makeScratchRoot("r1-temp-lookalike-", await fs.realpath(os.tmpdir()));
    t.after(() => fs.rm(dir, { recursive: true, force: true }));
    const root = path.join(dir, "outbox");
    const box = new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
    await box.init();
    await fs.mkdir(path.join(root, run));
    const file = path.join(root, run, name);
    await fs.writeFile(file, "unknown");
    assert.equal(await box.hasPendingPolicyOutcome(run), true);
    await fs.unlink(file);
    assert.equal(await box.hasPendingPolicyOutcome(run), true);
  });
}

it("R1 symlink with an owned-install name remains unknown and is never followed", async t => {
  const dir = makeScratchRoot("r1-temp-symlink-", await fs.realpath(os.tmpdir()));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "outbox");
  const box = new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
  await box.init();
  await fs.mkdir(path.join(root, run));
  const target = path.join(dir, "untouched");
  await fs.writeFile(target, "untouched");
  const file = path.join(root, run, "terminal-7.json.aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.tmp");
  await fs.symlink(target, file);
  assert.equal(await box.hasPendingPolicyOutcome(run), true);
  await fs.unlink(file);
  assert.equal(await box.hasPendingPolicyOutcome(run), true);
  assert.equal(await fs.readFile(target, "utf8"), "untouched");
});

it("R1 policy selection remains protected throughout its own temporary installation", async t => {
  const dir = makeScratchRoot("r1-policy-temp-", await fs.realpath(os.tmpdir()));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "outbox");
  const entered = latch(), resume = latch();
  const box = new Outbox({
    root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1,
    rawWrite: async (write, context) => {
      await write();
      if (context.kind === "terminal" && context.path.endsWith(".tmp")) {
        entered.resolve();
        await resume.promise;
      }
    },
  });
  await box.init();
  const request = {
    outcome_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", source_generation: 7, claim_generation: 7,
    detail: "refused" as const, continuation_mode: "same_conversation" as const,
    tag: "cyberPolicy" as const, phase: "implementation" as const,
    correlation_id: "pr-" + "a".repeat(32) + "-1", published: false,
  };
  const writing = box.journalPolicyHold(run, request);
  try {
    await entered.promise;
    assert.equal(await box.hasPendingPolicyOutcome(run), true);
  } finally { resume.resolve(); assert.equal((await writing).journaled, true); }
  assert.equal(await box.hasPendingPolicyOutcome(run), true);
  assert.equal((await box.readTerminalJournal(run, 7))?.body.outcome_id, request.outcome_id);
});
