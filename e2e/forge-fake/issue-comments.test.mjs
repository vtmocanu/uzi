import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import { once } from "node:events";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import https from "node:https";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const root = fileURLToPath(new URL("../../", import.meta.url));
const bot = { id: 1, login: "uzi-bot" };
const reviewer = { id: 2, login: "reviewer" };
const permissions = { pull: true, triage: false, push: true, maintain: false, admin: false };
const timestamp = (day) => `2020-01-${String(day).padStart(2, "0")}T00:00:00.000Z`;
const note = (id, project_id, issue_iid, day, author = { id: 1, username: "uzi-bot" }) => ({
  id, project_id, issue_iid, body: `note ${id}`, created_at: timestamp(day),
  author,
});

test("shared fake HTTPS comment and author contracts", { timeout: 30000 }, async (t) => {
  mkdirSync(path.join(root, ".uzi/scratch"), { recursive: true });
  const scratch = mkdtempSync(path.join(root, ".uzi/scratch/issue-comments-"));
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
  const legacy = { id: 11, issue_iid: 7, body: "legacy", created_at: timestamp(1) };
  const fixtures = [
    note(15, 1, 7, 4), note(13, 1, 7, 2), note(12, 1, 7, 2),
    note(14, 1, 7, 3, { id: 2, username: "reviewer" }), legacy,
    note(25, 2, 7, 5), note(23, 2, 7, 3), note(21, 2, 7, 1),
    note(24, 2, 7, 4), note(22, 2, 7, 2), note(30, 1, 8, 1),
    ...Array.from({ length: 105 }, (_, i) => note(100 + i, 1, 9, 1)),
  ];
  const statePath = path.join(scratch, "state.json");
  writeFileSync(statePath, JSON.stringify({ notes: fixtures }));
  let diagnostics = "";
  child = spawn(process.execPath, [path.join(root, "e2e/forge-fake/forge-fake.mjs")], {
    cwd: root,
    env: {
      PATH: process.env.PATH,
      FORGE_FAKE_PORT: "0", FORGE_FAKE_CERT: certPath, FORGE_FAKE_KEY: keyPath,
      FORGE_FAKE_STATE: statePath, FORGE_FAKE_BASE_URL: "https://configured.invalid",
      FORGE_FAKE_PROJECT: "group/repo", FORGE_FAKE_PROJECT2: "group/repo2",
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
  async function request(route, method = "GET", body) {
    try {
      return await new Promise((resolve, reject) => {
        const req = https.request(new URL(route, origin), {
          method, ca, headers: { "Content-Type": "application/json" },
        }, (res) => {
          let data = "";
          res.setEncoding("utf8");
          res.on("data", (chunk) => { data += chunk; });
          res.on("error", reject);
          res.on("end", () => {
            try { resolve({ status: res.statusCode, headers: res.headers, body: JSON.parse(data) }); }
            catch (error) { reject(error); }
          });
        });
        req.setTimeout(3000, () => req.destroy(new Error("HTTP request timed out")));
        req.on("error", reject);
        req.end(body === undefined ? undefined : JSON.stringify(body));
      });
    } catch (error) {
      throw new Error(`request ${method} ${route} failed; child ${child.exitCode}/${child.signalCode}\n${diagnostics}`, { cause: error });
    }
  }
  const expectedComment = (fixture) => ({
    id: fixture.id, body: fixture.body,
    user: fixture.author ? { id: fixture.author.id, login: fixture.author.login ?? fixture.author.username } : bot,
    created_at: fixture.created_at, updated_at: fixture.created_at,
  });
  async function get(route) {
    const response = await request(route);
    assert.equal(response.status, 200, route);
    return response;
  }

  // HTTP contracts only: projection.go skips bot comments in assessment.
  // github_issues.go ListComments uses PerPage=100 and resp.NextPage;
  // forgejo_issues.go ListIssueComments uses PageSize=50 and resp.NextPage.
  const roundtrips = { "group/repo": [], "group/repo2": [] };
  for (const [dialect, sizeKey, driverSize] of [["v1", "limit", 50], ["v3", "per_page", 100]]) {
    await t.test(`${dialect} scoping, ordering, pagination and shared POST roundtrip`, async () => {
      for (const [slug, ids] of [["group/repo", [11, 12, 13, 14, 15]], ["group/repo2", [21, 22, 23, 24, 25]]]) {
        const route = `/api/${dialect}/repos/${slug}/issues/7/comments`;
        for (let page = 1; page <= 4; page++) {
          const query = `?keep=a%2Fb&${sizeKey}=2&page=${page}`;
          const response = await get(route + query);
          const expected = ids.slice((page - 1) * 2, page * 2).map((id) => expectedComment(fixtures.find((n) => n.id === id)));
          assert.deepEqual(response.body, expected);
          if (page < 3) {
            const next = new URL(route + query, origin);
            next.searchParams.set("page", String(page + 1));
            assert.equal(response.headers.link, `<${next.href}>; rel="next"`);
            assert.deepEqual((await get(next.href)).body, ids.slice(page * 2, page * 2 + 2).map((id) => expectedComment(fixtures.find((n) => n.id === id))));
          } else assert.equal(response.headers.link, undefined);
        }
        assert.deepEqual((await get(route + `?${sizeKey}=${driverSize}`)).body, ids.map((id) => expectedComment(fixtures.find((n) => n.id === id))));
      }
      const base = `/api/${dialect}/repos/group/repo`;
      assert.deepEqual((await get(base + "/issues/8/comments")).body, [expectedComment(fixtures.find((n) => n.id === 30))]);
      assert.deepEqual((await get(base + "/issues/999/comments")).body, []);
      for (const query of [`page=0`, "page=-1", "page=1.5", "page=abc", "page=", "page=9007199254740992", `${sizeKey}=0`, `${sizeKey}=-2`, `${sizeKey}=1.5`, `${sizeKey}=bad`]) {
        assert.equal((await request(base + "/issues/7/comments?" + query)).status, 400, query);
      }
      // Exercise the actual driver page sizes as well as the small three-page fixture.
      for (let page = 1; page <= Math.ceil(105 / driverSize) + 1; page++) {
        const route = base + `/issues/9/comments?${sizeKey}=${driverSize}&page=${page}`;
        const response = await get(route);
        const start = (page - 1) * driverSize;
        const ids = Array.from({ length: 105 }, (_, i) => 100 + i).slice(start, start + driverSize);
        assert.deepEqual(response.body, ids.map((id) => expectedComment(fixtures.find((n) => n.id === id))));
        if (start + driverSize < 105) {
          const next = new URL(route, origin);
          next.searchParams.set("page", String(page + 1));
          assert.equal(response.headers.link, `<${next.href}>; rel="next"`);
        } else assert.equal(response.headers.link, undefined);
      }
      const bounded = await get(base + `/issues/9/comments?${sizeKey}=1000`);
      assert.equal(bounded.body.length, 100);
      assert.deepEqual(bounded.body.map((n) => n.id), Array.from({ length: 100 }, (_, i) => 100 + i));
      const tail = await get(bounded.headers.link.match(/^<([^>]+)>/)[1]);
      assert.deepEqual(tail.body.map((n) => n.id), [200, 201, 202, 203, 204]);
      assert.equal(tail.headers.link, undefined);
      for (const slug of ["group/repo", "group/repo2"]) {
        const route = `/api/${dialect}/repos/${slug}/issues/70/comments`;
        const before = Date.now();
        const posted = await request(route, "POST", { body: `${dialect} ${slug}` });
        assert.equal(posted.status, 201);
        assert.deepEqual(posted.body.user, bot);
        assert.equal(posted.body.body, `${dialect} ${slug}`);
        assert.equal(posted.body.updated_at, posted.body.created_at);
        assert.equal(new Date(posted.body.created_at).toISOString(), posted.body.created_at);
        assert.ok(Date.parse(posted.body.created_at) >= before && Date.parse(posted.body.created_at) <= Date.now());
        assert.ok(Number.isSafeInteger(posted.body.id));
        roundtrips[slug].push(posted.body);
        const otherDialect = dialect === "v1" ? "v3" : "v1";
        assert.deepEqual((await get(route)).body, roundtrips[slug]);
        const read = await get(route.replace(`/${dialect}/`, `/${otherDialect}/`));
        assert.deepEqual(read.body, roundtrips[slug]);
        const shared = (await get("/_e2e/state")).body.notes.find((n) => n.id === posted.body.id);
        assert.equal(shared.project_id, slug === "group/repo" ? 1 : 2);
        assert.deepEqual(shared.author, { id: 1, username: "uzi-bot" });
      }
      for (const suffix of ["/issues/7/comments", "/collaborators", ...(dialect === "v1" ? ["/teams"] : [])]) {
        const unknown = await request(`/api/${dialect}/repos/group/unknown${suffix}`);
        assert.equal(unknown.status, 404);
        assert.deepEqual(unknown.body, { message: "Not Found" });
      }
    });
  }

  for (const dialect of ["v1", "v3"]) {
    await t.test(`${dialect} reviewer comment identity and membership`, async () => {
      const comments = (await get(`/api/${dialect}/repos/group/repo/issues/7/comments`)).body;
      const author = comments.find((comment) => comment.id === 14).user;
      assert.deepEqual(author, reviewer);
      const identity = dialect === "v3"
        ? (await get(`/api/v3/user/${author.id}`)).body
        : (await get(`/api/v1/users/search?uid=${author.id}`)).body.data[0];
      assert.deepEqual(identity, author);
      for (const slug of ["group/repo", "group/repo2"]) {
        const collaborators = (await get(`/api/${dialect}/repos/${slug}/collaborators`)).body;
        const member = collaborators.find((user) => user.id === identity.id && user.login === identity.login);
        assert.deepEqual(member, dialect === "v3" ? { ...identity, permissions } : identity);
        if (dialect === "v1") {
          assert.deepEqual((await get(`/api/v1/repos/${slug}/collaborators/${identity.login}/permission`)).body,
            { permission: "write", role_name: "Write", user: identity });
        }
      }
    });
  }

  await t.test("positive bot author evidence, terminal collaborators and teams", async () => {
    // github_author.go uses singular Users.GetByID and all five permission pointers.
    assert.deepEqual((await get("/api/v3/user/1")).body, bot);
    // forgejo_author.go resolves uid, repository, teams, direct collaborators and permission.
    assert.deepEqual((await get("/api/v1/users/search?uid=1")).body, { data: [bot] });
    assert.deepEqual((await get("/api/v1/users/search?uid=999")).body, { data: [] });
    assert.equal((await request("/api/v3/user/999")).status, 404);
    for (const dialect of ["v1", "v3"]) {
      for (const [id, slug] of [[1, "group/repo"], [2, "group/repo2"]]) {
        const repo = (await get(`/api/${dialect}/repositories/${id}`)).body;
        assert.equal(repo.id, id);
        assert.equal(repo.full_name, slug);
        assert.deepEqual(repo.owner, { id: 1, login: "group" });
        // The owner's login differs from the bot: this proves no personal-owner eligibility.
        assert.notEqual(repo.owner.login, bot.login);
        const sizeKey = dialect === "v1" ? "limit" : "per_page";
        const route = `/api/${dialect}/repos/${slug}/collaborators`;
        const collaborators = [bot, reviewer].map((user) => dialect === "v3" ? { ...user, permissions } : user);
        for (const page of [1, 2, 8]) {
          const response = await get(route + `?${sizeKey}=${dialect === "v1" ? 50 : 100}&page=${page}`);
          assert.deepEqual(response.body, page === 1 ? collaborators : []);
          assert.equal(response.headers.link, undefined);
        }
        for (const page of [1, 2, 3]) {
          const response = await get(route + `?${sizeKey}=1&page=${page}`);
          assert.deepEqual(response.body, collaborators.slice(page - 1, page));
          if (page === 1) {
            const next = new URL(route + `?${sizeKey}=1&page=2`, origin);
            assert.equal(response.headers.link, `<${next.href}>; rel="next"`);
            assert.deepEqual((await get(next.href)).body, [collaborators[1]]);
          } else assert.equal(response.headers.link, undefined);
        }
        if (dialect === "v1") {
          const teams = await get(`/api/v1/repos/${slug}/teams?limit=50`);
          assert.deepEqual(teams.body, []);
          assert.equal(teams.headers.link, undefined);
          assert.deepEqual((await get(`/api/v1/repos/${slug}/collaborators/uzi-bot/permission`)).body,
            { permission: "write", role_name: "Write", user: bot });
          assert.deepEqual((await get(`/api/v1/repos/${slug}/collaborators/unrelated/permission`)).body,
            { permission: "write", role_name: "Write", user: bot });
        }
      }
      // Retain the existing numeric repository fallback.
      assert.equal((await get(`/api/${dialect}/repositories/999`)).body.id, 1);
    }
  });

  await t.test("GitLab notes remain shared and unchanged", async () => {
    const route = "/api/v4/projects/2/issues/80/notes";
    const posted = await request(route, "POST", { body: "gitlab shared note" });
    assert.equal(posted.status, 201);
    assert.deepEqual(posted.body.author, { id: 1, username: "uzi-bot" });
    assert.deepEqual((await get(route + "?sort=asc")).body, [posted.body]);
    for (const dialect of ["v1", "v3"]) {
      assert.deepEqual((await get(`/api/${dialect}/repos/group/repo2/issues/80/comments`)).body, [expectedComment(posted.body)]);
      assert.deepEqual((await get(`/api/${dialect}/repos/group/repo/issues/80/comments`)).body, []);
    }
    const shared = (await get("/_e2e/state")).body.notes;
    assert.deepEqual(shared.slice(0, fixtures.length), fixtures);
    assert.deepEqual(JSON.parse(readFileSync(statePath, "utf8")).notes, shared);
  });
});
