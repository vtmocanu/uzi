// Worker templates (PRD #18): the curated image variants a worker can be built
// from. This is the web-side mirror of the server registry (api/internal/
// workertmpl) and of the Dockerfiles under agent/templates/<name>/ — keep the
// three in sync. Hardcoded here the same way ModelSelect hardcodes MODEL_ALIASES:
// a tiny curated list needs no API round-trip.

/** Curated template names, in display order (base first). */
export const WORKER_TEMPLATES = ["base", "jvm"] as const;

export type WorkerTemplate = (typeof WORKER_TEMPLATES)[number];

/** The default template when none is chosen (today's minimal image). */
export const DEFAULT_WORKER_TEMPLATE: WorkerTemplate = "base";

/**
 * True when a worker's declared and reported templates disagree (PRD #18 drift).
 * Only a real conflict counts: both sides must be set. A null on either side
 * (no declared choice, or an older image that reports nothing) is unknown, not
 * drift, so it never badges. Soft signal — the caller shows it, never blocks on
 * it.
 */
export function hasTemplateDrift(
  declared: string | null,
  reported: string | null,
): boolean {
  return declared != null && reported != null && declared !== reported;
}
