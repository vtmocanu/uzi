import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { WorkerClient } from "../src/client.js";
import { DataVolumeGuard } from "../src/disk-full.js";
import type { StatfsSample } from "../src/stats.js";
import type { StateAck, StateRequest } from "../src/protocol.js";
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

class DiskParkClient extends WorkerClient {
  readonly reported: StateRequest[] = [];

  constructor(features: string[]) {
    super(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
    this.protocolFeatures = features;
  }

  override async reportState(_runId: string, body: StateRequest): Promise<StateAck> {
    this.reported.push({ ...body });
    if (body.status === "recovery_wait") {
      return { applied: true, status: "recovery_wait", recoveryRetryNotBefore: "2026-09-27T21:00:00Z" };
    }
    return { applied: body.status !== undefined, status: body.status };
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
  opts: { stat?: (p: string) => Promise<{ dev: number }>; onReclaim?: () => void } = {},
): GuardProbe {
  let reclaims = 0;
  const guard = new DataVolumeGuard({
    dataDir: fx.dataDir,
    statfs,
    ...(opts.stat ? { stat: opts.stat } : {}),
    reclaim: async () => {
      reclaims += 1;
      opts.onReclaim?.();
    },
  });
  return { guard, reclaims: () => reclaims };
}

function makeRunner(client: WorkerClient, factory: ExecutorFactory, dataVolume?: DataVolumeGuard): RunRunner {
  const { gitlab } = fakeGitlab();
  return new RunRunner(client, git, factory, nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    gitlab,
    recoveryRetryMs: 5,
    ...(dataVolume ? { dataVolume } : {}),
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
      const client = new DiskParkClient([FEATURE, FENCE, "recovery_park_cause"]);
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
      const client = new DiskParkClient([FEATURE, FENCE]);
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
      const client = new DiskParkClient([FEATURE, FENCE]);
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
      const client = new DiskParkClient([FEATURE, FENCE]);
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
      const client = new DiskParkClient([FEATURE, FENCE]);
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
      const client = new DiskParkClient([FEATURE, FENCE]);
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
      const client = new DiskParkClient([FENCE, "recovery_park_cause"]);
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
      const client = new DiskParkClient([FEATURE, FENCE]);
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
      const client = new DiskParkClient([FEATURE, FENCE]);
      const { factory } = homeFactory(homeRoot);
      const calls = failingClone();
      await makeRunner(client, factory).execute(gitlabClaim(1809, { claim_generation: 9 }));

      assert.strictEqual(park(client), undefined);
      assert.ok(statuses(client).includes("failed"));
      assert.strictEqual(calls(), 1);
    });
  });
});
