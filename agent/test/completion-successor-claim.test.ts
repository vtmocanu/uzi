import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import type { RunRunner } from "../src/runner.js";
import { latchResidueQuarantine } from "../src/residue-quarantine.js";
import type { SlotSnapshot } from "../src/completion-authority.js";
import { nullLogger } from "./helpers.js";
import { api, git, gitlabClaim, installHarness, plantAfterFetch } from "./runner-harness.js";
import {
  BRANCH, fenceOf, journalRaw, recordNames, rig, slotVia, snapshotFiles,
  type PrivateRunner, type Rig,
} from "./completion-retirement-fixture.js";
import type { AuthorityRuntime } from "../src/authority-runtime.js";

installHarness();

// issue #3404 follow-up - a successor run on the SAME branch as a just-completed owner. The owner's journal
// still carries its completion-source fence until the certified retirement (forgetCompletedGeneration)
// runs; the paced boot/live passes may not have reached it when the successor claims first. The successor's
// clone phase must run that SAME certified retirement itself (RecoveryCoordinator.retireCompletedOwner) and
// then seed, and must fail exactly as before whenever the certified proof does not hold. Real runner, real
// authority store, real recovery coordinator, real Git; production wiring (authorityRequired) throughout.

const STOPPED = "stop after successor admission";
const OWNER_FILE = "OWNER-ONLY.txt";

interface Row {
  name: string;
  /** The completed owner's kind and clone slug (its REAL canonical path). */
  owner: { kind: "issue" | "mr_rework"; slug: string };
  /** The successor's kind; it derives agent-issue-3404 for mr_rework, issue-3404 for issue. */
  successor: "issue" | "mr_rework";
  samePath: boolean;
  codex: boolean;
  unproven?: boolean;
  restart: boolean;
}

const ROWS: Row[] = [];
for (const restart of [false, true]) {
  const when = restart ? "after restart" : "same incarnation";
  ROWS.push({ name: `cross-path (issue owner, mr_rework successor), ${when}`, owner: { kind: "issue", slug: "issue-3404" },
    successor: "mr_rework", samePath: false, codex: false, restart });
  ROWS.push({ name: `same-path (mr_rework owner, mr_rework successor), ${when}`, owner: { kind: "mr_rework", slug: "agent-issue-3404" },
    successor: "mr_rework", samePath: true, codex: false, restart });
}

interface Scene {
  r: Rig;
  runner: RunRunner;
  rt: AuthorityRuntime;
  /** The owner's slot as published, before the successor claims. */
  ownerSlot: SlotSnapshot | null;
  /** What the successor saw when its executor was entered. */
  started?: { worktree: string; slot: SlotSnapshot | null };
  /** retireCompletedOwner outcomes, with the durable state observed the moment each returned. */
  retirements: Array<{ ok: boolean; slot: SlotSnapshot | null; fence: unknown; cloneExists: boolean }>;
  claim: ReturnType<typeof gitlabClaim>;
}

async function scene(t: TestContext, row: Row, options: { receipt?: boolean } = {}): Promise<Scene> {
  const r = await rig(t, { codex: row.codex, unproven: row.unproven, kind: row.owner.kind, slug: row.owner.slug });
  // The production wiring: the git cache carries the authority hooks and requires them.
  const restore = { authority: (git as unknown as { authority?: unknown }).authority, required: (git as unknown as { authorityRequired?: boolean }).authorityRequired };
  t.after(() => { Object.assign(git, { authority: restore.authority, authorityRequired: restore.required }); });
  fs.writeFileSync(path.join(r.clone, OWNER_FILE), "owner-only bytes\n");
  await r.end();
  if (options.receipt !== false) await r.receipt();
  let rt = r.rt;
  let runner = r.p as unknown as RunRunner;
  if (row.restart) {
    const second = r.restart();
    rt = second.rt;
    runner = second.p as unknown as RunRunner;
  } else {
    Object.assign(git, { authority: rt.hooks, authorityRequired: true });
  }
  const ownerSlot = await slotVia(rt, r.clone);
  const claim = gitlabClaim(3404, { run_id: randomUUID(), kind: row.successor, ...(row.successor === "mr_rework" ? { branch: BRANCH } : {}) });
  api.setOrphanClassification(r.runId, {
    status: "completed", repo_id: "r1", kind: row.owner.kind, issue_iid: 3404,
    branch: null, pipeline_ref: row.owner.kind === "mr_rework" ? BRANCH : null, pipeline_id: null,
  });
  const s: Scene = { r, runner, rt, ownerSlot, retirements: [], claim };
  const recovery = (runner as unknown as PrivateRunner).recovery;
  if (typeof recovery.retireCompletedOwner === "function") {
    const real = recovery.retireCompletedOwner.bind(recovery);
    recovery.retireCompletedOwner = async (...args) => {
      const ok = await real(...args);
      s.retirements.push({ ok, slot: await slotVia(rt, r.clone), fence: fenceOf(r), cloneExists: fs.existsSync(path.join(r.clone, ".git")) });
      return ok;
    };
  }
  (r.executor as unknown as { run: unknown }).run = async (ctx: { worktreePath: string }) => {
    s.started = { worktree: ctx.worktreePath, slot: await slotVia(rt, ctx.worktreePath) };
    throw new Error(STOPPED);
  };
  return s;
}

const failure = (s: Scene): unknown =>
  api.states.filter(state => state.runId === s.claim.run_id && state.body.status === "failed").at(-1)?.body.failure_reason;

/** Everything the fail-closed controls require to stay byte-identical. */
function durable(s: Scene) {
  return {
    journal: journalRaw(s.r),
    records: snapshotFiles(s.r),
    names: recordNames(s.r),
    owner: fs.readFileSync(path.join(s.r.clone, OWNER_FILE), "utf8"),
    head: fs.existsSync(path.join(s.r.clone, ".git")) ? fs.readFileSync(path.join(s.r.clone, ".git", "HEAD"), "utf8") : null,
    skills: fs.existsSync(s.r.skills),
  };
}

async function assertFailsClosed(s: Scene, before: ReturnType<typeof durable>): Promise<void> {
  assert.equal(s.started, undefined, "the model never starts");
  assert.notEqual(failure(s), undefined, "the run fails");
  assert.ok(!String(failure(s)).includes(STOPPED));
  assert.deepEqual(durable(s), before, "journal, records and the owner clone are byte-identical");
  assert.ok(fenceOf(s.r), "the owner's fence is untouched");
  assert.equal((await slotVia(s.rt, s.r.clone))?.state, s.ownerSlot?.state, "the owner's slot is untouched");
}

describe("a successor claim on a just-completed owner's branch", () => {
  for (const row of ROWS) {
    it(`retires the owner by certified retirement before admission: ${row.name}`, async t => {
      const s = await scene(t, row);
      assert.ok(fenceOf(s.r), "the owner's journal still carries its completion fence");
      await s.runner.execute(s.claim);

      assert.equal(String(failure(s)), STOPPED, "the successor was admitted and reached its executor");
      assert.equal(s.retirements.length, 1);
      const retired = s.retirements[0]!;
      assert.equal(retired.ok, true, "the certified retirement passed");
      assert.equal(retired.slot?.state, "retired", "the owner's slot was tombstoned BEFORE the successor was admitted");
      assert.equal(retired.fence, undefined, "the owner's fence was cleared BEFORE the successor was admitted");
      assert.equal(retired.cloneExists, false, "the owner's clone was retired");
      assert.equal(recordNames(s.r).length, 0, "the owner's records and bundles are gone");

      assert.ok(s.started, "the successor's executor started");
      assert.equal(String(failure(s)).includes(STOPPED), true, "the run proceeded past the clone phase");
      assert.equal(s.started.slot?.state, "active", "the successor holds an active epoch on its path");
      assert.notEqual(s.started.slot?.epoch, s.ownerSlot?.epoch, "a FRESH epoch, not the owner's");
      assert.equal(fs.existsSync(path.join(s.started.worktree, OWNER_FILE)), false, "the owner's bytes are not in the successor's clone");
      if (!row.samePath) {
        assert.notEqual(s.started.worktree, s.r.clone);
        assert.equal((await slotVia(s.rt, s.r.clone))?.state, "retired", "the owner's slot stays retired");
      } else {
        // The owner's rig clone is an attempt path under the shared canonical slug (`<slug>.attempt-...`); the
        // successor seeds at the canonical slug itself. No final `retired` slot is asserted for the shared slug.
        assert.ok(s.r.clone.startsWith(s.started.worktree), "the successor seeds at the owner's canonical slug");
      }
    });
  }

  it("a Codex owner with a proven seal is retired like a Claude one", async t => {
    const s = await scene(t, { name: "codex", owner: { kind: "issue", slug: "issue-3404" }, successor: "mr_rework", samePath: false, codex: true, restart: false });
    await s.runner.execute(s.claim);
    assert.deepEqual(s.retirements.map(x => [x.ok, x.slot?.state, x.fence]), [[true, "retired", undefined]]);
    assert.ok(s.started, "the successor's executor started");
    assert.equal(s.started.slot?.state, "active");
  });

  it("a Codex owner whose seal is unproven publishes no fence: the certified retirement declines and the unchanged orphan reclaim runs", async t => {
    const s = await scene(t, { name: "codex-unproven", owner: { kind: "issue", slug: "issue-3404" }, successor: "mr_rework", samePath: false,
      codex: true, unproven: true, restart: false });
    assert.equal(fenceOf(s.r), undefined, "an unproven seal never published a completion fence");
    await s.runner.execute(s.claim);
    assert.deepEqual(s.retirements.map(x => x.ok), [false], "nothing certified, nothing retired");
    assert.equal((await slotVia(s.rt, s.r.clone))?.state, s.ownerSlot?.state, "the owner's slot is untouched by the declined retirement");
    assert.ok(s.started, "the pre-existing reclaim path seeded the successor");
    assert.equal(fs.existsSync(path.join(s.started.worktree, OWNER_FILE)), false);
  });

  it("no persisted receipt: nothing is retired and the run fails as before", async t => {
    const s = await scene(t, ROWS[0]!, { receipt: false });
    const before = durable(s);
    await s.runner.execute(s.claim);
    assert.deepEqual(s.retirements.map(x => x.ok), [false]);
    await assertFailsClosed(s, before);
  });

  it("owner execution tail held past the bound: nothing is retired and the run fails as before", async t => {
    const s = await scene(t, ROWS[0]!);
    const before = durable(s);
    let release!: () => void;
    const held = new Promise<void>(resolve => { release = resolve; });
    (s.runner as unknown as { executionTails: Map<string, Promise<void>> }).executionTails.set(s.r.runId, held);
    try {
      await s.runner.execute(s.claim);
    } finally {
      release();
      (s.runner as unknown as { executionTails: Map<string, Promise<void>> }).executionTails.delete(s.r.runId);
    }
    assert.deepEqual(s.retirements.map(x => x.ok), [false]);
    await assertFailsClosed(s, before);
  });

  it("residue quarantine latched: nothing is retired and the run fails as before", async t => {
    const s = await scene(t, ROWS[0]!);
    const before = durable(s);
    plantAfterFetch(() => latchResidueQuarantine({ cause: "fixture", runId: s.claim.run_id, site: "terminal_drive" }, nullLogger()));
    await s.runner.execute(s.claim);
    assert.deepEqual(s.retirements.map(x => x.ok), [false]);
    await assertFailsClosed(s, before);
  });
});
