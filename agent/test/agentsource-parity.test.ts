import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { detectRepoAgents, repoAgentsDir, type RepoAgentNote } from "../src/repoagents.js";

// The TS half of the PRD #602 M3 differential test (architect N4). It runs the
// WORKER's repoagents.ts parser over the SAME committed corpus + hand-authored
// golden that api/internal/agentsource/parser_test.go asserts against. The two
// tests asserting one golden — neither snapshotted from a parser — is what proves
// the Go parser and repoagents.ts agree on the contract.
//
// The corpus and golden live beside the Go parser (they are shared, not duplicated),
// reached by a path relative to this file. repoagents.ts does not export
// parseAgentFile, so each fixture is driven through the exported detectRepoAgents by
// dropping it as the SOLE file in a throwaway `<clone>/.claude/agents/` — one
// under-cap, non-symlinked file, so detectRepoAgents is a thin wrapper over
// parseAgentFile there and its result maps 1:1 to the file's parse outcome.
const fixtureDir = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
  "..",
  "api",
  "internal",
  "agentsource",
  "testdata",
  "parity",
);

/** The shared golden, keyed by fixture filename. */
const golden = JSON.parse(fs.readFileSync(path.join(fixtureDir, "expected.json"), "utf8")) as Record<
  string,
  Record<string, unknown>
>;

/** Reduce a note to the golden's shape: reason + name, plus tools only when present. */
function noteShape(note: RepoAgentNote): Record<string, unknown> {
  return note.tools ? { reason: note.reason, name: note.name, tools: note.tools } : { reason: note.reason, name: note.name };
}

describe("agentsource parser parity (repoagents.ts ↔ api/internal/agentsource)", () => {
  const mdFiles = fs.readdirSync(fixtureDir).filter((f) => f.endsWith(".md"));
  assert.ok(mdFiles.length > 0, `parity corpus is empty: ${fixtureDir}`);

  for (const file of mdFiles) {
    it(`matches the golden for ${file}`, async () => {
      const want = golden[file];
      assert.ok(want, `no golden entry for fixture ${file}`);

      const clone = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-agentsource-"));
      try {
        const agentsDir = repoAgentsDir(clone);
        fs.mkdirSync(agentsDir, { recursive: true });
        // copyFileSync preserves bytes exactly (BOM / CRLF / bidi / zero-width).
        fs.copyFileSync(path.join(fixtureDir, file), path.join(agentsDir, file));

        const { agents, notes } = await detectRepoAgents(clone);

        let actual: Record<string, unknown>;
        if (agents.length === 1) {
          const a = agents[0]!;
          actual = {
            ok: true,
            name: a.name,
            description: a.description,
            prompt_body: a.prompt_body,
            tools: a.tools ?? null,
            model: a.model ?? "",
            notes: notes.map(noteShape),
          };
        } else {
          assert.equal(agents.length, 0, `fixture ${file} produced ${agents.length} agents`);
          assert.equal(notes.length, 1, `a skipped fixture must yield exactly one note: ${JSON.stringify(notes)}`);
          actual = { ok: false, skip: notes[0]!.reason, name: notes[0]!.name };
        }

        assert.deepEqual(actual, want);
      } finally {
        fs.rmSync(clone, { recursive: true, force: true });
      }
    });
  }
});

// Set-level differential (PRD #602 M3a). The per-fixture loop above drives ONE file
// through detectRepoAgents, so it exercises parseAgentFile but not the multi-file
// path: filename-order sort, the first-wins dedupe, and the file cap. These cases
// drive the REAL multi-file detectRepoAgents and are mirrored by Go ParseSet
// assertions in api/internal/agentsource/parser_test.go (TestParseSetCapsAndDedupe),
// so the set-level contract is pinned on both sides too. (too_large stays Go-only —
// sharing it would mean committing a >64KB fixture — see that test's comment.)
describe("agentsource parser parity — set level (detectRepoAgents ↔ ParseSet)", () => {
  const validFile = (name: string, body: string): string =>
    `---\nname: ${name}\ndescription: ok.\n---\n\n${body}\n`;

  /** Drop the given files into a throwaway clone and run detectRepoAgents over them. */
  async function detectOver(files: Record<string, string>) {
    const clone = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-agentsource-set-"));
    try {
      const agentsDir = repoAgentsDir(clone);
      fs.mkdirSync(agentsDir, { recursive: true });
      for (const [name, contents] of Object.entries(files)) {
        fs.writeFileSync(path.join(agentsDir, name), contents);
      }
      return await detectRepoAgents(clone);
    } finally {
      fs.rmSync(clone, { recursive: true, force: true });
    }
  }

  it("duplicate name: first file by name wins, later one is a duplicate note", async () => {
    const { agents, notes } = await detectOver({
      "z-second.md": validFile("dup", "loser"),
      "a-first.md": validFile("dup", "winner"),
    });
    assert.equal(agents.length, 1, `agents = ${JSON.stringify(agents)}`);
    assert.equal(agents[0]!.name, "dup");
    assert.equal(agents[0]!.prompt_body, "winner\n");
    assert.deepEqual(
      notes.map(noteShape),
      [{ reason: "duplicate", name: "dup" }],
    );
  });

  it("over the file cap: only the first 16 by name are considered, one aggregated over_limit note", async () => {
    const files: Record<string, string> = {};
    for (let i = 0; i < 17; i++) {
      const n = `a${String(i).padStart(2, "0")}`; // a00..a16
      files[`${n}.md`] = validFile(n, "body");
    }
    const { agents, notes } = await detectOver(files);
    assert.equal(agents.length, 16, `agents = ${agents.length}`);
    assert.equal(agents[0]!.name, "a00");
    assert.equal(agents[15]!.name, "a15");
    assert.deepEqual(
      notes.map((note) => ({ reason: note.reason, name: note.name, count: note.count })),
      [{ reason: "over_limit", name: "", count: 1 }],
    );
  });
});
