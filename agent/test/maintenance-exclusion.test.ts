import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { DindPruneGate } from "../src/dind-prune.js";
import { MaintenanceWriterExclusion, type MaintenanceExclusion, type MaintenanceExclusionDeps } from "../src/maintenance-exclusion.js";
import { MaintenanceResources } from "../src/recovery-maintenance.js";
import { dockerRunningContainers, recordMaintenanceBootstrap, setQuiescenceViewForTests, terminalMaintenanceProof } from "../src/run-quiescence.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import { recordingLogger } from "./helpers.js";

function fixture(overrides: Partial<MaintenanceExclusionDeps> = {}) {
  const gate = new DindPruneGate();
  const proofs: number[] = [];
  let idle = true, proven = true;
  const log = recordingLogger();
  const deps: MaintenanceExclusionDeps = {
    gate, isIdle: () => idle, log: log.logger,
    proof: async n => { proofs.push(n); return proven; },
    ...overrides,
  };
  return { gate, proofs, log, exclusion: new MaintenanceWriterExclusion(deps),
    setIdle: (value: boolean) => { idle = value; }, setProven: (value: boolean) => { proven = value; } };
}
const joined = () => true;

test("#3031 exclusion holds the claim gate for the work and reopens only its own owner", async () => {
  const f = fixture();
  const result = await f.exclusion.run(async exclusion => {
    assert.equal(f.gate.heldBy("terminal_maintenance"), true);
    assert.equal(f.gate.tryEnterClaim(), false, "no claim can start while exclusion holds");
    assert.equal(exclusion.open(), true);
    await exclusion.prove();
    return "done";
  }, joined);
  assert.deepEqual(result, { entered: true, value: "done" });
  assert.deepEqual(f.proofs, [1, 3], "one uncharged pass, three in the slot");
  assert.equal(f.gate.heldBy("terminal_maintenance"), false);
  assert.equal(f.gate.tryEnterClaim(), true, "a queued claim proceeds after reopen");
});

for (const [label, setup, reason] of [
  ["another owner holds the gate", (f: ReturnType<typeof fixture>) => f.gate.close("retained_ops"), "claim_gate_held"],
  ["a claim is in flight", (f: ReturnType<typeof fixture>) => { f.gate.tryEnterClaim(); }, "claims_in_flight"],
  ["the worker is busy", (f: ReturnType<typeof fixture>) => f.setIdle(false), "worker_not_idle"],
  ["the uncharged proof fails", (f: ReturnType<typeof fixture>) => f.setProven(false), "maintenance_writer_exclusion_unproven"],
] as const) {
  test(`#3031 exclusion refuses before any charge when ${label}`, async () => {
    const f = fixture();
    setup(f);
    let worked = false;
    const result = await f.exclusion.run(async () => { worked = true; }, joined);
    assert.deepEqual(result, { entered: false, reason });
    assert.equal(worked, false, "the producer never reaches its charge");
    assert.ok(f.proofs.every(n => n === 1), "a refusal never runs the charged proof");
    assert.equal(f.gate.heldBy("terminal_maintenance"), false);
  });
}

test("#3031 a writer appearing mid-slot fails the in-slot proof", async () => {
  let call = 0;
  const f = fixture({ proof: async () => ++call === 1 });
  await assert.rejects(f.exclusion.run(exclusion => exclusion.prove(), joined), /maintenance_writer_exclusion_unproven/);
  assert.equal(f.gate.heldBy("terminal_maintenance"), false, "a confirmed join reopens even after a refusal");
});

test("#3031 an escaped exclusion never revives when a later invocation holds the same owner", async () => {
  const f = fixture();
  let escaped!: MaintenanceExclusion;
  await f.exclusion.run(async exclusion => { escaped = exclusion; }, joined);
  assert.equal(escaped.open(), false);
  await f.exclusion.run(async current => {
    assert.equal(current.open(), true);
    assert.equal(escaped.open(), false, "run A's scope stays dead while run B holds terminal_maintenance");
    await assert.rejects(escaped.prove(), /maintenance_writer_exclusion_unproven/);
  }, joined);
  assert.deepEqual(f.proofs, [1, 1], "the escaped scope never ran a charged proof");
});

for (const owner of ["cache", "maintenance", "retained_ops"] as const) {
  test(`#3031 a foreign ${owner} hold revokes the current scope for good`, async () => {
    const f = fixture();
    await assert.rejects(f.exclusion.run(async exclusion => {
      f.gate.close(owner);
      assert.equal(exclusion.open(), false);
      f.gate.open(owner);
      assert.equal(exclusion.open(), false, "reopening the foreign owner does not reactivate the scope");
      await exclusion.prove();
    }, joined), /maintenance_writer_exclusion_unproven/);
    assert.deepEqual(f.proofs, [1]);
  });
}

test("#3031 open() turns false for good when the worker stops being idle or a claim enters", async () => {
  for (const interfere of ["idle", "claim"] as const) {
    const f = fixture();
    await assert.rejects(f.exclusion.run(async exclusion => {
      if (interfere === "idle") f.setIdle(false);
      else (f.gate as unknown as { inFlight: number }).inFlight = 1;
      assert.equal(exclusion.open(), false);
      f.setIdle(true);
      (f.gate as unknown as { inFlight: number }).inFlight = 0;
      assert.equal(exclusion.open(), false, "no revival");
      await exclusion.prove();
    }, joined), /maintenance_writer_exclusion_unproven/);
  }
});

test("#3031 claims stay gated until the whole boundary, including its joins, settles", async () => {
  const f = fixture();
  let releaseJoin!: () => void;
  const join = new Promise<void>(resolve => { releaseJoin = resolve; });
  const run = f.exclusion.run(async () => { await join; }, joined);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(await f.exclusion.run(async () => {}, joined), { entered: false, reason: "exclusion_busy" });
  assert.equal(f.gate.tryEnterClaim(), false, "liveness never reopens the gate before the join");
  releaseJoin();
  await run;
  assert.equal(f.gate.tryEnterClaim(), true);
});

for (const outcome of ["resolved", "rejected"] as const) {
  test(`#3031 unconfirmed settlement after ${outcome} work keeps claims gated until restart`, async () => {
    const f = fixture();
    const run = f.exclusion.run(async () => { if (outcome === "rejected") throw new Error("survivors"); }, () => false);
    if (outcome === "rejected") await assert.rejects(run, /survivors/);
    else await run;
    assert.equal(f.gate.heldBy("terminal_maintenance"), true);
    assert.equal(f.gate.tryEnterClaim(), false, "no claim may start beside an unjoined writer");
    assert.deepEqual(await f.exclusion.run(async () => {}, joined), { entered: false, reason: "exclusion_busy" });
    assert.ok(f.log.lines.some(l => (l as { diagnostic?: string }).diagnostic === "maintenance_writer_settlement_unconfirmed"));
  });
}

test("#3031 the Docker probe is one read-only GET of running containers; only an exact empty array is none", async t => {
  const seen: string[] = [];
  let reply: { status: number; body: string } = { status: 200, body: "[]" };
  const server = http.createServer((req, res) => {
    seen.push(`${req.method} ${req.url}`);
    res.writeHead(reply.status, { "content-type": "application/json" }).end(reply.body);
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => server.close());
  const host = `tcp://127.0.0.1:${(server.address() as { port: number }).port}`;
  assert.equal(await dockerRunningContainers(host), "none");
  for (const [next, expected] of [
    [{ status: 200, body: '[{"Id":"x"}]' }, "running"], [{ status: 500, body: "[]" }, "unknown"],
    [{ status: 200, body: "{}" }, "unknown"], [{ status: 200, body: "not json" }, "unknown"],
  ] as const) {
    reply = next;
    assert.equal(await dockerRunningContainers(host), expected);
  }
  assert.ok(seen.every(line => line === "GET /containers/json"), seen.join(", "));
  assert.equal(await dockerRunningContainers("not a docker host"), "unknown");
});

test("#3031 the uncharged proof bills its Docker request to the same discovery scope", async t => {
  // A fake proc tree with only this worker and its init, behind one proc mount.
  const root = fs.mkdtempSync(path.resolve("../.uzi/scratch/exclusion-proc-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const row = (pid: number, ppid: number) => {
    fs.mkdirSync(path.join(root, String(pid)));
    fs.writeFileSync(path.join(root, String(pid), "stat"), `${pid} (node) S ${ppid} ${pid} ${pid} ${"0 ".repeat(15)}100 0 0`);
    fs.writeFileSync(path.join(root, String(pid), "status"), `Name:\tnode\nState:\tS (sleeping)\nUid:\t1\t1\t1\t1\n`);
  };
  row(process.pid, 1); row(1, 0);
  fs.mkdirSync(path.join(root, "self"));
  fs.writeFileSync(path.join(root, "self", "mountinfo"), "22 1 0:5 / /proc rw - proc proc rw\n");
  setQuiescenceViewForTests({ procRoot: root });
  t.after(restoreHermeticView);
  recordMaintenanceBootstrap([]);
  let requests = 0;
  const server = http.createServer((_req, res) => { requests++; res.writeHead(200).end("[]"); });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => server.close());
  const host = `tcp://127.0.0.1:${(server.address() as { port: number }).port}`;
  assert.equal(await terminalMaintenanceProof(1, host), true);
  assert.equal(requests, 1);
  // An exhausted discovery request allowance starts no Docker request at all.
  const exhausted = await MaintenanceResources.runDiscoveryPass(async () => {
    await MaintenanceResources.runDiscovery(async () => { MaintenanceResources.consumeIfActive("rpcAttempts", 16); });
    return terminalMaintenanceProof(1, host).catch(() => false);
  });
  assert.equal(exhausted, false);
  assert.equal(requests, 1, "the request was refused by the shared discovery scope, not sent unscoped");
});

test("#3031 a scoped Docker request settles on a complete response, not at its timeout", async t => {
  const server = http.createServer((_req, res) => { res.writeHead(200).end("[]"); });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => server.close());
  const host = `tcp://127.0.0.1:${(server.address() as { port: number }).port}`;
  const started = Date.now();
  assert.equal(await MaintenanceResources.runDiscovery(() =>
    dockerRunningContainers(host, MaintenanceResources.signalIfActive())), "none");
  assert.ok(Date.now() - started < 2_000, `settled in ${Date.now() - started} ms`);
});

for (const owner of ["cache", "maintenance", "retained_ops"] as const) {
  test(`#3031 a foreign ${owner} close and reopen between checks still revokes the scope`, async () => {
    const f = fixture();
    await assert.rejects(f.exclusion.run(async exclusion => {
      assert.equal(exclusion.open(), true);
      f.gate.close(owner);
      f.gate.open(owner); // never observed by open() while closed
      assert.equal(exclusion.open(), false);
      await exclusion.prove();
    }, joined), /maintenance_writer_exclusion_unproven/);
    assert.deepEqual(f.proofs, [1]);
  });
}
