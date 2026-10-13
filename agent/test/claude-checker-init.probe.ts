// Outside the gate's *.test.ts glob: real SDK startup is a manual, bounded offline proof.
// node --import tsx test/claude-checker-init.probe.ts /absolute/native/claude [/absolute/sdk.mjs]
// Use the matching SDK/CLI pair to repeat this for deployed and newly pinned releases.
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { buildClaudeCrossCheckOptions } from "../src/claude-cross-check.js";
import { checkIsolatedInit } from "../src/isolated-executor.js";
import { promptStream } from "../src/sdk-messages.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";

const cli = process.argv[2];
if (!cli || !path.isAbsolute(cli)) throw new Error("an explicit absolute native CLI path is required");
const sdk = await import(process.argv[3] ? pathToFileURL(process.argv[3]).href : "@anthropic-ai/claude-agent-sdk") as { query: SdkQueryFn };
const root = await fs.mkdtemp(path.join(os.tmpdir(), "claude-init-probe-"));
const surface = { tools: ["Read", "Grep", "Glob"], mcpServers: [], plugins: [], skills: [] };
try {
  for (const disabled of [false, true]) {
    const home = path.join(root, disabled ? "after" : "before");
    await fs.mkdir(home);
    const abort = new AbortController();
    const options = buildClaudeCrossCheckOptions({ cwd: root, homeDir: home, oauthToken: "dummy-offline-credential",
      log: nullLogger(), secretPaths: [path.join(root, "worker-secret")], gate: { passed: false }, brief: "Return JSON findings.", maxTurns: 1 });
    // Copy the settings map before mutating it: the production builder shares the fixed map.
    options.settings = { enabledPlugins: { ...(options.settings as { enabledPlugins: Record<string, boolean> }).enabledPlugins,
      ...(disabled ? { "cc-plugin-plugin-authoring@builtin": false } : {}) } };
    if (!disabled) delete (options.settings as { enabledPlugins: Record<string, boolean> }).enabledPlugins["cc-plugin-plugin-authoring@builtin"];
    options.pathToClaudeCodeExecutable = cli;
    options.abortController = abort;
    options.env = { ...options.env, HTTPS_PROXY: "http://127.0.0.1:9", HTTP_PROXY: "http://127.0.0.1:9" };
    const timer = setTimeout(() => abort.abort(), 45000);
    let observed = false;
    try {
      for await (const msg of sdk.query({ prompt: promptStream('Return {"findings":[]}'), options })) {
        const frame = msg as Record<string, unknown>;
        if (frame.type !== "system" || frame.subtype !== "init") continue;
        observed = true;
        const refusal = checkIsolatedInit(msg, surface);
        console.log(JSON.stringify({ version: frame.claude_code_version, disabled, accepted: refusal === undefined,
          tools: frame.tools, agents: frame.agents, skills: frame.skills, plugins: frame.plugins, mcp_servers: frame.mcp_servers }));
        if (disabled) assert.equal(refusal, undefined);
        else assert.equal(refusal, "a plugin is loaded");
        abort.abort();
        break;
      }
      assert.ok(observed, "SDK did not emit init within the bounded probe");
    } finally { clearTimeout(timer); abort.abort(); }
  }
} finally { await fs.rm(root, { recursive: true, force: true }); }
