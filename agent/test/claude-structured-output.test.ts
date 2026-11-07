import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { CODE_VERDICT_SCHEMA, PLAN_VERDICT_SCHEMA, validateCodeVerdict, validateVerdict } from "../src/codex/cross-check.js";

// A deliberately small JSON Schema checker. It supports exactly SUPPORTED_KEYWORDS, and a test
// below proves both verdict schemas use nothing else, so a true result here is faithful.
const SUPPORTED_KEYWORDS = new Set([
 "type", "enum", "required", "properties", "additionalProperties", "items",
 "maxItems", "maxLength", "pattern", "minimum", "maximum", "description",
]);

type Schema = { readonly [key: string]: unknown };

function isRecord(v: unknown): v is Record<string, unknown> {
 return v !== null && typeof v === "object" && !Array.isArray(v);
}

function check(schema: Schema, value: unknown): boolean {
 const type = schema["type"];
 if (type === "object" && !isRecord(value)) return false;
 if (type === "array" && !Array.isArray(value)) return false;
 if (type === "string" && typeof value !== "string") return false;
 if (type === "integer" && !(typeof value === "number" && Number.isInteger(value))) return false;
 const enumValues = schema["enum"];
 if (Array.isArray(enumValues) && !enumValues.includes(value)) return false;
 if (isRecord(value)) {
  const properties = (schema["properties"] ?? {}) as Record<string, Schema>;
  for (const key of (schema["required"] ?? []) as string[]) if (!Object.hasOwn(value, key)) return false;
  for (const [key, child] of Object.entries(value)) {
   if (Object.hasOwn(properties, key)) {
    if (!check(properties[key] as Schema, child)) return false;
   } else if (schema["additionalProperties"] === false) return false;
  }
 }
 if (Array.isArray(value)) {
  if (typeof schema["maxItems"] === "number" && value.length > schema["maxItems"]) return false;
  const items = schema["items"];
  if (isRecord(items) && !value.every((v) => check(items, v))) return false;
 }
 if (typeof value === "string") {
  if (typeof schema["maxLength"] === "number" && [...value].length > schema["maxLength"]) return false;
  if (typeof schema["pattern"] === "string" && !new RegExp(schema["pattern"]).test(value)) return false;
 }
 if (typeof value === "number") {
  if (typeof schema["minimum"] === "number" && value < schema["minimum"]) return false;
  if (typeof schema["maximum"] === "number" && value > schema["maximum"]) return false;
 }
 return true;
}

function keywordsUsed(schema: unknown, out: Set<string> = new Set()): Set<string> {
 if (Array.isArray(schema)) for (const v of schema) keywordsUsed(v, out);
 else if (isRecord(schema)) {
  for (const [key, child] of Object.entries(schema)) {
   out.add(key);
   // "properties" maps property NAMES to schemas; the names are not keywords.
   if (key === "properties" && isRecord(child)) for (const sub of Object.values(child)) keywordsUsed(sub, out);
   else if (key !== "enum" && key !== "required") keywordsUsed(child, out);
  }
 }
 return out;
}

const planItem = { file: "a.ts", severity: "info", summary: "s", rationale: "r" };
const codeFinding = { id: "F1", severity: "minor", path: "a.ts", line: 1, title: "t", detail: "d" };

describe("verdict schemas and the in-test checker", () => {
 it("use only keywords the checker supports", () => {
  for (const schema of [PLAN_VERDICT_SCHEMA, CODE_VERDICT_SCHEMA]) {
   const unsupported = [...keywordsUsed(schema)].filter((k) => !SUPPORTED_KEYWORDS.has(k));
   assert.deepEqual(unsupported, []);
  }
 });

 it("survive a JSON round trip unchanged", () => {
  for (const schema of [PLAN_VERDICT_SCHEMA, CODE_VERDICT_SCHEMA]) {
   assert.deepEqual(JSON.parse(JSON.stringify(schema)), schema);
  }
 });

 const planAccepted: Array<[string, unknown]> = [
  ["approve with no items", { verdict: "approve", summary: "ok", items: [] }],
  ["revise with info item", { verdict: "revise", summary: "x", items: [planItem] }],
  ["revise with warning item", { verdict: "revise", summary: "x", items: [{ ...planItem, severity: "warning" }] }],
  ["block with error item", { verdict: "block", summary: "x", items: [{ ...planItem, severity: "error" }] }],
  ["empty strings", { verdict: "approve", summary: "", items: [{ file: "", severity: "info", summary: "", rationale: "" }] }],
  ["summary of exactly 4096 bytes", { verdict: "approve", summary: "x".repeat(4096), items: [] }],
  ["item with a 1900 character string field", { verdict: "revise", summary: "x", items: [{ ...planItem, rationale: "r".repeat(1900) }] }],
  ["twenty items",{ verdict: "revise", summary: "x", items: Array.from({ length: 20 }, () => planItem) }],
 ];
 for (const [name, v] of planAccepted) {
  it(`plan verdict accepted by validator and schema: ${name}`, () => {
   assert.doesNotThrow(() => validateVerdict(JSON.stringify(v)));
   assert.equal(check(PLAN_VERDICT_SCHEMA, v), true);
  });
 }

 const codeAccepted: Array<[string, unknown]> = [
  ["no findings", { findings: [] }],
  ["minor finding", { findings: [codeFinding] }],
  ["major finding at line 0", { findings: [{ ...codeFinding, severity: "major", line: 0 }] }],
  ["critical finding at max line", { findings: [{ ...codeFinding, severity: "critical", line: 2147483647 }] }],
  ["id of 64 characters", { findings: [{ ...codeFinding, id: "a_-".repeat(21) + "b" }] }],
  ["detail of 1900 characters", { findings: [{ ...codeFinding, detail: "d".repeat(1900) }] }],
  ["valid coverage",{ findings: [], coverage: [{ path: "a.ts", status: "examined", reason: "read" }] }],
  ["null coverage", { findings: [codeFinding], coverage: null }],
  ["coverage entries with an extra key", { findings: [], coverage: [{ path: "a.ts", status: "excluded", reason: "r", extra: 1 }] }],
  ["coverage as an object", { findings: [], coverage: { path: "a.ts" } }],
 ];
 for (const [name, v] of codeAccepted) {
  it(`code verdict accepted by validator and schema: ${name}`, () => {
   assert.doesNotThrow(() => validateCodeVerdict(JSON.stringify(v)));
   assert.equal(check(CODE_VERDICT_SCHEMA, v), true);
  });
 }

 const planRejected: Array<[string, unknown]> = [
  ["extra root key", { verdict: "approve", summary: "ok", items: [], extra: 1 }],
  ["bad verdict enum", { verdict: "maybe", summary: "ok", items: [] }],
  ["missing items", { verdict: "approve", summary: "ok" }],
  ["bad severity enum", { verdict: "revise", summary: "x", items: [{ ...planItem, severity: "fatal" }] }],
  ["extra item key", { verdict: "revise", summary: "x", items: [{ ...planItem, extra: 1 }] }],
  ["too many items", { verdict: "revise", summary: "x", items: Array.from({ length: 21 }, () => planItem) }],
  ["summary too long", { verdict: "approve", summary: "x".repeat(4097), items: [] }],
  ["items not an array", { verdict: "approve", summary: "x", items: "none" }],
 ];
 for (const [name, v] of planRejected) {
  it(`plan schema rejects: ${name}`, () => {
   assert.equal(check(PLAN_VERDICT_SCHEMA, v), false);
  });
 }

 const codeRejected: Array<[string, unknown]> = [
  ["findings not an array", { findings: "none" }],
  ["extra root key", { findings: [], extra: 1 }],
  ["bad id", { findings: [{ ...codeFinding, id: "bad id" }] }],
  ["bad severity", { findings: [{ ...codeFinding, severity: "blocker" }] }],
  ["negative line", { findings: [{ ...codeFinding, line: -1 }] }],
  ["fractional line", { findings: [{ ...codeFinding, line: 1.5 }] }],
  ["line above cap", { findings: [{ ...codeFinding, line: 2147483648 }] }],
  ["missing detail", { findings: [{ id: "F1", severity: "minor", path: "a", line: 1, title: "t" }] }],
 ];
 for (const [name, v] of codeRejected) {
  it(`code schema rejects: ${name}`, () => {
   assert.equal(check(CODE_VERDICT_SCHEMA, v), false);
  });
 }

 it("counts maxLength in code points", () => {
  const emoji = "\u{1F600}";
  assert.equal(check({ type: "string", maxLength: 2 }, emoji + emoji), true);
  assert.equal(check({ type: "string", maxLength: 2 }, emoji.repeat(3)), false);
 });
});
