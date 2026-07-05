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
  // Issue comments the autopilot terminal hook + poller post (GitLab "notes"). A
  // flat list carrying issue_iid so the harness can count per issue (PRD #19 M6).
  notes: /** @type {any[]} */ ([]),
  // Resource label events per issue (iid -> [event]), the signal the autopilot
  // detector reads to decide "who added the autopilot label, and which
  // application" (ListIssueLabelEvents). GitLab returns them oldest-first with
  // globally-monotonic ids; the fake preserves that (append-only, rising id).
  labelEvents: /** @type {Record<number, any[]>} */ ({}),
  nextIssueIid: 1,
  nextMrIid: 1,
  nextNoteId: 5000,
  nextLabelEventId: 100,
};

function persist() {
  try {
    fs.writeFileSync(
      STATE_FILE,
      JSON.stringify(
        { issues: Object.values(state.issues), mrs: state.mrs, notes: state.notes, labelEvents: state.labelEvents },
        null,
        2,
      ),
    );
  } catch {
    /* best-effort introspection sink */
  }
}

// Reload persisted state at startup. The fake is in-memory, but when STATE_FILE
// lives on a bind mount (the E2E overlay) this survives a container recreate — so
// the restart-resilience down/up does not make the fake "forget" issues and MRs
// (a real forge doesn't lose them when uzi restarts). Best-effort: a missing or
// corrupt file just means a fresh start.
function load() {
  try {
    const saved = JSON.parse(fs.readFileSync(STATE_FILE, "utf8"));
    for (const issue of saved.issues || []) state.issues[issue.iid] = issue;
    state.mrs = Array.isArray(saved.mrs) ? saved.mrs : [];
    state.notes = Array.isArray(saved.notes) ? saved.notes : [];
    state.labelEvents = saved.labelEvents && typeof saved.labelEvents === "object" ? saved.labelEvents : {};
    state.nextIssueIid = Math.max(0, ...Object.keys(state.issues).map(Number)) + 1;
    state.nextMrIid = Math.max(0, ...state.mrs.map((m) => m.iid)) + 1;
    state.nextNoteId = Math.max(4999, ...state.notes.map((n) => n.id)) + 1;
    state.nextLabelEventId = Math.max(99, ...Object.values(state.labelEvents).flat().map((e) => e.id)) + 1;
    log(
      "reloaded state:",
      Object.keys(state.issues).length, "issue(s),",
      state.mrs.length, "MR(s),",
      state.notes.length, "note(s)",
    );
  } catch {
    /* no prior state (fresh run) — start empty */
  }
}

function log(...args) {
  console.log(new Date().toISOString(), "[forge-fake]", ...args);
}

function makeIssue({ iid, title, description, labels, author }) {
  return {
    id: 1000 + iid,
    iid,
    project_id: PROJECT.id,
    title,
    description: description ?? "",
    state: "opened",
    labels: labels && labels.length ? labels : ["PRD"],
    web_url: `${PROJECT.web_url}/-/issues/${iid}`,
    // author drives autopilot attribution (the fallback when the label adder is
    // unmapped); default to the bot so the uzi-created issues are unchanged.
    author: { id: author ? 2 : 1, username: author || "uzi-bot" },
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
      return send(res, 200, {
        issues: Object.values(state.issues),
        mrs: state.mrs,
        notes: state.notes,
        labelEvents: state.labelEvents,
        project: PROJECT,
      });
    }
    if (method === "GET" && (path === "/_e2e/health" || path === "/")) {
      return send(res, 200, { ok: true });
    }

    // Create an issue directly with explicit labels + author — the harness
    // stand-in for a human filing a PRD/autopilot-labelled issue (uzi's own
    // CreateIssue always stamps just the PRD label, so autopilot flows need this).
    // E2E-only, matching the /_e2e/* mutator style.
    if (method === "POST" && path === "/_e2e/issues") {
      const body = await readBody(req);
      const iid = state.nextIssueIid++;
      const issue = makeIssue({
        iid,
        title: body.title || `issue ${iid}`,
        description: body.description,
        labels: normLabels(body.labels),
        author: typeof body.author === "string" ? body.author : "",
      });
      state.issues[iid] = issue;
      state.labelEvents[iid] = state.labelEvents[iid] || [];
      persist();
      log("_e2e issue created", iid, JSON.stringify(issue.title), "labels", JSON.stringify(issue.labels));
      return send(res, 201, issue);
    }

    // Append a resource label event to an issue — the harness stand-in for a
    // human adding/removing a label. The rising id models GitLab's globally-
    // monotonic event ids, so a remove+re-add mints a larger id (the autopilot
    // detector's transition-once retry signal).
    const labelEv = method === "POST" && path.match(/^\/_e2e\/issues\/(\d+)\/label-events$/);
    if (labelEv) {
      const body = await readBody(req);
      const iid = Number(labelEv[1]);
      if (!state.issues[iid]) return send(res, 404, { message: "404 Not found (no such issue)" });
      const action = body.action === "remove" ? "remove" : "add";
      const ev = {
        id: state.nextLabelEventId++,
        action,
        created_at: new Date().toISOString(),
        resource_type: "Issue",
        resource_id: state.issues[iid].id,
        user: { id: 2, username: String(body.username || "") },
        label: { id: 1, name: String(body.label || "") },
      };
      (state.labelEvents[iid] = state.labelEvents[iid] || []).push(ev);
      persist();
      log("_e2e label event", iid, action, ev.label.name, "by", ev.user.username, "id", ev.id);
      return send(res, 201, ev);
    }
    // Flip an MR's state — the harness stand-in for a reviewer closing, reopening,
    // or merging an MR (uzi itself only ever GETs the MR). E2E-only mutator,
    // matching the /_e2e/* introspection style.
    const mrState = method === "POST" && path.match(/^\/_e2e\/mrs\/(\d+)\/state$/);
    if (mrState) {
      const body = await readBody(req);
      const want = String(body.state || "");
      if (!["opened", "closed", "merged", "locked"].includes(want)) {
        return send(res, 400, { message: `bad state ${JSON.stringify(want)}` });
      }
      const mr = state.mrs.find((m) => m.iid === Number(mrState[1]));
      if (!mr) return send(res, 404, { message: "404 Not found (no such MR)" });
      mr.state = want;
      persist();
      log("MR", mr.iid, "state ->", want);
      return send(res, 200, mr);
    }

    // --- GitLab v4 subset -----------------------------------------------------
    // Token verify (api seed + connect): CurrentUser.
    if (method === "GET" && path === "/api/v4/user") {
      return send(res, 200, { id: 1, username: "uzi-bot", name: "uzi bot" });
    }

    // User lookup (human_username verified-or-warned save, PRD #19 M3): GitLab's
    // ?username= exact filter. The fake knows no human accounts, so it always
    // returns empty — every self-declared username saves WITH a warning (never a
    // hard reject), which is exactly the verified-or-warned path under test.
    if (method === "GET" && path === "/api/v4/users") {
      return send(res, 200, []);
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
        // UpdateIssue (label moves). Apply add_labels/remove_labels FAITHFULLY so a
        // poller reconcile (GET /issues) reflects the move: both the run-lifecycle
        // automation and the MR-close watcher (PRD #24) move cards forge-first, and
        // the column must survive the next re-sync instead of snapping back.
        const issue = state.issues[Number(issueGet[1])];
        if (!issue) return send(res, 404, { message: "404 Not found" });
        const body = await readBody(req);
        const remove = new Set(normLabels(body.remove_labels));
        const next = (issue.labels || []).filter((l) => !remove.has(l));
        for (const l of normLabels(body.add_labels)) if (!next.includes(l)) next.push(l);
        issue.labels = next;
        issue.updated_at = new Date().toISOString();
        persist();
        log("issue", issue.iid, "labels ->", JSON.stringify(issue.labels));
        return send(res, 200, issue);
      }
      // Resource label events (autopilot detector: who added the autopilot label,
      // and which application). Oldest-first with rising ids (see state.labelEvents).
      const labelEvents = rest.match(/^\/issues\/(\d+)\/resource_label_events$/);
      if (method === "GET" && labelEvents) {
        return send(res, 200, state.labelEvents[Number(labelEvents[1])] || []);
      }

      // Issue notes (comments). POST records the autopilot pre-run / terminal
      // comment; the harness reads them back via /_e2e/state to assert exactly-once.
      const notesRoute = rest.match(/^\/issues\/(\d+)\/notes$/);
      if (method === "POST" && notesRoute) {
        const body = await readBody(req);
        const iid = Number(notesRoute[1]);
        const note = { id: state.nextNoteId++, issue_iid: iid, body: body.body || "", created_at: new Date().toISOString() };
        state.notes.push(note);
        persist();
        log("note on issue", iid, JSON.stringify((body.body || "").slice(0, 72)));
        return send(res, 201, note);
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
      const mrGet = rest.match(/^\/merge_requests\/(\d+)$/);
      if (method === "GET" && mrGet) {
        // Single-MR GET (the MR-close watcher's GetMergeRequest). Returns the MR in
        // ANY state (opened|closed|merged|locked), unlike the list route below which
        // the worker uses and filters to opened.
        const mr = state.mrs.find((m) => m.iid === Number(mrGet[1]));
        return mr ? send(res, 200, mr) : send(res, 404, { message: "404 Not found" });
      }
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

load();
persist();
server.listen(PORT, () => log(`listening on :${PORT} (base ${BASE}, project ${PROJECT.path_with_namespace})`));
