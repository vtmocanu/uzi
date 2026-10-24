import { decodePolicyHoldAck, PolicyHoldUnsupported, type WorkerClient } from "./client.js";
import type { Logger } from "./log.js";
import type { Outbox } from "./outbox.js";
import type { PolicyHoldAck } from "./protocol.js";

type Observation = NonNullable<Awaited<ReturnType<Outbox["readPolicyForRetirement"]>>>;
export type PolicyResolveResult =
  | { kind: "busy" | "unavailable" | "unsupported" | "pending" }
  | { kind: "observed"; observation: Observation; ack: PolicyHoldAck };

const flights = new WeakMap<Outbox, Map<Readonly<object>, Promise<PolicyResolveResult>>>();

async function waitForPolicy(promise: Promise<PolicyResolveResult>, signal?: AbortSignal): Promise<PolicyResolveResult> {
  signal?.throwIfAborted();
  if (!signal) return promise;
  let abort!: () => void;
  const cancelled = new Promise<never>((_, reject) => {
    abort = () => reject(signal.reason);
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) abort();
  });
  try { return await Promise.race([promise, cancelled]); }
  finally { signal.removeEventListener("abort", abort); }
}

/** One authenticated observation and at most one bounded HTTP attempt. No deletion or execution
 * effects: even an exact ACK leaves source/custody retirement to the runner's production proof.
 * Heartbeat callers skip busy sends; the awaited boot gate joins their bounded attempt. */
export async function resolvePendingPolicy(
  deps: { outbox: Outbox; client: Pick<WorkerClient, "supportsPolicyHold" | "reportPolicyHold">; log: Logger },
  args: { runId: string; sourceGeneration: number; signal?: AbortSignal; waitForInFlight?: boolean },
): Promise<PolicyResolveResult> {
  const { outbox, client, log } = deps;
  args.signal?.throwIfAborted();
  let observation: Observation | undefined;
  try {
    observation = await outbox.readPolicyForRetirement(args.runId, args.sourceGeneration, { signal: args.signal });
  } catch {
    args.signal?.throwIfAborted();
    log.warn("policy hold observation unavailable; evidence retained");
    return { kind: "pending" };
  }
  if (!observation || observation.policy.server_settled) return { kind: "unavailable" };
  const key = observation.context.expectedIdentity;
  if (!key) return { kind: "unavailable" };
  let active = flights.get(outbox);
  if (!active) { active = new Map(); flights.set(outbox, active); }
  const previous = active.get(key);
  if (previous) return args.waitForInFlight ? waitForPolicy(previous, args.signal) : { kind: "busy" };
  const send = (async (): Promise<PolicyResolveResult> => {
  try {
    // Reauthentication happened before singleflight admission without an intervening await.
    const ack = decodePolicyHoldAck(await client.reportPolicyHold(args.runId, observation.request, args.signal), observation.request);
    args.signal?.throwIfAborted();
    if (ack.disposition === "unauthenticated_historical_outcome") {
      log.warn("policy historical outcome refused; evidence retained pending reconciliation");
    }
    if (ack.disposition !== "pending" && ack.disposition !== "unauthenticated_historical_outcome") {
      await outbox.markPolicyServerSettled(args.runId, args.sourceGeneration,
        { ...observation.context, signal: args.signal });
    }
    return { kind: "observed", observation, ack };
  } catch (err) {
    args.signal?.throwIfAborted();
    if (err instanceof PolicyHoldUnsupported) return { kind: "unsupported" };
    log.warn("policy hold delivery unresolved; evidence retained");
    return { kind: "pending" };
  } finally {
    active.delete(key);
  }
  })();
  active.set(key, send);
  return send;
}
