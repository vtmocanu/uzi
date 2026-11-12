// Issue #3644 Gate B (m4b part 2): the operator's guarded discard of an independently restored run, through the real
// RetainedSourceServer over its Unix socket. The server must hand GitCache the retained-home marker hooks, or the restore exit
// (S's discard, then P's, the marker removed last) can never be planned or finished.
import assert from "node:assert/strict";
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import { DindPruneGate } from "../src/dind-prune.js";
import { RetainedSourceServer } from "../src/ops/retained-source-server.js";
import { parseRestoreValue } from "../src/restore-journal.js";
import { StubExecutor } from "../src/executor.js";
import { nullLogger } from "./helpers.js";
import { git, installHarness } from "./runner-harness.js";
import { BRANCH, buildRig, cfgAll, cfgGet, cleanupRig, logged, resetRigRoots, rigDefaults, type Rig } from "./restore-route-rig.js";

installHarness();
rigDefaults.independentRestore = true;

type Json = Record<string, any>;
const servers: Array<() => Promise<void>> = [];
afterEach(async () => {
  for (const stop of servers.splice(0)) await stop();
  cleanupRig();
});
beforeEach(() => { resetRigRoots(); });

const restoreKey = `uzi-restore.${BRANCH}.journal`;
const markerFile = (r: Rig): string => path.join(r.dataDir, "retained-homes", `${r.runId}.json`);

function send(sock: string, req: unknown): Promise<Json> {
  return new Promise((resolve, reject) => {
    const conn = net.createConnection(sock);
    const chunks: Buffer[] = [];
    conn.on("error", reject);
    conn.on("connect", () => conn.write(`${JSON.stringify(req)}\n`));
    conn.on("data", c => chunks.push(Buffer.from(c)));
    conn.on("end", () => { try { resolve(JSON.parse(Buffer.concat(chunks).toString("utf8")) as Json); } catch (err) { reject(err); } });
  });
}

async function serve(r: Rig, state: { status: string }): Promise<string> {
  const server = new RetainedSourceServer({
    dataDir: r.dataDir, git, gate: new DindPruneGate(), runner: r.runner, log: nullLogger(), isIdle: () => true, authority: r.sRt,
    client: {
      listRecoveryHolds: async runId => ({ run_id: runId, holds: [] }),
      getRunOwnership: async () => ({ status: state.status, claim_generation: r.claim.claim_generation }),
    },
  });
  const ac = new AbortController();
  const served = server.serve(ac.signal);
  await server.ready;
  servers.push(async () => { ac.abort(); await server.close(); await served.catch(() => undefined); });
  return server.path;
}

async function inspectOf(sock: string, r: Rig, generation: number): Promise<Json> {
  const listed = await send(sock, { op: "inspect" });
  const found = (listed.sources as Json[]).filter(x => x.runId === r.runId && x.generation === generation);
  assert.equal(found.length, 1, JSON.stringify(listed.sources));
  return found[0]!;
}
const discard = (sock: string, r: Rig, source: Json): Promise<Json> =>
  send(sock, { op: "discard", run: r.runId, generation: source.generation, attempt: source.attempt, confirm: source.descriptor });

const sRig = (): Promise<Rig> => buildRig({ addrAware: true, successorExecutor: new StubExecutor(nullLogger()) });

describe("the guarded discard of a restored run (issue #3644 m4b part 2)", () => {
  it("a terminal run: S is discarded, then P, and P's exit removes the marker last and clears P's key and the restore key", async () => {
    const r = await sRig();
    await r.runner.execute(r.claim);
    assert.equal(logged(r, "independent restore seeded").length, 1);
    const sock = await serve(r, { status: "failed" });
    assert.equal(fs.existsSync(markerFile(r)), true, "the marker protects the predecessor's HOME and provision directory");
    const value = parseRestoreValue(cfgGet(r, restoreKey)!);
    assert.equal(value.kind, "entry");

    // S first.
    const s = await inspectOf(sock, r, 2);
    const sDone = await discard(sock, r, s);
    assert.equal(sDone.result.kind, "done", JSON.stringify(sDone));
    assert.equal(fs.existsSync(String(s.sources[0].clonePath)), false, "S's clone is gone");
    assert.equal(fs.existsSync(markerFile(r)), true, "S's discard leaves the marker");
    assert.ok(cfgGet(r, r.journalKey), "P's journal is still there");

    // Then P: through the committed restore exit.
    const p = await inspectOf(sock, r, 1);
    const pDone = await discard(sock, r, p);
    assert.equal(pDone.result.kind, "done", JSON.stringify(pDone));
    assert.equal(fs.existsSync(markerFile(r)), false, "the marker is removed");
    assert.equal(cfgGet(r, r.journalKey) ?? "", "", "P's key is cleared");
    assert.equal(cfgGet(r, restoreKey) ?? "", "", "the restore key is cleared");
    const ledger = cfgAll(r, r.ledgerKey).map(l => JSON.parse(l) as { attemptId: string; state: string });
    for (const id of [String(s.attempt), String(p.attempt)])
      assert.equal(ledger.filter(l => l.attemptId === id).at(-1)?.state, "retired", `${id} is retired`);
    // The owner discard is what closes P's hold: the inventory no longer reports a retained predecessor, so G is provable again.
    const inventory = await git.readInventoryCloneHeads(r.bare, r.runId);
    assert.equal(inventory.kind, "verified");
    assert.equal(inventory.kind === "verified" ? inventory.retainedPredecessors : "unknown", undefined);
  });

  it("neither P nor S is ever offered the ordinary local_discard, even for a terminal run", async () => {
    const r = await sRig();
    await r.runner.execute(r.claim);
    const sock = await serve(r, { status: "failed" });
    for (const generation of [1, 2]) {
      const selected = await send(sock, { op: "inspect", run: r.runId, generation, attempt: (await inspectOf(sock, r, generation)).attempt });
      const source = (selected.sources as Json[])[0]!;
      assert.notEqual(source.disposition?.kind, "local_discard", `generation ${generation}: ${JSON.stringify(source.disposition)}`);
      assert.notEqual(source.disposition?.kind, "local_protected_discard");
      if (generation === 1) assert.ok(source.disposition.consumers.includes("restore_predecessor_retained"), "P is named as a retained restore predecessor");
    }
  });

  it("a run that is not terminal refuses S's discard and changes nothing", async () => {
    const r = await sRig();
    await r.runner.execute(r.claim);
    const sock = await serve(r, { status: "running" });
    const config = fs.readFileSync(path.join(r.bare, "config"), "utf8");
    const marker = fs.readFileSync(markerFile(r), "utf8");
    const s = await inspectOf(sock, r, 2);
    const refused = await discard(sock, r, s);
    assert.equal(refused.result.kind, "refused", JSON.stringify(refused));
    assert.equal(refused.result.reason, "run_state_not_terminal");
    assert.equal(fs.existsSync(String(s.sources[0].clonePath)), true);
    assert.equal(fs.readFileSync(path.join(r.bare, "config"), "utf8"), config);
    assert.equal(fs.readFileSync(markerFile(r), "utf8"), marker);
  });

  it("P's own discard is refused while S is live: the entry keeps both", async () => {
    const r = await sRig();
    await r.runner.execute(r.claim);
    const sock = await serve(r, { status: "failed" });
    const config = fs.readFileSync(path.join(r.bare, "config"), "utf8");
    const p = await inspectOf(sock, r, 1);
    const refused = await discard(sock, r, p);
    assert.notEqual(refused.result.kind, "done", JSON.stringify(refused));
    assert.equal(fs.existsSync(markerFile(r)), true);
    assert.equal(fs.readFileSync(path.join(r.bare, "config"), "utf8"), config);
  });
});
