import { createHash } from "node:crypto";
import type { Readable } from "node:stream";

export const RECOVERY_DECODED_LIMIT = 1024 * 1024 * 1024;
export class RecoveryClosureLimitError extends Error {
  constructor() { super("reachable recovery history exceeds the 1 GiB decoded verification limit"); }
}
export interface ClosureBudget { delivered: number; limit: number }
export interface ClosureObject { oid: string; type: string; size: number }

/** Strict batch framing; only a pipe chunk and a bounded header are retained. */
export async function verifyClosureFrames(
  stream: Readable, objects: readonly ClosureObject[], budget: ClosureBudget, signal?: AbortSignal,
): Promise<void> {
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
      if (length > budget.limit - budget.delivered) throw new RecoveryClosureLimitError();
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
