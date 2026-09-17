// PRD #1391 Run B M3b — the write-ahead terminal-report SEND path (D3/D4/D9/D11/D13).
//
// This module owns the CONSUMER half of the terminal-journal store: it journals a run-lane
// terminal outcome BEFORE the first network attempt, then drives it to a durable resolution over
// the /state endpoint. The store half (journalTerminal/readTerminalJournal/retire/staleRetire/
// markBlocked and the canonicaliser) lives in outbox.ts (M3a). The two entry points here:
//
//   journalAndResolveTerminal — the write-ahead helper each terminal SITE calls: canonicalise the
//   body ONCE, journal it (so the outcome is durable before it is first sent), then resolve it. On
//   `reserve_exhausted` (the terminal-sized reserve could not admit it) the outcome is sent
//   UNJOURNALED with an error log, never a silent drop (SC2).
//
//   resolvePendingTerminal — the REUSABLE resolve primitive (M4 also calls it at boot): read the
//   journal, send the canonical bytes (adding the `messages_through_seq` fence ONLY when the api
//   advertised `terminal_fence`, D9), and classify the ack:
//     - 200, or a 409 whose returned status is terminal → retire (the transition landed);
//     - 409 stale_claim → local stale-retire (D11: increments stale_retired, NO server write,
//       NEVER rebinds to a newer generation);
//     - 409 messages_pending → run the gap-fill loop, then re-send;
//     - 409 gap_unrecoverable → mark the journal blocked (D13);
//     - any other non-terminal 409 (running/claimed/paused/completion-permit mismatch) or an
//       unreadable body / send failure → KEEP the journal for a later resolve.
//
// The send itself is a caller-supplied closure (the run-lane reportState choke point at a run site;
// a direct client.reportState in the judge/review lanes and at M4 boot), so the same primitive
// serves every terminal producer without importing the runner.

import { RequestError, type WorkerClient } from "./client.js";
import type { Logger } from "./log.js";
import type { Outbox } from "./outbox.js";
import { canonicalizeTerminalBody } from "./outbox.js";
import type { MessageGapsResponse, OutgoingMessage, StateAck, StateRequest } from "./protocol.js";
import { errMessage } from "./util.js";

/** The run's terminal statuses. A 409 whose returned status is one of these means the outcome has
 *  already landed (a lost ack after a prior commit, or a racing cancel), so the journal retires. */
const TERMINAL_STATUSES: ReadonlySet<string> = new Set(["completed", "failed", "cancelled"]);

/** The fixed reason a gap-fill tombstone records (PRD #1391 Run B M3). Mirrors Run A's dropped-range
 *  tombstone shape (event:"message_dropped" + reason) so the browser renders it the same way and the
 *  stream stays contiguous, but names THIS cause — a hole from before the feature, or a dropped
 *  range — so an operator can tell the two apart. */
const GAP_FILL_REASON = "unrecoverable gap";

/** Chunk size for posting gap-fill tombstones — one tombstone per missing seq, batched so a wide gap
 *  never posts thousands of one-message requests. Well under MAX_BATCH_BYTES (tombstones are ~150 B,
 *  so 500 ≈ ~75 KiB), mirroring the outbox drainer's OUTBOX_DRAIN_CHUNK. */
const TOMBSTONE_CHUNK = 500;

/** The outbox + client + config the terminal send path needs. `client` is narrowed to the three
 *  methods used so a test can pass a tiny fake, and so the shape reads as exactly the dependency
 *  surface. */
export interface TerminalOutboxDeps {
  outbox: Outbox;
  client: Pick<WorkerClient, "hasFeature" | "getMessageGaps" | "postMessages">;
  /** config.gapFillMax — the total per-seq tombstone budget before a hole is declared unrecoverable. */
  gapFillMax: number;
  /** config.outboxTerminalMaxBytes — the per-record canonicaliser cap. */
  terminalMaxBytes: number;
  log: Logger;
}

/** The caller-supplied terminal send: the run-lane reportState choke point (which stamps
 *  claim_generation and drives recovery), or a direct client.reportState in the judge/review lanes.
 *  Returns the ack the resolve path classifies. */
export type SendTerminalState = (body: StateRequest, signal?: AbortSignal) => Promise<StateAck>;

/** Build the terminal-resolve deps for a lane, or undefined when no usable outbox is wired (a test
 *  without spill, or a store that failed closed) — in which case the caller sends un-journaled. One
 *  builder so every lane (run, judge, review, M4 boot) applies the SAME isDisabled() gate. */
export function makeTerminalOutboxDeps(
  outbox: Outbox | undefined,
  client: TerminalOutboxDeps["client"],
  opts: { gapFillMax: number; terminalMaxBytes: number; log: Logger },
): TerminalOutboxDeps | undefined {
  if (!outbox || outbox.isDisabled()) return undefined;
  return { outbox, client, gapFillMax: opts.gapFillMax, terminalMaxBytes: opts.terminalMaxBytes, log: opts.log };
}

/**
 * Journal a DIRECT-lane terminal report (judge/review) write-ahead then resolve it, or send it
 * un-journaled when no outbox is wired. The judge and review lanes report state DIRECTLY (not
 * through the RunRunner reportState choke point), so the send is `client.reportState(runId, body)`.
 * They journal their terminal STATE only, NEVER the verdict/review POST (D6) — the caller posts the
 * verdict separately, before this, and never through this path.
 */
export async function postTerminalState(
  deps: TerminalOutboxDeps | undefined,
  client: Pick<WorkerClient, "reportState">,
  args: { runId: string; claimGeneration: number; phase: string; messagesThroughSeq: number; body: StateRequest },
): Promise<void> {
  const send: SendTerminalState = (b, sig) => client.reportState(args.runId, b, sig);
  if (!deps) {
    await send(args.body);
    return;
  }
  await journalAndResolveTerminal(deps, {
    runId: args.runId,
    claimGeneration: args.claimGeneration,
    phase: args.phase,
    messagesThroughSeq: args.messagesThroughSeq,
    body: args.body,
    send,
  });
}

interface ResolveArgs {
  runId: string;
  claimGeneration: number;
  send: SendTerminalState;
  signal?: AbortSignal;
}

/**
 * Journal a run-lane terminal outcome WRITE-AHEAD, then resolve it. The site has already closed /
 * final-flushed its batcher (so `messagesThroughSeq` is the run's DURABLE emitted tail) and passes
 * the report body sans the fence — this canonicalises it ONCE (so the journal and the first send are
 * byte-identical), journals it, and resolves it. On `reserve_exhausted` the outcome is sent
 * UNJOURNALED as today, logged and never silently dropped (SC2).
 */
export async function journalAndResolveTerminal(
  deps: TerminalOutboxDeps,
  args: {
    runId: string;
    claimGeneration: number;
    /** The phase at journal time (what #1390's snapshot reports for a pending outcome after a restart). */
    phase: string;
    /** The run's last emitted seq after the final flush — the DURABLE tail, never a server value. */
    messagesThroughSeq: number;
    body: StateRequest;
    send: SendTerminalState;
    signal?: AbortSignal;
  },
): Promise<void> {
  const { outbox, terminalMaxBytes, log } = deps;
  const { runId, claimGeneration, phase, messagesThroughSeq, body, send, signal } = args;
  // Canonicalise ONCE (D-A1/D2): the SAME bytes are journalled and sent, so the first send and any
  // replay are byte-identical, and no optional field is byte-cut past the api's scrubber.
  const canonical = canonicalizeTerminalBody(body as unknown as Record<string, unknown>, terminalMaxBytes);
  const result = await outbox.journalTerminal(runId, claimGeneration, phase, messagesThroughSeq, canonical);
  if (!result.journaled) {
    // reserve_exhausted (SC2): the terminal-sized reserve could not admit the journal. Send the
    // canonical body UNJOURNALED, with the fence when advertised, keeping today's semantics — the
    // send's own retries apply and a throw propagates to the executor catch (which finds NO journal
    // and takes today's fallback). Logged + counted, never a silent drop.
    log.error("outbox: terminal journal reserve exhausted; sending outcome unjournaled (SC2)", {
      run_id: runId,
      claim_generation: claimGeneration,
      status: typeof canonical.status === "string" ? canonical.status : "unknown",
    });
    await send(withTerminalFence(deps, canonical, messagesThroughSeq), signal);
    return;
  }
  await resolvePendingTerminal(deps, { runId, claimGeneration, send, signal });
}

/**
 * The reusable resolve primitive: read the run's pending terminal journal, send its canonical bytes
 * (adding the fence only when the api advertised `terminal_fence`), and classify the ack (see the
 * module header). Never THROWS on an expected failure — a send that exhausts its retries or a
 * transport blip leaves the journal installed for a later resolve, so a run-lane terminal site never
 * falls through to a second `failed` once the write-ahead journal exists (fact 2, D5).
 *
 * @public — the boot-time resolve (M4) is its cross-module consumer: on startup the worker resolves
 * every pending terminal journal (send, retire/stale-retire/mark-blocked) BEFORE its claim loops
 * start, with a direct client.reportState send. Used within this file today by journalAndResolveTerminal
 * (the write-ahead path) and postTerminalState (the judge/review path).
 */
export async function resolvePendingTerminal(deps: TerminalOutboxDeps, args: ResolveArgs): Promise<void> {
  const { outbox, log } = deps;
  const { runId, claimGeneration, send, signal } = args;
  const journal = await outbox.readTerminalJournal(runId, claimGeneration);
  if (!journal) return; // already retired / never installed / unreadable — nothing to resolve
  if (journal.blocked) return; // permanently blocked (D13): owner-resolved, never auto-retried here
  let ack: StateAck;
  try {
    ack = await send(withTerminalFence(deps, journal.body, journal.messagesThroughSeq), signal);
  } catch (err) {
    // The send exhausted its bounded retries, hit a fatal 4xx we do not classify, or was aborted.
    // Leave the journal installed (the outcome is durable) for a later resolve, and do NOT rethrow.
    log.warn("outbox: terminal report send failed; leaving the journal for a later resolve", {
      run_id: runId,
      claim_generation: claimGeneration,
      error: errMessage(err),
    });
    return;
  }
  if (ack.reason === "messages_pending") {
    await gapFillLoop(deps, args, journal.messagesThroughSeq, journal.body);
    return;
  }
  await actOnAck(deps, args, ack);
}

/** Classify a terminal-report ack that is NOT `messages_pending` and act on it. Order matters:
 *  stale_claim outranks a terminal status (a superseded generation must local-retire, never write). */
async function actOnAck(deps: TerminalOutboxDeps, args: ResolveArgs, ack: StateAck): Promise<void> {
  const { outbox } = deps;
  const { runId, claimGeneration } = args;
  if (ack.applied) {
    // 200: the transition landed. Retire the journal (the outcome is the run's outcome now).
    await outbox.retireTerminal(runId, claimGeneration);
    return;
  }
  if (ack.staleClaim) {
    // 409 stale_claim: a newer claim superseded this generation. Local-retire (D11) — increments
    // stale_retired, writes NOTHING to the server, and NEVER rebinds the outcome to a new generation.
    await outbox.staleRetireTerminal(runId, claimGeneration);
    return;
  }
  if (ack.reason === "gap_unrecoverable") {
    // 409 gap_unrecoverable: the hole below the fence exceeds the api's bound — mark blocked (D13).
    await outbox.markTerminalBlocked(runId, claimGeneration, "gap_unrecoverable");
    return;
  }
  if (ack.status !== undefined && TERMINAL_STATUSES.has(ack.status)) {
    // 409 whose returned status is terminal: the outcome already landed (a lost ack after a prior
    // commit, or a racing cancel). Retire.
    await outbox.retireTerminal(runId, claimGeneration);
    return;
  }
  // Any other non-terminal 409 (running / claimed / paused / completion-permit mismatch) or an
  // unreadable body: KEEP the journal — a later resolve retries it (fact 5).
}

/**
 * The gap-fill loop (D3): while the terminal report is refused `messages_pending`, read the run's
 * missing seq ranges up to the fence, fill each with ONE per-seq "unrecoverable gap" tombstone
 * carrying the journal's claim generation (so a NEWER flight refuses it as stale_claim rather than
 * having it touched), then re-send. Bounded by `gapFillMax`; past the bound, or on `gap_unrecoverable`,
 * mark the journal blocked and stop (D13). A tombstone append refused stale_claim stale-retires the
 * journal and stops (D11).
 */
async function gapFillLoop(
  deps: TerminalOutboxDeps,
  args: ResolveArgs,
  fence: number,
  body: Record<string, unknown>,
): Promise<void> {
  const { outbox, client, gapFillMax, log } = deps;
  const { runId, claimGeneration, send, signal } = args;
  let filledTotal = 0;
  for (;;) {
    if (signal?.aborted) return; // aborted: leave the journal for a later resolve
    // Collect every currently-missing seq in [1..fence], paging by keyset cursor. The gaps shrink as
    // we fill, but the keyset walk runs over higher closers than what we fill, so one pass covers the
    // whole hole (the trailing gap max_present..fence is included via the api's sentinel).
    const seqs: number[] = [];
    let cursor = 0;
    let overBound = false;
    for (;;) {
      if (signal?.aborted) return;
      let page: MessageGapsResponse;
      try {
        page = await client.getMessageGaps(runId, fence, undefined, cursor);
      } catch (err) {
        log.warn("outbox: message-gaps read failed during gap fill; leaving the journal", {
          run_id: runId,
          error: errMessage(err),
        });
        return;
      }
      for (const gap of page.gaps) {
        for (let seq = gap.first; seq <= gap.last; seq++) {
          if (filledTotal + seqs.length >= gapFillMax) {
            overBound = true;
            break;
          }
          seqs.push(seq);
        }
        if (overBound) break;
      }
      if (overBound || page.next_cursor === undefined) break;
      cursor = page.next_cursor;
    }
    if (overBound) {
      log.warn("outbox: gap fill would exceed WORKER_GAP_FILL_MAX; marking the terminal blocked", {
        run_id: runId,
        gap_fill_max: gapFillMax,
      });
      await outbox.markTerminalBlocked(runId, claimGeneration, "gap_unrecoverable");
      return;
    }
    const filledThisPass = seqs.length;
    for (let i = 0; i < seqs.length; i += TOMBSTONE_CHUNK) {
      const chunk = seqs.slice(i, i + TOMBSTONE_CHUNK).map((seq) => gapTombstone(seq));
      const outcome = await appendTombstones(deps, runId, chunk, claimGeneration, signal);
      if (outcome === "stale_claim") {
        // A newer flight owns the run — the fenced tombstone was refused. Local-retire (D11) and stop.
        log.warn("outbox: gap-fill tombstone refused as stale claim; stale-retiring the journal", {
          run_id: runId,
        });
        await outbox.staleRetireTerminal(runId, claimGeneration);
        return;
      }
      if (outcome === "error") return; // transient: leave the journal for a later resolve
      filledTotal += chunk.length;
    }
    // Re-send the terminal report now the gaps up to the fence are filled.
    let ack: StateAck;
    try {
      ack = await send(withTerminalFence(deps, body, fence), signal);
    } catch (err) {
      log.warn("outbox: terminal re-send failed after gap fill; leaving the journal", {
        run_id: runId,
        error: errMessage(err),
      });
      return;
    }
    if (ack.reason === "messages_pending") {
      // Still pending. If this pass filled NOTHING (no fillable gap, yet the fence is unsatisfied),
      // there is no progress to be made — mark blocked rather than spin forever.
      if (filledThisPass === 0) {
        log.warn("outbox: terminal still messages_pending with no fillable gap; marking blocked", {
          run_id: runId,
        });
        await outbox.markTerminalBlocked(runId, claimGeneration, "gap_unrecoverable");
        return;
      }
      continue; // re-read the (now smaller) gaps and fill again
    }
    await actOnAck(deps, args, ack);
    return;
  }
}

/** Post a chunk of gap-fill tombstones, carrying the journal's claim generation so a fenced api
 *  refuses them as `stale_claim` once a newer flight owns the run. Returns whether they landed, were
 *  stale-refused (a 409 whose body carries the stale_claim disposition), or hit a transient error. */
async function appendTombstones(
  deps: TerminalOutboxDeps,
  runId: string,
  msgs: OutgoingMessage[],
  claimGeneration: number,
  signal?: AbortSignal,
): Promise<"sent" | "stale_claim" | "error"> {
  try {
    await deps.client.postMessages(runId, msgs, claimGeneration, signal);
    return "sent";
  } catch (err) {
    if (err instanceof RequestError && err.status === 409 && err.body.includes('"stale_claim"')) {
      return "stale_claim";
    }
    deps.log.warn("outbox: gap-fill tombstone append failed", { run_id: runId, error: errMessage(err) });
    return "error";
  }
}

/** One per-seq "unrecoverable gap" tombstone (PRD #1391 Run B M3). Shape mirrors Run A's dropped
 *  tombstone so the browser renders it identically and the stream stays contiguous. */
function gapTombstone(seq: number): OutgoingMessage {
  return {
    seq,
    kind: "status",
    agent: "worker",
    payload: { text: `message unrecoverable: gap (seq ${seq})`, event: "message_dropped", reason: GAP_FILL_REASON },
  };
}

/** Add the `messages_through_seq` fence to a terminal body ONLY when the api advertised
 *  `terminal_fence` (D9): never send a fence to an api that would strict-decode it as an unknown
 *  field. The journalled body never carries the fence, so this is the single place it is added. */
function withTerminalFence(deps: TerminalOutboxDeps, body: Record<string, unknown>, fence: number): StateRequest {
  const out: StateRequest = { ...(body as unknown as StateRequest) };
  if (deps.client.hasFeature("terminal_fence")) out.messages_through_seq = fence;
  else delete out.messages_through_seq;
  return out;
}
