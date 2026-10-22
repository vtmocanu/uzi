import test, { describe } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHash } from "node:crypto";
import { execFileSync, spawn } from "node:child_process";
import { GitCache, WIP_PARK_COMMIT_PREFIX } from "../src/git.js";
import { MaintenanceResources, RecoveryMaintenance } from "../src/recovery-maintenance.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
import { nullLogger } from "./helpers.js";

type Step = "status" | "add" | "commit";
const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
const gitEnv = { ...cleanEnv, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
async function fixture(t: test.TestContext, monotonicNow?: () => number) {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/wip-capture-"));
  const root = path.join(dir, "source");
  await fs.mkdir(root);
  const gitIn = (...args: string[]) => execFileSync("/usr/bin/git", ["-C", root, ...args],
    { env: gitEnv, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
  gitIn("init");
  for (const [key, value] of [["user.name", "fixture"], ["user.email", "fixture@example.com"],
    ["commit.gpgsign", "false"], ["maintenance.auto", "false"], ["gc.auto", "0"], ["core.fsmonitor", "false"]])
    gitIn("config", key!, value!);
  await fs.writeFile(path.join(root, "tracked"), "original");
  await fs.writeFile(path.join(root, ".gitignore"), "ignored\n");
  gitIn("add", "-A"); gitIn("commit", "-m", "base");
  const old = gitIn("rev-parse", "HEAD").trim();
  const initialIndex = await fs.readFile(path.join(root, ".git/index"));
  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "fixture", monotonicNow });
  const admitted = await ledger.admit({
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  });
  assert.ok(admitted.ok, JSON.stringify(admitted));
  const git = new GitCache(dir, nullLogger());
  const signal = new AbortController();
  const events: string[] = [];
  let launches = 0, joins = 0;
  let intercept: ((request: BoundaryProcessRequest) => string | undefined) | undefined;
  const owner = async (request: BoundaryProcessRequest) => {
    launches++;
    const argv = [...request.argv];
    const command = argv.find(arg => ["status", "add", "commit", "config", "ls-files", "rev-parse", "cat-file", "ls-tree"].includes(arg));
    const label = command ?? (argv[2]?.includes("entryLimit") ? "proof" : "metadata");
    events.push("launch:" + label);
    if (MaintenanceResources.active()) {
      if (command === "status") assert.equal(request.env.GIT_OPTIONAL_LOCKS, "0");
      if (command) assert.equal(request.env.GIT_NO_REPLACE_OBJECTS, "1");
    }
    const replacement = intercept?.(request);
    const child = replacement === undefined
      ? spawn(argv[0]!, argv.slice(1), { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] })
      : spawn(process.execPath, ["-e", replacement], { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", code => { joins++; events.push("join:" + label); resolve({ code: code ?? 1 }); });
    });
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
      cancel: async () => { child.kill("SIGTERM"); await completed; } };
  };
  const before = async (step: Step, phase: "prepare" | "dispatch") => { events.push("before:" + step + ":" + phase); };
  const after = async (step: Step) => { events.push("after:" + step); assert.equal(launches, joins); };
  const run = <T>(action: () => T) => MaintenanceResources.run(admitted.value, action);
  const owned = <T>(action: () => Promise<T>) => git.withBoundaryProcessSpawner(owner, signal.signal, action);
  const capture = (callbacks = { beforeStep: before, afterRunnerGit: after }, source = root, head = old) =>
    run(() => owned(() => git.captureMaintenanceWip(source, head, callbacks)));
  t.after(async () => {
    assert.equal(launches, joins);
    await ledger.finish(admitted.value, "transient");
    await fs.rm(dir, { recursive: true, force: true });
  });
  return { root, dir, git, old, initialIndex, gitIn, run, owned, capture, events, before, after, signal,
    launches: () => launches, intercept: (fn: typeof intercept) => { intercept = fn; },
    dirty: () => fs.writeFile(path.join(root, "tracked"), "changed"),
    head: () => gitIn("rev-parse", "HEAD").trim(),
    index: () => fs.readFile(path.join(root, ".git/index")),
  };
}

describe("guarded maintenance WIP capture", { skip: process.platform !== "linux" && "Linux /proc capture required" }, () => {
  test("dirty capture uses three matched 16 MiB raw metadata envelopes without reducing source caps", async t => {
    const f = await fixture(t);
    await f.dirty();
    let proofs = 0;
    f.intercept(request => {
      if (request.argv[2]?.includes("entryLimit")) {
        proofs++;
        assert.deepEqual(request.argv.slice(4, 7), [String(64 * 1024 ** 2), "16384", String(16 * 1024 ** 2)]);
      }
      return undefined;
    });
    f.run(() => MaintenanceResources.consume("metadataBytes", 196 * 1024 ** 2));
    assert.equal((await f.capture()).kind, "committed");
    assert.equal(proofs, 3);
  });

  test("real tracked/untracked/binary WIP, ignored bytes and ordered owned effects", async t => {
    const f = await fixture(t);
    await f.dirty();
    const binary = Buffer.from([0, 255, 1, 0]);
    await fs.writeFile(path.join(f.root, "binary"), binary);
    await fs.writeFile(path.join(f.root, "untracked"), "new");
    await fs.writeFile(path.join(f.root, "ignored"), "private");
    const result = await f.capture();
    assert.equal(result.kind, "committed");
    assert.equal(result.parent, f.old);
    assert.match(result.head, /^[a-f0-9]{40}$/);
    assert.notEqual(result.head, f.old);
    assert.equal(f.gitIn("show", "HEAD:tracked"), "changed");
    assert.equal(f.gitIn("show", "HEAD:untracked"), "new");
    assert.deepEqual(execFileSync("/usr/bin/git", ["-C", f.root, "show", "HEAD:binary"], { env: gitEnv }), binary);
    assert.equal(f.gitIn("ls-files", "ignored"), "");
    assert.equal(await fs.readFile(path.join(f.root, "ignored"), "utf8"), "private");
    assert.equal(f.gitIn("show", "-s", "--format=%P"), f.old + "\n");
    assert.ok(f.gitIn("show", "-s", "--format=%s").startsWith(WIP_PARK_COMMIT_PREFIX));
    const effects = f.events.filter(event => /^(before|after):|^(launch|join):(status|add|commit)$/.test(event));
    assert.deepEqual(effects, ["status", "add", "commit"].flatMap(step =>
      ["before:" + step + ":prepare", "before:" + step + ":dispatch", "launch:" + step, "join:" + step, "after:" + step]));
    assert.equal(f.events.filter(e => e === "launch:proof").length, 3);
    for (const step of ["status", "add", "commit"]) {
      const start = f.events.indexOf("before:" + step + ":prepare");
      const end = f.events.indexOf("before:" + step + ":dispatch", start + 1);
      const preparation = f.events.slice(start + 1, end);
      assert.ok(preparation.includes("join:metadata"));
      assert.ok(preparation.includes("join:config"));
      assert.equal(preparation.filter(e => e === "join:ls-files").length, 2);
      assert.equal(preparation.at(-1), "join:proof");
    }
    // The three raw reservations consume 192 MiB; no refund after tiny proofs.
    f.run(() => MaintenanceResources.consume("sourceBytes", 64 * 1024 ** 2));
    assert.throws(() => f.run(() => MaintenanceResources.consume("sourceBytes", 1)), /maintenance_/);
  });

  test("caller quiescence is awaited before any next preparation", async t => {
    const f = await fixture(t); await f.dirty();
    let reached!: () => void, release!: () => void;
    const reachedPromise = new Promise<void>(resolve => { reached = resolve; });
    const releasePromise = new Promise<void>(resolve => { release = resolve; });
    const result = f.capture({ beforeStep: f.before, afterRunnerGit: async step => {
      await f.after(step);
      if (step === "status") { reached(); await releasePromise; }
    } });
    try {
      await reachedPromise;
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(f.events.at(-1), "after:status");
      assert.deepEqual(await f.index(), f.initialIndex);
    } finally { release(); await result; }
    assert.equal((await result).kind, "committed");
  });

  for (const step of ["status", "add"] as const) test("fresh config refusal after " + step, async t => {
    const f = await fixture(t); await f.dirty();
    await assert.rejects(f.capture({ beforeStep: f.before, afterRunnerGit: async current => {
      await f.after(current);
      if (current === step) f.gitIn("config", "filter.plant.process", "touch marker");
    } }), /refused/);
    assert.ok(f.events.includes("after:" + step));
    assert.ok(!f.events.includes("launch:" + (step === "status" ? "add" : "commit")));
    assert.equal(f.head(), f.old);
  });

  for (const corrupt of ["parent", "subject", "index"] as const) test("postcommit " + corrupt + " mismatch throws and keeps new HEAD", async t => {
    const f = await fixture(t); await f.dirty();
    await assert.rejects(f.capture({ beforeStep: f.before, afterRunnerGit: async step => {
      await f.after(step);
      if (step !== "commit") return;
      if (corrupt === "index") f.gitIn("update-index", "--force-remove", "tracked");
      else {
        const actual = f.gitIn("--no-replace-objects", "cat-file", "commit", f.head());
        const changed = corrupt === "parent" ? actual.replace("parent " + f.old, "parent " + "b".repeat(40))
          : actual.slice(0, actual.indexOf("\n\n") + 2) + "wrong subject\n";
        f.intercept(request => request.argv.includes("cat-file") ? "process.stdout.write(" + JSON.stringify(changed) + ")" : undefined);
      }
    } }), /refused/);
    assert.notEqual(f.head(), f.old);
    assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "changed");
  });

  for (const visible of [false, true]) test("cached index stat cannot hide changed tracked bytes" + (visible ? " with visible WIP" : ""), async t => {
    const f = await fixture(t);
    await fs.writeFile(path.join(f.root, "tracked"), "modified");
    if (visible) await fs.writeFile(path.join(f.root, "visible"), "new");
    const indexPath = path.join(f.root, ".git/index");
    const index = await f.index();
    assert.equal(index.readUInt32BE(4), 2);
    const st = await fs.stat(path.join(f.root, "tracked"), { bigint: true });
    let offset = 12;
    let found = false;
    for (let i = 0; i < index.readUInt32BE(8); i++) {
      const end = index.indexOf(0, offset + 62);
      assert.ok(end >= offset + 62);
      if (index.toString("utf8", offset + 62, end) === "tracked") {
        // Issue #3222: forge every cached stat field except the mode word, which keeps the
        // Git-valid mode `git add` wrote. A raw st.mode is 100600 under umask 077, and the
        // capture would then refuse at its mode guard before reaching the stat-cache check.
        const fields = [st.ctimeNs / 1000000000n, st.ctimeNs % 1000000000n,
          st.mtimeNs / 1000000000n, st.mtimeNs % 1000000000n,
          st.dev, st.ino, undefined, st.uid, st.gid, st.size];
        fields.forEach((value, field) => {
          if (value !== undefined) index.writeUInt32BE(Number(BigInt.asUintN(32, value)), offset + field * 4);
        });
        assert.equal(index.readUInt16BE(offset + 60), 7); // Ordinary path flags, no hiding bits.
        found = true;
      }
      offset += Math.ceil((end + 1 - offset) / 8) * 8;
    }
    assert.ok(found);
    createHash("sha1").update(index.subarray(0, -20)).digest().copy(index, index.length - 20);
    await fs.writeFile(indexPath, index);
    // Newer than the file's mtime, without a real-time wait or racy-cache fallback.
    const newer = Number(st.mtimeNs / 1000000000n) + 10;
    await fs.utimes(indexPath, newer, newer);
    assert.equal(f.gitIn("--no-optional-locks", "status", "--porcelain=v1"), visible ? "?? visible\n" : "");
    assert.equal(f.gitIn("show", ":tracked"), "original");
    const before = await f.index();
    let result: Awaited<ReturnType<typeof f.capture>> | undefined;
    try { result = await f.capture(); }
    catch (error) { assert.match(String(error), /refused/); }
    assert.notEqual(result?.kind, "clean", "forged index stat cache must not hide changed tracked bytes");
    // Issue #3222: an earlier refusal (e.g. at the index-mode guard) must not pass for this check.
    assert.ok(f.events.includes("launch:status"), "the capture reaches the stat-cache check");
    if (visible) assert.ok(f.events.includes("launch:add"), "visible WIP is staged before the refusal");
    if (result?.kind === "committed") {
      assert.equal(f.gitIn("show", "HEAD:tracked"), "modified", "successful tree must include masked tracked bytes");
      assert.equal(f.gitIn("show", "HEAD:visible"), "new");
    } else {
      assert.equal(f.head(), f.old);
      if (visible) {
        assert.equal(f.gitIn("show", ":tracked"), "original");
        assert.equal(f.gitIn("show", ":visible"), "new");
      } else assert.deepEqual(await f.index(), before);
      assert.ok(!f.events.includes("launch:commit"));
    }
    assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "modified");
  });

  test("an unsupported tracked index mode is refused before add, index and HEAD unchanged", async t => {
    const f = await fixture(t);
    await fs.writeFile(path.join(f.root, "tracked"), "modified");
    await fs.writeFile(path.join(f.root, "visible"), "new");
    const indexPath = path.join(f.root, ".git/index");
    const index = await f.index();
    let offset = 12;
    let found = false;
    for (let i = 0; i < index.readUInt32BE(8); i++) {
      const end = index.indexOf(0, offset + 62);
      assert.ok(end >= offset + 62);
      if (index.toString("utf8", offset + 62, end) === "tracked") {
        index.writeUInt32BE(0o100600, offset + 24); // The mode word; Git writes only 100644/100755.
        found = true;
      }
      offset += Math.ceil((end + 1 - offset) / 8) * 8;
    }
    assert.ok(found);
    createHash("sha1").update(index.subarray(0, -20)).digest().copy(index, index.length - 20);
    await fs.writeFile(indexPath, index);
    assert.match(f.gitIn("ls-files", "--stage", "tracked"), /^100600 /);
    const before = await f.index();
    await assert.rejects(f.capture(), /refused/);
    assert.ok(!f.events.includes("launch:add"), "refused before add");
    assert.ok(!f.events.includes("launch:commit"));
    assert.equal(f.head(), f.old);
    assert.deepEqual(await f.index(), before);
    assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "modified");
    assert.equal(await fs.readFile(path.join(f.root, "visible"), "utf8"), "new");
  });

  // issue #3239: the maintenance marker refuses the same files the park auto-save omits.
  const CAP = 8 * 1024 * 1024;
  for (const [name, file, bytes] of [["an untracked file over 8 MiB", "big.bin", CAP + 1],
    ["a core dump", "core.4242", 16], ["a nested bare core file", "sub/core", 16]] as const) {
    test("refuses " + name + ": no commit, HEAD unchanged, file kept (#3239)", async t => {
      const f = await fixture(t);
      await f.dirty();
      await fs.mkdir(path.join(f.root, path.dirname(file)), { recursive: true });
      await fs.writeFile(path.join(f.root, file), Buffer.alloc(bytes, 0x61));
      await assert.rejects(f.capture(), /maintenance_wip_capture_refused/);
      assert.ok(!f.events.includes("launch:commit"), "refused before commit");
      assert.equal(f.head(), f.old);
      assert.equal((await fs.stat(path.join(f.root, file))).size, bytes);
      assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "changed");
    });
  }

  test("an oversized TRACKED modification is refused; a file of exactly 8 MiB commits (#3239)", async t => {
    const f = await fixture(t);
    await fs.writeFile(path.join(f.root, "tracked"), Buffer.alloc(CAP + 1, 0x62));
    await assert.rejects(f.capture(), /maintenance_wip_capture_refused/);
    assert.ok(!f.events.includes("launch:commit"), "refused before commit");
    assert.equal(f.head(), f.old);
    assert.equal((await fs.stat(path.join(f.root, "tracked"))).size, CAP + 1);
    const g = await fixture(t);
    await g.dirty();
    await fs.writeFile(path.join(g.root, "exact.bin"), Buffer.alloc(CAP, 0x61));
    await fs.writeFile(path.join(g.root, "score.1"), "not a core file");
    const result = await g.capture();
    assert.equal(result.kind, "committed");
    assert.equal(g.gitIn("cat-file", "-s", "HEAD:exact.bin").trim(), String(CAP));
    assert.equal(g.gitIn("show", "HEAD:score.1"), "not a core file");
  });

  test("a core file and an oversized blob already in HEAD do not block an unrelated capture (#3239)", async t => {
    const f = await fixture(t);
    await fs.writeFile(path.join(f.root, "core"), "committed earlier");
    await fs.writeFile(path.join(f.root, "huge.bin"), Buffer.alloc(CAP + 1, 0x63));
    f.gitIn("add", "-A"); f.gitIn("commit", "-m", "history already holds them");
    const base = f.head();
    await f.dirty();
    const result = await f.capture(undefined, f.root, base);
    assert.equal(result.kind, "committed");
    assert.equal(result.parent, base);
    assert.equal(f.gitIn("show", "HEAD:tracked"), "changed");
  });

  test("clean is explicit, HEAD/index unchanged and only status attempted", async t => {
    const f = await fixture(t);
    assert.deepEqual(await f.capture(), { kind: "clean", head: f.old, parent: null });
    assert.equal(f.head(), f.old);
    assert.deepEqual(await f.index(), f.initialIndex);
    assert.deepEqual(f.events.filter(e => e.startsWith("after:")), ["after:status"]);
  });

  for (const stop of ["status", "add", "commit"] as const) {
    for (const fresh of [false, true]) test("eligibility refusal " + stop + (fresh ? " at dispatch" : " before preparation"), async t => {
      const f = await fixture(t); await f.dirty();
      let visits = 0;
      await assert.rejects(f.capture({
        beforeStep: async (step, phase) => { await f.before(step, phase); if (step === stop && ++visits === (fresh ? 2 : 1)) throw Error("stopped"); },
        afterRunnerGit: f.after,
      }), /stopped/);
      assert.equal(f.head(), f.old);
      assert.ok(!f.events.includes("launch:" + stop));
      if (stop !== "commit") assert.deepEqual(await f.index(), f.initialIndex);
      else assert.equal(f.gitIn("show", ":tracked"), "changed");
      if (stop === "status" && !fresh) assert.equal(f.launches(), 0);
    });

    test("command failure " + stop + " still joins caller quiescence and throws", async t => {
      const f = await fixture(t); await f.dirty();
      f.intercept(request => request.argv.includes(stop) ? "process.exitCode = 7" : undefined);
      await assert.rejects(f.capture(), /failed/);
      assert.equal(f.head(), f.old);
      assert.equal(f.events.at(-1), "after:" + stop);
      assert.ok(!f.events.includes("launch:" + (stop === "status" ? "add" : stop === "add" ? "commit" : "unused")));
    });

    test("post-command refusal " + stop + " retains mutation/evidence", async t => {
      const f = await fixture(t); await f.dirty();
      await assert.rejects(f.capture({
        beforeStep: f.before, afterRunnerGit: async step => { await f.after(step); if (step === stop) throw Error("quiescence"); },
      }), /quiescence/);
      assert.equal(f.events.at(-1), "after:" + stop);
      assert.equal(f.head() === f.old, stop !== "commit");
      if (stop === "status") assert.deepEqual(await f.index(), f.initialIndex);
      else assert.equal(f.gitIn("show", ":tracked"), "changed");
    });
  }

  for (const step of ["status", "add"] as const) test("content race after " + step + " refuses next effect", async t => {
    const f = await fixture(t); await f.dirty();
    await assert.rejects(f.capture({ beforeStep: f.before, afterRunnerGit: async current => {
      await f.after(current);
      if (current === step) await fs.writeFile(path.join(f.root, "tracked"), "raced!!");
    } }), /refused/);
    assert.equal(f.head(), f.old);
    assert.ok(f.events.includes("after:" + step));
    assert.ok(!f.events.includes("launch:" + (step === "status" ? "add" : "commit")));
  });

  for (const [key, value] of [
    ["filter.plant.clean", "touch marker"], ["core.worktree", "/tmp"], ["core.attributesFile", "/tmp"],
    ["core.excludesFile", "/tmp"], ["core.sparseCheckout", "true"], ["extensions.worktreeConfig", "true"],
    ["core.splitIndex", "true"], ["core.fsmonitor", "touch marker"], ["core.bare", "true"],
    ["include.path", "../included"], ["unknown.key", "unknown"],
  ]) test("unsupported config " + key + " refuses before status", async t => {
    const f = await fixture(t); await f.dirty(); f.gitIn("config", key!, value!);
    await assert.rejects(f.capture(), /refused/);
    assert.ok(!f.events.includes("launch:status"));
    assert.equal(f.head(), f.old);
    assert.deepEqual(await f.index(), f.initialIndex);
    await assert.rejects(fs.stat(path.join(f.root, "marker")), { code: "ENOENT" });
  });

  test("included filter config is read but refused before any filter executes", async t => {
    const f = await fixture(t); await f.dirty();
    await fs.writeFile(path.join(f.root, ".git/included"), '[filter "plant"]\nclean = touch marker\n');
    f.gitIn("config", "include.path", "included");
    await assert.rejects(f.capture(), /refused/);
    assert.ok(!f.events.includes("launch:status"));
    await assert.rejects(fs.stat(path.join(f.root, "marker")), { code: "ENOENT" });
  });

  test("unmerged index refuses before status", async t => {
    const f = await fixture(t), blob = f.gitIn("rev-parse", "HEAD:tracked").trim();
    execFileSync("/usr/bin/git", ["-C", f.root, "update-index", "--index-info"], {
      env: gitEnv, input: "0 " + "0".repeat(40) + "\ttracked\n100644 " + blob + " 1\ttracked\n",
    });
    await assert.rejects(f.capture(), /refused/);
    assert.ok(!f.events.includes("launch:status"));
  });

  test("split index is unsupported even without core.splitIndex configuration", async t => {
    const f = await fixture(t);
    f.gitIn("update-index", "--split-index");
    await assert.rejects(f.capture(), /refused/);
    assert.ok(!f.events.includes("launch:status"));
  });

  test("source metadata rows use source allowance rather than the 4096 admin cap", async t => {
    const f = await fixture(t);
    f.intercept(request => request.argv.includes("ls-files")
      ? 'process.stdout.write(Array.from({ length: 4100 }, (_, i) => ' +
        (request.argv.includes("--stage") ? '"100644 " + "a".repeat(40) + " 0\\tf"' : '"H f"') +
        ' + i + "\\0").join(""))' : undefined);
    let visits = 0;
    await assert.rejects(f.capture({ beforeStep: async (step, phase) => {
      await f.before(step, phase); if (++visits === 2) throw Error("prepared");
    }, afterRunnerGit: f.after }), /prepared/);
    assert.equal(visits, 2);
    assert.ok(!f.events.includes("launch:status"));
  });

  test("monotonic deadline between proof and dispatch refuses the effect", async t => {
    let clock = 0;
    const f = await fixture(t, () => clock);
    let visits = 0;
    await assert.rejects(f.capture({ beforeStep: async (step, phase) => {
      await f.before(step, phase); if (++visits === 2) clock = 121000;
    }, afterRunnerGit: f.after }), /maintenance_/);
    assert.ok(f.events.includes("join:proof"));
    assert.ok(!f.events.includes("launch:status"));
  });

  test("boundary abort after status blocks add and preserves the index", async t => {
    const f = await fixture(t); await f.dirty();
    await assert.rejects(f.capture({ beforeStep: f.before, afterRunnerGit: async step => {
      await f.after(step); f.signal.abort(Error("stopped"));
    } }), /stopped/);
    assert.deepEqual(await f.index(), f.initialIndex);
    assert.equal(f.head(), f.old);
    assert.ok(!f.events.includes("launch:add"));
  });

  for (const raw of ["partial", "ZZ unknown\0", "R  missing-original\0"]) test("malformed status " + JSON.stringify(raw) + " refuses add", async t => {
    const f = await fixture(t);
    f.intercept(request => request.argv.includes("status") ? "process.stdout.write(" + JSON.stringify(raw) + ")" : undefined);
    await assert.rejects(f.capture(), /refused/);
    assert.ok(f.events.includes("after:status"));
    assert.ok(!f.events.includes("launch:add"));
  });

  test("planted hooks stay inert under the existing trusted hooks pin", async t => {
    const f = await fixture(t); await f.dirty();
    await fs.writeFile(path.join(f.root, ".git/hooks/pre-commit"), "#!/bin/sh\ntouch marker\n", { mode: 0o755 });
    assert.equal((await f.capture()).kind, "committed");
    await assert.rejects(fs.stat(path.join(f.root, "marker")), { code: "ENOENT" });
  });

  for (const flag of ["--assume-unchanged", "--skip-worktree"]) test("index flag " + flag + " refuses before status", async t => {
    const f = await fixture(t); await f.dirty(); f.gitIn("update-index", flag, "tracked");
    await assert.rejects(f.capture(), /refused/);
    assert.ok(!f.events.includes("launch:status"));
    assert.equal(f.head(), f.old);
  });

  test("gitlink refuses before status", async t => {
    const f = await fixture(t);
    f.gitIn("update-index", "--add", "--cacheinfo", "160000", f.old, "submodule");
    await assert.rejects(f.capture(), /refused/);
    assert.ok(!f.events.includes("launch:status"));
  });

  for (const target of ["config", "index"]) test("malformed " + target + " is not clean", async t => {
    const f = await fixture(t); await fs.writeFile(path.join(f.root, ".git", target), "malformed");
    await assert.rejects(f.capture(), /refused/);
    assert.ok(!f.events.includes("launch:status"));
  });

  for (const target of ["config", "ls-files"]) test("partial NUL " + target + " refuses before status", async t => {
    const f = await fixture(t);
    f.intercept(request => request.argv.includes(target) ? 'process.stdout.write("partial")' : undefined);
    await assert.rejects(f.capture(), /refused/);
    assert.ok(!f.events.includes("launch:status"));
  });

  for (const resource of ["adminEntries", "metadataBytes"] as const) {
    test("helper capacity refusal before dispatch: " + resource, async t => {
      const f = await fixture(t);
      f.run(() => MaintenanceResources.consume(resource,
        resource === "adminEntries" ? 4096 - 32 : 256 * 1024 ** 2 - 1024));
      await assert.rejects(f.capture(), /maintenance_resource_exhausted/);
      assert.equal(f.launches(), 0);
      assert.equal(f.head(), f.old);
      assert.deepEqual(await f.index(), f.initialIndex);
    });
  }

  for (const depth of [128, 129]) test("clone path depth boundary " + depth, async t => {
    const f = await fixture(t);
    // Intercept only the first helper: no filesystem at the synthetic path is needed.
    f.intercept(() => "process.exitCode = 7");
    await assert.rejects(f.capture(undefined, "/" + Array(depth).fill("a").join("/")),
      depth === 128 ? /subprocess|failed/ : /refused/);
    assert.equal(f.launches(), depth === 128 ? 1 : 0);
    assert.equal(f.head(), f.old);
    assert.deepEqual(await f.index(), f.initialIndex);
  });

  for (const bytes of [4096, 4097]) test("clone path UTF8 byte boundary " + bytes, async t => {
    const f = await fixture(t);
    const source = "/" + "é".repeat(2047) + (bytes === 4096 ? "a" : "aa");
    assert.equal(Buffer.byteLength(source), bytes);
    f.intercept(() => "process.exitCode = 7");
    await assert.rejects(f.capture(undefined, source), bytes === 4096 ? /subprocess|failed/ : /refused/);
    assert.equal(f.launches(), bytes === 4096 ? 1 : 0);
  });

  test("unsafe source and exhausted quota refuse without a Git effect", async t => {
    const f = await fixture(t);
    await fs.symlink(f.root, path.join(f.dir, "alias"));
    await assert.rejects(f.capture(undefined, path.join(f.dir, "alias")), /refused|subprocess/);
    assert.ok(!f.events.includes("launch:status"));
    f.run(() => MaintenanceResources.consume("sourceBytes", 256 * 1024 ** 2));
    await assert.rejects(f.capture(), /maintenance_/);
    assert.ok(!f.events.includes("launch:status"));
  });

  test("unscoped/discovery/no owner, missing callbacks, invalid arguments and aborted scope refuse", async t => {
    const f = await fixture(t), callbacks = { beforeStep: f.before, afterRunnerGit: f.after };
    await assert.rejects(f.git.captureMaintenanceWip(f.root, f.old, callbacks), /maintenance_/);
    await assert.rejects(MaintenanceResources.runDiscovery(() => f.owned(() =>
      f.git.captureMaintenanceWip(f.root, f.old, callbacks))), /maintenance_/);
    await assert.rejects(f.run(() => f.git.captureMaintenanceWip(f.root, f.old, callbacks)), /maintenance_/);
    await assert.rejects(f.capture(undefined, f.root + "/.."), /refused/);
    await assert.rejects(f.capture(undefined, f.root, "a".repeat(39)), /refused/);
    await assert.rejects(f.capture(undefined, f.root, f.old + "\n"), /refused/);
    await assert.rejects(f.capture({ beforeStep: undefined!, afterRunnerGit: f.after }), /refused/);
    f.signal.abort(Error("deadline"));
    await assert.rejects(f.capture(), /deadline/);
    assert.equal(f.launches(), 0);
  });

  test("manifest visits share the raw reader source-entry reservation", async t => {
    const f = await fixture(t);
    const reader = f.git as unknown as {
      maintenanceWipInput(root: string, limits: object, expected?: string): Promise<{ digest: string; entries: number; agrees: boolean }>;
    };
    const baseline = await f.run(() => f.owned(() => reader.maintenanceWipInput(f.root, {})));
    const manifest = f.gitIn("ls-files", "--stage", "-z");
    const rows = manifest.split("\0").filter(Boolean).length;
    const checked = await f.run(() => f.owned(() => reader.maintenanceWipInput(f.root, {}, manifest)));
    assert.equal(checked.agrees, true);
    assert.equal(checked.digest, baseline.digest);
    assert.equal(checked.entries, baseline.entries + rows);
  });

  test("ordinary WIP remains best effort; active unguarded WIP throws before launching", async t => {
    const f = await fixture(t);
    await assert.rejects(f.run(() => f.owned(() => f.git.commitWipMarker(f.root))), /guard_required/);
    await assert.rejects(MaintenanceResources.runDiscovery(() => f.owned(() => f.git.commitWipMarker(f.root))), /maintenance_/);
    assert.equal(f.launches(), 0);
    assert.equal(await f.owned(() => f.git.commitWipMarker(f.root)), false);
    await f.dirty();
    assert.equal(await f.owned(() => f.git.commitWipMarker(f.root)), true);
    f.intercept(request => request.argv.includes("status") ? "process.exitCode = 7" : undefined);
    assert.equal(await f.owned(() => f.git.commitWipMarker(f.root)), false);
  });
});
