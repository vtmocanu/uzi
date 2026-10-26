// App-wide maintenance-mode notice (PRD #3454). Mounted once in AppShell's banner stack, beside
// VaultLockedBanner, for EVERY signed-in user: while an admin has paused intake, anyone who
// starts a run should know why it sits in the queue. INFO tone, not danger: nothing is broken,
// the instance is draining on purpose. Reads the one shared MaintenanceProvider poll and
// renders nothing while maintenance is off, before the first load, or when the read failed.
//
// The reason is admin-authored free text rendered as a single plain-text cell
// (stripDisplayCell, React-escaped), never as HTML.

import { Link } from "react-router-dom";

import { useAuth } from "../auth/AuthContext";
import { formatLocalTime } from "../lib/budget";
import { stripDisplayCell } from "../lib/safeText";
import { useMaintenance } from "../lib/useMaintenance";

// The pause mark: two bars, the shape of "intake paused". Decorative; the heading names it.
function PauseMark() {
  return (
    <svg viewBox="0 0 16 16" className="mt-0.5 h-4 w-4 shrink-0 text-info" aria-hidden="true">
      <circle cx="8" cy="8" r="7" fill="none" stroke="currentColor" strokeWidth="1.5" />
      <rect x="5.5" y="5" width="1.75" height="6" rx="0.5" fill="currentColor" />
      <rect x="8.75" y="5" width="1.75" height="6" rx="0.5" fill="currentColor" />
    </svg>
  );
}

export function MaintenanceBanner() {
  const { status } = useMaintenance();
  const { user } = useAuth();
  if (!status?.active) return null;

  const reason = stripDisplayCell(status.reason).trim();
  const since = formatLocalTime(status.since);

  return (
    <div
      role="status"
      // Scoped handle: RateLimitAnnouncer owns another role="status", so tests and assistive
      // tech find this region by its label.
      aria-label="Maintenance mode"
      className="mb-6 rounded-lg border border-info/40 bg-info/10 px-4 py-3"
    >
      <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between sm:gap-4">
        <div className="flex min-w-0 gap-2.5">
          <PauseMark />
          <div className="min-w-0">
            <p className="text-sm font-semibold text-fg">
              uzi is in maintenance mode{since && <span className="font-normal text-muted"> since {since}</span>}
            </p>
            <p className="mt-0.5 text-sm text-muted">
              New runs wait in the queue until it ends; running work finishes.
            </p>
            {reason !== "" && (
              <p className="mt-1 break-words text-sm text-fg">
                <span className="text-muted">Reason: </span>
                {reason}
              </p>
            )}
          </div>
        </div>
        {user?.is_admin === true && (
          <Link
            to="/admin/settings#maintenance"
            className="shrink-0 pl-6 text-sm text-info hover:underline focus-visible:underline sm:pl-0"
          >
            Manage maintenance
          </Link>
        )}
      </div>
    </div>
  );
}
