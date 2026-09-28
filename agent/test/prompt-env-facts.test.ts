// issue #1866 M1: the environment-facts block and its placement in the plan/implement prompts.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { buildEnvironmentFactsBlock, buildImplementPrompt, buildPlanPrompt } from "../src/prompt.js";
import type { EnvFacts } from "../src/env-probe.js";

const RULE =
  "When a specific gate is blocked by a verified environment limit, do not repeat it unchanged. Record it as not run or blocked, run the checks that remain valid, and name the CI or other test lane that must complete validation.";

const ALL_OK: EnvFacts = { harness: "claude", dockerWired: true, proc: "ok", home: "ok", tmp: "ok" };

describe("buildEnvironmentFactsBlock", () => {
  it("is empty without facts", () => {
    assert.equal(buildEnvironmentFactsBlock(undefined), "");
  });
  it("is empty when everything is ok and Docker is wired, on both harnesses", () => {
    assert.equal(buildEnvironmentFactsBlock(ALL_OK), "");
    assert.equal(buildEnvironmentFactsBlock({ ...ALL_OK, harness: "codex" }), "");
  });
  it("lists only the limited or unverified facts", () => {
    const b = buildEnvironmentFactsBlock({ ...ALL_OK, proc: "limited", tmp: "unverified" });
    assert.match(b, /^Environment facts for this run \(measured at run start by a fixed probe under your uid on the claude harness; a Bash tool call may differ/);
    assert.ok(b.includes("- /proc cannot be enumerated from your commands."));
    assert.ok(b.includes("- $TMPDIR writability: not verified."));
    assert.ok(!b.includes("$HOME"));
    assert.ok(!b.includes("Docker is not wired"));
    const bullets = b.split("\n").filter((l) => l.startsWith("- "));
    assert.equal(bullets.length, 2);
  });
  it("words each status", () => {
    const b = buildEnvironmentFactsBlock({ ...ALL_OK, proc: "unverified", home: "limited", tmp: "limited" });
    assert.ok(b.includes("- /proc enumeration: not verified (the probe failed or timed out)."));
    assert.ok(b.includes("- $HOME is not writable."));
    assert.ok(b.includes("- $TMPDIR is not writable."));
    const u = buildEnvironmentFactsBlock({ ...ALL_OK, home: "unverified" });
    assert.ok(u.includes("- $HOME writability: not verified."));
  });
  it("renders the Docker line as worker configuration", () => {
    const b = buildEnvironmentFactsBlock({ ...ALL_OK, dockerWired: false });
    assert.ok(b.includes("- Docker is not wired on this worker (worker configuration, not a probe)."));
    assert.equal(b.split("\n").filter((l) => l.startsWith("- ")).length, 1);
  });
  it("never mentions egress", () => {
    const b = buildEnvironmentFactsBlock({ harness: "codex", dockerWired: false, proc: "limited", home: "limited", tmp: "unverified" });
    assert.ok(!/egress/i.test(b));
    const c = buildEnvironmentFactsBlock({ harness: "claude", dockerWired: false, proc: "unverified", home: "unverified", tmp: "limited" });
    assert.ok(!/egress/i.test(c));
  });
  it("tells a codex lead that $HOME / $TMPDIR are private per command and not persistent", () => {
    const b = buildEnvironmentFactsBlock({ ...ALL_OK, harness: "codex", home: "limited", tmp: "limited" });
    assert.match(b, /through your command sandbox on the codex harness/);
    assert.ok(b.includes("- A command's own private $HOME is not writable."));
    assert.ok(b.includes("- A command's own private $TMPDIR is not writable."));
    assert.match(b, /each command gets its own private \$HOME and \$TMPDIR, which do not persist between commands/);
    // No home/tmp line ⇒ no persistence note.
    const p = buildEnvironmentFactsBlock({ ...ALL_OK, harness: "codex", proc: "limited" });
    assert.ok(!p.includes("do not persist"));
  });
  it("carries the rule verbatim on one line, then the plan-section guidance", () => {
    const lines = buildEnvironmentFactsBlock({ ...ALL_OK, proc: "limited" }).split("\n");
    assert.ok(lines.includes(RULE));
    assert.equal(
      lines[lines.length - 1],
      "A plan needs an **Environment limits** section only when one of these facts actually affects its planned validation. A missing /proc alone does not prove a gate is blocked: tests may skip explicitly while CI enforces them.",
    );
    assert.ok(lines.length <= 8);
  });
});

const PLAN_BASE = {
  issueIid: 7,
  issueTitle: "Fix login",
  issueDescription: "desc",
  branch: "agent/issue-7",
  subagentNames: ["coder", "reviewer"],
};

describe("buildPlanPrompt environment facts", () => {
  it("is byte-identical without facts and with all-ok facts", () => {
    assert.equal(buildPlanPrompt({ ...PLAN_BASE, environmentFacts: ALL_OK }), buildPlanPrompt(PLAN_BASE));
  });
  it("renders the block after the deps note when a limit exists", () => {
    const facts: EnvFacts = { ...ALL_OK, proc: "limited" };
    const p = buildPlanPrompt({ ...PLAN_BASE, environmentFacts: facts });
    const block = buildEnvironmentFactsBlock(facts);
    assert.ok(p.includes(`\n\n${block}\n\n`));
    const without = buildPlanPrompt(PLAN_BASE);
    assert.equal(p.replace(`\n\n${block}`, ""), without);
  });
});

describe("buildImplementPrompt environment facts", () => {
  const facts: EnvFacts = { ...ALL_OK, harness: "claude", home: "limited" };
  const base = { branch: "agent/issue-7", subagentNames: ["coder"], iteration: 1 };
  it("carries the block on the first turn", () => {
    const p = buildImplementPrompt({ ...base, first: true, environmentFacts: facts });
    assert.ok(p.includes(buildEnvironmentFactsBlock(facts)));
    assert.ok(p.includes(RULE));
  });
  it("omits it on later turns", () => {
    const p = buildImplementPrompt({ ...base, first: false, iteration: 2, environmentFacts: facts });
    assert.ok(!p.includes("Environment facts for this run"));
    assert.equal(p, buildImplementPrompt({ ...base, first: false, iteration: 2 }));
  });
  it("is byte-identical on the first turn without facts and with all-ok facts", () => {
    assert.equal(
      buildImplementPrompt({ ...base, first: true, environmentFacts: ALL_OK }),
      buildImplementPrompt({ ...base, first: true }),
    );
  });
});
