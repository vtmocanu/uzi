import assert from "node:assert/strict";
import { describe, it } from "node:test";
import type { ExecutorFactory } from "../src/runner.js";
import { fakeGitlab, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// issue #4066 - the worker-level shutdown barrier has its own 35s budget (main.ts) and must not move the runner's
// existing per-run publication budget: runner.shutdown() still derives shutdownDeadlineAt once from
// min(codexBoundaryDeadlineMs, shutdownPublishTimeoutMs) with the 15s default, and a repeated call changes nothing.

interface RunnerShutdownState { shutdownPublishTimeoutMs: number; shutdownDeadlineAt?: number; codexBoundaryDeadlineMs: number }

describe("issue #4066: the runner's shutdown budget is unchanged", () => {
  it("keeps the 15s publication default and fixes shutdownDeadlineAt at the first shutdown() only", () => {
    const { gitlab } = fakeGitlab();
    const unused: ExecutorFactory = () => { throw new Error("no run is started"); };
    const runner = runnerWith(unused, gitlab);
    const state = runner as unknown as RunnerShutdownState;
    assert.equal(state.shutdownPublishTimeoutMs, 15_000, "the default publication budget is still 15s");
    assert.equal(state.shutdownDeadlineAt, undefined, "nothing is fixed before the first shutdown");

    const before = Date.now();
    runner.shutdown();
    const first = state.shutdownDeadlineAt;
    assert.ok(first !== undefined);
    const budget = Math.min(state.codexBoundaryDeadlineMs, state.shutdownPublishTimeoutMs);
    assert.ok(first >= before + budget && first <= Date.now() + budget, "derived from the runner's own budget, not the worker's 35s");
    assert.ok(budget <= 15_000);

    runner.shutdown();
    runner.shutdown();
    assert.equal(state.shutdownDeadlineAt, first, "a repeated shutdown() resets nothing");
    assert.equal(state.shutdownPublishTimeoutMs, 15_000);
  });
});
