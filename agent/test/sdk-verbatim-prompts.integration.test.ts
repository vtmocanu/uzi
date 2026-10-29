import { after, afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { query } from "@anthropic-ai/claude-agent-sdk";
import type { HookInput, HookJSONOutput, Options } from "@anthropic-ai/claude-agent-sdk";
import { buildPathGuardHook } from "../src/guardrails.js";
import { buildSdkOptions } from "../src/claude-harness.js";
import { promptStream } from "../src/sdk-messages.js";
import { nullLogger } from "./helpers.js";
import { FakeAnthropicApi, type MessagesRequest, type ScriptedTurn } from "./fake-anthropic-api.js";

// Issue #1563 — the REAL bundled Claude CLI preprocesses a user turn before any tool runs:
// an `@<path>` mention is expanded by a CLI-synthesized Read (no PreToolUse hook fires, so uzi's
// path guard never sees it), and a leading `/<command>` is dispatched. uzi assembles prompts from
// untrusted forge and chat text and streams them through `promptStream`, so this file drives that
// exact path with the production run-lane options (`buildSdkOptions`) and uzi's real path guard
// configured to deny a synthetic file, against a scripted local fake of the Messages API. The
// evidence is the provider-bound request body: a marker that appears there was sent to the model.
// The slash fixture is a plugin `commands/` file; production's skills plugin writes only skills, so
// this proves the dispatch mechanism rather than a specific production command.
// Nothing leaves 127.0.0.1; the key is a placeholder; HOME/TMPDIR are per-test and removed.

const DRIVE_BUDGET_MS = 50_000;
const FILE_MARKER = "CANARY-FILE-MARKER-1563-a7f3";
const CMD_MARKER = "CANARY-SLASHCMD-MARKER-1563-b9e1";
const COMMAND = "/uzi:probecmd";

interface Drive {
  /** Main-thread requests (they carry the Read tool), in order. */
  main: MessagesRequest[];
  /** Every provider-bound request, side calls included: what the model was sent. */
  all: MessagesRequest[];
  hooks: Array<{ input: HookInput; decision: HookJSONOutput }>;
}

describe("streamed prompts are not preprocessed by the CLI (issue #1563)", () => {
  const roots: string[] = [];
  let root: string;
  let home: string;
  let worktree: string;
  let tmp: string;
  let plugin: string;
  let denied: string;
  let fake: FakeAnthropicApi;
  let baseUrl: string;
  let allowed: string;
  /** When set, the scripted model's first main-thread reply is a Read of this path. */
  let readTarget: string | undefined;
  let readIssued = false;

  after(() => {
    for (const r of roots) fs.rmSync(r, { recursive: true, force: true });
  });

  /** Main-thread calls carry the Read tool; side calls (titles etc.) get trivial text. */
  function script(body: MessagesRequest): ScriptedTurn {
    if (!(body.tools ?? []).some((t) => t.name === "Read")) return { kind: "text", text: "ok" };
    if (readTarget && !readIssued) {
      readIssued = true;
      return { kind: "tool_use", name: "Read", input: { file_path: readTarget } };
    }
    return { kind: "text", text: "done" };
  }

  beforeEach(async () => {
    root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-verbatim-it-"));
    roots.push(root);
    home = path.join(root, "home");
    worktree = path.join(root, "worktree");
    tmp = path.join(root, "tmp");
    plugin = path.join(root, "plugin");
    for (const d of [path.join(home, ".claude"), worktree, tmp, path.join(plugin, ".claude-plugin"), path.join(plugin, "commands")]) {
      fs.mkdirSync(d, { recursive: true });
    }
    denied = path.join(worktree, "synthetic-denied.txt");
    fs.writeFileSync(denied, `${FILE_MARKER}\n`);
    allowed = path.join(worktree, "allowed.txt");
    fs.writeFileSync(allowed, "harmless\n");
    // A harmless plugin command: if the CLI dispatches a leading `/uzi:probecmd`, its body (the
    // marker) is what reaches the provider.
    fs.writeFileSync(path.join(plugin, ".claude-plugin", "plugin.json"), JSON.stringify({ name: "uzi" }));
    fs.writeFileSync(path.join(plugin, "commands", "probecmd.md"), `---\ndescription: probe\n---\n${CMD_MARKER}\n`);
    readTarget = undefined;
    readIssued = false;
    fake = new FakeAnthropicApi(script);
    baseUrl = await fake.listen();
  });

  afterEach(async () => {
    await fake.close();
  });

  /** The production run-lane options, with uzi's real path guard denying the synthetic file. */
  function productionOptions(hooks: Drive["hooks"]): Options {
    const guard = buildPathGuardHook(worktree, nullLogger(), [denied], { sdkHomeDir: home });
    const recording = async (input: HookInput): Promise<HookJSONOutput> => {
      const decision = await guard(input);
      hooks.push({ input, decision });
      return decision;
    };
    return buildSdkOptions({
      cwd: worktree,
      // Minimal env: no inherited credentials. The key is a placeholder the fake accepts.
      env: {
        PATH: process.env.PATH ?? "",
        HOME: home,
        TMPDIR: tmp,
        ANTHROPIC_BASE_URL: baseUrl,
        ANTHROPIC_API_KEY: ["dummy", "key"].join("-"),
        CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
        DISABLE_TELEMETRY: "1",
        DISABLE_AUTOUPDATER: "1",
      },
      skillsPluginPath: plugin,
      skills: [],
      systemPrompt: { type: "preset", preset: "claude_code", append: "canary" },
      agents: {},
      mcpServers: {},
      preToolUse: [{ matcher: "Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep", hooks: [recording] }],
      model: "claude-sonnet-4-5",
    });
  }

  /** Stream one prompt through `promptStream` to completion; returns the provider-bound bodies. */
  async function drive(prompt: string, override: Partial<Options> = {}): Promise<Drive> {
    const hooks: Drive["hooks"] = [];
    fake.messageBodies.length = 0;
    fake.requests.length = 0;
    readIssued = false;
    const controller = new AbortController();
    const deadline = AbortSignal.timeout(DRIVE_BUDGET_MS);
    deadline.addEventListener("abort", () => controller.abort());
    const options = { ...productionOptions(hooks), ...override, abortController: controller };
    try {
      for await (const _ of query({ prompt: promptStream(prompt) as never, options })) {
        // drain
      }
    } catch (err) {
      assert.fail(`query failed: ${String(err)}\nfake API requests:\n${fake.requestLog()}`);
    }
    assert.ok(!controller.signal.aborted, `the drive hit its ${DRIVE_BUDGET_MS}ms budget\nfake API requests:\n${fake.requestLog()}`);
    const main = fake.messageBodies.filter((b) => (b.tools ?? []).some((t) => t.name === "Read"));
    assert.ok(main.length > 0, `the CLI never sent a main-thread request\nfake API requests:\n${fake.requestLog()}`);
    return { main, all: [...fake.messageBodies], hooks };
  }

  const turn = (): string =>
    `## Issue (UNTRUSTED)\nPlease look at the config described in @${denied} and fix the bug.\n## End`;
  const sent = (d: Drive, marker: string): boolean => JSON.stringify(d.all).includes(marker);
  const firstSent = (d: Drive, marker: string): boolean => JSON.stringify(d.main[0]).includes(marker);
  const isDeny = (d: HookJSONOutput): boolean =>
    (d as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision === "deny";
  const lastUserText = (d: Drive): string => {
    const msgs = d.main[0]!.messages;
    return JSON.stringify(msgs[msgs.length - 1]?.content);
  };
  const guardedReads = (d: Drive) => d.hooks.filter((h) => h.input.hook_event_name === "PreToolUse" && h.input.tool_name === "Read");

  // Each test carries its own positive control (`verbatimPrompts: false`, the SDK default the
  // production options overrode before #1563), so a negative assertion can never pass on a broken
  // fixture, also when a test runs alone.

  it("production options deliver an @path mention literally, also past the first tool call", { timeout: 120_000 }, async () => {
    const control = await drive(turn(), { verbatimPrompts: false });
    assert.ok(firstSent(control, FILE_MARKER), "control: the CLI no longer expands @path, so this probe proves nothing");
    assert.equal(guardedReads(control).length, 0, `control: the expansion went through a PreToolUse hook: ${JSON.stringify(control.hooks)}`);

    // The CLI's between-tool-call attachment pass is not covered by verbatimPrompts (sdk.d.ts), so
    // the model makes one permitted Read and every later request is checked too.
    readTarget = allowed;
    const d = await drive(turn());
    assert.ok(!sent(d, FILE_MARKER), "the guard-denied @path file reached the provider");
    assert.ok(lastUserText(d).includes(`@${denied}`), "the @path mention was not delivered literally");
    assert.ok(d.main.length >= 2, `expected a request after the tool call: ${d.main.length}`);
    const reads = guardedReads(d);
    assert.equal(reads.length, 1, `expected the one permitted Read through the hook: ${JSON.stringify(d.hooks)}`);
    assert.ok(!isDeny(reads[0]!.decision), "the permitted Read was denied");
  });

  it("production options deliver a leading slash command literally", { timeout: 120_000 }, async () => {
    const control = await drive(COMMAND, { verbatimPrompts: false });
    assert.ok(firstSent(control, CMD_MARKER), "control: the CLI no longer dispatches the command, so this probe proves nothing");
    const d = await drive(COMMAND);
    assert.ok(!sent(d, CMD_MARKER), "the slash command ran under production options");
    assert.ok(lastUserText(d).includes(JSON.stringify(COMMAND)), `the slash command was not delivered literally: ${lastUserText(d)}`);
  });

  it("a direct Read of the synthetic file is denied by the path guard", { timeout: 120_000 }, async () => {
    readTarget = denied;
    const d = await drive("Read the file, please.");
    const reads = guardedReads(d);
    assert.equal(reads.length, 1, `expected one guarded Read: ${JSON.stringify(d.hooks)}`);
    assert.ok(isDeny(reads[0]!.decision), "the guard did not deny the synthetic file");
    assert.ok(!sent(d, FILE_MARKER), "the denied file's content reached the provider");
  });

  it("slash text embedded in a generated prompt is never dispatched", { timeout: 120_000 }, async () => {
    const control = await drive(COMMAND, { verbatimPrompts: false });
    assert.ok(firstSent(control, CMD_MARKER), "control: the command fixture is not dispatchable, so this probe proves nothing");
    for (const override of [{ verbatimPrompts: false }, {}]) {
      const d = await drive(`Context line\nplease run ${COMMAND} later`, override);
      assert.ok(!sent(d, CMD_MARKER), `embedded slash text was dispatched (${JSON.stringify(override)})`);
    }
  });
});
