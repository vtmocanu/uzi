// M0 experiment only: fixed marker effects, not the production Bash/path policy.
import assert from "node:assert/strict";
import { appendFileSync, readFileSync } from "node:fs";
import path from "node:path";

const grants = Object.freeze({
  lead: Object.freeze(["m0_read", "m0_signal", "m0_delegate"]),
  coder: Object.freeze(["m0_read", "m0_write"]),
  reviewer: Object.freeze(["m0_read"]),
});
export const fixtureTools = ["m0_read", "m0_write", "m0_signal", "m0_delegate"].map((name) => ({
  name, description: "M0 worker-owned fixed harmless fixture",
  inputSchema: { type: "object", properties: {
    role: { type: "string" }, claimedRole: { type: "string" }, claimedRoot: { type: "boolean" },
    claimedThreadId: { type: "string" }, phase: { type: "string" },
  }, additionalProperties: false },
}));
export const reply = (success, text) => ({ result: { success, contentItems: [{ type: "inputText", text }] } });

export class FixtureBroker {
  #threads = new Map();
  #seen = new Set();
  #tasks = new Set();
  #revoked = false;
  #cursor = 0;
  #listener;
  #probe;
  #decide;
  #beforeEffect;
  constructor(probe, { decide = () => true, beforeEffect = async () => {} } = {}) {
    this.#probe = probe;
    this.#decide = decide;
    this.#beforeEffect = beforeEffect;
    // Read the real app-server notifications; never accept a model's turn ID.
    this.#listener = () => {
      for (; this.#cursor < probe.messages.length; this.#cursor++) {
        const event = probe.messages[this.#cursor];
        const state = this.#threads.get(event.params?.threadId);
        if (!state) continue;
        if (event.method === "turn/started") state.turn = event.params.turn.id;
        if (event.method === "turn/completed" && state.turn === event.params.turn.id) state.turn = null;
      }
    };
    probe.events.on("change", this.#listener);
  }

  // Worker-only operations: none is reachable through fixtureTools.
  register(threadId, { role, root = false, phase = "plan" }) {
    assert.ok(!this.#revoked && !this.#threads.has(threadId));
    assert.ok(Object.hasOwn(grants, role));
    assert.ok(["plan", "implement"].includes(phase));
    assert.equal(typeof root, "boolean");
    if (root) assert.ok(![...this.#threads.values()].some((state) => state.identity.root));
    this.#threads.set(threadId, { identity: Object.freeze({ role, root }), phase, turn: null });
  }

  setPhase(threadId, phase) {
    assert.ok(!this.#revoked && ["plan", "implement"].includes(phase));
    const state = this.#threads.get(threadId);
    assert.ok(state && !state.turn && this.#tasks.size === 0, "worker changes phase only after turns and callbacks settle");
    state.phase = phase;
  }

  get pendingCount() { return this.#tasks.size; }
  revoke() { this.#revoked = true; }
  async dispose() {
    this.revoke(); // Admission closes synchronously, before waiting for callbacks.
    await Promise.allSettled([...this.#tasks]);
    this.#probe.events.off("change", this.#listener);
  }

  handle(request) {
    // Snapshot before any await: later mutation of transport objects grants nothing.
    let params;
    try { params = structuredClone(request.params); }
    catch { return Promise.resolve(reply(false, "M0 invalid callback")); }
    const task = this.#run(params).catch(() => reply(false, "M0 policy failed"));
    this.#tasks.add(task);
    void task.then(() => this.#tasks.delete(task));
    return task;
  }

  #live(params, state) {
    return !this.#revoked && state && typeof params.turnId === "string"
      && state.turn === params.turnId && this.#threads.get(params.threadId) === state;
  }

  async #run(params) {
    const state = this.#threads.get(params?.threadId);
    if (!this.#live(params, state)) return reply(false, "M0 unknown or stale identity");
    if (typeof params.callId !== "string" || !params.callId || params.namespace !== null
      || !params.arguments || typeof params.arguments !== "object" || Array.isArray(params.arguments)) {
      return reply(false, "M0 invalid callback");
    }
    // callId is the runtime tool identity; JSON-RPC request.id is only routing.
    const key = JSON.stringify([params.threadId, params.turnId, params.callId]);
    if (this.#seen.has(key)) return reply(false, "M0 duplicate callback");
    this.#seen.add(key);
    const { role, root } = state.identity;
    if (!grants[role].includes(params.tool)) return reply(false, "M0 role denied");
    if (params.tool === "m0_signal" && !root) return reply(false, "M0 root required");
    if (params.tool === "m0_write" && state.phase !== "implement") return reply(false, "M0 plan write denied");
    if (params.tool === "m0_delegate" && (!root || !["coder", "reviewer"].includes(params.arguments.role))) {
      return reply(false, "M0 delegation denied");
    }
    const decision = await this.#decide(Object.freeze({ ...state.identity, phase: state.phase, tool: params.tool }));
    if (decision !== true) return reply(false, "M0 policy denied");
    await this.#beforeEffect(params);
    if (!this.#live(params, state)) return reply(false, "M0 revoked before effect");
    // These bounded synchronous fixture commits have no await between final
    // admission and effect. Real async adapters must establish their own barrier.
    if (params.tool === "m0_read") return reply(true, readFileSync(path.join(this.#probe.workspace, "policy-source"), "utf8"));
    if (params.tool === "m0_signal") {
      appendFileSync(path.join(this.#probe.workspace, "policy-signals"), "M0 root signal\n");
      return reply(true, "M0 signal recorded");
    }
    if (params.tool === "m0_write") {
      appendFileSync(path.join(this.#probe.workspace, "policy-writes"), "M0 coder write\n");
      return reply(true, "M0 write recorded");
    }
    const childId = await this.#probe.thread("never", fixtureTools);
    if (!this.#live(params, state)) return reply(false, "M0 revoked before child turn");
    this.register(childId, { role: params.arguments.role, phase: state.phase });
    const turnId = await this.#probe.turn(childId, `M0 child ${params.arguments.role}`);
    const terminal = await this.#probe.completed(childId, turnId);
    if (!this.#live(params, state)) return reply(false, "M0 revoked after child");
    if (terminal.params.turn.status !== "completed") return reply(false, "M0 child failed");
    return reply(true, "M0 delegated child completed");
  }
}
