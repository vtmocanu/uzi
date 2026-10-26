import { createHash } from "node:crypto";
import path from "node:path";
import { AuthorityRefusal, type AuthorityTransaction } from "./authority-lock.js";
import { leaseEpoch, type AuthorityStore, type SourceUseLease } from "./completion-authority.js";
import type { ExecutorSealMaterial } from "./codex/executor-lifetime.js";
import type { CompletionSource } from "./recovery.js";

// Version 2 completion sources and the source fence (issue #3404 M2 U1). Pure primitives:
// DORMANT, no production caller. A V2 source extends the V1 attempt provenance with the
// storage identity, the harness and the worker/executor incarnations that produced it, bound
// to one source-use epoch. `sourceCoreDigest` covers everything except the certificate, and the
// certificate is an HMAC over that digest minted only by the authority store (the key never
// leaves completion-authority.ts), so a source that was not published under the authority
// cannot present one.

const CORE_DOMAIN = "uzi completion source core v2\0";
const MAX_PATH = 4096;
const MAX_ITEM = 512;
const MAX_ROOTS = 1024;
const MAX_ROOT_CONTEXTS = 64;
const MAX_INCARNATIONS = 256;
const HEX40 = /^[0-9a-f]{40}$/;
const HEX64 = /^[0-9a-f]{64}$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const INCARNATION = /^[A-Za-z0-9._:-]{1,128}$/;
const AUTHORITY_DIR_NAME = ".uzi-authority";
const KINDS = ["issue", "mr_rework", "self_improve"];
const CONTEXT_KEYS = "barePath,branch,defaultIdentity,generation,kind,runId";

export interface CompletionSourceV2 {
  version: 2;
  context: CompletionSource["context"];
  clonePath: string;
  attemptId?: string;
  expectedHead: string;
  roots: CompletionSource["roots"];
  storageId: string;
  layout: string;
  /** Layout-relative canonical spelling of `clonePath`. */
  relativeSource: string;
  harness: "codex" | "claude";
  workerIncarnation: string;
  /** Codex only. */
  executorIncarnation?: string;
  /** Codex only; strictly ascending. */
  registryIncarnations?: string[];
  /** Codex only; strictly ascending. */
  facadeIncarnations?: string[];
  sourceUseEpoch: string;
  /** HMAC over `sourceCoreDigest` minted by the authority store; absent on an uncertified core. */
  certificate?: string;
}

/** The identity a source must be consistent with. */
export interface SourceExpectation { dataDir: string; storageId: string; layout: string }

function refuse(): never { throw new AuthorityRefusal("malformed completion source"); }
function isPlain(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    && Object.getPrototypeOf(value) === Object.prototype;
}
function exactKeys(value: unknown, required: string[], optional: string[] = []): Record<string, unknown> {
  if (!isPlain(value)) return refuse();
  const keys = Object.keys(value);
  if (required.some((k) => !keys.includes(k)) || keys.some((k) => !required.includes(k) && !optional.includes(k))) return refuse();
  if (keys.some((k) => value[k] === undefined)) return refuse();
  return value;
}
function str(value: unknown, max: number): string {
  if (typeof value !== "string" || value.length < 1 || value.length > max || value.includes("\0")) return refuse();
  return value;
}
function match(value: unknown, re: RegExp): string {
  if (typeof value !== "string" || !re.test(value)) return refuse();
  return value;
}
function generationOf(value: unknown): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0) return refuse();
  return value;
}
function absolute(value: unknown): string {
  const p = str(value, MAX_PATH);
  if (!path.isAbsolute(p) || path.resolve(p) !== p) return refuse();
  return p;
}

function checkContext(value: unknown): CompletionSource["context"] {
  const c = exactKeys(value, CONTEXT_KEYS.split(","));
  const identity = exactKeys(c.defaultIdentity, ["ref", "sha"]);
  if (!KINDS.includes(c.kind as string)) return refuse();
  const ref = str(identity.ref, MAX_ITEM);
  if (!ref.startsWith("refs/")) return refuse();
  return {
    runId: str(c.runId, 128), branch: str(c.branch, MAX_ITEM), kind: c.kind as "issue", barePath: absolute(c.barePath),
    generation: generationOf(c.generation), defaultIdentity: { ref, sha: match(identity.sha, HEX40) },
  };
}

function checkRoots(value: unknown, ctx: CompletionSource["context"]): CompletionSource["roots"] {
  if (!Array.isArray(value) || value.length > MAX_ROOTS) return refuse();
  return value.map((raw) => {
    const r = exactKeys(raw, ["sha", "contexts"]);
    if (!Array.isArray(r.contexts) || r.contexts.length < 1 || r.contexts.length > MAX_ROOT_CONTEXTS) return refuse();
    const contexts = r.contexts.map((entry) => {
      const c = checkContext(entry);
      if (c.runId !== ctx.runId || c.barePath !== ctx.barePath || c.generation !== ctx.generation ||
          c.branch !== ctx.branch || c.kind !== ctx.kind) return refuse();
      return c;
    });
    return { sha: match(r.sha, HEX40), contexts };
  });
}

function sortedIds(value: unknown): string[] {
  if (!Array.isArray(value) || value.length > MAX_INCARNATIONS) return refuse();
  const ids = value.map((id) => match(id, INCARNATION));
  for (let i = 1; i < ids.length; i++) if (!((ids[i - 1] as string) < (ids[i] as string))) return refuse();
  return ids;
}

function relativeOf(value: unknown): string {
  const rel = str(value, MAX_PATH);
  const parts = rel.split("/");
  if (path.isAbsolute(rel) || path.normalize(rel) !== rel || parts.some((p) => p === "" || p === "." || p === "..") ||
      parts[0] === AUTHORITY_DIR_NAME) return refuse();
  return rel;
}

/**
 * Strictly decode a V2 source: exact key set (codex carries the three incarnation lists,
 * claude none), bounded strings, hex and UUID formats, and a clone path that is the absolute
 * normalized join of `expected.dataDir` and `relativeSource`. Returns a fresh object.
 */
export function parseCompletionSourceV2(value: unknown, expected: SourceExpectation): CompletionSourceV2 {
  if (!isPlain(value)) return refuse();
  const codex = value.harness === "codex";
  if (!codex && value.harness !== "claude") return refuse();
  const base = ["version", "context", "clonePath", "expectedHead", "roots", "storageId", "layout", "relativeSource",
    "harness", "workerIncarnation", "sourceUseEpoch"];
  const o = exactKeys(value, codex ? [...base, "executorIncarnation", "registryIncarnations", "facadeIncarnations"] : base,
    ["attemptId", "certificate"]);
  if (o.version !== 2) return refuse();
  const context = checkContext(o.context);
  const clonePath = absolute(o.clonePath);
  const relativeSource = relativeOf(o.relativeSource);
  if (expected.dataDir !== path.resolve(expected.dataDir) || clonePath !== path.join(expected.dataDir, relativeSource)) return refuse();
  if (match(o.storageId, UUID) !== expected.storageId || o.layout !== expected.layout) return refuse();
  const out: CompletionSourceV2 = {
    version: 2, context, clonePath, expectedHead: match(o.expectedHead, HEX40), roots: checkRoots(o.roots, context),
    storageId: expected.storageId, layout: expected.layout, relativeSource, harness: codex ? "codex" : "claude",
    workerIncarnation: match(o.workerIncarnation, UUID), sourceUseEpoch: match(o.sourceUseEpoch, UUID),
  };
  if (o.attemptId !== undefined) {
    out.attemptId = match(o.attemptId, new RegExp(`^[0-9]{8}T[0-9]{6}Z-g${context.generation}-[0-9a-f]{16}$`));
  }
  if (codex) {
    out.executorIncarnation = match(o.executorIncarnation, INCARNATION);
    out.registryIncarnations = sortedIds(o.registryIncarnations);
    out.facadeIncarnations = sortedIds(o.facadeIncarnations);
  }
  if (o.certificate !== undefined) out.certificate = match(o.certificate, HEX64);
  return out;
}

function canonicalJson(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (typeof value === "object" && value !== null) {
    const record = value as Record<string, unknown>;
    return `{${Object.keys(record).sort().map((k) => `${JSON.stringify(k)}:${canonicalJson(record[k])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

/** sha256 hex over the domain-separated canonical JSON of the source minus its certificate. */
export function sourceCoreDigest(source: CompletionSourceV2): string {
  const { certificate: _certificate, ...core } = source;
  return createHash("sha256").update(CORE_DOMAIN).update(canonicalJson(core)).digest("hex");
}

// ---- fence ----------------------------------------------------------------------------

/** Exactly these three fields, in this order, wherever a fence is serialized. */
export interface CompletionSourceFence { version: 1; sourceUseEpoch: string; sourceCoreDigest: string }

export function parseSourceFence(value: unknown): CompletionSourceFence {
  const o = exactKeys(value, ["version", "sourceUseEpoch", "sourceCoreDigest"]);
  if (o.version !== 1) return refuse();
  return { version: 1, sourceUseEpoch: match(o.sourceUseEpoch, UUID), sourceCoreDigest: match(o.sourceCoreDigest, HEX64) };
}

/** Fixed field order, so JSON equality of two serializations is fence equality. */
export function serializeSourceFence(fence: CompletionSourceFence): string {
  const f = parseSourceFence(fence);
  return JSON.stringify({ version: f.version, sourceUseEpoch: f.sourceUseEpoch, sourceCoreDigest: f.sourceCoreDigest });
}

/** Parse serialized text; anything but the canonical spelling (reordered, spaced) refuses. */
export function parseSourceFenceJson(text: string): CompletionSourceFence {
  let raw: unknown;
  try { raw = JSON.parse(text); } catch { return refuse(); }
  const fence = parseSourceFence(raw);
  if (serializeSourceFence(fence) !== text) return refuse();
  return fence;
}

export function sourceFenceOf(source: CompletionSourceV2): CompletionSourceFence {
  return { version: 1, sourceUseEpoch: source.sourceUseEpoch, sourceCoreDigest: sourceCoreDigest(source) };
}

// ---- certificates -----------------------------------------------------------------------

/** Mint the certificate for a core. Needs a live transaction; the publication path only. */
export function certifySourceCore(tx: AuthorityTransaction, store: AuthorityStore, source: CompletionSourceV2): CompletionSourceV2 {
  const checked = parseCompletionSourceV2(source, { dataDir: store.dataDir, storageId: store.storageId, layout: store.layout });
  const { certificate: _old, ...bare } = checked;
  return { ...bare, certificate: store.certifySource(tx, sourceCoreDigest(bare)) };
}

/** Decode a stored source and require a certificate valid under this store's key. */
export function verifyCertifiedSource(tx: AuthorityTransaction, store: AuthorityStore, value: unknown): CompletionSourceV2 {
  const source = parseCompletionSourceV2(value, { dataDir: store.dataDir, storageId: store.storageId, layout: store.layout });
  if (!store.verifySourceCertificate(tx, sourceCoreDigest(source), source.certificate)) {
    throw new AuthorityRefusal("completion source certificate does not verify");
  }
  return source;
}

// ---- builders (uncertified cores) ---------------------------------------------------------

interface CoreInput {
  store: AuthorityStore;
  /** V1 attempt provenance; `clonePath` must lie under the store's data root. */
  base: CompletionSource;
  /** The exact live lease whose epoch the core is bound to. */
  lease: SourceUseLease;
}

function core(input: CoreInput, extra: Partial<CompletionSourceV2> & Pick<CompletionSourceV2, "harness">): CompletionSourceV2 {
  const base = structuredClone(input.base);
  const draft: Record<string, unknown> = {
    version: 2, context: base.context, clonePath: base.clonePath, expectedHead: base.expectedHead, roots: base.roots,
    storageId: input.store.storageId, layout: input.store.layout,
    relativeSource: path.relative(input.store.dataDir, base.clonePath).split(path.sep).join("/"),
    workerIncarnation: input.store.workerIncarnation, sourceUseEpoch: leaseEpoch(input.lease), ...extra,
  };
  if (base.attemptId !== undefined) draft.attemptId = base.attemptId;
  return parseCompletionSourceV2(draft, {
    dataDir: input.store.dataDir, storageId: input.store.storageId, layout: input.store.layout,
  });
}

/** Uncertified codex core from a terminal seal's material; incarnation lists are sorted. */
export function buildCodexSourceCore(input: CoreInput & { material: ExecutorSealMaterial }): CompletionSourceV2 {
  const sort = (ids: string[]): string[] => ids.sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
  return core(input, {
    harness: "codex", executorIncarnation: input.material.executorIncarnation,
    registryIncarnations: sort(input.material.incarnations.map((i) => i.registry)),
    facadeIncarnations: sort(input.material.incarnations.map((i) => i.facade)),
  });
}

/** Uncertified claude core: no executor, registry or facade incarnations. */
export function buildClaudeSourceCore(input: CoreInput): CompletionSourceV2 {
  return core(input, { harness: "claude" });
}
