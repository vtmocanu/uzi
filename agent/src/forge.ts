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
// MR/PR itself and does not need the 19-method Go `Forge` interface (D9). The
// three transport guards below (non-https refusal, redirect:"error",
// 409-on-duplicate → fetch existing) are INTERFACE requirements, not per-driver
// details — so they live in the shared base and a fresh driver cannot forget one.
//
// `fetchFn` is injectable so the MR/PR path is tested up to — never across — the
// network boundary with a fake transport (testing-credentials policy).

import { errMessage } from "./util.js";

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
) => Promise<{ status: number; text(): Promise<string> }>;

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

export class ForgeError extends Error {
  constructor(
    readonly status: number,
    readonly detail: string,
  ) {
    super(`forge API returned ${status}: ${detail}`);
    this.name = "ForgeError";
  }
}

/** The worker's minimal forge seam (D9): open (or resume) the MR/PR for a pushed
 *  branch. Deliberately one method — the worker never reads issues, labels, or
 *  pipelines; that surface is the Go driver's. */
export interface ForgeClient {
  createMergeRequest(p: CreateMrParams): Promise<MergeRequest>;
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
 *   3. `createMergeRequest` treats a 409 as "already exists" and returns the open
 *      MR/PR — tolerating the driver finding none (Forgejo's 409 also covers other
 *      conflicts), so a resumed finish step never dead-ends.
 * A driver supplies only the forge-specific bits: auth header, create URL/body, the
 * find-existing lookup, and the response parse.
 */
abstract class HttpForgeClient implements ForgeClient {
  protected readonly fetchFn: FetchFn;
  protected readonly httpTimeoutMs: number;

  constructor(opts: ForgeClientOptions = {}) {
    this.fetchFn = opts.fetchFn ?? (globalThis.fetch as unknown as FetchFn);
    this.httpTimeoutMs = opts.httpTimeoutMs ?? 30_000;
  }

  async createMergeRequest(p: CreateMrParams): Promise<MergeRequest> {
    const res = await this.request("POST", this.createUrl(p.repoUrl), p.pat, this.createBody(p));
    if (res.status === 201) return this.parseMr(await res.text());

    // An MR/PR for this branch may already exist (a resume, or a prior finish that
    // pushed + opened before the state report landed) — the forge answers 409. Fetch
    // and return the existing one so re-running the finish step never dead-ends.
    // Forgejo's 409 also covers non-duplicate conflicts (a real merge/branch clash),
    // so findOpenMr may legitimately find none; both drivers tolerate that and fall
    // through to the error below rather than pretending success.
    if (res.status === 409) {
      const existing = await this.findOpenMr(p);
      if (existing) return existing;
    }
    throw new ForgeError(res.status, (await safeText(res)).slice(0, 512));
  }

  protected async request(
    method: string,
    url: string,
    pat: string,
    body?: unknown,
  ): Promise<{ status: number; text(): Promise<string> }> {
    // Guard 1: never send the PAT over a non-https URL. The credential rides a
    // header, and only TLS keeps it off the wire — a plaintext (or malformed) URL
    // is a hard error, never a best-effort send.
    if (!isHttps(url)) throw new ForgeError(0, "refusing to send the PAT to a non-https forge URL");
    const headers: Record<string, string> = { ...this.authHeaders(pat) };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    try {
      return await this.fetchFn(url, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(this.httpTimeoutMs),
        // Guard 2: a redirect must never carry the PAT header to another origin —
        // turn any 3xx into a transport error instead of following it.
        redirect: "error",
      });
    } catch (err) {
      // A transport failure carries no forge status; surface it without the PAT
      // (the header, not the message, held it).
      throw new ForgeError(0, errMessage(err));
    }
  }

  /** The auth header carrying the PAT (GitLab `PRIVATE-TOKEN`, Forgejo `Authorization: token`). */
  protected abstract authHeaders(pat: string): Record<string, string>;
  /** The MR/PR create endpoint derived from the repo web URL. */
  protected abstract createUrl(repoUrl: string): string;
  /** The create request body (forge-specific field names). */
  protected abstract createBody(p: CreateMrParams): unknown;
  /** Find the existing OPEN MR/PR for this source→target on a 409; undefined when none. */
  protected abstract findOpenMr(p: CreateMrParams): Promise<MergeRequest | undefined>;
  /** Parse a create (201) response body into a MergeRequest. */
  protected abstract parseMr(text: string): MergeRequest;
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

  protected async findOpenMr(p: CreateMrParams): Promise<MergeRequest | undefined> {
    const q = `${this.createUrl(p.repoUrl)}?state=opened&source_branch=${encodeURIComponent(p.sourceBranch)}&target_branch=${encodeURIComponent(p.targetBranch)}`;
    const res = await this.request("GET", q, p.pat);
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

  protected async findOpenMr(p: CreateMrParams): Promise<MergeRequest | undefined> {
    // Direct base/head lookup (GetPullRequestByBaseHead, gitea 1.22+ ⇒ every
    // supported Forgejo). A 404 means no PR for this pair; a closed/merged match is
    // not a resume target — either way tolerate it (the 409 may have been a
    // non-duplicate conflict) and let the create error propagate.
    const { apiBase, owner, repo } = forgejoRepoParts(p.repoUrl);
    const url = `${apiBase}/api/v1/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${encodeURIComponent(p.targetBranch)}/${encodeURIComponent(p.sourceBranch)}`;
    const res = await this.request("GET", url, p.pat);
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
}

/** Pick the worker's forge client for a claim's `forge_type` (absent ⇒ gitlab, R8). */
export function forgeClientFor(forgeType: string | undefined, opts: ForgeClientOptions = {}): ForgeClient {
  return forgeType === "forgejo" ? new ForgejoClient(opts) : new GitLabClient(opts);
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

function safeJson(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}

async function safeText(res: { text(): Promise<string> }): Promise<string> {
  try {
    return (await res.text()).trim();
  } catch {
    return "";
  }
}
