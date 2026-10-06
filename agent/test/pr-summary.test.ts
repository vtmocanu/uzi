// PRD #1798 M2 (D4): the lead's structured `pr_summary` claims on signal_done. Pins the defensive
// parse (omission, malformed members, clamps), the main-thread-only guarantee, both harnesses'
// tool schemas, the reducer's last-wins fold, and the executor's HEAD stamp.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { z } from "zod";

import { buildSignalMcpServer, normalizeVerifiedSha, scanSignals } from "../src/signals.js";
import { buildCodexDynamicTools } from "../src/codex/dynamic-tools.js";
import { RunTurnReducerImpl } from "../src/harness-reducer.js";
import type { HarnessContext, HarnessContextHook, HarnessEvent, TurnSignals } from "../src/harness.js";
import { readWorktreeHeadSha, stampPrSummaryHead, worktreeHeadShaEnv } from "../src/executor.js";
import { WORKER_SPAWN_ENV } from "../src/worker-spawn-mark.js";
import { buildImplementPrompt, PR_SUMMARY_GUIDANCE } from "../src/prompt.js";
import { makeGitRepo, PR_SUMMARY_EXPECTED, PR_SUMMARY_INPUT } from "./pr-summary-fixture.js";

const DONE = "mcp__uzi__signal_done";

function doneFrame(input: unknown, extra: Record<string, unknown> = {}): unknown {
  return {
    type: "assistant",
    session_id: "s",
    ...extra,
    message: { content: [{ type: "tool_use", id: "t", name: DONE, input }] },
  };
}

const prSummaryOf = (input: unknown) => scanSignals(doneFrame({ pr_summary: input })).prSummary;

describe("scanSignals pr_summary on signal_done (PRD #1798 M2)", () => {
  it("a plain signal_done without pr_summary still scans to exactly { done: true }", () => {
    assert.deepStrictEqual(scanSignals(doneFrame({})), { done: true });
    assert.deepStrictEqual(scanSignals(doneFrame({ summary: "s" })), { done: true, summary: "s" });
  });

  it("parses the shared fixture: trims, drops malformed entries, ignores a model-supplied stamp", () => {
    assert.deepStrictEqual(scanSignals(doneFrame({ pr_summary: PR_SUMMARY_INPUT })), {
      done: true,
      prSummary: PR_SUMMARY_EXPECTED,
    });
  });

  it("drops a pr_summary that is not an object, or where nothing parses, without affecting done", () => {
    for (const bad of [null, "text", 42, true, [], ["what"], {}, { what: "   ", why: 3 },
      { changes: "not a list", verification: [{ command: "x" }], scope_notes: [{ kind: "added" }] },
      { review_pointers: [null, "", "  "] }]) {
      assert.deepStrictEqual(scanSignals(doneFrame({ pr_summary: bad })), { done: true }, JSON.stringify(bad));
    }
  });

  it("keeps each well-formed member on its own", () => {
    assert.deepStrictEqual(prSummaryOf({ what: "w" }), { what: "w" });
    assert.deepStrictEqual(prSummaryOf({ why: "y", changes: 5 }), { why: "y" });
    assert.deepStrictEqual(prSummaryOf({ verification: [{ command: "go test ./...", result: "pass", extra: 1 }] }), {
      verification: [{ command: "go test ./...", result: "pass" }],
    });
    for (const kind of ["added", "changed", "dropped", "deferred"]) {
      assert.deepStrictEqual(prSummaryOf({ scope_notes: [{ kind, text: "t" }] }), { scope_notes: [{ kind, text: "t" }] });
    }
    assert.equal(prSummaryOf({ verification: [{ command: "x", result: "PASS" }] }), undefined, "result is case-exact");
    assert.equal(prSummaryOf({ scope_notes: [{ kind: "Added", text: "t" }] }), undefined, "kind is case-exact");
  });

  it("clamps what/why to 4000 bytes and list items to 1000 bytes, at a code point boundary", () => {
    const got = prSummaryOf({
      what: "a".repeat(5000),
      why: "\u00e9".repeat(2500), // 5000 bytes of 2-byte code points
      changes: ["b".repeat(1500)],
      verification: [{ command: "c".repeat(1200), result: "pass" }],
      scope_notes: [{ kind: "added", text: "\u20ac".repeat(400) }], // 1200 bytes of 3-byte code points
      review_pointers: ["x" + "\u{1F600}".repeat(300)], // 1 + 1200 bytes; 4-byte code points
    })!;
    assert.equal(Buffer.byteLength(got.what!), 4000);
    assert.equal(got.why, "\u00e9".repeat(2000));
    assert.equal(Buffer.byteLength(got.changes![0]!), 1000);
    assert.equal(Buffer.byteLength(got.verification![0]!.command), 1000);
    assert.equal(got.scope_notes![0]!.text, "\u20ac".repeat(333));
    assert.equal(got.review_pointers![0], "x" + "\u{1F600}".repeat(249));
    for (const s of [got.why!, got.scope_notes![0]!.text, got.review_pointers![0]!]) {
      assert.ok(!s.includes("\uFFFD"), "no code point was split");
    }
  });

  it("keeps at most the first 50 valid entries of every list", () => {
    const many = (n: number) => Array.from({ length: n }, (_, i) => `item ${i}`);
    const got = prSummaryOf({
      changes: ["", ...many(60)],
      verification: many(60).map((command) => ({ command, result: "pass" })),
      scope_notes: many(60).map((text) => ({ kind: "changed", text })),
      review_pointers: many(51),
    })!;
    assert.deepStrictEqual(got.changes, many(50), "a dropped entry does not count toward the cap");
    assert.equal(got.verification!.length, 50);
    assert.equal(got.scope_notes!.length, 50);
    assert.deepStrictEqual(got.review_pointers, many(50));
  });

  it("ignores pr_summary on a subagent frame (main-thread only, like summary)", () => {
    for (const extra of [
      { subagent_type: "coder", parent_tool_use_id: "toolu_parent" },
      { subagent_type: "coder" },
      { parent_tool_use_id: "toolu_parent" },
    ]) {
      assert.deepStrictEqual(scanSignals(doneFrame({ pr_summary: PR_SUMMARY_INPUT }, extra)), {});
    }
  });

  it("never throws on hostile shapes", () => {
    // Tool input arrives as parsed JSON, so the guarantee pinned here is for JSON-shaped garbage.
    for (const bad of [{ changes: [[], {}, null] }, { verification: [null, [], "x"] }, { scope_notes: [1, "a"] }]) {
      assert.doesNotThrow(() => scanSignals(doneFrame({ pr_summary: bad })));
    }
  });
});

describe("pr_summary tool schemas (PRD #1798 M2)", () => {
  const doneShape = (server: unknown): Record<string, z.ZodTypeAny> => {
    const tools = (server as { instance?: { _registeredTools?: Record<string, { inputSchema?: unknown }> } }).instance
      ?._registeredTools;
    const shape = (tools?.["signal_done"]?.inputSchema as { shape?: Record<string, z.ZodTypeAny> } | undefined)?.shape;
    assert.ok(shape, "expected a zod object schema on signal_done");
    return shape!;
  };

  it("offers an optional pr_summary on the Claude signal_done in every option set", () => {
    for (const opts of [{}, { prdDonePath: true, milestones: true, reportOnly: true }]) {
      const shape = doneShape(buildSignalMcpServer(opts));
      assert.ok("pr_summary" in shape);
      assert.equal(shape["pr_summary"]!.safeParse(undefined).success, true, "the whole object is optional");
      assert.equal(shape["pr_summary"]!.safeParse({}).success, true, "every member is optional");
      assert.equal(shape["pr_summary"]!.safeParse(PR_SUMMARY_EXPECTED).success, true);
    }
  });

  it("offers the same pr_summary members on the Codex signal_done dynamic tool", () => {
    const spec = buildCodexDynamicTools({
      role: "lead",
      phase: "implement",
      allowedTools: new Set(["signal_done"]),
      allowedSkills: new Set(),
      isRoot: true,
    }).find((s) => s.name === "signal_done");
    const props = (spec?.inputSchema.properties ?? {}) as Record<string, { properties?: Record<string, unknown>; required?: unknown }>;
    const pr = props["pr_summary"];
    assert.ok(pr, "pr_summary is on the Codex schema");
    assert.equal(pr.required, undefined, "no member is required");
    assert.deepStrictEqual(Object.keys(pr.properties ?? {}).sort(), Object.keys(PR_SUMMARY_EXPECTED).sort());
    assert.equal((spec?.inputSchema.required as unknown) ?? undefined, undefined, "signal_done still needs no argument");
  });
});

describe("RunTurnReducerImpl pr_summary fold (PRD #1798 M2)", () => {
  const noContext: HarnessContextHook = {
    request() {},
    async get(): Promise<HarnessContext | undefined> {
      return undefined;
    },
  };
  const frame = (signals: Partial<TurnSignals>): HarnessEvent => ({
    kind: "frame",
    origin: { kind: "main" },
    attribution: {},
    items: [],
    signals,
  });

  it("is last-wins within a turn, keeps an earlier claim over a bare signal_done, and resets per turn", async () => {
    const r = new RunTurnReducerImpl(noContext);
    r.beginTurn();
    await r.accept(frame({ done: true, prSummary: { what: "first" } }));
    await r.accept(frame({ done: true, prSummary: { what: "second" } }));
    await r.accept(frame({ done: true }));
    assert.deepStrictEqual(r.finish({ kind: "exhausted" }).result.prSummary, { what: "second" });
    r.beginTurn();
    await r.accept(frame({ done: true }));
    assert.equal(r.finish({ kind: "exhausted" }).result.prSummary, undefined);
  });

  it("folds exactly what scanSignals produced for the fixture", async () => {
    const r = new RunTurnReducerImpl(noContext);
    r.beginTurn();
    await r.accept(frame(scanSignals(doneFrame({ pr_summary: PR_SUMMARY_INPUT })) as Partial<TurnSignals>));
    const result = r.finish({ kind: "exhausted" }).result;
    assert.equal(result.done, true);
    assert.deepStrictEqual(result.prSummary, PR_SUMMARY_EXPECTED);
  });
});

describe("verifiedAtSha stamp (PRD #1798 M2)", () => {
  it("normalizes only a 40-hex id, lowercased", () => {
    assert.equal(normalizeVerifiedSha("ABCDEF0123456789abcdef0123456789ABCDEF01\n"), "abcdef0123456789abcdef0123456789abcdef01");
    for (const bad of [undefined, 1, "", "abc123", "g".repeat(40), "a".repeat(64), "a".repeat(39)]) {
      assert.equal(normalizeVerifiedSha(bad), undefined, String(bad));
    }
  });

  it("reads the worktree HEAD of a real repository", async () => {
    const { dir, head, root } = makeGitRepo();
    try {
      assert.equal(await readWorktreeHeadSha(dir), head);
    } finally {
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("returns undefined (never throws) for a missing directory or a repo with no commit", async () => {
    const empty = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-prsum-empty-"));
    const previousCeiling = process.env.GIT_CEILING_DIRECTORIES;
    try {
      process.env.GIT_CEILING_DIRECTORIES = path.dirname(fs.realpathSync(empty));
      assert.equal(await readWorktreeHeadSha(path.join(empty, "missing")), undefined);
      assert.equal(await readWorktreeHeadSha(empty), undefined, "not a repository");
    } finally {
      if (previousCeiling === undefined) delete process.env.GIT_CEILING_DIRECTORIES;
      else process.env.GIT_CEILING_DIRECTORIES = previousCeiling;
      fs.rmSync(empty, { recursive: true, force: true });
    }
  });

  it("stamps a copy of the claim, replaces a stale stamp, and skips git when there is no claim", async () => {
    const claim = { what: "w", verifiedAtSha: "f".repeat(40) };
    const stamped = await stampPrSummaryHead(claim, "/wt", async () => "A".repeat(40));
    assert.deepStrictEqual(stamped, { what: "w", verifiedAtSha: "a".repeat(40) });
    assert.equal(claim.verifiedAtSha, "f".repeat(40), "the input is not mutated");
    assert.deepStrictEqual(await stampPrSummaryHead(claim, "/wt", async () => undefined), { what: "w" });
    assert.deepStrictEqual(await stampPrSummaryHead(claim, "/wt", async () => { throw new Error("git"); }), { what: "w" });
    let called = false;
    assert.equal(await stampPrSummaryHead(undefined, "/wt", async () => { called = true; return "a".repeat(40); }), undefined);
    assert.equal(called, false);
  });
});

describe("implement prompt pr_summary ask (PRD #1798 M2)", () => {
  it("asks for plain, behaviour-level claims and only verification that ran, on every implement turn", () => {
    for (const first of [true, false]) {
      const p = buildImplementPrompt({ branch: "b", subagentNames: ["coder"], first, iteration: first ? 1 : 2 });
      assert.ok(p.includes(PR_SUMMARY_GUIDANCE));
      assert.ok(p.indexOf("call the `signal_done` tool exactly once") < p.indexOf("pr_summary"), "it follows the signal_done line");
    }
    assert.match(PR_SUMMARY_GUIDANCE, /behaviour-level/);
    assert.match(PR_SUMMARY_GUIDANCE, /ONLY the checks\s+you actually ran/);
  });
});

describe("issue #1783 × PRD #1798: the pr_summary HEAD read is a marked, transport-pinned runner git", () => {
  it("passes only the named discovery ceiling alongside the worker mark and transport pins", () => {
    const previousCeiling = process.env.GIT_CEILING_DIRECTORIES;
    try {
      for (const ceiling of [undefined, "", path.dirname(fs.realpathSync(os.tmpdir()))]) {
        if (ceiling === undefined) delete process.env.GIT_CEILING_DIRECTORIES;
        else process.env.GIT_CEILING_DIRECTORIES = ceiling;
        const env = worktreeHeadShaEnv();
        assert.equal(env.GIT_CEILING_DIRECTORIES, ceiling, "the named ceiling is passed through exactly");
        assert.equal(Object.hasOwn(env, "GIT_CEILING_DIRECTORIES"), ceiling !== undefined);
        assert.ok(env[WORKER_SPAWN_ENV], "worker-marked: a concurrent quiescence scan must not read it as run residue");
        assert.equal(env.GIT_ALLOW_PROTOCOL, "none", "every transport pinned off, independent of the lazy-fetch pin");
        assert.equal(env.GIT_NO_LAZY_FETCH, "1");
      }
    } finally {
      if (previousCeiling === undefined) delete process.env.GIT_CEILING_DIRECTORIES;
      else process.env.GIT_CEILING_DIRECTORIES = previousCeiling;
    }
    assert.equal(worktreeHeadShaEnv().GIT_CEILING_DIRECTORIES, previousCeiling, "the prior ceiling is restored");
  });
});
