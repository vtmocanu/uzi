import { it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { StubExecutor, type RunContext } from "../src/executor.js";
import { Outbox } from "../src/outbox.js";
import { makeClaim, nullLogger, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, installHarness, runnerWith } from "./runner-harness.js";
import { HELD_LINUX_ONLY, heldServer, JOIN_TOKEN, recoveryOf } from "./runner-held-publication-fixture.js";

installHarness();

it("completed guarded handoff task replays its uploaded archive through the runner live pass", { skip: HELD_LINUX_ONLY }, async t => {
  const runId = randomUUID();
  const claim = makeClaim({ run_id: runId, kind: "task", issue_iid: null,
    issue_title: "Handoff fixture", branch: `uzi/task/${runId}`, open_mr: true,
    claim_generation: 1, inventory_guarded: true, auto_approve: true,
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
  });
  const server = heldServer(claim, ["recovery_inventory_v1"]);
  client.getRunOwnership = async () => ({ status: api.states.some(s => s.runId === runId && s.body.status === "completed")
    ? "completed" : "running", claim_generation: 1, inventory_guarded: true });
  let now = 1000;
  let source = "";
  const outbox = new Outbox({ root: path.join(fx.dataDir, "outbox"), log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
  await outbox.init();
  t.mock.method(git, "hasPhysicalTerminalProtection", (id: string) => outbox.hasPhysicalTerminalProtection(id));
  const executor = new StubExecutor(nullLogger());
  const execute = executor.run.bind(executor);
  t.mock.method(executor, "run", async (ctx: RunContext) => { source = ctx.worktreePath; return execute(ctx); });
  const { logger, lines } = recordingLogger();
  const r = runnerWith(() => ({ executor }), fakeGitlab().gitlab, JOIN_TOKEN, logger, { now: () => now, outbox });
  await r.execute(claim);
  assert.equal(server.archiveUploads(), 1);
  assert.equal(server.acked(), 0, "foreground execution tail withholds FINAL");
  assert.equal(r.isExecuting(runId), false);
  const before = (await recoveryOf(r).inspect(runId)).find(record => record.coverageDigest)!;
  assert.equal(before.state, "uploaded");
  server.onFinal = async () => { assert.equal(fs.existsSync(source), true, "source remains until actual FINAL ACK"); };
  now += 30_000;
  await r.resumeLiveRecoveries(now);
  assert.equal(server.acked(), 1, JSON.stringify({ records: await recoveryOf(r).inspect(runId), lines }));
  assert.equal(server.archiveUploads(), 1, "reuse the uploaded archive");
  assert.equal(server.heldCalls.length, 0, "completed self-contained inventory remains archive-first");
  const final = server.finals[0]!.disposition;
  assert.equal(final?.kind, "archive");
  if (final?.kind === "archive") {
    assert.equal(final.source_sha, before.sourceSha);
    assert.equal(final.coverage_digest, before.coverageDigest);
    assert.equal(final.capture_id, before.serverCaptureId);
  }
});
