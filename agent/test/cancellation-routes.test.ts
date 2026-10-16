import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { it } from "node:test";

const inventory = new URL("./cancellation-routes.md", import.meta.url);
const queryRoot = new URL("../../api/internal/store/queries/", import.meta.url);

it("every documented production cancellation route maps to existing preservation or safety tests", async () => {
  const map = await fs.readFile(inventory, "utf8");
  const producerRows = map.split("## Producers and arbitration")[1]!.split("### Current named SQL")[0]!;
  const consumerRows = map.split("## Worker consumers and preservation seams")[1]!;
  let routes = 0;
  for (const row of `${producerRows}\n${consumerRows}`.split("\n")) {
    if (!row.startsWith("| ") || row.includes("|---") || row.includes("| Production entry |") || row.includes("| Entry or race |")) continue;
    const cells = row.split("|").slice(1, -1);
    assert.equal(cells.length, 3, `malformed route mapping: ${row}`);
    const tests = [...cells[2]!.matchAll(/[\w-]+\.test\.ts/g)].map(match => match[0]);
    assert.ok(tests.length > 0, `route lacks preservation/safety coverage: ${cells[0]}`);
    for (const test of tests) await fs.access(new URL(test, import.meta.url));
    routes++;
  }
  assert.ok(routes > 0, "production route mapping must not be empty");
  assert.match(consumerRows, /\| steering\.ts[^\n]*terminal_cancelled[^\n]*terminal-cancelled\.test\.ts[^\n]*runner-cancellation-deadline\.test\.ts/,
    "server-side terminal cancellation needs an explicit deadline and retained-source coverage mapping");
});

it("tracked cancellation inventory enumerates every named SQL run-cancellation writer", async () => {
  const map = await fs.readFile(inventory, "utf8");
  const documented = [...map.matchAll(/^\| (\w+\.sql) \| (\w+) \|$/gm)]
    .map(([, file, query]) => `${file}:${query}`).sort();
  assert.ok(documented.length > 0, "inventory table must be present");
  const actual: string[] = [];
  for (const file of (await fs.readdir(queryRoot)).filter(file => file.endsWith(".sql"))) {
    const sql = await fs.readFile(new URL(file, queryRoot), "utf8");
    for (const query of sql.split(/(?=^-- name: )/m)) {
      const name = /^-- name: (\w+)/.exec(query)?.[1];
      const body = query.replace(/--[^\n]*/g, "");
      if (name && /UPDATE\s+runs\b[^;]*\bstatus\s*=\s*'cancelled'/is.test(body)) {
        actual.push(`${file}:${name}`);
      }
    }
  }
  assert.deepEqual(actual.sort(), documented, "new SQL cancellation writers need a preservation route and coverage mapping");
});
