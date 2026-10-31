import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fsp from "node:fs/promises";
import path from "node:path";

import type { RawWriteSeam } from "../src/outbox.js";
import {
  PUBLICATION_RESUME_UNAVAILABLE,
  PublicationIntentMismatchError,
  advancePublicationCandidate,
  enrollPublicationIntent,
  parseClaimPublicationResume,
  parsePublicationIntent,
  parsePublicationRecord,
  publicationParkRefusal,
  relatePublicationToClaim,
  resendPublicationRecord,
  type EnrolledPublicationRecord,
  type PublicationDeps,
  type PublicationIntentResult,
} from "../src/publication-intent.js";
import { CAND, CAND2, HEAD, RUN, cleanupRoots, intent, makeOutbox, noSleep, recordWithSeqs, scriptedApi, tmpRoot } from "./publication-intent-fixture.js";

// Issue #1324: the intent shape, its strict parser, and the enroll / advance orchestration with its
// fallback matrix (every failure classifies as exactly one of not_enrolled or uncertain).
afterEach(cleanupRoots);

// The api's top-level intent allowlist (api/internal/workersvc/publication_resume.go
// publicationIntentKeys). Hand-copied: the worker must never send a member outside it.
const API_INTENT_KEYS = ["v", "run_id", "repo_id", "branch", "claim_generation", "enrollment_seq", "original_head", "floor", "candidate", "candidate_seq", "source", "mr", "guard", "staged_description", "mac"];

const acked = (enrollment_seq = 1, candidate_seq = 1): PublicationIntentResult => ({ kind: "acked", enrollment_seq, candidate_seq, state: "active" });
const refused = (status: number, reason: string): PublicationIntentResult => ({ kind: "refused", status, reason });
const unknown: PublicationIntentResult = { kind: "unknown", detail: "socket hang up" };

function deps(root: string, api: ReturnType<typeof scriptedApi>, over: { rawWrite?: RawWriteSeam } = {}) {
  const outbox = makeOutbox(root, over.rawWrite ? { rawWrite: over.rawWrite } : {});
  const d: PublicationDeps = { store: outbox, api, sleep: noSleep };
  return { outbox, d };
}

const file = (root: string) => path.join(root, RUN, "publication-4.json");

describe("publication intent: strict parser", () => {
  it("round-trips a full intent and a record with sequence numbers", () => {
    const full = intent({ staged_description: { candidate: CAND, target: "main", contract_revision: 3, body: "## Summary" } });
    assert.deepEqual(parsePublicationIntent(JSON.parse(JSON.stringify(full))), full);
    assert.deepEqual(parsePublicationRecord({ ...full, enrollment_seq: 2, candidate_seq: 5 }), { ...full, enrollment_seq: 2, candidate_seq: 5 });
    assert.deepEqual(parsePublicationIntent(intent({ floor: { kind: "absent" }, candidate: null })).floor, { kind: "absent" });
  });

  it("only ever emits members the api's intent allowlist knows", () => {
    const keys = Object.keys(intent({ staged_description: { candidate: CAND, target: "main", contract_revision: 3, body: "b" } }));
    for (const key of keys) assert.ok(API_INTENT_KEYS.includes(key), key);
  });

  const cases: [string, (o: Record<string, unknown>) => void][] = [
    ["an unknown version", (o) => { o.v = 2; }],
    ["an unknown top-level member", (o) => { o.extra = 1; }],
    ["an unknown guard member", (o) => { (o.guard as Record<string, unknown>).future_guard = true; }],
    ["a missing guard member", (o) => { delete (o.guard as Record<string, unknown>).ci_fix_human_approved; }],
    ["a guard flag of the wrong type", (o) => { (o.guard as Record<string, unknown>).landed_checkpoint = "yes"; }],
    ["an invalid frozen held policy", (o) => { ((o.guard as Record<string, unknown>).frozen_held as Record<string, unknown>).flaggedCommits = ["nope"]; }],
    ["an unknown frozen held member", (o) => { ((o.guard as Record<string, unknown>).frozen_held as Record<string, unknown>).newGuard = 1; }],
    ["an invalid secret finding", (o) => { ((o.guard as Record<string, unknown>).secret_remediation as Record<string, unknown>).known = [{ file: "a" }]; }],
    ["a malformed object id", (o) => { o.original_head = "abc"; }],
    ["an upper-case uuid", (o) => { o.run_id = RUN.toUpperCase(); }],
    ["a zero claim generation", (o) => { o.claim_generation = 0; }],
    ["an absent floor carrying an oid", (o) => { o.floor = { kind: "absent", oid: HEAD }; }],
    ["an unverified floor", (o) => { o.floor = { kind: "unverified" }; }],
    ["a branch with a control character", (o) => { o.branch = "agent/x\u0007y"; }],
    ["a staged description for another candidate", (o) => { o.staged_description = { candidate: CAND2, target: "main", contract_revision: 3, body: "b" }; }],
    ["a staged description for another contract revision", (o) => { o.staged_description = { candidate: CAND, target: "main", contract_revision: 9, body: "b" }; }],
  ];
  for (const [name, mutate] of cases) {
    it(`refuses ${name} with the typed mismatch`, () => {
      const raw = JSON.parse(JSON.stringify(intent())) as Record<string, unknown>;
      mutate(raw);
      assert.throws(() => parsePublicationIntent(raw), (err: unknown) =>
        err instanceof PublicationIntentMismatchError && err.code === "publication_intent_mismatch");
    });
  }

  it("requires both sequence numbers or neither", () => {
    assert.throws(() => parsePublicationRecord({ ...intent(), enrollment_seq: 1 }), PublicationIntentMismatchError);
    assert.throws(() => parsePublicationRecord({ ...intent(), enrollment_seq: 0, candidate_seq: 1 }), PublicationIntentMismatchError);
  });

  it("types a claim's publication_resume and refuses an unknown guard state", () => {
    const claim = { enrollment_seq: 3, candidate_seq: 2, intent: JSON.parse(JSON.stringify(intent())) as unknown };
    assert.deepEqual(parseClaimPublicationResume(claim), { ...intent(), enrollment_seq: 3, candidate_seq: 2 });
    const future = JSON.parse(JSON.stringify(intent())) as { guard: Record<string, unknown> };
    future.guard.unknown_guard = {};
    assert.throws(() => parseClaimPublicationResume({ ...claim, intent: future }), PublicationIntentMismatchError);
  });

  it("relates a local record to the claim's single active enrollment without resurrecting an old one", () => {
    const rec = recordWithSeqs({ enrollment_seq: 2, candidate_seq: 3 });
    const active = (enrollment_seq: number, candidate_seq: number) => ({ enrollment_seq, candidate_seq, intent: intent() });
    assert.equal(relatePublicationToClaim(rec, undefined).kind, "no_active_enrollment");
    assert.equal(relatePublicationToClaim(rec, active(2, 3)).kind, "in_sync");
    assert.equal(relatePublicationToClaim(rec, active(2, 2)).kind, "local_ahead");
    assert.equal(relatePublicationToClaim(rec, active(2, 4)).kind, "server_ahead");
    assert.equal(relatePublicationToClaim(rec, active(5, 1)).kind, "superseded");
    assert.equal(relatePublicationToClaim(intent(), active(2, 3)).kind, "superseded", "a sequence-less record cannot claim an enrollment");
  });

  it("reads the two typed-park refusals off an ack reason", () => {
    assert.equal(publicationParkRefusal({ reason: "publication_enrolled" }), "publication_enrolled");
    assert.equal(publicationParkRefusal({ reason: "publication_not_enrolled" }), "publication_not_enrolled");
    assert.equal(publicationParkRefusal({ reason: "custody_unsettled" }), undefined);
    assert.equal(publicationParkRefusal({}), undefined);
  });
});

describe("publication intent: enrollment", () => {
  it("records locally first, enrolls, then records the acknowledged sequence numbers", async () => {
    const root = tmpRoot();
    const api = scriptedApi(acked(7, 1));
    const { outbox, d } = deps(root, api);
    await outbox.init();
    const seenByApi: string[] = [];
    const wrapped = { registerPublicationIntent: async (...args: Parameters<typeof api.registerPublicationIntent>) => {
      seenByApi.push(await fsp.readFile(file(root), "utf8").then(() => "local record present", () => "no local record"));
      return api.registerPublicationIntent(...args);
    } };
    const outcome = await enrollPublicationIntent({ ...d, api: wrapped }, intent());
    assert.equal(outcome.kind, "enrolled");
    assert.deepEqual(seenByApi, ["local record present"], "the record is durable BEFORE the request is sent");
    const [call] = api.calls;
    assert.deepEqual(Object.keys(call!.request).sort(), ["claim_generation", "intent"], "a new enrollment carries no sequence numbers");
    assert.equal(call!.request.claim_generation, 4);
    assert.deepEqual(call!.request.intent, JSON.parse(JSON.stringify(intent())), "the wire intent is the canonical body: no mac, no sequence numbers");
    const loaded = await outbox.loadPublicationRecord(RUN, 4);
    assert.deepEqual(loaded.kind === "ok" ? [loaded.record.enrollment_seq, loaded.record.candidate_seq] : [], [7, 1]);
    assert.deepEqual(outbox.listPublicationRegisterEntries(), [{ run_id: RUN, claim_generation: 4, enrollment_seq: 7, candidate_seq: 1 }]);
  });

  it("known not enrolled: an outbox that is not usable sends nothing and writes nothing", async () => {
    const api = scriptedApi(acked());
    const outcome = await enrollPublicationIntent({ store: makeOutbox(tmpRoot()), api, sleep: noSleep }, intent());
    assert.deepEqual(outcome, { kind: "not_enrolled", reason: "outbox_disabled", diagnostic: PUBLICATION_RESUME_UNAVAILABLE });
    assert.equal(api.calls.length, 0);
  });

  it("known not enrolled: ENOSPC and a failed write with verified absence send nothing", async () => {
    for (const [code, reason] of [["ENOSPC", "enospc"], ["EIO", "local_write_failed"]] as const) {
      const rawWrite: RawWriteSeam = async (write, ctx) => {
        if (ctx.kind === "publication" && ctx.path.endsWith(".tmp")) throw Object.assign(new Error(code), { code });
        await write();
      };
      const root = tmpRoot();
      const api = scriptedApi(acked());
      const { outbox, d } = deps(root, api, { rawWrite });
      await outbox.init();
      const outcome = await enrollPublicationIntent(d, intent());
      assert.deepEqual([outcome.kind, outcome.kind === "not_enrolled" ? outcome.reason : "", outcome.kind === "not_enrolled" ? outcome.diagnostic : ""],
        ["not_enrolled", reason, PUBLICATION_RESUME_UNAVAILABLE]);
      assert.equal(api.calls.length, 0);
      await assert.rejects(fsp.lstat(file(root)), { code: "ENOENT" }, "absence is verified, not assumed");
    }
  });

  it("known not enrolled: an oversize or invalid intent is refused before any write", async () => {
    const api = scriptedApi(acked());
    const root = tmpRoot();
    const { outbox, d } = deps(root, api);
    await outbox.init();
    const big = intent();
    big.guard.secret_remediation.known = Array.from({ length: 200 }, (_, i) => ({ file: "f".repeat(1000), start_line: i, commit: "a".repeat(40), rule_id: "r" }));
    const tooBig = await enrollPublicationIntent(d, big);
    assert.deepEqual([tooBig.kind, tooBig.kind === "not_enrolled" ? tooBig.reason : ""], ["not_enrolled", "intent_too_large"]);
    const invalid = await enrollPublicationIntent(d, { ...intent(), original_head: "nope" });
    assert.deepEqual([invalid.kind, invalid.kind === "not_enrolled" ? invalid.reason : ""], ["not_enrolled", "intent_invalid"]);
    assert.equal(api.calls.length, 0);
  });

  it("uncertain: a partial link or fsync keeps the record and makes no request", async () => {
    const root = tmpRoot();
    const api = scriptedApi(acked());
    const { outbox, d } = deps(root, api);
    await outbox.init();
    const target = outbox as unknown as { fsyncDir(dir: string, strict?: boolean): Promise<void> };
    const real = target.fsyncDir.bind(outbox);
    target.fsyncDir = async (dir, strict) => {
      if (dir === path.join(root, RUN)) throw new Error("fsync failed");
      await real(dir, strict);
    };
    const outcome = await enrollPublicationIntent(d, intent());
    assert.deepEqual([outcome.kind, outcome.kind === "uncertain" ? outcome.reason : ""], ["uncertain", "local_write_unverified"]);
    assert.equal(api.calls.length, 0);
    assert.ok((await fsp.readdir(path.join(root, RUN))).includes("publication-4.json"));
  });

  it("uncertain: a request whose outcome is unknown is retried identically a bounded number of times, and the record stays", async () => {
    const root = tmpRoot();
    const api = scriptedApi(unknown);
    const { outbox, d } = deps(root, api);
    await outbox.init();
    const outcome = await enrollPublicationIntent(d, intent());
    assert.deepEqual([outcome.kind, outcome.kind === "uncertain" ? outcome.reason : ""], ["uncertain", "ack_unknown"]);
    assert.equal(api.calls.length, 3, "bounded");
    assert.deepEqual(api.calls[1]!.request, api.calls[0]!.request, "each retry is the identical idempotent request");
    assert.deepEqual(api.calls[2]!.request, api.calls[0]!.request);
    assert.equal((await outbox.loadPublicationRecord(RUN, 4)).kind, "ok");
    assert.deepEqual(outbox.listPublicationRegisterEntries(), [], "an unacknowledged record is not attested on register");
  });

  it("enrolls after a lost ACK when a retry reaches the api", async () => {
    const root = tmpRoot();
    const api = scriptedApi(unknown, acked(2, 1));
    const { outbox, d } = deps(root, api);
    await outbox.init();
    const outcome = await enrollPublicationIntent(d, intent());
    assert.equal(outcome.kind, "enrolled");
    assert.equal(api.calls.length, 2);
  });

  it("uncertain: an aborted enrollment stops retrying", async () => {
    const root = tmpRoot();
    const api = scriptedApi(unknown);
    const { outbox, d } = deps(root, api);
    await outbox.init();
    const controller = new AbortController();
    const outcome = await enrollPublicationIntent({ ...d, signal: controller.signal, sleep: async () => { controller.abort(); } }, intent());
    assert.deepEqual([outcome.kind, outcome.kind === "uncertain" ? outcome.reason : ""], ["uncertain", "aborted"]);
    assert.equal(api.calls.length, 1);
  });

  it("uncertain: a disagreeing server (mismatch, retired, stale claim) never discards the record", async () => {
    for (const result of [refused(409, "publication_intent_mismatch"), refused(409, "publication_enrollment_retired"), refused(409, "stale_claim"), refused(404, "http_404")]) {
      const root = tmpRoot();
      const api = scriptedApi(result);
      const { outbox, d } = deps(root, api);
      await outbox.init();
      const outcome = await enrollPublicationIntent(d, intent());
      assert.deepEqual([outcome.kind, outcome.kind === "uncertain" ? outcome.reason : ""], ["uncertain", "server_disagrees"], JSON.stringify(result));
      assert.equal((await outbox.loadPublicationRecord(RUN, 4)).kind, "ok");
      assert.equal(api.calls.length, 1, "a definite refusal is not retried");
    }
  });

  it("known not enrolled: a declining server leaves no sequence-less record behind", async () => {
    for (const [result, reason] of [
      [refused(503, "publication_resume_disabled"), "server_declined"],
      [refused(400, "publication_resume_unsupported"), "server_declined"],
      [refused(400, "publication_kind_unsupported"), "server_declined"],
      [{ kind: "unsupported" } as PublicationIntentResult, "server_feature_unavailable"],
    ] as const) {
      const root = tmpRoot();
      const { outbox, d } = deps(root, scriptedApi(result));
      await outbox.init();
      const outcome = await enrollPublicationIntent(d, intent());
      assert.deepEqual([outcome.kind, outcome.kind === "not_enrolled" ? outcome.reason : "", outcome.kind === "not_enrolled" ? outcome.diagnostic : ""],
        ["not_enrolled", reason, PUBLICATION_RESUME_UNAVAILABLE]);
      assert.equal((await outbox.loadPublicationRecord(RUN, 4)).kind, "absent");
    }
  });

  it("uncertain: an ACK the local record cannot carry is still an enrollment", async () => {
    const root = tmpRoot();
    let writes = 0;
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "publication" && ctx.path.endsWith(".tmp") && ++writes > 1) throw Object.assign(new Error("io"), { code: "EIO" });
      await write();
    };
    const { outbox, d } = deps(root, scriptedApi(acked(3, 1)), { rawWrite });
    await outbox.init();
    const outcome = await enrollPublicationIntent(d, intent());
    assert.deepEqual([outcome.kind, outcome.kind === "uncertain" ? outcome.reason : ""], ["uncertain", "ack_not_recorded"]);
    assert.equal((await outbox.loadPublicationRecord(RUN, 4)).kind, "ok", "the sequence-less record remains; register reconciliation decides");
    assert.deepEqual(outbox.listPublicationRegisterEntries(), []);
  });

  it("uncertain: an existing acknowledged record is an existing enrollment, not a new one", async () => {
    const root = tmpRoot();
    const api = scriptedApi(acked(1, 1));
    const { outbox, d } = deps(root, api);
    await outbox.init();
    assert.equal((await enrollPublicationIntent(d, intent())).kind, "enrolled");
    api.calls.length = 0;
    const again = await enrollPublicationIntent(d, intent());
    assert.deepEqual([again.kind, again.kind === "uncertain" ? again.reason : ""], ["uncertain", "already_enrolled"]);
    assert.equal(api.calls.length, 0);
  });

  it("uncertain: a different record in the slot is never overwritten", async () => {
    const root = tmpRoot();
    const api = scriptedApi(acked());
    const { outbox, d } = deps(root, api);
    await outbox.init();
    await outbox.savePublicationRecord(intent({ original_head: "9".repeat(40) }));
    const outcome = await enrollPublicationIntent(d, intent());
    assert.deepEqual([outcome.kind, outcome.kind === "uncertain" ? outcome.reason : ""], ["uncertain", "existing_record_conflict"]);
    assert.equal(api.calls.length, 0);
  });

  it("completes a record saved by a crashed flight (no sequence numbers yet) by re-enrolling idempotently", async () => {
    const root = tmpRoot();
    const first = deps(root, scriptedApi(acked()));
    await first.outbox.init();
    await first.outbox.savePublicationRecord(intent());
    const restarted = deps(root, scriptedApi(acked(6, 2)));
    await restarted.outbox.init();
    const outcome = await enrollPublicationIntent(restarted.d, intent());
    assert.deepEqual(outcome.kind === "enrolled" ? [outcome.record.enrollment_seq, outcome.record.candidate_seq] : [], [6, 2]);
  });
});

describe("publication intent: candidate update and retirement", () => {
  async function enrolled(root: string, api: ReturnType<typeof scriptedApi>, over: { rawWrite?: RawWriteSeam } = {}) {
    const { outbox, d } = deps(root, api, over);
    await outbox.init();
    const start = await enrollPublicationIntent(d, intent());
    assert.equal(start.kind, "enrolled");
    return { outbox, d, record: (start as { record: EnrolledPublicationRecord }).record };
  }

  it("saves the transformed candidate locally first, then advances candidate_seq by one on the api", async () => {
    const root = tmpRoot();
    const api = scriptedApi(acked(1, 1), acked(1, 2));
    const { outbox, d, record } = await enrolled(root, api);
    const outcome = await advancePublicationCandidate(d, record, { candidate: CAND2 });
    assert.equal(outcome.kind, "enrolled");
    const update = api.calls[1]!.request;
    assert.deepEqual([update.enrollment_seq, update.candidate_seq, update.claim_generation, update.intent.candidate], [1, 2, 4, CAND2]);
    const loaded = await outbox.loadPublicationRecord(RUN, 4);
    assert.deepEqual(loaded.kind === "ok" ? [loaded.record.candidate, loaded.record.candidate_seq] : [], [CAND2, 2]);
  });

  it("a failed local save of the transformed candidate is uncertain, sends nothing and keeps the old record", async () => {
    const root = tmpRoot();
    let fail = false;
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (fail && ctx.kind === "publication") throw Object.assign(new Error("full"), { code: "ENOSPC" });
      await write();
    };
    const api = scriptedApi(acked(1, 1), acked(1, 2));
    const { outbox, d, record } = await enrolled(root, api, { rawWrite });
    fail = true;
    const outcome = await advancePublicationCandidate(d, record, { candidate: CAND2 });
    assert.deepEqual([outcome.kind, outcome.kind === "uncertain" ? outcome.reason : ""], ["uncertain", "candidate_save_failed"]);
    assert.equal(api.calls.length, 1, "no update request was sent");
    fail = false;
    const loaded = await outbox.loadPublicationRecord(RUN, 4);
    assert.deepEqual(loaded.kind === "ok" ? [loaded.record.candidate, loaded.record.candidate_seq] : [], [CAND, 1]);
  });

  it("recovers a lost candidate ACK by repeating the identical request, and treats a lying ack as disagreement", async () => {
    const root = tmpRoot();
    const lost = scriptedApi(acked(1, 1), unknown, acked(1, 2));
    const first = await enrolled(root, lost);
    const noRetry = { ...first.d, retryDelaysMs: [0] as const };
    const pending = await advancePublicationCandidate(noRetry, first.record, { candidate: CAND2 });
    assert.deepEqual([pending.kind, pending.kind === "uncertain" ? pending.reason : ""], ["uncertain", "ack_unknown"]);
    const loaded = await first.outbox.loadPublicationRecord(RUN, 4);
    assert.ok(loaded.kind === "ok" && loaded.record.enrollment_seq === 1);
    const resent = await resendPublicationRecord(first.d, loaded.record as EnrolledPublicationRecord);
    assert.equal(resent.kind, "enrolled");
    assert.deepEqual(lost.calls[2]!.request, lost.calls[1]!.request, "the repeat is byte-identical");

    const liar = scriptedApi(acked(1, 1), acked(1, 9));
    const other = await enrolled(tmpRoot(), liar);
    const result = await advancePublicationCandidate(other.d, other.record, { candidate: CAND2 });
    assert.deepEqual([result.kind, result.kind === "uncertain" ? result.reason : ""], ["uncertain", "server_disagrees"]);
  });

  it("discards the local record only when the api confirms THAT enrollment retired", async () => {
    for (const [result, discards] of [
      [refused(409, "publication_intent_mismatch"), false],
      [refused(409, "stale_claim"), false],
      [refused(400, "publication_resume_unsupported"), false],
      [{ kind: "unsupported" } as PublicationIntentResult, false],
      [unknown, false],
      [refused(409, "publication_enrollment_retired"), true],
    ] as const) {
      const root = tmpRoot();
      const api = scriptedApi(acked(1, 1), result);
      const { outbox, d, record } = await enrolled(root, api);
      const outcome = await resendPublicationRecord(d, record);
      assert.equal(outcome.kind === "retired", discards, JSON.stringify(result));
      assert.equal((await outbox.loadPublicationRecord(RUN, 4)).kind, discards ? "absent" : "ok", JSON.stringify(result));
      if (discards) assert.deepEqual(outcome, { kind: "retired", discarded: true });
    }
  });

  it("does not discard a record whose enrollment differs from the retired one", async () => {
    const root = tmpRoot();
    const api = scriptedApi(acked(1, 1), refused(409, "publication_enrollment_retired"));
    const { outbox, d, record } = await enrolled(root, api);
    const outcome = await resendPublicationRecord(d, { ...record, enrollment_seq: 2 });
    assert.deepEqual(outcome, { kind: "retired", discarded: false });
    assert.equal((await outbox.loadPublicationRecord(RUN, 4)).kind, "ok");
  });
});
