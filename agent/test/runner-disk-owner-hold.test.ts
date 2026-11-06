import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { DiskGovernor, DiskParkSignal } from "../src/cache-cap.js";
import { DataVolumeGuard } from "../src/disk-full.js";
import type { RecoveryJournalEntry } from "../src/git.js";
import type { RunContext } from "../src/executor.js";
import type { StateAck, StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { api, client, fx, git, installHarness } from "./runner-harness.js";
import { fixture, FULL, ROOMY, readGit } from "./runner-terminal-disk-deferral-fixture.js";

installHarness();

const RETRY = "2026-11-06T12:00:00Z";
const feed = (id: string) => api.messages(id).filter(m => m.kind === "status").map(m => String(m.payload.text));

// This is only the worker's ACK/scheduling model. SQL and owner Resume are exercised
// separately by LiveDB; this file makes no claim of a Node/PostgreSQL composition.
function parkBudget(max: number) {
  let counted = 0;
  let ownerHeld = false;
  return {
    get counted() { return counted; },
    get held() { return ownerHeld; },
    ack(body: StateRequest): StateAck {
      const preventive = body.disk_park_preventive === true;
      ownerHeld = !preventive && max > 0 && counted >= max;
      if (!preventive && !ownerHeld) counted++;
      return { applied: true, status: "recovery_wait", recoveryRetryNotBefore: ownerHeld ? null : RETRY };
    },
    ownerResume() {
      assert.equal(ownerHeld, true, "only an owner-held run needs this explicit authorization");
      ownerHeld = false; // Resume releases the hold, never the counted budget.
    },
  };
}

function preCloneFixture() {
  const guard = new DataVolumeGuard({
    dataDir: fx.dataDir, statfs: () => FULL, stat: async () => ({ dev: 7 }),
    reclaim: async () => {},
  });
  const f = fixture({
    features: ["recovery_cause_data_volume_full", "claim_generation_fence", "recovery_release_exact_echo"],
    runner: { dataVolume: guard },
  });
  // Like DiskParkClient in runner-disk-full-park.test.ts, prove this generation's
  // empty pre-clone custody release before the runner can send its park report.
  client.releaseRecoveryCustody = async (runId, generation) => {
    assert.equal(runId, f.claim.run_id);
    assert.equal(generation, f.claim.claim_generation);
    return { run_id: runId, released: true, holds_released: 1, generation };
  };
  return f;
}

describe("disk parks wait for their owner after the counted budget", () => {
  for (const retry of [null, undefined, RETRY] as const) {
    it(`pre-clone disk ACK keeps explicit retry semantics: ${String(retry)}`, async () => {
      const f = preCloneFixture();
      const report = client.reportState.bind(client);
      client.reportState = async (id, body, signal) => {
        if (body.status !== "recovery_wait") return report(id, body, signal);
        assert.equal(body.recovery_cause, "data_volume_full");
        assert.equal(body.claim_generation, f.claim.claim_generation);
        await report(id, body, signal);
        return { applied: true, status: "recovery_wait",
          ...(retry !== undefined ? { recoveryRetryNotBefore: retry } : {}) };
      };
      try {
        await f.runner.execute(f.claim);
        assert.equal(f.calls(), 0, "preflight does not admit an executor");
        const text = feed(f.claim.run_id).find(s => s.includes("data volume is full; parked"));
        assert.ok(text);
        if (retry === null) {
          assert.match(text, /waiting for owner Resume or Cancel/);
          assert.doesNotMatch(text, /retry after backoff/);
        } else {
          assert.doesNotMatch(text, /waiting for owner/);
          assert.ok(text.includes(retry ? `retry at ${retry}` : "retry after backoff"));
        }
      } finally { client.reportState = report; }
    });
  }

  it("lost pre-clone disk ACK reconciles a NULL hold without a second park report", async () => {
    const f = preCloneFixture();
    const report = client.reportState.bind(client);
    const ownership = client.getRunOwnership.bind(client);
    let sends = 0;
    client.reportState = async (id, body, signal) => {
      if (body.status !== "recovery_wait") return report(id, body, signal);
      sends++;
      assert.equal(body.recovery_cause, "data_volume_full");
      assert.equal(body.claim_generation, f.claim.claim_generation);
      await report(id, body, signal);
      throw new Error("park committed, reply lost");
    };
    client.getRunOwnership = async id => {
      const own = await ownership(id);
      return sends ? { ...own, status: "recovery_wait", recovery_retry_not_before: null } : own;
    };
    try {
      await f.runner.execute(f.claim);
      assert.equal(sends, 1);
      assert.equal(f.calls(), 0);
      assert.ok(feed(f.claim.run_id).some(text => text.includes("waiting for owner Resume or Cancel")));
      assert.equal(api.states.some(s => s.body.status === "failed"), false);
    } finally { client.reportState = report; client.getRunOwnership = ownership; }
  });

  for (const mode of ["counted", "preventive", "unlimited"] as const) {
    it(`cache rebuild oscillation: ${mode} preserves work and buys no unauthorized attempt`, async () => {
      // Hard-stop cases: 58% non-cache occupancy + 32% cache = 90%, over
      // the governor's 87% hard threshold. Cache alone stays below its 50% cap.
      // The preventive control uses 28% non-cache occupancy and 60% cache.
      // Real fixture bytes are tiny; no host volume is filled or sampled.
      const total = 10_000;
      const cacheBytes = mode === "preventive" ? 6_000 : 3_200;
      const budget = parkBudget(mode === "unlimited" ? 0 : 1);
      const paths: string[] = [];
      const commits: string[] = [];
      const admissions: { journal?: RecoveryJournalEntry; home?: string; inherited?: string[] }[] = [];
      const acks: StateAck[] = [];
      const requests: StateRequest[] = [];
      let homeInode: number | undefined;
      let home = "";
      const cache = () => path.join(home, ".cache", "go-build", "fixture");
      const baseUsage = mode === "preventive" ? 0.28 : 0.58;
      const used = () => baseUsage + (fs.existsSync(cache()) ? fs.statSync(cache()).size : 0) / total;
      const gov = new DiskGovernor({
        config: { capEnabled: true, capFraction: 0.5, lowWater: 0.6, maxConcurrentRuns: 1,
          hardStopEnabled: true, hardMargin: 0.03 },
        log: nullLogger(), volumeTotalBytes: () => total, thresholdOf: () => undefined,
        reclaim: async () => {},
        measure: async () => ({ cacheBytes: fs.existsSync(cache()) ? fs.statSync(cache()).size : 0,
          entries: 1, truncated: false }),
      });
      const journal = (): RecoveryJournalEntry => JSON.parse(readGit(git.barePathFor(fx.originPath),
        "config", "--local", "--get", "uzi-recovery.agent/issue-2201.clone"));
      const observe = (ctx: RunContext) => {
        admissions.push(paths.length ? {
          journal: journal(),
          home: fs.readFileSync(path.join(home, "session"), "utf8"),
          inherited: ["COMMITTED.txt", "DIRTY.txt"].map(name => fs.readFileSync(path.join(ctx.worktreePath, name), "utf8")),
        } : {});
        paths.push(ctx.worktreePath);
      };
      const options = {
        generation: 4,
        sample: ROOMY, error: new DiskParkSignal(mode === "preventive"), observe,
        runner: { diskGovernor: gov },
        afterWork: async (ctx: RunContext) => {
          commits.push(readGit(ctx.worktreePath, "rev-parse", "HEAD"));
          fs.mkdirSync(path.dirname(cache()), { recursive: true });
          fs.writeFileSync(cache(), "x".repeat(cacheBytes));
          if (mode === "preventive") {
            // Three live-process boundaries over cap trigger the preventive park.
            for (let turn = 0; turn < 3; turn++) {
              const decision = await ctx.cacheCapBoundary?.(async () => true);
              if (turn === 2) assert.equal(decision, "park");
            }
          } else {
            assert.ok(cacheBytes < gov.capBytes()!, "hard pressure comes from volume occupancy, not the per-run cap");
            assert.equal(await ctx.cacheCapBoundary?.(async () => false), "continue");
            assert.ok(used() >= gov.effectiveHardThreshold());
            // Event-driven pressure stop; timeout bounds a missing stop and is always cleared.
            let timer: NodeJS.Timeout | undefined;
            try {
              await new Promise<void>((resolve, reject) => {
                timer = setTimeout(() => reject(new Error("hard stop did not arrive")), 2000);
                if (ctx.signal?.aborted) return resolve();
                ctx.signal?.addEventListener("abort", () => resolve(), { once: true });
                gov.observe(used());
              });
            } finally { clearTimeout(timer); }
            assert.equal(ctx.pauseModeRequested?.(), "disk");
          }
        },
      };
      const f = fixture(options);
      home = f.home;
      const report = client.reportState.bind(client);
      client.reportState = async (id, body, signal) => {
        if (body.status !== "recovery_wait") return report(id, body, signal);
        await report(id, body, signal);
        const ack = budget.ack(body);
        requests.push(body);
        acks.push(ack);
        api.setOwnershipStatus(id, "recovery_wait", f.claim.claim_generation);
        return ack;
      };
      try {
        // Fresh claims are supplied by this scheduler model only for timed retries,
        // or after the explicit owner authorization below.
        for (let leg = 0; leg < 3; leg++) {
          if (leg === 2 && mode === "counted") {
            assert.equal(budget.held, true);
            const heldJournal = journal();
            const heldPaths = [...paths];
            const heldHome = fs.statSync(home).ino;
            const cloneInodes = heldPaths.map(p => fs.statSync(p).ino);
            const siblings = fs.readdirSync(path.dirname(f.clone())).sort();
            // Capacity can oscillate with no claim. Neither the runner nor governor
            // may use that relief to create a successor or replay the executor.
            for (let tick = 0; tick < 3; tick++) {
              gov.observe(0.58);
              gov.observe(0.90);
            }
            await new Promise<void>(resolve => setImmediate(resolve));
            assert.equal(f.calls(), 2);
            assert.deepEqual(paths, heldPaths);
            assert.deepEqual(heldPaths.map(p => fs.statSync(p).ino), cloneInodes);
            assert.deepEqual(fs.readdirSync(path.dirname(f.clone())).sort(), siblings, "capacity alone creates no successor directory");
            assert.deepEqual(journal(), heldJournal);
            assert.equal(fs.statSync(home).ino, heldHome);
            assert.equal(budget.counted, 1);
            assert.match(feed(f.claim.run_id).at(-1)!, /owner Resume or Cancel/);
            assert.doesNotMatch(feed(f.claim.run_id).at(-1)!, /resumes automatically/);
            budget.ownerResume();
            assert.equal(budget.counted, 1, "owner Resume does not reset the lifetime count");
          }
          options.generation = 4 + leg;
          f.claim.claim_generation = options.generation;
          f.setRoomy();
          api.setOwnershipStatus(f.claim.run_id, "claimed", f.claim.claim_generation);
          await f.runner.execute(f.claim);
          assert.equal(f.calls(), leg + 1);
          assert.equal(acks.length, leg + 1);
          assert.equal(fs.existsSync(cache()), false, "park drops the real cache fixture");
          assert.ok(used() < 0.60, "dropping cache restores usage below the soft level");
          assert.equal(fs.readFileSync(path.join(home, "session"), "utf8"), "transcript");
          homeInode ??= fs.statSync(home).ino;
          assert.equal(fs.statSync(home).ino, homeInode, "HOME identity survives the held stop and owner continuation");
          const parked = journal();
          assert.equal(parked.clonePath, paths[leg]);
          assert.equal(parked.runId, f.claim.run_id);
          assert.deepEqual(parked.retainedSources?.map(s => s.clonePath).sort() ?? [], paths.slice(0, leg).sort());
          if (leg) {
            assert.deepEqual(admissions[leg]!.inherited, ["committed sentinel\n", "dirty sentinel\n"]);
            assert.equal(admissions[leg]!.home, "transcript");
            assert.equal(admissions[leg]!.journal?.recovery?.attempts, leg);
            assert.equal(parked.recovery?.attempts, leg, "owner continuation keeps the retained recovery budget");
            assert.equal(parked.recovery?.source.clonePath, paths[0]);
          }
          for (let i = 0; i <= leg; i++) {
            assert.equal(fs.readFileSync(path.join(paths[i]!, "COMMITTED.txt"), "utf8"), "committed sentinel\n");
            assert.equal(fs.readFileSync(path.join(paths[i]!, "DIRTY.txt"), "utf8"), "dirty sentinel\n");
            assert.equal(readGit(paths[i]!, "show", `${commits[i]}:COMMITTED.txt`), "committed sentinel");
          }
        }
        assert.equal(new Set(paths).size, 3, "authorized generations have distinct clone identities");
        assert.deepEqual(acks.map(a => a.recoveryRetryNotBefore),
          mode === "counted" ? [RETRY, null, null] : [RETRY, RETRY, RETRY]);
        assert.equal(budget.counted, mode === "preventive" ? 0 : mode === "unlimited" ? 3 : 1);
        assert.ok(requests.every(b => b.recovery_cause === "data_volume_full"));
        assert.ok(requests.every(b => (b.disk_park_preventive === true) === (mode === "preventive")));
        assert.equal(api.states.some(s => ["failed", "completed"].includes(s.body.status ?? "")), false);
        assert.equal(f.forge.calls.length, 0, "no finalize or MR");
      } finally { client.reportState = report; }
    });
  }

  for (const { verified, retry } of [
    { verified: true, retry: null }, { verified: false, retry: null },
    { verified: true, retry: undefined }, { verified: true, retry: RETRY },
  ]) {
    it(`mid-run retry=${String(retry)} retains ${verified ? "captured" : "degraded"} work after lost/statusless replies`, async () => {
      const marker = git.commitWipMarker.bind(git);
      let captures = 0;
      git.commitWipMarker = async (...args) => { captures++; return verified ? marker(...args) : false; };
      const f = fixture({ error: new DiskParkSignal(false) });
      const report = client.reportState.bind(client);
      let sends = 0;
      client.reportState = async (id, body, signal) => {
        if (body.status !== "recovery_wait") return report(id, body, signal);
        sends++;
        if (sends === 1) throw new Error("lost reply");
        if (sends === 2) return { applied: false };
        await report(id, body, signal);
        return { applied: true, status: "recovery_wait",
          ...(retry !== undefined ? { recoveryRetryNotBefore: retry } : {}) };
      };
      try {
        await f.runner.execute(f.claim);
        assert.equal(sends, 3);
        assert.equal(captures, verified ? 1 : 3, "reply uncertainty never grants a fresh capture budget");
        assert.equal(f.calls(), 1);
        assert.equal(fs.readFileSync(path.join(f.clone(), "COMMITTED.txt"), "utf8"), "committed sentinel\n");
        assert.equal(fs.readFileSync(path.join(f.clone(), "DIRTY.txt"), "utf8"), "dirty sentinel\n");
        assert.equal(fs.readFileSync(path.join(f.home, "session"), "utf8"), "transcript");
        const text = feed(f.claim.run_id).at(-1)!;
        assert.equal(text.includes("owner Resume or Cancel"), retry === null);
        assert.equal(text.includes("unverified work and session"), !verified);
        assert.equal(text.includes("resumes automatically"), retry !== null);
        assert.equal(api.states.some(s => s.body.status === "failed"), false);
      } finally { client.reportState = report; git.commitWipMarker = marker; }
    });
  }
});
