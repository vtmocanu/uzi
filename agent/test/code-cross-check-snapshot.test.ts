import assert from "node:assert/strict";
import { it } from "node:test";
import fs from "node:fs";
import fsPromises from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import type { CodeSnapshotRetirementProof } from "../src/git.js";
import { GitCache } from "../src/git.js";
import { CrossCheckRunner } from "../src/cross-check-runner.js";
import type { WorkerClient } from "../src/client.js";
import type { ClaimResponse } from "../src/protocol.js";
import { makeFixture } from "./fixture-repo.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";

function runGit(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8" }).trim();
}
async function fixture() {
  const fx = makeFixture({ "source.txt": "base\n" });
  const base = runGit(fx.originPath, "rev-parse", "HEAD");
  fs.writeFileSync(path.join(fx.originPath, "source.txt"), "checked\n");
  runGit(fx.originPath, "add", "source.txt");
  runGit(fx.originPath, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-qm", "checked");
  const head = runGit(fx.originPath, "rev-parse", "HEAD");
  const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = await git.ensureClone(fx.originPath, "");
  await git.pinCodeSnapshot(bare, "lead", head, base);
  const claim: ClaimResponse = makeClaim({
    run_id: randomUUID(), kind: "cross_check", claim_generation: 1,
    repo: { id: "repo", url: "https://example.test/repo", clone_url: fx.originPath },
    secrets: { forge_pat: "", codex: { auth_mode: "api_key", access_token: "fixture-access", capability: "fixture-cap" } },
    cross_check: { stage: "code", lead_run_id: "lead", round: 1, head_commit: head, base_commit: base,
      candidate_digest: "a".repeat(64), deadline_at: new Date(Date.now() + 30000).toISOString(),
      plan_md: "approved plan", milestones: [], required_capabilities: [], required_tools: [], size_class: "s",
      code_context: {}, guidance_snapshot: "" },
  });
  let status = "running";
  const verdicts: unknown[] = [];
  const client = {
    reportState: async (_id: string, body: { status: string }) => { status = body.status; return { applied: true }; },
    getInputs: async () => ({ inputs: [] }),
    postMessages: async () => {},
    getRunOwnership: async () => ({ status, claim_generation: 1 }),
    getCodeSnapshotCleanup: async () => ({ protocol: "code_snapshot_cleanup_v1", lead_run_id: claim.cross_check!.lead_run_id,
      head_commit: claim.cross_check!.stage === "code" ? claim.cross_check!.head_commit : head,
      outcome: status === "completed" ? "completed" : "failed", lead_status: "running", owned_by_worker: true,
      checker_run_id: claim.run_id, checker_claim_generation: claim.claim_generation, checker_status: status }),
    reportCodeCrossCheckVerdict: async (_id: string, _gen: number, body: unknown) => { verdicts.push(body); },
  } as unknown as WorkerClient;
  return { fx, git, bare, head, base, claim, client, verdicts };
}

it("timeout with unconfirmed native cleanup retains the snapshot reader", async (t) => {
  const f = await fixture();
  try {
    t.mock.method(f.git, "closeCodeSnapshotReader", async () => assert.fail("unconfirmed reader cannot close"));
    await new CrossCheckRunner(f.client, f.git, nullLogger(), {
      homeRoot: f.fx.dataDir, modelTimeoutMs: 5, model: { run: async (_claim, _checkout, _home, signal) => {
        await new Promise<void>((resolve) => {
          if (signal.aborted) resolve();
          else signal.addEventListener("abort", () => resolve(), { once: true });
        });
        throw new Error("cross-check cleanup unconfirmed");
      } },
    }).execute(f.claim);
    assert.equal(await f.git.codeSnapshotTip(f.bare, "lead"), f.head);
    assert.equal(JSON.parse(fs.readFileSync(metadataPath(f.bare), "utf8")).readers[0].state, "OPEN");
    assert.deepEqual(f.verdicts, [{ outcome: "failed", reason_class: "model_timeout", findings: [] }]);
  } finally { f.fx.cleanup(); }
});

it("code snapshot ref survives until child settlement and is deleted by exact name", async (t) => {
  const f = await fixture();
  try {
    runGit(f.bare, "update-ref", "refs/uzi-cross-check/unrelated", f.head);
    runGit(f.bare, "update-ref", "refs/uzi-cross-check/unknown", f.head);
    runGit(f.bare, "update-ref", "refs/heads/live", f.head);
    t.mock.method(f.git, "ensureClone", async () => { assert.fail("code child must not prepare origin"); });
    let checkerHome: string | undefined;
    let modelRan = false;
    let modelHead: string | undefined;
    let modelPin: string | undefined;
    let readerState: string | undefined;
    let homeCleanupConfirmed = false;
    const readerCleanupPins: (string | undefined)[] = [];
    const readerCleanupVerdicts: unknown[][] = [];
    const homeCleanupPins: (string | undefined)[] = [];
    const deletionObservations: { homeCleanupConfirmed: boolean; homeExists: boolean | undefined }[] = [];
    const rm = fsPromises.rm.bind(fsPromises);
    t.mock.method(fsPromises, "rm", async (target: Parameters<typeof fsPromises.rm>[0], opts: Parameters<typeof fsPromises.rm>[1]) => {
      if (checkerHome && target === checkerHome) {
        homeCleanupPins.push(await f.git.codeSnapshotTip(f.bare, "lead").catch(() => undefined));
        await rm(target, opts);
        homeCleanupConfirmed = !fs.existsSync(checkerHome);
        return;
      }
      return rm(target, opts);
    });
    const deleteSnapshot = f.git.deleteCodeSnapshot.bind(f.git);
    t.mock.method(f.git, "deleteCodeSnapshot", async (bare: string, lead: string, head: string, proof?: CodeSnapshotRetirementProof) => {
      deletionObservations.push({ homeCleanupConfirmed,
        homeExists: checkerHome === undefined ? undefined : fs.existsSync(checkerHome) });
      await deleteSnapshot(bare, lead, head, proof);
    });
    const remove = f.git.removeRunnerClone.bind(f.git);
    t.mock.method(f.git, "removeRunnerClone", async (checkout: string) => {
      readerCleanupPins.push(await f.git.codeSnapshotTip(f.bare, "lead").catch(() => undefined));
      readerCleanupVerdicts.push([...f.verdicts]);
      await remove(checkout);
    });
    await new CrossCheckRunner(f.client, f.git, nullLogger(), {
      homeRoot: f.fx.dataDir, pollMs: 1, model: { run: async (claim, checkout, home, _signal, _usage, cleanupConfirmed) => {
        modelRan = true;
        checkerHome = home;
        modelHead = runGit(checkout, "rev-parse", "HEAD");
        modelPin = await f.git.codeSnapshotTip(f.bare, "lead").catch(() => undefined);
        readerState = JSON.parse(fs.readFileSync(metadataPath(f.bare), "utf8")).readers[0].state;
        assert.match(claim.cross_check?.stage === "code" ? claim.cross_check.code_diff! : "", /checked/);
        cleanupConfirmed?.();
        return '{"findings":[]}';
      } },
    }).execute(f.claim);
    assert.equal(modelRan, true, "model actually inspected the code snapshot");
    assert.equal(modelHead, f.head, "model checkout is exact H");
    assert.equal(modelPin, f.head, "live reader keeps ref");
    assert.equal(readerState, "OPEN", "reader is durably OPEN before provider launch");
    const completed = [{ outcome: "completed", findings: [] }];
    assert.deepEqual(f.verdicts, completed, "child settles successfully");
    assert.deepEqual(readerCleanupVerdicts, [completed], "settlement precedes reader cleanup");
    assert.deepEqual(readerCleanupPins, [f.head], "pin survives through reader cleanup");
    assert.deepEqual(homeCleanupPins, [f.head], "pin survives through HOME cleanup");
    assert.deepEqual(deletionObservations, [{ homeCleanupConfirmed: true, homeExists: false }],
      "confirmed HOME cleanup precedes ref deletion");
    await assert.rejects(f.git.codeSnapshotTip(f.bare, "lead"));
    assert.equal(await f.git.codeSnapshotTip(f.bare, "unknown"), f.head);
    assert.equal(runGit(f.bare, "rev-parse", "refs/heads/live"), f.head);
    assert.equal(await f.git.codeSnapshotTip(f.bare, "unrelated"), f.head);
  } finally { f.fx.cleanup(); }
});

for (const control of ["moved", "unknown-settlement", "cleanup-failed", "confinement-failed", "home-cleanup-failed", "cancelled-early-return", "malformed", "oversized-diff"] as const) {
  it(`code snapshot lifetime control: ${control}`, async (t) => {
    const f = await fixture();
    let called = false;
    try {
      if (control === "moved") runGit(f.bare, "update-ref", "refs/uzi-cross-check/lead", f.base);
      if (control === "unknown-settlement") t.mock.method(f.client, "getCodeSnapshotCleanup", async () => { throw new Error("unknown ownership"); });
      if (control === "cancelled-early-return") {
        let probes = 0;
        t.mock.method(f.client, "reportState", async () => ++probes === 1 ? { applied: true } : { staleClaim: true });
        t.mock.method(f.client, "getRunOwnership", async () => ({ status: "cancelled", claim_generation: 1 }));
      }
      if (control === "cleanup-failed") t.mock.method(f.git, "removeRunnerClone", async () => { throw new Error("cleanup unconfirmed"); });
      if (control === "oversized-diff") {
        // The production streaming reader is exercised against a real oversized Git blob.
        fs.writeFileSync(path.join(f.fx.originPath, "large.txt"), "x".repeat(2 * 1024 * 1024));
        runGit(f.fx.originPath, "add", "large.txt");
        runGit(f.fx.originPath, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-qm", "large");
        // Import only from this fixture's local origin; the code runner itself cannot fetch.
        runGit(f.bare, "-c", "protocol.file.allow=always", "fetch", f.fx.originPath, "main");
        f.head = runGit(f.fx.originPath, "rev-parse", "HEAD");
        await f.git.pinCodeSnapshot(f.bare, "large-lead", f.head, f.base);
        if (f.claim.cross_check?.stage === "code") {
          f.claim.cross_check.head_commit = f.head;
          f.claim.cross_check.lead_run_id = "large-lead";
        }
      }
      await new CrossCheckRunner(f.client, f.git, nullLogger(), {
        homeRoot: f.fx.dataDir, pollMs: 1, model: { run: async (claim, _checkout, home, _signal, _usage, cleanupConfirmed) => {
          called = true;
          if (control === "home-cleanup-failed") {
            const rm = fsPromises.rm.bind(fsPromises);
            t.mock.method(fsPromises, "rm", async (target: Parameters<typeof fsPromises.rm>[0], opts: Parameters<typeof fsPromises.rm>[1]) => {
              if (target === home) throw Object.assign(new Error("home cleanup unconfirmed"), { code: "EIO" });
              return rm(target, opts);
            });
          }
          if (control === "confinement-failed") throw new Error("cross-check cleanup unconfirmed");
          if (control === "oversized-diff") {
            assert.equal(claim.cross_check?.stage, "code");
            assert.equal(claim.cross_check?.stage === "code" && claim.cross_check.code_diff,
              "Diff exceeds 1 MiB; inspect committed files through Read and Search.");
          }
          cleanupConfirmed?.();
          return control === "malformed" ? '{"findings":[{"id":"invalid id"}]}' : '{"findings":[]}';
        } },
      }).execute(f.claim);
      if (control === "malformed" || control === "oversized-diff") {
        await assert.rejects(f.git.codeSnapshotTip(f.bare, control === "oversized-diff" ? "large-lead" : "lead"));
      } else {
        assert.equal(await f.git.codeSnapshotTip(f.bare, "lead"), control === "moved" ? f.base : f.head);
      }
      assert.equal(called, control !== "moved");
      if (control === "malformed") assert.deepEqual(f.verdicts, [{ outcome: "failed", reason_class: "malformed", findings: [] }]);
    } finally { f.fx.cleanup(); }
  });
}

for (const extraByte of [0, 1] as const) {
  it(`code snapshot streamed UTF-8 diff: 1 MiB${extraByte ? " plus one byte" : " exactly"}`, async () => {
    const f = await fixture();
    try {
      const maxBytes = 1024 * 1024;
      const file = path.join(f.fx.originPath, "boundary.txt");
      fs.writeFileSync(file, "é\n");
      runGit(f.fx.originPath, "add", "boundary.txt");
      runGit(f.fx.originPath, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-qm", "boundary");
      const diffArgs = ["diff", "--no-ext-diff", "--no-textconv", "--no-color", `${f.base}...HEAD`];
      // Measure Git's actual patch framing, then grow the committed UTF-8 blob to the boundary.
      const seed = execFileSync("git", ["-C", f.fx.originPath, ...diffArgs]);
      const paddingBytes = maxBytes + extraByte - seed.length;
      fs.writeFileSync(file, "é" + "é".repeat(Math.floor(paddingBytes / 2)) + "x".repeat(paddingBytes % 2) + "\n");
      runGit(f.fx.originPath, "add", "boundary.txt");
      runGit(f.fx.originPath, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "--amend", "--no-edit", "-q");
      const actualDiff = execFileSync("git", ["-C", f.fx.originPath, ...diffArgs], { maxBuffer: 2 * maxBytes });
      assert.equal(actualDiff.length, maxBytes + extraByte, "actual Git output, including patch framing");
      assert.ok(actualDiff.toString("utf8").length < actualDiff.length, "fixture counts UTF-8 bytes, not characters");
      runGit(f.bare, "-c", "protocol.file.allow=always", "fetch", f.fx.originPath, "main");
      f.head = runGit(f.fx.originPath, "rev-parse", "HEAD");
      await f.git.pinCodeSnapshot(f.bare, "boundary-lead", f.head, f.base);
      assert.equal(f.claim.cross_check?.stage, "code");
      if (f.claim.cross_check?.stage !== "code") assert.fail("code claim required");
      f.claim.cross_check.head_commit = f.head;
      f.claim.cross_check.lead_run_id = "boundary-lead";
      const streamed = await f.git.readBare(f.bare, [...diffArgs.slice(0, -1), `${f.base}...${f.head}`], { maxBytes });
      assert.equal(streamed.truncated, extraByte === 1);
      assert.equal(Buffer.byteLength(streamed.text), maxBytes);
      assert.equal(streamed.text.includes("�"), false);
      let observedStage: string | undefined;
      let observedDiff: string | undefined;
      await new CrossCheckRunner(f.client, f.git, nullLogger(), {
        homeRoot: f.fx.dataDir, pollMs: 1, model: { run: async (claim, _checkout, _home, _signal, _usage, cleanupConfirmed) => {
          observedStage = claim.cross_check?.stage;
          observedDiff = claim.cross_check?.stage === "code" ? claim.cross_check.code_diff : undefined;
          cleanupConfirmed?.();
          return '{"findings":[]}';
        } },
      }).execute(f.claim);
      assert.equal(observedStage, "code");
      assert.equal(observedDiff, extraByte === 1
        ? "Diff exceeds 1 MiB; inspect committed files through Read and Search."
        : actualDiff.toString("utf8"));
      assert.deepEqual(f.verdicts, [{ outcome: "completed", findings: [] }]);
    } finally { f.fx.cleanup(); }
  });
}

it("proof-free delete refuses a moved snapshot ref and pin refuses to replace it", async () => {
  const f = await fixture();
  try {
    runGit(f.bare, "update-ref", "refs/uzi-cross-check/lead", f.base);
    await assert.rejects(f.git.deleteCodeSnapshot(f.bare, "lead", f.head));
    await assert.rejects(f.git.pinCodeSnapshot(f.bare, "lead", f.head, f.base));
    assert.equal(await f.git.codeSnapshotTip(f.bare, "lead"), f.base);
  } finally { f.fx.cleanup(); }
});

function retirement(f: Awaited<ReturnType<typeof fixture>>, childId: string, childGeneration: number): CodeSnapshotRetirementProof {
  return {
    sealedOutcome: { persisted: true, sealed: true, stage: "code", leadId: "lead", head: f.head,
      pinGeneration: 0, outcome: "completed" },
    child: { childId, childGeneration, terminalStatus: "completed" },
  };
}
function metadataPath(bare: string, lead = "lead"): string {
  return path.join(bare, "uzi-code-snapshots", `code-snapshot-${lead}.json`);
}
async function closeReader(f: Awaited<ReturnType<typeof fixture>>, checkout: string, childId: string, childGeneration: number) {
  await f.git.removeRunnerClone(checkout);
  await f.git.closeCodeSnapshotReader(f.bare, "lead", checkout,
    { childId, childGeneration, terminalStatus: "completed", cleanupConfirmed: true });
}

it("durable OPEN holds across restart; exact CLOSED proof retires only its own ref", async () => {
  const f = await fixture();
  const childId = randomUUID();
  try {
    runGit(f.bare, "update-ref", "refs/uzi-cross-check/unknown", f.head);
    runGit(f.bare, "update-ref", "refs/heads/live", f.head);
    const checkout = await f.git.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 3);
    assert.equal(runGit(checkout, "rev-parse", "HEAD"), f.head);
    assert.deepEqual(await f.git.discoverOwedCandidates(), [], "snapshot metadata does not enter owed discovery");
    const restarted = new GitCache(f.fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    assert.deepEqual(await restarted.discoverCodeSnapshots(), []);
    await closeReader(f, checkout, childId, 3);
    const candidates = await restarted.discoverCodeSnapshots();
    assert.equal(candidates.length, 1);
    assert.equal(candidates[0]!.metadata.readers[0]!.state, "CLOSED");
    await restarted.deleteCodeSnapshot(f.bare, "lead", f.head, {
      ...retirement(f, childId, 3), bootCandidate: candidates[0], leadDisposition: "foreign",
    });
    await assert.rejects(f.git.codeSnapshotTip(f.bare, "lead"));
    assert.equal(fs.existsSync(metadataPath(f.bare)), false);
    assert.equal(await f.git.codeSnapshotTip(f.bare, "unknown"), f.head);
    assert.equal(runGit(f.bare, "rev-parse", "refs/heads/live"), f.head);
  } finally { f.fx.cleanup(); }
});

it("older OPEN and sibling reader survive newer CLOSED; latest child proof is mandatory", async () => {
  const f = await fixture();
  const childId = randomUUID();
  try {
    const old = await f.git.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 1);
    const sibling = await f.git.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 1);
    const latest = await f.git.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 2);
    await closeReader(f, latest, childId, 2);
    await closeReader(f, old, childId, 1);
    await f.git.removeRunnerClone(sibling);
    await f.git.closeCodeSnapshotReader(f.bare, "lead", path.basename(sibling),
      { childId, childGeneration: 1, terminalStatus: "failed", cleanupConfirmed: true });
    const [candidate] = await f.git.discoverCodeSnapshots();
    assert.equal(candidate!.metadata.readers.length, 3);
    await f.git.deleteCodeSnapshot(f.bare, "lead", f.head, retirement(f, childId, 2));
    await assert.rejects(f.git.codeSnapshotTip(f.bare, "lead"));
  } finally { f.fx.cleanup(); }
});

it("launch failure after durable registration leaves OPEN even without a checkout", async () => {
  const f = await fixture();
  const childId = randomUUID();
  try {
    const cloneParent = path.dirname(f.git.runnerClonePath(f.bare, "unused"));
    fs.mkdirSync(path.dirname(cloneParent), { recursive: true });
    fs.writeFileSync(cloneParent, "blocks mkdir");
    await assert.rejects(f.git.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 4));
    const stored = JSON.parse(fs.readFileSync(metadataPath(f.bare), "utf8"));
    assert.equal(stored.readers.length, 1);
    assert.equal(stored.readers[0].state, "OPEN");
    await assert.rejects(f.git.deleteCodeSnapshot(f.bare, "lead", f.head, retirement(f, childId, 4)));
    assert.equal(await f.git.codeSnapshotTip(f.bare, "lead"), f.head);
  } finally { f.fx.cleanup(); }
});

for (const guard of ["stale-boot", "disposition", "unsealed", "moved", "old-child", "old-launch", "open", "generation", "child"] as const) {
  it("independent snapshot refusal reaches guard: " + guard, async () => {
    const f = await fixture();
    const childId = randomUUID();
    try {
      const first = await f.git.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 1);
      if (guard === "generation" || guard === "child") {
        await assert.rejects(f.git.closeCodeSnapshotReader(f.bare, "lead", first,
          { childId: guard === "child" ? randomUUID() : childId, childGeneration: guard === "generation" ? 2 : 1,
            terminalStatus: "completed", cleanupConfirmed: true }), /reader proof mismatch/);
        return;
      }
      if (guard === "open") {
        await assert.rejects(f.git.deleteCodeSnapshot(f.bare, "lead", f.head, retirement(f, childId, 1)), /evidence incomplete/);
        return;
      }
      await closeReader(f, first, childId, 1);
      const [stale] = await f.git.discoverCodeSnapshots();
      const second = await f.git.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 2);
      await closeReader(f, second, childId, 2);
      const [current] = await f.git.discoverCodeSnapshots();
      const proof = retirement(f, childId, 2);
      if (guard === "old-launch") {
        await assert.rejects(f.git.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 1), /generation|child/);
        return;
      }
      if (guard === "stale-boot" || guard === "disposition" || guard === "moved") {
        proof.bootCandidate = guard === "stale-boot" ? stale : current;
        if (guard !== "disposition") proof.leadDisposition = "terminal";
      }
      if (guard === "unsealed") proof.sealedOutcome.sealed = false as never;
      if (guard === "old-child") proof.child!.childGeneration = 1;
      if (guard === "moved") runGit(f.bare, "update-ref", "refs/uzi-cross-check/lead", f.base);
      await assert.rejects(f.git.deleteCodeSnapshot(f.bare, "lead", f.head, proof),
        guard === "unsealed" ? /evidence incomplete/ : guard === "old-child" ? /latest child proof/
          : guard === "moved" ? /changed before retirement/ : /boot evidence changed/);
    } finally { f.fx.cleanup(); }
  });
}

for (const guard of ["valid", "generation", "restart", "missing", "boot", "receipt"] as const) {
 it("fresh no-reader receipt authority: " + guard, async () => {
  const f = await fixture();
  try {
    const created = await f.git.pinCodeSnapshot(f.bare, "fresh", f.head, f.base, true, 7);
    assert.ok(created.freshReceipt);
    assert.equal((await f.git.pinCodeSnapshot(f.bare, "fresh", f.head, f.base, true, 7)).freshReceipt, undefined);
    if (guard === "generation") {
      await assert.rejects(f.git.pinCodeSnapshot(f.bare, "fresh", f.head, f.base, true, 8), /generation|identity|metadata/);
      return;
    }
    const proof: CodeSnapshotRetirementProof = {
      sealedOutcome: { persisted: true, sealed: true, stage: "code", leadId: "fresh", head: f.head, pinGeneration: 7, outcome: "failed" },
      freshNoReader: { receipt: created.freshReceipt, serverAtomicallyTerminalNoChild: true },
    };
    if (guard === "restart") {
      const restarted = new GitCache(f.fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
      await assert.rejects(restarted.deleteCodeSnapshot(f.bare, "fresh", f.head, proof), /fresh no-reader receipt/);
    } else if (guard === "valid") {
      await f.git.deleteCodeSnapshot(f.bare, "fresh", f.head, proof);
      await assert.rejects(f.git.codeSnapshotTip(f.bare, "fresh"));
    } else {
      if (guard === "missing") delete proof.freshNoReader;
      if (guard === "receipt") proof.freshNoReader!.receipt = { ...created.freshReceipt, receiptId: randomUUID() };
      if (guard === "boot") {
        proof.bootCandidate = (await f.git.discoverCodeSnapshots()).find(c => c.leadId === "fresh");
        proof.leadDisposition = "terminal";
      }
      await assert.rejects(f.git.deleteCodeSnapshot(f.bare, "fresh", f.head, proof), /fresh no-reader receipt/);
    }
  } finally { f.fx.cleanup(); }
 });
}

it("pin=false only verifies objects; strict identities and malformed metadata retain refs", async () => {
  const f = await fixture();
  try {
    const verified = await f.git.pinCodeSnapshot(f.bare, "verify-only", f.head, f.base, false);
    assert.deepEqual(verified, { head_commit: f.head, base_commit: f.base });
    await assert.rejects(f.git.codeSnapshotTip(f.bare, "verify-only"));
    assert.equal(fs.existsSync(metadataPath(f.bare, "verify-only")), false);
    for (const lead of ["lead\n", "x".repeat(129), "lead/sub"]) {
      await assert.rejects(f.git.pinCodeSnapshot(f.bare, lead, f.head, f.base));
    }
    await assert.rejects(f.git.pinCodeSnapshot(f.bare, "bad", f.head + "\n", f.base));
    await assert.rejects(f.git.runnerCloneAtCommit(f.bare, f.head, randomUUID() + "\n", "lead", 1));
    await assert.rejects(f.git.runnerCloneAtCommit(f.bare, f.head, randomUUID(), "lead", -1));
    const badBare = path.join(path.dirname(f.bare), "not-a-bare");
    fs.mkdirSync(badBare, { mode: 0o700 });
    fs.symlinkSync(f.fx.originPath, path.join(path.dirname(f.bare), "untrusted-link"));
    assert.equal((await f.git.discoverCodeSnapshots()).length, 1, "bad bares do not block a valid sibling");
    for (const control of ["empty", "version", "oversized", "symlink", "absent"]) {
      // Each refusal reaches its own metadata guard, without a prior error's retained fence.
      const fresh = await fixture();
      try {
        const original = fs.readFileSync(metadataPath(fresh.bare), "utf8");
        if (control === "absent" || control === "symlink") {
          fs.unlinkSync(metadataPath(fresh.bare));
          if (control === "symlink") {
            const target = path.join(fresh.fx.dataDir, "metadata-target.json");
            fs.writeFileSync(target, original, { mode: 0o600 });
            fs.symlinkSync(target, metadataPath(fresh.bare));
          }
        } else fs.writeFileSync(metadataPath(fresh.bare),
          control === "empty" ? "{}" : control === "version" ? original.replace('"version":1', '"version":2') : " ".repeat(65537));
        assert.deepEqual(await fresh.git.discoverCodeSnapshots(), []);
        await assert.rejects(fresh.git.deleteCodeSnapshot(fresh.bare, "lead", fresh.head,
          retirement(fresh, randomUUID(), 1)),
          (err: unknown) => (err as NodeJS.ErrnoException).code !== "EEXIST",
          control + " reaches the metadata refusal rather than another case's fence");
        assert.equal(await fresh.git.codeSnapshotTip(fresh.bare, "lead"), fresh.head);
      } finally { fresh.fx.cleanup(); }
    }
  } finally { f.fx.cleanup(); }
});

it("CODE fence blocks other registration and retirement without losing OPEN", async (t) => {
  const f = await fixture();
  const other = new GitCache(f.fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const childId = randomUUID();
  let release!: () => void;
  let entered!: () => void;
  const paused = new Promise<void>(resolve => { entered = resolve; });
  const resume = new Promise<void>(resolve => { release = resolve; });
  const rename = fsPromises.rename.bind(fsPromises);
  let pending: Promise<string> | undefined;
  try {
    t.mock.method(fsPromises, "rename", async (from: Parameters<typeof fsPromises.rename>[0], to: Parameters<typeof fsPromises.rename>[1]) => {
      if (to === metadataPath(f.bare)) { entered(); await resume; }
      return rename(from, to);
    });
    pending = f.git.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 1);
    await paused;
    const fence = path.join(f.bare, "uzi-code-snapshots", "code-snapshot-lead.fence");
    const held = fs.statSync(fence);
    assert.equal(held.mode & 0o777, 0o600);
    await assert.rejects(other.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 1), { code: "EEXIST" });
    await assert.rejects(other.deleteCodeSnapshot(f.bare, "lead", f.head, retirement(f, childId, 1)), { code: "EEXIST" });
    assert.equal(fs.statSync(fence).ino, held.ino);
    release();
    const first = await pending;
    assert.equal(fs.existsSync(fence), false);
    const second = await other.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 1);
    const metadata = JSON.parse(fs.readFileSync(metadataPath(f.bare), "utf8"));
    assert.deepEqual(metadata.readers.map((r: { state: string }) => r.state), ["OPEN", "OPEN"]);
    assert.equal(new Set(metadata.readers.map((r: { readerKey: string }) => r.readerKey)).size, 2);
    await closeReader(f, first, childId, 1);
    await closeReader({ ...f, git: other }, second, childId, 1);
  } finally {
    release();
    await pending?.catch(() => undefined);
    f.fx.cleanup();
  }
});

it("CODE snapshot retains until closed", async () => {
  const f = await fixture();
  const other = new GitCache(f.fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const childId = randomUUID();
  try {
    const old = await f.git.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 1);
    const newer = await other.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 2);
    await closeReader({ ...f, git: other }, newer, childId, 2);
    assert.equal(await other.codeSnapshotCandidate(f.bare, "lead"), undefined);
    assert.equal(await f.git.codeSnapshotTip(f.bare, "lead"), f.head);
    assert.deepEqual(JSON.parse(fs.readFileSync(metadataPath(f.bare), "utf8")).readers.map((r: { state: string }) => r.state),
      ["OPEN", "CLOSED"]);
    await closeReader(f, old, childId, 1);
    const candidate = await other.codeSnapshotCandidate(f.bare, "lead");
    assert.ok(candidate);
    await other.deleteCodeSnapshot(f.bare, "lead", f.head,
      { ...retirement(f, childId, 2), bootCandidate: candidate, leadDisposition: "terminal" });
    await assert.rejects(f.git.codeSnapshotTip(f.bare, "lead"));
  } finally { f.fx.cleanup(); }
});

it("CODE failed operation fence is retained and another instance cannot remove it", async () => {
  const f = await fixture();
  const other = new GitCache(f.fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const childId = randomUUID();
  try {
    const cloneParent = path.dirname(f.git.runnerClonePath(f.bare, "unused"));
    fs.mkdirSync(path.dirname(cloneParent), { recursive: true });
    fs.writeFileSync(cloneParent, "blocks mkdir");
    await assert.rejects(f.git.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 1));
    const fence = path.join(f.bare, "uzi-code-snapshots", "code-snapshot-lead.fence");
    const held = fs.statSync(fence);
    await assert.rejects(other.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 1), { code: "EEXIST" });
    await assert.rejects(other.deleteCodeSnapshot(f.bare, "lead", f.head, retirement(f, childId, 1)), { code: "EEXIST" });
    await assert.rejects(other.pinCodeSnapshot(f.bare, "lead", f.head, f.base), { code: "EEXIST" });
    const key = JSON.parse(fs.readFileSync(metadataPath(f.bare), "utf8")).readers[0].readerKey;
    await assert.rejects(other.closeCodeSnapshotReader(f.bare, "lead", key,
      { childId, childGeneration: 1, terminalStatus: "completed", cleanupConfirmed: true }), { code: "EEXIST" });
    assert.equal(fs.statSync(fence).ino, held.ino);
    assert.equal(await other.codeSnapshotTip(f.bare, "lead"), f.head);
    assert.equal(JSON.parse(fs.readFileSync(metadataPath(f.bare), "utf8")).readers[0].state, "OPEN");
  } finally { f.fx.cleanup(); }
});

it("reader count limit preserves the existing OPEN history and refuses another launch", async () => {
  const f = await fixture();
  const childId = randomUUID();
  try {
    const stored = JSON.parse(fs.readFileSync(metadataPath(f.bare), "utf8"));
    stored.latestChild = { childId, childGeneration: 1 };
    stored.readers = Array.from({ length: 128 }, () => ({
      readerKey: `cross-check-${childId}-${randomUUID()}`, childId, childGeneration: 1, state: "OPEN",
    }));
    fs.writeFileSync(metadataPath(f.bare), JSON.stringify(stored));
    const before = fs.readFileSync(metadataPath(f.bare), "utf8");
    await assert.rejects(f.git.runnerCloneAtCommit(f.bare, f.head, childId, "lead", 1), /reader limit/);
    assert.equal(fs.readFileSync(metadataPath(f.bare), "utf8"), before);
    assert.deepEqual(await f.git.discoverCodeSnapshots(), []);
    assert.equal(await f.git.codeSnapshotTip(f.bare, "lead"), f.head);
  } finally { f.fx.cleanup(); }
});
