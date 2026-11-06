import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { classifyGitStderr, classifyGitStderrText, GitCache } from "../src/git.js";
import { nullLogger } from "./helpers.js";

// #3929: GitHub's bare `! [remote rejected] <sha> -> <branch> (failed)` is the only remote-rejected
// shape classified transient; everything that states a reason, and any ambiguity, stays permanent.

const SHA = "0123456789abcdef0123456789abcdef01234567";
const URL = "https://github.com/org/repo.git";
const status = (branch: string, reason = "failed") => ` ! [remote rejected] ${SHA} -> ${branch} (${reason})\n`;
const trailer = (url = URL) => `error: failed to push some refs to '${url}'\n`;
const full = (branch: string, reason = "failed", remote = "", url = URL) =>
  `To ${url}\n${status(branch, reason)}${remote}${trailer(url)}`;

describe("classifyGitStderrText", () => {
  const transient: Array<[string, string]> = [
    ["bare (failed)", full("agent/issue-7")],
    ["destination branch agent/issue-404", full("agent/issue-404")],
    ["URL containing repo-404 in To and trailer", full("agent/issue-7", "failed", "", "https://github.com/org/repo-404.git")],
    ["CR-separated lines", full("agent/issue-7").replace(/\n/g, "\r")],
    ["CRLF-separated lines", full("agent/issue-7").replace(/\n/g, "\r\n")],
    ["an empty remote: line", full("agent/issue-7", "failed", "remote: \n")],
  ];
  for (const [name, text] of transient) {
    it(`transient: ${name}`, () => assert.equal(classifyGitStderrText(text), "transient"));
  }

  const permanent: Array<[string, string]> = [
    ["protected branch hook declined", full("agent/issue-7", "protected branch hook declined")],
    ["pre-receive hook declined", full("agent/issue-7", "pre-receive hook declined")],
    ["workflow-scope reason", full("agent/issue-7", "refusing to allow a Personal Access Token to create or update workflow `.github/workflows/ci.yml` without `workflow` scope")],
    ["repository rule violations with GH013", full("agent/issue-7", "push declined due to repository rule violations",
      "remote: error: GH013: Repository rule violations found for refs/heads/agent/issue-7.\nremote: \n")],
    ["failed to update ref", full("agent/issue-7", "failed to update ref")],
    ["bare (failed) plus GH006", full("agent/issue-7", "failed", "remote: error: GH006: Protected branch update failed for refs/heads/agent/issue-7.\n")],
    ["bare (failed) plus GH001", full("agent/issue-7", "failed", "remote: error: GH001: Large files detected.\n")],
    // Each case below hits no permanent pattern: only the framing-only rule keeps it permanent.
    ["bare (failed) plus an unknown GH code", full("agent/issue-7", "failed", "remote: GH999: odd\n")],
    ["bare (failed) plus remote: error:", full("agent/issue-7", "failed", "remote: error: weird thing\n")],
    ["bare (failed) plus remote: fatal:", full("agent/issue-7", "failed", "remote: fatal: weird thing\n")],
    ["bare (failed) plus a push protection notice", full("agent/issue-7", "failed", "remote: push protection notice\n")],
    ["bare (failed) plus a workflow scope notice", full("agent/issue-7", "failed", "remote: update refused without workflow scope\n")],
    // GitHub's own workflow-scope wording backticks the scope name, so the literal "workflow scope"
    // marker misses it; a separate remote: line beside a bare (failed) must stay permanent.
    ["bare (failed) plus GitHub's backticked workflow-scope refusal line", full("agent/issue-7", "failed",
      "remote: refusing to allow a Personal Access Token to create or update workflow `.github/workflows/ci.yml` without `workflow` scope\n")],
    ["bare (failed) plus a refusing-to-allow line", full("agent/issue-7", "failed", "remote: refusing to allow this update\n")],
    ["bare (failed) plus an unmarked remote: policy line", full("agent/issue-7", "failed", "remote: Push blocked by repository policy\n")],
    ["bare (failed) plus a non-remote line", full("agent/issue-7", "failed", "hint: something odd\n")],
    ["one (failed) and one hook-declined status", `To ${URL}\n${status("a")}${status("b", "pre-receive hook declined")}${trailer()}`],
    ["[rejected] fetch first", `To ${URL}\n ! [rejected]        main -> main (fetch first)\n${trailer()}`],
    ["empty stderr", ""],
    ["whitespace stderr", "  \n"],
    ["non-push stderr", "fatal: the remote end hung up on policy grounds"],
    ["authentication failed trailer", `error: failed to push some refs to '${URL}': authentication failed`],
    ["permission denied To line", `To ${URL} permission denied`],
    // With a bare (failed) status, so an over-broad (prefix) framing removal would turn these transient.
    ["bare (failed) plus a trailer carrying authentication failed", `To ${URL}\n${status("agent/issue-7")}error: failed to push some refs to '${URL}': authentication failed\n`],
    ["bare (failed) plus a To line carrying permission denied", `To ${URL} permission denied\n${status("agent/issue-7")}${trailer()}`],
    ["PR hint carrying 403", full("agent/test", "failed", "remote:     https://github.com/org/403/pull/new/agent/test\n")],
    ["PR hint carrying a GH013 branch", full("GH013", "failed", "remote: Create a pull request for 'GH013' on GitHub by visiting:\n")],
    ["PR hint carrying agent/issue-404", full("agent/issue-404", "failed", "remote:      https://github.com/org/repo/pull/new/agent/issue-404\n")],
  ];
  for (const [name, text] of permanent) {
    it(`permanent: ${name}`, () => assert.equal(classifyGitStderrText(text), "permanent"));
  }
});

describe("classifyGitStderr through a real runGit failure", () => {
  async function classifyOf(stderr: string): Promise<"transient" | "permanent"> {
    const cache = new GitCache("/tmp/git-stderr-classify-unused", nullLogger());
    const seam = cache as unknown as { execScoped: (...a: unknown[]) => Promise<unknown>; runGit(cwd: string, args: string[]): Promise<string> };
    seam.execScoped = async (_cmd, args) => {
      throw Object.assign(new Error(`Command failed: git ${(args as string[]).join(" ")}`), { code: 128, stderr });
    };
    try {
      await seam.runGit("/tmp", ["push", "origin", `${SHA}:refs/heads/agent/issue-404`]);
    } catch (cause) {
      return classifyGitStderr(cause);
    }
    throw new Error("runGit did not fail");
  }

  it("retained bare (failed) stderr is transient even when the argv carries agent/issue-404", async () => {
    assert.equal(await classifyOf(full("agent/issue-404")), "transient");
  });

  it("retained stderr with a stated reason stays permanent", async () => {
    assert.equal(await classifyOf(full("agent/issue-404", "pre-receive hook declined")), "permanent");
  });
});
