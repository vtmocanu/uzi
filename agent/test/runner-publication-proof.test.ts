import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { Readable } from "node:stream";
import { createHash } from "node:crypto";
import { StubExecutor } from "../src/executor.js";
import { api, fx, git, fakeGitlab, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { FakeRecoveryClient } from "./codex-reap-fixture.js";
import { nullLogger } from "./helpers.js";

installHarness();
function cmd(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, "-c", "maintenance.auto=false", "-c", "gc.auto=0",
    "-c", "user.name=Test", "-c", "user.email=test@example.org", "-c", "commit.gpgsign=false", ...args],
    { encoding: "utf8", stdio: "pipe", env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } }).trim();
}

for (const kind of ["mr_rework", "ci_fix", "issue"] as const) {
  for (const captureFailure of kind === "issue" ? [false] : [false, true]) {
  it(`${kind} uses real publication proof with ${captureFailure ? "failed capture and retained custody" : "verified recovery capture"}`, async () => {
    const branch = "agent/issue-2404";
    const remote = path.join(fx.dataDir, "remote.git");
    cmd(fx.dataDir, "clone", "--bare", fx.originPath, remote);
    cmd(remote, "config", "maintenance.auto", "false");
    const writer = path.join(fx.dataDir, "writer");
    cmd(fx.dataDir, "clone", remote, writer);
    cmd(writer, "checkout", "-b", branch);
    fs.writeFileSync(path.join(writer, "floor.txt"), "floor\n");
    cmd(writer, "add", "floor.txt");
    cmd(writer, "commit", "-m", "P");
    cmd(writer, "push", "origin", branch);
    const claim = gitlabClaim(2404, { kind, branch, claim_generation: 7, base_branch: "main",
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: remote } });
    const original = git.pushBranch.bind(git);
    let R = "";
    let C = "";
    let H = "";
    git.pushBranch = async (...args) => {
      C = args[5]?.candidate ?? "";
      H = args[5]?.originalHead ?? "";
      cmd(writer, "commit", "--allow-empty", "-m", "R");
      cmd(writer, "push", "origin", branch);
      R = cmd(writer, "rev-parse", "HEAD");
      // Updating shared tracking must not replace the flight's captured P.
      cmd(args[0], "fetch", "origin");
      return original(...args);
    };
    const { gitlab, calls } = fakeGitlab();
    const recoveryClient = new FakeRecoveryClient();
    const archivedBundle = path.join(fx.dataDir, "uploaded.bundle");
    const upload = recoveryClient.uploadRecoveryBundle.bind(recoveryClient);
    recoveryClient.uploadRecoveryBundle = async (runId, captureId, manifest, stream) => {
      const chunks: Buffer[] = [];
      for await (const chunk of stream) chunks.push(Buffer.from(chunk));
      const bytes = Buffer.concat(chunks);
      assert.equal(bytes.length, manifest.byte_size);
      assert.equal(createHash("sha256").update(bytes).digest("hex"), manifest.checksum);
      fs.writeFileSync(archivedBundle, bytes);
      return upload(runId, captureId, manifest, Readable.from([bytes]));
    };
    const recoveryOptions = { client: recoveryClient, git, log: nullLogger(),
      recoveryRoot: git.recoveryRoot, workerToken: "publication-proof-worker-fixture" };
    const recovery = new RecoveryCoordinator(recoveryOptions);
    if (captureFailure) {
      git.produceRecoveryBundle = async () => { throw new Error("injected bundle production failure"); };
    }
    await runner(new StubExecutor(nullLogger()), gitlab, undefined, { recovery }).execute(claim);
    const terminal = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    assert.ok(terminal);
    assert.equal(terminal.fail_origin, undefined);
    assert.equal(terminal.branch_moved, kind === "issue" ? undefined : true);
    assert.equal(terminal.failure_reason, kind === "issue"
      ? "scratch_publication_refused: candidate history cannot be published (remote_branch_advanced at floor_refresh)"
      : `branch_moved: remote_branch_advanced; superseding_tip=${R}`);
    assert.match(C, /^[0-9a-f]{40}$/);
    assert.equal(H, C, "the plain flight retains the original head separately from candidate identity");
    assert.equal(cmd(remote, "rev-parse", branch), R);
    assert.equal(calls.length, 0);
    const bare = git.barePathFor(remote);
    // Reopen the authenticated coordinator journal as a restarted worker would.
    const records = await new RecoveryCoordinator(recoveryOptions).inspect(claim.run_id);
    assert.equal(records.length, 1);
    const record = records[0]!;
    assert.equal(record.generation, 7);
    assert.equal(record.sourceSha, H, "the recovery journal names original H");
    assert.equal(record.finalizationPin, true);
    assert.deepEqual(recoveryClient.releaseCalls, [], "supersession never claims publication custody release");
    if (captureFailure) {
      assert.equal(record.state, "needs_action");
      assert.equal(record.reason, "bundle_failed");
      assert.equal(recoveryClient.uploadCalls.length, 0);
      assert.equal(cmd(bare, "rev-parse", `refs/uzi-runner/${branch}`), H,
        "the retained tracking custody ref anchors the journaled original head");
      cmd(bare, "gc", "--prune=now");
      assert.equal(cmd(bare, "rev-parse", `${H}^{commit}`), H,
        "journaled source remains reachable after terminal cleanup and object pruning");
      assert.equal(fs.existsSync(archivedBundle), false);
    } else {
      assert.equal(record.state, "uploaded");
      assert.equal(recoveryClient.uploadCalls.length, 1);
      assert.equal(recoveryClient.reserveCalls[0]!.source_sha, C);
      assert.equal(recoveryClient.reserveCalls[0]!.generation, 7);
      assert.ok(record.bundlePath);
      assert.ok(fs.statSync(archivedBundle).size > 0);
      cmd(bare, "bundle", "verify", archivedBundle);
      assert.match(cmd(bare, "bundle", "list-heads", archivedBundle), new RegExp(`^${C} `));
    }
  });
  }
}
