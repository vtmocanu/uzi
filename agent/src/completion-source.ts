import { createHash } from "node:crypto";
import path from "node:path";
import { AuthorityRefusal, type AuthorityTransaction } from "./authority-lock.js";
import { leaseEpoch, type AuthorityStore, type SlotPublicationRead, type SourceUseLease } from "./completion-authority.js";
import { terminalSealMaterial, terminalSealStillAccepted } from "./codex/executor-lifetime.js";
import { type CompletionSource, validCompletionSource } from "./recovery.js";

// Version 2 completion sources and the source fence (issue #3404 M2 U1). Pure primitives:
// Used by publication (runner/recovery) and restart retirement. A V2 source extends the V1 attempt provenance with the
// storage identity, the harness and the worker/executor incarnations that produced it, bound
// to one source-use epoch. `sourceCoreDigest` covers everything except the certificate, and the
// certificate is an HMAC over that digest minted only by the authority store (the key never
// leaves completion-authority.ts) and only for the exact live lease of the source's slot
// (AuthorityStore.certifySource), so a source that was not produced under a live lease cannot
// present one. `verifyCertifiedSource` is a pure MAC check and says nothing about freshness: a
// consumer that needs the source to still be the published one uses `verifyPublishedSource`,
// which also compares the slot's publication (readPublication).

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
  /** Codex only: one [registry, facade] pair per inventoried registry, strictly ascending by registry id. */
  registryFacadePairs?: [string, string][];
  sourceUseEpoch: string;
  /** HMAC over `sourceCoreDigest` minted by the authority store; absent on an uncertified core. */
  certificate?: string;
}

/** The identity a source must be consistent with. */
export interface SourceExpectation { dataDir: string; storageId: string; layout: string }

function refuse(): never { throw new AuthorityRefusal("malformed completion source", "source_mismatch"); }
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

function checkPairs(value: unknown): [string, string][] {
  if (!Array.isArray(value) || value.length > MAX_INCARNATIONS) return refuse();
  const pairs = value.map((pair): [string, string] => {
    if (!Array.isArray(pair) || pair.length !== 2) return refuse();
    return [match(pair[0], INCARNATION), match(pair[1], INCARNATION)];
  });
  for (let i = 1; i < pairs.length; i++) {
    if (!(((pairs[i - 1] as [string, string])[0]) < (pairs[i] as [string, string])[0])) return refuse();
  }
  if (new Set(pairs.map((pair) => pair[1])).size !== pairs.length) return refuse();
  return pairs;
}

function relativeOf(value: unknown): string {
  const rel = str(value, MAX_PATH);
  const parts = rel.split("/");
  if (path.isAbsolute(rel) || path.normalize(rel) !== rel || parts.some((p) => p === "" || p === "." || p === "..") ||
      parts[0] === AUTHORITY_DIR_NAME) return refuse();
  return rel;
}

/**
 * Decode a V2 source: the V1 core first (recovery.ts `validCompletionSource`, so both versions
 * agree on context, roots and attempt shape), then the exact key set (codex carries the
 * registry/facade pairs, claude none), bounded strings, hex and UUID formats. With an
 * expectation the clone path must be the absolute normalized join of `expected.dataDir` and
 * `relativeSource` under that storage identity; without one only the shape is checked (digest
 * and fence computation). Returns a fresh object.
 */
function decodeSource(value: unknown, expected?: SourceExpectation): CompletionSourceV2 {
  if (!isPlain(value)) return refuse();
  const v1: Record<string, unknown> = {};
  for (const key of ["context", "clonePath", "expectedHead", "roots", "attemptId"]) if (value[key] !== undefined) v1[key] = value[key];
  let v1Valid = false;
  try { v1Valid = validCompletionSource(v1); } catch { v1Valid = false; }
  if (!v1Valid) return refuse();
  const codex = value.harness === "codex";
  if (!codex && value.harness !== "claude") return refuse();
  const base = ["version", "context", "clonePath", "expectedHead", "roots", "storageId", "layout", "relativeSource",
    "harness", "workerIncarnation", "sourceUseEpoch"];
  const o = exactKeys(value, codex ? [...base, "executorIncarnation", "registryFacadePairs"] : base,
    ["attemptId", "certificate"]);
  if (o.version !== 2) return refuse();
  const context = checkContext(o.context);
  const clonePath = absolute(o.clonePath);
  const relativeSource = relativeOf(o.relativeSource);
  const storageId = match(o.storageId, UUID);
  const layout = str(o.layout, 128);
  if (expected) {
    if (expected.dataDir !== path.resolve(expected.dataDir) || clonePath !== path.join(expected.dataDir, relativeSource)) return refuse();
    if (storageId !== expected.storageId || layout !== expected.layout) return refuse();
  } else if (!clonePath.endsWith(`/${relativeSource}`)) return refuse();
  const out: CompletionSourceV2 = {
    version: 2, context, clonePath, expectedHead: match(o.expectedHead, HEX40), roots: checkRoots(o.roots, context),
    storageId, layout, relativeSource, harness: codex ? "codex" : "claude",
    workerIncarnation: match(o.workerIncarnation, UUID), sourceUseEpoch: match(o.sourceUseEpoch, UUID),
  };
  if (o.attemptId !== undefined) {
    out.attemptId = match(o.attemptId, new RegExp(`^[0-9]{8}T[0-9]{6}Z-g${context.generation}-[0-9a-f]{16}$`));
  }
  if (codex) {
    out.executorIncarnation = match(o.executorIncarnation, INCARNATION);
    out.registryFacadePairs = checkPairs(o.registryFacadePairs);
  }
  if (o.certificate !== undefined) out.certificate = match(o.certificate, HEX64);
  return out;
}

/**
 * Shape-only strict decode of a persisted V2 source (exact key set, bounded strings, hex and UUID
 * formats), for a reader that has no storage identity: the journal record decoder. It proves nothing
 * about the certificate or freshness; use `verifyPublishedSource` for that.
 */
export function decodeCompletionSourceShape(value: unknown): CompletionSourceV2 {
  return decodeSource(value);
}

/** Strictly decode a V2 source against the storage identity it must belong to. */
export function parseCompletionSourceV2(value: unknown, expected: SourceExpectation): CompletionSourceV2 {
  return decodeSource(value, expected);
}

/**
 * Canonical JSON for digests. Throws on anything JSON would drop or conflate (undefined,
 * functions, symbols, bigint, non-finite numbers, negative zero, sparse arrays, non-plain
 * objects), so two values with one spelling can never differ.
 */
function canonicalJson(value: unknown): string {
  if (value === null || typeof value === "boolean" || typeof value === "string") return JSON.stringify(value);
  if (typeof value === "number") {
    if (!Number.isFinite(value) || Object.is(value, -0)) return refuse();
    return JSON.stringify(value);
  }
  if (Array.isArray(value)) {
    const items: string[] = [];
    for (let i = 0; i < value.length; i++) {
      if (!(i in value)) return refuse();
      items.push(canonicalJson(value[i]));
    }
    return `[${items.join(",")}]`;
  }
  if (isPlain(value)) {
    return `{${Object.keys(value).sort().map((k) => `${JSON.stringify(k)}:${canonicalJson(value[k])}`).join(",")}}`;
  }
  return refuse();
}

/**
 * sha256 hex over the domain-separated canonical JSON of the source minus its certificate. The
 * input is decoded first (shape only), so an unknown key or an undefined value refuses rather
 * than digesting differently from its absent spelling.
 */
export function sourceCoreDigest(source: CompletionSourceV2): string {
  const { certificate: _certificate, ...core } = decodeSource(source);
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
  // The digest decodes (and so validates) the whole source, epoch included, before the fence is returned.
  return { version: 1, sourceUseEpoch: source.sourceUseEpoch, sourceCoreDigest: sourceCoreDigest(source) };
}

// ---- certificates -----------------------------------------------------------------------

/**
 * Decode a stored source and require a certificate valid under this store's key. This is a pure
 * MAC check: it proves the core was certified, not that it is still the published source. Use
 * `verifyPublishedSource` for freshness.
 */
export function verifyCertifiedSource(tx: AuthorityTransaction, store: AuthorityStore, value: unknown): CompletionSourceV2 {
  const source = parseCompletionSourceV2(value, { dataDir: store.dataDir, storageId: store.storageId, layout: store.layout });
  if (!store.verifySourceCertificate(tx, sourceCoreDigest(source), source.certificate)) {
    throw new AuthorityRefusal("completion source certificate does not verify", "certificate_invalid");
  }
  return source;
}

/**
 * Certificate AND freshness: the slot for the source's path must currently publish this very
 * source (state sealed for codex or claude_ready for claude, the same epoch, harness and digest).
 * A pending footprint, a replayed or malformed slot, or a missing publication refuses.
 */
export async function verifyPublishedSource(
  tx: AuthorityTransaction, store: AuthorityStore, value: unknown,
): Promise<{ source: CompletionSourceV2; publication: SlotPublicationRead }> {
  const source = verifyCertifiedSource(tx, store, value);
  const publication = await store.readPublication(tx, source.clonePath);
  const state = source.harness === "codex" ? "sealed" : "claude_ready";
  if (publication === null || publication.state !== state || publication.harness !== source.harness ||
      publication.epoch !== source.sourceUseEpoch || publication.sourceCoreDigest !== sourceCoreDigest(source)) {
    throw new AuthorityRefusal("completion source is not the published source", "source_mismatch");
  }
  return { source, publication };
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

/**
 * Uncertified codex core from an opaque terminal seal. The material comes only from
 * `terminalSealMaterial(seal)`; it must have been sealed for exactly this lease and the issuing
 * lifetime must still accept it (answered from the seal's own recorded state, never a caller
 * callback). There is no plain-material input.
 */
export function buildCodexSourceCore(input: CoreInput & CodexSealInput): CompletionSourceV2 {
  const { executorIncarnation, registryFacadePairs } = codexSealFields(input.lease, input);
  return core(input, { harness: "codex", executorIncarnation, registryFacadePairs });
}

/** The terminal seal a codex source is derived from. */
export interface CodexSealInput {
  seal: unknown;
}

/**
 * The codex-only source fields, derived from an opaque terminal seal. The material comes only
 * from `terminalSealMaterial(seal)`; the seal must have been sealed for exactly this lease and
 * its issuing lifetime must still accept it. The store re-derives them with this same function to certify.
 */
export function codexSealFields(
  lease: SourceUseLease, input: CodexSealInput,
): { executorIncarnation: string; registryFacadePairs: [string, string][] } {
  const material = terminalSealMaterial(input.seal);
  if (!material) throw new AuthorityRefusal("not a terminal seal");
  if (material.sourceUse !== lease) throw new AuthorityRefusal("terminal seal belongs to another source use");
  if (!terminalSealStillAccepted(input.seal, lease)) throw new AuthorityRefusal("terminal seal is not accepted");
  const registryFacadePairs = material.incarnations.map((i): [string, string] => [i.registry, i.facade])
    .sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
  return { executorIncarnation: material.executorIncarnation, registryFacadePairs };
}

/** Uncertified claude core: no executor, registry or facade incarnations. */
export function buildClaudeSourceCore(input: CoreInput): CompletionSourceV2 {
  return core(input, { harness: "claude" });
}
