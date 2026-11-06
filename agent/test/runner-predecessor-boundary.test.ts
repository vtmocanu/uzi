// issue #2969 D4/D5/D6: the runner's predecessor per-call source boundary. A registered grant (exact execution
// tail, held scope, deadline) selects it through InventorySourceBoundaryContext.predecessor; it shares
// proveInventorySources with the ordinary boundary, binds a Codex capture attempt to its own clone path, compares a
// per-clone baseline on every prove, and gates predecessor FINAL on ignored residue. Real git for the content proof.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { InventorySourceBoundaryContext, InventorySourceDiagnostic } from "../src/recovery.js";
import { mintAttemptId, newRunAttempt, scanOnce, type QuiesceRunRequest, type RunAttempt } from "../src/run-quiescence.js";
import { GitCache, INVENTORY_CONTENT_PROOF_BOUNDS } from "../src/git.js";
import { fakeGitlab, git, installHarness, runner } from "./runner-harness.js";
installHarness();

type Refusal = { check: string; clonePath?: string };
interface Registration {
  token: object; live(): boolean; refusals: readonly Refusal[]; settlementLate: boolean;
  markSettlementLate(): void; dispose(): void;
}
interface Internals {
  withInventorySourceBoundary(context: InventorySourceBoundaryContext, action: (prove: () => Promise<boolean>) => Promise<void>): Promise<"passed" | "retained">;
  executionTails: Map<string, Promise<void>>;
  inventoryReservations: Set<string>;
  liveAttempts: { isLivePath(p: string): boolean; add(a: RunAttempt): void };
  freeCaptureFlights: WeakMap<object, unknown>;
  registerPredecessorBoundary(flight: unknown, held: unknown, opts: {
    generations: readonly number[]; deadlineAt: number;
    baseline?: ReadonlyArray<{ clonePath: string; branch: string; head?: string; identity?: string }>;
  }): Registration;
}
type Clone = { clonePath: string; branch: string; runId: string; head: string; identity: string };

const RUN = "run-2969";
const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
const GIT_ENV = { ...cleanEnv, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const quiescent = { process: { state: "quiescent" as const, processes: [], killed: [], detail: "" }, docker: { state: "not_wired" as const, removed: [], detail: "" } };

const clone = (name: string, head = "a".repeat(40), identity = "1:1"): Clone =>
  ({ clonePath: "/fixture/repo/" + name, branch: "task", runId: RUN, head, identity });

function world(opts: { codex?: boolean; claimGeneration?: number; deadlineAt?: number; clones?: () => Clone[];
  baseline?: Clone[]; attemptPath?: string; quiesce?: (req: QuiesceRunRequest) => unknown } = {}) {
  const requests: QuiesceRunRequest[] = [];
  const w: { cancelled: boolean; fence: unknown; clones: () => Clone[]; reads: unknown[][]; bounds: unknown[]; statusCalls: string[]; status: (p: string) => string[] | null; content: (p: string) => string; ignored: (p: string) => string } = {
    cancelled: false, fence: undefined, reads: [], bounds: [], statusCalls: [], status: () => [], clones: opts.clones ?? (() => [clone("issue-1")]),
    content: () => "clean", ignored: () => "clean",
  };
  const r = runner({ run: async () => { throw new Error("unreached executor"); } }, fakeGitlab().gitlab, undefined, {
    quiesceRun: async req => { requests.push(req); return ((await opts.quiesce?.(req)) ?? quiescent) as typeof quiescent; },
    predecessorFinalForTests: true, // the pass is disabled in production (issue #2969 fallback)
  });
  const internals = r as unknown as Internals;
  const tail = Promise.resolve();
  internals.executionTails.set(RUN, tail);
  const attempt = opts.codex ? newRunAttempt(RUN, opts.claimGeneration ?? 3, opts.attemptPath ?? "/fixture/repo/issue-1", () => []) : undefined;
  const flight = {
    runId: RUN, claimGeneration: opts.claimGeneration ?? 3, executionTail: tail, activeCaptureAttempt: attempt,
    executor: opts.codex ? { withCredentialFreeCapture: () => undefined } : {},
    steering: { isCancelled: () => w.cancelled, claimFence: () => w.fence },
  };
  const held = { deadlineAt: Infinity };
  internals.freeCaptureFlights.set(held, flight);
  git.readInventoryCloneHeads = (async (_bare: string, _run: string, ...rest: unknown[]) => {
    w.reads.push(rest);
    const clones = w.clones();
    return { kind: "verified", heads: [...new Set(clones.map(c => c.head))], clones, foreignOwners: [] };
  }) as typeof git.readInventoryCloneHeads;
  git.inventorySourceContentProof = (async (p: string, b?: unknown) => { w.bounds.push(b); return w.content(p); }) as typeof git.inventorySourceContentProof;
  git.inventoryIgnoredResidue = (async (p: string, b?: unknown) => { w.bounds.push(b); return w.ignored(p); }) as typeof git.inventoryIgnoredResidue;
  git.worktreeStatus = async (p: string) => { w.statusCalls.push(p); return w.status(p); };
  const registration = internals.registerPredecessorBoundary(flight, held, {
    generations: [1, 2], deadlineAt: opts.deadlineAt ?? Infinity,
    ...(opts.baseline ? { baseline: opts.baseline } : {}),
  });
  const enter = async (generation = 1, token: object | undefined = registration.token,
    action?: (prove: () => Promise<boolean>) => Promise<void>) => {
    const diagnostics: InventorySourceDiagnostic[] = [];
    const proves: boolean[] = [];
    const result = await internals.withInventorySourceBoundary({
      runId: RUN, generation, barePath: "/fixture/repo.git", reportDiagnostic: d => diagnostics.push(d),
      ...(token ? { predecessor: token } : {}),
    }, action ?? (async prove => { proves.push(await prove()); }));
    return { result, diagnostics, proves };
  };
  return { r, internals, w, tail, flight, held, registration, requests, attempt, enter };
}

describe("predecessor boundary: grant, tail and scope", () => {
  it("proves under the exact tail without installing a tail or a reservation", async () => {
    let seen: { tail: unknown; reservations: number } | undefined;
    const x = world();
    const out = await x.enter(1, x.registration.token, async prove => {
      assert.equal(await prove(), true);
      seen = { tail: x.internals.executionTails.get(RUN), reservations: x.internals.inventoryReservations.size };
    });
    assert.equal(out.result, "passed");
    assert.deepEqual(out.diagnostics, []);
    assert.equal(seen?.tail, x.tail, "the flight's own tail stays the run's tail");
    assert.equal(seen?.reservations, 0);
    assert.equal(x.internals.executionTails.get(RUN), x.tail);
    assert.deepEqual(x.registration.refusals, []);
    assert.deepEqual(x.w.reads.map(args => args[0]), [{ cloneIdentity: true }, { cloneIdentity: true }, { cloneIdentity: true }, { cloneIdentity: true }], "two reads per prove");
    assert.equal(x.requests[0]!.attempt, undefined, "Claude requests carry no attempt");
  });

  it("the ordinary boundary is base behaviour: worktreeStatus only, no identity option, no content proof or residue gate", async () => {
    const x = world();
    const diagnostics: InventorySourceDiagnostic[] = [];
    const result = await x.internals.withInventorySourceBoundary({
      runId: "run-ordinary", generation: 1, barePath: "/fixture/repo.git", reportDiagnostic: d => diagnostics.push(d),
    }, async prove => { assert.equal(await prove(), true); });
    assert.equal(result, "passed");
    assert.deepEqual(diagnostics, []);
    assert.deepEqual(x.w.reads.map(args => args.length), [0, 0, 0, 0]);
    assert.deepEqual(x.w.statusCalls, ["/fixture/repo/issue-1", "/fixture/repo/issue-1"]);
    assert.deepEqual(x.w.bounds, [], "zero content-proof and residue-listing calls on the ordinary path");
  });

  it("the ordinary boundary maps worktreeStatus exactly as base: null unreadable, non-empty dirty", async () => {
    for (const [status, expected] of [[null, "worktree_status_unreadable"], [["?? x"], "worktree_status_dirty"]] as const) {
      const x = world();
      x.w.status = () => status as string[] | null;
      const diagnostics: InventorySourceDiagnostic[] = [];
      const result = await x.internals.withInventorySourceBoundary({
        runId: "run-ordinary", generation: 1, barePath: "/fixture/repo.git", reportDiagnostic: d => diagnostics.push(d),
      }, async () => assert.fail("must not release"));
      assert.equal(result, "retained");
      assert.deepEqual(diagnostics, [{ check: expected }]);
      assert.deepEqual(x.requests.map(r => r.site), ["inventory_source", "inventory_source:after_runner_git"], "after-scan still runs");
      assert.deepEqual(x.w.bounds, []);
    }
  });

  it("a hung content proof never delays the ordinary path (it is never called)", async () => {
    const x = world();
    x.w.content = () => { throw new Error("content proof must not run on the ordinary path"); };
    git.inventorySourceContentProof = (() => new Promise(() => {})) as typeof git.inventorySourceContentProof;
    const result = await x.internals.withInventorySourceBoundary({ runId: "run-ordinary", generation: 1, barePath: "/fixture/repo.git" },
      async prove => { assert.equal(await prove(), true); });
    assert.equal(result, "passed");
  });

  it("a replaced tail records predecessor_scope_invalid, reports execution_tail_present and does nothing else", async () => {
    const x = world();
    const replacement = Promise.resolve();
    x.internals.executionTails.set(RUN, replacement);
    const out = await x.enter();
    assert.equal(out.result, "retained");
    assert.deepEqual(out.diagnostics, [{ check: "execution_tail_present" }]);
    assert.deepEqual(x.registration.refusals, [{ check: "predecessor_scope_invalid" }]);
    assert.equal(x.internals.executionTails.get(RUN), replacement, "the replacement tail is untouched");
    assert.equal(x.internals.inventoryReservations.size, 0);
    assert.equal(x.w.reads.length, 0);
    assert.equal(x.requests.length, 0);
  });

  it("a tail replaced during an await reaches the boundary and stops the proof", async () => {
    const x = world({ quiesce: () => { x.internals.executionTails.set(RUN, Promise.resolve()); } });
    const out = await x.enter();
    assert.equal(out.result, "retained");
    assert.deepEqual(out.diagnostics, [{ check: "boundary_exception" }]);
    assert.deepEqual(x.registration.refusals.map(r => r.check), ["predecessor_scope_invalid", "boundary_exception"]);
    assert.equal(x.requests.length, 2, "the after-scan finishes, then the dead scope stops the proof before any verdict");
  });

  it("a tail replaced between two proves stops the second", async () => {
    const x = world();
    const out = await x.enter(1, x.registration.token, async prove => {
      assert.equal(await prove(), true);
      x.internals.executionTails.set(RUN, Promise.resolve());
      await prove();
      assert.fail("a proof past a replaced tail must throw");
    });
    assert.equal(out.result, "retained");
    assert.deepEqual(out.diagnostics, [{ check: "boundary_exception" }]);
  });

  it("the predecessor path hands the grant deadline to both proofs; the ordinary path keeps the default", async () => {
    const deadlineAt = Date.now() + 600_000;
    const x = world({ deadlineAt });
    assert.equal((await x.enter(1, x.registration.token, async prove => { await prove(); })).result, "passed");
    assert.deepEqual(x.w.bounds, Array(4).fill({ ...INVENTORY_CONTENT_PROOF_BOUNDS, deadlineAt }), "content proof and residue listing, on each of two proves");
    x.w.bounds.length = 0;
    await x.internals.withInventorySourceBoundary({ runId: "run-ordinary", generation: 1, barePath: "/fixture/repo.git" },
      async prove => { await prove(); });
    assert.deepEqual(x.w.bounds, [], "ordinary: no content proof or residue listing at all");
  });

  it("a valid token with a foreign runId is refused", async () => {
    const x = world();
    const diagnostics: InventorySourceDiagnostic[] = [];
    const result = await x.internals.withInventorySourceBoundary({
      runId: "someone-else", generation: 1, barePath: "/fixture/repo.git", predecessor: x.registration.token,
      reportDiagnostic: d => diagnostics.push(d),
    }, async () => assert.fail("must not release"));
    assert.equal(result, "retained");
    assert.deepEqual(diagnostics, [{ check: "execution_tail_present" }]);
    assert.deepEqual(x.registration.refusals, [{ check: "predecessor_scope_invalid" }]);
    assert.equal(x.w.reads.length, 0);
  });

  it("a scope that dies after clone 1 stops the proof before clone 2's content proof", async () => {
    let contentCalls = 0;
    const x = world({
      clones: () => [clone("issue-1"), clone("issue-2")],
      quiesce: req => { if (req.site === "inventory_source:after_runner_git") x.w.cancelled = true; },
    });
    x.w.content = () => { contentCalls++; return "clean"; };
    const out = await x.enter();
    assert.equal(out.result, "retained");
    assert.equal(contentCalls, 1, "clone 2 was never proven");
    assert.equal(x.requests.length, 2);
    assert.ok(x.registration.refusals.some(r => r.check === "predecessor_scope_invalid"));
  });

  it("an unregistered, foreign or disposed token is dead", async () => {
    const x = world();
    for (const token of [{}, Object.freeze({})]) {
      const out = await x.enter(1, token);
      assert.equal(out.result, "retained");
      assert.deepEqual(out.diagnostics, [{ check: "execution_tail_present" }]);
    }
    assert.equal(x.registration.live(), true);
    x.registration.dispose();
    assert.equal(x.registration.live(), false);
    const out = await x.enter();
    assert.equal(out.result, "retained");
    assert.deepEqual(out.diagnostics, [{ check: "execution_tail_present" }]);
    assert.equal(x.w.reads.length, 0);
  });

  it("only an allowed generation strictly below the flight's claim generation is accepted", async () => {
    const x = world();
    assert.equal((await x.enter(1)).result, "passed");
    assert.equal((await x.enter(2)).result, "passed");
    for (const generation of [3, 4, 0, 7]) {
      const out = await x.enter(generation);
      assert.equal(out.result, "retained", "generation " + generation);
      assert.deepEqual(out.diagnostics, [{ check: "execution_tail_present" }]);
    }
    assert.throws(() => x.internals.registerPredecessorBoundary(x.flight, x.held, { generations: [3], deadlineAt: Infinity }), /below the executing claim/);
    assert.throws(() => x.internals.registerPredecessorBoundary(x.flight, x.held, { generations: [], deadlineAt: Infinity }), /below the executing claim/);
  });

  it("registration refuses a flight whose own tail is not the run's current tail", () => {
    const x = world();
    x.internals.executionTails.set(RUN, Promise.resolve());
    assert.throws(() => x.internals.registerPredecessorBoundary(x.flight, x.held, { generations: [1], deadlineAt: Infinity }), /own execution tail/);
  });

  it("a non-Linux host is refused as an invalid scope", async () => {
    const platform = Object.getOwnPropertyDescriptor(process, "platform")!;
    const x = world();
    try {
      Object.defineProperty(process, "platform", { ...platform, value: "darwin" });
      const out = await x.enter();
      assert.equal(out.result, "retained");
      assert.deepEqual(out.diagnostics, [{ check: "execution_tail_present" }]);
      assert.deepEqual(x.registration.refusals, [{ check: "predecessor_scope_invalid" }]);
    } finally { Object.defineProperty(process, "platform", platform); }
  });

  it("live() follows cancellation, a claim fence, an aborted held scope and dispose", () => {
    const x = world();
    assert.equal(x.registration.live(), true);
    x.w.cancelled = true;
    assert.equal(x.registration.live(), false);
    x.w.cancelled = false;
    x.w.fence = new Error("stale");
    assert.equal(x.registration.live(), false);
    x.w.fence = undefined;
    assert.equal(x.registration.live(), true);
    const ac = new AbortController();
    const y = world();
    Object.assign(y.held, { signal: ac.signal });
    assert.equal(y.registration.live(), true);
    ac.abort();
    assert.equal(y.registration.live(), false);
    y.registration.markSettlementLate();
    assert.equal(y.registration.settlementLate, true);
  });

  it("the Claude grant deadline is enforced at entry and during a proof", async () => {
    const early = world({ deadlineAt: Date.now() + 40 });
    await new Promise(resolve => setTimeout(resolve, 80));
    assert.equal(early.registration.live(), false);
    const late = await early.enter();
    assert.equal(late.result, "retained");
    assert.deepEqual(late.diagnostics, [{ check: "execution_tail_present" }]);
    const mid = world({ deadlineAt: Date.now() + 60, quiesce: () => new Promise(resolve => setTimeout(resolve, 120)) });
    const out = await mid.enter();
    assert.equal(out.result, "retained");
    assert.deepEqual(out.diagnostics, [{ check: "boundary_exception" }]);
    assert.ok(mid.registration.refusals.some(r => r.check === "predecessor_scope_invalid"));
  });
});

describe("predecessor boundary: live-path refusal and baseline", () => {
  it("a live attempt path refuses, whether the registry is stubbed or holds a foreign attempt on the clone", async () => {
    const stubbed = world();
    stubbed.internals.liveAttempts.isLivePath = () => true;
    let out = await stubbed.enter();
    assert.equal(out.result, "retained");
    assert.deepEqual(out.diagnostics, [{ check: "live_attempt_path" }]);
    assert.deepEqual(stubbed.registration.refusals, [{ check: "live_attempt_path", clonePath: "/fixture/repo/issue-1" }]);
    assert.equal(stubbed.requests.length, 0, "refused before any quiescence");

    const foreign = world();
    foreign.internals.liveAttempts.add(newRunAttempt("other-run", 1, "/fixture/repo/issue-1", () => []));
    out = await foreign.enter();
    assert.equal(out.result, "retained");
    assert.deepEqual(out.diagnostics, [{ check: "live_attempt_path" }]);
  });

  it("same-path directory replacement (same head, new dev:ino) is a STOP within and across calls", async () => {
    let identity = "1:1";
    const x = world({ clones: () => [clone("issue-1", "a".repeat(40), identity)] });
    assert.equal((await x.enter()).result, "passed", "seeds the baseline");
    identity = "1:2";
    const across = await x.enter();
    assert.equal(across.result, "retained");
    assert.deepEqual(across.diagnostics, [{ check: "source_identity_changed" }]);
    assert.deepEqual(x.registration.refusals, [{ check: "baseline_drift" }]);

    let reads = 0;
    const y = world({ clones: () => [clone("issue-1", "a".repeat(40), ++reads > 1 ? "9:9" : "1:1")] });
    const within = await y.enter();
    assert.equal(within.result, "retained");
    assert.deepEqual(within.diagnostics, [{ check: "source_identity_changed" }]);
  });

  it("duplicate-head drift (two clones swap heads) is a STOP although the head set is unchanged", async () => {
    const h1 = "1".repeat(40), h2 = "2".repeat(40);
    let swapped = false;
    const x = world({ clones: () => [clone("issue-1", swapped ? h2 : h1), clone("issue-2", swapped ? h1 : h2)] });
    assert.equal((await x.enter()).result, "passed");
    swapped = true;
    const out = await x.enter();
    assert.equal(out.result, "retained");
    assert.deepEqual(out.diagnostics, [{ check: "source_identity_changed" }]);
    assert.deepEqual(x.registration.refusals, [{ check: "baseline_drift" }]);
  });

  it("a pre-entry baseline (Codex) is compared from the first prove", async () => {
    const seeded = [clone("issue-1", "a".repeat(40), "1:1")];
    const same = world({ baseline: seeded });
    assert.equal((await same.enter()).result, "passed");
    const drifted = world({ baseline: seeded, clones: () => [clone("issue-1", "b".repeat(40), "1:1")] });
    const out = await drifted.enter();
    assert.equal(out.result, "retained");
    assert.deepEqual(drifted.registration.refusals, [{ check: "baseline_drift" }]);
    assert.throws(() => world({ baseline: [{ ...clone("x"), head: undefined } as never] }), /baseline is incomplete/);
  });

  it("an unverified inventory read refuses", async () => {
    const x = world();
    git.readInventoryCloneHeads = (async () => ({ kind: "unknown", cause: "clone_path_invalid" })) as typeof git.readInventoryCloneHeads;
    const out = await x.enter();
    assert.equal(out.result, "retained");
    assert.deepEqual(out.diagnostics, [{ check: "inventory_read_not_verified", cause: "clone_path_invalid" }]);
  });
});

describe("predecessor boundary: Codex attempt binding", () => {
  it("binds the capture attempt to its own clone path only", async () => {
    const x = world({ codex: true, clones: () => [clone("issue-1")] });
    const out = await x.enter();
    assert.equal(out.result, "passed");
    const first = x.requests[0]!;
    assert.equal(first.attempt, x.attempt);
    assert.equal(first.cloneKey, x.attempt!.cloneKey);
    assert.deepEqual(first.targetPaths, [x.attempt!.clonePath]);
    assert.equal(first.mode, "capture");
  });

  it("any other clone beside the bound one is a codex_attempt_mismatch STOP", async () => {
    const sibling = world({ codex: true, clones: () => [clone("issue-1"), clone("issue-2")] });
    const out = await sibling.enter();
    assert.equal(out.result, "retained");
    assert.deepEqual(sibling.registration.refusals.map(r => r.check), ["codex_attempt_mismatch"]);
    assert.equal(sibling.registration.refusals[0]!.clonePath, "/fixture/repo/issue-2");

    const other = world({ codex: true, clones: () => [clone("issue-9")] });
    const only = await other.enter();
    assert.equal(only.result, "retained");
    assert.deepEqual(other.registration.refusals.map(r => r.check), ["codex_attempt_mismatch"]);
    assert.equal(other.requests.length, 0, "no scan outside the bound path");
  });

  it("a Codex flight without a capture attempt proves nothing", async () => {
    const x = world({ codex: true });
    const bare = world({ codex: true });
    (bare.flight as { activeCaptureAttempt?: RunAttempt }).activeCaptureAttempt = undefined;
    const registration = bare.internals.registerPredecessorBoundary(bare.flight, bare.held, { generations: [1], deadlineAt: Infinity });
    const out = await bare.enter(1, registration.token);
    assert.equal(out.result, "retained");
    assert.deepEqual(registration.refusals.map(r => r.check), ["codex_attempt_mismatch"]);
    assert.equal((await x.enter()).result, "passed");
  });
});

describe("predecessor boundary: content proof, scratch and ignored residue (real git)", () => {
  function repo(t: { after(fn: () => void): void }, exclude = [".uzi/scratch/"]) {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pred-boundary-"));
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    const root = path.join(dir, "issue-1");
    fs.mkdirSync(root);
    const g = (...args: string[]) => execFileSync("git", ["-C", root, ...args],
      { env: GIT_ENV, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
    g("init", "-q", "-b", "main");
    for (const [k, v] of [["user.name", "fixture"], ["user.email", "fixture@example.com"], ["commit.gpgsign", "false"],
      ["gc.auto", "0"], ["core.fsmonitor", "false"]]) g("config", k!, v!);
    fs.writeFileSync(path.join(root, "tracked.txt"), "original\n");
    g("add", "-A"); g("commit", "-q", "-m", "base");
    fs.appendFileSync(path.join(root, ".git", "info", "exclude"), exclude.map(e => e + "\n").join(""));
    return { root, g };
  }
  // Both boundaries over the same real clone and the real strict proofs.
  async function bothPaths(root: string) {
    const x = world({ clones: () => [clone("issue-1")].map(c => ({ ...c, clonePath: root })) });
    git.inventorySourceContentProof = GitCache.prototype.inventorySourceContentProof.bind(git);
    git.inventoryIgnoredResidue = GitCache.prototype.inventoryIgnoredResidue.bind(git);
    git.worktreeStatus = GitCache.prototype.worktreeStatus.bind(git);
    const predecessor = await x.enter();
    const diagnostics: InventorySourceDiagnostic[] = [];
    const ordinary = await x.internals.withInventorySourceBoundary({
      runId: "run-ordinary", generation: 1, barePath: "/fixture/repo.git", reportDiagnostic: d => diagnostics.push(d),
    }, async prove => { assert.equal(await prove(), true); });
    return { predecessor, ordinary: { result: ordinary, diagnostics }, refusals: x.registration.refusals };
  }

  it("scratch-only content passes on both paths", async t => {
    const { root } = repo(t);
    fs.mkdirSync(path.join(root, ".uzi", "scratch", "deep"), { recursive: true });
    fs.writeFileSync(path.join(root, ".uzi", "scratch", "deep", "log.txt"), "scratch\n");
    const out = await bothPaths(root);
    assert.equal(out.predecessor.result, "passed");
    assert.equal(out.ordinary.result, "passed");
    assert.deepEqual(out.refusals, []);
  });

  for (const [name, exclude, files] of [
    ["an ignored build/report.log", ["build/"], ["build/report.log"]],
    ["an ignored node_modules/", ["node_modules/"], ["node_modules/pkg/index.js"]],
  ] as const) {
    it(`${name} is ignored_residue on the predecessor path only`, async t => {
      const { root } = repo(t, [".uzi/scratch/", ...exclude]);
      for (const f of files) { fs.mkdirSync(path.dirname(path.join(root, f)), { recursive: true }); fs.writeFileSync(path.join(root, f), "x\n"); }
      const out = await bothPaths(root);
      assert.equal(out.predecessor.result, "retained");
      assert.deepEqual(out.predecessor.diagnostics, [{ check: "worktree_status_dirty" }], "reported with the existing code");
      assert.deepEqual(out.refusals, [{ check: "ignored_residue", clonePath: root }]);
      assert.equal(out.ordinary.result, "passed", "the ordinary path is unchanged for ignored files");
      assert.deepEqual(out.ordinary.diagnostics, []);
    });
  }

  it("a WIP-omitted oversized file is worktree_status_dirty on both paths", async t => {
    const { root } = repo(t);
    fs.writeFileSync(path.join(root, "big.bin"), Buffer.alloc(8 * 1024 * 1024 + 1, 0x61));
    fs.writeFileSync(path.join(root, "small-wip.txt"), "ordinary\n");
    assert.equal(await git.commitWipMarker(root, { mode: "exclude" }), true);
    const out = await bothPaths(root);
    assert.equal(out.predecessor.result, "retained");
    assert.deepEqual(out.predecessor.diagnostics, [{ check: "worktree_status_dirty" }]);
    assert.deepEqual(out.refusals, [{ check: "worktree_status_dirty", clonePath: root }]);
    assert.equal(out.ordinary.result, "retained");
    assert.deepEqual(out.ordinary.diagnostics, [{ check: "worktree_status_dirty" }]);
  });

  it("content proof results map to the closed coordinator codes and the grant record", async () => {
    for (const [content, grant, reported] of [
      ["dirty", "worktree_status_dirty", "worktree_status_dirty"],
      ["unreadable", "worktree_status_unreadable", "worktree_status_unreadable"],
      ["over_bound", "over_bound", "worktree_status_unreadable"],
    ] as const) {
      const x = world();
      x.w.content = () => content;
      const ordinary = await x.internals.withInventorySourceBoundary({
        runId: "run-ordinary", generation: 1, barePath: "/fixture/repo.git",
      }, async prove => { assert.equal(await prove(), true); });
      assert.equal(ordinary, "passed", "the ordinary path ignores the content proof");
      const out = await x.enter();
      assert.equal(out.result, "retained");
      assert.deepEqual(out.diagnostics, [{ check: reported }]);
      assert.deepEqual(x.registration.refusals.map(r => r.check), [grant]);
    }
    const bound = world();
    bound.w.ignored = () => "over_bound";
    const out = await bound.enter();
    assert.deepEqual(out.diagnostics, [{ check: "worktree_status_unreadable" }]);
    assert.deepEqual(bound.registration.refusals.map(r => r.check), ["over_bound"]);
  });

  it("the ignored gate runs only on the predecessor path and only after a clean content proof", async () => {
    const x = world();
    let ignoredCalls = 0;
    x.w.ignored = () => { ignoredCalls++; return "clean"; };
    await x.internals.withInventorySourceBoundary({ runId: "run-ordinary", generation: 1, barePath: "/fixture/repo.git" },
      async prove => { await prove(); });
    assert.equal(ignoredCalls, 0, "ordinary path");
    await x.enter();
    assert.equal(ignoredCalls, 2, "predecessor path: once per prove");
    ignoredCalls = 0;
    x.w.content = () => "dirty";
    await x.enter();
    assert.equal(ignoredCalls, 0, "not listed once the content proof already refused");
  });

  it("a non-quiescent after-scan outranks every residue verdict (a STOP check, not a residue skip)", async () => {
    for (const [content, ignored] of [["dirty", "clean"], ["unreadable", "clean"], ["over_bound", "clean"], ["clean", "residue"], ["clean", "over_bound"]] as const) {
      const x = world({
        quiesce: req => req.site === "inventory_source:after_runner_git"
          ? { ...quiescent, process: { ...quiescent.process, state: "unverified" as const, detail: "raced" } } : undefined,
      });
      x.w.content = () => content;
      x.w.ignored = () => ignored;
      const out = await x.enter();
      assert.equal(out.result, "retained", `${content}/${ignored}`);
      assert.deepEqual(x.registration.refusals.map(r => r.check), ["process_not_quiescent_after_git"], `${content}/${ignored}`);
      assert.equal(out.diagnostics[0]?.check, "process_not_quiescent_after_git");
    }
  });

  it("quiescence runs before and after the proofs, and the after-scan still runs on a dirty clone", async () => {
    const x = world();
    x.w.content = () => "dirty";
    await x.enter();
    assert.deepEqual(x.requests.map(r => r.site), ["inventory_source", "inventory_source:after_runner_git"]);
  });
});

describe("worktreeStatus keeps serving its other callers", () => {
  it("returns plain porcelain lines, ignores ignored files, and does not use the byte transport", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pred-wt-status-"));
    try {
      const g = (...args: string[]) => execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
      g("init", "-q", "-b", "main");
      for (const [k, v] of [["user.name", "f"], ["user.email", "f@example.com"], ["commit.gpgsign", "false"], ["core.fsmonitor", "false"]]) g("config", k!, v!);
      fs.writeFileSync(path.join(dir, "a.txt"), "a\n");
      g("add", "-A"); g("commit", "-q", "-m", "base");
      fs.appendFileSync(path.join(dir, ".git", "info", "exclude"), "build/\n");
      fs.mkdirSync(path.join(dir, "build"));
      fs.writeFileSync(path.join(dir, "build", "out.log"), "o\n");
      let bytesCalls = 0;
      const seam = git as unknown as { runGitAsRunnerBytes: (...a: unknown[]) => Promise<unknown> };
      const real = seam.runGitAsRunnerBytes.bind(git);
      seam.runGitAsRunnerBytes = (...a) => { bytesCalls++; return real(...a); };
      assert.deepEqual(await git.worktreeStatus(dir), []);
      fs.writeFileSync(path.join(dir, "a.txt"), "changed\n");
      fs.writeFileSync(path.join(dir, "new.txt"), "n\n");
      assert.deepEqual(await git.worktreeStatus(dir), [" M a.txt", "?? new.txt"]);
      assert.equal(bytesCalls, 0);
    } finally { fs.rmSync(dir, { recursive: true, force: true }); }
  });
});

describe("ordinary FINAL equals base; the predecessor path is strict (real git)", () => {
  const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
  const GENV = { ...cleanEnv, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
  function repo(t: { after(fn: () => void): void }) {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pred-equiv-"));
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    const root = path.join(dir, "issue-1");
    fs.mkdirSync(root);
    const g = (...args: string[]) => execFileSync("git", ["-C", root, ...args], { env: GENV, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
    g("init", "-q", "-b", "main");
    for (const [k, v] of [["user.name", "f"], ["user.email", "f@example.com"], ["commit.gpgsign", "false"], ["gc.auto", "0"], ["core.fsmonitor", "false"]]) g("config", k!, v!);
    return { root, g, commit: () => { g("add", "-A"); g("commit", "-q", "-m", "c"); } };
  }
  async function both(root: string) {
    const x = world({ clones: () => [{ ...clone("issue-1"), clonePath: root }] });
    git.inventorySourceContentProof = GitCache.prototype.inventorySourceContentProof.bind(git);
    git.inventoryIgnoredResidue = GitCache.prototype.inventoryIgnoredResidue.bind(git);
    git.worktreeStatus = GitCache.prototype.worktreeStatus.bind(git);
    const predecessor = await x.enter();
    const diagnostics: InventorySourceDiagnostic[] = [];
    const ordinary = await x.internals.withInventorySourceBoundary({
      runId: "run-ordinary", generation: 1, barePath: "/fixture/repo.git", reportDiagnostic: d => diagnostics.push(d),
    }, async prove => { assert.equal(await prove(), true); });
    return { predecessor, ordinary, ordinaryDiagnostics: diagnostics, refusals: x.registration.refusals.map(r => r.check) };
  }

  for (const [name, attrs, write] of [
    ["eol=crlf", "*.txt eol=crlf\n", (root: string) => fs.writeFileSync(path.join(root, "a.txt"), "one\r\ntwo\r\n")],
    ["working-tree-encoding", "*.txt working-tree-encoding=UTF-16LE-BOM\n", (root: string) =>
      fs.writeFileSync(path.join(root, "a.txt"), Buffer.concat([Buffer.from([0xff, 0xfe]), Buffer.from("hello\n", "utf16le")]))],
  ] as const) {
    it(`a clean ${name} clone: ordinary passes, predecessor refuses as dirty`, async t => {
      const r = repo(t);
      fs.writeFileSync(path.join(r.root, ".gitattributes"), attrs);
      write(r.root);
      r.commit();
      assert.equal(r.g("status", "--porcelain"), "", "precondition: git calls the clone clean");
      const out = await both(r.root);
      assert.equal(out.ordinary, "passed");
      assert.deepEqual(out.ordinaryDiagnostics, []);
      assert.equal(out.predecessor.result, "retained");
      assert.deepEqual(out.refusals, ["worktree_status_dirty"]);
    });
  }

  it("a clean ident clone: ordinary passes, predecessor refuses as dirty", async t => {
    const r = repo(t);
    fs.writeFileSync(path.join(r.root, ".gitattributes"), "*.txt ident\n");
    fs.writeFileSync(path.join(r.root, "a.txt"), "$Id$\n");
    r.commit();
    fs.rmSync(path.join(r.root, "a.txt"));
    r.g("checkout", "--", "a.txt");
    assert.match(fs.readFileSync(path.join(r.root, "a.txt"), "utf8"), /\$Id: [0-9a-f]{40} \$/, "precondition: the worktree holds the expanded form");
    assert.equal(r.g("status", "--porcelain"), "");
    const out = await both(r.root);
    assert.equal(out.ordinary, "passed");
    assert.equal(out.predecessor.result, "retained");
    assert.deepEqual(out.refusals, ["worktree_status_dirty"]);
  });

  it("a gitlink: ordinary as base (passes), predecessor unreadable", async t => {
    const r = repo(t);
    fs.writeFileSync(path.join(r.root, "a.txt"), "a\n");
    r.commit();
    const sha = r.g("rev-parse", "HEAD");
    r.g("update-index", "--add", "--cacheinfo", `160000,${sha},sub`);
    fs.mkdirSync(path.join(r.root, "sub"));
    const base = await git.worktreeStatus(r.root);
    const out = await both(r.root);
    assert.equal(out.ordinary, base?.length === 0 ? "passed" : "retained", "ordinary follows worktreeStatus exactly");
    assert.equal(out.predecessor.result, "retained");
    assert.deepEqual(out.refusals, ["worktree_status_unreadable"]);
  });

  it("a path with a filter attribute: ordinary as base (passes), predecessor unreadable", async t => {
    const r = repo(t);
    fs.writeFileSync(path.join(r.root, ".git", "info", "attributes"), "*.f filter=ident-copy\n");
    fs.appendFileSync(path.join(r.root, ".git", "config"), '[filter "ident-copy"]\n\tclean = cat\n\tsmudge = cat\n');
    fs.writeFileSync(path.join(r.root, "a.f"), "payload\n");
    r.commit();
    assert.equal(r.g("status", "--porcelain"), "");
    const out = await both(r.root);
    assert.equal(out.ordinary, "passed");
    assert.equal(out.predecessor.result, "retained");
    assert.deepEqual(out.refusals, ["worktree_status_unreadable"]);
  });

  it("over the entry cap: ordinary passes, predecessor over_bound", async t => {
    const r = repo(t);
    fs.writeFileSync(path.join(r.root, "a.txt"), "a\n");
    fs.writeFileSync(path.join(r.root, "b.txt"), "b\n");
    r.commit();
    const shared = INVENTORY_CONTENT_PROOF_BOUNDS as { maxEntries: number; maxContentBytes: number };
    const saved = { ...shared };
    try {
      shared.maxEntries = 1;
      const entries = await both(r.root);
      assert.equal(entries.ordinary, "passed");
      assert.deepEqual(entries.refusals, ["over_bound"]);
      shared.maxEntries = saved.maxEntries;
      shared.maxContentBytes = 1;
      const bytes = await both(r.root);
      assert.equal(bytes.ordinary, "passed");
      assert.deepEqual(bytes.refusals, ["over_bound"]);
    } finally { Object.assign(shared, saved); }
  });
});

// Documents a RESIDUAL, it does not claim safety.
describe("escaped-writer residual (documented, #3831-related)", () => {
  it("residual (documented, #3831-related): an escaped writer outside the clone with no attempt marker is not detected", () => {
    const RUNNER = 10002, SELF = 900, CLONE = "/data/runner/repo/issue-1";
    const proc = (cwd: string, environ: string) => ({
      listPids: () => [4242],
      readStatus: () => `Name:\twriter\nState:\tS (sleeping)\nUid:\t${RUNNER}\t${RUNNER}\t${RUNNER}\t${RUNNER}\n`,
      readEnviron: () => environ,
      readCwd: () => cwd,
      readStat: (pid: number) => `${pid} (writer) S 1 ${pid} ${pid} ${"0 ".repeat(15)}5000 0 0`,
    });
    const req = {
      mode: "capture" as const, targetUid: RUNNER, targetKey: "repo/issue-1", targetPaths: [CLONE],
      liveMarkers: [], liveRoots: [], workerNonce: "nonce",
    };
    const scrubbed = "PATH=/usr/bin\0HOME=/tmp\0";
    // Controls: the scan does see a process whose cwd is inside the clone, and one carrying marker plus key.
    const inside = scanOnce(req, proc(CLONE + "/src", scrubbed), SELF);
    assert.equal(inside.survivors.length + inside.kill.length + inside.unverified.length, 1, "cwd inside the clone is in scope");
    const marker = `run-9:${mintAttemptId(1)}`;
    const marked = scanOnce(req, proc("/tmp/elsewhere", `UZI_RUN_ATTEMPT=${marker}\0UZI_RUN_CLONE_KEY=repo/issue-1\0`), SELF);
    assert.equal(marked.kill.length + marked.survivors.length + marked.unverified.length, 1, "marker plus clone key is in scope");
    // The residual: cwd outside every target path and no attempt marker or clone key.
    const escaped = scanOnce(req, proc("/tmp/elsewhere", scrubbed), SELF);
    assert.deepEqual(escaped, { kill: [], survivors: [], unverified: [] }, "not detected: it may still write into the clone");
  });
});
