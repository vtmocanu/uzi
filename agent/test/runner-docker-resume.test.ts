import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { RunContext } from "../src/executor.js";
import { fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

describe("Docker scratch resume runner context", () => {
  const cases = [
    { name: "Docker cold generation 2", dockerHost: "tcp://docker:2375", generation: 2, session: null, expected: true },
    { name: "Docker fresh generation 1", dockerHost: "tcp://docker:2375", generation: 1, session: null, expected: false },
    { name: "plain worker generation 2", dockerHost: undefined, generation: 2, session: null, expected: false },
    { name: "plain worker session resume", dockerHost: undefined, generation: 2, session: "prior-session", expected: false },
    { name: "Docker legacy session without generation", dockerHost: "tcp://docker:2375", generation: undefined, session: "prior-session", expected: true },
    { name: "Docker legacy session generation 0", dockerHost: "tcp://docker:2375", generation: 0, session: "prior-session", expected: true },
    { name: "Docker legacy fresh without generation", dockerHost: "tcp://docker:2375", generation: undefined, session: null, expected: false },
  ];

  for (const c of cases) {
    it(c.name, async () => {
      const claim = gitlabClaim(42, { claim_generation: c.generation, session_id: c.session });
      let captured: RunContext | undefined;
      const { gitlab } = fakeGitlab();
      await runnerWith(() => ({
        executor: {
          async run(ctx: RunContext) {
            captured = ctx;
            throw new Error("intentional stop after context capture");
          },
        },
      }), gitlab, undefined, undefined, {
        dockerHost: c.dockerHost,
        // Context-only test: runner cleanup must never contact a Docker endpoint.
        quiesceRun: async () => ({
          process: { state: "quiescent", processes: [], killed: [], detail: "context-only fixture" },
          docker: { state: "not_wired", removed: [], detail: "context-only fixture" },
        }),
      }).execute(claim);
      assert.ok(captured, "the real runner reached the executor");
      assert.equal(captured.dockerScratchResume, c.expected);
      assert.equal(captured.resumed, c.session != null, "legacy resumed remains session-only");
    });
  }
});
