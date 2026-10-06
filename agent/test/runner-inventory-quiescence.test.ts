import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import os from "node:os";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
installHarness();
for (const mode of ["boot", "live"] as const) for (const source of ["dirty-unverified", "clean-unverified", "dirty-quiescent", "foreground-wip"] as const) it(`${source} physical custody through runner ${mode} recovery`, async t => {
  const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith("GIT_")));
  const cmd = (dir:string, args:string[]) => execFileSync("git", ["-C", dir, ...args], {env:cleanEnv, encoding:"utf8", timeout:10000}).trim();
  const claim = gitlabClaim(2997, {claim_generation:37, inventory_guarded:true});
  const {gitlab} = fakeGitlab();
  client.hasFeature = name => name === "recovery_inventory_v1";
  client.getRunOwnership = async () => ({status: api.states.some(s => s.body.status === "failed") ? "failed" : "running", claim_generation:37, inventory_guarded:true});
  let open = true, finals = 0, blocked = false, clone = "", h = "";
  let executions = 0;
  let queued: Promise<void> | undefined;
  const qSites:string[] = [];
  let manifest: import("../src/protocol.js").RecoveryUploadManifest | undefined;
  let uploadedBundle: Buffer | undefined;

  const id = "00000000-0000-4000-8000-000000002997";
  client.listRecoveryHolds = async runId => ({run_id:runId, holds:open ? [{hold_id:"tester-hold", generation:37, inventory_guarded:true, has_available_capture:false}] : []});
  client.reserveRecoveryCapture = async () => ({capture_id:id, state:"preparing"});
  client.uploadRecoveryBundle = async (_run,_id,m,stream) => {
    const chunks: Buffer[] = [];
    for await (const chunk of stream) {
      assert.ok(chunk.length);
      chunks.push(Buffer.from(chunk));
    }
    uploadedBundle = Buffer.concat(chunks);
    assert.equal(uploadedBundle.length, m.byte_size, "uploaded size matches manifest");
    assert.equal(createHash("sha256").update(uploadedBundle).digest("hex"), m.checksum,
      "uploaded bytes match manifest checksum");
    manifest=m;
    return {capture_id:id, state:"available", manifest_bound:true};
  };
  client.getRecoveryCaptureStatus = async () => ({capture_id:id, state:manifest ? "available" : "preparing", manifest_bound:!!manifest, checksum:manifest?.checksum, byte_size:manifest?.byte_size, expires_at:"2099-01-01T00:00:00Z"});
  client.releaseRecoveryCustody = async (_run, _generation, _evidence, disposition) => {
    if (source === "foreground-wip") {
      assert.ok(disposition?.kind === "archive");
      assert.ok(uploadedBundle, "FINAL has captured upload bytes");
      const imported = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-uploaded-wip-"));
      fs.chmodSync(imported, 0o700);
      t.after(() => fs.rmSync(imported, { recursive: true, force: true }));
      const bundlePath = path.join(imported, "uploaded.bundle");
      fs.writeFileSync(bundlePath, uploadedBundle, { mode: 0o600 });
      cmd(imported, ["init"]);
      cmd(imported, ["bundle", "verify", bundlePath]);
      cmd(imported, ["fetch", bundlePath, "refs/heads/recovered-source:refs/heads/recovered-source"]);
      assert.equal(cmd(imported, ["rev-parse", "refs/heads/recovered-source"]), disposition.source_sha);
      assert.equal(cmd(imported, ["show", disposition.source_sha + ":tester-source.txt"]),
        "unpublished dirty source", "the uploaded archive includes foreground WIP contents");
      assert.ok(queued, "claim queued during physical proof");
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(executions, 1, "queued executor cannot write during FINAL");
      assert.equal(r.isExecuting(claim.run_id), true, "public reclaim guard includes custody lane");
    }
    finals++;open=false;return {run_id:claim.run_id,generation:37,released:true,holds_released:1};
  };
  client.publishCheckpoint = async (_run,_tip,pack) => {
    for await (const chunk of pack) assert.ok(chunk.length);
    return {ok:true,body:{published:false,ref:"refs/uzi-checkpoints/tester",reason:"workflow_scope"}};
  };
  const r = runner({run:async ctx => {
    executions++;
    clone=ctx.worktreePath;
    fs.writeFileSync(path.join(clone,"tester-source.txt"), "committed H\n");
    cmd(clone,["add","tester-source.txt"]);
    cmd(clone,["-c","user.name=tester","-c","user.email=tester@example.test","-c","commit.gpgsign=false","commit","-m","H"]);
    h=cmd(clone,["rev-parse","HEAD"]);
    await ctx.checkpoint!({reap:false});
    assert.ok((await git.enumerateOwedCandidates(git.barePathFor(fx.originPath),claim.run_id)).some(c=>c.sha===h));
    fs.writeFileSync(path.join(clone,"tester-source.txt"),"unpublished dirty source\n");
    blocked=source !== "foreground-wip";
    throw new Error("tester executor failed after dirtying source");
  }},gitlab,"journal-key",{checkpointIntervalMs:1, quiesceRun:async req => {
    qSites.push(req.site ?? "unknown");
    if (source === "foreground-wip" && manifest && req.site === "inventory_source" && !queued) {
      queued = r.execute({ ...claim, claim_generation: 38 });
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(executions, 1, "queued executor cannot write during physical proof");
    }
    const refused = blocked || (source === "foreground-wip" && req.site === "terminal_retire" && !!clone && cmd(clone, ["status", "--porcelain"]) === "");
    return {process:{state:refused ? "unverified" : "quiescent", processes:[],killed:[],detail:blocked ? "tester physical quiescence unverified" : ""},docker:{state:"not_wired",removed:[], detail:""}};
  }});
  await r.execute(claim);
  assert.ok(api.states.some(s=>s.body.status==="failed"));
  assert.equal(finals,0,"foreground retains hold");
  assert.equal(fs.existsSync(clone),true);
  if (source === "foreground-wip") {
    assert.notEqual(cmd(clone,["rev-parse","HEAD"]), h, "foreground committed WIP");
    assert.equal(cmd(clone,["status", "--porcelain"]), "");
  } else {
    assert.equal(cmd(clone,["rev-parse","HEAD"]),h);
  }
  assert.equal(fs.readFileSync(path.join(clone,"tester-source.txt"),"utf8"),"unpublished dirty source\n");

  if (source === "clean-unverified") cmd(clone, ["checkout", "--", "tester-source.txt"]);
  if (source === "dirty-quiescent") blocked = false;
  const snapshot=await r.snapshotBootRecoveries();
  if (mode === "boot") await r.resumePendingRecoveries(undefined,snapshot);
  else await r.resumeLiveRecoveries(Date.now());
  assert.ok(qSites.includes("inventory_source"));
  if (source === "foreground-wip") {
    await queued;
    assert.equal(finals, 1, "transferred WIP permits FINAL after restart");
    assert.equal(open, false);
    assert.equal(executions, 1);
  } else {
    assert.equal(open, true);
    assert.equal(fs.existsSync(clone), true);
    assert.equal(finals,0,"unverified or dirty clone must retain generation hold after recovery");
  }
});
