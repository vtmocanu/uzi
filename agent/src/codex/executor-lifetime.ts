import { randomUUID } from "node:crypto";
import { TrustedExecutionRefusal } from "../trusted-execution-refusal.js";

declare const terminalSealBrand: unique symbol;
/**
 * Worker-private, runtime-issued evidence. The value itself carries no data: every fact
 * lives in module-private state keyed by this exact object, so a structurally similar
 * object, a JSON copy, a label or a saved id can never stand in for it.
 */
export type ExecutorTerminalSeal = { readonly [terminalSealBrand]: true };
export type ExecutorSealResult =
  | { readonly kind: "sealed"; readonly seal: ExecutorTerminalSeal }
  | { readonly kind: "unproven" };

/** Random per-incarnation identities captured at seal time, for M2 publication digests. */
export interface ExecutorSealMaterial {
  readonly sourceUse: object;
  readonly executorIncarnation: string;
  /** One entry per inventoried registry, in construction order. */
  readonly incarnations: readonly { readonly registry: string; readonly facade: string }[];
}

interface SealState { readonly lifetime: ExecutorLifetime; readonly sourceUse: object; readonly material: ExecutorSealMaterial }
const sealStates = new WeakMap<object, SealState>();

/** Read the digest material of a seal this module issued; undefined for anything else. */
export function terminalSealMaterial(seal: unknown): ExecutorSealMaterial | undefined {
  return typeof seal === "object" && seal !== null ? sealStates.get(seal)?.material : undefined;
}

/**
 * Whether the lifetime that issued `seal` still accepts it for `sourceUse`, answered from the
 * seal's own recorded state. A caller cannot substitute its own answer, so a seal revoked after
 * issue (distrust, a failing facade or registry check, a late tail) is never accepted.
 */
export function terminalSealStillAccepted(seal: unknown, sourceUse: object): boolean {
  if (typeof seal !== "object" || seal === null) return false;
  const state = sealStates.get(seal);
  return state !== undefined && state.lifetime.accepts(seal, sourceUse);
}

interface Incarnation {
  readonly id: string;
  facade?: { readonly id: string; readonly identity: object; readonly check: () => boolean };
  readonly check: () => boolean;
}

/**
 * One executor's irreversible admission fence. Monotonic: open, sealing, sealed. A failed
 * or unproven seal stays in `sealing` forever (permanently closed, never re-sealable).
 * Cleanup may retain already admitted work after closure; it cannot admit a new producer.
 * Settlement has no delivery timeout: `seal` awaits every retained operation until it
 * actually settles. Most tails carry their own bound, but some do not: the codex command
 * cache helper's settlement (HeldRunCommandCache.observeCleanup) ends only when the helper's
 * channel closes or its process exits. A holder that never closes keeps the seal pending,
 * and with it the flight's end-of-run publication (publishFlightSource awaits
 * sealLifetime) and the rest of that flight's source-use end and teardown, which run after
 * it. Nothing publishes or is released early: it fails closed. A failed sibling does not
 * skip any other tail. No execute/run
 * promise belongs in this owner. Nothing here may be awaited while holding a safety,
 * journal or bare lock.
 */
export class ExecutorLifetime {
  private phase: "open" | "sealing" | "sealed" = "open";
  private readonly tails = new Set<Promise<unknown>>();
  private readonly incarnations = new Map<object, Incarnation>();
  private readonly incarnationId = randomUUID();
  private failure = false;
  private completion?: Promise<ExecutorSealResult>;
  private sealedFor?: object;

  /**
   * This lifetime's executor incarnation: the value `terminalSealMaterial(...).executorIncarnation`
   * reports, so a source-use lease admitted with `{ executor: lifetime.executorIncarnation }`
   * matches the seal this lifetime later produces.
   */
  get executorIncarnation(): string { return this.incarnationId; }

  assertOpen(): void {
    if (this.phase !== "open") throw new TrustedExecutionRefusal("codex executor lifetime closed");
  }

  isOpen(): boolean { return this.phase === "open"; }

  /**
   * Refuse after closure BEFORE invoking `action`, then own the promise it returns. The phase
   * check is synchronous (no timing change for open callers) but the refusal is a rejected
   * promise, so fire-and-forget callers (`void x().catch(...)`) cannot be thrown past; a seal
   * that closes admission while the operation is in flight joins its actual settlement.
   */
  admit<T>(action: () => Promise<T>): Promise<T> {
    if (this.phase !== "open") return Promise.reject(new TrustedExecutionRefusal("codex executor lifetime closed"));
    let pending: Promise<T>;
    try { pending = action(); } catch (error) { pending = Promise.reject(error); }
    this.track(pending);
    return pending;
  }

  /** Cleanup only: retain the actual operation, never a bounded delivery wrapper. A
   *  rejected cleanup is uncertainty and forbids a seal. */
  retain<T>(pending: Promise<T>): Promise<T> {
    this.track(pending);
    void pending.catch(() => this.distrust());
    return pending;
  }

  /** Join an already-owned operation whose rejection is its own caller's business (a failed
   *  setup is cleaned and judged through the registry facts, not by its rejection). */
  join<T>(pending: Promise<T>): Promise<T> {
    this.track(pending);
    return pending;
  }

  private track(pending: Promise<unknown>): void {
    this.tails.add(pending);
    const done = (): void => { this.tails.delete(pending); };
    void pending.then(done, done);
  }

  distrust(): void { this.failure = true; }

  inventory(identity: object, check: () => boolean): void {
    this.assertOpen();
    if (this.incarnations.has(identity)) throw new Error("duplicate Codex incarnation");
    this.incarnations.set(identity, { id: randomUUID(), check });
  }

  bindFacade(identity: object, facade: object, check: () => boolean): void {
    const entry = this.incarnations.get(identity);
    if (!entry || (entry.facade !== undefined && entry.facade.identity !== facade)) {
      this.distrust();
      throw new Error("unknown Codex facade incarnation");
    }
    entry.facade = { id: randomUUID(), identity: facade, check };
  }

  /**
   * sourceUse is an opaque caller-owned identity (M2 binds the SourceUseLease), not a
   * run-id/path/SHA assertion; the SAME object must be passed to `accepts`. This seam
   * does not authorize publication. `settle` starts cleanup the owner itself must drive;
   * `eligible` is the owner's own terminal predicate, read before and after the joins.
   */
  seal(sourceUse: object, settle: () => Promise<void>, eligible: () => boolean): Promise<ExecutorSealResult> {
    if (this.completion) {
      // A repeat call is proof only for the identical source use.
      return sourceUse === this.sealedFor ? this.completion : this.completion.then(() => ({ kind: "unproven" } as const));
    }
    this.sealedFor = sourceUse;
    this.phase = "sealing"; // irrevocable, before any callback or await
    if (typeof sourceUse !== "object" || sourceUse === null) this.distrust();
    let initiallyEligible = false;
    try { initiallyEligible = eligible(); } catch { this.distrust(); }
    this.completion = Promise.resolve().then(async () => {
      try { await settle(); } catch { this.distrust(); }
      // Closed admission bounds the producer set. Cleanup may append finite descendants;
      // join those too.
      while (this.tails.size) await Promise.allSettled(this.tails);
      let finallyEligible = false;
      try { finallyEligible = eligible(); } catch { this.distrust(); }
      if (!initiallyEligible || !finallyEligible || !this.proven()) return { kind: "unproven" } as const;
      const seal = Object.freeze({}) as ExecutorTerminalSeal;
      const material: ExecutorSealMaterial = Object.freeze({
        sourceUse,
        executorIncarnation: this.incarnationId,
        incarnations: Object.freeze([...this.incarnations.values()].map(entry =>
          Object.freeze({ registry: entry.id, facade: entry.facade!.id }))),
      });
      sealStates.set(seal, { lifetime: this, sourceUse, material });
      this.phase = "sealed";
      return { kind: "sealed", seal } as const;
    });
    return this.completion;
  }

  /** Exact, still-current proof: same lifetime, same source use, nothing revoked since. */
  accepts(seal: unknown, sourceUse: object): seal is ExecutorTerminalSeal {
    if (typeof seal !== "object" || seal === null || this.phase !== "sealed") return false;
    const state = sealStates.get(seal);
    return state !== undefined && state.lifetime === this && state.sourceUse === sourceUse &&
      this.tails.size === 0 && this.proven();
  }

  private proven(): boolean {
    return !this.failure && [...this.incarnations.values()].every(entry =>
      entry.facade !== undefined && entry.facade.check() && entry.check());
  }
}
