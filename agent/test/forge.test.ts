import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  GitLabClient,
  ForgejoClient,
  GitHubClient,
  ForgeError,
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
}

function recorder(responses: Array<{ status: number; body: unknown }>): { fetchFn: FetchFn; calls: Call[] } {
  const calls: Call[] = [];
  let i = 0;
  const fetchFn: FetchFn = async (url, init) => {
    calls.push({ url, method: init.method, headers: init.headers, body: init.body, redirect: init.redirect });
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
