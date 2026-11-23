import assert from "node:assert/strict";
import { test } from "node:test";
import { promises as fs } from "node:fs";
import path from "node:path";
import os from "node:os";
import { execFileSync } from "node:child_process";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { createInterface } from "node:readline";
import { mkdirSync, writeFileSync, rmSync } from "node:fs";
import { launchCodexRoot, CODEX_BIN, SUPERVISOR_BIN, type SupervisorProcess } from "../src/codex/launcher.js";
import { createCodexTransport } from "../src/codex/transport.js";
import { createCodexAppServerAuth } from "../src/codex/appserver-auth.js";
import { GitCache, RECOVERY_BUNDLE_REF } from "../src/git.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { CodexSessionStore, seedCodexSessionArtifacts } from "../src/codex/session-state.js";

// This is a path-selection proof for a controlled, non-echoing transcript.
// It is deliberately NOT a general secret-content scan. The separate Git bundle
// test below exercises repository recovery, not CODEX_HOME packaging.
test("provider auth-state canaries stay outside persisted and per-root session subsets", async (t) => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "provider-session-canary-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const home = path.join(root, "source");
  const store = path.join(root, "store");
  const staging = path.join(root, "epoch.session-seed");
  const finalHome = path.join(root, "epoch", "codex");
  const rollout = "sessions/2026/rollout-safe.jsonl";
  const transcript = '{"type":"session_meta","payload":{"id":"controlled-session"}}\n';
  const canaries = ["auth", "config", "cache", "extra-ca"].map(kind => ["canary", kind, "distinct-private-value"].join("-"));
  for (const [index, rel] of ["auth.json", "config.toml", "cache/state.json", "provider-extra-ca.crt"].entries()) {
    const file = path.join(home, rel);
    await fs.mkdir(path.dirname(file), { recursive: true });
    await fs.writeFile(file, canaries[index]!);
  }
  await fs.mkdir(path.dirname(path.join(home, rollout)), { recursive: true });
  await fs.writeFile(path.join(home, rollout), transcript);
  // Denied names inside sessions must also be excluded despite the jsonl suffix.
  await fs.writeFile(path.join(home, "sessions/auth-token.jsonl"), canaries[0]!);
  await fs.writeFile(path.join(home, "sessions/credential-config.jsonl"), canaries[1]!);
  await fs.writeFile(path.join(home, "sessions/cache.jsonl"), canaries[2]!);
  const persisted = await CodexSessionStore.persist(home, store);
  assert.equal(persisted.files, 1);
  const generation = (await fs.readFile(path.join(store, "current"), "utf8")).trim();
  assert.match(generation, /^g[0-9a-f]{24}$/);
  const storedHome = path.join(store, "generations", generation);
  assert.equal((await CodexSessionStore.adopt(store, staging, { destination: "runner-seed" })).files, 1);
  assert.equal((await seedCodexSessionArtifacts(path.join(staging, "sessions"), path.join(finalHome, "sessions"))).files, 1);
  for (const subset of [storedHome, staging, finalHome]) {
    const files = (await fs.readdir(subset, { recursive: true, withFileTypes: true }))
      .filter(entry => entry.isFile())
      .map(entry => path.relative(subset, path.join(entry.parentPath, entry.name))).sort();
    assert.deepEqual(files, [rollout], "the complete subset contains only the allowed session");
    const content = await fs.readFile(path.join(subset, rollout), "utf8");
    assert.equal(content, transcript);
    for (const canary of canaries) assert.equal(content.includes(canary), false);
    for (const rel of ["auth.json", "config.toml", "cache/state.json", "provider-extra-ca.crt"]) {
      await assert.rejects(fs.stat(path.join(subset, rel)), { code: "ENOENT" });
    }
  }
  // Source presence is a positive control: absence downstream cannot be explained
  // by a fixture that never planted auth/config/cache/CA state.
  for (const [index, rel] of ["auth.json", "config.toml", "cache/state.json", "provider-extra-ca.crt"].entries()) {
    assert.equal(await fs.readFile(path.join(home, rel), "utf8"), canaries[index]);
  }
});

test("provider auth-state outside repository is absent from actual Git recovery bundle objects", async (t) => {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const env: NodeJS.ProcessEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
  Object.assign(env, { GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" });
  const git = (cwd: string, ...args: string[]) => execFileSync("git", ["-C", cwd, ...args], { env, encoding: "utf8", stdio: "pipe" }).trim();
  const authHome = path.join(fx.dataDir, "agent-home", "run", "codex-data", "namespace-epoch-0", "codex");
  assert.equal(path.relative(fx.originPath, authHome).startsWith(".." + path.sep), true, "provider home is outside the repository");
  const canaries = ["auth", "config", "cache", "extra-ca"].map(kind => ["bundle-private", kind, "canary"].join("-"));
  for (const [index, rel] of ["auth.json", "config.toml", "cache/state.json", "provider-extra-ca.crt"].entries()) {
    const file = path.join(authHome, rel);
    await fs.mkdir(path.dirname(file), { recursive: true });
    await fs.writeFile(file, canaries[index]!);
  }
  // Independent session-store positive control: Git recovery packages repository
  // objects only; this nonempty persisted JSONL is NOT part of the bundle.
  const rollout = "sessions/rollout-controlled.jsonl";
  const transcript = '{"type":"session_meta","payload":{"id":"bundle-control-session"}}\n';
  await fs.mkdir(path.dirname(path.join(authHome, rollout)), { recursive: true });
  await fs.writeFile(path.join(authHome, rollout), transcript);
  const store = path.join(fx.dataDir, "codex-session-store");
  assert.equal((await CodexSessionStore.persist(authHome, store)).files, 1);
  const generation = (await fs.readFile(path.join(store, "current"), "utf8")).trim();
  assert.match(generation, /^g[0-9a-f]{24}$/);
  assert.deepEqual(await fs.readFile(path.join(store, "generations", generation, rollout)), Buffer.from(transcript));
  const bare = path.join(fx.dataDir, "worker.git");
  git(fx.dataDir, "clone", "--bare", fx.originPath, bare);
  const sourceSha = git(bare, "rev-parse", "HEAD");
  const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bundle = await cache.produceRecoveryBundle(bare, { sourceSha, outPath: path.join(fx.dataDir, "recovery.bundle") });
  assert.equal(bundle.selfContained, true);
  const imported = path.join(fx.dataDir, "imported.git");
  git(fx.dataDir, "init", "--bare", imported);
  git(imported, "fetch", bundle.bundlePath, RECOVERY_BUNDLE_REF + ":refs/heads/recovered");
  assert.equal(git(imported, "rev-parse", "recovered"), sourceSha);
  assert.equal(git(imported, "ls-tree", "-r", "--name-only", "recovered"), "README.md");
  assert.equal(git(imported, "show", "recovered:README.md"), "# fixture");
  // Inspect all imported objects, not a substring search on compressed bundle bytes.
  const objects = git(imported, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)").split("\n");
  assert.ok(objects.length >= 3);
  for (const oid of objects) {
    assert.match(oid, /^[0-9a-f]{40,64}$/);
    const content = git(imported, "cat-file", "-p", oid);
    for (const canary of canaries) assert.equal(content.includes(canary), false, "private canary absent from every recovery object");
  }
  for (const [index, rel] of ["auth.json", "config.toml", "cache/state.json", "provider-extra-ca.crt"].entries())
    assert.equal(await fs.readFile(path.join(authHome, rel), "utf8"), canaries[index]);
  // No content scanning is asserted: a credential echoed into committed repository
  // content could enter this Git archive. This is not a CODEX_HOME archive.
});

// Real launcher, auth owner, JSONL transport and filesystem. Only privileged tree
// operations and the supervisor/app-server peer are injected. The peer models an
// auth write on login; it makes no assertion about pinned Codex's on-disk format,
// uid isolation, real process drainage or the executor's orchestration.
for (const terminal of ["drained", "unconfirmed"] as const) {
  test(`provider login auth-state cleanup follows launcher disposal: ${terminal}`, async (t) => {
    assert.ok(process.env.TMPDIR && path.isAbsolute(process.env.TMPDIR), "use an absolute scratch TMPDIR");
    const parent = await fs.mkdtemp(path.join(process.env.TMPDIR, "provider-launch-auth-"));
    t.after(() => fs.rm(parent, { recursive: true, force: true }));
    const root = path.join(parent, "epoch");
    const home = path.join(root, "codex");
    const binding = {
      provider_id: "canary-provider", revision: 1, engine: "codex" as const,
      protocol: "openai_responses" as const, base_url: "https://gateway.example/v1",
      auth_style: "bearer" as const, credential: "distinct-private-auth-canary",
      model_map: { default: { model: "gateway-model", context_window: 64000 } },
    };
    const peer = new EventEmitter();
    const stdin = new PassThrough();
    const stdout = new PassThrough();
    const stderr = new PassThrough();
    const control = new PassThrough();
    const evidence = new PassThrough();
    const streams = [stdin, stdout, stderr, control, evidence];
    t.after(() => { for (const stream of streams) stream.destroy(); });
    const send = (value: unknown) => stdout.write(JSON.stringify(value) + "\n");
    const lifecycle = (value: unknown) => evidence.write(JSON.stringify(value) + "\n");
    const methods: string[] = [];
    const rpc = createInterface({ input: stdin });
    const commands = createInterface({ input: control });
    t.after(() => { rpc.close(); commands.close(); });
    rpc.on("line", line => {
      const frame = JSON.parse(line);
      methods.push(frame.method);
      if (frame.method === "initialize") {
        send({ id: frame.id, result: {
          userAgent: "simulated-app-server", codexHome: home,
          platformFamily: "simulated", platformOs: "simulated",
        } });
      } else if (frame.method === "account/login/start") {
        assert.deepEqual(frame.params, { type: "apiKey", apiKey: binding.credential });
        // Model the peer persisting the credential only after it receives login.
        writeFileSync(path.join(home, "auth.json"), frame.params.apiKey);
        send({ id: frame.id, result: { type: "apiKey" } });
      } else assert.equal(frame.method, "initialized");
    });
    let disposalRequests = 0;
    commands.on("line", line => {
      const frame = JSON.parse(line);
      assert.equal(frame.op, "dispose");
      disposalRequests++;
      lifecycle({
        event: "dispose", id: frame.id, state: terminal,
        ...(terminal === "drained" ? { authority: "ECHILD+__WALL" } : { reason: "deadline" }),
        killed: [], reaped: [],
      });
      if (terminal === "drained") {
        setImmediate(() => {
          peer.emit("exit", 0, null);
          stdout.end(); stderr.end(); evidence.end();
          setImmediate(() => peer.emit("close", 0, null));
        });
      }
    });
    let removals = 0;
    const handle = await launchCodexRoot({
      ownedDataRoot: root, providerBinding: binding, model: "default",
      codexBin: CODEX_BIN, supervisorBin: SUPERVISOR_BIN,
      kind: "provider", childArgv: ["app-server"], cwd: path.join(parent, "work"),
      useAppServerAuth: true, authMode: "provider",
    }, {
      env: { UZI_UID_SPLIT: "1" }, resolveRunnerUid: () => 10002,
      rootStartTime: () => 1, assertNoUnexpectedSystemConfig: () => {},
      makeRunnerTrees: request => {
        assert.equal(request.root, root);
        assert.equal(request.sharedSessionRead?.codexHome, home);
        for (const dir of request.dirs) mkdirSync(dir, { recursive: true });
        for (const file of request.files) writeFileSync(file.path, file.content, { mode: file.mode });
      },
      removeRunnerTree: request => {
        assert.equal(disposalRequests, 1, "removal follows supervisor disposal");
        assert.equal(request.root, root);
        removals++;
        rmSync(request.root, { recursive: true });
      },
      spawnSupervisor: (_command, _args, options) => {
        assert.equal(JSON.stringify(options.env).includes(binding.credential), false);
        lifecycle({
          event: "started", supervisorPid: 4242, childPid: 4243,
          subreaper: true, nondumpable: true, uid: 10002,
          liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true,
        });
        return Object.assign(peer, { pid: 4242, stdin, stdout, stderr, stdio: streams }) as unknown as SupervisorProcess;
      },
    });
    const transport = createCodexTransport({ inbound: handle.transport.stdout!, outbound: handle.transport.stdin! });
    t.after(() => transport.close());
    const auth = createCodexAppServerAuth({ mode: "provider", apiKey: binding.credential });
    let outcome: Awaited<ReturnType<typeof handle.dispose>> | undefined;
    try {
      await assert.rejects(fs.stat(path.join(home, "auth.json")), { code: "ENOENT" });
      await auth.authenticate(transport);
      assert.deepEqual(methods, ["initialize", "initialized", "account/login/start"]);
      assert.equal(await fs.readFile(path.join(home, "auth.json"), "utf8"), binding.credential);
      const config = await fs.readFile(path.join(home, "config.toml"), "utf8");
      assert.match(config, /requires_openai_auth = true/);
      assert.equal(config.includes(binding.credential), false);
      await fs.mkdir(path.join(home, "cache"));
      await fs.writeFile(path.join(home, "cache/state.json"), "distinct-private-cache-canary");
      await fs.mkdir(path.join(home, "sessions"), { recursive: true });
      const transcript = '{"type":"session_meta","payload":{"id":"safe-session"}}\n';
      await fs.writeFile(path.join(home, "sessions/rollout-safe.jsonl"), transcript);
      const store = path.join(parent, "store");
      const staging = path.join(parent, "staging");
      assert.equal((await CodexSessionStore.persist(home, store)).files, 1);
      assert.equal((await CodexSessionStore.adopt(store, staging, { destination: "runner-seed" })).files, 1);
      const names = (await fs.readdir(staging, { recursive: true, withFileTypes: true }))
        .filter(entry => entry.isFile())
        .map(entry => path.relative(staging, path.join(entry.parentPath, entry.name)));
      assert.deepEqual(names, ["sessions/rollout-safe.jsonl"]);
      assert.equal(await fs.readFile(path.join(staging, names[0]!), "utf8"), transcript);
    } finally {
      auth.closeAdmissionAndCancel();
      await transport.close();
      outcome = await handle.dispose(1000);
      await auth.drainInterceptedRequests();
    }
    assert.equal(outcome.clean, terminal === "drained", JSON.stringify(outcome));
    assert.equal(disposalRequests, 1);
    assert.equal(removals, terminal === "drained" ? 1 : 0);
    if (terminal === "drained") {
      await assert.rejects(fs.stat(root), { code: "ENOENT" });
      assert.equal((await handle.dispose(1000)).clean, true);
      assert.equal(removals, 1, "repeated disposal does not remove twice");
    } else {
      assert.equal(await fs.readFile(path.join(home, "auth.json"), "utf8"), binding.credential);
      assert.ok(await fs.stat(path.join(home, "config.toml")));
      assert.equal(await fs.readFile(path.join(home, "cache/state.json"), "utf8"), "distinct-private-cache-canary");
    }
    assert.equal(await fs.readFile(path.join(parent, "staging/sessions/rollout-safe.jsonl"), "utf8"),
      '{"type":"session_meta","payload":{"id":"safe-session"}}\n', "safe session survives root disposal");
  });
}

test("allowed transcripts retain echoed private content: no general content-scan guarantee", async (t) => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "provider-session-echo-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const home = path.join(root, "source");
  const store = path.join(root, "store");
  const finalHome = path.join(root, "runner");
  const echoed = ["canary", "explicit-transcript-echo", "private-value"].join("-");
  const transcript = JSON.stringify({ type: "message", text: echoed }) + "\n";
  await fs.mkdir(path.join(home, "sessions"), { recursive: true });
  await fs.writeFile(path.join(home, "sessions/rollout-echo.jsonl"), transcript);
  assert.equal((await CodexSessionStore.persist(home, store)).files, 1);
  assert.equal((await CodexSessionStore.adopt(store, finalHome, { destination: "runner-seed" })).files, 1);
  assert.equal(await fs.readFile(path.join(finalHome, "sessions/rollout-echo.jsonl"), "utf8"), transcript);
});
