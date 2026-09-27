// PRD #1798 M6: an in-memory model of the api's four pr-description routes (stage, bind, lookup,
// ack; api/internal/workersvc/pr_descriptions.go), for the publisher and runner tests. It answers
// JSON the real WorkerClient decodes, so the fields a test publishes are real
// SanitizedPrDescriptionFields instances and a 409 is a real typed PrDescriptionConflict.
//
// "Sanitizing" here is a stand-in for the api's sanitizer (the security boundary lives in the api
// and has its own tests): closing keywords are broken with U+200B after their first letter and
// `<` is encoded, which is enough for a test to show that only the returned text is published.

import { randomUUID } from "node:crypto";
import { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

type Json = Record<string, unknown>;

interface Version {
  id: string;
  run_id: string;
  claim_generation: number;
  mr_iid: number | null;
  fields: Json;
  size: unknown;
  base_sha: string;
  head_sha: string;
  target_branch: string;
  source: string;
  rendered_region_sha256: string | null;
  state: "pending" | "published" | "abandoned";
  created_at: string;
  published_at: string | null;
}

interface Pr {
  mr_iid: number;
  lock_version: number;
  last_outcome: string | null;
  published_version_id: string | null;
}

export type PrDescOp = "stage" | "bind" | "lookup" | "ack";

export interface PrDescCall {
  op: PrDescOp;
  runId: string;
  body: Json;
}

const ZW = "​";

function sanitize(s: unknown): string {
  return String(s ?? "")
    .replace(/</gu, "&lt;")
    .replace(/\b(clos|fix|resolv|implement)/giu, (m) => `${m[0]}${ZW}${m.slice(1)}`);
}

function sanitizeFields(f: Json): Json {
  const list = (v: unknown): string[] => (Array.isArray(v) ? v.map(sanitize) : []);
  return {
    summary: sanitize(f.summary),
    changes: list(f.changes),
    scope_notes: Array.isArray(f.scope_notes)
      ? (f.scope_notes as Json[]).map((n) => ({ kind: n.kind, text: sanitize(n.text) }))
      : [],
    review_pointers: list(f.review_pointers),
    verification: Array.isArray(f.verification)
      ? (f.verification as Json[]).map((v) => ({ command: sanitize(v.command), result: v.result, verified_at_sha: v.verified_at_sha }))
      : [],
  };
}

export class FakePrDescApi {
  readonly versions = new Map<string, Version>();
  readonly prs = new Map<number, Pr>();
  readonly calls: PrDescCall[] = [];
  /** Queued failures per op: each entry answers one call with that status (and 409 reason). */
  private readonly failures: Partial<Record<PrDescOp, { status: number; reason?: string }[]>> = {};

  /** Answer the next call of `op` with `status` (a 409 carries `reason`). */
  failNext(op: PrDescOp, status: number, reason?: string): void {
    (this.failures[op] ??= []).push({ status, reason });
  }

  /** A PR record with a published version, for a test that models a PR uzi already described. */
  seedPublished(mrIid: number, region: { sha256: string; headSha: string; fields?: Json }): Version {
    const v: Version = {
      id: randomUUID(),
      run_id: randomUUID(),
      claim_generation: 1,
      mr_iid: mrIid,
      fields: sanitizeFields(region.fields ?? {}),
      size: null,
      base_sha: "b".repeat(40),
      head_sha: region.headSha,
      target_branch: "main",
      source: "generated",
      rendered_region_sha256: region.sha256,
      state: "published",
      created_at: "2026-09-27T10:00:00Z",
      published_at: "2026-09-27T10:00:00Z",
    };
    this.versions.set(v.id, v);
    this.prs.set(mrIid, { mr_iid: mrIid, lock_version: 1, last_outcome: "published", published_version_id: v.id });
    return v;
  }

  /** The PR record as the claim's pr_description carries it. */
  state(mrIid: number): Json | null {
    const pr = this.prs.get(mrIid);
    if (!pr) return null;
    const pub = pr.published_version_id ? this.versions.get(pr.published_version_id) : undefined;
    return { mr_iid: pr.mr_iid, lock_version: pr.lock_version, last_outcome: pr.last_outcome, published_version: pub ? { ...pub } : null };
  }

  acks(): string[] {
    return this.calls.filter((c) => c.op === "ack").map((c) => String(c.body.outcome));
  }

  handle(op: PrDescOp, runId: string, body: Json): { status: number; body: unknown } {
    this.calls.push({ op, runId, body });
    const fail = this.failures[op]?.shift();
    if (fail) return { status: fail.status, body: fail.reason ? { error: "injected", reason: fail.reason } : { error: "injected" } };
    switch (op) {
      case "stage": {
        const v: Version = {
          id: randomUUID(),
          run_id: runId,
          claim_generation: Number(body.claim_generation),
          mr_iid: typeof body.mr_iid === "number" ? body.mr_iid : null,
          fields: body.source === "deterministic_only" ? sanitizeFields({}) : sanitizeFields(body.fields as Json),
          size: body.size ?? null,
          base_sha: String(body.base_sha),
          head_sha: String(body.head_sha),
          target_branch: String(body.target_branch),
          source: String(body.source),
          rendered_region_sha256: null,
          state: "pending",
          created_at: "2026-09-27T10:00:00Z",
          published_at: null,
        };
        this.versions.set(v.id, v);
        return { status: 200, body: { version: { ...v } } };
      }
      case "bind": {
        const v = this.versions.get(String(body.version_id));
        const mrIid = Number(body.mr_iid);
        if (!v) return { status: 404, body: { error: "not found" } };
        if (v.state !== "pending" || (v.mr_iid !== null && v.mr_iid !== mrIid)) return this.conflict("version_conflict");
        if (v.rendered_region_sha256 !== null && v.rendered_region_sha256 !== body.rendered_region_sha256) {
          return this.conflict("version_conflict");
        }
        v.mr_iid = mrIid;
        v.rendered_region_sha256 = String(body.rendered_region_sha256);
        if (!this.prs.has(mrIid)) this.prs.set(mrIid, { mr_iid: mrIid, lock_version: 0, last_outcome: null, published_version_id: null });
        return { status: 200, body: { version: { ...v }, pr: this.state(mrIid) } };
      }
      case "lookup": {
        const mrIid = Number(body.mr_iid);
        const hash = String(body.region_sha256);
        const pr = this.prs.get(mrIid);
        const pub = pr?.published_version_id ? this.versions.get(pr.published_version_id) : undefined;
        if (pub && pub.rendered_region_sha256 === hash) {
          return { status: 200, body: { match: "published", matched_version_id: pub.id, pr: this.state(mrIid) } };
        }
        const pending = [...this.versions.values()].find(
          (v) => v.mr_iid === mrIid && v.state === "pending" && v.rendered_region_sha256 === hash,
        );
        if (pending) return { status: 200, body: { match: "pending", matched_version_id: pending.id, pr: this.state(mrIid) } };
        return { status: 200, body: { match: "none", matched_version_id: null, pr: this.state(mrIid) } };
      }
      case "ack": {
        const v = this.versions.get(String(body.version_id));
        if (!v) return { status: 404, body: { error: "not found" } };
        if (v.mr_iid === null || v.rendered_region_sha256 === null) return this.conflict("version_conflict");
        const pr = this.prs.get(v.mr_iid)!;
        if (Number(body.expected_lock_version) !== pr.lock_version) return this.conflict("lock_conflict");
        const outcome = String(body.outcome);
        let recovered: string | null = null;
        const observed = body.observed_region_sha256;
        if (outcome === "published" || (typeof observed === "string" && observed === v.rendered_region_sha256)) {
          if (outcome !== "published") recovered = v.id;
          v.state = "published";
          v.published_at = "2026-09-27T10:00:01Z";
          pr.published_version_id = v.id;
        } else {
          v.state = "abandoned";
        }
        pr.lock_version++;
        pr.last_outcome = outcome;
        return { status: 200, body: { pr: this.state(v.mr_iid), recovered_version_id: recovered } };
      }
    }
  }

  private conflict(reason: string): { status: number; body: unknown } {
    return { status: 409, body: { error: "conflict", reason } };
  }
}

const FAKE_BASE = "http://pr-desc.fake.test";

/**
 * A real WorkerClient whose pr-description requests are answered by `api` through a stubbed
 * global fetch (other URLs go to the real fetch). Call `restore()` when done.
 */
export function clientFor(api: FakePrDescApi): { client: WorkerClient; restore: () => void } {
  const orig = globalThis.fetch;
  globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input instanceof Request ? input.url : input);
    if (!url.startsWith(FAKE_BASE)) return orig(input, init);
    const m = /\/runs\/([^/]+)\/pr-description\/(stage|bind|lookup|ack)$/.exec(url);
    if (!m) return new Response(JSON.stringify({ error: "not found" }), { status: 404 });
    const body = init?.body ? (JSON.parse(String(init.body)) as Json) : {};
    const res = api.handle(m[2] as PrDescOp, decodeURIComponent(m[1]!), body);
    return new Response(JSON.stringify(res.body), { status: res.status, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  const client = new WorkerClient(FAKE_BASE, "token", "0.1.0-test", nullLogger(), { sleep: async () => {} });
  return { client, restore: () => (globalThis.fetch = orig) };
}
