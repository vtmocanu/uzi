import { MaintenanceResources } from "./recovery-maintenance.js";
import { createHash } from "node:crypto";
import type { Readable } from "node:stream";

export const RECOVERY_DECODED_LIMIT = 8 * 1024 ** 3;
const RECOVERY_DECODED_LIMIT_CEILING = 64 * 1024 ** 3;
const limitContract = "UZI_RECOVERY_DECODED_LIMIT_BYTES must be decimal bytes in the range 1..68719476736";

export function validateRecoveryDecodedLimitBytes(value: number): number {
  if (!Number.isSafeInteger(value) || value < 1 || value > RECOVERY_DECODED_LIMIT_CEILING) {
    throw new Error(limitContract);
  }
  return value;
}

export function parseRecoveryDecodedLimitBytes(value: string | undefined): number {
  if (value === undefined) return RECOVERY_DECODED_LIMIT;
  const trimmed = value.trim();
  if (!/^[0-9]+$/.test(trimmed)) throw new Error(limitContract);
  return validateRecoveryDecodedLimitBytes(Number(trimmed));
}

export function formatRecoveryDecodedLimit(limit: number): string {
  const gib = limit / 1024 ** 3;
  return `${limit} bytes${gib >= 1 ? ` (${gib} GiB)` : ""}`;
}

export function recoveryClosureLimitDetail(limit: number): string {
  return `reachable recovery history exceeds the ${formatRecoveryDecodedLimit(limit)} decoded verification limit`;
}

export class RecoveryClosureLimitError extends Error {
  constructor(readonly limit: number = RECOVERY_DECODED_LIMIT) {
    super(recoveryClosureLimitDetail(limit));
  }
}
export interface ClosureBudget { delivered: number; limit: number }
export interface ClosureObject { oid: string; type: string; size: number }

/** Strict batch framing; only a pipe chunk and a bounded header are retained. */
export async function verifyClosureFrames(
  stream: Readable, objects: readonly ClosureObject[], budget: ClosureBudget, signal?: AbortSignal,
): Promise<void> {
  // Refuse object production in metadata discovery even for empty batches.
  MaintenanceResources.consumeIfActive("decoded", 0);
  const iterator = stream[Symbol.asyncIterator]();
  let chunk: Buffer = Buffer.alloc(0);
  let offset = 0;
  const available = async (): Promise<boolean> => {
    signal?.throwIfAborted();
    if (offset < chunk.length) return true;
    const next = await iterator.next();
    signal?.throwIfAborted();
    if (next.done) return false;
    chunk = Buffer.isBuffer(next.value) ? next.value : Buffer.from(next.value);
    offset = 0;
    return chunk.length > 0 || available();
  };
  const byte = async (): Promise<number> => {
    if (!await available()) throw new Error("truncated recovery object frame");
    MaintenanceResources.consumeIfActive("metadataBytes", 1);
    return chunk[offset++]!;
  };
  for (const object of objects) {
    let header = "";
    for (;;) {
      const c = await byte();
      if (c === 10) break;
      if (header.length >= 100 || c < 32 || c > 126) throw new Error("invalid recovery object header");
      header += String.fromCharCode(c);
    }
    if (header !== `${object.oid} ${object.type} ${object.size}`) throw new Error("unexpected recovery object header");
    const hash = createHash("sha1").update(`${object.type} ${object.size}\0`);
    let remaining = object.size;
    while (remaining > 0) {
      if (!await available()) throw new Error("truncated recovery object contents");
      const length = Math.min(remaining, chunk.length - offset);
      if (length > budget.limit - budget.delivered) throw new RecoveryClosureLimitError(budget.limit);
      MaintenanceResources.consumeIfActive("decoded", length);
      budget.delivered += length;
      hash.update(chunk.subarray(offset, offset + length));
      offset += length;
      remaining -= length;
    }
    if (await byte() !== 10) throw new Error("invalid recovery object separator");
    if (hash.digest("hex") !== object.oid) throw new Error("recovery object hash mismatch");
  }
  if (await available()) throw new Error("extra recovery object output");
  signal?.throwIfAborted();
}
