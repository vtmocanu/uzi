import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { describe, it, type TestContext } from "node:test";
import { api, git, installHarness } from "./runner-harness.js";
import {
  OWNER_FILE, confirmParks, gitIn, scene, terminal, type Scene, type SceneOptions,
} from "./runner-orphan-remediation-scene.js";

installHarness();

// issue #4058 M2 - the races after the initial classification, and the negative controls that must keep their existing
// terminal refusals: a record enrolled after the journal was classified, a re-read that disagrees, and records that are
// forged, malformed, cross-branch, canonical-path or on a live owner.

const linux = { skip: process.platform !== "linux" };
type ReadFence = typeof git.readOrphanJournalFence;
type Fence = Awaited<ReturnType<ReadFence>>;

const snapshot = (s: Scene) => ({
  durable: s.durable(), journal: s.journalRaw(), owner: fs.readFileSync(path.join(s.clone, OWNER_FILE), "utf8"),
});
const failures = (s: Scene): string[] => api.states
  .filter(x => x.runId === s.claim.run_id && x.body.status === "failed").map(x => String(x.body.failure_reason));
const subreasons = (s: Scene) => s.lines.filter(l => l.event === "orphan_reclaim_refused").map(l => `${l.stage}/${l.reason}/${l.retirement_subreason}`);

/** Enroll the record right after the first journal classification returned, so the effect (not the classification) meets it. */
function enrollAfterFirstRead(t: TestContext, s: Scene, script: Record<number, () => Promise<Fence>> = {}) {
  const real = git.readOrphanJournalFence.bind(git);
  let n = 0;
  const reads = t.mock.method(git, "readOrphanJournalFence", (async (...args: Parameters<ReadFence>) => {
    const call = ++n;
    if (call === 1) {
      const first = await real(...args);
      await s.enroll();
      return first;
    }
    return script[call] ? script[call]!() : real(...args);
  }) as never);
  return { reads };
}

for (const worker of [true, false]) {
  const branch = worker ? "releaseAttemptInPlace" : "retireRunnerClone";
  const base: SceneOptions = { entry: "A", attemptPaths: worker, enroll: false };

  describe(`a record enrolled after the classification (${branch})`, linux, () => {
    it("the typed refusal re-reads an exact record and the run holds", async t => {
      confirmParks(t);
      const s = await scene(t, base);
      enrollAfterFirstRead(t, s);
      const before = { ...snapshot(s) };
      await s.run();
      // The enrolment itself changed the journal; every byte after it must equal the enrolled journal.
      assert.equal(s.parks(), 1);
      assert.deepEqual(terminal(s), []);
      assert.equal(s.entered.value, false);
      assert.notEqual(snapshot(s).journal, before.journal);
      assert.equal(JSON.parse(s.journalRaw()).remediationRecovery.branch, s.branch);
      assert.ok(fs.existsSync(s.clone));
      assert.equal(s.guidance().length >= 1, true);
      assert.ok(subreasons(s).includes("completion_handoff/completion_retirement_pending/remediation_retained"));
    });

    it("a re-read that reports the pair changed re-admits, and the next classification holds", async t => {
      confirmParks(t);
      const s = await scene(t, base);
      enrollAfterFirstRead(t, s, { 2: async () => ({ state: "changed" }) });
      await s.run();
      assert.ok(s.entries.length >= 2, "the claimant was re-admitted");
      assert.equal(s.parks(), 1);
      assert.deepEqual(terminal(s), []);
    });

    it("a re-read with no remediation re-admits, and the next classification holds", async t => {
      confirmParks(t);
      const s = await scene(t, base);
      enrollAfterFirstRead(t, s, { 2: async () => ({ state: "unfenced", retained: false }) });
      await s.run();
      assert.ok(s.entries.length >= 2);
      assert.equal(s.parks(), 1);
      assert.deepEqual(terminal(s), []);
    });

    it("a re-read naming another branch keeps the path_error refusal and holds nothing", async t => {
      confirmParks(t);
      const s = await scene(t, base);
      enrollAfterFirstRead(t, s, { 2: async () => ({ state: "unfenced", retained: false, remediation: "mismatch" }) });
      await s.run();
      assert.equal(s.parks(), 0);
      assert.deepEqual(s.guidance(), []);
      assert.equal(failures(s).length, 1, "the original terminal refusal");
      assert.ok(s.lines.some(l => l.event === "orphan_reclaim_refused" && l.stage === "path" && l.reason === "path_error"));
      assert.equal(s.entered.value, false);
    });

    it("an unreadable re-read is a nonterminal stop with no hold reported", async t => {
      confirmParks(t);
      const s = await scene(t, base);
      enrollAfterFirstRead(t, s, { 2: async () => { throw new Error("EIO: journal unreadable"); } });
      await s.run();
      assert.equal(s.parks(), 0);
      assert.deepEqual(s.guidance(), []);
      assert.deepEqual(terminal(s), [], "no terminal report");
      assert.equal(s.entered.value, false);
      assert.ok(s.lines.some(l => l.event === "orphan_reclaim_refused" && l.stage === "retirement" &&
        l.retirement_subreason === "remediation_retained"));
      assert.ok(fs.existsSync(s.clone));
    });
  });
}

describe("a record enrolled between the classification and the retained snapshot", linux, () => {
  for (const authority of [false, true]) {
    it(`the detach guard refuses, the re-read sees the record and the run holds (${authority ? "authority" : "no authority"})`, async t => {
      confirmParks(t);
      const s = await scene(t, { entry: "A", attemptPaths: true, enroll: false, retained: true, authority });
      const snap = git.terminalRetainedSnapshot.bind(git);
      let enrolled = false;
      t.mock.method(git, "terminalRetainedSnapshot", (async (...args: Parameters<typeof snap>) => {
        if (!enrolled) { enrolled = true; await s.enroll(); }
        return snap(...args);
      }) as never);
      await s.run();
      assert.equal(enrolled, true);
      assert.equal(s.parks(), 1);
      assert.deepEqual(terminal(s), []);
      assert.equal(s.calls.detach(), 1, "the detach guard was the refusing seam");
      assert.ok(JSON.parse(s.journalRaw()).remediationRecovery, "the journal still carries the record");
      assert.ok(fs.existsSync(s.clone));
      assert.equal(s.entered.value, false);
    });
  }
});

describe("negative controls keep their existing terminal refusals", linux, () => {
  const rewrite = (s: Scene, edit: (j: Record<string, any>) => void) => {
    const j = JSON.parse(s.journalRaw());
    edit(j);
    gitIn(s.bare, "config", "--local", `uzi-recovery.${s.branch}.clone`, JSON.stringify(j));
  };
  async function refused(t: TestContext, s: Scene) {
    const before = snapshot(s);
    await s.run();
    assert.equal(s.parks(), 0, "never held");
    assert.deepEqual(s.guidance(), []);
    assert.equal(failures(s).length, 1, "the existing terminal refusal");
    assert.equal(s.entered.value, false);
    assert.deepEqual(snapshot(s), before, "nothing changed");
    assert.equal(s.calls.release() + s.calls.retire() + s.calls.detach(), 0);
    void t;
  }

  it("a live (nonterminal) owner", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true });
    api.setOrphanClassification(s.ownerRun, { status: "running", repo_id: "r1", kind: "issue", issue_iid: s.iid, branch: null, pipeline_ref: null, pipeline_id: null });
    await refused(t, s);
    assert.ok(s.lines.some(l => l.reason === "nonterminal_owner"));
  });

  it("an owner of another repository", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true });
    api.setOrphanClassification(s.ownerRun, { status: "cancelled", repo_id: "other", kind: "issue", issue_iid: s.iid, branch: null, pipeline_ref: null, pipeline_id: null });
    await refused(t, s);
    assert.ok(s.lines.some(l => l.reason === "repo_mismatch"));
  });

  it("an owner whose derived branch differs", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true });
    api.setOrphanClassification(s.ownerRun, { status: "cancelled", repo_id: "r1", kind: "issue", issue_iid: s.iid + 1, branch: null, pipeline_ref: null, pipeline_id: null });
    await refused(t, s);
    assert.ok(s.lines.some(l => l.reason === "branch_mismatch"));
  });

  it("a forged record (another attempt id) stays a path_error", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true });
    rewrite(s, j => { j.remediationRecovery.attemptId = "20261011T000000Z-g29-bbbbbbbbbbbbbbbb"; });
    await refused(t, s);
    console.log("PROBE", s.entries.map(e => e.constructor.name), failures(s));
  });

  it("a malformed record (unknown field) stays a path_error", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "B", attemptPaths: true });
    rewrite(s, j => { j.remediationRecovery.unexpected = 1; });
    await refused(t, s);
    console.log("PROBE", s.entries.map(e => e.constructor.name), failures(s));
  });

  it("a schema-valid record naming another branch is a path_error at the first classification", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: false });
    rewrite(s, j => { j.remediationRecovery.branch = "agent/issue-9999"; });
    await refused(t, s);
    assert.ok(s.lines.some(l => l.stage === "path" && l.reason === "path_error"));
  });

  it("a hand-written canonical-path remediation journal is refused as before and never held", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true });
    rewrite(s, j => { delete j.attemptId; delete j.remediationRecovery.attemptId; });
    await refused(t, s);
  });
});
