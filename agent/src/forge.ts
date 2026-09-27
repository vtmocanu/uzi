// Worker-performed merge-request / pull-request creation (PRD #4 §Workflow, the
// primary directive; PRD #65 D9 generalises it to a second forge).
//
// The AGENT never has a push credential and never talks to the forge; the WORKER
// does, holding the bot PAT. After the agent signals done and the worker pushes
// the branch (git.ts), it opens the MR/PR here. The PAT rides an auth HEADER only
// — never the URL, never argv, never a log line — mirroring the header-auth
// discipline the git layer uses for clone/fetch/push. The MR/PR links the issue
// and is NEVER merged (humans merge).
//
// Only `createMergeRequest` is needed on the worker seam: the worker opens the
// MR/PR itself and does not need the 20-method Go `Forge` interface (D9). The
// three transport guards below (non-https refusal, redirect:"error",
// duplicate-status → fetch existing) are INTERFACE requirements, not per-driver
// details — so they live in the shared base and a fresh driver cannot forget one.
//
// `fetchFn` is injectable so the MR/PR path is tested up to — never across — the
// network boundary with a fake transport (testing-credentials policy).

import { errMessage } from "./util.js";

/** What the transport answers: the subset of the fetch `Response` the drivers read. `body` is the
 *  streaming body a real `Response` carries; it is OPTIONAL so a fake transport may answer with
 *  `text()` alone (every existing fake does). A bounded read streams `body` when it is present and
 *  stops at its cap; without it, it falls back to `text()` and checks the cap afterwards. */
export interface FetchResponse {
  status: number;
  text(): Promise<string>;
  body?: ReadableStream<Uint8Array> | null;
}

/** Injectable transport (default = global fetch). */
export type FetchFn = (
  url: string,
  init: {
    method: string;
    headers: Record<string, string>;
    body?: string;
    signal?: AbortSignal;
    /** Pinned to "error" so a 3xx cannot replay the PAT header cross-origin. */
    redirect?: "error" | "follow" | "manual";
  },
) => Promise<FetchResponse>;

/** PRD #1798 M6 (H1): the byte cap on a single-MR/PR detail read. A forge caps a description at
 *  about 1,048,576 characters (FORGE_BODY_MAX_CHARS), up to 4 UTF-8 bytes each, plus 64 KiB of
 *  headroom for JSON escaping and the rest of the object. */
export const MR_DETAIL_MAX_BYTES = 4 * 1_048_576 + 64 * 1024;

/** The bytes an error body is read to before it is cut (the message keeps 512 characters). */
const ERROR_BODY_MAX_BYTES = 4096;

export interface CreateMrParams {
  /** The forge WEB url of the repo. Each driver derives its own API base + project
   *  path from it: GitLab keeps the whole namespaced path (nested subgroups);
   *  Forgejo takes the last two segments as owner/repo and keeps any ROOT_URL
   *  subpath as the API base (D9 subpath fix). */
  repoUrl: string;
  pat: string;
  sourceBranch: string;
  targetBranch: string;
  title: string;
  description: string;
}

export interface MergeRequest {
  /** GitLab MR iid / Forgejo PR number — a per-project sequential id, identical in
   *  meaning across forges (D2), so one field carries both. */
  iid: number;
  webUrl: string;
}

/** Normalised lifecycle state of an MR/PR (PRD #1798 D11). GitLab's `opened` maps to
 *  `open`; GitHub/Forgejo report a merged PR as `closed` + a merged flag, mapped to
 *  `merged`. `locked` is GitLab-only. */
export type MergeRequestState = "open" | "closed" | "merged" | "locked";

/** A forge-neutral single-MR/PR read (PRD #1798 D11): the fields the description
 *  writer needs to bind a write to a snapshot. `description` is "" when the forge
 *  reports no body (null). */
export interface MergeRequestDetail {
  /** Validated 40-hex source-branch head commit id. */
  headSha: string;
  /** The MR/PR's own target (base) branch, never the repo default branch. */
  targetBranch: string;
  description: string;
  state: MergeRequestState;
}

export class ForgeError extends Error {
  constructor(
    readonly status: number,
    readonly detail: string,
  ) {
    super(`forge API returned ${status}: ${detail}`);
    this.name = "ForgeError";
  }
}

/** A response body over its byte cap (status 0: no forge status is at fault). It is a
 *  deterministic answer, not a transport blip, so a caller's retry should treat it as permanent
 *  (the runner's PR-description retry does); every caller treats it as an unreadable MR/PR. */
export class ForgeResponseTooLarge extends ForgeError {
  constructor(readonly maxBytes: number) {
    super(0, `response too large (over ${maxBytes} bytes)`);
    this.name = "ForgeResponseTooLarge";
  }
}

/** The worker's minimal forge seam (D9): open (or resume) the MR/PR for a pushed
 *  branch. Deliberately one method — the worker never reads issues, labels, or
 *  pipelines; that surface is the Go driver's. */
export interface ForgeClient {
  createMergeRequest(p: CreateMrParams, signal?: AbortSignal): Promise<MergeRequest>;
  /** Read the current head SHA of an existing MR/PR (PRD #1226 M4, D5). Forge-neutral:
   *  each driver derives its own single-item URL and parses the true source-branch head
   *  out of the response, returning a validated 40-hex commit id. The exact-head verify
   *  consumes it to bind a completion permit to (run, contract_revision, branch, head).
   *  Throws a ForgeError on a non-200, a missing head field, or a malformed SHA — the
   *  caller treats a failed head-read as "cannot verify H" (NON-TERMINAL, keeps the
   *  session live). The PAT rides the auth header only, same as createMergeRequest. */
  getMergeRequestHead(repoUrl: string, pat: string, iid: number, signal?: AbortSignal): Promise<string>;
  /** Forge-neutral, best-effort rewrite of an existing MR/PR body (PRD #1225, CodeRabbit
   *  !1254). Used by the completion interlock to reconcile the `Closes #N` line: when the
   *  interlock holds an unverified/unreadable PR head it rewrites the body to an unverified
   *  variant that carries NO `Closes #N`, and on a verified completion it re-asserts the
   *  canonical body. Each driver derives the single-item update URL and body-field name; the
   *  https-only, redirect:"error" and transient-5xx guards are inherited from `request`.
   *  Throws a ForgeError on a non-2xx so the caller can log-and-continue best-effort. */
  updateMergeRequestDescription(repoUrl: string, pat: string, iid: number, description: string, signal?: AbortSignal): Promise<void>;
  /** Forge-neutral read of one existing MR/PR (PRD #1798 D11): head SHA, target branch,
   *  current description and normalised state. Same single-item resource and transport
   *  guards as getMergeRequestHead. Throws a ForgeError on a non-200 (a 404 for a missing
   *  MR/PR included), a transient status, or a body missing a valid head SHA, a target
   *  branch or a known state. The PAT rides the auth header only. */
  getMergeRequest(repoUrl: string, pat: string, iid: number, signal?: AbortSignal): Promise<MergeRequestDetail>;
}

export interface ForgeClientOptions {
  fetchFn?: FetchFn;
  httpTimeoutMs?: number;
}

/**
 * Shared transport for every forge driver. It owns the three D9 guards so a new
 * driver inherits them by construction rather than re-implementing (and possibly
 * forgetting) them:
 *   1. `request` refuses a non-https URL before the PAT leaves the process.
 *   2. `request` pins `redirect:"error"` so a 3xx cannot replay the PAT header.
 *   3. `createMergeRequest` treats a driver-declared `duplicateStatuses()` set as
 *      "maybe already exists" and returns the open MR/PR — tolerating the driver
 *      finding none (Forgejo's 409, and GitHub's generic-validation 422, also cover
 *      non-duplicate causes), so a resumed finish step never dead-ends.
 * A driver supplies only the forge-specific bits: auth header, create URL/body, the
 * find-existing lookup, the response parse, and (when it differs from the {409}
 * default) which statuses mean "maybe already exists".
 */
abstract class HttpForgeClient implements ForgeClient {
  protected readonly fetchFn: FetchFn;
  protected readonly httpTimeoutMs: number;

  constructor(opts: ForgeClientOptions = {}) {
    this.fetchFn = opts.fetchFn ?? (globalThis.fetch as unknown as FetchFn);
    this.httpTimeoutMs = opts.httpTimeoutMs ?? 30_000;
  }

  async createMergeRequest(p: CreateMrParams, signal?: AbortSignal): Promise<MergeRequest> {
    const res = await this.request("POST", this.createUrl(p.repoUrl), p.pat, this.createBody(p), signal);
    if (res.status === 201) return this.parseMr(await res.text());

    // An MR/PR for this branch may already exist (a resume, or a prior finish that
    // pushed + opened before the state report landed). Which status the forge answers
    // with differs (GitLab/Forgejo 409, GitHub 422), so each driver declares its own
    // `duplicateStatuses()` set instead of the base hardcoding one — a blanket
    // 409‖422 would newly route GitLab/Forgejo 422s into find-existing (SC8: no run
    // changed on existing forges). On a match we fetch and return the existing MR/PR
    // so re-running the finish step never dead-ends. Those statuses also cover
    // non-duplicate causes (Forgejo's 409 = other conflicts, GitHub's 422 = any
    // validation error), so findOpenMr may legitimately find none; every driver
    // tolerates that and falls through to the error below rather than pretending
    // success.
    if (this.duplicateStatuses().includes(res.status)) {
      const existing = await this.findOpenMr(p, signal);
      if (existing) return existing;
    }
    throw new ForgeError(res.status, (await safeText(res)).slice(0, 512));
  }

  /** HTTP statuses that mean "an MR/PR may already exist for this head/base → look it
   *  up". GitLab/Forgejo answer 409; GitHubClient widens this to add 422 (D9/R6). */
  protected duplicateStatuses(): number[] {
    return [409];
  }

  /**
   * Read the head SHA of an existing MR/PR through the shared transport (PRD #1226 M4,
   * D5). The forge-specific single-item URL and the head-SHA parse are the only
   * per-driver bits (headUrl/parseHead), mirroring the createUrl/parseMr split — so a new
   * driver inherits the https-only, redirect:"error" and transient-5xx guards from
   * `request` by construction.
   *
   * A transient status (5xx/408/429) already surfaced as a transient ForgeError inside
   * `request`, so the Layer A forge-retry loop can re-run. A non-transient non-200 (a 404
   * for an unknown MR, a 403 for a scope) is a HARD read failure here: it throws a
   * ForgeError the caller reads as "cannot verify H" and keeps the run live. The PAT never
   * leaves the auth header.
   */
  async getMergeRequestHead(repoUrl: string, pat: string, iid: number, signal?: AbortSignal): Promise<string> {
    const res = await this.request("GET", this.headUrl(repoUrl, iid), pat, undefined, signal);
    if (res.status !== 200) throw new ForgeError(res.status, (await safeText(res)).slice(0, 512));
    return this.parseHead(await res.text());
  }

  /**
   * Read one existing MR/PR through the shared transport (PRD #1798 D11). Same resource
   * (`headUrl`) and the same non-200 handling as getMergeRequestHead; the per-driver bit
   * is `parseMrDetail`.
   */
  async getMergeRequest(repoUrl: string, pat: string, iid: number, signal?: AbortSignal): Promise<MergeRequestDetail> {
    const res = await this.request("GET", this.headUrl(repoUrl, iid), pat, undefined, signal);
    if (res.status !== 200) throw new ForgeError(res.status, (await safeText(res)).slice(0, 512));
    // H1: a hostile or broken forge must not make the worker buffer an unbounded body.
    return this.parseMrDetail(await readCapped(res, MR_DETAIL_MAX_BYTES));
  }

  /**
   * Rewrite an existing MR/PR body through the shared transport (PRD #1225). The
   * single-item URL is the SAME resource as `headUrl` for all three drivers (GitLab
   * `merge_requests/{iid}`, Forgejo/GitHub `pulls/{iid}`), so it is reused rather than
   * duplicated. The HTTP method and body-field name are the only per-driver bits
   * (updateMethod/updateBody): GitLab uses PUT + `description`, Forgejo/GitHub use
   * PATCH + `body`. Any 2xx is success (GitLab/GitHub answer 200, Forgejo/Gitea 201).
   * A non-2xx throws a ForgeError; the interlock caller treats a failed rewrite as
   * best-effort and leaves the created MR/PR as-is.
   */
  async updateMergeRequestDescription(repoUrl: string, pat: string, iid: number, description: string, signal?: AbortSignal): Promise<void> {
    const res = await this.request(this.updateMethod(), this.headUrl(repoUrl, iid), pat, this.updateBody(description), signal);
    if (res.status < 200 || res.status >= 300) throw new ForgeError(res.status, (await safeText(res)).slice(0, 512));
  }

  protected async request(
    method: string,
    url: string,
    pat: string,
    body?: unknown,
    signal?: AbortSignal,
  ): Promise<FetchResponse> {
    // Guard 1: never send the PAT over a non-https URL. The credential rides a
    // header, and only TLS keeps it off the wire — a plaintext (or malformed) URL
    // is a hard error, never a best-effort send.
    if (!isHttps(url)) throw new ForgeError(0, "refusing to send the PAT to a non-https forge URL");
    const headers: Record<string, string> = { ...this.authHeaders(pat) };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    let res: FetchResponse;
    try {
      res = await this.fetchFn(url, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: signal
          ? AbortSignal.any([signal, AbortSignal.timeout(this.httpTimeoutMs)])
          : AbortSignal.timeout(this.httpTimeoutMs),
        // Guard 2: a redirect must never carry the PAT header to another origin —
        // turn any 3xx into a transport error instead of following it.
        redirect: "error",
      });
    } catch (err) {
      // A transport failure carries no forge status; surface it without the PAT
      // (the header, not the message, held it).
      throw new ForgeError(0, errMessage(err));
    }
    // PRD #284 D3: a transient forge status must SURFACE as a transient ForgeError
    // at the single transport point, so the Layer A retry loop (forge-retry.ts) can
    // re-run — otherwise findOpenMr's `if (res.status !== 200) return undefined;`
    // silently swallows a 5xx GET, failing a run whose MR actually exists. 409/422
    // (< 500) still return normally into duplicateStatuses; a 404 still returns
    // undefined. Body is capped like the createMergeRequest error path.
    if (res.status >= 500 || res.status === 408 || res.status === 429) {
      throw new ForgeError(res.status, (await safeText(res)).slice(0, 512));
    }
    return res;
  }

  /** The auth header carrying the PAT (GitLab `PRIVATE-TOKEN`, Forgejo `Authorization: token`). */
  protected abstract authHeaders(pat: string): Record<string, string>;
  /** The MR/PR create endpoint derived from the repo web URL. */
  protected abstract createUrl(repoUrl: string): string;
  /** The create request body (forge-specific field names). */
  protected abstract createBody(p: CreateMrParams): unknown;
  /** Find the existing OPEN MR/PR for this source→target on a 409; undefined when none. */
  protected abstract findOpenMr(p: CreateMrParams, signal?: AbortSignal): Promise<MergeRequest | undefined>;
  /** Parse a create (201) response body into a MergeRequest. */
  protected abstract parseMr(text: string): MergeRequest;
  /** The single-MR/PR read endpoint derived from the repo web URL + iid (D5). */
  protected abstract headUrl(repoUrl: string, iid: number): string;
  /** Parse a 200 single-MR/PR response body into the validated 40-hex head SHA;
   *  throw a ForgeError when the head field is absent or malformed. */
  protected abstract parseHead(text: string): string;
  /** Parse a 200 single-MR/PR response body into a MergeRequestDetail; throw a
   *  ForgeError on malformed JSON, an invalid head SHA, a missing target branch, a
   *  non-string description or an unknown state (PRD #1798 D11). */
  protected abstract parseMrDetail(text: string): MergeRequestDetail;

  /** HTTP method for the single-item body rewrite. PATCH is correct for Forgejo and
   *  GitHub; GitLabClient overrides to PUT. */
  protected updateMethod(): string {
    return "PATCH";
  }

  /** Request body for the single-item body rewrite. Forgejo and GitHub name the field
   *  `body`; GitLabClient overrides to `description`. */
  protected updateBody(description: string): unknown {
    return { body: description };
  }
}

/** GitLab REST driver (`/api/v4`, `PRIVATE-TOKEN` header). */
export class GitLabClient extends HttpForgeClient {
  protected authHeaders(pat: string): Record<string, string> {
    return { "PRIVATE-TOKEN": pat };
  }

  protected createUrl(repoUrl: string): string {
    const projectSeg = encodeURIComponent(gitlabProjectPath(repoUrl));
    return `${gitlabBaseUrl(repoUrl)}/api/v4/projects/${projectSeg}/merge_requests`;
  }

  protected createBody(p: CreateMrParams): unknown {
    return {
      source_branch: p.sourceBranch,
      target_branch: p.targetBranch,
      title: p.title,
      description: p.description,
      // Primary directive: never auto-merge; a human merges. Keep the branch so a
      // re-run/resume can find and reuse the same MR.
      remove_source_branch: false,
      squash: false,
    };
  }

  protected async findOpenMr(p: CreateMrParams, signal?: AbortSignal): Promise<MergeRequest | undefined> {
    const q = `${this.createUrl(p.repoUrl)}?state=opened&source_branch=${encodeURIComponent(p.sourceBranch)}&target_branch=${encodeURIComponent(p.targetBranch)}`;
    const res = await this.request("GET", q, p.pat, undefined, signal);
    if (res.status !== 200) return undefined;
    const list = safeJson(await res.text());
    if (Array.isArray(list) && list.length > 0) return parseGitlabMr(list[0]);
    return undefined;
  }

  protected parseMr(text: string): MergeRequest {
    const mr = parseGitlabMr(safeJson(text));
    if (!mr) throw new ForgeError(201, "merge request response missing iid");
    return mr;
  }

  protected headUrl(repoUrl: string, iid: number): string {
    const projectSeg = encodeURIComponent(gitlabProjectPath(repoUrl));
    return `${gitlabBaseUrl(repoUrl)}/api/v4/projects/${projectSeg}/merge_requests/${encodeURIComponent(String(iid))}`;
  }

  protected parseHead(text: string): string {
    const head = parseGitlabHead(safeJson(text));
    if (!head) throw new ForgeError(200, "merge request response missing a valid head sha");
    return head;
  }

  protected parseMrDetail(text: string): MergeRequestDetail {
    const obj = safeJson(text);
    const headSha = parseGitlabHead(obj);
    if (!headSha) throw new ForgeError(200, "merge request response missing a valid head sha");
    const rec = obj as Record<string, unknown>;
    return {
      headSha,
      targetBranch: requireBranch(rec["target_branch"], "merge request"),
      description: bodyText(rec["description"], "merge request"),
      state: gitlabState(rec["state"]),
    };
  }

  /** GitLab rewrites the MR resource with PUT and a `description` field (not PATCH/`body`). */
  protected override updateMethod(): string {
    return "PUT";
  }

  protected override updateBody(description: string): unknown {
    return { description };
  }
}

/** Forgejo REST driver (`/api/v1`, `Authorization: token` header). PRs are modelled
 *  as issues but the pulls endpoints are dedicated; Forgejo ≥16.0.0 is guaranteed
 *  (D4), so the `pulls/{base}/{head}` lookup (gitea 1.22+) is always available. */
export class ForgejoClient extends HttpForgeClient {
  protected authHeaders(pat: string): Record<string, string> {
    return { Authorization: `token ${pat}` };
  }

  protected createUrl(repoUrl: string): string {
    const { apiBase, owner, repo } = forgejoRepoParts(repoUrl);
    return `${apiBase}/api/v1/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls`;
  }

  protected createBody(p: CreateMrParams): unknown {
    // Forgejo's CreatePullRequestOption: head/base branch names, title, body.
    return { head: p.sourceBranch, base: p.targetBranch, title: p.title, body: p.description };
  }

  protected async findOpenMr(p: CreateMrParams, signal?: AbortSignal): Promise<MergeRequest | undefined> {
    // Direct base/head lookup (GetPullRequestByBaseHead, gitea 1.22+ ⇒ every
    // supported Forgejo). A 404 means no PR for this pair; a closed/merged match is
    // not a resume target — either way tolerate it (the 409 may have been a
    // non-duplicate conflict) and let the create error propagate.
    const { apiBase, owner, repo } = forgejoRepoParts(p.repoUrl);
    const url = `${apiBase}/api/v1/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${encodeURIComponent(p.targetBranch)}/${encodeURIComponent(p.sourceBranch)}`;
    const res = await this.request("GET", url, p.pat, undefined, signal);
    if (res.status !== 200) return undefined;
    const obj = safeJson(await res.text());
    if (!obj || typeof obj !== "object") return undefined;
    if ((obj as Record<string, unknown>)["state"] !== "open") return undefined;
    return parseForgejoMr(obj);
  }

  protected parseMr(text: string): MergeRequest {
    const mr = parseForgejoMr(safeJson(text));
    if (!mr) throw new ForgeError(201, "pull request response missing number");
    return mr;
  }

  protected headUrl(repoUrl: string, iid: number): string {
    const { apiBase, owner, repo } = forgejoRepoParts(repoUrl);
    return `${apiBase}/api/v1/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${encodeURIComponent(String(iid))}`;
  }

  protected parseHead(text: string): string {
    const head = parseForgejoHead(safeJson(text));
    if (!head) throw new ForgeError(200, "pull request response missing a valid head sha");
    return head;
  }

  protected parseMrDetail(text: string): MergeRequestDetail {
    return parsePrDetail(safeJson(text));
  }
}

/** GitHub REST driver (`api.github.com`, `Authorization: Bearer` header). PRs live at
 *  `/repos/{owner}/{repo}/pulls`; the create response uses `number` (the iid) +
 *  `html_url` (the web URL). GitHub answers 422 (not 409) when a PR already exists for
 *  the same head/base, so it widens `duplicateStatuses()` (D9/R6). */
export class GitHubClient extends HttpForgeClient {
  protected authHeaders(pat: string): Record<string, string> {
    return { Authorization: `Bearer ${pat}` };
  }

  protected createUrl(repoUrl: string): string {
    const { apiBase, owner, repo } = githubRepoParts(repoUrl);
    return `${apiBase}/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls`;
  }

  protected createBody(p: CreateMrParams): unknown {
    // GitHub's create-PR body: head/base branch names, title, body.
    return { head: p.sourceBranch, base: p.targetBranch, title: p.title, body: p.description };
  }

  /** GitHub returns 422 (its generic validation status), not 409, when a PR already
   *  exists for this head/base — add it to the duplicate set (R6). A 422 for some
   *  OTHER reason still surfaces because findOpenMr finds no open PR and the create
   *  error falls through, the same tolerance the Forgejo 409 path has. */
  protected override duplicateStatuses(): number[] {
    return [409, 422];
  }

  protected async findOpenMr(p: CreateMrParams, signal?: AbortSignal): Promise<MergeRequest | undefined> {
    // List open PRs filtered by head (`owner:branch`, same-repo) and base. A match is
    // the first array element; anything else (empty list, non-200) means no resume
    // target and the create error propagates.
    const { apiBase, owner, repo } = githubRepoParts(p.repoUrl);
    const head = `${owner}:${p.sourceBranch}`;
    const url = `${apiBase}/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls?state=open&head=${encodeURIComponent(head)}&base=${encodeURIComponent(p.targetBranch)}`;
    const res = await this.request("GET", url, p.pat, undefined, signal);
    if (res.status !== 200) return undefined;
    const list = safeJson(await res.text());
    if (!Array.isArray(list) || list.length === 0) return undefined;
    const first = list[0];
    if (!first || typeof first !== "object") return undefined;
    if ((first as Record<string, unknown>)["state"] !== "open") return undefined;
    return parseGitHubMr(first);
  }

  protected parseMr(text: string): MergeRequest {
    const mr = parseGitHubMr(safeJson(text));
    if (!mr) throw new ForgeError(201, "pull request response missing number");
    return mr;
  }

  protected headUrl(repoUrl: string, iid: number): string {
    const { apiBase, owner, repo } = githubRepoParts(repoUrl);
    return `${apiBase}/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${encodeURIComponent(String(iid))}`;
  }

  protected parseHead(text: string): string {
    const head = parseGitHubHead(safeJson(text));
    if (!head) throw new ForgeError(200, "pull request response missing a valid head sha");
    return head;
  }

  protected parseMrDetail(text: string): MergeRequestDetail {
    return parsePrDetail(safeJson(text));
  }
}

/** Pick the worker's forge client for a claim's `forge_type` (absent ⇒ gitlab, R8). */
export function forgeClientFor(forgeType: string | undefined, opts: ForgeClientOptions = {}): ForgeClient {
  return forgeType === "github"
    ? new GitHubClient(opts)
    : forgeType === "forgejo"
      ? new ForgejoClient(opts)
      : new GitLabClient(opts);
}

/** Derive the GitLab API base (scheme://host) from a repo web/clone URL. */
export function gitlabBaseUrl(repoUrl: string): string {
  const u = new URL(repoUrl);
  return `${u.protocol}//${u.host}`;
}

/** Derive the namespaced GitLab project path (`group/sub/repo`) from a repo URL. */
export function gitlabProjectPath(repoUrl: string): string {
  const u = new URL(repoUrl);
  return u.pathname.replace(/^\/+/, "").replace(/\/+$/, "").replace(/\.git$/, "");
}

/**
 * Split a Forgejo repo web URL into its API base + owner + repo (D9 subpath fix).
 * Forgejo repos are always `{ROOT_URL}/{owner}/{repo}` — owner is one user/org and
 * repo is one name, so the last two path segments are owner/repo and everything
 * before them is the ROOT_URL subpath, which stays on the API base. So
 * `https://example.com/git/owner/repo` → base `https://example.com/git`, not
 * `https://example.com` with `git` leaking into the project path.
 */
export function forgejoRepoParts(repoUrl: string): { apiBase: string; owner: string; repo: string } {
  const u = new URL(repoUrl);
  const segs = u.pathname.replace(/^\/+/, "").replace(/\/+$/, "").split("/").filter(Boolean);
  const repo = (segs.pop() ?? "").replace(/\.git$/, "");
  const owner = segs.pop() ?? "";
  if (!owner || !repo) throw new ForgeError(0, "cannot derive owner/repo from Forgejo repo URL");
  const subpath = segs.length > 0 ? `/${segs.join("/")}` : "";
  return { apiBase: `${u.protocol}//${u.host}${subpath}`, owner, repo };
}

/**
 * Split a GitHub repo web URL into its API base + owner + repo (D3 host mapping).
 * Unlike Forgejo, GitHub's API lives on a DIFFERENT SUBDOMAIN, not a path: the web
 * host `github.com` maps to `api.github.com` (`https://github.com/owner/repo` →
 * base `https://api.github.com`). The last two path segments are owner/repo (a
 * trailing `.git` is stripped); there is no ROOT_URL subpath to preserve.
 * GHES (self-hosted, `api.v3` under a path) is out of scope for v1 (github.com only,
 * D3); for any non-github.com host we fall back to prefixing `api.`, but the
 * supported surface is github.com.
 */
export function githubRepoParts(repoUrl: string): { apiBase: string; owner: string; repo: string } {
  const u = new URL(repoUrl);
  const segs = u.pathname.replace(/^\/+/, "").replace(/\/+$/, "").split("/").filter(Boolean);
  const repo = (segs.pop() ?? "").replace(/\.git$/, "");
  const owner = segs.pop() ?? "";
  if (!owner || !repo) throw new ForgeError(0, "cannot derive owner/repo from GitHub repo URL");
  const host = u.host === "github.com" ? "api.github.com" : `api.${u.host.replace(/^www\./, "")}`;
  return { apiBase: `${u.protocol}//${host}`, owner, repo };
}

function isHttps(url: string): boolean {
  try {
    return new URL(url).protocol === "https:";
  } catch {
    return false;
  }
}

/** Parse a GitLab MR object (`iid`, `web_url`). */
function parseGitlabMr(obj: unknown): MergeRequest | undefined {
  if (!obj || typeof obj !== "object") return undefined;
  const rec = obj as Record<string, unknown>;
  const iid = rec["iid"];
  if (typeof iid !== "number") return undefined;
  const webUrl = typeof rec["web_url"] === "string" ? (rec["web_url"] as string) : "";
  return { iid, webUrl };
}

/** Parse a Forgejo PR object (`number` = iid, `html_url` = web URL). */
function parseForgejoMr(obj: unknown): MergeRequest | undefined {
  if (!obj || typeof obj !== "object") return undefined;
  const rec = obj as Record<string, unknown>;
  const iid = rec["number"];
  if (typeof iid !== "number") return undefined;
  const webUrl = typeof rec["html_url"] === "string" ? (rec["html_url"] as string) : "";
  return { iid, webUrl };
}

/** Parse a GitHub PR object (`number` = iid, `html_url` = web URL). */
function parseGitHubMr(obj: unknown): MergeRequest | undefined {
  if (!obj || typeof obj !== "object") return undefined;
  const rec = obj as Record<string, unknown>;
  const iid = rec["number"];
  if (typeof iid !== "number") return undefined;
  const webUrl = typeof rec["html_url"] === "string" ? (rec["html_url"] as string) : "";
  return { iid, webUrl };
}

/** A git commit id is exactly 40 lowercase-or-uppercase hex chars. The forges emit
 *  lowercase; case-insensitive here keeps the validator lenient without normalizing —
 *  the exact string is returned verbatim for the head identity. */
function isCommitSha(v: unknown): v is string {
  return typeof v === "string" && /^[0-9a-f]{40}$/i.test(v);
}

/** Parse a GitLab MR object's head SHA (D5). PREFERS `diff_refs.head_sha` (the true
 *  source-branch tip) and falls back to the top-level `sha`, which can LAG the branch
 *  tip in some MR states. Returns undefined when neither is a valid 40-hex commit id. */
function parseGitlabHead(obj: unknown): string | undefined {
  if (!obj || typeof obj !== "object") return undefined;
  const rec = obj as Record<string, unknown>;
  const diffRefs = rec["diff_refs"];
  if (diffRefs && typeof diffRefs === "object") {
    const headSha = (diffRefs as Record<string, unknown>)["head_sha"];
    if (isCommitSha(headSha)) return headSha;
  }
  const sha = rec["sha"];
  if (isCommitSha(sha)) return sha;
  return undefined;
}

/** Parse a Forgejo PR object's head SHA from `head.sha` (D5); undefined when absent or
 *  not a valid 40-hex commit id. */
function parseForgejoHead(obj: unknown): string | undefined {
  return prHeadSha(obj);
}

/** Parse a GitHub PR object's head SHA from `head.sha` (D5); undefined when absent or
 *  not a valid 40-hex commit id. */
function parseGitHubHead(obj: unknown): string | undefined {
  return prHeadSha(obj);
}

/** Forgejo and GitHub both nest the source-branch tip at `head.sha`; shared extractor. */
function prHeadSha(obj: unknown): string | undefined {
  if (!obj || typeof obj !== "object") return undefined;
  const head = (obj as Record<string, unknown>)["head"];
  if (!head || typeof head !== "object") return undefined;
  const sha = (head as Record<string, unknown>)["sha"];
  return isCommitSha(sha) ? sha : undefined;
}

/** GitLab MR `state` → normalised state (`opened` → `open`); unknown → ForgeError. */
function gitlabState(v: unknown): MergeRequestState {
  switch (v) {
    case "opened":
      return "open";
    case "closed":
    case "merged":
    case "locked":
      return v;
    default:
      throw new ForgeError(200, "merge request response has an unknown state");
  }
}

/** Forgejo and GitHub share the PR shape the detail read needs: `head.sha`, `base.ref`,
 *  `body`, and `state` open/closed with a merge marker (`merged: true`, or GitHub's
 *  non-null `merged_at`) that turns a closed PR into `merged`. */
function parsePrDetail(obj: unknown): MergeRequestDetail {
  const headSha = prHeadSha(obj);
  if (!headSha) throw new ForgeError(200, "pull request response missing a valid head sha");
  const rec = obj as Record<string, unknown>;
  const base = rec["base"];
  const targetBranch = requireBranch(
    base && typeof base === "object" ? (base as Record<string, unknown>)["ref"] : undefined,
    "pull request",
  );
  const merged = rec["merged"] === true || (typeof rec["merged_at"] === "string" && rec["merged_at"] !== "");
  const raw = rec["state"];
  if (raw !== "open" && raw !== "closed") throw new ForgeError(200, "pull request response has an unknown state");
  return {
    headSha,
    targetBranch,
    description: bodyText(rec["body"], "pull request"),
    state: merged ? "merged" : raw,
  };
}

function requireBranch(v: unknown, what: string): string {
  if (typeof v !== "string" || v === "") throw new ForgeError(200, `${what} response missing a target branch`);
  return v;
}

/** A null/absent body is an empty description; any other non-string is malformed. */
function bodyText(v: unknown, what: string): string {
  if (v === null || v === undefined) return "";
  if (typeof v !== "string") throw new ForgeError(200, `${what} response has a non-string description`);
  return v;
}

function safeJson(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}

/** An error body for a message: at most ERROR_BODY_MAX_BYTES are read (a streaming body is
 *  cancelled past that), and a read failure is "". */
async function safeText(res: FetchResponse): Promise<string> {
  try {
    return (await readPrefix(res, ERROR_BODY_MAX_BYTES)).text.trim();
  } catch {
    return "";
  }
}

/**
 * The body as UTF-8 text, read to at most `maxBytes` bytes. A streaming body stops being read at the
 * first chunk that crosses the cap and is cancelled (the rest is never buffered); a transport
 * without `body` (a test fake) is read with text() and its UTF-8 length checked against the cap.
 * `over` reports that the body was longer than `maxBytes` (then `text` is the decoded prefix).
 */
async function readPrefix(res: FetchResponse, maxBytes: number): Promise<{ text: string; over: boolean }> {
  const stream = res.body;
  if (!stream) {
    const text = await res.text();
    if (Buffer.byteLength(text, "utf8") <= maxBytes) return { text, over: false };
    return { text: Buffer.from(text, "utf8").subarray(0, maxBytes).toString("utf8"), over: true };
  }
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  let over = false;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      if (total + value.byteLength > maxBytes) {
        chunks.push(value.subarray(0, maxBytes - total));
        total = maxBytes;
        over = true;
        break;
      }
      chunks.push(value);
      total += value.byteLength;
    }
  } finally {
    if (over) await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
  return { text: Buffer.concat(chunks, total).toString("utf8"), over };
}

/** The whole body, or a ForgeResponseTooLarge once it passes `maxBytes`. */
async function readCapped(res: FetchResponse, maxBytes: number): Promise<string> {
  const { text, over } = await readPrefix(res, maxBytes);
  if (over) throw new ForgeResponseTooLarge(maxBytes);
  return text;
}
