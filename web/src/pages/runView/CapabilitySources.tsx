import type { Run } from "../../lib/api";
import { stripUnsafeChars } from "../../lib/safeText";
import { useDemoMode } from "../../lib/demoMode";

const reasons: Record<string,string> = {
  jvm_manifest: "JVM build manifest", docker_marker: "Docker or Compose marker",
  testcontainers_reference: "testcontainers reference", docker_command: "Docker command match",
};

export function CapabilitySources({ provenance }: { provenance: Run["capability_provenance"] }) {
  const demo = useDemoMode();
  if (!provenance?.capabilities.length) return null;
  return <div role="group" aria-label="Capability sources" className="mb-2 space-y-2 text-xs text-muted">
    {!provenance.hints_known && <p>Repo-hint history unavailable.</p>}
    {!provenance.inference_known && <p>Inference evidence unavailable from this worker.</p>}
    {provenance.capabilities.map(item => <details key={item.capability}>
      <summary className="cursor-pointer">
        <span className="font-mono">{stripUnsafeChars(item.capability)}</span>: {item.sources.map(source =>
          source === "repo_hint" ? "repo hint" : source === "inferred" ? "inferred" : "source unknown").join(" + ")}
        {item.inference_ignored ? "; inference ignored" : ""}{item.required ? "; required" : "; not currently required"}
      </summary>
      {!demo && <ul className="mt-1 space-y-1 pl-3">
        {item.triggers.map((trigger, i) => <li key={i}>
          <span className="font-mono break-all">{stripUnsafeChars(trigger.path)}</span> ({reasons[trigger.reason] ?? "Detector match"})
        </li>)}
        {item.truncated && <li>Additional or invalid trigger paths omitted.</li>}
        {item.sources.includes("inferred") && !item.triggers.length && <li>Trigger paths unavailable.</li>}
      </ul>}
    </details>)}
  </div>;
}
