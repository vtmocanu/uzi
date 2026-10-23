import { it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { checkpointCoordinatesForClaim } from "../src/checkpoint-coordinates.js";
import type { ClaimResponse, RunKind } from "../src/protocol.js";
import { makeClaim } from "./helpers.js";

const ROOT = "11111111-1111-4111-8111-111111111111";
const CHILD = "22222222-2222-4222-8222-222222222222";
const REPO = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const USER = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
function task(): ClaimResponse {
  return makeClaim({ run_id: CHILD, kind: "task", issue_iid: null,
    branch: "uzi/task/11111111-1111-4111-8111-111111111111",
    repo: { id: REPO, clone_url: "unused", url: "unused" },
    checkpoint_coordinates: { version: 1, work_branch: "uzi/task/11111111-1111-4111-8111-111111111111",
      checkpoint_key: "uzi/task/22222222-2222-4222-8222-222222222222",
      run_id: CHILD, user_id: USER, repo_id: REPO, kind: "task", issue_iid: null,
      provenance: [CHILD, ROOT] } });
}

it("consumes every coordinates-v1 oracle without reauthorizing the SQL lineage graph", () => {
  const cases = JSON.parse(readFileSync(new URL("../../fixtures/checkpoint-branch/coordinates-v1.json", import.meta.url), "utf8")) as Array<{
    name: string; nodes: Array<{ id: string; kind: RunKind; branch?: string | null; issue_iid?: number }>;
    eligible: boolean; work_branch: string | null; checkpoint_key: string | null;
  }>;
  for (const c of cases) {
    const node = c.nodes[0]!;
    const claim = makeClaim({ run_id: node.id, kind: node.kind, branch: node.branch,
      issue_iid: node.issue_iid ?? null, repo: { id: REPO, clone_url: "unused", url: "unused" } });
    // The API supplies a descriptor only after SQL authorizes these cases. Ineligible
    // tasks are tested through descriptor absence, not a second graph implementation.
    if (c.eligible) claim.branch = c.work_branch; // claim assembly supplies the canonical branch
    if (c.eligible) claim.checkpoint_coordinates = { version: 1, work_branch: c.work_branch,
      checkpoint_key: c.checkpoint_key, run_id: node.id, user_id: USER, repo_id: REPO,
      kind: node.kind, issue_iid: node.issue_iid ?? null, provenance: c.nodes.map(n => n.id) };
    const pair = checkpointCoordinatesForClaim(claim);
    assert.equal(pair?.work_branch ?? null, c.work_branch, c.name);
    assert.equal(pair?.checkpoint_key ?? null, c.checkpoint_key, c.name);
  }
});

it("absence preserves legacy equal pairs and never authorizes a task from its branch", () => {
  const claim = task();
  delete claim.checkpoint_coordinates;
  assert.equal(checkpointCoordinatesForClaim(claim), undefined);
  assert.equal(checkpointCoordinatesForClaim(makeClaim({ kind: "issue", issue_iid: 42 }))?.checkpoint_key, "agent/issue-42");
  assert.equal(checkpointCoordinatesForClaim(makeClaim({ run_id: ROOT, kind: "self_improve" }))?.checkpoint_key,
    "uzi/self-improve/11111111-1111-4111-8111-111111111111");
});

it("a missing descriptor cannot discard a task checkpoint anchor or unresolved write", () => {
  for (const evidence of [
    { checkpoint_tip: "a".repeat(40) },
    { checkpoint_attested_source: "b".repeat(40) },
    { checkpoint_write_unresolved: true },
  ]) {
    const claim = { ...task(), ...evidence };
    delete claim.checkpoint_coordinates;
    assert.throws(() => checkpointCoordinatesForClaim(claim), /requires authorized coordinates/);
  }
  const legacy = task();
  delete legacy.checkpoint_coordinates;
  legacy.checkpoint_write_unresolved = false;
  assert.equal(checkpointCoordinatesForClaim(legacy), undefined, "absence of a checkpoint does not authorize a key");
});

it("present malformed or unsupported descriptors fail closed", () => {
  const edits: unknown[] = [null, [], "v1", { version: 2 }, { run_id: ROOT }, { repo_id: USER },
    { user_id: "not-a-uuid" }, { kind: "issue" }, { issue_iid: 42 }, { work_branch: "feature/foreign" },
    { checkpoint_key: "uzi/task/11111111-1111-4111-8111-111111111111" },
    { provenance: [] }, { provenance: [ROOT, CHILD] }, { provenance: [CHILD, CHILD] },
    { provenance: [CHILD, USER] }, { provenance: Array(65).fill(CHILD) }];
  for (const edit of edits) {
    const claim = task();
    claim.checkpoint_coordinates = edit && typeof edit === "object" && !Array.isArray(edit)
      ? { ...claim.checkpoint_coordinates as object, ...edit } : edit;
    assert.throws(() => checkpointCoordinatesForClaim(claim), /invalid authorized/);
  }
  assert.throws(() => checkpointCoordinatesForClaim({ ...task(), checkpoint_coordinates: {} }));
  const claim = task();
  claim.branch = "feature/foreign";
  assert.throws(() => checkpointCoordinatesForClaim(claim));
});

it("descriptor acknowledgement needs no protocol feature bit, and its copy stays frozen", () => {
  const claim = task();
  const pair = checkpointCoordinatesForClaim(claim)!;
  assert.equal(pair.checkpoint_key, "uzi/task/22222222-2222-4222-8222-222222222222");
  const wire = claim.checkpoint_coordinates as { checkpoint_key: string; provenance: string[] };
  wire.checkpoint_key = "foreign";
  wire.provenance.pop();
  assert.equal(pair.checkpoint_key, "uzi/task/22222222-2222-4222-8222-222222222222");
  assert.deepEqual(pair.provenance, [CHILD, ROOT]);
  assert.ok(Object.isFrozen(pair));
  assert.ok(Object.isFrozen(pair.provenance));
});
