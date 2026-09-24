// Issue #1555: every uzi MCP tool's input schema must render the SAME `required` set in
// Zod v4's output-mode JSON Schema (`z.toJSONSchema(s)`, the default) as in input mode
// (`{ io: "input" }`). A field built with `.optional().default(x)` is optional on INPUT
// but always present on OUTPUT, so output mode lists it in `required`. Claude's deferred
// ToolSearch path enforces the published schema, so a call omitting that field was
// rejected (MCP -32602 "expected nonoptional") even though the handler never needed it.
//
// The guard: for every tool on every uzi MCP server (all optional tools/fields enabled),
// output-mode `required` must be a subset of input-mode `required` at every nested path.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { z } from "zod";
import { buildSignalMcpServer } from "../src/signals.js";
import { buildMemoryServer } from "../src/memory-tools.js";
import { buildFindingsToolsServer } from "../src/findings-tools.js";
import { buildForgeToolsServer } from "../src/forge-tools.js";
import { buildUziToolsServer } from "../src/uzi-tools.js";
import type { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

type JsonSchema = Record<string, unknown>;

interface RegisteredTool {
  inputSchema?: unknown;
}

/** Building a server never calls the client; a bare object keeps the test hermetic. */
const client = {} as unknown as WorkerClient;

function registeredTools(server: unknown): Record<string, RegisteredTool> {
  const tools = (server as { instance?: { _registeredTools?: Record<string, RegisteredTool> } }).instance
    ?._registeredTools;
  assert.ok(tools, "expected the sdk server to expose its registered tools");
  return tools!;
}

/** Every uzi MCP server, built with every conditional tool and field switched on. */
function allServers(): Array<{ server: string; tools: Record<string, RegisteredTool> }> {
  const deps = { client, runId: "run-parity", log: nullLogger() };
  return [
    {
      server: "signals",
      tools: registeredTools(
        buildSignalMcpServer({
          prdDonePath: true,
          milestones: true,
          progress: true,
          checkpoint: true,
          reportOnly: true,
        }),
      ),
    },
    { server: "memory", tools: registeredTools(buildMemoryServer(deps).server) },
    { server: "findings", tools: registeredTools(buildFindingsToolsServer({ ...deps, emit: () => {} }).server) },
    { server: "forge", tools: registeredTools(buildForgeToolsServer(deps).server) },
    { server: "uzi", tools: registeredTools(buildUziToolsServer({ ...deps, emit: () => {} }).server) },
  ];
}

const isObj = (v: unknown): v is JsonSchema => typeof v === "object" && v !== null && !Array.isArray(v);

/** Walk output- and input-mode schemas in lockstep and collect every path where the
 *  output mode requires a property the input mode does not. */
function requiredDrift(out: unknown, inp: unknown, path: string, acc: string[]): void {
  if (!isObj(out) || !isObj(inp)) return;
  const outReq = Array.isArray(out.required) ? (out.required as string[]) : [];
  const inReq = new Set(Array.isArray(inp.required) ? (inp.required as string[]) : []);
  for (const k of outReq) if (!inReq.has(k)) acc.push(`${path}.${k}`);

  for (const key of ["properties", "$defs", "definitions", "patternProperties"]) {
    const o = out[key];
    const i = inp[key];
    if (isObj(o) && isObj(i)) {
      for (const name of Object.keys(o)) requiredDrift(o[name], i[name], `${path}.${name}`, acc);
    }
  }
  for (const key of ["items", "additionalProperties", "not", "contains", "propertyNames"]) {
    requiredDrift(out[key], inp[key], `${path}.${key}`, acc);
  }
  for (const key of ["anyOf", "oneOf", "allOf", "prefixItems", "items"]) {
    const o = out[key];
    const i = inp[key];
    if (Array.isArray(o) && Array.isArray(i)) {
      o.forEach((sub, idx) => requiredDrift(sub, i[idx], `${path}.${key}[${idx}]`, acc));
    }
  }
}

describe("uzi MCP tool schemas: output-mode required ⊆ input-mode required (issue #1555)", () => {
  it("no tool publishes a field as required that its input schema treats as optional", () => {
    const covered: string[] = [];
    const drift: string[] = [];
    const perServer = new Map<string, number>();

    for (const { server, tools } of allServers()) {
      for (const [name, t] of Object.entries(tools)) {
        const schema = t.inputSchema;
        assert.ok(schema, `${server}/${name}: expected a zod inputSchema`);
        let outSchema: unknown;
        let inSchema: unknown;
        try {
          outSchema = z.toJSONSchema(schema as z.ZodType);
          inSchema = z.toJSONSchema(schema as z.ZodType, { io: "input" });
        } catch (err) {
          assert.fail(`${server}/${name}: z.toJSONSchema threw: ${(err as Error).message}`);
        }
        const acc: string[] = [];
        requiredDrift(outSchema, inSchema, name, acc);
        drift.push(...acc);
        covered.push(name);
        perServer.set(server, (perServer.get(server) ?? 0) + 1);
      }
    }

    assert.ok(covered.length > 0, "expected at least one tool to be covered");
    assert.ok(covered.includes("report_progress"), `report_progress not covered; got ${covered.join(", ")}`);
    assert.ok(covered.includes("save_memory"), `save_memory not covered; got ${covered.join(", ")}`);
    for (const server of ["signals", "memory", "findings", "forge", "uzi"]) {
      assert.ok((perServer.get(server) ?? 0) > 0, `no tool covered from the ${server} server`);
    }
    assert.deepStrictEqual(
      drift,
      [],
      `output-mode JSON Schema requires fields the input schema treats as optional (a .default() on an MCP tool field): ${drift.join(", ")}`,
    );
  });
});
