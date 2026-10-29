// Issue #1563 M2: every Claude SDK query path must hand the CLI `verbatimPrompts: true`, so a
// streamed prompt is delivered as written (no `@path` expansion, no leading-slash command
// dispatch) before any PreToolUse hook runs. Each path-level test drives the real production
// entry point with a stub queryFn and asserts the options ACTUALLY passed to the query, which
// also covers the spreads (`{ ...base, abortController }`) that derive the final options.
// The assertion is strict (`=== true`) so an omitted key and an explicit `false` both fail.

import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import { randomUUID } from "node:crypto";
import path from "node:path";
import { fileURLToPath } from "node:url";
import type { Options as SdkOptions } from "@anthropic-ai/claude-agent-sdk";

import { ClaudeHarness, buildSdkOptions, type ClaudeTurnConfig } from "../src/claude-harness.js";
import { ClaudeAdviceHarness } from "../src/claude-advice-harness.js";
import { ChatExecutor, buildChatSdkOptions, buildChatSystemPrompt, UZI_SRC_DIR, type ChatContext } from "../src/chat-executor.js";
import { IsolatedExecutor, buildIsolatedSdkOptions, isolateSdkEnv, type IsolatedContext } from "../src/isolated-executor.js";
import { ClaudeCrossCheck, buildClaudeCrossCheckOptions } from "../src/claude-cross-check.js";
import { JobRunner, buildJobResultServer, buildJobSdkOptions, laneJobSurface } from "../src/job-runner.js";
import { buildFetchToolsServer } from "../src/fetch-tools.js";
import { buildSdkEnv } from "../src/sdk-env.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import type { RunTurnRequest } from "../src/harness.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

const OAUTH = "dummy-oauth-token-verbatim-0000";

const dirs: string[] = [];
afterEach(async () => {
  while (dirs.length) await fsp.rm(dirs.pop()!, { recursive: true, force: true });
});
function tmp(prefix: string): string {
  const d = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  dirs.push(d);
  return d;
}

function assertVerbatim(options: SdkOptions | undefined, where: string): void {
  assert.ok(options, `${where}: the query was never called`);
  assert.equal(options.verbatimPrompts, true, `${where}: verbatimPrompts must be exactly true`);
}

/** A stub query that records the options it was handed and yields no frames. */
function capturing(): { queryFn: SdkQueryFn; seen: { options?: SdkOptions } } {
  const seen: { options?: SdkOptions } = {};
  const queryFn = ((params: { options: SdkOptions }) => {
    seen.options = params.options;
    return (async function* () {
      /* no frames */
    })();
  }) as unknown as SdkQueryFn;
  return { queryFn, seen };
}

const runLaneConfig: ClaudeTurnConfig = {
  cwd: "/tmp/uzi-verbatim-worktree",
  env: {},
  skillsPluginPath: "/tmp/uzi-verbatim-plugin",
  skills: [],
  systemPrompt: "sys",
  agents: {},
  mcpServers: {},
  preToolUse: [],
};

describe("verbatimPrompts reaches every Claude SDK query (#1563 M2)", () => {
  it("run lane: ClaudeHarness turn passes verbatimPrompts true to the query", async () => {
    const { queryFn, seen } = capturing();
    const harness = new ClaudeHarness({
      queryFn,
      spawn: () => ({ pid: undefined }),
      kill: () => true,
      log: nullLogger(),
      contextUsageTimeoutMs: 2000,
      spawnedPids: new Set<number>(),
      homeDir: path.join(os.tmpdir(), "uzi-verbatim-run-home-does-not-exist"),
    });
    harness.prepareTurn(runLaneConfig, { pid: undefined });
    const request: RunTurnRequest = {
      prompt: "p",
      systemPrompt: "sys",
      signal: new AbortController().signal,
      phase: "plan",
      agents: {},
      leadSkills: [],
    };
    for await (const _ of harness.startTurn(request).events) {
      /* drain to trigger the query */
    }
    assertVerbatim(seen.options, "run lane");
  });

  it("chat: ChatExecutor turn passes verbatimPrompts true to the query", async () => {
    const homeDir = tmp("uzi-verbatim-chat-");
    const { queryFn, seen } = capturing();
    let delivered = 0;
    const ctx: ChatContext = {
      runId: "chat-1",
      oauthToken: OAUTH,
      sessionId: null,
      emit: () => {},
      onSessionId: () => {},
      maxTurns: 5,
      turnTimeoutMs: 5000,
      nextUserMessage: () =>
        Promise.resolve(delivered++ === 0 ? { text: "hi", inputId: 1 } : undefined),
      followUpIncluded: () => {},
    };
    await new ChatExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assertVerbatim(seen.options, "chat");
  });

  it("advice: ClaudeAdviceHarness.run passes verbatimPrompts true to the query", async () => {
    const { queryFn, seen } = capturing();
    const abort = new AbortController();
    await new ClaudeAdviceHarness({
      token: "dummy-advice-tok",
      homeDir: path.join(os.tmpdir(), "uzi-verbatim-advice-home-does-not-exist"),
      abort,
      queryFn,
      denyReason: "advice lane is read-only and runs no tools",
      log: nullLogger(),
    }).run(
      { label: "review", systemPrompt: "sys", prompt: "p", output: { kind: "text" }, signal: abort.signal, timeoutMs: 5000 },
      { onTerminal() {} },
    );
    assertVerbatim(seen.options, "advice");
  });

  it("isolated: IsolatedExecutor.run passes verbatimPrompts true to the query", async () => {
    const root = tmp("uzi-verbatim-isolated-");
    const workspace = path.join(root, "workspace");
    fs.mkdirSync(workspace);
    fs.mkdirSync(path.join(root, "home"));
    const fetch = buildFetchToolsServer({ fetcherUrl: "https://127.0.0.1:1", ca: "", credential: "cred", workspace, log: nullLogger() });
    const ctx: IsolatedContext = {
      runId: "run-1",
      oauthToken: OAUTH,
      workspace,
      homeDir: path.join(root, "home"),
      prompt: "research",
      fetchServer: fetch.server,
      emit: () => {},
      signal: new AbortController().signal,
      maxTurns: 5,
      timeoutMs: 30_000,
    };
    const { queryFn, seen } = capturing();
    // No init frame arrives, so the run fails closed; only the captured options matter here.
    await assert.rejects(new IsolatedExecutor(nullLogger(), { queryFn, kill: () => true, secretPaths: ["/tmp/uzi-synthetic-deny/denied-file"] }).run(ctx));
    assertVerbatim(seen.options, "isolated");
  });

  it("checker: ClaudeCrossCheck.run passes verbatimPrompts true to the query", async () => {
    const { queryFn, seen } = capturing();
    const checker = new ClaudeCrossCheck(nullLogger(), { secretPaths: ["/tmp/uzi-synthetic-deny/"], queryFn, kill: () => true });
    const claim = {
      kind: "cross_check", run_id: "check", issue_iid: null, issue_title: "Issue",
      repo: { id: "repo", full_name: "owner/repo", url: "https://example.test/owner/repo", clone_url: "https://example.test/owner/repo.git", default_branch: "main" },
      last_seq: 0, agents: [], issue_description: "Untrusted issue", claim_generation: 7,
      config: { default_model: "sonnet", default_effort: "high" },
      secrets: { forge_pat: "", anthropic_oauth_token: OAUTH },
      cross_check: { stage: "plan", lead_run_id: "lead", round: 1, candidate_digest: "digest",
        deadline_at: "2099-01-01T00:00:00Z", plan_md: "Plan", milestones: [], required_capabilities: [],
        required_tools: [], size_class: "small", base_commit: "a".repeat(40), planning_diff: "diff" },
    } as unknown as ClaimResponse;
    // The stream ends with no verdict, so the check fails; only the captured options matter here.
    await assert.rejects(checker.run(claim, "/checkout", "/home", new AbortController().signal, async () => {}));
    assertVerbatim(seen.options, "checker");
  });

  describe("job runner", () => {
    function jobClaim(isolated: boolean): ClaimResponse {
      return {
        run_id: randomUUID(),
        kind: "job",
        issue_iid: null,
        issue_title: "",
        issue_description: "",
        repo: undefined,
        secrets: { forge_pat: "", anthropic_oauth_token: OAUTH },
        last_seq: 0,
        agents: [],
        claim_generation: 7,
        budget_wall_seconds: 30,
        job: { type: "research", title: "T", prompt: "Summarise the input.", inputs: [{ name: "doc.md", content: "body" }] },
        config: {},
        ...(isolated ? { isolated_fetch: { credential: "lane-fetch-cred-" + "0123456789abcdef", profile: "vendor-docs", hosts: ["docs.example.com"] } } : {}),
      } as unknown as ClaimResponse;
    }
    const client = {
      reportState: async () => ({ applied: true }),
      postMessages: async () => {},
      getInputs: async () => ({ inputs: [], receipts: false }),
      ackInputs: async () => ({ active: true, inputs: [] }),
      applyInputs: async () => ({ active: true, inputs: [] }),
      postJobResult: async () => {},
      hasFeature: () => false,
    } as unknown as WorkerClient;

    async function runJob(isolated: boolean): Promise<SdkOptions | undefined> {
      const root = tmp("uzi-verbatim-job-");
      const caFile = path.join(root, "ca.pem");
      await fsp.writeFile(caFile, "not parsed unless a fetch happens\n");
      const { queryFn, seen } = capturing();
      const runner = new JobRunner(client, nullLogger(), {
        queryFn,
        jobsRoot: path.join(root, "jobs"),
        batchMs: 5,
        cancelPollMs: 10,
        outputRetryDelaysMs: [1, 1],
        ...(isolated ? { fetcherUrl: "https://uzi-fetcher.test:8443", fetcherCaFile: caFile, splitActive: false } : {}),
      });
      // The stream ends without a submitted result, so the job fails; that is reported, not thrown.
      await runner.execute(jobClaim(isolated)).catch(() => {});
      return seen.options;
    }

    it("job (non-lane): JobRunner.execute passes verbatimPrompts true to the query", async () => {
      assertVerbatim(await runJob(false), "job non-lane");
    });

    it("job (lane): JobRunner.execute with isolated_fetch passes verbatimPrompts true to the query", async () => {
      const options = await runJob(true);
      assertVerbatim(options, "job lane");
      assert.ok(Object.keys(options!.mcpServers ?? {}).includes("uzi_fetch"), "this drove the lane builder, not the plain one");
    });
  });
});

describe("verbatimPrompts is set by every exported options builder (#1563 M2)", () => {
  it("buildSdkOptions (run lane)", () => {
    assert.equal(buildSdkOptions(runLaneConfig).verbatimPrompts, true);
  });

  it("buildChatSdkOptions", () => {
    const options = buildChatSdkOptions({
      env: { CLAUDE_CODE_OAUTH_TOKEN: OAUTH, HOME: "/data/agent-home", PATH: "/usr/bin", ANTHROPIC_API_KEY: undefined, ANTHROPIC_AUTH_TOKEN: undefined },
      systemPrompt: buildChatSystemPrompt(),
      cwd: UZI_SRC_DIR,
      log: nullLogger(),
      secretPaths: ["/tmp/uzi-synthetic-deny/denied-file"],
    });
    assert.equal(options.verbatimPrompts, true);
  });

  it("buildIsolatedSdkOptions", () => {
    const root = tmp("uzi-verbatim-iso-builder-");
    const fetch = buildFetchToolsServer({ fetcherUrl: "https://127.0.0.1:1", ca: "", credential: "cred", workspace: root, log: nullLogger() });
    const options = buildIsolatedSdkOptions({
      env: isolateSdkEnv(buildSdkEnv(OAUTH, root)),
      cwd: root,
      log: nullLogger(),
      secretPaths: ["/tmp/uzi-synthetic-deny/denied-file"],
      fetchServer: fetch.server,
      gate: { passed: false },
      maxTurns: 5,
    });
    assert.equal(options.verbatimPrompts, true);
  });

  it("buildClaudeCrossCheckOptions", () => {
    const options = buildClaudeCrossCheckOptions({
      cwd: "/checkout", homeDir: "/home", oauthToken: OAUTH, log: nullLogger(),
      secretPaths: ["/tmp/uzi-synthetic-deny/"], gate: { passed: true }, brief: "brief",
    });
    assert.equal(options.verbatimPrompts, true);
  });

  for (const lane of [false, true]) {
    it(`buildJobSdkOptions (${lane ? "with" : "without"} lane)`, () => {
      const rs = buildJobResultServer({});
      const options = buildJobSdkOptions({
        env: {},
        systemPrompt: "s",
        workDir: "/data/jobs/run/work",
        log: nullLogger(),
        secretPaths: [],
        resultServer: rs.server,
        toolNames: rs.toolNames,
        ...(lane ? { lane: { surface: laneJobSurface(undefined), gate: { passed: false }, fetchServer: {} as never } } : {}),
      });
      assert.equal(options.verbatimPrompts, true);
    });
  }
});

// A future Claude options builder must not silently omit the option. Every builder keeps the
// literal `settingSources: []` (semgrep/settings-sources-isolation.yml), so each such literal in
// agent/src, however it is formatted, must sit in an object literal that also sets
// `verbatimPrompts: true` (issue #1563; a test, not a Semgrep rule, by the issue's choice). A builder
// that drops `settingSources` altogether is the Semgrep rule's and review's concern, not this test's.

/** The text of the object literal enclosing `index`: from its opening `{` to the matching `}`. */
function enclosingObject(text: string, index: number): string {
  let depth = 0;
  let start = index;
  for (; start >= 0; start--) {
    const c = text[start];
    if (c === "}") depth++;
    else if (c === "{") {
      if (depth === 0) break;
      depth--;
    }
  }
  depth = 0;
  let end = index;
  for (; end < text.length; end++) {
    const c = text[end];
    if (c === "{") depth++;
    else if (c === "}") {
      if (depth === 0) break;
      depth--;
    }
  }
  return text.slice(start, end + 1);
}

describe("every settingSources: [] literal in agent/src is paired with verbatimPrompts: true (#1563)", () => {
  it("finds the seven known builders and no unpaired literal", () => {
    const srcDir = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src");
    const files = fs.readdirSync(srcDir, { recursive: true, encoding: "utf8" }).filter((f) => f.endsWith(".ts"));
    const sites: string[] = [];
    const unpaired: string[] = [];
    for (const rel of files) {
      const text = fs.readFileSync(path.join(srcDir, rel), "utf8");
      for (const m of text.matchAll(/settingSources\s*:\s*\[\s*\]/g)) {
        const line = text.slice(0, m.index).split("\n").length;
        // Skip mentions inside comments; the builders carry the literal as code.
        const lineText = text.split("\n")[line - 1]!.trim();
        if (lineText.startsWith("//") || lineText.startsWith("*") || lineText.startsWith("/*")) continue;
        sites.push(`${rel}:${line}`);
        if (!/verbatimPrompts\s*:\s*true\b/.test(enclosingObject(text, m.index))) unpaired.push(`${rel}:${line}`);
      }
    }
    assert.deepEqual(unpaired, [], `settingSources: [] without verbatimPrompts: true at ${unpaired.join(", ")}`);
    assert.equal(sites.length, 7, `expected the seven known Claude options builders, found ${sites.join(", ")}`);
  });
});
