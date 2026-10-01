// Issue #1514: the operator scope ceiling at a signal_done exit.
//
// The loop-top honor gate (sdk-executor) latches `scopeCapped` only on a FURTHER iteration. A lead
// that finishes its last permitted milestone and calls signal_done in the same turn exits through
// the done path without ever reaching that gate, so the run would finalize as a full delivery
// (`Closes #N`, no scope_capped report). This pure helper lets the done exit make the same
// decision from the last served ceiling plus the completed ids the run has declared.

interface ScopeCap {
  completedCount: number;
  total: number;
  /** The served ceiling that was reached, for the steer_ack payload. Not part of the latched
   *  `scopeCapped` value (ExecutorResult shape is `{completedCount, total}`). */
  ceiling: number;
}

/**
 * Returns the cap to latch at a done exit, or undefined when the run is not scope-capped.
 * Capped iff a numeric ceiling was served, the frozen list is known and non-empty, and the
 * completed count (the larger of the server's count and the frozen ids in the union of the
 * server's completed ids and the ids declared locally) has
 * reached the ceiling while milestones remain. `count < total` keeps a genuinely full delivery
 * closing the issue.
 */
export function scopeCapAtDone(args: {
  served: { scopeCeiling?: number; completedCount?: number; completedIds?: readonly string[] } | undefined;
  frozen: readonly { id: string }[] | undefined | null;
  completedIds: Iterable<string>;
}): ScopeCap | undefined {
  const { served, frozen, completedIds } = args;
  if (!served || typeof served.scopeCeiling !== "number") return undefined;
  if (!frozen || frozen.length === 0) return undefined;
  const frozenIds = new Set(frozen.map((m) => m.id));
  const done = new Set<string>();
  for (const id of served.completedIds ?? []) if (frozenIds.has(id)) done.add(id);
  for (const id of completedIds) if (frozenIds.has(id)) done.add(id);
  const count = Math.max(served.completedCount ?? 0, done.size);
  if (count >= served.scopeCeiling && count < frozen.length) {
    return { completedCount: count, total: frozen.length, ceiling: served.scopeCeiling };
  }
  return undefined;
}

/** The steer_ack payload shared by the loop-top gate and the done exit. */
export function scopeSteerAckPayload(ceiling: number, completed: number): Record<string, unknown> {
  return {
    text: `finalizing at ${completed} completed milestone(s) (operator ceiling was ${ceiling}); starting no further milestone`,
    directive: "scope",
    ceiling,
    completed,
  };
}
