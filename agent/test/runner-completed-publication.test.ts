import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { mintAttemptId } from "../src/run-quiescence.js";
import type { RecoveryCoordinator } from "../src/recovery.js";
import { fixture as codexReapFixture } from "./codex-reap-fixture.js";
import { Outbox } from "../src/outbox.js";
import { resolvePendingTerminal } from "../src/terminal-resolve.js";
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
  for (const variant of kind === "self_improve" ? ["receipt", "excluded", "lost-ack"] : kind === "issue" ? ["receipt", "codex", "lost-ack", "capacity", "capacity-codex"] : ["receipt", "lost-ack"]) {
    for (const host of process.platform === "linux" && (variant === "lost-ack" || variant === "excluded" || variant === "capacity" || variant === "capacity-codex") ? (variant.startsWith("capacity") ? ["native", "darwin", "darwin-retained"] : ["native", "darwin"]) : ["native"]) {
      const capacity = variant === "capacity" || variant === "capacity-codex";
      const excluded = variant === "excluded" || capacity;
      const lostAck = variant === "lost-ack";
      it(`${capacity ? "MR2598 regression 3: " : ""}${kind}/${variant}/${host}: full execute ${lostAck ? "preserves lost-ACK completion through closed fallback, exact replay and platform cleanup policy" : excluded ? "falls back when advertised API omits receipt" : "accepts authenticated completion receipt without archive or heartbeat"}`, { concurrency: false }, async t => {
        const platform = Object.getOwnPropertyDescriptor(process, "platform")!;
        try {
          const linux = host === "native" && process.platform === "linux";
          const claim = gitlabClaim(2507, {
            run_id: literal.run_id, kind, claim_generation: 29, inventory_guarded: true,
            ...(kind === "mr_rework" ? { issue_iid: null, branch: "agent/issue-42", base_branch: "main" } : {}),
            repo: { id: literal.repo_id, url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath, forge_type: "gitlab" },
          });
          const branch = kind === "self_improve" ? `uzi/self-improve/${claim.run_id}`
            : kind === "mr_rework" ? "agent/issue-42" : "agent/issue-2507";
          api.setRegisterProtocolFeatures(["claim_generation_fence", "recovery_inventory_v1", feature, "terminal_rejection_report"]);
          const registration = await client.register("completion-full-execute", undefined, undefined, undefined, ["claim_generation_fence", "recovery_inventory_v1", feature]);
          assert.ok(registration.worker_id);
          if (capacity) {
            for (let slot = 0; slot < 4096; slot++)
              client.canStartPublicationCompletion("00000000-0000-4000-8000-" + String(slot).padStart(12, "0"), 1);
            assert.equal(client.canStartPublicationCompletion(claim.run_id, claim.claim_generation!), false);
          }
          let serverAuthoritativeHoldReleased = false, replaying = false;
          let storedAck: typeof wire.ack | undefined;
          let originalTerminal: Awaited<ReturnType<Outbox["readTerminalJournal"]>>;
          let open = true, manifest: { checksum: string; byte_size: number } | undefined;
          let reserves = 0, uploads = 0, finals = 0;
          let firstCaptureJournal: Awaited<ReturnType<Outbox["readTerminalJournal"]>>;
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
            if (capacity && reserves === 1) {
              firstCaptureJournal = await outbox.readTerminalJournal(claim.run_id, claim.claim_generation!);
            }
            if (serverAuthoritativeHoldReleased) throw new Error("capture reservation refused: hold closed");
            return { capture_id: captureId, state: "preparing" };
          };
          client.uploadRecoveryBundle = async (_run, _id, m, stream) => {
            uploads++;
            assert.equal(serverAuthoritativeHoldReleased, false, "closed hold refuses upload");
            for await (const chunk of stream) assert.ok(chunk);
            manifest = m;
            return { capture_id: captureId, state: "available", manifest_bound: true };
          };
          client.getRecoveryCaptureStatus = async () => ({
            capture_id: captureId, state: manifest ? "available" : "preparing", manifest_bound: !!manifest,
            checksum: manifest?.checksum, byte_size: manifest?.byte_size,
            expires_at: new Date(Date.now() + 60_000).toISOString(),
          });
          const finalInventories: Awaited<ReturnType<typeof git.readInventoryCloneHeads>>[] = [];
          client.releaseRecoveryCustody = async (runId, generation, evidence, disposition) => {
            assert.equal(runId, claim.run_id);
            assert.equal(generation, claim.claim_generation);
            assert.equal(finalBoundaryAuthorized, true, "FINAL follows source proof after execution tail removal");
            assert.equal(evidence, undefined);
            assert.equal(disposition?.kind, "archive");
            assert.match(disposition!.coverage_digest, /^[0-9a-f]{64}$/);
            const records = await recovery.inspect(claim.run_id);
            assert.ok(records.some(record => record.generation === generation &&
              record.coverageDigest === disposition!.coverage_digest &&
              record.serverCaptureId === (disposition as { capture_id: string }).capture_id &&
              record.sourceSha === (disposition as { source_sha: string }).source_sha),
              "FINAL matches authenticated exact-generation archive coverage");
            const inventory = await git.readInventoryCloneHeads(await git.ensureClone(fx.originPath), claim.run_id);
            finalInventories.push(inventory);
            assert.equal(inventory.kind, "verified");
            if (process.platform !== "linux" && inventory.kind === "verified") assert.deepEqual(inventory.clones, []);
            finals++;
            assert.equal(serverAuthoritativeHoldReleased, false, "no archive FINAL after completed release");
            open = false;
            return { run_id: claim.run_id, generation: claim.claim_generation!, released: true, holds_released: 1 };
          };
          let checkpointHead = "", finalHead = "", clone = "", checkpoints = 0, retainedClone = "";
          let executeSettled = false, finalBoundaryAuthorized = false;
          client.publishCheckpoint = async (_run, tip, pack) => {
            const chunks: Buffer[] = [];
            for await (const chunk of pack) chunks.push(Buffer.from(chunk));
            command(fx.originPath, ["index-pack", "--stdin", "--fix-thin"], Buffer.concat(chunks));
            command(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, tip]);
            checkpoints++;
            return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${branch}` } };
          };
          // Reuse the real registry/process-spawner fixture rather than copying the sink rig.
          const codex = (variant === "codex" || variant === "capacity-codex") ? codexReapFixture([], () => false) : undefined;
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
          let forgeUnavailable = false;
          const forge = fakeGitlab({ intercept: () => {
            if (forgeUnavailable) throw new Error("forge unavailable during stored-receipt replay");
            return undefined;
          } });
          const r = runner({ ...(codex ? { safety: codex.safety } : {}), run: async ctx => {
            clone = ctx.worktreePath;
            assert.equal(ctx.branch, branch);
            checkpointHead = commit(clone, "checkpoint.txt");
            await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
            assert.equal(checkpoints, 1, "real checkpoint pack delivered before new work");
            finalHead = commit(clone, "post-checkpoint.txt");
            fs.writeFileSync(path.join(clone, "deliberate-leftover.txt"), "discard only after safe retirement\n");
            forge.pr.head = finalHead;
            if (host === "darwin-retained") {
              const bare = await git.ensureClone(fx.originPath);
              const attemptId = mintAttemptId(claim.claim_generation!);
              const siblingBranch = branch + "-retained";
              const sibling = await git.runnerCloneForBranch(bare, siblingBranch, "retained-source", {
                beforeFree: async () => { throw new Error("retained source cannot free an incumbent"); },
              }, claim.run_id, false, undefined, {
                attemptId, isLive: () => false, beforeSeed: async paths => assert.deepEqual(paths, []),
                quiescent: async () => true,
              });
              retainedClone = sibling.path;
              await git.markRecoveryCapture(bare, sibling.path, siblingBranch, claim.run_id, attemptId);
            }
            if (capacity && host.startsWith("darwin")) Object.defineProperty(process, "platform", { ...platform, value: "darwin" });
            return { branch: ctx.branch };
          } }, forge.gitlab, "completion-full-execute-key", {
            outbox, checkpointIntervalMs: 1, checkpointTickIntervalMs: 0,
          });
          const recovery = (r as unknown as { recovery: RecoveryCoordinator }).recovery;
          const boundaryRunner = r as unknown as {
            withInventorySourceBoundary(context: { runId: string }, action: (prove: () => Promise<boolean>) => Promise<void>): Promise<"passed" | "retained">;
          };
          const sourceBoundary = boundaryRunner.withInventorySourceBoundary.bind(r);
          t.mock.method(boundaryRunner, "withInventorySourceBoundary", async (context: Parameters<typeof sourceBoundary>[0], action: Parameters<typeof sourceBoundary>[1]) => {
            const executionTailPresent = r.isExecuting(context.runId);
            return sourceBoundary(context, async prove => {
              assert.equal(executionTailPresent, false, "execution tail cannot authorize FINAL");
              finalBoundaryAuthorized = true;
              try { await action(prove); } finally { finalBoundaryAuthorized = false; }
            });
          });
          let firstPreparationJournal: Awaited<ReturnType<Outbox["readTerminalJournal"]>>;
          let preparationObserved = false;
          const freezeInventory = recovery.freezeInventory.bind(recovery);
          const freeze = t.mock.method(recovery, "freezeInventory", async (...args: Parameters<typeof freezeInventory>) => {
            if (capacity && finalHead && !preparationObserved) {
              preparationObserved = true;
              firstPreparationJournal = await outbox.readTerminalJournal(claim.run_id, claim.claim_generation!);
            }
            return freezeInventory(...args);
          });
          const capture = t.mock.method(recovery, "captureAndUpload");
          const transfer = t.mock.method(r as unknown as {
            transferRestorePointToTrustedBare: (...args: unknown[]) => Promise<unknown>;
          }, "transferRestorePointToTrustedBare");
          let acknowledgements = 0, accepted = 0;
          const originalFetch = globalThis.fetch;
          t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
            const url = new URL(input instanceof Request ? input.url : String(input));
            if (url.origin === new URL(baseUrl).origin && url.pathname.endsWith("/terminal-rejection-custody")) {
              assert.equal(Number(url.searchParams.get("generation")), claim.claim_generation);
              return Response.json({ run_id: claim.run_id, worker_id: registration.worker_id,
                generation: claim.claim_generation, exact_holds: [{ id: literal.hold_id, state: open ? "open" : "released" }],
                sibling_holds: [], exact_count: 1, sibling_count: 0, complete: true,
                exact_complete: true, sibling_complete: true, outcome: open ? "retained" : "settled" });
            }
            const response = await originalFetch(input, init);
            if (url.origin !== new URL(baseUrl).origin || url.pathname !== `/api/worker/runs/${claim.run_id}/state`) return response;
            const body = JSON.parse(String(init?.body)) as StateRequest;
            if (body.status !== "completed") return response;
            if (capacity && executeSettled) return Response.json({ run: { ...wire.ack.run, id: claim.run_id, worker_id: registration.worker_id } });
            // Provision real clones on the host; simulate Darwin for terminal fallback and retirement.
            if (host.startsWith("darwin")) Object.defineProperty(process, "platform", { ...platform, value: "darwin" });
            acknowledgements++;
            if (replaying) {
              assert.ok(storedAck);
              assert.deepEqual(body, { ...originalTerminal!.body, claim_generation: claim.claim_generation },
                "exact original terminal body replays with its generation fence after WIP fallback");
              assert.equal(serverAuthoritativeHoldReleased, true, "replay never reopens hold");
              return Response.json(storedAck);
            }
            assert.equal(response.status, 200);
            assert.equal(insideFinalize, false, "deferred Codex completion sends after finalize boundary release");
            if (!capacity) assert.equal(body.completion_final_head, finalHead, "terminal carries the delivered post-checkpoint SHA");
            assert.notEqual(finalHead, checkpointHead);
            assert.equal(command(fx.originPath, ["rev-parse", `refs/heads/${branch}`]), finalHead, "receipt follows actual push");
            assert.equal(r.isExecuting(claim.run_id), true, "own execution tail is still present at ACK");
            assert.equal(fs.existsSync(clone), true, "release cannot delete the active clone");
            if (!capacity) assert.equal(command(clone, ["rev-parse", "HEAD"]), finalHead);
            assert.equal(fs.existsSync(path.join(clone, "deliberate-leftover.txt")), true, "dirty leftovers survive until ACK");
            if (!excluded) {
              assert.equal(freeze.mock.callCount(), 0, "no preterminal inventory freeze");
              assert.equal(transfer.mock.callCount(), 0, "no preterminal restore-point transfer");
              assert.equal(wip.mock.callCount(), 0, "no WIP marker before receipt");
              if (!lostAck) open = false;
              serverAuthoritativeHoldReleased = true;
            }
            const ack = { ...wire.ack, run: { ...wire.ack.run, id: claim.run_id, worker_id: registration.worker_id } };
            if (excluded) delete ack.completed_publication_receipt;
            else ack.completed_publication_receipt = {
              ...literal, run_id: claim.run_id, worker_id: registration.worker_id,
              generation: claim.claim_generation, repo_id: claim.repo!.id, branch,
              final_head: finalHead, observed_branch_head: finalHead, mr_iid: body.mr_iid,
            };
            if (lostAck) {
              storedAck ??= ack;
              assert.deepEqual(ack, storedAck, "server stores an immutable receipt across lost retries");
              const installed = await outbox.readTerminalJournal(claim.run_id, claim.claim_generation!);
              assert.ok(installed, "original completion installed write-ahead before released response is lost");
              originalTerminal ??= installed;
              assert.deepEqual(installed, originalTerminal, "all initial completed responses are lost with journal preserved");
              throw new TypeError("response lost after authoritative completed release");
            }
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
          executeSettled = true;
          if (capacity) {
            assert.ok(firstPreparationJournal, "full execute original not journaled before first preparation");
            assert.ok(firstCaptureJournal, "full execute original not journaled before first capture");
            assert.equal(firstCaptureJournal.body.status, "completed");
          }
          assert.equal(acknowledgements, lostAck ? 3 : 1, "all initial completed retries lose their responses");
          assert.equal(api.states.filter(s => s.body.status === "failed").length, 0);
          assert.equal(api.states.filter(s => s.body.status === "completed").length, lostAck ? 3 : 1);
          assert.equal(r.isExecuting(claim.run_id), false, "execute releases its tail");
          assert.equal(heartbeat.mock.callCount(), 0, "custody disposition needs no heartbeat");
          if (lostAck) {
            assert.equal(accepted, 0);
            assert.equal(serverAuthoritativeHoldReleased, true);
            assert.equal(await recovery.hasPersistedCompletionReceipt(claim.run_id, claim.claim_generation!), false);
            assert.deepEqual(await outbox.readTerminalJournal(claim.run_id, claim.claim_generation!), originalTerminal);
            assert.equal(originalTerminal!.body.completion_final_head, finalHead);
            assert.ok(transfer.mock.callCount() > 0, "unknown ACK invokes real restore-point fallback");
            assert.ok(wip.mock.callCount() > 0, "fallback captures untracked work");
            assert.ok(freeze.mock.callCount() > 0);
            assert.ok(capture.mock.callCount() > 0);
            assert.ok(reserves > 0, "stale authenticated open listing reaches closed reservation refusal");
            assert.equal(uploads, 0);
            assert.equal(finals, 0);
            assert.ok(fs.existsSync(clone), "closed refusal grants no physical deletion authority");
            assert.equal(command(fx.originPath, ["rev-parse", `refs/heads/${branch}`]), finalHead);
            const records = await recovery.inspect(claim.run_id);
            assert.ok(records.length > 0, "original completing generation remains physically retained");
            assert.ok(records.every(record => record.generation === claim.claim_generation));
            const retire = outbox.retireTerminal.bind(outbox);
            let retirements = 0;
            t.mock.method(outbox, "retireTerminal", async (...args: Parameters<typeof retire>) => {
              assert.equal(await recovery.hasPersistedCompletionReceipt(claim.run_id, claim.claim_generation!), true,
                "authenticated generation receipt persists before terminal retirement");
              retirements++;
              return retire(...args);
            });
            forgeUnavailable = true;
            const forgeCalls = forge.all.length;
            replaying = true;
            let replayError: unknown;
            await resolvePendingTerminal(r.protectRecoveryTerminalDeps({ outbox, client,
              gapFillMax: 100, terminalMaxBytes: 1 << 20, log: nullLogger() }), {
              runId: claim.run_id, claimGeneration: claim.claim_generation!, send: async body => {
                try {
                  const fencedBody = { ...body, claim_generation: claim.claim_generation! };
                  const ack = await client.reportState(claim.run_id, fencedBody);
                  await r.observeSettlementTerminalAck(claim.run_id, claim.claim_generation!, fencedBody, ack);
                  return ack;
                } catch (error) { replayError = error; throw error; }
              },
            });
            assert.equal(replayError, undefined, "stored receipt replay must succeed");
            assert.equal(retirements, 1);
            assert.equal(accepted, 1);
            assert.equal(await outbox.readTerminalJournal(claim.run_id, claim.claim_generation!), undefined);
            const persisted = await recovery.inspect(claim.run_id);
            assert.ok(persisted.some(record => record.completionReceipt?.final_head === finalHead));
            assert.ok(persisted.length > 1, "WIP fallback leaves multiple existing exact-generation records");
            const boundSource = persisted[0]?.completionSource;
            assert.ok(boundSource);
            assert.notEqual(command(clone, ["rev-parse", "HEAD"]), finalHead, "local WIP HEAD advanced after delivered completion");
            for (const record of persisted) {
              assert.deepEqual(record.completionSource, boundSource,
                "all existing same-attempt records bind the same authenticated source snapshot");
              assert.equal(record.completionSource?.expectedHead, command(clone, ["rev-parse", "HEAD"]));
            }
            const cleanup = git.cleanupRecoveryGeneration.bind(git);
            const cleanupResults: string[] = [];
            t.mock.method(git, "cleanupRecoveryGeneration", async (...args: Parameters<typeof cleanup>) => {
              const result = await cleanup(...args);
              cleanupResults.push(result);
              return result;
            });
            await r.resumePendingRecoveries();
            assert.equal(fs.existsSync(clone), !linux, "physical cleanup retires own source only on Linux: " + JSON.stringify({
              cleanupResults, pins: command(boundSource.context.barePath, ["for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-recovery-pin", "refs/uzi-coverage", "refs/uzi-owed"]),
              cloneHead: fs.existsSync(clone) ? command(clone, ["rev-parse", "HEAD"]) : null,
              records: persisted.map(record => ({ sourceSha: record.sourceSha, expectedHead: record.completionSource?.expectedHead,
                hasReceipt: !!record.completionReceipt, roots: record.completionSource?.roots })),
            }));
            if (!linux) {
              const retained = await recovery.inspect(claim.run_id);
              assert.equal(retained.length, persisted.length, "non-Linux retains all completing-generation recovery records");
              assert.equal(await recovery.hasPersistedCompletionReceipt(claim.run_id, claim.claim_generation!), true);
              const receipts = retained.flatMap(record => record.completionReceipt ? [record.completionReceipt] : []);
              assert.ok(receipts.length > 0, "non-Linux retains the persisted authenticated lost-ACK receipt");
              for (const receipt of receipts) assert.deepEqual(receipt, storedAck!.completed_publication_receipt);
              for (const record of retained) {
                assert.equal(record.generation, claim.claim_generation);
                assert.deepEqual(record.completionSource, boundSource, "non-Linux retains the completing source snapshot");
              }
            }
            assert.equal(forge.all.length, forgeCalls, "stored receipt replay and cleanup need no forge reads");
            assert.equal(serverAuthoritativeHoldReleased, true);
            assert.equal(finals, 0);
            assert.equal(api.states.filter(s => s.body.status === "failed").length, 0);
          } else if (excluded) {
            assert.equal(accepted, 0);
            assert.ok(transfer.mock.callCount() > 0, "receipt absence prepares the existing restore point");
            assert.ok(wip.mock.callCount() > 0, "fallback captures deliberate untracked work");
            assert.ok(freeze.mock.callCount() > 0);
            assert.ok(capture.mock.callCount() > 0);
            assert.ok(reserves > 0);
            assert.ok(uploads > 0);
            // The archive may wait for the still-held execution tail. Re-drive once after execute.
            await r.resumePendingRecoveries();
            if (linux || (capacity && host !== "darwin-retained")) {
              assert.ok(finals > 0, "verified source permits archive FINAL after the execution tail settles");
              assert.ok(finalInventories.length > 0);
            } else {
              assert.equal(finals, 0, "non-Linux archive boundary refuses FINAL");
              assert.ok(fs.existsSync(retainedClone || clone), "non-Linux retains its physical source");
              assert.equal(outbox.hasPendingTerminal(claim.run_id, claim.claim_generation!), true);
              const retained = await recovery.inspect(claim.run_id);
              assert.ok(retained.length > 0, "non-Linux retains excluded generation recovery records");
              assert.ok(retained.every(record => record.generation === claim.claim_generation && !record.completionReceipt));
            }
            if (capacity) {
              const completed = api.states.find(s => s.body.status === "completed");
              assert.ok(completed);
              assert.equal("completion_final_head" in completed.body, false,
                "refused new attempt sends ordinary completion without disposition");
              assert.equal("completion_final_head" in firstPreparationJournal!.body, false,
                "refused new attempt journals ordinary completion before preparation");
              assert.equal("completion_final_head" in firstCaptureJournal!.body, false,
                "capture retains the same ordinary terminal body");
              if (host !== "darwin-retained") {
                await resolvePendingTerminal(r.protectRecoveryTerminalDeps({ outbox, client,
                  gapFillMax: 100, terminalMaxBytes: 1 << 20, log: nullLogger() }), {
                  runId: claim.run_id, claimGeneration: claim.claim_generation!,
                  send: body => client.reportState(claim.run_id, { ...body, claim_generation: claim.claim_generation }),
                });
                assert.equal(outbox.hasPendingTerminal(claim.run_id, claim.claim_generation!), false,
                  "real ordinary archive FINAL allows pending terminal retirement");
                assert.equal(await outbox.readTerminalJournal(claim.run_id, claim.claim_generation!), undefined);
              }
            }
            assert.equal(api.states.filter(s => s.body.status === "failed").length, 0);
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
        } finally { Object.defineProperty(process, "platform", platform); }
      });
    }
  }
}
