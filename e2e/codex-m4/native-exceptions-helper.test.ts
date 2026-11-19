import { test } from "node:test";
import assert from "node:assert/strict";
import { INVENTORY_TITLE, runtimeInventory } from "./native-exceptions.js";

test(INVENTORY_TITLE, () => {
  const inventory = runtimeInventory([{ tools: [{ type: "function", name: "outer" }], input: [
    { type: "additional_tools", tools: [{ type: "namespace", name: "functions", tools: [
      { name: "exec", description: "declare const tools: { uzi_read(args: {}): Promise<unknown>; };" },
      { name: "request_user_input_async" },
    ] }, { type: "namespace", name: "clock", tools: [{ name: "curr_time" }] }] },
  ] }]);
  assert.deepEqual(inventory.names, ["outer", "functions", "exec", "uzi_read", "request_user_input_async", "clock", "curr_time"]);
  assert.match(inventory.descriptions, /uzi_read/);
  assert.deepEqual(inventory.nestedNames, ["uzi_read"]);
  assert.ok(inventory.structuredNames.includes("clock.curr_time"));
  assert.deepEqual(inventory.unparsed, []);
  assert.equal(inventory.observed, true);
});

function execInventory(description: string) {
  return runtimeInventory([{ additional_tools: [{ type: "namespace", name: "functions", tools: [
    { name: "exec", description },
  ] }] }]);
}

for (const unknown of ["unknown_tool(): Promise<unknown>;", "unknown_tool: () => Promise<unknown>;"]) {
  test(`inventory preserves unknown callable: ${unknown}`, () => {
    const inventory = execInventory(`declare const tools: { uzi_read(args: { callback: (argument: { misleading(): void }) => void }): Promise<unknown>; ${unknown} };`);
    assert.deepEqual(inventory.nestedNames, ["uzi_read", "unknown_tool"]);
    assert.deepEqual(inventory.names, ["functions", "exec", "uzi_read", "unknown_tool"]);
    assert.deepEqual(inventory.structuredNames, ["functions", "functions.exec"]);
    assert.deepEqual(inventory.unparsed, []);
  });
}

test("inventory parses fenced declarations, multiple blocks and quoted members", () => {
  const inventory = execInventory('prose\n```ts\ndeclare const tools: { "clock__curr_time": () => Promise<{current_time: string}> };\n```\nmore\n```ts\ndeclare const tools: { apply_patch(input: string): Promise<unknown> };\n```');
  assert.deepEqual(inventory.nestedNames, ["clock__curr_time", "apply_patch"]);
  assert.deepEqual(inventory.unparsed, []);
});

for (const declaration of [
  "declare const tools: { uzi_read(): Promise<unknown>;",
  "declare const tools: { uzi_read(: Promise<unknown>; };",
  "declare const tools: { uzi_read(): Promise<unknown>; unknown_tool: unknown; };",
  "declare const tools: { uzi_read(): Promise<unknown>; [key: string]: unknown; };",
  "declare const tools: { uzi_read(): Promise<unknown>; }; unexpected();",
  "declare const tools: SomeAlias;",
  "```ts\ndeclare const tools: { ok(): void };\n```\ndeclare const tools: { unfinished",
]) {
  test(`inventory rejects incomplete or unconsumed evidence: ${declaration}`, () => {
    assert.ok(execInventory(declaration).unparsed.length > 0);
  });
}

for (const declaration of [
  "declare const /* inventory */ tools: { unknown_tool(): void };",
  "declare /* inventory */ const tools: { unknown_tool(): void };",
  "declare const tools: { /* inventory */ unknown_tool /* member */ (): void };",
  "declare const tools: { unknown_tool /* member */ : () => void };",
]) {
  for (const fenced of [false, true]) {
    test(`inventory parses commented declarations (fenced=${fenced}): ${declaration}`, () => {
      const description = fenced ? `prose\n\`\`\`ts\n${declaration}\n\`\`\`` : declaration;
      const inventory = execInventory(description);
      assert.deepEqual(inventory.nestedNames, ["unknown_tool"]);
      assert.deepEqual(inventory.unparsed, []);
    });
  }
}

for (const declaration of [
  "declare namespace tools { function unknown_tool(): Promise<unknown>; }",
  "declare /* inventory */ namespace tools { function unknown_tool(): void; }",
  "declare let /* inventory */ tools: { unknown_tool(): void };",
  "declare const /* inventory */ tools: SomeAlias;",
  "declare function unknown_tool(): void;",
  "declare class tools { unknown_tool(): void; }",
]) {
  for (const fenced of [false, true]) {
    test(`inventory rejects unsupported declarations (fenced=${fenced}): ${declaration}`, () => {
      const description = fenced ? `prose\n\`\`\`ts\n${declaration}\n\`\`\`` : declaration;
      assert.ok(execInventory(description).unparsed.length > 0);
    });
  }
}

test("inventory supports shared type fences alongside tools declarations", () => {
  const inventory = execInventory("Shared types\n```ts\ntype Args = { value: string };\n```\nTools\n```ts\ndeclare const tools: { unknown_tool(args: Args): void };\n```");
  assert.deepEqual(inventory.nestedNames, ["unknown_tool"]);
  assert.deepEqual(inventory.unparsed, []);
});

test("inventory excludes oversized descriptions while retaining sibling evidence", () => {
  const oversized = "declare const tools: { omitted(): void };".padEnd(262145, " ");
  const inventory = runtimeInventory([{ tools: [
    { name: "oversized", description: oversized },
    { name: "exec", description: "declare const tools: { retained(): void };" },
  ] }]);
  assert.deepEqual(inventory.unparsed, ["description size limit"]);
  assert.equal(inventory.descriptions, "declare const tools: { retained(): void };");
  assert.deepEqual(inventory.nestedNames, ["retained"]);
});

test("inventory traverses structured wrappers without treating schema names as tools", () => {
  const inventory = runtimeInventory([{ tools: [{ type: "function", function: {
    name: "unknown_direct", parameters: { name: "not_a_tool" },
  } }], additional_tools: { namespaces: [{ type: "namespace", name: "other", tools: [{ name: "unknown_member" }] }] },
  input: [{ type: "message", name: "not_inventory" }] }]);
  assert.deepEqual(inventory.names, ["unknown_direct", "other", "unknown_member"]);
  assert.deepEqual(inventory.structuredNames, ["unknown_direct", "other", "other.unknown_member"]);
  assert.deepEqual(inventory.unparsed, []);
});

test("inventory distinguishes absent observations and malformed containers", () => {
  for (const requests of [[], [{}], [{ tools: [] }]]) {
    const inventory = runtimeInventory(requests);
    assert.equal(inventory.observed, false);
    assert.deepEqual(inventory.names, []);
  }
  for (const tools of [null, "unknown", [{ type: "namespace", name: "missing" }], [{ name: 7 }], [{}]]) {
    assert.ok(runtimeInventory([{ tools }]).unparsed.length > 0);
  }
});

test("inventory bounds cyclic traversal, large containers and descriptions", () => {
  const cyclic: { tools?: unknown } = {};
  cyclic.tools = cyclic;
  for (const tools of [cyclic, Array(20001).fill({ name: "control" }), [{ name: "exec", description: "x".repeat(262145) }]]) {
    assert.ok(runtimeInventory([{ tools }]).unparsed.length > 0);
  }
});
