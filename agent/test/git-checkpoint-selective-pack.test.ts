import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { PassThrough } from "node:stream";
import type { BoundaryProcessSpawner } from "../src/git.js";
import { CheckpointUnsafeCleanupError } from "../src/checkpoint-stop.js";
import fs from "node:fs";
import path from "node:path";
import { GitCache, type GitCacheOptions } from "../src/git.js";
import { nullLogger } from "./helpers.js";
import { RunRunner } from "../src/runner.js";
import { WorkerClient } from "../src/client.js";

const scratch = path.resolve("../.uzi/scratch");
function fixture(bounds?: GitCacheOptions["checkpointPackBounds"], rawPath?: Buffer) {
  fs.mkdirSync(scratch, { recursive: true });
  const root = fs.mkdtempSync(path.join(scratch, "selective-pack-"));
  const bare = path.join(root, "repo.git");
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
    GIT_AUTHOR_NAME: "test", GIT_AUTHOR_EMAIL: "test@example.com",
    GIT_COMMITTER_NAME: "test", GIT_COMMITTER_EMAIL: "test@example.com" };
  const git = (args: string[], input?: string | Buffer) =>
    execFileSync("git", ["--git-dir", bare, ...args], { env, input, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
  execFileSync("git", ["init", "--bare", bare], { env, stdio: "pipe" });
  git(["config", "maintenance.auto", "false"]);
  const blob = (content: string) => git(["hash-object", "-w", "--stdin"], content);
  const tree = (entries: [string, string][]) => git(["mktree"],
    entries.map(([name, oid]) => `100644 blob ${oid}\t${JSON.stringify(name)}\n`).join(""));
  const commit = (tree: string, parent?: string, subject = "work") =>
    git(["commit-tree", tree, ...(parent ? ["-p", parent] : [])], subject + "\n");
  const baseBlob = blob("base"), sharedBlob = blob("public"), privateBlob = blob("private");
  const floorTree = tree([["base", baseBlob]]);
  const floor = commit(floorTree, undefined, "floor");
  const defaultTree = tree([['quote" and 雪', sharedBlob]]);
  const defaultSha = commit(defaultTree, floor, "default");
  const candidateTree = rawPath
    ? git(["mktree", "-z"], Buffer.concat([
      Buffer.from(`100644 blob ${sharedBlob}\tquote" and 雪\0`),
      Buffer.from(`100644 blob ${privateBlob}\t`), rawPath, Buffer.from("\0"),
    ]))
    : tree([['quote" and 雪', sharedBlob], ["private", privateBlob]]);
  const first = commit(candidateTree, defaultSha, "first");
  const candidate = commit(candidateTree, first, "second");
  const branch = "agent/test";
  git(["update-ref", `refs/remotes/origin/${branch}`, floor]);
  git(["update-ref", `refs/uzi-runner/${branch}`, candidate]);
  git(["update-ref", "refs/remotes/origin/開発/custom", defaultSha]);
  const cache = new GitCache(root, nullLogger(), undefined, { checkpointPackBounds: bounds });
  let packs = 0;
  const inventory = async (packed: { pack: NodeJS.ReadableStream; exited: Promise<number> }) => {
    const chunks: Buffer[] = [];
    for await (const chunk of packed.pack) chunks.push(Buffer.from(chunk));
    assert.equal(await packed.exited, 0);
    const file = path.join(root, `inventory-${packs++}.pack`);
    fs.writeFileSync(file, Buffer.concat(chunks));
    git(["index-pack", file]);
    const output = git(["verify-pack", "-v", file.replace(/\.pack$/, ".idx")]);
    return output.split("\n").filter(row => /^[0-9a-f]{40} /.test(row))
      .map(row => row.split(" ").slice(0, 2).join(" ")).sort();
  };
  const optimized = [`${defaultSha} commit`, `${defaultTree} tree`, `${first} commit`,
    `${candidate} commit`, `${candidateTree} tree`, `${privateBlob} blob`].sort();
  const legacy = [...optimized, `${sharedBlob} blob`].sort();
  return { root, bare, git, cache, branch, floor, defaultSha, candidate, candidateTree, first,
    sharedBlob, privateBlob, defaultTree, optimized, legacy, inventory,
    cleanup: () => fs.rmSync(root, { recursive: true, force: true }) };
}

function supervised(f: ReturnType<typeof fixture>, fault?: "completion" | "cancellation") {
  const calls: { args: readonly string[]; input: Buffer[] }[] = [];
  const spawner: BoundaryProcessSpawner = async request => {
    const args = request.argv.slice(1);
    const input: Buffer[] = [];
    calls.push({ args, input });
    const child = spawn(request.argv[0]!, [...args], { cwd: request.cwd, env: request.env });
    const stdin = new PassThrough();
    stdin.on("data", chunk => input.push(Buffer.from(chunk)));
    stdin.on("error", () => undefined);
    child.stdin.on("error", () => undefined);
    stdin.pipe(child.stdin);
    const preparation = args.includes("rev-list") &&
      args[args.indexOf("rev-list") + 1] === f.candidate;
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", code => {
        if (preparation && fault === "completion") reject(new Error("unverified completion"));
        else resolve({ code: code ?? -1 });
      });
    });
    void completed.catch(() => undefined);
    return { stdin, stdout: child.stdout, stderr: child.stderr, completed,
      cancel: async () => {
        // This test owner has one exact child. Cancellation joins its close, without retries.
        child.kill("SIGTERM");
        await completed;
        if (preparation && fault === "cancellation") throw new Error("unverified cancellation");
      } };
  };
  return { calls, spawner };
}

it("selective producer preserves invalid UTF8 filename bytes with whitespace and quotes", async () => {
  const rawPath = Buffer.concat([Buffer.from('bad-'), Buffer.from([0xff]), Buffer.from('- name"quote')]);
  const f = fixture(undefined, rawPath);
  const owner = supervised(f);
  try {
    await f.cache.withBoundaryProcessSpawner(owner.spawner, new AbortController().signal, async () => {
      const packed = await f.cache.checkpointPack(f.bare, f.branch, undefined, undefined,
        undefined, undefined, undefined, undefined, { defaultBranch: "開発/custom" });
      assert.ok(packed);
      assert.deepEqual(await f.inventory(packed), f.optimized);
    });
    const producers = owner.calls.filter(call => call.args.includes("pack-objects"));
    assert.equal(producers.length, 1);
    assert.equal(producers[0]!.args.includes("--revs"), false, "optimization must select the explicit-object producer");
    const manifest = Buffer.concat(producers[0]!.input);
    assert.ok(manifest.includes(Buffer.concat([Buffer.from(f.privateBlob + " "), rawPath, Buffer.from("\n")])),
      "the producer must receive the original filename bytes");
    assert.equal(manifest.includes(Buffer.from(f.sharedBlob)), false);
  } finally { f.cleanup(); }
});

for (const fault of ["completion", "cancellation"] as const) {
  it(`preparation cap refuses legacy fallback on unverified ${fault}`, async () => {
    const f = fixture({ bytes: 1 });
    const owner = supervised(f, fault);
    try {
      await assert.rejects(f.cache.withBoundaryProcessSpawner(owner.spawner, new AbortController().signal,
        () => f.cache.checkpointPack(f.bare, f.branch, undefined, undefined,
          undefined, undefined, undefined, undefined, { defaultBranch: "開発/custom" })),
      error => error instanceof CheckpointUnsafeCleanupError);
      assert.equal(owner.calls.filter(call => call.args.includes("pack-objects")).length, 0);
    } finally { f.cleanup(); }
  });
}

it("selective non-thin pack preserves every commit/tree and original quoted path rows", async () => {
  const f = fixture();
  try {
    const range = await f.cache.resolveCheckpointRange(f.bare, f.branch, {
      defaultBranch: "開発/custom", confirmedTip: f.first,
    });
    assert.equal(range?.defaultBlobSha, f.defaultSha);
    assert.ok(range?.scanFloorShas?.includes(f.first));
    const packed = await f.cache.checkpointPack(f.bare, f.branch, undefined, range!);
    assert.ok(packed);
    assert.deepEqual(await f.inventory(packed), f.optimized);
    // Confirmed tips affect scans only: the first commit and private blob must still ship.
    assert.ok(f.optimized.includes(`${f.first} commit`));
  } finally { f.cleanup(); }
});

for (const mode of ["absent", "invalid", "missing", "mirror", "conflict", "valid"] as const) {
  it(`server-only default provenance: ${mode}`, async () => {
    const f = fixture();
    try {
      let name: string | undefined = "開発/custom";
      if (mode === "absent") name = undefined;
      if (mode === "invalid") name = "../bad";
      if (mode === "missing") name = "missing";
      if (mode === "mirror") {
        f.git(["update-ref", "refs/heads/開発/custom", f.defaultSha]);
        f.git(["update-ref", "-d", "refs/remotes/origin/開発/custom"]);
      }
      if (mode !== "valid") f.git(["symbolic-ref", "refs/remotes/origin/HEAD",
        mode === "conflict" ? "refs/remotes/origin/other" : "refs/remotes/origin/開発/custom"]);
      const packed = await f.cache.checkpointPack(f.bare, f.branch, undefined, undefined,
        undefined, undefined, undefined, undefined, { defaultBranch: name });
      assert.ok(packed);
      assert.deepEqual(await f.inventory(packed), mode === "valid" ? f.optimized : f.legacy);
    } finally { f.cleanup(); }
  });
}

for (const name of ["開発/custom\u00a0", " 開発/custom "]) {
  it(`actual flight preserves exact default authority in inventory: ${JSON.stringify(name)}`, async () => {
    const f = fixture();
    try {
      if (name.endsWith("\u00a0")) f.git(["update-ref", `refs/remotes/origin/${name}`, f.floor]);
      const client = new WorkerClient("http://unused", "test", "test", nullLogger());
      const runner = new RunRunner(client, f.cache, () => ({ executor: { run: async () => ({ branch: f.branch }) } }),
        nullLogger(), 1);
      const claim = { repo: { default_branch: name }, secrets: {}, last_seq: 0, kind: "issue" } as Parameters<RunRunner["buildFlight"]>[0];
      const flight = runner["buildFlight"](claim, "test", { run: async () => ({ branch: f.branch }) }, undefined, "", []);
      const owner = supervised(f);
      const packed = await f.cache.withBoundaryProcessSpawner(owner.spawner, new AbortController().signal,
        () => f.cache.checkpointPack(f.bare, f.branch, undefined, undefined, undefined, undefined,
          undefined, undefined, { defaultBranch: flight.checkpointDefaultBranch }));
      assert.ok(packed);
      assert.deepEqual(await f.inventory(packed), f.legacy, "trimmed-ref-only shared blob must stay in the pack");
      const producer = owner.calls.find(call => call.args.includes("pack-objects"))!;
      assert.equal(producer.args.includes("--revs"), !name.endsWith("\u00a0"),
        "valid exact Unicode authority selects optimization; ASCII-spaced authority stays legacy");
      assert.equal(owner.calls.some(call => call.args.includes("cat-file")), name.endsWith("\u00a0"));
    } finally { f.cleanup(); }
  });
}

it("candidate equal to default retains candidate commits and trees but no blobs", async () => {
  const f = fixture();
  try {
    f.git(["update-ref", "refs/remotes/origin/開発/custom", f.candidate]);
    const packed = await f.cache.checkpointPack(f.bare, f.branch, undefined, undefined,
      undefined, undefined, undefined, undefined, { defaultBranch: "開発/custom" });
    assert.ok(packed);
    assert.deepEqual(await f.inventory(packed),
      [`${f.defaultSha} commit`, `${f.defaultTree} tree`, `${f.first} commit`,
        `${f.candidate} commit`, `${f.candidateTree} tree`].sort());
  } finally { f.cleanup(); }
});

for (const captured of [true, false]) it(`replay pins captured default including absence: ${captured}`, async () => {
  const f = fixture();
  try {
    const range = await f.cache.resolveCheckpointRange(f.bare, f.branch,
      { defaultBranch: captured ? "開発/custom" : undefined });
    assert.ok(range);
    const packed = await f.cache.checkpointPack(f.bare, f.branch, undefined, range);
    assert.ok(packed);
    const expected = captured ? f.optimized : f.legacy;
    assert.deepEqual(await f.inventory(packed), expected);
    f.git(["update-ref", "refs/remotes/origin/開発/custom", f.candidate]);
    f.git(["symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/other"]);
    range.defaultBlobSha = f.candidate;
    assert.deepEqual(await f.inventory(await packed.replay()), expected);
  } finally { f.cleanup(); }
});

for (const bounds of [{ records: 1 }, { bytes: 1 }, { lineBytes: 1 }, { durationMs: 0 }]) {
  it(`preparation cap falls back to exact legacy inventory: ${JSON.stringify(bounds)}`, async () => {
    const f = fixture(bounds);
    try {
      const packed = await f.cache.checkpointPack(f.bare, f.branch, undefined, undefined,
        undefined, undefined, undefined, undefined, { defaultBranch: "開発/custom" });
      assert.ok(packed);
      assert.deepEqual(await f.inventory(packed), f.legacy);
    } finally { f.cleanup(); }
  });
}

it("explicit expired native lifetime refuses publication before any preparation", async () => {
  const f = fixture();
  try {
    await assert.rejects(f.cache.checkpointPack(f.bare, f.branch, undefined, undefined,
      undefined, undefined, undefined, undefined, { defaultBranch: "開発/custom", deadline: Date.now() - 1 }));
  } finally { f.cleanup(); }
});

it("native cancellation in beforePack cannot launch a producer or fallback", async () => {
  const f = fixture();
  const controller = new AbortController();
  try {
    await assert.rejects(f.cache.checkpointPack(f.bare, f.branch, undefined, undefined,
      undefined, undefined, async () => { controller.abort(new Error("stop publication")); },
      undefined, { defaultBranch: "開発/custom", signal: controller.signal }), /stop publication/);
  } finally { f.cleanup(); }
});
