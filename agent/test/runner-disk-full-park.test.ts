import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { RequestError, WorkerClient } from "../src/client.js";
import { DataVolumeGuard } from "../src/disk-full.js";
import type { StatfsSample } from "../src/stats.js";
import type { RecoveryReleaseResponse, StateAck, StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { TOKEN, api, baseUrl, fakeGitlab, fx, git, gitlabClaim, installHarness } from "./runner-harness.js";

installHarness();

// PRD #1809 M5 (D6): a clone/fetch that fails because the worker's data volume is full runs the
// D7 reclaim, retries once, and, still full, parks the run in recovery_wait with the typed
// data_volume_full cause instead of failing it (the #1798 incident: a resume's first fetch hit
// ENOSPC and the run failed). Seams only, never a real full disk: `git.ensureClone` is overridden
// to throw git's own diagnostic, statfs is a fake, and the reclaim is a counter.

const SESSION_ID = "22222222-2222-2222-2222-222222221809";
const FEATURE = "recovery_cause_data_volume_full";
/** Stamping claim_generation on /state needs the fence feature (or the switch capability). */
const FENCE = "claim_generation_fence";

const GIT_ENOSPC = () =>
  new Error(
    "git fetch --prune origin failed: error: unable to write file ./objects/pack/tmp_pack_x\n" +
      "fatal: unable to write loose object file: No space left on device",
  );

const GIB = 1024 ** 3;
/** A 25 GiB volume with `availBytes` free and `ffree` of 1.6M inodes free. */
function volume(availBytes: number, ffree = 1_500_000): StatfsSample {
  const bsize = 4096;
  return {
    bsize,
    blocks: (25 * GIB) / bsize,
    bfree: availBytes / bsize,
    bavail: availBytes / bsize,
    files: 1_600_000,
    ffree,
  };
}
const FULL = volume(8 * 1024 * 1024); // 8 MiB free: below the 256 MiB floor
const ROOMY = volume(10 * GIB);

/** Proves an exact-generation custody release; every api with the typed cause advertises it. */
const ECHO = "recovery_release_exact_echo";

interface DiskParkHooks {
  /** The ack (or throw) for the recovery_wait park report. Default: a fresh 200 park. */
  onParkReport?: (body: StateRequest) => StateAck;
  /** The release response (or throw). Default: an exact release of the named generation. */
  onRelease?: (generation?: number) => RecoveryReleaseResponse;
}

class DiskParkClient extends WorkerClient {
  readonly reported: StateRequest[] = [];
  readonly releaseCalls: Array<{ generation?: number; evidence?: string }> = [];
  /** Release and park calls in the order they reached the api. */
  readonly order: string[] = [];

  constructor(
    features: string[],
    private readonly hooks: DiskParkHooks = {},
  ) {
    super(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
    this.protocolFeatures = features;
  }

  override async reportState(_runId: string, body: StateRequest): Promise<StateAck> {
    this.reported.push({ ...body });
    if (body.status === "recovery_wait") {
      this.order.push("park");
      if (this.hooks.onParkReport) return this.hooks.onParkReport(body);
      return { applied: true, status: "recovery_wait", recoveryRetryNotBefore: "2026-09-27T21:00:00Z" };
    }
    return { applied: body.status !== undefined, status: body.status };
  }

  override async releaseRecoveryCustody(
    _runId: string,
    generation?: number,
    releaseEvidence?: string,
  ): Promise<RecoveryReleaseResponse> {
    this.releaseCalls.push({ generation, evidence: releaseEvidence });
    this.order.push("release");
    if (this.hooks.onRelease) return this.hooks.onRelease(generation);
    return { run_id: "x", released: true, holds_released: 1, generation };
  }
}

/** Materializes the run HOME when the factory runs (a pre-clone park never reaches run()); on a
 *  resume leg it also writes the resumable transcript and a Go build cache beside it. */
function homeFactory(homeRoot: string, opts: { resume?: boolean } = {}): { factory: ExecutorFactory; runHome: () => string } {
  let runHome = "";
  const factory: ExecutorFactory = (runId) => {
    runHome = path.join(homeRoot, runId);
    fs.mkdirSync(runHome, { recursive: true });
    if (opts.resume) {
      const proj = path.join(runHome, ".claude", "projects", "proj");
      fs.mkdirSync(proj, { recursive: true });
      fs.writeFileSync(path.join(proj, `${SESSION_ID}.jsonl`), "transcript", "utf8");
      fs.mkdirSync(path.join(runHome, ".cache", "go-build", "0a"), { recursive: true });
      fs.writeFileSync(path.join(runHome, ".cache", "go-build", "0a", "obj"), "x".repeat(4096));
    }
    return {
      homeDir: runHome,
      executor: {
        run: async () => {
          throw new Error("executor.run() must never be reached on a pre-clone park");
        },
      },
    };
  };
  return { factory, runHome: () => runHome };
}

interface GuardProbe {
  guard: DataVolumeGuard;
  reclaims: () => number;
}

function guardWith(
  statfs: (p: string) => StatfsSample,
  opts: {
    stat?: (p: string) => Promise<{ dev: number }>;
    onReclaim?: () => void;
    /** Replaces the reclaim pass's completion (e.g. a pass that never finishes). */
    reclaimPass?: () => Promise<void>;
  } = {},
): GuardProbe {
  let reclaims = 0;
  const guard = new DataVolumeGuard({
    dataDir: fx.dataDir,
    statfs,
    ...(opts.stat ? { stat: opts.stat } : {}),
    reclaim: async () => {
      reclaims += 1;
      opts.onReclaim?.();
      if (opts.reclaimPass) await opts.reclaimPass();
    },
  });
  return { guard, reclaims: () => reclaims };
}

function makeRunner(
  client: WorkerClient,
  factory: ExecutorFactory,
  dataVolume?: DataVolumeGuard,
  opts: { dataVolumeReclaimWaitMs?: number } = {},
): RunRunner {
  const { gitlab } = fakeGitlab();
  return new RunRunner(client, git, factory, nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    gitlab,
    recoveryRetryMs: 5,
    ...(dataVolume ? { dataVolume } : {}),
    ...opts,
  });
}

const statuses = (c: DiskParkClient) => c.reported.map((b) => String(b.status));
const park = (c: DiskParkClient) => c.reported.find((b) => b.status === "recovery_wait");
const feed = (runId: string) =>
  api
    .messages(runId)
    .filter((m) => m.kind === "status")
    .map((m) => String(m.payload.text));

async function withHome(prefix: string, fn: (homeRoot: string) => Promise<void>): Promise<void> {
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  try {
    await fn(homeRoot);
  } finally {
    fs.rmSync(homeRoot, { recursive: true, force: true });
  }
}

/** ensureClone that always throws git's ENOSPC diagnostic, counting its calls. */
function failingClone(): () => number {
  let calls = 0;
  git.ensureClone = async () => {
    calls += 1;
    throw GIT_ENOSPC();
  };
  return () => calls;
}

describe("RunRunner — data_volume_full park at clone/fetch (PRD #1809 M5, D6)", () => {
  it("REGRESSION #1798: a resume whose first fetch hits ENOSPC on a full data volume parks recovery_wait/data_volume_full instead of failing", async () => {
    await withHome("uzi-diskpark-resume-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO, "recovery_park_cause"]);
      const { factory, runHome } = homeFactory(homeRoot, { resume: true });
      const calls = failingClone();
      // The claim preflight (the first sample) still sees room; the fetch then fills the volume,
      // so this exercises the typed handling around the fetch, not the preflight.
      let samples = 0;
      const probe = guardWith(() => (samples++ === 0 ? ROOMY : FULL));
      const claim = gitlabClaim(1801, { claim_generation: 4, session_id: SESSION_ID });
      await makeRunner(client, factory, probe.guard).execute(claim);

      const body = park(client);
      assert.ok(body, "the run must be parked, not failed");
      assert.strictEqual(body!.recovery_cause, "data_volume_full");
      assert.strictEqual(body!.claim_generation, 4, "the park is claim-fenced");
      assert.strictEqual(body!.disk_park_preventive, undefined, "a disk-full park is counted (preventive absent)");
      assert.ok(!statuses(client).includes("failed"), "a parked run reports no failure");
      assert.strictEqual(probe.reclaims(), 1, "exactly one reclaim pass before the retry");
      assert.strictEqual(calls(), 2, "the clone/fetch is retried exactly once");
      const texts = feed(claim.run_id).filter((t) => /data volume is full; parked/.test(t));
      assert.strictEqual(texts.length, 1, "exactly one park feed event");
      assert.match(texts[0]!, /retry at 2026-09-27T21:00:00Z/);
      // flight.parked on a resume leg: the HOME (and its transcript) is kept for the resume, and
      // M1's park cache drop removed the rebuildable Go build cache.
      assert.ok(fs.existsSync(path.join(runHome(), ".claude", "projects", "proj", `${SESSION_ID}.jsonl`)), "transcript kept");
      assert.strictEqual(fs.existsSync(path.join(runHome(), ".cache", "go-build")), false, "caches dropped per M1");
    });
  });

  it("the claim preflight parks a run whose data volume stays full after a reclaim, before any clone", async () => {
    await withHome("uzi-diskpark-preflight-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO]);
      const { factory } = homeFactory(homeRoot);
      const calls = failingClone();
      const probe = guardWith(() => FULL);
      await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1802, { claim_generation: 2 }));

      assert.strictEqual(park(client)?.recovery_cause, "data_volume_full");
      assert.strictEqual(calls(), 0, "the preflight parks before the clone/fetch writes");
      assert.strictEqual(probe.reclaims(), 1);
    });
  });

  it("reclaim-then-retry succeeds: no park, the clone proceeds", async () => {
    await withHome("uzi-diskpark-retry-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO]);
      const { factory } = homeFactory(homeRoot);
      const original = git.ensureClone.bind(git);
      let calls = 0;
      let full = false; // the preflight sees room; the first fetch then fills the volume
      git.ensureClone = async (...args: Parameters<typeof git.ensureClone>) => {
        calls += 1;
        if (calls === 1) {
          full = true;
          throw GIT_ENOSPC();
        }
        return original(...args);
      };
      const probe = guardWith(() => (full ? FULL : ROOMY), {
        onReclaim: () => {
          full = false;
        },
      });
      const claim = gitlabClaim(1803, { claim_generation: 3 });
      await makeRunner(client, factory, probe.guard).execute(claim);

      assert.strictEqual(calls, 2, "retried once after the reclaim");
      assert.strictEqual(probe.reclaims(), 1);
      assert.strictEqual(park(client), undefined, "a successful retry never parks");
      assert.ok(
        feed(claim.run_id).some((t) => /runner clone ready/.test(t)),
        "the run went on past the clone",
      );
    });
  });

  it("ENOSPC on another mount (destination not on the data volume): today's handling, no reclaim, no park", async () => {
    await withHome("uzi-diskpark-othermount-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO]);
      const { factory } = homeFactory(homeRoot);
      const calls = failingClone();
      const dataDir = path.resolve(fx.dataDir);
      // The data dir is device 1; the bare's path (under dataDir/repos) reports device 2, as a
      // separate mount under the data dir would.
      const stat = async (p: string) => ({ dev: path.resolve(p) === dataDir ? 1 : 2 });
      // Roomy at the preflight (the first sample), full after it, so the test isolates the
      // destination attribution of the failure.
      let samples = 0;
      const probe = guardWith(() => (samples++ === 0 ? ROOMY : FULL), { stat });
      await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1804, { claim_generation: 5 }));

      assert.strictEqual(park(client), undefined);
      assert.ok(statuses(client).includes("failed"), "today's handling: the permanent git error fails the run");
      assert.strictEqual(probe.reclaims(), 0);
      assert.strictEqual(calls(), 1, "no retry");
    });
  });

  it("a statfs failure is unknown: today's handling", async () => {
    await withHome("uzi-diskpark-statfs-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO]);
      const { factory } = homeFactory(homeRoot);
      failingClone();
      const probe = guardWith(() => {
        throw Object.assign(new Error("statfs failed"), { code: "EIO" });
      });
      await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1805, { claim_generation: 5 }));

      assert.strictEqual(park(client), undefined);
      assert.ok(statuses(client).includes("failed"));
      assert.strictEqual(probe.reclaims(), 0);
    });
  });

  it("inode exhaustion (bytes fine, ffree ~0) classifies as full and parks", async () => {
    await withHome("uzi-diskpark-inodes-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO]);
      const { factory } = homeFactory(homeRoot);
      const calls = failingClone();
      // Roomy at the preflight; then bytes stay fine while the inodes run out.
      let samples = 0;
      const probe = guardWith(() => (samples++ === 0 ? ROOMY : volume(10 * GIB, 3)));
      await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1806, { claim_generation: 6 }));

      assert.strictEqual(park(client)?.recovery_cause, "data_volume_full");
      assert.ok(!statuses(client).includes("failed"));
      assert.strictEqual(calls(), 2, "classified at the fetch, retried once after the reclaim");
    });
  });

  it("an older api (feature absent) gets the untyped recovery_wait, never the data_volume_full cause", async () => {
    await withHome("uzi-diskpark-oldapi-", async (homeRoot) => {
      const client = new DiskParkClient([FENCE, ECHO, "recovery_park_cause"]);
      const { factory } = homeFactory(homeRoot);
      const calls = failingClone();
      const probe = guardWith(() => FULL);
      await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1807, { claim_generation: 7 }));

      const body = park(client);
      assert.ok(body, "still parked");
      assert.strictEqual(body!.recovery_cause, undefined, "no data_volume_full cause to an older api");
      assert.strictEqual(body!.disk_park_preventive, undefined);
      assert.ok(!client.reported.some((b) => b.recovery_cause === "data_volume_full"));
      assert.strictEqual(calls(), 2, "no preflight against an older api; the typed retry still runs");
    });
  });

  it("a claim without a claim generation (a chat-shaped claim) is never parked with this cause", async () => {
    await withHome("uzi-diskpark-nogen-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO]);
      const { factory } = homeFactory(homeRoot);
      failingClone();
      const probe = guardWith(() => FULL);
      await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1808, { claim_generation: 0 }));

      assert.strictEqual(park(client), undefined, "the api 400s this park without a generation");
      assert.ok(!client.reported.some((b) => b.recovery_cause === "data_volume_full"));
      assert.ok(statuses(client).includes("failed"), "today's handling");
    });
  });

  it("no guard wired: a disk-full clone fails exactly as before", async () => {
    await withHome("uzi-diskpark-noguard-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO]);
      const { factory } = homeFactory(homeRoot);
      const calls = failingClone();
      await makeRunner(client, factory).execute(gitlabClaim(1809, { claim_generation: 9 }));

      assert.strictEqual(park(client), undefined);
      assert.ok(statuses(client).includes("failed"));
      assert.strictEqual(calls(), 1);
    });
  });
});

describe("RunRunner — the data_volume_full park releases the pre-clone custody hold (PRD #1809 M5b B1)", () => {
  // ClaimRun opens a custody hold for every claim. The api's data_volume_full park settles none
  // (a mid-run disk park keeps custody), and nothing later releases a pre-clone generation's hold,
  // so the worker releases it itself, exactly like the forge pre-clone park's older-api fallback.

  it("typed: releases THIS generation's hold with positive proof BEFORE the park report", async () => {
    await withHome("uzi-diskpark-rel-typed-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO, "recovery_park_cause"]);
      const { factory } = homeFactory(homeRoot);
      failingClone();
      let samples = 0;
      const probe = guardWith(() => (samples++ === 0 ? ROOMY : FULL));
      await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1811, { claim_generation: 11 }));

      assert.deepStrictEqual(client.releaseCalls, [{ generation: 11, evidence: undefined }], "one exact release of generation 11, no evidence field");
      assert.deepStrictEqual(client.order, ["release", "park"], "the hold is released before the park is reported");
      assert.strictEqual(park(client)?.recovery_cause, "data_volume_full");
      assert.ok(!statuses(client).includes("failed"));
    });
  });

  it("the preflight park releases the hold too", async () => {
    await withHome("uzi-diskpark-rel-pre-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO]);
      const { factory } = homeFactory(homeRoot);
      const calls = failingClone();
      const probe = guardWith(() => FULL);
      await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1812, { claim_generation: 12 }));

      assert.strictEqual(calls(), 0);
      assert.deepStrictEqual(client.releaseCalls.map((c) => c.generation), [12]);
      assert.deepStrictEqual(client.order, ["release", "park"]);
    });
  });

  it("untyped (older api): releases with proof, then sends the untyped park", async () => {
    await withHome("uzi-diskpark-rel-untyped-", async (homeRoot) => {
      const client = new DiskParkClient([FENCE, ECHO]);
      const { factory } = homeFactory(homeRoot);
      failingClone();
      const probe = guardWith(() => FULL);
      await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1813, { claim_generation: 13 }));

      assert.deepStrictEqual(client.releaseCalls.map((c) => c.generation), [13]);
      assert.deepStrictEqual(client.order, ["release", "park"]);
      assert.strictEqual(park(client)?.recovery_cause, undefined);
    });
  });

  for (const [label, rel] of [
    ["no generation echo", { run_id: "x", released: true, holds_released: 1 }],
    ["nothing released", { run_id: "x", released: false, holds_released: 0 }],
    ["retained", { run_id: "x", released: false, holds_released: 0, retained: true, reason: "ambiguous" }],
    ["another generation echoed", { run_id: "x", released: true, holds_released: 1, generation: 99 }],
  ] as Array<[string, RecoveryReleaseResponse]>) {
    it(`a release without positive proof (${label}) never parks: today's failed path`, async () => {
      await withHome("uzi-diskpark-rel-noproof-", async (homeRoot) => {
        const client = new DiskParkClient([FEATURE, FENCE, ECHO], { onRelease: () => rel });
        const { factory } = homeFactory(homeRoot);
        failingClone();
        const probe = guardWith(() => FULL);
        await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1814, { claim_generation: 14 }));

        assert.strictEqual(client.releaseCalls.length, 1);
        assert.strictEqual(park(client), undefined, "no park without proof");
        assert.ok(statuses(client).includes("failed"));
      });
    });
  }

  it("a release call that throws never parks: today's failed path", async () => {
    await withHome("uzi-diskpark-rel-throw-", async (homeRoot) => {
      const client = new DiskParkClient([FENCE, ECHO], {
        onRelease: () => {
          throw new RequestError("POST", "/archives/release", 503, "unavailable");
        },
      });
      const { factory } = homeFactory(homeRoot);
      failingClone();
      const probe = guardWith(() => FULL);
      await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1815, { claim_generation: 15 }));

      assert.strictEqual(park(client), undefined);
      assert.ok(statuses(client).includes("failed"));
    });
  });

  it("an api without the exact-release echo: no release, no park, today's failed path", async () => {
    await withHome("uzi-diskpark-rel-noecho-", async (homeRoot) => {
      const client = new DiskParkClient([FENCE]);
      const { factory } = homeFactory(homeRoot);
      failingClone();
      const probe = guardWith(() => FULL);
      await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1816, { claim_generation: 16 }));

      assert.strictEqual(client.releaseCalls.length, 0);
      assert.strictEqual(park(client), undefined);
      assert.ok(statuses(client).includes("failed"));
    });
  });
});

describe("RunRunner — the data_volume_full park's other exits (PRD #1809 M5b N5)", () => {
  it("a typed park the api rejects 400 takes today's failed path", async () => {
    await withHome("uzi-diskpark-400-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO], {
        onParkReport: () => {
          throw new RequestError("POST", "/state", 400, "invalid state");
        },
      });
      const { factory } = homeFactory(homeRoot);
      failingClone();
      const probe = guardWith(() => FULL);
      const claim = gitlabClaim(1821, { claim_generation: 21 });
      await makeRunner(client, factory, probe.guard).execute(claim);

      assert.strictEqual(park(client)?.recovery_cause, "data_volume_full", "the typed park was sent once");
      assert.ok(statuses(client).includes("failed"), "a 400 park falls through to the failed report");
      assert.ok(!feed(claim.run_id).some((t) => /data volume is full; parked/.test(t)), "no park feed event");
    });
  });

  it("a stale_claim ack stops silently: no failed report, no park event", async () => {
    await withHome("uzi-diskpark-stale-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO], {
        onParkReport: () => ({ applied: false, status: "running", reason: "stale_claim" }),
      });
      const { factory } = homeFactory(homeRoot);
      failingClone();
      const probe = guardWith(() => FULL);
      const claim = gitlabClaim(1822, { claim_generation: 22 });
      await makeRunner(client, factory, probe.guard).execute(claim);

      assert.ok(!statuses(client).includes("failed"), "a stale claim reports nothing further");
      assert.strictEqual(client.reported.filter((b) => b.status === "recovery_wait").length, 1, "no re-report");
      assert.ok(!feed(claim.run_id).some((t) => /data volume is full; parked/.test(t)));
    });
  });

  for (const status of ["cancelled", "failed"] as const) {
    it(`an authoritative terminal ack (${status}) stops without a park or a second report`, async () => {
      await withHome("uzi-diskpark-terminal-", async (homeRoot) => {
        const client = new DiskParkClient([FEATURE, FENCE, ECHO], {
          onParkReport: () => ({ applied: status === "cancelled", status }),
        });
        const { factory } = homeFactory(homeRoot);
        failingClone();
        const probe = guardWith(() => FULL);
        const claim = gitlabClaim(1823, { claim_generation: 23 });
        await makeRunner(client, factory, probe.guard).execute(claim);

        assert.ok(!statuses(client).includes("failed"), "no failed report after a terminal ack");
        assert.strictEqual(client.reported.filter((b) => b.status === "recovery_wait").length, 1);
        assert.ok(!feed(claim.run_id).some((t) => /data volume is full; parked/.test(t)));
      });
    });
  }
});

describe("RunRunner — the data-volume reclaim wait is bounded and abortable (PRD #1809 M5b N3)", () => {
  /** A reclaim pass that never finishes (a stuck in-flight pass plus a fresh one). */
  const stuckPass = () => new Promise<void>(() => {});

  it("a pass that outlasts the bound: the retry runs anyway, then the run parks", async () => {
    await withHome("uzi-diskpark-wait-timeout-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO]);
      const { factory } = homeFactory(homeRoot);
      const calls = failingClone();
      let samples = 0;
      const probe = guardWith(() => (samples++ === 0 ? ROOMY : FULL), { reclaimPass: stuckPass });
      const started = Date.now();
      await makeRunner(client, factory, probe.guard, { dataVolumeReclaimWaitMs: 50 }).execute(
        gitlabClaim(1831, { claim_generation: 31 }),
      );

      assert.ok(Date.now() - started < 30_000, "the run did not wait on the stuck pass");
      assert.strictEqual(calls(), 2, "the retry still runs after the bound");
      assert.strictEqual(park(client)?.recovery_cause, "data_volume_full");
    });
  });

  it("a cancel during the wait parks at once on the full verdict, without the retry", async () => {
    await withHome("uzi-diskpark-wait-cancel-", async (homeRoot) => {
      const claim = gitlabClaim(1832, { claim_generation: 32 });
      // The api's stamped stop turns the park into a cancel (e02e83fa); model that ack.
      const client = new DiskParkClient([FEATURE, FENCE, ECHO], {
        onParkReport: () => ({ applied: true, status: "cancelled" }),
      });
      const { factory } = homeFactory(homeRoot);
      const calls = failingClone();
      let samples = 0;
      const probe = guardWith(() => (samples++ === 0 ? ROOMY : FULL), {
        onReclaim: () => api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]),
        reclaimPass: stuckPass,
      });
      const started = Date.now();
      // The default 2-minute bound stays in force: only the cancel can end the wait in time.
      await makeRunner(client, factory, probe.guard).execute(claim);

      assert.ok(Date.now() - started < 60_000, "the cancel ended the wait");
      assert.strictEqual(calls(), 1, "no retry after a cancel");
      assert.deepStrictEqual(client.order, ["release", "park"], "the claim-fenced park still settles the hold");
      assert.ok(!statuses(client).includes("failed"));
    });
  });

  it("a worker shutdown during the wait parks at once, without the retry", async () => {
    await withHome("uzi-diskpark-wait-shutdown-", async (homeRoot) => {
      const client = new DiskParkClient([FEATURE, FENCE, ECHO]);
      const { factory } = homeFactory(homeRoot);
      const calls = failingClone();
      let runner: RunRunner | undefined;
      const probe = guardWith(() => FULL, {
        onReclaim: () => setTimeout(() => runner!.shutdown(), 20),
        reclaimPass: stuckPass,
      });
      runner = makeRunner(client, factory, probe.guard);
      const started = Date.now();
      await runner.execute(gitlabClaim(1833, { claim_generation: 33 }));

      assert.ok(Date.now() - started < 60_000, "the shutdown ended the wait");
      assert.strictEqual(calls(), 0, "the preflight parked before any clone");
      assert.strictEqual(probe.reclaims(), 1);
      assert.strictEqual(park(client)?.recovery_cause, "data_volume_full");
    });
  });
});

describe("RunRunner — the clone/fetch is classified with the pre-op sample (PRD #1809 M5b N1)", () => {
  it("a volume full before the write but roomy after git cleaned up still classifies full", async () => {
    await withHome("uzi-diskpark-preop-", async (homeRoot) => {
      // An older api: no preflight, so the samples are exactly: pre-op, post-failure, pre-op
      // (retry), post-failure (retry). Each pre-op sample is full, each post-failure one roomy.
      const client = new DiskParkClient([FENCE, ECHO]);
      const { factory } = homeFactory(homeRoot);
      const calls = failingClone();
      let samples = 0;
      const probe = guardWith(() => (samples++ % 2 === 0 ? FULL : ROOMY));
      await makeRunner(client, factory, probe.guard).execute(gitlabClaim(1841, { claim_generation: 41 }));

      assert.strictEqual(calls(), 2, "classified full, reclaimed, retried once");
      assert.ok(park(client), "parked on the min(pre, post) verdict");
    });
  });
});
