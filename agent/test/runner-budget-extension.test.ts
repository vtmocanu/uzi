import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { type Executor } from "../src/executor.js";
import type { IterationBudget, StateAck } from "../src/protocol.js";
import {
  client,
  fakeGitlab,
  gitlabClaim,
  installHarness,
  runner,
  simulateCommittedWork,
} from "./runner-harness.js";

installHarness();

// PRD #1189 M1 (D6) — RunRunner's reportIteration callback maps the served TOTAL wall off the
// running-report ACK (StateAck.budgetTotalSeconds, minted by client.reportState from
// budget_total_seconds) onto the IterationBudget the sdk-executor reads (b.totalWallSeconds), AND
// counts it in the "any field changed" gate so a run whose ACK carries ONLY the total still returns
// a non-undefined budget. This callback is constructed inside RunRunner.execute, so it is driven
// through a custom executor that calls ctx.reportIteration and captures its return; the ACK is
// shaped by intercepting client.reportState (the same seam runner-checkpoint.test.ts uses).

describe("RunRunner reportIteration budget-total mapping (PRD #1189 M1)", () => {
  it("maps budgetTotalSeconds onto IterationBudget.totalWallSeconds and marks the budget changed", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(88);
    // Inject the served TOTAL wall onto the turn-boundary running-report ACK only (a report that
    // carries iteration_count), delegating every other /state call to the real client.
    const origReportState = client.reportState.bind(client);
    (client as unknown as { reportState: unknown }).reportState = async (...args: unknown[]) => {
      const body = args[1] as { status?: string; iteration_count?: number } | undefined;
      const ack = (await (origReportState as (...a: unknown[]) => Promise<StateAck>)(...args)) as StateAck;
      if (body?.status === "running" && typeof body.iteration_count === "number") {
        return { ...ack, budgetTotalSeconds: 7200 };
      }
      return ack;
    };
    try {
      let captured: IterationBudget | undefined | void;
      const exec: Executor = {
        run: async (ctx) => {
          captured = await ctx.reportIteration!(1, undefined);
          simulateCommittedWork(); // non-empty diff so the run finalizes normally
          return { branch: ctx.branch };
        },
        killAgentTree: () => {},
      };
      await runner(exec, gitlab).execute(claim);

      assert.ok(captured, "an ACK carrying only the total still returns a budget (the changed gate counts it)");
      assert.strictEqual(
        (captured as IterationBudget).totalWallSeconds,
        7200,
        "ack.budgetTotalSeconds was mapped to IterationBudget.totalWallSeconds",
      );
      // The plain wall mapping is independent: no budget_wall_seconds was served, so it stays absent.
      assert.strictEqual(
        (captured as IterationBudget).wallSeconds,
        undefined,
        "totalWallSeconds is carried ALONGSIDE wallSeconds, not in place of it",
      );
    } finally {
      (client as unknown as { reportState: unknown }).reportState = origReportState;
    }
  });

  it("leaves totalWallSeconds absent when the ACK serves no total (back-compat)", async () => {
    // The back-compat control: an ordinary running report whose ACK serves no total must NOT
    // fabricate a totalWallSeconds — the executor keeps its current wall. (completedCount rides
    // every ACK since PRD #634, so `served` itself is non-undefined; the point here is that the
    // extension field is only present when the server actually serves it.)
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(89);
    let captured: IterationBudget | undefined | void;
    const exec: Executor = {
      run: async (ctx) => {
        captured = await ctx.reportIteration!(1, undefined);
        simulateCommittedWork();
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    await runner(exec, gitlab).execute(claim);
    assert.strictEqual(
      (captured as IterationBudget | undefined)?.totalWallSeconds,
      undefined,
      "no served total ⇒ totalWallSeconds absent, so the executor keeps its current wall",
    );
  });
});
