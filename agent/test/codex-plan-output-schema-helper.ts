import assert from "node:assert/strict";
import { PassThrough, Writable } from "node:stream";
import { CodexCrossCheck, PLAN_VERDICT_SCHEMA, type CrossCheckModelDeps } from "../src/codex/cross-check.js";
import type { CodexRootHandle } from "../src/codex/launcher.js";
import type { ClaimResponse } from "../src/protocol.js";
import { recordingLogger } from "./helpers.js";

export const ANCHOR = "export const schemaProbeAnchor = 4103;\n";
export const VERDICT = JSON.stringify({ verdict: "approve", summary: "Synthetic anchor checked", items: [] });
export const CAPTURE_LIMIT = 4 * 1024 * 1024;
export type Frame = { id?: number | string; method?: string; params?: Record<string, any>; result?: any; error?: unknown };
export type Event = { direction: "in" | "out"; frame: Frame; adapted?: Frame };
export interface SchemaFixture {
 version: 1;
 provenance: { codexVersion: string; command: string; capturedAt: string; substitutions: string[]; limitations: string[] };
 events: Event[];
 providerRequests: Record<string, any>[];
 verdict: string;
}

export function frameSink(onFrame: (frame: Frame) => void): Writable {
 let pending = "";
 return new Writable({ write(chunk, _encoding, done) {
  try {
   pending += chunk.toString();
   assert.ok(Buffer.byteLength(pending) <= CAPTURE_LIMIT, "NDJSON buffer cap");
   let index: number;
   while ((index = pending.indexOf("\n")) >= 0) {
    const line = pending.slice(0, index); pending = pending.slice(index + 1);
    if (line) onFrame(JSON.parse(line));
   }
   done();
  } catch (error) { done(error as Error); }
 } });
}

export function streamRoot(onFrame: (frame: Frame, emit: (frame: Frame) => void) => void,
 disposed: string[], name: string): CodexRootHandle {
 const stdout = new PassThrough();
 const emit = (frame: Frame): void => { stdout.write(JSON.stringify(frame) + "\n"); };
 return {
  transport: { stdin: frameSink(frame => onFrame(frame, emit)), stdout, stderr: null },
  whenFailed: new Promise<Error>(() => {}), failed: undefined,
  dispose: async () => { disposed.push(name); stdout.end(); return { clean: true }; },
 } as unknown as CodexRootHandle;
}

export function probeClaim(): ClaimResponse {
 return {
  kind: "cross_check", run_id: "schema-probe", issue_iid: null, issue_title: "Synthetic schema probe",
  repo: { id: "repo", full_name: "owner/repo", url: "https://example.test/repo", clone_url: "https://example.test/repo.git", default_branch: "main" },
  last_seq: 0, agents: [], issue_description: "Read anchor.ts and assess the synthetic plan.",
  config: { default_model: "gpt-6-astra", default_effort: "high" },
  secrets: { forge_pat: "", codex: { auth_mode: "api_key", access_token: "synthetic-claim", capability: "synthetic-capability" } },
  cross_check: { stage: "plan", lead_run_id: "lead", round: 1, candidate_digest: "synthetic",
   deadline_at: "2099-01-01T00:00:00Z", plan_md: "Verify anchor.ts", milestones: [],
   required_capabilities: [], required_tools: [], size_class: "small", base_commit: "a".repeat(40), planning_diff: "" },
 } as ClaimResponse;
}

export async function runChecker(deps: CrossCheckModelDeps, checkout: string, home: string, credential: string,
 signal: AbortSignal): Promise<{ text: string; cleanup: boolean }> {
 let cleanup = false;
 const checker = new CodexCrossCheck({
  releaseCodex: async () => ({ auth_mode: "api_key", access_token: credential }),
  refreshCodex: async () => { throw new Error("unexpected refresh"); },
 }, recordingLogger().logger, deps);
 const text = await checker.run(probeClaim(), checkout, home, signal, async () => {}, () => { cleanup = true; });
 return { text, cleanup };
}

/** Evidence checks are independent of the replay driver: missing callbacks cannot silently pass. */
export function assertFixture(f: SchemaFixture, expectedVersion = "0.164.0"): void {
 assert.equal(f.version, 1);
 // Replay defaults to the recorded runtime; a real capture supplies its independently checked version.
 assert.equal(f.provenance.codexVersion, `codex-cli ${expectedVersion}`);
 assert.ok(f.events.length > 0 && f.events.length < 10000);
 assert.ok(Buffer.byteLength(JSON.stringify(f)) <= CAPTURE_LIMIT);
 assert.equal(f.verdict, VERDICT);
 const changed = f.events.filter(e => e.adapted);
 assert.equal(changed.length, 2);
 for (const e of changed) {
  const expected = structuredClone(e.frame);
  if (expected.method === "thread/start") {
   assert.equal(expected.params!.modelProvider, "openai");
   expected.params!.modelProvider = "uzi-m3b-openai";
  } else {
   assert.equal(expected.method, "turn/start");
   assert.equal(Object.hasOwn(expected.params!, "outputSchema"), false, "M1 precedes production wiring");
   expected.params!.outputSchema = PLAN_VERDICT_SCHEMA;
  }
  assert.deepEqual(e.adapted, expected, "only approved outbound coordinates may change");
 }
 const turns = changed.filter(e => e.frame.method === "turn/start");
 assert.equal(turns.length, 1);
 assert.deepEqual(turns[0]!.adapted!.params!.outputSchema, PLAN_VERDICT_SCHEMA);
 assert.ok(f.providerRequests.length >= 2 && f.providerRequests.length <= 3);
 for (const request of f.providerRequests) {
  assert.equal(request.text?.format?.type, "json_schema");
  assert.equal(request.text.format.strict, true);
  assert.deepEqual(request.text.format.schema, PLAN_VERDICT_SCHEMA);
 }
 const calls = f.events.filter(e => e.direction === "in" && e.frame.method === "item/tool/call");
 assert.equal(calls.length, 1);
 assert.equal(calls[0]!.frame.params!.tool, "uzi_read");
 assert.equal(calls[0]!.frame.params!.arguments.path, "anchor.ts");
 const reply = f.events.find(e => e.direction === "out" && !e.frame.method && e.frame.id === calls[0]!.frame.id);
 assert.equal(reply?.frame.result?.success, true);
 assert.ok(JSON.stringify(reply!.frame.result).includes(ANCHOR.trim()));
 assert.ok(f.providerRequests.slice(1).some(r => JSON.stringify(r.input).includes(ANCHOR.trim())), "provider received Read result");
 const terminals = f.events.filter(e => e.direction === "in" && e.frame.method === "turn/completed");
 assert.equal(terminals.length, 1);
 assert.equal(terminals[0]!.frame.params!.turn.status, "completed");
}

/** Replays captured server frames through real transport, auth, broker and verdict validation.
 * The synthetic fileop owns only anchor.ts; this makes no OS confinement claim.
 */
export async function replaySchemaFixture(f: SchemaFixture): Promise<{ text: string; cleanup: boolean; disposed: string[]; reads: number }> {
 assert.ok(f.events.length < 10000 && Buffer.byteLength(JSON.stringify(f)) <= CAPTURE_LIMIT);
 const disposed: string[] = [];
 let reads = 0;
 let cursor = 0;
 const ids = new Map<number | string, number | string>();
 const emittedCalls = new Set<number | string>();
 const hasTerminal = f.events.some(e => e.direction === "in" && e.frame.method === "turn/completed");
 const provider = streamRoot((frame, emit) => {
  if (frame.method === "initialize") { emit({ id: frame.id, result: { userAgent: "fixture", codexHome: "/fixture/home/.codex", platformFamily: "unix", platformOs: "linux" } }); return; }
  if (frame.method === "account/login/start") { emit({ id: frame.id, result: { type: "apiKey" } }); return; }
  if (frame.method === "initialized") return;
  const next = f.events[cursor++];
  assert.equal(next?.direction, "out", "unexpected checker request");
  assert.equal(frame.method, next.frame.method);
  if (frame.method) {
   if (next.frame.id !== undefined && frame.id !== undefined) ids.set(next.frame.id, frame.id);
   if (frame.method === "thread/start") {
    // No schema adapter in replay: production request regression is M2.
    assert.equal(frame.params!.modelProvider, "openai");
   }
  } else {
   assert.equal(frame.id, next.frame.id);
   assert.equal(frame.result?.success, true);
   assert.ok(JSON.stringify(frame.result).includes(ANCHOR.trim()), "actual broker Read result");
  }
  while (f.events[cursor]?.direction === "in") {
   const inbound = structuredClone(f.events[cursor++]!.frame);
   if (!inbound.method && inbound.id !== undefined && ids.has(inbound.id)) inbound.id = ids.get(inbound.id);
   if (inbound.method === "item/tool/call" && inbound.id !== undefined) emittedCalls.add(inbound.id);
   emit(inbound);
  }
  // Refuse a missing callback or terminal by ending the tape, not by waiting for a timer.
  const nextFrame = f.events[cursor]?.frame;
  if ((!hasTerminal && cursor === f.events.length)
   || (nextFrame && !nextFrame.method && nextFrame.id !== undefined && !emittedCalls.has(nextFrame.id))) {
   (provider.transport.stdout as PassThrough).end();
  }
 }, disposed, "provider");
 const fileop = streamRoot((frame, emit) => {
  const op = frame as Frame & { op: string; path: string };
  if (op.op === "stat") emit({ id: op.id, ok: true } as Frame);
  else {
   assert.equal(op.op, "read"); assert.equal(op.path, "anchor.ts"); reads++;
   emit({ id: op.id, ok: true, data: Buffer.from(ANCHOR).toString("base64"), size: Buffer.byteLength(ANCHOR) } as Frame);
  }
 }, disposed, "fileop");
 const abort = new AbortController();
 // Keep the deadline referenced: a missing terminal leaves only in-memory streams alive.
 const timer = setTimeout(() => abort.abort(new Error("replay deadline")), 2000);
 let result: Awaited<ReturnType<typeof runChecker>>;
 try {
  result = await runChecker({ prepareHome: async () => {}, launchProvider: async () => provider, launchFileop: async () => fileop },
   "/fixture/checkout", "/fixture/home", "synthetic-replay", abort.signal);
 } finally { clearTimeout(timer); }
 assert.equal(cursor, f.events.length, "all captured frames consumed");
 assert.equal(reads, 1);
 assert.deepEqual(disposed, ["provider", "fileop"]);
 assert.equal(result.cleanup, true);
 assert.equal(result.text, f.verdict);
 return { ...result, disposed, reads };
}
