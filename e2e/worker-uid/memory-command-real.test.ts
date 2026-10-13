import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { access, mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { promisify } from "node:util";
import { CodexCallbackBroker, type CapturedMemoryContext } from "../../agent/src/codex/broker.js";
import { makeDefaultSpawnCommand } from "../../agent/src/codex/codex-executor.js";
import { launchCodexEffectRoot, type CodexRootHandle, type DisposeOutcome } from "../../agent/src/codex/launcher.js";
import { boundedCommandWait } from "../../agent/src/codex/memory-command.js";
import { projectToolOutput } from "../../agent/src/codex/projection.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../../agent/src/codex/registry.js";
import { COMMAND_UID } from "../../agent/src/runner-uid.js";
import { WorkerMemoryCommands } from "../../agent/src/worker-memory-commands.js";
import type { WorkerMemoryPressureResult } from "../../agent/src/protocol.js";

const root = fileURLToPath(new URL("../..", import.meta.url));
const budgetMs = 5000;
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}
async function ready(predicate: () => boolean) {
  // At most 250 attempts / five seconds. Failure stops setup; finally drains both roots.
  for (let i = 0; i < 250; i++) {
    if (predicate()) return;
    await new Promise((done) => setTimeout(done, 20));
  }
  assert.fail("real command enrollment prerequisite: five-second readiness deadline");
}

test("real adopted detached tree: larger selection, exact pressure failure and sibling survival", { timeout: 60000 }, async () => {
  assert.equal(process.platform, "linux", "prerequisite: Linux supervisor");
  assert.equal(process.getuid?.(), COMMAND_UID,
    "prerequisite: run this bounded command-only proof as cap-less command uid 10003");
  const scratch = path.join(root, ".uzi/scratch");
  await mkdir(scratch, { recursive: true });
  const dir = await mkdtemp(path.join(scratch, "memory-real."));
  const commands = new WorkerMemoryCommands();
  const flight = {};
  const context = commands.begin({
    run_id: randomUUID(), worker_id: randomUUID(), register_nonce: "real-fixture",
    claim_generation: 1, memory_episode: 0, flight, lifecycleRevision: 1,
  });
  const captured: CapturedMemoryContext = { commands, context, flight, lifecycleRevision: 1 };
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const releases = [deferred<void>(), deferred<void>()];
  const handles: CodexRootHandle[] = [];
  const primaryExits: Promise<{ event: "child_exit"; code: number }>[] = [];
  const disposals: (DisposeOutcome | undefined)[] = [];
  const evidence: Awaited<ReturnType<CodexRootHandle["observe"]>>[] = [];
  const replies: Promise<Awaited<ReturnType<CodexCallbackBroker["handleToolCall"]>>>[] = [];
  try {
    const bins = path.join(dir, "bins");
    await mkdir(bins);
    // One bounded build of just the two helpers; no installs, production edits or full gates.
    await promisify(execFile)("go", ["build", "-mod=vendor", "-o", bins + "/", ".", "./cmdsandbox"], {
      cwd: path.join(root, "agent/codex/supervisor"), timeout: 30000,
      env: { ...process.env, GOFLAGS: "-buildvcs=false", TMPDIR: dir }, maxBuffer: 64 * 1024,
    });
    const fixture = path.join(dir, "tree.mjs");
    await writeFile(fixture, `
import { spawn } from "node:child_process";
import { existsSync, writeFileSync } from "node:fs";
const count = Number(process.argv[2]);
const gate = process.argv[3];
writeFileSync(gate + ".ready", "");
// Bounded setup handshake: stable primary first, then fork after broker enrollment.
for (let i = 0; !existsSync(gate); i++) {
  if (i === 250) throw new Error("setup handshake deadline");
  await new Promise((done) => setTimeout(done, 20));
}
const bytes = count === 3 ? 8 * 1024 * 1024 : 1024 * 1024;
const leaf = "globalThis.resident = Buffer.alloc(Number(process.argv[1]), 1); process.send('ready'); process.disconnect(); setTimeout(() => {}, 20000);";
const children = Array.from({ length: count }, () => spawn(process.execPath, ["-e", leaf, String(bytes)], {
  detached: true, stdio: ["ignore", "ignore", "ignore", "ipc"], env: { PATH: "/usr/bin:/bin" },
}));
await Promise.all(children.map((child) => new Promise((resolve, reject) => {
  child.once("error", reject);
  child.once("exit", () => reject(new Error("leaf exited before readiness")));
  child.once("message", () => { child.unref(); resolve(); });
})));
console.log(JSON.stringify(children.map((child) => child.pid)));
process.exit(7);
`);
    const launch = async (spec: Parameters<typeof launchCodexEffectRoot>[0]) => {
      const index = handles.length;
      assert.ok(index < 2, "fixture admits exactly two supervisor roots");
      // Profile injection only: this worker is already command uid, so no reuid is needed.
      // The REAL supervisor still verifies caps/NNP/subreaper/nondumpability before fork.
      // This does not prove the worker-to-command uid transition or M3 runtime wiring.
      const handle = await launchCodexEffectRoot({ ...spec, command: path.join(bins, "cmdsandbox") }, {
        env: { UZI_UID_SPLIT: "1" },
        helperBinsForTest: { supervisor: path.join(bins, "codex-supervisor"), crossCheckSandbox: "" },
        deadlines: { started: budgetMs },
      });
      handles.push(handle);
      const exit = handle.waitChild(budgetMs);
      primaryExits.push(exit);
      void exit.catch(() => {});
      return {
        ...handle,
        get failed() { return handle.failed; },
        observe: async (ms?: number) => {
          // Only fixture-owned readiness files; never inspect process tables or environments.
          const gate = path.join(dir, `go-${index === 0 ? 3 : 1}.ready`);
          let present = false;
          // Up to 250 file checks / five seconds. Failure blocks this enrollment only.
          for (let attempt = 0; attempt < 250; attempt++) {
            try { await access(gate); present = true; break; }
            catch (error) {
              if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
            }
            await new Promise((done) => setTimeout(done, 20));
          }
          assert.ok(present, "prerequisite: fixture primary readiness handshake");
          const observation = await handle.observe(ms);
          evidence[index] = observation;
          return observation;
        },
        // Controlled ordinary-cleanup barrier only. The actual primary exits and the
        // actual supervisor retains adopted leaves until memory cancellation disposes it.
        waitChild: async () => {
          const result = await exit;
          await boundedCommandWait(releases[index]!.promise, 15000);
          return result;
        },
        dispose: async (ms?: number) => {
          const outcome = await handle.dispose(ms);
          disposals[index] = outcome;
          return outcome;
        },
      };
    };
    let spawnFailure: unknown;
    const spawnCommand = makeDefaultSpawnCommand(registry, launch, budgetMs, root,
      { PATH: path.dirname(process.execPath) + ":/usr/bin:/bin" }, "off", undefined, undefined, 15000, captured);
    const broker = new CodexCallbackBroker({
      registry, capturedMemory: captured, worktreePath: root,
      spawnCommand: async (argv, opts) => {
        try { return await spawnCommand(argv, opts); }
        catch (error) { spawnFailure = error; throw error; }
      },
      grants: { role: "coder", phase: "implement", isRoot: true,
        allowedTools: new Set(["Bash"]), allowedSkills: new Set() },
      fileop: { op: async () => ({ ok: false }) }, delegate: async () => ({ ok: false }),
    });
    // Only fixed, inert fixture commands are executed; no candidate screening payloads.
    for (const [index, count] of [3, 1].entries()) {
      const gate = path.join(dir, `go-${count}`);
      const quote = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'";
      const command = `${quote(process.execPath)} ${quote(fixture)} ${count} ${quote(gate)}`;
      let earlyReply: Awaited<ReturnType<CodexCallbackBroker["handleToolCall"]>> | undefined;
      const reply = broker.handleToolCall({
        threadId: "real-thread", turnId: "real-turn", callId: `real-call-${index}`,
      }, "Bash", { command }, "root");
      replies.push(reply);
      void reply.then((result) => { earlyReply = result; });
      await ready(() => {
        if (earlyReply) assert.fail("real launch/enrollment prerequisite: " +
          String(spawnFailure instanceof Error ? spawnFailure.stack : spawnFailure) + " " +
          JSON.stringify({ reply: earlyReply, evidence }));
        return handles.length === index + 1 && commands.eligibleEntries()?.length === index + 1;
      });
      await writeFile(gate, "");
      assert.equal((await boundedCommandWait(primaryExits[index]!, budgetMs)).code, 7);
    }
    assert.equal(registry.rootCount(), 2, "both actual supervisors are registered");
    const [large, sibling] = commands.eligibleEntries()!;
    assert.ok(large && sibling);
    for (const [index, entry] of [large, sibling].entries()) {
      const evidence = await handles[index]!.observe(budgetMs);
      assert.equal(evidence.state, "complete");
      if (evidence.state !== "complete") assert.fail("prerequisite: complete real supervisor observation");
      assert.deepEqual(evidence.supervisor, entry.root);
      assert.deepEqual(evidence.root, entry.primary);
      assert.equal(evidence.processes.length, index === 0 ? 3 : 1);
      assert.ok(evidence.processes.every((row) => row.ppid === entry.root.pid),
        "detached leaves were adopted after the primary exited");
      assert.ok(evidence.processes.every((row) => row.pid !== entry.primary?.pid));
    }
    const observations = await Promise.all([large.observe(), sibling.observe()]);
    const selection = commands.select([
      { entry: large, observation: observations[0]! },
      { entry: sibling, observation: observations[1]! },
    ], performance.now(), budgetMs);
    assert.equal(selection?.entry, large, "actual larger aggregate RSS wins");
    assert.ok(selection);
    commands.prepareOutcome(large);
    await large.cancel({ remainingMs: budgetMs, signal: new AbortController().signal, authorized: () => true });
    assert.equal(commands.hasConfirmedStop(large), true);
    assert.equal(disposals[0]?.clean, true);
    assert.equal(disposals[0]?.event?.state, "drained");
    assert.equal(disposals[0]?.event?.authority, "ECHILD+__WALL");
    assert.equal(registry.hasLiveCommandRoot(), true, "sibling remains registered and live");
    assert.equal(disposals[1], undefined);
    const survivor = await handles[1]!.observe(budgetMs);
    assert.equal(survivor.state, "complete");
    if (survivor.state !== "complete") assert.fail("sibling observation unavailable");
    assert.equal(survivor.processes.length, 1);
    assert.deepEqual(survivor.processes.map(({ pid, startTime }) => ({ pid, startTime })),
      observations[1]!.rows.map(({ pid, startTime }) => ({ pid, startTime })),
      "the same sibling identity survives the selected drain");
    const feedback: WorkerMemoryPressureResult = {
      run_id: context.run_id, worker_id: context.worker_id, claim_generation: 1, memory_episode: 0,
      intervention_id: randomUUID(), tool_id: large.toolId, attempt_id: large.attemptId,
      command_id: large.commandId, command: large.command, code: "worker_memory_pressure",
      observed_total_bytes: 900, limit_bytes: 1000, tree_rss_bytes_approx: selection.rssBytesApprox,
      outcome: "confirmed_drained", cancellation_confirmed: true, retry_blocked: true,
      guidance: "Use smaller or serial commands after recovery.",
    };
    assert.equal(commands.resolveToolOutcome(large, { ...feedback, attempt_id: sibling.attemptId }), false);
    assert.equal(commands.resolveToolOutcome(large, feedback), true);
    releases[0]!.resolve();
    const reply = await boundedCommandWait(replies[0]!, budgetMs);
    assert.equal(reply.ok, false);
    if (reply.ok) assert.fail("exact structured pressure failure expected");
    assert.equal(reply.code, "worker_memory_pressure");
    assert.deepEqual(reply.memoryPressure, feedback);
    assert.deepEqual(JSON.parse(projectToolOutput(reply, (s) => s)), feedback);
    releases[1]!.resolve();
    const siblingReply = await boundedCommandWait(replies[1]!, budgetMs);
    assert.equal(siblingReply.ok, true, "ordinary completion retains the shell output envelope");
    if (!siblingReply.ok) assert.fail("ordinary shell output expected");
    assert.equal((siblingReply.output as { code: number }).code, 7);
    assert.equal(registry.hasLiveCommandRoot(), false);
    assert.equal(registry.isPoisoned(), false);
    assert.equal(commands.eligibleEntries()?.length, 0);
  } finally {
    for (const release of releases) release.resolve();
    // Exactly two possible handles, five seconds each; failure never blocks sibling cleanup.
    const cleanup = await Promise.allSettled(handles.map((handle) => handle.dispose(budgetMs)));
    const joined = await Promise.allSettled(replies.map((reply) => boundedCommandWait(reply, budgetMs)));
    await rm(dir, { recursive: true, force: true });
    for (const result of cleanup) {
      assert.equal(result.status, "fulfilled", "owned supervisor cleanup must settle");
      if (result.status === "fulfilled") assert.equal(result.value.clean, true, "cleanup must attest drain");
    }
    for (const result of joined) assert.equal(result.status, "fulfilled", "owned broker callback must settle");
  }
});
