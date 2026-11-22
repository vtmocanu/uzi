import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { parseCodexAgentFile } from "../src/repoagents-codex.js";
import { CODEX_ROLE_MODELS } from "../src/codex/render.js";
import { describeRepoAgentNote, filterRepoAgentModel } from "../src/repoagents.js";

const source = (extra = "") =>
  'name = "coder"\ndescription = "Works."\ndeveloper_instructions = "Implement."\n' + extra;

describe("Codex TOML model projection", () => {
  for (const model of CODEX_ROLE_MODELS) {
    it(`accepts supported pin ${model} with only validated template fields`, () => {
      const parsed = parseCodexAgentFile(source(`model = "${model}"\nmodel_reasoning_effort = "high"\nconfig_file = "outside"\nnickname_candidates = ["x"]\nunknown = true`), "file");
      assert.equal(parsed.ok, true);
      if (!parsed.ok) return;
      assert.deepEqual(parsed, { ok: true, template: {
        name: "coder", description: "Works.", prompt_body: "Implement.", model, effort: "high",
      }, notes: [] });
      Object.freeze(parsed.template);
      const codex = filterRepoAgentModel(parsed.template, "codex");
      const claude = filterRepoAgentModel(parsed.template, "claude");
      assert.equal(codex.model, model);
      assert.equal(Object.hasOwn(claude, "model"), false);
      // The model projection leaves effort alone; the family-by-folder drop is projectForHarness's.
      assert.deepEqual(claude, { name: "coder", description: "Works.", prompt_body: "Implement.", effort: "high" });
      assert.equal(parsed.template.model, model);
      assert.notEqual(codex, parsed.template);
    });
  }

  for (const extra of ["", 'model = ""']) {
    it(`inherits with ${extra || "absent model"}`, () => {
      const parsed = parseCodexAgentFile(source(extra), "file");
      assert.equal(parsed.ok, true);
      if (!parsed.ok) return;
      assert.equal(Object.hasOwn(parsed.template, "model"), false);
      for (const harness of ["codex", "claude"] as const) {
        assert.deepEqual(filterRepoAgentModel(parsed.template, harness), parsed.template);
      }
    });
  }

  for (const level of ["low", "medium", "high", "xhigh", "max"]) {
    it(`keeps model_reasoning_effort ${level} as the template effort`, () => {
      const parsed = parseCodexAgentFile(source(`model_reasoning_effort = "${level}"`), "file");
      assert.deepEqual(parsed, { ok: true, template: {
        name: "coder", description: "Works.", prompt_body: "Implement.", effort: level,
      }, notes: [] });
    });
  }

  for (const literal of ['"HIGH"', '"runtime"', '"ultra"', '" high"', '"high\\n"', "3", "true", '["high"]']) {
    it(`drops an invalid model_reasoning_effort ${literal} with a note that never echoes it`, () => {
      const parsed = parseCodexAgentFile(source(`model_reasoning_effort = ${literal}`), "file");
      assert.deepEqual(parsed, { ok: true, template: {
        name: "coder", description: "Works.", prompt_body: "Implement.",
      }, notes: [{ name: "coder", reason: "effort_ignored" }] });
      if (!parsed.ok) return;
      const text = describeRepoAgentNote(parsed.notes[0]!);
      assert.equal(text, 'repo agent "coder" declared an invalid effort; it follows the run\'s effort');
    });
  }

  it("treats an empty model_reasoning_effort as inherit, without a note", () => {
    const parsed = parseCodexAgentFile(source('model_reasoning_effort = ""'), "file");
    assert.deepEqual(parsed, { ok: true, template: {
      name: "coder", description: "Works.", prompt_body: "Implement.",
    }, notes: [] });
  });

  it("describes an effort note with its file clause only when a file is present", () => {
    assert.equal(
      describeRepoAgentNote({ name: "coder", reason: "effort_ignored", file: "coder-file" }),
      'repo agent "coder" (file "coder-file") declared an invalid effort; it follows the run\'s effort',
    );
  });

  const hostile: { literal: string; raw?: string }[] = [
    ...["1", "1.5", "true", "[]", '["gpt-6-astra"]', '{ pin = "gpt-6-astra" }',
    "1979-05-27T07:32:00Z",
    ].map((literal) => ({ literal })),
    ...[
      " ", " gpt-6-astra", "gpt-6-astra ", "gpt-6 astra", "gpt-6\tastra",
      "gpt-6\nastra", "gpt-6\u0000astra", "gpt-6\u007fastra", "gpt-6\u202eastra",
      "gpt-6\u200bastra", "gpt-6\ufffdastra", "x".repeat(101), "é".repeat(101),
      "opus", "sonnet", "haiku", "claude-opus-4-8", "claude-sonnet-4-6",
      "gpt-5", "gpt-7-custom-preview", "gpt-6-astra-unknown",
    ].map((raw) => ({ literal: JSON.stringify(raw).replace(/\u007f/g, "\\u007f"), raw })),
  ];
  hostile.forEach(({ literal, raw }, index) => {
    it(`rejects hostile model case ${index} with a bounded non-echoing note`, () => {
      const parsed = parseCodexAgentFile(source(`model = ${literal}`), "file");
      assert.deepEqual(parsed, { ok: false, name: "coder", reason: "invalid" });
      if (parsed.ok) return;
      const note = describeRepoAgentNote(parsed);
      assert.equal(note, 'repo agent "coder" was skipped: unreadable or invalid file, name, description, model, or empty body');
      assert.ok(note.length < 200);
      assert.equal(note.includes("model ="), false);
      assert.equal(note.includes(literal), false);
      // Normal prose contains spaces; the exact-message assertion covers whitespace-only values.
      if (raw !== undefined && raw.trim() !== "") assert.equal(note.includes(raw), false);
    });
  });

  for (const key of ["features", "skills", "sandbox_mode", "tools"]) {
    it(`preserves ${key} restriction precedence over an invalid model`, () => {
      assert.deepEqual(parseCodexAgentFile(source(`model = 12\n${key} = true`), "file"), {
        ok: false, name: "coder", reason: "restriction_unsupported", restrictions: [key],
      });
      assert.deepEqual(parseCodexAgentFile(source(`model = 12\n${key} = true`).replace('name = "coder"', 'name = 1'), "file"), {
        ok: false, name: "file", reason: "invalid",
      });
    });
  }
});
