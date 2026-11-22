import assert from "node:assert/strict";
import { test } from "node:test";
import { promises as fs } from "node:fs";
import path from "node:path";
import os from "node:os";
import { execFileSync } from "node:child_process";
import { GitCache, RECOVERY_BUNDLE_REF } from "../src/git.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { CodexSessionStore, seedCodexSessionArtifacts } from "../src/codex/session-state.js";

// This is a path-selection proof for a controlled, non-echoing transcript.
// It is deliberately NOT a general secret-content scan. The separate Git bundle\n// test below exercises repository recovery, not CODEX_HOME packaging.
test("provider auth-state canaries stay outside persisted and per-root session subsets", async (t) => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "provider-session-canary-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const home = path.join(root, "source");
  const store = path.join(root, "store");
  const staging = path.join(root, "epoch.session-seed");
  const finalHome = path.join(root, "epoch", "codex");
  const rollout = "sessions/2026/rollout-safe.jsonl";
  const transcript = '{"type":"session_meta","payload":{"id":"controlled-session"}}\n';
  const canaries = ["auth", "config", "cache"].map(kind => ["canary", kind, "distinct-private-value"].join("-"));
  for (const [index, rel] of ["auth.json", "config.toml", "cache/state.json"].entries()) {
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
    for (const rel of ["auth.json", "config.toml", "cache/state.json"]) {
      await assert.rejects(fs.stat(path.join(subset, rel)), { code: "ENOENT" });
    }
  }
  // Source presence is a positive control: absence downstream cannot be explained
  // by a fixture that never planted auth/config/cache state.
  for (const [index, rel] of ["auth.json", "config.toml", "cache/state.json"].entries()) {
    assert.equal(await fs.readFile(path.join(home, rel), "utf8"), canaries[index]);
  }
});

test("provider auth-state outside repository is absent from actual Git recovery bundle objects", async (t) => {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const env: NodeJS.ProcessEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
  Object.assign(env, { GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" });
  const git = (cwd: string, ...args: string[]) => execFileSync("git", ["-C", cwd, ...args], { env, encoding: "utf8", stdio: "pipe" }).trim();
  const authHome = path.join(fx.dataDir, "provider-home");
  const canaries = ["auth", "config", "cache"].map(kind => ["bundle-private", kind, "canary"].join("-"));
  for (const [index, rel] of ["auth.json", "config.toml", "cache/state.json"].entries()) {
    const file = path.join(authHome, rel);
    await fs.mkdir(path.dirname(file), { recursive: true });
    await fs.writeFile(file, canaries[index]!);
  }
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
    for (const canary of canaries) assert.equal(content.includes(canary), false);
  }
  for (const [index, rel] of ["auth.json", "config.toml", "cache/state.json"].entries())
    assert.equal(await fs.readFile(path.join(authHome, rel), "utf8"), canaries[index]);
  // No content scanning is asserted: a credential echoed into committed repository
  // content could enter this Git archive. This is not a CODEX_HOME archive.
});

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
