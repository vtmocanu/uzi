import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  mergeTerminalRecord, type TerminalDelta, type TerminalOperationRecord, type TerminalOperationStore,
} from "../src/terminal-operation.js";
import type { TerminalPreservationResult } from "../src/client.js";
import { preserveInLane, type TerminalLaneEnv, type TerminalLaneFlight } from "../src/terminal-preservation-lane.js";
import type { TerminalPreservationRequest } from "../src/protocol.js";

// #4007 slice B repair: lane-level cases for the snapshot manifest root.

const RUN = "11111111-1111-4111-8111-111111111111";
const A = "a".repeat(40), B = "b".repeat(40), C = "c".repeat(40);
const SNAP_TREE = "d".repeat(40);

function memStore(): TerminalOperationStore & { record?: TerminalOperationRecord } {
  const s: TerminalOperationStore & { record?: TerminalOperationRecord } = {
    async read() { return s.record ? { kind: "ok", record: s.record } : { kind: "absent" }; },
    async create(record) { if (s.record) return { kind: "exists" }; s.record = record; return { kind: "created", record }; },
    async update(_r, _g, delta: TerminalDelta) {
      if (!s.record) return { kind: "absent" };
      const m = mergeTerminalRecord(s.record, delta);
      if (!m.ok) return { kind: "rejected", reason: m.reason };
      s.record = m.record;
      return { kind: "ok", record: m.record };
    },
    async addLocalBlock(r, g, reasons) { return s.update(r, g, { addLocalBlock: reasons }); },
    async runBlock() {
      const r = s.record;
      if (r && (r.localBlock.length > 0 || r.flaggedCommits.length > 0 || r.closedReason === "secret_found")) return { kind: "blocked", reason: r.localBlock[0] ?? "secret_found" };
      return { kind: "clear" };
    },
    async invoke(_r, _g, at) {
      if (!s.record) return { kind: "absent" };
      const m = mergeTerminalRecord(s.record, { appendInvocations: [{ at, outcome: "invoked" }] });
      if (!m.ok) return { kind: "rejected", reason: m.reason };
      s.record = m.record;
      return { kind: "invoked", record: m.record };
    },
  };
  return s;
}

const CLONE = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-tp-repair-"));
const IDENTITY = (() => { const st = fs.lstatSync(CLONE); return `${st.dev}:${st.ino}`; })();
process.on("exit", () => fs.rmSync(CLONE, { recursive: true, force: true }));

function rig(snap: unknown) {
  const store = memStore();
  const sent: TerminalPreservationRequest[] = [];
  const flight: TerminalLaneFlight = { runId: RUN, claimGeneration: 1, runKind: "issue", barePath: "/bare", branch: "agent/issue-1", worktreePath: CLONE, attemptId: "at1" };
  const env = {
    git: {
      captureTerminalSnapshot: async (_clone: string, opts: { onPhase: (p: string, i: unknown) => Promise<void> }) => {
        if ((snap as { kind: string }).kind === "captured") {
          await opts.onPhase("capturing", { cloneRef: "refs/x", bareRef: "refs/x" });
          await opts.onPhase("captured", { cloneRef: "refs/x", bareRef: "refs/x", commit: C });
        }
        return snap;
      },
      listRunBares: async () => ["/bare"],
      readInventoryCloneHeads: async () => ({
        kind: "verified", heads: [A], foreignOwners: [], clones: [{ clonePath: CLONE, branch: "agent/issue-1", runId: RUN, head: A, identity: IDENTITY }],
      }),
      readTerminalAttribution: async () => ({
        kind: "verified", attempts: [{ attemptId: "at1", clonePath: CLONE, branch: "agent/issue-1", state: "live" }],
        journal: [], stoppedMarkers: [], protected: [], owedRoots: [],
      }),
      readTerminalCloneLocalRefs: async () => ({ ok: true }),
      ancestry: async () => "ancestor",
      revParse: async (_bare: string, spec: string) => (spec === `${A}^{tree}` ? B : undefined),
    } as never,
    recovery: { readTerminalRecords: async () => ({ kind: "records", records: [] }) } as never, client: { publishTerminalPreservation: async () => ({ kind: "final", outcome: "published", response: {} }) as unknown as TerminalPreservationResult },
    hooks: {
      workerId: "w1", store, owner: (fn: () => Promise<unknown>) => fn(),
      deps: {
        aggregate: async ({ base }: { base: string }) => ({ tip: base }),
        prepare: async () => ({ pack: Buffer.from("p"), range: { tipSha: C, excludeSha: A }, digests: { scan: "s", transport: "t" } }),
        recheck: async () => undefined,
        send: async (req: TerminalPreservationRequest) => { sent.push(req); return { kind: "final", outcome: "published", response: {} } as unknown as TerminalPreservationResult; },
      },
    },
    workerId: "w1", defaultBranch: "main", egressBound: false,
    skip: () => undefined, failureFirst: () => undefined,
    policy: () => ({ version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: null, extraFloors: [] }),
    remediationHold: async () => false, ciHold: async () => "clear" as const,
    log: () => undefined, now: () => Date.now(),
  } as unknown as TerminalLaneEnv;
  return { store, sent, flight, env };
}

describe("terminal lane snapshot root (#4007 slice B repair)", () => {
  it("declares a captured snapshot as the one extra manifest root and publishes", async () => {
    const r = rig({ kind: "captured", root: C, parent: A, tree: SNAP_TREE, ignored: { files: 0, bytes: 0 } });
    const out = await preserveInLane(r.env, r.flight, { status: "completed" });
    assert.equal(out.outcome, "published", JSON.stringify(out));
    const sid = r.sent[0]!.manifest.roots[0]!.source_id;
    assert.deepEqual(r.sent[0]!.manifest.roots, [
      { source_id: sid, attempt_id: "at1", commit: A, tree: B },
      { source_id: `${sid}:snapshot`, attempt_id: "at1", commit: C, tree: SNAP_TREE },
    ]);
    // The resume path re-checks it from the persisted manifest.
    assert.equal(r.store.record!.manifest!.roots.length, 2);
  });

  it("declares no snapshot root for a clean tree", async () => {
    const r = rig({ kind: "clean", root: A, tree: B, ignored: { files: 0, bytes: 0 } });
    const out = await preserveInLane(r.env, r.flight, { status: "completed" });
    assert.equal(out.outcome, "published", JSON.stringify(out));
    assert.deepEqual(r.sent[0]!.manifest.roots.length, 1);
  });
});
