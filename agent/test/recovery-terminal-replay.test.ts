import { NO_SCAN_NEEDLES } from "./run-env-test-support.js";
/**
 * #4007 slice B M6-V: the guarded replay of a frozen OPEN held record runs the terminal-preservation operation before
 * any held FINAL can be minted, and only inside the inventory source boundary. Production-dark: every case that turns
 * the gate on does so through the constructor-only `terminalPreservationTestOverride`.
 */
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Readable } from "node:stream";
import { makeScratchRoot } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import { RecoveryCoordinator, type RecoveryRecord } from "../src/recovery.js";
import type { PositiveOwedCandidateContext, OwedCandidate } from "../src/git.js";
import type { TerminalPreservationRequest } from "../src/protocol.js";
import type { TerminalPreservationResult } from "../src/client.js";

const H = "a".repeat(40);
const H2 = "b".repeat(40);
const RUN = "00000000-0000-4000-8000-000000000001";
const HOLD = "00000000-0000-4000-8000-000000000007";
const REPO = "00000000-0000-4000-8000-000000000008";
const PUBLICATION = "00000000-0000-4000-8000-000000000009";
const ATTEMPT = "20260101T000000Z-g7-0123456789abcdef";

const FINAL: TerminalPreservationResult = { kind: "final", outcome: "published", response: {} as never };

async function rig(opts: { gate: boolean; boundary?: "pass" | "refuse"; heldBinding?: boolean; kind?: "issue" | "ci_fix" | "prompt" | "task" } = { gate: true }) {
  const root = makeScratchRoot("terminal-replay-", os.tmpdir());
  const barePath = path.join(root, "repo.git");
  const clonePath = path.join(root, "clones", `issue-1.attempt-${ATTEMPT}`);
  await fs.mkdir(clonePath, { recursive: true });
  const st = await fs.lstat(clonePath);
  const identity = `${st.dev}:${st.ino}`;
  const context: PositiveOwedCandidateContext = {
    runId: RUN, generation: 7, kind: opts.kind ?? "issue", branch: "task", barePath,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: "d".repeat(40) },
  };
  const s = {
    hold: "open" as "open" | "discarded" | "unknown", status: "failed", ownGeneration: 8,
    extraClone: false, owed: [] as string[], ancestors: new Set<string>([H]),
    restrictionsBlockedFromHoldRead: Infinity, holdReads: 0, dirty: [] as string[] | null,
    terminal: [] as TerminalPreservationRequest[], terminalResult: FINAL as TerminalPreservationResult,
    order: [] as string[], publishHeld: 0, releases: 0, boundaryRefused: opts.boundary === "refuse",
    boundaryProofCalls: 0, findings: [] as Array<{ commit: string }>, scanCalls: 0, findingsFromCall: 1,
  };
  const candidates = [{ sha: H, pinRef: "refs/owed/a", contexts: [structuredClone(context)] }] as OwedCandidate[];
  const exactHold = () => s.hold === "unknown" ? undefined : ({
    run_id: RUN, hold_id: HOLD, repo_id: REPO, generation: 7, state: s.hold,
    original_worker_id: "original-worker", original_worker_identity: "original-worker", inventory_guarded: true,
    run_status: s.status, current_claim_generation: s.ownGeneration, claim_released: true,
    ...(s.hold === "open" ? { live_worker_id: "original-worker", live_run_id: RUN } : { release_evidence: "owner_discard" }),
  });
  const client = {
    hasFeature: () => true,
    hasHeldPublicationV2: () => true,
    getRunOwnership: async () => ({ status: s.status, claim_generation: s.ownGeneration, inventory_guarded: true }),
    listRecoveryHolds: async () => ({ run_id: RUN, holds: s.hold === "open" ? [{
      hold_id: HOLD, generation: 7, inventory_guarded: true, has_available_capture: false }] : [] }),
    getExactRecoveryHold: async () => { s.holdReads++; s.order.push("hold"); return exactHold(); },
    publishHeld: async (run: string, gen: number, tip: string) => {
      s.order.push("held"); s.publishHeld++;
      return { ok: true, body: { publication_id: PUBLICATION, ref: `refs/uzi-held/${run}/${gen}`, tip, state: "created" } };
    },
    releaseRecoveryCustody: async (...args: unknown[]) => { s.releases++; return { run_id: args[0], generation: args[1], released: true, holds_released: 1 }; },
    reserveRecoveryCapture: async () => { throw new Error("archive reserve is out of scope"); },
    publishTerminalPreservation: async (_run: string, req: TerminalPreservationRequest) => {
      s.order.push("terminal"); s.terminal.push(structuredClone(req)); return s.terminalResult;
    },
  };
  const git = {
    recoverySettlementRoot: path.join(root, "settlement"),
    deleteRecoveryPin: async () => undefined,
    enumerateOwedCandidates: async () => structuredClone(candidates),
    discoverOwedCandidates: async () => [{ context, candidates: structuredClone(candidates) }],
    resolveRecoveryBareDir: async () => barePath,
    buildRecoveryCoverage: async () => ({ sha: "c".repeat(40), fingerprint: "f".repeat(64) }),
    ancestry: async (_bare: string, head: string) => s.ancestors.has(head) ? "ancestor" : "unknown",
    readRemediationRestrictionFacts: async () => ({
      enrolled: false, blocked: s.holdReads >= s.restrictionsBlockedFromHoldRead, roots: [], flaggedOIDs: [], fingerprint: "unenrolled" }),
    resolveCheckpointRange: async (_b: string, _br: string, o: { tipSha: string }) => { s.order.push("range"); return { tipSha: o.tipSha, excludeSha: "e".repeat(40) }; },
    secretScanCheckpointRange: async () => ({ trusted: true, findings: ++s.scanCalls >= s.findingsFromCall ? s.findings : [] }),
    rangeTouchesWorkflows: async () => "clean",
    changedFilesNoRenames: async () => [],
    changedCiPathsPerCommit: async () => [],
    terminalScanDigest: async () => "scan-digest",
    terminalTransportDigest: async () => "transport-digest",
    checkpointPack: async (_b: string, _br: string, _o: unknown, _r: unknown, _s: unknown, tip: string) =>
      ({ tipOid: tip, pack: Readable.from([Buffer.from("PACK")]), exited: Promise.resolve(0) }),
    listRunBares: async () => [barePath],
    readInventoryCloneHeads: async () => ({
      kind: "verified", heads: [H], foreignOwners: [],
      clones: [{ clonePath, branch: "task", runId: RUN, head: H, identity },
        ...(s.extraClone ? [{ clonePath: clonePath + "-other", branch: "task", runId: RUN, head: H2, identity: "1:1" }] : [])],
    }),
    readTerminalAttribution: async () => ({
      kind: "verified", attempts: [{ attemptId: ATTEMPT, clonePath, branch: "task", state: "live" }],
      journal: [], stoppedMarkers: [], protected: [], owedRoots: s.owed,
    }),
    readTerminalCloneLocalRefs: async () => ({ ok: true }),
    commitTreeOid: async (_b: string, sha: string) => sha[0]!.repeat(40),
    worktreeStatus: async () => s.dirty,
  };
  const make = (gate = opts.gate) => new RecoveryCoordinator({ ...NO_SCAN_NEEDLES,
    recoveryRoot: path.join(root, "journal"), workerToken: "local-worker-fixture", log: nullLogger(),
    client: client as never, git: git as never, now: () => 1000,
    ...(gate ? { terminalPreservationTestOverride: {} } : {}),
    withInventorySourceBoundary: async (_context, action) => {
      if (s.boundaryRefused) return "retained";
      await action(async () => { s.boundaryProofCalls++; return true; });
      return "passed";
    },
  });
  const coordinator = make();
  const record = await coordinator.freezeInventory({
    context, currentSha: H, defaultBranch: "main", ...(opts.heldBinding === false ? {} : { repoId: REPO }),
    heldPolicy: { version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: [".custom-ci/**"], extraFloors: [] },
  }) as RecoveryRecord;
  assert.ok(record);
  const operate = (c = coordinator, r: RecoveryRecord = record) => c.operateFrozenInventory({ record: r, barePath, defaultBranch: "main" });
  return { root, s, coordinator, make, record, operate, clonePath, close: () => fs.rm(root, { recursive: true, force: true }) };
}

describe("M6-V guarded replay of a frozen open held record", () => {
  it("published terminal preservation lets the held FINAL proceed, after it", async () => {
    const f = await rig();
    try {
      await f.operate();
      assert.equal(f.s.terminal.length, 1);
      assert.equal(f.s.publishHeld, 1);
      assert.ok(f.s.order.indexOf("terminal") < f.s.order.indexOf("held"), "terminal send precedes the held create");
      const req = f.s.terminal[0]!;
      assert.equal(req.generation, 7);
      assert.equal(req.tip, f.record.sourceSha, "the frozen source is the tip; no new capture");
      assert.equal(req.disposition, "preserve");
      assert.equal(req.prior_tip, undefined);
      assert.equal(req.manifest.worker_id, "original-worker");
      assert.deepEqual(req.manifest.roots.map(r => [r.commit, r.attempt_id]), [[H, ATTEMPT]]);
      assert.deepEqual(req.manifest.abandoned, []);
      assert.ok(req.pack);
    } finally { await f.close(); }
  });

  it("unmeasured dirty bytes of the frozen clone are recorded as cold_dirty, never captured", async () => {
    const f = await rig();
    try {
      await fs.writeFile(path.join(f.clonePath, "scratch.txt"), "12345");
      f.s.dirty = ["?? scratch.txt"];
      await f.operate();
      const req = f.s.terminal[0]!;
      assert.deepEqual(req.manifest.abandoned.map(a => [a.class, a.extents]), [["cold_dirty", { files: 1, bytes: 5 }]]);
    } finally { await f.close(); }
  });

  it("an unreadable dirty state refuses before any send", async () => {
    const f = await rig();
    try {
      f.s.dirty = null;
      await f.operate();
      assert.equal(f.s.terminal.length, 0);
      assert.equal(f.s.publishHeld, 0);
    } finally { await f.close(); }
  });

  for (const unlatched of [false, true]) {
    it(`a secret finding (${unlatched ? "latch write fails: secret_found_unlatched" : "latched"}) blocks the held policy and sends nothing`, async () => {
      const f = await rig();
      try {
        f.s.findings = [{ commit: H }];
        f.s.findingsFromCall = 2; // the held preparation scans clean first; the terminal preparation then finds
        if (unlatched) {
          const real = f.coordinator.terminalOperationStore();
          f.coordinator.terminalOperationStore = () => ({ ...real, addLocalBlock: async () => ({ kind: "error", reason: "write_failed" }) });
        }
        await f.operate();
        assert.equal(f.s.scanCalls >= 2, true, "the terminal preparation scanned");
        assert.equal(f.s.terminal.length, 0);
        assert.equal(f.s.publishHeld, 0);
        const latest = await (f.coordinator as unknown as { readLatest(r: RecoveryRecord): Promise<{ kind: string; record: RecoveryRecord }> }).readLatest(f.record);
        assert.equal(latest.kind, "ok");
        assert.equal(latest.record.heldPolicy?.blocked, true, "the frozen policy is blocked once a secret finding is reported");
      } finally { await f.close(); }
    });
  }

  const notFinal: Array<[string, TerminalPreservationResult]> = [
    ["unknown transport", { kind: "unknown", cause: "transport" }],
    ["not final", { kind: "unknown", cause: "not_final", httpStatus: 200 }],
    ["retryable", { kind: "unknown", cause: "retryable", reason: "busy" as never, httpStatus: 429 }],
    ["refused", { kind: "refused", reason: "unsupported" as never, httpStatus: 409 }],
  ];
  for (const [name, result] of notFinal) {
    it(`${name} keeps the record and mints no held FINAL`, async () => {
      const f = await rig();
      try {
        f.s.terminalResult = result;
        await f.operate();
        assert.equal(f.s.terminal.length, 1);
        assert.equal(f.s.publishHeld, 0);
        assert.equal(f.s.releases, 0);
        const after = (await f.coordinator.inspect(RUN))[0]!;
        assert.equal(after.finalRequest, undefined);
        assert.equal(after.finalAcknowledged, undefined);
      } finally { await f.close(); }
    });
  }

  it("a restriction refusal after the hold read sends no terminal request and no held create", async () => {
    const f = await rig();
    try {
      // Reads before the insertion (the pre-closure exact read, publish-time restriction checks and heldOperationState) allow; the insertion denies.
      f.s.restrictionsBlockedFromHoldRead = 5;
      await f.operate();
      assert.equal(f.s.terminal.length, 0);
      assert.equal(f.s.publishHeld, 0);
    } finally { await f.close(); }
  });

  for (const hold of ["discarded", "unknown"] as const) {
    it(`a ${hold} hold never reaches the terminal operation`, async () => {
      const f = await rig();
      try {
        f.s.hold = hold;
        await f.operate();
        assert.equal(f.s.terminal.length, 0);
        assert.equal(f.s.publishHeld, 0);
        await assert.rejects(fs.stat(path.join(f.root, "terminal-preservation")), { code: "ENOENT" });
      } finally { await f.close(); }
    });
  }

  it("a record without an exact hold binding never reaches the terminal operation", async () => {
    const f = await rig({ gate: true, heldBinding: false });
    try {
      await f.operate();
      assert.equal(f.s.terminal.length, 0);
      await assert.rejects(fs.stat(path.join(f.root, "terminal-preservation")), { code: "ENOENT" });
    } finally { await f.close(); }
  });

  it("a non-terminal run (another claim generation still executing) never reaches the terminal operation", async () => {
    const f = await rig();
    try {
      f.s.status = "running";
      await f.operate();
      assert.equal(f.s.terminal.length, 0);
    } finally { await f.close(); }
  });

  it("a second attempt in the universe keeps the work and sends nothing", async () => {
    const f = await rig();
    try {
      f.s.extraClone = true;
      await f.operate();
      assert.equal(f.s.terminal.length, 0);
      assert.equal(f.s.publishHeld, 0);
    } finally { await f.close(); }
  });

  it("a root the frozen tip does not contain keeps the work and sends nothing", async () => {
    const f = await rig();
    try {
      f.s.owed = [H2];
      await f.operate();
      assert.equal(f.s.terminal.length, 0);
      assert.equal(f.s.publishHeld, 0);
    } finally { await f.close(); }
  });

  it("a competing source consumer (boundary refusal) sends nothing", async () => {
    const f = await rig({ gate: true, boundary: "refuse" });
    try {
      await f.operate();
      assert.equal(f.s.terminal.length, 0);
      assert.equal(f.s.publishHeld, 0);
    } finally { await f.close(); }
  });

  it("a prepared record from a crash resumes only here with the identical request; rediscovery never sends", async () => {
    const f = await rig();
    try {
      f.s.terminalResult = { kind: "unknown", cause: "transport" };
      await f.operate();
      assert.equal(f.s.terminal.length, 1);
      const first = f.s.terminal[0]!;

      // A restart: a new coordinator over the same disk. Rediscovery reports the prepared record and sends nothing.
      const restarted = f.make();
      const found = await restarted.resumeTerminalOperations();
      assert.equal(found.resumePending.length, 1);
      assert.equal(f.s.terminal.length, 1, "rediscovery never sends");

      f.s.terminalResult = FINAL;
      const frozen = (await restarted.inspect(RUN))[0]!;
      await f.operate(restarted, frozen);
      assert.equal(f.s.terminal.length, 2);
      assert.deepEqual(f.s.terminal[1], first, "the resumed request is byte-identical");
      assert.equal(f.s.publishHeld, 1);
    } finally { await f.close(); }
  });

  for (const kind of ["ci_fix", "prompt", "task"] as const) {
    it(`a ${kind} held record runs no terminal producer work and keeps the shipped held flow`, async () => {
      const gated = await rig({ gate: true, kind });
      const off = await rig({ gate: false, kind });
      try {
        assert.equal(gated.record.kind, kind);
        await gated.operate();
        await off.operate();
        assert.equal(gated.s.terminal.length, 0);
        assert.equal(gated.s.publishHeld, 1);
        assert.deepEqual(gated.s.order, off.s.order, "the same call sequence as with the gate off");
        await assert.rejects(fs.stat(path.join(gated.root, "terminal-preservation")), { code: "ENOENT" });
      } finally { await gated.close(); await off.close(); }
    });
  }

  it("with the gate off the closure's call sequence is the base one and nothing terminal is created", async () => {
    const off = await rig({ gate: false });
    const on = await rig({ gate: true });
    try {
      await off.operate();
      await on.operate();
      assert.equal(off.s.terminal.length, 0);
      assert.equal(off.s.publishHeld, 1);
      await assert.rejects(fs.stat(path.join(off.root, "terminal-preservation")), { code: "ENOENT" });
      // The gated run differs only by the terminal send and its extra hold read, placed before the held create.
      // (the terminal operation resolves its own range once more).
      const base = (order: string[]) => order.filter(x => x !== "hold");
      assert.deepEqual(base(on.s.order), ["range", "range", "terminal", "held"]);
      assert.deepEqual(base(off.s.order), ["range", "held"]);
      assert.ok(on.s.holdReads > off.s.holdReads);
      assert.ok(off.s.order.indexOf("range") < off.s.order.indexOf("held"));
    } finally { await off.close(); await on.close(); }
  });
});
