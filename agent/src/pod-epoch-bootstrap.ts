type Runtime = "containerd-runc" | "crio-crun";
type Consumption = "fresh" | "existing" | "full" | "uncertain";

interface RuntimePort {
  inspectConsumption(podUid: string): Consumption;
}

interface Bootstrap {
  readonly podUid: string;
  readonly runtime?: Runtime;
  readonly evidenceFile?: "/etc/uzi/pod-epoch/evidence";
  readonly firstMode: "shared" | "exclusive";
  readonly reason?: "runtime_not_declared" | "runtime_not_supported" | "no_projection" |
    "unsupported_projection" | "already_consumed" | "consumption_full" | "consumption_uncertain";
}

/** Read only the bootstrap environment. This snapshot grants no admission authority.
 * Call acquire with firstMode, then durably consume under ownership before config
 * or admission. Only a fresh durable consumer holding EX may evaluate evidence.
 */
export function inspectPodEpochBootstrap(
  environment: Pick<NodeJS.ProcessEnv, "UZI_POD_UID" | "UZI_POD_EPOCH_RUNTIME" | "UZI_POD_EPOCH_EVIDENCE_FILE">,
  runtimePort: RuntimePort,
): Readonly<Bootstrap> {
  const podUid = environment.UZI_POD_UID ?? "";
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(podUid)) {
    throw new Error("invalid pod UUID in read-only epoch bootstrap");
  }
  const declared = environment.UZI_POD_EPOCH_RUNTIME;
  const projection = environment.UZI_POD_EPOCH_EVIDENCE_FILE;
  const runtime: Runtime | undefined = declared === "containerd-runc" || declared === "crio-crun" ? declared : undefined;
  const evidenceFile: Bootstrap["evidenceFile"] = projection === "/etc/uzi/pod-epoch/evidence" ? projection : undefined;
  const base = { podUid, runtime, evidenceFile };
  const shared = (reason: NonNullable<Bootstrap["reason"]>): Readonly<Bootstrap> =>
    Object.freeze({ ...base, firstMode: "shared", reason });
  if (!declared) return shared("runtime_not_declared");
  if (!runtime) return shared("runtime_not_supported");
  if (!projection) return shared("no_projection");
  if (!evidenceFile) return shared("unsupported_projection");
  switch (runtimePort.inspectConsumption(podUid)) {
    case "existing": return shared("already_consumed");
    case "full": return shared("consumption_full");
    case "uncertain": return shared("consumption_uncertain");
    case "fresh": return Object.freeze({ ...base, firstMode: "exclusive" });
  }
}
