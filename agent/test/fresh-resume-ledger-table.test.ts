import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { isFreshLedgerState, parseAttemptLedgerEntry } from "../src/git.js";
import { judgeRetainedEvidence } from "../src/pod-epoch-run-retention.js";
import { LEDGER_READER_TABLE } from "../src/ledger-reader-table.js";

// issue #4628: the attempt ledger learned fresh_pending and fresh_unbound. Every function that calls the parser has one row in
// LEDGER_READER_TABLE, and this registry fails when a new caller shows up without a decision (or a row outlives its function).

const SRC = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src");
const DECL = /^(?:  (?:private |public |protected |static )*(?:async )?(?:function\s+)?([A-Za-z_]\w*)\s*(?:<[^>]*>)?\(|(?:export )?(?:async )?function\s+([A-Za-z_]\w*)\s*(?:<[^>]*>)?\()/;
const NOT_A_DECLARATION = new Set(["if", "for", "while", "switch", "catch", "return", "await", "function", "constructor"]);

/** A call of the parser or of the shared ledger reads that wrap it (a definition has no leading dot). */
const READ_CALL = /parseAttemptLedgerEntry\(|\.readAttemptLedger\(|\.readAllAttemptLedgers\(/;

/** `file#function` for each call of the parser or a shared ledger read: the nearest enclosing top-level function or class method declaration. */
function parserCallers(): Set<string> {
  const out = new Set<string>();
  for (const file of fs.readdirSync(SRC).filter(name => name.endsWith(".ts"))) {
    const lines = fs.readFileSync(path.join(SRC, file), "utf8").split("\n");
    lines.forEach((line, i) => {
      if (!READ_CALL.test(line) || /^\s*(\*|\/\/)/.test(line) || line.includes("export function parseAttemptLedgerEntry")) return;
      for (let j = i; j >= 0; j--) {
        const m = DECL.exec(lines[j]!);
        const name = m?.[1] ?? m?.[2];
        const indent = /^ */.exec(lines[j]!)![0].length;
        if (name !== undefined && !NOT_A_DECLARATION.has(name) && (indent === 0 || indent === 2)) { out.add(`src/${file}#${name}`); return; }
      }
      throw new Error(`no enclosing function for ${file}:${i + 1}`);
    });
  }
  return out;
}

describe("the attempt-ledger reader table", () => {
  it("has a disposition for every function that calls parseAttemptLedgerEntry, readAttemptLedger or readAllAttemptLedgers", () => {
    const rows = new Set(LEDGER_READER_TABLE.map(row => row.caller));
    const missing = [...parserCallers()].filter(caller => !rows.has(caller));
    assert.deepEqual(missing, [], "a new parser caller needs a row in ledger-reader-table.ts (and a decision about fresh states)");
  });

  it("names no function that is gone, and every row says what it does", () => {
    for (const row of LEDGER_READER_TABLE) {
      const [file, fn] = row.caller.split("#") as [string, string];
      const source = fs.readFileSync(path.join(SRC, "..", file), "utf8");
      assert.match(source, new RegExp(`\\b${fn}\\b`), `${row.caller} no longer exists`);
      assert.ok(row.note.length >= 10, `${row.caller} needs a note`);
    }
    assert.equal(new Set(LEDGER_READER_TABLE.map(row => row.caller)).size, LEDGER_READER_TABLE.length, "one row per caller");
  });

  it("the registry scan itself finds the callers the issue names", () => {
    const callers = parserCallers();
    for (const expected of ["src/git.ts#compactAttemptLedger", "src/git.ts#scanRunBareConfig", "src/restore-route.ts#classifyRestoreRelationships",
      "src/restore-boundary.ts#enumerateAttributedPaths", "src/git.ts#remediationSourceUnderLock", "src/git.ts#seedIndependentAttempt",
      "src/git.ts#retireRunnerCloneImpl"]) assert.ok(callers.has(expected), expected);
  });
});

describe("the parser and the readers that decide on a state", () => {
  const value = (state: string) => JSON.stringify({ attemptId: "20260101T000000Z-g2-0123456789abcdef", runId: "r", clonePath: "/data/runner/x/k.attempt-20260101T000000Z-g2-0123456789abcdef", state });

  it("accepts both fresh states and still refuses an unknown one", () => {
    assert.equal(parseAttemptLedgerEntry(value("fresh_pending"))?.state, "fresh_pending");
    assert.equal(parseAttemptLedgerEntry(value("fresh_unbound"))?.state, "fresh_unbound");
    assert.equal(parseAttemptLedgerEntry(value("fresh_whatever")), undefined);
    assert.equal(isFreshLedgerState("fresh_pending"), true);
    assert.equal(isFreshLedgerState("fresh_unbound"), true);
    assert.equal(isFreshLedgerState("live"), false);
  });

  it("run retention keeps a fresh attempt active, never ended", () => {
    for (const ledgerState of ["fresh_pending", "fresh_unbound"]) {
      const verdict = judgeRetainedEvidence({ kind: "attempt", ledgerState, presence: "missing", sources: [{ presence: "missing", live: false }] } as never);
      assert.equal(verdict.state, "active");
      assert.match(JSON.stringify(verdict), new RegExp(`ledger_${ledgerState}`));
    }
  });
});
