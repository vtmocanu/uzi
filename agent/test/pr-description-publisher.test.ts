import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { PrDescriptionMalformedResponse, type WorkerClient } from "../src/client.js";
import { ForgeError, ForgeResponseTooLarge, MR_DETAIL_MAX_BYTES, type MergeRequestDetail } from "../src/forge.js";
import type { DeliveryContext } from "../src/pr-description-context.js";
import {
  PrDescriptionPublisher,
  isDeterministicRegion,
  reconcileCompletion,
  repoPathFromUrl,
  withStalenessLine,
  type DeliveryPass,
  type PublicationSpec,
  type PublisherForge,
  type PublisherApi,
} from "../src/pr-description-publisher.js";
import {
  BODY_CAP_CHARS,
  COMPLETION_START,
  REGION_END,
  REGION_START,
  STALENESS_START,
  closingDirectiveFor,
  parseOwnedBlocks,
  regionSha256,
  renderBody,
  renderCompletionBlock,
  renderRegion,
} from "../src/pr-description.js";
import { PR_DESC_ACK_OUTCOMES, type PrDescriptionSize, type PrDescriptionState } from "../src/protocol.js";
import type { PrSummaryClaim } from "../src/signals.js";
import type { DeliverySummary } from "../src/summary-runner.js";
import { FakePrDescApi, clientFor } from "./fake-pr-desc-api.js";
import { nullLogger } from "./helpers.js";

// PRD #1798 M6 (M6b): the PR-description publisher (steps 1-9, D8-D12, D15, D17) and the completion
// interlock's reconcile, driven against an in-memory forge and an in-memory model of the api's
// pr-description routes (real WorkerClient decoding, so published fields are real sanitized
// instances).

const RUN = "11111111-2222-3333-4444-555555555555";
const H1 = "a".repeat(40);
const H2 = "c".repeat(40);
const H3 = "d".repeat(40);
const BASE = "b".repeat(40);
const MR = 42;
const IID = 7;
const SIZE_LINE = "**Size:** code +3 −1 · 1 file";
const SIZE_TABLE = [
  "**Size:** 3 files",
  "",
  "| Category | Added | Deleted |",
  "|:---------|------:|--------:|",
  "| Code | +1,203 | −1 |",
  "| Docs | +2 | −0 |",
  "| **Total** | **+1,205** | **−1** |",
].join("\n");
const ZERO = { added: 0, deleted: 0 };
const SIZE: PrDescriptionSize = { unavailable: false, files: 1, code: { added: 3, deleted: 1 }, tests: ZERO, docs: ZERO, config: ZERO, generated: ZERO, vendored: ZERO };
const CTX: DeliveryContext = {
  issue: "",
  prd: null,
  plan: null,
  previous: null,
  claims: null,
  commits: "",
  paths: "",
  diff: "",
  truncated: { issue: false, prd: false, plan: false, previous: false, claims: false, commits: false, paths: false, diff: false },
  bytes: 0,
};
const DIAGRAM = {
  kind: "sequence" as const,
  nodes: [{ key: "worker", label: "Worker" }, { key: "api", label: "API" }],
  edges: [{ from: "worker", to: "api", label: "stage" }, { from: "api", to: "worker", label: "ack" }],
};
const SUMMARY: DeliverySummary = {
  summary: "Reviewers can now mark a finding done from the Findings page.",
  changes: ["Web: a Mark done button on each row."],
  scope_notes: [],
  review_pointers: ["The disposition rules are shared with the judge."],
};
const LEAD: PrSummaryClaim = {
  what: "Adds a Mark done button.",
  why: "Findings piled up.",
  verification: [{ command: "task gate:web", result: "pass" }],
  verifiedAtSha: H1,
};

// ── Fakes ───────────────────────────────────────────────────────────────────────────────────

class FakeForge implements PublisherForge {
  pr = { head: H1, target: "main", description: "" };
  reads = 0;
  readonly writes: string[] = [];
  /** Runs before the nth read (1-based), so a test can edit the PR between reads. */
  beforeRead?: (n: number, pr: FakeForge["pr"]) => void;
  failRead?: (n: number) => boolean;
  failWrite = false;
  /** Runs after a write lands, so a test can model a concurrent edit the confirm read sees. */
  afterWrite?: (pr: FakeForge["pr"]) => void;

  async getMergeRequest(_url?: string, _pat?: string, _iid?: number, signal?: AbortSignal): Promise<MergeRequestDetail> {
    // Like the real client's fetch: a call under an aborted signal fails before it reaches the forge.
    if (signal?.aborted) throw new ForgeError(0, "aborted");
    this.reads++;
    this.beforeRead?.(this.reads, this.pr);
    if (this.failRead?.(this.reads)) throw new ForgeError(503, "down");
    return { headSha: this.pr.head, targetBranch: this.pr.target, description: this.pr.description, state: "open" };
  }

  async updateMergeRequestDescription(_url: string, _pat: string, _iid: number, description: string, signal?: AbortSignal): Promise<void> {
    if (signal?.aborted) throw new ForgeError(0, "aborted");
    if (this.failWrite) throw new ForgeError(500, "write refused");
    this.writes.push(description);
    this.pr.description = description;
    this.afterWrite?.(this.pr);
  }
}

class FakePass implements DeliveryPass {
  deadlines = 0;
  readonly calls: number[] = [];
  constructor(private readonly out: DeliverySummary | null) {}
  deliverySummaryDeadline(): number {
    this.deadlines++;
    return 1_000_000;
  }
  async generateDeliverySummary(input: { deadlineMs: number }): Promise<DeliverySummary | null> {
    this.calls.push(input.deadlineMs);
    return this.out;
  }
}

let api: FakePrDescApi;
let client: WorkerClient;
let restore: () => void;
/** Every ack outcome any test sent, checked against the six api values at the end. */
const everyAck: string[] = [];

beforeEach(() => {
  api = new FakePrDescApi();
  ({ client, restore } = clientFor(api));
});
afterEach(() => {
  everyAck.push(...api.acks());
  restore();
});

function completion(closes: boolean, staleness?: { describedSha: string; headSha: string }): string {
  return renderCompletionBlock({ issueIid: IID, branch: "agent/issue-7", closes, staleness });
}

function makeSpec(over: Partial<PublicationSpec> = {}): PublicationSpec {
  return {
    runId: RUN,
    claimGeneration: 1,
    claim: { run_id: RUN, secrets: {} },
    repoUrl: "https://gitlab.example.test/o/r",
    pat: "pat",
    repoPath: "o/r",
    mode: "own",
    completionCloses: true,
    blindFallback: true,
    facts: async () => ({ baseSha: BASE, size: { line: SIZE_LINE, size: SIZE } }),
    context: async () => CTX,
    completion: (st) => completion(true, st),
    ...over,
  };
}

interface Rig {
  forge: FakeForge;
  emitted: string[];
  publisher: PrDescriptionPublisher;
  pass: FakePass | null;
}

function rig(pass: FakePass | null = new FakePass(SUMMARY)): Rig {
  const forge = new FakeForge();
  const emitted: string[] = [];
  const publisher = new PrDescriptionPublisher({
    forge,
    api: client,
    pass,
    log: nullLogger(),
    emit: (t) => emitted.push(t),
    sleep: async () => {},
    headLagRetryMs: 0,
  });
  return { forge, emitted, publisher, pass };
}

function apiWith(over: Partial<PublisherApi>): PublisherApi {
  return {
    stagePrDescription: client.stagePrDescription.bind(client),
    bindPrDescription: client.bindPrDescription.bind(client),
    lookupPrDescription: client.lookupPrDescription.bind(client),
    ackPrDescription: client.ackPrDescription.bind(client),
    ...over,
  };
}

describe("api outage while publishing a PR description", () => {
  it("skips later advisory api calls after staging loses transport, so terminal reporting is not held by retries", async () => {
    const forge = new FakeForge();
    const calls: string[] = [];
    const publisher = new PrDescriptionPublisher({
      forge,
      api: apiWith({
        stagePrDescription: async () => { calls.push("stage"); throw new TypeError("fetch failed"); },
        lookupPrDescription: async () => { calls.push("lookup"); throw new TypeError("fetch failed"); },
        bindPrDescription: async () => { calls.push("bind"); throw new TypeError("fetch failed"); },
        ackPrDescription: async () => { calls.push("ack"); throw new TypeError("fetch failed"); },
      }),
      pass: null,
      log: nullLogger(),
      emit: () => {},
      sleep: async () => { calls.push("sleep"); },
      headLagRetryMs: 0,
    });
    const pub = await publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    forge.pr.description = pub.initialBody(completion(true));
    await pub.publish(MR);
    assert.deepEqual(calls, ["stage"]);
  });

  it("stops replaying after the first transport failure from lookup, but retries HTTP 503", async () => {
    const forge = new FakeForge();
    const calls: string[] = [];
    const publisher = new PrDescriptionPublisher({
      forge,
      api: apiWith({
        lookupPrDescription: async () => { calls.push("lookup"); throw new TypeError("fetch failed"); },
        bindPrDescription: async () => { calls.push("bind"); throw new TypeError("fetch failed"); },
        ackPrDescription: async () => { calls.push("ack"); throw new TypeError("fetch failed"); },
      }),
      pass: null,
      log: nullLogger(),
      emit: () => {},
      sleep: async () => { calls.push("sleep"); },
      headLagRetryMs: 0,
    });
    const pub = await publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    forge.pr.description = pub.initialBody(completion(true));
    await pub.publish(MR);
    assert.deepEqual(calls, ["lookup"]);

    api.failNext("lookup", 503);
    api.failNext("lookup", 503);
    const healthy = rig(null);
    const healthyPub = await healthy.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    healthy.forge.pr.description = healthyPub.initialBody(completion(true));
    await healthyPub.publish(MR);
    assert.equal(api.calls.filter((c) => c.op === "lookup").length, 3);
  });

  it("still replays a malformed lookup response because the api answered", async () => {
    const forge = new FakeForge();
    let attempts = 0;
    const publisher = new PrDescriptionPublisher({
      forge,
      api: apiWith({
        lookupPrDescription: async (runId, request, signal) => {
          attempts++;
          if (attempts === 1) throw new PrDescriptionMalformedResponse("/pr-description/lookup", "lookup");
          return client.lookupPrDescription(runId, request, signal);
        },
      }),
      pass: null,
      log: nullLogger(),
      emit: () => {},
      sleep: async () => {},
      headLagRetryMs: 0,
    });
    const pub = await publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    forge.pr.description = pub.initialBody(completion(true));
    await pub.publish(MR);
    assert.equal(attempts, 2);
  });
});

/** A new PR: prepare, create with the initial body, publish. */
async function newPr(r: Rig, spec: PublicationSpec = makeSpec()) {
  const pub = await r.publisher.prepare(spec, { headSha: H1, targetBranch: "main" });
  r.forge.pr.description = pub.initialBody(spec.completion());
  const out = await pub.publish(MR);
  return { pub, out };
}

/** The PR's record as the claim carries it, decoded by the real client. */
async function priorState(): Promise<PrDescriptionState> {
  const res = await client.lookupPrDescription(RUN, { claim_generation: 1, mr_iid: MR, region_sha256: "0".repeat(64) });
  api.calls.length = 0;
  return res.pr!;
}

function stages() {
  return api.calls.filter((c) => c.op === "stage").map((c) => c.body);
}

const OLD_REGION = [REGION_START, "An older summary of this PR.", "", SIZE_LINE, "", "Describes `aaaaaaa` against `main`.", REGION_END].join("\n");

// ── Step 1 and the fallback ladder (D8) ──────────────────────────────────────────────────────

describe("publisher: staging and the fallback ladder (D8)", () => {
  it("the fake bind records an optional diagram flag and rejects a changed flag or hash", async () => {
    const r = rig(new FakePass({ ...SUMMARY, diagram: DIAGRAM }));
    await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    const version = [...api.versions.values()][0]!;
    assert.equal(version.region_has_diagram, null);
    assert.deepEqual(version.fields.diagram, { ...DIAGRAM, title: "" });
    const body = { version_id: version.id, mr_iid: MR, rendered_region_sha256: "a".repeat(64), region_has_diagram: true };
    assert.equal(api.handle("bind", RUN, body).status, 200);
    assert.equal(version.region_has_diagram, true);
    assert.equal(api.state(MR)?.diagram_published, false);
    assert.equal(api.handle("bind", RUN, { ...body, region_has_diagram: false }).status, 409);
    assert.equal(api.handle("bind", RUN, { version_id: version.id, mr_iid: MR, rendered_region_sha256: body.rendered_region_sha256 }).status, 409);
    assert.equal(api.handle("bind", RUN, { ...body, rendered_region_sha256: "b".repeat(64) }).status, 409);
  });

  it("binds a diagram only when the final region contains it", async () => {
    const r = rig(new FakePass({ ...SUMMARY, diagram: DIAGRAM }));
    const { pub } = await newPr(r);
    assert.ok(pub.region.includes("```mermaid\n"));
    const bind = api.calls.find((c) => c.op === "bind")!;
    assert.equal(bind.body.region_has_diagram, true);
    assert.equal(bind.body.rendered_region_sha256, regionSha256(pub.region));
    assert.equal(api.state(MR)?.diagram_published, true);
  });

  it("restages prose before bind when the body cap drops a diagram, keeping the size table", async () => {
    const r = rig(new FakePass({ ...SUMMARY, diagram: DIAGRAM }));
    const spec = makeSpec({ facts: async () => ({ baseSha: BASE, size: { line: SIZE_TABLE, size: SIZE } }) });
    const pub = await r.publisher.prepare(spec, { headSha: H1, targetBranch: "main" });
    const less = pub.region.replace(/```mermaid\n[\s\S]*?```\n\n/u, "");
    assert.notEqual(less, pub.region);
    const prefix = `${"p".repeat(BODY_CAP_CHARS - renderBody(less, completion(true)).length - 1)}\n`;
    r.forge.pr.description = prefix + pub.initialBody(completion(true));
    await pub.publish(MR);
    assert.deepEqual(stages().map((s) => s.source), ["generated", "generated"]);
    assert.equal((stages()[1]!.fields as { diagram?: unknown }).diagram, undefined);
    assert.equal(api.calls.find((c) => c.op === "bind")!.body.region_has_diagram, false);
    const parsed = parseOwnedBlocks(r.forge.pr.description);
    assert.ok(parsed.kind === "ok" && parsed.region?.includes(SIZE_TABLE));
    assert.equal(parsed.kind === "ok" && parsed.region?.includes("```mermaid"), false);
    assert.equal(api.state(MR)?.diagram_published, false);
  });

  it("restages when the renderer's 6 KiB cap omits a diagram", async () => {
    const r = rig(new FakePass({ ...SUMMARY, summary: "x".repeat(5850), diagram: DIAGRAM }));
    const { pub } = await newPr(r);
    assert.ok(!pub.region.includes("```mermaid"));
    assert.ok(pub.region.includes("x".repeat(5850)));
    assert.deepEqual(stages().map((s) => s.source), ["generated", "generated"]);
    assert.equal((stages()[1]!.fields as { diagram?: unknown }).diagram, undefined);
    assert.equal(api.calls.find((c) => c.op === "bind")!.body.region_has_diagram, false);
  });

  it("the closing interlock's whole-body rewrite retries without the diagram", async () => {
    const r = rig(new FakePass({ ...SUMMARY, diagram: DIAGRAM }));
    const pub = await r.publisher.prepare(makeSpec({ interlockIssueIid: IID, completionCloses: false }), { headSha: H1, targetBranch: "main" });
    const less = pub.region.replace(/```mermaid\n[\s\S]*?```\n\n/u, "");
    const base = completion(false);
    const longCompletion = `${base}\n${"q".repeat(BODY_CAP_CHARS - renderBody(less, base).length - 1)}`;
    const spec = makeSpec({ interlockIssueIid: IID, completionCloses: false, completion: () => longCompletion });
    const prepared = await r.publisher.prepare(spec, { headSha: H1, targetBranch: "main" });
    r.forge.pr.description = `Closes #7\n\n${prepared.initialBody(longCompletion)}`;
    await prepared.publish(MR);
    assert.ok(!r.forge.pr.description.includes("Closes #7"));
    assert.ok(r.forge.pr.description.includes(SUMMARY.summary));
    assert.ok(!r.forge.pr.description.includes("```mermaid"));
    assert.equal(api.calls.filter((c) => c.op === "bind").at(-1)!.body.region_has_diagram, false);
  });

  it("initialBody tries diagram-less prose before the size-only region", async () => {
    const r = rig(new FakePass({ ...SUMMARY, diagram: DIAGRAM }));
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    const less = pub.region.replace(/```mermaid\n[\s\S]*?```\n\n/u, "");
    const completionText = `${completion(true)}\n${"q".repeat(BODY_CAP_CHARS - renderBody(less, completion(true)).length - 1)}`;
    const body = pub.initialBody(completionText);
    assert.ok(body.includes(SUMMARY.summary));
    assert.ok(!body.includes("```mermaid"));
    assert.ok(body.length <= BODY_CAP_CHARS);
  });

  it("a bound diagram replaced by a diagram-less recompose gets a new bind", async () => {
    const r = rig(new FakePass({ ...SUMMARY, diagram: DIAGRAM }));
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    r.forge.pr.description = pub.initialBody(completion(true));
    const less = pub.region.replace(/```mermaid\n[\s\S]*?```\n\n/u, "");
    const prefix = `${"p".repeat(BODY_CAP_CHARS - renderBody(less, completion(true)).length - 1)}\n`;
    r.forge.beforeRead = (n, pr) => { if (n === 2) pr.description = prefix + pr.description; };
    await pub.publish(MR);
    const binds = api.calls.filter((c) => c.op === "bind");
    assert.deepEqual(binds.map((c) => c.body.region_has_diagram), [true, false]);
    assert.deepEqual(stages().map((s) => s.source), ["generated", "generated"]);
    assert.deepEqual(api.acks(), ["skipped_snapshot_moved", "published"]);
    assert.equal(api.state(MR)?.diagram_published, false);
  });

  it("drops malformed diagrams from a custom delivery pass while retaining prose", async () => {
    const malformed = { ...DIAGRAM, edges: [{ from: "worker", to: "missing" }, { from: "api", to: "worker", label: "ack" }] };
    const r = rig(new FakePass({ ...SUMMARY, diagram: malformed }));
    await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    const stage = stages().at(-1)!;
    assert.equal(stage.source, "generated");
    assert.equal((stage.fields as { diagram?: unknown }).diagram, undefined);
    assert.equal((stage.fields as { summary: string }).summary, SUMMARY.summary);
  });

  it("keeps prose when the API rejects a hostile diagram label from the editor", async () => {
    const hostile = { ...DIAGRAM, edges: [{ ...DIAGRAM.edges[0]!, label: "Fixes #1" }, DIAGRAM.edges[1]!] };
    const r = rig(new FakePass({ ...SUMMARY, diagram: hostile }));
    await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    const stage = stages().at(-1)!;
    assert.equal((stage.fields as { diagram?: unknown }).diagram !== undefined, true);
    const stored = [...api.versions.values()][0]!;
    assert.equal(stored.fields.diagram, undefined);
    assert.equal(stored.fields.summary, SUMMARY.summary);
    assert.deepEqual(stored.fields.changes, SUMMARY.changes);
  });

  it("drops diagrams for docs-only and binary-only zero-code sizes, but keeps them when size is unavailable", async () => {
    for (const [name, size, kept] of [
      ["docs", { ...SIZE, code: ZERO, docs: { added: 3, deleted: 0 } }, false],
      ["binary", { ...SIZE, code: ZERO }, false],
      ["unavailable", { ...SIZE, unavailable: true, code: ZERO }, true],
    ] as const) {
      const r = rig(new FakePass({ ...SUMMARY, diagram: DIAGRAM }));
      await r.publisher.prepare(makeSpec({ facts: async () => ({ baseSha: BASE, size: { line: SIZE_LINE, size } }) }), { headSha: H1, targetBranch: "main" });
      const stage = stages().at(-1)!;
      assert.equal((stage.fields as { diagram?: unknown }).diagram !== undefined, kept, name);
    }
  });

  it("rung 1: the editor pass stages `generated` with the lead's stamped checks, and a new PR is published as created", async () => {
    const r = rig();
    const { pub, out } = await newPr(r, makeSpec({ lead: LEAD }));
    const [stage] = stages();
    assert.equal(stage!.source, "generated");
    assert.deepEqual((stage!.fields as { verification: unknown }).verification, [
      { command: "task gate:web", result: "pass", verified_at_sha: H1 },
    ]);
    assert.equal(stage!.head_sha, H1);
    assert.equal(stage!.base_sha, BASE);
    assert.match(pub.region, /Reviewers can now mark a finding done/);
    assert.match(pub.region, /Reported by the agent at `aaaaaaa`/);
    assert.equal(r.forge.writes.length, 0, "the body it was created with is already current: no write");
    assert.deepEqual(api.acks(), ["published"]);
    const bind = api.calls.find((c) => c.op === "bind")!.body;
    assert.equal(bind.rendered_region_sha256, regionSha256(pub.region), "bound with the exact region hash");
    assert.equal(out.describedSha, H1);
  });

  it("rung 2: a failed pass with lead claims stages `lead_only` (what + why), with the rung-2 note", async () => {
    const r = rig(new FakePass(null));
    const { pub } = await newPr(r, makeSpec({ lead: LEAD }));
    const [stage] = stages();
    assert.equal(stage!.source, "lead_only");
    assert.equal((stage!.fields as { summary: string }).summary, "Adds a Mark done button. Findings piled up.");
    assert.match(pub.region, /Summary written by the agent, not checked against the diff/);
    assert.deepEqual(api.acks(), ["published"]);
  });

  it("rung 3: no pass and no claims stages `deterministic_only`; the region is the size and provenance lines", async () => {
    const r = rig(null);
    const { pub } = await newPr(r);
    assert.equal(stages()[0]!.source, "deterministic_only");
    assert.equal(pub.region, renderRegion({ sizeLine: SIZE_LINE, headSha: H1, targetBranch: "main" }).text);
    assert.ok(isDeterministicRegion(pub.region));
  });

  it("rung 3: a failed stage is never retried; nothing is bound or acked and the region is deterministic", async () => {
    const r = rig();
    api.failNext("stage", 503);
    const { pub, out } = await newPr(r, makeSpec({ lead: LEAD }));
    assert.equal(stages().length, 1, "stage is never retried");
    assert.ok(!api.calls.some((c) => c.op === "bind" || c.op === "ack"));
    assert.ok(isDeterministicRegion(pub.region));
    assert.equal(out.ack, undefined);
  });

  it("lead checks without a verified_at stamp are dropped (the api rejects unstamped entries)", async () => {
    const r = rig();
    await newPr(r, makeSpec({ lead: { ...LEAD, verifiedAtSha: undefined } }));
    assert.deepEqual((stages()[0]!.fields as { verification: unknown[] }).verification, []);
  });

  it("the editor pass and a regeneration share ONE deadline", async () => {
    const pass = new FakePass(SUMMARY);
    const r = rig(pass);
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    r.forge.pr.description = pub.initialBody(completion(true));
    r.forge.pr.head = H2; // read 1 sees another head: the one regeneration
    await pub.publish(MR);
    assert.equal(pass.deadlines, 1, "the deadline is computed once per publication");
    assert.deepEqual(pass.calls, [1_000_000, 1_000_000]);
  });
});

// ── Composition: preservation, human edits and markers (D10, D15) ─────────────────────────────

describe("publisher: preservation and human-edit protection (D10)", () => {
  it("text outside both blocks survives a refresh byte-for-byte", async () => {
    const r = rig();
    api.seedPublished(MR, { sha256: regionSha256(OLD_REGION), headSha: H1 });
    const before = "## Template\r\nFilled by a person.  \n\n";
    const between = "\n\n<details>bot</details>\n\n";
    const after = "\n\n## Summary by a review bot\nKeep\tme\r\n";
    r.forge.pr.description = before + OLD_REGION + between + completion(false) + after;
    const pub = await r.publisher.prepare(makeSpec({ prior: await priorState() }), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.equal(r.forge.writes.length, 1);
    const written = r.forge.writes[0]!;
    assert.equal(written, before + pub.region + between + completion(true) + after);
    assert.deepEqual(api.acks(), ["published"]);
  });

  it("a missing completion block is appended in place on an own-mode publication; region and other text are kept", async () => {
    const r = rig();
    api.seedPublished(MR, { sha256: regionSha256(OLD_REGION), headSha: H1 });
    const before = "## Template\r\nFilled by a person.  \n\n";
    const after = "\n\n<details>bot</details>\n\n## Summary by a review bot\nKeep\tme";
    r.forge.pr.description = before + OLD_REGION + after;
    assert.equal(parseOwnedBlocks(r.forge.pr.description).kind, "ok");
    const pub = await r.publisher.prepare(makeSpec({ prior: await priorState() }), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.equal(r.forge.writes.length, 1);
    const written = r.forge.writes[0]!;
    assert.equal(written, `${before}${pub.region}${after}\n\n${completion(true)}`, "appended in place, not rewritten whole");
    assert.notEqual(written, renderBody(pub.region, completion(true)));
    assert.equal(written.split(COMPLETION_START).length - 1, 1, "exactly one completion block");
    assert.ok(written.startsWith(before), "the human text is kept byte for byte");
    assert.ok(written.includes(after), "the bot text is kept byte for byte");
    const parsed = parseOwnedBlocks(written);
    assert.ok(parsed.kind === "ok");
    assert.equal(parsed.kind === "ok" && parsed.completion, completion(true));
    assert.deepEqual(api.acks(), ["published"]);
  });

  it("an edited region is not overwritten: skipped_human_edit, a run message, and the completion block still written", async () => {
    const r = rig();
    api.seedPublished(MR, { sha256: regionSha256(OLD_REGION), headSha: H1 });
    const edited = OLD_REGION.replace("An older summary", "A maintainer's own summary");
    r.forge.pr.description = `${edited}\n\n${completion(false)}`;
    const pub = await r.publisher.prepare(makeSpec({ prior: await priorState() }), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    const parsed = parseOwnedBlocks(r.forge.pr.description);
    assert.equal(parsed.kind, "ok");
    assert.equal(parsed.kind === "ok" && parsed.region, edited, "the edited region is kept byte for byte");
    assert.equal(parsed.kind === "ok" && parsed.completion, completion(true), "the completion block is rewritten");
    assert.deepEqual(api.acks(), ["skipped_human_edit"]);
    assert.equal(out.ack, "skipped_human_edit");
    assert.ok(r.emitted.some((t) => /edited by a person/.test(t)), "a run status message names the skip");
  });

  it("a region with no published version is overwritten only when it has uzi's deterministic shape", async () => {
    // Bodies created before this publisher: a size-line-only region, no provenance, no version.
    const legacy = [REGION_START, SIZE_LINE, REGION_END].join("\n");
    const r = rig();
    r.forge.pr.description = `${legacy}\n\n${completion(true)}`;
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.equal(r.forge.pr.description, renderBody(pub.region, completion(true)));
    assert.deepEqual(api.acks(), ["published"]);

    const r2 = rig();
    const foreign = [REGION_START, "Someone wrote this.", REGION_END].join("\n");
    r2.forge.pr.description = `${foreign}\n\n${completion(true)}`;
    const pub2 = await r2.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    await pub2.publish(MR);
    assert.match(r2.forge.pr.description, /Someone wrote this\./);
    assert.deepEqual(api.acks().slice(-1), ["skipped_human_edit"]);
  });

  it("a legacy one-line size region with no published version is refreshed to the table (issue #2061)", async () => {
    const legacy = [REGION_START, SIZE_LINE, "", "Describes `aaaaaaa` against `main`.", REGION_END].join("\n");
    const r = rig();
    r.forge.pr.description = `${legacy}\n\n${completion(true)}`;
    const pub = await r.publisher.prepare(
      makeSpec({ facts: async () => ({ baseSha: BASE, size: { line: SIZE_TABLE, size: SIZE } }) }),
      { headSha: H1, targetBranch: "main" },
    );
    await pub.publish(MR);
    assert.ok(pub.region.includes(SIZE_TABLE), "the new region carries the table");
    assert.equal(r.forge.pr.description, renderBody(pub.region, completion(true)));
    assert.deepEqual(api.acks(), ["published"]);
  });

  it("a versioned region whose table was edited is human: left alone (issue #2061)", async () => {
    const table = renderRegion({ sizeLine: SIZE_TABLE, headSha: H1, targetBranch: "main" }).text;
    const r = rig();
    api.seedPublished(MR, { sha256: regionSha256(table), headSha: H1 });
    const edited = table.replace("| Docs | +2 |", "| Docs | +9 |");
    assert.notEqual(edited, table);
    r.forge.pr.description = `${edited}\n\n${completion(false)}`;
    const pub = await r.publisher.prepare(makeSpec({ prior: await priorState() }), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    const parsed = parseOwnedBlocks(r.forge.pr.description);
    assert.equal(parsed.kind === "ok" && parsed.region, edited, "the edited table is kept byte for byte");
    assert.equal(out.ack, "skipped_human_edit");
    assert.deepEqual(api.acks(), ["skipped_human_edit"]);
  });

  it("a region removed from a PR with a published version is not put back: skipped_no_region", async () => {
    const r = rig();
    api.seedPublished(MR, { sha256: regionSha256(OLD_REGION), headSha: H1 });
    r.forge.pr.description = `Only human text.\n\n${completion(false)}`;
    const pub = await r.publisher.prepare(makeSpec({ prior: await priorState() }), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.equal(r.forge.pr.description, `Only human text.\n\n${completion(true)}`);
    assert.deepEqual(api.acks(), ["skipped_no_region"]);
  });

  it("malformed markers: skipped_malformed and the body is left as it is", async () => {
    const r = rig();
    const body = `${REGION_START}\nx\n${REGION_START}\n${REGION_END}\n\n${completion(true)}`;
    r.forge.pr.description = body;
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.equal(r.forge.pr.description, body);
    assert.equal(r.forge.writes.length, 0);
    assert.deepEqual(api.acks(), ["skipped_malformed"]);
  });

  it("a legacy PR without markers is rewritten whole (D10, today's behaviour)", async () => {
    const r = rig();
    r.forge.pr.description = "Old body. Implements the thing.";
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.equal(r.forge.pr.description, renderBody(pub.region, completion(true)));
    assert.deepEqual(api.acks(), ["published"]);
  });

  it("D15: a body over the cap drops the region to the size line and restages deterministic_only", async () => {
    const r = rig();
    const bulk = "x".repeat(BODY_CAP_CHARS - 200);
    r.forge.pr.description = `${bulk}\n\n${[REGION_START, SIZE_LINE, REGION_END].join("\n")}\n\n${completion(true)}`;
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.deepEqual(stages().map((s) => s.source), ["generated", "deterministic_only"]);
    const parsed = parseOwnedBlocks(r.forge.pr.description);
    assert.ok(parsed.kind === "ok" && parsed.region !== undefined && isDeterministicRegion(parsed.region));
    assert.ok(r.forge.pr.description.startsWith(bulk), "preserved text is never truncated");
    // The superseded version was never bound, so it stays pending (never acked); only its
    // replacement is published.
    const [v1] = [...api.versions.values()].filter((v) => v.source === "generated");
    assert.equal(v1!.state, "pending");
    assert.deepEqual(api.acks(), ["published"]);
  });
});

// ── The completion interlock inside the publisher (amended D10) ────────────────────────────────

describe("publisher: a non-closing completion block scans the whole body (amended D10)", () => {
  const nonClosing = (over: Partial<PublicationSpec> = {}) =>
    makeSpec({ interlockIssueIid: IID, completionCloses: false, completion: (st) => completion(false, st), ...over });

  it("a human-typed `Closes #7` outside the blocks forces a whole-body non-closing rewrite", async () => {
    const r = rig();
    api.seedPublished(MR, { sha256: regionSha256(OLD_REGION), headSha: H1 });
    r.forge.pr.description = `Closes #7 please\n\n${OLD_REGION}\n\n${completion(false)}`;
    const pub = await r.publisher.prepare(nonClosing({ prior: await priorState() }), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    assert.equal(r.forge.pr.description, renderBody(pub.region, completion(false)));
    assert.equal(closingDirectiveFor(r.forge.pr.description, IID, "o/r"), false);
    assert.ok(out.interlockRewrite);
    assert.deepEqual(api.acks(), ["published"]);
  });

  it("`Closes #7` typed into the (human-edited) region yields too: the region is replaced by uzi's", async () => {
    const r = rig();
    api.seedPublished(MR, { sha256: regionSha256(OLD_REGION), headSha: H1 });
    const edited = OLD_REGION.replace("An older summary of this PR.", "Closes #7");
    r.forge.pr.description = `${edited}\n\n${completion(false)}`;
    const pub = await r.publisher.prepare(nonClosing({ prior: await priorState() }), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.equal(r.forge.pr.description, renderBody(pub.region, completion(false)));
  });

  it("malformed markers with a closing directive are rewritten whole", async () => {
    const r = rig();
    r.forge.pr.description = `Fixes #7\n${REGION_START}\n${REGION_START}\n\n${completion(false)}`;
    const pub = await r.publisher.prepare(nonClosing(), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.equal(r.forge.pr.description, renderBody(pub.region, completion(false)));
  });

  it("a closing completion (a legacy issue run) keeps human text as it is", async () => {
    const r = rig();
    r.forge.pr.description = `Closes #7 please\n\n${[REGION_START, SIZE_LINE, REGION_END].join("\n")}\n\n${completion(true)}`;
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.ok(r.forge.pr.description.startsWith("Closes #7 please\n\n"));
  });
});

// ── D11 revalidation ─────────────────────────────────────────────────────────────────────────

describe("publisher: D11 revalidation between reads", () => {
  it("a description changed between read 1 and read 2 is recomposed ONCE from the newer read", async () => {
    const r = rig();
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    r.forge.pr.description = `Human notes.\n\n${[REGION_START, SIZE_LINE, REGION_END].join("\n")}\n\n${completion(true)}`;
    r.forge.beforeRead = (n, pr) => {
      if (n === 2) pr.description += "\n\nA late review-bot note.";
    };
    await pub.publish(MR);
    assert.equal(r.forge.writes.length, 1);
    assert.match(r.forge.writes[0]!, /A late review-bot note\.$/, "the write is composed from the newer read");
    assert.equal(stages().length, 1, "the recompose does not restage");
    assert.deepEqual(api.acks(), ["published"]);
  });

  it("a head that moved between reads acks the bound version skipped_snapshot_moved, then restages for the new head", async () => {
    const r = rig();
    const { pub } = await (async () => {
      const p = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
      r.forge.pr.description = p.initialBody(completion(true));
      r.forge.beforeRead = (n, pr) => {
        if (n === 2) pr.head = H2;
      };
      await p.publish(MR);
      return { pub: p };
    })();
    assert.deepEqual(stages().map((s) => s.head_sha), [H1, H2]);
    assert.deepEqual(api.acks(), ["skipped_snapshot_moved", "published"]);
    assert.match(pub.region, /Describes `ccccccc` against `main`\./);
    assert.equal(parseOwnedBlocks(r.forge.pr.description).kind === "ok" && (parseOwnedBlocks(r.forge.pr.description) as { region?: string }).region, pub.region);
  });

  it("read 1 on another target regenerates for the PR's actual target (the size line is recomputed against it)", async () => {
    const r = rig();
    const targets: string[] = [];
    const spec = makeSpec({
      facts: async (s) => {
        targets.push(s.targetBranch);
        return { baseSha: BASE, size: { line: SIZE_LINE, size: SIZE } };
      },
    });
    const pub = await r.publisher.prepare(spec, { headSha: H1, targetBranch: "main" });
    r.forge.pr.description = pub.initialBody(completion(true));
    r.forge.pr.target = "develop";
    await pub.publish(MR);
    assert.deepEqual(targets, ["main", "develop"]);
    assert.match(r.forge.pr.description, /Describes `aaaaaaa` against `develop`\./);
    assert.deepEqual(api.acks(), ["published"], "the version for `main` was never bound, so it is never acked");
  });

  it("a second head move after the regeneration is spent skips the region, still writing the completion block", async () => {
    const r = rig();
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    r.forge.pr.description = `${OLD_REGION}\n\n${completion(false)}`;
    api.seedPublished(MR, { sha256: regionSha256(OLD_REGION), headSha: H1 });
    r.forge.beforeRead = (n, pr) => {
      if (n === 2) pr.head = H2;
      if (n === 3) pr.head = H3;
    };
    await pub.publish(MR);
    assert.deepEqual(api.acks(), ["skipped_snapshot_moved", "skipped_snapshot_moved"]);
    const parsed = parseOwnedBlocks(r.forge.pr.description);
    assert.ok(parsed.kind === "ok");
    assert.equal(parsed.kind === "ok" && parsed.region, OLD_REGION, "the region is left");
    // The completion block is written from the LATEST read (head H3) with the staleness line.
    assert.equal(parsed.kind === "ok" && parsed.completion, completion(true, { describedSha: H1, headSha: H3 }));
  });

  it("with the regeneration spent, the completion block is still interlock-checked", async () => {
    const r = rig();
    const spec = makeSpec({ interlockIssueIid: IID, completionCloses: false, completion: (st) => completion(false, st) });
    const pub = await r.publisher.prepare(spec, { headSha: H1, targetBranch: "main" });
    r.forge.pr.description = `Resolves #7\n\n${[REGION_START, SIZE_LINE, REGION_END].join("\n")}\n\n${completion(false)}`;
    r.forge.beforeRead = (n, pr) => {
      if (n === 2) pr.head = H2;
      if (n === 3) pr.head = H3;
    };
    await pub.publish(MR);
    assert.equal(closingDirectiveFor(r.forge.pr.description, IID, "o/r"), false);
    assert.ok(r.forge.pr.description.startsWith(REGION_START), "rewritten whole");
  });

  it("never writes a body composed from a read older than the latest read", async () => {
    const r = rig();
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    r.forge.pr.description = `${[REGION_START, SIZE_LINE, REGION_END].join("\n")}\n\n${completion(false)}`;
    const seen: string[] = [];
    r.forge.beforeRead = (n, pr) => {
      if (n === 2) pr.description = `Edited between reads.\n\n${pr.description}`;
      seen.push(pr.description);
    };
    await pub.publish(MR);
    const latest = seen[seen.length - 1]!;
    const written = r.forge.writes[0]!;
    const w = parseOwnedBlocks(written);
    const l = parseOwnedBlocks(latest);
    assert.ok(w.kind === "ok" && l.kind === "ok");
    assert.equal(w.kind === "ok" && w.before, l.kind === "ok" && l.before, "the outside text is the latest read's");
  });
});

describe("publisher: D11 budgets once spent", () => {
  it("the recompose is spent on the first change; a change seen after a restage skips the region (skipped_human_edit)", async () => {
    // The first description change grows the body over the D15 cap, so the recompose changes the
    // region: the bound version is acked skipped_snapshot_moved (cap supersession) and a
    // deterministic_only version is bound and revalidated by read 3, which sees a second change.
    const r = rig();
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    r.forge.pr.description = pub.initialBody(completion(true));
    r.forge.beforeRead = (n, pr) => {
      if (n === 2) pr.description = `${"y".repeat(BODY_CAP_CHARS)}\n\n${pr.description}`;
      if (n === 3) pr.description = `Second edit.\n\n${pr.description}`;
    };
    await pub.publish(MR);
    assert.deepEqual(api.acks(), ["skipped_snapshot_moved", "skipped_human_edit"]);
    assert.deepEqual(stages().map((s) => s.source), ["generated", "deterministic_only"]);
    assert.ok(r.forge.pr.description.startsWith("Second edit.\n\n"), "the completion block is written from the latest read");
    assert.ok(r.emitted.some((t) => /edited by a person/.test(t)));
  });

  it("D15 cap supersession of a BOUND version acks skipped_snapshot_moved, then publishes the deterministic version", async () => {
    const r = rig();
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    r.forge.pr.description = pub.initialBody(completion(true));
    r.forge.beforeRead = (n, pr) => {
      if (n === 2) pr.description = `${"y".repeat(BODY_CAP_CHARS)}\n\n${pr.description}`;
    };
    await pub.publish(MR);
    assert.deepEqual(api.acks(), ["skipped_snapshot_moved", "published"]);
    const parsed = parseOwnedBlocks(r.forge.pr.description);
    assert.ok(parsed.kind === "ok" && parsed.region !== undefined && isDeterministicRegion(parsed.region));
  });
});

// ── Step 9: acks ───────────────────────────────────────────────────────────────────────────────

describe("publisher: acknowledgement (step 9)", () => {
  it("a failed forge write acks write_failed", async () => {
    const r = rig();
    r.forge.pr.description = `Legacy body`;
    r.forge.failWrite = true;
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    assert.deepEqual(api.acks(), ["write_failed"]);
    assert.equal(out.wrote, false);
  });

  it("published only for a version confirmed on the forge: a post-write mismatch is recorded (write_failed), not retried", async () => {
    const r = rig();
    r.forge.pr.description = `Legacy body`;
    r.forge.afterWrite = (pr) => {
      pr.description = pr.description.replace("Reviewers can now", "Someone changed");
    };
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.deepEqual(api.acks(), ["write_failed"]);
    assert.equal(r.forge.writes.length, 1, "not retried");
  });

  it("an unconfirmable write leaves the version bound and pending (lost-ack recovery), never acked published", async () => {
    const r = rig();
    r.forge.pr.description = `Legacy body`;
    r.forge.failRead = (n) => n === 3;
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.deepEqual(api.acks(), []);
    const v = [...api.versions.values()][0]!;
    assert.equal(v.state, "pending");
    assert.equal(v.rendered_region_sha256, regionSha256(pub.region));
  });

  it("a lost lock compare-and-swap refreshes the lock version and replays the ack", async () => {
    const r = rig();
    api.failNext("ack", 409, "lock_conflict");
    await newPr(r);
    assert.deepEqual(api.acks(), ["published", "published"]);
    assert.equal([...api.versions.values()][0]!.state, "published");
  });

  it("a read-1 failure is advisory: nothing is written or acked", async () => {
    const r = rig();
    r.forge.failRead = () => true;
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    assert.equal(out.wrote, false);
    assert.deepEqual(api.acks(), []);
    assert.equal(out.region, pub.region);
  });
});

// ── D17 refresh runs and the staleness line (D12) ─────────────────────────────────────────────

describe("publisher: refresh runs (D17) and the staleness line (D12)", () => {
  const refresh = (over: Partial<PublicationSpec> = {}) =>
    makeSpec({
      mode: "refresh",
      completion: () => renderCompletionBlock({ branch: "agent/issue-7", closes: false, kindLine: "Automated MR rework." }),
      ...over,
    });

  it("a legacy PR without uzi markers is left untouched: no write, no bind, no ack", async () => {
    const r = rig();
    r.forge.pr.description = "Closes #7\n\nA human wrote all of this.";
    const pub = await r.publisher.prepare(refresh(), { headSha: H2, targetBranch: "main" });
    r.forge.pr.head = H2;
    await pub.publish(MR);
    assert.equal(r.forge.writes.length, 0);
    assert.ok(!api.calls.some((c) => c.op === "bind" || c.op === "ack"));
  });

  it("a marked PR gets a new region; its completion block keeps Closes and loses a now-current staleness line", async () => {
    const r = rig();
    api.seedPublished(MR, { sha256: regionSha256(OLD_REGION), headSha: H1 });
    const closing = completion(true, { describedSha: H1, headSha: H2 });
    assert.ok(closing.includes(STALENESS_START));
    r.forge.pr.head = H2;
    r.forge.pr.description = `${OLD_REGION}\n\n${closing}`;
    const pub = await r.publisher.prepare(refresh({ prior: await priorState() }), { headSha: H2, targetBranch: "main" });
    await pub.publish(MR);
    const parsed = parseOwnedBlocks(r.forge.pr.description);
    assert.ok(parsed.kind === "ok");
    assert.equal(parsed.kind === "ok" && parsed.region, pub.region);
    assert.equal(parsed.kind === "ok" && parsed.completion, completion(true), "only the staleness line changed: Closes is kept");
    assert.deepEqual(api.acks(), ["published"]);
  });

  it("a marked PR with no completion block stays without one: the region is refreshed, other text kept", async () => {
    const r = rig();
    api.seedPublished(MR, { sha256: regionSha256(OLD_REGION), headSha: H1 });
    const before = "## Template\r\nFilled by a person.  \n\n";
    const after = "\n\n<details>bot</details>\n\n## Summary by a review bot\nKeep\tme";
    r.forge.pr.head = H2;
    r.forge.pr.description = before + OLD_REGION + after;
    const pub = await r.publisher.prepare(refresh({ prior: await priorState() }), { headSha: H2, targetBranch: "main" });
    await pub.publish(MR);
    assert.equal(r.forge.writes.length, 1);
    const written = r.forge.writes[0]!;
    assert.equal(written, before + pub.region + after);
    assert.ok(!written.includes(COMPLETION_START), "no completion block is added on a refresh");
    assert.deepEqual(api.acks(), ["published"]);
  });

  it("a refresh that skips an edited region adds the staleness line and changes nothing else in the completion block", async () => {
    const r = rig();
    api.seedPublished(MR, { sha256: regionSha256(OLD_REGION), headSha: H1 });
    const edited = OLD_REGION.replace("older", "hand-written");
    r.forge.pr.head = H2;
    r.forge.pr.description = `${edited}\n\n${completion(true)}`;
    const pub = await r.publisher.prepare(refresh({ prior: await priorState() }), { headSha: H2, targetBranch: "main" });
    await pub.publish(MR);
    assert.equal(r.forge.pr.description, `${edited}\n\n${completion(true, { describedSha: H1, headSha: H2 })}`);
    assert.match(r.forge.pr.description, /This description may be outdated: it describes `aaaaaaa`; the PR head is `ccccccc`\./);
    assert.deepEqual(api.acks(), ["skipped_human_edit"]);
  });
});

// ── The interlock's reconcile ─────────────────────────────────────────────────────────────────

describe("reconcileCompletion (the interlock's read-replace-write)", () => {
  const region = renderRegion({ sizeLine: SIZE_LINE, headSha: H1, targetBranch: "main" }).text;
  const args = (forge: FakeForge, closes: boolean) => ({
    forge,
    repoUrl: "https://gitlab.example.test/o/r",
    pat: "pat",
    mrIid: MR,
    completion: () => completion(closes),
    ownRegion: region,
    nonClosing: closes ? undefined : { issueIid: IID, repoPath: "o/r" },
    log: nullLogger(),
  });

  it("replaces only the completion block and confirms from the re-read", async () => {
    const f = new FakeForge();
    f.pr.description = `Human text.\n\n${region}\n\n${completion(false)}\n\nBot text.`;
    assert.equal(await reconcileCompletion(args(f, true)), "confirmed");
    assert.equal(f.pr.description, `Human text.\n\n${region}\n\n${completion(true)}\n\nBot text.`);
  });

  it("malformed or missing completion markers get a whole-body rewrite", async () => {
    for (const body of ["No markers at all.", `${region}\n\n${region}`, `${region}\n\nhuman only`]) {
      const f = new FakeForge();
      f.pr.description = body;
      assert.equal(await reconcileCompletion(args(f, true)), "confirmed", body);
      assert.equal(f.pr.description, renderBody(region, completion(true)));
    }
  });

  it("never reports confirmed when the re-read does not carry the rendered block", async () => {
    const f = new FakeForge();
    f.pr.description = `${region}\n\n${completion(false)}`;
    f.afterWrite = (pr) => {
      pr.description = pr.description.replace("Closes #7", "Related.");
    };
    assert.equal(await reconcileCompletion(args(f, true)), "unconfirmed");
  });

  it("non-closing: a human `Closes #7` forces a whole-body non-closing rewrite, re-read and re-scanned", async () => {
    const f = new FakeForge();
    f.pr.description = `Closes #7\n\n${region}\n\n${completion(true)}`;
    assert.equal(await reconcileCompletion(args(f, false)), "confirmed");
    assert.equal(f.pr.description, renderBody(region, completion(false)));
  });

  it("non-closing: a rewrite that cannot be written is `closing` (the caller fails closed)", async () => {
    const f = new FakeForge();
    f.pr.description = `Closes #7\n\n${region}\n\n${completion(true)}`;
    f.failWrite = true;
    assert.equal(await reconcileCompletion(args(f, false)), "closing");
  });

  it("non-closing: a directive that survives the whole-body rewrite is `closing`", async () => {
    const f = new FakeForge();
    f.pr.description = `Closes #7\n\n${region}\n\n${completion(true)}`;
    f.afterWrite = (pr) => {
      pr.description = `Fixes #7\n\n${pr.description}`;
    };
    assert.equal(await reconcileCompletion(args(f, false)), "closing");
  });

  it("non-closing: a failed read falls back to the blind whole-body rewrite, reported as `blind` (L3)", async () => {
    const f = new FakeForge();
    f.pr.description = `Closes #7`;
    f.failRead = () => true;
    assert.equal(await reconcileCompletion(args(f, false)), "blind", "a write nothing re-read is never `confirmed`");
    assert.equal(f.pr.description, renderBody(region, completion(false)));
  });

  it("closing: a failed read is unconfirmed (the caller holds) and nothing is written", async () => {
    const f = new FakeForge();
    f.failRead = () => true;
    assert.equal(await reconcileCompletion(args(f, true)), "unconfirmed");
    assert.equal(f.writes.length, 0);
  });
});

// ── The reconcile confirms what it wrote (ADR 1225, PRD #1798 M6) ─────────────────────────────

describe("reconcileCompletion confirms against the block it WROTE, not one re-rendered for the re-read head", () => {
  const region = renderRegion({ sizeLine: SIZE_LINE, headSha: H1, targetBranch: "main" }).text;
  // The runner's completion: the D12 staleness line for the head the read reports.
  const staleArgs = (forge: FakeForge, closes: boolean) => ({
    forge,
    repoUrl: "https://gitlab.example.test/o/r",
    pat: "pat",
    mrIid: MR,
    completion: (head?: string) => completion(closes, head ? { describedSha: H1, headSha: head } : undefined),
    ownRegion: region,
    nonClosing: closes ? undefined : { issueIid: IID, repoPath: "o/r" },
    log: nullLogger(),
  });

  it("closing: a head that moves between the write and the re-read is still `confirmed` (the caller's head re-verify then decides)", async () => {
    const f = new FakeForge();
    f.pr.description = `${region}\n\n${completion(false)}`;
    f.afterWrite = (pr) => {
      pr.head = H2;
    };
    assert.equal(await reconcileCompletion(staleArgs(f, true)), "confirmed");
    assert.equal(f.writes.length, 1);
    assert.equal(parseOwnedBlocks(f.pr.description).kind, "ok");
    assert.ok(f.pr.description.endsWith(completion(true)), "the block written for the head the read reported");
  });

  it("hold path: a head move between the write and the re-read causes no needless whole-body rewrite", async () => {
    const f = new FakeForge();
    f.pr.description = `Human notes.\n\n${region}\n\n${completion(true)}\n\nBot text.`;
    f.afterWrite = (pr) => {
      pr.head = H2;
    };
    assert.equal(await reconcileCompletion(staleArgs(f, false)), "confirmed");
    assert.equal(f.writes.length, 1, "one write: the completion block alone");
    assert.equal(f.pr.description, `Human notes.\n\n${region}\n\n${completion(false)}\n\nBot text.`);
  });

  it("closing: a re-read that fails after the write is `unconfirmed` (the runner strips Closes, then holds or fails closed)", async () => {
    const f = new FakeForge();
    f.pr.description = `${region}\n\n${completion(false)}`;
    f.failRead = (n) => n === 2;
    assert.equal(await reconcileCompletion(staleArgs(f, true)), "unconfirmed");
  });

  it("an over-cap detail read (ForgeResponseTooLarge) takes the unreadable path: blind when non-closing, unconfirmed when closing", async () => {
    const tooLarge = new FakeForge();
    tooLarge.getMergeRequest = async () => {
      throw new ForgeResponseTooLarge(MR_DETAIL_MAX_BYTES);
    };
    assert.equal(await reconcileCompletion(staleArgs(tooLarge, false)), "blind");
    assert.equal(tooLarge.pr.description, renderBody(region, completion(false)));
    const closing = new FakeForge();
    closing.getMergeRequest = tooLarge.getMergeRequest;
    assert.equal(await reconcileCompletion(staleArgs(closing, true)), "unconfirmed");
    assert.equal(closing.writes.length, 0);
  });
});

// ── closingRemains (amended D10, M1) ──────────────────────────────────────────────────────────

describe("publisher: closingRemains on a non-closing issue publication", () => {
  const nonClosing = (over: Partial<PublicationSpec> = {}) =>
    makeSpec({ interlockIssueIid: IID, completionCloses: false, completion: (st) => completion(false, st), ...over });

  it("an adopted closing body whose rewrite fails: closingRemains", async () => {
    const r = rig();
    r.forge.pr.description = `${[REGION_START, SIZE_LINE, REGION_END].join("\n")}\n\n${completion(true)}`;
    r.forge.failWrite = true;
    const pub = await r.publisher.prepare(nonClosing(), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    assert.equal(out.wrote, false);
    assert.equal(out.closingRemains, true);
  });

  it("a human `Closes #7` whose rewrite is written but cannot be re-read: closingRemains (unconfirmed)", async () => {
    const r = rig();
    r.forge.pr.description = `Closes #7\n\n${[REGION_START, SIZE_LINE, REGION_END].join("\n")}\n\n${completion(false)}`;
    r.forge.failRead = (n) => n === 3;
    const pub = await r.publisher.prepare(nonClosing(), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    assert.equal(out.wrote, true);
    assert.equal(out.closingRemains, true);
  });

  it("a confirmed rewrite clears it; a closing publication never reports it", async () => {
    const r = rig();
    r.forge.pr.description = `Closes #7\n\n${completion(true)}`;
    const pub = await r.publisher.prepare(nonClosing(), { headSha: H1, targetBranch: "main" });
    assert.equal((await pub.publish(MR)).closingRemains, false);
    const c = rig();
    c.forge.pr.description = `Closes #7\n\n${completion(true)}`;
    c.forge.failWrite = true;
    const closing = await c.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    assert.equal((await closing.publish(MR)).closingRemains, false);
  });

  it("an over-cap read (ForgeResponseTooLarge) is an unreadable PR: the blind non-closing rewrite runs", async () => {
    const r = rig();
    r.forge.pr.description = `Closes #7\n\n${completion(true)}`;
    r.forge.getMergeRequest = async () => {
      throw new ForgeResponseTooLarge(MR_DETAIL_MAX_BYTES);
    };
    const pub = await r.publisher.prepare(nonClosing(), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    assert.equal(out.wrote, true);
    assert.deepEqual(r.forge.writes, [pub.initialBody(completion(false))], "uzi's region and the non-closing block, nothing preserved");
    assert.equal(closingDirectiveFor(r.forge.pr.description, IID, "o/r"), false);
    assert.equal(out.closingRemains, false);
    assert.deepEqual(api.acks(), [], "a blind rewrite binds and acks nothing");
  });

  it("blindFallback off (an interlocked run): an unread PR is never rewritten blind, and closingRemains fails closed", async () => {
    const r = rig();
    r.forge.pr.description = `Notes from the maintainer.\n\n${completion(false)}`;
    r.forge.failRead = (n) => n === 1;
    const pub = await r.publisher.prepare(nonClosing({ blindFallback: false }), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    assert.equal(out.wrote, false);
    assert.deepEqual(r.forge.writes, [], "no blind write: the interlock's reconcile owns the body");
    assert.equal(out.closingRemains, true);
  });

  it("no read ever succeeds and the blind rewrite is refused: closingRemains (nothing observed is not proof)", async () => {
    const r = rig();
    r.forge.pr.description = `Closes #7\n\n${completion(true)}`;
    r.forge.failRead = () => true;
    r.forge.failWrite = true;
    const pub = await r.publisher.prepare(nonClosing(), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    assert.equal(out.wrote, false);
    assert.equal(out.closingRemains, true);
  });

  it("no read ever succeeds on a CLOSING publication: nothing is written and nothing is reported", async () => {
    const r = rig();
    r.forge.failRead = () => true;
    const pub = await r.publisher.prepare(makeSpec({ interlockIssueIid: IID }), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    assert.equal(r.forge.writes.length, 0);
    assert.equal(out.closingRemains, false);
  });

  it("the blind rewrite runs under the caller's signal, not the spent budget", async () => {
    const r = rig();
    let writeSignal: AbortSignal | undefined;
    r.forge.getMergeRequest = (_u?: string, _p?: string, _i?: number, signal?: AbortSignal) =>
      new Promise<MergeRequestDetail>((_resolve, reject) => {
        signal?.addEventListener("abort", () => reject(new ForgeError(0, "aborted")), { once: true });
      });
    const write = r.forge.updateMergeRequestDescription.bind(r.forge);
    r.forge.updateMergeRequestDescription = async (u, p, i, d, signal?: AbortSignal) => {
      writeSignal = signal;
      return write(u, p, i, d);
    };
    const publisher = new PrDescriptionPublisher({ forge: r.forge, api: client, pass: null, log: nullLogger(), emit: () => {}, forgeBudgetMs: 30 });
    const caller = new AbortController();
    const pub = await publisher.prepare(nonClosing({ signal: caller.signal }), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    assert.equal(out.wrote, true, "the budget ended the hung read, and the blind rewrite still ran");
    assert.equal(writeSignal, caller.signal);
    assert.equal(out.closingRemains, false);
  });
});

describe("publisher: a stale_claim at staging judges closingRemains like a stop at bind or lookup", () => {
  const nonClosing = (over: Partial<PublicationSpec> = {}) =>
    makeSpec({ interlockIssueIid: IID, completionCloses: false, completion: (st) => completion(false, st), ...over });

  for (const [op, closing] of [
    ["stage", true],
    ["stage", false],
    ["bind", true],
    ["bind", false],
  ] as const) {
    it(`${op} answers 409 stale_claim over a ${closing ? "closing" : "non-closing"} body: closingRemains is ${closing}, nothing is written`, async () => {
      const r = rig();
      r.forge.pr.description = `${closing ? "Closes #7" : "Notes"}\n\n${[REGION_START, SIZE_LINE, REGION_END].join("\n")}\n\n${completion(false)}`;
      api.failNext(op, 409, "stale_claim");
      const pub = await r.publisher.prepare(nonClosing(), { headSha: H1, targetBranch: "main" });
      const out = await pub.publish(MR);
      assert.equal(r.forge.writes.length, 0, "a stopped publication writes nothing");
      assert.ok(r.forge.reads >= 1, "the PR was read");
      assert.equal(out.closingRemains, closing);
    });
  }

  it("a stale_claim at staging over an unreadable PR: no write, and closingRemains fails closed", async () => {
    const r = rig();
    r.forge.failRead = () => true;
    api.failNext("stage", 409, "stale_claim");
    const pub = await r.publisher.prepare(nonClosing(), { headSha: H1, targetBranch: "main" });
    const out = await pub.publish(MR);
    assert.equal(r.forge.writes.length, 0);
    assert.equal(out.closingRemains, true);
  });
});

// ── A stale claim stops the publication (Low L1) ──────────────────────────────────────────────

describe("publisher: a stale_claim / run_terminal 409 stops every further region write", () => {
  for (const [op, reason] of [
    ["stage", "stale_claim"],
    ["stage", "run_terminal"],
    ["bind", "stale_claim"],
    ["bind", "run_terminal"],
    ["lookup", "run_terminal"],
  ] as const) {
    it(`${op} answers 409 ${reason}: nothing is written and no further api call is made`, async () => {
      const r = rig();
      // A body the publication would otherwise rewrite (a legacy body is rewritten whole; deciding
      // that reaches the lookup route).
      r.forge.pr.description = "Legacy body";
      api.failNext(op, 409, reason);
      const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
      const before = api.calls.length;
      const out = await pub.publish(MR);
      assert.equal(r.forge.writes.length, 0, "no forge write");
      assert.equal(out.wrote, false);
      const after = api.calls.slice(before).map((c) => c.op);
      const stoppedAt = api.calls.findIndex((c) => c.op === op);
      assert.equal(api.calls.length - 1, stoppedAt, `no api call after the 409 (${after.join(",")})`);
      assert.deepEqual(api.acks(), []);
    });
  }

  it("an ack answered 409 stale_claim stops too: no restage, no later ack, no write", async () => {
    const pass = new FakePass(SUMMARY);
    const r = rig(pass);
    r.forge.pr.description = "Legacy body";
    // The head moves between read 1 and read 2: the bound version is acked skipped_snapshot_moved,
    // which the api refuses as stale; without the stop, the publication would restage and write.
    r.forge.beforeRead = (n, pr) => {
      if (n === 2) pr.head = H2;
    };
    api.failNext("ack", 409, "stale_claim");
    const pub = await r.publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    await pub.publish(MR);
    assert.deepEqual(api.acks(), ["skipped_snapshot_moved"]);
    assert.equal(stages().length, 1, "no restage after the stop");
    assert.equal(pass.calls.length, 1, "no editor pass after the stop");
    assert.equal(r.forge.writes.length, 0);
  });
});

// ── One time budget (Timing) ──────────────────────────────────────────────────────────────────

describe("publisher: one overall time budget", () => {
  it("the head-lag wait honours the abort signal", async () => {
    const forge = new FakeForge();
    forge.pr.head = H2; // read 1 lags the landed head, so the publisher waits before re-reading
    const publisher = new PrDescriptionPublisher({ forge, api: client, pass: null, log: nullLogger(), emit: () => {}, headLagRetryMs: 60_000 });
    const abort = new AbortController();
    const pub = await publisher.prepare(makeSpec({ signal: abort.signal }), { headSha: H1, targetBranch: "main" });
    const started = Date.now();
    setTimeout(() => abort.abort(), 20);
    const out = await pub.publish(MR);
    assert.ok(Date.now() - started < 5_000, "the 60 s wait ended with the signal");
    assert.equal(out.wrote, false);
    assert.equal(forge.reads, 1, "no re-read after the abort");
  });

  it("the forge share of the budget bounds a publication whose forge never answers", async () => {
    const forge = new FakeForge();
    forge.getMergeRequest = (_u?: string, _p?: string, _i?: number, signal?: AbortSignal) =>
      new Promise<MergeRequestDetail>((_resolve, reject) => {
        signal?.addEventListener("abort", () => reject(new ForgeError(0, "aborted")), { once: true });
      });
    const publisher = new PrDescriptionPublisher({ forge, api: client, pass: null, log: nullLogger(), emit: () => {}, forgeBudgetMs: 50 });
    const pub = await publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    const started = Date.now();
    const out = await pub.publish(MR);
    assert.ok(Date.now() - started < 5_000, "the budget ended the hung read");
    assert.equal(out.wrote, false);
  });

  it("the forge budget starts at publish(), not prepare(): a create slower than the budget still leaves publish its reads", async () => {
    const forge = new FakeForge();
    const publisher = new PrDescriptionPublisher({ forge, api: client, pass: null, log: nullLogger(), emit: () => {}, sleep: async () => {}, headLagRetryMs: 0, forgeBudgetMs: 100 });
    const pub = await publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    // The create (between prepare and publish) takes longer than the whole forge budget.
    await new Promise((r) => setTimeout(r, 250));
    forge.pr.description = pub.initialBody(completion(true));
    const out = await pub.publish(MR);
    assert.ok(forge.reads >= 2, `publish read the PR (read 1 and read 2): ${forge.reads}`);
    assert.equal(out.ack, "published", "the publication ran to its ack under its own budget");
  });

  it("the budget covers the editor pass's deadline plus the forge share", async () => {
    let seen: AbortSignal | undefined;
    const forge = new FakeForge();
    const inner = forge.getMergeRequest.bind(forge);
    forge.getMergeRequest = (_u?: string, _p?: string, _i?: number, signal?: AbortSignal) => {
      seen = signal;
      return inner();
    };
    const pass = new FakePass(SUMMARY);
    pass.deliverySummaryDeadline = () => Date.now() + 200;
    const publisher = new PrDescriptionPublisher({ forge, api: client, pass, log: nullLogger(), emit: () => {}, sleep: async () => {}, headLagRetryMs: 0, forgeBudgetMs: 100 });
    const pub = await publisher.prepare(makeSpec(), { headSha: H1, targetBranch: "main" });
    forge.pr.description = pub.initialBody(completion(true));
    await pub.publish(MR);
    assert.ok(seen && !seen.aborted, "the forge calls ride the budgeted signal");
    await new Promise((r) => setTimeout(r, 450));
    assert.ok(seen.aborted, "the budget (pass 200 ms + forge 100 ms) has expired");
  });
});

// ── Helpers and invariants ────────────────────────────────────────────────────────────────────

describe("publisher helpers", () => {
  it("withStalenessLine inserts, replaces and removes only the staleness pair", () => {
    const plain = completion(true);
    const stale = completion(true, { describedSha: H1, headSha: H2 });
    const pair = stale.slice(stale.indexOf(STALENESS_START), stale.indexOf("<!-- /uzi:staleness -->") + "<!-- /uzi:staleness -->".length);
    assert.equal(withStalenessLine(plain, pair), stale);
    assert.equal(withStalenessLine(stale, ""), plain);
    const other = completion(true, { describedSha: H3, headSha: H2 });
    const otherPair = other.slice(other.indexOf(STALENESS_START), other.indexOf("<!-- /uzi:staleness -->") + "<!-- /uzi:staleness -->".length);
    assert.equal(withStalenessLine(stale, otherPair), other);
  });

  it("withStalenessLine leaves a block untouched when its staleness pair is not uzi's exact line", () => {
    const stale = completion(true, { describedSha: H1, headSha: H2 });
    const start = stale.indexOf(STALENESS_START) + STALENESS_START.length;
    const end = stale.indexOf("<!-- /uzi:staleness -->");
    // A person or review bot put a closing directive inside the pair: neither a removal nor a
    // replacement may delete it, since that would change what the PR closes on a D17 refresh.
    const forged = `${stale.slice(0, start)}\nCloses #7\n${stale.slice(end)}`;
    const other = completion(true, { describedSha: H3, headSha: H2 });
    const otherPair = other.slice(other.indexOf(STALENESS_START), other.indexOf("<!-- /uzi:staleness -->") + "<!-- /uzi:staleness -->".length);
    assert.equal(withStalenessLine(forged, ""), forged);
    assert.equal(withStalenessLine(forged, otherPair), forged);
    // The genuine line with text appended inside the pair is an edit too.
    const edited = `${stale.slice(0, end)}Closes #7\n${stale.slice(end)}`;
    assert.equal(withStalenessLine(edited, ""), edited);
    // A duplicated or lone marker is malformed: untouched, and nothing is inserted beside it.
    const dup = `${stale}\n${STALENESS_START}`;
    assert.equal(withStalenessLine(dup, ""), dup);
    const lone = completion(true).replace(`\n---\n`, `\n${STALENESS_START}\n---\n`);
    assert.equal(withStalenessLine(lone, otherPair), lone);
  });

  it("isDeterministicRegion accepts only the size and provenance shapes", () => {
    assert.ok(isDeterministicRegion([REGION_START, SIZE_LINE, REGION_END].join("\n")));
    assert.ok(isDeterministicRegion(renderRegion({ sizeLine: SIZE_LINE, headSha: H1, targetBranch: "main" }).text));
    assert.ok(!isDeterministicRegion(OLD_REGION));
    assert.ok(!isDeterministicRegion([REGION_START, SIZE_LINE, "Closes #7", REGION_END].join("\n")));
  });

  it("isDeterministicRegion: the size table, with and without provenance; legacy shapes still accepted (issue #2061)", () => {
    const prov = "Describes `aaaaaaa` against `main`.";
    const wrap = (...inner: string[]) => [REGION_START, ...inner, REGION_END].join("\n");
    assert.ok(isDeterministicRegion(wrap(SIZE_TABLE)));
    assert.ok(isDeterministicRegion(wrap(SIZE_TABLE, "", prov)));
    assert.ok(isDeterministicRegion(renderRegion({ sizeLine: SIZE_TABLE, headSha: H1, targetBranch: "main" }).text));
    assert.ok(isDeterministicRegion(wrap(SIZE_LINE)));
    assert.ok(isDeterministicRegion(wrap(SIZE_LINE, "", prov)));
    assert.ok(isDeterministicRegion(wrap(prov)));
    assert.ok(isDeterministicRegion(wrap()));
  });

  it("isDeterministicRegion rejects a table that is not exactly canonical (issue #2061)", () => {
    const prov = "Describes `aaaaaaa` against `main`.";
    const wrap = (...inner: string[]) => [REGION_START, ...inner, REGION_END].join("\n");
    const bad: [string, string][] = [
      ["extra row", SIZE_TABLE.replace("| **Total**", "| Config | +1 | −0 |\n| **Total**")],
      ["extra trailing text", `${SIZE_TABLE}\nSomething else`],
      ["altered header", SIZE_TABLE.replace("| Category |", "| Kind |")],
      ["altered alignment row", SIZE_TABLE.replace("|:---------|------:|--------:|", "|:---------|:-----:|--------:|")],
      ["wrong total", SIZE_TABLE.replace("**+1,205**", "**+1,206**")],
      ["reordered buckets", SIZE_TABLE.replace("| Code | +1,203 | −1 |\n| Docs | +2 | −0 |", "| Docs | +2 | −0 |\n| Code | +1,203 | −1 |")],
      ["duplicate bucket", SIZE_TABLE.replace("| Docs | +2 | −0 |", "| Code | +2 | −0 |")],
      ["ASCII hyphen", SIZE_TABLE.replaceAll("−", "-")],
      ["missing comma", SIZE_TABLE.replace("+1,203", "+1203")],
      ["leading zero", SIZE_TABLE.replace("+2 |", "+02 |")],
      // Both round-trip through formatSizeTable, so only their own guards reject them.
      ["no category rows", ["**Size:** 0 files", "", "| Category | Added | Deleted |", "|:---------|------:|--------:|", "| **Total** | **+0** | **−0** |"].join("\n")],
      ["fewer files than rows", SIZE_TABLE.replace("**Size:** 3 files", "**Size:** 1 file")],
    ];
    for (const [name, table] of bad) {
      assert.ok(table !== SIZE_TABLE, name);
      assert.ok(!isDeterministicRegion(wrap(table)), name);
      assert.ok(!isDeterministicRegion(wrap(table, "", prov)), name);
    }
    assert.ok(!isDeterministicRegion(wrap(SIZE_TABLE, "", "not provenance")));
    assert.ok(!isDeterministicRegion(wrap(SIZE_TABLE, prov)));
  });

  it("repoPathFromUrl", () => {
    assert.equal(repoPathFromUrl("https://gitlab.example.test/group/sub/proj.git"), "group/sub/proj");
    assert.equal(repoPathFromUrl("https://github.com/o/r/"), "o/r");
    assert.equal(repoPathFromUrl("not a url"), undefined);
  });

  it("no rendered region contains a closing directive (hostile fields, targets and sizes)", async () => {
    const hostile = ["Closes #7", "Fixes o/r#7", "resolves https://gitlab.example.test/o/r/-/issues/7", "Implements #7", "fix&#101;s #7"];
    for (const text of hostile) {
      for (const target of ["main", "Closes #7", "fix/#7", "fixes-#7"]) {
        const r = rig(new FakePass({ summary: text, changes: [text], scope_notes: [{ kind: "added", text }], review_pointers: [text] }));
        const pub = await r.publisher.prepare(makeSpec({ lead: { ...LEAD, what: text } }), { headSha: H1, targetBranch: target });
        assert.equal(closingDirectiveFor(pub.region, IID, "o/r"), false, `${text} / ${target}`);
      }
    }
  });

  it("every ack any test sent is one of the six api outcomes", () => {
    assert.ok(everyAck.length > 0);
    for (const a of everyAck) assert.ok((PR_DESC_ACK_OUTCOMES as readonly string[]).includes(a), a);
  });
});
