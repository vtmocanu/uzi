// Hosted worker sizes (PRD #58): the curated presets a hosted worker can be
// provisioned at. This is the web-side mirror of the server registry (api/internal/
// workersize), exactly as workerTemplates.ts mirrors workertmpl.Names — a tiny
// curated list needs no API round-trip, and GET /workers/hosted/config deliberately
// does not ship it. Unlike the template list, this one has a drift gate:
// workerSizes.test.ts checks it against the cross-module golden the api generates.
//
// THE QUANTITIES LANDED IN M6 (2026-07-17). This file used to say "names only ...
// do not add a number to this file, or to the UI, before then", because the authority
// did not exist yet: what each preset buys is the CONTROLLER's preset table
// (Decision 1 — the api may not know a pod spec), and until M3 wrote it, any number
// here would have been invented. It exists now, so the numbers below are MIRRORED
// from it, not chosen here, and workerSizes.test.ts pins them to the golden that
// table generates. The old rule still holds in the form that matters: do not put a
// number in this file that does not come from that golden.

/** Curated size names, in display order (smallest first). Lowercase on the wire. */
export const WORKER_SIZES = ["s", "m", "l"] as const;

export type WorkerSize = (typeof WORKER_SIZES)[number];

/**
 * What one preset buys, as raw k8s quantity strings — rendered verbatim, never
 * reformatted into a number and a unit. The strings are the authority's own spelling
 * ("250m", "4Gi"); re-deriving them here is how a display drifts from the pod.
 *
 * ONLY THE LIMITS, and only the fields shown. The presets are Burstable (requests <
 * limits), and the golden carries both — but a user choosing between sizes is
 * choosing a CEILING ("will my build fit?"), while the request is a scheduling
 * detail they cannot observe. The UI says "up to" for exactly this reason.
 *
 * `/nix` is deliberately absent: it is a flat 20Gi on every size (PRD #87 sized
 * it for the prebaked Chromium closure), so it cannot inform a choice between them.
 */
export type WorkerSizeSpec = {
  readonly cpuLimit: string;
  readonly memoryLimit: string;
  readonly data: string;
};

/**
 * Mirror of controller/internal/preset's table, via its display golden. This is the
 * third copy (controller table -> golden -> here) and the golden buys the one
 * property that matters: they cannot drift SILENTLY. Editing a preset quantity turns
 * the controller's golden test red, and regenerating it turns workerSizes.test.ts red
 * until this constant follows.
 *
 * It does NOT cover deployment skew — a released bundle can show last release's
 * numbers, the same limit the name mirror already carries. Strictly less harmful than
 * a stale name, though: a stale label misinforms, a stale name strands a worker.
 */
export const WORKER_SIZE_SPECS: Record<WorkerSize, WorkerSizeSpec> = {
  s: { cpuLimit: "1", memoryLimit: "2Gi", data: "5Gi" },
  m: { cpuLimit: "2", memoryLimit: "4Gi", data: "10Gi" },
  l: { cpuLimit: "4", memoryLimit: "12Gi", data: "20Gi" },
};

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
 * S stays offered, but is not what we hand someone who never opens the field — and the
 * reason CHANGED after this was written, so the argument is worth reading rather than
 * inheriting. This comment used to say "whether S can run a real session is still
 * UNMEASURED — no benchmark has spawned an SDK subprocess". That is no longer true: a
 * live capstone measured a complete real SDK run peaking at 676 MiB (cgroup
 * memory.peak), so S's 2Gi fits the AGENT roughly three times over and S is not the
 * OOM risk it looked like. Corrected 2026-07-17 (M6); see PRD #58's M6 bullet.
 *
 * M survives that correction for a DIFFERENT reason than it was chosen for: what 676
 * MiB bounds is the agent, not the USER'S BUILD, which nothing has measured (the e2e
 * repo is a single-commit fake and compiles nothing — a JVM test suite or a large `go
 * build` is what dwarfs the agent). So the default still rests on the asymmetry: an
 * over-sized default wastes a LIMIT, and limits reserve nothing (only requests do; an
 * idle worker measures ~130 MiB and ~0 CPU whichever preset it is), whereas an
 * under-sized one OOMs the shared cgroup, killing the container and requeueing every
 * in-flight run — which FAILS them past RUN_MAX_REQUEUES, with no pod-phase status in
 * v1 to explain why. M is also compose parity and the formula's floor. Do not
 * re-litigate S on the strength of the 676 MiB number: it bounds the agent, not the
 * build.
 *
 * Still open, and NOT closed by showing the numbers: nothing gives a user a reason to
 * pick S over L, since the quota counts WORKERS and every size costs exactly one. The
 * quantities fix the INFORMED half (a user could not previously see what a size buys);
 * the INCENTIVE half is untouched and was deliberately deferred — the user declined
 * both structural levers (a resource-weighted quota; offering one size only) twice.
 * The argument lives in PRD #58's M6 bullet and is deliberately not restated here; two
 * prose copies of it would drift.
 */
export const DEFAULT_WORKER_SIZE: WorkerSize = "m";

/**
 * Display spelling of a size name. Upper-case is for READING only — the wire value
 * is lowercase and nothing upper-cased is ever a value (workersize.Valid("M") is
 * false, and a Go test pins that). Keep this the only place the two diverge.
 */
function sizeLabel(size: string): string {
  return size.toUpperCase();
}

/**
 * One preset's quantities, as a single readable clause for the picker:
 * `up to 2 CPU / 4Gi RAM / 10Gi disk`.
 *
 * "up to" qualifies all three honestly: CPU and memory are cgroup ceilings a
 * Burstable pod bursts into, and the disk is a volume of that size — in every case
 * the number is the most the worker can use, which is the question a user picking a
 * size is actually asking.
 *
 * Returns "" for an unknown name rather than throwing. A size the api ships and this
 * mirror lacks is already a caught, red build (workerSizes.test.ts); at RUNTIME the
 * only way here is deployment skew, and the right answer there is a picker that still
 * works with one unlabelled option — never a crashed settings page.
 */
export function sizeSummary(size: string): string {
  const spec = WORKER_SIZE_SPECS[size as WorkerSize];
  if (!spec) return "";
  return `up to ${spec.cpuLimit} CPU / ${spec.memoryLimit} RAM / ${spec.data} disk`;
}

/**
 * The picker's option text: `M — up to 2 CPU / 4Gi RAM / 10Gi disk`.
 *
 * A <select> option can hold only text, and this is deliberately where the numbers
 * go: the whole point is that the quantities are visible AT the moment of choosing,
 * not in a table somewhere else. Degrades to the bare label under skew.
 */
export function sizeOptionLabel(size: string): string {
  const summary = sizeSummary(size);
  return summary ? `${sizeLabel(size)} — ${summary}` : sizeLabel(size);
}
