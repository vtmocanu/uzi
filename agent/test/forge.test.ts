import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  GitLabClient,
  ForgejoClient,
  GitHubClient,
  ForgeError,
  ForgeResponseTooLarge,
  MR_DETAIL_MAX_BYTES,
  forgeClientFor,
  gitlabBaseUrl,
  gitlabProjectPath,
  forgejoRepoParts,
  githubRepoParts,
  type FetchFn,
} from "../src/forge.js";

// The MR/PR path is exercised up to — never across — the network boundary via an
// injected fake transport (testing-credentials policy). The PAT rides an auth
// header only, never the URL or body.

interface Call {
  url: string;
  method: string;
  headers: Record<string, string>;
  body?: string;
  redirect?: string;
  signal?: AbortSignal | null;
}

function recorder(responses: Array<{ status: number; body: unknown }>): { fetchFn: FetchFn; calls: Call[] } {
  const calls: Call[] = [];
  let i = 0;
  const fetchFn: FetchFn = async (url, init) => {
    calls.push({ url, method: init.method, headers: init.headers, body: init.body, redirect: init.redirect, signal: init.signal });
    const r = responses[Math.min(i, responses.length - 1)]!;
    i++;
    return { status: r.status, text: async () => (typeof r.body === "string" ? r.body : JSON.stringify(r.body)) };
  };
  return { fetchFn, calls };
}

const PAT = "glpat-fixture-do-not-scan";
const base = {
  repoUrl: "https://gitlab.example.com/group/sub/repo",
  pat: PAT,
  sourceBranch: "agent/issue-5",
  targetBranch: "main",
  title: "Fix login",
  description: "Closes #5",
};

describe("GitLabClient.createMergeRequest", () => {
  it("POSTs to the MR endpoint with the PAT in the header (never URL/body) and returns iid + web url", async () => {
    const { fetchFn, calls } = recorder([{ status: 201, body: { iid: 12, web_url: "https://gitlab.example.com/group/sub/repo/-/merge_requests/12" } }]);
    const mr = await new GitLabClient({ fetchFn }).createMergeRequest(base);

    assert.deepStrictEqual(mr, { iid: 12, webUrl: "https://gitlab.example.com/group/sub/repo/-/merge_requests/12" });
    const call = calls[0]!;
    assert.strictEqual(call.method, "POST");
    // Project path is URL-encoded (subgroups included).
    assert.match(call.url, /\/api\/v4\/projects\/group%2Fsub%2Frepo\/merge_requests$/);
    assert.strictEqual(call.headers["PRIVATE-TOKEN"], PAT);
    assert.ok(!call.url.includes(PAT), "PAT not in URL");
    assert.ok(!(call.body ?? "").includes(PAT), "PAT not in body");
    const body = JSON.parse(call.body ?? "{}");
    assert.strictEqual(body.source_branch, "agent/issue-5");
    assert.strictEqual(body.target_branch, "main");
    assert.strictEqual(body.remove_source_branch, false);
  });

  it("is idempotent: on 409 it finds and returns the existing open MR", async () => {
    const { fetchFn, calls } = recorder([
      { status: 409, body: { message: ["Another open merge request already exists for this source branch: !7"] } },
      { status: 200, body: [{ iid: 7, web_url: "https://gitlab.example.com/x/-/merge_requests/7" }] },
    ]);
    const mr = await new GitLabClient({ fetchFn }).createMergeRequest(base);
    assert.deepStrictEqual(mr, { iid: 7, webUrl: "https://gitlab.example.com/x/-/merge_requests/7" });
    // Second call is the GET lookup, still carrying the PAT header only.
    assert.strictEqual(calls[1]!.method, "GET");
    assert.strictEqual(calls[1]!.headers["PRIVATE-TOKEN"], PAT);
    assert.match(calls[1]!.url, /state=opened&source_branch=agent%2Fissue-5/);
  });

  it("propagates boundary cancellation through the duplicate-MR lookup GET", async () => {
    const calls: Call[] = [];
    let lookupSettled = false;
    const fetchFn: FetchFn = async (url, init) => {
      calls.push({ url, method: init.method, headers: init.headers, body: init.body, redirect: init.redirect, signal: init.signal });
      if (calls.length === 1) return { status: 409, text: async () => "duplicate" };
      assert.ok(init.signal, "duplicate-MR lookup must receive the boundary signal");
      await new Promise<void>((_, reject) => {
        const abort = (): void => {
          lookupSettled = true;
          reject(new Error("lookup cancelled"));
        };
        if (init.signal?.aborted) abort();
        else init.signal?.addEventListener("abort", abort, { once: true });
      });
      throw new Error("unreachable");
    };
    const abort = new AbortController();
    const startedAt = Date.now();
    const lookup = new GitLabClient({ fetchFn, httpTimeoutMs: 5_000 }).createMergeRequest(base, abort.signal);
    setTimeout(() => abort.abort(), 10);

    await assert.rejects(lookup, /lookup cancelled/);
    assert.equal(calls.length, 2);
    assert.equal(calls[1]?.method, "GET");
    assert.equal(calls[1]?.signal?.aborted, true);
    assert.equal(lookupSettled, true, "createMergeRequest waited for the lookup to observe cancellation");
    assert.ok(Date.now() - startedAt < 1_000, "the independent 5s lookup timeout did not control cancellation");
  });

  it("throws a ForgeError (no PAT in the message) on an unexpected status", async () => {
    const { fetchFn } = recorder([{ status: 403, body: { message: "insufficient scope" } }]);
    await assert.rejects(
      new GitLabClient({ fetchFn }).createMergeRequest(base),
      (err: unknown) => err instanceof ForgeError && err.status === 403 && !err.message.includes(PAT),
    );
  });

  it("does NOT route a 422 into find-existing — GitLab's duplicate is 409, so a 422 surfaces the real error (SC8: no run changed on existing forges)", async () => {
    // Only one response is queued: if the base wrongly treated 422 as a duplicate it
    // would fire a second (GET) request and the recorder would clamp to the same 422,
    // but the invariant under test is that the 422 propagates untouched.
    const { fetchFn, calls } = recorder([{ status: 422, body: { message: "validation failed" } }]);
    await assert.rejects(
      new GitLabClient({ fetchFn }).createMergeRequest(base),
      (err: unknown) => err instanceof ForgeError && err.status === 422,
    );
    assert.strictEqual(calls.length, 1, "no find-existing lookup on a GitLab 422");
  });

  it("pins redirect:error so a 3xx cannot replay the PAT header cross-origin (N1)", async () => {
    const { fetchFn, calls } = recorder([{ status: 201, body: { iid: 1, web_url: "https://x/1" } }]);
    await new GitLabClient({ fetchFn }).createMergeRequest(base);
    assert.strictEqual(calls[0]!.redirect, "error");
  });

  it("refuses a non-https base URL and never sends the PAT (N1)", async () => {
    const { fetchFn, calls } = recorder([{ status: 201, body: {} }]);
    await assert.rejects(
      new GitLabClient({ fetchFn }).createMergeRequest({ ...base, repoUrl: "http://gitlab.example.com/group/sub/repo" }),
      (err: unknown) => err instanceof ForgeError && /non-https/.test(err.message),
    );
    assert.strictEqual(calls.length, 0, "no request made to a non-https base");
  });
});

// Forgejo speaks /api/v1 with `Authorization: token`, PRs at /pulls, and its create
// response uses `number` (the iid) + `html_url` (the web URL).
const fjBase = {
  repoUrl: "https://forgejo.example.com/org/repo",
  pat: PAT,
  sourceBranch: "agent/issue-5",
  targetBranch: "main",
  title: "Fix login",
  description: "Closes #5",
};

describe("ForgejoClient.createMergeRequest", () => {
  it("POSTs to the pulls endpoint with the token header and maps number→iid, html_url→webUrl", async () => {
    const { fetchFn, calls } = recorder([{ status: 201, body: { number: 9, html_url: "https://forgejo.example.com/org/repo/pulls/9" } }]);
    const mr = await new ForgejoClient({ fetchFn }).createMergeRequest(fjBase);

    assert.deepStrictEqual(mr, { iid: 9, webUrl: "https://forgejo.example.com/org/repo/pulls/9" });
    const call = calls[0]!;
    assert.strictEqual(call.method, "POST");
    assert.match(call.url, /\/api\/v1\/repos\/org\/repo\/pulls$/);
    // PAT rides the Authorization header only, never URL/body.
    assert.strictEqual(call.headers["Authorization"], `token ${PAT}`);
    assert.ok(!call.url.includes(PAT), "PAT not in URL");
    assert.ok(!(call.body ?? "").includes(PAT), "PAT not in body");
    // Forgejo's field names: head/base/body (not source_branch/target_branch/description).
    const body = JSON.parse(call.body ?? "{}");
    assert.strictEqual(body.head, "agent/issue-5");
    assert.strictEqual(body.base, "main");
    assert.strictEqual(body.body, "Closes #5");
  });

  it("is idempotent: on 409 it fetches the existing OPEN PR via pulls/{base}/{head}", async () => {
    const { fetchFn, calls } = recorder([
      { status: 409, body: { message: "pull request already exists" } },
      { status: 200, body: { number: 7, html_url: "https://forgejo.example.com/org/repo/pulls/7", state: "open" } },
    ]);
    const mr = await new ForgejoClient({ fetchFn }).createMergeRequest(fjBase);
    assert.deepStrictEqual(mr, { iid: 7, webUrl: "https://forgejo.example.com/org/repo/pulls/7" });
    // The lookup encodes base + head as path segments (branch names may contain `/`).
    assert.strictEqual(calls[1]!.method, "GET");
    assert.strictEqual(calls[1]!.headers["Authorization"], `token ${PAT}`);
    assert.match(calls[1]!.url, /\/pulls\/main\/agent%2Fissue-5$/);
  });

  it("tolerates a 409 with no open PR (Forgejo's 409 also covers other conflicts): 404 lookup → the create error surfaces", async () => {
    const { fetchFn } = recorder([
      { status: 409, body: { message: "merge conflict" } },
      { status: 404, body: { message: "pull request does not exist" } },
    ]);
    await assert.rejects(
      new ForgejoClient({ fetchFn }).createMergeRequest(fjBase),
      (err: unknown) => err instanceof ForgeError && err.status === 409,
    );
  });

  it("does not resume a closed/merged PR match (state !== open) → the create error surfaces", async () => {
    const { fetchFn } = recorder([
      { status: 409, body: { message: "conflict" } },
      { status: 200, body: { number: 3, html_url: "https://forgejo.example.com/org/repo/pulls/3", state: "closed" } },
    ]);
    await assert.rejects(
      new ForgejoClient({ fetchFn }).createMergeRequest(fjBase),
      (err: unknown) => err instanceof ForgeError && err.status === 409,
    );
  });

  it("does NOT route a 422 into find-existing — Forgejo's duplicate is 409, so a 422 surfaces the real error (SC8: no run changed on existing forges)", async () => {
    const { fetchFn, calls } = recorder([{ status: 422, body: { message: "validation error" } }]);
    await assert.rejects(
      new ForgejoClient({ fetchFn }).createMergeRequest(fjBase),
      (err: unknown) => err instanceof ForgeError && err.status === 422,
    );
    assert.strictEqual(calls.length, 1, "no find-existing lookup on a Forgejo 422");
  });

  it("pins redirect:error so a 3xx cannot replay the token header cross-origin (N1)", async () => {
    const { fetchFn, calls } = recorder([{ status: 201, body: { number: 1, html_url: "https://x/1" } }]);
    await new ForgejoClient({ fetchFn }).createMergeRequest(fjBase);
    assert.strictEqual(calls[0]!.redirect, "error");
  });

  it("refuses a non-https base URL and never sends the PAT (N1) — the guard is inherited, not re-implemented", async () => {
    const { fetchFn, calls } = recorder([{ status: 201, body: {} }]);
    await assert.rejects(
      new ForgejoClient({ fetchFn }).createMergeRequest({ ...fjBase, repoUrl: "http://forgejo.example.com/org/repo" }),
      (err: unknown) => err instanceof ForgeError && /non-https/.test(err.message),
    );
    assert.strictEqual(calls.length, 0, "no request made to a non-https base");
  });

  it("keeps a ROOT_URL subpath on the API base (D9), so owner/repo do not absorb it", async () => {
    const { fetchFn, calls } = recorder([{ status: 201, body: { number: 2, html_url: "https://example.com/git/org/repo/pulls/2" } }]);
    await new ForgejoClient({ fetchFn }).createMergeRequest({ ...fjBase, repoUrl: "https://example.com/git/org/repo.git" });
    // Subpath /git stays on the base; the project is still org/repo, not git/org.
    assert.match(calls[0]!.url, /^https:\/\/example\.com\/git\/api\/v1\/repos\/org\/repo\/pulls$/);
  });
});

// GitHub speaks api.github.com (a DIFFERENT subdomain from the github.com web host,
// D3) with `Authorization: Bearer`, PRs at /pulls, and its create response uses
// `number` (the iid) + `html_url` (the web URL). Its "PR already exists" status is
// 422, not 409.
const ghBase = {
  repoUrl: "https://github.com/octo/repo",
  pat: PAT,
  sourceBranch: "agent/issue-5",
  targetBranch: "main",
  title: "Fix login",
  description: "Closes #5",
};

describe("GitHubClient.createMergeRequest", () => {
  it("POSTs to api.github.com/repos/{o}/{r}/pulls with a Bearer header and maps number→iid, html_url→webUrl", async () => {
    const { fetchFn, calls } = recorder([{ status: 201, body: { number: 42, html_url: "https://github.com/octo/repo/pull/42" } }]);
    const mr = await new GitHubClient({ fetchFn }).createMergeRequest(ghBase);

    assert.deepStrictEqual(mr, { iid: 42, webUrl: "https://github.com/octo/repo/pull/42" });
    const call = calls[0]!;
    assert.strictEqual(call.method, "POST");
    // API host is api.github.com (subdomain), NOT github.com with an /api path.
    assert.strictEqual(call.url, "https://api.github.com/repos/octo/repo/pulls");
    // PAT rides the Authorization: Bearer header only, never URL/body.
    assert.strictEqual(call.headers["Authorization"], `Bearer ${PAT}`);
    assert.ok(!call.url.includes(PAT), "PAT not in URL");
    assert.ok(!(call.body ?? "").includes(PAT), "PAT not in body");
    // GitHub's create-PR body: head/base/title/body.
    const body = JSON.parse(call.body ?? "{}");
    assert.strictEqual(body.head, "agent/issue-5");
    assert.strictEqual(body.base, "main");
    assert.strictEqual(body.title, "Fix login");
    assert.strictEqual(body.body, "Closes #5");
  });

  it("is idempotent: on 422 (GitHub's duplicate status) it finds and returns the existing open PR", async () => {
    const { fetchFn, calls } = recorder([
      { status: 422, body: { message: "A pull request already exists for octo:agent/issue-5." } },
      { status: 200, body: [{ number: 7, html_url: "https://github.com/octo/repo/pull/7", state: "open" }] },
    ]);
    const mr = await new GitHubClient({ fetchFn }).createMergeRequest(ghBase);
    assert.deepStrictEqual(mr, { iid: 7, webUrl: "https://github.com/octo/repo/pull/7" });
    // Second call is the GET lookup, head as owner:branch (same-repo), still Bearer only.
    const lookup = calls[1]!;
    assert.strictEqual(lookup.method, "GET");
    assert.strictEqual(lookup.headers["Authorization"], `Bearer ${PAT}`);
    assert.match(lookup.url, /\/repos\/octo\/repo\/pulls\?state=open&head=octo%3Aagent%2Fissue-5&base=main$/);
  });

  it("also treats a 409 as duplicate (the shared default stays in the set) and finds the existing PR", async () => {
    const { fetchFn } = recorder([
      { status: 409, body: { message: "conflict" } },
      { status: 200, body: [{ number: 8, html_url: "https://github.com/octo/repo/pull/8", state: "open" }] },
    ]);
    const mr = await new GitHubClient({ fetchFn }).createMergeRequest(ghBase);
    assert.deepStrictEqual(mr, { iid: 8, webUrl: "https://github.com/octo/repo/pull/8" });
  });

  it("tolerates a 422 for some OTHER reason (find-existing finds none) → the create error surfaces, not swallowed", async () => {
    const { fetchFn } = recorder([
      { status: 422, body: { message: "Validation failed: base is invalid" } },
      { status: 200, body: [] },
    ]);
    await assert.rejects(
      new GitHubClient({ fetchFn }).createMergeRequest(ghBase),
      (err: unknown) => err instanceof ForgeError && err.status === 422,
    );
  });

  it("does not resume a closed PR match (state !== open) → the create error surfaces", async () => {
    const { fetchFn } = recorder([
      { status: 422, body: { message: "already exists" } },
      { status: 200, body: [{ number: 3, html_url: "https://github.com/octo/repo/pull/3", state: "closed" }] },
    ]);
    await assert.rejects(
      new GitHubClient({ fetchFn }).createMergeRequest(ghBase),
      (err: unknown) => err instanceof ForgeError && err.status === 422,
    );
  });

  it("pins redirect:error so a 3xx cannot replay the Bearer header cross-origin (N1) — inherited, not re-implemented", async () => {
    const { fetchFn, calls } = recorder([{ status: 201, body: { number: 1, html_url: "https://x/1" } }]);
    await new GitHubClient({ fetchFn }).createMergeRequest(ghBase);
    assert.strictEqual(calls[0]!.redirect, "error");
  });

  it("refuses a non-https base URL and never sends the PAT (N1) — the guard is inherited", async () => {
    const { fetchFn, calls } = recorder([{ status: 201, body: {} }]);
    await assert.rejects(
      new GitHubClient({ fetchFn }).createMergeRequest({ ...ghBase, repoUrl: "http://github.com/octo/repo" }),
      (err: unknown) => err instanceof ForgeError && /non-https/.test(err.message),
    );
    assert.strictEqual(calls.length, 0, "no request made to a non-https base");
  });
});

// PRD #284 M6 (D3): the retry loop wraps the WHOLE createMergeRequest, so a
// transient failure of the findOpenMr GET during a duplicate POST must surface as a
// transient ForgeError (>=500) — not be swallowed to undefined and then reported as
// the POST's permanent 409/422 — so withForgeRetry re-runs a run whose MR exists.
describe("createMergeRequest — transient findOpenMr during a duplicate (PRD #284 D3)", () => {
  it("adopt-existing is preserved: a duplicate POST + 200 GET returns the existing MR", async () => {
    const { fetchFn } = recorder([
      { status: 409, body: { message: ["Another open merge request already exists: !7"] } },
      { status: 200, body: [{ iid: 7, web_url: "https://gitlab.example.com/x/-/merge_requests/7" }] },
    ]);
    const mr = await new GitLabClient({ fetchFn }).createMergeRequest(base);
    assert.deepStrictEqual(mr, { iid: 7, webUrl: "https://gitlab.example.com/x/-/merge_requests/7" });
  });

  it("a 5xx on the findOpenMr GET now THROWS a transient ForgeError (>=500), not the POST's 409", async () => {
    const { fetchFn, calls } = recorder([
      { status: 409, body: { message: "duplicate" } },
      { status: 503, body: "service unavailable" },
    ]);
    await assert.rejects(
      new GitLabClient({ fetchFn }).createMergeRequest(base),
      (err: unknown) => err instanceof ForgeError && err.status === 503,
    );
    // Both the POST and the GET were made; the GET's 5xx surfaced as transient.
    assert.strictEqual(calls.length, 2);
    assert.strictEqual(calls[1]!.method, "GET");
  });

  it("a duplicate POST + a 404 (no existing MR) still fails fast with the POST status", async () => {
    const { fetchFn } = recorder([
      { status: 409, body: { message: "duplicate" } },
      { status: 404, body: { message: "not found" } },
    ]);
    await assert.rejects(
      new GitLabClient({ fetchFn }).createMergeRequest(base),
      (err: unknown) => err instanceof ForgeError && err.status === 409,
    );
  });
});

// PRD #1226 M4 (D5): the forge-neutral PR-head read. Each driver GETs its own single-MR/PR
// endpoint and parses the true source-branch head, returning a validated 40-hex commit id;
// a non-200, a missing head field, or a malformed SHA throws a ForgeError (the caller treats
// a failed head-read as "cannot verify H" — non-terminal). The PAT rides the auth header only.
const HEAD_DIFF_REFS = "1111111111111111111111111111111111111111";
const HEAD_TOP_LEVEL = "2222222222222222222222222222222222222222";
const HEAD_PR = "abcdef0123456789abcdef0123456789abcdef01";

describe("GitLabClient.getMergeRequestHead", () => {
  it("GETs the single-MR endpoint (PAT header only) and PREFERS diff_refs.head_sha over the top-level sha", async () => {
    const { fetchFn, calls } = recorder([
      { status: 200, body: { iid: 42, sha: HEAD_TOP_LEVEL, diff_refs: { head_sha: HEAD_DIFF_REFS } } },
    ]);
    const head = await new GitLabClient({ fetchFn }).getMergeRequestHead(base.repoUrl, PAT, 42);

    assert.strictEqual(head, HEAD_DIFF_REFS);
    const call = calls[0]!;
    assert.strictEqual(call.method, "GET");
    assert.match(call.url, /\/api\/v4\/projects\/group%2Fsub%2Frepo\/merge_requests\/42$/);
    assert.strictEqual(call.headers["PRIVATE-TOKEN"], PAT);
    assert.ok(!call.url.includes(PAT), "PAT not in URL");
  });

  it("falls back to the top-level sha when diff_refs.head_sha is absent", async () => {
    const { fetchFn } = recorder([{ status: 200, body: { iid: 42, sha: HEAD_TOP_LEVEL } }]);
    const head = await new GitLabClient({ fetchFn }).getMergeRequestHead(base.repoUrl, PAT, 42);
    assert.strictEqual(head, HEAD_TOP_LEVEL);
  });

  it("throws a ForgeError when both head fields are absent", async () => {
    const { fetchFn } = recorder([{ status: 200, body: { iid: 42 } }]);
    await assert.rejects(
      new GitLabClient({ fetchFn }).getMergeRequestHead(base.repoUrl, PAT, 42),
      (err: unknown) => err instanceof ForgeError,
    );
  });

  it("throws a ForgeError on a malformed (non-40-hex) sha", async () => {
    const { fetchFn } = recorder([{ status: 200, body: { iid: 42, diff_refs: { head_sha: "not-a-real-sha" } } }]);
    await assert.rejects(
      new GitLabClient({ fetchFn }).getMergeRequestHead(base.repoUrl, PAT, 42),
      (err: unknown) => err instanceof ForgeError,
    );
  });

  it("throws a ForgeError (no PAT in the message) on a non-200", async () => {
    const { fetchFn } = recorder([{ status: 404, body: { message: "404 Not found" } }]);
    await assert.rejects(
      new GitLabClient({ fetchFn }).getMergeRequestHead(base.repoUrl, PAT, 42),
      (err: unknown) => err instanceof ForgeError && err.status === 404 && !err.message.includes(PAT),
    );
  });
});

describe("ForgejoClient.getMergeRequestHead", () => {
  it("GETs the single-PR endpoint (token header only) and parses head.sha", async () => {
    const { fetchFn, calls } = recorder([{ status: 200, body: { number: 42, head: { sha: HEAD_PR } } }]);
    const head = await new ForgejoClient({ fetchFn }).getMergeRequestHead(fjBase.repoUrl, PAT, 42);

    assert.strictEqual(head, HEAD_PR);
    const call = calls[0]!;
    assert.strictEqual(call.method, "GET");
    assert.match(call.url, /\/api\/v1\/repos\/org\/repo\/pulls\/42$/);
    assert.strictEqual(call.headers["Authorization"], `token ${PAT}`);
    assert.ok(!call.url.includes(PAT), "PAT not in URL");
  });

  it("throws a ForgeError when head.sha is missing", async () => {
    const { fetchFn } = recorder([{ status: 200, body: { number: 42, head: {} } }]);
    await assert.rejects(
      new ForgejoClient({ fetchFn }).getMergeRequestHead(fjBase.repoUrl, PAT, 42),
      (err: unknown) => err instanceof ForgeError,
    );
  });

  it("throws a ForgeError on a malformed head.sha", async () => {
    const { fetchFn } = recorder([{ status: 200, body: { number: 42, head: { sha: "deadbeef" } } }]);
    await assert.rejects(
      new ForgejoClient({ fetchFn }).getMergeRequestHead(fjBase.repoUrl, PAT, 42),
      (err: unknown) => err instanceof ForgeError,
    );
  });

  it("throws a ForgeError on a non-200", async () => {
    const { fetchFn } = recorder([{ status: 404, body: { message: "not found" } }]);
    await assert.rejects(
      new ForgejoClient({ fetchFn }).getMergeRequestHead(fjBase.repoUrl, PAT, 42),
      (err: unknown) => err instanceof ForgeError && err.status === 404,
    );
  });
});

describe("GitHubClient.getMergeRequestHead", () => {
  it("GETs api.github.com/repos/{o}/{r}/pulls/{n} (Bearer header only) and parses head.sha", async () => {
    const { fetchFn, calls } = recorder([{ status: 200, body: { number: 42, head: { sha: HEAD_PR } } }]);
    const head = await new GitHubClient({ fetchFn }).getMergeRequestHead(ghBase.repoUrl, PAT, 42);

    assert.strictEqual(head, HEAD_PR);
    const call = calls[0]!;
    assert.strictEqual(call.method, "GET");
    assert.strictEqual(call.url, "https://api.github.com/repos/octo/repo/pulls/42");
    assert.strictEqual(call.headers["Authorization"], `Bearer ${PAT}`);
    assert.ok(!call.url.includes(PAT), "PAT not in URL");
  });

  it("throws a ForgeError when head.sha is missing", async () => {
    const { fetchFn } = recorder([{ status: 200, body: { number: 42, head: {} } }]);
    await assert.rejects(
      new GitHubClient({ fetchFn }).getMergeRequestHead(ghBase.repoUrl, PAT, 42),
      (err: unknown) => err instanceof ForgeError,
    );
  });

  it("throws a ForgeError on a non-200", async () => {
    const { fetchFn } = recorder([{ status: 403, body: { message: "forbidden" } }]);
    await assert.rejects(
      new GitHubClient({ fetchFn }).getMergeRequestHead(ghBase.repoUrl, PAT, 42),
      (err: unknown) => err instanceof ForgeError && err.status === 403,
    );
  });
});

// PRD #1225 (CodeRabbit !1254): the completion interlock reconciles the MR/PR body (drops the
// `Closes #N` line on an unverified-head hold, re-asserts it on a verified completion) via
// updateMergeRequestDescription. Each driver must hit the SAME single-item resource as
// getMergeRequestHead with the FORGE-CORRECT method + body field: GitLab PUT + `description`,
// Forgejo/GitHub PATCH + `body`. This pins the per-driver updateMethod()/updateBody() split so a
// flip on the non-GitLab drivers (which the runner-level interlock test only covers for GitLab)
// is caught here. The PAT rides the auth header only.
const RECONCILED_BODY = "Related to #5.\n\n---\nunverified";

describe("GitLabClient.updateMergeRequestDescription", () => {
  it("PUTs the single-MR endpoint with a `description` field (PAT header only)", async () => {
    const { fetchFn, calls } = recorder([{ status: 200, body: { iid: 42 } }]);
    await new GitLabClient({ fetchFn }).updateMergeRequestDescription(base.repoUrl, PAT, 42, RECONCILED_BODY);

    const call = calls[0]!;
    assert.strictEqual(call.method, "PUT");
    assert.match(call.url, /\/api\/v4\/projects\/group%2Fsub%2Frepo\/merge_requests\/42$/);
    assert.strictEqual(call.headers["PRIVATE-TOKEN"], PAT);
    assert.ok(!call.url.includes(PAT), "PAT not in URL");
    const body = JSON.parse(call.body ?? "{}") as Record<string, unknown>;
    assert.strictEqual(body.description, RECONCILED_BODY);
    assert.ok(!("body" in body), "GitLab uses `description`, not `body`");
  });

  it("accepts a 2xx and throws a ForgeError on a non-2xx (no PAT in the message)", async () => {
    const ok = recorder([{ status: 200, body: {} }]);
    await new GitLabClient({ fetchFn: ok.fetchFn }).updateMergeRequestDescription(base.repoUrl, PAT, 42, RECONCILED_BODY);
    const bad = recorder([{ status: 404, body: { message: "404 Not found" } }]);
    await assert.rejects(
      new GitLabClient({ fetchFn: bad.fetchFn }).updateMergeRequestDescription(base.repoUrl, PAT, 42, RECONCILED_BODY),
      (err: unknown) => err instanceof ForgeError && err.status === 404 && !err.message.includes(PAT),
    );
  });
});

describe("ForgejoClient.updateMergeRequestDescription", () => {
  it("PATCHes the single-PR endpoint with a `body` field (token header only); accepts 201", async () => {
    const { fetchFn, calls } = recorder([{ status: 201, body: { number: 42 } }]);
    await new ForgejoClient({ fetchFn }).updateMergeRequestDescription(fjBase.repoUrl, PAT, 42, RECONCILED_BODY);

    const call = calls[0]!;
    assert.strictEqual(call.method, "PATCH");
    assert.match(call.url, /\/api\/v1\/repos\/org\/repo\/pulls\/42$/);
    assert.strictEqual(call.headers["Authorization"], `token ${PAT}`);
    assert.ok(!call.url.includes(PAT), "PAT not in URL");
    const body = JSON.parse(call.body ?? "{}") as Record<string, unknown>;
    assert.strictEqual(body.body, RECONCILED_BODY);
    assert.ok(!("description" in body), "Forgejo uses `body`, not `description`");
  });

  it("throws a ForgeError on a non-2xx", async () => {
    const { fetchFn } = recorder([{ status: 404, body: { message: "not found" } }]);
    await assert.rejects(
      new ForgejoClient({ fetchFn }).updateMergeRequestDescription(fjBase.repoUrl, PAT, 42, RECONCILED_BODY),
      (err: unknown) => err instanceof ForgeError && err.status === 404,
    );
  });
});

describe("GitHubClient.updateMergeRequestDescription", () => {
  it("PATCHes api.github.com/repos/{o}/{r}/pulls/{n} with a `body` field (Bearer header only)", async () => {
    const { fetchFn, calls } = recorder([{ status: 200, body: { number: 42 } }]);
    await new GitHubClient({ fetchFn }).updateMergeRequestDescription(ghBase.repoUrl, PAT, 42, RECONCILED_BODY);

    const call = calls[0]!;
    assert.strictEqual(call.method, "PATCH");
    assert.strictEqual(call.url, "https://api.github.com/repos/octo/repo/pulls/42");
    assert.strictEqual(call.headers["Authorization"], `Bearer ${PAT}`);
    assert.ok(!call.url.includes(PAT), "PAT not in URL");
    const body = JSON.parse(call.body ?? "{}") as Record<string, unknown>;
    assert.strictEqual(body.body, RECONCILED_BODY);
    assert.ok(!("description" in body), "GitHub uses `body`, not `description`");
  });

  it("throws a ForgeError on a non-2xx", async () => {
    const { fetchFn } = recorder([{ status: 403, body: { message: "forbidden" } }]);
    await assert.rejects(
      new GitHubClient({ fetchFn }).updateMergeRequestDescription(ghBase.repoUrl, PAT, 42, RECONCILED_BODY),
      (err: unknown) => err instanceof ForgeError && err.status === 403,
    );
  });
});

// PRD #1798 M3 (D11): the forge-neutral single-MR/PR read. Each driver GETs the SAME
// single-item resource as getMergeRequestHead and normalises head SHA, target branch,
// description and state. The payloads below are recorded-shape responses from each forge's
// single-item endpoint, trimmed to the fields the parser reads plus a few neighbours so a
// parser keyed on the wrong field fails. The PAT rides the auth header only.
const MR_HEAD = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b";
const MR_BODY = "## Summary\n\nFixes the login redirect.\n\nCloses #5";

/** GitLab `GET /projects/:id/merge_requests/:iid` (trimmed). */
function gitlabMrFixture(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 90210,
    iid: 42,
    project_id: 311,
    title: "Fix login redirect",
    description: MR_BODY,
    state: "opened",
    merged_at: null,
    closed_at: null,
    target_branch: "release/2.x",
    source_branch: "agent/issue-5",
    draft: false,
    sha: MR_HEAD,
    merge_commit_sha: null,
    diff_refs: {
      base_sha: "0000000000000000000000000000000000000abc",
      head_sha: MR_HEAD,
      start_sha: "0000000000000000000000000000000000000abc",
    },
    web_url: "https://gitlab.example.com/group/sub/repo/-/merge_requests/42",
    ...over,
  };
}

/** GitHub `GET /repos/{owner}/{repo}/pulls/{n}` (trimmed). */
function githubPrFixture(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    url: "https://api.github.com/repos/octo/repo/pulls/42",
    number: 42,
    state: "open",
    locked: false,
    title: "Fix login redirect",
    body: MR_BODY,
    created_at: "2026-09-20T10:00:00Z",
    closed_at: null,
    merged_at: null,
    merge_commit_sha: null,
    draft: false,
    head: { label: "octo:agent/issue-5", ref: "agent/issue-5", sha: MR_HEAD },
    base: { label: "octo:release/2.x", ref: "release/2.x", sha: "0000000000000000000000000000000000000abc" },
    merged: false,
    mergeable: true,
    html_url: "https://github.com/octo/repo/pull/42",
    ...over,
  };
}

/** Forgejo `GET /api/v1/repos/{owner}/{repo}/pulls/{index}` (trimmed). */
function forgejoPrFixture(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 7001,
    number: 42,
    title: "Fix login redirect",
    body: MR_BODY,
    state: "open",
    is_locked: false,
    draft: false,
    mergeable: true,
    merged: false,
    merged_at: null,
    merge_commit_sha: null,
    // label differs from ref so a parser reading `label` instead of `ref` would fail.
    base: { label: "org:release/2.x", ref: "release/2.x", sha: "0000000000000000000000000000000000000abc", repo_id: 3 },
    head: { label: "agent/issue-5", ref: "agent/issue-5", sha: MR_HEAD, repo_id: 3 },
    closed_at: null,
    html_url: "https://example.com/git/org/repo/pulls/42",
    ...over,
  };
}

const fjSubpathRepo = "https://example.com/git/org/repo";

type Driver = {
  name: string;
  make: (fetchFn: FetchFn) => GitLabClient | ForgejoClient | GitHubClient;
  repoUrl: string;
  url: string;
  authHeader: string;
  authValue: string;
  fixture: (over?: Record<string, unknown>) => Record<string, unknown>;
  bodyField: string;
  withoutTarget: Record<string, unknown>;
  withSha: (sha: unknown) => Record<string, unknown>;
};

const drivers: Driver[] = [
  {
    name: "GitLabClient",
    make: (fetchFn) => new GitLabClient({ fetchFn }),
    repoUrl: base.repoUrl,
    url: "https://gitlab.example.com/api/v4/projects/group%2Fsub%2Frepo/merge_requests/42",
    authHeader: "PRIVATE-TOKEN",
    authValue: PAT,
    fixture: gitlabMrFixture,
    bodyField: "description",
    withoutTarget: { target_branch: undefined },
    withSha: (sha) => ({ sha, diff_refs: { head_sha: sha } }),
  },
  {
    name: "GitHubClient",
    make: (fetchFn) => new GitHubClient({ fetchFn }),
    repoUrl: ghBase.repoUrl,
    url: "https://api.github.com/repos/octo/repo/pulls/42",
    authHeader: "Authorization",
    authValue: `Bearer ${PAT}`,
    fixture: githubPrFixture,
    bodyField: "body",
    withoutTarget: { base: { label: "octo:x", sha: MR_HEAD } },
    withSha: (sha) => ({ head: { ref: "agent/issue-5", sha } }),
  },
  {
    name: "ForgejoClient",
    make: (fetchFn) => new ForgejoClient({ fetchFn }),
    repoUrl: fjSubpathRepo,
    url: "https://example.com/git/api/v1/repos/org/repo/pulls/42",
    authHeader: "Authorization",
    authValue: `token ${PAT}`,
    fixture: forgejoPrFixture,
    bodyField: "body",
    withoutTarget: { base: { label: "x", sha: MR_HEAD } },
    withSha: (sha) => ({ head: { ref: "agent/issue-5", sha } }),
  },
];

for (const d of drivers) {
  describe(`${d.name}.getMergeRequest`, () => {
    it("GETs the single-item URL with the PAT in the auth header only, redirect:error, and parses an open MR/PR", async () => {
      const { fetchFn, calls } = recorder([{ status: 200, body: d.fixture() }]);
      const mr = await d.make(fetchFn).getMergeRequest(d.repoUrl, PAT, 42);

      assert.deepStrictEqual(mr, { headSha: MR_HEAD, targetBranch: "release/2.x", description: MR_BODY, state: "open" });
      assert.strictEqual(calls.length, 1);
      const call = calls[0]!;
      assert.strictEqual(call.method, "GET");
      assert.strictEqual(call.url, d.url);
      assert.strictEqual(call.redirect, "error");
      assert.strictEqual(call.body, undefined);
      assert.strictEqual(call.headers[d.authHeader], d.authValue);
      assert.ok(!call.url.includes(PAT), "PAT not in URL");
      for (const [k, v] of Object.entries(call.headers)) {
        if (k !== d.authHeader) assert.ok(!v.includes(PAT), `PAT only in ${d.authHeader}, found in ${k}`);
      }
    });

    it("maps a null description/body to the empty string", async () => {
      const { fetchFn } = recorder([{ status: 200, body: d.fixture({ [d.bodyField]: null }) }]);
      const mr = await d.make(fetchFn).getMergeRequest(d.repoUrl, PAT, 42);
      assert.strictEqual(mr.description, "");
    });

    it("rejects a non-string description/body with the parser's ForgeError (not coerced)", async () => {
      const { fetchFn } = recorder([{ status: 200, body: d.fixture({ [d.bodyField]: 42 }) }]);
      await assert.rejects(
        d.make(fetchFn).getMergeRequest(d.repoUrl, PAT, 42),
        (err: unknown) => err instanceof ForgeError && /non-string description/.test(err.message),
      );
    });

    it("throws ForgeError(404) for a missing MR/PR", async () => {
      const { fetchFn } = recorder([{ status: 404, body: { message: "404 Not Found" } }]);
      await assert.rejects(
        d.make(fetchFn).getMergeRequest(d.repoUrl, PAT, 42),
        (err: unknown) => err instanceof ForgeError && err.status === 404 && !err.message.includes(PAT),
      );
    });

    it("surfaces a transient 5xx as a ForgeError carrying the status", async () => {
      const { fetchFn, calls } = recorder([{ status: 503, body: "upstream unavailable" }]);
      await assert.rejects(
        d.make(fetchFn).getMergeRequest(d.repoUrl, PAT, 42),
        (err: unknown) => err instanceof ForgeError && err.status === 503,
      );
      assert.strictEqual(calls.length, 1, "the client does not retry by itself; forge-retry owns that");
    });

    it("throws a ForgeError on a malformed head sha", async () => {
      const { fetchFn } = recorder([{ status: 200, body: d.fixture(d.withSha("deadbeef")) }]);
      await assert.rejects(d.make(fetchFn).getMergeRequest(d.repoUrl, PAT, 42), (err: unknown) => err instanceof ForgeError);
    });

    it("throws a ForgeError when the target branch is missing", async () => {
      const { fetchFn } = recorder([{ status: 200, body: d.fixture(d.withoutTarget) }]);
      await assert.rejects(
        d.make(fetchFn).getMergeRequest(d.repoUrl, PAT, 42),
        (err: unknown) => err instanceof ForgeError && /target branch/.test(err.message),
      );
    });

    it("throws a ForgeError on an unknown state", async () => {
      const { fetchFn } = recorder([{ status: 200, body: d.fixture({ state: "reopened-ish" }) }]);
      await assert.rejects(
        d.make(fetchFn).getMergeRequest(d.repoUrl, PAT, 42),
        (err: unknown) => err instanceof ForgeError && /unknown state/.test(err.message),
      );
    });

    it("throws a ForgeError on a malformed JSON body", async () => {
      const { fetchFn } = recorder([{ status: 200, body: "<html>not json</html>" }]);
      await assert.rejects(d.make(fetchFn).getMergeRequest(d.repoUrl, PAT, 42), (err: unknown) => err instanceof ForgeError);
    });

    it("refuses a non-https repo URL before sending the PAT", async () => {
      const { fetchFn, calls } = recorder([{ status: 200, body: d.fixture() }]);
      await assert.rejects(
        d.make(fetchFn).getMergeRequest(d.repoUrl.replace("https://", "http://"), PAT, 42),
        (err: unknown) => err instanceof ForgeError && err.status === 0,
      );
      assert.strictEqual(calls.length, 0);
    });
  });
}

// PRD #1798 M6 (H1): the detail read is byte-capped. A real fetch Response streams its `body`; the
// read stops at the cap and cancels the stream, so a hostile 64 MiB answer is never buffered.
describe("getMergeRequest — the byte-capped detail read (H1)", () => {
  const MiB = 1_048_576;
  /** A 64 MiB body served lazily in 1 MiB chunks, counting what the reader actually pulled. */
  function huge(): { body: ReadableStream<Uint8Array>; pulled: () => number; cancelled: () => boolean } {
    let pulled = 0;
    let cancelled = false;
    const chunk = new Uint8Array(MiB).fill(0x61);
    const body = new ReadableStream<Uint8Array>({
      pull(ctl) {
        if (pulled >= 64 * MiB) return ctl.close();
        pulled += chunk.byteLength;
        ctl.enqueue(chunk);
      },
      cancel() {
        cancelled = true;
      },
    });
    return { body, pulled: () => pulled, cancelled: () => cancelled };
  }

  it("a 64 MiB streamed answer is refused with ForgeResponseTooLarge after reading about the cap, and the stream is cancelled", async () => {
    const h = huge();
    let textCalled = false;
    const fetchFn: FetchFn = async () => ({
      status: 200,
      body: h.body,
      text: async () => {
        textCalled = true;
        return "";
      },
    });
    await assert.rejects(new GitLabClient({ fetchFn }).getMergeRequest(base.repoUrl, PAT, 42), (err: unknown) => {
      assert.ok(err instanceof ForgeResponseTooLarge, String(err));
      assert.ok(err instanceof ForgeError);
      assert.strictEqual(err.status, 0);
      assert.match(err.message, /response too large/);
      return true;
    });
    assert.strictEqual(textCalled, false, "the whole body is never buffered through text()");
    assert.ok(h.pulled() <= MR_DETAIL_MAX_BYTES + 2 * MiB, `pulled ${h.pulled()} bytes`);
    assert.ok(h.cancelled(), "the rest of the stream is cancelled");
    assert.strictEqual(MR_DETAIL_MAX_BYTES, 4 * MiB + 64 * 1024);
  });

  it("a streamed answer under the cap parses exactly as text() does (a real Response)", async () => {
    const fixture = { sha: MR_HEAD, target_branch: "release/2.x", description: "Ünïcödé 🎉 body", state: "opened" };
    const fetchFn: FetchFn = async () => new Response(JSON.stringify(fixture), { status: 200 });
    const mr = await new GitLabClient({ fetchFn }).getMergeRequest(base.repoUrl, PAT, 42);
    assert.deepStrictEqual(mr, { headSha: MR_HEAD, targetBranch: "release/2.x", description: "Ünïcödé 🎉 body", state: "open" });
  });

  it("a transport without a streaming body (text() only) is still refused over the cap", async () => {
    const big = JSON.stringify({ sha: MR_HEAD, target_branch: "main", description: "x".repeat(MR_DETAIL_MAX_BYTES), state: "opened" });
    const fetchFn: FetchFn = async () => ({ status: 200, text: async () => big });
    await assert.rejects(new GitLabClient({ fetchFn }).getMergeRequest(base.repoUrl, PAT, 42), (err: unknown) => err instanceof ForgeResponseTooLarge);
  });

  it("an error answer's body is read only to a small cap before the message is cut", async () => {
    for (const status of [404, 503]) {
      const h = huge();
      const fetchFn: FetchFn = async () => ({ status, body: h.body, text: async () => "" });
      await assert.rejects(new GitLabClient({ fetchFn }).getMergeRequest(base.repoUrl, PAT, 42), (err: unknown) => {
        assert.ok(err instanceof ForgeError && !(err instanceof ForgeResponseTooLarge), String(err));
        assert.strictEqual(err.status, status);
        assert.ok(err.detail.length <= 512);
        return true;
      });
      assert.ok(h.pulled() <= 2 * MiB, `pulled ${h.pulled()} bytes for a ${status}`);
      assert.ok(h.cancelled());
    }
  });
});

describe("getMergeRequest state mapping (recorded closed/merged/locked shapes)", () => {
  const read = async (client: (f: FetchFn) => GitLabClient | ForgejoClient | GitHubClient, repoUrl: string, payload: unknown) => {
    const { fetchFn } = recorder([{ status: 200, body: payload }]);
    return (await client(fetchFn).getMergeRequest(repoUrl, PAT, 42)).state;
  };
  const gl = (f: FetchFn) => new GitLabClient({ fetchFn: f });
  const gh = (f: FetchFn) => new GitHubClient({ fetchFn: f });
  const fj = (f: FetchFn) => new ForgejoClient({ fetchFn: f });
  const MERGED_AT = "2026-09-21T12:00:00Z";

  it("GitLab: opened/closed/merged/locked", async () => {
    assert.strictEqual(await read(gl, base.repoUrl, gitlabMrFixture({ state: "opened" })), "open");
    assert.strictEqual(await read(gl, base.repoUrl, gitlabMrFixture({ state: "closed", closed_at: MERGED_AT })), "closed");
    assert.strictEqual(
      await read(gl, base.repoUrl, gitlabMrFixture({ state: "merged", merged_at: MERGED_AT, merge_commit_sha: MR_HEAD })),
      "merged",
    );
    assert.strictEqual(await read(gl, base.repoUrl, gitlabMrFixture({ state: "locked" })), "locked");
  });

  it("GitHub: open, closed-unmerged, closed+merged (flag), closed+merged_at only, closed+merged flag only", async () => {
    assert.strictEqual(await read(gh, ghBase.repoUrl, githubPrFixture()), "open");
    assert.strictEqual(await read(gh, ghBase.repoUrl, githubPrFixture({ state: "closed", closed_at: MERGED_AT })), "closed");
    assert.strictEqual(
      await read(gh, ghBase.repoUrl, githubPrFixture({ state: "closed", closed_at: MERGED_AT, merged_at: MERGED_AT, merged: true })),
      "merged",
    );
    // The list endpoint omits `merged`; merged_at alone still reads as merged.
    const { merged: _drop, ...noFlag } = githubPrFixture({ state: "closed", merged_at: MERGED_AT });
    assert.strictEqual(await read(gh, ghBase.repoUrl, noFlag), "merged");
    // The flag alone (merged_at null) still reads as merged.
    assert.strictEqual(
      await read(gh, ghBase.repoUrl, githubPrFixture({ state: "closed", merged: true, merged_at: null })),
      "merged",
    );
  });

  it("Forgejo: open, closed-unmerged, closed+merged, closed+merged flag only", async () => {
    assert.strictEqual(await read(fj, fjSubpathRepo, forgejoPrFixture()), "open");
    assert.strictEqual(await read(fj, fjSubpathRepo, forgejoPrFixture({ state: "closed", closed_at: MERGED_AT })), "closed");
    assert.strictEqual(
      await read(fj, fjSubpathRepo, forgejoPrFixture({ state: "closed", merged: true, merged_at: MERGED_AT })),
      "merged",
    );
    // The flag alone (merged_at null) still reads as merged.
    assert.strictEqual(
      await read(fj, fjSubpathRepo, forgejoPrFixture({ state: "closed", merged: true, merged_at: null })),
      "merged",
    );
  });

  it("GitHub/Forgejo have no locked state: a GitLab-only value is rejected", async () => {
    await assert.rejects(read(gh, ghBase.repoUrl, githubPrFixture({ state: "locked" })), (e: unknown) => e instanceof ForgeError);
    await assert.rejects(read(fj, fjSubpathRepo, forgejoPrFixture({ state: "merged" })), (e: unknown) => e instanceof ForgeError);
  });
});

describe("forgeClientFor", () => {
  it("selects GitLab for absent/gitlab, Forgejo for forgejo, GitHub for github", () => {
    assert.ok(forgeClientFor(undefined) instanceof GitLabClient);
    assert.ok(forgeClientFor("gitlab") instanceof GitLabClient);
    assert.ok(forgeClientFor("forgejo") instanceof ForgejoClient);
    assert.ok(forgeClientFor("github") instanceof GitHubClient);
  });
});

describe("URL helpers", () => {
  it("derives the GitLab base URL and namespaced project path (subgroups kept)", () => {
    assert.strictEqual(gitlabBaseUrl("https://gitlab.example.com/group/sub/repo"), "https://gitlab.example.com");
    assert.strictEqual(gitlabProjectPath("https://gitlab.example.com/group/sub/repo"), "group/sub/repo");
    assert.strictEqual(gitlabProjectPath("https://gitlab.example.com/org/repo.git"), "org/repo");
  });

  it("splits a Forgejo repo URL into apiBase + owner + repo, preserving a subpath (D9)", () => {
    assert.deepStrictEqual(forgejoRepoParts("https://forgejo.example.com/org/repo"), {
      apiBase: "https://forgejo.example.com",
      owner: "org",
      repo: "repo",
    });
    assert.deepStrictEqual(forgejoRepoParts("https://example.com/git/org/repo.git"), {
      apiBase: "https://example.com/git",
      owner: "org",
      repo: "repo",
    });
  });

  it("rejects a Forgejo URL that has no owner/repo", () => {
    assert.throws(() => forgejoRepoParts("https://forgejo.example.com/only-one"), (e: unknown) => e instanceof ForgeError);
  });

  it("maps a github.com web URL to the api.github.com base (subdomain, not path) + owner/repo", () => {
    assert.deepStrictEqual(githubRepoParts("https://github.com/octo/repo"), {
      apiBase: "https://api.github.com",
      owner: "octo",
      repo: "repo",
    });
    assert.deepStrictEqual(githubRepoParts("https://github.com/octo/repo.git"), {
      apiBase: "https://api.github.com",
      owner: "octo",
      repo: "repo",
    });
  });

  it("rejects a GitHub URL that has no owner/repo", () => {
    assert.throws(() => githubRepoParts("https://github.com/only-one"), (e: unknown) => e instanceof ForgeError);
  });
});
