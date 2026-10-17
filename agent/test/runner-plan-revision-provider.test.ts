// Portable M3 reproducer: overlay this file and plan-revision-provider-fixture.ts,
// plus FakeApi compatibility, on b0893dce. No new production helper is imported.
import assert from "node:assert/strict";
import { it } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { api, client, fakeGitlab, installHarness, runner } from "./runner-harness.js";
import { feed, freshClaim, gates, newApi, row, send, statuses, until } from "./gate-revision-harness.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { providerFixture } from "./plan-revision-provider-fixture.js";

installHarness();
const SOURCE = "# Source plan";
const REVISED = "# Revised plan";
function barrier() {
  let release!: () => void;
  return { promise: new Promise<void>(resolve => { release = resolve; }), release: () => release() };
}
function enable() {
  newApi();
  api.planRevisionRetries = true;
  client.protocolFeatures = [...client.protocolFeatures, "plan_revision_retry_v1"];
}

it("M3 cancel racing an expired flight's replacement starts no replacement provider turn", async () => {
  enable();
  const fixture = providerFixture("claude", n => n === 1 ? { plan: SOURCE } : { failure: true });
  const claim = freshClaim(1, { config: { plan_max_revisions: 3 } });
  const id = claim.run_id;
  const r = runner(fixture.executor, fakeGitlab().gitlab, undefined, {
    planRevisionRetryPolicy: { redrives: 0, sleep: async (ms, signal) => { await delay(ms, undefined, { signal }); } },
  });
  const done = r.execute(claim);
  try {
    assert.ok(await until(() => gates(id).length === 1));
    const [i] = send(id, row("revise_plan", "I"));
    assert.ok(await until(() => feed(id).some(text => text.includes("retry allowance exhausted"))));
    const [j, cancel] = send(id, row("revise_plan", "J"), row("cancel"));
    await done;
    assert.equal(fixture.starts.length, 3);
    assert.equal(api.isApplied(id, i!.id), false);
    assert.equal(api.isApplied(id, j!.id), false);
    assert.ok(api.isAcked(id, cancel!.id));
    assert.equal(gates(id).length, 1);
  } finally { r.shutdown(); await done; }
});

for (const harness of ["claude", "codex"] as const) {
  for (const mode of (harness === "claude" ? ["revision", "answered continuation", "sessionless revision"] : ["revision", "answered continuation"])) {
    it(`M3 ${harness}: real 429 inner exhaustion redrives ${mode} without operator input`, async () => {
      enable();
      const answered = mode === "answered continuation";
      const sessionless = mode === "sessionless revision";
      const offset = answered ? 1 : 0;
      const fixture = providerFixture(harness, n => n === 1 ? { plan: SOURCE }
        : answered && n === 2 ? { ask: true }
        : n <= 3 + offset ? { failure: true } : { plan: REVISED }, sessionless);
      const claim = freshClaim(1, { config: { plan_max_revisions: 2 },
        ...(harness === "codex" ? { secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } } : {}) });
      const id = claim.run_id;
      const sleeping = barrier();
      const release = barrier();
      const waits: number[] = [];
      const r = runner(fixture.executor, fakeGitlab().gitlab, undefined, {
        questionTimeoutMs: 3000,
        planRevisionRetryPolicy: { random: () => 0, sleep: async (ms, signal) => {
          signal.throwIfAborted();
          waits.push(ms); sleeping.release(); await release.promise;
        } },
      });
      const done = r.execute(claim);
      try {
        assert.ok(await until(() => gates(id).length === 1), "source gate published");
        const source = api.gateOf(id);
        const [input] = send(id, row("revise_plan", "original I"));
        if (answered) {
          assert.ok(await until(() => api.states.some(s => s.runId === id && s.body.status === "awaiting_input")));
          const question = api.states.find(s => s.runId === id && s.body.status === "awaiting_input")!.body.open_question_id;
          send(id, row("answer", JSON.stringify({ question_id: question, answers: ["server"] })));
        }
        // Bounded observation also gives base a behavioral failure instead of hanging
        // on a barrier that the old production runner cannot reach.
        assert.ok(await until(() => waits.length > 0), `real provider exhaustion must enter runner redrive backoff: ${JSON.stringify({ starts: fixture.starts.length, states: api.states.filter(s => s.runId === id).map(s => s.body.status), feed: feed(id) })}`);
        await sleeping.promise;
        assert.equal(fixture.starts.length, 3 + offset, "both actual inner attempts finish before outer retry");
        assert.equal(api.isApplied(id, input!.id), false);
        assert.deepEqual(api.gateOf(id), source);
        assert.equal(statuses(id).includes("recovery_wait"), false);
        release.release();
        assert.ok(await until(() => gates(id).some(g => g.plan_md === REVISED)), "same flight publishes without another revise");
        assert.equal(fixture.starts.length, 4 + offset);
        assert.equal(waits.reduce((sum, ms) => sum + ms, 0), 5000);
        const failed = fixture.starts.slice(1 + offset);
        assert.equal(failed[0]!.prompt, failed[1]!.prompt);
        assert.equal(failed[1]!.prompt, failed[2]!.prompt, "outer retry preserves exact failed prompt");
        if (answered) assert.match(failed[2]!.prompt, /server/);
        if (harness === "claude") assert.equal(failed[2]!.resume, sessionless ? undefined : `session-${3 + offset}`, "latest failed session wins; absent sessions stay absent");
        const publication = gates(id).find(g => g.plan_md === REVISED)!;
        assert.equal(publication.revision_input_id, input!.id);
        assert.equal(publication.expected_gate_revision, source.revision);
        assert.ok(api.isApplied(id, input!.id));
        assert.equal(statuses(id).includes("recovery_wait"), false);
        send(id, row("cancel"));
        await done;
      } finally {
        release.release(); r.shutdown(); await done;
      }
    });
  }

  it(`M3 ${harness}: server-accepted replacement starts beyond the understated claim cap`, async () => {
    enable();
    let id: string;
    let iId: number;
    let jId: number;
    const fixture = providerFixture(harness, n => {
      if (n === 1) return { plan: SOURCE };
      if (n <= 3) return { failure: true };
      assert.equal(n, 4, "only J starts after I's two failed attempts");
      assert.equal(api.isApplied(id, iId), false, "I remains pending before J publishes");
      assert.equal(api.isApplied(id, jId), false);
      assert.equal(api.gateOf(id).revision, 1);
      return { plan: REVISED };
    });
    // The claim understates the server's current allowance. Both rows below are
    // source-bound admissions; the live replacement helper validates J.
    const claim = freshClaim(1, { config: { plan_max_revisions: 1 },
      ...(harness === "codex" ? { secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } } : {}) });
    id = claim.run_id;
    const r = runner(fixture.executor, fakeGitlab().gitlab, undefined, {
      planRevisionRetryPolicy: { redrives: 0, sleep: async (ms, signal) => { await delay(ms, undefined, { signal }); } },
    });
    const done = r.execute(claim);
    try {
      assert.ok(await until(() => gates(id).length === 1));
      const source = api.gateOf(id);
      const [i] = send(id, row("revise_plan", "exhausted I"));
      iId = i!.id;
      assert.ok(await until(() => feed(id).some(text => text.includes("retry allowance exhausted"))));
      assert.equal(fixture.starts.length, 3);
      const [j] = send(id, row("revise_plan", "accepted J"));
      jId = j!.id;
      assert.equal(j!.gate_revision, source.revision);
      assert.equal(j!.gate_binding, "bound");
      assert.ok(await until(() => gates(id).some(g => g.plan_md === REVISED)), "J must publish through the real driver beyond claim cap 1");
      assert.equal(fixture.starts.length, 4);
      assert.match(fixture.starts[3]!.prompt, /accepted J/);
      assert.doesNotMatch(fixture.starts[3]!.prompt, /exhausted I/);
      const publication = gates(id).find(g => g.plan_md === REVISED)!;
      assert.equal(publication.revision_input_id, jId);
      assert.equal(publication.expected_gate_revision, source.revision);
      assert.deepEqual(publication.superseded_revision_input_ids, [iId]);
      assert.ok(api.isApplied(id, jId) && api.isApplied(id, iId));
      assert.ok(api.isInputSuperseded(id, iId));
      assert.equal(api.isInputSuperseded(id, jId), false);
      send(id, row("cancel")); await done;
    } finally { r.shutdown(); await done; }
  });

  it(`M3 ${harness}: ordinary I to J to K stays pending until K publication; lost ACK replays once`, async () => {
    enable();
    const fixture = providerFixture(harness, n => n === 1 ? { plan: SOURCE } : n <= 5 ? { failure: true } : { plan: REVISED });
    const claim = freshClaim(1, { config: { plan_max_revisions: 3 },
      ...(harness === "codex" ? { secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } } : {}) });
    const id = claim.run_id;
    const r = runner(fixture.executor, fakeGitlab().gitlab, undefined, {
      planRevisionRetryPolicy: { redrives: 0, sleep: async (ms, signal) => { await delay(ms, undefined, { signal }); } },
    });
    api.afterPersistState(id, body => body.plan_md === REVISED, "drop");
    const done = r.execute(claim);
    try {
      assert.ok(await until(() => gates(id).length === 1));
      const [i] = send(id, row("revise_plan", "ordinary I"));
      assert.ok(await until(() => feed(id).some(text => text.includes(`pending revision ${i!.id},`))));
      assert.equal(fixture.starts.length, 3);
      const [j] = send(id, row("revise_plan", "ordinary J"));
      assert.ok(await until(() => feed(id).some(text => text.includes(`pending revision ${j!.id},`))));
      assert.equal(fixture.starts.length, 5);
      assert.equal(api.isApplied(id, i!.id), false);
      assert.equal(api.isApplied(id, j!.id), false, "taking J does not settle I or J");
      const stale = row("revise_plan", "stale J", { gate_binding: "bound", gate_revision: 99 });
      send(id, stale);
      assert.ok(await until(() => api.isAcked(id, stale.id)));
      assert.equal(fixture.starts.length, 5, "wrong-source J starts nothing");
      const [k] = send(id, row("revise_plan", "ordinary K"));
      assert.ok(await until(() => gates(id).filter(g => g.plan_md === REVISED).length === 2), "lost publication ACK replayed");
      const reports = gates(id).filter(g => g.plan_md === REVISED);
      assert.deepEqual(reports[0], reports[1], "same full publication including presentation and source fence");
      assert.equal(reports[0]!.revision_input_id, k!.id);
      assert.deepEqual(reports[0]!.superseded_revision_input_ids, [i!.id, j!.id]);
      assert.equal(api.gateOf(id).revision, 2, "replay allocates no second revision");
      assert.equal(fixture.starts.length, 6);
      assert.match(fixture.starts[5]!.prompt, /ordinary K/);
      assert.doesNotMatch(fixture.starts[5]!.prompt, /ordinary J/);
      assert.ok(api.isApplied(id, i!.id) && api.isApplied(id, j!.id) && api.isApplied(id, k!.id));
      assert.ok(api.isInputSuperseded(id, i!.id) && api.isInputSuperseded(id, j!.id));
      assert.equal(api.isInputSuperseded(id, k!.id), false);
      assert.equal(api.inputReceiptCalls.some(c => c.kind === "applied" && c.ids.some(n => [i!.id, j!.id, k!.id].includes(n))), false);
      send(id, row("cancel")); await done;
    } finally { r.shutdown(); await done; }
  });

  for (const answeredAfterRetry of [false, true]) it(`M3 ${harness}: persistent 429 spends exactly three redrives; continuation=${answeredAfterRetry}`, async () => {
    enable();
    const fixture = providerFixture(harness, n => n === 1 ? { plan: SOURCE }
      : answeredAfterRetry && n === 4 ? { ask: true } : { failure: true });
    const claim = freshClaim(1, { config: { plan_max_revisions: 2 },
      ...(harness === "codex" ? { secrets: { forge_pat: "fixture-forge-pat", codex: fixture.credential } } : {}) });
    const id = claim.run_id;
    const waits: number[] = [];
    const expectedStarts = answeredAfterRetry ? 10 : 9;
    const activeRuns = new ActiveRunRegistry();
    const r = runner(fixture.executor, fakeGitlab().gitlab, undefined, {
      activeRuns, questionTimeoutMs: 3000,
      planRevisionRetryPolicy: { random: () => 0, sleep: async (ms, signal) => {
        if (fixture.starts.length === expectedStarts) await delay(ms, undefined, { signal });
        else { signal.throwIfAborted(); waits.push(ms); }
      } },
    });
    const done = r.execute(claim);
    try {
      assert.ok(await until(() => gates(id).length === 1));
      const source = api.gateOf(id);
      const [input] = send(id, row("revise_plan", "original I"));
      if (answeredAfterRetry) {
        assert.ok(await until(() => api.states.some(s => s.runId === id && s.body.status === "awaiting_input")));
        const question = api.states.find(s => s.runId === id && s.body.status === "awaiting_input")!.body.open_question_id;
        assert.equal(waits.reduce((sum, ms) => sum + ms, 0), 5000, "one allowance already spent before clarification");
        send(id, row("answer", JSON.stringify({ question_id: question, answers: ["server"] })));
      }
      assert.ok(await until(() => feed(id).some(text => text.includes("retry allowance exhausted"))), "persistent provider exhaustion reaches live expiry");
      assert.equal(fixture.starts.length, expectedStarts, "initial + two inner attempts × four batches + optional clarification");
      assert.ok(activeRuns.build().active.some(run => run.run_id === id && run.phase === "awaiting_approval"), "expired live gate remains snapshot-listed and busy");
      assert.equal(waits.reduce((sum, ms) => sum + ms, 0), 35000, "5/10/20s sliced backoffs, no real waits");
      assert.deepEqual(api.gateOf(id), source);
      assert.equal(api.isApplied(id, input!.id), false);
      const [approve, followup] = send(id, row("approve_plan"), row("follow_up", "do not consume as revision"));
      assert.ok(await until(() => api.isAcked(id, approve!.id) && api.isAcked(id, followup!.id)));
      assert.equal(fixture.starts.length, expectedStarts);
      assert.equal(api.isApplied(id, approve!.id), false);
      assert.equal(api.isApplied(id, followup!.id), false);
      assert.equal(statuses(id).includes("recovery_wait"), false);
      send(id, row("cancel")); await done;
      assert.equal(fixture.starts.length, expectedStarts, "cancel after expiry starts no provider work");
      assert.equal(activeRuns.has(id), false);
    } finally { r.shutdown(); await done; }
  });
}
