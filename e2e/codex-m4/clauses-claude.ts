// PRD #1287 — Claude adapter clause contributions. C2 APPENDS the real per-clause
// hook/construction/reducer/advice rows here (existing Claude source stays read-only).
//
// C1 SEEDS this file with placeholder rows ONLY to demonstrate the shape and keep the file
// non-empty; they are NOT required by the C1 completeness matrix (run-completeness.ts), so
// their tests need not have executed at C1. C2 replaces these seeds with executed rows and
// tightens the required matrix. Every id below is prefixed `claude-c1-seed-` so C2 can find
// and remove them wholesale.

import type { ClauseRow } from "./clause.js";

export const CLAUDE_CLAUSES: ClauseRow[] = [
  {
    // C1 SEED — replaced by C2's real deny-all-hook row. Shape demonstration only.
    id: "claude-c1-seed-deny-all-hook",
    adapter: "claude",
    layer: "U",
    family: "Isolation and compatibility",
    seam: "agent/src/*: the Claude deny-all PreToolUse hook + literal settingSources: []",
    positiveControl: "(C2) an allowed tool-less text turn returns its result unchanged",
    negativeOracle: "(C2) the deny-all hook blocks a shell/file/network tool with no side effect",
    intendedOutcome: "Claude stays tool-less with its deny-all hook; existing defaults unchanged",
    tests: ["(C1 seed) claude deny-all hook placeholder — real coverage lands in C2"],
  },
  {
    // C1 SEED — replaced by C2's real advice-ceiling row. Shape demonstration only.
    id: "claude-c1-seed-advice-ceiling",
    adapter: "claude",
    layer: "U",
    family: "Advice ceiling",
    seam: "agent/src/*: Claude advice harness capability ceiling + cleanup semantics",
    positiveControl: "(C2) legitimate advice text/pure-cell result preserves current semantics",
    negativeOracle: "(C2) shell/files/network/delegation/credential advice effects never occur",
    intendedOutcome: "advice retains its lane-specific ceiling and cleanup/error semantics",
    tests: ["(C1 seed) claude advice ceiling placeholder — real coverage lands in C2"],
  },
];
