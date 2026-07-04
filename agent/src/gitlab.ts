// Worker-performed GitLab merge-request creation (PRD #4 §Workflow, primary
// directive).
//
// The AGENT never has a push credential and never talks to GitLab; the WORKER
// does, holding the bot PAT. After the agent signals done and the worker pushes
// the branch (git.ts), it opens the MR here. The PAT rides the `PRIVATE-TOKEN`
// HEADER only — never the URL, never argv, never a log line — mirroring the
// header-auth discipline the git layer uses for clone/fetch/push. The MR links
// the issue and is NEVER merged (humans merge).
//
// `fetchFn` is injectable so the MR path is tested up to — never across — the
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
  /** GitLab base URL, e.g. https://gitlab.example.com (no trailing slash). */
  baseUrl: string;
  /** Namespaced project path, e.g. `group/sub/repo` (URL-encoded internally). */
  projectPath: string;
  pat: string;
  sourceBranch: string;
  targetBranch: string;
  title: string;
  description: string;
}

export interface MergeRequest {
  iid: number;
  webUrl: string;
}

export class GitLabError extends Error {
  constructor(
    readonly status: number,
    readonly detail: string,
  ) {
    super(`GitLab API returned ${status}: ${detail}`);
    this.name = "GitLabError";
  }
}

export interface GitLabClientOptions {
  fetchFn?: FetchFn;
  httpTimeoutMs?: number;
}

export class GitLabClient {
  private readonly fetchFn: FetchFn;
  private readonly httpTimeoutMs: number;

  constructor(opts: GitLabClientOptions = {}) {
    this.fetchFn = opts.fetchFn ?? (globalThis.fetch as unknown as FetchFn);
    this.httpTimeoutMs = opts.httpTimeoutMs ?? 30_000;
  }

  /**
   * Open a merge request for the pushed branch, returning its iid + web URL.
   * Idempotent across a resume: if an open MR already exists for the same source
   * branch (GitLab answers 409), the existing one is fetched and returned rather
   * than erroring, so re-running the finish step never dead-ends.
   */
  async createMergeRequest(p: CreateMrParams): Promise<MergeRequest> {
    // Refuse a non-https base: the PAT rides a header, and only TLS keeps it off
    // the wire. A plaintext (or malformed) base URL is a hard error, never a
    // best-effort send (M4 audit N1, same PAT class as the git host-scoping).
    if (!isHttps(p.baseUrl)) throw new GitLabError(0, "refusing to send the PAT to a non-https GitLab base URL");
    const projectSeg = encodeURIComponent(p.projectPath);
    const base = `${p.baseUrl.replace(/\/+$/, "")}/api/v4/projects/${projectSeg}/merge_requests`;
    const res = await this.request("POST", base, p.pat, {
      source_branch: p.sourceBranch,
      target_branch: p.targetBranch,
      title: p.title,
      description: p.description,
      // Primary directive: never auto-merge; a human merges. Keep the branch so a
      // re-run/resume can find and reuse the same MR.
      remove_source_branch: false,
      squash: false,
    });

    if (res.status === 201) return parseMr(await res.text());

    // An MR for this branch may already exist (resume, or a prior finish that
    // pushed + opened before the state report landed). GitLab returns 409.
    if (res.status === 409) {
      const existing = await this.findOpenMr(base, p.pat, p.sourceBranch, p.targetBranch);
      if (existing) return existing;
    }
    throw new GitLabError(res.status, (await safeText(res)).slice(0, 512));
  }

  private async findOpenMr(
    base: string,
    pat: string,
    sourceBranch: string,
    targetBranch: string,
  ): Promise<MergeRequest | undefined> {
    const q = `${base}?state=opened&source_branch=${encodeURIComponent(sourceBranch)}&target_branch=${encodeURIComponent(targetBranch)}`;
    const res = await this.request("GET", q, pat);
    if (res.status !== 200) return undefined;
    const list = safeJson(await res.text());
    if (Array.isArray(list) && list.length > 0) return parseMrObject(list[0]);
    return undefined;
  }

  private async request(
    method: string,
    url: string,
    pat: string,
    body?: unknown,
  ): Promise<{ status: number; text(): Promise<string> }> {
    const headers: Record<string, string> = { "PRIVATE-TOKEN": pat };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    try {
      return await this.fetchFn(url, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(this.httpTimeoutMs),
        // A redirect must never carry the PAT header to another origin: turn any
        // 3xx into a transport error instead of following it (M4 audit N1).
        redirect: "error",
      });
    } catch (err) {
      // A transport failure carries no GitLab status; surface it without the PAT
      // (the header, not the message, held it).
      throw new GitLabError(0, errMessage(err));
    }
  }
}

/** Derive the GitLab base URL (scheme://host) from a repo web/clone URL. */
export function gitlabBaseUrl(repoUrl: string): string {
  const u = new URL(repoUrl);
  return `${u.protocol}//${u.host}`;
}

/** Derive the namespaced project path (`group/sub/repo`) from a repo URL. */
export function gitlabProjectPath(repoUrl: string): string {
  const u = new URL(repoUrl);
  return u.pathname.replace(/^\/+/, "").replace(/\/+$/, "").replace(/\.git$/, "");
}

function isHttps(baseUrl: string): boolean {
  try {
    return new URL(baseUrl).protocol === "https:";
  } catch {
    return false;
  }
}

function parseMr(text: string): MergeRequest {
  const mr = parseMrObject(safeJson(text));
  if (!mr) throw new GitLabError(201, "merge request response missing iid");
  return mr;
}

function parseMrObject(obj: unknown): MergeRequest | undefined {
  if (!obj || typeof obj !== "object") return undefined;
  const rec = obj as Record<string, unknown>;
  const iid = rec["iid"];
  if (typeof iid !== "number") return undefined;
  const webUrl = typeof rec["web_url"] === "string" ? (rec["web_url"] as string) : "";
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
