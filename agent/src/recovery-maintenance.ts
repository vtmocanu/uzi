import { AsyncLocalStorage } from "node:async_hooks";
import { createHash, createHmac, randomUUID, timingSafeEqual } from "node:crypto";
import { constants as C } from "node:fs";
import fs from "node:fs/promises";
import path from "node:path";
import { performance } from "node:perf_hooks";
import { z } from "zod";
import { MaintenanceBudget } from "./maintenance-budget.js";

export const MAINTENANCE_VERIFIER_VERSION = 1;
const DEFAULT_MAINTENANCE_DECODED_CAP = 8 * 1024 ** 3;
const DAY = 86_400_000, SLOT = 120_000, SPACING = 180_000;
const TABLE_BYTES = 16 * 1024 * 1024, MARKER_BYTES = 1024 * 1024, CAPACITY = 4096;
const safe = z.string().regex(/^[A-Za-z0-9_-]{1,128}(?![\s\S])/);
const digest = z.string().regex(/^[a-f0-9]{64}(?![\s\S])/);
const integer = z.number().int().min(0).max(Number.MAX_SAFE_INTEGER);
const identitySchema = z.object({
  run: safe, generation: integer.min(1), hold: safe, repo: safe, custodian: safe,
  bareBasename: z.string().refine(value => value.length > 0 && value !== "." && value !== ".." &&
    !/[/\\\p{Cc}\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069\uD800-\uDFFF]/u.test(value) &&
    Buffer.byteLength(value, "utf8") <= 255), origin: digest, originalSource: digest, sourceConfig: digest,
}).strict();
export type MaintenanceIdentity = z.infer<typeof identitySchema>;
const configSchema = z.object({
  decodedCap: integer.min(1).max(64 * 1024 ** 3),
  verifierVersion: integer.min(1).max(MAINTENANCE_VERIFIER_VERSION),
}).strict();
export type MaintenanceConfig = z.infer<typeof configSchema>;

/** Digests are caller-validated stable identities, never raw URLs, configs or credentials. */
export interface MaintenanceAdmission {
  identity: MaintenanceIdentity;
  config: MaintenanceConfig;
  exactCustody: true;
  terminalNonStopped: true;
  /** Current source: original input or a previously indexed output of this input. */
  source: string;
}
/** Fresh physical/content proof, established independently under the charged slot. */
export interface MaintenanceEligibility extends MaintenanceAdmission {
  quiescent: true;
  sourceOutputContinuity: true;
}
export type MaintenanceDenial =
  | "maintenance_state_invalid" | "maintenance_identity_invalid" | "maintenance_ineligible"
  | "maintenance_table_capacity" | "maintenance_config_changed" | "maintenance_lineage_invalid"
  | "maintenance_clock_invalid" | "maintenance_cycle_expired" | "maintenance_cycle_exhausted"
  | "maintenance_slot_spacing" | "maintenance_mutation_fenced" | "maintenance_token_invalid"
  | "maintenance_resource_exhausted" | "maintenance_slot_active" | "maintenance_discovery_only";
export type MaintenanceResult<T> = { ok: true; value: T } | { ok: false; reason: MaintenanceDenial };
const deny = <T>(reason: MaintenanceDenial): MaintenanceResult<T> => ({ ok: false, reason });
class MaintenanceRefusal extends Error {
  constructor(readonly reason: MaintenanceDenial) { super(reason); }
}
function refuse(reason: MaintenanceDenial): never { throw new MaintenanceRefusal(reason); }
const quotaSchema = z.object({
  decoded: integer, closureEntries: integer, sourceBytes: integer, sourceEntries: integer,
  artifactBytes: integer, metadataBytes: integer, adminEntries: integer, sources: integer,
  launches: integer, rpcAttempts: integer,
}).strict();
export type MaintenanceResource = keyof z.infer<typeof quotaSchema>;
type Quotas = z.infer<typeof quotaSchema>;
function quotas(cap: number): Quotas {
  return { decoded: cap, closureEntries: 65536, sourceBytes: 256 * 1024 ** 2,
    sourceEntries: 65536, artifactBytes: 256 * 1024 ** 2, metadataBytes: 256 * 1024 ** 2,
    adminEntries: 4096, sources: 128, launches: 512, rpcAttempts: 32 };
}
function multiply(q: Quotas, count: number): Quotas {
  return Object.fromEntries(Object.entries(q).map(([k, v]) => [k, v * count])) as Quotas;
}
const slotSchema = z.object({
  nonce: safe, ordinal: integer.min(1).max(3), start: integer, deadline: integer,
  nextStart: integer, reserved: quotaSchema,
}).strict();
const cycleSchema = z.object({
  identity: identitySchema, config: configSchema, admittedAt: integer, originalCycleExpiry: integer,
  lastObserved: integer, slots: z.array(slotSchema).max(3), reservedWork: integer,
  reserved: quotaSchema, outputs: z.array(digest).max(CAPACITY),
  exhausted: z.boolean(), fenced: z.boolean(),
}).strict();
type Cycle = z.infer<typeof cycleSchema>;
const tableSchema = z.object({
  version: z.literal(1), ledger: safe, complete: z.literal(true),
  cycles: z.array(cycleSchema).max(CAPACITY),
}).strict();
type Table = z.infer<typeof tableSchema>;
const markerSchema = z.object({
  version: z.literal(1), ledger: safe, tableHash: digest, history: z.array(digest).max(CAPACITY),
}).strict();
type Marker = z.infer<typeof markerSchema>;
function canonical(value: unknown): string {
  if (Array.isArray(value)) return "[" + value.map(canonical).join(",") + "]";
  if (value !== null && typeof value === "object") {
    return "{" + Object.entries(value).sort(([a], [b]) => a.localeCompare(b, "en"))
      .map(([k, v]) => JSON.stringify(k) + ":" + canonical(v)).join(",") + "}";
  }
  return JSON.stringify(value);
}
function hash(value: unknown): string { return createHash("sha256").update(canonical(value)).digest("hex"); }
function cycleKey(identity: MaintenanceIdentity, config: MaintenanceConfig): string { return hash([identity, config]); }
function index(table: Table): string[] {
  // Every original input, immutable config and output has a separate retained index entry.
  return [...new Set(table.cycles.flatMap(cycle => {
    const id = hash(cycle.identity);
    return [id, hash([id, cycle.config]), ...cycle.outputs.map(output => hash([id, output]))];
  }))].sort();
}
const locks = new Map<string, Promise<void>>();
const active = new Map<string, Map<string, Live>>();
async function locked<T>(root: string, action: () => Promise<T>): Promise<T> {
  const previous = locks.get(root) ?? Promise.resolve();
  let release!: () => void;
  const next = new Promise<void>(resolve => { release = resolve; });
  locks.set(root, next);
  await previous;
  try { return await action(); }
  finally { release(); if (locks.get(root) === next) locks.delete(root); }
}
export interface MaintenanceSlot {
  readonly ordinal: number;
  readonly admittedAt: number;
  readonly originalCycleExpiry: number;
  readonly start: number;
  readonly deadline: number;
}
interface Live {
  root: string; cycle: string; nonce: string; deadline: number; monotonicDeadline: number; now: () => number; monotonicNow: () => number;
  verified: boolean; verification: object; source: string;
  revoked: boolean; finished: boolean; lastObserved: number; used: Quotas; limits: Quotas; signal?: AbortSignal;
  controller: AbortController; timer?: ReturnType<typeof setTimeout>;
}
const tokens = new WeakMap<MaintenanceSlot, Live>();
const resources = new AsyncLocalStorage<MaintenanceSlot>();
/** Authority-free ledger. Callers must independently prove every eligibility assertion each time. */
export class RecoveryMaintenance {
  private readonly key: Buffer;
  private readonly requestedRoot: string;
  constructor(private readonly options: {
    /** Dedicated sibling of recovery/, conventionally dataDir/recovery-maintenance. */
    root: string; workerToken: string; now?: () => number;
    /** Test-only monotonic clock; production always uses performance.now. */
    monotonicNow?: () => number;
    /** Fault injection only: exceptions preserve all precharged work. */
    prechargeHook?: (phase: "before" | "after") => void | Promise<void>;
    initializationHook?: (phase: "mkdir" | "table" | "pair" | "installed") => void | Promise<void>;
  }) {
    this.requestedRoot = path.resolve(options.root);
    this.key = createHmac("sha256", options.workerToken).update("uzi-recovery-maintenance-v1").digest();
  }

  /** Defaults apply only to fresh caller configuration; persisted legacy caps are never inferred. */
  static configuration(decodedCap = DEFAULT_MAINTENANCE_DECODED_CAP): MaintenanceConfig {
    return configSchema.parse({ decodedCap, verifierVersion: MAINTENANCE_VERIFIER_VERSION });
  }

  private now(): number {
    const now = (this.options.now ?? Date.now)();
    if (!Number.isSafeInteger(now) || now < 0) refuse("maintenance_clock_invalid");
    return now;
  }
  private async root(): Promise<string> {
    if (!this.options.workerToken || path.basename(this.requestedRoot) !== "recovery-maintenance")
      refuse("maintenance_state_invalid");
    // Reject symlink components rather than following even an otherwise canonical alias.
    let current = path.parse(this.requestedRoot).root;
    const parent = path.dirname(this.requestedRoot);
    for (const part of parent.slice(current.length).split(path.sep).filter(Boolean)) {
      current = path.join(current, part);
      const stat = await fs.lstat(current);
      if (!stat.isDirectory() || stat.isSymbolicLink()) refuse("maintenance_state_invalid");
    }
    return path.join(await fs.realpath(parent), "recovery-maintenance");
  }
  private mac(value: unknown): string { return createHmac("sha256", this.key).update(canonical(value)).digest("hex"); }
  private async read(file: string, max: number): Promise<unknown> {
    const handle = await fs.open(file, C.O_RDONLY | C.O_NOFOLLOW | C.O_NONBLOCK);
    try {
      const stat = await handle.stat();
      if (!stat.isFile() || stat.nlink !== 1 || stat.size > max || stat.size < 1)
        refuse("maintenance_state_invalid");
      // A fixed bounded allocation; reads cannot grow with a concurrently enlarged file.
      const buffer = Buffer.alloc(max + 1);
      let length = 0;
      while (length < buffer.length) {
        const { bytesRead } = await handle.read(buffer, length, buffer.length - length, null);
        if (!bytesRead) break;
        MaintenanceResources.consumeIfActive("metadataBytes", bytesRead);
        length += bytesRead;
      }
      if (length > max) refuse("maintenance_state_invalid");
      const raw = buffer.toString("utf8", 0, length);
      const envelope = z.object({ value: z.unknown(), mac: digest }).strict().parse(JSON.parse(raw));
      // Also rejects duplicate keys, whitespace variations and noncanonical encodings.
      if (raw !== canonical(envelope)) refuse("maintenance_state_invalid");
      const expected = Buffer.from(this.mac(envelope.value), "hex");
      if (!timingSafeEqual(expected, Buffer.from(envelope.mac, "hex"))) refuse("maintenance_state_invalid");
      return envelope.value;
    } finally { await handle.close(); }
  }
  private async sync(dir: string): Promise<void> {
    const handle = await fs.open(dir, C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW);
    try { await handle.sync(); } finally { await handle.close(); }
  }
  private async write(dir: string, name: string, value: unknown, max: number): Promise<void> {
    const raw = canonical({ value, mac: this.mac(value) });
    if (Buffer.byteLength(raw) > max) refuse("maintenance_table_capacity");
    const temp = path.join(dir, ".tmp-" + randomUUID());
    const handle = await fs.open(temp, C.O_WRONLY | C.O_CREAT | C.O_EXCL | C.O_NOFOLLOW, 0o600);
    try { await handle.writeFile(raw); await handle.sync(); } finally { await handle.close(); }
    // A failed temp or rename is evidence: it is deliberately not removed.
    await fs.rename(temp, path.join(dir, name));
    await this.sync(dir);
    if (canonical(await this.read(path.join(dir, name), max)) !== canonical(value))
      refuse("maintenance_state_invalid");
  }
  private marker(table: Table): Marker {
    const history = index(table);
    if (history.length > CAPACITY) refuse("maintenance_table_capacity");
    return { version: 1, ledger: table.ledger, tableHash: hash(table), history };
  }
  private async save(root: string, table: Table): Promise<void> {
    const marker = this.marker(table);
    tableSchema.parse(table);
    this.validate(table, marker);
    for (const [value, max] of [[table, TABLE_BYTES], [marker, MARKER_BYTES]] as const)
      if (Buffer.byteLength(canonical({ value, mac: this.mac(value) })) > max) refuse("maintenance_table_capacity");
    // Crash between these replacements fails closed on the authenticated hash mismatch.
    await this.write(root, "complete.json", table, TABLE_BYTES);
    await this.write(root, "initialized.json", marker, MARKER_BYTES);
    await this.sync(path.dirname(root));
  }
  private validate(table: Table, marker: Marker): void {
    if (marker.ledger !== table.ledger || marker.tableHash !== hash(table) ||
      canonical(marker.history) !== canonical(index(table)) || new Set(marker.history).size !== marker.history.length)
      refuse("maintenance_state_invalid");
    const ids = new Set<string>(), outputs = new Map<string, string>(), nonces = new Set<string>();
    const holds = new Map<string, Cycle[]>();
    const sourceBindings = new Map<string, string>();
    for (const cycle of table.cycles) {
      const id = cycleKey(cycle.identity, cycle.config);
      if (ids.has(id)) refuse("maintenance_state_invalid");
      ids.add(id);
      const source = canonical([cycle.identity.run, cycle.identity.generation, cycle.identity.bareBasename, cycle.identity.originalSource]);
      const priorBinding = sourceBindings.get(source);
      if (priorBinding !== undefined && priorBinding !== hash(cycle.identity)) refuse("maintenance_state_invalid");
      sourceBindings.set(source, hash(cycle.identity));
      const hold = canonical([cycle.identity.run, cycle.identity.generation, cycle.identity.hold]);
      const siblings = holds.get(hold) ?? [];
      if (siblings.some(c => c.identity.repo !== cycle.identity.repo || c.identity.custodian !== cycle.identity.custodian ||
        c.identity.bareBasename !== cycle.identity.bareBasename || c.identity.origin !== cycle.identity.origin ||
        (c.identity.originalSource === cycle.identity.originalSource && hash(c.identity) !== hash(cycle.identity)))) refuse("maintenance_state_invalid");
      siblings.push(cycle); holds.set(hold, siblings);
      if (!Number.isSafeInteger(cycle.admittedAt + DAY) ||
        cycle.originalCycleExpiry !== cycle.admittedAt + DAY ||
        cycle.lastObserved < cycle.admittedAt ||
        cycle.reservedWork !== cycle.slots.length * SLOT ||
        canonical(cycle.reserved) !== canonical(multiply(quotas(cycle.config.decodedCap), cycle.slots.length)))
        refuse("maintenance_state_invalid");
      let prior = cycle.admittedAt - SPACING;
      for (const [i, slot] of cycle.slots.entries()) {
        if (nonces.has(slot.nonce) || slot.ordinal !== i + 1 || slot.start < prior + SPACING || slot.start < cycle.admittedAt ||
          slot.start > cycle.lastObserved || slot.deadline <= slot.start ||
          slot.deadline > Math.min(slot.start + SLOT, cycle.originalCycleExpiry) ||
          slot.nextStart !== slot.start + SPACING || !Number.isSafeInteger(slot.nextStart) ||
          canonical(slot.reserved) !== canonical(quotas(cycle.config.decodedCap)))
          refuse("maintenance_state_invalid");
        nonces.add(slot.nonce);
        prior = slot.start;
      }
      if (new Set(cycle.outputs).size !== cycle.outputs.length) refuse("maintenance_state_invalid");
      for (const output of cycle.outputs) {
        // An output has one original producer; it cannot be used to mint another input.
        if ((outputs.has(output) && outputs.get(output) !== hash(cycle.identity)) || output === cycle.identity.originalSource) refuse("maintenance_state_invalid");
        outputs.set(output, hash(cycle.identity));
      }
    }
    for (const cycle of table.cycles)
      if (outputs.has(cycle.identity.originalSource)) refuse("maintenance_state_invalid");
  }
  private async load(root: string, initialize = true): Promise<Table> {
    let stat;
    try { stat = await fs.lstat(root); }
    catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      if (!initialize) refuse("maintenance_state_invalid");
      const stage = root + ".initializing";
      // Exclusive deterministic mkdir preserves any interrupted initialization evidence.
      await fs.mkdir(stage, { mode: 0o700 });
      await this.sync(stage);
      await this.sync(path.dirname(root));
      await this.options.initializationHook?.("mkdir");
      const table: Table = { version: 1, ledger: randomUUID(), complete: true, cycles: [] };
      await this.write(stage, "complete.json", table, TABLE_BYTES);
      await this.options.initializationHook?.("table");
      await this.write(stage, "initialized.json", this.marker(table), MARKER_BYTES);
      await this.options.initializationHook?.("pair");
      await fs.rename(stage, root);
      await this.sync(path.dirname(root));
      await this.options.initializationHook?.("installed");
      return this.load(root);
    }
    try {
      await fs.lstat(root + ".initializing");
      refuse("maintenance_state_invalid");
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
    if (!stat.isDirectory() || stat.isSymbolicLink()) refuse("maintenance_state_invalid");
    const names: string[] = [];
    const directory = await fs.opendir(root);
    for await (const entry of directory) {
      names.push(entry.name);
      if (names.length > 2) refuse("maintenance_state_invalid");
    }
    names.sort();
    if (canonical(names) !== canonical(["complete.json", "initialized.json"])) refuse("maintenance_state_invalid");
    const table = tableSchema.parse(await this.read(path.join(root, "complete.json"), TABLE_BYTES));
    const marker = markerSchema.parse(await this.read(path.join(root, "initialized.json"), MARKER_BYTES));
    this.validate(table, marker);
    return table;
  }
  private async transaction<T>(action: (root: string, table: Table) => Promise<MaintenanceResult<T>>, initialize = true): Promise<MaintenanceResult<T>> {
    try {
      const root = await this.root();
      return await locked(root, async () => {
        try { return await action(root, await this.load(root, initialize)); }
        catch (error) { return deny(error instanceof MaintenanceRefusal ? error.reason : "maintenance_state_invalid"); }
      });
    } catch (error) { return deny(error instanceof MaintenanceRefusal ? error.reason : "maintenance_state_invalid"); }
  }
  /**
   * Read-only evidence routing from an authenticated output index to its original
   * identity. No content/eligibility proof, clock observation, allowance or token:
   * callers must freshly prove source/config/output continuity and admit separately.
   * Exhausted/expired history remains inspectable; absent/partial history is denied.
   */
  async resolveOutput(
    outputDigest: string,
    expected: Omit<MaintenanceIdentity, "originalSource" | "sourceConfig">,
  ): Promise<MaintenanceResult<MaintenanceIdentity>> {
    return this.transaction(async (_root, table) => {
      const binding = identitySchema.omit({ originalSource: true, sourceConfig: true });
      if (!digest.safeParse(outputDigest).success || !binding.safeParse(expected).success)
        return deny("maintenance_identity_invalid");
      // The authenticated table bounds this scan to CAPACITY cycles/outputs and
      // validate rejects conflicting producers before any matching entry is used.
      const matches = table.cycles.filter(cycle => cycle.outputs.includes(outputDigest));
      if (!matches.length) return deny("maintenance_lineage_invalid");
      const identity = matches[0]!.identity;
      if (matches.some(cycle => canonical(cycle.identity) !== canonical(identity)))
        return deny("maintenance_lineage_invalid");
      const { originalSource: _source, sourceConfig: _config, ...actual } = identity;
      if (canonical(actual) !== canonical(expected)) return deny("maintenance_identity_invalid");
      return { ok: true, value: structuredClone(identity) };
    }, false);
  }
  private eligible(input: MaintenanceAdmission): void {
    if (!identitySchema.safeParse(input.identity).success || !configSchema.safeParse(input.config).success ||
      !digest.safeParse(input.source).success) refuse("maintenance_identity_invalid");
    if (input.config.verifierVersion !== MAINTENANCE_VERIFIER_VERSION || input.exactCustody !== true || input.terminalNonStopped !== true) refuse("maintenance_ineligible");
  }
  private observe(cycle: Cycle, now: number): void {
    if (now < cycle.lastObserved) refuse("maintenance_clock_invalid");
    if (now >= cycle.originalCycleExpiry) refuse("maintenance_cycle_expired");
    cycle.lastObserved = now;
  }
  async admit(input: MaintenanceAdmission, options: { deadline?: number; signal?: AbortSignal; newOrigin?: true } = {}): Promise<MaintenanceResult<MaintenanceSlot>> {
    // Metadata discovery cannot initialize storage, precharge work or mint a permit.
    if (discovery.getStore()) return deny("maintenance_discovery_only");
    return this.transaction(async (root, table) => {
      this.eligible(input);
      const monotonicNow = this.options.monotonicNow ?? (() => performance.now());
      const startedMonotonic = monotonicNow();
      if (!Number.isFinite(startedMonotonic) || startedMonotonic < 0) return deny("maintenance_clock_invalid");
      const now = this.now(), id = hash(input.identity), key = cycleKey(input.identity, input.config);
      const lineage = table.cycles.filter(c => hash(c.identity) === id);
      if (lineage.some(c => now < c.lastObserved)) return deny("maintenance_clock_invalid");
      // Preserve forward observations even on cap/expiry refusal; another configuration
      // or later rollback must not forget them. This never rebases any expiry.
      if (lineage.length) {
        for (const c of lineage) c.lastObserved = now;
        await this.save(root, table);
      }
      let cycle = table.cycles.find(c => cycleKey(c.identity, c.config) === key);
      if (!cycle) {
        if (lineage.length) {
          const version = Math.max(...lineage.map(c => c.config.verifierVersion));
          const cap = Math.max(...lineage.filter(c => c.config.verifierVersion === version).map(c => c.config.decodedCap));
          if (input.config.verifierVersion < version ||
            (input.config.verifierVersion === version && input.config.decodedCap <= cap))
            return deny("maintenance_config_changed");
        }
        if (table.cycles.some(c => c.identity.run === input.identity.run &&
          c.identity.generation === input.identity.generation && c.identity.bareBasename === input.identity.bareBasename &&
          c.identity.originalSource === input.identity.originalSource &&
          (c.identity.hold !== input.identity.hold || c.identity.repo !== input.identity.repo ||
           c.identity.custodian !== input.identity.custodian || c.identity.origin !== input.identity.origin)))
          return deny("maintenance_identity_invalid");
        const hold = table.cycles.filter(c => c.identity.run === input.identity.run &&
          c.identity.generation === input.identity.generation && c.identity.hold === input.identity.hold);
        if (hold.some(c => c.identity.repo !== input.identity.repo || c.identity.custodian !== input.identity.custodian ||
          c.identity.bareBasename !== input.identity.bareBasename || c.identity.origin !== input.identity.origin))
          return deny("maintenance_identity_invalid");
        if (hold.some(c => c.identity.originalSource === input.identity.originalSource && hash(c.identity) !== id))
          return deny("maintenance_config_changed");
        if (table.cycles.some(c => c.outputs.includes(input.identity.originalSource)))
          return deny("maintenance_lineage_invalid");
        // A changed tip cannot erase unresolved mutation provenance in this custody.
        // Only an explicit positively verified nonmutating outcome resolves it.
        if (hold.some(c => c.fenced))
          return deny("maintenance_mutation_fenced");
        if (input.source !== input.identity.originalSource && !table.cycles.some(c => hash(c.identity) === id && c.outputs.includes(input.source)))
          return deny("maintenance_lineage_invalid");
        if (!Number.isSafeInteger(now + DAY)) return deny("maintenance_clock_invalid");
        cycle = { identity: structuredClone(input.identity), config: structuredClone(input.config),
          admittedAt: now, originalCycleExpiry: now + DAY, lastObserved: now, slots: [],
          reservedWork: 0, reserved: multiply(quotas(input.config.decodedCap), 0),
          outputs: [], exhausted: false, fenced: false };
        table.cycles.push(cycle);
      }
      this.observe(cycle, now);
      // Check prospective history before any durable write.
      this.marker(table);
      if (input.source !== cycle.identity.originalSource && !table.cycles.some(c => hash(c.identity) === id && c.outputs.includes(input.source)))
        return deny("maintenance_lineage_invalid");
      if (cycle.exhausted || cycle.slots.length >= 3) return deny("maintenance_cycle_exhausted");
      // Initial publication cannot replace an indexed origin whose journal is missing.
      // Continuations use authenticated output continuity through the existing API.
      if (options.newOrigin && lineage.some(c => c.outputs.length > 0))
        return deny("maintenance_lineage_invalid");
      const priorLive = active.get(root)?.get(id);
      if (priorLive) priorLive.lastObserved = Math.max(priorLive.lastObserved, now);
      if (priorLive && (now >= priorLive.deadline || priorLive.signal?.aborted)) {
        priorLive.revoked = true;
        priorLive.controller.abort(new Error("maintenance_token_invalid"));
      }
      if (active.get(root)?.has(id)) return deny("maintenance_slot_active");
      if (lineage.some(c => cycleKey(c.identity, c.config) !== key && c.fenced))
        return deny("maintenance_mutation_fenced");
      if (cycle.slots.length && now < cycle.slots[cycle.slots.length - 1]!.nextStart)
        return deny("maintenance_slot_spacing");
      if (options.signal?.aborted) return deny("maintenance_ineligible");
      if (options.deadline !== undefined && (!Number.isSafeInteger(options.deadline) || options.deadline <= now))
        return deny("maintenance_clock_invalid");
      const deadline = Math.min(now + SLOT, cycle.originalCycleExpiry, options.deadline ?? Infinity);
      const slot = { nonce: randomUUID(), ordinal: cycle.slots.length + 1, start: now,
        deadline, nextStart: now + SPACING, reserved: quotas(cycle.config.decodedCap) };
      if (!Number.isSafeInteger(slot.nextStart)) return deny("maintenance_clock_invalid");
      cycle.fenced = true;
      cycle.slots.push(slot);
      cycle.reservedWork += SLOT;
      cycle.reserved = multiply(slot.reserved, cycle.slots.length);
      this.marker(table); // Capacity check before the slot precharge.
      await this.options.prechargeHook?.("before");
      await this.save(root, table);
      await this.options.prechargeHook?.("after");
      const observedAfter = this.now();
      if (observedAfter < now) return deny("maintenance_clock_invalid");
      // This observation is part of admission even when it refuses the charged slot.
      // Persist it before returning, so rollback cannot reopen an observed expiry.
      if (observedAfter > now) {
        for (const sibling of table.cycles) if (hash(sibling.identity) === id) sibling.lastObserved = observedAfter;
        await this.save(root, table);
      }
      const monotonicDeadline = startedMonotonic + deadline - now;
      const finishedMonotonic = monotonicNow();
      if (!Number.isFinite(finishedMonotonic) || finishedMonotonic < startedMonotonic) return deny("maintenance_clock_invalid");
      if (observedAfter >= deadline || finishedMonotonic >= monotonicDeadline || options.signal?.aborted)
        return deny("maintenance_token_invalid");
      const token = Object.freeze({ ordinal: slot.ordinal, admittedAt: cycle.admittedAt,
        originalCycleExpiry: cycle.originalCycleExpiry, start: now, deadline });
      const live: Live = { root, cycle: key, nonce: slot.nonce, deadline, now: () => this.now(),
        monotonicDeadline, monotonicNow,
        verified: false, verification: {}, source: input.source, revoked: false, finished: false, lastObserved: observedAfter, used: multiply(slot.reserved, 0), limits: slot.reserved, signal: options.signal, controller: new AbortController() };
      live.signal = AbortSignal.any([live.controller.signal, ...(options.signal ? [options.signal] : [])]);
      live.timer = setTimeout(() => live.controller.abort(new Error("maintenance deadline exhausted")), Math.max(0, live.monotonicDeadline - live.monotonicNow()));
      live.timer.unref();
      tokens.set(token, live);
      const running = active.get(root) ?? new Map<string, Live>();
      running.set(id, live); active.set(root, running);
      return { ok: true, value: token };
    });
  }
  /** Index proposed outputs before effects using the fence already persisted with the charge. */
  async beforeMutation(token: MaintenanceSlot, outputs: readonly string[]): Promise<MaintenanceResult<void>> {
    return this.transaction(async (root, table) => {
      const live = checked(token);
      if (!live.verified) return deny("maintenance_ineligible");
      const verification = live.verification;
      if (outputs.length > CAPACITY) return deny("maintenance_table_capacity");
      if (live.root !== root) return deny("maintenance_token_invalid");
      const cycle = table.cycles.find(c => cycleKey(c.identity, c.config) === live.cycle);
      if (!cycle || cycle.slots.at(-1)?.nonce !== live.nonce) return deny("maintenance_token_invalid");
      this.observe(cycle, this.now());
      if (active.get(root)?.get(hash(cycle.identity)) !== live) return deny("maintenance_token_invalid");
      for (const output of outputs) {
        if (!digest.safeParse(output).success || table.cycles.some(c =>
          c.identity.originalSource === output || (hash(c.identity) !== hash(cycle.identity) && c.outputs.includes(output))))
          return deny("maintenance_lineage_invalid");
        if (!cycle.outputs.includes(output)) cycle.outputs.push(output);
      }
      cycle.fenced = true;
      await this.save(root, table);
      checked(token);
      // A refresh can invalidate proof while output persistence is in flight.
      if (!live.verified || live.verification !== verification) return deny("maintenance_ineligible");
      return { ok: true, value: undefined };
    }, false);
  }
  /** Bind fresh positive proof to this exact live reservation; never clear durable uncertainty. */
  async revalidate(token: MaintenanceSlot, input: MaintenanceEligibility): Promise<MaintenanceResult<void>> {
    // Invalidate synchronously, including failures before the transaction callback.
    // One live ticket bounds state; older completions cannot publish or erase later proof.
    const reservation = tokens.get(token), verification = {};
    if (reservation) { reservation.verified = false; reservation.verification = verification; }
    return this.transaction(async (root, table) => {
      const live = checked(token);
      this.eligible(input);
      if (input.quiescent !== true || input.sourceOutputContinuity !== true)
        return deny("maintenance_ineligible");
      if (live.root !== root) return deny("maintenance_token_invalid");
      const cycle = table.cycles.find(c => cycleKey(c.identity, c.config) === live.cycle);
      if (!cycle || cycle.slots.at(-1)?.nonce !== live.nonce ||
        active.get(root)?.get(hash(cycle.identity)) !== live) return deny("maintenance_token_invalid");
      if (canonical(input.identity) !== canonical(cycle.identity)) return deny("maintenance_identity_invalid");
      if (canonical(input.config) !== canonical(cycle.config)) return deny("maintenance_config_changed");
      if (input.source !== live.source || (input.source !== cycle.identity.originalSource &&
        !table.cycles.some(c => hash(c.identity) === hash(cycle.identity) && c.outputs.includes(input.source))))
        return deny("maintenance_lineage_invalid");
      const now = this.now();
      const siblings = table.cycles.filter(c => hash(c.identity) === hash(cycle.identity));
      if (siblings.some(c => now < c.lastObserved)) return deny("maintenance_clock_invalid");
      this.observe(cycle, now);
      await this.save(root, table);
      checked(token); // Persistence time is charged; it cannot revive a timed out proof.
      if (live.verification !== verification) return deny("maintenance_ineligible");
      live.verified = true;
      return { ok: true, value: undefined };
    }, false);
  }
  /**
   * Call only after all owned work has joined. Revoked tokens may finish exactly once.
   * Both nonmutating outcomes assert positive actual no-source-effect knowledge;
   * permanent_nonmutating also ends this configuration cycle after a known refusal.
   * They require the positive proof already bound by revalidate, even after revocation.
   */
  async finish(token: MaintenanceSlot, outcome: "transient" | "complete" | "permanent" | "nonmutating" | "permanent_nonmutating" = "complete"): Promise<MaintenanceResult<void>> {
    const live = tokens.get(token);
    if (!live || live.finished) return deny("maintenance_token_invalid");
    if (live.timer) clearTimeout(live.timer);
    const verified = live.verified;
    live.verified = false;
    live.finished = true;
    live.revoked = true;
    live.controller.abort(new Error("maintenance_token_invalid"));
    // Joined bookkeeping is bounded by the authenticated table format; it grants
    // no production and must still persist after the work scope has been revoked.
    try {
      return await resources.exit(() => discovery.exit(() => this.transaction(async (root, table) => {
        if (root !== live.root) return deny("maintenance_token_invalid");
        const cycle = table.cycles.find(c => cycleKey(c.identity, c.config) === live.cycle);
        if (!cycle || !cycle.slots.some(s => s.nonce === live.nonce)) return deny("maintenance_token_invalid");
        // Caller joins all owned work before finish; nonmutating requires positive proof.
        if (outcome === "nonmutating" || outcome === "permanent_nonmutating") {
          if (!verified) return deny("maintenance_ineligible");
          cycle.fenced = false;
        }
        if (outcome === "complete" || outcome === "permanent" || outcome === "permanent_nonmutating") cycle.exhausted = true;
        await this.save(root, table);
        return { ok: true, value: undefined };
      }, false)));
    } finally {
      const running = active.get(live.root);
      // Retain the reservation through settlement, then release only this joined owner.
      // At most CAPACITY entries; a failed write still releases the owner without refund.
      for (const [id, reservation] of running ?? []) if (reservation === live) running!.delete(id);
    }
  }
}
function checked(token: MaintenanceSlot): Live {
  const live = tokens.get(token);
  if (!live || live.revoked) refuse("maintenance_token_invalid");
  try { MaintenanceBudget.current()?.check(); }
  catch (error) {
    live.revoked = true;
    live.controller.abort(error);
    throw error;
  }
  let now: number;
  try { now = live.now(); }
  catch (error) {
    live.revoked = true;
    live.controller.abort(error);
    throw error;
  }
  if (now < live.lastObserved) {
    live.revoked = true;
    live.controller.abort(new Error("maintenance_clock_invalid"));
    refuse("maintenance_clock_invalid");
  }
  live.lastObserved = now;
  if (now >= live.deadline || live.monotonicNow() >= live.monotonicDeadline || live.signal?.aborted) {
    live.revoked = true;
    live.controller.abort(new Error("maintenance_token_invalid"));
    refuse("maintenance_token_invalid");
  }
  return live;
}
interface Discovery {
  deadline: number; monotonicDeadline: number; signal: AbortSignal; controller: AbortController;
  used: Partial<Record<MaintenanceResource | "candidates", number>>;
}
const discovery = new AsyncLocalStorage<Discovery>();
const discoveryLimits: Partial<Record<MaintenanceResource | "candidates", number>> = {
  metadataBytes: 32 * 1024 ** 2, adminEntries: 4096, rpcAttempts: 16, candidates: 4, launches: 512,
};
function checkDiscovery(scope: Discovery): void {
  try { MaintenanceBudget.current()?.check(); }
  catch (error) { scope.controller.abort(error); throw error; }
  if (scope.signal.aborted || Date.now() >= scope.deadline || performance.now() >= scope.monotonicDeadline) {
    scope.controller.abort(new Error("maintenance_resource_exhausted"));
    refuse("maintenance_resource_exhausted");
  }
}
/** Separate producer resource scope. Nested fallback retains the original token and counters. */
export class MaintenanceResources {
  static run<T>(token: MaintenanceSlot, action: () => T): T {
    if (discovery.getStore()) refuse("maintenance_token_invalid");
    const current = resources.getStore() ?? token;
    checked(current);
    return resources.run(current, action);
  }
  static check(): void {
    const token = resources.getStore();
    if (!token) refuse("maintenance_token_invalid");
    checked(token);
  }
  /** Metadata discovery never mints or accepts a producer token. Nested calls share all counters. */
  static async runDiscovery<T>(action: () => Promise<T>, options: { signal?: AbortSignal; deadline?: number } = {}): Promise<T> {
    if (resources.getStore()) refuse("maintenance_token_invalid");
    const parent = discovery.getStore();
    if (parent) checkDiscovery(parent);
    const duration = Math.max(0, Math.min(10_000, (options.deadline ?? Infinity) - Date.now(),
      parent ? parent.deadline - Date.now() : Infinity));
    return MaintenanceBudget.runProducer(async budget => {
      const scope: Discovery = { deadline: Math.min(Date.now() + duration, parent?.deadline ?? Infinity),
        monotonicDeadline: Math.min(performance.now() + duration, parent?.monotonicDeadline ?? Infinity),
        signal: budget.signal, controller: parent?.controller ?? new AbortController(), used: parent?.used ?? {} };
      scope.signal = AbortSignal.any([budget.signal, scope.controller.signal, ...(parent ? [parent.signal] : [])]);
      return discovery.run(scope, async () => {
        checkDiscovery(scope);
        const value = await action();
        checkDiscovery(scope);
        return value;
      });
    }, { durationMs: duration, signal: options.signal });
  }
  static active(): boolean { return !!resources.getStore() || !!discovery.getStore(); }
  /** Producer pagination must not borrow a charged production slot. */
  static discoveryActive(): boolean { return !!discovery.getStore() && !resources.getStore(); }
  static remainingMsIfActive(): number {
    const token = resources.getStore();
    if (token) {
      const live = checked(token);
      return Math.max(0, Math.min(live.deadline - live.now(), live.monotonicDeadline - live.monotonicNow()));
    }
    const scope = discovery.getStore();
    return scope ? Math.max(0, Math.min(scope.deadline - Date.now(), scope.monotonicDeadline - performance.now())) : Infinity;
  }
  static signalIfActive(): AbortSignal | undefined {
    const token = resources.getStore();
    return token ? tokens.get(token)?.signal : discovery.getStore()?.signal;
  }
  static checkIfActive(): void {
    if (resources.getStore()) this.check();
    const scope = discovery.getStore();
    if (scope) checkDiscovery(scope);
  }
  static consumeIfActive(resource: MaintenanceResource | "candidates", amount: number): void {
    if (resources.getStore()) {
      if (resource === "candidates") return;
      this.consume(resource, amount);
    }
    const scope = discovery.getStore();
    if (!scope) return;
    checkDiscovery(scope);
    const limit = discoveryLimits[resource];
    const used = scope.used[resource] ?? 0;
    if (limit === undefined || !Number.isSafeInteger(amount) || amount < 0 || amount > limit - used) {
      scope.controller.abort(new Error("maintenance_resource_exhausted"));
      refuse("maintenance_resource_exhausted");
    }
    scope.used[resource] = used + amount;
  }
  /** Count raw nonempty rows before callers allocate arrays or interpret metadata. */
  static consumeMetadataEntriesIfActive(value: string, separator: "\0" | "\n"): void {
    if (!this.active()) return;
    this.checkIfActive();
    for (let start = 0; start < value.length;) {
      const end = value.indexOf(separator, start);
      if (end !== start) this.consumeIfActive("adminEntries", 1);
      if (end < 0) break;
      start = end + 1;
    }
    this.checkIfActive();
  }
  /** Charge before delivery/launch/RPC. A failed charge never delivers partial allowance. */
  static consume(resource: MaintenanceResource, amount: number): void {
    const token = resources.getStore();
    if (!token) refuse("maintenance_token_invalid");
    const live = checked(token);
    if (!Object.hasOwn(live.limits, resource) || !Number.isSafeInteger(amount) || amount < 0 ||
      amount > live.limits[resource] - live.used[resource]) {
      live.revoked = true;
      live.controller.abort(new Error("maintenance_resource_exhausted"));
      refuse("maintenance_resource_exhausted");
    }
    live.used[resource] += amount;
  }
}
