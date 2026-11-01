import { it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { Outbox } from "../src/outbox.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore } from "../src/completion-authority.js";
import { testLockOptions } from "./authority-test-support.js";
import { nullLogger } from "./helpers.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import {
  api, client, fx, git, homeDir, gitlabClaim, installHarness, runnerWith, fakeGitlab,
} from "./runner-harness.js";
import type { Logger } from "../src/log.js";
import { quiesceRunAttempt, type QuiesceRunOutcome } from "../src/run-quiescence.js";

installHarness();

const STOP = "UZI_STUB_PRE_ENROLLMENT_STOP";
const FIRST = "UZI_STUB_FIRST_PRE_ENROLLMENT_STOP";
const SOURCE = "UZI_STUB_SOURCE_END_READY";
const TERMINAL = "UZI_STUB_POST_PUBLICATION_BARRIER";
const ACK = "UZI_STUB_PRE_RUNNING_ACK_BARRIER";

function logger(onInfo?: (message: string, fields?: Record<string, unknown>) => void) {
  const lines: Array<{ message: string; fields?: Record<string, unknown> }> = [];
  const log: Logger = {
    ...nullLogger(),
    info(message, fields) { lines.push({ message, fields }); onInfo?.(message, fields); },
    warn(message, fields) { lines.push({ message, fields }); },
    error(message, fields) { lines.push({ message, fields }); },
    child() { return log; },
  };
  return { log, lines };
}

async function setup() {
  const outbox = new Outbox({
    root: path.join(fx.dataDir, "outbox"), log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000,
  });
  await outbox.init();
  // WorkerLifetime deliberately has no release; its W descriptor lasts until this test process exits.
  // acquireWorkerLifetime joins each finite flock helper before returning.
  const lifetime = await acquireWorkerLifetime(fx.dataDir, testLockOptions());
  const authority = new AuthorityRuntime({ lifetime, workerToken: "fixture-worker", timeoutMs: 5000 });
  return { outbox, authority };
}

function claim(description: string, generation = 1) {
  const c = gitlabClaim(52007, { run_id: randomUUID(), claim_generation: generation, issue_description: description });
  fs.mkdirSync(path.join(homeDir, c.run_id), { recursive: true });
  return c;
}

const ready = (lines: ReturnType<typeof logger>["lines"]) =>
  lines.filter(line => line.message === "stub: source-end ready");

async function assertRetained(outbox: Outbox, runId: string, generation: number, clone: string) {
  assert.deepEqual(outbox.listPendingFinalizes(), [{ run_id: runId, claim_generation: generation }]);
  assert.equal(outbox.hasPublicationRecords(runId), false);
  assert.equal(outbox.hasPendingTerminal(runId, generation), false);
  const files = fs.readdirSync(path.join(fx.dataDir, "outbox", runId));
  assert.ok(files.includes(`finalize-${generation}.json`));
  assert.equal(files.some(name => name.startsWith("publication-") || name.startsWith("terminal-")), false);
  assert.equal(api.states.some(s => s.runId === runId && !["running", "claimed"].includes(s.body.status)), false);
  assert.ok(fs.existsSync(clone), "the clone remains in local custody");
  assert.ok(fs.existsSync(path.join(homeDir, runId)), "session HOME remains in local custody");
}

for (const [sentinel, generation] of [[STOP, 1], [STOP, 2], [FIRST, 1]] as const) {
  it(`${sentinel} retains a durable finalize with full ordinary process/command/source proof, generation ${generation}, no checkpoint`, async t => {
    const { outbox, authority } = await setup();
    const c = claim(sentinel, generation);
    const events: string[] = [];
    let clone = "";
    const executor = new class extends StubExecutor {
      override async run(ctx: RunContext) {
        clone = ctx.worktreePath;
        events.push("executor");
        return super.run(ctx);
      }
      async settleWorkerCommands() { events.push("commands"); return { confirmed: true }; }
    }(nullLogger());
    const proof: string[] = [];
    const log = logger();
    const checkpoint = t.mock.method(client, "publishCheckpoint", async () => { throw new Error("no checkpoint expected"); });
    const r = runnerWith(() => ({ executor, homeDir: path.join(homeDir, c.run_id) }), fakeGitlab().gitlab,
      undefined, log.log, { outbox, authority, checkpointIntervalMs: 0,
        quiesceRun: async req => {
          assert.ok(req.site);
          proof.push(req.site);
          events.push(`proof:${req.site}`);
          return quiesceRunAttempt(req);
        },
      });
    await r.execute(c);
    assert.equal(checkpoint.mock.callCount(), 0);
    assert.ok(proof.includes("finalize"), "ordinary finalize quiescence ran after executor launch");
    assert.ok(events.indexOf("proof:finalize") > events.indexOf("executor"));
    assert.ok(events.includes("commands"));
    await assertRetained(outbox, c.run_id, generation, clone);
    assert.equal(log.lines.filter(l => l.message === "stub: pre-enrollment retained stop").length, 1);
    assert.deepEqual(ready(log.lines).map(l => l.fields), [{
      run_id: c.run_id, claim_generation: generation, sources: 1,
    }]);
    await authority.withTransaction(async tx => {
      const store = await openAuthorityStore(tx, { workerToken: "fixture-worker" });
      const slot = await store.readSlot(tx, await store.slotKey(tx, clone));
      assert.equal(slot?.state, "ineligible", "end receipt reflects durable source state");
      assert.equal(slot?.owner.worker, authority.authorityIncarnation());
    });
  });
}

for (const entry of ["publication-1.json", "publication-rejected.tmp"]) {
  it(`physical ${entry} blocks the pre-enrollment diagnostic even without an authenticated record`, async () => {
    const { outbox, authority } = await setup();
    const c = claim(STOP);
    let clone = "";
    let publication = "";
    const executor = new class extends StubExecutor {
      override async run(ctx: RunContext) {
        clone = ctx.worktreePath;
        const result = await super.run(ctx);
        publication = path.join(fx.dataDir, "outbox", c.run_id, entry);
        fs.mkdirSync(path.dirname(publication), { recursive: true });
        fs.writeFileSync(publication, "rejected fixture record\n");
        return result;
      }
    }(nullLogger());
    const log = logger();
    await runnerWith(() => ({ executor, homeDir: path.join(homeDir, c.run_id) }), fakeGitlab().gitlab,
      undefined, log.log, { outbox, authority, checkpointIntervalMs: 0 }).execute(c);
    assert.equal(outbox.hasPublicationRecords(c.run_id), false);
    assert.equal(await outbox.hasPublicationFile(c.run_id), true);
    assert.equal(fs.readFileSync(publication, "utf8"), "rejected fixture record\n");
    assert.deepEqual(outbox.listPendingFinalizes(), [{ run_id: c.run_id, claim_generation: 1 }]);
    assert.equal(outbox.hasPendingTerminal(c.run_id, 1), false);
    assert.ok(fs.existsSync(clone));
    assert.ok(fs.existsSync(path.join(homeDir, c.run_id)));
    assert.equal(log.lines.some(l => l.message === "stub: pre-enrollment retained stop"), false);
    assert.equal(ready(log.lines).length, 0);
  });
}

for (const processState of ["absent", "unverified", "survivors"] as const) {
  it(`${processState} process proof retains without SOURCE-END READY`, async () => {
    const { outbox, authority } = await setup();
    const c = claim(STOP);
    let clone = "";
    const executor = new class extends StubExecutor {
      override async run(ctx: RunContext) { clone = ctx.worktreePath; return super.run(ctx); }
    }(nullLogger());
    const log = logger();
    const r = runnerWith(() => ({ executor, homeDir: path.join(homeDir, c.run_id) }), fakeGitlab().gitlab,
      undefined, log.log, { outbox, authority, checkpointIntervalMs: 0,
        quiesceRun: async req => ({
          ...(req.site !== "finalize" || processState !== "absent" ? {
            process: { state: req.site === "finalize" ? processState : "quiescent", processes: [], killed: [], detail: "" },
          } : {}),
          docker: { state: "not_wired", removed: [], detail: "" },
        }) as QuiesceRunOutcome,
      });
    await r.execute(c);
    await assertRetained(outbox, c.run_id, 1, clone);
    assert.equal(ready(log.lines).length, 0);
    assert.equal(log.lines.some(l => l.message === "stub: pre-enrollment retained stop"), false);
    await authority.withTransaction(async tx => {
      const store = await openAuthorityStore(tx, { workerToken: "fixture-worker" });
      const slot = await store.readSlot(tx, await store.slotKey(tx, clone));
      assert.equal(slot?.state, "active", "unproven process leaves the source active");
    });
  });
}

for (const defect of ["missing", "partial", "epoch", "incarnation"] as const) {
  it(`${defect} source-end receipts cannot announce readiness`, async t => {
    const { outbox, authority } = await setup();
    const c = claim(STOP);
    let clone = "";
    const executor = new class extends StubExecutor {
      override async run(ctx: RunContext) { clone = ctx.worktreePath; return super.run(ctx); }
    }(nullLogger());
    const extra = path.join(fx.dataDir, "additional-source");
    fs.mkdirSync(extra);
    const bind = authority.bindSource.bind(authority);
    t.mock.method(authority, "bindSource", async (...args: Parameters<typeof bind>) => {
      const lease = await bind(...args);
      await bind(extra);
      return lease;
    });
    const end = authority.endSourceUse.bind(authority);
    t.mock.method(authority, "endSourceUse", async (...args: Parameters<typeof end>) => {
      if (defect === "missing") return { ended: [], kept: [...args[0]], endedSlots: [] };
      const result = await end(...args);
      return { ...result, endedSlots: defect === "partial" ? result.endedSlots.slice(1) :
        result.endedSlots.map(slot => ({ ...slot, ...(defect === "epoch" ? { epoch: randomUUID() } : { worker: randomUUID() }) })) };
    });
    const log = logger();
    const r = runnerWith(() => ({ executor, homeDir: path.join(homeDir, c.run_id) }), fakeGitlab().gitlab,
      undefined, log.log, { outbox, authority, checkpointIntervalMs: 0 });
    await r.execute(c);
    await assertRetained(outbox, c.run_id, 1, clone);
    assert.equal(ready(log.lines).length, 0);
  });
}

it("unconfirmed command settlement cannot announce readiness", async () => {
  const { outbox, authority } = await setup();
  const c = claim(STOP);
  let clone = "";
  const executor = new class extends StubExecutor {
    override async run(ctx: RunContext) { clone = ctx.worktreePath; return super.run(ctx); }
    async settleWorkerCommands() { return { confirmed: false }; }
  }(nullLogger());
  const log = logger();
  await runnerWith(() => ({ executor, homeDir: path.join(homeDir, c.run_id) }), fakeGitlab().gitlab,
    undefined, log.log, { outbox, authority, checkpointIntervalMs: 0 }).execute(c);
  await assertRetained(outbox, c.run_id, 1, clone);
  assert.equal(ready(log.lines).length, 0);
});

it("first-generation stop is inactive on a later generation", async () => {
  const { outbox, authority } = await setup();
  const c = claim(FIRST, 2);
  const forge = fakeGitlab();
  followSuccessfulPush(git, forge.pr, true);
  const log = logger();
  await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), forge.gitlab,
    undefined, log.log, { outbox, authority, checkpointIntervalMs: 0 }).execute(c);
  assert.ok(api.states.some(s => s.runId === c.run_id && s.body.status === "completed"));
  assert.equal(log.lines.some(l => l.message === "stub: pre-enrollment retained stop"), false);
  assert.equal(ready(log.lines).length, 0);
});

it("a real executor facade with sentinel, env and StubExecutor prototype never activates fixtures", async t => {
  const { outbox, authority } = await setup();
  const c = claim([STOP, FIRST, SOURCE, TERMINAL, ACK].join(" "));
  const previous = process.env.UZI_STUB_PRE_ENROLLMENT_STOP;
  process.env.UZI_STUB_PRE_ENROLLMENT_STOP = "1";
  t.after(() => {
    if (previous === undefined) delete process.env.UZI_STUB_PRE_ENROLLMENT_STOP;
    else process.env.UZI_STUB_PRE_ENROLLMENT_STOP = previous;
  });
  const actual = new StubExecutor(nullLogger());
  // A live harness may carry any methods or prototype; only trusted constructor identity qualifies.
  const facade: Executor = Object.assign(Object.create(StubExecutor.prototype), {
    run: (ctx: RunContext) => actual.run(ctx),
  });
  assert.equal(StubExecutor.fixture(facade), undefined);
  const forge = fakeGitlab();
  followSuccessfulPush(git, forge.pr, true);
  const log = logger();
  await runnerWith(() => ({ executor: facade }), forge.gitlab, undefined, log.log,
    { outbox, authority, checkpointIntervalMs: 0 }).execute(c);
  assert.ok(api.states.some(s => s.runId === c.run_id && s.body.status === "completed"));
  assert.equal(log.lines.some(l => l.message.startsWith("stub: fixture") || l.message === "stub: pre-enrollment retained stop"), false);
  assert.equal(ready(log.lines).length, 0);
});

for (const mode of ["timeout", "cancel", "already-cancelled"] as const) {
  it(`bounded barrier ${mode} settles and cleans its release marker`, async () => {
    const c = claim(ACK);
    const log = logger();
    let checked = false;
    const executor = new class extends StubExecutor {
      override async run(ctx: RunContext) {
        const result = await super.run(ctx);
        const controller = new AbortController();
        if (mode === "already-cancelled") controller.abort(new Error("fixture cancelled"));
        await assert.rejects(this.fixtureBarrier("running-ack", ctx.runId, c.claim_generation!,
          controller.signal, () => { if (mode === "cancel") controller.abort(new Error("fixture cancelled")); }),
          mode === "timeout" ? /timeout/ : /fixture cancelled/);
        checked = true;
        assert.equal(fs.existsSync(path.join(fx.dataDir, `cdr-outbox-running-ack-${ctx.runId}-1`)), false);
        // Wrong run/generation does not even announce readiness.
        await this.fixtureBarrier("running-ack", randomUUID(), 1, controller.signal,
          () => assert.fail("wrong run activated"));
        await this.fixtureBarrier("running-ack", ctx.runId, 2, controller.signal,
          () => assert.fail("wrong generation activated"));
        return result;
      }
    }(nullLogger(), { fixtureBarrierTimeoutMs: 10, fixtureBarrierDirectory: fx.dataDir });
    const forge = fakeGitlab();
    followSuccessfulPush(git, forge.pr, true);
    await runnerWith(() => ({ executor }), forge.gitlab, undefined, log.log,
      { checkpointIntervalMs: 0 }).execute(c);
    assert.equal(checked, true);
    assert.ok(api.states.some(s => s.runId === c.run_id && s.body.status === "completed"));
  });
}

it("ordinary cooperative pause teardown emits SOURCE-END READY without the retained-stop hook", async t => {
  const { outbox, authority } = await setup();
  const c = claim(SOURCE);
  const publish = t.mock.method(client, "publishCheckpoint", async (...[_runId, _tip, pack]: Parameters<typeof client.publishCheckpoint>) => {
    for await (const _chunk of pack) { /* join the ordinary checkpoint pack */ }
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-52007" } };
  });
  let parked = false;
  const executor = new class extends StubExecutor {
    override async run(ctx: RunContext): Promise<ExecutorResult> {
      const result = await super.run(ctx);
      await ctx.checkpoint?.({ reap: false });
      const at = { completedCount: 1, total: 2 };
      parked = await ctx.parkForPause!(at);
      return parked ? { ...result, pausedAt: at } : result;
    }
  }(nullLogger());
  const log = logger();
  await runnerWith(() => ({ executor, homeDir: path.join(homeDir, c.run_id) }), fakeGitlab().gitlab,
    undefined, log.log, { outbox, authority, checkpointIntervalMs: 0 }).execute(c);
  assert.equal(parked, true);
  assert.ok(publish.mock.callCount() > 0);
  assert.equal(api.states.some(s => s.runId === c.run_id && s.body.status === "paused"), true);
  assert.deepEqual(ready(log.lines).map(l => l.fields), [{ run_id: c.run_id, claim_generation: 1, sources: 1 }]);
});

for (const stage of ["terminal", "running-ack"] as const) {
  it(`${stage} barrier is generation-isolated and ordered around enrollment, running ACK and terminal`, async t => {
    const { outbox } = await setup();
    api.setRegisterProtocolFeatures(["publication_resume_v1", "claim_generation_fence", "completion_non_closing_v1"]);
    await client.register("stub-fixture");
    const c = claim(stage === "terminal" ? TERMINAL : ACK, 2);
    c.repo.id = randomUUID();
    c.config = { completion_contract_version: 1, contract_revision: 1, completion_scope: { deferred: [] } };
    api.setPublicationState(c.run_id, []);
    api.setCompletionPermitResponse(true);
    api.completionPermitScope = c.config.completion_scope ?? {};
    const events: string[] = [];
    api.publicationIntentWire = req => {
      events.push("enroll");
      return { status: 200, body: { enrollment_seq: req.body.enrollment_seq ?? 1,
        candidate_seq: req.body.candidate_seq ?? 1, state: "active" } };
    };
    const push = git.pushBranch.bind(git);
    t.mock.method(git, "pushBranch", async (...args: Parameters<typeof push>) => { events.push("push"); return push(...args); });
    api.stateAckWire = body => { if (body.status === "running") events.push("running"); return undefined; };
    const forge = fakeGitlab();
    followSuccessfulPush(git, forge.pr, true);
    let headReads = 0;
    const readHead = forge.gitlab.getMergeRequestHead.bind(forge.gitlab);
    t.mock.method(forge.gitlab, "getMergeRequestHead", async (...args: Parameters<typeof readHead>) => {
      const head = await readHead(...args);
      headReads++;
      events.push("head-verified");
      return head;
    });
    let reached = false;
    let wrongRelease: Promise<void> | undefined;
    const marker = path.join(fx.dataDir, `cdr-outbox-${stage}-${c.run_id}-2`);
    const wrong = path.join(fx.dataDir, `cdr-outbox-${stage}-${c.run_id}-1`);
    const access = fs.promises.access.bind(fs.promises);
    const unlink = fs.promises.unlink.bind(fs.promises);
    // Keep release-file I/O deterministic: the first check runs immediately after ready().
    // All its promise continuations settle before the setImmediate observation below.
    t.mock.method(fs.promises, "access", async (...args: Parameters<typeof access>) => {
      if (args[0] === marker || args[0] === wrong) fs.accessSync(args[0]);
      else await access(...args);
    });
    t.mock.method(fs.promises, "unlink", async (...args: Parameters<typeof unlink>) => {
      if (args[0] === marker || args[0] === wrong) fs.unlinkSync(args[0]);
      else await unlink(...args);
    });
    const releasedStages = new Set<string>();
    const executor = new class extends StubExecutor {
      override async fixtureBarrier(...args: Parameters<StubExecutor["fixtureBarrier"]>) {
        await super.fixtureBarrier(...args);
        releasedStages.add(args[0]);
      }
    }(nullLogger(), { fixtureBarrierDirectory: fx.dataDir, fixtureBarrierTimeoutMs: 5000 });
    const log = logger((message, fields) => {
      if (message !== "stub: fixture barrier ready") return;
      reached = true;
      assert.deepEqual(fields, { stage, run_id: c.run_id, claim_generation: 2 });
      assert.equal(outbox.listPendingFinalizes().length, 1);
      assert.equal(outbox.hasPublicationRecords(c.run_id), true, "enrollment is already durable");
      assert.equal(outbox.hasPendingTerminal(c.run_id, 2), false);
      assert.ok(events.includes("enroll"));
      if (stage === "terminal") {
        assert.ok(events.includes("push"));
        assert.equal(headReads, 2, "both PR head checks after publication API work finished");
      } else {
        assert.equal(events.includes("push"), false);
        assert.equal(headReads, 0);
      }
      const running = events.filter(e => e === "running").length;
      fs.writeFileSync(wrong, "release\n");
      wrongRelease = new Promise<void>((resolve, reject) => setImmediate(() => {
        try {
          assert.equal(releasedStages.has(stage), false, "wrong generation did not release the fixture await");
          assert.equal(events.filter(e => e === "running").length, running, "wrong generation did not release the barrier");
          assert.equal(outbox.hasPendingTerminal(c.run_id, 2), false, "wrong generation did not journal a terminal");
          assert.equal(api.states.some(s => s.runId === c.run_id &&
            ["completed", "failed", "cancelled"].includes(s.body.status)), false,
          "wrong generation did not send a terminal state");
          resolve();
        } catch (error) {
          reject(error);
        } finally {
          fs.writeFileSync(marker, "release\n");
        }
      }));
      void wrongRelease.catch(() => undefined);
    });
    try {
      await runnerWith(() => ({ executor }), forge.gitlab,
        undefined, log.log, { outbox, checkpointIntervalMs: 0 }).execute(c);
      await wrongRelease;
      const diagnostic = JSON.stringify({ events, states: api.states, lines: log.lines });
      assert.equal(reached, true, diagnostic);
      assert.ok(api.states.some(s => s.runId === c.run_id && s.body.status === "completed"), diagnostic);
      assert.equal(headReads, 2, "all publication head checks finished before terminal journaling");
      if (stage === "running-ack") assert.ok(events.lastIndexOf("running") < events.indexOf("push"));
    } finally {
      fs.rmSync(marker, { force: true }); fs.rmSync(wrong, { force: true });
    }
  });
}
