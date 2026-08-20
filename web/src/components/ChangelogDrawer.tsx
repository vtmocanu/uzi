// ChangelogDrawer — the in-app release-notes panel (PRD #415). A LEFT slide-in
// overlay built ON the shared Modal shell, so the four dialog a11y behaviours
// (focus-in on open, Tab-trap, Escape close, focus-restore on close) and the
// backdrop click come from ONE correct implementation rather than being
// re-derived here. Modal centres a card by default; the className override below
// turns that backdrop into a full-screen scrim with the panel pinned to the left.
//
// The panel enters with a real LEFT SLIDE (M3): the inner DrawerPanel mounts
// translated fully off-screen left (`-translate-x-full`) and an on-mount effect
// flips it to `translate-x-0`, so the `transition-transform` runs each time the
// drawer opens. DrawerPanel is a child mounted only while `open`, so its effect
// fires on every open (the always-mounted ChangelogDrawer would fire it once).
// Under prefers-reduced-motion the transition is suppressed and the panel simply
// appears in place.
//
// M3 renders each release RICHLY, newest-first (the `releases` array already is):
// a version heading that links to its GitHub release tag, an optional title-marker
// subtitle, status-toned category groups, and bullets rendered as trusted inline
// markdown — with `PRD #N` references linkified (web-only, on the bullet text) to
// the repo's issues and styled quieter than the changelog's own PR links.

import { useEffect, useState } from "react";
import type { Components } from "react-markdown";
import { Modal } from "./Modal";
import { MarkdownCore } from "./MarkdownCore";
import { displayVersion } from "./BuildInfoPopover";
import { releases as bundledReleases } from "../lib/changelog";
import type { Release } from "../lib/changelog";
import { compareSemver, parseSemver } from "../lib/semver";
import { cx } from "./ui";
import { XIcon } from "./icons";

// Pinned repo base for release-tag and issue links, mirroring docs.ts hardcoding
// REPO_BLOB_BASE. Overridable at build time for a fork; resolved once here.
// Trailing slashes are trimmed so `${repoBase}/…` never doubles up.
const repoBase = (
  (import.meta.env.VITE_UZI_CHANGELOG_REPO_URL as string | undefined) ||
  "https://github.com/vtmocanu/uzi"
).replace(/\/+$/, "");

// Web-only, post-parse PRD linkify: turn the exact `PRD #<digits>` token into a
// markdown link to the repo's issue tracker, on the bullet TEXT before rendering.
// The CHANGELOG keeps `PRD #N` PLAIN (its own link script leaves it alone), so
// there is never an existing link to double-wrap. Cross-repo refs (`k8s #119593`)
// and bare `[#12](url)` PR links do not match `PRD #\d+` and are left untouched.
// This does NOT touch CHANGELOG.md or the parser's `body` — it operates on a copy
// of the bullet string at render time only (M4 parity compares raw `body`).
const PRD_REF_RE = /PRD #(\d+)/g;
function linkifyPrdRefs(text: string): string {
  return text.replace(PRD_REF_RE, (_m, n: string) => `[PRD #${n}](${repoBase}/issues/${n})`);
}

// Status-tone dot per Keep-a-Changelog category. Every stem here is a real token
// in tailwind.config.js (verified against `npm run check-styles`, which fails on a
// non-existent color stem): Added→ok, Changed→info (the accent/blue token that
// exists), Fixed→warn, Security→danger, Dependencies→faint. An unknown category
// falls back to the neutral `muted` token rather than rendering no dot.
const CATEGORY_DOT: Record<string, string> = {
  Added: "bg-ok",
  Changed: "bg-info",
  Fixed: "bg-warn",
  Security: "bg-danger",
  Dependencies: "bg-faint",
};
function categoryDot(category: string): string {
  return CATEGORY_DOT[category] ?? "bg-muted";
}

// Trusted-content link policy for MarkdownCore. The changelog is repo-authored, so
// unlike the untrusted `<Markdown>` wrapper this adds NO bidi-strip / LLM link
// policy and NO in-app routing — every changelog link is absolute and opens in a
// new tab. PRD-issue links (produced by linkifyPrdRefs, recognised by their
// `${repoBase}/issues/` prefix) render QUIETER than the changelog's own PR refs.
const markdownComponents: Components = {
  a({ href, children, node: _node, ...props }) {
    const isPrdRef = typeof href === "string" && href.startsWith(`${repoBase}/issues/`);
    return (
      <a
        {...props}
        href={href}
        target="_blank"
        rel="noopener noreferrer"
        className={cx(
          "underline underline-offset-2",
          isPrdRef ? "text-faint hover:text-muted" : "text-info hover:text-fg",
        )}
      >
        {children}
      </a>
    );
  },
};

export function ChangelogDrawer({
  open,
  onClose,
  version,
}: {
  open: boolean;
  onClose: () => void;
  // The instance's running version, so the matching release is flagged
  // "You're running this". Optional: `undefined` (or a non-semver `dev`/`demo`)
  // flags nothing and shows no banner, the correct fail-safe when the build-info
  // fetch has not resolved or the build carries no release version.
  version?: string;
}) {
  // Mount the Modal only while open, like the existing modal consumers
  // (Repos.tsx / AdminBlockedRepos.tsx). The closed drawer costs nothing and the
  // Modal's focus-restore fires on unmount. Mounting DrawerPanel only here is also
  // what makes the slide replay on every open (see DrawerPanel's enter effect).
  if (!open) return null;

  return (
    <Modal
      label="Release notes"
      onClose={onClose}
      // Full-screen scrim, panel anchored LEFT (justify-start) instead of the
      // default centred card.
      className="fixed inset-0 z-50 flex justify-start bg-black/60"
    >
      <DrawerPanel onClose={onClose} version={version} />
    </Modal>
  );
}

function DrawerPanel({ onClose, version }: { onClose: () => void; version?: string }) {
  // Two-phase enter for the left slide: this panel mounts translated fully
  // off-screen left, then the effect flips `entered` true after mount so the
  // `transition-transform` actually animates. DrawerPanel remounts on each open
  // (ChangelogDrawer returns null while closed), so this fires every open. Under
  // prefers-reduced-motion the transition is suppressed and it appears in place.
  const [entered, setEntered] = useState(false);
  useEffect(() => {
    setEntered(true);
  }, []);

  // Marker/banner state keys off the RUNNING version. A non-semver running
  // version (dev/demo/undefined) is not comparable, so parseSemver returns null,
  // markers and the banner are suppressed, and nothing is ever flagged "Newer".
  const running = version !== undefined ? parseSemver(version) : null;

  // Greatest RELEASED version on the changelog, for the "· vY available" clause
  // and the per-release "Newer" markers. `[Unreleased]`/`[NOT RELEASED]` sections
  // carry `released === false` and are excluded, so an unreleased section is never
  // the "available" target nor flagged current.
  let greatestReleased: string | null = null;
  if (running !== null) {
    for (const r of bundledReleases) {
      if (!r.released) continue;
      if (greatestReleased === null || compareSemver(r.version, greatestReleased) > 0) {
        greatestReleased = r.version;
      }
    }
  }

  const showAvailable =
    running !== null &&
    greatestReleased !== null &&
    version !== undefined &&
    compareSemver(greatestReleased, version) > 0;

  return (
    <div
      data-testid="changelog-panel"
      data-entered={entered ? "true" : "false"}
      className={cx(
        // Left-anchored full-height column with its OWN scroll context, so the
        // body scrolls independently of the page behind the scrim.
        "flex h-full w-full max-w-md flex-col border-r border-edge bg-surface shadow-2xl",
        // The left slide: off-screen until `entered` flips it in. The transition is
        // disabled under prefers-reduced-motion (the panel then appears in place).
        "transition-transform duration-200 motion-reduce:transition-none",
        entered ? "translate-x-0" : "-translate-x-full",
      )}
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

      {/* The running-version banner. Only shown for a comparable (semver) running
          version; the "· vY available" clause appears only when a strictly greater
          release exists. */}
      {running !== null && version !== undefined && (
        <div
          data-testid="changelog-banner"
          className="border-b border-edge bg-raised px-5 py-2 text-xs text-muted"
        >
          This instance runs{" "}
          <span className="font-mono font-semibold text-fg">{displayVersion(version)}</span>
          {showAvailable && greatestReleased !== null && (
            <>
              {" · "}
              <span className="font-mono font-semibold text-ok">
                {displayVersion(greatestReleased)}
              </span>{" "}
              available
            </>
          )}
        </div>
      )}

      <div data-testid="changelog-body" className="flex-1 overflow-y-auto px-5 py-4">
        {bundledReleases.length === 0 ? (
          <p className="text-sm text-faint">No release notes yet.</p>
        ) : (
          <ul className="space-y-6">
            {bundledReleases.map((release) => (
              <ReleaseItem
                key={release.version}
                release={release}
                running={running}
                runningVersion={version}
              />
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

function ReleaseItem({
  release,
  running,
  runningVersion,
}: {
  release: Release;
  running: [number, number, number] | null;
  runningVersion?: string;
}) {
  // Only a RELEASED version links to a GitHub release tag — that tag exists. An
  // `[Unreleased]` heading (non-semver) AND a staged `[NOT RELEASED]` section
  // (semver, but `released === false`, so its `vX.Y.Z` tag does not exist yet)
  // both stay plain text rather than linking to a 404.
  const tagHref = release.released ? `${repoBase}/releases/tag/v${release.version}` : null;

  // Markers only apply when the running version is comparable (semver). A released
  // section whose version equals the running one is "You're running this"; a
  // strictly-greater released section is "Newer". `released === false` sections are
  // never marked.
  const isCurrent =
    running !== null && release.released && runningVersion !== undefined && release.version === runningVersion;
  const isNewer =
    running !== null &&
    release.released &&
    runningVersion !== undefined &&
    compareSemver(release.version, runningVersion) > 0;

  return (
    <li className="border-b border-edge pb-6 last:border-b-0">
      <div className="flex flex-wrap items-baseline gap-2">
        <h3 className="font-mono text-sm font-semibold text-fg">
          {tagHref ? (
            <a
              href={tagHref}
              target="_blank"
              rel="noopener noreferrer"
              className="hover:underline"
            >
              {displayVersion(release.version)}
            </a>
          ) : (
            displayVersion(release.version)
          )}
        </h3>
        {isCurrent && (
          <span className="rounded-md border border-brand/40 bg-brand/10 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-brand">
            You're running this
          </span>
        )}
        {isNewer && (
          <span className="rounded-md border border-ok/40 bg-ok/10 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-ok">
            Newer
          </span>
        )}
      </div>
      {release.date && <p className="mt-0.5 text-xs text-faint">{release.date}</p>}
      {release.titleMarker && (
        <p className="mt-1 text-xs italic text-muted">{release.titleMarker}</p>
      )}

      {release.groups.length > 0 && (
        <div className="mt-3 space-y-3">
          {release.groups.map((group, gi) => (
            <div key={`${group.category}-${gi}`}>
              <div className="flex items-center gap-2">
                <span
                  data-testid="category-dot"
                  aria-hidden="true"
                  className={cx("inline-block h-1.5 w-1.5 shrink-0 rounded-full", categoryDot(group.category))}
                />
                <span className="text-xs font-semibold uppercase tracking-wide text-muted">
                  {group.category}
                </span>
              </div>
              <ul className="mt-1.5 space-y-1.5 pl-4">
                {group.bullets.map((bullet, bi) => (
                  <li key={bi} className="text-xs text-fg">
                    <MarkdownCore
                      className="changelog-bullet"
                      content={linkifyPrdRefs(bullet)}
                      components={markdownComponents}
                    />
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>
      )}
    </li>
  );
}
