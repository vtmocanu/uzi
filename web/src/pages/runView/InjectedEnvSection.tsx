import type { RunInjectedEnvVar } from "../../lib/apiTypes";
import { stripUnsafeChars } from "../../lib/safeText";
import { Badge } from "../../components/ui";

// PRD #4416 M2: the variables the run's current claim set in the environment of every command it
// ran. The server sends names, scope and the secret flag only (owner-only); this section renders
// exactly those and never a value. Absent or empty renders nothing.
export function InjectedEnvSection({ vars }: { vars: RunInjectedEnvVar[] | undefined }) {
  if (!vars || vars.length === 0) return null;
  return (
    <section aria-label="Variables injected" className="rounded-xl border border-edge bg-surface p-4">
      <h2 className="mb-2 text-sm font-semibold text-fg">Variables injected</h2>
      <p className="mb-2 text-xs text-muted">Set for this run&rsquo;s current attempt. Values are never shown here.</p>
      <ul className="space-y-1 text-xs">
        {vars.map(v => (
          <li key={`${v.scope}:${v.name}`} className="flex flex-wrap items-center gap-2">
            <span className="font-mono break-all text-fg">{stripUnsafeChars(v.name)}</span>
            <span className="text-muted">{stripUnsafeChars(v.scope)}</span>
            {v.secret && <Badge tone="warning">secret</Badge>}
          </li>
        ))}
      </ul>
    </section>
  );
}
