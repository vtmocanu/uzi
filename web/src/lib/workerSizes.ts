// Hosted worker sizes (PRD #58): the curated presets a hosted worker can be
// provisioned at. This is the web-side mirror of the server registry (api/internal/
// workersize), exactly as workerTemplates.ts mirrors workertmpl.Names — a tiny
// curated list needs no API round-trip, and GET /workers/hosted/config deliberately
// does not ship it. Unlike the template list, this one has a drift gate:
// workerSizes.test.ts checks it against the cross-module golden the api generates.
//
// NAMES ONLY, and that is a decision rather than an omission. What cpu/memory/disk
// each preset actually buys is the CONTROLLER's preset table (Decision 1: the api may
// not know a pod spec), and that table does not exist yet — M3 writes it. Nobody has
// chosen the quantities, so displaying one here would mean inventing it, and a number
// M5 invents that M3 later contradicts is a lie to the user with no loud failure.
// Showing the quantities is deferred to M6, once there is an authority to read them
// from. Do not add a number to this file, or to the UI, before then.

/** Curated size names, in display order (smallest first). Lowercase on the wire. */
export const WORKER_SIZES = ["s", "m", "l"] as const;

export type WorkerSize = (typeof WORKER_SIZES)[number];

/**
 * The size a fresh provision form starts on: M, for parity with the worker every user
 * already runs by hand.
 *
 * M is NOT "the middle one" — it is what this repo's own sizing formula computes as the
 * FLOOR for a working worker. docker-compose.yml budgets ~1.5 GiB per slot, and the
 * default config is 1 run slot + 1 chat session = 2 slots = 3 GiB, plus ~1 GiB headroom
 * = 4 GiB. That is exactly compose's own AGENT_MEM_LIMIT=4g / AGENT_CPUS=2, and exactly
 * the k8s block docs/worker-setup.md already publishes. Two independent artifacts
 * already agree on it, which makes M the only preset that is known-good rather than
 * guessed.
 *
 * The chat slot cannot be switched off, which is what puts the floor at 4 GiB rather
 * than at one slot: WORKER_CHAT_SESSIONS=0 silently falls back to 1 (positiveInt,
 * agent/src/config.ts), so every worker budgets a run AND a chat.
 *
 * S stays offered, but is not what we hand someone who never opens the field. Whether
 * it can run a real session is still UNMEASURED — no benchmark has spawned an SDK
 * subprocess, which is what dominates a real run — and it sits below even the bare
 * two-slot total before headroom. The default rests on the asymmetry: an over-sized
 * default wastes a LIMIT, and limits reserve nothing (only requests do; an idle worker
 * measures ~130 MiB and ~0 CPU whichever preset it is), whereas an under-sized one OOMs
 * the shared cgroup, killing the container and requeueing every in-flight run — which
 * FAILS them past RUN_MAX_REQUEUES, with no pod-phase status in v1 to explain why.
 *
 * Still open, and untouched by this: nothing gives a user a reason to pick S over L
 * either, since the quota counts WORKERS and every size costs exactly one. This fixes
 * what we hand them, not the incentive. The argument lives in PRD #58's M6 bullet and
 * is deliberately not restated here; two prose copies of it would drift.
 */
export const DEFAULT_WORKER_SIZE: WorkerSize = "m";

/**
 * Display spelling of a size name. Upper-case is for READING only — the wire value
 * is lowercase and nothing upper-cased is ever a value (workersize.Valid("M") is
 * false, and a Go test pins that). Keep this the only place the two diverge.
 */
export function sizeLabel(size: string): string {
  return size.toUpperCase();
}
