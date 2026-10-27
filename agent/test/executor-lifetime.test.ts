import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { ExecutorLifetime, terminalSealMaterial, terminalSealStillAccepted, type ExecutorTerminalSeal } from "../src/codex/executor-lifetime.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { createCodexExecutionSafety, type CodexExecutionSafetyImpl } from "../src/codex/safety.js";
import { CodexCallbackBroker, type FileopRequest, type FileopResponse, type RunGrants } from "../src/codex/broker.js";

// #3404 M1b: the closed Codex executor lifetime. These tests drive the lifetime owner, a real
// registry and facade, and the real broker/auth admission seams with barriers (no sleeps).

function barrier() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}
const flush = () => new Promise<void>(resolve => setImmediate(resolve));
const never = async (): Promise<never> => { throw new Error("no root lane in this test"); };

function origin(lifetime: ExecutorLifetime): { registry: ExecutionRegistry; facade: CodexExecutionSafetyImpl } {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(0), lifetime);
  return { registry, facade: createCodexExecutionSafety(registry, never, never) };
}
/** The production teardown order: quiesce, reap, then dispose through the facade. */
async function disposeOrigin(o: { registry: ExecutionRegistry; facade: CodexExecutionSafetyImpl }): Promise<void> {
  if (o.registry.state() !== "poisoned") {
    const closed = await o.registry.quiesceChildren(1000);
    assert.equal(closed.kind, "quiescent");
    if (closed.kind === "quiescent") assert.equal((await o.registry.reapProcesses(1000, closed.epoch)).kind, "observed_empty");
  }
  const outcome = await o.facade.dispose({ boundary: "terminal", deadlineMs: 1000 });
  assert.equal(outcome.kind, "disposed");
}
const settled = async (): Promise<void> => undefined;

describe("ExecutorLifetime (#3404 M1b)", () => {
  it("closes admission synchronously: a factory called after seal() is refused before it is invoked", async () => {
    const lifetime = new ExecutorLifetime();
    const o = origin(lifetime);
    await disposeOrigin(o);
    let invoked = 0;
    const sealing = lifetime.seal({}, settled, () => true);
    // No await between seal() and the call: the fence is already shut.
    const refused = lifetime.admit(async () => { invoked++; });
    await assert.rejects(refused, /lifetime closed/);
    // A fire-and-forget caller's `.catch` must see the refusal, never a synchronous throw.
    assert.doesNotThrow(() => { void lifetime.admit(async () => { invoked++; }).catch(() => undefined); });
    assert.equal(lifetime.isOpen(), false);
    assert.equal(o.registry.reserveLaunch("command").kind, "denied");
    assert.throws(() => new ExecutionRegistry(newLocalExecutionEpoch(0), lifetime), /lifetime closed/);
    assert.equal((await sealing).kind, "sealed");
    assert.equal(invoked, 0);
  });

  it("a call admitted before the seal is joined, and a seal never completes before it settles", async () => {
    const lifetime = new ExecutorLifetime();
    const o = origin(lifetime);
    await disposeOrigin(o);
    const entered = barrier(), release = barrier();
    const work = lifetime.admit(async () => { entered.resolve(); await release.promise; });
    await entered.promise;
    let sealed = false;
    const sealing = lifetime.seal({}, settled, () => true).then(r => { sealed = true; return r; });
    await flush();
    assert.equal(sealed, false);
    release.resolve();
    await work;
    assert.equal((await sealing).kind, "sealed");
  });

  it("every registry origin is inventoried: one undisposed registry makes the seal unproven", async () => {
    // The five executor origins (capture, summary, provision, installer, provider) all pass the
    // lifetime to the constructor; a single undisposed one is therefore always visible.
    for (const origins of [1, 2, 5]) {
      for (let undisposed = 0; undisposed < origins; undisposed++) {
        const lifetime = new ExecutorLifetime();
        const all = Array.from({ length: origins }, () => origin(lifetime));
        for (let i = 0; i < all.length; i++) if (i !== undisposed) await disposeOrigin(all[i]!);
        assert.equal((await lifetime.seal({}, settled, () => true)).kind, "unproven", `origin ${undisposed} of ${origins}`);
      }
    }
  });

  it("a facade that never bound (or an unknown facade) cannot be proven", async () => {
    const lifetime = new ExecutorLifetime();
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(0), lifetime);
    await registry.quiesceChildren(100);
    assert.equal((await registry.disposeTools(100)).kind, "disposed");
    assert.equal((await lifetime.seal({}, settled, () => true)).kind, "unproven");
    assert.throws(() => lifetime.bindFacade({}, {}, () => true), /unknown Codex facade/);
  });

  it("an intentionally capture-drained registry keeps its original disposal facts", async () => {
    const lifetime = new ExecutorLifetime();
    const o = origin(lifetime);
    const observation = await o.registry.settleForCapture(500, 5);
    assert.equal(observation.kind, "observed_empty");
    assert.equal(o.registry.state(), "poisoned", "capture poison is intentional");
    await disposeOrigin(o);
    const source = {};
    const result = await lifetime.seal(source, settled, () => true);
    assert.equal(result.kind, "sealed");
    if (result.kind !== "sealed") return;
    assert.equal(lifetime.accepts(result.seal, source), true);
  });

  it("a registry poisoned after its disposal revokes the proof (before and after sealing)", async () => {
    const before = new ExecutorLifetime();
    const a = origin(before);
    await disposeOrigin(a);
    a.registry.poison({ category: "tool", message: "late root custody" });
    assert.equal((await before.seal({}, settled, () => true)).kind, "unproven");

    const after = new ExecutorLifetime();
    const b = origin(after);
    await disposeOrigin(b);
    const source = {};
    const result = await after.seal(source, settled, () => true);
    assert.equal(result.kind, "sealed");
    if (result.kind !== "sealed") return;
    b.registry.poison({ category: "tool", message: "rejected root arrived after disposal" });
    assert.equal(after.accepts(result.seal, source), false, "revocable witness is rechecked at every accepts()");
  });

  it("a failed or rejected cleanup is unproven forever and cannot be re-sealed", async () => {
    const lifetime = new ExecutorLifetime();
    const o = origin(lifetime);
    await disposeOrigin(o);
    const failing = Promise.reject(new Error("transport close failed"));
    lifetime.retain(failing).catch(() => undefined);
    const first = lifetime.seal({}, settled, () => true);
    assert.equal((await first).kind, "unproven");
    // The same promise, a different source use, and a settle callback that now succeeds.
    assert.deepEqual(await lifetime.seal({}, settled, () => true), await first);
    assert.equal(lifetime.isOpen(), false);
    assert.throws(() => lifetime.assertOpen(), /lifetime closed/);
  });

  it("a settle callback that throws, or an ineligible owner, is unproven but still joins every tail", async () => {
    for (const mode of ["throws", "ineligible"] as const) {
      const lifetime = new ExecutorLifetime();
      const o = origin(lifetime);
      await disposeOrigin(o);
      const gate = barrier();
      lifetime.admit(async () => { await gate.promise; }).catch(() => undefined);
      let done = false;
      const sealing = lifetime.seal({}, mode === "throws" ? async () => { throw new Error("boom"); } : settled,
        () => mode !== "ineligible").then(r => { done = true; return r; });
      await flush();
      assert.equal(done, false, `${mode}: tail still pending`);
      gate.resolve();
      assert.equal((await sealing).kind, "unproven");
    }
  });

  it("a second seal call for a different source use is unproven, the same one repeats the result", async () => {
    const lifetime = new ExecutorLifetime();
    await disposeOrigin(origin(lifetime));
    const first = {}, second = {};
    const sealed = await lifetime.seal(first, settled, () => true);
    assert.equal(sealed.kind, "sealed");
    assert.equal(await lifetime.seal(first, settled, () => true), sealed);
    assert.equal((await lifetime.seal(second, settled, () => true)).kind, "unproven");
    if (sealed.kind === "sealed") {
      assert.equal(lifetime.accepts(sealed.seal, first), true);
      assert.equal(lifetime.accepts(sealed.seal, second), false);
    }
  });

  it("a non-object source use is unproven", async () => {
    const lifetime = new ExecutorLifetime();
    const o = origin(lifetime);
    await disposeOrigin(o);
    assert.equal((await lifetime.seal("run-1" as never, settled, () => true)).kind, "unproven");
  });

  it("terminalSealStillAccepted answers from the seal's own lifetime and follows a later revocation", async () => {
    const lifetime = new ExecutorLifetime();
    let facadeOk = true;
    const identity = {};
    lifetime.inventory(identity, () => true);
    lifetime.bindFacade(identity, {}, () => facadeOk);
    const source = Object.freeze({ lease: "opaque" });
    const result = await lifetime.seal(source, settled, () => true);
    assert.equal(result.kind, "sealed");
    if (result.kind !== "sealed") return;
    assert.equal(terminalSealStillAccepted(result.seal, source), true);
    assert.equal(terminalSealStillAccepted(result.seal, { lease: "opaque" }), false, "another source use");
    assert.equal(terminalSealStillAccepted({}, source), false);
    assert.equal(terminalSealStillAccepted(null, source), false);
    assert.equal(terminalSealStillAccepted(JSON.parse(JSON.stringify(result.seal)), source), false);
    facadeOk = false;
    assert.equal(terminalSealStillAccepted(result.seal, source), false, "revoked after issue");
    facadeOk = true;
    lifetime.distrust();
    assert.equal(terminalSealStillAccepted(result.seal, source), false, "distrusted after issue");
  });

  it("forged, copied, foreign and mismatched seals are rejected; material is readable only from the real seal", async () => {
    const lifetime = new ExecutorLifetime();
    const o = origin(lifetime);
    await disposeOrigin(o);
    const source = Object.freeze({ lease: "opaque" });
    const result = await lifetime.seal(source, settled, () => true);
    assert.equal(result.kind, "sealed");
    if (result.kind !== "sealed") return;
    const seal = result.seal;
    assert.equal(lifetime.accepts(seal, source), true);
    assert.equal(lifetime.accepts({}, source), false);
    assert.equal(lifetime.accepts(Object.freeze({}), source), false);
    assert.equal(lifetime.accepts(JSON.parse(JSON.stringify(seal)), source), false);
    assert.equal(lifetime.accepts({ ...(seal as object) }, source), false);
    assert.equal(lifetime.accepts(seal, { lease: "opaque" }), false, "structurally equal source use is not the same identity");
    assert.equal(lifetime.accepts(seal, {}), false);
    assert.equal(lifetime.accepts("sealed", source), false);
    assert.equal(lifetime.accepts(null, source), false);

    // Another executor's lifetime, sealed over the very same source use, does not accept it.
    const other = new ExecutorLifetime();
    await disposeOrigin(origin(other));
    const otherResult = await other.seal(source, settled, () => true);
    assert.equal(otherResult.kind, "sealed");
    assert.equal(other.accepts(seal, source), false);
    if (otherResult.kind === "sealed") assert.equal(lifetime.accepts(otherResult.seal, source), false);

    assert.equal(terminalSealMaterial({}), undefined);
    assert.equal(terminalSealMaterial(JSON.parse(JSON.stringify(seal))), undefined);
    const material = terminalSealMaterial(seal)!;
    assert.equal(material.sourceUse, source);
    assert.equal(material.incarnations.length, 1);
    assert.match(material.executorIncarnation, /^[0-9a-f-]{36}$/);
    assert.equal(lifetime.executorIncarnation, material.executorIncarnation);
    assert.notEqual(other.executorIncarnation, lifetime.executorIncarnation);
    assert.match(material.incarnations[0]!.registry, /^[0-9a-f-]{36}$/);
    assert.match(material.incarnations[0]!.facade, /^[0-9a-f-]{36}$/);
    assert.notEqual(material.incarnations[0]!.registry, material.incarnations[0]!.facade);
    const otherMaterial = terminalSealMaterial((otherResult as { seal?: ExecutorTerminalSeal }).seal)!;
    assert.notEqual(otherMaterial.executorIncarnation, material.executorIncarnation);
    assert.notEqual(otherMaterial.incarnations[0]!.registry, material.incarnations[0]!.registry);
    assert.throws(() => { (material as { executorIncarnation: string }).executorIncarnation = "x"; }, TypeError);
  });

  it("a registry built without a lifetime (the standalone cross-check shape) contributes to no seal", async () => {
    const lifetime = new ExecutorLifetime();
    const standalone = new ExecutionRegistry(newLocalExecutionEpoch(0));
    await disposeOrigin({ registry: standalone, facade: createCodexExecutionSafety(standalone, never, never) });
    assert.equal(standalone.workAdmissionOpen(), true);
    const o = origin(lifetime);
    await disposeOrigin(o);
    const result = await lifetime.seal({}, settled, () => true);
    assert.equal(result.kind, "sealed");
    if (result.kind !== "sealed") return;
    assert.equal(terminalSealMaterial(result.seal)!.incarnations.length, 1, "only the inventoried registry is in the seal");
    // Sealing the owning lifetime never closes a lifetime-less registry's admission.
    assert.equal(standalone.reserveLaunch("command").kind, "denied", "disposed, but for its own reason");
    assert.equal(standalone.workAdmissionOpen(), true);
  });
});

describe("post-seal retained closures have no effect (#3404 M1b)", () => {
  const grants: RunGrants = {
    role: "coder", phase: "implement",
    allowedTools: new Set(["Bash", "Read"]), allowedSkills: new Set<string>(), isRoot: true,
  };

  it("a broker request, spawn and fileop retained across the seal are refused before any effect", async () => {
    const lifetime = new ExecutorLifetime();
    const o = origin(lifetime);
    await disposeOrigin(o);
    const effects = { spawn: 0, fileop: 0, delegate: 0 };
    const broker = new CodexCallbackBroker({
      registry: o.registry,
      spawnCommand: async () => { effects.spawn++; return { code: 0, stdout: "", stderr: "" }; },
      fileop: { op: async (_request: FileopRequest): Promise<FileopResponse> => { effects.fileop++; return { ok: true, size: 0, data: "" }; } },
      worktreePath: "/tmp/uzi-lifetime-worktree",
      grants,
      delegate: async () => { effects.delegate++; return { ok: true, output: {} }; },
      allowedRoles: new Set(["coder"]),
    });
    const sealed = await lifetime.seal({}, settled, () => true);
    assert.equal(sealed.kind, "sealed");
    const after = await broker.handleToolCall({ threadId: "t", turnId: "u", callId: "c-1" }, "Bash", { command: "echo hi" }, "root");
    assert.equal(after.ok, false);
    if (!after.ok) assert.equal(after.code, "admission_closed");
    const read = await broker.handleToolCall({ threadId: "t", turnId: "u", callId: "c-2" }, "Read", { path: "a" }, "root");
    assert.equal(read.ok, false);
    assert.deepEqual(effects, { spawn: 0, fileop: 0, delegate: 0 });
    assert.equal(o.registry.isPoisoned(), false, "a refused post-seal call must not revoke the sealed proof");
  });

  it("a broker request already in flight is joined by the seal, and one arriving after is denied", async () => {
    const lifetime = new ExecutorLifetime();
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(0), lifetime);
    const facade = createCodexExecutionSafety(registry, never, never);
    const entered = barrier(), release = barrier();
    let spawns = 0;
    const broker = new CodexCallbackBroker({
      registry,
      spawnCommand: async () => { spawns++; entered.resolve(); await release.promise; return { code: 0, stdout: "", stderr: "" }; },
      fileop: { op: async () => ({ ok: true, size: 0, data: "" }) },
      worktreePath: "/tmp/uzi-lifetime-worktree",
      grants, delegate: async () => ({ ok: true, output: {} }), allowedRoles: new Set(["coder"]),
    });
    const inFlight = broker.handleToolCall({ threadId: "t", turnId: "u", callId: "c-a" }, "Bash", { command: "echo hi" }, "root");
    await entered.promise;
    let sealed = false;
    const sealing = lifetime.seal({}, settled, () => true).then(r => { sealed = true; return r; });
    const late = await broker.handleToolCall({ threadId: "t", turnId: "u", callId: "c-b" }, "Bash", { command: "echo late" }, "root");
    assert.equal(late.ok, false);
    assert.equal(spawns, 1, "the late call never reached the spawn seam");
    await flush();
    assert.equal(sealed, false, "seal waits for the admitted call");
    release.resolve();
    await inFlight;
    assert.equal((await sealing).kind, "unproven", "the registry was never disposed, so nothing is proven");
    assert.ok(facade);
  });
});
