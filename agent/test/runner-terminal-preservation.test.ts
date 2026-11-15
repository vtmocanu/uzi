import { installSourcePublicationContext } from "./publication-fixture.js";
import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { fixture as codexReapFixture } from "./codex-reap-fixture.js";
import type { StateRequest } from "../src/protocol.js";
import type { RunContext } from "../src/executor.js";
import { Outbox } from "../src/outbox.js";
import { resolvePendingTerminal } from "../src/terminal-resolve.js";
import { CodexBoundaryError } from "../src/codex/safety.js";
import type { Logger } from "../src/log.js";
import { nullLogger } from "./helpers.js";
import { api, baseUrl, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner, runnerWith } from "./runner-harness.js";
import {
  mergeTerminalRecord, type TerminalDelta, type TerminalManifest, type TerminalOperationRecord, type TerminalOperationStore,
} from "../src/terminal-operation.js";
import type { TerminalPreservationResult } from "../src/client.js";
import {
  preserveInLane, terminalHeadAdmitted, type TerminalLaneEnv, type TerminalLaneFlight,
} from "../src/terminal-preservation-lane.js";
import type { TerminalPreservationTestHooks } from "../src/terminal-preservation.js";

installHarness();
const wire = JSON.parse(fs.readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
const literal = wire.ack.completed_publication_receipt;
const feature = "recovery_completed_publication_v1";
const RUN = "11111111-1111-4111-8111-111111111111";
const A = "a".repeat(40), B = "b".repeat(40), C = "c".repeat(40);
const MANIFEST: TerminalManifest = {
  run_id: RUN, generation: 1, worker_id: "w1",
  roots: [{ source_id: "s1", attempt_id: "at1", commit: A, tree: B }], abandoned: [],
};

/** In-memory store over the real monotonic merge, so the engine's record-first and invoke-before-send rules are exercised. */
function memStore(): TerminalOperationStore & { record?: TerminalOperationRecord } {
  const s: TerminalOperationStore & { record?: TerminalOperationRecord } = {
    async read() { return s.record ? { kind: "ok", record: s.record } : { kind: "absent" }; },
    async create(record) {
      if (s.record) return { kind: "exists" };
      s.record = record;
      return { kind: "created", record };
    },
    async update(_r, _g, delta: TerminalDelta) {
      if (!s.record) return { kind: "absent" };
      const m = mergeTerminalRecord(s.record, delta);
      if (!m.ok) return { kind: "rejected", reason: m.reason };
      s.record = m.record;
      return { kind: "ok", record: m.record };
    },
    async addLocalBlock(r, g, reasons, flagged) {
      return s.update(r, g, { addLocalBlock: reasons, ...(flagged?.length ? { addFlaggedCommits: flagged } : {}) });
    },
    async runBlock() {
      const r = s.record;
      if (r && (r.localBlock.length > 0 || r.flaggedCommits.length > 0 || r.closedReason === "secret_found")) return { kind: "blocked", reason: r.localBlock[0] ?? "secret_found" };
      return { kind: "clear" };
    },
    async invoke(_r, _g, at) {
      if (!s.record) return { kind: "absent" };
      if (s.record.localBlock.length > 0) return { kind: "blocked", reason: s.record.localBlock[0]! };
      const m = mergeTerminalRecord(s.record, { appendInvocations: [{ at, outcome: "invoked" }] });
      if (!m.ok) return { kind: "rejected", reason: m.reason };
      s.record = m.record;
      return { kind: "invoked", record: m.record };
    },
  };
  return s;
}

const FINAL = { kind: "final", outcome: "published", response: {} } as unknown as TerminalPreservationResult;

interface Rig {
  store: ReturnType<typeof memStore>;
  sends: number;
  order: string[];
  flight: TerminalLaneFlight;
  env: TerminalLaneEnv;
}

function rig(over: {
  env?: Partial<TerminalLaneEnv>; hooks?: TerminalPreservationTestHooks | undefined;
  send?: (signal?: AbortSignal) => Promise<TerminalPreservationResult>;
  prepare?: () => Promise<unknown>; capture?: () => Promise<unknown>; kind?: string;
} = {}): Rig {
  const store = memStore();
  const r = { store, sends: 0, order: [] as string[] } as Rig;
  r.flight = {
    runId: RUN, claimGeneration: 1, runKind: over.kind ?? "issue", barePath: "/bare", branch: "agent/issue-1",
    worktreePath: "/clone", attemptId: "at1",
  };
  const hooks: TerminalPreservationTestHooks | undefined = "hooks" in over ? over.hooks : {
    workerId: "w1", store, owner: fn => fn(),
    deps: {
      universe: async () => ({ kind: "proved", sources: [], roots: [], universeDigest: "u1" }),
      capture: async onPhase => {
        r.order.push("capture");
        await onPhase("capturing", { cloneRef: "refs/uzi-terminal-local/x/g1", bareRef: "refs/uzi-terminal-local/x/g1" });
        await onPhase("captured", { cloneRef: "refs/uzi-terminal-local/x/g1", bareRef: "refs/uzi-terminal-local/x/g1", commit: A });
        return { kind: "ok", base: A, extraRoots: [], manifest: MANIFEST };
      },
      aggregate: async () => ({ tip: A }),
      prepare: (over.prepare ?? (async () => ({
        pack: Buffer.from("pack"), range: { tipSha: A, excludeSha: C }, digests: { scan: "s", transport: "t" },
      }))) as never,
      coverage: () => ({ ok: true }),
      recheck: async () => undefined,
      send: async (_req, signal) => { r.sends++; r.order.push("send"); return over.send ? over.send(signal) : FINAL; },
    },
  };
  r.env = {
    git: {} as never, recovery: {} as never, client: { publishTerminalPreservation: async () => FINAL }, hooks,
    workerId: "w1", defaultBranch: "main", egressBound: false,
    skip: () => undefined, failureFirst: () => undefined,
    policy: () => ({ version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: null, extraFloors: [] }),
    remediationHold: async () => false, ciHold: async () => "clear",
    log: () => undefined, now: () => Date.now(), ...over.env,
  };
  return r;
}
const body = { status: "completed" } as const;

describe("terminal-preservation lane (#4007 slice B)", () => {
  it("gate off: nothing runs, nothing is stored on the flight, and no head proof is required of a non-branch kind", async () => {
    const r = rig({ hooks: undefined });
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "kept");
    assert.equal(out.reason, "disabled");
    assert.equal(r.flight.terminalPreservation, undefined);
    assert.equal(r.sends, 0);
    assert.equal(r.store.record, undefined);
  });

  it("published: head admitted only when it is a declared manifest root", async () => {
    const r = rig();
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "published");
    assert.equal(r.sends, 1);
    assert.equal(terminalHeadAdmitted(r.flight, A), true);
    assert.equal(terminalHeadAdmitted(r.flight, C), false, "a head outside the manifest is never proven");
    assert.equal(r.store.record?.phase, "closed");
  });

  it("recorded counts as proof too", async () => {
    const r = rig({ send: async () => ({ kind: "final", outcome: "recorded", response: {} } as unknown as TerminalPreservationResult) });
    assert.equal((await preserveInLane(r.env, r.flight, body)).outcome, "recorded");
    assert.equal(terminalHeadAdmitted(r.flight, A), true);
  });

  for (const [name, send] of [
    ["refused", async () => ({ kind: "refused", reason: "forge_unavailable", httpStatus: 409 } as unknown as TerminalPreservationResult)],
    ["unknown", async () => ({ kind: "unknown", cause: "transport" } as TerminalPreservationResult)],
    ["send throws", async (): Promise<TerminalPreservationResult> => { throw new Error("boom"); }],
  ] as const) {
    it(`${name}: no head proof, no throw`, async () => {
      const r = rig({ send });
      const out = await preserveInLane(r.env, r.flight, body);
      assert.notEqual(out.outcome, "published");
      assert.equal(terminalHeadAdmitted(r.flight, A), false);
      assert.equal(r.sends, 1);
    });
  }

  it("a send aborted by the permit after invoked is publish_unknown with no head", async () => {
    const permit = new AbortController();
    const r = rig({
      env: { signal: permit.signal },
      send: async signal => {
        permit.abort();
        assert.equal(signal!.aborted, true, "the permit abort reaches the send");
        return { kind: "unknown", cause: "aborted" };
      },
    });
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "unknown");
    assert.equal(r.store.record?.invocations.at(-1)?.outcome, "publish_unknown");
    assert.equal(terminalHeadAdmitted(r.flight, A), false);
  });

  it("sub-deadline expiry during the send is publish_unknown with no head", async () => {
    const r = rig({
      send: async signal => {
        await new Promise<void>(res => { if (signal!.aborted) res(); else signal!.addEventListener("abort", () => res(), { once: true }); });
        return { kind: "unknown", cause: "aborted" };
      },
    });
    r.env.hooks = { ...r.env.hooks!, capMs: 40 };
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "unknown");
    assert.equal(r.store.record?.invocations.at(-1)?.outcome, "publish_unknown");
    assert.equal(terminalHeadAdmitted(r.flight, A), false);
  });

  it("a permit that fails before invoked sends nothing and proves no head", async () => {
    const permit = new AbortController();
    permit.abort();
    const r = rig({ env: { signal: permit.signal } });
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "kept");
    assert.equal(r.sends, 0);
    assert.equal(r.store.record?.invocations.some(i => i.outcome === "invoked"), false);
    assert.equal(terminalHeadAdmitted(r.flight, A), false);
  });

  it("sub-deadline expiry during preparation keeps the work and sends nothing", async () => {
    const r = rig({
      hooks: undefined,
    });
    r.env.hooks = {
      workerId: "w1", capMs: 15, store: r.store, owner: fn => fn(),
      deps: {
        universe: async () => ({ kind: "proved", sources: [], roots: [], universeDigest: "u1" }),
        capture: async onPhase => {
          await onPhase("capturing", { cloneRef: "x", bareRef: "x" });
          await onPhase("captured", { cloneRef: "x", bareRef: "x" });
          return { kind: "ok", base: A, extraRoots: [], manifest: MANIFEST };
        },
        aggregate: async () => ({ tip: A }),
        prepare: async () => {
          await new Promise(res => setTimeout(res, 60));
          return { pack: Buffer.alloc(0), range: { tipSha: A, excludeSha: C }, digests: { scan: "s", transport: "t" } };
        },
        coverage: () => ({ ok: true }), recheck: async () => undefined,
        send: async () => { r.sends++; return FINAL; },
      },
    };
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "kept");
    assert.equal(r.sends, 0);
  });

  it("a permit too short to leave a usable window (4 s) never starts: kept deadline, no capture, no record", async () => {
    const r = rig({ env: { remainingMs: 4_000 } });
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "kept");
    assert.equal(out.reason, "deadline");
    assert.equal(r.order.length, 0);
    assert.equal(r.sends, 0);
    assert.equal(r.store.record, undefined);
  });

  it("a Codex-shaped 30 s permit runs under a sub-deadline of at most permit minus the proportional reserve", async t => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    let entered!: () => void;
    const inSend = new Promise<void>(res => { entered = res; });
    let sendSignal: AbortSignal | undefined;
    const r = rig({
      env: { remainingMs: 30_000 },
      send: async signal => {
        sendSignal = signal;
        entered();
        await new Promise<void>(res => { if (signal!.aborted) res(); else signal!.addEventListener("abort", () => res(), { once: true }); });
        return { kind: "unknown", cause: "aborted" };
      },
    });
    const run = preserveInLane(r.env, r.flight, body);
    await inSend;
    t.mock.timers.tick(22_499);
    assert.equal(sendSignal!.aborted, false, "reserve = min(30 s, 30 s / 4) leaves 22.5 s");
    t.mock.timers.tick(2);
    assert.equal(sendSignal!.aborted, true, "the send is aborted no later than permit minus reserve");
    const out = await run;
    assert.equal(out.outcome, "unknown");
    assert.equal(r.store.record?.invocations.at(-1)?.outcome, "publish_unknown");
    assert.equal(terminalHeadAdmitted(r.flight, A), false);
  });

  it("a permit abort mid-send on a 30 s permit aborts the send: publish_unknown, no head", async () => {
    const permit = new AbortController();
    const r = rig({
      env: { signal: permit.signal, remainingMs: 30_000 },
      send: async signal => {
        permit.abort();
        assert.equal(signal!.aborted, true);
        return { kind: "unknown", cause: "aborted" };
      },
    });
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "unknown");
    assert.equal(r.store.record?.invocations.at(-1)?.outcome, "publish_unknown");
    assert.equal(terminalHeadAdmitted(r.flight, A), false);
  });

  it("active secret remediation refuses before any universe read, capture or record", async () => {
    let reads = 0;
    const r = rig({ env: { policy: () => ({ version: 1, blocked: true, everKnown: true, flaggedCommits: [], ciGlobs: null, extraFloors: [] }) } });
    const universe = r.env.hooks!.deps!.universe!;
    r.env.hooks!.deps!.universe = async (...a) => { reads++; return universe(...a); };
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "kept");
    assert.equal(out.reason, "secret_remediation_pending");
    assert.equal(reads, 0);
    assert.equal(r.order.length, 0);
    assert.equal(r.store.record, undefined);
    assert.equal(terminalHeadAdmitted(r.flight, A), false);
  });

  it("a remediation hold that appears after prepare stops the send", async () => {
    let held = false;
    const r = rig({
      env: { remediationHold: async () => held },
      prepare: async () => { held = true; return { pack: Buffer.from("pack"), range: { tipSha: A, excludeSha: C }, digests: { scan: "s", transport: "t" } }; },
    });
    delete r.env.hooks!.deps!.recheck; // exercise the lane's own late gate, not the hooked one
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "kept");
    assert.equal(r.sends, 0);
    assert.equal(terminalHeadAdmitted(r.flight, A), false);
  });

  it("a policy that turns blocked after prepare stops the send", async () => {
    let blocked = false;
    const r = rig({
      env: { policy: () => ({ version: 1, blocked, everKnown: false, flaggedCommits: [], ciGlobs: null, extraFloors: [] }) },
      prepare: async () => { blocked = true; return { pack: Buffer.from("pack"), range: { tipSha: A, excludeSha: C }, digests: { scan: "s", transport: "t" } }; },
    });
    delete r.env.hooks!.deps!.recheck;
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "kept");
    assert.equal(r.sends, 0);
  });

  it("an unknown permit remainder uses the cap and runs", async () => {
    const r = rig();
    assert.equal((await preserveInLane(r.env, r.flight, body)).outcome, "published");
  });

  it("quiesce receives the lane's own sub-deadline", async () => {
    let seen: { deadlineAt: number } | undefined;
    const r = rig({ env: { remainingMs: 20_000, quiesce: async o => { seen = o; } } });
    const before = Date.now();
    await preserveInLane(r.env, r.flight, body);
    assert.ok(seen, "quiesce got an options argument");
    // sub = min(cap, permit - min(reserve, permit/4)); never later than now + the permit remainder.
    assert.ok(seen.deadlineAt > before && seen.deadlineAt <= Date.now() + 20_000, `deadlineAt ${seen.deadlineAt - before} ms ahead`);
  });

  it("a quiesce that returns after the sub-deadline keeps the work: deadline, nothing read, captured or stored", async () => {
    const r = rig({ env: { quiesce: async () => { await new Promise(res => setTimeout(res, 120)); } } });
    r.env.hooks = { ...r.env.hooks!, capMs: 40, deps: { ...r.env.hooks!.deps!, universe: async () => { r.order.push("universe"); throw new Error("must not run"); } } };
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "kept");
    assert.equal(out.reason, "deadline");
    assert.deepEqual(r.order, []);
    assert.equal(r.sends, 0);
    assert.equal(r.store.record, undefined);
  });

  it("a default-branch lookup cut short by the window keeps the output as default_branch_unresolved, never main", async () => {
    let tipCalls = 0;
    const r = rig({ env: {
      defaultBranch: undefined,
      git: { defaultBranchTip: async () => { tipCalls++; return undefined; } } as never,
      lookupDefaultBranch: async b => {
        await new Promise<void>(res => { if (b.signal.aborted) res(); else b.signal.addEventListener("abort", () => res(), { once: true }); });
        return { kind: "absent" };
      },
    } });
    r.env.hooks = { ...r.env.hooks!, capMs: 150, deps: { ...r.env.hooks!.deps!, universe: async () => { r.order.push("universe"); throw new Error("must not run"); } } };
    const started = Date.now();
    const out = await preserveInLane(r.env, r.flight, body);
    const elapsed = Date.now() - started;
    assert.equal(out.outcome, "kept");
    assert.equal(out.reason, "default_branch_unresolved");
    assert.ok(elapsed < 150 + 1500, `elapsed ${elapsed} ms`);
    assert.equal(tipCalls, 0);
    assert.deepEqual(r.order, []);
    assert.equal(r.sends, 0);
    assert.equal(r.store.record, undefined);
  });

  // The rig's hooked prepare would hide the resolved branch, so these two drop it and let the lane's real prepare run; the
  // observable is the branch the lane hands to git.defaultBranchTip. Whatever that real prepare then does with the stub
  // git (it is not a real repository) is not under test.
  async function resolvedBranch(lookup: TerminalLaneEnv["lookupDefaultBranch"]): Promise<{ branches: string[]; reason: string | undefined; looked: number }> {
    const branches: string[] = [];
    let looked = 0;
    const r = rig({ env: {
      defaultBranch: undefined,
      git: { defaultBranchTip: async (_bare: string, b: string) => { branches.push(b); return undefined; } } as never,
      lookupDefaultBranch: async b => { looked++; return lookup!(b); },
    } });
    const { prepare: _hooked, ...deps } = r.env.hooks!.deps!;
    r.env.hooks = { ...r.env.hooks!, deps };
    const out = await preserveInLane(r.env, r.flight, body);
    return { branches, reason: out.reason, looked };
  }

  it("a default-branch lookup that proves no default branch with the budget intact is not refused and falls back to main", async () => {
    const o = await resolvedBranch(async () => ({ kind: "absent" }));
    assert.equal(o.looked, 1);
    assert.notEqual(o.reason, "default_branch_unresolved");
    assert.deepEqual(o.branches, ["main"]);
  });

  it("a default-branch lookup that resolves a name hands that branch to the tip read", async () => {
    const o = await resolvedBranch(async () => ({ kind: "resolved", name: "develop" }));
    assert.equal(o.looked, 1);
    assert.notEqual(o.reason, "default_branch_unresolved");
    assert.deepEqual(o.branches, ["develop"]);
  });

  async function keptUnresolved(lookup: NonNullable<TerminalLaneEnv["lookupDefaultBranch"]>) {
    const r = rig({ env: { defaultBranch: undefined, lookupDefaultBranch: lookup } });
    const out = await preserveInLane(r.env, r.flight, body);
    return { out, r };
  }

  it("a default-branch lookup that throws with the budget intact keeps the output: nothing captured or sent", async () => {
    const o = await keptUnresolved(async () => { throw new Error("lookup failed"); });
    assert.equal(o.out.outcome, "kept");
    assert.equal(o.out.reason, "default_branch_unresolved");
    assert.deepEqual(o.r.order, []);
    assert.equal(o.r.sends, 0);
    assert.equal(o.r.store.record, undefined);
  });

  it("a default-branch lookup that reports unknown with the budget intact keeps the output: nothing captured or sent", async () => {
    const o = await keptUnresolved(async () => ({ kind: "unknown", detail: "dangling" }));
    assert.equal(o.out.outcome, "kept");
    assert.equal(o.out.reason, "default_branch_unresolved");
    assert.deepEqual(o.r.order, []);
    assert.equal(o.r.sends, 0);
    assert.equal(o.r.store.record, undefined);
  });

  // The tip read runs under the lane's window through env.readDefaultBranchTip; the real prepare runs (hooked one dropped) and
  // any git call other than the tip read is recorded, so a refusal before prepareFrozenHeldPinned shows as an empty list.
  async function tipReadRig(readTip: NonNullable<TerminalLaneEnv["readDefaultBranchTip"]>, capMs: number) {
    const otherGitCalls: string[] = [];
    const git = new Proxy({}, { get: (_t, name) => (name === "defaultBranchTip"
      ? async () => { throw new Error("the lane must use readDefaultBranchTip"); }
      : async () => { otherGitCalls.push(String(name)); return undefined; }) });
    const r = rig({ env: { defaultBranch: "main", git: git as never, readDefaultBranchTip: readTip } });
    const { prepare: _hooked, ...deps } = r.env.hooks!.deps!;
    r.env.hooks = { ...r.env.hooks!, capMs, deps };
    const started = Date.now();
    const out = await preserveInLane(r.env, r.flight, body);
    return { out, elapsed: Date.now() - started, otherGitCalls, r };
  }

  it("a default-branch tip read that stalls past the window keeps the output as a prepare deadline before any prepare git call", async () => {
    const capMs = 150;
    const o = await tipReadRig(async (_bare, _branch, b) => {
      await new Promise<void>(res => { if (b.signal.aborted) res(); else b.signal.addEventListener("abort", () => res(), { once: true }); });
      return undefined;
    }, capMs);
    assert.equal(o.out.outcome, "kept");
    assert.match(String(o.out.reason), /deadline/);
    assert.ok(o.elapsed < capMs + 1500, `elapsed ${o.elapsed} ms`);
    assert.deepEqual(o.otherGitCalls, [], "prepareFrozenHeldPinned never started");
    assert.equal(o.r.sends, 0);
  });

  it("the tip read is handed the lane's live signal and a deadline inside the lane window", async () => {
    let seen: { aborted: boolean; deadline: number; branch: string; at: number } | undefined;
    const before = Date.now();
    const o = await tipReadRig(async (_bare, branch, b) => {
      seen = { aborted: b.signal.aborted, deadline: b.deadline, branch, at: Date.now() };
      return undefined;
    }, 5_000);
    assert.ok(seen, "the tip read ran");
    assert.equal(seen.aborted, false);
    assert.equal(seen.branch, "main");
    assert.ok(Number.isFinite(seen.deadline) && seen.deadline > seen.at && seen.deadline <= before + 5_000 + 1_000, `deadline ${seen.deadline - before} ms ahead`);
    void o;
  });

  it("a cooperative quiesce that honours the deadlineAt it receives returns the lane kept within its budget", async () => {
    const cap = 200;
    let received: number | undefined;
    const r = rig({ env: { quiesce: async o => {
      received = o.deadlineAt;
      await new Promise(res => setTimeout(res, Math.max(0, o.deadlineAt - Date.now())));
      throw new Error("quiescence deadline");
    } } });
    r.env.hooks = { ...r.env.hooks!, capMs: cap, deps: { ...r.env.hooks!.deps!, universe: async () => { r.order.push("universe"); throw new Error("must not run"); } } };
    const started = Date.now();
    const out = await preserveInLane(r.env, r.flight, body);
    const elapsed = Date.now() - started;
    assert.ok(received !== undefined, "quiesce received its deadline");
    assert.equal(out.outcome, "kept");
    assert.ok(out.reason === "deadline" || out.reason === "not_quiescent", `reason ${out.reason}`);
    assert.ok(elapsed < 1500, `returned within the cap plus slack (took ${elapsed} ms)`);
    assert.deepEqual(r.order, [], "no universe or capture call");
    assert.equal(r.sends, 0);
    assert.equal(r.store.record, undefined, "nothing stored");
  });

  it("a permit abort during quiesce keeps the work: deadline, nothing read, captured or stored", async () => {
    const permit = new AbortController();
    const r = rig({ env: { signal: permit.signal, quiesce: async () => { permit.abort(); } } });
    const out = await preserveInLane(r.env, r.flight, body);
    assert.equal(out.outcome, "kept");
    assert.equal(out.reason, "deadline");
    assert.deepEqual(r.order, []);
    assert.equal(r.sends, 0);
    assert.equal(r.store.record, undefined);
  });

  it("direct quiescence runs before capture; a failed proof keeps the work and sends nothing", async () => {
    const ok = rig();
    ok.env.quiesce = async () => { ok.order.push("quiesce"); };
    await preserveInLane(ok.env, ok.flight, body);
    assert.deepEqual(ok.order.slice(0, 2), ["quiesce", "capture"]);
    const bad = rig({ env: { quiesce: async () => { throw new Error("residue"); } } });
    const out = await preserveInLane(bad.env, bad.flight, body);
    assert.equal(out.reason, "not_quiescent");
    assert.equal(bad.sends, 0);
    assert.equal(bad.order.length, 0);
  });

  for (const [name, reason] of [
    ["policy hold / park", "policy_or_park"], ["pending terminal", "pending_terminal"],
    ["previously attempted", "previously_attempted"], ["permanent-failure reap", "permanent_failure_reap"],
    ["claim fence", "claim_fence"],
  ] as const) {
    it(`skips on ${name}`, async () => {
      const r = rig({ env: { skip: () => reason } });
      const out = await preserveInLane(r.env, r.flight, body);
      assert.equal(out.reason, reason);
      assert.equal(r.order.length, 0);
      assert.equal(terminalHeadAdmitted(r.flight, A), false, "a skipped branch-source completion carries no proven head");
    });
  }

  for (const origin of ["push_secret_blocked", "worker_residue_blocked"]) {
    it(`failure-first: ${origin} preserves nothing`, async () => {
      const r = rig();
      const out = await preserveInLane(r.env, r.flight, { status: "failed", fail_origin: origin });
      assert.equal(out.reason, origin);
      assert.equal(r.order.length, 0);
    });
  }

  it("failure-first: an env latch (secret block, residue quarantine) preserves nothing", async () => {
    const r = rig({ env: { failureFirst: () => "residue_quarantined" } });
    assert.equal((await preserveInLane(r.env, r.flight, { status: "failed" })).reason, "residue_quarantined");
    assert.equal(r.order.length, 0);
  });

  for (const kind of ["ci_fix", "chat", "judge", "cross_check", "task"]) {
    it(`${kind} is not branch source: no send, and its head is not gated`, async () => {
      const r = rig({ kind });
      const out = await preserveInLane(r.env, r.flight, body);
      assert.equal(out.outcome, "kept");
      assert.equal(r.sends, 0);
      assert.equal(terminalHeadAdmitted(r.flight, A), true);
    });
  }

  it("a secret finding latches the flight and sends nothing", async () => {
    const r = rig({ prepare: async () => ({ reason: "secret_found", flaggedCommits: [A] }) });
    const out = await preserveInLane(r.env, r.flight, { status: "failed" });
    assert.equal(out.reason, "secret_found");
    assert.equal(r.flight.secretBlocked, true);
    assert.equal(r.sends, 0);
    assert.deepEqual(r.store.record?.localBlock, ["secret_found"]);
  });

  it("is total: a throwing capture or a throwing predicate yields kept, never a throw", async () => {
    const a = rig({ capture: async () => { throw new Error("x"); } });
    a.env.hooks!.deps!.capture = async () => { throw new Error("capture exploded"); };
    assert.equal((await preserveInLane(a.env, a.flight, body)).outcome, "kept");
    const b = rig({ env: { skip: () => { throw new Error("predicate exploded"); } } });
    const out = await preserveInLane(b.env, b.flight, body);
    assert.equal(out.outcome, "kept");
    assert.match(out.reason!, /^lane_error/);
  });

  it("is idempotent per flight: the second call returns the first result and sends nothing more", async () => {
    const r = rig();
    const first = await preserveInLane(r.env, r.flight, body);
    const second = await preserveInLane(r.env, r.flight, body);
    assert.deepEqual(second, first);
    assert.equal(r.sends, 1);
  });
});

// ---- full-execute drives -----------------------------------------------------------------------------------

function command(dir: string, args: string[], input?: Buffer): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", input }).trim();
}
function commit(dir: string, name: string): string {
  fs.writeFileSync(path.join(dir, name), name + "\n");
  command(dir, ["add", name]);
  command(dir, ["-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", name]);
  return command(dir, ["rev-parse", "HEAD"]);
}

type SendKind = "published" | "refused" | "unknown" | "throws";
interface Drive {
  kind?: "issue" | "mr_rework" | "self_improve" | "ci_fix" | "chat";
  codex?: boolean;
  /** false: no override at all (the production-dark gate stays off). */
  gate?: boolean;
  send?: SendKind;
  /** Replaces the manifest root commit (default: the delivered head). */
  rootCommit?: string;
  capMs?: number;
  prepareDelayMs?: number;
  /** A confirmed-empty diff: the typed no-changes failure of phasePublish. */
  emptyDiff?: boolean;
  failRun?: string;
  deps?: NonNullable<TerminalPreservationTestHooks["deps"]>;
  /** Every completed-report response is lost (the server applied it): the terminal journal stays pending for a replay. */
  lostAcks?: boolean;
  /** Codex: the finalize boundary fails after the lane prepared and before it invoked, then again after the publish returned. */
  unwind?: boolean;
}
interface Driven {
  completed: StateRequest[];
  failed: StateRequest[];
  finalHead: string;
  sends: number;
  withBoundaryCalls: string[];
  quiesced: number;
  /** Arguments of every quiesceOrBlockSink call, in order. */
  quiesceArgs: unknown[][];
  order: string[];
  lane: unknown[];
  sites: Array<{ signal?: AbortSignal; deadlineAt?: number; at: number }>;
  /** The state of the fake server's terminal contribution for the completing generation (undefined: none recorded). */
  contribution?: "published" | "refused" | "publish_unknown";
  /** Completed reports the fake server verified and released custody for, and the reasons it refused the others. */
  serverReleases: number;
  refusals: string[];
  /** Whether the worker has been told to release custody (the worker only does so on an authenticated receipt). */
  custodyReleasedByWorker: () => boolean;
  journal: () => Promise<{ body: StateRequest } | undefined>;
  /** The real worker-side replay of the pending terminal journal over client.reportState. */
  replay: () => Promise<void>;
  warnings: string[];
  unwindSignal: AbortSignal;
}

async function drive(t: TestContext, d: Drive = {}): Promise<Driven> {
  const kind = d.kind ?? "issue";
  const claim = gitlabClaim(2507, {
    run_id: literal.run_id, kind, claim_generation: 29, inventory_guarded: true,
    ...(kind === "mr_rework" ? { issue_iid: null, branch: "agent/issue-42", base_branch: "main" } : {}),
    repo: { id: literal.repo_id, url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath, forge_type: "gitlab" },
  });
  const branch = kind === "self_improve" ? `uzi/self-improve/${claim.run_id}` : kind === "mr_rework" ? "agent/issue-42" : "agent/issue-2507";
  if (kind === "mr_rework") installSourcePublicationContext(api, claim, command(fx.originPath, ["rev-parse", "main"]), null, "not_applicable");
  api.setRegisterProtocolFeatures(["claim_generation_fence", "recovery_inventory_v1", feature, "terminal_rejection_report",
    ...(kind === "mr_rework" ? ["refresh_publication_context_v1"] : [])]);
  const registration = await client.register("tp-lane", undefined, undefined, undefined, ["claim_generation_fence", "recovery_inventory_v1", feature]);
  const out: Driven = {
    completed: [], failed: [], finalHead: "", sends: 0, withBoundaryCalls: [], quiesced: 0, quiesceArgs: [], order: [], lane: [], sites: [],
    serverReleases: 0, refusals: [], warnings: [], unwindSignal: undefined as never,
    custodyReleasedByWorker: () => !open, journal: async () => undefined, replay: async () => undefined,
  };
  let open = true;
  let replaying = false;
  client.getRunOwnership = async () => ({
    status: api.states.some(s => s.body.status === "completed") ? "completed" : "running",
    claim_generation: claim.claim_generation, inventory_guarded: true,
  });
  client.listRecoveryHolds = async runId => ({ run_id: runId, holds: open ? [{
    hold_id: literal.hold_id, generation: claim.claim_generation!, inventory_guarded: true, has_available_capture: false }] : [] });
  client.reserveRecoveryCapture = async () => { throw new Error("capture reservation refused: hold closed"); };
  client.releaseRecoveryCustody = async (runId, generation) => {
    open = false;
    return { run_id: runId, generation, released: true, holds_released: 1 };
  };
  client.publishCheckpoint = async (_run, tip, pack) => {
    const chunks: Buffer[] = [];
    for await (const chunk of pack) chunks.push(Buffer.from(chunk));
    command(fx.originPath, ["index-pack", "--stdin", "--fix-thin"], Buffer.concat(chunks));
    command(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, tip]);
    return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${branch}` } };
  };
  const safetyFx = d.codex ? codexReapFixture([], () => false) : undefined;
  const unwindControl = new AbortController();
  out.unwindSignal = unwindControl.signal;
  if (safetyFx) {
    t.after(() => fs.rmSync(safetyFx.root, { recursive: true, force: true }));
    const wb = safetyFx.safety.withBoundary.bind(safetyFx.safety);
    safetyFx.safety.withBoundary = (request, action) => {
      out.withBoundaryCalls.push(request.boundary);
      if (d.unwind && request.boundary === "finalize") {
        // The real facade runs the real publish; only its permit signal is ours (aborted mid-lane by the prepare dep) and the
        // boundary reports a failure once the publish has returned, exactly as a deadline expiring around a committed publish does.
        return wb(request, async permit => {
          // The permit keeps its identity (spawnBoundaryProcess refuses a copy as stale); only the signal the runner reads is ours.
          const realSignal = permit.signal;
          Object.defineProperty(permit, "signal", { value: AbortSignal.any([realSignal, unwindControl.signal]), configurable: true });
          await action(permit);
          throw new CodexBoundaryError("action", [{ category: "protocol", message: "finalize deadline" } as never]);
        });
      }
      return wb(request, action);
    };
  }
  const outbox = new Outbox({ root: path.join(fx.dataDir, "outbox"), log: nullLogger(), runMaxBytes: 64 << 20, maxBytes: 128 << 20, retentionMs: 86_400_000 });
  await outbox.init();
  const forge = fakeGitlab();
  const send = d.send ?? "published";
  if (d.emptyDiff) t.mock.method(git, "changedFiles", async () => []);
  const hooks: TerminalPreservationTestHooks | undefined = d.gate === false ? undefined : {
    workerId: registration.worker_id!, attemptId: "at1", ...(d.capMs !== undefined ? { capMs: d.capMs } : {}),
    deps: {
      universe: async () => ({ kind: "proved", sources: [], roots: [], universeDigest: "u1" }),
      capture: async onPhase => {
        out.order.push("capture");
        await onPhase("capturing", { cloneRef: "refs/uzi-terminal-local/x/g1", bareRef: "refs/uzi-terminal-local/x/g1" });
        await onPhase("captured", { cloneRef: "refs/uzi-terminal-local/x/g1", bareRef: "refs/uzi-terminal-local/x/g1" });
        return { kind: "ok", base: out.finalHead, extraRoots: [], manifest: {
          run_id: claim.run_id, generation: claim.claim_generation!, worker_id: registration.worker_id!,
          roots: [{ source_id: "s1", attempt_id: "at1", commit: d.rootCommit ?? out.finalHead, tree: B }], abandoned: [] } };
      },
      aggregate: async () => ({ tip: out.finalHead }),
      prepare: async () => (d.prepareDelayMs ? new Promise<void>(res => setTimeout(res, d.prepareDelayMs)) : Promise.resolve()).then(() => {
        if (d.unwind) unwindControl.abort();
        return { pack: Buffer.alloc(0), range: { tipSha: out.finalHead, excludeSha: C }, digests: { scan: "s", transport: "t" } };
      }),
      coverage: () => ({ ok: true }),
      recheck: async () => undefined,
      send: async () => {
        out.sends++;
        out.order.push("send");
        if (send === "throws") { out.contribution = "publish_unknown"; throw new Error("send exploded"); }
        if (send === "refused") { out.contribution = "refused"; return { kind: "refused", reason: "forge_unavailable", httpStatus: 409 } as unknown as TerminalPreservationResult; }
        if (send === "unknown") { out.contribution = "publish_unknown"; return { kind: "unknown", cause: "transport" }; }
        out.contribution = "published";
        return FINAL;
      },
      ...d.deps,
    },
  };
  const warnLog: Logger = { ...nullLogger(), warn: (message: string) => { out.warnings.push(message); }, child: () => warnLog };
  const executor = { ...(safetyFx ? { safety: safetyFx.safety } : {}), run: async (ctx: RunContext) => {
    const clone = ctx.worktreePath;
    commit(clone, "checkpoint.txt");
    await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
    out.finalHead = commit(clone, "post-checkpoint.txt");
    forge.pr.head = out.finalHead;
    if (d.failRun) throw new Error(d.failRun);
    return { branch: ctx.branch };
  } } as never;
  const r = runnerWith(() => ({ executor }), forge.gitlab, "tp-lane-key", warnLog, {
    outbox, checkpointIntervalMs: 1, checkpointTickIntervalMs: 0,
    ...(hooks ? { terminalPreservationTestOverride: hooks } : {}),
  });
  const quiesce = t.mock.method(r as unknown as { quiesceOrBlockSink: (...a: unknown[]) => Promise<void> }, "quiesceOrBlockSink");
  const lane = r as unknown as { preserveTerminalInLane: (...a: unknown[]) => Promise<unknown> };
  const realLane = lane.preserveTerminalInLane.bind(r);
  t.mock.method(lane, "preserveTerminalInLane", async (...a: unknown[]) => { out.sites.push({ ...(a[3] as object), at: Date.now() }); const res = await realLane(...a); out.lane.push(res); return res; });
  const originalFetch = globalThis.fetch;
  t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    if (url.origin === new URL(baseUrl).origin && url.pathname.endsWith("/terminal-rejection-custody")) {
      return Response.json({ run_id: claim.run_id, worker_id: registration.worker_id, generation: claim.claim_generation,
        exact_holds: [{ id: literal.hold_id, state: open ? "open" : "released" }], sibling_holds: [], exact_count: 1, sibling_count: 0,
        complete: true, exact_complete: true, sibling_complete: true, outcome: open ? "retained" : "settled" });
    }
    const response = await originalFetch(input, init);
    if (url.origin !== new URL(baseUrl).origin || url.pathname !== `/api/worker/runs/${claim.run_id}/state`) return response;
    const body = JSON.parse(String(init?.body)) as StateRequest;
    if (body.status === "completed") {
      out.completed.push(body);
      const ack = { ...wire.ack, run: { ...wire.ack.run, id: claim.run_id, worker_id: registration.worker_id } };
      // Mirrors workersvc verifyCompletedPublication: a body without the frozen head is not capable, and a head is
      // released only while the generation's terminal contribution is absent or published/recorded (terminalGateReason).
      const reason = !body.completion_final_head ? "completion_identity_missing"
        : out.contribution === undefined || out.contribution === "published" ? ""
          : out.contribution === "refused" ? "terminal_preservation_refused" : "terminal_preservation_pending";
      if (reason === "") {
        out.serverReleases++;
        ack.completed_publication_receipt = {
          ...literal, run_id: claim.run_id, worker_id: registration.worker_id, generation: claim.claim_generation,
          repo_id: claim.repo!.id, branch, final_head: out.finalHead, observed_branch_head: out.finalHead, mr_iid: body.mr_iid };
      } else {
        out.refusals.push(reason);
        delete ack.completed_publication_receipt;
      }
      if (d.lostAcks && !replaying) throw new TypeError("response lost after the server applied the completed report");
      return Response.json(ack);
    }
    if (body.status === "failed") out.failed.push(body);
    return response;
  });
  out.journal = () => outbox.readTerminalJournal(claim.run_id, claim.claim_generation!) as Promise<{ body: StateRequest } | undefined>;
  out.replay = async () => {
    replaying = true;
    const fenced = (body: StateRequest): StateRequest => ({ ...body, claim_generation: claim.claim_generation! });
    // The worker's boot replay: resolvePendingTerminal over client.reportState, stamped with the journal's generation.
    await resolvePendingTerminal(r.protectRecoveryTerminalDeps({ outbox, client, gapFillMax: 100, terminalMaxBytes: 1 << 20, log: nullLogger() } as never), {
      runId: claim.run_id, claimGeneration: claim.claim_generation!,
      send: async body => {
        const ack = await client.reportState(claim.run_id, fenced(body));
        await r.observeSettlementTerminalAck(claim.run_id, claim.claim_generation!, fenced(body), ack);
        return ack;
      },
    });
  };
  await r.execute(claim);
  out.quiesced = quiesce.mock.callCount();
  out.quiesceArgs = quiesce.mock.calls.map(c => [...c.arguments]);
  return out;
}


/** The terminal bodies today's code produces for these scenarios (commit ids normalised), captured with the gate off. */
const COMPLETED_WITH_HEAD = '{"status":"completed","branch":"agent/issue-2507","mr_iid":42,"mr_web_url":"https://gitlab.example.test/org/repo/-/merge_requests/42","completion_final_head":"<head0>","claim_generation":29}';
const COMPLETED_NO_HEAD = '{"status":"completed","branch":"agent/issue-2507","mr_iid":42,"mr_web_url":"https://gitlab.example.test/org/repo/-/merge_requests/42","claim_generation":29}';
const FAILED_GENERIC = '{"status":"failed","failure_reason":"agent exploded","claim_generation":29}';
const normalise = (value: unknown, heads: string[]): string =>
  heads.reduce((text, head, i) => text.split(head).join(`<head${i}>`), JSON.stringify(value));
const statuses = (): unknown[] => api.states.map(x => x.body.status);
/** withBoundary requests the harness opens with the gate off (the lane must add none). */
const BOUNDARIES = { completed: { claude: [], codex: ["checkpoint", "finalize"] }, failed: { claude: [], codex: ["checkpoint", "terminal"] } };

describe("terminal preservation through a full execute", { concurrency: false }, () => {
  for (const codex of [false, true]) {
    const harness = codex ? "codex" : "claude";
    const h = codex ? "codex" : "claude";

    it(`${harness}: gate OFF golden: untouched body, call order and boundaries; the lane never runs`, async t => {
      const off = await drive(t, { codex, gate: false });
      assert.equal(normalise(off.completed[0], [off.finalHead]), COMPLETED_WITH_HEAD);
      assert.deepEqual(statuses(), ["running", "running", "completed"]);
      assert.deepEqual(off.withBoundaryCalls, BOUNDARIES.completed[h]);
      assert.equal(off.quiesced, 1);
      assert.equal(off.sends, 0);
      assert.deepEqual(off.order, []);
      assert.equal(off.lane.length, 0);
    });

    it(`${harness}: gate OFF golden (failed): untouched failed body`, async t => {
      const off = await drive(t, { codex, gate: false, failRun: "agent exploded" });
      assert.equal(normalise(off.failed[0], [off.finalHead]), FAILED_GENERIC);
      assert.deepEqual(statuses(), ["running", "running", "failed"]);
      assert.deepEqual(off.withBoundaryCalls, BOUNDARIES.failed[h]);
      assert.equal(off.quiesced, 2);
      assert.equal(off.lane.length, 0);
    });

    it(`${harness}: published attaches completion_final_head, quiesces directly and opens no boundary of its own`, async t => {
      const on = await drive(t, { codex });
      assert.equal(normalise(on.completed[0], [on.finalHead]), COMPLETED_WITH_HEAD);
      assert.deepEqual(statuses(), ["running", "running", "completed"]);
      assert.equal(on.sends, 1);
      assert.equal(on.quiesced, 2, "exactly one direct quiescence proof (quiesceOrBlockSink) more than the gate-off run");
      assert.deepEqual(on.withBoundaryCalls, BOUNDARIES.completed[h], "no withBoundary from the lane");
    });

    for (const send of ["refused", "unknown", "throws"] as const) {
      it(`${harness}: ${send} send proves no head but the terminal report still goes out`, async t => {
        const out = await drive(t, { codex, send });
        assert.equal(out.sends, 1);
        assert.equal(out.completed.length, 1);
        assert.equal(normalise(out.completed[0], [out.finalHead]), COMPLETED_NO_HEAD, "no unproven head on the wire");
        assert.equal(out.failed.length, 0);
        assert.deepEqual(out.withBoundaryCalls, BOUNDARIES.completed[h]);
      });
    }

    it(`${harness}: a head that is not a declared manifest root is never attached`, async t => {
      const out = await drive(t, { codex, rootCommit: "d".repeat(40) });
      assert.equal(out.sends, 1);
      assert.equal(normalise(out.completed[0], [out.finalHead]), COMPLETED_NO_HEAD);
    });

    it(`${harness}: sub-deadline expiry before invoked sends nothing, proves no head, and the run still completes`, async t => {
      const out = await drive(t, { codex, capMs: 40, prepareDelayMs: 300 });
      assert.equal(out.sends, 0);
      assert.equal(normalise(out.completed[0], [out.finalHead]), COMPLETED_NO_HEAD);
      assert.equal(out.failed.length, 0);
    });

    it(`${harness}: a generic failure runs the lane from the reap body and reports the same failure`, async t => {
      const on = await drive(t, { codex, failRun: "agent exploded" });
      assert.deepEqual(on.order, ["capture", "send"]);
      assert.equal(normalise(on.failed[0], [on.finalHead]), FAILED_GENERIC);
      assert.deepEqual(statuses(), ["running", "running", "failed"]);
      assert.equal(on.quiesced, 2, "the reap already proved quiescence: no extra proof");
      assert.deepEqual(on.withBoundaryCalls, BOUNDARIES.failed[h]);
    });

    it(`${harness}: the lane's quiesce closure forwards its sub-deadline into quiesceOrBlockSink`, async t => {
      const on = await drive(t, { codex });
      const forwarded = on.quiesceArgs.filter(a => (a[3] as { deadlineAt?: number } | undefined)?.deadlineAt !== undefined);
      assert.equal(forwarded.length, 1, "exactly the lane's call carries a deadline");
      const at = (forwarded[0]![3] as { deadlineAt: number }).deadlineAt;
      assert.ok(at > Date.now() - 600_000 && at <= Date.now() + 130_000, "an absolute epoch-ms deadline within the lane cap");
      assert.ok((forwarded[0]![3] as { stop?: { signal?: unknown } }).stop?.signal instanceof AbortSignal, "the lane's abort signal rides along as the sink-gate stop");
    });

    it(`${harness}: the reap-body lane call carries the boundary deadline${codex ? " and the permit signal" : ""}`, async t => {
      const on = await drive(t, { codex, failRun: "agent exploded" });
      const site = on.sites[0]!;
      assert.equal(typeof site.deadlineAt, "number", "the reap body passes the boundary deadline, not the 120 s cap");
      const window = site.deadlineAt! - site.at;
      assert.ok(window > 0 && window <= 30_000, `deadline window ${window} ms is within the 30 s boundary`);
      if (codex) assert.ok(site.signal instanceof AbortSignal, "the Codex permit signal reaches the lane");
    });
  }

  // Release ordering: the worker-side half (the frozen body, its journal, its replay) against a fake server whose
  // completed-publication verification follows workersvc verifyCompletedPublication.
  for (const codex of [false, true]) {
    const harness = codex ? "codex" : "claude";
    for (const send of ["refused", "unknown", "throws"] as const) {
      it(`${harness}: ${send} terminal contribution: the journaled body and its replay carry no head, and the server refuses release`, async t => {
        const out = await drive(t, { codex, send, lostAcks: true });
        const journaled = await out.journal();
        assert.ok(journaled, "the completed terminal is journaled write-ahead and still pending after the lost responses");
        assert.equal(journaled.body.completion_final_head, undefined, "no unproven head in the journal");
        assert.ok(out.completed.length > 0 && out.completed.every(b => b.completion_final_head === undefined), "no unproven head on any initial send");
        assert.equal(out.custodyReleasedByWorker(), false);
        await out.replay();
        const replayed = out.completed.at(-1)!;
        assert.equal(replayed.completion_final_head, undefined, "the replayed body is the journaled one: still no head");
        assert.equal(normalise(replayed, [out.finalHead]), COMPLETED_NO_HEAD);
        assert.equal(out.serverReleases, 0, "the server never releases for an unproven terminal");
        assert.ok(out.refusals.length > 0 && out.refusals.every(r => r === "completion_identity_missing"));
        assert.equal(out.custodyReleasedByWorker(), false, "custody stays open");
        assert.equal(out.failed.length, 0);
      });
    }

    it(`${harness}: a published contribution: the journaled body carries the head, the replay is released by the server`, async t => {
      const out = await drive(t, { codex, lostAcks: true });
      const journaled = await out.journal();
      assert.ok(journaled);
      assert.equal(journaled.body.completion_final_head, out.finalHead, "the proven head is journaled");
      assert.ok(out.serverReleases > 0, "the server verified the head against the published contribution and released");
      assert.equal(out.custodyReleasedByWorker(), false, "the worker was never told: every response was lost");
      const before = out.completed.length;
      await out.replay();
      assert.equal(out.completed.length, before + 1);
      assert.equal(normalise(out.completed.at(-1), [out.finalHead]), COMPLETED_WITH_HEAD);
      assert.equal(await out.journal(), undefined, "the replay retires the journal");
      assert.deepEqual(out.refusals, []);
    });
  }

  it("the fake server's rule: a head over an unproven contribution is refused, a headless body never verified", async t => {
    const out = await drive(t, { send: "unknown" });
    assert.equal(out.contribution, "publish_unknown");
    const forged = { ...out.completed[0]!, completion_final_head: out.finalHead };
    const ack = await client.reportState(literal.run_id, forged as StateRequest).catch(() => undefined);
    assert.equal(ack?.completedPublicationReceipt, undefined);
    assert.equal(out.refusals.at(-1), "terminal_preservation_pending", "the model refuses a head over a pending contribution");
    assert.equal(out.serverReleases, 0);
  });

  it("codex committed unwind: a finalize boundary that fails after the lane prepared and before it invoked sends nothing, proves no head, and still reports the committed publication", async t => {
    const out = await drive(t, { codex: true, unwind: true });
    assert.deepEqual(out.order, ["capture"], "the lane captured and prepared, then never sent");
    assert.equal(out.sends, 0, "zero terminal-preservation sends");
    assert.ok(out.warnings.some(w => /Codex finalize boundary failed after committed publish/.test(w)), "the real finalize catch took the committed arm");
    assert.equal(out.failed.length, 0, "the committed publication is not reported failed");
    assert.equal(out.completed.length, 1);
    assert.equal(normalise(out.completed[0], [out.finalHead]), COMPLETED_NO_HEAD, "the deferred committed report carries the branch and MR facts and no head");
    assert.equal(out.serverReleases, 0);
    assert.deepEqual(statuses(), ["running", "running", "completed"]);
  });

  it("a typed phasePublish failure keeps its typed failure and the lane runs in place, once", async t => {
    const on = await drive(t, { emptyDiff: true });
    assert.equal(on.completed.length, 0);
    assert.equal(on.failed.length, 1);
    assert.match(on.failed[0]!.failure_reason!, /^signal_done was called but no changes were committed/);
    assert.equal(on.failed[0]!.completion_final_head, undefined);
    assert.deepEqual(on.order, ["capture", "send"]);
  });
});

describe("terminal preservation adapter predicates (gate on, real RunRunner method)", () => {
  type Adapter = { preserveTerminalInLane(claim: unknown, flight: unknown, body: unknown, site: unknown): Promise<{ outcome: string; reason?: string }> };
  function adapter(): { r: Adapter; sends: () => number; reads: () => number; flight: (over?: Record<string, unknown>) => Record<string, unknown>; claim: unknown } {
    let sends = 0, reads = 0;
    const r = runner({ run: async () => ({}) } as never, fakeGitlab().gitlab, "tp-adapter-key", {
      terminalPreservationTestOverride: {
        workerId: "w1", attemptId: "at1", store: memStore(), owner: fn => fn(),
        deps: {
          universe: async () => { reads++; return { kind: "proved", sources: [], roots: [], universeDigest: "u" }; },
          capture: async onPhase => {
            await onPhase("capturing", { cloneRef: "x", bareRef: "x" });
            await onPhase("captured", { cloneRef: "x", bareRef: "x" });
            return { kind: "ok", base: A, extraRoots: [], manifest: MANIFEST };
          },
          aggregate: async () => ({ tip: A }),
          prepare: async () => ({ pack: Buffer.alloc(0), range: { tipSha: A, excludeSha: C }, digests: { scan: "s", transport: "t" } }),
          coverage: () => ({ ok: true }), recheck: async () => undefined,
          send: async () => { sends++; return FINAL; },
        },
      },
    }) as unknown as Adapter;
    const flight = (over: Record<string, unknown> = {}): Record<string, unknown> => ({
      runId: RUN, claimGeneration: 1, runKind: "issue", barePath: "/bare", branch: "agent/issue-1", worktreePath: "/clone",
      attemptId: "at1", iterationParkState: "none", policyHold: undefined, completionSendAttempted: false,
      completionHoldOutcome: "none", runLog: { info: () => undefined }, ...over,
    });
    return { r, sends: () => sends, reads: () => reads, flight, claim: { repo: { default_branch: "main" } } };
  }
  const site = { quiesce: false, site: "t" };

  it("sends for a clean branch-source flight (control)", async () => {
    const a = adapter();
    assert.equal((await a.r.preserveTerminalInLane(a.claim, a.flight(), { status: "completed" }, site)).outcome, "published");
    assert.equal(a.sends(), 1);
  });

  for (const [name, over, body, reason] of [
    ["a policy hold", { policyHold: {} }, { status: "completed" }, "policy_or_park"],
    ["an iteration park", { iterationParkState: "entered" }, { status: "completed" }, "policy_or_park"],
    ["a failed report over an unresolved completion hold", { completionHoldOutcome: "unresolved" }, { status: "failed" }, "completion_hold"],
    ["a previously attempted completion", { completionSendAttempted: true }, { status: "completed" }, "previously_attempted"],
    ["a steering claim fence", { steering: { claimFence: () => ({}) } }, { status: "completed" }, "claim_fence"],
    ["a permanent-failure reap", { permanentFailureReap: true }, { status: "failed" }, "permanent_failure_reap"],
    ["a secret latch on the flight", { secretBlocked: true }, { status: "failed" }, "secret_blocked"],
    ["a worker_residue_blocked failure_reason mapping", {}, { status: "failed", failure_reason: "worker_residue_blocked: survivor" }, "worker_residue_blocked"],
    ["a push_secret_blocked fail_origin", {}, { status: "failed", fail_origin: "push_secret_blocked" }, "push_secret_blocked"],
  ] as const) {
    it(`skips for ${name}`, async () => {
      const a = adapter();
      const out = await a.r.preserveTerminalInLane(a.claim, a.flight(over), body, site);
      assert.equal(out.outcome, "kept");
      assert.equal(out.reason, reason);
      assert.equal(a.sends(), 0);
    });
  }

  it("refuses on active secret remediation before any universe read", async () => {
    const a = adapter();
    const out = await a.r.preserveTerminalInLane(a.claim, a.flight({ remediationRestrictions: {} }), { status: "completed" }, site);
    assert.equal(out.outcome, "kept");
    assert.equal(out.reason, "secret_remediation_pending");
    assert.equal(a.reads(), 0);
    assert.equal(a.sends(), 0);
  });

  it("a permit deadline 4 s away starts nothing; one 30 s away runs", async () => {
    const short = adapter();
    const out = await short.r.preserveTerminalInLane(short.claim, short.flight(), { status: "completed" }, { ...site, deadlineAt: Date.now() + 4_000 });
    assert.equal(out.reason, "deadline");
    assert.equal(short.reads(), 0);
    const long = adapter();
    const ok = await long.r.preserveTerminalInLane(long.claim, long.flight(), { status: "completed" }, { ...site, deadlineAt: Date.now() + 30_000 });
    assert.equal(ok.outcome, "published");
  });

  for (const [name, over, reason] of [
    ["a ci_fix run", { runKind: "ci_fix" }, "not_branch_source"],
    ["a chat run", { runKind: "chat" }, "not_branch_source"],
    ["a run with no adopted source", { worktreePath: undefined }, "source_unknown"],
  ] as const) {
    it(`sends nothing for ${name}`, async () => {
      const a = adapter();
      const out = await a.r.preserveTerminalInLane(a.claim, a.flight(over), { status: "completed" }, site);
      assert.equal(out.reason, reason);
      assert.equal(a.sends(), 0);
    });
  }
});

describe("terminal preservation adapter: default-branch git reads run under the lane's window (real RunRunner method)", () => {
  type Lane = { preserveTerminalInLane(claim: unknown, flight: unknown, body: unknown, site: unknown): Promise<{ outcome: string; reason?: string }> };
  type GitSpy = {
    withReportProofBudget: (b: { signal: AbortSignal; deadline: number }, action: () => Promise<unknown>) => Promise<unknown>;
    defaultBranchTip: (bare: string, branch: string) => Promise<string | undefined>;
    terminalDefaultBranch: (bare: string) => Promise<{ kind: "absent" }>;
  };
  const CAP_MS = 150;

  /** The real RunRunner with the lane's own prepare (the hooked one dropped, so the tip read is the lane's). The runner's git
   *  is spied: `withReportProofBudget` calls through while recording the signal it was bound to, and a stall waits on that
   *  signal, so a read made outside a bound window never stalls and shows as not inside. */
  function rigWithSpiedGit(t: TestContext) {
    const r = runner({ run: async () => ({}) } as never, fakeGitlab().gitlab, "tp-adapter-window-key", {
      terminalPreservationTestOverride: {
        workerId: "w1", attemptId: "at1", capMs: CAP_MS, store: memStore(), owner: fn => fn(),
        deps: {
          universe: async () => ({ kind: "proved", sources: [], roots: [], universeDigest: "u" }),
          capture: async onPhase => {
            await onPhase("capturing", { cloneRef: "x", bareRef: "x" });
            await onPhase("captured", { cloneRef: "x", bareRef: "x" });
            return { kind: "ok", base: A, extraRoots: [], manifest: MANIFEST };
          },
          aggregate: async () => ({ tip: A }),
          coverage: () => ({ ok: true }), recheck: async () => undefined,
          send: async () => FINAL,
        },
      },
    }) as unknown as Lane & { git: GitSpy };
    const g = r.git;
    const bound: AbortSignal[] = [];
    let depth = 0;
    const real = g.withReportProofBudget.bind(g);
    t.mock.method(g, "withReportProofBudget", async (b: { signal: AbortSignal; deadline: number }, action: () => Promise<unknown>) => {
      bound.push(b.signal);
      depth++;
      try { return await real(b, action); } finally { depth--; }
    });
    const reads = { insideBudget: [] as boolean[] };
    const stall = async (): Promise<undefined> => {
      reads.insideBudget.push(depth > 0);
      if (depth === 0) return undefined;
      const sig = bound[bound.length - 1]!;
      await new Promise<void>(res => { if (sig.aborted) res(); else sig.addEventListener("abort", () => res(), { once: true }); });
      return undefined;
    };
    t.mock.method(g, "defaultBranchTip", stall);
    t.mock.method(g, "terminalDefaultBranch", async (): Promise<{ kind: "absent" }> => { await stall(); return { kind: "absent" }; });
    const flight = {
      runId: RUN, claimGeneration: 1, runKind: "issue", barePath: "/bare", branch: "agent/issue-1", worktreePath: "/clone",
      attemptId: "at1", iterationParkState: "none", policyHold: undefined, completionSendAttempted: false,
      completionHoldOutcome: "none", runLog: { info: () => undefined },
    };
    return { r, flight, reads };
  }
  const laneSite = { quiesce: false, site: "t" };

  it("a default-branch tip read runs inside the report-proof window and a stall past it refuses as a prepare deadline", async t => {
    const { r, flight, reads } = rigWithSpiedGit(t);
    const started = Date.now();
    const out = await r.preserveTerminalInLane({ repo: { default_branch: "main" } }, flight, { status: "completed" }, laneSite);
    const elapsed = Date.now() - started;
    assert.deepEqual(reads.insideBudget, [true], "defaultBranchTip was called once, inside withReportProofBudget");
    assert.equal(out.outcome, "kept");
    assert.match(String(out.reason), /deadline/);
    assert.ok(elapsed < CAP_MS + 1500, `elapsed ${elapsed} ms`);
  });

  it("a default-branch lookup runs inside the report-proof window and a stall past it keeps as default_branch_unresolved", async t => {
    const { r, flight, reads } = rigWithSpiedGit(t);
    const started = Date.now();
    const out = await r.preserveTerminalInLane({ repo: {} }, flight, { status: "completed" }, laneSite);
    const elapsed = Date.now() - started;
    assert.deepEqual(reads.insideBudget, [true], "terminalDefaultBranch was called once, inside withReportProofBudget");
    assert.equal(out.outcome, "kept");
    assert.equal(out.reason, "default_branch_unresolved");
    assert.ok(elapsed < CAP_MS + 1500, `elapsed ${elapsed} ms`);
  });
});

describe("terminal preservation adapter: default branch read from real bare repositories (real RunRunner method)", () => {
  type Lane = { preserveTerminalInLane(claim: unknown, flight: unknown, body: unknown, site: unknown): Promise<{ outcome: string; reason?: string }>; git: { defaultBranchTip: (bare: string, branch: string) => Promise<string | undefined> } };
  const gitIdentity = { GIT_AUTHOR_NAME: "t", GIT_AUTHOR_EMAIL: "t@example.com", GIT_COMMITTER_NAME: "t", GIT_COMMITTER_EMAIL: "t@example.com" };
  const laneSite = { quiesce: false, site: "t" };

  function git(cwd: string, args: string[], input?: string): string {
    return execFileSync("git", ["-C", cwd, ...args], { env: { ...process.env, ...gitIdentity }, encoding: "utf8", input, stdio: ["pipe", "pipe", "pipe"] }).trim();
  }

  /** A real bare repository with one commit object (unreferenced); `ref(name, sha)` writes a loose ref file directly. */
  function bareRepo(t: TestContext) {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "lane-bare-"));
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    const bare = path.join(dir, "bare.git");
    execFileSync("git", ["init", "-q", "--bare", bare], { env: { ...process.env, ...gitIdentity } });
    git(bare, ["symbolic-ref", "HEAD", "refs/heads/main"]);
    const tree = git(bare, ["mktree"], "");
    const commit = git(bare, ["commit-tree", tree, "-m", "m"]);
    const file = (rel: string, content: string) => {
      fs.mkdirSync(path.dirname(path.join(bare, rel)), { recursive: true });
      fs.writeFileSync(path.join(bare, rel), content);
    };
    return { dir, bare, commit, file, ref: (name: string, sha: string) => file(name, `${sha}\n`) };
  }

  /** The real RunRunner; capture and send are counted. No git method is mocked unless `branches` is passed, which records the
   *  branch handed to the tip read (the lane's own prepare then runs). */
  function realRig(t: TestContext, barePath: string, observeTip = false) {
    const calls = { capture: 0, send: 0, universe: 0 };
    const branches: string[] = [];
    const r = runner({ run: async () => ({}) } as never, fakeGitlab().gitlab, "tp-real-bare-key", {
      terminalPreservationTestOverride: {
        workerId: "w1", attemptId: "at1", capMs: 5_000, store: memStore(), owner: fn => fn(),
        deps: {
          universe: async () => { calls.universe++; return { kind: "proved", sources: [], roots: [], universeDigest: "u" }; },
          capture: async onPhase => {
            calls.capture++;
            await onPhase("capturing", { cloneRef: "x", bareRef: "x" });
            await onPhase("captured", { cloneRef: "x", bareRef: "x" });
            return { kind: "ok", base: A, extraRoots: [], manifest: MANIFEST };
          },
          aggregate: async () => ({ tip: A }),
          coverage: () => ({ ok: true }), recheck: async () => undefined,
          ...(observeTip ? {} : { prepare: async () => ({ pack: Buffer.from("pack"), range: { tipSha: A, excludeSha: C }, digests: { scan: "s", transport: "t" } }) }),
          send: async () => { calls.send++; return FINAL; },
        },
      },
    }) as unknown as Lane;
    if (observeTip) t.mock.method(r.git, "defaultBranchTip", async (_bare: string, branch: string) => { branches.push(branch); return undefined; });
    const flight = {
      runId: RUN, claimGeneration: 1, runKind: "issue", barePath, branch: "agent/issue-1", worktreePath: "/clone",
      attemptId: "at1", iterationParkState: "none", policyHold: undefined, completionSendAttempted: false,
      completionHoldOutcome: "none", runLog: { info: () => undefined },
    };
    return { r, flight, calls, branches };
  }

  async function keeps(t: TestContext, barePath: string) {
    const { r, flight, calls } = realRig(t, barePath);
    const out = await r.preserveTerminalInLane({ repo: {} }, flight, { status: "completed" }, laneSite);
    assert.equal(out.outcome, "kept");
    assert.equal(out.reason, "default_branch_unresolved");
    assert.deepEqual(calls, { capture: 0, send: 0, universe: 0 });
  }

  it("R1: origin/HEAD points at a ref whose object is missing, origin/main valid: kept before capture and send", async t => {
    const b = bareRepo(t);
    b.ref("refs/remotes/origin/main", b.commit);
    b.ref("refs/remotes/origin/develop", "d".repeat(40));
    git(b.bare, ["symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop"]);
    await keeps(t, b.bare);
  });

  it("R2: a garbage loose ref for origin/develop that origin/HEAD names, origin/main valid: kept", async t => {
    const b = bareRepo(t);
    b.ref("refs/remotes/origin/main", b.commit);
    b.file("refs/remotes/origin/develop", "this is not a sha\n");
    git(b.bare, ["symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop"]);
    await keeps(t, b.bare);
  });

  it("R3: a bare path that is a plain non-git directory: kept", async t => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "lane-notgit-"));
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    await keeps(t, dir);
  });

  it("R4: a detached HEAD and no main or master: kept", async t => {
    const b = bareRepo(t);
    fs.writeFileSync(path.join(b.bare, "HEAD"), `${b.commit}\n`);
    await keeps(t, b.bare);
  });

  it("R5: an unborn HEAD and a refs/heads/main that is a dangling symbolic ref: kept", async t => {
    const b = bareRepo(t);
    b.file("refs/heads/main", "ref: refs/heads/gone\n");
    await keeps(t, b.bare);
  });

  it("R6: origin/HEAD symref to a nonexistent origin/develop, origin/main valid: kept", async t => {
    const b = bareRepo(t);
    b.ref("refs/remotes/origin/main", b.commit);
    git(b.bare, ["symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop"]);
    await keeps(t, b.bare);
  });

  it("R7: a dangling symlink at refs/remotes/origin/HEAD, origin/main valid: kept", async t => {
    const b = bareRepo(t);
    b.ref("refs/remotes/origin/main", b.commit);
    fs.symlinkSync("/nonexistent-target", path.join(b.bare, "refs/remotes/origin/HEAD"));
    await keeps(t, b.bare);
  });

  it("R8: a dangling symlink at the ancestor refs/remotes/origin, unborn HEAD, origin/main absent: kept", async t => {
    const b = bareRepo(t);
    fs.mkdirSync(path.join(b.bare, "refs/remotes"), { recursive: true });
    fs.symlinkSync("/nonexistent-target", path.join(b.bare, "refs/remotes/origin"));
    await keeps(t, b.bare);
  });

  async function tipBranch(t: TestContext, barePath: string) {
    const { r, flight, branches } = realRig(t, barePath, true);
    const out = await r.preserveTerminalInLane({ repo: {} }, flight, { status: "completed" }, laneSite);
    return { out, branches };
  }

  it("C1: origin/HEAD names a valid origin/develop: the tip read receives develop", async t => {
    const b = bareRepo(t);
    b.ref("refs/remotes/origin/develop", b.commit);
    b.ref("refs/remotes/origin/main", b.commit);
    git(b.bare, ["symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop"]);
    const o = await tipBranch(t, b.bare);
    assert.notEqual(o.out.reason, "default_branch_unresolved");
    assert.deepEqual(o.branches, ["develop"]);
  });

  it("C2: an empty bare repository proves no default branch and proceeds with main", async t => {
    const b = bareRepo(t);
    const o = await tipBranch(t, b.bare);
    assert.notEqual(o.out.reason, "default_branch_unresolved");
    assert.deepEqual(o.branches, ["main"]);
  });

  it("C2b: an empty bare repository reaches the send with the lane's hooked prepare", async t => {
    const b = bareRepo(t);
    const { r, flight, calls } = realRig(t, b.bare);
    const out = await r.preserveTerminalInLane({ repo: {} }, flight, { status: "completed" }, laneSite);
    assert.notEqual(out.reason, "default_branch_unresolved");
    assert.equal(calls.send, 1);
  });

  it("C5: no origin/HEAD and a valid origin/main resolves main and proceeds", async t => {
    const b = bareRepo(t);
    b.ref("refs/remotes/origin/main", b.commit);
    const o = await tipBranch(t, b.bare);
    assert.notEqual(o.out.reason, "default_branch_unresolved");
    assert.deepEqual(o.branches, ["main"]);
  });
});

describe("terminal-preservation lane: snapshot cleanup bound and remnant (#4007)", () => {
  /** A rig whose universe is the lane's own (a real clone dir and a fake git), so the lane builds the capture call itself. */
  function snapRig(over: Partial<TerminalLaneEnv>, snap: unknown): { r: Rig; calls: Array<Record<string, unknown>>; logs: Array<[string, Record<string, unknown>]>; cleanup(): void } {
    const clone = fs.mkdtempSync(path.join(os.tmpdir(), "lane-clone-"));
    const st = fs.lstatSync(clone);
    const calls: Array<Record<string, unknown>> = [];
    const logs: Array<[string, Record<string, unknown>]> = [];
    const r = rig({ env: over });
    r.flight.worktreePath = clone;
    r.env.hooks = { workerId: "w1", store: r.store, owner: fn => fn(), deps: { recheck: async () => undefined } };
    r.env.git = {
      listRunBares: async () => ["/bare"],
      readInventoryCloneHeads: async () => ({
        kind: "verified", foreignOwners: [],
        clones: [{ clonePath: clone, head: A, identity: `${st.dev}:${st.ino}`, branch: "agent/issue-1" }],
      }),
      readTerminalAttribution: async () => ({
        kind: "verified", journal: [], stoppedMarkers: [], protected: [], owedRoots: [],
        attempts: [{ attemptId: "at1", clonePath: clone, branch: "agent/issue-1", state: "live" }],
      }),
      readTerminalCloneLocalRefs: async () => ({ ok: true }),
      ancestry: async () => "ancestor",
      captureTerminalSnapshot: async (_clone: string, opts: Record<string, unknown>) => { calls.push(opts); return snap; },
    } as never;
    r.env.recovery = { readTerminalRecords: async () => ({ kind: "records", records: [] }) } as never;
    r.env.log = (step, fields) => { logs.push([step, fields]); };
    return { r, calls, logs, cleanup: () => fs.rmSync(clone, { recursive: true, force: true }) };
  }
  const refused = { kind: "refused", reason: "x" };

  it("passes cleanupBound from the permit deadline and the permit's own signal, not the window signal", async () => {
    const permit = new AbortController();
    const t0 = Date.now(); // the fake clock stays at or after the real one, so the universe's own wall-clock check never lapses
    const s = snapRig({ now: () => t0, remainingMs: 60_000, signal: permit.signal }, refused);
    try {
      await preserveInLane(s.r.env, s.r.flight, body);
      assert.equal(s.calls.length, 1);
      const bound = s.calls[0]!.cleanupBound as { deadlineAt: number; signal?: AbortSignal };
      assert.equal(bound.deadlineAt, t0 + 60_000);
      assert.equal(bound.signal, permit.signal);
      assert.notEqual(bound.signal, s.calls[0]!.signal, "the lane's own window signal is a different signal");
    } finally { s.cleanup(); }
  });

  it("a signal-only permit (no remainder) passes the signal and no deadlineAt", async () => {
    const permit = new AbortController();
    const s = snapRig({ signal: permit.signal }, refused);
    try {
      await preserveInLane(s.r.env, s.r.flight, body);
      assert.equal(s.calls.length, 1);
      const bound = s.calls[0]!.cleanupBound as { deadlineAt?: number; signal?: AbortSignal };
      assert.equal(bound.signal, permit.signal);
      assert.equal("deadlineAt" in bound, false);
    } finally { s.cleanup(); }
  });

  it("logs terminal_snapshot_remnant with the snapshot's remnant", async () => {
    const remnant = { ref: "refs/uzi-terminal-local/x", expectedOid: A, state: "retained" };
    const s = snapRig({}, { kind: "refused", reason: "x", remnant });
    try {
      await preserveInLane(s.r.env, s.r.flight, body);
      assert.deepEqual(s.logs.filter(l => l[0] === "terminal_snapshot_remnant"), [["terminal_snapshot_remnant", { remnant }]]);
    } finally { s.cleanup(); }
  });

  it("passes no cleanupBound without a permit remainder or signal, and logs no remnant", async () => {
    const s = snapRig({}, refused);
    try {
      await preserveInLane(s.r.env, s.r.flight, body);
      assert.equal(s.calls.length, 1);
      assert.equal("cleanupBound" in s.calls[0]!, false);
      assert.equal(s.logs.some(l => l[0] === "terminal_snapshot_remnant"), false);
    } finally { s.cleanup(); }
  });
});
