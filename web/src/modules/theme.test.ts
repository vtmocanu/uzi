// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { currentThemeMode, subscribeThemeMode } from "./theme";

const stamp = (v: string | null) => {
  if (v === null) delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = v;
};
// MutationObserver callbacks are microtasks.
const flush = () => new Promise<void>((r) => setTimeout(r, 0));

beforeEach(() => stamp("ember"));
afterEach(() => stamp(null));

describe("module theme", () => {
  it("derives the mode from the stamped theme's polarity", () => {
    stamp("ember");
    expect(currentThemeMode()).toBe("dark");
    stamp("dawn");
    expect(currentThemeMode()).toBe("light");
  });

  it("falls back to the default theme for a missing or unknown stamp", () => {
    stamp("not-a-theme");
    expect(currentThemeMode()).toBe("dark");
    stamp(null);
    expect(currentThemeMode()).toBe("dark");
  });

  it("notifies only when the polarity changes", async () => {
    const cb = vi.fn();
    const off = subscribeThemeMode(cb);
    stamp("mission"); // dark -> dark
    await flush();
    expect(cb).not.toHaveBeenCalled();
    stamp("dawn"); // dark -> light
    await flush();
    expect(cb).toHaveBeenCalledTimes(1);
    expect(cb).toHaveBeenLastCalledWith("light");
    stamp("hall"); // light -> light
    await flush();
    expect(cb).toHaveBeenCalledTimes(1);
    stamp("shadow-ish-bogus"); // light -> default dark
    await flush();
    expect(cb).toHaveBeenLastCalledWith("dark");
    off();
  });

  it("stops notifying after unsubscribe", async () => {
    const cb = vi.fn();
    subscribeThemeMode(cb)();
    stamp("dawn");
    await flush();
    expect(cb).not.toHaveBeenCalled();
  });
});
