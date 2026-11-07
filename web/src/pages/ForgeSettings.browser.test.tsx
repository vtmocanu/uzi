/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import { afterEach, describe, expect, it } from "vitest";
import { cdp } from "vitest/browser";
import { cleanup, render } from "@testing-library/react";

// Issue #3986: the connect-form sync flash (.forge-flash) must stop under
// prefers-reduced-motion. Both rules are unlayered with equal specificity, so source
// order decides the winner; only a real engine computes that, hence Chromium.
afterEach(() => cleanup());

describe(".forge-flash", () => {
  it("animates by default and stops under prefers-reduced-motion", async () => {
    const { container } = render(<div className="forge-flash">field</div>);
    const el = container.querySelector(".forge-flash") as HTMLElement;
    expect(el).not.toBeNull();
    expect(getComputedStyle(el).animationName, "animates without the preference").toBe("forge-flash");
    const session = cdp();
    try {
      await session.send("Emulation.setEmulatedMedia", {
        features: [{ name: "prefers-reduced-motion", value: "reduce" }],
      });
      expect(getComputedStyle(el).animationName).toBe("none");
    } finally {
      await session.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-reduced-motion", value: "" }] });
    }
  });
});
