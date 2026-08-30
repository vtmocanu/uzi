// Update escalation banner (PRD #836 M6, surface 4 — the "pull-cord"): the loudest
// upstream-release surface, deliberately reserved for the far-behind / security case.
// It is ADMIN-ONLY and self-gating: a non-admin never fetches (the admin endpoint would
// 403), and even for an admin it renders nothing unless the banner toggle is on, the
// server derived far_behind, the snooze has not been set for the current release, and a
// latest_tag exists. Reading the notes is a LINK OUT — there is no ChangelogDrawer here
// (that component sources a build-time bundle that cannot hold entries newer than this
// build). The Dismiss action is a SERVER-SIDE snooze keyed to the release tag, so it
// clears itself when a newer release ships — never on a timer, never per-session.

import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, isHttpsUrl, type ReleaseCheckStatus } from "../lib/api";
import { displayVersion } from "./BuildInfoPopover";
import { Button, cx } from "./ui";

export function UpdateEscalationBanner() {
  const { user } = useAuth();
  const isAdmin = user?.is_admin === true;
  const [status, setStatus] = useState<ReleaseCheckStatus | null>(null);
  // Local optimistic hide on Dismiss, on top of the server-side snooze the refresh
  // reflects (banner_snoozed → true) — so the banner disappears the instant it is
  // dismissed even before the snooze round-trip returns.
  const [dismissed, setDismissed] = useState(false);
  const [snoozing, setSnoozing] = useState(false);

  useEffect(() => {
    // No fetch for non-admins: the admin endpoint 403s a member, and the banner is
    // admin-only anyway. Fetch once on mount; a load error is silent (non-critical nudge).
    if (!isAdmin) return;
    let cancelled = false;
    void (async () => {
      try {
        const { release_check } = await api.getReleaseCheck();
        if (!cancelled) setStatus(release_check);
      } catch {
        // A failed check is not worth an error surface — render nothing.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [isAdmin]);

  if (!isAdmin) return null;
  if (dismissed || !status) return null;

  // Self-gate: the banner toggle on AND not already snoozed for this release AND a
  // known latest_tag AND a newer release genuinely exists (update_available) AND the
  // escalation condition holds — far_behind OR a security release. The update_available
  // requirement is what guards the security arm: `security` is derived purely from the
  // release body and does NOT imply a newer release exists, so without it the banner
  // could claim a security release "is available" for the version already running.
  // far_behind already implies update_available server-side, so this only tightens the
  // new security arm.
  const show =
    status.release_check_banner_enabled === true &&
    status.banner_snoozed === false &&
    !!status.latest_tag &&
    status.update_available === true &&
    (status.far_behind === true || status.security === true);
  if (!show) return null;

  const version = displayVersion(status.latest_tag ?? "");
  const security = status.security === true;

  const dismiss = async () => {
    setSnoozing(true);
    // Optimistically hide right away; then refresh from the snooze response so
    // banner_snoozed is true if this component ever re-evaluates.
    setDismissed(true);
    try {
      const { release_check } = await api.snoozeReleaseBanner();
      setStatus(release_check);
    } catch {
      // The local hide already took effect; a failed snooze just means it may return on
      // the next mount — acceptable for a nudge.
    } finally {
      setSnoozing(false);
    }
  };

  return (
    <div
      role="alert"
      className={cx(
        "mb-6 rounded-lg border px-4 py-3",
        security ? "border-danger/40 bg-danger/10 text-danger" : "border-warn/40 bg-warn/10 text-warn",
      )}
    >
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="min-w-0">
          <p className="text-sm font-semibold text-fg">
            <span aria-hidden="true">{security ? "🔒" : "⚠"}</span>{" "}
            {security ? `Security release ${version} is available` : `A newer uzi (${version}) is available`}
          </p>
          <p className="mt-0.5 text-sm text-muted">
            {security
              ? "This is a security release and you're significantly behind — update at the next opportunity."
              : "You're significantly behind the latest release — update at the next opportunity."}
          </p>
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-3">
          {status.notes_url && isHttpsUrl(status.notes_url) && (
            <a
              href={status.notes_url}
              target="_blank"
              rel="noopener noreferrer"
              className="text-sm text-info hover:underline"
            >
              Release notes <span aria-hidden="true">&#8599;</span>
              <span className="sr-only">(opens in new tab)</span>
            </a>
          )}
          {/* Internal route → react-router Link (client-side nav), NOT a raw anchor
              (an M4 review finding). */}
          <Link to="/admin/settings" className="text-sm text-info hover:underline">
            Update guide
          </Link>
          <Button
            variant="secondary"
            size="sm"
            onClick={dismiss}
            disabled={snoozing}
            aria-label={`Dismiss update banner for ${status.latest_tag}`}
          >
            {snoozing ? "Dismissing…" : "Dismiss"}
          </Button>
        </div>
      </div>
    </div>
  );
}
