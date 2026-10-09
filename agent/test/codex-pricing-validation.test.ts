import assert from "node:assert/strict";
import { it } from "node:test";
import { mkdir, mkdtemp, readFile, writeFile, rm } from "node:fs/promises";
import { resolve } from "node:path";
import { spawnSync } from "node:child_process";

const root = resolve(import.meta.dirname, "../..");
// Scratch stays inside the repo so the spawned tsx child resolves node_modules;
// a fresh CI checkout has no .uzi/scratch yet, so create it first.
async function scratchDir(prefix: string): Promise<string> {
  await mkdir(resolve(root, ".uzi/scratch"), { recursive: true });
  return mkdtemp(resolve(root, `.uzi/scratch/${prefix}`));
}

function changed(path: string[], value: unknown, remove = false): unknown {
  const copy: unknown = structuredClone(table);
  let parent = copy as Record<string, unknown>;
  for (const key of path.slice(0, -1)) parent = parent[key] as Record<string, unknown>;
  const key = path.at(-1)!;
  if (remove) delete parent[key];
  else parent[key] = value;
  return copy;
}

function rejects(value: unknown, path: string): void {
  assert.throws(() => validateCodexPricing(value), (error: unknown) =>
    error instanceof Error && error.name === "CodexPricingValidationError" && error.message.includes(path));
}

const row = ["models", "gpt-5.6-sol"];
const required = [
  ["version"], ["input_tier_threshold_tokens"], ["models"],
  ...["verified_at", "sources", "low", "high"].map(key => [...row, key]),
  ...["low", "high"].flatMap(tier =>
    ["uncached_input", "cached_input", "cache_write", "output"].map(key => [...row, tier, key])),
];

for (const path of required) {
  for (const mode of ["missing", "null"] as const) {
    it(`rejects ${mode} ${path.join(".")}`, () => {
      rejects(changed(path, null, mode === "missing"), path.at(-1)!);
    });
  }
}

for (const path of [[], ["models"], row, [...row, "low"], [...row, "high"]]) {
  it(`rejects nonobjects at ${path.join(".") || "table"}`, () => {
    for (const value of [null, [], "", 1, true]) {
      rejects(path.length === 0 ? value : changed(path, value), path.at(-1) ?? "table");
    }
  });
  it(`rejects ${path.length === 1 && path[0] === "models" ? "invalid extra rows" : "unknown fields"} at ${path.join(".") || "table"}`, () => {
    rejects(changed([...path, "extra"], 0), "extra");
  });
}

it("rejects empty and wrongly typed versions, thresholds and models", () => {
  for (const value of ["", null, 1, false, [], {}]) rejects(changed(["version"], value), "version");
  for (const value of [null, "272000", false, [], {}, 0, -1, 0.5, 272000.5, NaN, Infinity, -Infinity]) {
    rejects(changed(["input_tier_threshold_tokens"], value), "input_tier_threshold_tokens");
  }
  rejects(changed(["models"], {}), "models");
  rejects(changed(["models", ""], table.models["gpt-6-astra"]), "models");
});

for (const tier of ["low", "high"]) {
  for (const bucket of ["uncached_input", "cached_input", "cache_write", "output"]) {
    it(`validates numeric ${tier}.${bucket}, accepting zero`, () => {
      const path = [...row, tier, bucket];
      for (const value of ["1", false, [], {}, -0.1, NaN, Infinity, -Infinity]) rejects(changed(path, value), bucket);
      assert.doesNotThrow(() => validateCodexPricing(changed(path, 0)));
    });
  }
}

for (const field of ["verified_at", "promo_review_date"]) {
  it(`validates real Gregorian ${field} dates and year boundaries`, () => {
    const path = [...row, field];
    for (const value of [null, 20261001, false, [], {}, "", "2026-2-01", "2026-02-29", "1900-02-29",
      "0000-01-01", "10000-01-01", "2026-00-01", "2026-13-01", "2026-01-00", "2026-04-31",
      "2026-01-32", "2026-01-01T00:00:00Z", "2026-01-01\n"]) {
      rejects(changed(path, value), field);
    }
    for (const value of ["0001-01-01", "9999-12-31", "0004-02-29", "2000-02-29", "2024-02-29", "1900-02-28"]) {
      assert.doesNotThrow(() => validateCodexPricing(changed(path, value)));
    }
  });
}

it("validates nonempty HTTPS sources using the portable ASCII host grammar", () => {
  for (const value of [null, "", {}, false, []]) rejects(changed([...row, "sources"], value), "sources");
  for (const value of [null, 1, false, {}, [], "", "http://example.com", "HTTPS://example.com",
    "https://", "https:///path", "https://?x", "https://#x", "https://example.com/a b",
    "https://example.com/\n", "https://example.com/\u0000", " https://example.com",
    "https://example.com/\\x", "https://user@example.com", "https://-bad.example",
    "https://example..com", "https://example.com:", "https://example.com:0",
    "https://example.com:65536", "https://example.com/%zz", "https://example.com/%1",
    "https://999.1.1.1", "https://[::1]", "https://é.example"]) {
    rejects(changed([...row, "sources"], [value]), "sources[0]");
  }
  for (const value of ["https://example.com", "https://example.com:443/a%20b?q=x#y", "https://127.0.0.1/path"]) {
    assert.doesNotThrow(() => validateCodexPricing(changed([...row, "sources"], [value])));
  }
});

it("rejects overflowing raw JSON numbers and uses parser last-key-wins", () => {
  const raw = JSON.stringify(table);
  rejects(JSON.parse(raw.replace('"input_tier_threshold_tokens":272000', '"input_tier_threshold_tokens":1e400')), "input_tier_threshold_tokens");
  rejects(JSON.parse(raw.replace('"uncached_input":10', '"uncached_input":1e400')), "uncached_input");
  assert.equal(validateCodexPricing(JSON.parse(raw.replace('"version":', '"version":null,"version":'))).version, table.version);
  rejects(JSON.parse(raw.replace('"version":"' + table.version + '"', '"version":"' + table.version + '","version":null')), "version");
});

it("keeps canonical metadata and supports optional promo dates and own model keys", () => {
  assert.equal(validateCodexPricing(table).version, "openai-standard-2026-10-01");
  assert.deepEqual(Object.keys(table.models), ["gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol"]);
  for (const [model, data] of Object.entries(table.models)) {
    assert.deepEqual(data.sources, [
      "https://developers.openai.com/api/docs/pricing",
      `https://developers.openai.com/api/docs/models/${model}`,
    ]);
    assert.equal(data.verified_at, model === "gpt-6-sol" ? "2026-09-23" : model === "gpt-6.1-sol" ? "2026-10-01" : "2026-09-13");
    assert.equal(Object.hasOwn(data, "promo_review_date"), model === "gpt-5.6-sol");
  }
  assert.doesNotThrow(() => validateCodexPricing(changed([...row, "promo_review_date"], undefined, true)));
  const own = JSON.parse(JSON.stringify(table).replace('"gpt-6-astra":', '"__proto__":'));
  assert.ok(Object.hasOwn(validateCodexPricing(own).models, "__proto__"));
});

it("malformed pricing data fails during actual module load", async () => {
  const scratch = await scratchDir("pricing-load-");
  try {
    for (const name of ["codex-pricing.ts", "codex-pricing-validation.ts"]) {
      await writeFile(resolve(scratch, name), await readFile(resolve(root, "agent/src/codex", name)));
    }
    await writeFile(resolve(scratch, "package.json"), '{"type":"module"}');
    // Two independent module loads, each capped at 10 seconds; any failure fails this test.
    const fixtures = [
      { raw: '{"version":"broken","input_tier_threshold_tokens":272000,"models":{}}', context: "models:" },
      { raw: JSON.stringify(table).replace('"uncached_input":10', '"uncached_input":1e400'), context: "models.gpt-6-astra.low.uncached_input:" },
    ];
    for (const fixture of fixtures) {
      await writeFile(resolve(scratch, "codex-pricing.json"), fixture.raw);
      const child = spawnSync(process.execPath, ["--import", "tsx", resolve(scratch, "codex-pricing.ts")], {
        cwd: resolve(root, "agent"), encoding: "utf8", timeout: 10000,
      });
      assert.equal(child.error, undefined);
      assert.notEqual(child.status, 0, child.stdout + child.stderr);
      assert.ok(child.stderr.includes("CodexPricingValidationError: " + fixture.context), child.stderr);
    }
  } finally {
    await rm(scratch, { recursive: true, force: true });
  }
});
for (const [name, path] of [
  ["absent Sol row", row],
  ["absent optional promotion", [...row, "promo_review_date"]],
] as const) {
  it(`valid pricing data loads with ${name}`, async () => {
    const scratch = await scratchDir("pricing-load-");
    try {
      for (const name of ["codex-pricing.ts", "codex-pricing-validation.ts"]) {
        await writeFile(resolve(scratch, name), await readFile(resolve(root, "agent/src/codex", name)));
      }
      await writeFile(resolve(scratch, "package.json"), '{"type":"module"}');
      await writeFile(resolve(scratch, "codex-pricing.json"), JSON.stringify(changed([...path], undefined, true)));
      await writeFile(resolve(scratch, "check.ts"), `
        import assert from "node:assert/strict";
        import { SOL_PROMO_REVIEW_DATE, priceCodexResponse } from "./codex-pricing.js";
        assert.equal(SOL_PROMO_REVIEW_DATE, undefined);
        assert.equal(priceCodexResponse("gpt-6-astra", {
          inputTokens: 1000, cachedInputTokens: 0, cacheWriteInputTokens: 0,
          outputTokens: 200, reasoningOutputTokens: 0, totalTokens: 1200,
        }, new Date("2027-01-01T00:00:00Z")), 0.02);
      `);
      // Each independent module load is capped at 10 seconds; a failure fails its test.
      const child = spawnSync(process.execPath, ["--import", "tsx", resolve(scratch, "check.ts")], {
        cwd: resolve(root, "agent"), encoding: "utf8", timeout: 10000,
      });
      assert.equal(child.error, undefined);
      assert.equal(child.status, 0, child.stdout + child.stderr);
    } finally {
      await rm(scratch, { recursive: true, force: true });
    }
  });
}
import { priceCodexResponse } from "../src/codex/codex-pricing.js";
import { validateCodexPricing, validateCodexPricingBytes } from "../src/codex/codex-pricing-validation.js";
import table from "../src/codex/codex-pricing.json" with { type: "json" };

it("inherited object keys are unknown models", () => {
  const usage = {
    inputTokens: 1, cachedInputTokens: 0, cacheWriteInputTokens: 0,
    outputTokens: 0, reasoningOutputTokens: 0, totalTokens: 1,
  };
  for (const model of ["constructor", "toString", "__proto__", "hasOwnProperty"]) {
    assert.equal(priceCodexResponse(model, usage, new Date("2026-01-01")), undefined);
  }
});

it("ExactModelKeys: raw Unicode is checked before module validation", async () => {
  const suite = await readFile(resolve(root, "scripts/pricing-freshness.test.sh"), "utf8");
  const corpus = suite.split("cat <<'UNICODE_FIXTURES'\n")[1]!.split("\nUNICODE_FIXTURES")[0]!;
  const scratch = await scratchDir("pricing-unicode-");
  try {
    for (const name of ["codex-pricing.ts", "codex-pricing-validation.ts"]) {
      await writeFile(resolve(scratch, name), await readFile(resolve(root, "agent/src/codex", name)));
    }
    await writeFile(resolve(scratch, "package.json"), '{"type":"module"}');
    // One independent module load per fixture, each capped at 10 seconds.
    // An assertion failure stops this test; finally removes its scratch.
    for (const line of corpus.split("\n")) {
      const [name, verdict, template] = line.split("|");
      const raw = template!.replaceAll("@ROW@", JSON.stringify(table.models["gpt-6-astra"]));
      await writeFile(resolve(scratch, "codex-pricing.json"), raw);
      const child = spawnSync(process.execPath, ["--import", "tsx", resolve(scratch, "codex-pricing.ts")], {
        cwd: resolve(root, "agent"), encoding: "utf8", timeout: 10000,
      });
      assert.equal(child.error, undefined, name);
      if (verdict === "accept") assert.equal(child.status, 0, name + child.stderr);
      else {
        assert.notEqual(child.status, 0, name);
        if (name !== "two values") assert.match(child.stderr, /CodexPricingValidationError/, name);
      }
    }
  } finally {
    await rm(scratch, { recursive: true, force: true });
  }
});

it("raw UTF8 rejection occurs before replacement decoding at actual module load", async () => {
  const suite = await readFile(resolve(root, "scripts/pricing-freshness.test.sh"), "utf8");
  const corpus = suite.split("cat <<'RAW_UTF8_FIXTURES'\n")[1]!.split("\nRAW_UTF8_FIXTURES")[0]!;
  const scratch = await scratchDir("pricing-bytes-");
  try {
    for (const name of ["codex-pricing.ts", "codex-pricing-validation.ts"]) {
      await writeFile(resolve(scratch, name), await readFile(resolve(root, "agent/src/codex", name)));
    }
    await writeFile(resolve(scratch, "package.json"), '{"type":"module"}');
    const row = JSON.stringify({ ...table.models["gpt-6-astra"], low: { ...table.models["gpt-6-astra"]!.low, output: 2 } });
    const replacementRow = JSON.stringify({ ...table.models["gpt-6-astra"], low: { ...table.models["gpt-6-astra"]!.low, output: 1 } });
    const prefix = '{"version":"fixture","input_tier_threshold_tokens":1,';
    // Fixed corpus and four independent placements; each load has a 10-second cap.
    // A failed assertion stops the test, with scratch removed by finally.
    for (const line of corpus.split("\n")) {
      const [name, verdict, hex] = line.split("|");
      const bytes = Buffer.from(hex!, "hex");
      const placements = [
        ['"models":{"', '":' + row + ',"�":' + replacementRow + '}}'],
        ['"models":{"�":' + replacementRow + ',"', '":' + row + '}}'],
        ['"version":"', '","version":"fixture","models":{"model":' + row + '}}'],
        ['"models":{"discard":{"text":"', '"}},"models":{"model":' + row + '}}'],
      ];
      for (const [index, [before, after]] of placements.entries()) {
        const raw = Buffer.concat([Buffer.from(prefix + before), bytes, Buffer.from(after!)]);
        if (verdict === "accept") assert.doesNotThrow(() => validateCodexPricingBytes(raw), name);
        else assert.throws(() => validateCodexPricingBytes(raw), /CodexPricingValidationError: table:.*UTF-8/, name);
        await writeFile(resolve(scratch, "codex-pricing.json"), raw);
        const key = bytes.toString("utf8");
        const expectations = index < 2
          ? [[key, key === "�" && index === 0 ? 1 : 2], ["�", key === "�" && index === 1 ? 2 : 1]]
          : [["model", 2]];
        await writeFile(resolve(scratch, "check.ts"), [
          'import assert from "node:assert/strict";',
          'import { priceCodexResponse } from "./codex-pricing.js";',
          'const usage = {inputTokens:0,cachedInputTokens:0,cacheWriteInputTokens:0,outputTokens:1000000,reasoningOutputTokens:0,totalTokens:1000000};',
          'for (const [key, expected] of ' + JSON.stringify(expectations) + ') {',
          'assert.equal(priceCodexResponse(key, usage, new Date("2026-01-01")), expected); }',
        ].join("\n"));
        const child = spawnSync(process.execPath, ["--import", "tsx", resolve(scratch, "check.ts")], {
          cwd: resolve(root, "agent"), encoding: "utf8", timeout: 10000,
        });
        assert.equal(child.error, undefined, name);
        if (verdict === "accept") assert.equal(child.status, 0, name + child.stderr);
        else {
          assert.notEqual(child.status, 0, "raw UTF8 rejection: " + name);
          assert.match(child.stderr, /CodexPricingValidationError: table:.*UTF-8/, name);
        }
      }
    }
  } finally {
    await rm(scratch, { recursive: true, force: true });
  }
});

it("direct validator rejects lone Unicode in versions and model keys", () => {
  for (const value of ["\ud800", "\udc00"]) {
    rejects(changed(["version"], value), "version");
    rejects(changed(["models"], { [value]: table.models["gpt-6-astra"] }), "models");
  }
  for (const value of ["�", "\ud800\udc00", "\udbff\udfff"]) {
    assert.doesNotThrow(() => validateCodexPricing(changed(["version"], value)));
    assert.doesNotThrow(() => validateCodexPricing(changed(["models"], { [value]: table.models["gpt-6-astra"] })));
  }
});

it("priceCodexResponse keeps exact Unicode keys and decoded duplicate last wins", async () => {
  const scratch = await scratchDir("pricing-keys-");
  try {
    for (const name of ["codex-pricing.ts", "codex-pricing-validation.ts"]) {
      await writeFile(resolve(scratch, name), await readFile(resolve(root, "agent/src/codex", name)));
    }
    await writeFile(resolve(scratch, "package.json"), '{"type":"module"}');
    const row = (output: number) => JSON.stringify({
      ...table.models["gpt-6-astra"], low: { ...table.models["gpt-6-astra"]!.low, output },
    });
    const raw = '{"version":"fixture","input_tier_threshold_tokens":1,"models":{' +
      '"�":' + row(1) + ',"𐀀":' + row(99) + ',"\\ud800\\udc00":' + row(2) +
      ',"é":' + row(3) + ',"e\\u0301":' + row(4) + ',"\\udbff\\udfff":' + row(5) + '}}';
    await writeFile(resolve(scratch, "codex-pricing.json"), raw);
    const expectations = [["�", 1], ["𐀀", 2], ["é", 3], ["e\u0301", 4], ["\udbff\udfff", 5], ["unknown", null]];
    await writeFile(resolve(scratch, "check.ts"), [
      'import assert from "node:assert/strict";',
      'import { priceCodexResponse } from "./codex-pricing.js";',
      'const usage = {inputTokens:0,cachedInputTokens:0,cacheWriteInputTokens:0,outputTokens:1000000,reasoningOutputTokens:0,totalTokens:1000000};',
      'for (const [model, expected] of ' + JSON.stringify(expectations) + ') {',
      'assert.equal(priceCodexResponse(model, usage, new Date("2026-01-01")), expected ?? undefined); }',
    ].join("\n"));
    const child = spawnSync(process.execPath, ["--import", "tsx", resolve(scratch, "check.ts")], {
      cwd: resolve(root, "agent"), encoding: "utf8", timeout: 10000,
    });
    assert.equal(child.error, undefined);
    assert.equal(child.status, 0, child.stdout + child.stderr);
  } finally {
    await rm(scratch, { recursive: true, force: true });
  }
});
