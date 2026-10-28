import assert from "node:assert/strict";
import fs from "node:fs";
import { spawn } from "node:child_process";
import { PassThrough } from "node:stream";
import path from "node:path";
import { CodexOperationAdmissionError } from "../src/client.js";
import type { RunContext } from "../src/executor.js";
import { commitInTree, trackedFile } from "./runner-codex-sinks-fixture.js";
import { it } from "node:test";
import { api, client, fakeGitlab, installHarness, runner, runnerWith } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";
import { freshClaim, gates, newApi, row, send, until } from "./gate-revision-harness.js";
import { providerFixture, type Frame } from "./plan-revision-provider-fixture.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { CodexOperationRequest } from "../src/protocol.js";
import { AdmissionClock, flushUntil } from "./operation-admission-clock.js";

installHarness();
const FEATURE = "codex_account_operation_admission_v1";
function enable(): void {
  newApi();
  api.planRevisionRetries = true;
  client.protocolFeatures = [...client.protocolFeatures, "plan_revision_retry_v1", FEATURE];
}
for (const generation of [0, undefined]) for (const supported of [false, true])
  it(`M2 subscription missing positive generation ${generation} uses negotiated policy=${supported}`, async t => {
    enable();
    if (!supported) client.protocolFeatures = client.protocolFeatures.filter(f => f !== FEATURE);
    const clock = new AdmissionClock();
    const registry = new ActiveRunRegistry();
    const fixture = providerFixture("codex", () => ({ plan: "# legacy or unavailable" }), false, { subscription: true });
    const claim = freshClaim(7, { claim_generation: generation,
      secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } });
    const original = client.checkCodexOperationAdmission.bind(client);
    let reads = 0;
    t.mock.method(client, "checkCodexOperationAdmission", async (...args: Parameters<typeof original>) => {
      reads++;
      return original(...args);
    });
    const r = runner(fixture.executor, fakeGitlab().gitlab, undefined, { activeRuns: registry,
      codexOperationWaitPolicy: { now: () => clock.now, sleep: clock.sleep } });
    let ended = false;
    const done = r.execute(claim).finally(() => { ended = true; });
    try {
      assert.ok(await until(() => ended || fixture.starts.length > 0 || clock.sleepers.size > 0));
      if (supported) {
        assert.equal(fixture.starts.length, 0, "negotiated admission cannot spend with an unproven generation");
        assert.equal(ended, false, "missing generation is unavailable, not proven ownership loss");
        assert.equal(clock.sleepers.size, 1);
        assert.equal(reads, 1);
        const entry = registry.build().active.find(e => e.run_id === claim.run_id)!;
        assert.equal(entry.claim_generation, 0);
        assert.equal(entry.account_cooldown, undefined, "zero generation cannot carry valid cooldown display");
      } else {
        assert.equal(fixture.starts.length, 1, "never-negotiated flight retains legacy dispatch");
        assert.equal(reads, 0);
      }
    } finally { r.shutdown(); await done; }
  });

it("M2 actual runner checkpoint packs local bytes while admission HTTP remains held", async t => {
  enable();
  const fixture = providerFixture("codex", () => ({ plan: "# never dispatched" }), false, {
    subscription: true, deps: {
      deferRegistryTeardown: true,
      boundaryDeadlineMs: 5000,
      // Only worker-generated Git argv runs here. Provider/fileop roots remain inert.
      launchEffectRoot: async spec => {
        const argv = spec.identity === "worker_pat" ? [spec.command, ...spec.args]
          : spec.args.slice(spec.args.indexOf("--") + 1);
        const fileop = argv[0] === "/usr/local/bin/uzi-codex-fileop";
        if (!fileop) assert.equal(path.basename(argv[0]!), "git", "physical fixture executes only worker Git");
        const child = fileop ? undefined : spawn(argv[0]!, argv.slice(1), {
          cwd: spec.cwd, env: spec.env, stdio: ["pipe", "pipe", "pipe"],
        });
        const terminal = child ? new Promise<number>((resolve, reject) => {
          child.once("error", reject);
          child.once("exit", code => resolve(code ?? 1));
        }) : Promise.resolve(0);
        return {
          started: { event: "started", supervisorPid: 200, childPid: 201, subreaper: true,
            nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
          supervisorPid: 200,
          transport: child ? { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr }
            : { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough() },
          snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
          observe: async () => ({ event: "observe", id: 1, state: "unavailable", reason: "unreadable" }),
          waitChild: async () => ({ event: "child_exit", code: await terminal }),
          dispose: async () => {
            if (child && child.exitCode === null) child.kill("SIGKILL");
            await terminal;
            return { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } };
          },
          failed: undefined, whenFailed: new Promise<Error>(() => {}),
        };
      },
    },
  });
  const claim = freshClaim(7, { secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } });
  const registry = new ActiveRunRegistry();
  let context!: RunContext;
  let executorSettled = false;
  const run = fixture.executor.run.bind(fixture.executor);
  let finishCapture!: () => void;
  const captureFinished = new Promise<void>(resolve => { finishCapture = resolve; });
  t.mock.method(fixture.executor, "run", async (ctx: RunContext) => {
    context = ctx;
    // The externally driven checkpoint remains owned by this fixture's flight until joined.
    try { return await run(ctx); } finally { executorSettled = true; await captureFinished; }
  });
  let releaseHTTP!: () => void;
  let httpSettled = false;
  t.mock.method(client, "checkCodexOperationAdmission", async (_id: string, request: CodexOperationRequest) => {
    await new Promise<void>(resolve => { releaseHTTP = resolve; });
    httpSettled = true;
    return { decision: "admit", operation_id: request.operation_id, claim_generation: request.claim_generation };
  });
  t.mock.method(client, "refreshCodex", async () => ({ access_token: "fixture-key", generation: 1, outcome: "adopted" }));
  const packed: { tip: string; bytes: number }[] = [];
  let releaseUpload!: () => void;
  const uploadReleased = new Promise<void>(resolve => { releaseUpload = resolve; });
  t.mock.method(client, "publishCheckpoint", async (...[_id, tip, pack]: Parameters<typeof client.publishCheckpoint>) => {
    let bytes = 0;
    for await (const chunk of pack) bytes += Buffer.byteLength(chunk);
    packed.push({ tip, bytes });
    await uploadReleased;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/m2-held" } };
  });
  const diagnostics: unknown[] = [];
  const logger = nullLogger();
  logger.warn = (message, fields) => { diagnostics.push({ message, fields }); };
  logger.info = (message, fields) => { diagnostics.push({ message, fields }); };
  logger.child = () => logger;
  const r = runnerWith(() => ({ executor: fixture.executor }), fakeGitlab().gitlab, undefined, logger, {
    activeRuns: registry, checkpointIntervalMs: 0, shutdownPublishTimeoutMs: 5000,
  });
  const done = r.execute(claim);
  let checkpoint: Promise<void> | undefined;
  try {
    assert.ok(await until(() => !!releaseHTTP));
    const before = registry.build().active.find(e => e.run_id === claim.run_id)!;
    assert.equal(before.claim_generation, 7);
    commitInTree(context.worktreePath, "M2-HELD-ADMISSION.txt", "durable M2 admission sentinel\n");
    checkpoint = context.checkpoint!({ reap: true, sink: "done_checkpoint" });
    assert.ok(await until(() => packed.length > 0), `physical checkpoint pack completes before the outstanding admission HTTP reply: ${JSON.stringify(diagnostics)}`);
    assert.equal(httpSettled, false);
    assert.equal(fixture.starts.length, 0);
    assert.ok(packed[0]!.bytes > 12, "real git pack stream contains objects");
    assert.match(packed[0]!.tip, /^[0-9a-f]{40,64}$/);
    assert.equal(trackedFile(claim.issue_iid!, "M2-HELD-ADMISSION.txt"), "durable M2 admission sentinel\n",
      "actual durable worker bare contains the captured local change");
    assert.equal(fs.readFileSync(path.join(context.worktreePath, "M2-HELD-ADMISSION.txt"), "utf8"),
      "durable M2 admission sentinel\n", "capture retains the original local source");
    const held = registry.build().active.find(e => e.run_id === claim.run_id)!;
    assert.equal(held.claim_generation, before.claim_generation);
    // Resolve admission before shutdown: only the captured epoch's guard can stop this send.
    releaseHTTP();
    assert.ok(await until(() => executorSettled), "the reaped epoch refuses the late admission before shutdown");
    assert.equal(fixture.starts.length, 0, "capture, independently of shutdown, revokes this operation");
    r.shutdown();
    finishCapture();
    releaseUpload();
    await checkpoint;
    r.shutdown();
    releaseHTTP();
    await done;
    assert.equal(fixture.starts.length, 0, "late reply cannot dispatch into the checkpointed epoch");
  } finally { releaseUpload?.(); releaseHTTP?.(); finishCapture(); r.shutdown(); await checkpoint; await done; }
});

// Replacement is delivered through the real input poller, not a private recovery hook.
for (const boundary of ["cooldown", "HTTP"] as const)
  it(`M2 replacement revision during ${boundary} invalidates old dispatch without a policy tick`, async t => {
    enable();
    const clock = new AdmissionClock();
    const registry = new ActiveRunRegistry();
    const fixture = providerFixture("codex", n => ({ plan: n === 1 ? "# original source" : "# replacement result" }), false, { subscription: true });
    const claim = freshClaim(7, { config: { plan_max_revisions: 3 },
      secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } });
    const id = claim.run_id;
    const reads: CodexOperationRequest[] = [];
    const sleepSignals: AbortSignal[] = [];
    let release!: () => void;
    t.mock.method(client, "checkCodexOperationAdmission", async (_id: string, request: CodexOperationRequest) => {
      reads.push(structuredClone(request));
      if (reads.length === 2 && boundary === "HTTP")
        await new Promise<void>(resolve => { release = resolve; });
      if (reads.length === 2 && boundary === "cooldown")
        return { decision: "wait", ...request, class: "overload",
          episode_id: "00000000-0000-4000-8000-000000000091",
          server_now: new Date(0).toISOString(), eligible_at: new Date(1000).toISOString() };
      return { decision: "admit", operation_id: request.operation_id, claim_generation: request.claim_generation };
    });
    const r = runner(fixture.executor, fakeGitlab().gitlab, undefined, { activeRuns: registry,
      codexOperationWaitPolicy: { now: () => clock.now, sleep: (ms, signal) => {
        sleepSignals.push(signal); return clock.sleep(ms, signal);
      } } });
    const done = r.execute(claim);
    try {
      assert.ok(await until(() => gates(id).length === 1));
      const source = structuredClone(gates(id)[0]!);
      const [old] = send(id, row("revise_plan", "obsolete feedback unique"));
      assert.ok(await until(() => boundary === "HTTP" ? !!release : sleepSignals.length === 1));
      const before = registry.build().active.find(e => e.run_id === id)!;
      const [replacement] = send(id, row("revise_plan", "replacement feedback unique"));
      assert.ok(await until(() => api.isAcked(id, replacement!.id)), "replacement reached the real input delivery boundary");
      if (boundary === "cooldown") {
        // No virtual time advance: a replacement must interrupt this exact sleeper.
        await flushUntil(() => sleepSignals[0]!.aborted).catch(() => {
          assert.fail("replacement reached input delivery but did not abort the obsolete cooldown sleeper at virtual time 0");
        });
        assert.equal(clock.now, 0);
        assert.equal(reads.length, 2, "source replacement does not cause an early policy poll");
      } else {
        release();
        // A late admit cannot send the predecessor while the replacement is queued.
        await flushUntil(() => fixture.starts.length > 1 || clock.sleepers.size > 0 || reads.length > 2);
        assert.equal(fixture.starts.length, 1, "late HTTP admit dispatched obsolete revision feedback");
      }
      assert.equal(api.isApplied(id, old!.id), false);
      assert.equal(api.isApplied(id, replacement!.id), false);
      assert.equal(gates(id).length, 1);
      assert.deepEqual(gates(id)[0], source, "pending replacement retains the exact source presentation");
      const after = registry.build().active.find(e => e.run_id === id)!;
      assert.equal(after.claim_generation, before.claim_generation);
      assert.equal(fixture.starts.length, 1);
      // Even after wakeup the successor has to recheck under its own source.
      assert.ok(await until(() => clock.sleepers.size > 0));
      clock.advance(boundary === "cooldown" ? 1000 : 15000);
      assert.ok(await until(() => fixture.starts.length === 2));
      assert.equal(reads.at(-1)!.source?.revision_input_id, replacement!.id);
      assert.notEqual(reads.at(-1)!.operation_id, reads[1]!.operation_id);
      assert.equal(reads.at(-1)!.source?.presentation_id, source.presentation_id);
      assert.equal(fixture.starts[1]!.threadId, fixture.starts[0]!.threadId);
      assert.match(fixture.starts[1]!.prompt, /replacement feedback unique/);
      assert.doesNotMatch(fixture.starts[1]!.prompt, /obsolete feedback unique/);
      assert.ok(await until(() => gates(id).length === 2));
      assert.ok(api.isApplied(id, replacement!.id));
      send(id, row("cancel"));
      await done;
    } finally { release?.(); r.shutdown(); await done; }
  });

for (const boundary of ["HTTP", "cooldown"] as const)
  it(`M2 real human revision feature rollback during ${boundary} waits without losing its source`, async t => {
    enable();
    const features = [...client.protocolFeatures];
    const clock = new AdmissionClock();
    const fixture = providerFixture("codex", n => ({ plan: n === 1 ? "# source" : "# revised" }), false, { subscription: true });
    const claim = freshClaim(7, { secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } });
    const reads: CodexOperationRequest[] = [];
    let release!: () => void;
    t.mock.method(client, "checkCodexOperationAdmission", async (_id: string, request: CodexOperationRequest) => {
      reads.push(structuredClone(request));
      if (reads.length === 2) {
        if (boundary === "HTTP") await new Promise<void>(resolve => { release = resolve; });
        else return { decision: "wait", ...request, class: "overload",
          episode_id: "00000000-0000-4000-8000-000000000094",
          server_now: new Date(0).toISOString(), eligible_at: new Date(1000).toISOString() };
      }
      return { decision: "admit", operation_id: request.operation_id, claim_generation: request.claim_generation };
    });
    const r = runner(fixture.executor, fakeGitlab().gitlab, undefined,
      { codexOperationWaitPolicy: { now: () => clock.now, sleep: clock.sleep } });
    const done = r.execute(claim);
    let ended = false;
    void done.then(() => { ended = true; });
    try {
      assert.ok(await until(() => gates(claim.run_id).length === 1));
      const source = structuredClone(gates(claim.run_id)[0]!);
      const [revision] = send(claim.run_id, row("revise_plan", "retain rollback feedback"));
      assert.ok(await until(() => boundary === "HTTP" ? !!release : clock.sleepers.size === 1));
      client.clearFeatures();
      if (boundary === "HTTP") release();
      else clock.advance(1000);
      assert.ok(await until(() => ended || clock.sleepers.size === 1));
      assert.equal(ended, false, "negotiation rollback must remain an engaged unavailable wait");
      assert.equal(fixture.starts.length, 1);
      assert.equal(reads.length, 2);
      assert.equal(api.isApplied(claim.run_id, revision!.id), false);
      assert.deepEqual(gates(claim.run_id), [source]);
      clock.advance(14999);
      await flushUntil(() => clock.sleepers.size === 1);
      assert.equal(reads.length, 2);
      client.protocolFeatures = features;
      clock.advance(1);
      assert.ok(await until(() => gates(claim.run_id).length === 2));
      assert.equal(fixture.starts.length, 2);
      assert.equal(reads[2]!.operation_id, reads[1]!.operation_id);
      assert.deepEqual(reads[2]!.source, reads[1]!.source);
      assert.equal(reads[2]!.claim_generation, 7);
      assert.ok(api.isApplied(claim.run_id, revision!.id));
      assert.match(fixture.starts[1]!.prompt, /retain rollback feedback/);
      send(claim.run_id, row("cancel"));
      await done;
    } finally { client.protocolFeatures = features; release?.(); r.shutdown(); await done; }
  });

for (const reason of ["authority_lost", "stale_source", "route_uncertain"] as const)
  it(`M2 real revision admission maps ${reason} without dispatch or feedback settlement`, async t => {
    enable();
    const clock = new AdmissionClock();
    const fixture = providerFixture("codex", () => ({ plan: "# retained source" }), false, { subscription: true });
    const claim = freshClaim(7, { secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } });
    let reads = 0;
    t.mock.method(client, "checkCodexOperationAdmission", async (_id: string, request: CodexOperationRequest) => {
      if (++reads > 1) throw new CodexOperationAdmissionError(404, reason);
      return { decision: "admit", operation_id: request.operation_id, claim_generation: request.claim_generation };
    });
    const r = runner(fixture.executor, fakeGitlab().gitlab, undefined,
      { codexOperationWaitPolicy: { now: () => clock.now, sleep: clock.sleep } });
    const done = r.execute(claim);
    let ended = false;
    void done.then(() => { ended = true; });
    try {
      assert.ok(await until(() => gates(claim.run_id).length === 1));
      const source = structuredClone(gates(claim.run_id)[0]!);
      const [revision] = send(claim.run_id, row("revise_plan", "pending typed refusal"));
      assert.ok(await until(() => ended || clock.sleepers.size > 0));
      if (reason === "route_uncertain") {
        assert.equal(ended, false, "untyped 404 cannot imply authority loss");
        assert.equal([...clock.sleepers][0]!.at, 15000);
      } else {
        assert.equal(ended, true, "typed refusal stops this root");
      }
      assert.equal(fixture.starts.length, 1);
      assert.equal(reads, 2);
      assert.deepEqual(gates(claim.run_id), [source]);
      assert.equal(api.isApplied(claim.run_id, revision!.id), false);
      if (!ended) send(claim.run_id, row("cancel"));
      await done;
    } finally { r.shutdown(); await done; }
  });

const modes = ["initial", "implementation", "human revision", "clarification", "prose", "retry"] as const;
for (const mode of modes) it(`M2 real subscription runner: ${mode} cannot send at cooldown boundary until its own admit`, async t => {
  enable();
  const clock = new AdmissionClock();
  const registry = new ActiveRunRegistry();
  const script = (n: number): Frame => {
    if (mode === "clarification" && n === 1) return { ask: true };
    if (mode === "prose" && n === 1) return { prose: "Here is a prose-only draft." };
    if (mode === "retry" && n === 1) return { failure: true };
    if (mode === "implementation" && n > 1) return { done: true };
    return { plan: n === 1 ? "# source" : "# revised" };
  };
  const fixture = providerFixture("codex", script, false, { subscription: true });
  const claim = freshClaim(7, { config: { plan_max_revisions: 3 },
    secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } });
  const id = claim.run_id;
  const target = mode === "initial" ? 1 : 2;
  const reads: CodexOperationRequest[] = [];
  let reply!: () => void;
  let held = false;
  t.mock.method(client, "checkCodexOperationAdmission", async (_id: string, request: CodexOperationRequest) => {
    reads.push(structuredClone(request));
    if (reads.length === target) return { decision: "wait", ...request, class: "overload",
      episode_id: "00000000-0000-4000-8000-000000000001",
      server_now: new Date(0).toISOString(), eligible_at: new Date(1000).toISOString() };
    if (reads.length === target + 1) {
      held = true;
      await new Promise<void>(resolve => { reply = resolve; });
    }
    return { decision: "admit", operation_id: request.operation_id, claim_generation: request.claim_generation };
  });
  const r = runner(fixture.executor, fakeGitlab().gitlab, undefined, { activeRuns: registry,
    codexOperationWaitPolicy: { now: () => clock.now, sleep: clock.sleep } });
  const done = r.execute(claim);
  let inputId: number | undefined;
  try {
    if (mode === "human revision" || mode === "implementation") {
      assert.ok(await until(() => gates(id).length === 1));
      const [input] = send(id, row(mode === "human revision" ? "revise_plan" : "approve_plan",
        mode === "human revision" ? "feedback exactly once" : null));
      inputId = input!.id;
    }
    if (mode === "clarification") {
      assert.ok(await until(() => api.states.some(s => s.runId === id && s.body.status === "awaiting_input")));
      const q = api.states.find(s => s.runId === id && s.body.status === "awaiting_input")!.body.open_question_id;
      send(id, row("answer", JSON.stringify({ question_id: q, answers: ["server"] })));
    }
    assert.ok(await until(() => clock.sleepers.size > 0), "root reached real admission");
    assert.equal(fixture.starts.length, target - 1);
    const snapshot = registry.build().active.find(e => e.run_id === id)!;
    assert.equal(snapshot.claim_generation, 7);
    assert.equal(snapshot.account_cooldown?.operation_id, reads.at(-1)!.operation_id);
    if (mode === "human revision") {
      assert.equal(reads.at(-1)!.source?.revision_input_id, inputId);
      assert.equal(api.isApplied(id, inputId!), false);
    }
    clock.advance(999);
    assert.equal(fixture.starts.length, target - 1);
    clock.advance(1);
    assert.ok(await until(() => held));
    assert.equal(fixture.starts.length, target - 1, "timer expiry is not an admit");
    assert.equal(reads.at(-1)!.operation_id, reads[target - 1]!.operation_id);
    reply();
    assert.ok(await until(() => fixture.starts.length === target));
    if (target > 1) assert.equal(fixture.starts[0]!.threadId, fixture.starts[1]!.threadId);
    if (mode === "human revision") {
      assert.ok(await until(() => gates(id).some(g => g.plan_md === "# revised")));
      assert.equal(fixture.starts[1]!.prompt.split("feedback exactly once").length - 1, 1);
      assert.ok(api.isApplied(id, inputId!));
    }
    send(id, row("cancel"));
    await done;
    assert.equal(reads.length, target + 1);
  } finally { reply?.(); r.shutdown(); await done; }
});

it("M2 sticky cancel wakes rearmed admission after a refused pause spent the shared signal", async t => {
  enable();
  const clock = new AdmissionClock();
  const fixture = providerFixture("codex", () => ({ done: true }), false, { subscription: true });
  const claim = freshClaim(7, { plan_approved: true, plan_md: "# approved",
    secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } });
  let context!: RunContext;
  let refusals = 0, reads = 0;
  const run = fixture.executor.run.bind(fixture.executor);
  t.mock.method(fixture.executor, "run", async (ctx: RunContext) => {
    context = ctx;
    ctx.planApproved = true;
    // Model the existing refused owner park; no capture or publication is claimed here.
    const pauseMode = ctx.pauseModeRequested;
    ctx.pauseModeRequested = () => refusals ? null : pauseMode?.() ?? null;
    ctx.parkForPause = async () => { refusals++; return false; };
    return run(ctx);
  });
  t.mock.method(client, "checkCodexOperationAdmission", async (_id: string, request: CodexOperationRequest) => {
    reads++;
    return { decision: "wait", ...request, class: "overload",
      episode_id: "00000000-0000-4000-8000-000000000093",
      server_now: new Date(0).toISOString(), eligible_at: new Date(15000).toISOString() };
  });
  const r = runner(fixture.executor, fakeGitlab().gitlab, undefined,
    { codexOperationWaitPolicy: { now: () => clock.now, sleep: clock.sleep } });
  const done = r.execute(claim);
  let ended = false;
  void done.then(() => { ended = true; });
  try {
    assert.ok(await until(() => clock.sleepers.size === 1));
    send(claim.run_id, row("pause", "now"));
    assert.ok(await until(() => refusals === 1 && clock.sleepers.size === 1), JSON.stringify({ refusals, ended, reads, states: api.states, sleepers: clock.sleepers.size }));
    assert.equal(context.signal?.aborted, true, "the shared signal was spent by pause");
    send(claim.run_id, row("cancel"));
    assert.ok(await until(() => ended), "sticky cancel ends the rearmed wait without a clock tick");
    assert.equal(clock.now, 0);
    assert.equal(reads, 1, "refusal preserves the shared scheduler deadline");
    assert.equal(fixture.starts.length, 0);
  } finally { r.shutdown(); await done; }
});

for (const control of ["cancel", "shutdown", "reject"] as const)
  it(`M2 real subscription runner: ${control} wakes admission without another policy read`, async t => {
    enable();
    const clock = new AdmissionClock();
    const fixture = providerFixture("codex", n => ({ plan: n === 1 ? "# source" : "# revised" }), false, { subscription: true });
    const claim = freshClaim(2, { secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } });
    let reads = 0;
    t.mock.method(client, "checkCodexOperationAdmission", async (_id: string, request: CodexOperationRequest) => {
      reads++;
      return reads === 1 ? { decision: "admit", operation_id: request.operation_id, claim_generation: request.claim_generation }
        : { decision: "wait", ...request, class: "rate_limit", episode_id: "00000000-0000-4000-8000-000000000002",
          server_now: new Date(0).toISOString(), eligible_at: new Date(15000).toISOString() };
    });
    const r = runner(fixture.executor, fakeGitlab().gitlab, undefined,
      { codexOperationWaitPolicy: { now: () => clock.now, sleep: clock.sleep } });
    const done = r.execute(claim);
    try {
      assert.ok(await until(() => gates(claim.run_id).length === 1));
      send(claim.run_id, row("revise_plan", "pending feedback"));
      assert.ok(await until(() => clock.sleepers.size === 1));
      if (control === "shutdown") r.shutdown();
      else send(claim.run_id, row(control === "reject" ? "reject_plan" : "cancel"));
      assert.ok(await until(() => clock.sleepers.size === 0), "control ended the wait without virtual time advancing");
      await done;
      assert.equal(fixture.starts.length, 1);
      assert.equal(reads, 2);
    } finally { r.shutdown(); await done; }
  });

// Retained reviewer/tester/auditor repros: invalidate both sides of the awaited proof.
for (const boundary of ["source proof", "final guard"] as const)
  for (const change of ["rollback", "re-registration"] as const)
    it(`M2 ${change} during ${boundary} retains feedback until a fresh admit`, async t => {
      enable();
      const features = [...client.protocolFeatures];
      const clock = new AdmissionClock();
      const fixture = providerFixture("codex", n => ({ plan: n === 1 ? "# source" : "# revised" }), false, { subscription: true });
      const claim = freshClaim(7, { secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } });
      let reads = 0, holdNextProof = false;
      let release: (() => void) | undefined;
      t.mock.method(client, "checkCodexOperationAdmission", async (_id: string, request: CodexOperationRequest) => {
        if (++reads === 2 && boundary === "source proof") holdNextProof = true;
        return { decision: "admit", operation_id: request.operation_id, claim_generation: request.claim_generation };
      });
      const ownership = client.getRunOwnership.bind(client);
      t.mock.method(client, "getRunOwnership", async (...args: Parameters<typeof client.getRunOwnership>) => {
        if (holdNextProof) {
          holdNextProof = false;
          await new Promise<void>(resolve => { release = resolve; });
        }
        return ownership(...args);
      });
      if (boundary === "final guard") {
        const run = fixture.executor.run.bind(fixture.executor);
        t.mock.method(fixture.executor, "run", async (ctx: RunContext) => {
          const prepare = ctx.prepareCodexRootDispatch!;
          let preparations = 0;
          ctx.prepareCodexRootDispatch = async (...args) => {
            const permit = await prepare(...args);
            if (++preparations === 2) await new Promise<void>(resolve => { release = resolve; });
            return permit;
          };
          return run(ctx);
        });
      }
      const r = runner(fixture.executor, fakeGitlab().gitlab, undefined,
        { codexOperationWaitPolicy: { now: () => clock.now, sleep: clock.sleep } });
      const done = r.execute(claim);
      try {
        assert.ok(await until(() => gates(claim.run_id).length === 1));
        const [feedback] = send(claim.run_id, row("revise_plan", "retain pending feedback"));
        assert.ok(await until(() => !!release));
        assert.equal(reads, 2);
        assert.equal(fixture.starts.length, 1);
        client.clearFeatures();
        if (change === "re-registration") client.protocolFeatures = features;
        release!();
        assert.ok(await until(() => fixture.starts.length > 1 || clock.sleepers.size > 0));
        assert.equal(fixture.starts.length, 1);
        assert.equal(api.isApplied(claim.run_id, feedback!.id), false);
        assert.equal(gates(claim.run_id).length, 1);
        client.protocolFeatures = features;
        clock.advance(14999);
        assert.equal(reads, 2);
        assert.equal(fixture.starts.length, 1);
        clock.advance(1);
        assert.ok(await until(() => gates(claim.run_id).length === 2));
        assert.equal(reads, 3);
        assert.equal(fixture.starts.length, 2);
        assert.equal(fixture.starts[1]!.threadId, fixture.starts[0]!.threadId);
        assert.equal(fixture.starts[1]!.prompt.split("retain pending feedback").length - 1, 1);
        assert.equal(api.isApplied(claim.run_id, feedback!.id), true);
      } finally {
        client.protocolFeatures = features;
        release?.();
        send(claim.run_id, row("cancel"));
        r.shutdown();
        await done;
      }
    });

it("M2 re-registration plus source replacement at final guard preserves unavailable deadline", async t => {
  enable();
  const features = [...client.protocolFeatures];
  const clock = new AdmissionClock();
  const fixture = providerFixture("codex", n => ({ plan: n === 1 ? "# source" : "# replacement" }), false, { subscription: true });
  const claim = freshClaim(7, { config: { plan_max_revisions: 3 }, secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } });
  let reads = 0;
  let release: (() => void) | undefined;
  t.mock.method(client, "checkCodexOperationAdmission", async (_id: string, request: CodexOperationRequest) => {
    reads++;
    return { decision: "admit", operation_id: request.operation_id, claim_generation: request.claim_generation };
  });
  const run = fixture.executor.run.bind(fixture.executor);
  t.mock.method(fixture.executor, "run", async (ctx: RunContext) => {
    const prepare = ctx.prepareCodexRootDispatch!;
    let preparations = 0;
    ctx.prepareCodexRootDispatch = async (...args) => {
      const permit = await prepare(...args);
      if (++preparations === 2) await new Promise<void>(resolve => { release = resolve; });
      return permit;
    };
    return run(ctx);
  });
  const r = runner(fixture.executor, fakeGitlab().gitlab, undefined,
    { codexOperationWaitPolicy: { now: () => clock.now, sleep: clock.sleep } });
  const done = r.execute(claim);
  try {
    assert.ok(await until(() => gates(claim.run_id).length === 1));
    send(claim.run_id, row("revise_plan", "old feedback"));
    assert.ok(await until(() => !!release));
    client.clearFeatures();
    client.protocolFeatures = features;
    const [replacement] = send(claim.run_id, row("revise_plan", "new feedback"));
    assert.ok(await until(() => api.isAcked(claim.run_id, replacement!.id)));
    release!();
    assert.ok(await until(() => reads > 2 || clock.sleepers.size > 0));
    assert.equal(reads, 2, "obsolete permit must establish 15s unavailable deadline before replacement reads");
    assert.equal(fixture.starts.length, 1);
    clock.advance(14999);
    assert.equal(reads, 2);
    clock.advance(1);
    assert.ok(await until(() => gates(claim.run_id).length === 2));
  } finally {
    client.protocolFeatures = features;
    release?.();
    send(claim.run_id, row("cancel"));
    r.shutdown();
    await done;
  }
});

it("M3 revision child and advice reuse the immutable source without spending another revision", async t => {
  enable();
  const fixture = providerFixture("codex", n => ({ plan: n === 1 ? "# original" : "# revised" }), false, { subscription: true });
  const reads: CodexOperationRequest[] = [];
  t.mock.method(client, "checkCodexOperationAdmission", async (_id: string, request: CodexOperationRequest) => {
    reads.push(structuredClone(request));
    return { decision: "admit", operation_id: request.operation_id, claim_generation: request.claim_generation };
  });
  const original = fixture.executor.run.bind(fixture.executor);
  t.mock.method(fixture.executor, "run", async (ctx: RunContext) => {
    const prepare = ctx.prepareCodexRootDispatch!;
    ctx.prepareCodexRootDispatch = async (signal, check, kind) => {
      const permit = await prepare(signal, check, kind);
      if (reads.at(-1)?.source && (!kind || kind === "root")) {
        assert.equal((await prepare(signal, check, "child"))(), true);
        assert.equal((await prepare(signal, check, "advice"))(), true);
      }
      return permit;
    };
    return original(ctx);
  });
  const claim = freshClaim(7, { config: { plan_max_revisions: 1 },
    secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } });
  const r = runner(fixture.executor, fakeGitlab().gitlab);
  const done = r.execute(claim);
  try {
    assert.ok(await until(() => gates(claim.run_id).length === 1));
    const [revision] = send(claim.run_id, row("revise_plan", "one revision only"));
    assert.ok(await until(() => gates(claim.run_id).length === 2));
    const revisionReads = reads.filter(read => !!read.source);
    assert.deepEqual(revisionReads.map(read => read.kind), ["root", "child", "advice"]);
    for (const read of revisionReads) assert.deepEqual(read.source, {
      gate_revision: 1,
      presentation_id: gates(claim.run_id)[0]!.presentation_id,
      revision_input_id: revision!.id,
    });
    assert.equal(new Set(revisionReads.map(read => read.operation_id)).size, 3);
    assert.equal(fixture.starts.length, 2);
    assert.ok(api.isApplied(claim.run_id, revision!.id));
    send(claim.run_id, row("cancel"));
    await done;
  } finally { r.shutdown(); await done; }
});

for (const kind of ["child", "advice"] as const) {
  it(`M3 ${kind === "child" ? "child admission" : "actual advice"} snapshot labels only required work`, async t => {
    enable();
    const clock = new AdmissionClock();
    const registry = new ActiveRunRegistry();
    const fixture = providerFixture("codex", () => ({ plan: "# Plan" }), false, { subscription: true });
    let waited = false;
    t.mock.method(client, "checkCodexOperationAdmission", async (_id: string, request: CodexOperationRequest) => {
      if (request.kind === kind && !waited) {
        waited = true;
        return { decision: "wait", ...request, class: "overload", episode_id: "00000000-0000-4000-8000-000000000091",
          server_now: new Date(0).toISOString(), eligible_at: new Date(1000).toISOString() };
      }
      return { decision: "admit", operation_id: request.operation_id, claim_generation: request.claim_generation };
    });
    const original = fixture.executor.run.bind(fixture.executor);
    t.mock.method(fixture.executor, "run", async (ctx: RunContext) => {
      const permit = await ctx.prepareCodexRootDispatch!(new AbortController().signal, () => {}, kind);
      assert.equal(permit(), true);
      return original(ctx);
    });
    const claim = freshClaim(7, { secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } });
    const r = runner(fixture.executor, fakeGitlab().gitlab, undefined, { activeRuns: registry,
      codexOperationWaitPolicy: { now: () => clock.now, sleep: clock.sleep } });
    const done = r.execute(claim);
    try {
      assert.ok(await until(() => clock.sleepers.size === 1));
      const snapshot = registry.build().active.find(entry => entry.run_id === claim.run_id)!;
      assert.equal(snapshot.account_cooldown?.kind, kind === "child" ? "child" : undefined);
      clock.advance(1000);
      assert.ok(await until(() => gates(claim.run_id).length === 1));
      assert.equal(registry.build().active.find(entry => entry.run_id === claim.run_id)?.account_cooldown, undefined);
      send(claim.run_id, row("cancel"));
      await done;
    } finally { r.shutdown(); await done; }
  });
}
