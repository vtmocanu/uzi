import { vi } from "vitest";

// Supply layout and IntersectionObserver delivery for jsdom. Keep frame batches
// separate so the real SectionPills evaluator can run between them, then model
// a jump clamped at the page end.
export function installAnchorFrames(targetId: string) {
  const frames = new Map<number, FrameRequestCallback>();
  let nextId = 1;
  let landed = false;
  const observers: ControlledIntersectionObserver[] = [];
  class ControlledIntersectionObserver implements IntersectionObserver {
    readonly root: Element | Document | null;
    readonly rootMargin: string;
    readonly scrollMargin = "0px";
    readonly thresholds: number[];
    readonly targets = new Set<Element>();
    readonly rawRootMargin: string | undefined;

    constructor(private readonly callback: IntersectionObserverCallback, options: IntersectionObserverInit = {}) {
      this.root = options.root ?? null;
      this.rawRootMargin = options.rootMargin;
      this.rootMargin = options.rootMargin ?? "0px";
      this.thresholds = Array.isArray(options.threshold) ? options.threshold : [options.threshold ?? 0];
      observers.push(this);
    }
    observe(target: Element) { this.targets.add(target); }
    unobserve(target: Element) { this.targets.delete(target); }
    disconnect() { this.targets.clear(); }
    takeRecords(): IntersectionObserverEntry[] { return []; }
    deliver() { this.callback([], this); }
  }
  vi.stubGlobal("IntersectionObserver", ControlledIntersectionObserver);
  vi.stubGlobal("ResizeObserver", class {
    observe() {}
    unobserve() {}
    disconnect() {}
  });
  vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
    const id = nextId++;
    frames.set(id, callback);
    return id;
  });
  vi.stubGlobal("cancelAnimationFrame", (id: number) => { frames.delete(id); });
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  Object.defineProperty(window, "scrollY", { configurable: true, get: () => landed ? 500 : 0 });
  const rects = vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function(this: HTMLElement) {
    if (this.hasAttribute("data-pinned-block-root")) return new DOMRect(0, 0, 800, 100);
    if (observers.some((o) => o.root === document && o.rawRootMargin === undefined && o.targets.has(this))) {
      return new DOMRect(0, landed ? window.innerHeight : 10000, 800, 0);
    }
    const ids = Array.from(document.querySelectorAll<HTMLAnchorElement>('nav[aria-label="Sections on this page"] a'))
      .map((a) => a.getAttribute("href")!.slice(1));
    const index = ids.indexOf(this.id);
    if (index >= 0) {
      const top = landed ? 200 + (index - ids.indexOf(targetId)) * 100 : 300 + index * 100;
      return new DOMRect(0, top, 800, 60);
    }
    return new DOMRect(24, 0, 200, 20);
  });
  const lineObserver = () => {
    const target = document.getElementById(targetId);
    return observers.find((o) => o.root === document && !!target && o.targets.has(target));
  };
  return {
    hasLineObserver: () => lineObserver() !== undefined,
    advanceFrame() {
      // A nested rAF belongs to the next batch, as in the browser.
      const batch = Array.from(frames);
      for (const [id, callback] of batch) {
        if (frames.delete(id)) callback(0);
      }
    },
    evaluateAtTop() {
      if (landed) throw new Error("observer fixture already scrolled");
      const observer = lineObserver();
      if (!observer) throw new Error("section observer is not mounted");
      observer.deliver();
    },
    landAtEnd() { landed = true; },
    restore() {
      frames.clear();
      rects.mockRestore();
      if (scrollY) Object.defineProperty(window, "scrollY", scrollY);
      else Reflect.deleteProperty(window, "scrollY");
      vi.unstubAllGlobals();
    },
  };
}
