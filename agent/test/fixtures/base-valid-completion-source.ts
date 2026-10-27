// Captured verbatim from the base revision a1454259ded86a8eb4c1d48f68113d1d56c302b0
// (`git show a1454259:agent/src/recovery.ts`, functions validInventoryContext and validCompletionSource). They
// stand for the strict persisted-source gate of a worker built BEFORE #3404: coerceRecord there does
// `if (!validCompletionSource(obj.completionSource)) return null;`, and readRecordResult/listRecords then treat
// the record as unreadable and skip it. Do not edit the function bodies.
import path from "node:path";
import { RUN_KINDS, type RunKind } from "../../src/protocol.js";
import type { CompletionSource } from "../../src/recovery.js";

function validInventoryContext(value: unknown, runId: string, barePath?: string): boolean {
  if (!value || typeof value !== "object") return false;
  const c = value as Record<string, unknown>;
  if (c.runId !== runId || typeof c.branch !== "string" || !c.branch ||
      typeof c.barePath !== "string" || !path.isAbsolute(c.barePath) ||
      (barePath !== undefined && c.barePath !== barePath)) return false;
  if (c.origin === "historical") {
    return Object.keys(c).sort().join(",") === "barePath,branch,defaultIdentity,generation,kind,origin,producer,runId" &&
      c.generation === null && c.producer === "unknown" && c.kind === null && c.defaultIdentity === null;
  }
  const expected = c.generation === null
    ? "barePath,branch,defaultIdentity,generation,kind,legacy,runId"
    : "barePath,branch,defaultIdentity,generation,kind,runId";
  if (Object.keys(c).sort().join(",") !== expected ||
      (c.generation === null ? c.legacy !== true : !Number.isSafeInteger(c.generation) || (c.generation as number) <= 0) ||
      !RUN_KINDS.includes(c.kind as RunKind)) return false;
  const identity = c.defaultIdentity as Record<string, unknown> | undefined;
  return !!identity && Object.keys(identity).sort().join(",") === "ref,sha" &&
    typeof identity.ref === "string" && identity.ref.startsWith("refs/") &&
    typeof identity.sha === "string" && /^[0-9a-f]{40}$/.test(identity.sha);
}

/** Completion source shape is checked before authenticating the containing record. */
export function validCompletionSource(value: unknown): value is CompletionSource {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const s = value as CompletionSource;
  const keys = Object.keys(s).sort().join(",");
  if (keys !== "clonePath,context,expectedHead,roots" && keys !== "attemptId,clonePath,context,expectedHead,roots") return false;
  const ctx = s.context;
  return !!ctx && validInventoryContext(ctx, ctx.runId, ctx.barePath) && !("legacy" in ctx) && !("origin" in ctx) &&
    ["issue", "mr_rework", "self_improve"].includes(ctx.kind) &&
    typeof s.clonePath === "string" && path.isAbsolute(s.clonePath) && path.resolve(s.clonePath) === s.clonePath &&
    typeof s.expectedHead === "string" && /^[0-9a-f]{40}$/.test(s.expectedHead) &&
    (s.attemptId === undefined || (typeof s.attemptId === "string" &&
      new RegExp("^[0-9]{8}T[0-9]{6}Z-g" + ctx.generation + "-[0-9a-f]{16}$").test(s.attemptId))) &&
    Array.isArray(s.roots) && s.roots.every(r => r && /^[0-9a-f]{40}$/.test(r.sha) &&
      Array.isArray(r.contexts) && r.contexts.length > 0 &&
      r.contexts.every(c => validInventoryContext(c, ctx.runId, ctx.barePath) &&
        !("legacy" in c) && !("origin" in c) && c.generation === ctx.generation &&
        c.branch === ctx.branch && c.kind === ctx.kind));
}
