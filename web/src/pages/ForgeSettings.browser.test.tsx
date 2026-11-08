/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cdp } from "vitest/browser";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ForgeSettings } from "./ForgeSettings";
import { api } from "../lib/api";
import { mockForgeConfigSyncForges } from "../mocks/data";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      forgeConfig: vi.fn(),
      listConnections: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

beforeEach(() => {
  mockApi.forgeConfig.mockResolvedValue(mockForgeConfigSyncForges);
  mockApi.listConnections.mockResolvedValue({ connections: [] });
});

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

// Issue #4097: the real connect form puts .forge-flash on the wrapper of whichever
// field a sync auto-changed. Under reduced motion the animation is `none`, so no
// animationend fires to clear the class and it stays observable without any wait.
describe("connect form sync flash (PRD #337 D9)", () => {
  const baseUrlSelect = () => screen.getByLabelText("Forge base URL") as HTMLSelectElement;
  const typeSelect = () => screen.getByLabelText("Forge type") as HTMLSelectElement;
  // Select renders a bare <select>, so its parent is the flash wrapper.
  const urlWrapper = () => baseUrlSelect().parentElement as HTMLElement;
  const typeWrapper = () => typeSelect().parentElement as HTMLElement;

  async function withReducedMotion(body: () => Promise<void>) {
    const session = cdp();
    try {
      await session.send("Emulation.setEmulatedMedia", {
        features: [{ name: "prefers-reduced-motion", value: "reduce" }],
      });
      await body();
    } finally {
      await session.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-reduced-motion", value: "" }] });
    }
  }

  it("a base-URL change that syncs the type flashes the type field", async () => {
    await withReducedMotion(async () => {
      render(
        <MemoryRouter>
          <ForgeSettings />
        </MemoryRouter>,
      );
      await screen.findByLabelText("Forge type");
      expect(urlWrapper().classList.contains("forge-flash")).toBe(false);
      expect(typeWrapper().classList.contains("forge-flash")).toBe(false);

      fireEvent.change(baseUrlSelect(), { target: { value: "https://github.com" } });

      await expect.poll(() => typeSelect().value).toBe("github");
      expect(typeWrapper().classList.contains("forge-flash"), "type wrapper flashes").toBe(true);
      expect(urlWrapper().classList.contains("forge-flash"), "url wrapper does not").toBe(false);
    });
  });

  it("a type change that syncs the base URL flashes the URL field", async () => {
    await withReducedMotion(async () => {
      render(
        <MemoryRouter>
          <ForgeSettings />
        </MemoryRouter>,
      );
      await screen.findByLabelText("Forge type");
      expect(urlWrapper().classList.contains("forge-flash")).toBe(false);
      expect(typeWrapper().classList.contains("forge-flash")).toBe(false);

      fireEvent.change(typeSelect(), { target: { value: "github" } });

      await expect.poll(() => baseUrlSelect().value).toBe("https://github.com");
      expect(urlWrapper().classList.contains("forge-flash"), "url wrapper flashes").toBe(true);
      expect(typeWrapper().classList.contains("forge-flash"), "type wrapper does not").toBe(false);
    });
  });
});
