import { TrustedExecutionRefusal } from "../trusted-execution-refusal.js";

declare const terminalSealBrand: unique symbol;
/** Worker-private, runtime-issued evidence. Serializing this value loses its authority. */
export type ExecutorTerminalSeal = { readonly [terminalSealBrand]: true };
export type ExecutorSealResult =
  | { readonly kind: "sealed"; readonly seal: ExecutorTerminalSeal }
  | { readonly kind: "unproven" };

/**
 * One executor's irreversible admission fence. Cleanup may retain already admitted work
 * after closure; it cannot admit a new producer. Settlement has no delivery timeout:
 * each retained operation must terminate under its own original bound. A failed sibling
 * does not skip any other tail. No execute/run promise belongs in this owner.
 */
export class ExecutorLifetime {
  private phase: "open" | "sealing" | "sealed" = "open";
  private readonly tails = new Set<Promise<unknown>>();
  private readonly incarnations = new Map<object, { facade?: object; facadeCheck?: () => boolean; check: () => boolean }>();
  private readonly seals = new WeakMap<object, object>();
  private failure = false;
  private completion?: Promise<ExecutorSealResult>;

  assertOpen(): void {
    if (this.phase !== "open") throw new TrustedExecutionRefusal("codex executor lifetime closed");
  }

  isOpen(): boolean { return this.phase === "open"; }

  /** Install ownership before invoking a potentially synchronous factory. */
  admit<T>(action: () => Promise<T>): Promise<T> {
    this.assertOpen();
    const pending = Promise.resolve().then(() => {
      this.assertOpen();
      return action();
    });
    return this.retain(pending);
  }

  /** Cleanup only: retain the actual operation, never a bounded delivery wrapper. */
  retain<T>(pending: Promise<T>): Promise<T> {
    this.tails.add(pending);
    void pending.then(
      () => this.tails.delete(pending),
      () => this.tails.delete(pending),
    );
    return pending;
  }

  distrust(): void { this.failure = true; }

  inventory(identity: object, check: () => boolean): void {
    this.assertOpen();
    if (this.incarnations.has(identity)) throw new Error("duplicate Codex incarnation");
    this.incarnations.set(identity, { check });
  }

  bindFacade(identity: object, facade: object, check: () => boolean): void {
    const entry = this.incarnations.get(identity);
    if (!entry || (entry.facade !== undefined && entry.facade !== facade)) {
      this.distrust();
      throw new Error("unknown Codex facade incarnation");
    }
    entry.facade = facade;
    entry.facadeCheck = check;
  }

  /**
   * sourceUse is an opaque caller-owned identity, not a run-id/path/SHA assertion.
   * M2 must retain it in its own execution-tail/source owner and pass the SAME object
   * when validating. This seam does not authorize publication.
   */
  seal(sourceUse: object, settle: () => Promise<void>, eligible: () => boolean): Promise<ExecutorSealResult> {
    if (this.completion) return this.completion;
    this.phase = "sealing"; // irrevocable, before any callback or await
    if (typeof sourceUse !== "object" || sourceUse === null) this.distrust();
    const initiallyEligible = eligible();
    this.completion = Promise.resolve().then(async () => {
      try { await settle(); } catch { this.distrust(); }
      // Closed admission bounds the producer set. Cleanup may append finite descendants;
      // join those too, without holding any safety, capture, journal or bare lock.
      while (this.tails.size) await Promise.allSettled([...this.tails]);
      if (!initiallyEligible || !eligible() || !this.proven()) return { kind: "unproven" };
      const seal = Object.freeze({}) as ExecutorTerminalSeal;
      this.seals.set(seal, sourceUse);
      this.phase = "sealed";
      return { kind: "sealed", seal };
    });
    return this.completion;
  }

  accepts(seal: unknown, sourceUse: object): seal is ExecutorTerminalSeal {
    return typeof seal === "object" && seal !== null && this.phase === "sealed" &&
      this.seals.get(seal) === sourceUse && this.tails.size === 0 && this.proven();
  }

  private proven(): boolean {
    return !this.failure && [...this.incarnations.values()].every(entry =>
      entry.facade !== undefined && entry.facadeCheck?.() === true && entry.check());
  }
}
