// Issue #3644 Gate B (m4b part 2), PRD #3831 section 8.1: the delayed config child whose namespace survives its worker.
//
// A real child process (same PID namespace as this test) is blocked on a FIFO immediately before `git config` on the bare. The
// predecessor P "dies" as a PROCESS only: its authority incarnation is abandoned (its slot stays active under it), while the
// child, and so the namespace, live on. A successor S in the same compose slot whose execution identity is UNCHANGED (same
// namespace inode, init start ticks and boot id: both boundary records are written from the same procfs) attempts the restore.
//
//   safe   : the real predicate refuses at R5 (`same_execution`) before any successor byte; the released child's write then lands
//            with no S attribution anywhere in the bare.
//   unsafe : the named unsafe variant, run every time through the TEST-ONLY RunnerOptions.restoreSkipExecutionComparison seam
//            (the R5 comparison disabled); S is admitted and seeds, and the released child's stale write lands AFTER the restore
//            entry was written.
import assert from "node:assert/strict";
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import { StubExecutor } from "../src/executor.js";
import { nullLogger } from "./helpers.js";
import { installHarness } from "./runner-harness.js";
import {
  BRANCH, GIT_ENV, assertStoppedUntouched, buildRig, cfgAll, cfgGet, cleanupRig, drive, failed, logged, refusals, resetRigRoots, rigDefaults, type Rig,
} from "./restore-route-rig.js";

installHarness();
rigDefaults.independentRestore = true;

const children: ChildProcess[] = [];
afterEach(() => {
  for (const child of children.splice(0)) if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
  cleanupRig();
});
beforeEach(() => { resetRigRoots(); });

const restoreKey = `uzi-restore.${BRANCH}.journal`;
const DELAYED_KEY = "uzi.delayedwriter";
const configText = (r: Rig): string => fs.readFileSync(path.join(r.bare, "config"), "utf8");
const markerFile = (r: Rig): string => path.join(r.dataDir, "retained-homes", `${r.runId}.json`);

interface Delayed { child: ChildProcess; exited: Promise<number | null>; release(): Promise<void>; alive(): boolean }

/** Starts the writer and returns once it is blocked on the gate, before it has written anything. */
async function startDelayedWriter(r: Rig): Promise<Delayed> {
  const dir = fs.mkdtempSync(path.join(r.tmp, "delayed-"));
  const gate = path.join(dir, "gate");
  const ready = path.join(dir, "ready");
  execFileSync("mkfifo", [gate]);
  const script = 'touch "$READY"; read -r _ < "$GATE"; exec git -C "$BARE" config --local "$KEY" stale';
  const child = spawn("sh", ["-c", script], {
    env: { ...GIT_ENV, READY: ready, GATE: gate, BARE: r.bare, KEY: DELAYED_KEY }, stdio: "ignore" });
  children.push(child);
  const exited = new Promise<number | null>((resolve, reject) => { child.once("exit", code => resolve(code)); child.once("error", reject); });
  for (let i = 0; i < 400 && !fs.existsSync(ready); i++) await new Promise(res => setTimeout(res, 25));
  assert.equal(fs.existsSync(ready), true, "the writer reached its gate");
  // Give the shell the moment it needs to block in open(2) on the FIFO; the write cannot happen before the gate opens.
  await new Promise(res => setTimeout(res, 100));
  assert.equal(child.exitCode, null, "the writer is blocked, not finished");
  return {
    child, exited, alive: () => child.exitCode === null && child.signalCode === null,
    release: async () => { const handle = await fsp.open(gate, "w"); try { await handle.write("go\n"); } finally { await handle.close(); } },
  };
}

const sRig = (unsafe = false): Promise<Rig> => buildRig({ addrAware: true, pBoundary: "same_execution", successorExecutor: new StubExecutor(nullLogger()),
  ...(unsafe ? { runnerExtra: { restoreSkipExecutionComparison: true } } : {}) });

describe("a delayed config writer whose namespace survives its worker (PRD #3831 8.1)", () => {
  it("refuses at R5 (same_execution) before any successor byte; the released child's write then lands with no S attribution present", async () => {
    const r = await sRig();
    const writer = await startDelayedWriter(r);
    const before = configText(r);
    assert.equal(cfgGet(r, DELAYED_KEY), undefined, "nothing of the writer is in the bare yet");

    const { before: snap, traps } = await drive(r);
    // The real predicate refused on the unchanged execution identity ...
    assert.equal(refusals(r).length, 1, JSON.stringify(refusals(r)));
    assert.equal(refusals(r)[0]?.reason, "same_execution");
    assert.equal(logged(r, "independent restore seeded").length, 0);
    // ... and wrote nothing at all: the common contract of a refused route.
    await assertStoppedUntouched(r, snap, traps);
    assert.equal(configText(r), before, "the bare's config is byte-identical after the refusal");
    assert.equal(fs.existsSync(markerFile(r)), false, "no marker");
    assert.equal(cfgGet(r, restoreKey), undefined, "no restore entry");
    assert.equal(cfgAll(r, r.ledgerKey).length, 1, "no S ledger entry: only P's");
    assert.equal(writer.alive(), true, "the delayed writer outlived the refused admission");

    await writer.release();
    assert.equal(await writer.exited, 0, "the writer finished its git config");
    const after = configText(r);
    assert.equal(cfgGet(r, DELAYED_KEY), "stale", "the stale write landed");
    assert.notEqual(after, before);
    assert.equal(cfgGet(r, restoreKey), undefined, "with no restore entry present");
    assert.equal(fs.existsSync(markerFile(r)), false, "no marker present");
    assert.equal(cfgAll(r, r.ledgerKey).length, 1, "and no S attribution of any kind");
    assert.equal(failed(r).length, 0, "the refusal is nonterminal");
  });

  it("UNSAFE (execution comparison disabled by the test-only seam): S is admitted and the released child's stale write lands after the restore entry", async () => {
    const r = await sRig(true);
    const writer = await startDelayedWriter(r);
    await r.runner.execute(r.claim);
    assert.equal(logged(r, "independent restore seeded").length, 1, "without the comparison S is admitted and seeds");
    assert.notEqual(cfgGet(r, restoreKey), undefined, "the restore entry is written");
    const atRestore = configText(r);
    assert.equal(cfgGet(r, DELAYED_KEY), undefined);

    await writer.release();
    assert.equal(await writer.exited, 0);
    const after = configText(r);
    assert.notEqual(after, atRestore, "the bare's config changed after the restore entry was written");
    assert.equal(cfgGet(r, DELAYED_KEY), "stale", "by the stale predecessor-era write");
    assert.notEqual(cfgGet(r, restoreKey), undefined, "and S's attribution is present beside it");
  });
});
