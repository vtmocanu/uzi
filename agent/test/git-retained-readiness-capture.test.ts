import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { PassThrough } from "node:stream";
import { test } from "node:test";
import { GitCache } from "../src/git.js";
import { SUPERVISOR_BIN, type CodexRootHandle, type CodexEffectLaunchSpec } from "../src/codex/launcher.js";
import { type RecoverySource } from "../src/recovery-progress.js";
import type { Logger } from "../src/log.js";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";

const source = fs.readFileSync(new URL("../src/git.ts", import.meta.url), "utf8");
const helper = source.split("const RETAINED_READINESS_HELPER = String.raw\x60")[1]!.split("\x60;")[0]!;
const logger = { info() {}, warn() {}, error() {}, debug() {} } as unknown as Logger;

function fixture() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "readiness-capture-"));
  const clone = path.join(dir, "clone"), bare = path.join(dir, "bare");
  fs.mkdirSync(path.join(clone, ".git/objects"), { recursive: true });
  fs.mkdirSync(path.join(bare, "objects/info"), { recursive: true });
  const roots = [clone, path.join(clone, ".git"), path.join(clone, ".git/objects"),
    bare, path.join(bare, "objects")];
  for (const root of [...roots, path.join(bare, "objects/info")]) fs.chmodSync(root, 0o750);
  const identities = roots.map(root => { const st = fs.statSync(root, { bigint: true }); return String(st.dev) + ":" + String(st.ino); });
  return { dir, clone, bare, roots, identities, cleanup() { fs.rmSync(dir, { recursive: true, force: true }); } };
}

function latch() {
  let resolve!: () => void;
  const promise = new Promise<void>(done => { resolve = done; });
  return { promise, resolve };
}

type Attempt = { ok: true } | { ok: false; error: unknown };
for (const mode of ["outside-positive", "awaited-capture", "ignored-capture"] as const) {
  test("readiness capture contract: " + mode, async t => {
    const f = fixture(), hold = latch(), entered = latch();
    const controller = new AbortController();
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(2852));
    let unexpectedSpawns = 0, launches = 0, waits = 0, disposals = 0;
    let waited = false, settled = false;
    let spec: CodexEffectLaunchSpec | undefined;
    let attempt: Promise<Attempt> | undefined;
    let operation: Promise<unknown> | undefined;
    let captureOutcome: unknown;
    const snapshot = () => ({ launches, waits, disposals, settled, roots: registry.rootCount(),
      attributions: attributions.length });
    let inside: ReturnType<typeof snapshot> | undefined, afterCapture: typeof inside;
    const attributions: unknown[][] = [], classifications: unknown[][] = [];
    const safety = createCodexExecutionSafety(registry,
      async () => { unexpectedSpawns++; throw Error("unexpected generic spawn"); }, undefined, undefined,
      async () => { unexpectedSpawns++; throw Error("unexpected owned process"); },
      () => () => {});
    const cache = new GitCache(f.dir, logger, undefined, {
      retainedReadinessLaunchForTest: async request => {
        launches++; spec = request;
        const stdin = new PassThrough(), stdout = new PassThrough(), stderr = new PassThrough();
        // A single child with no descendants: joining it and draining both pipes
        // precedes this root's clean disposal. This is not epoch-wide evidence.
        return {
          started: { event: "started", supervisorPid: 91001, childPid: 91002,
            subreaper: true, nondumpable: true, uid: 10003, liveCapsZero: true,
            capBoundingSet: "0x0", noNewPrivs: true },
          supervisorPid: 91001, transport: { stdin, stdout, stderr },
          failed: undefined, whenFailed: new Promise<Error>(() => {}),
          async snapshot() { throw Error("unexpected snapshot"); },
          async observe() { throw Error("unexpected observation"); },
          async waitChild() {
            waits++; entered.resolve();
            if (mode === "ignored-capture") await hold.promise;
            stdout.end("ok 1 0\n"); stderr.end();
            waited = true;
            return { event: "child_exit", code: 0 };
          },
          async dispose() {
            disposals++;
            assert.equal(waited, true, "clean disposal follows child join");
            assert.equal(stdout.readableEnded, true);
            assert.equal(stderr.readableEnded, true);
            return { clean: true, event: { event: "dispose", id: 1, state: "drained" } };
          },
        } satisfies CodexRootHandle;
      },
    });
    // Existing known-source guard seams are identical outside and inside capture.
    const seams = cache as unknown as {
      checkedRecovery: (...args: unknown[]) => Promise<object>;
      classifyOwnerClonePath: (...args: unknown[]) => Promise<"canonical">;
    };
    seams.checkedRecovery = async (...args) => { attributions.push(args); return {}; };
    seams.classifyOwnerClonePath = async (...args) => { classifications.push(args); return "canonical"; };
    const expected = { clonePath: f.clone, runId: "run" } as RecoverySource;
    const begin = () => {
      // Attach rejection handling immediately, including for an ignored caller.
      attempt = cache.normalizeRetainedSourceObjects(f.bare, "branch", "key", expected)
        .then<Attempt, Attempt>(() => ({ ok: true }), error => ({ ok: false, error }))
        .then(value => { settled = true; return value; });
      return attempt;
    };
    let operationResult: unknown;
    try {
      operation = cache.withRecoveryOperation(controller.signal, Date.now() + 60000, async () => {
        if (mode === "outside-positive") return begin();
        captureOutcome = await safety.withCredentialFreeCapture(10000, context =>
          cache.withCredentialFreeCapture(context, async () => {
            const pending = begin();
            if (mode === "awaited-capture") await pending;
            else await Promise.race([entered.promise, pending]);
            inside = snapshot();
            return "capture action returned";
          }));
        afterCapture = snapshot();
        return "outer operation returned";
      });
      operationResult = await operation;
    } finally {
      hold.resolve();
      try {
        await Promise.allSettled([...(attempt ? [attempt] : []), ...(operation ? [operation] : [])]);
        assert.deepEqual(await safety.dispose({ boundary: "checkpoint", deadlineMs: 10000 }), { kind: "disposed" });
      } finally { f.cleanup(); }
    }
    t.diagnostic(JSON.stringify({ mode, inside, afterCapture, launches, waits, disposals,
      attributions: attributions.length, classifications: classifications.length,
      attempt: attempt ? await attempt : undefined, operationResult, captureOutcome }));
    assert.equal(unexpectedSpawns, 0);
    assert.equal(registry.rootCount(), 0);
    if (mode === "outside-positive") {
      assert.deepEqual(operationResult, { ok: true });
      assert.equal(launches, 1); assert.equal(waits, 1); assert.equal(disposals, 1);
      assert.deepEqual(attributions, [
        [f.bare, "branch", "key", expected], [f.bare, "branch", "key", expected],
      ]);
      assert.deepEqual(classifications, [
        [f.bare, "branch", "key", expected.runId, expected.clonePath],
        [f.bare, "branch", "key", expected.runId, expected.clonePath],
      ]);
      assert.ok(spec);
      assert.equal(spec.identity, "command");
      assert.equal(spec.command, process.execPath);
      assert.equal(spec.supervisorBin, SUPERVISOR_BIN);
      assert.equal(spec.cwd, "/");
      assert.deepEqual(spec.env, { LANG: "C" });
      assert.equal(spec.args[0], "-e");
      assert.equal(spec.args[1], helper);
      const payload = JSON.parse(spec.args[2]!);
      assert.deepEqual(payload.roots, f.roots);
      assert.deepEqual(payload.identities, f.identities);
      assert.equal(Number.isFinite(payload.deadline), true);
    } else {
      assert.equal(launches, 0, "CAPTUREOUTCOME: unsupported retained readiness must refuse before direct helper launch");
      assert.deepEqual(inside, { launches: 0, waits: 0, disposals: 0, settled: true, roots: 0, attributions: 0 });
      assert.deepEqual(afterCapture, inside);
      assert.equal(spec, undefined);
      assert.deepEqual(attributions, []);
      assert.deepEqual(classifications, []);
      assert.equal(operationResult, "outer operation returned");
      assert.equal((captureOutcome as { kind: string }).kind, "observed_empty");
      assert.ok(attempt);
      const result = await attempt;
      assert.equal(result.ok, false);
      if (result.ok) assert.fail("bounded trusted refusal required");
      assert.ok(result.error instanceof TrustedExecutionRefusal);
      assert.equal(result.error.message, "credential-free capture launch refused");
      assert.equal(result.error.cause, undefined);
    }
  });
}
