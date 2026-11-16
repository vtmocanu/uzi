import test from "node:test";
import assert from "node:assert/strict";
import { coordinateCodexSwitch, type CodexSwitchCoordinatorOptions } from "../src/codex-switch-coordinator.js";
import type { LedgerApplyResult } from "../src/credential-switch-op-ledger.js";
import type { CodexSwitchAuthority } from "../src/codex-switch-preflight.js";

function fixture() {
  const events: string[] = [];
  const journal = new Set<string>();
  const holds: { opId: string; reason: string }[] = [];
  const identity = Object.freeze({ worker: "worker", generation: 7, worktree: "tree", attempt: 3 });
  const abort = new AbortController();
  let time = 0;
  const op = { claimGeneration: 7, registerNonce: "nonce", opId: "op", targetSecretId: "B", revision: 1 };
  const authority: CodexSwitchAuthority = { owned: true, claimGeneration: 7, registerNonce: "nonce",
    routeAvailable: true, ledgerAvailable: true, pending: op };
  const applied: LedgerApplyResult = { kind: "codex_applied", codex: { auth_mode: "api_key",
    access_token: "fixture-access", capability: "fixture-capability" }, capabilityEpoch: 41, ordinal: 9,
    messageSeq: 73, replay: false };
  const event = (name: string) => { events.push(name); assert.equal(identity.generation, 7); };
  const o: CodexSwitchCoordinatorOptions = {
    initial: op, signal: abort.signal, now: () => time, deadline: 100_000, previousOrdinal: 8,
    preserveConversation: false,
    reconcile: async () => ({ ...authority }),
    preflight: {
      probe: async p => { event("probe"); return { kind: "ready", expectedEpoch: 11, ...p }; },
      delay: async ms => { time += ms; },
      confirmFailedClear: async () => { event("clear"); authority.pending = undefined; return true; },
    },
    confirmPersistFailedClear: async () => { event("persist-clear"); authority.pending = undefined; return true; },
    drainAndSuspend: async () => { event("drain"); },
    persistRollout: async () => { event("persist"); return { kind: "usable", sessionId: "saved" }; },
    reopenA: async () => { event("A"); },
    teardown: async () => { event("reap"); return true; },
    reportUnsettled: async () => { event("unsettled"); },
    apply: async p => { event("apply"); journal.add(p.opId); authority.pending = undefined; return applied; },
    uncertainHold: async (p, reason) => {
      event("hold"); holds.push({ opId: p.opId, reason });
      return authority.pending?.opId === p.opId || (!authority.pending && journal.has(p.opId));
    },
    requestAndConfirmOwnerPause: async () => { event("ordinary-pause"); return true; },
    registerSecrets: async bundle => {
      event("register"); assert.ok(Object.isFrozen(bundle)); assert.ok(Object.isFrozen(bundle.binding));
      assert.equal(bundle.ordinal, 9); assert.equal(bundle.capabilityEpoch, 41);
    },
    publish: async () => { event("publish"); },
    restartAndResume: async (_bundle, saved) => {
      assert.equal(saved, "saved"); event("root"); event("login"); event("resume"); return { kind: "resumed" };
    },
    startFreshLineage: async () => { event("fresh"); },
  };
  const commit = (result: LedgerApplyResult): CodexSwitchCoordinatorOptions["apply"] => async p => {
    event("apply"); journal.add(p.opId); authority.pending = undefined; return result;
  };
  return { o, events, authority, applied, abort, journal, holds, commit, advance: () => { time = 100_000; } };
}

test("complete immutable B ordered after persistence and reap; counters are independent", async () => {
  const f = fixture();
  assert.deepEqual(await coordinateCodexSwitch(f.o), { kind: "codex_resumed_B" });
  assert.deepEqual(f.events, ["drain", "persist", "probe", "reap", "apply", "register", "publish", "root", "login", "resume"]);
  assert.ok(!("release" in f.o) && !("reclaim" in f.o));
});

for (const result of [
  ...["credential_busy", "vault_locked", "policy_refusal", "baseline_missing", "baseline_mismatch",
    "target_moved", "target_mismatch", "credential_disabled", "credential_rejected", "credential_unavailable",
    "credential_changed", "credential_gone", "credential_not_decryptable", "no_pending_switch", "generation_mismatch"]
    .map(reason => ({ kind: "not_applied", reason } as const)),
  { kind: "route_absent" }, { kind: "ledger_unavailable" }, { kind: "uncertain", detail: "lost_reply" },
  { kind: "refused_committed", reason: "disabled" }, { kind: "applied", token: "fixture", ordinal: 9, messageSeq: 3, replay: false },
] as LedgerApplyResult[]) {
  test("post-teardown " + JSON.stringify(result) + " holds without clear or A", async () => {
    const f = fixture(); f.o.apply = async () => result;
    assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_owner_paused");
    assert.ok(f.events.includes("hold"));
    assert.ok(!f.events.includes("clear") && !f.events.includes("A") && !f.events.includes("publish"));
  });
}

for (const available of ["routeAvailable", "ledgerAvailable"] as const) {
  for (const after of [false, true]) test(available + " absent after teardown=" + after, async () => {
    const f = fixture();
    if (after) f.o.teardown = async () => { f.events.push("reap"); f.authority[available] = false; return true; };
    else f.authority[available] = false;
    assert.equal((await coordinateCodexSwitch(f.o)).kind, after ? "codex_owner_paused" : "codex_retained_stop");
    assert.equal(f.events.includes("reap"), after);
    assert.ok(!f.events.includes("A"));
  });
}

for (const confirmed of [true, false]) test("persist failure requires exact confirmed clear " + confirmed, async () => {
  const f = fixture(); f.o.persistRollout = async () => ({ kind: "failed" });
  f.o.confirmPersistFailedClear = async () => { if (confirmed) f.authority.pending = undefined; return confirmed; };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, confirmed ? "codex_continued_A" : "codex_retained_stop");
  assert.equal(f.events.includes("A"), confirmed); assert.ok(!f.events.includes("reap"));
});

test("preflight definitive refusal reopens A only before teardown", async () => {
  const f = fixture(); f.o.preflight.probe = async p => ({ kind: "not_applied", reason: "vault_locked", ...p });
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_continued_A");
  assert.deepEqual(f.events, ["drain", "persist", "clear", "A"]);
});

for (const throws of [false, true]) test("unproven reap settles neither account " + throws, async () => {
  const f = fixture(); f.o.teardown = async () => { if (throws) throw Error("reap"); return false; };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_retained_stop");
  assert.ok(f.events.includes("unsettled")); assert.ok(!f.events.includes("apply") && !f.events.includes("hold"));
});

for (const stage of ["registerSecrets", "publish", "restartAndResume"] as const) test(stage + " failure holds", async () => {
  const f = fixture(); f.o[stage] = async () => { throw Error("ambiguous"); };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_owner_paused");
  assert.ok(f.events.includes("hold")); assert.ok(!f.events.includes("fresh"));
});

test("apply lost reply holds", async () => {
  const f = fixture(); f.o.apply = async () => { throw Error("lost reply"); };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_owner_paused");
});
test("replayed full authorization is rejected", async () => {
  const f = fixture(); f.o.apply = f.commit({ ...f.applied, replay: true } as LedgerApplyResult);
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_owner_paused");
});

for (const fence of ["owned", "nonce", "generation", "apply", "cancel", "deadline"]) test("fence " + fence, async () => {
  const f = fixture();
  f.o.teardown = async () => {
    if (fence === "owned") f.authority.owned = false;
    if (fence === "nonce") f.authority.registerNonce = "other";
    if (fence === "generation") f.authority.claimGeneration++;
    if (fence === "cancel") f.abort.abort();
    if (fence === "deadline") f.advance();
    return true;
  };
  if (fence === "apply") f.o.apply = async () => ({ kind: "fenced" });
  assert.equal((await coordinateCodexSwitch(f.o)).kind, ["cancel", "deadline"].includes(fence) ? "codex_retained_stop" : "codex_fenced");
  assert.ok(!f.events.includes("publish") && !f.events.includes("hold"));
});

test("supersession follows current op without second teardown", async () => {
  const f = fixture(); let count = 0;
  f.o.apply = async p => {
    if (++count === 1) { f.authority.pending = { ...p, opId: "next", revision: 2 }; return { kind: "superseded" }; }
    assert.equal(p.opId, "next"); f.journal.add(p.opId); f.authority.pending = undefined; return f.applied;
  };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_resumed_B");
  assert.equal(f.events.filter(x => x === "reap").length, 1);
});

for (const confirmed of [true, false]) test("supersession without op ordinary owner pause " + confirmed, async () => {
  const f = fixture(); f.o.apply = async () => { f.authority.pending = undefined; return { kind: "superseded" }; };
  f.o.requestAndConfirmOwnerPause = async () => { f.events.push("ordinary-pause"); return confirmed; };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, confirmed ? "codex_owner_paused" : "codex_retained_stop");
  assert.ok(f.events.includes("ordinary-pause") && !f.events.includes("hold"));
});

for (const mode of ["subscription", "api_key"] as const) test("complete mode replacement " + mode, async () => {
  const f = fixture();
  f.o.apply = f.commit({ ...f.applied, codex: mode === "api_key" ?
    { auth_mode: mode, access_token: "fixture", capability: "cap" } :
    { auth_mode: mode, access_token: "fixture", capability: "cap", generation: 117, chatgpt_account_id: "account", chatgpt_plan_type: null } } as LedgerApplyResult);
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_resumed_B");
});

for (const malformed of ["ordinal", "bundle"]) test("invalid applied " + malformed, async () => {
  const f = fixture(); f.o.apply = f.commit({ ...f.applied, ...(malformed === "ordinal" ? { ordinal: 41 } : { codex: {} }) } as LedgerApplyResult);
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_owner_paused");
  assert.ok(!f.events.includes("register"));
});

for (const preserve of [true, false]) for (const kind of ["confirmed_resume_refusal", "uncertain"] as const)
  test("resume " + kind + " preserve=" + preserve, async () => {
    const f = fixture(); f.o.preserveConversation = preserve; f.o.restartAndResume = async () => ({ kind });
    assert.equal((await coordinateCodexSwitch(f.o)).kind, kind === "uncertain" ? "codex_owner_paused" :
      preserve ? "PolicyContinuationRejected" : "codex_fresh_B");
    assert.equal(f.events.includes("fresh"), kind === "confirmed_resume_refusal" && !preserve);
  });

test("commit clears stamp and journal permits lost-reply hold", async () => {
  const f = fixture(); const apply = f.o.apply;
  f.o.apply = async (p, b) => { await apply(p, b); throw Error("lost reply"); };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_owner_paused");
  assert.equal(f.authority.pending, undefined);
  assert.ok(f.journal.has("op"));
  assert.deepEqual(f.holds, [{ opId: "op", reason: "apply_uncertain" }]);
});

for (const stage of ["apply", "publish"] as const) test("new pending at " + stage + " binds hold to newer op", async () => {
  const f = fixture();
  if (stage === "apply") {
    const apply = f.o.apply;
    f.o.apply = async (p, b) => { const result = await apply(p, b); f.authority.pending = { ...p, opId: "new", revision: 2 }; return result; };
  } else {
    f.o.publish = async () => { f.authority.pending = { ...f.o.initial, opId: "new", revision: 2 }; };
  }
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_owner_paused");
  assert.equal(f.holds[0]?.opId, "new");
  assert.ok(!f.events.includes("resume"));
  if (stage === "apply") assert.ok(!f.events.includes("publish"));
});

for (const priorAmbiguous of [false, true]) test("definitive refusal reason preserves ledger ambiguity " + priorAmbiguous, async () => {
  const f = fixture();
  f.o.apply = async () => ({ kind: "not_applied", reason: "vault_locked", ...(priorAmbiguous ? { priorAmbiguous: true as const } : {}) });
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_owner_paused");
  assert.equal(f.holds[0]?.reason, priorAmbiguous ? "apply_uncertain" : "not_applied_after_teardown");
});

test("unconfirmed op hold with pending retains custody", async () => {
  const f = fixture(); f.o.apply = async () => ({ kind: "uncertain", detail: "lost_reply" });
  f.o.uncertainHold = async () => false;
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_retained_stop");
  assert.ok(!f.events.includes("ordinary-pause"));
});

for (const throwing of [false, true]) test("persistence failure has independent safe clear context " + throwing, async () => {
  const f = fixture();
  f.o.persistRollout = async () => { if (throwing) throw Error("persist"); return { kind: "failed" }; };
  f.o.preflight.confirmFailedClear = async () => { assert.fail("fabricated API refusal"); };
  f.o.confirmPersistFailedClear = async (op, bounds) => {
    assert.equal(op.opId, "op"); bounds.guard(); f.authority.pending = undefined; return true;
  };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_continued_A");
});

for (const field of ["capabilityEpoch", "messageSeq"] as const) test("zero " + field + " rejected", async () => {
  const f = fixture(); f.o.apply = f.commit({ ...f.applied, [field]: 0 } as LedgerApplyResult);
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_owner_paused");
  assert.ok(!f.events.includes("publish"));
});

test("outer short deadline clips probe delay and clear adapters", async () => {
  for (const refusal of [false, true]) {
    const f = fixture(); f.o.deadline = 100;
    f.o.preflight.probe = async (p, b) => {
      assert.equal(b.deadline, 100);
      return { kind: "not_applied", reason: refusal ? "vault_locked" : "credential_busy", ...p };
    };
    f.o.preflight.delay = async (ms, b) => { assert.equal(b.deadline, 100); assert.ok(ms <= 100); f.advance(); };
    f.o.preflight.confirmFailedClear = async (_p, _r, b) => { assert.equal(b.deadline, 100); f.authority.pending = undefined; return true; };
    assert.equal((await coordinateCodexSwitch(f.o)).kind, refusal ? "codex_continued_A" : "codex_retained_stop");
  }
});

for (const afterTeardown of [false, true]) test("barrier supersession follows with sticky teardown " + afterTeardown, async () => {
  const f = fixture(); let changed = false;
  const reconcile = f.o.reconcile;
  let count = 0;
  f.o.reconcile = async b => {
    count++;
    // Initial loop authority + preflight before/after + coordinator barrier.
    if (!afterTeardown && count === 4 && !changed) {
      changed = true; f.authority.pending = { ...f.o.initial, opId: "new", revision: 2 };
    }
    return reconcile(b);
  };
  if (afterTeardown) f.o.teardown = async () => {
    f.events.push("reap"); f.authority.pending = { ...f.o.initial, opId: "new", revision: 2 }; return true;
  };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_resumed_B");
  assert.ok(f.journal.has("new"));
  assert.equal(f.events.filter(e => e === "reap").length, 1);
  assert.ok(!f.events.includes("A") && !f.events.includes("clear"));
});

for (const stage of ["registerSecrets", "publish", "restartAndResume"] as const)
  test("truthful hold reason for " + stage, async () => {
    const f = fixture(); f.o[stage] = async () => { throw Error("failure"); };
    assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_owner_paused");
    assert.equal(f.holds[0]?.reason, stage === "restartAndResume" ? "initialization_failed" : "apply_uncertain");
  });

test("committed report refusal never uses clean stopped settlement", async () => {
  const f = fixture(); const apply = f.o.apply;
  f.o.apply = async (p, b) => { await apply(p, b); throw Error("lost"); };
  f.o.uncertainHold = async p => { assert.equal(p.opId, "op"); f.events.push("journal-refused"); return false; };
  f.o.requestAndConfirmOwnerPause = async () => { f.events.push("ordinary-pause"); return false; };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_retained_stop");
  assert.ok(f.events.includes("journal-refused"));
  assert.ok(!f.events.includes("ordinary-pause"));
});

test("preflight reconciliation gets clipped outer bounds", async () => {
  const f = fixture(); f.o.deadline = 50;
  f.o.reconcile = async b => { assert.equal(b.deadline, 50); b.guard(); return { ...f.authority }; };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_resumed_B");
});

test("cancellation during throwing persistence cannot clear or reopen", async () => {
  const f = fixture();
  f.o.persistRollout = async () => { f.abort.abort(); throw Error("persist"); };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_retained_stop");
  assert.ok(!f.events.includes("persist-clear") && !f.events.includes("A"));
});

test("resumes the exact session attested by the usable persisted rollout", async () => {
  const f = fixture();
  f.o.persistRollout = async () => ({ kind: "usable", sessionId: "current-thread" });
  f.o.restartAndResume = async (_bundle, saved) => {
    assert.equal(saved, "current-thread"); return { kind: "resumed" };
  };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_resumed_B");
});

test("an empty saved session is failed persistence before any teardown", async () => {
  const f = fixture();
  f.o.persistRollout = async () => ({ kind: "usable", sessionId: "" });
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_continued_A");
  assert.ok(f.events.includes("persist-clear"));
  assert.ok(!f.events.includes("reap") && !f.events.includes("apply"));
});

test("a supposedly applied op whose stamp remains is held before B publication", async () => {
  const f = fixture(); f.o.apply = async () => f.applied;
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_owner_paused");
  assert.ok(!f.events.includes("register") && !f.events.includes("root"));
});


for (const change of ["pending", "owned", "nonce", "generation"] as const)
  test("clear response reconciles " + change + " before any A turn", async () => {
    const f = fixture();
    f.o.preflight.probe = async p => p.opId === "op"
      ? { kind: "not_applied", reason: "vault_locked", ...p } : { kind: "ready", expectedEpoch: 11, ...p };
    f.o.preflight.confirmFailedClear = async () => {
      f.authority.pending = change === "pending" ? { ...f.o.initial, opId: "new", revision: 2 } : undefined;
      if (change === "owned") f.authority.owned = false;
      if (change === "nonce") f.authority.registerNonce = "new-nonce";
      if (change === "generation") f.authority.claimGeneration++;
      return true;
    };
    assert.equal((await coordinateCodexSwitch(f.o)).kind, change === "pending" ? "codex_resumed_B" : "codex_fenced");
    assert.ok(!f.events.includes("A"));
    assert.deepEqual([...f.journal], change === "pending" ? ["new"] : []);
  });

for (const barrier of ["before-teardown", "after-teardown", "after-clear"] as const)
  test("successors share ten second budget at " + barrier, async () => {
    const f = fixture(); let time = 0; let reads = 0;
    const deadlines: number[] = [];
    f.o.now = () => time;
    const successor = () => { f.authority.pending = { ...f.o.initial, opId: "new", revision: 2 }; };
    const reconcile = f.o.reconcile;
    f.o.reconcile = async b => {
      if (++reads === 4 && barrier === "before-teardown") successor();
      return reconcile(b);
    };
    f.o.teardown = async () => {
      f.events.push("reap");
      if (barrier === "after-teardown") successor();
      return true;
    };
    f.o.preflight.confirmFailedClear = async () => { successor(); return true; };
    f.o.preflight.probe = async (p, b) => {
      deadlines.push(b.deadline);
      if (p.opId === "op") {
        time += 9000;
        return barrier === "after-clear" ? { kind: "not_applied", reason: "vault_locked", ...p } : { kind: "ready", expectedEpoch: 11, ...p };
      }
      assert.equal(b.deadline - time, 1000);
      time += 1000;
      b.signal.throwIfAborted();
      if (time >= b.deadline) throw new Error("deadline");
      return { kind: "ready", expectedEpoch: 11, ...p };
    };
    assert.equal((await coordinateCodexSwitch(f.o)).kind,
      barrier === "after-teardown" ? "codex_owner_paused" : "codex_retained_stop");
    assert.deepEqual(deadlines, [10000, 10000]);
    assert.equal(time, 10000);
    assert.equal(f.events.includes("reap"), barrier === "after-teardown");
    assert.ok(!f.events.includes("A") && !f.events.includes("apply"));
    if (barrier === "after-teardown") assert.equal(f.holds[0]?.opId, "new");
  });

for (const mode of ["transport", "busy", "clear-transport"] as const)
  test("shared budget preserves bounded " + mode + " retries", async () => {
    const f = fixture(); let time = 0; let probes = 0; let clears = 0;
    f.o.now = () => time;
    f.o.preflight.delay = async (ms, b) => {
      assert.equal(b.deadline, 10000); time += ms;
    };
    f.o.preflight.probe = async (p, b) => {
      assert.equal(b.deadline, 10000); probes++;
      if (mode === "transport") throw Error("transport");
      return { kind: "not_applied", reason: mode === "busy" ? "credential_busy" : "vault_locked", ...p };
    };
    f.o.preflight.confirmFailedClear = async (_p, _r, b) => {
      assert.equal(b.deadline, 10000); clears++;
      throw Error("clear transport");
    };
    assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_retained_stop");
    assert.equal(probes, mode === "transport" ? 3 : mode === "busy" ? 6 : 1);
    assert.equal(clears, mode === "transport" ? 0 : 1);
    assert.ok(time <= 10000);
    assert.ok(!f.events.includes("reap") && !f.events.includes("apply") && !f.events.includes("A"));
  });

test("deadline exhausted by old probe retains A before teardown", async () => {
  const f = fixture(); let time = 0;
  f.o.now = () => time;
  f.o.preflight.probe = async p => { time = 10000; return { kind: "ready", expectedEpoch: 11, ...p }; };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_retained_stop");
  assert.ok(!f.events.includes("reap") && !f.events.includes("apply") && !f.events.includes("A"));
});

test("successor epoch advance cannot relatch the stopped settlement tuple", async () => {
  const f = fixture();
  let probes = 0;
  let applies = 0;
  f.o.preflight.probe = async p => ({ kind: "ready", expectedEpoch: ++probes === 1 ? 11 : 12, ...p });
  f.o.apply = async p => {
    applies++;
    f.authority.pending = { ...p, opId: "successor", revision: 2 };
    return { kind: "superseded" };
  };
  f.o.uncertainHold = async () => { f.authority.pending = undefined; return false; };
  f.o.requestAndConfirmOwnerPause = async tuple => {
    assert.deepEqual(tuple, { claimGeneration: 7, registerNonce: "nonce", expectedEpoch: 11 });
    assert.ok(Object.isFrozen(tuple));
    return false;
  };
  assert.equal((await coordinateCodexSwitch(f.o)).kind, "codex_retained_stop");
  assert.equal(applies, 1);
  assert.equal(probes, 2);
  assert.equal(f.events.filter(e => e === "reap").length, 1);
  assert.ok(!f.events.includes("A") && !f.events.includes("publish") && !f.events.includes("root"));
});
