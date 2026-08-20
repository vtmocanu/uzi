// ChangelogDrawer — the in-app release-notes panel (PRD #415 M2). A LEFT slide-in
// overlay built ON the shared Modal shell, so the four dialog a11y behaviours
// (focus-in on open, Tab-trap, Escape close, focus-restore on close) and the
// backdrop click come from ONE correct implementation rather than being
// re-derived here. Modal centres a card by default; the className override below
// turns that backdrop into a full-screen scrim with the panel pinned to the left.
//
// M2 renders a MINIMAL but non-vacuous list: each release's version heading and
// date, sourced from the build-time-bundled `releases` (web/src/lib/changelog.ts).
// M3 enriches this IN PLACE with title markers, grouped bullets and PRD links —
// the per-release block below is deliberately shaped so that enrichment is an
// addition inside it, not a rewrite.

import { Modal } from "./Modal";
import { displayVersion } from "./BuildInfoPopover";
import { releases as bundledReleases } from "../lib/changelog";
import { XIcon } from "./icons";

export function ChangelogDrawer({
  open,
  onClose,
  version,
}: {
  open: boolean;
  onClose: () => void;
  // The instance's running version, so the matching release is flagged "current".
  // Optional: `undefined` simply flags nothing, which is the correct fail-safe
  // when the build-info fetch has not resolved.
  version?: string;
}) {
  // Mount the Modal only while open, like the existing modal consumers
  // (Repos.tsx / AdminBlockedRepos.tsx). The closed drawer costs nothing and the
  // Modal's focus-restore fires on unmount.
  if (!open) return null;

  return (
    <Modal
      label="Release notes"
      onClose={onClose}
      // Full-screen scrim, panel anchored LEFT (justify-start) instead of the
      // default centred card.
      className="fixed inset-0 z-50 flex justify-start bg-black/60"
    >
      <div
        className={
          // Left-anchored full-height column with its OWN scroll context, so the
          // body scrolls independently of the page behind the scrim. The slide
          // transition is disabled under prefers-reduced-motion.
          "flex h-full w-full max-w-md flex-col border-r border-edge bg-surface shadow-2xl " +
          "transition-transform duration-200 motion-reduce:transition-none"
        }
      >
        <header className="flex items-center justify-between gap-3 border-b border-edge px-5 py-4">
          <h2 className="text-base font-semibold text-fg">Release notes</h2>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close release notes"
            className="rounded-md p-1 text-muted transition-colors hover:bg-raised hover:text-fg"
          >
            <XIcon />
          </button>
        </header>

        <div data-testid="changelog-body" className="flex-1 overflow-y-auto px-5 py-4">
          {bundledReleases.length === 0 ? (
            <p className="text-sm text-faint">No release notes yet.</p>
          ) : (
            <ul className="space-y-4">
              {bundledReleases.map((release) => {
                const isCurrent = version !== undefined && release.version === version;
                return (
                  <li key={release.version} className="border-b border-edge pb-4 last:border-b-0">
                    <div className="flex items-baseline gap-2">
                      <h3 className="font-mono text-sm font-semibold text-fg">
                        {displayVersion(release.version)}
                      </h3>
                      {isCurrent && (
                        <span className="rounded-md border border-brand/40 bg-brand/10 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-brand">
                          current
                        </span>
                      )}
                    </div>
                    {release.date && (
                      <p className="mt-0.5 text-xs text-faint">{release.date}</p>
                    )}
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      </div>
    </Modal>
  );
}
