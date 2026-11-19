import { test } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import type { ChildProcess, SpawnOptions } from "node:child_process";
import { managedBareGroupTrustForTest, probeRunnerGroupTrust, type RunnerGroupTrustDeps } from "../src/runner-group-trust.js";

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

for (const [name, runnerS, cmdS] of [
  ["worker gid in runner Groups", runnerOk({ Groups: "10001 10002" }), cmdOk()],
  ["worker gid in runner-cmd Groups", runnerOk(), cmdOk({ Groups: "10002 10001" })],
  ["CapEff non-zero", runnerOk({ CapEff: "0000000000000080" }), cmdOk()],
  ["CapPrm non-zero", runnerOk(), cmdOk({ CapPrm: "0000000000000001" })],
  ["CapInh non-zero", runnerOk({ CapInh: "0000000000000040" }), cmdOk()],
  ["CapAmb non-zero", runnerOk(), cmdOk({ CapAmb: "0000000000000040" })],
  ["CapBnd other value", runnerOk({ CapBnd: "000001ffffffffff" }), cmdOk()],
  ["NoNewPrivs 0", runnerOk(), cmdOk({ NoNewPrivs: "0" })],
  ["fs uid mismatch", runnerOk({ Uid: "10002\t10002\t10002\t10001" }), cmdOk()],
  ["fs gid mismatch", runnerOk(), cmdOk({ Gid: "10003\t10003\t10003\t10001" })],
  ["gid field holds worker gid", runnerOk({ Gid: "10001\t10002\t10002\t10002" }), cmdOk()],
  ["missing line", runnerOk({ NoNewPrivs: null }), cmdOk()],
  ["duplicate line", runnerOk() + "NoNewPrivs:\t1\n", cmdOk()],
  ["unparsable cap", runnerOk({ CapEff: "zz" }), cmdOk()],
] as const) {
  test(`off: ${name}`, async () => {
    const { result } = await withSplitEnv(() => probe([{ stdout: runnerS }, { stdout: cmdS }]));
    assert.equal(result.state, "off");
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
      probe([{ hang: true }, { stdout: cmdOk() }], { timeoutMs: 30, kill: (w) => { killed.push(w); } }));
    assert.equal(result.state, "off");
    assert.deepEqual(killed, ["runner"]);
    await new Promise((r) => setImmediate(r));
    assert.deepEqual(rejections, []);
  } finally {
    process.off("unhandledRejection", onRej);
  }
});

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
