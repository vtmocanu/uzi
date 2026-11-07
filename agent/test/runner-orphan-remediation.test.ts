import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { describe, it, type TestContext } from "node:test";
import { api, client, git, installHarness } from "./runner-harness.js";
import {
  FEATURE, OWNER_FILE, confirmParks, scene, terminal, type Scene, type SceneOptions,
} from "./runner-orphan-remediation-scene.js";

installHarness();

// issue #4058 M2 - a successor reclaiming a cancelled predecessor's orphaned attempt whose recovery journal carries an
// active remediation recovery record holds for the owner instead of failing terminally or releasing the source.

const linux = { skip: process.platform !== "linux" };

/** The bytes a hold must leave untouched. */
function snapshot(s: Scene) {
  return {
    durable: s.durable(), journal: s.journalRaw(),
    owner: fs.readFileSync(path.join(s.clone, OWNER_FILE), "utf8"),
    head: fs.readFileSync(path.join(s.clone, ".git", "HEAD"), "utf8"),
  };
}

/** Everything a confirmed hold promises. `parks` is the number of park reports the run's claims made in total. */
function assertHeld(s: Scene, before: ReturnType<typeof snapshot>, parks = 1, claim = s.claim): void {
  assert.equal(s.entered.value, false, "no model turn");
  assert.equal(s.parks(claim), parks, "exactly the expected typed park reports");
  assert.deepEqual(terminal(s, claim), [], "no terminal report");
  assert.deepEqual(snapshot(s), before, "journal, ledger, attempt markers, descriptors and the owner path are byte-identical");
  assert.ok(fs.existsSync(s.clone), "the predecessor path is still present");
  assert.equal(s.calls.release(), 0, "no attempt release");
  assert.equal(s.calls.retire(), 0, "no clone retirement");
  assert.equal(s.calls.detach(), 0, "no detach");
  assert.equal(s.calls.handoff(), 0, "no completion handoff");
  assert.deepEqual(s.calls.quiesce.filter(site => site === "orphan_reclaim"), [], "no orphan quiescence attempt");
  assert.equal(api.states.some(x => x.runId === claim.run_id && (x.body.status === "completed")), false);
}

function assertGuidance(s: Scene, claim = s.claim): void {
  const text = s.guidance(claim);
  assert.ok(text.length >= 1, "guidance delivered");
  const joined = text.join("\n");
  assert.ok(joined.includes(s.ownerRun), "names the predecessor run id");
  assert.ok(!joined.includes(s.clone) && !joined.includes(s.bare) && !joined.includes(s.tip) && !/Error/.test(joined),
    "no path, commit id or inner error text");
  assert.ok(joined.includes("Resume on this worker will hold again unchanged"), "the park is reported, so Resume holds again");
  assert.ok(!joined.includes("not placed on an owner hold"));
  const log = api.requestLog;
  const messages = log.indexOf("messages");
  const park = log.indexOf("state:recovery_wait");
  assert.ok(messages >= 0 && park > messages, "the guidance reached the server before the park: " + log.join(","));
}

interface Row extends SceneOptions { name: string; error: string }
const ROWS: Row[] = [];
for (const worker of [true, false]) for (const entry of ["A", "B"] as const) {
  for (const variant of ["plain", "retained", "authority", "fenced", "fenced+retained"] as const) {
    // Case B (the same canonical slug) meets the owner's attempt-path journal as a foreign capture when attempt paths are
    // on or the journal is retained; otherwise the claimant's canonical path differs from the journaled one (Case A).
    const error = entry === "B" && (worker || variant.endsWith("retained")) ? "ForeignCaptureBlockedError" : "CapturePathMismatchError";
    ROWS.push({
      name: `${error} (entry ${entry}) via ${worker ? "releaseAttemptInPlace" : "retireRunnerClone"}, ${variant}`,
      entry, attemptPaths: worker, error,
      authority: variant.startsWith("authority") || variant.startsWith("fenced"),
      fenced: variant.startsWith("fenced"), retained: variant.endsWith("retained"),
    });
  }
}

describe("a remediation-enrolled orphan holds the successor", linux, () => {
  for (const row of ROWS) {
    it(row.name, async t => {
      confirmParks(t);
      const s = await scene(t, row);
      const before = snapshot(s);
      await s.run();
      assert.equal(s.entries[0]!.constructor.name, row.error);
      assertHeld(s, before);
      assertGuidance(s);
      assert.ok(s.lines.some(l => l.event === "orphan_reclaim_refused" && l.stage === "completion_handoff" &&
        l.reason === "completion_retirement_pending" && l.retirement_subreason === "remediation_retained"));
    });
  }

  it("a custody-only block written through the public writer holds the same way", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true, blocked: true });
    const before = snapshot(s);
    await s.run();
    assertHeld(s, before);
  });

  it("a forward-fix descendant that still contains a flagged ancestor holds", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true });
    await s.forwardFix();
    const before = snapshot(s);
    await s.run();
    assertHeld(s, before);
  });

  it("a live process on the predecessor path still holds, with no quiescence attempt", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "B", attemptPaths: true });
    s.quiesceBlocked.value = true;
    const before = snapshot(s);
    await s.run();
    assertHeld(s, before);
    assert.deepEqual(s.calls.quiesce, []);
  });

  it("an authority worker whose fenced owner would never answer holds without calling the completion handoff", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true, authority: true, fenced: true });
    ((s.runner as unknown as { recovery: unknown }).recovery as { retireCompletedSourceForHandoff: unknown }).retireCompletedSourceForHandoff =
      () => new Promise(() => {});
    const before = snapshot(s);
    await s.run();
    assert.equal(s.parks(), 1);
    assert.deepEqual(snapshot(s), before);
    assert.deepEqual(terminal(s), []);
  });

  it("a pending bare-lock writer makes the classification wait, then hold", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true });
    const before = snapshot(s);
    const locks = git as unknown as { withLock<T>(key: string, fn: () => Promise<T>): Promise<T> };
    // The writer takes the bare lock the moment the claimant asks to classify the journal, so the read queues behind it.
    const read = git.readOrphanJournalFence.bind(git);
    let writer: Promise<void> | undefined;
    t.mock.method(git, "readOrphanJournalFence", (async (...args: Parameters<typeof read>) => {
      if (writer === undefined) {
        let held!: () => void;
        const holding = new Promise<void>(resolve => { held = resolve; });
        writer = locks.withLock(s.bare, async () => { held(); await new Promise(resolve => setTimeout(resolve, 200)); });
        await holding;
      }
      return read(...args);
    }) as never);
    let done = false;
    const running = s.run().then(() => { done = true; });
    await new Promise(resolve => setTimeout(resolve, 60));
    assert.equal(done, false, "the claimant is still waiting behind the writer");
    await Promise.all([running, writer]);
    assert.ok(writer, "the writer ran");
    assertHeld(s, before);
  });

  it("an unenrolled ordinary attempt still reclaims (positive control)", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true, enroll: false });
    await s.run();
    assert.equal(s.entered.value, true, "the successor reached its executor");
    assert.equal(s.parks(), 0);
    assert.deepEqual(s.guidance(), []);
    assert.equal(s.calls.release(), 1);
  });
});

describe("the park acknowledgement", linux, () => {
  it("a lost ACK resolved by an ownership read confirms through the replay", async t => {
    const s = await scene(t, { entry: "A", attemptPaths: true });
    const before = snapshot(s);
    let reports = 0;
    confirmParks(t, async (body, original) => {
      const ack = await original();
      if (body.status === "recovery_wait" && ++reports === 1) throw new Error("connection reset after the server applied the park");
      return ack;
    });
    api.setOwnershipStatus(s.claim.run_id, "recovery_wait", 5);
    await s.run();
    assert.equal(reports, 2, "the park was replayed once");
    assert.deepEqual(terminal(s), []);
    assert.deepEqual(snapshot(s), before);
  });

  it("a lost ACK followed by a stale_claim replay is an unconfirmed nonterminal stop", async t => {
    const s = await scene(t, { entry: "A", attemptPaths: true });
    const before = snapshot(s);
    let reports = 0;
    confirmParks(t, async (body, original) => {
      if (body.status === "recovery_wait" && ++reports === 1) {
        await original();
        api.failStateWhen(s.claim.run_id, b => b.status === "recovery_wait", { disposition: "stale_claim", runStatus: "queued" });
        throw new Error("connection reset after the server applied the park");
      }
      return original();
    });
    api.setOwnershipStatus(s.claim.run_id, "running", 5);
    await s.run();
    assert.equal(s.parks(), 1, "only the first (applied) report is on the server");
    assert.deepEqual(terminal(s), []);
    assert.deepEqual(snapshot(s), before);
    assert.equal(s.entered.value, false);
  });

  it("an unresolved ACK is a nonterminal stop", async t => {
    // The FakeApi ACK echoes the status without the typed cause and the ownership read names no generation.
    const s = await scene(t, { entry: "A", attemptPaths: true });
    const before = snapshot(s);
    await s.run();
    assert.deepEqual(terminal(s), []);
    assert.deepEqual(snapshot(s), before);
    assert.equal(s.entered.value, false);
  });

  it("without the negotiated cause nothing is parked but the guidance is delivered and the stop is nonterminal", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true, features: ["claim_generation_fence"] });
    const before = snapshot(s);
    await s.run();
    assert.equal(s.parks(), 0);
    assert.deepEqual(terminal(s), []);
    assert.equal(s.guidance().length >= 1, true);
    const joined = s.guidance().join("\n");
    assert.ok(joined.includes("This run was not placed on an owner hold"), "the no-park wording");
    assert.ok(joined.includes("each automatic retry on this worker will stop the same way"));
    assert.ok(!joined.includes("Resume on this worker"), "no Resume promise when nothing is parked");
    assert.deepEqual(snapshot(s), before);
    assert.equal(client.protocolFeatures?.includes(FEATURE), false);
  });
});

describe("owner cancellation around the hold", linux, () => {
  it("a Cancel pending before the hold reports the run cancelled and parks nothing", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true });
    const before = snapshot(s);
    s.beforeReclaim = flight => flight.steering.markCancelled();
    await s.run();
    assert.equal(s.parks(), 0);
    assert.deepEqual(s.guidance(), []);
    assert.ok(api.states.some(x => x.runId === s.claim.run_id && x.body.status === "failed" && /cancel/i.test(String(x.body.failure_reason))),
      "the existing cancelled report: " + s.reported().join(","));
    assert.deepEqual(snapshot(s), before);
  });

  it("a Cancel during the ambiguous park send is a plain stop with no terminal report", async t => {
    const s = await scene(t, { entry: "A", attemptPaths: true });
    const before = snapshot(s);
    confirmParks(t, async (body, original) => {
      if (body.status === "recovery_wait") {
        s.flight!.steering.markCancelled();
        await original();
        throw new Error("connection reset after the server applied the park");
      }
      return original();
    });
    await s.run();
    assert.deepEqual(terminal(s), []);
    assert.deepEqual(snapshot(s), before);
  });

  it("a Cancel marked after the flight ended causes no further request or local change", async t => {
    // run() has already returned, so this checks only that nothing the flight left behind reacts to a late Cancel.
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true });
    const before = snapshot(s);
    await s.run();
    assertHeld(s, before);
    const log = [...api.requestLog];
    s.flight!.steering.markCancelled();
    await new Promise(resolve => setTimeout(resolve, 100));
    assert.deepEqual(api.requestLog, log);
    assert.deepEqual(snapshot(s), before);
  });
});

describe("the guidance is confirmed before the park, whatever the message transport does", linux, () => {
  async function heldWith(t: TestContext, arrange: (s: Scene) => void, delivered = true): Promise<Scene> {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true });
    const before = snapshot(s);
    arrange(s);
    // Server-side order: the guidance message text (not any "messages" entry) must already be stored when the park lands.
    let guidanceAtPark: boolean | undefined;
    const reportReal = client.reportState.bind(client) as unknown as (...a: unknown[]) => Promise<unknown>;
    t.mock.method(client, "reportState", (async (...args: unknown[]) => {
      if ((args[1] as Record<string, unknown>).status === "recovery_wait") guidanceAtPark ??= s.guidance().length > 0;
      return reportReal(...args);
    }) as never);
    await s.run();
    if (delivered) assert.equal(guidanceAtPark, true, "the guidance text reached the server before the park report");
    assert.equal(s.parks(), 1, "the confirmed park ACK");
    assert.deepEqual(terminal(s), [], "no failed report");
    assert.deepEqual(snapshot(s), before);
    assert.equal(s.entered.value, false);
    return s;
  }

  it("an already-running periodic send is joined", async t => {
    const real = client.postMessages.bind(client);
    let first = true;
    t.mock.method(client, "postMessages", (async (...args: Parameters<typeof real>) => {
      if (first) { first = false; await new Promise(resolve => setTimeout(resolve, 80)); }
      return real(...args);
    }) as never);
    await heldWith(t, s => {
      s.beforeReclaim = flight => {
        flight.batcher.emit({ kind: "status", agent: "worker", payload: { text: "earlier message" } });
        void flight.batcher.flush();
      };
    });
  });

  it("a transport failure of the guidance batch", async t => {
    // The failed batch is retried later, so the order is not guaranteed here: only the unconfirmed-but-reported park is.
    await heldWith(t, () => api.failMessagesNext(1, 503), false);
  });

  it("a lost message ACK", async t => {
    const real = client.postMessages.bind(client);
    let lost = false;
    t.mock.method(client, "postMessages", (async (...args: Parameters<typeof real>) => {
      await real(...args);
      if (!lost) { lost = true; throw new Error("connection reset after the server applied the batch"); }
    }) as never);
    await heldWith(t, () => {});
  });

  it("a permanent batch refusal drives the real breaker callback without failing the run", async t => {
    await heldWith(t, () => api.failMessagesNext(1, 403), false);
    assert.ok(api.states.every(x => x.body.status !== "failed"));
  });

  it("an unconfirmed guidance flush is logged as guidance_unconfirmed and the park is still reported", async t => {
    // A permanent refusal trips the batcher, so flushOwned returns false without waiting out GUIDANCE_FLUSH_MS.
    const s = await heldWith(t, () => api.failMessagesNext(1, 403), false);
    assert.ok(s.lines.some(l => l.msg === "guidance_unconfirmed" && l.reason === "not_confirmed"),
      s.lines.map(l => String(l.msg)).join(" | "));
    assert.equal(s.lines.filter(l => l.msg === "guidance_unconfirmed").length, 1, "logged once");
  });
});

describe("repeated claims, restarts and a server-only discard", linux, () => {
  it("each Resume claim holds again with nothing changed", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true });
    const before = snapshot(s);
    await s.run();
    assertHeld(s, before);
    for (const [i, generation] of [6, 7, 8].entries()) {
      await s.run(s.nextClaim(generation));
      assertHeld(s, before, i + 2);
    }
  });

  it("a restarted worker (fresh runner and cache over the same dirs) holds the same way", async t => {
    confirmParks(t);
    const s = await scene(t, { entry: "B", attemptPaths: true, authority: true, fenced: true, retained: true });
    const before = snapshot(s);
    await s.run();
    assertHeld(s, before);
    const cold = s.coldRunner();
    await s.run(s.nextClaim(6), cold);
    assertHeld(s, before, 2);
  });

  it("a cold reader holds from the worker's own durable bytes, independent of server-side custody state", async t => {
    // The FakeApi has no way to drop a predecessor custody hold or recovery record, so a server-only discard cannot be
    // simulated; this proves the worker's hold depends only on its durable bytes, via a cold reader.
    confirmParks(t);
    const s = await scene(t, { entry: "A", attemptPaths: true });
    const before = snapshot(s);
    await s.run();
    api.setOrphanClassification(s.ownerRun, {
      status: "cancelled", repo_id: "r1", kind: "issue", issue_iid: s.iid, branch: null, pipeline_ref: null, pipeline_id: null,
    });
    await s.run(s.nextClaim(6), s.coldRunner());
    assertHeld(s, before, 2);
  });
});

describe("the worker-side cancellation release log", linux, () => {
  it("names the retained remediation record instead of the generic failure", async t => {
    const s = await scene(t, { entry: "A", attemptPaths: true });
    // A flight of the owner's own run that captured its predecessor attempt and is released at its end.
    const runner = s.runner as unknown as { phaseClone(...a: unknown[]): Promise<unknown> };
    runner.phaseClone = async (_claim: unknown, flight: Record<string, unknown>) => {
      Object.assign(flight, { predecessorCapture: true, predecessorCaptureVerified: true, worktreePath: s.clone,
        barePath: s.bare, branch: s.branch });
      throw new Error("fixture stops after the predecessor capture");
    };
    const before = snapshot(s);
    await s.run({ ...s.nextClaim(6), run_id: s.ownerRun });
    const msgs = s.lines.map(l => String(l.msg));
    assert.ok(msgs.some(m => m === "predecessor attempt release refused: active remediation record retained (remediation_retained); nothing released, journal kept"),
      msgs.filter(m => m.startsWith("predecessor")).join(" | "));
    assert.ok(!msgs.some(m => m.includes("failed before any write")));
    assert.deepEqual(snapshot(s), before);
  });
});
