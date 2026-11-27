// Animated sidebar icons with a personal opt-out (issue #4510). Two halves live here:
//
//  - SIDEBAR_MOTION_ICONS (one SVG per NAV_SECTIONS key) and MODULE_MOTION_ICONS (one per
//    module vocabulary icon, issue #4777, reached through sidebarMotionGlyph). Each copies the matching
//    icons.tsx glyph's markup verbatim (same svg attributes and path data) and adds `data-nav-motion`
//    plus class hooks the CSS section "Sidebar icon motion" in index.css animates. At rest the
//    glyph draws exactly like the icons.tsx original (a browser test rasterizes both and
//    compares pixels).
//      Copy rule: where a moving part shares a path with a stationary part, the original path
//      data stays byte-identical and visible at rest (splitting a path changes antialiasing,
//      which the pixel comparison catches), and the moving part is a separate copy with inline
//      opacity="0". While playing, the keyframes hide that original (class nm-orig) and show
//      the copies, so nothing is drawn twice. Other parts may move the original element itself.
//  - useNavIconMotion: the click hook. The icon plays on click only (Enter on a focused link
//    fires click, so keyboard activation plays; hover and focus alone play nothing). It only
//    toggles the `nm-play` class on the row's svg; every animation rule is gated in CSS by the
//    root `data-motion="on"` attribute and by prefers-reduced-motion, and this hook re-checks
//    both before playing.

import { useCallback, useRef, type ReactElement, type ReactNode, type RefObject } from "react";
import { isModuleIconName, type ModuleIconName } from "../modules/icons";

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

// The children of each built-in glyph, keyed by NAV_SECTIONS key (navItems.tsx). The comment
// on each entry names the icons.tsx component it mirrors. Both SIDEBAR_MOTION_ICONS and the
// module twins below wrap these, so a module's motion stays in lockstep with the built-in's.
const BUILTIN_PARTS = {
  // HomeIcon: the door is also drawn as a rest-invisible copy that fills with the accent.
  overview: (): ReactNode =>
    <>
      <path d="m3 9 9-7 9 7v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
      <path className="nm-orig" d="M9 22V12h6v10" />
      <path className="nm-door" d="M9 22V12h6v10" opacity="0" />
    </>,
  // BoardIcon: the joined column path stays the rest glyph; while playing it hides and two
  // rest-invisible column copies show and move.
  repos: (): ReactNode =>
    <>
      <rect x="3" y="3" width="18" height="18" rx="2" />
      <path className="nm-orig" d="M8 7v10M16 7v6" />
      <path className="nm-col-a" d="M8 7v10" opacity="0" />
      <path className="nm-col-b" d="M16 7v6" opacity="0" />
    </>,
  // ActivityIcon: a pathLength=1 copy retraces by dash offset.
  runs: (): ReactNode =>
    <>
      <path className="nm-orig" d="M22 12h-4l-3 9L9 3l-3 9H2" />
      <path className="nm-trace" d="M22 12h-4l-3 9L9 3l-3 9H2" pathLength="1" opacity="0" />
    </>,
  // BugIcon: the body paths never move; the six leg paths (already separate elements in the
  // original) are grouped, in their original order, per side.
  findings: (): ReactNode =>
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
  // ClockIcon: the hands are ONE joined path (M12 7v5l3 2). It stays the rest glyph; while
  // playing it hides and two copies show, only the minute hand turning.
  schedules: (): ReactNode =>
    <>
      <circle cx="12" cy="12" r="9" />
      <path className="nm-orig" d="M12 7v5l3 2" />
      <path className="nm-minute" d="M12 12V7" opacity="0" />
      <path className="nm-hour" d="M12 12l3 2" opacity="0" />
    </>,
  // ChatIcon: one moving element, so the original path itself carries the class.
  chat: (): ReactNode =>
    <path
      className="nm-bubble"
      d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8z"
    />,
  // BotIcon: the antenna and the two eye dots are already separate elements.
  agents: (): ReactNode =>
    <>
      <rect x="4" y="8" width="16" height="12" rx="2" />
      <path className="nm-antenna" d="M12 8V4M8 4h8" />
      <circle className="nm-eye" cx="9" cy="13" r="0.5" fill="currentColor" />
      <circle className="nm-eye" cx="15" cy="13" r="0.5" fill="currentColor" />
      <path d="M9 17h6" />
    </>,
  // SkillIcon: the big sparkle moves itself; the joined small-sparkle path stays the rest glyph
  // and, while playing, hides behind two rest-invisible copies that twinkle.
  skills: (): ReactNode =>
    <>
      <path
        className="nm-star"
        d="m12 3-1.9 5.8a2 2 0 0 1-1.3 1.3L3 12l5.8 1.9a2 2 0 0 1 1.3 1.3L12 21l1.9-5.8a2 2 0 0 1 1.3-1.3L21 12l-5.8-1.9a2 2 0 0 1-1.3-1.3z"
      />
      <path className="nm-orig" d="M5 3v4M3 5h4M19 17v4M17 19h4" />
      <path className="nm-mini" d="M5 3v4M3 5h4" opacity="0" />
      <path className="nm-mini nm-mini2" d="M19 17v4M17 19h4" opacity="0" />
    </>,
  // ServerIcon: the original LED path hides while playing; accent LED copies blink alone.
  workers: (): ReactNode =>
    <>
      <rect x="2" y="4" width="20" height="7" rx="2" />
      <rect x="2" y="13" width="20" height="7" rx="2" />
      <path className="nm-orig" d="M6 7.5h.01M6 16.5h.01" />
      <path className="nm-led nm-led1" d="M6 7.5h.01" opacity="0" />
      <path className="nm-led nm-led2" d="M6 16.5h.01" opacity="0" />
    </>,
  // ScaleIcon: the joined post+base+beam path stays the rest glyph. While playing it hides;
  // a stationary post copy shows and a beam copy rides in the same group as the two pans.
  judge: (): ReactNode =>
    <>
      <g className="nm-beam">
        <path d="m16 16 3-8 3 8c-.87.65-1.92 1-3 1s-2.13-.35-3-1z" />
        <path d="m2 16 3-8 3 8c-.87.65-1.92 1-3 1s-2.13-.35-3-1z" />
        <path className="nm-beam-bar" d="M3 7h2c2 0 5-1 7-2 2 1 5 2 7 2h2" opacity="0" />
      </g>
      <path className="nm-orig" d="M7 21h10M12 3v18M3 7h2c2 0 5-1 7-2 2 1 5 2 7 2h2" />
      <path className="nm-post" d="M7 21h10M12 3v18" opacity="0" />
    </>,
  // GearIcon: the hub circle and the cog turn together.
  settings: (): ReactNode =>
    <g className="nm-gear">
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
    </g>,
  // ShieldIcon: the shield swells; a rest-invisible check draws over it.
  admin: (): ReactNode =>
    <>
      <path className="nm-shield" d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
      <path className="nm-check" d="m9 12 2 2 4-4" pathLength="1" opacity="0" />
    </>,
  // BookIcon: both original paths stay put; the accent ribbon is a rest-invisible overlay.
  docs: (): ReactNode =>
    <>
      <path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20" />
      <path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z" />
      <path className="nm-ribbon" d="M14 2v7l1.5-1.3L17 9V2z" opacity="0" />
    </>,
} satisfies Record<string, () => ReactNode>;

type BuiltinKey = keyof typeof BUILTIN_PARTS;

// Keyed by NAV_SECTIONS key (navItems.tsx).
export const SIDEBAR_MOTION_ICONS: Record<string, () => ReactElement> = Object.fromEntries(
  (Object.keys(BUILTIN_PARTS) as BuiltinKey[]).map((key) => [key, () => glyph(key, BUILTIN_PARTS[key]())]),
);

// The motion key a runtime module entry's glyph carries (issue #4777). The module still only
// names a vocabulary icon; the host owns every glyph and its motion, one twin per icon.
export function moduleMotionKey(name: ModuleIconName): string {
  return `module-${name}`;
}

// A module twin that plays exactly its built-in sibling's motion (same parts, same CSS rules).
function twin(name: ModuleIconName, builtin: BuiltinKey): () => ReactElement {
  return () => glyph(moduleMotionKey(name), BUILTIN_PARTS[builtin]());
}

// LogoIcon's root (icons.tsx): a filled glyph, no stroke attributes.
function logoGlyph(key: string, children: ReactNode): ReactElement {
  return (
    <svg data-nav-motion={key} viewBox="0 0 24 24" fill="currentColor" width="1em" height="1em" aria-hidden="true">
      {children}
    </svg>
  );
}

// One motion per module vocabulary icon (modules/icons.tsx). Thirteen reuse a built-in
// destination's motion; eight are module-only. Each new one follows the copy rule above.
export const MODULE_MOTION_ICONS: Record<ModuleIconName, () => ReactElement> = {
  activity: twin("activity", "runs"),
  board: twin("board", "repos"),
  book: twin("book", "docs"),
  bot: twin("bot", "agents"),
  bug: twin("bug", "findings"),
  // ChartLineIcon: the trace is redrawn by a pathLength=1 copy (the original hides for that
  // stretch only), then the end dot, which moves itself, pops in.
  "chart-line": () =>
    glyph(
      moduleMotionKey("chart-line"),
      <>
        <path d="M3 3v18h18" />
        <path className="nm-orig" d="M7 15l4-4 3 3 5-6" />
        <path className="nm-draw" d="M7 15l4-4 3 3 5-6" pathLength="1" opacity="0" />
        <circle className="nm-dot" cx="19" cy="8" r="1" fill="currentColor" />
      </>,
    ),
  chat: twin("chat", "chat"),
  clock: twin("clock", "schedules"),
  // FactoryIcon: the joined window path hides while playing; three window copies light up.
  factory: () =>
    glyph(
      moduleMotionKey("factory"),
      <>
        <path d="M2 20a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2V8l-7 5V8l-7 5V4a2 2 0 0 0-2-2H4a2 2 0 0 0-2 2z" />
        <path className="nm-orig" d="M17 18h1M12 18h1M7 18h1" />
        <path className="nm-win3" d="M17 18h1" opacity="0" />
        <path className="nm-win2" d="M12 18h1" opacity="0" />
        <path className="nm-win1" d="M7 18h1" opacity="0" />
      </>,
    ),
  // FileTextIcon: the joined fold+lines path hides; a still fold copy shows and the lines copy
  // writes in.
  "file-text": () =>
    glyph(
      moduleMotionKey("file-text"),
      <>
        <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
        <path className="nm-orig" d="M14 2v6h6M16 13H8M16 17H8M10 9H8" />
        <path className="nm-fold" d="M14 2v6h6" opacity="0" />
        <path className="nm-lines" d="M16 13H8M16 17H8M10 9H8" pathLength="1" opacity="0" />
      </>,
    ),
  gear: twin("gear", "settings"),
  // GitIcon: a filled logo glyph; the mark itself pops and tilts.
  git: () =>
    logoGlyph(
      moduleMotionKey("git"),
      <path
        className="nm-mark"
        d="M23.546 10.93 13.067.452c-.604-.603-1.582-.603-2.188 0L8.708 2.627l2.76 2.76c.645-.215 1.377-.07 1.889.441.516.515.658 1.258.438 1.9l2.658 2.66c.645-.223 1.387-.078 1.9.435.721.72.721 1.884 0 2.604-.719.719-1.881.719-2.6 0-.539-.541-.674-1.337-.404-1.996L12.86 8.955v6.525c.176.086.342.203.488.348.713.721.713 1.883 0 2.6-.719.721-1.889.721-2.609 0-.719-.719-.719-1.879 0-2.598.182-.18.387-.316.605-.406V8.835c-.217-.091-.424-.222-.6-.401-.545-.545-.676-1.342-.396-2.009L7.636 3.7.45 10.881c-.6.605-.6 1.584 0 2.189l10.48 10.477c.604.604 1.582.604 2.186 0l10.43-10.43c.605-.603.605-1.582 0-2.187"
      />,
    ),
  home: twin("home", "overview"),
  // KeyIcon: one path, so it carries the class and swings about its ring.
  key: () =>
    glyph(
      moduleMotionKey("key"),
      <path
        className="nm-key"
        d="m21 2-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0 3 3L22 7l-3-3m-3.5 3.5L19 4"
      />,
    ),
  // PackageIcon: both paths ride in one group that hops.
  package: () =>
    glyph(
      moduleMotionKey("package"),
      <g className="nm-box">
        <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" />
        <path d="M3.27 6.96 12 12.01l8.73-5.05M12 22.08V12" />
      </g>,
    ),
  scale: twin("scale", "judge"),
  // SearchIcon: the lens and its handle sweep together.
  search: () =>
    glyph(
      moduleMotionKey("search"),
      <g className="nm-lens">
        <circle cx="11" cy="11" r="7" />
        <path d="m21 21-4.3-4.3" />
      </g>,
    ),
  server: twin("server", "workers"),
  shield: twin("shield", "admin"),
  skill: twin("skill", "skills"),
  // TerminalIcon: the joined prompt+cursor path hides; a still prompt copy shows and the
  // cursor copy blinks.
  terminal: () =>
    glyph(
      moduleMotionKey("terminal"),
      <>
        <path className="nm-orig" d="m4 17 6-6-6-6M12 19h8" />
        <path className="nm-prompt" d="m4 17 6-6-6-6" opacity="0" />
        <path className="nm-cursor" d="M12 19h8" opacity="0" />
      </>,
    ),
};

// The glyph a nav row plays for a motion key: a built-in destination's own key (a NAV_SECTIONS
// key), or `module-<icon>` for a runtime module entry. Anything else has no motion.
export function sidebarMotionGlyph(key: string): (() => ReactElement) | undefined {
  if (Object.prototype.hasOwnProperty.call(SIDEBAR_MOTION_ICONS, key)) return SIDEBAR_MOTION_ICONS[key];
  if (key.startsWith("module-")) {
    const name = key.slice("module-".length);
    if (isModuleIconName(name)) return MODULE_MOTION_ICONS[name];
  }
  return undefined;
}

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
  onClick: () => void;
};

// Attach `ref` to the row element that contains the glyph; `onClick` goes on the same element.
// The icon plays on click only: Enter on a focused link fires click, so keyboard activation
// plays too, while hover and focus alone play nothing. It never calls preventDefault or delays
// navigation.
export function useNavIconMotion(): NavIconMotion {
  const ref = useRef<HTMLElement | null>(null);
  const onClick = useCallback(() => play(ref.current), []);
  return { ref, onClick };
}
