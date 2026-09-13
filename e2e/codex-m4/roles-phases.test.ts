// PRD #1287 C3 (D7 layer U) — roles and phases: IMMUTABLE grants decide, unknown input fails
// closed, valid allocated actions still run.
//
// The grants are the REAL render output (renderCodexRun / buildCodexRunPlan) for a realistic
// agents map, then fed to the REAL broker — so this ties the production grant DERIVATION to the
// production ENFORCEMENT, not a hand-built grant. The negative-effect oracles are the recording
// spawn/fileop spies and the delegate/skill handler counters; a denial is a zero call count, never
// denial text (D4).

import { before, describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  loadUModules,
  makeUBroker,
  agent,
  allow,
  runRequest,
  rt,
  type UModules,
} from "./harness-u.js";
import { recordEvidence } from "./evidence.js";
import { CODEX_U_PHASE_GRANTS_TITLE, CODEX_U_ROLE_FAILCLOSED_TITLE } from "./titles-c3.js";

import type { ToolHandler } from "../../agent/src/codex/broker.js";

let mods: UModules;
before(async () => {
  mods = await loadUModules();
});

describe("codex U roles and phases (real render grants → real broker)", () => {
  it(CODEX_U_PHASE_GRANTS_TITLE, async () => {
    // Plan phase: render a plan-turn run; its lead grants carry phase="plan".
    const planPlan = mods.buildCodexRunPlan(runRequest({ phase: "plan" }));
    assert.equal(planPlan.lead.grants.phase, "plan", "the render derived plan-phase lead grants");
    const planBroker = makeUBroker(mods, { grants: planPlan.lead.grants });
    const planWrite = await planBroker.broker.handleToolCall(rt(), "apply_patch", { path: "src/x.ts", content: "x" }, "root");
    assert.equal(planWrite.ok, false, "a plan-phase write is denied");
    if (!planWrite.ok) assert.equal(planWrite.code, "write_denied_in_plan", "denied because writes are not permitted in the plan phase");
    assert.equal(planBroker.fileop.ops.length, 0, "the plan-phase write never reached the fileop client");

    // Implement phase: render an implement-turn run; the SAME write is now permitted.
    const implPlan = mods.buildCodexRunPlan(runRequest({ phase: "implement" }));
    assert.equal(implPlan.lead.grants.phase, "implement", "the render derived implement-phase lead grants");
    const implBroker = makeUBroker(mods, { grants: implPlan.lead.grants });
    const implWrite = await implBroker.broker.handleToolCall(rt(), "apply_patch", { path: "src/x.ts", content: "x" }, "root");
    assert.equal(implWrite.ok, true, "the later implement-phase write is allowed");
    assert.deepEqual(implBroker.fileop.ops.map((o) => o.op), ["write"], "the implement-phase write reached the fileop client");

    recordEvidence(CODEX_U_PHASE_GRANTS_TITLE, "pass");
  });

  it(CODEX_U_ROLE_FAILCLOSED_TITLE, async () => {
    // A realistic run: one known subagent role, one granted lead skill.
    const plan = mods.buildCodexRunPlan(
      runRequest({
        phase: "implement",
        agents: { coder: agent({ tools: allow(["Read", "Bash"]) }) },
        leadSkills: ["debugging-skill"],
      }),
    );
    assert.ok(plan.allowedRoles.has("coder"), "the render exposed the known delegation role");
    assert.ok(plan.lead.grants.allowedSkills.has("debugging-skill"), "the render granted the lead skill");

    let delegatedRole: string | undefined;
    let skillLoaded: string | undefined;
    const toolHandlers = new Map<string, ToolHandler>([
      ["Skill", (args) => {
        skillLoaded = (args as { skill?: string }).skill;
        return Promise.resolve({ loaded: skillLoaded });
      }],
    ]);
    const h = makeUBroker(mods, {
      grants: plan.lead.grants,
      allowedRoles: plan.allowedRoles,
      toolHandlers,
      delegate: async (req) => {
        delegatedRole = req.role;
        return { ok: true, output: { child: "settled" } };
      },
    });

    // FAIL CLOSED: an unknown tool, a spoofed/unknown delegation role, a disallowed skill.
    const unknownTool = await h.broker.handleToolCall(rt(), "Frobnicate", {}, "root");
    assert.equal(unknownTool.ok, false, "an unknown tool is denied");
    if (!unknownTool.ok) assert.equal(unknownTool.code, "unknown_tool");

    const spoofedRole = await h.broker.handleToolCall(rt(), "spawn_agent", { subagent_type: "attacker" }, "root");
    assert.equal(spoofedRole.ok, false, "a spoofed/unknown delegation role is denied");
    if (!spoofedRole.ok) assert.equal(spoofedRole.code, "unknown_role");
    assert.equal(delegatedRole, undefined, "the unknown role never reached the delegate seam");

    const badSkill = await h.broker.handleToolCall(rt(), "Skill", { skill: "not-granted" }, "root");
    assert.equal(badSkill.ok, false, "a disallowed skill is denied");
    if (!badSkill.ok) assert.equal(badSkill.code, "denied_skill");
    assert.equal(skillLoaded, undefined, "the disallowed skill never reached the handler");

    // VALID ALLOCATED ACTIONS STILL RUN: a granted tool, a known-role delegation, a granted skill.
    const okBash = await h.broker.handleToolCall(rt(), "Bash", { command: "echo ok" }, "root");
    assert.equal(okBash.ok, true, "a granted tool runs");
    assert.equal(h.spawn.calls.length, 1, "the granted Bash reached the command spawn seam");

    const okDelegate = await h.broker.handleToolCall(rt(), "spawn_agent", { subagent_type: "coder" }, "root");
    assert.equal(okDelegate.ok, true, "a known-role delegation runs");
    assert.equal(delegatedRole, "coder", "the known role reached the delegate seam");

    const okSkill = await h.broker.handleToolCall(rt(), "Skill", { skill: "debugging-skill" }, "root");
    assert.equal(okSkill.ok, true, "a granted skill loads");
    assert.equal(skillLoaded, "debugging-skill", "the granted skill reached the handler");

    recordEvidence(CODEX_U_ROLE_FAILCLOSED_TITLE, "pass");
  });
});
