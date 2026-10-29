import { useEffect, useState } from "react";
import { Link } from "react-router-dom";

import { useHealthStatus } from "../lib/useAdminHealth";
import { SeverityShape } from "./healthSeverity";
import { Button } from "./ui";

export function HealthAttentionBanner() {
  const { doc } = useHealthStatus();
  const [dismissed, setDismissed] = useState<Set<string>>(() => new Set());

  useEffect(() => {
    if (!doc) return;
    // Missing checks or metadata do not resolve a warning. Only an explicitly
    // observed known non-Warn severity in the last successful document does.
    const resolved = new Set(doc.checks
      .filter((check) => ["ok", "unknown", "danger", "na"].includes(check.severity))
      .map((check) => check.id));
    setDismissed((previous) => {
      if (![...previous].some((id) => resolved.has(id))) return previous;
      return new Set([...previous].filter((id) => !resolved.has(id)));
    });
  }, [doc]);

  const checks = doc?.checks.filter((check) =>
    check.attention_banner === true && check.scope === "owner" &&
    check.severity === "warn" && !dismissed.has(check.id),
  ) ?? [];
  if (checks.length === 0) return null;

  return (
    <div
      role="status"
      aria-live="polite"
      aria-label="Admin health attention"
      className="mb-6 rounded-lg border border-warn/40 bg-warn/10 px-4 py-3"
    >
      <p className="text-sm font-semibold text-fg">
        <SeverityShape severity="warn" className="mr-1.5 align-middle text-warn" />
        Health checks need attention
      </p>
      <ul className="mt-2 space-y-2">
        {checks.map((check) => (
          <li key={check.id} className="flex items-start justify-between gap-3">
            <div className="min-w-0 text-sm">
              <p className="font-semibold text-fg">{check.title}</p>
              <p className="text-muted">{check.summary}</p>
            </div>
            <Button
              variant="secondary"
              size="sm"
              aria-label={`Dismiss ${check.title}`}
              onClick={() => setDismissed((previous) => new Set([...previous, check.id]))}
            >
              Dismiss
            </Button>
          </li>
        ))}
      </ul>
      <Link to="/admin/health" className="mt-2 inline-block text-sm text-info hover:underline">
        Open health
      </Link>
    </div>
  );
}
