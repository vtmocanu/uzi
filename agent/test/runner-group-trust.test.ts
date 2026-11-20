import { test } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import type { ChildProcess, SpawnOptions } from "node:child_process";
import { isIssuedManagedBareGroupTrust, managedBareGroupTrustForTest, probeRunnerGroupTrust, type RunnerGroupTrustDeps } from "../src/runner-group-trust.js";

type Status = Partial<Record<string, string | null>>;

function statusText(uid: number, gid: number, over: Status = {}): string {
  const f: Record<string, string | null> = {
    Uid: `${uid}\t${uid}\t${uid}\t${uid}`,
    Gid: `${gid}\t${gid}\t${gid}\t${gid}`,
    Groups: "10002",
    CapInh: "0000000000000000",
    CapPrm: "0000000000000000",
    CapEff: "0000000000000000",
    CapBnd: "00000000000000c0",
    CapAmb: "0000000000000000",
    NoNewPrivs: "1",
    ...over,
  };
  const lines = ["Name:\tcat"];
  for (const [k, v] of Object.entries(f)) if (v !== null) lines.push(`${k}:\t${v}`);
  return lines.join("\n") + "\n";
}
const runnerOk = (over: Status = {}) => statusText(10002, 10002, over);
const cmdOk = (over: Status = {}) => statusText(10003, 10003, { Groups: "10002", ...over });

type Behavior = { stdout?: string; code?: number | null; signal?: NodeJS.Signals | null; error?: boolean; hang?: boolean };

function fakeSpawn(behaviors: Behavior[], calls: { command: string; args: string[]; options: SpawnOptions }[]) {
  let i = 0;
  return (command: string, args: string[], options: SpawnOptions): ChildProcess => {
    calls.push({ command, args, options });
    const b = behaviors[i++] ?? {};
    const child = new EventEmitter() as unknown as ChildProcess & { stdout: EventEmitter; stderr: EventEmitter };
    (child as unknown as { stdout: EventEmitter }).stdout = new EventEmitter();
    (child as unknown as { stderr: EventEmitter }).stderr = new EventEmitter();
    (child as unknown as { pid: number }).pid = 4242 + i;
    (child as unknown as { kill: () => boolean }).kill = () => true;
    setImmediate(() => {
      if (b.error) { child.emit("error", Object.assign(new Error("spawn ENOENT"), { code: "ENOENT" })); return; }
      if (b.hang) return;
      if (b.stdout !== undefined) child.stdout.emit("data", Buffer.from(b.stdout));
      child.emit("close", b.code === undefined ? 0 : b.code, b.signal ?? null);
    });
    return child;
  };
}

async function probe(behaviors: Behavior[], over: RunnerGroupTrustDeps = {}) {
  const calls: { command: string; args: string[]; options: SpawnOptions }[] = [];
  const result = await probeRunnerGroupTrust({
    spawn: fakeSpawn(behaviors, calls),
    split: true,
    getuid: () => 10001,
    getgid: () => 10001,
    timeoutMs: 2000,
    kill: () => {},
    ...over,
  });
  return { result, calls };
}

const clean = (): Behavior[] => [{ stdout: runnerOk() }, { stdout: cmdOk() }];

async function withSplitEnv<T>(fn: () => Promise<T>): Promise<T> {
  const prev = process.env.UZI_UID_SPLIT;
  process.env.UZI_UID_SPLIT = "1";
  try { return await fn(); } finally {
    if (prev === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = prev;
  }
}

test("clean status for both identities is trusted with gid 10001 and frozen", async () => {
  const { result } = await withSplitEnv(() => probe(clean()));
  assert.deepEqual({ ...result }, { state: "trusted", gid: 10001 });
  assert.ok(Object.isFrozen(result));
});

test("probe spawns the wrapped cat for runner and runner-cmd with a fixed env", async () => {
  const { calls } = await withSplitEnv(() => probe(clean()));
  assert.equal(calls.length, 2);
  const runner = calls.find((c) => c.args.includes("runner"));
  const cmd = calls.find((c) => c.args.includes("10003"));
  assert.ok(runner && cmd);
  for (const [c, who] of [[runner, "runner"], [cmd, "10003"]] as const) {
    assert.equal(c.command, "/bin/setpriv");
    assert.equal(c.args[c.args.indexOf("--reuid") + 1], who);
    assert.deepEqual(c.args.slice(-2), ["/bin/cat", "/proc/self/status"]);
    assert.deepEqual(c.options.env, { PATH: "/usr/bin:/bin" });
    assert.deepEqual(c.options.stdio, ["ignore", "pipe", "pipe"]);
  }
});

test("the test factory runs the same evaluator", () => {
  const t = managedBareGroupTrustForTest({ runnerStatus: runnerOk(), commandStatus: cmdOk() });
  assert.equal(t.state, "trusted");
  assert.ok(Object.isFrozen(t));
  const bad = managedBareGroupTrustForTest({ runnerStatus: runnerOk({ Groups: "10001" }), commandStatus: cmdOk() });
  assert.equal(bad.state, "off");
});

for (const [name, runnerS, cmdS, reason] of [
  ["worker gid in runner Groups", runnerOk({ Groups: "10001 10002" }), cmdOk(), "runner_worker_gid_present"],
  ["worker gid in runner-cmd Groups", runnerOk(), cmdOk({ Groups: "10002 10001" }), "runner_cmd_worker_gid_present"],
  ["CapEff non-zero", runnerOk({ CapEff: "0000000000000080" }), cmdOk(), "runner_CapEff"],
  ["CapPrm non-zero", runnerOk(), cmdOk({ CapPrm: "0000000000000001" }), "runner_cmd_CapPrm"],
  ["CapInh non-zero", runnerOk({ CapInh: "0000000000000040" }), cmdOk(), "runner_CapInh"],
  ["CapAmb non-zero", runnerOk(), cmdOk({ CapAmb: "0000000000000040" }), "runner_cmd_CapAmb"],
  ["CapBnd other value", runnerOk({ CapBnd: "000001ffffffffff" }), cmdOk(), "runner_CapBnd"],
  ["NoNewPrivs 0", runnerOk(), cmdOk({ NoNewPrivs: "0" }), "runner_cmd_NoNewPrivs"],
  ["fs uid mismatch", runnerOk({ Uid: "10002\t10002\t10002\t10001" }), cmdOk(), "runner_uid"],
  ["fs gid mismatch", runnerOk(), cmdOk({ Gid: "10003\t10003\t10003\t10001" }), "runner_cmd_gid"],
  ["three-field Uid line", runnerOk({ Uid: "10002\t10002\t10002" }), cmdOk(), "runner_uid"],
  ["five-field Gid line", runnerOk(), cmdOk({ Gid: "10003\t10003\t10003\t10003\t10003" }), "runner_cmd_gid"],
  ["real gid field holds worker gid", runnerOk({ Gid: "10001\t10002\t10002\t10002" }), cmdOk(), "runner_gid"],
  ["missing line", runnerOk({ NoNewPrivs: null }), cmdOk(), "runner_NoNewPrivs_missing"],
  ["duplicate line", runnerOk() + "NoNewPrivs:\t1\n", cmdOk(), "runner_NoNewPrivs_duplicate"],
  ["unparsable cap", runnerOk({ CapEff: "zz" }), cmdOk(), "runner_CapEff_unparsable"],
] as const) {
  test(`off: ${name}`, async () => {
    const { result } = await withSplitEnv(() => probe([{ stdout: runnerS }, { stdout: cmdS }]));
    assert.equal(result.state, "off");
    assert.equal(result.state === "off" ? result.reason : undefined, reason);
  });
}

test("CapBnd zero is accepted", async () => {
  const { result } = await withSplitEnv(() => probe([{ stdout: runnerOk({ CapBnd: "0000000000000000" }) }, { stdout: cmdOk() }]));
  assert.equal(result.state, "trusted");
});

test("off: spawn ENOENT error event", async () => {
  const { result } = await withSplitEnv(() => probe([{ error: true }, { stdout: cmdOk() }]));
  assert.equal(result.state, "off");
});

test("off: spawn throws synchronously", async () => {
  const { result } = await withSplitEnv(() => probe([], { spawn: () => { throw new Error("boom"); } }));
  assert.equal(result.state, "off");
});

test("off: timeout without unhandled rejection", async () => {
  const killed: string[] = [];
  const rejections: unknown[] = [];
  const onRej = (e: unknown) => { rejections.push(e); };
  process.on("unhandledRejection", onRej);
  try {
    const { result } = await withSplitEnv(() =>
      probe([{ hang: true }, { stdout: cmdOk() }], { timeoutMs: 30, joinGraceMs: 20, kill: (w) => { killed.push(w); } }));
    assert.equal(result.state, "off");
    assert.deepEqual(killed, ["runner"]);
    await new Promise((r) => setImmediate(r));
    assert.deepEqual(rejections, []);
  } finally {
    process.off("unhandledRejection", onRej);
  }
});

// A child that hangs until killed; after the kill request it closes `closeDelayMs` later (never when undefined).
function killableSpawn(events: string[], closeDelayMs: number | undefined, trigger?: "oversize" | "stdout_error") {
  let n = 0;
  return (): ChildProcess => {
    const mine = n++;
    const child = new EventEmitter() as unknown as ChildProcess & { stdout: EventEmitter; stderr: EventEmitter };
    (child as unknown as { stdout: EventEmitter }).stdout = new EventEmitter();
    (child as unknown as { stderr: EventEmitter }).stderr = new EventEmitter();
    (child as unknown as { pid: number }).pid = 5000 + mine;
    (child as unknown as { kill: () => boolean }).kill = () => true;
    if (mine === 0) {
      (child as unknown as { kill: () => boolean }).kill = () => {
        events.push("kill");
        if (closeDelayMs !== undefined) {
          setTimeout(() => { events.push("close"); child.emit("close", null, "SIGKILL"); }, closeDelayMs);
        }
        return true;
      };
      if (trigger === "oversize") setImmediate(() => child.stdout.emit("data", Buffer.alloc(65 * 1024, 0x78)));
      if (trigger === "stdout_error") setImmediate(() => child.stdout.emit("error", new Error("EIO")));
    } else {
      setImmediate(() => { child.stdout.emit("data", Buffer.from(cmdOk())); child.emit("close", 0, null); });
    }
    return child;
  };
}

test("timeout: the probe waits for the killed child to close before resolving off", async () => {
  const events: string[] = [];
  const rejections: unknown[] = [];
  const onRej = (e: unknown) => { rejections.push(e); };
  process.on("unhandledRejection", onRej);
  try {
    const result = await withSplitEnv(() => probeRunnerGroupTrust({
      spawn: killableSpawn(events, 80), split: true, getuid: () => 10001, getgid: () => 10001,
      timeoutMs: 20, joinGraceMs: 2000, kill: () => {},
    }));
    events.push("resolved");
    assert.deepEqual(events, ["kill", "close", "resolved"]);
    assert.deepEqual({ ...result }, { state: "off", reason: "runner_timeout" });
    await new Promise((r) => setImmediate(r));
    assert.deepEqual(rejections, []);
  } finally {
    process.off("unhandledRejection", onRej);
  }
});

test("timeout: a child that never closes after kill resolves off after the join grace", async () => {
  const events: string[] = [];
  const rejections: unknown[] = [];
  const onRej = (e: unknown) => { rejections.push(e); };
  process.on("unhandledRejection", onRej);
  try {
    const t0 = Date.now();
    const result = await withSplitEnv(() => probeRunnerGroupTrust({
      spawn: killableSpawn(events, undefined), split: true, getuid: () => 10001, getgid: () => 10001,
      timeoutMs: 20, joinGraceMs: 60, kill: () => {},
    }));
    assert.deepEqual(events, ["kill"]);
    assert.ok(Date.now() - t0 >= 70, "waited for the grace after the timeout");
    assert.deepEqual({ ...result }, { state: "off", reason: "runner_timeout_unjoined" });
    await new Promise((r) => setImmediate(r));
    assert.deepEqual(rejections, []);
  } finally {
    process.off("unhandledRejection", onRej);
  }
});

for (const trigger of ["oversize", "stdout_error"] as const) {
  test(`${trigger}: the probe waits for the killed child to close, and reports unjoined when it never does`, async () => {
    const events: string[] = [];
    const base = { split: true, getuid: () => 10001, getgid: () => 10001, timeoutMs: 5000, kill: () => {} };
    const joined = await withSplitEnv(() => probeRunnerGroupTrust({
      ...base, spawn: killableSpawn(events, 80, trigger), joinGraceMs: 2000,
    }));
    events.push("resolved");
    assert.deepEqual(events, ["kill", "close", "resolved"]);
    assert.deepEqual({ ...joined }, { state: "off", reason: `runner_${trigger}` });
    const never: string[] = [];
    const unjoined = await withSplitEnv(() => probeRunnerGroupTrust({
      ...base, spawn: killableSpawn(never, undefined, trigger), joinGraceMs: 40,
    }));
    assert.deepEqual(never, ["kill"]);
    assert.deepEqual({ ...unjoined }, { state: "off", reason: `runner_${trigger}_unjoined` });
  });
}

test("off: oversize stdout", async () => {
  const { result } = await withSplitEnv(() => probe([{ stdout: runnerOk() + "x".repeat(65 * 1024) }, { stdout: cmdOk() }]));
  assert.equal(result.state, "off");
});

test("off: non-zero exit and signal", async () => {
  const a = await withSplitEnv(() => probe([{ stdout: runnerOk(), code: 1 }, { stdout: cmdOk() }]));
  assert.equal(a.result.state, "off");
  const b = await withSplitEnv(() => probe([{ stdout: runnerOk() }, { stdout: cmdOk(), code: null, signal: "SIGKILL" }]));
  assert.equal(b.result.state, "off");
});

test("off: single-uid spawns nothing", async () => {
  const { result, calls } = await probe(clean(), { split: false });
  assert.deepEqual({ ...result }, { state: "off", reason: "single_uid" });
  assert.equal(calls.length, 0);
});

test("off: worker gid is not 10001, nothing spawned", async () => {
  const { result, calls } = await withSplitEnv(() => probe(clean(), { getgid: () => 10004 }));
  assert.equal(result.state, "off");
  assert.equal(calls.length, 0);
});

test("off: worker uid is not 10001", async () => {
  const { result } = await withSplitEnv(() => probe(clean(), { getuid: () => 0 }));
  assert.equal(result.state, "off");
});

test("only module-issued values are recognised; a cast literal is not", async () => {
  const { result } = await withSplitEnv(() => probe([{ stdout: runnerOk() }, { stdout: cmdOk() }]));
  assert.equal(isIssuedManagedBareGroupTrust(result), true);
  assert.equal(isIssuedManagedBareGroupTrust({ state: "trusted", gid: 10001 }), false);
  assert.equal(isIssuedManagedBareGroupTrust(Object.freeze({ ...result })), false);
});

test("no production module imports the test factory", async () => {
  const fs = await import("node:fs");
  const path = await import("node:path");
  const root = new URL("../src/", import.meta.url).pathname;
  const hits: string[] = [];
  const walk = (dir: string) => {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, e.name);
      if (e.isDirectory()) walk(p);
      else if (/\.(ts|mts|mjs|js)$/.test(e.name) && !p.endsWith(path.join("src", "runner-group-trust.ts")) &&
        fs.readFileSync(p, "utf8").includes("managedBareGroupTrustForTest")) hits.push(p);
    }
  };
  walk(root);
  assert.ok(fs.readFileSync(path.join(root, "runner-group-trust.ts"), "utf8").includes("managedBareGroupTrustForTest"), "scan target exists");
  assert.deepEqual(hits, []);
});
