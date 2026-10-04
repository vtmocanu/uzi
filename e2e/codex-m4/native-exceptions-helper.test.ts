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
  assert.deepEqual(inventory.names, ["outer", "functions", "exec", "request_user_input_async", "clock", "curr_time"]);
  assert.match(inventory.descriptions, /uzi_read/);
});
