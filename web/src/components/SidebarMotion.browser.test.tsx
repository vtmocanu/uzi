/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { cdp, userEvent } from "vitest/browser";
import { act, cleanup, render } from "@testing-library/react";
import { useEffect, type ReactElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter, useNavigate, type NavigateFunction } from "react-router-dom";
import { NavItem } from "./AppShell";
import { NAV_SECTIONS, type NavDef } from "./navItems";
import { SIDEBAR_MOTION_ICONS } from "./sidebarMotion";
import { ClockIcon } from "./icons";
import type { HealthDoc } from "../lib/api";

// Issue #4510: the opt-in animated sidebar icons, against the real stylesheet in Chromium.
// jsdom cannot run CSS animations, rasterize SVG or emulate reduced motion, so the contract
// (gating, one-shot restart, stationary geometry, rest pixels) is proven here.

const DEFS: NavDef[] = NAV_SECTIONS.flatMap((s) => s.items);
type Rail = "expanded" | "collapsed";
const RAILS: Rail[] = ["expanded", "collapsed"];
const PLAY = "nm-play";
const root = document.documentElement;

function setMotion(v: "on" | "off") {
  root.dataset.motion = v;
}

async function emulateReducedMotion(reduce: boolean) {
  await cdp().send("Emulation.setEmulatedMedia", {
    features: [{ name: "prefers-reduced-motion", value: reduce ? "reduce" : "" }],
  });
}

// Pointer coordinates for CDP mouse events are page coordinates; the test runs in an iframe.
function pagePoint(el: Element) {
  const r = el.getBoundingClientRect();
  const frame = window.frameElement as HTMLElement | null;
  const f = frame?.getBoundingClientRect();
  // The runner may scale the iframe: map iframe-local CSS px to the page.
  const scale = frame && frame.offsetWidth ? (f?.width ?? 0) / frame.offsetWidth : 1;
  return {
    x: (f?.x ?? 0) + (r.x + r.width / 2) * scale,
    y: (f?.y ?? 0) + (r.y + r.height / 2) * scale,
  };
}

const frames = async (n = 2) => {
  for (let i = 0; i < n; i++) await new Promise((r) => requestAnimationFrame(() => r(null)));
};

// Every animation or transition running on the svg or below it.
const allAnims = (svg: Element) => svg.getAnimations({ subtree: true });
// CSS keyframe animations only: the 60ms press transition on an :active svg is not a play.
const cssAnims = (svg: Element) => allAnims(svg).filter((a): a is CSSAnimation => a instanceof CSSAnimation);
const endTimeOf = (a: Animation) => Number(a.effect?.getComputedTiming().endTime);

function seek(svg: Element, ms: number) {
  for (const a of cssAnims(svg)) {
    a.pause();
    a.currentTime = ms;
  }
}

const nav: { go: NavigateFunction | null } = { go: null };
function CaptureNavigate() {
  const go = useNavigate();
  useEffect(() => {
    nav.go = go;
  }, [go]);
  return null;
}

const healthCounts = { ok: 0, warn: 1, danger: 1, unknown: 0, na: 0 } as HealthDoc["counts"];

function row(def: NavDef, rail: Rail, extra: Record<string, unknown> = {}): ReactElement {
  return (
    <NavItem
      to={def.to}
      label={def.label}
      icon={def.icon}
      motionKey={def.key}
      exactOnly={def.exactOnly}
      collapsed={rail === "collapsed"}
      {...extra}
    />
  );
}

function shell(children: ReactElement): ReactElement {
  return (
    <MemoryRouter initialEntries={["/start"]}>
      <CaptureNavigate />
      <div style={{ padding: 60, width: 260 }}>{children}</div>
    </MemoryRouter>
  );
}

function mount(key: string, rail: Rail, extra: Record<string, unknown> = {}) {
  const def = DEFS.find((d) => d.key === key)!;
  const utils = render(shell(row(def, rail, extra)));
  const link = utils.container.querySelector("a") as HTMLAnchorElement;
  const svg = link.querySelector("svg[data-nav-motion]") as SVGSVGElement;
  return { ...utils, def, link, svg, rerenderRow: (e: Record<string, unknown> = {}) => utils.rerender(shell(row(def, rail, e))) };
}

async function hover(link: HTMLElement) {
  await userEvent.unhover(document.body);
  await userEvent.hover(link);
}

async function awaitRest(svg: Element) {
  await expect.poll(() => svg.classList.contains(PLAY), { timeout: 4000 }).toBe(false);
}

beforeEach(async () => {
  setMotion("on");
  await emulateReducedMotion(false);
  await userEvent.unhover(document.body);
});

afterEach(async () => {
  cleanup();
  root.removeAttribute("data-motion");
  nav.go = null;
  await emulateReducedMotion(false);
});

describe.each(RAILS)("%s rail: gestures start real CSS animations", (rail) => {
  it("hover, keyboard focus and click each play, finish within 0.7s-1.8s, then repeat", { timeout: 120000 }, async () => {
    for (const def of DEFS) {
      const gestures: Array<[string, (m: ReturnType<typeof mount>) => Promise<void>]> = [
        ["hover", async (m) => hover(m.link)],
        ["focus", async () => userEvent.tab()],
        ["click", async (m) => m.link.click()],
      ];
      for (const [name, gesture] of gestures) {
        const m = mount(def.key, rail);
        await gesture(m);
        const first = cssAnims(m.svg);
        expect(first.length, `${def.key} ${name}`).toBeGreaterThan(0);
        if (name === "hover") {
          // One-shot, bounded: finite, fill none, whole icon between 0.7s and 1.8s.
          let total = 0;
          for (const a of first) {
            const t = a.effect!.getComputedTiming();
            expect(Number.isFinite(Number(t.endTime)), `${def.key} infinite`).toBe(true);
            expect(t.fill, `${def.key} fill`).toBe("none");
            total = Math.max(total, Number(t.endTime));
          }
          expect(total, `${def.key} total`).toBeGreaterThanOrEqual(700);
          expect(total, `${def.key} total`).toBeLessThanOrEqual(1800);
        }
        for (const a of allAnims(m.svg)) a.finish();
        await awaitRest(m.svg);
        expect(allAnims(m.svg), `${def.key} ${name} rest`).toHaveLength(0);
        // The same gesture again runs again.
        if (name === "focus") {
          // After a scripted blur Tab continues from the link's sequential focus position and
          // would not land on it again. Park focus on a sentinel that precedes the row, then Tab.
          const sentinel = document.createElement("button");
          document.body.prepend(sentinel);
          try {
            sentinel.focus();
            await userEvent.tab();
            expect(document.activeElement, `${def.key} focus repeat lands on the link`).toBe(m.link);
          } finally {
            sentinel.remove();
          }
        } else {
          await gesture(m);
        }
        expect(cssAnims(m.svg).length, `${def.key} ${name} repeat`).toBeGreaterThan(0);
        cleanup();
        await userEvent.unhover(document.body);
      }
    }
  });

  it("restarts a mid-flight gesture: fresh animations near time zero, no queue", { timeout: 120000 }, async () => {
    for (const def of DEFS) {
      const m = mount(def.key, rail);
      await hover(m.link);
      const before = cssAnims(m.svg);
      expect(before.length, def.key).toBeGreaterThan(0);
      seek(m.svg, 350);
      await userEvent.click(m.link);
      const after = cssAnims(m.svg);
      expect(after.length, `${def.key} single set`).toBe(before.length);
      for (const a of after) {
        expect(before.includes(a), `${def.key} replaced`).toBe(false);
        expect(Number(a.currentTime), `${def.key} restarted`).toBeLessThan(300);
      }
      cleanup();
      await userEvent.unhover(document.body);
    }
  });

  it("clears the play class when the last animation ends during a press (the :active transition does not count)", { timeout: 60000 }, async () => {
    const m = mount("overview", rail);
    await hover(m.link);
    expect(cssAnims(m.svg).length).toBeGreaterThan(0);
    // Test-only: stretch the :active transition (an inline duration outranks the stylesheet's
    // 60ms) so it is reliably still running after the CDP press round trip on a slow host.
    m.svg.style.transitionDuration = "10s";
    const { x, y } = pagePoint(m.link);
    await cdp().send("Input.dispatchMouseEvent", { type: "mousePressed", x, y, button: "left", clickCount: 1 });
    try {
      expect(m.link.matches(":active"), "link is :active").toBe(true);
      // Held state is applied while the pointer is down.
      expect(getComputedStyle(m.svg).transform, "the :active dip is applied while held").not.toBe("none");
      // Hold the :active transition running while the keyframe animations finish, so the
      // test proves the transition is not counted toward "last animation ended".
      const transition = allAnims(m.svg).find((a) => a instanceof CSSTransition) as CSSTransition | undefined;
      expect(transition, "the :active CSSTransition exists").toBeDefined();
      expect(transition!.transitionProperty, "it is the press transform transition").toBe("transform");
      transition!.pause();
      for (const a of cssAnims(m.svg)) a.finish();
      await awaitRest(m.svg);
      expect(allAnims(m.svg).includes(transition!), "the transition is still alive").toBe(true);
    } finally {
      await cdp().send("Input.dispatchMouseEvent", { type: "mouseReleased", x, y, button: "left", clickCount: 1 });
    }
  });

  it("lets a moving glyph overflow its box only while it plays", { timeout: 30000 }, async () => {
    const m = mount("chat", rail);
    expect(getComputedStyle(m.svg).overflow, "rest").toBe("hidden");
    await hover(m.link);
    expect(cssAnims(m.svg).length).toBeGreaterThan(0);
    expect(getComputedStyle(m.svg).overflow, "during play").toBe("visible");
    for (const a of allAnims(m.svg)) a.finish();
    await awaitRest(m.svg);
    expect(getComputedStyle(m.svg).overflow, "back at rest").toBe("hidden");
  });

  it("plays nothing with data-motion off", { timeout: 120000 }, async () => {
    setMotion("off");
    for (const def of DEFS) {
      const m = mount(def.key, rail);
      await hover(m.link);
      m.link.click();
      await userEvent.click(m.link);
      await userEvent.tab();
      expect(allAnims(m.svg), def.key).toHaveLength(0);
      expect(m.svg.classList.contains(PLAY), def.key).toBe(false);
      // Press and hold: the :active dip is part of the motion setting, so with it off the svg
      // neither transforms nor transitions while the pointer is down.
      const { x, y } = pagePoint(m.link);
      await cdp().send("Input.dispatchMouseEvent", { type: "mouseMoved", x, y });
      await cdp().send("Input.dispatchMouseEvent", { type: "mousePressed", x, y, button: "left", clickCount: 1 });
      try {
        expect(m.link.matches(":active"), `${def.key} link is :active`).toBe(true);
        expect(getComputedStyle(m.svg).transform, `${def.key} held`).toBe("none");
        expect(allAnims(m.svg), `${def.key} held animations`).toHaveLength(0);
      } finally {
        await cdp().send("Input.dispatchMouseEvent", { type: "mouseReleased", x, y, button: "left", clickCount: 1 });
      }
      cleanup();
      await userEvent.unhover(document.body);
    }
  });

  it("plays nothing under prefers-reduced-motion, and leaves nothing to replay afterwards", { timeout: 120000 }, async () => {
    await emulateReducedMotion(true);
    for (const def of DEFS) {
      const m = mount(def.key, rail);
      await hover(m.link);
      m.link.click();
      await userEvent.tab();
      expect(allAnims(m.svg), `${def.key} reduced`).toHaveLength(0);
      await emulateReducedMotion(false);
      await frames();
      expect(allAnims(m.svg), `${def.key} switched back`).toHaveLength(0);
      expect(m.svg.classList.contains(PLAY), `${def.key} class`).toBe(false);
      await emulateReducedMotion(true);
      cleanup();
      await userEvent.unhover(document.body);
    }
  });

  it("the stylesheet itself gates on prefers-reduced-motion: a hand-added play class animates nothing", async () => {
    await emulateReducedMotion(true);
    for (const def of DEFS) {
      const m = mount(def.key, rail);
      // Bypass the hook: only the CSS media query can stop this.
      m.svg.classList.add(PLAY);
      await frames();
      expect(allAnims(m.svg), `${def.key} hand-added`).toHaveLength(0);
      m.svg.classList.remove(PLAY);
      cleanup();
    }
  });

  it("cancels at once when the setting turns off mid-flight and does not replay when it is back on", { timeout: 120000 }, async () => {
    for (const def of DEFS) {
      const m = mount(def.key, rail);
      await hover(m.link);
      expect(cssAnims(m.svg).length, def.key).toBeGreaterThan(0);
      setMotion("off");
      expect(allAnims(m.svg), `${def.key} off`).toHaveLength(0);
      setMotion("on");
      await frames();
      expect(allAnims(m.svg), `${def.key} back on`).toHaveLength(0);
      expect(m.svg.classList.contains(PLAY), `${def.key} class`).toBe(false);
      cleanup();
      await userEvent.unhover(document.body);
    }
  });

  it("never animates without a gesture: rerender, navigation, off to on, also after a finished play", async () => {
    for (const def of [DEFS[0]!, DEFS.find((d) => d.key === "schedules")!]) {
      const m = mount(def.key, rail);
      expect(allAnims(m.svg), "mount").toHaveLength(0);
      m.rerenderRow({ badge: 3, badgeLabel: "pending" });
      await frames();
      expect(allAnims(m.svg), "rerender").toHaveLength(0);
      await act(async () => nav.go!("/elsewhere"));
      await act(async () => nav.go!(def.to));
      await frames();
      expect(allAnims(m.svg), "navigation").toHaveLength(0);
      setMotion("off");
      await frames();
      setMotion("on");
      await frames();
      expect(allAnims(m.svg), "off then on").toHaveLength(0);

      await hover(m.link);
      expect(cssAnims(m.svg).length).toBeGreaterThan(0);
      for (const a of allAnims(m.svg)) a.finish();
      await awaitRest(m.svg);
      m.rerenderRow();
      await act(async () => nav.go!("/elsewhere"));
      setMotion("off");
      await frames();
      setMotion("on");
      await frames();
      expect(allAnims(m.svg), "after a finished play").toHaveLength(0);
      cleanup();
      await userEvent.unhover(document.body);
    }
  });
});

describe("collapsed rail: wrapper and dots stay still", () => {
  it("keeps the badge dot, the severity pip and the icon wrapper unmoved while the svg plays", async () => {
    const cases: Array<[string, Record<string, unknown>]> = [
      ["runs", { badge: 4, badgeLabel: "in progress" }],
      ["admin", { healthPip: healthCounts }],
    ];
    for (const [key, extra] of cases) {
      const m = mount(key, "collapsed", extra);
      const wrapper = m.svg.parentElement as HTMLElement;
      const dot = wrapper.querySelector('span[aria-hidden="true"]') as HTMLElement;
      expect(dot, `${key} dot`).not.toBeNull();
      const rectOf = (el: Element) => {
        const r = el.getBoundingClientRect();
        return [r.x, r.y, r.width, r.height].map((n) => Math.round(n * 100) / 100);
      };
      const dotBefore = rectOf(dot);
      const wrapperBefore = rectOf(wrapper);
      expect(getComputedStyle(wrapper).transform).toBe("none");

      await hover(m.link);
      expect(cssAnims(m.svg).length, key).toBeGreaterThan(0);
      for (const t of [100, 400, 700]) {
        seek(m.svg, t);
        expect(rectOf(dot), `${key} dot @${t}`).toEqual(dotBefore);
        expect(rectOf(wrapper), `${key} wrapper @${t}`).toEqual(wrapperBefore);
        expect(getComputedStyle(wrapper).transform, `${key} wrapper transform`).toBe("none");
        expect(getComputedStyle(dot).transform, `${key} dot transform`).toBe("none");
      }
      expect(wrapper.getAnimations(), `${key} wrapper animations`).toHaveLength(0);
      expect(dot.getAnimations(), `${key} dot animations`).toHaveLength(0);
      cleanup();
      await userEvent.unhover(document.body);
    }
  });
});

describe("static icons never animate", () => {
  it("a ClockIcon in a button and the NAV_SECTIONS icons stay still under hover, focus and click", async () => {
    setMotion("on");
    const { container } = render(
      <div style={{ padding: 60 }}>
        <button type="button" id="clock">
          <ClockIcon />
        </button>
        {DEFS.map((d) => (
          <button type="button" key={d.key} data-k={d.key}>
            {d.icon}
          </button>
        ))}
      </div>,
    );
    const buttons = Array.from(container.querySelectorAll("button"));
    expect(buttons).toHaveLength(DEFS.length + 1);
    for (const b of buttons) {
      expect(b.querySelector("[data-nav-motion]")).toBeNull();
      await hover(b);
      b.focus();
      await userEvent.tab({ shift: true });
      await userEvent.tab();
      await userEvent.click(b);
      expect(b.getAnimations({ subtree: true }).filter((a) => a instanceof CSSAnimation)).toHaveLength(0);
      expect(b.querySelector(`.${PLAY}`)).toBeNull();
    }
  });
});

const SIZE = 96;
async function rasterize(markup: string): Promise<Uint8ClampedArray> {
  const holder = document.createElement("div");
  holder.innerHTML = markup;
  const svg = holder.querySelector("svg")!;
  svg.setAttribute("width", String(SIZE));
  svg.setAttribute("height", String(SIZE));
  svg.setAttribute("stroke", "rgb(26, 43, 60)");
  svg.setAttribute("style", "color: rgb(26, 43, 60)");
  const xml = new XMLSerializer().serializeToString(svg);
  const url = URL.createObjectURL(new Blob([xml], { type: "image/svg+xml;charset=utf-8" }));
  try {
    const img = new Image();
    img.src = url;
    await img.decode();
    const canvas = document.createElement("canvas");
    canvas.width = SIZE;
    canvas.height = SIZE;
    const ctx = canvas.getContext("2d")!;
    ctx.fillStyle = "#fff";
    ctx.fillRect(0, 0, SIZE, SIZE);
    ctx.drawImage(img, 0, 0, SIZE, SIZE);
    return ctx.getImageData(0, 0, SIZE, SIZE).data;
  } finally {
    URL.revokeObjectURL(url);
  }
}

describe("rest geometry", () => {
  it("each sidebar glyph rasterizes pixel-identically to its icons.tsx original", { timeout: 60000 }, async () => {
    // The original is each live NAV_SECTIONS def.icon (the icons.tsx element the sidebar used
    // before this feature), so swapping an icon in navItems.tsx without updating the sidebar
    // glyph fails here.
    expect(Object.keys(SIDEBAR_MOTION_ICONS).sort()).toEqual(DEFS.map((d) => d.key).sort());
    // rasterize() forces both SVGs to SIZE x SIZE, so first pin the intrinsic sizing it would
    // hide (a 1em -> 2em width change would otherwise widen the rail icon unnoticed).
    const sizing = (markup: string) => {
      const holder = document.createElement("div");
      holder.innerHTML = markup;
      const svg = holder.querySelector("svg")!;
      return ["width", "height", "viewBox"].map((k) => `${k}=${svg.getAttribute(k)}`).join(" ");
    };
    for (const def of DEFS) {
      const origMarkup = renderToStaticMarkup(def.icon as ReactElement);
      const glyphMarkup = renderToStaticMarkup(SIDEBAR_MOTION_ICONS[def.key]!());
      expect(sizing(glyphMarkup), `${def.key} intrinsic size`).toBe(sizing(origMarkup));
      const a = await rasterize(origMarkup);
      const b = await rasterize(glyphMarkup);
      let inked = 0;
      for (let i = 0; i < a.length; i += 4) if (a[i] !== 255 || a[i + 1] !== 255 || a[i + 2] !== 255) inked++;
      expect(inked, `${def.key} draws something`).toBeGreaterThan(50);
      let mismatch = -1;
      for (let i = 0; i < a.length; i++) {
        if (a[i] !== b[i]) {
          mismatch = i;
          break;
        }
      }
      const detail = mismatch < 0 ? "" : `byte ${mismatch}: original ${a[mismatch]} vs sidebar ${b[mismatch]}`;
      expect(detail, `${def.key} rest pixels`).toBe("");
    }
  });
});

describe("stationary geometry while a part moves", () => {
  const transformKeyframes = (a: Animation) =>
    (a.effect as KeyframeEffect).getKeyframes().some((k) => "transform" in k && k.transform !== undefined);
  // getBoundingClientRect includes CSS transforms; getBBox ignores them and could not fail here.
  const box = (el: SVGGraphicsElement) => {
    const b = el.getBoundingClientRect();
    return [b.x, b.y, b.width, b.height];
  };
  const q = (svg: Element, sel: string) => svg.querySelector(sel) as SVGGraphicsElement;

  it("schedules: the hour hand never transforms, the minute hand steps through 12 positions, the joined original hides", async () => {
    for (const rail of RAILS) {
      const m = mount("schedules", rail);
      const hour = q(m.svg, ".nm-hour");
      const minute = q(m.svg, ".nm-minute");
      const orig = q(m.svg, ".nm-orig");
      const hourBox = box(hour);
      await hover(m.link);
      expect(getComputedStyle(orig).opacity, "orig hidden at start").toBe("0");
      const minuteAnim = cssAnims(m.svg).find((a) => a.animationName === "nm-minute")!;
      expect(minuteAnim, "minute animation").toBeDefined();
      expect(endTimeOf(minuteAnim)).toBe(1800);
      // animation-timing-function is per keyframe, not the effect-level easing.
      expect(getComputedStyle(minute).animationTimingFunction).toContain("steps(12");
      expect(hour.getAnimations().some(transformKeyframes), "hour transform animation").toBe(false);

      let prev = "";
      for (let k = 0; k < 12; k++) {
        seek(m.svg, k * 150 + 10);
        const t = getComputedStyle(minute).transform;
        expect(t, `minute step ${k} differs from step ${k - 1}`).not.toBe(prev);
        prev = t;
        expect(getComputedStyle(hour).transform, `hour transform @${k}`).toBe("none");
        expect(box(hour), `hour bbox @${k}`).toEqual(hourBox);
        expect(getComputedStyle(orig).opacity, `orig opacity @${k}`).toBe("0");
        expect(getComputedStyle(hour).opacity, `hour visible @${k}`).toBe("1");
      }
      cleanup();
      await userEvent.unhover(document.body);
    }
  });

  it("judge: the post copy holds still while the beam copy and pans swing", async () => {
    const m = mount("judge", "expanded");
    const post = q(m.svg, ".nm-post");
    const beam = q(m.svg, ".nm-beam");
    const postBox = box(post);
    await hover(m.link);
    expect(post.getAnimations().some(transformKeyframes)).toBe(false);
    let moved = false;
    for (const t of [200, 500, 800, 1100]) {
      seek(m.svg, t);
      expect(getComputedStyle(post).transform, `post @${t}`).toBe("none");
      expect(box(post), `post bbox @${t}`).toEqual(postBox);
      expect(getComputedStyle(post).opacity).toBe("1");
      expect(getComputedStyle(q(m.svg, ".nm-orig")).opacity, `orig @${t}`).toBe("0");
      if (getComputedStyle(beam).transform !== "none") moved = true;
    }
    expect(moved, "beam moved").toBe(true);
  });

  it("workers: each rack LED is drawn exactly once at every moment, accent in its own half", async () => {
    const m = mount("workers", "expanded");
    const orig = q(m.svg, ".nm-orig");
    const led1 = q(m.svg, ".nm-led1");
    const led2 = q(m.svg, ".nm-led2");
    await hover(m.link);
    const shown = (el: Element) => (Number(getComputedStyle(el).opacity) > 0 ? 1 : 0);
    for (const t of [100, 400, 600, 830, 1000, 1245, 1500, 1650]) {
      seek(m.svg, t);
      // The original holds both LEDs; it is hidden for the whole play, so each copy must show.
      expect(shown(orig), `orig @${t}`).toBe(0);
      expect(shown(led1), `led1 @${t}`).toBe(1);
      expect(shown(led2), `led2 @${t}`).toBe(1);
    }
    seek(m.svg, 100);
    expect(getComputedStyle(led1).strokeWidth, "led1 accent @100").toBe("4px");
    expect(getComputedStyle(led2).strokeWidth, "led2 plain @100").toBe("2px");
    seek(m.svg, 1000);
    expect(getComputedStyle(led1).strokeWidth, "led1 plain @1000").toBe("2px");
    expect(getComputedStyle(led2).strokeWidth, "led2 accent @1000").toBe("4px");
  });

  it("findings: the body never transforms while the legs rock", async () => {
    const m = mount("findings", "expanded");
    const body = Array.from(m.svg.children).filter((c) => c.tagName.toLowerCase() === "path") as SVGGraphicsElement[];
    expect(body).toHaveLength(5);
    const before = body.map(box);
    await hover(m.link);
    const legs = [q(m.svg, ".nm-legs-l"), q(m.svg, ".nm-legs-r")];
    for (const el of body) expect(el.getAnimations().some(transformKeyframes)).toBe(false);
    seek(m.svg, 130);
    body.forEach((el, i) => {
      expect(getComputedStyle(el).transform, `body ${i}`).toBe("none");
      expect(box(el), `body bbox ${i}`).toEqual(before[i]);
    });
    for (const leg of legs) expect(getComputedStyle(leg).transform).not.toBe("none");
  });
});
