// @vitest-environment jsdom
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, renderHook, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { NavItem, forgeIcon } from "./AppShell";
import { NAV_SECTIONS } from "./navItems";
import { SIDEBAR_MOTION_ICONS, useNavIconMotion } from "./sidebarMotion";
import { HomeIcon } from "./icons";

// The jsdom half of issue #4510: the gesture hook's gating and class lifecycle, and the glyph
// registry's shape. Real animations, reduced motion and pixel equality are in
// SidebarMotion.browser.test.tsx.

const PLAY = "nm-play";
const KEYS = NAV_SECTIONS.flatMap((s) => s.items.map((i) => i.key));

function setMotion(value: "on" | "off" | null) {
  if (value === null) document.documentElement.removeAttribute("data-motion");
  else document.documentElement.dataset.motion = value;
}

type ItemProps = { onNavigate?: () => void; motionKey?: string; icon?: ReactNode; indent?: boolean };

function renderItem(props: ItemProps = {}) {
  const { motionKey = "overview", ...rest } = props;
  const utils = render(
    <MemoryRouter>
      <NavItem to="/dashboard" label="Overview" icon={<HomeIcon />} motionKey={"motionKey" in props ? props.motionKey : motionKey} {...rest} />
    </MemoryRouter>,
  );
  const link = utils.getByRole("link");
  const svg = () => link.querySelector("svg") as SVGElement;
  return { ...utils, link, svg };
}

afterEach(() => {
  cleanup();
  setMotion(null);
  vi.restoreAllMocks();
  // @ts-expect-error test-only cleanup of the stub below
  delete window.matchMedia;
});

describe("sidebar motion gestures", () => {
  it("plays on click only when data-motion is on", () => {
    const { link, svg } = renderItem();
    expect(svg().getAttribute("data-nav-motion")).toBe("overview");

    // Motion unset: click plays nothing.
    fireEvent.click(link);
    expect(svg().classList.contains(PLAY)).toBe(false);

    setMotion("off");
    fireEvent.click(link);
    expect(svg().classList.contains(PLAY)).toBe(false);

    // Motion on, same render: pointerenter and focus add nothing, click does.
    setMotion("on");
    fireEvent.pointerEnter(link);
    fireEvent.focus(link);
    expect(svg().classList.contains(PLAY)).toBe(false);
    fireEvent.click(link);
    expect(svg().classList.contains(PLAY)).toBe(true);
  });

  it("focus does not play, even when keyboard-visible", () => {
    setMotion("on");
    const { link, svg } = renderItem();
    vi.spyOn(link, "matches").mockReturnValue(true);
    fireEvent.focus(link);
    expect(svg().classList.contains(PLAY)).toBe(false);

    // Paired positive on the same link.
    fireEvent.click(link);
    expect(svg().classList.contains(PLAY)).toBe(true);
  });

  it("returns exactly ref and onClick from the hook", () => {
    const { result } = renderHook(() => useNavIconMotion());
    expect(Object.keys(result.current).sort()).toEqual(["onClick", "ref"]);
  });

  it("does not play under prefers-reduced-motion", () => {
    setMotion("on");
    window.matchMedia = vi.fn().mockReturnValue({
      matches: true,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    }) as unknown as typeof window.matchMedia;
    const { link, svg } = renderItem();
    fireEvent.click(link);
    expect(svg().classList.contains(PLAY)).toBe(false);
  });

  it("restarts on a repeat gesture instead of queueing", () => {
    setMotion("on");
    const { link, svg } = renderItem();
    const add = vi.spyOn(svg().classList, "add");
    const remove = vi.spyOn(svg().classList, "remove");
    fireEvent.click(link);
    fireEvent.click(link);
    expect(add.mock.calls.filter(([c]) => c === PLAY)).toHaveLength(2);
    // Each play drops the class before re-adding it.
    expect(remove.mock.calls.filter(([c]) => c === PLAY).length).toBeGreaterThanOrEqual(2);
    expect(svg().classList.contains(PLAY)).toBe(true);
  });

  it("clears the class on animationend and animationcancel", () => {
    setMotion("on");
    const { link, svg } = renderItem();
    fireEvent.click(link);
    expect(svg().classList.contains(PLAY)).toBe(true);
    svg().dispatchEvent(new Event("animationend", { bubbles: true }));
    expect(svg().classList.contains(PLAY)).toBe(false);

    fireEvent.click(link);
    expect(svg().classList.contains(PLAY)).toBe(true);
    svg().dispatchEvent(new Event("animationcancel", { bubbles: true }));
    expect(svg().classList.contains(PLAY)).toBe(false);
  });

  it("drops a playing class when the setting turns off", async () => {
    setMotion("on");
    const { link, svg } = renderItem();
    fireEvent.click(link);
    expect(svg().classList.contains(PLAY)).toBe(true);
    setMotion("off");
    await waitFor(() => expect(svg().classList.contains(PLAY)).toBe(false));
  });

  it("still runs onNavigate on click, with or without a glyph", () => {
    setMotion("on");
    const onNavigate = vi.fn();
    const { link } = renderItem({ onNavigate });
    fireEvent.click(link);
    expect(onNavigate).toHaveBeenCalledTimes(1);

    cleanup();
    const plain = vi.fn();
    const r = renderItem({ onNavigate: plain, motionKey: undefined });
    fireEvent.click(r.link);
    expect(plain).toHaveBeenCalledTimes(1);
  });

  it("leaves rows without a motionKey and the NAV_SECTIONS icons static", () => {
    setMotion("on");
    const { link } = renderItem({ motionKey: undefined, icon: forgeIcon("gitlab"), indent: true });
    expect(link.querySelector("[data-nav-motion]")).toBeNull();
    fireEvent.pointerEnter(link);
    fireEvent.click(link);
    expect(link.querySelector(`.${PLAY}`)).toBeNull();

    for (const def of NAV_SECTIONS.flatMap((s) => s.items)) {
      const { container, unmount } = render(<>{def.icon}</>);
      expect(container.querySelector("[data-nav-motion]"), def.key).toBeNull();
      unmount();
    }
  });
});

describe("SIDEBAR_MOTION_ICONS", () => {
  it("has exactly one glyph per NAV_SECTIONS key, each tagged with its key", () => {
    expect(Object.keys(SIDEBAR_MOTION_ICONS).sort()).toEqual([...KEYS].sort());
    expect(KEYS).toHaveLength(13);
    for (const key of KEYS) {
      const { container, unmount } = render(<>{SIDEBAR_MOTION_ICONS[key]()}</>);
      const svg = container.querySelector("svg");
      expect(svg?.getAttribute("data-nav-motion"), key).toBe(key);
      expect(svg?.getAttribute("viewBox")).toBe("0 0 24 24");
      expect(svg?.getAttribute("aria-hidden")).toBe("true");
      unmount();
    }
  });

  it("keeps schedules' hands as distinct hour and minute parts", () => {
    const { container } = render(<>{SIDEBAR_MOTION_ICONS.schedules()}</>);
    const hour = container.querySelector(".nm-hour");
    const minute = container.querySelector(".nm-minute");
    expect(hour).not.toBeNull();
    expect(minute).not.toBeNull();
    expect(hour).not.toBe(minute);
    expect(hour?.getAttribute("d")).not.toBe(minute?.getAttribute("d"));
    // The joined original hands stay the rest glyph; the parts are rest-invisible copies.
    expect(container.querySelector(".nm-orig")?.getAttribute("d")).toBe("M12 7v5l3 2");
    expect(hour?.getAttribute("opacity")).toBe("0");
    expect(minute?.getAttribute("opacity")).toBe("0");
  });
});
