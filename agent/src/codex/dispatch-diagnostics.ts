// The fixed Codex dispatch-completion diagnostics shared by their producers (the harness and the
// delegation runner) and their read-side consumer (the judge evidence reducer), so the two cannot
// drift. Dependency-free on purpose: judge-evidence imports it without pulling in Codex runtime.

/** The synthesized lead completion for a bound dispatch the run stopped (abort/stop/close). */
export const DISPATCH_STOPPED = "delegation stopped by the run; child settlement not confirmed";
/** The synthesized lead completion for a bound dispatch still open when its turn finished. */
export const DISPATCH_OPEN_AT_TURN_END = "delegation still open at turn end; child settlement not confirmed";
/** The synthesized lead completion for a bound dispatch whose turn stream failed (a protocol or
 *  transport throw, e.g. an unexpected provider EOF). */
export const DISPATCH_STREAM_FAILED = "delegation ended by a provider stream failure; child settlement not confirmed";

/** The bound-phase `child_timeout` message is `PREFIX + formatChildTurnBudget(ms) + SUFFIX`. Only
 *  this phase can reach a persisted dispatch completion: an unbound dispatch projects none. */
export const CHILD_TIMEOUT_BOUND_PREFIX = "child_timeout: the delegated child exceeded its ";
export const CHILD_TIMEOUT_BOUND_SUFFIX = " turn budget; interrupt requested, partial work may remain";
