import assert from "node:assert/strict";
import type { RunContext, ExecutorResult } from "../src/executor.js";
import type { MilestoneProgress } from "../src/protocol.js";

export const dependencyWaitPlan = "M1: implement and checkpoint the independent slice.\nM2: integrate only after the dependency is available.";
export const dependencyWaitQuestion = "Is the dependency ready for M2?";
export const dependencyWaitAnswer = "Still waiting on dependency; stay parked; keep M1 intact";
export const dependencyWaitProgress = { completed: ["m1"], in_progress: [] };
export const dependencyWaitMilestones = [
  { id: "m1", title: "Independent slice" },
  { id: "m2", title: "Dependency integration" },
];

export interface WaitSurface {
  continuation: string;
  advertisedTools: unknown;
}
export interface WaitObservation {
  error: string | null;
  paused: boolean;
  providerCalls: number;
  iterations: number[];
}

/**
 * M3 can supply a chooser that inspects the actual tools and continuation before
 * returning a control call. Without a chooser, every wait response is prose.
 * Executor iteration limits bound playback; there are no timers or retries here.
 */
export function scriptedDependencyWait(
  wording: "identical" | "reworded",
  chooseControl?: (surface: WaitSurface) => { tool: string; input: Record<string, unknown> } | undefined,
) {
  const surfaces: WaitSurface[] = [];
  return {
    surfaces,
    next(surface: WaitSurface) {
      surfaces.push(surface);
      const control = chooseControl?.(surface);
      return control ?? {
        text: wording === "identical"
          ? "Waiting on the dependency; M1 remains checkpointed."
          : `Dependency is still unavailable (update ${surfaces.length}); preserving completed M1.`,
      };
    },
  };
}

export function dependencyWaitCallbacks() {
  const asked: string[][] = [];
  const checkpoints: Parameters<NonNullable<RunContext["checkpoint"]>>[0][] = [];
  const iterations: number[] = [];
  const progress: (MilestoneProgress | undefined)[] = [];
  const callbacks: Partial<RunContext> = {
    planApproved: true,
    approvedPlan: dependencyWaitPlan,
    frozenMilestones: dependencyWaitMilestones,
    config: { max_iterations: 8 },
    worktreeFingerprint: async () => "M1-CHECKPOINT-UNCHANGED",
    askUser: async (questions) => {
      asked.push(questions.map((q) => q.question));
      return { kind: "answer", answers: [dependencyWaitAnswer] };
    },
    checkpoint: async (opts) => { checkpoints.push(opts); },
    reportIteration: async (iteration, value) => {
      iterations.push(iteration);
      progress.push(value);
    },
    recordCompletionAttempt: async () => {
      assert.fail("waiting must never attempt completion");
    },
    enterCompletionHold: async () => {
      assert.fail("pre-completion waiting must never enter a completion hold");
    },
  };
  return {
    callbacks, asked, checkpoints, iterations,
    assertPreserved() {
      assert.deepEqual(asked, [[dependencyWaitQuestion]]);
      assert.ok(checkpoints.some((c) => c.reap && JSON.stringify(c.progress) === JSON.stringify(dependencyWaitProgress)));
      assert.ok(progress.length > 1);
      for (const value of progress.slice(1)) {
        assert.deepEqual(value?.completed, dependencyWaitProgress.completed);
        assert.deepEqual(value?.in_progress, dependencyWaitProgress.in_progress);
      }
    },
  };
}

export async function observeDependencyWait(
  run: Promise<ExecutorResult>,
  counts: () => { providerCalls: number; iterations: number[] },
): Promise<WaitObservation> {
  try {
    const result = await run;
    return { error: null, paused: result.pausedAt !== undefined, ...counts() };
  } catch (error) {
    assert.ok(error instanceof Error);
    return { error: error.message, paused: false, ...counts() };
  }
}

/** Standalone red probe and M3 can share this behavioural expectation. */
export function expectDependencyWaitPaused(observation: WaitObservation) {
  assert.equal(observation.error, null, "dependency waiting must not fail the run");
  assert.equal(observation.paused, true, "dependency waiting must return paused");
}
