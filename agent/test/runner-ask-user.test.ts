import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { FakeApi } from "./fake-api.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { makeClaim, nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import {
  StubExecutor,
  STUB_ASK_SENTINEL,
  type Executor,
  type RunContext,
} from "../src/executor.js";
import { GitLabClient, type FetchFn } from "../src/forge.js";
import { RunRunner, REASON_QUESTION_TIMEOUT } from "../src/runner.js";
import { SteeringChannel, type AnswerVerdict } from "../src/steering.js";
import type { AskUserQuestion, UserInput } from "../src/protocol.js";

/**
 * PRD #88 M6 — the `RunRunner` layer of the clarification park.
 *
 * Deliberately a separate file from `runner.test.ts`, and deliberately NOT in
 * `ask-user.test.ts`. D-S: every M1 coverage finding converged on one layer —
 * `RunRunner.askUser` is where the mechanism lives and where nothing tested it. A
 * `SteeringChannel` test cannot observe its caller, so a bare-channel fixture is the
 * wrong layer by construction no matter what the test is named. The screen applied
 * to every test here is the reviewer's: *what would I change in `runner.ts` to make
 * this fail?* Each test names its mutation.
 *
 * D-W — THE BOUNDED DEADLINE BELOW IS PART OF THE INSTRUMENT, NOT A SPEED KNOB.
 * Every mutation in this file breaks the park by making it never resolve, because a
 * park blocks by design. On the 24h production default that surfaces as a wall-clock
 * TEST TIMEOUT — and `node --test` prints `ℹ fail 0` while timing out (CLAUDE.md,
 * measured twice on PRD #121: the exit code tells the truth, the tally lies). A
 * mutation control that "reddens" by timing out is precisely the reading this repo
 * warns about. Bounding the deadline converts every one of those hangs into a NAMED
 * failure: the run fails with REASON_QUESTION_TIMEOUT and the assertion that fires
 * says which property broke.
 */

const TOKEN = "tkn-askuser-123456";

/** The answer deadline every run here is given. Long enough that a correct
 *  implementation never reaches it while an answer is actually being delivered
 *  (the steering channel polls every 5ms), short enough that a broken park fails
 *  as REASON_QUESTION_TIMEOUT inside the test rather than hanging. See D-W. */
const QUESTION_TIMEOUT_MS = 600;

let api: FakeApi;
let fx: Fixture;
let git: GitCache;
let client: WorkerClient;

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  const baseUrl = await api.listen();
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger());
  client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
    sleep: async () => {},
    terminalRetrySchedule: [1, 1],
  });
});

afterEach(async () => {
  await api.close();
  fx.cleanup();
});

/** A GitLab client whose transport is captured; opens MR !42 with no network. */
function fakeGitlab(): GitLabClient {
  const fetchFn: FetchFn = async () => ({
    status: 201,
    text: async () =>
      JSON.stringify({
        iid: 42,
        web_url: "https://gitlab.example.test/org/repo/-/merge_requests/42",
      }),
  });
  return new GitLabClient({ fetchFn });
}

function runnerFor(
  executor: Executor,
  opts: { questionTimeoutMs?: number } = {},
): RunRunner {
  return new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0, // disabled — the gate resolves from injected inputs
    questionTimeoutMs: opts.questionTimeoutMs ?? QUESTION_TIMEOUT_MS,
    gitlab: fakeGitlab(),
  });
}

const claimFor = (iid: number, overrides = {}) =>
  makeClaim({
    issue_iid: iid,
    issue_title: `Fix thing ${iid}`,
    repo: {
      id: "r1",
      url: "https://gitlab.example.test/org/repo",
      clone_url: fx.originPath,
    },
    last_seq: 0,
    secrets: { forge_pat: "fixture-forge-pat-000000" },
    ...overrides,
  });

function input(kind: UserInput["kind"], body?: string): UserInput {
  return { id: 1, kind, body: body ?? null };
}

/** An `answer` steering input in the wire shape submitAnswer persists (D-P C2):
 *  JSON `{question_id, answers}`, never bare prose. */
function answerInput(questionId: string, ...answers: string[]): UserInput {
  return input("answer", JSON.stringify({ question_id: questionId, answers }));
}

/** The question ids the run reported, in park order, read back off the /state
 *  reports rather than guessed — the worker mints them. */
function parkedQuestionIds(runId: string): string[] {
  return api.states
    .filter((s) => s.runId === runId && s.body.status === "awaiting_input")
    .map((s) => s.body.open_question_id ?? "");
}

function questionMessageIds(runId: string): string[] {
  return api
    .messages(runId)
    .filter((m) => m.kind === "question")
    .map((m) => (m.payload as { question_id?: string }).question_id ?? "");
}

function statuses(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
}

function failureReason(runId: string): string {
  return (
    api.states
      .filter((s) => s.runId === runId && s.body.status === "failed")
      .map((s) => s.body.failure_reason ?? "")
      .at(-1) ?? ""
  );
}

/** Commit a marker so the runner has something to push, exactly as StubExecutor
 *  does. gpg signing forced off so a host signing config cannot block it. */
function commitMarker(worktreePath: string, label: string): void {
  fs.writeFileSync(path.join(worktreePath, "UZI_RUN.md"), `# ${label}\n`);
  execFileSync("git", ["add", "UZI_RUN.md"], { cwd: worktreePath });
  execFileSync(
    "git",
    [
      "-c",
      "user.name=uzi-agent",
      "-c",
      "user.email=uzi-agent@uzi.local",
      "-c",
      "commit.gpgsign=false",
      "commit",
      "-m",
      `uzi test: ${label}`,
    ],
    { cwd: worktreePath },
  );
}

interface AskLog {
  asked: AskUserQuestion[][];
  verdicts: AnswerVerdict[];
}

/**
 * An executor that asks `count` questions in sequence and then commits. The stub
 * executor asks exactly ONCE (`executor.ts`, pre-gate), so the two-park scenarios —
 * the per-run deadline and the stale-answer discard — are unreachable through it and
 * need this vehicle instead.
 */
function askingExecutor(count: number, log: AskLog): Executor {
  return {
    async run(ctx: RunContext) {
      for (let i = 1; i <= count; i++) {
        const qs: AskUserQuestion[] = [
          { question: `question ${i}?`, header: `Q${i}` },
        ];
        log.asked.push(qs);
        log.verdicts.push(await ctx.askUser!(qs));
      }
      commitMarker(ctx.worktreePath, `asked ${count}`);
      return { branch: ctx.branch };
    },
  };
}

describe("PRD #88 M6 — RunRunner.askUser", () => {
  /**
   * D-S mutation 1: `steering.bumpEpoch()` inserted into `RunRunner.askUser`.
   *
   * Triaged at `39cffd4c` and re-confirmed here: the shipped code is clean (the sole
   * `bumpEpoch` call site in runner.ts is inside `gatePlan`, guarded by `gatedRuns`),
   * so this closes a COVERAGE gap rather than fixing a defect.
   *
   * The channel is constructed inside `RunRunner.execute` and `RunnerOptions` carries
   * no override, so the instance is captured through a prototype hook on `start()`
   * rather than by adding a production seam that would exist only for this test.
   *
   * WHY THE ASSERTION IS ON THE EPOCH AND NOT ON AN END STATE. The tempting
   * end-to-end version — buffer an `approve_plan` and show a bumped epoch strands it
   * as stale — does NOT discriminate, and this was measured rather than assumed:
   * `route()` stamps an input with the epoch CURRENT WHEN IT ARRIVES, and the first
   * `gatePlan` does not bump (`gatedRuns` is empty), so an approve arriving after the
   * park is stamped at the bumped epoch and matches the gate anyway. The run would
   * complete under the mutation. The epoch value itself is the only thing that moves.
   */
  it("leaves the plan-gate epoch untouched across a park and an answer", async () => {
    const claim = claimFor(41, {
      issue_description: `clarify first: ${STUB_ASK_SENTINEL}`,
    });
    let captured: SteeringChannel | undefined;
    const origStart = SteeringChannel.prototype.start;
    SteeringChannel.prototype.start = function (
      this: SteeringChannel,
    ): void {
      // Capturing `this` IS the fixture: the test patches the prototype precisely to
      // get hold of the instance the runner constructs. An arrow function cannot do
      // it, since it would not receive a `this` to capture (PRD #103 M3, oxlint
      // typescript/no-this-alias).
      // eslint-disable-next-line typescript/no-this-alias
      captured = this;
      return origStart.call(this);
    };
    try {
      api.onState(claim.run_id, (body) => {
        if (body.status === "awaiting_input" && body.open_question_id) {
          api.setInputs(claim.run_id, [
            answerInput(body.open_question_id, "proceed"),
            input("approve_plan"),
          ]);
        }
      });
      await runnerFor(
        new StubExecutor(nullLogger(), { planGate: true }),
      ).execute(claim);
    } finally {
      SteeringChannel.prototype.start = origStart;
    }

    // Positive control: the run really did park and really did gate, so the epoch
    // being 0 is a fact about an exercised path, not about a run that did nothing.
    assert.ok(captured, "the runner must have started a steering channel");
    assert.deepStrictEqual(
      statuses(claim.run_id).filter(
        (s) => s === "awaiting_input" || s === "awaiting_approval",
      ),
      ["awaiting_input", "awaiting_approval"],
      "the run must park on the question AND open the plan gate",
    );
    assert.strictEqual(statuses(claim.run_id).at(-1), "completed");
    assert.strictEqual(
      captured!.currentEpoch(),
      0,
      "a clarification park must not disturb the plan-gate epoch: gatePlan decides " +
        '"first gate does not bump" from gatedRuns, not from the epoch value, so a ' +
        "bump on the question path would shift the first plan gate off epoch 0 " +
        "while that set still says first gate (D-F)",
    );
  });

  /**
   * D-S mutation 2 / D-E CASE 2 (the HONOURED half of the mirror pair): replace the
   * `openQuestionIds.get(runId)` reuse in `askUser` with a fresh `randomUUID()`.
   *
   * The scenario is the ordinary worker death, not an exotic one: the user answers,
   * the worker dies before consuming, the run is requeued, and the resumed worker's
   * steering channel — which `runner.ts` starts BEFORE the checkout — consumes the
   * answer minutes before the executor can re-park. Every clock- and
   * arrival-ordinal-keyed design failed exactly here, which is why the key is
   * identity: the server re-delivers the open question id on the claim, the re-park
   * re-uses it verbatim, and the pre-death answer still matches.
   *
   * Under the mutation the re-park mints a new id, `takeAnswerEvent` discards the
   * buffered answer as naming a different question, and the park runs to its
   * deadline — a user who answered correctly watches their answer evaporate.
   */
  it("honours an answer that arrived BEFORE a re-park, because the id is re-used", async () => {
    const seeded = "11111111-2222-3333-4444-555555555555";
    const claim = claimFor(42, {
      issue_description: `clarify first: ${STUB_ASK_SENTINEL}`,
      open_question_id: seeded,
    });
    // Written before the worker died; already waiting when this worker starts.
    api.setInputs(claim.run_id, [
      answerInput(seeded, "answered before the death"),
    ]);

    await runnerFor(new StubExecutor(nullLogger())).execute(claim);

    assert.deepStrictEqual(
      parkedQuestionIds(claim.run_id),
      [seeded],
      "a resumed worker must re-park on the SAME question id the claim carried, " +
        "never mint a new one (D-E case 2)",
    );
    assert.deepStrictEqual(
      questionMessageIds(claim.run_id),
      [seeded],
      "and the feed's question message must carry that same id",
    );
    assert.strictEqual(
      statuses(claim.run_id).at(-1),
      "completed",
      `the pre-death answer must resolve the re-park; run failed with ` +
        `"${failureReason(claim.run_id)}"`,
    );
    const echoed = api
      .messages(claim.run_id)
      .find((m) => m.kind === "answer");
    assert.deepStrictEqual(
      (echoed?.payload as { answers?: unknown })?.answers,
      ["answered before the death"],
      "and the lead must receive the text the user actually wrote",
    );
  });

  /**
   * D-E CASE 1 (the DISCARDED half of the mirror pair): a reply written against Q1
   * but consumed after the Q2 park must NOT resolve Q2.
   *
   * The pair is required together and neither half substitutes for the other: a
   * guard tightened enough to discard case 1 rejects case 2 unless it is keyed on
   * identity, and a guard loose enough to honour case 2 accepts case 1 unless it is
   * keyed on identity. A fixture holding only one cannot tell a correct
   * implementation from either failure direction.
   *
   * This asserts the WORKER-side discard specifically. The api rejects a stale
   * answer at submit too (`submitAnswer`'s ErrStaleAnswer, covered in the Go
   * suite), but that is a second, independent layer — the FakeApi here is a raw
   * input queue on purpose, so the worker's own guard is what is under test.
   */
  it("discards an answer naming the PREVIOUS question after a re-park on a new one", async () => {
    const log: AskLog = { asked: [], verdicts: [] };
    const claim = claimFor(43);
    let parks = 0;
    let firstId = "";
    api.onState(claim.run_id, (body) => {
      if (body.status !== "awaiting_input" || !body.open_question_id) return;
      parks++;
      if (parks === 1) {
        firstId = body.open_question_id;
        api.setInputs(claim.run_id, [answerInput(firstId, "answer to Q1")]);
      } else {
        // Stale: written against Q1, arriving while Q2 is the open question.
        api.setInputs(claim.run_id, [answerInput(firstId, "answer to Q1")]);
      }
    });

    await runnerFor(askingExecutor(2, log)).execute(claim);

    const ids = parkedQuestionIds(claim.run_id);
    assert.strictEqual(ids.length, 2, "the run must park twice");
    assert.notStrictEqual(
      ids[0],
      ids[1],
      "a genuinely new question gets a NEW id (only a re-park re-uses one)",
    );
    assert.strictEqual(
      statuses(claim.run_id).at(-1),
      "failed",
      "Q2 must not be resolved by an answer written against Q1",
    );
    assert.strictEqual(
      failureReason(claim.run_id),
      REASON_QUESTION_TIMEOUT,
      "and the run must die on its deadline with Q2 still unanswered, not " +
        "silently continue on the stale text",
    );
    assert.deepStrictEqual(
      log.verdicts.map((v) => (v.kind === "answer" ? v.answers : [v.kind])),
      [["answer to Q1"]],
      "the lead must have received exactly ONE answer — Q1's",
    );
  });

  /**
   * D-S mutation 4: make the deadline per-question instead of per-run, i.e. drop the
   * `questionDeadlines.get(runId)` reuse and compute `Date.now() + timeoutMs` at
   * every park.
   *
   * The documented bound is one budget for the RUN — N questions must not extend a
   * parked run indefinitely. A per-question clock is the plausible misreading, and
   * it is the shape a later "optimisation" would reach for.
   *
   * Timing-based, so the margins are wide and the measured numbers are reported
   * rather than a tight bound asserted. With a 1500ms budget and Q1 answered at
   * ~900ms, a correct run dies ~1500ms after the FIRST park; a per-question run gets
   * a fresh 1500ms at the second park and dies ~2400ms after the first. The
   * threshold sits between them with ~450ms of slack on each side.
   */
  it("spends ONE answer deadline across the whole run, not one per question", async () => {
    const budgetMs = 1500;
    const answerDelayMs = 900;
    const log: AskLog = { asked: [], verdicts: [] };
    const claim = claimFor(44);
    let parks = 0;
    let firstParkAt = 0;
    api.onState(claim.run_id, (body) => {
      if (body.status !== "awaiting_input" || !body.open_question_id) return;
      parks++;
      if (parks !== 1) return; // Q2 is deliberately never answered.
      firstParkAt = Date.now();
      const qid = body.open_question_id;
      setTimeout(
        () => api.setInputs(claim.run_id, [answerInput(qid, "answer to Q1")]),
        answerDelayMs,
      );
    });

    await runnerFor(askingExecutor(2, log), {
      questionTimeoutMs: budgetMs,
    }).execute(claim);
    const elapsedMs = Date.now() - firstParkAt;

    assert.strictEqual(parks, 2, "the run must park twice");
    assert.strictEqual(
      failureReason(claim.run_id),
      REASON_QUESTION_TIMEOUT,
      "the unanswered second question must expire the run",
    );
    assert.ok(
      elapsedMs < budgetMs + answerDelayMs / 2,
      `the whole run must die within ONE ${budgetMs}ms budget measured from the ` +
        `FIRST park, not ${budgetMs}ms from each park; measured ${elapsedMs}ms ` +
        `(a per-question deadline would land near ${budgetMs + answerDelayMs}ms)`,
    );
  });
});
