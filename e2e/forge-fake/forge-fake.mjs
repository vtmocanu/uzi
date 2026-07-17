// forge-fake — a tiny stand-in for a forge, used only by the E2E harness. It
// serves, over HTTPS on :443, the subset of the forge REST API that the uzi api
// (issue create/get, token verify, project + issue listing, privilege + branch
// checks, CI) and the uzi worker (merge/pull-request create) actually call, plus
// an introspection endpoint the harness reads to assert what was recorded.
//
// It speaks BOTH forge dialects over ONE shared in-memory state, selected by path
// prefix: the GitLab v4 API (`/api/v4/*`) and — for PRD #65's Forgejo lane — the
// Forgejo v1 API (`/api/v1/*`). The driver hits whichever matches the connection's
// forge_type; the `/_e2e/*` mutators and the git smart-HTTP handler are
// forge-agnostic and shared. UZI_E2E_FORGE selects the lane in run-e2e.sh.
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

// The projects this fake serves. The first (group/repo) is always present; a
// SECOND (FORGE_FAKE_PROJECT2, e.g. group/repo2) is added only when that env is
// set. path_with_namespace + web_url must match what the harness seeds
// (UZI_SEED_FORGE_REPOS) and what the worker derives the MR/clone URLs from.
//
// The second project exists solely for the PRD #42 M5 bounded-concurrency
// scenario, which runs two issues on two DIFFERENT repos so a cap-2 worker's two
// runs get independent git bare-caches (no per-repo GitCache serialization). Every
// OTHER harness phase touches only the first project, and resolveProject() falls
// back to it, so single-project behavior is byte-identical when the env is unset.
function makeProject(id, pathWithNamespace) {
  return {
    id,
    path_with_namespace: pathWithNamespace,
    default_branch: "main",
    web_url: `${BASE}/${pathWithNamespace}`,
  };
}
const PROJECTS = [makeProject(1, process.env.FORGE_FAKE_PROJECT || "group/repo")];
if (process.env.FORGE_FAKE_PROJECT2) PROJECTS.push(makeProject(2, process.env.FORGE_FAKE_PROJECT2));
const PROJECT = PROJECTS[0]; // back-compat alias for the single-project references

// resolveProject maps a GitLab `:id` path param — a numeric project id (uzi's
// client-go) OR a url-encoded path_with_namespace (the worker addresses MRs by
// encoded path) — to one of PROJECTS. Falls back to the first project, so a
// single-project run is identical to the pre-multi-project behavior where `:id`
// was ignored entirely.
function resolveProject(idParam) {
  const raw = decodeURIComponent(String(idParam || ""));
  return PROJECTS.find((p) => String(p.id) === raw || p.path_with_namespace === raw) || PROJECTS[0];
}

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
  // CI pipelines per ref (PRD #6). Keyed by ref (a branch name); each value is a
  // GitLab-shaped pipeline plus its jobs (with traces). The harness seeds/flips
  // these via the /_e2e/pipelines mutator to drive the CI-status + Fix CI +
  // verification flow. LatestMRPipeline resolves an MR to its source_branch's ref.
  pipelines: /** @type {Record<string, any>} */ ({}),
  // Forgejo Actions workflow runs (PRD #65 M9). A flat list (NOT keyed by ref like
  // GitLab's pipelines) so 2+ runs can exist on ONE branch/sha — the id-DESC
  // ordering the driver's `[0]`-is-newest depends on. The /api/v1 readers return
  // this sorted id DESC; the harness appends via /_e2e/actions-runs.
  forgejoRuns: /** @type {any[]} */ ([]),
  // Stable label name -> numeric id map for the Forgejo lane. Forgejo's label
  // writes are keyed by id (ReplaceIssueLabels takes []int), so the fake must
  // assign each label name a stable id and map ids back to names on a PUT.
  forgejoLabelIds: /** @type {Record<string, number>} */ ({}),
  nextIssueIid: 1,
  nextMrIid: 1,
  nextNoteId: 5000,
  nextLabelEventId: 100,
  nextPipelineId: 900,
  nextJobId: 700,
  nextForgejoRunId: 3000,
  nextForgejoJobId: 3500,
  nextForgejoLabelId: 200,
  // The version /api/v1/version reports. Default is a D4a-passing release; the
  // harness flips it (via /_e2e/forgejo-version) to a < 16.0.0 string for one
  // assertion, to prove the privilege sweep raises the version-downgrade finding.
  forgejoVersion: "16.0.0+gitea-1.22.0",
};

function persist() {
  try {
    fs.writeFileSync(
      STATE_FILE,
      JSON.stringify(
        { issues: Object.values(state.issues), mrs: state.mrs, notes: state.notes, labelEvents: state.labelEvents, pipelines: state.pipelines, forgejoRuns: state.forgejoRuns, forgejoLabelIds: state.forgejoLabelIds },
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
    state.pipelines = saved.pipelines && typeof saved.pipelines === "object" ? saved.pipelines : {};
    state.forgejoRuns = Array.isArray(saved.forgejoRuns) ? saved.forgejoRuns : [];
    state.forgejoLabelIds = saved.forgejoLabelIds && typeof saved.forgejoLabelIds === "object" ? saved.forgejoLabelIds : {};
    state.nextPipelineId = Math.max(899, ...Object.values(state.pipelines).map((p) => p.id)) + 1;
    state.nextForgejoRunId = Math.max(2999, ...state.forgejoRuns.map((r) => r.id)) + 1;
    state.nextForgejoJobId = Math.max(3499, ...state.forgejoRuns.flatMap((r) => (r.jobs || []).map((j) => j.id))) + 1;
    state.nextForgejoLabelId = Math.max(199, ...Object.values(state.forgejoLabelIds)) + 1;
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

function makeIssue({ iid, title, description, labels, author, project }) {
  const proj = project || PROJECT;
  return {
    id: 1000 + iid,
    iid,
    project_id: proj.id,
    title,
    description: description ?? "",
    state: "opened",
    labels: labels && labels.length ? labels : ["PRD"],
    web_url: `${proj.web_url}/-/issues/${iid}`,
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

// pipelineInfo strips the internal jobs list to the GitLab PipelineInfo shape the
// list endpoints return (PRD #6).
function pipelineInfo(p) {
  return {
    id: p.id, iid: p.iid, project_id: p.project_id, status: p.status,
    ref: p.ref, sha: p.sha, web_url: p.web_url, created_at: p.created_at, updated_at: p.updated_at,
  };
}

// jobInfo maps an internal job to the GitLab Job shape ListPipelineJobs returns.
function jobInfo(j) {
  return { id: j.id, name: j.name, stage: j.stage, status: j.status, web_url: j.web_url };
}

// labels may arrive as a JSON array or a comma-joined string, depending on the
// client-go version; normalize to an array.
function normLabels(v) {
  if (Array.isArray(v)) return v.map(String);
  if (typeof v === "string" && v.trim()) return v.split(",").map((s) => s.trim());
  return [];
}

// ============================================================================
// Forgejo /api/v1 translation (PRD #65 M9). The Forgejo route table below shares
// the SAME in-memory `state` as the GitLab /api/v4 table — the /_e2e mutators and
// the git smart-HTTP handler are forge-agnostic — but the wire SHAPES differ, so
// these helpers translate a GitLab-shaped state record into what the Forgejo
// driver actually parses. Every shape here mirrors the driver as-built (verified
// against a live 16.0.0 container while writing M2/M4/M5): a PR is an issue with a
// non-null pull_request (R4); an issue's `label` in the timeline is a SINGLE
// object; the token payload carries token_last_eight + scopes and no expiry;
// Actions runs come back id-DESC as {total_count, workflow_runs}.
// ============================================================================

// flabelId assigns each label NAME a stable numeric id (Forgejo keys label writes
// by id). Minted on first sight and persisted, so the catalog, an issue's labels,
// and a ReplaceIssueLabels PUT all agree on the same id for a name.
function flabelId(name) {
  if (state.forgejoLabelIds[name] == null) state.forgejoLabelIds[name] = state.nextForgejoLabelId++;
  return state.forgejoLabelIds[name];
}
// flabelName reverses flabelId (for a ReplaceIssueLabels PUT, which sends ids).
function flabelName(id) {
  for (const [name, n] of Object.entries(state.forgejoLabelIds)) if (n === id) return name;
  return null;
}
// forgejoWebUrl rewrites a GitLab-style web_url into Forgejo's grammar so a card's
// rendered link is forge-honest (/-/issues/N -> /issues/N, /-/merge_requests/N ->
// /pulls/N). Correctness only; both are same-host https.
function forgejoWebUrl(glUrl) {
  return String(glUrl || "").replace("/-/issues/", "/issues/").replace("/-/merge_requests/", "/pulls/");
}
// forgejoIssueState maps the cache's "opened"/"closed" onto Forgejo's "open"/"closed".
function forgejoIssueState(s) {
  return s === "closed" ? "closed" : "open";
}
// toForgejoIssue maps a stored GitLab-shaped issue to a Forgejo issue: `number` for
// the index, `body` for the description, a SINGLE-OBJECT labels array, and an
// explicit null pull_request (this is a real issue, not a PR).
function toForgejoIssue(issue) {
  return {
    id: issue.id,
    number: issue.iid,
    title: issue.title,
    body: issue.description ?? "",
    state: forgejoIssueState(issue.state),
    labels: (issue.labels || []).map((n) => ({ id: flabelId(n), name: n })),
    html_url: forgejoWebUrl(issue.web_url),
    user: { id: issue.author?.id ?? 1, login: issue.author?.username || "uzi-bot" },
    updated_at: issue.updated_at,
    created_at: issue.created_at,
    pull_request: null,
  };
}
// forgejoPRState maps a stored MR's GitLab-style state (opened|closed|merged|locked)
// onto Forgejo's {state, merged}: open -> {open,false}, merged -> {closed,true},
// closed/locked -> {closed,false}. Verified live on 16.0.0.
function forgejoPRState(mrState) {
  if (mrState === "merged") return { state: "closed", merged: true };
  if (mrState === "opened") return { state: "open", merged: false };
  return { state: "closed", merged: false };
}
// mrHeadSha is the deterministic head commit the fake assigns a PR, so the harness
// can drive Actions runs for the PR's head without reading it back. Real Forgejo
// keys a PR's runs to its head SHA; LatestMRPipeline resolves the PR head then
// filters runs by it, so the fake's PR head.sha and the run head_sha must agree.
function mrHeadSha(sourceBranch) {
  return `sha-${sourceBranch}`;
}
// toForgejoPR maps a stored MR to a Forgejo pull request. head.sha is deterministic
// (see mrHeadSha) so LatestMRPipeline can find the run.
function toForgejoPR(mr) {
  const s = forgejoPRState(mr.state);
  return {
    id: mr.id,
    number: mr.iid,
    title: mr.title,
    body: mr.description ?? "",
    state: s.state,
    merged: s.merged,
    html_url: forgejoWebUrl(mr.web_url),
    head: { ref: mr.source_branch, sha: mr.head_sha || mrHeadSha(mr.source_branch) },
    base: { ref: mr.target_branch },
  };
}
// mrToForgejoIssue models an MR as an issue with a non-null pull_request — exactly
// how Forgejo returns PRs on the /issues route. The Forgejo /issues reader emits
// these ALONGSIDE real issues so the driver's R4 filter (pull_request != null) is
// exercised end-to-end: an MR must NEVER reach the board as a card.
function mrToForgejoIssue(mr) {
  const s = forgejoPRState(mr.state);
  return {
    id: mr.id,
    // A distinct high number (NOT mr.iid) so a card leaking here is DETECTABLE: if
    // the driver's R4 filter regressed, this PR would surface as card #(10000+iid),
    // which no real issue ever has. The entry is always dropped (pull_request !=
    // null) before the driver reads its number, so the offset is invisible in
    // practice; it only makes a filter FAILURE visible.
    number: 10000 + mr.iid,
    title: mr.title || `PR ${mr.iid}`,
    body: mr.description ?? "",
    state: s.state,
    labels: [],
    html_url: forgejoWebUrl(mr.web_url),
    user: { id: 1, login: "uzi-bot" },
    updated_at: new Date().toISOString(),
    created_at: new Date().toISOString(),
    pull_request: { merged: s.merged, html_url: forgejoWebUrl(mr.web_url) },
  };
}
// forgejoRunSummary is the {total_count, workflow_runs} shape ListRepoActionRuns
// returns, sorted id DESC (newest first) — the ordering the driver's `[0]`-is-newest
// depends on. Filters by branch and/or head_sha, and honours the driver's limit.
function forgejoRunSummary({ branch, headSha, limit }) {
  let runs = state.forgejoRuns.slice();
  if (branch) runs = runs.filter((r) => r.head_branch === branch);
  if (headSha) runs = runs.filter((r) => r.head_sha === headSha);
  runs.sort((a, b) => b.id - a.id); // id DESC: newest first (models run_list.go ToOrders)
  const total = runs.length;
  if (limit && limit > 0) runs = runs.slice(0, limit);
  return {
    total_count: total,
    workflow_runs: runs.map((r) => ({
      id: r.id,
      head_branch: r.head_branch,
      head_sha: r.head_sha,
      status: r.status,
      html_url: r.html_url,
      started_at: r.started_at,
      completed_at: r.completed_at,
    })),
  };
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
        projects: PROJECTS,
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

    // Set/flip a ref's pipeline — the harness stand-in for CI running (PRD #6).
    // Body: {ref, status, sha?, jobs?} where jobs is [{name, stage, status, trace}].
    // The id auto-increments (so a re-flip after a fix gets an id > the failure's,
    // which is what the verification "observed id > snapshot id" guard needs); pass
    // id explicitly to pin it. E2E-only mutator.
    if (method === "POST" && path === "/_e2e/pipelines") {
      const body = await readBody(req);
      const ref = String(body.ref || "");
      if (!ref) return send(res, 400, { message: "ref is required" });
      const id = Number.isFinite(body.id) ? body.id : state.nextPipelineId++;
      const jobs = (body.jobs || []).map((j) => ({
        id: state.nextJobId++,
        name: j.name || "job",
        stage: j.stage || "test",
        status: j.status || "failed",
        web_url: `${PROJECT.web_url}/-/jobs/${state.nextJobId}`,
        trace: j.trace || "",
      }));
      state.pipelines[ref] = {
        id,
        iid: id,
        project_id: PROJECT.id,
        ref,
        sha: body.sha || `sha-${id}`,
        status: String(body.status || "failed"),
        web_url: `${PROJECT.web_url}/-/pipelines/${id}`,
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
        jobs,
      };
      persist();
      log("pipeline set", ref, "->", state.pipelines[ref].status, "#" + id);
      return send(res, 200, state.pipelines[ref]);
    }

    // Append a Forgejo Actions workflow run (PRD #65 M9). Unlike /_e2e/pipelines
    // (one pipeline per ref), this APPENDS, so 2+ runs can share a branch/sha — the
    // id-DESC ordering the driver's [0]-is-newest relies on. Body:
    // {branch?, sha?, status?, id?, jobs?:[{name,status,log}]}. A later append gets a
    // higher auto-id (so a post-fix run outranks the failure), which is what the
    // verification "observed id > snapshot id" guard needs.
    if (method === "POST" && path === "/_e2e/actions-runs") {
      const body = await readBody(req);
      const id = Number.isFinite(body.id) ? body.id : state.nextForgejoRunId++;
      const jobs = (body.jobs || []).map((j) => ({
        id: state.nextForgejoJobId++,
        name: j.name || "build",
        status: j.status || "failure",
        log: j.log ?? j.trace ?? "",
        html_url: `${PROJECT.web_url}/actions/runs/${id}`,
      }));
      const run = {
        id,
        head_branch: String(body.branch || ""),
        head_sha: String(body.sha || ""),
        status: String(body.status || "failure"),
        html_url: `${PROJECT.web_url}/actions/runs/${id}`,
        started_at: new Date().toISOString(),
        completed_at: new Date().toISOString(),
        jobs,
      };
      state.forgejoRuns.push(run);
      persist();
      log("actions run set", run.head_branch || run.head_sha, "->", run.status, "#" + id);
      return send(res, 201, { id, status: run.status });
    }

    // Set the version /api/v1/version reports (PRD #65 M9): the harness flips it
    // below 16.0.0 to prove the privilege sweep raises the version-downgrade
    // finding, then restores it. Body: {version}.
    if (method === "POST" && path === "/_e2e/forgejo-version") {
      const body = await readBody(req);
      state.forgejoVersion = String(body.version || "16.0.0+gitea-1.22.0");
      log("forgejo version ->", state.forgejoVersion);
      return send(res, 200, { version: state.forgejoVersion });
    }

    // --- Forgejo /api/v1 subset (PRD #65 M9) ----------------------------------
    // Selected by path prefix, over the SAME state as the GitLab table. The driver
    // hits this table when the connection's forge_type is 'forgejo'; the GitLab
    // lane never reaches it, so /api/v4 behaviour is byte-identical.
    if (path === "/api/v1/version") {
      // Default is a D4a-passing release; the harness can flip it below 16.0.0 to
      // exercise the version gate on the sweep (/_e2e/forgejo-version).
      return send(res, 200, { version: state.forgejoVersion });
    }
    if (method === "GET" && path === "/api/v1/user") {
      // GetMyUserInfo (VerifyToken + TokenInfo bot-identify). is_admin is emitted
      // (Forgejo has no omitempty) and false — a compliant non-admin bot.
      return send(res, 200, { id: 1, login: "uzi-bot", is_admin: false });
    }
    if (method === "GET" && path === "/api/v1/users/search") {
      // GetUserByID (ProjectRole resolves the numeric bot id -> login). Only the
      // bot (uid=1) is known.
      const uid = url.searchParams.get("uid");
      return send(res, 200, { data: uid === "1" ? [{ id: 1, login: "uzi-bot" }] : [] });
    }
    const fjTokens = path.match(/^\/api\/v1\/users\/([^/]+)\/tokens$/);
    if (method === "GET" && fjTokens) {
      // TokenInfo (hand-rolled). token_last_eight identifies the authenticating PAT;
      // read it straight off the Authorization header so it always matches whatever
      // PAT the harness uses (M4's fail-safe fires on 0 or >1 matches). No expiry /
      // active field (Forgejo has none) -> the driver sets Active=true, ExpiresAt=0.
      // Scopes are REORDERED vs mint order to exercise D6b's set-compare.
      const m = /^token\s+(.+)$/i.exec(req.headers["authorization"] || "");
      const pat = m ? m[1] : "";
      const last8 = pat.length >= 8 ? pat.slice(-8) : pat;
      return send(res, 200, [
        { id: 1, name: "uzi-bot", token_last_eight: last8, scopes: ["write:issue", "write:repository", "read:user"] },
      ]);
    }
    const fjUser = path.match(/^\/api\/v1\/users\/([^/]+)$/);
    if (method === "GET" && fjUser && fjUser[1] !== "search") {
      // UserExists (human_username verify). The fake knows no human accounts, so
      // 404 -> the driver returns (false, nil) -> saved WITH a warning, never a hard
      // reject (the verified-or-warned path, mirroring the GitLab table's empty list).
      return send(res, 404, { message: "user does not exist" });
    }
    const fjRepoById = path.match(/^\/api\/v1\/repositories\/(\d+)$/);
    if (method === "GET" && fjRepoById) {
      // GetRepoByID (the driver resolves a numeric project id -> owner/repo slug).
      const project = resolveProject(fjRepoById[1]);
      const [owner, ...rest] = project.path_with_namespace.split("/");
      return send(res, 200, {
        id: project.id,
        name: rest.join("/") || project.path_with_namespace,
        full_name: project.path_with_namespace,
        owner: { id: 1, login: owner },
        default_branch: project.default_branch,
        html_url: project.web_url,
        permissions: { admin: false, push: true, pull: true },
      });
    }
    if (method === "GET" && path === "/api/v1/user/repos") {
      // ListProjects. permissions.push=true so the driver's client-side push filter
      // keeps them (a read-only repo would be dropped).
      return send(
        res,
        200,
        PROJECTS.map((p) => ({
          id: p.id,
          name: p.path_with_namespace.split("/").slice(1).join("/"),
          full_name: p.path_with_namespace,
          html_url: p.web_url,
          default_branch: p.default_branch,
          permissions: { admin: false, push: true, pull: true },
        })),
      );
    }

    // Repo-scoped: /api/v1/repos/{owner}/{repo}/<rest>.
    const fjRepo = path.match(/^\/api\/v1\/repos\/([^/]+)\/([^/]+)(\/.*)?$/);
    if (fjRepo) {
      const project = resolveProject(`${fjRepo[1]}/${fjRepo[2]}`);
      const rest = fjRepo[3] || "";

      // Issues list. Emits real issues AND every MR as a pull_request issue, so the
      // driver's R4 filter (pull_request != null) is exercised: an MR must never
      // reach the board. The driver also asks type=issues; the client-side filter is
      // the guarantee, so returning both here is the honest stress.
      if (method === "GET" && rest === "/issues") {
        const issues = Object.values(state.issues).map(toForgejoIssue);
        const prs = state.mrs.map(mrToForgejoIssue);
        return send(res, 200, [...issues, ...prs]);
      }
      const fjIssueGet = rest.match(/^\/issues\/(\d+)$/);
      if (method === "GET" && fjIssueGet) {
        const issue = state.issues[Number(fjIssueGet[1])];
        return issue ? send(res, 200, toForgejoIssue(issue)) : send(res, 404, { message: "issue does not exist" });
      }
      if (method === "POST" && rest === "/issues") {
        // CreateIssue: body {title, body, labels:[ids]}. Map ids -> names.
        const body = await readBody(req);
        const iid = state.nextIssueIid++;
        const labels = (Array.isArray(body.labels) ? body.labels : []).map((id) => flabelName(Number(id))).filter(Boolean);
        const issue = makeIssue({ iid, title: body.title || `issue ${iid}`, description: body.body, labels, project });
        state.issues[iid] = issue;
        persist();
        log("fj issue created", iid, JSON.stringify(issue.title));
        return send(res, 201, toForgejoIssue(issue));
      }
      // Issue labels: GET current [{id,name}]; PUT replaces the full set by ids
      // (ReplaceIssueLabels). The full-set replace with id<->name mapping is the
      // driver's D3 client-side computation landing here.
      const fjIssueLabels = rest.match(/^\/issues\/(\d+)\/labels$/);
      if (fjIssueLabels) {
        const issue = state.issues[Number(fjIssueLabels[1])];
        if (!issue) return send(res, 404, { message: "issue does not exist" });
        if (method === "GET") {
          return send(res, 200, (issue.labels || []).map((n) => ({ id: flabelId(n), name: n })));
        }
        if (method === "PUT") {
          const body = await readBody(req);
          const names = (Array.isArray(body.labels) ? body.labels : []).map((id) => flabelName(Number(id))).filter(Boolean);
          issue.labels = names;
          issue.updated_at = new Date().toISOString();
          persist();
          log("fj issue", issue.iid, "labels ->", JSON.stringify(issue.labels));
          return send(res, 200, names.map((n) => ({ id: flabelId(n), name: n })));
        }
      }
      // Issue comments (CreateIssueNote). Recorded in the shared notes list so the
      // harness reads them back via /_e2e/state exactly like the GitLab lane.
      const fjNotes = rest.match(/^\/issues\/(\d+)\/comments$/);
      if (method === "POST" && fjNotes) {
        const body = await readBody(req);
        const iid = Number(fjNotes[1]);
        const note = { id: state.nextNoteId++, issue_iid: iid, body: body.body || "", created_at: new Date().toISOString() };
        state.notes.push(note);
        persist();
        log("fj note on issue", iid, JSON.stringify((body.body || "").slice(0, 72)));
        return send(res, 201, { id: note.id, body: note.body });
      }
      // Issue timeline (ListIssueLabelEvents). Translate the shared labelEvents into
      // Forgejo timeline entries: type "label", body "1"=add / ""=remove, and a
      // SINGLE-OBJECT label (the shape the driver hand-parses — the SDK's []*Label
      // cannot).
      const fjTimeline = rest.match(/^\/issues\/(\d+)\/timeline$/);
      if (method === "GET" && fjTimeline) {
        const evts = state.labelEvents[Number(fjTimeline[1])] || [];
        return send(
          res,
          200,
          evts.map((e) => ({
            id: e.id,
            type: "label",
            body: e.action === "remove" ? "" : "1",
            user: { id: e.user?.id ?? 2, login: e.user?.username || "" },
            label: { id: flabelId(e.label?.name || ""), name: e.label?.name || "" },
            created_at: e.created_at,
          })),
        );
      }
      // Repo label catalog (ListRepoLabels / EnsureLabels). GET returns every known
      // label; POST mints one. The driver resolves label names -> ids here.
      if (rest === "/labels") {
        if (method === "GET") {
          return send(res, 200, Object.keys(state.forgejoLabelIds).map((n) => ({ id: state.forgejoLabelIds[n], name: n, color: "cccccc" })));
        }
        if (method === "POST") {
          const body = await readBody(req);
          const name = String(body.name || "label");
          return send(res, 201, { id: flabelId(name), name, color: (body.color || "#cccccc").replace(/^#/, "") });
        }
      }
      // Branch protection (DefaultBranchProtection, D6). Reader-gated and computed
      // for the caller: compliant fixture = protected, bot cannot push or merge.
      const fjBranch = rest.match(/^\/branches\/(.+)$/);
      if (method === "GET" && fjBranch) {
        return send(res, 200, {
          name: decodeURIComponent(fjBranch[1]),
          protected: true,
          user_can_push: false,
          user_can_merge: false,
          effective_branch_protection_name: "",
        });
      }
      // The admin-gated route the driver must NEVER call (a write bot 403s it). If it
      // ever does, fail loudly rather than pretending it works.
      if (rest.match(/^\/branch_protections\//)) {
        return send(res, 403, { message: "must be repo admin" });
      }
      // Collaborator permission (ProjectRole, D7). The bot is a write collaborator ->
      // RoleWrite, member=true (compliant, no finding).
      const fjPerm = rest.match(/^\/collaborators\/([^/]+)\/permission$/);
      if (method === "GET" && fjPerm) {
        return send(res, 200, { permission: "write", role_name: "Write", user: { id: 1, login: "uzi-bot" } });
      }

      // Pull requests.
      if (method === "POST" && rest === "/pulls") {
        // CreatePullRequest (the worker opens the PR). 409 on an existing OPEN PR for
        // the same head, so a resumed finish reuses it (matches Forgejo's
        // ErrPullRequestAlreadyExists). head/base are branch names.
        const body = await readBody(req);
        const dup = state.mrs.find((m) => m.source_branch === body.head && m.target_branch === body.base && m.state === "opened");
        if (dup) {
          log("fj PR create 409 (open PR exists)", dup.iid, body.head);
          return send(res, 409, { message: `pull request already exists for these targets: ${dup.iid}` });
        }
        const iid = state.nextMrIid++;
        const mr = {
          id: 5000 + iid,
          iid,
          project_id: project.id,
          source_branch: body.head,
          target_branch: body.base,
          head_sha: mrHeadSha(body.head),
          title: body.title,
          description: body.body,
          state: "opened",
          web_url: `${project.web_url}/-/merge_requests/${iid}`,
        };
        state.mrs.push(mr);
        persist();
        log("fj PR created", iid, mr.source_branch, "->", mr.target_branch);
        return send(res, 201, toForgejoPR(mr));
      }
      const fjPrByBaseHead = rest.match(/^\/pulls\/([^/]+)\/([^/]+)$/);
      if (method === "GET" && fjPrByBaseHead && !/^\d+$/.test(fjPrByBaseHead[1])) {
        // GetPullRequestByBaseHead (the worker's 409-resume fetch). base/head are
        // branch names. Tolerates finding no open PR (Forgejo's 409 also covers other
        // conflicts) with a 404.
        const base = decodeURIComponent(fjPrByBaseHead[1]);
        const head = decodeURIComponent(fjPrByBaseHead[2]);
        const mr = state.mrs.find((m) => m.source_branch === head && m.target_branch === base && m.state === "opened");
        return mr ? send(res, 200, toForgejoPR(mr)) : send(res, 404, { message: "pull request does not exist" });
      }
      const fjPrGet = rest.match(/^\/pulls\/(\d+)$/);
      if (method === "GET" && fjPrGet) {
        // GetMergeRequest / LatestMRPipeline head resolution.
        const mr = state.mrs.find((m) => m.iid === Number(fjPrGet[1]));
        return mr ? send(res, 200, toForgejoPR(mr)) : send(res, 404, { message: "pull request does not exist" });
      }
      const fjPrMerge = rest.match(/^\/pulls\/(\d+)\/merge$/);
      if (method === "POST" && fjPrMerge) {
        const mr = state.mrs.find((m) => m.iid === Number(fjPrMerge[1]));
        if (!mr) return send(res, 404, { message: "pull request does not exist" });
        mr.state = "merged";
        persist();
        log("fj PR merged", mr.iid);
        return send(res, 200, {});
      }

      // Actions (CI-fix loop, canned — Variant A). Runs come back id-DESC.
      if (method === "GET" && rest === "/actions/runs") {
        const limit = Number(url.searchParams.get("limit")) || 0;
        return send(res, 200, forgejoRunSummary({
          branch: url.searchParams.get("branch") || "",
          headSha: url.searchParams.get("head_sha") || "",
          limit,
        }));
      }
      const fjRunJobs = rest.match(/^\/actions\/runs\/(\d+)\/jobs$/);
      if (method === "GET" && fjRunJobs) {
        const run = state.forgejoRuns.find((r) => r.id === Number(fjRunJobs[1]));
        const jobs = run ? run.jobs : [];
        return send(res, 200, {
          total_count: jobs.length,
          jobs: jobs.map((j) => ({ id: j.id, run_id: run.id, name: j.name, status: j.status, html_url: j.html_url })),
        });
      }
      const fjJobLogs = rest.match(/^\/actions\/jobs\/(\d+)\/logs$/);
      if (method === "GET" && fjJobLogs) {
        const job = state.forgejoRuns.flatMap((r) => r.jobs || []).find((j) => j.id === Number(fjJobLogs[1]));
        res.writeHead(200, { "Content-Type": "text/plain" });
        return res.end(job ? job.log : "");
      }
    }

    // --- GitLab v4 subset -----------------------------------------------------
    // Token verify (api seed + connect): CurrentUser. is_admin is omitted (a
    // compliant, non-admin bot) — GitLab only returns it for an admin caller.
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

    // Token introspection (PRD #5 privilege check). Over-privileged iff the PAT
    // itself signals it (contains "overpriv"), so the harness can drive both the
    // compliant seed PAT and a rejected over-privileged one against one fake.
    if (method === "GET" && path === "/api/v4/personal_access_tokens/self") {
      const tok = req.headers["private-token"] || "";
      const scopes = tok.includes("overpriv") ? ["api", "sudo"] : ["api"];
      return send(res, 200, { id: 1, name: "uzi-bot", revoked: false, active: true, scopes, expires_at: null });
    }

    // Project listing (api seed / repo discovery). Returns every served project so
    // the seed upserts a repo row per project (only the requested ones are enabled).
    if (method === "GET" && path === "/api/v4/projects") {
      return send(res, 200, PROJECTS);
    }

    // Everything below is scoped to a project: /api/v4/projects/:id/<rest>. :id is
    // numeric (api client-go) or a url-encoded path (worker MR). resolveProject()
    // maps it to a served project (falling back to the first), so issue/MR
    // attribution is per-project when a second project is served and identical to
    // the single-project behavior otherwise.
    const proj = path.match(/^\/api\/v4\/projects\/([^/]+)(\/.*)?$/);
    if (proj) {
      const project = resolveProject(proj[1]);
      const rest = proj[2] || "";

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
        const issue = makeIssue({ iid, title: body.title || `issue ${iid}`, description: body.description, labels, project });
        state.issues[iid] = issue;
        persist();
        log("issue created", iid, JSON.stringify(issue.title), "in", project.path_with_namespace);
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

      // Pipelines (PRD #6). LatestPipeline: newest branch pipeline for a ref
      // (the driver asks per_page=1, so the single cached pipeline suffices).
      if (method === "GET" && rest === "/pipelines") {
        const ref = url.searchParams.get("ref");
        const p = ref ? state.pipelines[ref] : null;
        return send(res, 200, p ? [pipelineInfo(p)] : []);
      }
      // ListPipelineJobs — only at Fix CI snapshot time.
      const pjobs = rest.match(/^\/pipelines\/(\d+)\/jobs$/);
      if (method === "GET" && pjobs) {
        const p = Object.values(state.pipelines).find((x) => x.id === Number(pjobs[1]));
        return send(res, 200, p ? p.jobs.map(jobInfo) : []);
      }
      // JobLogTail — plain text trace, like GitLab.
      const jtrace = rest.match(/^\/jobs\/(\d+)\/trace$/);
      if (method === "GET" && jtrace) {
        const job = Object.values(state.pipelines)
          .flatMap((x) => x.jobs)
          .find((j) => j.id === Number(jtrace[1]));
        res.writeHead(200, { "Content-Type": "text/plain" });
        return res.end(job ? job.trace : "");
      }
      // LatestMRPipeline — resolve MR -> source_branch -> that ref's pipeline. This
      // is how a fix branch's post-fix pipeline is observed for verification.
      const mrPipes = rest.match(/^\/merge_requests\/(\d+)\/pipelines$/);
      if (method === "GET" && mrPipes) {
        const mr = state.mrs.find((m) => m.iid === Number(mrPipes[1]));
        const p = mr ? state.pipelines[mr.source_branch] : null;
        return send(res, 200, p ? [pipelineInfo(p)] : []);
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
        // GitLab returns 409 when an OPEN MR already exists for this
        // source->target branch pair; the worker catches it and reuses the existing
        // MR (findOpenMr) instead of stacking a second one. Emulate that so a ci_fix
        // landing on an agent branch updates the existing MR (PRD #6), not a dupe.
        const dup = state.mrs.find(
          (m) => m.source_branch === body.source_branch && m.target_branch === body.target_branch && m.state === "opened",
        );
        if (dup) {
          log("MR create 409 (open MR exists)", dup.iid, body.source_branch);
          return send(res, 409, { message: [`Another open merge request already exists for this source branch: !${dup.iid}`] });
        }
        const iid = state.nextMrIid++;
        const mr = {
          id: 5000 + iid,
          iid,
          project_id: project.id,
          source_branch: body.source_branch,
          target_branch: body.target_branch,
          title: body.title,
          description: body.description,
          state: "opened",
          web_url: `${project.web_url}/-/merge_requests/${iid}`,
        };
        state.mrs.push(mr);
        persist();
        log("MR created", iid, mr.source_branch, "->", mr.target_branch, "in", project.path_with_namespace);
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
server.listen(PORT, () =>
  log(`listening on :${PORT} (base ${BASE}, projects ${PROJECTS.map((p) => p.path_with_namespace).join(", ")})`),
);
