type DiskRecovery = {
  status: string;
  recovery_wait_cause?: string | null;
  recovery_retry_not_before?: string | null;
};

// Only explicit null proves an owner hold; elapsed finite timestamps remain timed.
export function diskWaitMode(run: DiskRecovery): "held" | "timed" | "unknown" | null {
  if (run.status !== "recovery_wait" || run.recovery_wait_cause !== "data_volume_full") return null;
  if (run.recovery_retry_not_before === null) return "held";
  return typeof run.recovery_retry_not_before === "string" && Number.isFinite(Date.parse(run.recovery_retry_not_before))
    ? "timed" : "unknown";
}

export function diskWaitLabel(run: DiskRecovery): string | null {
  const mode = diskWaitMode(run);
  return mode === "held" ? "Disk recovery needs your decision"
    : mode === "timed" ? "Waiting for disk space"
    : mode === "unknown" ? "Disk retry timing unknown" : null;
}

export function diskWaitDescription(mode: ReturnType<typeof diskWaitMode>): string {
  return mode === "held"
    ? "Automatic disk recovery has stopped. Only the confirmed run owner can Resume or Cancel. There is no scheduled retry."
    : mode === "timed"
      ? "The worker's disk is full or nearly full. uzi frees space on the worker and the run retries automatically after its scheduled time, including when already due. Admission may still refuse execution."
      : "The worker's disk is full or nearly full. Retry timing is unavailable; neither an owner hold nor an automatic retry date is established.";
}
