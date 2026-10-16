import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { once } from "node:events";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import https from "node:https";
import * as module from "node:module";
import path from "node:path";
import test, { run as runTests } from "node:test";
import { tap } from "node:test/reporters";
import { fileURLToPath, pathToFileURL } from "node:url";

const root = fileURLToPath(new URL("../../", import.meta.url));
console.log(`# Node ${process.version}`);
// Preserve the repository's Go module/toolchain policy, including cold-cache setup.
const goEnv = { ...process.env };
const separator = process.argv.indexOf("--");
const command = process.argv.slice(separator + 1);
// Taskfile owns flag values; validate the command's structure without duplicating its recipe.
assert.ok(separator >= 2, "precondition: pass the Go command after --");
assert.equal(command[0], "go", "precondition: expected go executable");
assert.equal(command[1], "test");
const flags = command.slice(2, -1);
assert.equal(command.at(-1), "./internal/forge");
for (const pattern of [/^-buildvcs=false$/, /^-count=1$/, /^-v$/, /^-timeout=[1-9]\d*s$/, /^-run=\^TestForgeFakeOperationContracts\$$/]) {
  assert.equal(flags.filter((flag) => pattern.test(flag)).length, 1, `precondition: Go command missing/duplicated ${pattern}`);
}
assert.equal(flags.length, 5);
console.log("# Go command:", command.join(" "));

function run(executable, args, options = {}) {
  try {
    return execFileSync(executable, args, { cwd: root, encoding: "utf8", timeout: 10000, stdio: "pipe", ...options }).trim();
  } catch (error) {
    throw new Error(`precondition/fixture command failed: ${executable} ${args.join(" ")}\n${error.stderr ?? ""}`, { cause: error });
  }
}

// Only owned children are signalled. TERM has a two-second grace, then KILL and reap.
async function stop(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  const closed = once(child, "close");
  child.kill("SIGTERM");
  const timer = setTimeout(() => child.kill("SIGKILL"), 2000);
  try { await closed; } finally { clearTimeout(timer); }
}

async function contracts(t) {
  assert.equal(typeof module.stripTypeScriptTypes, "function", "precondition: Node needs node:module.stripTypeScriptTypes (22.13+)");
  const probe = module.stripTypeScriptTypes("export class Probe { constructor(public value: number) {} }", { mode: "transform" });
  const { Probe } = await import(`data:text/javascript,${encodeURIComponent(probe)}`);
  assert.equal(new Probe(7).value, 7, "precondition: TypeScript transform must support parameter properties");
  console.log("#", run("go", ["version"], { env: goEnv }));
  for (const [tool, args] of [["git", ["--version"]], ["openssl", ["version"]]]) console.log("#", run(tool, args));

  mkdirSync(path.join(root, ".uzi/scratch"), { recursive: true });
  const scratch = mkdtempSync(path.join(root, ".uzi/scratch/operation-contracts-"));
  let fake;
  let go;
  let diagnostics = "";
  t.after(async () => {
    await stop(go);
    await stop(fake);
    rmSync(scratch, { recursive: true, force: true });
  });
  const esm = path.join(scratch, "esm");
  mkdirSync(esm);
  writeFileSync(path.join(esm, "package.json"), '{"type":"module"}\n');
  for (const name of ["forge", "util"]) {
    const source = readFileSync(path.join(root, `agent/src/${name}.ts`), "utf8");
    const transformed = module.stripTypeScriptTypes(source, { mode: "transform" });
    // Audit the complete transformed graph: a new static/dynamic dependency fails loudly.
    assert.doesNotMatch(transformed, /\bimport\s*\(/, "new dynamic dependency requires an explicit graph audit");
    const dependencies = [...transformed.matchAll(/(?:\bfrom\s*|\bimport\s*\(\s*|\bimport\s*)(["'])([^"']+)\1/g)].map((match) => match[2]);
    assert.deepEqual(dependencies, name === "forge" ? ["./util.js"] : ["node:timers/promises"], `unexpected runtime dependency in ${name}.ts`);
    writeFileSync(path.join(esm, `${name}.js`), transformed);
  }
  const { forgeClientFor, ForgeError } = await import(pathToFileURL(path.join(esm, "forge.js")).href);
  const cert = path.join(scratch, "cert.pem");
  const key = path.join(scratch, "key.pem");
  run("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1", "-keyout", key, "-out", cert]);
  const ca = readFileSync(cert);
  const gitRoot = path.join(scratch, "gitroot");
  mkdirSync(gitRoot);
  const git = (repo, ...args) => run("git", ["--git-dir", path.join(gitRoot, `${repo}.git`), ...args], {
    env: { ...process.env, GIT_AUTHOR_NAME: "Fixture", GIT_AUTHOR_EMAIL: "fixture@example.com", GIT_COMMITTER_NAME: "Fixture", GIT_COMMITTER_EMAIL: "fixture@example.com" },
  });
  for (const repo of ["repo", "repo2"]) git(repo, "init", "--bare");
  const tree = git("repo", "hash-object", "-w", "-t", "tree", "--stdin");
  const a = git("repo", "commit-tree", tree, "-m", "base A");
  const b = git("repo", "commit-tree", tree, "-p", a, "-m", "descendant B");
  const c = git("repo", "commit-tree", tree, "-p", a, "-m", "sibling C");
  for (const [ref, sha] of [["refs/heads/main", a], ["refs/heads/simple", b], ["refs/heads/agent/slash", b], ["refs/uzi-checkpoints/agent/slash", a]]) git("repo", "update-ref", ref, sha);
  const tree2 = git("repo2", "hash-object", "-w", "-t", "tree", "--stdin");
  const second = git("repo2", "commit-tree", tree2, "-m", "distinct second project");
  assert.notEqual(second, a);
  for (const branch of ["main", "simple", "agent/slash"]) git("repo2", "update-ref", `refs/heads/${branch}`, second);

  const humanNotes = (project, issue, offset) => Array.from({ length: 105 }, (_, i) => ({
    id: offset + i, project_id: project, issue_iid: issue,
    body: `project ${project}, issue ${issue}, human ${i}: café\nline two`,
    created_at: new Date(Date.UTC(2020, 0, 1, 0, 0, i)).toISOString(),
    author: { id: 10 + i % 3, username: `human-${i % 3}` },
    system: false,
  }));
  const threads = [
    { project: 1, issue: 7, notes: humanNotes(1, 7, 1000) },
    { project: 2, issue: 7, notes: humanNotes(2, 7, 2000) },
    { project: 1, issue: 8, notes: humanNotes(1, 8, 3000) },
  ];
  const statePath = path.join(scratch, "state.json");
  writeFileSync(statePath, JSON.stringify({
    issues: [7, 8, 9].map((iid) => ({ id: iid, iid, project_id: 1, title: `issue ${iid}`, state: "opened", labels: [] })),
    notes: [...threads.flatMap((thread) => thread.notes).reverse(),
      { id: 4000, project_id: 1, issue_iid: 10, system: true, body: "system activity", created_at: "2020-01-01T00:00:00.000Z", author: { id: 1, username: "system" } },
      ...humanNotes(1, 10, 5000).reverse()],
  }));
  fake = spawn(process.execPath, [path.join(root, "e2e/forge-fake/forge-fake.mjs")], {
    cwd: root, env: { PATH: process.env.PATH, FORGE_FAKE_PORT: "0", FORGE_FAKE_CERT: cert, FORGE_FAKE_KEY: key,
      FORGE_FAKE_STATE: statePath, FORGE_FAKE_BASE_URL: "https://configured.invalid",
      FORGE_FAKE_PROJECT: "group/repo", FORGE_FAKE_PROJECT2: "group/repo2", FORGE_FAKE_GIT_ROOT: gitRoot },
    stdio: ["ignore", "pipe", "pipe"],
  });
  fake.stdout.on("data", (data) => { diagnostics = (diagnostics + data).slice(-65536); });
  fake.stderr.on("data", (data) => { diagnostics = (diagnostics + data).slice(-65536); });
  const port = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => finish(new Error(`fake startup timed out\n${diagnostics}`)), 10000);
    const onData = () => { const match = diagnostics.match(/listening on :(\d+)/); if (match) finish(null, Number(match[1])); };
    const onExit = (code, signal) => finish(new Error(`fake startup exit ${code}/${signal}\n${diagnostics}`));
    const onError = (error) => finish(error);
    function finish(error, value) {
      clearTimeout(timer);
      fake.stdout.off("data", onData);
      fake.off("exit", onExit);
      fake.off("error", onError);
      if (error) reject(error); else resolve(value);
    }
    fake.stdout.on("data", onData);
    fake.once("exit", onExit);
    fake.once("error", onError);
    onData();
  });
  const endpoint = `https://127.0.0.1:${port}`;
  // Only the fetchFn seam is adapted. This does not cover DNS or default fetch.
  const fetchFn = (dialect) => async (url, init) => {
    const generated = new URL(url);
    const expectedOrigin = dialect === "github" ? "https://api.contract.invalid" : "https://contract.invalid";
    assert.equal(generated.origin, expectedOrigin, "unexpected generated API origin");
    const prefix = dialect === "gitlab" ? "/api/v4/projects/group%2Frepo/merge_requests" : dialect === "forgejo" ? "/api/v1/repos/group/repo/pulls" : "/repos/group/repo/pulls";
    assert.ok(generated.pathname === prefix || generated.pathname.startsWith(prefix + "/"), `unexpected generated API path ${generated.pathname}`);
    assert.equal(init.redirect, "error");
    const mapped = new URL((dialect === "github" ? "/api/v3" : "") + generated.pathname + generated.search, endpoint);
    return await new Promise((resolve, reject) => {
      const req = https.request(mapped, { ca, method: init.method, headers: init.headers, signal: init.signal }, (res) => {
        const chunks = [];
        let bytes = 0;
        res.on("data", (chunk) => {
          bytes += chunk.length;
          if (bytes > 8 * 1024 * 1024) res.destroy(new Error("contract response exceeds 8 MiB"));
          else chunks.push(chunk);
        });
        res.on("error", reject);
        res.on("end", () => {
          if (res.statusCode >= 300 && res.statusCode < 400) return reject(new Error("redirect refused"));
          const body = Buffer.concat(chunks);
          resolve({ status: res.statusCode, text: async () => body.toString("utf8"),
            body: new ReadableStream({ start(controller) { controller.enqueue(body); controller.close(); } }) });
        });
      });
      const timer = setTimeout(() => req.destroy(new Error("HTTPS contract request timed out")), 5000);
      req.on("close", () => clearTimeout(timer));
      req.on("error", reject);
      req.end(init.body);
    });
  };
  const mrs = [];
  for (const dialect of ["gitlab", "github", "forgejo"]) {
    await t.test(`worker ${dialect}: create/get/update-description/head (Go create/update unsupported)`, async () => {
      const source = `agent/${dialect}`;
      git("repo", "update-ref", `refs/heads/${source}`, a);
      const client = forgeClientFor(dialect, { fetchFn: fetchFn(dialect), httpTimeoutMs: 5000 });
      const repoUrl = "https://contract.invalid/group/repo";
      const pat = "contract-fixture";
      const created = await client.createMergeRequest({ repoUrl, pat, sourceBranch: source, targetBranch: "main", title: `contract ${dialect}`, description: "initial description" });
      assert.ok(Number.isSafeInteger(created.iid) && created.iid > 0);
      assert.ok(!mrs.some((mr) => mr.iid === created.iid), "distinct MR identity");
      const suffix = dialect === "gitlab" ? "/-/merge_requests/" : dialect === "github" ? "/pull/" : "/pulls/";
      const webUrl = `https://configured.invalid/group/repo${suffix}${created.iid}`;
      assert.equal(created.webUrl, webUrl);
      const initial = await client.getMergeRequest(repoUrl, pat, created.iid);
      assert.deepEqual(initial, { headSha: git("repo", "rev-parse", `refs/heads/${source}`), targetBranch: "main", description: "initial description", state: "open", title: `contract ${dialect}` });
      const description = `updated ${dialect}: café\nsecond line`;
      await client.updateMergeRequestDescription(repoUrl, pat, created.iid, description);
      assert.deepEqual(await client.getMergeRequest(repoUrl, pat, created.iid), { ...initial, description });
      git("repo", "update-ref", `refs/heads/${source}`, b);
      const head = git("repo", "rev-parse", `refs/heads/${source}`);
      assert.equal(await client.getMergeRequestHead(repoUrl, pat, created.iid), head);
      assert.deepEqual(await client.getMergeRequest(repoUrl, pat, created.iid), { ...initial, description, headSha: head });
      await assert.rejects(client.getMergeRequest(repoUrl, pat, 99999), (error) => error instanceof ForgeError && error.status === 404);
      mrs.push({ dialect, iid: created.iid, webUrl, source, target: "main", head, state: "opened" });
    });
  }
  const comparisons = [[b, a, "ahead"], [a, b, "behind"], [b, b, "equal"], [b, c, "diverged"]].map(([head, candidate, name]) => ({
    head, candidate, name, ancestor: git("repo", "merge-base", head, candidate) === candidate,
  }));
  const manifest = {
    version: 1, endpoint,
    branches: [
      { project: 1, name: "simple", sha: git("repo", "rev-parse", "refs/heads/simple") },
      { project: 1, name: "agent/slash", sha: git("repo", "rev-parse", "refs/heads/agent/slash") },
      { project: 2, name: "simple", sha: git("repo2", "rev-parse", "refs/heads/simple") },
      { project: 2, name: "agent/slash", sha: git("repo2", "rev-parse", "refs/heads/agent/slash") },
    ],
    checkpoint: git("repo", "rev-parse", "refs/uzi-checkpoints/agent/slash"),
    comparisons, mrs,
    threads: [...threads, { project: 1, issue: 10, notes: humanNotes(1, 10, 5000), gitlabOnly: true }].map(({ notes, ...thread }) => ({
      ...thread, comments: notes.map((note) => ({ authorForgeUserID: note.author.id, authorUsername: note.author.username, body: note.body, createdAt: note.created_at })),
    })),
  };
  const manifestPath = path.join(scratch, "manifest.json");
  writeFileSync(manifestPath, JSON.stringify(manifest));
  await t.test("Go Forge interface over trusted HTTPS", async () => {
    let output = "";
    const started = performance.now();
    go = spawn(command[0], command.slice(1), { cwd: path.join(root, "api"),
      env: { ...goEnv, SSL_CERT_FILE: cert, HTTPS_PROXY: "", HTTP_PROXY: "", ALL_PROXY: "", NO_PROXY: "127.0.0.1",
        UZI_FORGE_FAKE_REQUIRED: "1", UZI_FORGE_FAKE_MANIFEST: manifestPath },
      stdio: ["ignore", "pipe", "pipe"] });
    for (const stream of [go.stdout, go.stderr]) stream.on("data", (data) => {
      output += data;
      if (output.length > 2 * 1024 * 1024) go.kill("SIGKILL");
    });
    const timer = setTimeout(() => { console.error("Go child exceeded 240s including compilation"); go.kill("SIGKILL"); }, 240000);
    let code;
    try { [code] = await once(go, "close"); } finally { clearTimeout(timer); }
    console.log(output.trim().split("\n").map((line) => "# " + line).join("\n"));
    console.log(`# Go child wall ${((performance.now() - started) / 1000).toFixed(2)}s (includes compilation)`);
    assert.equal(code, 0, `Go contract failed\n${output}\nfake diagnostics:\n${diagnostics}`);
    assert.match(output, /^=== RUN\s+TestForgeFakeOperationContracts$/m);
    assert.match(output, /^--- PASS: TestForgeFakeOperationContracts /m);
    assert.doesNotMatch(output, /--- SKIP:/, "required Go contract must have zero skips");
  });
}

// Select TAP explicitly: Node versions differ in their default reporter.
const results = runTests({ files: [], isolation: "none", setup() {
  test("production forge operation contracts against offline HTTPS fake", { timeout: 330000 }, contracts);
} });
results.on("test:fail", () => { process.exitCode = 1; });
results.compose(tap).pipe(process.stdout);
