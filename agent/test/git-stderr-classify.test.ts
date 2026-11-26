import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { classifyGitStderr, classifyGitStderrText, GitCache } from "../src/git.js";
import { nullLogger } from "./helpers.js";

// #3929, #4438: GitHub's bare `! [remote rejected] <sha> -> <branch> (failed)` and its
// `(missing necessary objects)` sibling are the only remote-rejected shapes classified transient;
// everything that states any other reason, and any ambiguity, stays permanent.

const SHA = "0123456789abcdef0123456789abcdef01234567";
const URL = "https://github.com/org/repo.git";
const MNO = "missing necessary objects";
// The exact stderr observed in #4438 (leading space and five spaces after `]` are load-bearing).
const OBSERVED_MNO_STDERR =
  "To https://github.com/vtmocanu/uzi.git\n" +
  " ! [remote rejected]     8e69f19016fa9f10cd8ba8ced7f7afc6d8cccd27 -> agent/issue-4395 (missing necessary objects)\n" +
  "error: failed to push some refs to 'https://github.com/vtmocanu/uzi.git'\n";
// #4716: the exact stderr observed from GitHub (request id synthetic, same shape). Padding as stored:
// 8 trailing spaces on the banner and Request ID lines, none on the Time line, 5 spaces after `]`.
const ISE_B1 = "remote: Internal Server Error        \n";
const ISE_B2 = "remote: Request ID 0000:1111:2222222:3333333:44444444        \n";
const ISE_B3 = "remote: Time 2026-11-25T16:15:24Z\n";
const ISE_TO = "To https://github.com/vtmocanu/uzi.git\n";
const ISE_STATUS_LINE = " ! [remote rejected]     169cad138411a3b8a728c858f322d44fec6b7825 -> agent/issue-4416 (Internal Server Error)\n";
const ISE_TRAILER = "error: failed to push some refs to 'https://github.com/vtmocanu/uzi.git'\n";
const OBSERVED_ISE_STDERR = ISE_B1 + ISE_B2 + ISE_B3 + ISE_TO + ISE_STATUS_LINE + ISE_TRAILER;
/** The observed shape with its banner and status lines replaced; `extra` lands before the trailer. */
const ise = (o: { b1?: string; b2?: string; b3?: string; statusLine?: string; extra?: string } = {}) =>
  (o.b1 ?? ISE_B1) + (o.b2 ?? ISE_B2) + (o.b3 ?? ISE_B3) + ISE_TO + (o.statusLine ?? ISE_STATUS_LINE) + (o.extra ?? "") + ISE_TRAILER;
const iseStatus = (reason: string) => ` ! [remote rejected]     ${SHA} -> agent/issue-4416 (${reason})\n`;
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
    ["observed #4438 stderr (missing necessary objects)", OBSERVED_MNO_STDERR],
    ["missing necessary objects", full("agent/issue-7", MNO)],
    ["missing necessary objects, destination agent/issue-404", full("agent/issue-404", MNO)],
    ["missing necessary objects, CR-separated", full("agent/issue-7", MNO).replace(/\n/g, "\r")],
    ["missing necessary objects, CRLF-separated", full("agent/issue-7", MNO).replace(/\n/g, "\r\n")],
    ["missing necessary objects with an empty remote: line", full("agent/issue-7", MNO, "remote: \n")],
    ["both retryable reasons in one push", `To ${URL}\n${status("a")}${status("b", MNO)}${trailer()}`],
    // #4716: GitHub's Internal Server Error refusal with its complete banner.
    ["observed #4716 stderr (Internal Server Error with its banner)", OBSERVED_ISE_STDERR],
    ["observed #4716 stderr, CRLF-separated", OBSERVED_ISE_STDERR.replace(/\n/g, "\r\n")],
    ["observed #4716 stderr with an empty remote: line", ise({ extra: "remote: \n" })],
    ["observed #4716 stderr with a tab-padded banner", ise({ b1: "remote: Internal Server Error\t\n" })],
    // Controls: ordinary transient wording still reaches the generic fallback.
    ["could not resolve host", "fatal: unable to access 'https://github.com/org/repo.git/': Could not resolve host: github.com"],
    ["numeric 503", "fatal: unable to access 'https://github.com/org/repo.git/': The requested URL returned error: 503"],
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
    // #4438: only the exact bare reasons retry; near-misses and other reasons stay permanent.
    ["missing necessary objects with trailing text", `To ${URL}\n ! [remote rejected] ${SHA} -> agent/issue-7 (${MNO}) x\n${trailer()}`],
    ["singular missing necessary object", full("agent/issue-7", "missing necessary object")],
    ["missing necessary objects with a detail suffix", full("agent/issue-7", `${MNO}: abc123`)],
    ["unpacker error", full("agent/issue-7", "n/a (unpacker error)")],
    ["bad pack", full("agent/issue-7", "bad pack")],
    ["shallow update not allowed", full("agent/issue-7", "shallow update not allowed")],
    ["status line without the ! marker", `To ${URL}\n [remote rejected] ${SHA} -> agent/issue-7 (${MNO})\n${trailer()}`],
    ["status line without ->", `To ${URL}\n ! [remote rejected] ${SHA} agent/issue-7 (${MNO})\n${trailer()}`],
    ["status line without parens around the reason", `To ${URL}\n ! [remote rejected] ${SHA} -> agent/issue-7 ${MNO}\n${trailer()}`],
    ["missing necessary objects plus a pre-receive hook declined ref", `To ${URL}\n${status("a", MNO)}${status("b", "pre-receive hook declined")}${trailer()}`],
    ["bare (failed) plus a bad pack ref", `To ${URL}\n${status("a")}${status("b", "bad pack")}${trailer()}`],
    ["missing necessary objects plus GH006", full("agent/issue-7", MNO, "remote: error: GH006: Protected branch update failed for refs/heads/agent/issue-7.\n")],
    ["missing necessary objects plus GH001", full("agent/issue-7", MNO, "remote: error: GH001: Large files detected.\n")],
    ["missing necessary objects plus an unmarked remote: policy line", full("agent/issue-7", MNO, "remote: Push blocked by repository policy\n")],
    ["missing necessary objects plus a non-remote line", full("agent/issue-7", MNO, "hint: something odd\n")],
    ["[rejected] fetch first beside missing necessary objects", `To ${URL}\n${status("a", MNO)} ! [rejected]        main -> main (fetch first)\n${trailer()}`],
    ["[rejected] non-fast-forward beside missing necessary objects", `To ${URL}\n${status("a", MNO)} ! [rejected]        main -> main (non-fast-forward)\n${trailer()}`],
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
    // #4716 lookalikes: only the exact observed shape retries.
    ["bare (Internal Server Error) with no banner", ISE_TO + ISE_STATUS_LINE + ISE_TRAILER],
    ["ISE banner missing the Internal Server Error line", ise({ b1: "" })],
    ["ISE banner missing the Request ID line", ise({ b2: "" })],
    ["ISE banner missing the Time line", ise({ b3: "" })],
    ["ISE banner with only the Internal Server Error line", ise({ b2: "", b3: "" })],
    ["ISE banner with only the Request ID line", ise({ b1: "", b3: "" })],
    ["ISE banner with only the Time line", ise({ b1: "", b2: "" })],
    ["ISE banner with the Internal Server Error line repeated", ise({ b1: ISE_B1 + ISE_B1 })],
    ["ISE banner with the Request ID line repeated", ise({ b2: ISE_B2 + ISE_B2 })],
    ["ISE banner with the Time line repeated", ise({ b3: ISE_B3 + ISE_B3 })],
    ["ISE banner with a second (Internal Server Error) ref", ise({ extra: iseStatus("Internal Server Error") })],
    ["ISE banner with a second (failed) ref", ise({ extra: iseStatus("failed") })],
    ["ISE banner with a second [rejected] ref", ise({ extra: " ! [rejected]        main -> main (fetch first)\n" })],
    ["ISE banner with a (failed) status line", ise({ statusLine: iseStatus("failed") })],
    ["ISE banner with a (missing necessary objects) status line", ise({ statusLine: iseStatus(MNO) })],
    ["ISE banner plus GH006", ise({ extra: "remote: error: GH006: Protected branch update failed for refs/heads/agent/issue-4416.\n" })],
    ["ISE banner plus GH013", ise({ extra: "remote: error: GH013: Repository rule violations found for refs/heads/agent/issue-4416.\n" })],
    ["ISE banner plus a push protection line", ise({ extra: "remote: - GITHUB PUSH PROTECTION\n" })],
    ["ISE banner plus remote: error:", ise({ extra: "remote: error: weird thing\n" })],
    ["ISE banner plus remote: fatal:", ise({ extra: "remote: fatal: weird thing\n" })],
    ["ISE banner plus a free-text remote: line", ise({ extra: "remote: Push blocked by repository policy\n" })],
    ["Internal Server Error banner with extra text", ise({ b1: "remote: Internal Server Error x\n" })],
    ["lowercase internal server error banner", ise({ b1: "remote: internal server error\n" })],
    ["remote: error: Internal Server Error banner", ise({ b1: "remote: error: Internal Server Error\n" })],
    ["Internal Server Error banner with two spaces after remote:", ise({ b1: "remote:  Internal Server Error\n" })],
    ["Internal Server Error banner with a leading space", ise({ b1: " remote: Internal Server Error\n" })],
    ["Request ID with trailing text", ise({ b2: "remote: Request ID 0000:1111:2222222:3333333:44444444 extra\n" })],
    ["Request ID with a non-hex token", ise({ b2: "remote: Request ID 0000:XYZW:2222222\n" })],
    ["Request ID with lowercase hex", ise({ b2: "remote: Request ID 0000:abcd:2222222\n" })],
    ["Request ID with a single group", ise({ b2: "remote: Request ID 00001111\n" })],
    ["Time with fractional seconds", ise({ b3: "remote: Time 2026-11-25T16:15:24.123Z\n" })],
    ["Time with an offset", ise({ b3: "remote: Time 2026-11-25T16:15:24+00:00\n" })],
    ["Time with a date only", ise({ b3: "remote: Time 2026-11-25\n" })],
    ["Time with trailing text", ise({ b3: "remote: Time 2026-11-25T16:15:24Z extra\n" })],
    ["lowercase time banner", ise({ b3: "remote: time 2026-11-25t16:15:24z\n" })],
    ["(Internal Server Error) with trailing text", ise({ statusLine: ` ! [remote rejected]     ${SHA} -> agent/issue-4416 (Internal Server Error) x\n` })],
    ["lowercase (internal server error)", ise({ statusLine: iseStatus("internal server error") })],
    ["(Internal Server Errors)", ise({ statusLine: iseStatus("Internal Server Errors") })],
    ["ISE banner with a [rejected] status line", ise({ statusLine: " ! [rejected]        agent/issue-4416 -> agent/issue-4416 (fetch first)\n" })],
    ["ISE banner with a non-fast-forward status line", ise({ statusLine: " ! [rejected]        agent/issue-4416 -> agent/issue-4416 (non-fast-forward)\n" })],
    ["observed #4716 stderr with a leading space before the first line", " " + OBSERVED_ISE_STDERR],
    // Legacy input handling is unchanged: the direct text rule reads exactly the string passed in.
    ["a leading-space To line beside a bare (failed), passed directly", ` To ${URL}\n${status("agent/issue-7")}${trailer()}`],
    // Controls: an auth error mixed with transient wording, and an unrecognized error.
    ["authentication failed mixed with a 503", "remote: Authentication failed\nfatal: unable to access: The requested URL returned error: 503"],
    ["403 mixed with could not resolve host", "fatal: unable to access: The requested URL returned error: 403\nCould not resolve host: github.com"],
    ["unrecognized error", "fatal: something entirely unexpected happened"],
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

  it("retained observed #4438 missing-necessary-objects stderr is transient", async () => {
    assert.equal(await classifyOf(OBSERVED_MNO_STDERR), "transient");
  });

  it("retained observed #4716 Internal Server Error stderr with its banner is transient", async () => {
    assert.equal(await classifyOf(OBSERVED_ISE_STDERR), "transient");
  });

  it("retained #4716 stderr with a leading space before the banner stays permanent (not trimmed away)", async () => {
    assert.equal(await classifyOf(" " + OBSERVED_ISE_STDERR), "permanent");
  });

  it("retained leading-space To line beside a bare (failed) stays transient (legacy trimmed input unchanged)", async () => {
    assert.equal(await classifyOf(` To ${URL}\n${status("agent/issue-7")}${trailer()}`), "transient");
  });

  it("retained stderr with a stated reason stays permanent", async () => {
    assert.equal(await classifyOf(full("agent/issue-404", "pre-receive hook declined")), "permanent");
  });
});
