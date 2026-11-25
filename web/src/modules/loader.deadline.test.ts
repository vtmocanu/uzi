// @vitest-environment jsdom
// A Retry shares ONE load deadline across re-registration and load. This file mocks
// ./federationEntry (the boundary loader.ts calls), because the federationEntry wrapper
// is where re-registration can take time (it imports the federation chunk first).
import { beforeEach, describe, expect, it, vi } from "vitest";

const ent = vi.hoisted(() => ({
  loadModuleRemote: vi.fn(),
  reRegisterModule: vi.fn(),
  registerModuleRemotes: vi.fn(),
}));
vi.mock("./federationEntry", () => ent);

import { loadModulePage, ModuleLoadError, resetModuleLoaderForTests, retryModuleLoad } from "./loader";

const entry = { id: "demo", page: "Page" };

beforeEach(() => {
  resetModuleLoaderForTests();
  ent.loadModuleRemote.mockReset();
  ent.reRegisterModule.mockReset();
});

describe("retryModuleLoad deadline", () => {
  it("rejects at 15 s overall when re-registration and load each stay under 15 s", async () => {
    vi.useFakeTimers();
    try {
      ent.loadModuleRemote.mockRejectedValueOnce(new Error("down"));
      await loadModulePage(entry).catch(() => undefined);
      // Re-registration takes 10 s and the load never settles: each phase alone is under
      // the 15 s budget, together they exceed it.
      ent.reRegisterModule.mockImplementation(() => new Promise<void>((r) => setTimeout(r, 10000)));
      ent.loadModuleRemote.mockReturnValue(new Promise(() => {}));
      let settled: unknown = null;
      void retryModuleLoad(entry).catch((e: unknown) => {
        settled = e;
      });
      await vi.advanceTimersByTimeAsync(14999);
      expect(settled).toBeNull();
      await vi.advanceTimersByTimeAsync(1);
      expect(settled).toBeInstanceOf(ModuleLoadError);
      expect((settled as ModuleLoadError).kind).toBe("transient");
    } finally {
      vi.useRealTimers();
    }
  });

  it("a backward wall-clock step during re-registration does not stretch the budget", async () => {
    vi.useFakeTimers();
    try {
      ent.loadModuleRemote.mockRejectedValueOnce(new Error("down"));
      await loadModulePage(entry).catch(() => undefined);
      ent.reRegisterModule.mockImplementation(
        () =>
          new Promise<void>((r) =>
            setTimeout(() => {
              vi.setSystemTime(Date.now() - 60 * 60 * 1000); // the wall clock jumps back 1 h
              r();
            }, 10000),
          ),
      );
      ent.loadModuleRemote.mockReturnValue(new Promise(() => {}));
      let settled: unknown = null;
      void retryModuleLoad(entry).catch((e: unknown) => {
        settled = e;
      });
      await vi.advanceTimersByTimeAsync(14999);
      expect(settled).toBeNull();
      await vi.advanceTimersByTimeAsync(1);
      expect(settled).toBeInstanceOf(ModuleLoadError);
    } finally {
      vi.useRealTimers();
    }
  });
});
