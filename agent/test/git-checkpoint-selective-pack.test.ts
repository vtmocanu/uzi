import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { PassThrough } from "node:stream";
import type { BoundaryProcessSpawner } from "../src/git.js";
import { CheckpointUnsafeCleanupError } from "../src/checkpoint-stop.js";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { GitCache, OVERLAY_COMMIT_PREFIX, ScratchPublicationError, type GitCacheOptions } from "../src/git.js";
import { nullLogger } from "./helpers.js";
import { RunRunner } from "../src/runner.js";
import { runEnvClaimForms } from "../src/run-env-forms.js";
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
  return { root, bare, git, cache, branch, floor, defaultSha, candidate, candidateTree, first, blob, tree, commit,
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

for (const matching of [false, true]) {
  it(`origin HEAD preserves Unicode whitespace authority: matching=${matching}`, async () => {
    const f = fixture();
    try {
      const name = matching ? "開発/custom\u00a0" : "開発/custom";
      const head = "refs/remotes/origin/開発/custom\u00a0";
      f.git(["check-ref-format", head]);
      f.git(["update-ref", head, f.defaultSha]);
      f.git(["symbolic-ref", "refs/remotes/origin/HEAD", head]);
      const packed = await f.cache.checkpointPack(f.bare, f.branch, undefined, undefined,
        undefined, undefined, undefined, undefined, { defaultBranch: name });
      assert.ok(packed);
      assert.deepEqual(await f.inventory(packed), matching ? f.optimized : f.legacy);
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
      const flight = runner["buildFlight"](claim, "test", { run: async () => ({ branch: f.branch }) }, undefined, "", [], runEnvClaimForms(claim.secrets.run_env, claim.secrets.run_env_redact));
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

for (const pinned of [true, false]) {
  for (const mutation of ["move", "delete", "head-conflict", "create", "head-consistent", "no-head"] as const) {
    it(`replay retains default capture: pinned=${pinned}, mutation=${mutation}`, async () => {
      const f = fixture();
      try {
        const ref = "refs/remotes/origin/開発/custom";
        const head = "refs/remotes/origin/HEAD";
        const captured = mutation !== "create" && mutation !== "head-consistent";
        if (mutation === "create") f.git(["update-ref", "-d", ref]);
        if (mutation === "head-consistent") {
          f.git(["symbolic-ref", head, "refs/remotes/origin/other"]);
        }
        const options = { defaultBranch: "開発/custom" };
        const range = pinned
          ? await f.cache.resolveCheckpointRange(f.bare, f.branch, options)
          : undefined;
        if (pinned) {
          assert.ok(range);
          assert.equal(range.defaultBlobSha, captured ? f.defaultSha : undefined);
        }
        const packed = await f.cache.checkpointPack(f.bare, f.branch, undefined, range ?? undefined,
          undefined, undefined, undefined, undefined, options);
        assert.ok(packed);
        const expected = captured ? f.optimized : f.legacy;
        assert.deepEqual(await f.inventory(packed), expected);
        if (mutation === "move") f.git(["update-ref", ref, f.candidate]);
        if (mutation === "delete") f.git(["update-ref", "-d", ref]);
        if (mutation === "create") f.git(["update-ref", ref, f.defaultSha]);
        if (mutation === "head-conflict") {
          f.git(["symbolic-ref", head, "refs/remotes/origin/other"]);
        }
        if (mutation === "head-consistent") f.git(["symbolic-ref", head, ref]);
        // Caller mutation must not replace the capture held by the replay closure.
        if (range) range.defaultBlobSha = f.candidate;
        assert.deepEqual(await f.inventory(await packed.replay()), expected);
      } finally { f.cleanup(); }
    });
  }
}

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

for (const pinned of [false, true]) {
  it(`selective packing preserves zero-range transport commit (${pinned ? "pinned" : "unpinned"})`, async () => {
    const f = fixture();
    try {
      f.git(["update-ref", "-d", `refs/remotes/origin/${f.branch}`]);
      f.git(["update-ref", "refs/remotes/origin/main", f.candidate]);
      const range = await f.cache.resolveCheckpointRange(f.bare, f.branch, { defaultBranch: "main" });
      assert.ok(range);
      assert.equal(range.tipSha, f.candidate);
      assert.equal(range.excludeSha, f.candidate);
      assert.equal(range.defaultBlobSha, f.candidate);
      assert.deepEqual(range.packExcludeShas, [f.first]);
      const packed = await f.cache.checkpointPack(f.bare, f.branch, undefined, pinned ? range : undefined,
        undefined, undefined, undefined, undefined, { defaultBranch: "main" });
      assert.ok(packed);
      const expected = [`${f.candidate} commit`];
      assert.deepEqual(await f.inventory(packed), expected, "zero range still transports its declared commit");
      assert.deepEqual(await f.inventory(await packed.replay()), expected, "replay keeps the captured transport exclusions");
    } finally { f.cleanup(); }
  });
}

// ---- Server-confirmed checkpoint tip (C) as a BLOB-ONLY omission (issue #4439) ----
type Fx = ReturnType<typeof fixture>;
const QUOTED = 'quote" and 雪';
const sha256 = (b: Buffer) => createHash("sha256").update(b).digest("hex");

function parsePack(rows: string[]) {
  const of = (type: string) => rows.filter(r => r.endsWith(` ${type}`)).map(r => r.split(" ")[0]!);
  return { commits: of("commit"), trees: of("tree"), blobs: of("blob") };
}

/** Linear history F -> D(default) -> X -> C -> T; blobs: bx only in X, bc in C and T, bt new in T. */
function linear(f: Fx) {
  const bx = f.blob("only-in-x"), bc = f.blob("in-c"), bt = f.blob("new-in-t"), by = f.blob("new-in-y");
  const mk = (extra: [string, string][]) => f.tree([[QUOTED, f.sharedBlob], ...extra]);
  const x = f.commit(mk([["x", bx]]), f.defaultSha, "x");
  const c = f.commit(mk([["c", bc]]), x, "c");
  const t = f.commit(mk([["c", bc], ["t", bt]]), c, "t");
  f.git(["update-ref", `refs/uzi-runner/${f.branch}`, t]);
  return { bx, bc, bt, by, x, c, t, mk };
}

type PackedStream = { pack: NodeJS.ReadableStream; exited: Promise<number> };
async function packRows(f: Fx, options: Record<string, unknown> = {}, range?: Parameters<GitCache["checkpointPack"]>[3]) {
  const packed = await f.cache.checkpointPack(f.bare, f.branch, undefined, range,
    undefined, undefined, undefined, undefined, { defaultBranch: "開発/custom", ...options });
  assert.ok(packed);
  return packed;
}
const rowsOf = async (f: Fx, options: Record<string, unknown> = {}, range?: Parameters<GitCache["checkpointPack"]>[3]) =>
  f.inventory(await packRows(f, options, range));
const packBytes = async (s: PackedStream) => {
  const chunks: Buffer[] = [];
  for await (const chunk of s.pack) chunks.push(Buffer.from(chunk));
  assert.equal(await s.exited, 0);
  return Buffer.concat(chunks);
};
/** sha256 of the captured pack bytes: an ineligible C must change nothing, not merely the inventory. */
const bytesHashOf = async (f: Fx, options: Record<string, unknown> = {}, range?: Parameters<GitCache["checkpointPack"]>[3]) =>
  sha256(await packBytes(await packRows(f, options, range)));
const rowsOfBytes = (f: Fx, bytes: Buffer, name: string) => {
  const file = path.join(f.root, name);
  fs.writeFileSync(file, bytes);
  f.git(["index-pack", file]);
  return f.git(["verify-pack", "-v", file.replace(/\.pack$/, ".idx")]).split("\n")
    .filter(row => /^[0-9a-f]{40} /.test(row)).map(row => row.split(" ").slice(0, 2).join(" ")).sort();
};
const isAncestor = (f: Fx, a: string, b: string) => {
  try { f.git(["merge-base", "--is-ancestor", a, b]); return true; } catch { return false; }
};

/** Subset oracle: same commits and trees as without C; blobs only ever removed. */
function assertBlobOnlyOmission(withC: string[], noC: string[]) {
  const a = parsePack(withC), b = parsePack(noC);
  assert.deepEqual([...a.commits].sort(), [...b.commits].sort(), "commits are exactly those of the no-C selection");
  assert.deepEqual([...a.trees].sort(), [...b.trees].sort(), "trees are exactly those of the no-C selection");
  for (const blob of a.blobs) assert.ok(b.blobs.includes(blob), `blob ${blob} must be in the no-C selection`);
}

it("confirmed tip omits blobs at or below it: linear history", async () => {
  const f = fixture();
  try {
    const h = linear(f);
    const noC = await rowsOf(f);
    const withC = await rowsOf(f, { confirmedBlobTip: h.c });
    assertBlobOnlyOmission(withC, noC);
    const before = parsePack(noC).blobs, after = parsePack(withC).blobs;
    assert.ok(before.includes(h.bx) && before.includes(h.bc) && before.includes(h.bt), "baseline carries all three");
    assert.deepEqual(after, [h.bt], "historical X-only blob and C's unchanged blob are omitted; T's new blob ships");
  } finally { f.cleanup(); }
});

it("confirmed tip omits a blob below it that a merged side branch re-carries", async () => {
  const f = fixture();
  try {
    const h = linear(f);
    const y = f.commit(h.mk([["x", h.bx], ["y", h.by]]), h.x, "y");
    const m = f.git(["commit-tree", h.mk([["c", h.bc], ["t", h.bt], ["y", h.by]]), "-p", h.t, "-p", y], "merge\n");
    f.git(["update-ref", `refs/uzi-runner/${f.branch}`, m]);
    const noC = await rowsOf(f);
    const withC = await rowsOf(f, { confirmedBlobTip: h.c });
    assertBlobOnlyOmission(withC, noC);
    assert.ok(parsePack(noC).blobs.includes(h.bx));
    const after = parsePack(withC).blobs;
    assert.equal(after.includes(h.bx), false, "X-only blob is below C: omitted although Y re-carries it");
    assert.equal(after.includes(h.bc), false);
    assert.ok(after.includes(h.by) && after.includes(h.bt));
    assert.ok(parsePack(withC).commits.includes(y), "side-branch commit still ships");
  } finally { f.cleanup(); }
});

it("main merged after C: default blobs stay omitted and C's blobs are omitted too", async () => {
  const f = fixture();
  try {
    const h = linear(f);
    const bd = f.blob("default-later");
    const d2 = f.commit(f.tree([[QUOTED, f.sharedBlob], ["d2", bd]]), f.defaultSha, "d2");
    f.git(["update-ref", "refs/remotes/origin/開発/custom", d2]);
    const m = f.git(["commit-tree", f.tree([[QUOTED, f.sharedBlob], ["c", h.bc], ["d2", bd], ["t", h.bt]]),
      "-p", h.c, "-p", d2], "merge main\n");
    f.git(["update-ref", `refs/uzi-runner/${f.branch}`, m]);
    const noC = await rowsOf(f);
    const withC = await rowsOf(f, { confirmedBlobTip: h.c });
    assertBlobOnlyOmission(withC, noC);
    assert.equal(parsePack(noC).blobs.includes(bd), false, "default blob omitted today (#3171)");
    assert.deepEqual(parsePack(withC).blobs, [h.bt]);
  } finally { f.cleanup(); }
});

it("eligibility is judged on the chain candidate, not the raw tip (#2733)", async () => {
  const f = fixture();
  try {
    const wf = (blob: string) => f.git(["mktree"], `100644 blob ${blob}\tci.yml\n`);
    const dotGithub = (blob: string) => f.git(["mktree"], `040000 tree ${wf(blob)}\tworkflows\n`);
    const mk = (extra: [string, string][], wfBlob: string) => f.git(["mktree"],
      [`100644 blob ${f.sharedBlob}\t${JSON.stringify(QUOTED)}\n`,
        ...extra.map(([n, o]) => `100644 blob ${o}\t${JSON.stringify(n)}\n`),
        `040000 tree ${dotGithub(wfBlob)}\t.github\n`].join(""));
    const wf1 = f.blob("name: ci\n# v1\n"), wf2 = f.blob("name: ci\n# v2\n");
    const br = f.blob("in-r"), bn = f.blob("new-in-n");
    const d2 = f.commit(mk([], wf1), f.defaultSha, "d2");
    f.git(["update-ref", "refs/remotes/origin/開発/custom", d2]);
    // The chain's workflow gate diffs against the generic default-branch ref.
    f.git(["update-ref", "refs/remotes/origin/main", d2]);
    const r = f.commit(mk([["r", br]], wf1), d2, "r");
    const p = f.commit(mk([["r", br]], wf2), r, `${OVERLAY_COMMIT_PREFIX} wrapper`);
    const n = f.commit(mk([["r", br], ["n", bn]], wf1), r, "n");
    f.git(["update-ref", `refs/uzi-runner/${f.branch}`, n]);
    assert.equal(isAncestor(f, p, n), false, "raw tip does not descend from C");
    const chained = async (options: Record<string, unknown>) => {
      const packed = await f.cache.checkpointPack(f.bare, f.branch, undefined, undefined, undefined, undefined,
        undefined, [p], { defaultBranch: "開発/custom", ...options });
      assert.ok(packed);
      assert.notEqual(packed.tipOid, n, "a chain wrapper is the candidate");
      assert.equal(isAncestor(f, p, packed.tipOid), true, "C descends into the chain candidate");
      return f.inventory(packed);
    };
    const noC = await chained({});
    const withC = await chained({ confirmedBlobTip: p });
    assertBlobOnlyOmission(withC, noC);
    assert.ok(parsePack(noC).blobs.includes(br) && parsePack(noC).blobs.includes(wf2));
    const after = parsePack(withC).blobs;
    assert.equal(after.includes(br), false);
    assert.equal(after.includes(wf2), false);
    assert.ok(after.includes(bn));
  } finally { f.cleanup(); }
});

for (const control of ["equals candidate", "not an ancestor", "unresolvable", "malformed"] as const) {
  it(`ineligible confirmed tip leaves the selection byte-identical: ${control}`, async () => {
    const f = fixture();
    try {
      const h = linear(f);
      // Sits on X (an ancestor of the candidate) but is not itself one: wrongly using it as an
      // omission floor would also hide X's blobs, which are still unpublished.
      const side = f.commit(h.mk([["side", f.blob("side")]]), h.x, "side");
      const confirmed = { "equals candidate": h.t, "not an ancestor": side,
        unresolvable: "e".repeat(40), malformed: "not-a-sha" }[control];
      const noC = await rowsOf(f);
      assert.ok(parsePack(noC).blobs.includes(h.bx), "baseline carries X's unpublished blob");
      assert.equal(await bytesHashOf(f, { confirmedBlobTip: confirmed }), await bytesHashOf(f));
      assert.deepEqual(await rowsOf(f, { confirmedBlobTip: confirmed }), noC);
    } finally { f.cleanup(); }
  });
}

it("confirmed tip equal to the floor is ineligible", async () => {
  const f = fixture();
  try {
    linear(f);
    assert.equal(await bytesHashOf(f, { confirmedBlobTip: f.floor }), await bytesHashOf(f));
  } finally { f.cleanup(); }
});

it("overlay wrapper: eligibility is judged on the wrapper, and C below it omits blobs only", async () => {
  const f = fixture();
  try {
    const wf = (blob: string) => f.git(["mktree"], `100644 blob ${blob}\tci.yml\n`);
    const dotGithub = (blob: string) => f.git(["mktree"], `040000 tree ${wf(blob)}\tworkflows\n`);
    const mk = (extra: [string, string][], wfBlob: string) => f.git(["mktree"],
      [`100644 blob ${f.sharedBlob}\t${JSON.stringify(QUOTED)}\n`,
        ...extra.map(([n, o]) => `100644 blob ${o}\t${JSON.stringify(n)}\n`),
        `040000 tree ${dotGithub(wfBlob)}\t.github\n`].join(""));
    const wf1 = f.blob("name: ci\n# v1\n"), wf2 = f.blob("name: ci\n# v2\n");
    const bx = f.blob("only-in-x"), bc = f.blob("in-c"), bt = f.blob("new-in-t");
    const d2 = f.commit(mk([], wf1), f.defaultSha, "d2");
    f.git(["update-ref", "refs/remotes/origin/開発/custom", d2]);
    // The default branch is ahead on .github/workflows: the branch is behind and an overlay is built.
    const dm = f.commit(mk([], wf2), d2, "main advances its workflow");
    const origin = path.join(f.root, "origin.git");
    execFileSync("git", ["init", "--bare", origin], { stdio: "pipe" });
    f.git(["remote", "add", "origin", origin]);
    f.git(["push", "origin", `${dm}:refs/heads/main`]);
    f.git(["update-ref", "refs/remotes/origin/main", dm]);
    const x = f.commit(mk([["x", bx]], wf1), d2, "x");
    const c = f.commit(mk([["x", bx], ["c", bc]], wf1), x, "c");
    const t = f.commit(mk([["x", bx], ["c", bc], ["t", bt]], wf1), c, "t");
    f.git(["update-ref", `refs/uzi-runner/${f.branch}`, t]);
    const overlayPack = async (options: Record<string, unknown>) => {
      const packed = await f.cache.checkpointPack(f.bare, f.branch, { defaultBranch: "main" }, undefined,
        undefined, undefined, undefined, undefined, { defaultBranch: "開発/custom", ...options });
      assert.ok(packed);
      assert.notEqual(packed.tipOid, t, "the overlay wrapper is the candidate, not the raw tip");
      assert.ok(f.git(["log", "-1", "--format=%s", packed.tipOid]).startsWith(OVERLAY_COMMIT_PREFIX));
      assert.equal(isAncestor(f, c, packed.tipOid), true, "C is an ancestor of the wrapper");
      return f.inventory(packed);
    };
    const noC = await overlayPack({});
    const withC = await overlayPack({ confirmedBlobTip: c });
    assertBlobOnlyOmission(withC, noC);
    assert.ok(parsePack(noC).blobs.includes(bx) && parsePack(noC).blobs.includes(bc), "baseline carries C-reachable blobs");
    const after = parsePack(withC).blobs;
    assert.equal(after.includes(bc), false, "a C-reachable blob is omitted");
    assert.equal(after.includes(bx), false);
    assert.ok(after.includes(bt) && after.includes(wf2), "the wrapper's own workflow blob and T's new blob still ship");
  } finally { f.cleanup(); }
});

/** F -> D1(default) -> X -> Mm(merge X,D1) -> C -> T, with the default later advanced to D2 (child of
 *  D1). T carries D2's blob bd2 by copy without merging D2, so D2 is a commit exclusion only: it is
 *  not an edge, hence only an explicit `^D2^{tree}` keeps bd2 out of the blob selection. */
function advancedDefault(f: Fx) {
  const bx = f.blob("only-in-x"), bc = f.blob("in-c"), bt = f.blob("new-in-t"), bd2 = f.blob("default-later");
  const mk = (extra: [string, string][]) => f.tree([[QUOTED, f.sharedBlob], ...extra]);
  const x = f.commit(mk([["x", bx]]), f.defaultSha, "x");
  const mm = f.git(["commit-tree", mk([["x", bx]]), "-p", x, "-p", f.defaultSha], "merge main\n");
  const c = f.commit(mk([["x", bx], ["c", bc]]), mm, "c");
  const d2 = f.commit(mk([["d2", bd2]]), f.defaultSha, "d2");
  const t = f.commit(mk([["x", bx], ["c", bc], ["t", bt], ["d2", bd2]]), c, "t");
  f.git(["update-ref", "refs/remotes/origin/開発/custom", d2]);
  f.git(["update-ref", `refs/uzi-runner/${f.branch}`, t]);
  return { bx, bc, bt, bd2, x, mm, c, d2, t };
}

it("confirmed tip keeps default-tree exclusion: a default blob copied into the candidate is omitted", async () => {
  const f = fixture();
  try {
    const h = advancedDefault(f);
    const noC = await rowsOf(f);
    const withC = await rowsOf(f, { confirmedBlobTip: h.c });
    assertBlobOnlyOmission(withC, noC);
    assert.ok(parsePack(noC).blobs.includes(h.bd2), "baseline ships the copied default blob");
    assert.deepEqual(parsePack(withC).blobs, [h.bt], "bd2 is excluded through the default tree; C's blobs are omitted");
  } finally { f.cleanup(); }
});

it("confirmed tip equal to the floor adds no default-tree exclusion (byte-identical to no C)", async () => {
  const f = fixture();
  try {
    const h = advancedDefault(f);
    const noC = await rowsOf(f);
    assert.ok(parsePack(noC).blobs.includes(h.bd2));
    assert.equal(await bytesHashOf(f, { confirmedBlobTip: f.floor }), await bytesHashOf(f));
  } finally { f.cleanup(); }
});

it("replay refuses when the captured confirmed tip is no longer resolvable, and spawns no producer", async () => {
  const f = fixture();
  const owner = supervised(f);
  try {
    const h = advancedDefault(f);
    const internals = f.cache as unknown as { revParse: (barePath: string, ref: string) => Promise<string | null> };
    const original = internals.revParse.bind(f.cache);
    await f.cache.withBoundaryProcessSpawner(owner.spawner, new AbortController().signal, async () => {
      const packed = await packRows(f, { confirmedBlobTip: h.c });
      assert.deepEqual(parsePack(await f.inventory(packed)).blobs, [h.bt], "first open used the confirmed tip");
      const producers = () => owner.calls.filter(call => call.args.includes("pack-objects")).length;
      assert.equal(producers(), 1);
      internals.revParse = async (barePath, ref) => ref === `${h.c}^{commit}` ? null : original(barePath, ref);
      try {
        await assert.rejects(packed.replay(),
          (error: unknown) => error instanceof ScratchPublicationError && /confirmed tip is unavailable/.test(error.message));
      } finally { internals.revParse = original; }
      assert.equal(producers(), 1, "replay spawned no pack-objects producer");
    });
  } finally { f.cleanup(); }
});

it("legacy producers ignore the confirmed tip: no captured default, and the zero-range envelope", async () => {
  const f = fixture();
  try {
    const h = linear(f);
    const noDefault = await rowsOf(f, { defaultBranch: undefined });
    assert.equal(await bytesHashOf(f, { defaultBranch: undefined, confirmedBlobTip: h.c }),
      await bytesHashOf(f, { defaultBranch: undefined }));
    assert.ok(parsePack(noDefault).blobs.includes(f.sharedBlob), "legacy producer keeps the shared blob");

    f.git(["update-ref", "-d", `refs/remotes/origin/${f.branch}`]);
    f.git(["update-ref", "refs/remotes/origin/main", h.t]);
    const zero = await rowsOf(f, { defaultBranch: "main" });
    assert.ok(zero.includes(`${h.t} commit`));
    assert.equal(await bytesHashOf(f, { defaultBranch: "main", confirmedBlobTip: h.c }),
      await bytesHashOf(f, { defaultBranch: "main" }));
  } finally { f.cleanup(); }
});

it("legacy producers never re-check the confirmed tip on replay: unpinned, pinned and zero-range", async () => {
  const f = fixture();
  try {
    const h = linear(f);
    const internals = f.cache as unknown as { revParse: (barePath: string, ref: string) => Promise<string | null> };
    const original = internals.revParse.bind(f.cache);
    const replayBytes = async (packed: Awaited<ReturnType<typeof packRows>>) => {
      const first = await packBytes(packed);
      internals.revParse = async (barePath, ref) => ref === `${h.c}^{commit}` ? null : original(barePath, ref);
      try {
        assert.equal(sha256(await packBytes(await packed.replay())), sha256(first));
      } finally { internals.revParse = original; }
    };
    // Unpinned, no captured default.
    await replayBytes(await packRows(f, { defaultBranch: undefined, confirmedBlobTip: h.c }));
    // Pinned range with no captured default but a confirmedBlobSha carried on it.
    const range = await f.cache.resolveCheckpointRange(f.bare, f.branch, {});
    assert.ok(range);
    assert.equal(range.defaultBlobSha, undefined);
    range.confirmedBlobSha = h.c;
    await replayBytes(await packRows(f, {}, range));
    // Zero-range transport envelope.
    f.git(["update-ref", "-d", `refs/remotes/origin/${f.branch}`]);
    f.git(["update-ref", "refs/remotes/origin/main", h.t]);
    await replayBytes(await packRows(f, { defaultBranch: "main", confirmedBlobTip: h.c }));
    // Pinned zero-range envelope: default, confirmed tip and transport excludes all captured.
    const zeroRange = await f.cache.resolveCheckpointRange(f.bare, f.branch,
      { defaultBranch: "main", confirmedTip: h.c, captureConfirmedBlob: true });
    assert.ok(zeroRange);
    assert.equal(zeroRange.tipSha, zeroRange.excludeSha);
    assert.ok(zeroRange.packExcludeShas !== undefined && zeroRange.defaultBlobSha !== undefined);
    assert.equal(zeroRange.confirmedBlobSha, h.c);
    await replayBytes(await packRows(f, {}, zeroRange));
  } finally { f.cleanup(); }
});

it("pinned range carries the confirmed tip only when asked, and scan floors are unchanged", async () => {
  const f = fixture();
  try {
    const h = linear(f);
    const base = { defaultBranch: "開発/custom", confirmedTip: h.c };
    const plain = await f.cache.resolveCheckpointRange(f.bare, f.branch, base);
    const captured = await f.cache.resolveCheckpointRange(f.bare, f.branch, { ...base, captureConfirmedBlob: true });
    assert.ok(plain && captured);
    assert.equal("confirmedBlobSha" in plain, false);
    assert.equal(captured.confirmedBlobSha, h.c);
    assert.deepEqual(captured.scanFloorShas, plain.scanFloorShas);
    assert.ok(plain.scanFloorShas?.includes(h.c), "scan floor behaviour is today's");
    assert.equal((await f.cache.resolveCheckpointRange(f.bare, f.branch,
      { defaultBranch: "開発/custom", captureConfirmedBlob: true }))?.confirmedBlobSha, undefined);
    assert.equal((await f.cache.resolveCheckpointRange(f.bare, f.branch,
      { ...base, confirmedTip: "e".repeat(40), captureConfirmedBlob: true }))?.confirmedBlobSha, undefined);
    assert.equal((await f.cache.resolveCheckpointRange(f.bare, f.branch,
      { ...base, confirmedTip: f.floor, captureConfirmedBlob: true }))?.confirmedBlobSha, undefined);
  } finally { f.cleanup(); }
});

it("pinned: only pinned.confirmedBlobSha counts, and replay is byte-identical after refs move", async () => {
  const f = fixture();
  try {
    const h = linear(f);
    const opts = { defaultBranch: "開発/custom" };
    const base = await f.cache.resolveCheckpointRange(f.bare, f.branch, opts);
    const range = await f.cache.resolveCheckpointRange(f.bare, f.branch,
      { ...opts, confirmedTip: h.c, captureConfirmedBlob: true });
    assert.ok(base && range);
    const noC = await rowsOf(f, {}, base);
    // The unpinned option is ignored when a pinned range is supplied.
    assert.deepEqual(await rowsOf(f, { confirmedBlobTip: h.c }, base), noC);

    const packed = await packRows(f, { confirmedBlobTip: "e".repeat(40) }, range);
    const first = await packBytes(packed);
    const withC = rowsOfBytes(f, first, "pinned.pack");
    assertBlobOnlyOmission(withC, noC);
    assert.deepEqual(parsePack(withC).blobs, [h.bt]);
    // Move every ref the producer could consult; the captured literals must still be used.
    f.git(["update-ref", `refs/uzi-runner/${f.branch}`, h.x]);
    f.git(["update-ref", "refs/remotes/origin/開発/custom", h.t]);
    range.confirmedBlobSha = f.floor;
    assert.equal(sha256(await packBytes(await packed.replay())), sha256(first));
  } finally { f.cleanup(); }
});

it("pinned: malformed confirmedBlobSha is refused with the typed range error; well-formed missing one is ineligible", async () => {
  const f = fixture();
  try {
    const h = linear(f);
    const range = await f.cache.resolveCheckpointRange(f.bare, f.branch,
      { defaultBranch: "開発/custom", confirmedTip: h.c, captureConfirmedBlob: true });
    assert.ok(range);
    await assert.rejects(rowsOf(f, {}, { ...range, confirmedBlobSha: "not-a-sha" }),
      (error: unknown) => error instanceof ScratchPublicationError && /40-hex/.test(error.message));
    // Eligibility's rev-parse fails before openProducer's re-check could, so a missing object
    // degrades to today's bytes; the openProducer re-check guards only a later replay.
    const noC = await rowsOf(f, {}, { ...range, confirmedBlobSha: undefined });
    assert.deepEqual(await rowsOf(f, {}, { ...range, confirmedBlobSha: "e".repeat(40) }), noC);
  } finally { f.cleanup(); }
});

it("expired parent deadline with an eligible confirmed tip starts no producer", async () => {
  const f = fixture();
  const owner = supervised(f);
  try {
    const h = linear(f);
    await assert.rejects(f.cache.withBoundaryProcessSpawner(owner.spawner, new AbortController().signal,
      () => f.cache.checkpointPack(f.bare, f.branch, undefined, undefined, undefined, undefined, undefined, undefined,
        { defaultBranch: "開発/custom", confirmedBlobTip: h.c, deadline: Date.now() - 1 })));
    assert.equal(owner.calls.filter(call => call.args.includes("pack-objects")).length, 0);
  } finally { f.cleanup(); }
});
