import { it } from "node:test";
import type { InventorySourceBoundaryContext, InventorySourceDiagnostic } from "../src/recovery.js";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import os from "node:os";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
installHarness();

type BoundaryRunner = {
  withInventorySourceBoundary(context: InventorySourceBoundaryContext, action: (prove: () => Promise<boolean>) => Promise<void>): Promise<"passed" | "retained">;
  executionTails: Map<string, Promise<void>>;
  inventoryReservations: Set<string>;
  liveAttempts: { isLivePath(p: string): boolean };
};
const diagnosticCauses = [
  "attribution_unreadable", "clone_ancestor_invalid", "clone_path_invalid",
  "clone_head_unreadable", "git_or_filesystem_error", "other",
] as const;

it("#2507 actual runner boundary reports every refusal and preserves lane cleanup/order", async () => {
  const platform = Object.getOwnPropertyDescriptor(process, "platform")!;
  const cases = [
    "execution_tail_present", "boundary_exception", "inventory_read_not_verified",
    "non_linux_host", "live_attempt_path", "process_not_quiescent_before_git",
    "process_not_quiescent_after_git", "worktree_status_dirty", "worktree_status_unreadable",
    "source_identity_changed",
  ] as const;
  try {
    for (const check of cases) for (const late of [false, true]) {
      Object.defineProperty(process, "platform", { ...platform, value: "linux" });
      let active = !late, reads = 0, releaseCalls = 0;
      const ops: string[] = [], diagnostics: InventorySourceDiagnostic[] = [];
      const clones = [
        { clonePath: "/fixture/repo/issue-1", branch: "task", runId: "run-1" },
        { clonePath: "/fixture/repo/issue-2", branch: "task", runId: "run-1" },
      ];
      const r = runner({ run: async () => { throw new Error("unreached executor"); } }, fakeGitlab().gitlab,
        undefined, { quiesceRun: async req => {
          ops.push(req.site!);
          return { process: { state: active && (
            check === "process_not_quiescent_before_git" && req.site === "inventory_source" ||
            ["process_not_quiescent_after_git", "worktree_status_dirty", "worktree_status_unreadable"].includes(check) &&
              req.site === "inventory_source:after_runner_git") ? "unverified" : "quiescent",
            processes: [], killed: [], detail: "" }, docker: { state: "not_wired", removed: [], detail: "" } };
        } });
      const boundary = r as unknown as BoundaryRunner;
      const siblingTail = Promise.resolve();
      boundary.executionTails.set("sibling", siblingTail);
      boundary.liveAttempts.isLivePath = () => active && check === "live_attempt_path";
      git.readInventoryCloneHeads = async () => {
        ops.push("inventory"); reads++;
        if (active && check === "boundary_exception") throw new Error("opaque fixture exception");
        if (active && check === "inventory_read_not_verified") return { kind: "unknown", cause: "clone_path_invalid" };
        return { kind: "verified", heads: [active && check === "source_identity_changed" && reads > 1 ? "b".repeat(40) : "a".repeat(40)],
          clones, foreignOwners: [] };
      };
      git.worktreeStatus = async () => {
        ops.push("status");
        return active && check === "worktree_status_unreadable" ? null :
          active && check === "worktree_status_dirty" ? ["dirty"] : [];
      };
      if (check === "execution_tail_present") boundary.executionTails.set("run-1", siblingTail);
      if (!late && check === "non_linux_host") Object.defineProperty(process, "platform", { ...platform, value: "darwin" });
      const result = await boundary.withInventorySourceBoundary({
        runId: "run-1", generation: 7, barePath: "/fixture/repo.git", reportDiagnostic: d => diagnostics.push(d),
      }, async prove => {
        active = true;
        if (check === "non_linux_host") Object.defineProperty(process, "platform", { ...platform, value: "darwin" });
        if (await prove()) releaseCalls++;
      });
      assert.equal(releaseCalls, 0, check);
      assert.equal(diagnostics.length, 1, check);
      assert.equal(diagnostics[0]!.check, check);
      assert.equal(result, late && !["execution_tail_present", "boundary_exception"].includes(check) ? "passed" : "retained", check + " late=" + late);
      assert.equal(boundary.inventoryReservations.size, 0);
      assert.equal(boundary.executionTails.get("sibling"), siblingTail);
      assert.equal(boundary.executionTails.has("run-1"), check === "execution_tail_present");
      if (check.startsWith("worktree_status") || check === "process_not_quiescent_after_git") {
        const statusIndex = ops.lastIndexOf("status");
        assert.equal(ops[statusIndex + 1], "inventory_source:after_runner_git", "after-process runs even on unreadable/dirty status");
      }
    }
  } finally { Object.defineProperty(process, "platform", platform); }
});

it("#2507 runner distinguishes final-read identity drift from later proof drift", async () => {
  for (const driftRead of [2, 3]) {
    const r = runner({ run: async () => { throw new Error("unreached"); } }, fakeGitlab().gitlab);
    let reads = 0, releases = 0;
    const diagnostics: InventorySourceDiagnostic[] = [];
    git.readInventoryCloneHeads = async () => ({ kind: "verified",
      heads: [++reads >= driftRead ? "b".repeat(40) : "a".repeat(40)], clones: [], foreignOwners: [] });
    const result = await (r as unknown as BoundaryRunner).withInventorySourceBoundary({
      runId: "run-1", generation: 7, barePath: "/fixture/repo.git", reportDiagnostic: d => diagnostics.push(d),
    }, async prove => { if (await prove()) releases++; });
    assert.equal(result, driftRead === 2 ? "retained" : "passed");
    assert.equal(releases, 0);
    assert.equal(reads, driftRead);
    assert.deepEqual(diagnostics, [{ check: "source_identity_changed" }]);
  }
});

for (const failure of ["process", "dirty", "unreadable"] as const) {
  it(`#2507 second sibling ${failure} refuses and first failure follows existing status order`, {
    skip: process.platform !== "linux" && "physical source proof deliberately refuses non-Linux before sibling quiescence",
  }, async () => {
    const visited: string[] = [], diagnostics: InventorySourceDiagnostic[] = [];
    const r = runner({ run: async () => { throw new Error("unreached"); } }, fakeGitlab().gitlab,
      undefined, { quiesceRun: async req => {
        visited.push(req.targetPaths![0] + ":" + req.site);
        return { process: { state: req.targetPaths![0] === "/fixture/second" &&
          (failure === "process" || req.site === "inventory_source:after_runner_git") ? "unverified" : "quiescent",
          processes: [], killed: [], detail: "" }, docker: { state: "not_wired", removed: [], detail: "" } };
      } });
    git.readInventoryCloneHeads = async () => ({ kind: "verified", heads: ["a".repeat(40)],
      clones: ["first", "second", "third"].map(name => ({
        clonePath: "/fixture/" + name, branch: "task", runId: "run-1",
      })), foreignOwners: [] });
    git.worktreeStatus = async clone => {
      visited.push(clone + ":status");
      return clone === "/fixture/second" ? failure === "unreadable" ? null : ["dirty"] : [];
    };
    assert.equal(await (r as unknown as BoundaryRunner).withInventorySourceBoundary({
      runId: "run-1", generation: 7, barePath: "/fixture/repo.git", reportDiagnostic: d => diagnostics.push(d),
    }, async () => assert.fail("release")), "retained");
    assert.deepEqual(diagnostics, [{ check: failure === "process" ? "process_not_quiescent_before_git" :
      failure === "dirty" ? "worktree_status_dirty" : "worktree_status_unreadable" }]);
    assert.deepEqual(visited, ["/fixture/first:inventory_source", "/fixture/first:status",
      "/fixture/first:inventory_source:after_runner_git", "/fixture/second:inventory_source",
      ...(failure === "process" ? [] : ["/fixture/second:status", "/fixture/second:inventory_source:after_runner_git"])]);
  });
}

for (const cause of [...diagnosticCauses, undefined, "invalid:raw/path"]) for (const finalRead of [false, true]) {
  it(`#2507 runner normalizes inventory cause ${cause} finalRead=${finalRead}`, async () => {
    const r = runner({ run: async () => { throw new Error("unreached"); } }, fakeGitlab().gitlab);
    const diagnostics: InventorySourceDiagnostic[] = [];
    let reads = 0;
    git.readInventoryCloneHeads = async () => ++reads === (finalRead ? 2 : 1)
      ? { kind: "unknown", cause } as never : { kind: "verified", heads: [], clones: [], foreignOwners: [] };
    assert.equal(await (r as unknown as BoundaryRunner).withInventorySourceBoundary({
      runId: "run-1", generation: 7, barePath: "/fixture/repo.git", reportDiagnostic: d => diagnostics.push(d),
    }, async () => { assert.fail("refusal must not release"); }), "retained");
    assert.deepEqual(diagnostics, [{ check: "inventory_read_not_verified", cause: diagnosticCauses.includes(cause as never) ? cause : "other" }]);
  });
}

it("#2507 runner observer cannot throw, reject, delay proof or leak first failures between runs", async () => {
  const r = runner({ run: async () => { throw new Error("unreached"); } }, fakeGitlab().gitlab);
  const boundary = r as unknown as BoundaryRunner;
  git.readInventoryCloneHeads = async (_bare, runId) => ({ kind: "unknown",
    cause: runId === "one" ? "clone_path_invalid" : "attribution_unreadable" });
  const seen: InventorySourceDiagnostic[] = [];
  const observers = [
    () => { throw new Error("observer throw"); },
    () => Promise.reject(new Error("observer rejection")),
    () => new Promise(() => {}),
    // Intentional hostile observer fixtures exercise inventorySourceReporter promise assimilation.
    // oxlint-disable-next-line unicorn/no-thenable
    () => Object.defineProperty({}, "then", { get() { throw new Error("then access"); } }),
    // oxlint-disable-next-line unicorn/no-thenable
    () => ({ then() { throw new Error("then setup"); } }),
  ];
  for (const reportDiagnostic of observers) {
    assert.equal(await boundary.withInventorySourceBoundary({ runId: "one", generation: 7,
      barePath: "/fixture/repo.git", reportDiagnostic }, async () => assert.fail("release")), "retained");
  }
  await Promise.all(["one", "two"].map(runId => boundary.withInventorySourceBoundary({
    runId, generation: 7, barePath: "/fixture/repo.git", reportDiagnostic: d => seen.push(d),
  }, async () => assert.fail("release"))));
  assert.deepEqual(seen.map(d => d.check === "inventory_read_not_verified" && d.cause),
    ["clone_path_invalid", "attribution_unreadable"]);
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.equal(boundary.executionTails.size, 0);
  assert.equal(boundary.inventoryReservations.size, 0);
});

for (const mode of ["boot", "live"] as const) for (const source of ["dirty-unverified", "clean-unverified", "dirty-quiescent", "foreground-wip"] as const) it(`${source} physical custody through runner ${mode} recovery`, {
  skip: process.platform !== "linux" && "physical source FINAL proof requires Linux process attribution",
}, async t => {
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
