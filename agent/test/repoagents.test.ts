import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { realProcfsSkip } from "./real-procfs.js";

const HAS_PROCFS = process.platform === "linux";

import {
  REPO_AGENTS_MAX_FILES,
  REPO_AGENT_DENIED_TOOLS,
  REPO_AGENT_MAX_DESCRIPTION_LEN,
  describeRepoAgentNote,
  detectRepoAgents,
  filterRepoAgentModel,
  repoAgentSummaries,
  repoAgentsDir,
  type RepoAgentNoteReason,
} from "../src/repoagents.js";
import { assembleAgents, subagentsFromTemplates } from "../src/agents.js";
import { REQUESTED_SOURCE_TABLE, NO_ROSTER_NOTE, caseLabel } from "./requested-agent-source-table.js";
import {
  type AgentSelection,
  type AgentSelectionParse,
  type AgentSource,
  type AgentTemplate,
  AGENT_EXCLUSIONS_MAX,
  encodeAgentSelection,
  parseAgentSelection,
  planAgentSource,
  resolveAgentSelection,
  seedAgentSelection,
} from "../src/protocol.js";

let clone: string;
beforeEach(() => {
  clone = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-repoagents-"));
});
afterEach(() => {
  fs.rmSync(clone, { recursive: true, force: true });
});

/** Write <clone>/.claude/agents/<file>. */
function writeAgent(file: string, content: string): void {
  const dir = repoAgentsDir(clone);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, file), content, "utf8");
}

/** The reasons of the notes emitted, in order. */
function reasons(notes: { reason: RepoAgentNoteReason }[]): RepoAgentNoteReason[] {
  return notes.map((n) => n.reason);
}

const CODER = [
  "---",
  "name: coder",
  "description: Implements features and fixes bugs.",
  "model: opus",
  "---",
  "",
  "Implement the requested change.",
  "",
].join("\n");

describe("filterRepoAgentModel", () => {
  const roleModels = ["gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6-luna"];
  const claudeModels = ["opus", "sonnet", "haiku", "claude-opus-4-8", "customClaude", "vgpt-6-sol", "gpt-6-solv"];
  for (const harness of ["claude", "codex"] as const) {
    it(`${harness}: projects all role ids and Claude aliases/custom ids without mutating input`, () => {
      for (const model of [...roleModels, ...claudeModels]) {
        const template: AgentTemplate = Object.freeze({
          name: "coder", description: "Works.", prompt_body: "Implement.", model, tools: ["Read"],
        });
        const expectedModel = (harness === "codex" ? roleModels : claudeModels).includes(model) ? model : undefined;
        const result = filterRepoAgentModel(template, harness);
        assert.equal(result.model, expectedModel, `${harness}: ${model}`);
        assert.equal(Object.hasOwn(result, "model"), expectedModel !== undefined);
        assert.deepEqual(result.tools, ["Read"]);
        assert.equal(result.prompt_body, "Implement.");
        assert.equal(template.model, model);
        assert.notEqual(result, template);
      }
    });

    it(`${harness}: preserves empty, explicitly undefined and absent pins`, () => {
      for (const pin of [{}, { model: undefined }, { model: "" }]) {
        const template: AgentTemplate = Object.freeze({ name: "coder", description: "Works.", prompt_body: "Implement.", ...pin });
        assert.deepEqual(filterRepoAgentModel(template, harness), template);
      }
    });
  }
});

describe("detectRepoAgents", { skip: !HAS_PROCFS ? "reads procfs (Linux only)" : realProcfsSkip("detectRepoAgents") }, () => {
  it("returns nothing when the repo has no .claude/agents", async () => {
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents, []);
    assert.deepEqual(notes, []);
  });

  it("parses frontmatter + body into an AgentTemplate", async () => {
    writeAgent("coder.md", CODER);
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(notes, []);
    assert.equal(agents.length, 1);
    assert.deepEqual(agents[0], {
      name: "coder",
      description: "Implements features and fixes bugs.",
      prompt_body: "Implement the requested change.\n",
      model: "opus",
    });
  });

  it("keeps a declared tools allowlist, and inherits when no tools key is present", async () => {
    writeAgent("coder.md", CODER); // no tools: line
    writeAgent("reviewer.md", "---\nname: reviewer\ndescription: Reviews.\ntools: Bash, Read, Grep\n---\n\nReview.\n");
    const { agents } = await detectRepoAgents(clone);
    // Absent `tools` must stay ABSENT: agents.ts reads null/absent/empty as
    // "inherit all tools", which is the contract the built-in coder relies on.
    assert.equal(agents.find((a) => a.name === "coder")!.tools, undefined);
    assert.deepEqual(agents.find((a) => a.name === "reviewer")!.tools, ["Bash", "Read", "Grep"]);
  });

  it("parses the inline-bracket and indented block-sequence forms of tools", async () => {
    writeAgent("bracket.md", "---\nname: bracket\ndescription: x.\ntools: [Bash, Read]\n---\n\nbody\n");
    writeAgent("block.md", "---\nname: block\ndescription: x.\ntools:\n  - Bash\n  - Grep\nmodel: sonnet\n---\n\nbody\n");
    const { agents } = await detectRepoAgents(clone);
    assert.deepEqual(agents.find((a) => a.name === "bracket")!.tools, ["Bash", "Read"]);
    const block = agents.find((a) => a.name === "block")!;
    assert.deepEqual(block.tools, ["Bash", "Grep"]);
    // The key that follows the block sequence still parses (the sequence ended).
    assert.equal(block.model, "sonnet");
  });

  it("parses a ZERO-INDENT block sequence (prettier/yamlfmt output), not a false drop", async () => {
    // `tools:\n- Bash\n- Read` — the items sit at column 0. The old `/^\s+-/` regex
    // needed indentation, missed them, and dropped the agent with a FALSE
    // `tools_all_denied` even though Bash/Read are allowed (reviewer).
    writeAgent("zero.md", "---\nname: zero\ndescription: x.\ntools:\n- Bash\n- Read\n---\n\nbody\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents.map((a) => a.name), ["zero"]);
    assert.deepEqual(agents[0]!.tools, ["Bash", "Read"]);
    assert.deepEqual(notes, []);
  });

  it("names an agent after its filename slug when the frontmatter omits name", async () => {
    writeAgent("spec-keeper.md", "---\ndescription: Keeps specs in sync.\n---\n\nKeep specs in sync.\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(notes, []);
    assert.equal(agents[0]!.name, "spec-keeper");
  });

  it("parses a file with a leading UTF-8 BOM and CRLF line endings", async () => {
    // A Windows-authored repo: BOM used to make lines[0] !== "---" → invalid.
    writeAgent("bom.md", "\uFEFF---\r\nname: bom\r\ndescription: from windows.\r\n---\r\n\r\nbody\r\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(notes, []);
    assert.equal(agents[0]!.name, "bom");
    assert.equal(agents[0]!.description, "from windows.");
  });

  it("skips a file that is unparseable, unnamed, undescribed, or bodiless — never throwing", async () => {
    writeAgent("nofrontmatter.md", "# Just a document\n");
    writeAgent("unterminated.md", "---\nname: unterminated\ndescription: x.\n");
    writeAgent("Bad_Name.md", "---\ndescription: slug fallback fails the kebab rule.\n---\n\nbody\n");
    writeAgent("badname.md", "---\nname: Not Kebab\ndescription: x.\n---\n\nbody\n");
    writeAgent("nodesc.md", "---\nname: nodesc\n---\n\nbody\n");
    writeAgent("nobody.md", "---\nname: nobody\ndescription: x.\n---\n\n");
    writeAgent("good.md", CODER);

    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents.map((a) => a.name), ["coder"], "one valid file survives its six broken neighbours");
    assert.equal(notes.length, 6);
    assert.ok(notes.every((n) => n.reason === "invalid"));
    // The traversal-ish filename never lands verbatim in a note as a name.
    assert.ok(notes.every((n) => !n.name.includes("/")));
  });

  it("drops a reserved doc name that fails to parse (README) silently, but keeps a reserved name that IS a valid agent (issue #715)", async () => {
    // A README with no frontmatter used to surface an `invalid` note and count as a
    // failure; now a conventional non-role doc that fails to parse is dropped silently.
    // The name alone is NOT disqualifying: `security.md` with valid frontmatter parses
    // as a real agent and is kept. A malformed file with an ordinary name still fails.
    writeAgent("README.md", "# The roster\n\nJust docs, no frontmatter.\n");
    writeAgent("broken.md", "no frontmatter here\n");
    writeAgent("security.md", "---\nname: security\ndescription: the security reviewer.\n---\n\nReview for vulns.\n");
    writeAgent("good.md", CODER);

    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents.map((a) => a.name).sort(), ["coder", "security"], "reserved name with valid frontmatter is kept as an agent");
    // README is dropped silently; the only note is the genuinely malformed role.
    assert.deepEqual(notes, [{ name: "broken", reason: "invalid" }]);
    assert.ok(notes.every((n) => n.name.toLowerCase() !== "readme"), "README must never surface a note");
  });

  it("rejects a description that opens a YAML block scalar (`>` / `|`)", async () => {
    // `description: >` used to be KEPT with description === ">" — a one-character
    // junk description reaching the picker and, in M3, the delegation prompt.
    writeAgent("folded.md", "---\nname: folded\ndescription: >\nfolded body text\n---\n\nbody\n");
    writeAgent("literal.md", "---\nname: literal\ndescription: |-\n---\n\nbody\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents, []);
    assert.deepEqual(reasons(notes), ["invalid", "invalid"]);
  });

  it("rejects a description carrying control or bidirectional/format characters", async () => {
    // ANSI escape (control) and U+202E RIGHT-TO-LEFT OVERRIDE (format) both reach
    // the plan-gate panel as plain text; a bidi override can visually reorder it.
    writeAgent("ansi.md", "---\nname: ansi\ndescription: \u001b[31mALERT\u001b[0m\n---\n\nbody\n");
    writeAgent("bidi.md", "---\nname: bidi\ndescription: safe\u202edeliver\n---\n\nbody\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents, []);
    assert.deepEqual(reasons(notes), ["invalid", "invalid"]);
  });

  it("drops an oversized file without reading it", async () => {
    writeAgent("big.md", "---\nname: big\ndescription: x.\n---\n\n" + "x".repeat(64 * 1024 + 1));
    writeAgent("small.md", CODER);
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents.map((a) => a.name), ["coder"]);
    assert.deepEqual(notes, [{ name: "big", reason: "too_large" }]);
  });

  it("caps the description by UTF-8 bytes, not UTF-16 units (F3)", async () => {
    // '好' is 3 UTF-8 bytes / 1 UTF-16 unit. 400 of them are 1200 bytes but only 400
    // units, so the old `.length` (UTF-16) check accepted it while the API's Go
    // len() (bytes) then 400'd the whole report — and the fire-and-forget swallow
    // dropped the ENTIRE roster to NULL. Measuring bytes on both sides closes that:
    // the worker now drops just this agent here, before the API ever sees it.
    const multibyte = "好".repeat(400);
    assert.equal([...multibyte].length, 400, "sanity: 400 code points / UTF-16 units");
    assert.ok(Buffer.byteLength(multibyte, "utf8") > REPO_AGENT_MAX_DESCRIPTION_LEN, "sanity: over the byte cap");
    writeAgent("cjk.md", `---\nname: cjk\ndescription: ${multibyte}\n---\n\nbody\n`);
    // An ASCII description exactly at the byte cap still passes: only the unit
    // changed, not the cap. (1025 would be dropped — proven by the byte-count above.)
    writeAgent("atcap.md", `---\nname: atcap\ndescription: ${"x".repeat(REPO_AGENT_MAX_DESCRIPTION_LEN)}\n---\n\nbody\n`);
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents.map((a) => a.name), ["atcap"]);
    assert.deepEqual(notes, [{ name: "cjk", reason: "invalid" }]);
  });

  it("caps the roster at 16 files and reports the overflow as ONE aggregated note", async () => {
    // 20 files: a00…a19. The first 16 by filename are kept; the remaining 4 collapse
    // into a single note, never one-per-file (a 10k-file repo must not flood the seq).
    for (let i = 0; i < 20; i++) {
      const n = `a${String(i).padStart(2, "0")}`;
      writeAgent(`${n}.md`, `---\nname: ${n}\ndescription: agent ${i}.\n---\n\nbody\n`);
    }
    const { agents, notes } = await detectRepoAgents(clone);
    assert.equal(agents.length, REPO_AGENTS_MAX_FILES);
    assert.equal(agents.at(-1)!.name, "a15");
    assert.deepEqual(notes, [{ name: "", reason: "over_limit", count: 4 }]);
    assert.equal(describeRepoAgentNote(notes[0]!), "4 agent file(s) past the cap of 16 were ignored");
  });

  it("dedupes on name, first file (by filename) wins", async () => {
    writeAgent("a-first.md", "---\nname: dup\ndescription: the winner.\n---\n\nfirst\n");
    writeAgent("z-second.md", "---\nname: dup\ndescription: the loser.\n---\n\nsecond\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.equal(agents.length, 1);
    assert.equal(agents[0]!.description, "the winner.");
    assert.deepEqual(notes, [{ name: "dup", reason: "duplicate" }]);
  });

  it("strips the denylisted tools, keeps WebFetch/WebSearch and unknown names", async () => {
    writeAgent(
      "researcher.md",
      "---\nname: researcher\ndescription: x.\ntools: Read, Agent, WebFetch, WebSearch, ScheduleWakeup, CronCreate, SendMessage\n---\n\nbody\n",
    );
    const { agents, notes } = await detectRepoAgents(clone);
    // Only Agent + the deferral tools are stripped. WebFetch/WebSearch are HONORED
    // (user decision: Bash egress makes denying them theatre). SendMessage is kept
    // too — and, corrected 2026-08-03 (issue #210), it is NOT "silently unavailable"
    // as this comment used to say: the worker SDK provides it, so a repo agent that
    // declares it gets a working channel to the run's main thread. The assertion is
    // unchanged; only the reason it survives was stated wrongly.
    assert.deepEqual(agents[0]!.tools, ["Read", "WebFetch", "WebSearch", "SendMessage"]);
    for (const denied of REPO_AGENT_DENIED_TOOLS) {
      assert.ok(!agents[0]!.tools!.includes(denied), `${denied} must never survive`);
    }
    assert.deepEqual(reasons(notes), ["tools_filtered"]);
    // The note names exactly what was removed, in declaration order.
    assert.deepEqual(notes[0]!.tools, ["Agent", "ScheduleWakeup", "CronCreate"]);
  });

  it("denies `Task` as the canonical alias of `Agent`", async () => {
    // `Task` canonicalizes to `Agent` in the SDK, so it must be stripped too.
    writeAgent("t.md", "---\nname: t\ndescription: x.\ntools: Read, Task\n---\n\nbody\n");
    // A file declaring ONLY Task resolves to an empty allowlist → dropped.
    writeAgent("taskonly.md", "---\nname: taskonly\ndescription: x.\ntools: [Task]\n---\n\nbody\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents.map((a) => a.name), ["t"]);
    assert.deepEqual(agents[0]!.tools, ["Read"]);
    const tFiltered = notes.find((n) => n.name === "t" && n.reason === "tools_filtered");
    assert.deepEqual(tFiltered!.tools, ["Task"]);
    assert.ok(notes.some((n) => n.name === "taskonly" && n.reason === "tools_all_denied"));
  });

  it("skips an agent whose declared tools are ALL denied, rather than granting inherit-all", async () => {
    // The trap: an empty `tools` array is read as inherit-all (agents.ts), so an
    // agent that asked for nothing but denied tools must be dropped, not widened.
    writeAgent("spawner.md", "---\nname: spawner\ndescription: x.\ntools: [Agent]\n---\n\nbody\n");
    writeAgent("deferrer.md", "---\nname: deferrer\ndescription: x.\ntools: ScheduleWakeup, CronCreate\n---\n\nbody\n");
    // `tools:` with no value is a DECLARED (empty) allowlist, not an absent key.
    writeAgent("empty.md", "---\nname: empty\ndescription: x.\ntools:\n---\n\nbody\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents, []);
    assert.deepEqual(reasons(notes), ["tools_all_denied", "tools_all_denied", "tools_all_denied"]);
  });

  it("drops malformed tool tokens without dropping the agent", async () => {
    writeAgent("odd.md", "---\nname: odd\ndescription: x.\ntools: Read, *, ../etc, mcp__srv__tool\n---\n\nbody\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents[0]!.tools, ["Read", "mcp__srv__tool"]);
    assert.deepEqual(notes, []);
  });

  it("honors any well-formed model (full id, not just an alias); ignores only unusable strings", async () => {
    writeAgent("aliased.md", "---\nname: aliased\ndescription: x.\nmodel: haiku\n---\n\nbody\n");
    writeAgent("full.md", "---\nname: full\ndescription: x.\nmodel: claude-opus-4-8\n---\n\nbody\n");
    writeAgent("typo.md", "---\nname: typo\ndescription: x.\nmodel: opusss\n---\n\nbody\n");
    // Ignored: a value with interior whitespace could never be a model id.
    writeAgent("spaced.md", "---\nname: spaced\ndescription: x.\nmodel: two words\n---\n\nbody\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.equal(agents.find((a) => a.name === "aliased")!.model, "haiku");
    assert.equal(agents.find((a) => a.name === "full")!.model, "claude-opus-4-8");
    // A typo is a valid TOKEN and is honored; the SDK surfaces the real error later.
    assert.equal(agents.find((a) => a.name === "typo")!.model, "opusss");
    assert.equal(agents.find((a) => a.name === "spaced")!.model, undefined);
    assert.deepEqual(reasons(notes), ["model_ignored"]);
    assert.equal(notes[0]!.name, "spaced");
  });

  it("regression (c): Markdown Codex pins never reach Claude SDK definitions", async () => {
    const roleModels = ["gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6-luna"];
    for (const [i, model] of roleModels.entries()) {
      writeAgent(`role-${i}.md`, `---\nname: role-${i}\ndescription: Codex role.\nmodel: ${model}\n---\n\nRole body.\n`);
    }
    writeAgent("lead.md", "---\nname: lead\ndescription: Repo lead.\nmodel: gpt-6-sol\n---\n\nRepo lead body.\n");
    writeAgent("alias.md", CODER);
    writeAgent("custom.md", "---\nname: custom\ndescription: Custom Claude.\nmodel: vgpt-6-sol\n---\n\nCustom body.\n");

    const claude = await detectRepoAgents(clone, "claude");
    const claudeDefinitions = subagentsFromTemplates(claude.agents, new Set());
    assert.equal(claude.folder, ".claude/agents");
    assert.deepEqual(claude.notes, []);
    for (const name of ["lead", ...roleModels.map((_, i) => `role-${i}`)]) {
      assert.ok(claudeDefinitions[name], `${name} remains an ordinary subagent`);
      assert.equal(Object.hasOwn(claudeDefinitions[name]!, "model"), false, `${name}: Codex pin must not reach Claude SDK`);
    }
    assert.equal(claudeDefinitions.coder!.model, "opus");
    assert.equal(claudeDefinitions.custom!.model, "vgpt-6-sol");

    // No native Codex folder: the same Markdown roster is delivered as fallback.
    const codex = await detectRepoAgents(clone, "codex");
    assert.equal(codex.folder, ".claude/agents");
    assert.deepEqual(codex.notes, [], "opposite-family pins are silently omitted");
    for (const [i, model] of roleModels.entries()) {
      assert.equal(codex.agents.find((a) => a.name === `role-${i}`)!.model, model);
    }
    assert.equal(codex.agents.find((a) => a.name === "lead")!.model, "gpt-6-sol");
    for (const name of ["coder", "custom"]) {
      assert.equal(Object.hasOwn(codex.agents.find((a) => a.name === name)!, "model"), false);
    }
  });

  it("keeps a repo file named `lead` as a subagent candidate (never filtered here)", async () => {
    // PRD Decision 3: the orchestrator always comes from the claim payload, so a repo
    // `lead.md` is just another subagent candidate. Detection must NOT drop it; the
    // guard against it reaching the main-thread prompt is M3 routing (see below).
    writeAgent("lead.md", "---\nname: lead\ndescription: the repo's own lead.\n---\n\nrepo lead body\n");
    writeAgent("coder.md", CODER);
    const { agents } = await detectRepoAgents(clone);
    assert.deepEqual(agents.map((a) => a.name), ["coder", "lead"]);
  });

  it("never follows a symlinked agents dir or agent file", async () => {
    const outside = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-outside-"));
    fs.writeFileSync(path.join(outside, "leak.md"), "---\nname: leaked\ndescription: secret.\n---\n\nbody\n");
    try {
      writeAgent("legit.md", "---\nname: legit\ndescription: ok.\n---\n\nbody\n");
      fs.symlinkSync(path.join(outside, "leak.md"), path.join(repoAgentsDir(clone), "linked.md"));
      const { agents } = await detectRepoAgents(clone);
      assert.deepEqual(agents.map((a) => a.name), ["legit"], "a symlinked *.md is never read");

      // …and the whole dir being a symlink yields nothing at all.
      const other = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-clone2-"));
      fs.mkdirSync(path.join(other, ".claude"), { recursive: true });
      fs.symlinkSync(outside, path.join(other, ".claude", "agents"));
      assert.deepEqual((await detectRepoAgents(other)).agents, []);
      fs.rmSync(other, { recursive: true, force: true });
    } finally {
      fs.rmSync(outside, { recursive: true, force: true });
    }
  });

  it("sanitizes a filename's bidi/format characters out of a note's name", async () => {
    // An unparseable file whose NAME carries a bidi override: the note must not leak
    // it into the run stream. safeLabel restricts to [A-Za-z0-9._-].
    writeAgent("ev\u202eil.md", "not a frontmatter file\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents, []);
    assert.equal(notes.length, 1);
    assert.equal(notes[0]!.name, "evil");
    assert.ok(!notes[0]!.name.includes("\u202e"));
  });

  it("ignores non-markdown entries and subdirectories", async () => {
    writeAgent("coder.md", CODER);
    writeAgent("notes.txt", "---\nname: notes\ndescription: x.\n---\n\nbody\n");
    fs.mkdirSync(path.join(repoAgentsDir(clone), "nested.md"));
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents.map((a) => a.name), ["coder"]);
    assert.deepEqual(notes, []);
  });

  it("summarizes to names + descriptions only — prompt bodies stay worker-side", async () => {
    writeAgent("coder.md", CODER);
    const { agents } = await detectRepoAgents(clone);
    const summaries = repoAgentSummaries(agents);
    assert.deepEqual(summaries, [{ name: "coder", description: "Implements features and fixes bugs." }]);
    assert.ok(!JSON.stringify(summaries).includes("Implement the requested change"));
  });
});

describe("repo agents are structurally denied Agent by the assembly path", () => {
  // The load-bearing guarantee is NOT the parser strip — it is agents.ts setting
  // disallowedTools on every subagent. These tests pin that a repo-shaped template
  // (routed through the normal subagent assembly M3 uses) can never spawn nested
  // agents, EVEN when it declares no tools (inherit-all).
  it("a repo agent with no `tools:` key is still denied Agent", () => {
    const { subagents } = assembleAgents([{ name: "researcher", description: "x.", prompt_body: "do research" }]);
    assert.ok(subagents.researcher, "assembled as a subagent");
    assert.equal(subagents.researcher!.tools, undefined, "inherits all tools");
    assert.ok(subagents.researcher!.disallowedTools?.includes("Agent"), "Agent denied structurally");
  });

  it("documents the M3 trap: assembleAgents hoists a `lead`-named template to the MAIN THREAD", () => {
    // This is exactly what M3 must NOT do with the repo roster: a `lead`-named repo
    // file fed through assembleAgents becomes the main-thread system prompt — the
    // repo-authored prompt injection settingSources:[] exists to prevent.
    const { subagents, leadSystemPrompt } = assembleAgents([
      { name: "lead", description: "x.", prompt_body: "REPO-AUTHORED SYSTEM PROMPT" },
    ]);
    assert.equal(subagents.lead, undefined, "a lead-named template is NOT an invokable subagent");
    assert.equal(leadSystemPrompt, "REPO-AUTHORED SYSTEM PROMPT", "it becomes the main-thread prompt");
  });
});

describe("repo agents: committed fixture roster", { skip: !HAS_PROCFS ? "reads procfs (Linux only)" : realProcfsSkip("repoagents: committed fixture roster") }, () => {
  // A committed corpus of REAL, hand-authored role frontmatter that this test OWNS.
  //
  // detectRepoAgents is a PRODUCT function: it parses agents out of a USER'S cloned
  // repo (runner.ts calls it with the worktree path). It used to be pointed at uzi's
  // OWN `.claude/agents/` — the repo's dev-team roster, which CLAUDE.md declares
  // "decoupled — it is free to drift and product changes must never touch it". That
  // was a layering violation (#62): a dev-team member authoring a malformed or
  // denied-tool role file reddened a product parser test. The fix keeps the value of
  // reading genuine hand-authored frontmatter — the files below are verbatim copies
  // of real role files — while freezing them into a fixture the test controls, so no
  // roster change anywhere can touch this test.
  //
  // The fixture is chosen to exercise the real-world shapes: an inherit-all role
  // (`coder`, no `tools:` key), a role declaring WebFetch/WebSearch alongside
  // unknown-to-SDK team tools that are kept but silently unavailable
  // (`researcher`: TaskUpdate/TaskList/… ), and a normal declared allowlist
  // (`reviewer`). The role-specific parsing rules themselves are proven above
  // against controlled fixtures; this block proves those rules hold against what a
  // human actually writes.
  //
  // This is NOT an appear/vanish guard on any roster — a role appearing or vanishing
  // is not this test's business. The dev-team/product parity signal belongs to #63's
  // nudge, with an actionable message rather than an array diff.
  const fixtureRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "fixtures", "repoagents");

  it("parses every fixture agent file cleanly", async () => {
    const agentsDir = repoAgentsDir(fixtureRoot);
    assert.ok(fs.existsSync(agentsDir), `committed fixture missing: ${agentsDir}`);

    const onDisk = fs.readdirSync(agentsDir).filter((f) => f.endsWith(".md"));
    assert.ok(onDisk.length > 0, `${agentsDir} has no .md files — the fixture this test reads is gone`);

    const { agents, notes } = await detectRepoAgents(fixtureRoot);

    // EVERY fixture file yields exactly one agent: the real canary that a file the
    // parser chokes on would be missing here, whatever it is called. The count is
    // read from the directory so adding a fixture file never needs a magic number.
    assert.equal(agents.length, onDisk.length, `parsed ${agents.length} agents from ${onDisk.length} files in ${agentsDir}`);

    // Nothing stripped, skipped, duplicated, or over-cap. This single assertion is
    // also what proves WebFetch/WebSearch are honored for these files WITHOUT
    // naming a role: a stripped tool would surface here as a `tools_filtered` note.
    assert.deepEqual(notes, [], JSON.stringify(notes));

    // Properties that hold for any roster, of any size.
    assert.ok(agents.every((a) => a.description.length > 0 && a.prompt_body.trim().length > 0));
    assert.ok(agents.every((a) => !(a.tools ?? []).some((tool) => REPO_AGENT_DENIED_TOOLS.includes(tool))));

    // The fixture is only meaningful if it actually carries the real-world shapes it
    // was chosen for: an inherit-all role, and WebFetch/WebSearch surviving on real
    // hand-authored frontmatter.
    assert.ok(agents.some((a) => a.tools === undefined), "fixture exercises an inherit-all role (no tools: key)");
    assert.ok(
      agents.some((a) => (a.tools ?? []).includes("WebFetch") && (a.tools ?? []).includes("WebSearch")),
      "fixture exercises WebFetch/WebSearch survival",
    );
  });
});

describe("parseAgentSelection", () => {
  it("round-trips an encoded selection", () => {
    const sel = { source: "repo" as const, exclusions: ["tester", "web-ux"] };
    assert.deepEqual(parseAgentSelection(encodeAgentSelection(sel)), { status: "ok", selection: sel });
  });

  it("treats a missing or empty exclusions list as none", () => {
    for (const raw of ['{"source":"own"}', '{"source":"own","exclusions":null}', '{"source":"own","exclusions":[]}']) {
      assert.deepEqual(parseAgentSelection(raw), { status: "ok", selection: { source: "own", exclusions: [] } });
    }
  });

  it("dedupes and trims exclusion names", () => {
    assert.deepEqual(parseAgentSelection('{"source":"repo","exclusions":[" coder ","coder"]}'), {
      status: "ok",
      selection: { source: "repo", exclusions: ["coder"] },
    });
  });

  it("reports ABSENT for an absent/blank body — never invalid", () => {
    for (const raw of [undefined, null, "", "   "]) {
      assert.deepEqual(parseAgentSelection(raw as string | null | undefined), { status: "absent" }, `absent: ${String(raw)}`);
    }
  });

  it("reports INVALID for a body that was sent but is malformed", () => {
    const bad = [
      "not json",
      "[]",
      "null",
      '"repo"',
      "{}",
      '{"source":"REPO"}',
      '{"source":"other"}',
      '{"source":"repo","exclusions":"coder"}', // not an array
      '{"source":"repo","exclusions":[1]}', // not strings
      '{"source":"repo","exclusions":["Not Kebab"]}',
      '{"source":"repo","exclusions":["../etc"]}',
      `{"source":"repo","exclusions":["${"a".repeat(65)}"]}`,
      `{"source":"repo","exclusions":[${Array.from({ length: AGENT_EXCLUSIONS_MAX + 1 }, (_, i) => `"a${i}"`).join(",")}]}`,
    ];
    for (const raw of bad) {
      assert.deepEqual(parseAgentSelection(raw), { status: "invalid" }, `should be invalid: ${raw}`);
    }
  });

  it("ignores unknown keys rather than rejecting the body", () => {
    assert.deepEqual(parseAgentSelection('{"source":"own","exclusions":[],"extra":"ignored"}'), {
      status: "ok",
      selection: { source: "own", exclusions: [] },
    });
  });
});

describe("resolveAgentSelection — the fallback never resolves toward the untrusted repo source", () => {
  it("passes an ok selection through unchanged", () => {
    const sel = { source: "repo" as const, exclusions: ["tester"] };
    assert.deepEqual(resolveAgentSelection({ status: "ok", selection: sel }, true), { selection: sel });
  });

  it("falls back to `own` (with a note) on an ok `repo` selection against a clone with no roster (issue #231)", () => {
    const sel = { source: "repo" as const, exclusions: ["tester"] };
    const resolved = resolveAgentSelection({ status: "ok", selection: sel }, /* repoAvailable */ false);
    assert.equal(resolved.selection.source, "own");
    assert.deepEqual(resolved.selection.exclusions, []);
    assert.ok(resolved.note && resolved.note.length > 0, "a note explains the fallback");
  });

  it("leaves an ok `own` selection alone even when the clone has no roster", () => {
    const sel = { source: "own" as const, exclusions: ["tester"] };
    assert.deepEqual(resolveAgentSelection({ status: "ok", selection: sel }, false), { selection: sel });
  });

  it("forces `own` (with a note) on a malformed body, even when repo agents exist", () => {
    const resolved = resolveAgentSelection({ status: "invalid" }, /* repoAvailable */ true);
    assert.equal(resolved.selection.source, "own");
    assert.deepEqual(resolved.selection.exclusions, []);
    assert.ok(resolved.note && resolved.note.length > 0, "a note explains the fallback");
  });

  it("uses the run default on an absent body: repo when detected, own otherwise", () => {
    assert.deepEqual(resolveAgentSelection({ status: "absent" }, true), {
      selection: { source: "repo", exclusions: [] },
    });
    assert.deepEqual(resolveAgentSelection({ status: "absent" }, false), {
      selection: { source: "own", exclusions: [] },
    });
  });
});

describe("resolveAgentSelection — the requested source (PRD #3914)", () => {
  const own = { source: "own" as const, exclusions: [] };
  const repo = { source: "repo" as const, exclusions: [] };
  const noRoster = "this clone has no repo agent roster; using your own agent templates";
  const cases: [string, AgentSelectionParse, boolean, unknown, { selection: AgentSelection; note?: string }][] = [
    ["absent, unset, roster", { status: "absent" }, true, undefined, { selection: repo }],
    ["absent, unset, no roster", { status: "absent" }, false, undefined, { selection: own }],
    ["absent, null, roster", { status: "absent" }, true, null, { selection: repo }],
    ["absent, own, roster", { status: "absent" }, true, "own", { selection: own }],
    ["absent, own, no roster", { status: "absent" }, false, "own", { selection: own }],
    ["absent, repo, roster", { status: "absent" }, true, "repo", { selection: repo }],
    ["absent, repo, no roster", { status: "absent" }, false, "repo", { selection: own, note: noRoster }],
    ["absent, unknown value, roster", { status: "absent" }, true, "both", { selection: repo }],
    ["absent, unknown value, no roster", { status: "absent" }, false, "both", { selection: own }],
    ["ok own wins over repo", { status: "ok", selection: { source: "own", exclusions: ["x"] } }, true, "repo", { selection: { source: "own", exclusions: ["x"] } }],
    ["ok repo wins over own", { status: "ok", selection: { source: "repo", exclusions: ["x"] } }, true, "own", { selection: { source: "repo", exclusions: ["x"] } }],
    ["ok repo, no roster, own requested", { status: "ok", selection: { source: "repo", exclusions: ["x"] } }, false, "own", { selection: own, note: noRoster }],
    ["invalid ignores a repo request", { status: "invalid" }, true, "repo", {
      selection: own, note: "the submitted agent selection was malformed; using your own agent templates" }],
  ];
  for (const [name, parse, roster, requested, want] of cases) {
    it(name, () => {
      assert.deepEqual(resolveAgentSelection(parse, roster, requested as AgentSource | null | undefined), want);
    });
  }

  for (const c of REQUESTED_SOURCE_TABLE) {
    it(`shared call-site table: ${caseLabel(c)}`, () => {
      const resolved = resolveAgentSelection(seedAgentSelection(undefined), c.roster, c.requested);
      assert.equal(resolved.selection.source, c.want);
      assert.equal(resolved.note === NO_ROSTER_NOTE, c.note);
    });
  }
});

describe("planAgentSource (PRD #3914 M3)", () => {
  const sources: (AgentSource | null | undefined)[] = [undefined, null, "own", "repo"];
  for (const persisted of sources) {
    for (const requested of sources) {
      for (const roster of [true, false]) {
        const want: AgentSource = requested === "repo" && roster && persisted !== "own" ? "repo" : "own";
        it(`persisted=${String(persisted)} requested=${String(requested)} roster=${roster} plans with ${want}`, () => {
          assert.equal(planAgentSource(persisted, requested, roster), want);
        });
      }
    }
  }

  it("an unset request with a persisted repo source from autopilot plans with owner templates", () => {
    assert.equal(planAgentSource("repo", undefined, true), "own");
    assert.equal(planAgentSource("repo", null, true), "own");
  });

  it("a persisted own source narrows a requested repo source to owner templates", () => {
    assert.equal(planAgentSource("own", "repo", true), "own");
  });

  it("an unknown persisted or requested value never widens to repo", () => {
    assert.equal(planAgentSource("both" as unknown as AgentSource, "repo", true), "own");
    assert.equal(planAgentSource(undefined, "both" as unknown as AgentSource, true), "own");
  });
});

describe("seedAgentSelection (issue #1718)", () => {
  it("seeds ok from a persisted selection, absent otherwise (never invalid)", () => {
    const sel = { source: "repo" as const, exclusions: ["x"] };
    assert.deepEqual(seedAgentSelection(sel), { status: "ok", selection: sel });
    assert.deepEqual(seedAgentSelection(undefined), { status: "absent" });
    assert.deepEqual(seedAgentSelection(null), { status: "absent" });
    // The absent seed resolves exactly as a gate-less run should.
    assert.deepEqual(resolveAgentSelection(seedAgentSelection(undefined), true).selection.source, "repo");
  });
});
