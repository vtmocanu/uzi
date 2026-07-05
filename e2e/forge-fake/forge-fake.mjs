// forge-fake — a tiny stand-in for a GitLab instance, used only by the M6 E2E
// harness. It serves, over HTTPS on :443, the subset of the GitLab v4 REST API
// that the uzi api (issue create/get, token verify, project + issue listing) and
// the uzi worker (merge-request create/list) actually call, plus an
// introspection endpoint the harness reads to assert what was recorded.
//
// It is deliberately NOT a real forge: there is exactly one project, auth is not
// checked (the harness uses dummy credentials per the testing-credentials
// policy), and everything is in-memory. The git remote itself is a local bare
// repo the worker reaches via a `url.insteadOf` rewrite (see the compose
// overlay), so this service never speaks the git wire protocol.
//
// HTTPS is mandatory: the worker refuses to send the PAT to a non-https GitLab
// base (gitlab.ts), and the api's SSRF allowlist requires https bases — so this
// fake genuinely exercises those guards. The self-signed cert is trusted by the
// api (SSL_CERT_FILE), the worker's fetch (NODE_EXTRA_CA_CERTS), and git
// (GIT_SSL_CAINFO), all wired in the overlay.

import https from "node:https";
import fs from "node:fs";
import { spawn, execFileSync } from "node:child_process";

const PORT = Number(process.env.FORGE_FAKE_PORT || 443);
const CERT = process.env.FORGE_FAKE_CERT || "/certs/cert.pem";
const KEY = process.env.FORGE_FAKE_KEY || "/certs/key.pem";
const STATE_FILE = process.env.FORGE_FAKE_STATE || "/tmp/forge-fake-state.json";
const BASE = process.env.FORGE_FAKE_BASE_URL || "https://forge-fake.e2e";

// The single project this fake serves. path_with_namespace + web_url must match
// what the harness seeds (UZI_SEED_FORGE_REPOS) and what the worker derives the
// MR/clone URLs from.
const PROJECT = {
  id: 1,
  path_with_namespace: process.env.FORGE_FAKE_PROJECT || "group/repo",
  default_branch: "main",
};
PROJECT.web_url = `${BASE}/${PROJECT.path_with_namespace}`;

// --- git smart-HTTP (optional; the E2E_GIT_SMART_HTTP variant) ----------------
// Serves the single bare repo under GIT_ROOT via `git http-backend`, gated on an
// HTTP Basic credential. This lets the harness assert the worker sends
// git-over-HTTPS *Basic* auth (the M6 fix) — which the default local-path remote
// cannot exercise. Dormant in the default harness (the worker never git-talks to
// forge-fake there). Requires git in the image (see Dockerfile).
const GIT_ROOT = process.env.FORGE_FAKE_GIT_ROOT || "/gitroot";
const GIT_EXEC_PATH = (() => {
  try {
    return execFileSync("git", ["--exec-path"], { encoding: "utf8" }).trim();
  } catch {
    return "/usr/lib/git-core";
  }
})();
const GIT_HTTP_BACKEND = `${GIT_EXEC_PATH}/git-http-backend`;
const EXPECT_USER = process.env.FORGE_FAKE_EXPECT_USER || "";
const EXPECT_PAT = process.env.FORGE_FAKE_EXPECT_PAT || "";

const state = {
  issues: /** @type {Record<number, any>} */ ({}),
  mrs: /** @type {any[]} */ ([]),
  nextIssueIid: 1,
  nextMrIid: 1,
};

function persist() {
  try {
    fs.writeFileSync(
      STATE_FILE,
      JSON.stringify({ issues: Object.values(state.issues), mrs: state.mrs }, null, 2),
    );
  } catch {
    /* best-effort introspection sink */
  }
}

function log(...args) {
  console.log(new Date().toISOString(), "[forge-fake]", ...args);
}

function makeIssue({ iid, title, description, labels }) {
  return {
    id: 1000 + iid,
    iid,
    project_id: PROJECT.id,
    title,
    description: description ?? "",
    state: "opened",
    labels: labels && labels.length ? labels : ["PRD"],
    web_url: `${PROJECT.web_url}/-/issues/${iid}`,
    author: { id: 1, username: "uzi-bot" },
    updated_at: new Date().toISOString(),
    created_at: new Date().toISOString(),
  };
}

function readBody(req) {
  return new Promise((resolve) => {
    let buf = "";
    req.on("data", (c) => (buf += c));
    req.on("end", () => {
      try {
        resolve(buf ? JSON.parse(buf) : {});
      } catch {
        resolve({});
      }
    });
  });
}

function send(res, status, body) {
  const payload = body === undefined ? "" : JSON.stringify(body);
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(payload);
}

// labels may arrive as a JSON array or a comma-joined string, depending on the
// client-go version; normalize to an array.
function normLabels(v) {
  if (Array.isArray(v)) return v.map(String);
  if (typeof v === "string" && v.trim()) return v.split(",").map((s) => s.trim());
  return [];
}

// Bridge one request to `git http-backend` (CGI). Streams the request body to
// the backend's stdin and parses its CGI response (headers until a blank line,
// then a binary body) back onto the HTTP response.
function handleGit(req, res, url) {
  if (EXPECT_PAT && !basicAuthOk(req)) {
    res.writeHead(401, { "WWW-Authenticate": 'Basic realm="forge-fake"', "Content-Type": "text/plain" });
    res.end("401 (forge-fake git: missing/invalid HTTP Basic credential)");
    log("git 401", req.method, url.pathname);
    return;
  }
  // Map any /<ns…>/<repo>.git/<rest> onto the single bare repo GIT_ROOT/repo.git.
  const rest = url.pathname.replace(/^.*?\.git/, "");
  const cgi = spawn(GIT_HTTP_BACKEND, [], {
    env: {
      ...process.env,
      GIT_PROJECT_ROOT: GIT_ROOT,
      GIT_HTTP_EXPORT_ALL: "1",
      PATH_INFO: `/repo.git${rest}`,
      REQUEST_METHOD: req.method || "GET",
      QUERY_STRING: url.search.replace(/^\?/, ""),
      CONTENT_TYPE: req.headers["content-type"] || "",
      CONTENT_LENGTH: req.headers["content-length"] || "",
      REMOTE_USER: EXPECT_USER || "anon",
      REMOTE_ADDR: req.socket.remoteAddress || "127.0.0.1",
    },
  });
  req.pipe(cgi.stdin);
  let buf = Buffer.alloc(0);
  let headersDone = false;
  cgi.stdout.on("data", (d) => {
    if (headersDone) return void res.write(d);
    buf = Buffer.concat([buf, d]);
    let sepLen = 4;
    let idx = buf.indexOf("\r\n\r\n");
    if (idx < 0) {
      idx = buf.indexOf("\n\n");
      sepLen = 2;
    }
    if (idx < 0) return;
    headersDone = true;
    const rawHeaders = buf.slice(0, idx).toString("utf8");
    const body = buf.slice(idx + sepLen);
    let status = 200;
    const outHeaders = {};
    for (const line of rawHeaders.split(/\r?\n/)) {
      const c = line.indexOf(":");
      if (c < 0) continue;
      const k = line.slice(0, c).trim();
      const v = line.slice(c + 1).trim();
      if (k.toLowerCase() === "status") status = parseInt(v, 10) || 200;
      else outHeaders[k] = v;
    }
    res.writeHead(status, outHeaders);
    if (body.length) res.write(body);
  });
  cgi.stdout.on("end", () => res.end());
  cgi.stderr.on("data", (d) => log("git-http-backend:", d.toString().trim()));
  cgi.on("error", (e) => {
    log("git backend spawn error", e.message);
    if (!res.headersSent) res.writeHead(500);
    res.end();
  });
}

// The Authorization: Basic credential must decode to EXPECT_USER:EXPECT_PAT. This
// is the regression guard: git-over-HTTPS must send Basic (the M6 auth fix), not
// GitLab's REST-only PRIVATE-TOKEN.
function basicAuthOk(req) {
  const m = /^Basic\s+(.+)$/i.exec(req.headers["authorization"] || "");
  if (!m) return false;
  const decoded = Buffer.from(m[1], "base64").toString("utf8");
  const i = decoded.indexOf(":");
  const user = i >= 0 ? decoded.slice(0, i) : "";
  const pass = i >= 0 ? decoded.slice(i + 1) : "";
  if (EXPECT_USER && user !== EXPECT_USER) return false;
  return pass === EXPECT_PAT;
}

const server = https.createServer(
  { cert: fs.readFileSync(CERT), key: fs.readFileSync(KEY) },
  async (req, res) => {
    const method = req.method || "GET";
    const url = new URL(req.url || "/", BASE);
    const path = url.pathname;

    // git smart-HTTP (before the JSON routes): any *.git path.
    if (/\.git(\/|$)/.test(path)) return handleGit(req, res, url);

    // --- introspection (harness reads this to assert recorded state) ---------
    if (method === "GET" && path === "/_e2e/state") {
      return send(res, 200, { issues: Object.values(state.issues), mrs: state.mrs, project: PROJECT });
    }
    if (method === "GET" && (path === "/_e2e/health" || path === "/")) {
      return send(res, 200, { ok: true });
    }

    // --- GitLab v4 subset -----------------------------------------------------
    // Token verify (api seed + connect): CurrentUser. is_admin is omitted (a
    // compliant, non-admin bot) — GitLab only returns it for an admin caller.
    if (method === "GET" && path === "/api/v4/user") {
      return send(res, 200, { id: 1, username: "uzi-bot", name: "uzi bot" });
    }

    // Token introspection (PRD #5 privilege check). Over-privileged iff the PAT
    // itself signals it (contains "overpriv"), so the harness can drive both the
    // compliant seed PAT and a rejected over-privileged one against one fake.
    if (method === "GET" && path === "/api/v4/personal_access_tokens/self") {
      const tok = req.headers["private-token"] || "";
      const scopes = tok.includes("overpriv") ? ["api", "sudo"] : ["api"];
      return send(res, 200, { id: 1, name: "uzi-bot", revoked: false, active: true, scopes, expires_at: null });
    }

    // Project listing (api seed / repo discovery).
    if (method === "GET" && path === "/api/v4/projects") {
      return send(res, 200, [PROJECT]);
    }

    // Everything below is scoped to a project: /api/v4/projects/:id/<rest>. :id
    // is numeric (api client-go) or a url-encoded path (worker MR); one project,
    // so the id value is ignored.
    const proj = path.match(/^\/api\/v4\/projects\/[^/]+(\/.*)?$/);
    if (proj) {
      const rest = proj[1] || "";

      // Issues.
      const issueGet = rest.match(/^\/issues\/(\d+)$/);
      if (method === "GET" && issueGet) {
        const issue = state.issues[Number(issueGet[1])];
        return issue ? send(res, 200, issue) : send(res, 404, { message: "404 Not found" });
      }
      if (method === "PUT" && issueGet) {
        // UpdateIssue (label moves): echo back, best-effort.
        const issue = state.issues[Number(issueGet[1])];
        if (!issue) return send(res, 404, { message: "404 Not found" });
        issue.updated_at = new Date().toISOString();
        persist();
        return send(res, 200, issue);
      }
      if (method === "GET" && rest === "/issues") {
        // ListProjectIssues (poller/sync). Return all recorded issues; the caller
        // filters by label. Keeps a reconcile pass from evicting the cache.
        return send(res, 200, Object.values(state.issues));
      }
      if (method === "POST" && rest === "/issues") {
        const body = await readBody(req);
        const iid = state.nextIssueIid++;
        const labels = normLabels(body.labels);
        const issue = makeIssue({ iid, title: body.title || `issue ${iid}`, description: body.description, labels });
        state.issues[iid] = issue;
        persist();
        log("issue created", iid, JSON.stringify(issue.title));
        return send(res, 201, issue);
      }

      // Privilege check (PRD #5): effective membership + protected-branch shape.
      // Compliant fixtures: the bot is a Developer (30) on a protected default
      // branch whose only push access level is Maintainer (40) — no Developer or
      // per-user push, so the checker reports least-privilege.
      const memberAll = rest.match(/^\/members\/all\/(\d+)$/);
      if (method === "GET" && memberAll) {
        return send(res, 200, { id: Number(memberAll[1]), username: "uzi-bot", access_level: 30 });
      }
      const protBranch = rest.match(/^\/protected_branches\/(.+)$/);
      if (method === "GET" && protBranch) {
        return send(res, 200, {
          id: 1,
          name: decodeURIComponent(protBranch[1]),
          push_access_levels: [{ access_level: 40 }],
        });
      }

      // Labels (defensive; not on the harness critical path).
      if (method === "GET" && rest === "/labels") return send(res, 200, []);
      if (method === "POST" && rest === "/labels") {
        const body = await readBody(req);
        return send(res, 201, { id: 1, name: body.name || "label", color: body.color || "#cccccc" });
      }

      // Merge requests.
      if (method === "POST" && rest === "/merge_requests") {
        const body = await readBody(req);
        const iid = state.nextMrIid++;
        const mr = {
          id: 5000 + iid,
          iid,
          project_id: PROJECT.id,
          source_branch: body.source_branch,
          target_branch: body.target_branch,
          title: body.title,
          description: body.description,
          state: "opened",
          web_url: `${PROJECT.web_url}/-/merge_requests/${iid}`,
        };
        state.mrs.push(mr);
        persist();
        log("MR created", iid, mr.source_branch, "->", mr.target_branch);
        return send(res, 201, mr);
      }
      if (method === "GET" && rest === "/merge_requests") {
        const src = url.searchParams.get("source_branch");
        const list = state.mrs.filter((m) => (src ? m.source_branch === src : true) && m.state === "opened");
        return send(res, 200, list);
      }
    }

    log("unhandled", method, path);
    send(res, 404, { message: "404 Not found (forge-fake)" });
  },
);

persist();
server.listen(PORT, () => log(`listening on :${PORT} (base ${BASE}, project ${PROJECT.path_with_namespace})`));
