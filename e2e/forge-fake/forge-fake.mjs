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

const server = https.createServer(
  { cert: fs.readFileSync(CERT), key: fs.readFileSync(KEY) },
  async (req, res) => {
    const method = req.method || "GET";
    const url = new URL(req.url || "/", BASE);
    const path = url.pathname;

    // --- introspection (harness reads this to assert recorded state) ---------
    if (method === "GET" && path === "/_e2e/state") {
      return send(res, 200, { issues: Object.values(state.issues), mrs: state.mrs, project: PROJECT });
    }
    if (method === "GET" && (path === "/_e2e/health" || path === "/")) {
      return send(res, 200, { ok: true });
    }

    // --- GitLab v4 subset -----------------------------------------------------
    // Token verify (api seed + connect): CurrentUser.
    if (method === "GET" && path === "/api/v4/user") {
      return send(res, 200, { id: 1, username: "uzi-bot", name: "uzi bot" });
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
