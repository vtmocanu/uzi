// The run's current Anthropic credential (issue #4201). An in-place credential switch replaces the
// OAuth token mid-run, so every consumer that used to capture the claim's token once reads it from
// here at use time. A snapshot is immutable; only the holder advances, and only forward.

import { inspect } from "node:util";

export type CredentialSnapshot = Readonly<{ token: string; ordinal: number }>;

const REDACTED = "[redacted]";

/** A frozen snapshot whose token reads normally but is redacted when the object is serialized or
 *  inspected (non-enumerable toJSON and inspect hooks), so a stray log of it cannot leak the credential. */
function makeSnapshot(token: string, ordinal: number): CredentialSnapshot {
  const snap = { token, ordinal };
  Object.defineProperty(snap, "toJSON", { value: () => ({ ordinal, token: REDACTED }), enumerable: false });
  Object.defineProperty(snap, inspect.custom, { value: () => ({ ordinal, token: REDACTED }), enumerable: false });
  return Object.freeze(snap) as CredentialSnapshot;
}

export class CredentialHolder {
  #snapshot: CredentialSnapshot;

  /** Seeded with the claim's token at ordinal 0 (the base epoch). */
  constructor(seed: { token: string; ordinal: 0 }) {
    if (seed.token === "") throw new Error("credential holder needs a non-empty token");
    this.#snapshot = makeSnapshot(seed.token, seed.ordinal);
  }

  /** The current credential. The returned snapshot never changes; read again after a switch. */
  current(): CredentialSnapshot {
    return this.#snapshot;
  }

  /** Advance to a later credential epoch. Throws on an empty token or an ordinal that is not a
   *  safe integer strictly above the current one, leaving the current credential untouched. */
  publish(next: { token: string; ordinal: number }): void {
    if (typeof next.token !== "string" || next.token === "") throw new Error("credential holder needs a non-empty token");
    if (!Number.isSafeInteger(next.ordinal) || next.ordinal <= this.#snapshot.ordinal) {
      throw new Error(`credential ordinal ${next.ordinal} must be a safe integer above ${this.#snapshot.ordinal}`);
    }
    this.#snapshot = makeSnapshot(next.token, next.ordinal);
  }

  toJSON(): { ordinal: number; token: string } {
    return { ordinal: this.#snapshot.ordinal, token: REDACTED };
  }

  [inspect.custom](): { ordinal: number; token: string } {
    return this.toJSON();
  }
}
