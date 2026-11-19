// Opt-in animated sidebar icons (issue #4510). Two halves live here:
//
//  - SIDEBAR_MOTION_ICONS: one SVG per NAV_SECTIONS key. Each copies the matching icons.tsx
//    glyph's markup verbatim (same svg attributes and path data) and adds `data-nav-motion`
//    plus class hooks the CSS section "Sidebar icon motion" in index.css animates. At rest the
//    glyph draws exactly like the icons.tsx original (a browser test rasterizes both and
//    compares pixels).
//      Copy rule: where a moving part shares a path with a stationary part, the original path
//      data stays byte-identical and visible at rest (splitting a path changes antialiasing,
//      which the pixel comparison catches), and the moving part is a separate copy with inline
//      opacity="0". While playing, the keyframes hide that original (class nm-orig) and show
//      the copies, so nothing is drawn twice. Other parts may move the original element itself.
//  - useNavIconMotion: the gesture hook. It only toggles the `nm-play` class on the row's svg;
//    every animation rule is gated in CSS by the root `data-motion="on"` attribute and by
//    prefers-reduced-motion, and this hook re-checks both before playing.

import { useCallback, useRef, type FocusEvent, type ReactElement, type ReactNode, type RefObject } from "react";

const PLAY_CLASS = "nm-play";

function glyph(key: string, children: ReactNode): ReactElement {
  return (
    <svg
      data-nav-motion={key}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      width="1em"
      height="1em"
      aria-hidden="true"
    >
      {children}
    </svg>
  );
}

// Keyed by NAV_SECTIONS key (navItems.tsx). The comment on each entry names the icons.tsx
// component it mirrors.
export const SIDEBAR_MOTION_ICONS: Record<string, () => ReactElement> = {
  // HomeIcon: the door is also drawn as a rest-invisible copy that fills with the accent.
  overview: () =>
    glyph(
      "overview",
      <>
        <path d="m3 9 9-7 9 7v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
        <path className="nm-orig" d="M9 22V12h6v10" />
        <path className="nm-door" d="M9 22V12h6v10" opacity="0" />
      </>,
    ),
  // BoardIcon: the joined column path stays the rest glyph; while playing it hides and two
  // rest-invisible column copies show and move.
  repos: () =>
    glyph(
      "repos",
      <>
        <rect x="3" y="3" width="18" height="18" rx="2" />
        <path className="nm-orig" d="M8 7v10M16 7v6" />
        <path className="nm-col-a" d="M8 7v10" opacity="0" />
        <path className="nm-col-b" d="M16 7v6" opacity="0" />
      </>,
    ),
  // ActivityIcon: a pathLength=1 copy retraces by dash offset.
  runs: () =>
    glyph(
      "runs",
      <>
        <path className="nm-orig" d="M22 12h-4l-3 9L9 3l-3 9H2" />
        <path className="nm-trace" d="M22 12h-4l-3 9L9 3l-3 9H2" pathLength="1" opacity="0" />
      </>,
    ),
  // BugIcon: the body paths never move; the six leg paths (already separate elements in the
  // original) are grouped, in their original order, per side.
  findings: () =>
    glyph(
      "findings",
      <>
        <path d="M12 20v-9" />
        <path d="M14 7a4 4 0 0 1 4 4v3a6 6 0 0 1-12 0v-3a4 4 0 0 1 4-4z" />
        <path d="M14.12 3.88 16 2" />
        <g className="nm-legs-r">
          <path d="M21 21a4 4 0 0 0-3.81-4" />
          <path d="M21 5a4 4 0 0 1-3.55 3.97" />
          <path d="M22 13h-4" />
        </g>
        <g className="nm-legs-l">
          <path d="M3 21a4 4 0 0 1 3.81-4" />
          <path d="M3 5a4 4 0 0 0 3.55 3.97" />
          <path d="M6 13H2" />
        </g>
        <path d="m8 2 1.88 1.88" />
        <path d="M9 7.13V6a3 3 0 1 1 6 0v1.13" />
      </>,
    ),
  // ClockIcon: the hands are ONE joined path (M12 7v5l3 2). It stays the rest glyph; while
  // playing it hides and two copies show, only the minute hand turning.
  schedules: () =>
    glyph(
      "schedules",
      <>
        <circle cx="12" cy="12" r="9" />
        <path className="nm-orig" d="M12 7v5l3 2" />
        <path className="nm-minute" d="M12 12V7" opacity="0" />
        <path className="nm-hour" d="M12 12l3 2" opacity="0" />
      </>,
    ),
  // ChatIcon: one moving element, so the original path itself carries the class.
  chat: () =>
    glyph(
      "chat",
      <path
        className="nm-bubble"
        d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8z"
      />,
    ),
  // BotIcon: the antenna and the two eye dots are already separate elements.
  agents: () =>
    glyph(
      "agents",
      <>
        <rect x="4" y="8" width="16" height="12" rx="2" />
        <path className="nm-antenna" d="M12 8V4M8 4h8" />
        <circle className="nm-eye" cx="9" cy="13" r="0.5" fill="currentColor" />
        <circle className="nm-eye" cx="15" cy="13" r="0.5" fill="currentColor" />
        <path d="M9 17h6" />
      </>,
    ),
  // SkillIcon: the big sparkle moves itself; the joined small-sparkle path stays the rest glyph
  // and, while playing, hides behind two rest-invisible copies that twinkle.
  skills: () =>
    glyph(
      "skills",
      <>
        <path
          className="nm-star"
          d="m12 3-1.9 5.8a2 2 0 0 1-1.3 1.3L3 12l5.8 1.9a2 2 0 0 1 1.3 1.3L12 21l1.9-5.8a2 2 0 0 1 1.3-1.3L21 12l-5.8-1.9a2 2 0 0 1-1.3-1.3z"
        />
        <path className="nm-orig" d="M5 3v4M3 5h4M19 17v4M17 19h4" />
        <path className="nm-mini" d="M5 3v4M3 5h4" opacity="0" />
        <path className="nm-mini nm-mini2" d="M19 17v4M17 19h4" opacity="0" />
      </>,
    ),
  // ServerIcon: the original LED path hides while playing; accent LED copies blink alone.
  workers: () =>
    glyph(
      "workers",
      <>
        <rect x="2" y="4" width="20" height="7" rx="2" />
        <rect x="2" y="13" width="20" height="7" rx="2" />
        <path className="nm-orig" d="M6 7.5h.01M6 16.5h.01" />
        <path className="nm-led nm-led1" d="M6 7.5h.01" opacity="0" />
        <path className="nm-led nm-led2" d="M6 16.5h.01" opacity="0" />
      </>,
    ),
  // ScaleIcon: the joined post+base+beam path stays the rest glyph. While playing it hides;
  // a stationary post copy shows and a beam copy rides in the same group as the two pans.
  judge: () =>
    glyph(
      "judge",
      <>
        <g className="nm-beam">
          <path d="m16 16 3-8 3 8c-.87.65-1.92 1-3 1s-2.13-.35-3-1z" />
          <path d="m2 16 3-8 3 8c-.87.65-1.92 1-3 1s-2.13-.35-3-1z" />
          <path className="nm-beam-bar" d="M3 7h2c2 0 5-1 7-2 2 1 5 2 7 2h2" opacity="0" />
        </g>
        <path className="nm-orig" d="M7 21h10M12 3v18M3 7h2c2 0 5-1 7-2 2 1 5 2 7 2h2" />
        <path className="nm-post" d="M7 21h10M12 3v18" opacity="0" />
      </>,
    ),
  // GearIcon: the hub circle and the cog turn together.
  settings: () =>
    glyph(
      "settings",
      <g className="nm-gear">
        <circle cx="12" cy="12" r="3" />
        <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
      </g>,
    ),
  // ShieldIcon: the shield swells; a rest-invisible check draws over it.
  admin: () =>
    glyph(
      "admin",
      <>
        <path className="nm-shield" d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
        <path className="nm-check" d="m9 12 2 2 4-4" pathLength="1" opacity="0" />
      </>,
    ),
  // BookIcon: both original paths stay put; the accent ribbon is a rest-invisible overlay.
  docs: () =>
    glyph(
      "docs",
      <>
        <path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20" />
        <path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z" />
        <path className="nm-ribbon" d="M14 2v7l1.5-1.3L17 9V2z" opacity="0" />
      </>,
    ),
};

// Whether a gesture may start motion now. The CSS gates repeat both checks; this keeps the
// class from being set at all, so nothing is left behind to replay when a preference flips.
function motionAllowed(): boolean {
  if (document.documentElement.dataset.motion !== "on") return false;
  try {
    if (typeof window.matchMedia === "function" && window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      return false;
    }
  } catch {
    // no usable matchMedia: treat as no reduce preference
  }
  return true;
}

// Only CSS keyframe animations count: the 60ms :active press transition on the svg is a
// CSSTransition and must not keep the play class alive.
function animationsLeft(svg: SVGElement): boolean {
  if (typeof svg.getAnimations !== "function") return false;
  const isCss = (a: Animation) => typeof CSSAnimation === "undefined" || a instanceof CSSAnimation;
  return svg.getAnimations({ subtree: true }).some(isCss);
}

// animationend/animationcancel bubble from the parts. The class goes once none of the svg's
// animations remain; where getAnimations is missing (jsdom) any such event clears it.
function onAnimationDone(event: Event) {
  const svg = event.currentTarget as SVGElement;
  if (typeof svg.getAnimations !== "function" || !animationsLeft(svg)) svg.classList.remove(PLAY_CLASS);
}

const wired = new WeakSet<SVGElement>();
let watching = false;

function stopAllPlaying() {
  for (const svg of Array.from(document.querySelectorAll(`svg.${PLAY_CLASS}`))) svg.classList.remove(PLAY_CLASS);
}

// Turning the setting off, or reduced motion on, mid-flight makes the CSS stop matching and
// cancels the animations, but a lingering class would replay them the moment the preference
// flipped back. Drop it as soon as either changes.
function watchPreferences() {
  if (watching) return;
  watching = true;
  if (typeof MutationObserver === "function") {
    // A record whose previous value was not "on" means the setting spent time off even if it
    // is back on by the time this callback runs, so the lingering class must still go.
    new MutationObserver((records) => {
      if (document.documentElement.dataset.motion !== "on" || records.some((r) => r.oldValue !== "on")) {
        stopAllPlaying();
      }
    }).observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["data-motion"],
      attributeOldValue: true,
    });
  }
  try {
    if (typeof window.matchMedia === "function") {
      window.matchMedia("(prefers-reduced-motion: reduce)").addEventListener("change", (e) => {
        if (e.matches) stopAllPlaying();
      });
    }
  } catch {
    // no usable matchMedia
  }
}

function play(host: HTMLElement | null) {
  const svg = host?.querySelector<SVGElement>("svg[data-nav-motion]");
  if (!svg || !motionAllowed()) return;
  watchPreferences();
  if (!wired.has(svg)) {
    wired.add(svg);
    svg.addEventListener("animationend", onAnimationDone);
    svg.addEventListener("animationcancel", onAnimationDone);
  }
  // Restart, never queue: drop the class, flush style, add it back.
  svg.classList.remove(PLAY_CLASS);
  void svg.getBoundingClientRect();
  svg.classList.add(PLAY_CLASS);
  // Nothing animated (a rule did not match): do not leave the class to replay later.
  if (typeof svg.getAnimations === "function" && !animationsLeft(svg)) svg.classList.remove(PLAY_CLASS);
}

export type NavIconMotion = {
  ref: RefObject<HTMLElement | null>;
  onPointerEnter: () => void;
  onFocus: (e: FocusEvent<HTMLElement>) => void;
  onClick: () => void;
};

// Attach `ref` to the row element that contains the glyph; the three handlers go on the same
// element. None of them calls preventDefault or delays navigation.
export function useNavIconMotion(): NavIconMotion {
  const ref = useRef<HTMLElement | null>(null);
  const onPointerEnter = useCallback(() => play(ref.current), []);
  const onClick = useCallback(() => play(ref.current), []);
  const onFocus = useCallback((e: FocusEvent<HTMLElement>) => {
    // Pointer-driven focus (a click) is covered by onClick; only keyboard focus plays here.
    let visible = false;
    try {
      visible = e.currentTarget.matches(":focus-visible");
    } catch {
      // :focus-visible unsupported
    }
    if (visible) play(ref.current);
  }, []);
  return { ref, onPointerEnter, onFocus, onClick };
}
