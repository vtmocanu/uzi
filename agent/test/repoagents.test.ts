import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import {
  REPO_AGENTS_MAX_FILES,
  REPO_AGENT_DENIED_TOOLS,
  describeRepoAgentNote,
  detectRepoAgents,
  repoAgentSummaries,
  repoAgentsDir,
  type RepoAgentNoteReason,
} from "../src/repoagents.js";
import { MODEL_ALIASES } from "../src/models.js";
import { AGENT_EXCLUSIONS_MAX, encodeAgentSelection, parseAgentSelection } from "../src/protocol.js";

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

describe("detectRepoAgents", () => {
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

  it("parses the inline-bracket and block-sequence forms of tools", async () => {
    writeAgent("bracket.md", "---\nname: bracket\ndescription: x.\ntools: [Bash, Read]\n---\n\nbody\n");
    writeAgent("block.md", "---\nname: block\ndescription: x.\ntools:\n  - Bash\n  - Grep\nmodel: sonnet\n---\n\nbody\n");
    const { agents } = await detectRepoAgents(clone);
    assert.deepEqual(agents.find((a) => a.name === "bracket")!.tools, ["Bash", "Read"]);
    const block = agents.find((a) => a.name === "block")!;
    assert.deepEqual(block.tools, ["Bash", "Grep"]);
    // The key that follows the block sequence still parses (the sequence ended).
    assert.equal(block.model, "sonnet");
  });

  it("names an agent after its filename slug when the frontmatter omits name", async () => {
    writeAgent("spec-keeper.md", "---\ndescription: Keeps specs in sync.\n---\n\nKeep specs in sync.\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(notes, []);
    assert.equal(agents[0]!.name, "spec-keeper");
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

  it("drops an oversized file without reading it", async () => {
    writeAgent("big.md", "---\nname: big\ndescription: x.\n---\n\n" + "x".repeat(64 * 1024 + 1));
    writeAgent("small.md", CODER);
    const { agents, notes } = await detectRepoAgents(clone);
    assert.deepEqual(agents.map((a) => a.name), ["coder"]);
    assert.deepEqual(notes, [{ name: "big", reason: "too_large" }]);
  });

  it("caps the roster at 16 files, in filename order", async () => {
    // 18 files: a00…a17. The first 16 by filename are kept.
    for (let i = 0; i < 18; i++) {
      const n = `a${String(i).padStart(2, "0")}`;
      writeAgent(`${n}.md`, `---\nname: ${n}\ndescription: agent ${i}.\n---\n\nbody\n`);
    }
    const { agents, notes } = await detectRepoAgents(clone);
    assert.equal(agents.length, REPO_AGENTS_MAX_FILES);
    assert.equal(agents.at(-1)!.name, "a15");
    assert.deepEqual(notes, [
      { name: "a16", reason: "over_limit" },
      { name: "a17", reason: "over_limit" },
    ]);
  });

  it("dedupes on name, first file (by filename) wins", async () => {
    writeAgent("a-first.md", "---\nname: dup\ndescription: the winner.\n---\n\nfirst\n");
    writeAgent("z-second.md", "---\nname: dup\ndescription: the loser.\n---\n\nsecond\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.equal(agents.length, 1);
    assert.equal(agents[0]!.description, "the winner.");
    assert.deepEqual(notes, [{ name: "dup", reason: "duplicate" }]);
  });

  it("strips every denylisted tool from a declared allowlist, keeping unknown names", async () => {
    writeAgent(
      "researcher.md",
      "---\nname: researcher\ndescription: x.\ntools: Read, Agent, WebFetch, WebSearch, ScheduleWakeup, CronCreate, SendMessage\n---\n\nbody\n",
    );
    const { agents, notes } = await detectRepoAgents(clone);
    // Agent (nested spawning), the deferral tools, and the network tools are gone.
    // SendMessage is unknown to the worker SDK — kept, silently unavailable.
    assert.deepEqual(agents[0]!.tools, ["Read", "SendMessage"]);
    for (const denied of REPO_AGENT_DENIED_TOOLS) {
      assert.ok(!agents[0]!.tools!.includes(denied), `${denied} must never survive`);
    }
    assert.deepEqual(reasons(notes), ["tools_filtered"]);
    // The note names exactly what was removed, in declaration order.
    assert.deepEqual(notes[0]!.tools, ["Agent", "WebFetch", "WebSearch", "ScheduleWakeup", "CronCreate"]);
  });

  it("skips an agent whose declared tools are ALL denied, rather than granting inherit-all", async () => {
    // The trap: an empty `tools` array is read as inherit-all (agents.ts), so an
    // agent that asked for nothing but WebFetch/Agent must be dropped, not widened.
    writeAgent("net.md", "---\nname: net\ndescription: x.\ntools: WebFetch, WebSearch\n---\n\nbody\n");
    writeAgent("spawner.md", "---\nname: spawner\ndescription: x.\ntools: [Agent]\n---\n\nbody\n");
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

  it("honors an alias model and ignores anything else", async () => {
    writeAgent("aliased.md", "---\nname: aliased\ndescription: x.\nmodel: haiku\n---\n\nbody\n");
    writeAgent("custom.md", "---\nname: custom\ndescription: x.\nmodel: claude-opus-4-8-20990101\n---\n\nbody\n");
    writeAgent("typo.md", "---\nname: typo\ndescription: x.\nmodel: opusss\n---\n\nbody\n");
    const { agents, notes } = await detectRepoAgents(clone);
    assert.equal(agents.find((a) => a.name === "aliased")!.model, "haiku");
    // A custom ID or a typo inherits the run default — it is never pinned.
    assert.equal(agents.find((a) => a.name === "custom")!.model, undefined);
    assert.equal(agents.find((a) => a.name === "typo")!.model, undefined);
    assert.deepEqual(reasons(notes), ["model_ignored", "model_ignored"]);
    // The rejected model string never reaches the run stream.
    assert.ok(!describeRepoAgentNote(notes[0]!).includes("claude-opus"));
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

describe("repo agents: uzi's own .claude/agents", () => {
  // The repo this worker package lives in ships the eight dev-team roles. Parsing
  // them is the acceptance check for M1: real files, real frontmatter, real tools.
  const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

  it("detects all eight dev-team agents with their tools filtered", async (t) => {
    if (!fs.existsSync(path.join(repoRoot, ".claude", "agents"))) return t.skip("not in a source checkout");
    const { agents, notes } = await detectRepoAgents(repoRoot);
    assert.deepEqual(
      agents.map((a) => a.name),
      ["auditor", "coder", "documenter", "fact-checker", "reviewer", "spec-keeper", "tester", "web-ux"],
    );
    assert.ok(agents.every((a) => a.description.length > 0 && a.prompt_body.trim().length > 0));
    // `coder` declares no tools (inherit-all); every other file declares an
    // allowlist including WebFetch, which repo agents never receive.
    assert.equal(agents.find((a) => a.name === "coder")!.tools, undefined);
    assert.ok(agents.every((a) => !(a.tools ?? []).some((tool) => REPO_AGENT_DENIED_TOOLS.includes(tool))));
    // The Claude Code team tools these files declare are unknown to the worker
    // SDK: kept in the allowlist, silently unavailable — not a drop, not an error.
    assert.ok(agents.find((a) => a.name === "reviewer")!.tools!.includes("SendMessage"));
    // Nothing was skipped; the only notes are the WebFetch/WebSearch strips.
    assert.ok(notes.every((n) => n.reason === "tools_filtered"), JSON.stringify(notes));
  });
});

describe("MODEL_ALIASES", () => {
  it("matches the web ModelSelect list (the two must not drift)", async (t) => {
    const modelSelect = path.resolve(
      path.dirname(fileURLToPath(import.meta.url)),
      "..",
      "..",
      "web/src/components/ModelSelect.tsx",
    );
    if (!fs.existsSync(modelSelect)) return t.skip("not in a source checkout");
    const src = fs.readFileSync(modelSelect, "utf8");
    const m = /export const MODEL_ALIASES = \[([^\]]*)\]/.exec(src);
    assert.ok(m, "could not find MODEL_ALIASES in ModelSelect.tsx");
    const web = m[1]!.split(",").map((s) => s.trim().replace(/^"|"$/g, "")).filter(Boolean);
    assert.deepEqual([...MODEL_ALIASES], web);
  });
});

describe("parseAgentSelection", () => {
  it("round-trips an encoded selection", () => {
    const sel = { source: "repo" as const, exclusions: ["tester", "web-ux"] };
    assert.deepEqual(parseAgentSelection(encodeAgentSelection(sel)), sel);
  });

  it("treats a missing or empty exclusions list as none", () => {
    assert.deepEqual(parseAgentSelection('{"source":"own"}'), { source: "own", exclusions: [] });
    assert.deepEqual(parseAgentSelection('{"source":"own","exclusions":null}'), { source: "own", exclusions: [] });
    assert.deepEqual(parseAgentSelection('{"source":"own","exclusions":[]}'), { source: "own", exclusions: [] });
  });

  it("dedupes and trims exclusion names", () => {
    assert.deepEqual(parseAgentSelection('{"source":"repo","exclusions":[" coder ","coder"]}'), {
      source: "repo",
      exclusions: ["coder"],
    });
  });

  it("returns undefined for anything that is not a well-formed selection", () => {
    const bad = [
      undefined,
      null,
      "",
      "   ",
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
      assert.equal(parseAgentSelection(raw as string | null | undefined), undefined, `should reject: ${String(raw)}`);
    }
  });

  it("ignores unknown keys rather than rejecting the body", () => {
    assert.deepEqual(parseAgentSelection('{"source":"own","exclusions":[],"extra":"ignored"}'), {
      source: "own",
      exclusions: [],
    });
  });
});
