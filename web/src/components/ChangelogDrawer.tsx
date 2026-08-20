// The in-app changelog drawer (PRD #415 M2 shell, M3 content). A LEFT slide-in
// overlay built on the shared Modal shell, so it inherits the four dialog
// behaviours for free — focus moves in on open, Tab is trapped and wraps, Escape
// closes, and focus is restored to the trigger (the footer's Changelog button) on
// close.
//
// Modal's default className centres a card; here we OVERRIDE it with the backdrop +
// a LEFT-anchored panel. The ref/tabIndex/onKeyDown that make the trap work live on
// Modal's own container (the backdrop), so the focusable panel content must sit
// INSIDE that container — which it does, as Modal's `children`.
//
// M3 enriches each entry: a version heading that links to its release tag (for a
// released semver version), the date + release-title marker subtitle, category
// groups with a status-tone dot, and per-release current/newer markers derived from
// `runningVersion`. A banner at the top of the body summarises running-vs-latest.
// All marker/banner logic is GUARDED on `runningVersion` parsing as plain semver
// (parseSemver): a `dev`/`demo`/absent version is NEUTRAL — no markers, no banner —
// so this never mis-labels a non-release build as "behind everything".

import type { Components } from "react-markdown";
import { XIcon } from "./icons";
import { Modal } from "./Modal";
import { MarkdownCore } from "./MarkdownCore";
import { cx } from "./ui";
import { displayVersion } from "./BuildInfoPopover";
import { releases, type Release } from "../lib/changelog";
import { parseSemver, compareSemver } from "../lib/semver";
import { linkifyPrdRefs, releaseTagUrl } from "../lib/changelogLinks";

// A release is eligible for a "current"/"newer" marker (and to be the banner's
// "available" target) ONLY when it is a real, released semver version. `[Unreleased]`
// and `[NOT RELEASED]` are never eligible — they carry no tag and describe work that
// is not (or not yet) a running version.
function isMarkerEligible(r: Release): boolean {
  return !r.unreleased && !r.notReleased && parseSemver(r.version) !== null;
}

// Category → status-dot tone. The dot classes are written as full literals (not
// `bg-${tone}`) so Tailwind's content scan and check-styles can both see them.
const CATEGORY_DOT: Record<string, string> = {
  Added: "bg-ok",
  Changed: "bg-info",
  Fixed: "bg-warn",
  Security: "bg-danger",
  Dependencies: "bg-faint",
};

// Bullet-markdown link policy. Bullets are repo-authored changelog lines, but they
// carry issue/PR links to GitHub, so every anchor opens in a new tab with
// `noopener`. A PRD-issue link (text `PRD #N`) renders in a QUIETER, muted style
// than a PR ref, per PRD Decision 3 — the PR number is the primary reference, the
// PRD is context. `{...props}` is spread FIRST so our href/target/rel/className win.
const bulletComponents: Components = {
  a({ href, children, node: _node, ...props }) {
    const label = Array.isArray(children) ? children.join("") : String(children ?? "");
    const isPrd = /^PRD #\d+$/.test(label.trim());
    return (
      <a
        {...props}
        href={href}
        target="_blank"
        rel="noopener noreferrer"
        className={isPrd ? "text-faint hover:text-muted" : "text-info hover:underline"}
      >
        {children}
      </a>
    );
  },
};

export function ChangelogDrawer({
  runningVersion,
  onClose,
}: {
  // The version this instance is running (PRD #175's coordinate). Drives the
  // current/newer markers and the banner when it parses as plain semver; a
  // `dev`/`demo`/absent value renders NO markers and NO banner. May be undefined.
  runningVersion?: string;
  onClose: () => void;
}) {
  const running = parseSemver(runningVersion);

  // The newest eligible (released semver) version, used for the banner's "available"
  // target. Computed by numeric semver compare so 0.10.0 beats 0.9.0.
  const eligibleVersions = releases.filter(isMarkerEligible).map((r) => r.version);
  const newestEligible =
    eligibleVersions.length > 0
      ? eligibleVersions.reduce((best, v) => (compareSemver(v, best) > 0 ? v : best))
      : null;

  // Banner: only when running parses as semver. A strictly-newer eligible release
  // makes it informative ("… available"); otherwise a neutral running line.
  let banner: string | null = null;
  if (running && runningVersion) {
    if (newestEligible && compareSemver(newestEligible, runningVersion) > 0) {
      banner = `This instance runs v${runningVersion} · v${newestEligible} available`;
    } else {
      banner = `Running v${runningVersion}`;
    }
  }

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
          // Reduced-motion users get no transition.
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
          {banner && (
            <div
              data-testid="changelog-banner"
              className="mb-3 rounded-md border border-edge bg-raised px-3 py-2 text-xs text-muted"
            >
              {banner}
            </div>
          )}
          {releases.length === 0 ? (
            <p className="text-sm text-faint">No changelog entries.</p>
          ) : (
            <ul className="space-y-6">
              {releases.map((release) => (
                <ReleaseEntry
                  key={release.version}
                  release={release}
                  running={running}
                  runningVersion={runningVersion}
                />
              ))}
            </ul>
          )}
        </div>
      </div>
    </Modal>
  );
}

function ReleaseEntry({
  release,
  running,
  runningVersion,
}: {
  release: Release;
  running: [number, number, number] | null;
  runningVersion?: string;
}) {
  const { version, date, titleMarker, groups, unreleased } = release;
  const hasTag = isMarkerEligible(release);
  const heading = unreleased ? "Unreleased" : displayVersion(version);

  // Marker: only when running parses as semver AND this release is eligible.
  let marker: { text: string; className: string } | null = null;
  if (running && runningVersion && hasTag) {
    const cmp = compareSemver(version, runningVersion);
    if (cmp === 0) {
      marker = {
        text: "You're running this",
        className: "border-ok/40 bg-ok/10 text-ok",
      };
    } else if (cmp > 0) {
      marker = {
        text: "Newer",
        className: "border-info/40 bg-info/10 text-info",
      };
    }
  }

  // Subtitle: date, then the optional release-title marker, joined on a dot.
  const subParts: string[] = [];
  if (date) subParts.push(date);
  if (titleMarker) subParts.push(titleMarker);

  return (
    <li>
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="font-mono text-sm font-semibold text-fg">
          {hasTag ? (
            <a
              href={releaseTagUrl(version)}
              target="_blank"
              rel="noopener noreferrer"
              className="hover:underline"
            >
              {heading}
            </a>
          ) : (
            heading
          )}
        </h3>
        {marker && (
          <span
            data-testid="release-marker"
            className={cx(
              "rounded border px-1.5 py-0.5 text-[10px] font-medium",
              marker.className,
            )}
          >
            {marker.text}
          </span>
        )}
      </div>
      {subParts.length > 0 && (
        <p className="mt-0.5 text-xs text-faint">{subParts.join(" · ")}</p>
      )}
      {groups.map((group) => (
        <div key={group.category} className="mt-3">
          <div className="flex items-center gap-1.5">
            <span
              aria-hidden="true"
              className={cx(
                "h-1.5 w-1.5 rounded-full",
                CATEGORY_DOT[group.category] ?? "bg-faint",
              )}
            />
            <span className="text-xs font-semibold uppercase tracking-wider text-muted">
              {group.category}
            </span>
          </div>
          <ul className="mt-1 space-y-1">
            {group.bullets.map((bullet, i) => (
              <li key={`${group.category}-${i}`} className="pl-3">
                <MarkdownCore
                  content={linkifyPrdRefs(bullet)}
                  components={bulletComponents}
                  className="text-sm text-muted [&_p]:m-0"
                />
              </li>
            ))}
          </ul>
        </div>
      ))}
    </li>
  );
}
