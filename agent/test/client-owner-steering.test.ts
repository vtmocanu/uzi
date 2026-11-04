import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { it } from "node:test";
import { WorkerClient } from "../src/client.js";
import { OWNER_STEERING_WIRE_MAX_BYTES } from "../src/code-cross-check-contract.js";
import { SEMANTIC_ENVELOPE_MAX_BYTES } from "../src/semantic-plan.js";
import { nullLogger, makeClaim } from "./helpers.js";
import { semanticBoundaryFixture } from "./semantic-boundary-fixture.js";

const BASE = 2 * 1024 * 1024;
const snapshot = JSON.stringify({ version: 1, cutoff_input_id: 3, owner_entries: [], question_context: [], omitted_entries: 0, overflow: false, overflow_reason: "" });
const steering = { snapshot, digest: createHash("sha256").update(snapshot).digest("hex") };
const planCheck = { stage: "plan" as const, round: 1, lead_run_id: "lead", candidate_digest: "b".repeat(64), deadline_at: "2030-01-01T00:00:00Z",
  plan_md: "plan", milestones: [], required_capabilities: [], required_tools: [], size_class: "s", base_commit: "a".repeat(40), planning_diff: "" };
const codeCheck = { stage: "code" as const, round: 1, lead_run_id: "lead", candidate_digest: "c".repeat(64), deadline_at: "2030-01-01T00:00:00Z",
  plan_md: "plan", milestones: [], required_capabilities: [], required_tools: [], size_class: "s", base_commit: "a".repeat(40),
  head_commit: "b".repeat(40), code_context: {}, code_diff: "", guidance_snapshot: "" };
const withFetch = (res: () => Response) => new WorkerClient("http://example.test", "fixture", "test", nullLogger(), { fetch: async () => res() });

/** JSON text padded with trailing whitespace to exactly `bytes`, or its natural size when larger. */
function padded(value: unknown, bytes: number): string {
  const text = JSON.stringify(value);
  assert.ok(Buffer.byteLength(text) <= bytes);
  return text + " ".repeat(bytes - Buffer.byteLength(text));
}
const hdr = (extra: Record<string, string> = {}) => ({ "X-Uzi-Claim-Kind": "cross_check", ...extra });
const planClaim = (withField: boolean) => makeClaim({ kind: "cross_check", cross_check: planCheck,
  ...(withField ? { cross_check_owner_steering: steering } : {}) });

async function accepts(body: string, headers: Record<string, string>): Promise<boolean> {
  try { await withFetch(() => new Response(body, { headers })).claimCrossCheck(); return true; }
  catch (e) { assert.match(String(e), /exceeds|owner steering|marker/, "bounded refusal"); return false; }
}

it("owner steering header grants exactly the wire allowance on a legacy plan claim", async () => {
  const h = hdr({ "X-Uzi-Owner-Steering": "v1" });
  const cap = BASE + OWNER_STEERING_WIRE_MAX_BYTES;
  assert.equal(await accepts(padded(planClaim(true), cap), h), true, "cap accepted");
  assert.equal(await accepts(padded(planClaim(true), cap) + " ", h), false, "cap+1 refused");
  const got = await withFetch(() => new Response(padded(planClaim(true), BASE + 1000), { headers: h })).claimCrossCheck();
  assert.deepEqual(got?.cross_check_owner_steering, steering);
});

it("without the header the base cap is unchanged for plan and code stage", async () => {
  assert.equal(await accepts(padded(planClaim(false), BASE), hdr()), true);
  assert.equal(await accepts(padded(planClaim(false), BASE + 1), hdr()), false);
  const code = makeClaim({ kind: "cross_check", cross_check: codeCheck });
  assert.equal(await accepts(padded(code, BASE), hdr()), true);
  assert.equal(await accepts(padded(code, BASE + 1), hdr()), false);
});

it("unknown header value, header without field, field without header and code-stage field are refused", async () => {
  for (const v of ["v2", "", "V1", "v1,v1"]) {
    assert.equal(await accepts(JSON.stringify(planClaim(true)), hdr({ "X-Uzi-Owner-Steering": v })), false, `value ${v}`);
  }
  assert.equal(await accepts(JSON.stringify(planClaim(false)), hdr({ "X-Uzi-Owner-Steering": "v1" })), false, "header w/o field");
  assert.equal(await accepts(JSON.stringify(planClaim(true)), hdr()), false, "field w/o header");
  const code = makeClaim({ kind: "cross_check", cross_check: codeCheck, cross_check_owner_steering: steering });
  assert.equal(await accepts(JSON.stringify(code), hdr({ "X-Uzi-Owner-Steering": "v1" })), false, "code stage field");
  const badDigest = makeClaim({ kind: "cross_check", cross_check: planCheck, cross_check_owner_steering: { ...steering, digest: "0".repeat(64) } });
  assert.equal(await accepts(JSON.stringify(badDigest), hdr({ "X-Uzi-Owner-Steering": "v1" })), false, "digest mismatch");
});

it("a stray steering header on a code claim grants no allowance", async () => {
  const code = makeClaim({ kind: "cross_check", cross_check: codeCheck });
  assert.equal(await accepts(padded(code, BASE + 1), hdr({ "X-Uzi-Owner-Steering": "v1" })), false);
});

it("semantic claim: base 64 MiB plus the allowance with the header, 64 MiB without", async () => {
  const f = semanticBoundaryFixture();
  const sem = (withField: boolean) => makeClaim({ kind: "cross_check", cross_check: f.checker,
    ...(withField ? { cross_check_owner_steering: steering } : {}) });
  const marker = { "X-Uzi-Candidate-Identity-Version": "semantic_v1" };
  const withH = hdr({ ...marker, "X-Uzi-Owner-Steering": "v1" });
  const cap = SEMANTIC_ENVELOPE_MAX_BYTES + OWNER_STEERING_WIRE_MAX_BYTES;
  assert.equal(await accepts(padded(sem(true), cap), withH), true);
  assert.equal(await accepts(padded(sem(true), cap) + " ", withH), false);
  assert.equal(await accepts(padded(sem(false), SEMANTIC_ENVELOPE_MAX_BYTES), hdr(marker)), true);
  assert.equal(await accepts(padded(sem(false), SEMANTIC_ENVELOPE_MAX_BYTES) + " ", hdr(marker)), false);
});

it("the steering header on a non-claim semantic response leaves the cap at the semantic envelope", async () => {
  const f = semanticBoundaryFixture();
  const body = JSON.stringify(f.response).padEnd(SEMANTIC_ENVELOPE_MAX_BYTES + 1, " ");
  const res = () => new Response(body, { headers: { "X-Uzi-Candidate-Identity-Version": "semantic_v1",
    "X-Uzi-Claim-Kind": "cross_check", "X-Uzi-Owner-Steering": "v1" } });
  await assert.rejects(withFetch(res).planCrossCheckStatus("run", 1), /exceeds 67108864/);
});
