import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { RecoveryCoordinator } from "../src/recovery.js";
import { fixture as codexReapFixture } from "./codex-reap-fixture.js";
import { Outbox } from "../src/outbox.js";
import type { StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { api, baseUrl, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();
const wire = JSON.parse(fs.readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
const literal = wire.ack.completed_publication_receipt;
const feature = "recovery_completed_publication_v1";

function command(dir: string, args: string[], input?: Buffer): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", input }).trim();
}
function commit(dir: string, name: string): string {
  fs.writeFileSync(path.join(dir, name), name + "\n");
  command(dir, ["add", name]);
  command(dir, ["-c", "user.name=test", "-c", "user.email=test@example.com",
    "-c", "commit.gpgsign=false", "commit", "-m", name]);
  return command(dir, ["rev-parse", "HEAD"]);
}

for (const kind of ["issue", "mr_rework", "self_improve"] as const) {
  for (const variant of kind === "self_improve" ? ["receipt", "excluded"] : kind === "issue" ? ["receipt", "codex"] : ["receipt"]) {
    const excluded = variant === "excluded";
    it(`${kind}/${variant}: full execute ${excluded ? "falls back when advertised API omits receipt" : "accepts authenticated completion receipt without archive or heartbeat"}`, async t => {
      const claim = gitlabClaim(2507, {
        run_id: literal.run_id, kind, claim_generation: 29, inventory_guarded: true,
        ...(kind === "mr_rework" ? { issue_iid: null, branch: "agent/issue-42", base_branch: "main" } : {}),
        repo: { id: literal.repo_id, url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath, forge_type: "gitlab" },
      });
      const branch = kind === "self_improve" ? `uzi/self-improve/${claim.run_id}`
        : kind === "mr_rework" ? "agent/issue-42" : "agent/issue-2507";
      api.setRegisterProtocolFeatures(["claim_generation_fence", "recovery_inventory_v1", feature]);
      const registration = await client.register("completion-full-execute", undefined, undefined, undefined, ["claim_generation_fence", "recovery_inventory_v1", feature]);
      assert.ok(registration.worker_id);
      let open = true, manifest: { checksum: string; byte_size: number } | undefined;
      let reserves = 0, uploads = 0, finals = 0;
      const captureId = "00000000-0000-4000-8000-000000002507";
      // The same exact-generation guarded-server archive seam as runner-inventory-terminal-settle.
      client.getRunOwnership = async () => ({
        status: api.states.some(s => s.body.status === "completed") ? "completed" : "running",
        claim_generation: claim.claim_generation, inventory_guarded: true,
      });
      client.listRecoveryHolds = async runId => ({ run_id: runId, holds: open ? [{
        hold_id: literal.hold_id, generation: claim.claim_generation!, inventory_guarded: true,
        has_available_capture: !!manifest,
      }] : [] });
      client.reserveRecoveryCapture = async () => {
        reserves++;
        return { capture_id: captureId, state: "preparing" };
      };
      client.uploadRecoveryBundle = async (_run, _id, m, stream) => {
        uploads++;
        for await (const chunk of stream) assert.ok(chunk);
        manifest = m;
        return { capture_id: captureId, state: "available", manifest_bound: true };
      };
      client.getRecoveryCaptureStatus = async () => ({
        capture_id: captureId, state: manifest ? "available" : "preparing", manifest_bound: !!manifest,
        checksum: manifest?.checksum, byte_size: manifest?.byte_size,
        expires_at: new Date(Date.now() + 60_000).toISOString(),
      });
      client.releaseRecoveryCustody = async () => {
        finals++;
        open = false;
        return { run_id: claim.run_id, generation: claim.claim_generation!, released: true, holds_released: 1 };
      };
      let checkpointHead = "", finalHead = "", clone = "", checkpoints = 0;
      client.publishCheckpoint = async (_run, tip, pack) => {
        const chunks: Buffer[] = [];
        for await (const chunk of pack) chunks.push(Buffer.from(chunk));
        command(fx.originPath, ["index-pack", "--stdin", "--fix-thin"], Buffer.concat(chunks));
        command(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, tip]);
        checkpoints++;
        return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${branch}` } };
      };
      // Reuse the real registry/process-spawner fixture rather than copying the sink rig.
      const codex = variant === "codex" ? codexReapFixture([], () => false) : undefined;
      if (codex) t.after(() => fs.rmSync(codex.root, { recursive: true, force: true }));
      let insideFinalize = false;
      if (codex) {
        const boundary = codex.safety.withBoundary.bind(codex.safety);
        codex.safety.withBoundary = (request, action) => boundary(request, async permit => {
          if (request.boundary === "finalize") insideFinalize = true;
          try { return await action(permit); }
          finally { if (request.boundary === "finalize") insideFinalize = false; }
        });
      }
      const wip = t.mock.method(git, "commitWipMarker");
      const heartbeat = t.mock.method(client, "heartbeat");
      const outbox = new Outbox({
        root: path.join(fx.dataDir, "outbox"), log: nullLogger(),
        runMaxBytes: 64 << 20, maxBytes: 128 << 20, retentionMs: 86_400_000,
      });
      await outbox.init();
      const forge = fakeGitlab();
      const r = runner({ ...(codex ? { safety: codex.safety } : {}), run: async ctx => {
        clone = ctx.worktreePath;
        assert.equal(ctx.branch, branch);
        checkpointHead = commit(clone, "checkpoint.txt");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        assert.equal(checkpoints, 1, "real checkpoint pack delivered before new work");
        finalHead = commit(clone, "post-checkpoint.txt");
        fs.writeFileSync(path.join(clone, "deliberate-leftover.txt"), "discard only after safe retirement\n");
        forge.pr.head = finalHead;
        return { branch: ctx.branch };
      } }, forge.gitlab, "completion-full-execute-key", {
        outbox, checkpointIntervalMs: 1, checkpointTickIntervalMs: 0,
      });
      const recovery = (r as unknown as { recovery: RecoveryCoordinator }).recovery;
      const freeze = t.mock.method(recovery, "freezeInventory");
      const capture = t.mock.method(recovery, "captureAndUpload");
      const transfer = t.mock.method(r as unknown as {
        transferRestorePointToTrustedBare: (...args: unknown[]) => Promise<unknown>;
      }, "transferRestorePointToTrustedBare");
      let acknowledgements = 0, accepted = 0;
      const originalFetch = globalThis.fetch;
      t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
        const url = new URL(input instanceof Request ? input.url : String(input));
        const response = await originalFetch(input, init);
        if (url.origin !== new URL(baseUrl).origin || url.pathname !== `/api/worker/runs/${claim.run_id}/state`) return response;
        const body = JSON.parse(String(init?.body)) as StateRequest;
        if (body.status !== "completed") return response;
        acknowledgements++;
        assert.equal(response.status, 200);
        assert.equal(insideFinalize, false, "deferred Codex completion sends after finalize boundary release");
        assert.equal(body.completion_final_head, finalHead, "terminal carries the delivered post-checkpoint SHA");
        assert.notEqual(finalHead, checkpointHead);
        assert.equal(command(fx.originPath, ["rev-parse", `refs/heads/${branch}`]), finalHead, "receipt follows actual push");
        assert.equal(r.isExecuting(claim.run_id), true, "own execution tail is still present at ACK");
        assert.equal(fs.existsSync(clone), true, "release cannot delete the active clone");
        assert.equal(command(clone, ["rev-parse", "HEAD"]), finalHead);
        assert.equal(fs.existsSync(path.join(clone, "deliberate-leftover.txt")), true, "dirty leftovers survive until ACK");
        if (!excluded) {
          assert.equal(freeze.mock.callCount(), 0, "no preterminal inventory freeze");
          assert.equal(transfer.mock.callCount(), 0, "no preterminal restore-point transfer");
          assert.equal(wip.mock.callCount(), 0, "no WIP marker before receipt");
          open = false;
        }
        const ack = { ...wire.ack, run: { ...wire.ack.run, id: claim.run_id, worker_id: registration.worker_id } };
        if (excluded) delete ack.completed_publication_receipt;
        else ack.completed_publication_receipt = {
          ...literal, run_id: claim.run_id, worker_id: registration.worker_id,
          generation: claim.claim_generation, repo_id: claim.repo!.id, branch,
          final_head: finalHead, observed_branch_head: finalHead, mr_iid: body.mr_iid,
        };
        return Response.json(ack);
      });
      const report = client.reportState.bind(client);
      t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
        const ack = await report(...args);
        if (args[1].status === "completed") {
          if (codex) assert.equal(args[2], undefined, "deferred terminal carries no expired boundary signal");
          if (excluded) assert.equal(ack.completedPublicationReceipt, undefined);
          else {
            assert.ok(ack.completedPublicationReceipt, "real WorkerClient authenticated the wire receipt");
            assert.equal(ack.completedPublicationReceipt.worker_id, registration.worker_id);
            accepted++;
          }
        }
        return ack;
      });
      await r.execute(claim);
      assert.equal(acknowledgements, 1, "one completed terminal");
      assert.equal(api.states.filter(s => s.body.status === "failed").length, 0);
      assert.equal(api.states.filter(s => s.body.status === "completed").length, 1);
      assert.equal(r.isExecuting(claim.run_id), false, "execute releases its tail");
      assert.equal(heartbeat.mock.callCount(), 0, "custody disposition needs no heartbeat");
      if (excluded) {
        assert.equal(accepted, 0);
        assert.ok(transfer.mock.callCount() > 0, "receipt absence prepares the existing restore point");
        assert.ok(wip.mock.callCount() > 0, "fallback captures deliberate untracked work");
        assert.ok(freeze.mock.callCount() > 0);
        assert.ok(capture.mock.callCount() > 0);
        assert.ok(reserves > 0);
        assert.ok(uploads > 0);
        // The archive may wait for the still-held execution tail. Re-drive once after execute.
        await r.resumePendingRecoveries();
        assert.ok(finals > 0, "fallback uses archive FINAL after the execution tail settles");
      } else {
        assert.equal(accepted, 1);
        assert.equal(transfer.mock.callCount(), 0);
        assert.equal(wip.mock.callCount(), 0);
        assert.equal(freeze.mock.callCount(), 0);
        assert.equal(capture.mock.callCount(), 0);
        assert.equal(reserves, 0);
        assert.equal(uploads, 0);
        assert.equal(finals, 0);
        assert.equal(open, false);
        assert.equal(await recovery.hasPersistedCompletionReceipt(claim.run_id, claim.claim_generation!), true);
        assert.equal(await outbox.readTerminalJournal(claim.run_id, claim.claim_generation!), undefined, "receipt persisted before terminal retirement");
      }
    });
  }
}
