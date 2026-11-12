// Worker hibernation display helpers (PRD #4205). Pure: the badge, the capacity math and the
// admin controls all key off hibernation_state first, since a non-"none" worker reports
// upgrade_status "unknown" and is never outdated/rolling/offline-broken.

import type { Worker } from "./api";
import { ApiError } from "./apiError";

export type HibernationState = "none" | "preparing" | "hibernated" | "waking";

/** The worker's hibernation state; an absent field (older api, bare fixture) reads "none". */
export function hibernationState(w: Pick<Worker, "hibernation_state">): HibernationState {
  return w.hibernation_state ?? "none";
}

export function isHibernating(w: Pick<Worker, "hibernation_state">): boolean {
  return hibernationState(w) !== "none";
}

const LABELS: Record<Exclude<HibernationState, "none">, { label: string; title: string }> = {
  preparing: { label: "hibernating", title: "Hibernating: finishing its shutdown. It takes no new runs." },
  hibernated: { label: "hibernated", title: "Hibernated: its pod is scaled down on purpose. It wakes when a run needs it, or an admin can wake it." },
  waking: { label: "waking", title: "Waking: its pod is starting again. It takes no new runs until it is ready." },
};

export function hibernationPresentation(w: Pick<Worker, "hibernation_state">): { label: string; title: string } | null {
  const s = hibernationState(w);
  return s === "none" ? null : LABELS[s];
}

// Human text for the reason_code of a refused hibernate/wake (409). Codes are the closed
// strings of api/internal/workersvc/hibernation.go (Refusal*) plus "not_hibernating" from
// the wake path. An unknown code falls back to the server message.
const REFUSALS: Record<string, string> = {
  setting_off: "Worker hibernation is turned off for this instance. An admin can turn it on in Admin settings.",
  not_persistent_hosted: "Only persistent hosted workers can hibernate.",
  already_hibernating: "This worker is already hibernating.",
  no_pod_identity: "The worker has not reported its pod identity yet. Try again once it is online.",
  controller_not_advertising: "The worker controller is not advertising hibernation support, so the pod cannot be scaled down.",
  busy: "The worker is busy with runs. Wait for it to go idle.",
  custody_hold: "The worker holds unpublished work that needs an owner decision. Resolve the custody hold first.",
  intake_fenced: "The worker is cordoned, draining or rolling. Try again when it is accepting runs.",
  pending_wake_demand: "A queued run woke this worker and is still waiting for it.",
  not_hibernating: "This worker is not hibernating.",
};

/** Message for a failed hibernate/wake call: a 409 carries a reason_code mapped to closed
 *  copy; any other error shows the server message or the caller's fallback. */
export function hibernationErrorMessage(err: unknown, fallback: string): string {
  if (!(err instanceof ApiError)) return fallback;
  const code = (err.body as { reason_code?: unknown } | null)?.reason_code;
  return (err.status === 409 && typeof code === "string" && REFUSALS[code]) || err.message || fallback;
}
