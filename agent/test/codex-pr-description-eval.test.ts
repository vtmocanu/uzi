import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import type { ChildProcess } from "node:child_process";
import { makePrDescriptionEvalCodexFactory } from "../src/codex/codex-executor.js";
import { CODEX_BIN } from "../src/codex/launcher.js";
import { runEvalCli } from "../src/pr-description-eval-cli.js";
import { createCodexTransport } from "../src/codex/transport.js";
import type { PrDescriptionEvalLaunchTestSeams } from "../src/codex/pr-description-eval-launch.js";

const key = ["sk-", "eval-fixture-with-no-provider-authority"].join("");
const checkout = path.resolve("..");
const request = (signal = new AbortController().signal, timeoutMs = 1000) => ({
  label: "summary" as const, prompt: "Summarize the supplied change.",
  systemPrompt: "Return plain prose.", model: "gpt-6-sol",
  output: { kind: "text" as const }, signal, timeoutMs, graceMs: 5,
});
const policy = { onTerminal() {} };

function fixture(options: {
  signal?: AbortSignal;
  hang?: boolean; startupError?: boolean; disposalError?: boolean; text?: string;
  startup?: PrDescriptionEvalLaunchTestSeams["startup"];
  ignoreTerm?: boolean;
} = {}) {
  const events: string[] = [];
  let root = "";
  let config = "";
  let launchEnv: NodeJS.ProcessEnv = {};
  let argv: string[] = [];
  let bin = "";
  let active!: () => void;
  const running = new Promise<void>((resolve) => { active = resolve; });
  let spawned!: () => void;
  const ready = new Promise<void>((resolve) => { spawned = resolve; });
  const child = new EventEmitter() as ChildProcess;
  const stdin = new PassThrough();
  const stdout = new PassThrough();
  Object.assign(child, { stdin, stdout, stderr: new PassThrough(), pid: 42 });
  child.kill = (signal) => {
    events.push(String(signal));
    if (options.ignoreTerm && signal === "SIGTERM") return true;
    void fs.stat(root).then(() => {
      events.push("exit");
      stdout.end();
      child.emit("close", 0, signal);
    });
    return true;
  };
  let buffer = "";
  stdin.on("data", (chunk: Buffer) => {
    buffer += chunk.toString();
    for (;;) {
      const newline = buffer.indexOf("\n");
      if (newline < 0) break;
      const rpc = JSON.parse(buffer.slice(0, newline));
      buffer = buffer.slice(newline + 1);
      if (rpc.id === undefined) continue;
      events.push(rpc.method);
      if (rpc.method === "thread/start" || rpc.method === "turn/start") {
        assert.ok(!JSON.stringify(rpc.params).includes(key));
      }
      let result: unknown = {};
      if (rpc.method === "initialize") result = { userAgent: "codex/test", codexHome: launchEnv.CODEX_HOME, platformFamily: "unix", platformOs: "linux" };
      if (rpc.method === "account/login/start") {
        assert.deepEqual(rpc.params, { type: "apiKey", apiKey: key });
        result = { type: "apiKey" };
      }
      if (rpc.method === "thread/start") result = { thread: { id: "th" } };
      if (rpc.method === "turn/start") { result = { turn: { id: "tn" } }; active(); }
      stdout.write(JSON.stringify({ id: rpc.id, result }) + "\n");
      if (rpc.method === "turn/start" && !options.hang) {
        stdout.write(JSON.stringify({ method: "item/completed", params: {
          threadId: "th", turnId: "tn", item: { type: "agentMessage", text: options.text ?? "Readable summary" },
        } }) + "\n");
        stdout.write(JSON.stringify({ method: "turn/completed", params: {
          threadId: "th", turn: { id: "tn", status: "completed" },
        } }) + "\n");
      }
    }
  });
  const controller = new AbortController();
  const factory = makePrDescriptionEvalCodexFactory({
    checkoutPath: checkout, apiKey: key, signal: options.signal ?? controller.signal,
    testSeams: {
      terminateGraceMs: 5,
      spawn(command, args, opts) {
        bin = command; argv = args; launchEnv = opts.env;
        root = path.dirname(opts.cwd);
        events.push("spawn");
        spawned();
        queueMicrotask(() => child.emit(options.startupError ? "error" : "spawn", new Error("fake startup")));
        return child;
      },
      startup: options.startup,
      transport(opts) {
        const transport = createCodexTransport(opts);
        if (!options.disposalError) return transport;
        return new Proxy(transport, {
          get(target, prop) {
            if (prop === "close") return async () => {
              await target.close(); events.push("dispose-error"); throw new Error("fake disposal");
            };
            const value = Reflect.get(target, prop);
            return typeof value === "function" ? value.bind(target) : value;
          },
        });
      },
    },
  });
  return {
    factory, controller, ready, running, events,
    async inspect() {
      config = await fs.readFile(path.join(launchEnv.CODEX_HOME!, "config.toml"), "utf8");
      assert.equal(bin, CODEX_BIN);
      assert.deepEqual(argv, ["app-server"]);
      assert.ok(root.startsWith(path.join(checkout, ".uzi", "scratch") + path.sep));
      for (const name of ["HOME", "CODEX_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "TMPDIR", "TMP", "TEMP"]) {
        assert.equal(path.dirname(launchEnv[name]!), root);
        assert.equal((await fs.stat(launchEnv[name]!)).mode & 0o777, 0o700);
      }
      assert.equal((await fs.stat(root)).mode & 0o777, 0o700);
      assert.deepEqual(Object.keys(launchEnv).sort(), [
        "PATH", "LANG", "HOME", "CODEX_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME",
        "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "TMPDIR", "TMP", "TEMP",
      ].sort());
      assert.ok(!JSON.stringify({ argv, config, launchEnv }).includes(key));
      assert.match(config, /gpt-6-sol/);
      assert.match(config, /shell_tool = false/);
      assert.match(config, /code_mode_host = false/);
      assert.equal((await fs.stat(path.join(launchEnv.CODEX_HOME!, "config.toml"))).mode & 0o777, 0o600);
    },
    async rootGone() { await assert.rejects(fs.stat(root), { code: "ENOENT" }); },
    async removed() {
      assert.ok(events.includes("exit"));
      await assert.rejects(fs.stat(root), { code: "ENOENT" });
      await factory.close();
      await factory.close();
      await assert.rejects(factory.run(request(), policy));
      await assert.rejects(fs.stat(root), { code: "ENOENT" });
    },
  };
}

test("CLI uses a fresh sanctioned factory per explicit Codex pass and waits for cleanup", async () => {
  const signals = new EventEmitter();
  const fixtures: ReturnType<typeof fixture>[] = [];
  const rows: string[] = [];
  const result = await runEvalCli(["--harness", "codex", "--model", "gpt-6-sol", "--prompt-mode", "baseline"], {
    checkout, env: { OPENAI_API_KEY: key }, signals, write: (r) => rows.push(r),
    codexFactory(options) {
      for (const name of ["SIGINT", "SIGTERM", "SIGHUP"]) assert.equal(signals.listenerCount(name), 1);
      assert.equal(options.apiKey, key);
      const f = fixture({ signal: options.signal, text: '{"summary":"Implement the visible change."}' });
      fixtures.push(f);
      return {
        ...f.factory,
        run(request, policy) {
          assert.equal(request.model, "gpt-6-sol");
          assert.match(request.systemPrompt, /usually absent/);
          return f.factory.run(request, policy);
        },
      };
    },
  });
  assert.equal(result, 0);
  assert.equal(fixtures.length, 5);
  assert.equal(rows.length, 5);
  for (const f of fixtures) await f.removed();
  for (const row of rows) { assert.equal(JSON.parse(row).model_identity, "requested_alias"); assert.ok(!row.includes(key)); }
  for (const name of ["SIGINT", "SIGTERM", "SIGHUP"]) assert.equal(signals.listenerCount(name), 0);
});

test("success: real harness/login/transport; isolated root, minimal env, idempotent close", async () => {
  const f = await fixture({ hang: true });
  const pass = f.factory.run(request(), policy);
  await f.ready;
  await f.inspect();
  // Handled CLI signals are represented by aborting the provided controller.
  f.controller.abort();
  await assert.rejects(pass);
  await f.removed();

  const success = await fixture();
  assert.equal((await success.factory.run(request(), policy)).text, "Readable summary");
  assert.ok(success.events.includes("account/login/start"));
  await success.removed();
});

test("startup error awaits child exit and removes root", async () => {
  const f = await fixture({ startupError: true });
  await assert.rejects(f.factory.run(request(), policy));
  await f.removed();
});

test("pass policy error awaits cleanup", async () => {
  const f = await fixture();
  await assert.rejects(f.factory.run(request(), { onTerminal() { throw new Error("policy failure"); } }), /policy failure/);
  await f.removed();
});

test("active cancellation cleans up and escalates TERM to KILL", async () => {
  const f = await fixture({ hang: true, ignoreTerm: true });
  const pass = f.factory.run(request(), policy);
  const rejected = assert.rejects(pass);
  await f.running;
  f.controller.abort();
  await rejected;
  const term = f.events.indexOf("SIGTERM");
  const kill = f.events.indexOf("SIGKILL");
  assert.ok(term >= 0 && kill >= 0 && term < kill);
  await f.removed();
});

for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  test(`handled ${signal} cancels active pass and awaits close`, async () => {
    const f = await fixture({ hang: true });
    const pass = f.factory.run(request(), policy);
    await f.running;
    f.controller.abort(new Error(signal));
    await assert.rejects(pass);
    await f.factory.close();
    await f.removed();
  });
}

test("abort during startup and late readiness never recreates root", async () => {
  let resolve!: () => void;
  const late = new Promise<void>((done) => { resolve = done; });
  const f = await fixture({ startup: () => late });
  const pass = f.factory.run(request(), policy);
  await f.ready;
  f.controller.abort();
  await assert.rejects(pass);
  await f.removed();
  resolve();
  await new Promise<void>((done) => setImmediate(done));
  await f.removed();
  assert.equal(f.events.filter((event) => event === "spawn").length, 1);
});

test("transport disposal failure still exits child before removing root", async () => {
  const f = await fixture({ disposalError: true });
  await assert.rejects(f.factory.run(request(), policy));
  assert.ok(f.events.indexOf("dispose-error") < f.events.indexOf("exit"));
  await assert.rejects(f.factory.close(), /cleanup failed/);
  assert.ok(f.events.includes("exit"));
  await f.rootGone();
});

test("startup timeout owns late readiness and awaits removal", async () => {
  let resolve: (() => void) | undefined;
  const f = await fixture({ startup: () => new Promise<void>((done) => { resolve = done; }) });
  await assert.rejects(f.factory.run(request(undefined, 30), policy), /exceeded 30ms/);
  // The startup deadline may expire during filesystem provisioning, before spawn.
  if (f.events.includes("spawn")) await f.removed();
  else {
    assert.deepEqual(f.events, []);
    await f.factory.close();
    await assert.rejects(f.factory.run(request(), policy));
  }
  resolve?.();
  await new Promise<void>((done) => setImmediate(done));
  if (f.events.includes("spawn")) await f.rootGone();
});

test("explicit close cancels startup; request signal cancels active pass", async () => {
  const starting = await fixture({ startup: () => new Promise<void>(() => {}) });
  const pass = starting.factory.run(request(), policy);
  const rejected = assert.rejects(pass);
  await starting.ready;
  await starting.factory.close();
  await rejected;
  await starting.removed();
  assert.equal(starting.events.filter((event) => event === "SIGTERM").length, 1);
  assert.equal(starting.events.filter((event) => event === "exit").length, 1);

  const active = await fixture({ hang: true });
  const controller = new AbortController();
  const running = active.factory.run(request(controller.signal), policy);
  const stopped = assert.rejects(running);
  await active.running;
  controller.abort();
  await stopped;
  await active.removed();
});

test("keeps the existing advice limit: response larger than 64 KiB is accepted", async () => {
  const text = "x".repeat(96 * 1024);
  const f = await fixture({ text });
  assert.equal((await f.factory.run(request(), policy)).text, text);
  await f.removed();
});

test("close before admission creates nothing and pre-aborted caller is refused", async () => {
  const controller = new AbortController();
  controller.abort();
  let spawns = 0;
  const f = makePrDescriptionEvalCodexFactory({
    checkoutPath: checkout, apiKey: key, signal: controller.signal,
    testSeams: { spawn() { spawns++; throw new Error("must not spawn"); } },
  });
  await f.close(); await f.close();
  await assert.rejects(f.run(request(), policy));
  assert.equal(spawns, 0);
});
