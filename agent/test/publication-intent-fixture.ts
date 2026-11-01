import { createHmac } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { Outbox, type RawWriteSeam } from "../src/outbox.js";
import type {
  PublicationApi,
  PublicationIntentBody,
  PublicationIntentRequest,
  PublicationIntentResult,
  PublicationOwnerKind,
  PublicationRecord,
} from "../src/publication-intent.js";
import { nullLogger } from "./helpers.js";

// Shared fixtures for the issue #1324 publication-intent tests: a valid intent, a throwaway outbox
// root, a scripted api, and a MAC forger that is only used to build records the real writer never
// produces (a wrong domain, a future version).

export const RUN = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
export const RUN_B = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
export const REPO = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
export const HEAD = "1".repeat(40);
export const FLOOR = "2".repeat(40);
export const CAND = "3".repeat(40);
export const CAND2 = "4".repeat(40);

export function intent(over: Partial<PublicationIntentBody> = {}): PublicationIntentBody {
  return {
    v: 1,
    run_id: RUN,
    repo_id: REPO,
    branch: "agent/issue-1324",
    claim_generation: 4,
    original_head: HEAD,
    floor: { kind: "pinned", oid: FLOOR },
    candidate: CAND,
    source: { pin_generation: 2, checkpoint_key: null },
    mr: { title: "Fix the thing", summary: "A lead summary.", closing: ["#1324"], contract_revision: 3 },
    guard: {
      frozen_held: { version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: null, extraFloors: [FLOOR] },
      secret_remediation: {
        attempts: 0, known: [], flagged_commits: [], flagged_findings: [], blocked: [],
        ever_known: false, withhold_paths: false, overflow: false,
      },
      published_tip: FLOOR,
      published_real_tips: [FLOOR],
      landed_checkpoint: true,
      ci_fix_human_approved: false,
      ci_config_guard_globs: [".github/workflows/**"],
      last_checkpoint_ref_tip: FLOOR,
    },
    ...over,
  };
}

const roots: string[] = [];

export async function cleanupRoots(): Promise<void> {
  for (const root of roots.splice(0)) await fsp.rm(root, { recursive: true, force: true }).catch(() => undefined);
}

export function tmpRoot(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "publication-"));
  roots.push(dir);
  return path.join(dir, "outbox");
}

export function makeOutbox(root: string, opts: { rawWrite?: RawWriteSeam; now?: () => number; retentionMs?: number } = {}): Outbox {
  return new Outbox({
    root,
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: opts.retentionMs ?? 7 * 86_400_000,
    ...(opts.rawWrite ? { rawWrite: opts.rawWrite } : {}),
    ...(opts.now ? { now: opts.now } : {}),
  });
}

/** A scripted api: each call consumes the next scripted result (the last one repeats). */
export function scriptedApi(...script: PublicationIntentResult[]): PublicationApi & { calls: { runId: string; request: PublicationIntentRequest }[] } {
  const calls: { runId: string; request: PublicationIntentRequest }[] = [];
  return {
    calls,
    async registerPublicationIntent(runId, request) {
      calls.push({ runId, request: JSON.parse(JSON.stringify(request)) as PublicationIntentRequest });
      return script[Math.min(calls.length - 1, script.length - 1)] ?? { kind: "unknown", detail: "unscripted" };
    },
  };
}

export const noSleep = async (): Promise<void> => undefined;

function canonical(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value) ?? "null";
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  const o = value as Record<string, unknown>;
  return `{${Object.keys(o).sort().filter((k) => o[k] !== undefined).map((k) => `${JSON.stringify(k)}:${canonical(o[k])}`).join(",")}}`;
}

/** Seal `body` with the outbox key at `root` under an arbitrary MAC domain (to build records the real writer never writes). */
export async function forgeRecord(root: string, domain: string, body: object): Promise<string> {
  const key = await fsp.readFile(path.join(root, ".key"));
  const mac = createHmac("sha256", key).update(domain).update(canonical(body)).digest("hex");
  return JSON.stringify({ ...body, mac });
}

export const PUBLICATION_DOMAIN = "uzi.outbox.publication.v1";
export const FINALIZE_DOMAIN = "uzi.outbox.finalize.v1";

/** The local record for an intent: the body plus the caller-supplied owner kind ("issue" unless stated). */
export function stored(over: Partial<PublicationIntentBody> = {}, ownerKind: PublicationOwnerKind = "issue"): PublicationRecord {
  return { ...intent(over), owner_kind: ownerKind };
}

export function recordWithSeqs(over: Partial<PublicationRecord> = {}): PublicationRecord {
  return { ...stored(), enrollment_seq: 1, candidate_seq: 1, ...over };
}
