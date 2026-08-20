// The in-app changelog drawer (PRD #415 M2). A LEFT slide-in overlay built on the
// shared Modal shell, so it inherits the four dialog behaviours for free — focus
// moves in on open, Tab is trapped and wraps, Escape closes, and focus is restored
// to the trigger (the footer's Changelog button) on close.
//
// Modal's default className centres a card; here we OVERRIDE it with the backdrop +
// a LEFT-anchored panel. The ref/tabIndex/onKeyDown that make the trap work live on
// Modal's own container (the backdrop), so the focusable panel content must sit
// INSIDE that container — which it does, as Modal's `children`.
//
// M2 is deliberately minimal: a heading, a close control, and one version heading
// per release, enough for the scroll- and focus-trap tests. M3 enriches each entry
// with dates, release-title markers, the running-version marker, category groups
// and links; `runningVersion` is threaded now (carried on a data attribute) so that
// work has the value without another wiring pass.

import { XIcon } from "./icons";
import { Modal } from "./Modal";
import { displayVersion } from "./BuildInfoPopover";
import { releases } from "../lib/changelog";

export function ChangelogDrawer({
  runningVersion,
  onClose,
}: {
  // The version this instance is running (PRD #175's coordinate). Unused for
  // rendering in M2 — M3 marks the matching release — but threaded now and carried
  // on a data attribute so it is available without re-wiring. May be undefined.
  runningVersion?: string;
  onClose: () => void;
}) {
  return (
    <Modal
      label="Changelog"
      onClose={onClose}
      // The backdrop container. Flex so the panel anchors left and the empty space
      // to its right is the clickable backdrop (Modal closes on a mousedown whose
      // target IS this container).
      className="fixed inset-0 z-50 flex bg-black/60"
    >
      <div
        data-running-version={runningVersion}
        className={[
          // A LEFT slide-in panel: full height, bounded width, its own column.
          "relative flex h-full w-full max-w-md flex-col border-r border-edge bg-surface shadow-2xl",
          // Reduced-motion users get no transition (nothing animates the mount here
          // yet, but this keeps the panel honouring the preference as M3 adds it).
          "transition-transform motion-reduce:transition-none",
        ].join(" ")}
      >
        <div className="flex items-center justify-between border-b border-edge px-4 py-3">
          <h2 className="text-lg font-semibold tracking-tight text-fg">Changelog</h2>
          <button
            type="button"
            aria-label="Close changelog"
            onClick={onClose}
            className="rounded-md p-1 text-muted hover:bg-raised hover:text-fg"
          >
            <XIcon />
          </button>
        </div>
        {/* The body owns its OWN scroll (overflow-y-auto) so a long changelog scrolls
            independently of the header, which stays pinned. */}
        <div className="flex-1 overflow-y-auto px-4 py-3" data-testid="changelog-body">
          {releases.length === 0 ? (
            <p className="text-sm text-faint">No changelog entries.</p>
          ) : (
            <ul className="space-y-4">
              {releases.map((release) => (
                <li key={release.version}>
                  <h3 className="font-mono text-sm font-semibold text-fg">
                    {displayVersion(release.version)}
                  </h3>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </Modal>
  );
}
