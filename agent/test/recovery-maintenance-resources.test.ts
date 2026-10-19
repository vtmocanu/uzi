import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { performance } from "node:perf_hooks";
import { MaintenanceResources as R, RecoveryMaintenance, type MaintenanceEligibility } from "../src/recovery-maintenance.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { RecoveryCoordinator, type ClosedSourceObservation, type RecoveryArchiveClient } from "../src/recovery.js";
import { nullLogger } from "./helpers.js";

const caps = { metadataBytes: 32 * 1024 ** 2, adminEntries: 4096, rpcAttempts: 16, candidates: 4 } as const;
for (const resource of Object.keys(caps) as Array<keyof typeof caps>) {
  test(`sequential discovery aggregates ${resource} before delivery`, async () => {
    let delivered = 0;
    await R.runDiscoveryPass(async () => {
      await R.runDiscovery(async () => R.consumeIfActive(resource, caps[resource] / 2 + 1));
      assert.equal(R.active(), false);
      await assert.rejects(R.runDiscovery(async () => {
        R.consumeIfActive(resource, caps[resource] / 2);
        delivered++;
      }), /maintenance_resource_exhausted/);
    });
    assert.equal(delivered, 0);
  });
}

for (const clock of ["wall", "monotonic"] as const) {
  test(`sequential discovery retains its first ${clock} deadline across the gap`, async t => {
    let wall = 1_000_000, mono = 0;
    t.mock.method(Date, "now", () => wall);
    t.mock.method(performance, "now", () => mono);
    await R.runDiscoveryPass(async () => {
      await R.runDiscovery(async () => { wall += 4000; mono += 4000; });
      if (clock === "wall") wall += 6000;
      else mono += 6000;
      await assert.rejects(R.runDiscovery(async () => assert.fail("expired segment ran")), /maintenance_resource_exhausted/);
      assert.throws(() => R.checkDiscoveryPass(), /maintenance_resource_exhausted/);
    });
  });
}

test("pass starts lazily, nested passes cannot renew counters or deadline", async t => {
  let wall = 1_000_000, mono = 0;
  t.mock.method(Date, "now", () => wall);
  t.mock.method(performance, "now", () => mono);
  await R.runDiscoveryPass(async () => {
    wall += 20_000; mono += 20_000;
    await R.runDiscovery(async () => {
      R.consumeIfActive("rpcAttempts", 15);
      await R.runDiscoveryPass(() => R.runDiscovery(async () => R.consumeIfActive("rpcAttempts", 1)));
    });
    wall += 9999; mono += 9999;
    await R.runDiscoveryPass(async () => {
      await R.runDiscovery(async () => R.consumeIfActive("rpcAttempts", 0));
      wall++; mono++;
      await assert.rejects(R.runDiscovery(async () => {}), /maintenance_resource_exhausted/);
    });
  });
});

test("caught exhaustion still fails the final discovery check", async () => {
  await assert.rejects(R.runDiscoveryPass(() => R.runDiscovery(async () => {
    assert.throws(() => R.consumeIfActive("candidates", 5), /maintenance_resource_exhausted/);
  })), /maintenance_resource_exhausted/);
});

test("concurrent unrelated passes have independent counters and revocation", async () => {
  let ready!: () => void, resume!: () => void;
  const waiting = new Promise<void>(resolve => { ready = resolve; });
  const release = new Promise<void>(resolve => { resume = resolve; });
  const first = R.runDiscoveryPass(async () => {
    await R.runDiscovery(async () => R.consumeIfActive("candidates", 4));
    ready();
    await release;
    await R.runDiscovery(async () => R.consumeIfActive("candidates", 0));
  });
  await waiting;
  try {
    await R.runDiscoveryPass(() => R.runDiscovery(async () => R.consumeIfActive("candidates", 4)));
  } finally { resume(); }
  await first;
});

test("closed pass and closed segment revoke escaped asynchronous continuations", async () => {
  let release!: () => void;
  const wait = new Promise<void>(resolve => { release = resolve; });
  let escaped!: Promise<void>, segment!: Promise<void>;
  await R.runDiscoveryPass(async () => {
    escaped = wait.then(() => R.runDiscovery(async () => assert.fail("closed pass ran")));
    await R.runDiscovery(async () => {
      segment = wait.then(() => R.consumeIfActive("rpcAttempts", 1));
    });
  });
  const deniedPass = assert.rejects(escaped, /maintenance_resource_exhausted/);
  const deniedSegment = assert.rejects(segment, /maintenance_resource_exhausted/);
  release();
  await Promise.all([deniedPass, deniedSegment]);
});

test("nested pass closure revokes its escaped callback while outer pass remains open", async () => {
  let release!: () => void;
  const wait = new Promise<void>(resolve => { release = resolve; });
  let escaped!: Promise<void>;
  await R.runDiscoveryPass(async () => {
    await R.runDiscoveryPass(async () => {
      escaped = wait.then(() => R.runDiscovery(async () => assert.fail("closed nested pass ran")));
      await R.runDiscovery(async () => R.consumeIfActive("candidates", 1));
    });
    const denied = assert.rejects(escaped, /maintenance_resource_exhausted/);
    release();
    await denied;
    await R.runDiscovery(async () => R.consumeIfActive("candidates", 3));
  });
});

test("cancellation between segments cannot authorize another segment or charge check", async () => {
  const controller = new AbortController();
  await R.runDiscoveryPass(async () => {
    await R.runDiscovery(async () => {});
    controller.abort(new Error("cancelled pass"));
    await assert.rejects(R.runDiscovery(async () => assert.fail("cancelled segment ran")), /cancelled pass/);
    assert.throws(() => R.checkDiscoveryPass(), /cancelled pass/);
  }, { signal: controller.signal });
});

test("tighter parent deadline and nested cancellation win", async () => {
  let now = 0;
  const parent = new MaintenanceBudget(50, () => now);
  try {
    await MaintenanceBudget.run(parent, () => R.runDiscoveryPass(async () => {
      await R.runDiscovery(async () => {});
      now = 50;
      await assert.rejects(R.runDiscovery(async () => {}), /maintenance deadline/);
    }));
  } finally { parent.dispose(); }
  const controller = new AbortController();
  await R.runDiscoveryPass(async () => {
    await R.runDiscovery(async () => {});
    await R.runDiscoveryPass(async () => {}, { signal: controller.signal });
    controller.abort();
    await assert.rejects(R.runDiscovery(async () => {}), /abort/i);
  });
});

for (const registration of ["between segments", "during active segment"] as const) {
  test(`nested pass cancellation aborts pending operation registered ${registration}`, async () => {
    const controller = new AbortController();
    const reason = new Error("nested pass cancelled");
    await R.runDiscoveryPass(async () => {
      await R.runDiscovery(async () => {});
      const operation = async (register: (action: () => Promise<void>) => Promise<void>) => {
        const signal = R.signalIfActive()!;
        let release!: () => void;
        const pending = new Promise<void>(resolve => { release = resolve; });
        signal.addEventListener("abort", release, { once: true });
        try {
          await register(async () => {
            controller.abort(reason);
            assert.equal(signal.aborted, true,
              "nested pass cancellation must abort active discovery operation signal before any resource check");
            assert.equal(signal.reason, reason);
            await pending;
          });
        } finally {
          signal.removeEventListener("abort", release);
          release();
          await pending;
        }
      };
      await assert.rejects(
        registration === "between segments"
          ? R.runDiscoveryPass(() => R.runDiscovery(() => operation(action => action())), { signal: controller.signal })
          : R.runDiscovery(() => operation(action => R.runDiscoveryPass(action, { signal: controller.signal }))),
        error => error === reason,
      );
      await assert.rejects(R.runDiscovery(async () => assert.fail("cancelled sibling ran")), error => error === reason);
    });
  });
}

test("charged production keeps one token and its own counters and timer", async t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let wall = 1_000_000, mono = 0;
  t.mock.method(Date, "now", () => wall);
  t.mock.method(performance, "now", () => mono);
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/discovery-production-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const ledger = new RecoveryMaintenance({ root: path.join(root, "recovery-maintenance"), workerToken: "fixture",
    now: () => wall, monotonicNow: () => mono });
  const input: MaintenanceEligibility = {
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(4096), source: "b".repeat(64),
    exactCustody: true, terminalNonStopped: true, quiescent: true, sourceOutputContinuity: true,
  };
  await R.runDiscoveryPass(async () => {
    await R.runDiscovery(async () => R.consumeIfActive("metadataBytes", caps.metadataBytes));
    wall += 9000; mono += 9000;
    await R.runDiscovery(async () => R.consumeIfActive("metadataBytes", 0));
    R.checkDiscoveryPass();
    const admitted = await ledger.admit(input);
    assert.ok(admitted.ok, JSON.stringify(admitted));
    const token = admitted.value;
    try {
      await R.run(token, () => MaintenanceBudget.runProducer(async () => {
        assert.equal(R.discoveryActive(), false);
        R.consumeIfActive("metadataBytes", caps.metadataBytes);
        await assert.rejects(R.runDiscovery(async () => {}), /maintenance_token_invalid/);
        wall += 20_000; mono += 20_000;
        t.mock.timers.tick(20_000);
        await R.run(token, async () => {
          R.check();
          R.consumeIfActive("metadataBytes", 1);
          assert.equal(R.signalIfActive()?.aborted, false);
        });
      }));
    } finally { assert.ok((await ledger.finish(token, "transient")).ok); }
    const table = JSON.parse(await fs.readFile(path.join(root, "recovery-maintenance", "complete.json"), "utf8")).value;
    assert.equal(table.cycles[0].slots.length, 1);
  });
});

for (const resource of Object.keys(caps) as Array<keyof typeof caps>) {
  test(`terminal caller shares both observations' ${resource} allowance before charge`, async t => {
    const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/discovery-caller-"));
    t.after(() => fs.rm(root, { recursive: true, force: true }));
    const runId = "00000000-0000-4000-8000-000000000031";
    const barePath = path.join(root, "repo.git");
    const genesis = { runId, generation: 1, clonePath: path.join(root, "source"), attemptId: "attempt" };
    const observation: ClosedSourceObservation = {
      genesis, terminal: "normal_failed",
      originalContext: { barePath, branch: "task", key: "key", kind: "issue", defaultBranch: "main",
        cloneUrl: "https://example.com/org/repo.git", repoId: "repo", baseCommit: "c".repeat(40),
        context: { runId, generation: 1, barePath, branch: "task", kind: "issue",
          defaultIdentity: { ref: "refs/remotes/origin/main", sha: "d".repeat(40) } } },
      family: { originals: [{ ...genesis, identity: "1:2" }], observedExecution: { runId, generation: 1 }, metadata: "family" },
    };
    const token = Object.freeze({});
    let observations = 0, captures = 0;
    const client: RecoveryArchiveClient = {
      observeTerminalMaintenanceHold: async () => {
        observations++;
        R.consumeIfActive(resource, caps[resource] / 2 + 1);
        return { ok: true, token, hold: { run_id: runId, generation: 1, hold_id: "hold", repo_id: "repo",
          original_worker_id: "worker", original_worker_identity: "label", live_worker_id: "worker",
          live_run_id: runId, inventory_guarded: true, state: "open", run_status: "failed",
          current_claim_generation: 1, claim_released: true } };
      },
      isCurrentTerminalMaintenanceHold: candidate => candidate === token,
      reserveRecoveryCapture: async () => { throw new Error("unexpected reserve"); },
      getRecoveryCaptureStatus: async () => { throw new Error("unexpected status"); },
      uploadRecoveryBundle: async () => { throw new Error("unexpected upload"); },
      releaseRecoveryCustody: async () => { throw new Error("unexpected FINAL"); },
      listRecoveryHolds: async () => { throw new Error("unexpected advisory list"); },
    };
    const coordinator = new RecoveryCoordinator({
      recoveryRoot: path.join(root, "recovery"), workerToken: "fixture", client, log: nullLogger(),
      git: { matchMaintenanceSourceFamily: async () => ({ sources: observation.family!.originals, metadata: "family" }),
        maintenanceOriginDigest: async () => "b".repeat(64), worktreeHead: async () => "a".repeat(40) } as never,
      withTerminalSourceBoundary: async (_run, _paths, action) => ({ entered: true, value: await action({
        open: () => true, isLivePath: () => false, quiesce: async () => true,
        captureWip: async () => { captures++; throw new Error("unexpected capture"); },
      }) }),
    });
    await coordinator.resumeTerminalSource(runId, Promise.resolve(observation), 1, new AbortController().signal);
    assert.equal(observations, 2);
    assert.equal(captures, 0);
    await assert.rejects(fs.stat(path.join(root, "recovery-maintenance", "complete.json")), { code: "ENOENT" });
  });
}
