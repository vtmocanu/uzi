import { it, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";
import { codexRig } from "./runner-codex-sinks-fixture.js";
import { observe } from "./runner-codex-capture-proof-fixture.js";
import type { ExecutorFactory } from "../src/runner.js";
import { noProofReseed, nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";
installHarness();
import { heldServer, JOIN_TOKEN } from "./runner-held-publication-fixture.js";
function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_"))), GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}
async function seed(iid = 2512) {
  const claim = gitlabClaim(iid, { claim_generation: 2, session_id: "predecessor-session" });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", 2);
  return { claim, bare, clone };
}
function readJournal(bare: string, branch: string): any {
  return JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
}
let callbackFailures: unknown[] = [];
afterEach(() => {
  const failures = callbackFailures;
  callbackFailures = [];
  assert.deepEqual(failures, [], "runner must not swallow model callback assertions");
});
for (const stage of ["newcapture", "captured", "adopting", "missing-capability"] as string[]) {
  it(`A guarded retained public scope proof: ${stage}`, async t => {
    const { claim, bare, clone } = await seed();
    claim.inventory_guarded = true;
    claim.repo.id = "00000000-0000-4000-8000-000000000072";
    const features = ["recovery_inventory_v1", "claim_generation_fence", "recovery_held_publication_v2"];
    api.setRegisterProtocolFeatures(features);
    const registration = await client.register("guarded-retained-proof", undefined, undefined, undefined, ["recovery_held_publication_v2"]);
    const server = heldServer(claim, features);
    const holdId = "00000000-0000-4000-8000-000000000071";
    client.listRecoveryHolds = async () => ({run_id:claim.run_id, holds:[{hold_id:holdId,generation:2,inventory_guarded:true,has_available_capture:server.archiveUploads()>0}]});
    const realFetch = globalThis.fetch;
    t.mock.method(globalThis, "fetch", async (input: Parameters<typeof realFetch>[0], init?: Parameters<typeof realFetch>[1]) => {
      if (!String(input).endsWith(`/runs/${claim.run_id}/recovery-holds?generation=2`)) return realFetch(input, init);
      return Response.json({run_id:claim.run_id, exact_hold:{run_id:claim.run_id,generation:2,hold_id:holdId,repo_id:claim.repo.id,inventory_guarded:true,original_worker_id:registration.worker_id,original_worker_identity:"original",state:"open",live_worker_id:registration.worker_id,live_run_id:claim.run_id,run_status:"running",current_claim_generation:2,claim_released:false}});
    });
    // Exercise the real authenticated exact-hold decoder before the producer.
    const exact = await client.getExactRecoveryHold(claim.run_id, 2, {holdId,repoId:claim.repo.id});
    assert.equal(exact?.hold_id, holdId);
    if (stage === "captured" || stage === "adopting") {
      const key = "issue-2512";
      const source = { runId: claim.run_id, clonePath: clone.path };
      const reservation = await git.reserveRecoveryIteration(bare, clone.branch, key, source, 2);
      await git.commitWipMarker(clone.path);
      await fixtureFetchTracking(git, bare, clone.path, clone.branch, claim.run_id, 2);
      const tip = command(clone.path, "rev-parse", "HEAD");
      await git.recordRecoveryCapture(bare, clone.branch, key, source, reservation.attempts, tip);
      if (stage === "adopting") {
        const seam = t.mock.method(git as any, "seedRunnerClone", async () => { throw new Error("setup interrupted after adopting journal"); });
        await assert.rejects(git.prepareRecoverySuccessor(bare, clone.branch, key, source,
          reservation.attempts, "20261008T171401Z-g2-0123456789abcdef"), /setup interrupted/);
        seam.mock.restore();
        assert.equal(readJournal(bare, clone.branch).recovery.stage, "adopting");
      }
    }
    const observer = observe({ sourcePath: clone.path });
    const before = readJournal(bare, clone.branch);
    let verifiedCalls = 0, verifiedTrue = 0, facts = 0;
    const factScopes: boolean[] = [];
    const verify = git.verifiedRecoveryRestorePoint.bind(git);
    t.mock.method(git, "verifiedRecoveryRestorePoint", async (...args: Parameters<typeof verify>) => {
      observer.recordSource(clone.path);
      verifiedCalls++;
      const result = await verify(...args);
      if (result) verifiedTrue++;
      return result;
    });
    const record = git.recordRecoveryCapture.bind(git);
    t.mock.method(git, "recordRecoveryCapture", async (...args: Parameters<typeof record>) => {
      const result = await record(...args);
      facts++; factScopes.push(observer.isActive());
      return result;
    });
    let created = 0, models = 0, oldModels = 0, oldDisposed = false;
    const make: ExecutorFactory = runId => {
      const ordinal = ++created;
      const rig = ordinal === 1 ? observer.rig : codexRig();
      const dispose = rig.safety.dispose.bind(rig.safety);
      return { homeDir: path.join(homeDir, runId), executor: {
        sandboxesCommands: true,
        safety: { ...rig.safety, dispose: async request => {
          const result = await dispose(request);
          if (ordinal === 1) oldDisposed = result.kind === "disposed";
          return result;
        } },
        settleForCredentialFreeCapture: rig.settle,
        ...(stage === "missing-capability" ? {} : {
          withCredentialFreeCapture: ordinal === 1 ? observer.capture : rig.capture,
        }),
        run: async ctx => {
          if (ordinal === 1) oldModels++;
          models++;
          try {
            assert.equal(ordinal, 2);
            assert.equal(oldDisposed, true);
            assert.notEqual(ctx.worktreePath, clone.path);
            assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "retained.txt"), "utf8"), "only local dirty work\n");
          } catch (error) { callbackFailures.push(error); }
          throw new Error("fixture model interruption");
        },
      } };
    };
    const r = runnerWith(make, fakeGitlab().gitlab, JOIN_TOKEN, nullLogger(), { recoveryRetryMs: 1 });
    const recovery = (r as any).recovery;
    const spans: {method:string,phase:string,active:boolean,result?:boolean}[] = [];
    let capturedFrozen: any;
    for (const [target, methods] of [[recovery,["freezeInventory","captureAndUpload","verifiedLocalCapture"]],[git,["verifyRecoveryClosure","recordRecoveryCapture"]]] as const) {
      for (const method of methods) {
        const original = (target as any)[method].bind(target);
        t.mock.method(target as any, method, async (...args:any[]) => {
          spans.push({method,phase:"entry",active:observer.isActive()});
          const result = await original(...args);
          spans.push({method,phase:"return",active:observer.isActive(),result:!!result});
          if (method === "freezeInventory" && observer.isActive()) capturedFrozen = result;
          if (method === "verifiedLocalCapture" && observer.isActive()) console.log("LOCAL_CAPTURE", JSON.stringify({prerequisites:result?.prerequisiteShas,sourceSha:result?.sourceSha,checksum:result?.checksum}));
          return result;
        });
      }
    }
    observer.arm();
    try {
      await observer.execute(r.execute(claim));
      console.log("GUARDED_SPANS", JSON.stringify(spans));
      console.log("GUARDED_SERVER", JSON.stringify({reserves:server.archiveReserves(),uploads:server.archiveUploads(),finals:server.finals}));
      if (stage !== "missing-capability") {
      for (const method of ["freezeInventory","captureAndUpload","verifiedLocalCapture","verifyRecoveryClosure","recordRecoveryCapture"]) {
        assert.ok(spans.some(x=>x.method===method && x.phase==="entry" && x.active), method+" entered held");
        assert.ok(spans.some(x=>x.method===method && x.phase==="return" && x.active), method+" returned held");
      }
      assert.ok(server.archiveUploads()>0, "real guarded bundle uploaded");
      assert.ok(capturedFrozen?.coverageDigest, "real frozen inventory identity");
      const afterRecords = await recovery.inspect(claim.run_id);
      const same = afterRecords.find((x:any)=>x.captureId===capturedFrozen.captureId);
      assert.ok(same, "outside A frozen capture identity retained");
      assert.equal(same.coverageDigest,capturedFrozen.coverageDigest);
      assert.equal(same.inventoryCurrentSha,capturedFrozen.inventoryCurrentSha);
      assert.equal(server.acked(),0,"no terminal custody release claimed");
      } else {
        assert.deepEqual(spans, [], "missing scope forbids guarded producers and journal");
        assert.equal(server.archiveReserves(), 0);
        assert.equal(server.archiveUploads(), 0);
      }
      console.log("RETAINED_FACTS", JSON.stringify({ stage, verifiedCalls, verifiedTrue, facts, factScopes, created, models, oldModels, oldDisposed }));
      assert.equal(oldModels, 0);
      assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
      if (stage === "missing-capability") {
        observer.assertRefusal();
        assert.equal(models, 0);
        assert.equal(facts, 0);
        assert.equal(readJournal(bare, clone.branch).clonePath, before.clonePath);
      } else {
        assert.equal(models, 1);
        assert.equal(created, 2);
        assert.equal(verifiedCalls, stage === "newcapture" ? 0 : 1);
        assert.equal(verifiedTrue, stage === "newcapture" ? 0 : 1, "real seeded restore point verified");
        assert.ok(readJournal(bare, clone.branch).retainedSources.some((s:any) => s.clonePath === clone.path),
          "predecessor journal custody retained");
        assert.equal(facts, 1);
        assert.deepEqual(factScopes, [true], "capture held through actual journal capture fact");
        observer.assertProof(true);
      }
    } finally { observer.restore(); }
  });
}
