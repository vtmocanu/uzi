import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { GitLabClient, GitLabError, gitlabBaseUrl, gitlabProjectPath, type FetchFn } from "../src/gitlab.js";

// The MR path is exercised up to — never across — the network boundary via an
// injected fake transport (testing-credentials policy). The PAT rides the
// PRIVATE-TOKEN header only, never the URL or body.

interface Call {
  url: string;
  method: string;
  headers: Record<string, string>;
  body?: string;
}

function recorder(responses: Array<{ status: number; body: unknown }>): { fetchFn: FetchFn; calls: Call[] } {
  const calls: Call[] = [];
  let i = 0;
  const fetchFn: FetchFn = async (url, init) => {
    calls.push({ url, method: init.method, headers: init.headers, body: init.body });
    const r = responses[Math.min(i, responses.length - 1)]!;
    i++;
    return { status: r.status, text: async () => (typeof r.body === "string" ? r.body : JSON.stringify(r.body)) };
  };
  return { fetchFn, calls };
}

const PAT = "glpat-fixture-do-not-scan";
const base = {
  baseUrl: "https://gitlab.example.com",
  projectPath: "group/sub/repo",
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

  it("throws a GitLabError (no PAT in the message) on an unexpected status", async () => {
    const { fetchFn } = recorder([{ status: 403, body: { message: "insufficient scope" } }]);
    await assert.rejects(
      new GitLabClient({ fetchFn }).createMergeRequest(base),
      (err: unknown) => err instanceof GitLabError && err.status === 403 && !err.message.includes(PAT),
    );
  });
});

describe("GitLab URL helpers", () => {
  it("derives the base URL and namespaced project path", () => {
    assert.strictEqual(gitlabBaseUrl("https://gitlab.example.com/group/sub/repo"), "https://gitlab.example.com");
    assert.strictEqual(gitlabProjectPath("https://gitlab.example.com/group/sub/repo"), "group/sub/repo");
    assert.strictEqual(gitlabProjectPath("https://gitlab.example.com/org/repo.git"), "org/repo");
  });
});
