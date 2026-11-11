import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import { once } from "node:events";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import https from "node:https";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

// Per-iid MR/PR GET on the shared forge fake, as the ci_fix creation binding
// (workersvc.resolveCIFixBinding -> GetMergeRequestSummary) reads it: state, source
// branch and iid for GitLab; state/merged, head.ref and number for Forgejo and GitHub.
const root = fileURLToPath(new URL("../../", import.meta.url));
const mr = (iid, source_branch, state) => ({
  id: 5000 + iid, iid, project_id: 1, source_branch, target_branch: "main",
  title: `MR ${iid}`, description: "", state,
  web_url: `https://configured.invalid/group/repo/-/merge_requests/${iid}`,
});

test("shared fake serves a single MR/PR by iid in all three dialects", { timeout: 30000 }, async (t) => {
  mkdirSync(path.join(root, ".uzi/scratch"), { recursive: true });
  const scratch = mkdtempSync(path.join(root, ".uzi/scratch/mr-get-"));
  let child;
  t.after(async () => {
    if (child && child.exitCode === null && child.signalCode === null) {
      const exited = once(child, "exit");
      child.kill("SIGTERM");
      const timer = setTimeout(() => child.kill("SIGKILL"), 2000);
      try { await exited; } finally { clearTimeout(timer); }
    }
    rmSync(scratch, { recursive: true, force: true });
  });
  const certPath = path.join(scratch, "cert.pem");
  const keyPath = path.join(scratch, "key.pem");
  try {
    execFileSync("openssl", [
      "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
      "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1",
      "-keyout", keyPath, "-out", certPath,
    ], { timeout: 10000, stdio: "pipe" });
  } catch (error) {
    throw new Error("forge-fake tests require OpenSSL with req -addext support", { cause: error });
  }
  const statePath = path.join(scratch, "state.json");
  writeFileSync(statePath, JSON.stringify({
    mrs: [mr(1, "agent/open", "opened"), mr(2, "agent/merged", "merged"), mr(3, "agent/closed", "closed"),
      { ...mr(4, "agent/open", "opened"), project_id: 2 },
      // Fork MRs into project 1: one reusing an own MR's branch name, one alone on its branch.
      { ...mr(5, "agent/open", "opened"), source_project_id: 99 },
      { ...mr(6, "agent/forkonly", "opened"), source_project_id: 99 }],
  }));
  const gitRoot = path.join(scratch, "gitroot");
  mkdirSync(gitRoot);
  const restoreRepos = ["restore45a", "restore45b", "restore52a", "restore52b"];
  const restoreHeads = new Map();
  for (const name of ["repo", "repo2", ...restoreRepos]) {
    const bare = path.join(gitRoot, `${name}.git`);
    const git = (...args) => execFileSync("git", ["--git-dir", bare, ...args], {
      encoding: "utf8", input: "", env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    }).trim();
    execFileSync("git", ["init", "--bare", "-q", bare]);
    const tree = git("hash-object", "-w", "-t", "tree", "--stdin");
    const head = git("-c", "user.name=fixture", "-c", "user.email=fixture@e2e.invalid", "commit-tree", tree, "-m", name);
    git("update-ref", "refs/heads/main", head);
    restoreHeads.set(name, head);
  }
  let diagnostics = "";
  child = spawn(process.execPath, [path.join(root, "e2e/forge-fake/forge-fake.mjs")], {
    cwd: root,
    env: {
      PATH: process.env.PATH,
      FORGE_FAKE_PORT: "0", FORGE_FAKE_CERT: certPath, FORGE_FAKE_KEY: keyPath,
      FORGE_FAKE_STATE: statePath, FORGE_FAKE_BASE_URL: "https://configured.invalid",
      FORGE_FAKE_PROJECT: "group/repo", FORGE_FAKE_PROJECT2: "group/repo2",
      FORGE_FAKE_PROJECT3: "group/restore45a", FORGE_FAKE_PROJECT4: "group/restore45b",
      FORGE_FAKE_PROJECT5: "group/restore52a", FORGE_FAKE_PROJECT6: "group/restore52b",
      FORGE_FAKE_GIT_ROOT: gitRoot,
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  child.stdout.on("data", (data) => { diagnostics += data; });
  child.stderr.on("data", (data) => { diagnostics += data; });
  const port = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => finish(new Error(`fake startup timed out\n${diagnostics}`)), 10000);
    const onData = () => {
      const match = diagnostics.match(/listening on :(\d+)/);
      if (match) finish(null, Number(match[1]));
    };
    const onExit = (code, signal) => finish(new Error(`fake exited during startup: ${code}/${signal}\n${diagnostics}`));
    const onError = (error) => finish(error);
    function finish(error, value) {
      clearTimeout(timer);
      child.stdout.off("data", onData);
      child.off("exit", onExit);
      child.off("error", onError);
      if (error) reject(error); else resolve(value);
    }
    child.stdout.on("data", onData);
    child.once("exit", onExit);
    child.once("error", onError);
    onData();
  });
  assert.ok(port > 0);
  const origin = `https://127.0.0.1:${port}`;
  const ca = readFileSync(certPath);
  const get = (route) => call("GET", route);
  async function call(method, route, payload) {
    try {
      return await new Promise((resolve, reject) => {
        const headers = payload === undefined ? {} : { "content-type": "application/json" };
        const req = https.request(new URL(route, origin), { method, ca, headers }, (res) => {
          let data = "";
          res.setEncoding("utf8");
          res.on("data", (chunk) => { data += chunk; });
          res.on("error", reject);
          res.on("end", () => {
            try { resolve({ status: res.statusCode, body: JSON.parse(data) }); } catch (error) { reject(error); }
          });
        });
        req.setTimeout(3000, () => req.destroy(new Error("HTTP request timed out")));
        req.on("error", reject);
        req.end(payload === undefined ? undefined : JSON.stringify(payload));
      });
    } catch (error) {
      throw new Error(`${method} ${route} failed; child ${child.exitCode}/${child.signalCode}\n${diagnostics}`, { cause: error });
    }
  }

  await t.test("independent-restore fixtures advertise distinct projects and smart-HTTP bares", async () => {
    for (const [index, name] of restoreRepos.entries()) {
      const res = await get(`/api/v4/projects/${index + 3}`);
      assert.equal(res.status, 200);
      assert.equal(res.body.path_with_namespace, `group/${name}`);
      const output = execFileSync("git", ["-c", `http.sslCAInfo=${certPath}`, "ls-remote", `${origin}/group/${name}.git`, "refs/heads/main"], {
        encoding: "utf8", timeout: 10000,
        env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
      });
      assert.equal(output.trim().split(/\s+/)[0], restoreHeads.get(name));
    }
    assert.equal(new Set(restoreRepos.map(name => restoreHeads.get(name))).size, 4);
    assert.throws(() => execFileSync("git", ["-c", `http.sslCAInfo=${certPath}`, "ls-remote", `${origin}/group/no-such-fixture.git`, "refs/heads/main"], {
      encoding: "utf8", timeout: 10000, stdio: "pipe",
      env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    }), "an unknown project must not be served from another fixture's bare");
  });

  await t.test("GitLab /merge_requests/:iid returns state, source_branch and iid", async () => {
    const route = "/api/v4/projects/1/merge_requests";
    for (const [iid, branch, state] of [[1, "agent/open", "opened"], [2, "agent/merged", "merged"], [3, "agent/closed", "closed"]]) {
      const res = await get(`${route}/${iid}`);
      assert.equal(res.status, 200, `iid ${iid}`);
      assert.equal(res.body.iid, iid);
      assert.equal(res.body.state, state);
      assert.equal(res.body.source_branch, branch);
    }
    assert.equal((await get(`${route}/999`)).status, 404);
  });

  await t.test("GitLab /merge_requests list is project-scoped and carries source/target project ids", async () => {
    // The worker's duplicate-create lookup (GitLabClient.findOpenMr) adopts only an
    // own-project MR, so each listed row must name its source and target project.
    const res = await get("/api/v4/projects/1/merge_requests?state=opened&source_branch=agent%2Fopen&target_branch=main");
    assert.equal(res.status, 200);
    // The fork MR !5 reuses the branch name: listed (as GitLab does) with its own source project.
    assert.deepEqual(res.body.map((m) => [m.iid, m.source_project_id, m.target_project_id]), [[1, 1, 1], [5, 99, 1]]);
    const other = await get("/api/v4/projects/2/merge_requests?state=opened&source_branch=agent%2Fopen&target_branch=main");
    assert.deepEqual(other.body.map((m) => [m.iid, m.source_project_id, m.target_project_id]), [[4, 2, 2]]);
  });

  await t.test("GitLab MR create 409s only on an open own-source MR, not on a fork MR", async () => {
    // GitLab's duplicate-create conflict is per source project: a fork MR on the same branch
    // name must not manufacture the 409 that sends the worker into findOpenMr.
    const route = "/api/v4/projects/1/merge_requests";
    const dup = await call("POST", route, { source_branch: "agent/open", target_branch: "main", title: "dup" });
    assert.equal(dup.status, 409);
    const forkOnly = await call("POST", route, { source_branch: "agent/forkonly", target_branch: "main", title: "ours" });
    assert.equal(forkOnly.status, 201);
    assert.equal(forkOnly.body.project_id, 1);
    const list = await get(`${route}?state=opened&source_branch=agent%2Fforkonly&target_branch=main`);
    assert.deepEqual(list.body.map((m) => [m.iid, m.source_project_id]), [[6, 99], [forkOnly.body.iid, 1]]);
  });

  for (const [dialect, name, notFound] of [["v1", "Forgejo", "pull request does not exist"], ["v3", "GitHub", "Not Found"]]) {
    await t.test(`${name} /pulls/:n returns state, merged, head.ref and number`, async () => {
      const route = `/api/${dialect}/repos/group/repo/pulls`;
      const open = await get(`${route}/1`);
      assert.equal(open.status, 200);
      assert.equal(open.body.number, 1);
      assert.equal(open.body.state, "open");
      assert.equal(open.body.merged, false);
      assert.equal(open.body.head.ref, "agent/open");
      // Merged and closed-unmerged are both state "closed"; only `merged` tells them apart.
      const merged = await get(`${route}/2`);
      assert.equal(merged.status, 200);
      assert.equal(merged.body.number, 2);
      assert.equal(merged.body.state, "closed");
      assert.equal(merged.body.merged, true);
      assert.equal(merged.body.head.ref, "agent/merged");
      const closed = await get(`${route}/3`);
      assert.equal(closed.body.state, "closed");
      assert.equal(closed.body.merged, false);
      const missing = await get(`${route}/999`);
      assert.equal(missing.status, 404);
      assert.equal(missing.body.message, notFound);
    });
  }
});
