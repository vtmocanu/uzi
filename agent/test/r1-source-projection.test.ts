import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ActiveSnapshot } from "../src/protocol.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { makeScratchRoot } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

const run = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const other = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
async function fixture(t: { after(fn: () => Promise<void>): void }) {
  const dir = makeScratchRoot("r1-source-", await fs.realpath(os.tmpdir()));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "outbox");
  const box = new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
  await box.init();
  return { box, root, file: path.join(root, run, "terminal-7.json") };
}
function sources(box: Outbox) {
  return (box as Outbox & { listPendingOutcomeSources?: Outbox["listPendingTerminals"] })
    .listPendingOutcomeSources?.() ?? box.listPendingTerminals();
}
for (const alias of ["ordinary", "unknown", "policy-unknown", "settled", "settled-policy"] as const) for (const reverse of [false, true]) {
  it("R1 real supplier preserves independent policy evidence beside " + alias + " alias, reverse=" + reverse, async t => {
    const { box, root } = await fixture(t);
    const upper = run.toUpperCase();
    const policy = {
      outcome_id: other, source_generation: 7, claim_generation: 7,
      detail: "refused" as const, continuation_mode: "same_conversation" as const,
      published: false, tag: "cyberPolicy" as const, phase: "implementation" as const,
      correlation_id: "pr-" + "a".repeat(32) + "-1",
    };
    if (alias === "settled-policy") await box.journalPolicyHold(upper, policy);
    else await box.journalTerminal(upper, 7, "running", 0, { status: "failed" });
    if (alias === "settled" || alias === "settled-policy") {
      const observed = await box.readTerminalJournalForRetirement(upper, 7);
      assert.ok(observed);
      await box.markTerminalServerSettled(upper, 7, observed.context);
    }
    assert.equal((await box.journalPolicyHold(run, policy)).journaled, true);
    if (alias === "unknown" || alias === "policy-unknown") {
      const damaged = alias === "unknown" ? upper : run;
      await fs.writeFile(path.join(root, damaged, "terminal-7.json"), "{");
      assert.equal((await box.observeTerminalAuthentication(damaged, 7)).kind, "malformed");
    }
    // A restart rederives the damaged policy kind without its independent in-memory choice.
    const inventory = alias === "policy-unknown"
      ? new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 })
      : box;
    if (inventory !== box) await inventory.init();
    await scan(inventory);
    t.after(() => inventory.closeTerminalObservationScan());
    const registry = new ActiveRunRegistry(() => {
      const entries = inventory.listPendingOutcomeSources();
      return reverse ? entries.reverse() : entries;
    }, () => 1, () => inventory.policyInventoryCoverageAvailable());
    assert.equal(registry.beginPolicyOwnershipObservation(run)(9), true);
    assert.equal(registry.claimsPausedByPendingOverflow(true), false);
    const snapshot = registry.build(true);
    assert.deepEqual(snapshot.pending_all, [{ run_id: run, claim_generation: 7 }]);
    assert.deepEqual(snapshot.pending_policy_protection, [{ run_id: run, owner_generation: 9 }]);
    assert.ok(inventory.listPendingOutcomeSources().some(p =>
      p.outcome_kind === (alias === "policy-unknown" ? "unknown" : "policy_hold") && !p.server_settled),
      "unknown or settled aliases must preserve independent unsettled policy facts");
    assert.deepEqual(snapshot.active, alias === "unknown" || alias === "policy-unknown" ? [] : [
      { run_id: run, claim_generation: 9, phase: "running", terminal_pending: true },
    ], "unknown evidence grants no lease; authenticated policy leases only the observed owner");
    if (alias !== "policy-unknown") assert.deepEqual((await box.readTerminalJournal(run, 7))?.body, policy);
  });
}

async function scan(box: Outbox) {
  for (let i = 0; i < 20; i++) {
    if ((await box.scanTerminalObservationsPage()).passComplete) return;
  }
  assert.fail("physical pass must complete without duplicating a counted source");
}
it("R1 exact MAC scope projects source7 and independent owner9 without pausing below cap", async t => {
  const { box, file } = await fixture(t);
  await box.journalTerminal(run, 7, "running", 0, { status: "failed" });
  const record = JSON.parse(await fs.readFile(file, "utf8"));
  record.body = { status: "completed" };
  await fs.writeFile(file, JSON.stringify(record));
  assert.equal((await box.observeTerminalAuthentication(run, 7)).kind, "mac_failure");
  const registry = new ActiveRunRegistry(() => sources(box), () => 2, () => box.policyInventoryCoverageAvailable());
  registry.beginPolicyOwnershipObservation(run)(9);
  await scan(box);
  assert.equal(registry.claimsPausedByPendingOverflow(true), false,
    "known source and independently observed owner must permit unrelated below-cap claims");
  const snapshot = registry.build(true);
  assert.deepEqual(snapshot.pending_all, [{ run_id: run, claim_generation: 7 }]);
  assert.deepEqual(snapshot.pending_policy_protection, [{ run_id: run, owner_generation: 9 }]);
  assert.equal(snapshot.pending_overflow, true);
  assert.deepEqual(snapshot.active, [], "physical unknown grants no source-generation lease");
  assert.equal(registry.claimsPausedByPendingOverflow(), true);
  assert.deepEqual(box.listPendingTerminals(), [], "scope is not an installed journal or send source");
  await fs.unlink(file);
  await scan(box);
  assert.equal(await box.hasPendingPolicyOutcome(run), true);
  assert.deepEqual(sources(box).map(p => p.claim_generation), [7], "disappearance does not release source evidence");
});
it("R1 full physical catalog scans an already counted MAC scope without allocating metadata", async t => {
  const { box, file } = await fixture(t);
  await box.journalTerminal(run, 7, "running", 0, { status: "failed" });
  const record = JSON.parse(await fs.readFile(file, "utf8"));
  record.body = {};
  await fs.writeFile(file, JSON.stringify(record));
  assert.equal((await box.observeTerminalAuthentication(run, 7)).kind, "mac_failure");
  const tokens = Array.from({ length: 1023 }, (_, i) => box.reservePolicyFlight(other, i + 1));
  assert.ok(tokens.every(Boolean));
  await scan(box);
  assert.equal(box.policyInventoryCoverageAvailable(), true);
  assert.deepEqual(box.listPendingTerminals(), []);
  for (const token of tokens) box.releasePolicyFlight(token!);
});

for (const kind of ["malformed", "leafsymlink", "oversized"] as const) {
  it("R1 actual " + kind + " leaf supplies grouping identity only", async t => {
    const { box, root, file } = await fixture(t);
    await box.journalTerminal(run, 7, "running", 0, { status: "failed" });
    await fs.unlink(file);
    if (kind === "leafsymlink") {
      const target = path.join(root, "inert-target");
      await fs.writeFile(target, "untouched");
      await fs.symlink(target, file);
    } else await fs.writeFile(file, kind === "oversized" ? "x".repeat(1000001) : "{");
    assert.equal((await box.observeTerminalAuthentication(run, 7)).kind,
      kind === "leafsymlink" ? "symlink" : kind);
    await scan(box);
    assert.equal(box.policyInventoryCoverageAvailable(), true);
    const pending = sources(box);
    assert.equal(new Set(pending.map(p => p.run_id + "/" + p.claim_generation)).size, 1);
    assert.ok(pending.some(p => p.outcome_kind === "unknown"), "physical evidence remains independent of installed metadata");
    const registry = new ActiveRunRegistry(() => sources(box), () => 2, () => box.policyInventoryCoverageAvailable());
    registry.beginPolicyOwnershipObservation(run)(9);
    assert.equal(registry.claimsPausedByPendingOverflow(true), false);
    assert.deepEqual(registry.build(true).active, []);
    await fs.unlink(file);
    await scan(box);
    assert.equal(await box.hasPendingPolicyOutcome(run), true);
    assert.equal(sources(box)[0]?.claim_generation, 7);
    assert.equal(await box.cleanupRejectedTerminalFiles(run, 7, other, [], async () => {
      assert.fail("grouping identity cannot authorize custody or deletion");
    }), 0);
  });
}

it("R1 unsafe parent stays opaque after later safe leaf observation and EOF", async t => {
  const { box, root, file } = await fixture(t);
  const actual = path.join(root, "actual");
  await fs.mkdir(actual);
  await fs.symlink(actual, path.join(root, run));
  assert.equal((await box.observeTerminalAuthentication(run, 7)).kind, "symlink");
  assert.deepEqual(sources(box), [], "requested generation is not physical source evidence");
  await fs.unlink(path.join(root, run));
  await fs.mkdir(path.join(root, run));
  await fs.writeFile(file, "{");
  assert.equal((await box.observeTerminalAuthentication(run, 7)).kind, "malformed");
  await scan(box);
  assert.equal(box.policyInventoryCoverageAvailable(), false, "later leaf cannot promote an opaque scope");
  await fs.unlink(file);
  await scan(box);
  assert.equal(box.policyInventoryCoverageAvailable(), false);
  assert.equal(await box.hasPendingPolicyOutcome(run), true);
});

it("R1 scoped source groups remain paused at zero cap and over cap", async t => {
  const { box, root, file } = await fixture(t);
  await fs.mkdir(path.dirname(file), { recursive: true });
  await fs.symlink(path.join(root, "missing"), file);
  await box.observeTerminalAuthentication(run, 7);
  await fs.mkdir(path.join(root, other));
  await fs.symlink(path.join(root, "missing"), path.join(root, other, "terminal-8.json"));
  await box.observeTerminalAuthentication(other, 8);
  await scan(box);
  for (const cap of [0, 1, 2]) {
    const registry = new ActiveRunRegistry(() => sources(box), () => cap, () => box.policyInventoryCoverageAvailable());
    registry.beginPolicyOwnershipObservation(run)(9);
    registry.beginPolicyOwnershipObservation(other)(10);
    assert.equal(registry.claimsPausedByPendingOverflow(true), cap < 2);
    assert.equal(registry.build(true).pending_overflow, true);
    assert.deepEqual(registry.build(true).active, []);
  }
});

for (const disabled of [false, true]) it("R1 Worker observes scope-only ownership before register/boot/drain/sweep, disabled=" + disabled, async t => {
  const { box, root, file } = await fixture(t);
  await fs.mkdir(path.dirname(file), { recursive: true });
  await fs.symlink(path.join(root, "missing"), file);
  await box.observeTerminalAuthentication(run, 7);
  await scan(box);
  assert.deepEqual(box.listPendingTerminals(), []);
  if (disabled) {
    // Exercise the public failed-closed initialization path while retaining memory.
    await fs.rename(root, root + "-retained");
    await fs.symlink(root + "-retained", root);
    await box.init();
  }
  const registry = new ActiveRunRegistry(() => sources(box), () => 2, () => box.policyInventoryCoverageAvailable());
  const observed: string[] = [];
  const registered: ActiveSnapshot[] = [];
  const client = {
    getRunOwnership: async (runId: string) => {
      observed.push(runId);
      return { status: "running", claim_generation: 9, claim_released: false };
    },
    register: async (...args: unknown[]) => {
      const snapshot = args.find(arg => arg && typeof arg === "object" && "snapshot_epoch" in arg);
      assert.ok(snapshot, "scope-only register must carry a snapshot");
      registered.push(snapshot as ActiveSnapshot);
      return {};
    },
    reportState: async () => { assert.fail("scope is not a terminal send source"); },
    hasFeature: () => false,
  } as unknown as WorkerClient;
  const worker = new Worker({ dockerWiring: {} } as Config, client, {} as RunRunner,
    {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, nullLogger(), undefined, box, undefined, registry);
  const seam = worker as unknown as {
    registerWithRetry(signal: AbortSignal): Promise<void>;
    resolveBootTerminals(signal: AbortSignal): Promise<void>;
    resolveRunTerminal(runId: string): Promise<void>;
    sweepPendingTerminals(): Promise<void>;
  };
  const signal = new AbortController().signal;
  await seam.registerWithRetry(signal);
  await seam.resolveBootTerminals(signal);
  await seam.resolveRunTerminal(run);
  await seam.sweepPendingTerminals();
  assert.deepEqual(observed, [run, run, run, run]);
  assert.equal(registered.length, 1);
  assert.deepEqual(registered[0]?.pending_all, [{ run_id: run, claim_generation: 7 }]);
  assert.deepEqual(registry.build(true).pending_policy_protection, [{ run_id: run, owner_generation: 9 }]);
  assert.deepEqual(registry.build(true).active, []);
});

