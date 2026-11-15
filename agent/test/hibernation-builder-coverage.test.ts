import { after, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
import type { Config } from "../src/config.js";
import { createDindBuilderPrune } from "../src/dind-builder-prune.js";
import { DindPruneGate, TRIGGER_STREAK, dockerExecEnvironment } from "../src/dind-prune.js";
import {
  HibernationInventoryController, hibernationWriters,
  type HibernationInventoryBody, type HibernationInventoryMember, type InventoryRequest,
} from "../src/hibernation-inventory.js";
import { nullLogger } from "./helpers.js";

// The builder leg builds its child env once per process, which creates the shared empty
// DOCKER_CONFIG dir in tmpdir; remove it so tmpdir-leak-guard stays clean.
after(() => { rmSync(dockerExecEnvironment("unix:///unused").DOCKER_CONFIG as string, { recursive: true, force: true }); });

const wire = JSON.parse(readFileSync(new URL("../../fixtures/hibernation-inventory/wire.json", import.meta.url), "utf8"));
const member = wire.heartbeat_response_member.awaiting.hibernation_inventory as HibernationInventoryMember;

async function inventory(gate: DindPruneGate): Promise<InventoryRequest> {
  let mode: "shared" | "exclusive" = "shared";
  let posted: HibernationInventoryBody | undefined;
  const controller = new HibernationInventoryController({
    registry: hibernationWriters, gate, laneSupported: true,
    client: {
      hasFeature: () => true,
      currentRegisterNonce: () => member.register_nonce,
      postHibernationInventory: async (body) => { posted = body; },
      postHibernationExclusionFinal: async () => {},
    },
    ownership: {
      get mode() { return mode; },
      async upgradeExclusive() { mode = "exclusive"; return true; },
      async reacquireShared() { mode = "shared"; },
    } as never,
    podUid: () => member.pod_uid, isIdle: () => true, proof: async () => true,
    census: async () => ({
      complete: true, entries: [], fingerprint: "f".repeat(64), digest: "d".repeat(64),
      docker: { containers: [], volumes: [], images_count: 0, build_cache_bytes: 0 }, uncovered: [],
    }),
    recomputeFingerprint: async () => ({ fingerprint: "f".repeat(64), digest: "d".repeat(64) }),
    fatalExit: (reason) => assert.fail(reason), log: nullLogger(),
  });
  try {
    controller.observe(member);
    await controller.whenIdle();
    assert.ok(posted && "complete" in posted, "the real controller must post an inventory");
    return posted;
  } finally {
    controller.observe(null);
    await controller.whenIdle();
    controller.stop();
  }
}

it("a configured builder leg keeps inventory incomplete across unknown and local child settlement", async () => {
  const gate = new DindPruneGate();
  const config: Pick<Config, "dindPruneEnabled" | "dindBuilderPruneEnabled" | "dockerWiring" | "workerName"> = {
    dindPruneEnabled: true, dindBuilderPruneEnabled: true,
    dockerWiring: { dockerHost: "unix:///test-dind.sock" }, workerName: "builder-coverage-test",
  };
  const deps = { gate, log: nullLogger() };
  assert.equal(createDindBuilderPrune({ ...config, dindPruneEnabled: false }, deps), undefined);
  assert.equal(createDindBuilderPrune({ ...config, dindBuilderPruneEnabled: false }, deps), undefined);
  assert.equal(createDindBuilderPrune({ ...config, dockerWiring: {} }, deps), undefined);
  assert.deepEqual(hibernationWriters.uncovered(), []);
  assert.equal((await inventory(gate)).complete, true);

  let now = 1_700_000_000_000;
  let closeChild!: () => void;
  let observedClose!: () => void;
  const childSettled = new Promise<void>((resolve) => { closeChild = resolve; });
  const closeObserved = new Promise<void>((resolve) => { observedClose = resolve; });
  const builder = createDindBuilderPrune(config, {
    ...deps, now: () => now, pluginPath: fileURLToPath(import.meta.url),
    readSample: () => ({ epochS: now / 1000, bytesUsed: 95, bytesTotal: 100, inodesUsed: 1, inodesTotal: 100 }),
    exec: async () => ({ kind: "killed", reason: "timeout", unsettled: { pid: 123, pgid: 123, settled: childSettled } }),
    onSettled: observedClose,
  });
  assert.ok(builder);
  let outcome;
  for (let i = 0; i < TRIGGER_STREAK; i++) { now += 30_000; outcome = await builder.tick(); }
  assert.equal(outcome, "unknown");
  const unknown = await inventory(gate);
  assert.equal(unknown.complete, false, "an unclosed builder child cannot yield complete inventory");
  assert.deepEqual(unknown.exclusion.uncovered, ["dind_builder"]);

  closeChild();
  await closeObserved;
  const closed = await inventory(gate);
  assert.equal(closed.complete, false, "local close is not proof of daemon-side settlement");
  assert.deepEqual(closed.exclusion.uncovered, ["dind_builder"]);
});
