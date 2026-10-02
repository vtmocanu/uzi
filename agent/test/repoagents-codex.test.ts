import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { realProcfsSkip } from "./real-procfs.js";

const HAS_PROCFS = process.platform === "linux";
const repoAgentReadSkip = (label: string): string | false =>
  !HAS_PROCFS ? "reads procfs (Linux only)" : realProcfsSkip(`repoagents-codex: ${label}`);

import {
  REPO_AGENTS_MAX_FILES,
  describeRepoAgentNote,
  detectRepoAgents,
  repoAgentSummaries,
  type DetectedRepoAgents,
} from "../src/repoagents.js";

// Issue #2085: `.codex/agents/*.toml` as a second repo-agent source, plus the
// containment both loaders share.

let clone: string;
let outside: string;
beforeEach(() => {
  clone = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-repoagents-codex-"));
  outside = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-repoagents-outside-"));
});
afterEach(() => {
  fs.rmSync(clone, { recursive: true, force: true });
  fs.rmSync(outside, { recursive: true, force: true });
});

function write(root: string, folder: ".codex" | ".claude", file: string, content: string): void {
  const dir = path.join(root, folder, "agents");
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, file), content, "utf8");
}
const toml = (file: string, content: string) => write(clone, ".codex", file, content);
const md = (file: string, content: string) => write(clone, ".claude", file, content);

const TOML_OK = (name: string, extra = "") =>
  `name = "${name}"\ndescription = "Does ${name} things."\ndeveloper_instructions = "Follow the ${name} procedure."\n${extra}`;
const MD_OK = (name: string) => `---\nname: ${name}\ndescription: Does ${name} things.\n---\n\nBody of ${name}.\n`;

const names = (r: DetectedRepoAgents) => r.agents.map((a) => a.name);
const reasons = (r: DetectedRepoAgents) => r.notes.map((n) => n.reason);

describe("Codex TOML projection", { skip: repoAgentReadSkip("TOML projection") }, () => {
  it("projects exactly name, description and developer_instructions onto a template", async () => {
    toml("coder.toml", TOML_OK("coder"));
    const r = await detectRepoAgents(clone, "codex");
    assert.equal(r.folder, ".codex/agents");
    assert.deepEqual(r.notes, []);
    assert.deepEqual(r.agents, [
      { name: "coder", description: "Does coder things.", prompt_body: "Follow the coder procedure." },
    ]);
    assert.deepEqual(Object.keys(r.agents[0]!).sort(), ["description", "name", "prompt_body"]);
  });

  it("ignores model, effort, nicknames, config_file, includes and unknown keys", async () => {
    toml(
      "coder.toml",
      TOML_OK(
        "coder",
        [
          'model = "gpt-5"',
          'model_reasoning_effort = "high"',
          'nickname_candidates = ["a", "b"]',
          'config_file = "other.toml"',
          'includes = ["x"]',
          'mystery = "value"',
          "[mystery_table]",
          "k = 1",
        ].join("\n"),
      ),
    );
    const r = await detectRepoAgents(clone, "codex");
    assert.deepEqual(r.notes, []);
    const t = r.agents[0]!;
    assert.deepEqual(Object.keys(t).sort(), ["description", "name", "prompt_body"]);
    assert.equal(t.model, undefined);
    assert.equal(t.tools, undefined);
  });

  for (const key of ["features", "skills", "sandbox_mode", "tools"] as const) {
    it(`skips an agent declaring the ${key} restriction, with a note`, async () => {
      const extra = key === "sandbox_mode" ? `${key} = "read-only"\n` : key === "tools" ? `${key} = ["shell"]\n` : `[${key}]\nx = true\n`;
      // Top-level scalars must precede tables, so put the restriction after the fields.
      toml("coder.toml", TOML_OK("coder", extra));
      toml("other.toml", TOML_OK("other"));
      const r = await detectRepoAgents(clone, "codex");
      assert.deepEqual(names(r), ["other"]);
      assert.deepEqual(r.notes, [{ name: "coder", reason: "restriction_unsupported", restrictions: [key] }]);
      assert.equal(
        describeRepoAgentNote(r.notes[0]!),
        `repo agent "coder" was skipped: it declares a Codex restriction (${key}) uzi cannot honour yet`,
      );
    });
  }

  it("lists every restriction key present, from our own constants only", async () => {
    toml("coder.toml", TOML_OK("coder", 'sandbox_mode = "x"\ntools = []\n'));
    const r = await detectRepoAgents(clone, "codex");
    assert.deepEqual(r.notes[0]!.restrictions, ["sandbox_mode", "tools"]);
  });

  it("notes malformed TOML, duplicate keys and duplicate tables as invalid", async () => {
    toml("bad-syntax.toml", "name = \n");
    toml("dup-key.toml", 'name = "a"\nname = "b"\ndescription = "d"\ndeveloper_instructions = "x"\n');
    toml("dup-table.toml", TOML_OK("dt", "[t]\na = 1\n[t]\nb = 2\n"));
    const r = await detectRepoAgents(clone, "codex");
    assert.deepEqual(r.agents, []);
    assert.deepEqual(r.notes.map((n) => [n.name, n.reason]), [
      ["bad-syntax", "invalid"],
      ["dup-key", "invalid"],
      ["dup-table", "invalid"],
    ]);
  });

  it("skips an oversized file with too_large without parsing it", async () => {
    toml("big.toml", TOML_OK("big", `# ${"x".repeat(70 * 1024)}\n`));
    toml("ok.toml", TOML_OK("ok"));
    const r = await detectRepoAgents(clone, "codex");
    assert.deepEqual(names(r), ["ok"]);
    assert.deepEqual(r.notes, [{ name: "big", reason: "too_large" }]);
  });

  it("rejects a missing or non-string name (no filename fallback), description, or empty instructions", async () => {
    toml("no-name.toml", 'description = "d"\ndeveloper_instructions = "x"\n');
    toml("num-name.toml", 'name = 5\ndescription = "d"\ndeveloper_instructions = "x"\n');
    toml("bad-name.toml", 'name = "Not Kebab"\ndescription = "d"\ndeveloper_instructions = "x"\n');
    toml("no-desc.toml", 'name = "a"\ndeveloper_instructions = "x"\n');
    toml("num-desc.toml", 'name = "b"\ndescription = 3\ndeveloper_instructions = "x"\n');
    toml("empty-desc.toml", 'name = "c"\ndescription = "  "\ndeveloper_instructions = "x"\n');
    toml("bidi-desc.toml", 'name = "d"\ndescription = "ev\\u202eil"\ndeveloper_instructions = "x"\n');
    toml("no-instr.toml", 'name = "e"\ndescription = "d"\n');
    toml("blank-instr.toml", 'name = "f"\ndescription = "d"\ndeveloper_instructions = "  \\n "\n');
    toml("num-instr.toml", 'name = "g"\ndescription = "d"\ndeveloper_instructions = 1\n');
    const r = await detectRepoAgents(clone, "codex");
    assert.deepEqual(r.agents, []);
    assert.equal(r.notes.length, 10);
    assert.ok(r.notes.every((n) => n.reason === "invalid"));
  });

  it("trims name and description, caps description bytes, and keeps the first of duplicate names", async () => {
    toml("a.toml", 'name = "  twin "\ndescription = "  first  "\ndeveloper_instructions = "one"\n');
    toml("b.toml", 'name = "twin"\ndescription = "second"\ndeveloper_instructions = "two"\n');
    toml("long.toml", `name = "long"\ndescription = "${"é".repeat(600)}"\ndeveloper_instructions = "x"\n`);
    const r = await detectRepoAgents(clone, "codex");
    assert.deepEqual(r.agents.map((a) => [a.name, a.description]), [["twin", "first"]]);
    assert.deepEqual(r.notes.map((n) => [n.name, n.reason]), [["twin", "duplicate"], ["long", "invalid"]]);
  });

  it("applies the candidate cap to .toml files with one aggregated note", async () => {
    for (let i = 0; i < REPO_AGENTS_MAX_FILES + 4; i++) {
      const n = `agent-${String(i).padStart(2, "0")}`;
      toml(`${n}.toml`, TOML_OK(n));
    }
    toml("notes.md", "ignored");
    const r = await detectRepoAgents(clone, "codex");
    assert.equal(r.agents.length, REPO_AGENTS_MAX_FILES);
    assert.deepEqual(r.notes, [{ name: "", reason: "over_limit", count: 4 }]);
  });

  it("summaries carry the folder only when one is passed", async () => {
    toml("coder.toml", TOML_OK("coder"));
    const r = await detectRepoAgents(clone, "codex");
    assert.deepEqual(repoAgentSummaries(r.agents), [{ name: "coder", description: "Does coder things." }]);
    assert.ok(!("folder" in repoAgentSummaries(r.agents)[0]!));
    assert.deepEqual(repoAgentSummaries(r.agents, r.folder!), [
      { name: "coder", description: "Does coder things.", folder: ".codex/agents" },
    ]);
  });
});

describe("native-folder precedence", { skip: repoAgentReadSkip("native-folder precedence") }, () => {
  it("claude harness: only .claude, only .codex, both", async () => {
    md("m.md", MD_OK("m"));
    let r = await detectRepoAgents(clone, "claude");
    assert.deepEqual([names(r), r.folder], [["m"], ".claude/agents"]);

    fs.rmSync(path.join(clone, ".claude"), { recursive: true });
    toml("t.toml", TOML_OK("t"));
    r = await detectRepoAgents(clone, "claude");
    assert.deepEqual([names(r), r.folder], [["t"], ".codex/agents"]);

    md("m.md", MD_OK("m"));
    r = await detectRepoAgents(clone, "claude");
    assert.deepEqual([names(r), r.folder], [["m"], ".claude/agents"], "never merged");
  });

  it("codex harness: only .codex, only .claude, both", async () => {
    toml("t.toml", TOML_OK("t"));
    let r = await detectRepoAgents(clone, "codex");
    assert.deepEqual([names(r), r.folder], [["t"], ".codex/agents"]);

    fs.rmSync(path.join(clone, ".codex"), { recursive: true });
    md("m.md", MD_OK("m"));
    r = await detectRepoAgents(clone, "codex");
    assert.deepEqual([names(r), r.folder], [["m"], ".claude/agents"]);

    toml("t.toml", TOML_OK("t"));
    r = await detectRepoAgents(clone, "codex");
    assert.deepEqual([names(r), r.folder], [["t"], ".codex/agents"], "never merged");
  });

  it("neither folder present: empty, no notes, folder null", async () => {
    for (const h of ["claude", "codex"] as const) {
      assert.deepEqual(await detectRepoAgents(clone, h), { agents: [], notes: [], folder: null });
    }
  });

  it("a present-but-empty native folder does not fall back", async () => {
    fs.mkdirSync(path.join(clone, ".codex", "agents"), { recursive: true });
    md("m.md", MD_OK("m"));
    const r = await detectRepoAgents(clone, "codex");
    assert.deepEqual(r, { agents: [], notes: [], folder: ".codex/agents" });
    fs.rmSync(path.join(clone, ".codex"), { recursive: true });
    fs.mkdirSync(path.join(clone, ".claude", "agents"), { recursive: true });
    fs.rmSync(path.join(clone, ".claude", "agents", "m.md"));
    toml("t.toml", TOML_OK("t"));
    assert.deepEqual(await detectRepoAgents(clone, "claude"), { agents: [], notes: [], folder: ".claude/agents" });
  });

  it("an all-invalid native folder does not fall back", async () => {
    toml("bad.toml", "not toml at all = = =\n");
    md("m.md", MD_OK("m"));
    const r = await detectRepoAgents(clone, "codex");
    assert.deepEqual(r.agents, []);
    assert.deepEqual(reasons(r), ["invalid"]);
    assert.equal(r.folder, ".codex/agents");
  });

  it("a rejected (symlinked) native folder does not fall back", async () => {
    fs.mkdirSync(path.join(outside, "agents"), { recursive: true });
    fs.symlinkSync(outside, path.join(clone, ".codex"));
    md("m.md", MD_OK("m"));
    const r = await detectRepoAgents(clone, "codex");
    assert.deepEqual(r.agents, []);
    assert.deepEqual(r.notes, [{ name: "", reason: "unsafe_path", folder: ".codex/agents" }]);
    assert.equal(r.folder, ".codex/agents");
    assert.equal(
      describeRepoAgentNote(r.notes[0]!),
      "the repo's .codex/agents/ was not read: a path component is a symlink or escapes the clone",
    );
  });
});

describe("containment shared by both loaders", () => {
  const LEAK_TOML = TOML_OK("leaked");
  const LEAK_MD = MD_OK("leaked");

  function leakDir(): void {
    fs.mkdirSync(path.join(outside, "agents"), { recursive: true });
    fs.writeFileSync(path.join(outside, "agents", "leak.toml"), LEAK_TOML);
    fs.writeFileSync(path.join(outside, "agents", "leak.md"), LEAK_MD);
  }

  for (const [harness, folder] of [
    ["codex", ".codex"],
    ["claude", ".claude"],
  ] as const) {
    it(`rejects a symlinked ${folder} directory`, async () => {
      leakDir();
      fs.symlinkSync(outside, path.join(clone, folder));
      const r = await detectRepoAgents(clone, harness);
      assert.deepEqual(r.agents, []);
      assert.deepEqual(reasons(r), ["unsafe_path"]);
    });

    it(`rejects a symlinked ${folder}/agents directory`, async () => {
      leakDir();
      fs.mkdirSync(path.join(clone, folder));
      fs.symlinkSync(path.join(outside, "agents"), path.join(clone, folder, "agents"));
      const r = await detectRepoAgents(clone, harness);
      assert.deepEqual(r.agents, []);
      assert.deepEqual(reasons(r), ["unsafe_path"]);
    });

    it(`never reads a symlinked leaf file in ${folder}/agents`, { skip: repoAgentReadSkip(`symlinked leaf (${folder})`) }, async () => {
      leakDir();
      const ext = harness === "codex" ? "toml" : "md";
      (harness === "codex" ? toml : md)(`legit.${ext}`, harness === "codex" ? TOML_OK("legit") : MD_OK("legit"));
      fs.symlinkSync(path.join(outside, "agents", `leak.${ext}`), path.join(clone, folder, "agents", `linked.${ext}`));
      const r = await detectRepoAgents(clone, harness);
      assert.deepEqual(names(r), ["legit"]);
    });

    it(`refuses a file whose leaf is swapped for a symlink after discovery (${folder})`, async () => {
      leakDir();
      const ext = harness === "codex" ? "toml" : "md";
      (harness === "codex" ? toml : md)(`x.${ext}`, harness === "codex" ? TOML_OK("x") : MD_OK("x"));
      const r = await detectRepoAgents(clone, harness, {
        beforeOpen: async (full) => {
          fs.rmSync(full);
          fs.symlinkSync(path.join(outside, "agents", `leak.${ext}`), full);
        },
      });
      assert.deepEqual(r.agents, []);
      assert.deepEqual(r.notes, [{ name: "x", reason: "unsafe_path" }]);
      assert.equal(describeRepoAgentNote(r.notes[0]!), 'repo agent file "x" was skipped: its location could not be verified inside the agents folder');
    });

    it(`refuses a leaf swapped for a symlink to ANOTHER file in the same folder (${folder})`, { skip: repoAgentReadSkip(`same-folder leaf swap (${folder})`) }, async () => {
      const ext = harness === "codex" ? "toml" : "md";
      const put = harness === "codex" ? toml : md;
      put(`a-target.${ext}`, harness === "codex" ? TOML_OK("a-target") : MD_OK("a-target"));
      put(`x.${ext}`, harness === "codex" ? TOML_OK("x") : MD_OK("x"));
      // The link target sits directly in the agents folder, so only O_NOFOLLOW
      // (ELOOP on open) refuses it; the containment check alone would pass.
      const r = await detectRepoAgents(clone, harness, {
        beforeOpen: async (full) => {
          if (!full.endsWith(`x.${ext}`)) return;
          fs.rmSync(full);
          fs.symlinkSync(`a-target.${ext}`, full);
        },
      });
      assert.deepEqual(names(r), ["a-target"]);
      assert.deepEqual(r.notes, [{ name: "x", reason: "unsafe_path" }]);
    });

    it(`skips a leaf swapped for a FIFO without blocking (${folder})`, { skip: repoAgentReadSkip(`FIFO leaf swap (${folder})`) }, async () => {
      const ext = harness === "codex" ? "toml" : "md";
      (harness === "codex" ? toml : md)(`x.${ext}`, harness === "codex" ? TOML_OK("x") : MD_OK("x"));
      let fifoPath = "";
      const run = detectRepoAgents(clone, harness, {
        beforeOpen: async (full) => {
          fs.rmSync(full);
          try {
            execFileSync("mkfifo", [full], { stdio: "pipe" });
          } catch {
            return; // mkfifo unavailable (non-POSIX); the file stays a regular one
          }
          fifoPath = full;
        },
      });
      let timer: ReturnType<typeof setTimeout> | undefined;
      let timedOut = false;
      const guard = new Promise<never>((_, rej) => {
        timer = setTimeout(() => {
          timedOut = true;
          rej(new Error("detectRepoAgents blocked opening a FIFO"));
        }, 3000);
      });
      try {
        const r = await Promise.race([run, guard]);
        if (fifoPath === "") return;
        assert.deepEqual(r.agents, []);
        assert.deepEqual(r.notes, []);
      } finally {
        clearTimeout(timer);
        if (timedOut && fifoPath !== "") {
          // Release the parked open so the process can exit.
          const w = fs.openSync(fifoPath, fs.constants.O_WRONLY | fs.constants.O_NONBLOCK);
          fs.closeSync(w);
          await Promise.race([run.catch(() => undefined), new Promise((res) => setTimeout(res, 1000))]);
        }
      }
    });

    it(`notes an unreadable file as invalid (${folder})`, { skip: process.getuid?.() === 0 }, async () => {
      const ext = harness === "codex" ? "toml" : "md";
      (harness === "codex" ? toml : md)(`x.${ext}`, harness === "codex" ? TOML_OK("x") : MD_OK("x"));
      const file = path.join(clone, folder, "agents", `x.${ext}`);
      fs.chmodSync(file, 0o000);
      try {
        const r = await detectRepoAgents(clone, harness);
        assert.deepEqual(r.agents, []);
        assert.deepEqual(r.notes, [{ name: "x", reason: "invalid" }]);
      } finally {
        fs.chmodSync(file, 0o644);
      }
    });

    it(`stays silent for a file that vanishes between discovery and open (${folder})`, async () => {
      const ext = harness === "codex" ? "toml" : "md";
      (harness === "codex" ? toml : md)(`x.${ext}`, harness === "codex" ? TOML_OK("x") : MD_OK("x"));
      const r = await detectRepoAgents(clone, harness, { beforeOpen: async (full) => fs.rmSync(full) });
      assert.deepEqual(r.agents, []);
      assert.deepEqual(r.notes, []);
    });

    it(`refuses a file when ${folder} is swapped for a symlink between discovery and open`, async () => {
      const ext = harness === "codex" ? "toml" : "md";
      const write1 = harness === "codex" ? toml : md;
      // The in-repo file is invalid; the outside same-named file is a VALID agent, so a
      // read through the swapped ancestor would surface "leaked".
      write1(`same.${ext}`, "garbage");
      leakDir();
      fs.copyFileSync(path.join(outside, "agents", `leak.${ext}`), path.join(outside, "agents", `same.${ext}`));
      let swapped = false;
      const r = await detectRepoAgents(clone, harness, {
        beforeOpen: async () => {
          if (swapped) return;
          swapped = true;
          fs.renameSync(path.join(clone, folder), path.join(clone, `${folder}-real`));
          fs.symlinkSync(outside, path.join(clone, folder));
        },
      });
      assert.ok(swapped);
      assert.deepEqual(r.agents, [], "the outside file's content was never parsed");
      assert.deepEqual(r.notes, [{ name: "same", reason: "unsafe_path" }]);
    });
  }

  describe("injected handle-path resolver (fail closed)", () => {
    function setup(): string {
      toml("x.toml", TOML_OK("x"));
      return fs.realpathSync(path.join(clone, ".codex", "agents"));
    }

    it("accepts a file whose handle resolves directly inside the agents folder", async () => {
      const agentsReal = setup();
      const r = await detectRepoAgents(clone, "codex", { resolveHandlePath: async () => path.join(agentsReal, "x.toml") });
      assert.deepEqual(names(r), ["x"]);
    });

    it("skips when the resolver throws", async () => {
      setup();
      const r = await detectRepoAgents(clone, "codex", {
        resolveHandlePath: async () => {
          throw new Error("no procfs");
        },
      });
      assert.deepEqual(r.agents, []);
      assert.deepEqual(r.notes, [{ name: "x", reason: "unsafe_path" }]);
    });

    it("skips when the resolver returns an empty string", async () => {
      setup();
      const r = await detectRepoAgents(clone, "codex", { resolveHandlePath: async () => "" });
      assert.deepEqual(reasons(r), ["unsafe_path"]);
    });

    it("skips when the resolver returns a path outside the folder", async () => {
      setup();
      const r = await detectRepoAgents(clone, "codex", { resolveHandlePath: async () => path.join(outside, "x.toml") });
      assert.deepEqual(r.agents, []);
      assert.deepEqual(reasons(r), ["unsafe_path"]);
    });

    it("skips when the resolver returns a file in a different parent (parent, child dir, prefix sibling)", async () => {
      const agentsReal = setup();
      for (const p of [
        path.join(path.dirname(agentsReal), "x.toml"),
        path.join(agentsReal, "sub", "x.toml"),
        `${agentsReal}-evil${path.sep}x.toml`,
      ]) {
        const r = await detectRepoAgents(clone, "codex", { resolveHandlePath: async () => p });
        assert.deepEqual(r.agents, [], p);
        assert.deepEqual(reasons(r), ["unsafe_path"], p);
      }
    });
  });

  // One real check of the default resolver; the rest of the suite injects.
  it("default resolver: accepts a real file and refuses a real ancestor swap", { skip: repoAgentReadSkip("default resolver") }, async () => {
    toml("x.toml", TOML_OK("x"));
    const ok = await detectRepoAgents(clone, "codex");
    assert.deepEqual(names(ok), ["x"]);

    fs.mkdirSync(path.join(outside, "agents"), { recursive: true });
    fs.writeFileSync(path.join(outside, "agents", "x.toml"), TOML_OK("leaked"));
    const swapped = await detectRepoAgents(clone, "codex", {
      beforeOpen: async () => {
        fs.renameSync(path.join(clone, ".codex"), path.join(clone, ".codex-real"));
        fs.symlinkSync(outside, path.join(clone, ".codex"));
      },
    });
    assert.deepEqual(swapped.agents, []);
    assert.deepEqual(swapped.notes, [{ name: "x", reason: "unsafe_path" }]);
  });
});
