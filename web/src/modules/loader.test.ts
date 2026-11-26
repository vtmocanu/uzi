// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const fed = vi.hoisted(() => ({
  loadModuleRemote: vi.fn(),
  reRegisterModule: vi.fn(),
  registerModuleRemotes: vi.fn(),
  setHostShared: vi.fn(),
}));
vi.mock("./federation", () => fed);

import {
  classifyLoadError,
  hasModuleLoaded,
  isStaleChunkError,
  loadModulePage,
  MODULE_LOAD_TIMEOUT_MS,
  ModuleLoadError,
  resetModuleLoaderForTests,
  retryModuleLoad,
} from "./loader";

const entry = { id: "demo", page: "Page" };
const Page = () => null;
const good = { default: Page, hostContract: 1 };

const failure = async (p: Promise<unknown>): Promise<ModuleLoadError> => {
  try {
    await p;
  } catch (e) {
    return e as ModuleLoadError;
  }
  throw new Error("expected a rejection");
};

beforeEach(() => {
  resetModuleLoaderForTests();
  fed.loadModuleRemote.mockReset();
  fed.reRegisterModule.mockReset();
});
afterEach(() => {
  delete (globalThis as { __UZI_MODULE_LOAD_TIMEOUT_MS__?: number }).__UZI_MODULE_LOAD_TIMEOUT_MS__;
});

describe("loadModulePage", () => {
  it("loads <id>/<page> and returns the default export", async () => {
    fed.loadModuleRemote.mockResolvedValue(good);
    expect(await loadModulePage(entry)).toBe(Page);
    expect(fed.loadModuleRemote).toHaveBeenCalledWith("demo/Page");
    expect(hasModuleLoaded("demo")).toBe(true);
  });

  it("strips a leading ./ from the expose name", async () => {
    fed.loadModuleRemote.mockResolvedValue(good);
    await loadModulePage({ id: "demo", page: "./Page" });
    expect(fed.loadModuleRemote).toHaveBeenCalledWith("demo/Page");
  });

  it("joins an in-flight load instead of calling loadRemote twice", async () => {
    fed.loadModuleRemote.mockResolvedValue(good);
    await Promise.all([loadModulePage(entry), loadModulePage(entry)]);
    expect(fed.loadModuleRemote).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["a contract major of 2", { default: Page, hostContract: 2 }],
    ["no hostContract", { default: Page }],
    ["no default export", { hostContract: 1 }],
    ["a null module", null],
  ])("treats %s as incompatible and never as loaded", async (_n, mod) => {
    fed.loadModuleRemote.mockResolvedValue(mod);
    const err = await failure(loadModulePage(entry));
    expect(err.kind).toBe("incompatible");
    expect(hasModuleLoaded("demo")).toBe(false);
  });

  it("classifies a shared-version refusal as incompatible, including inside a cause", async () => {
    fed.loadModuleRemote.mockRejectedValue(
      Object.assign(new Error("#RUNTIME-015: outer"), {
        cause: new Error('Version 19.0.0 from host of shared singleton module react does not satisfy the requirement of demo which needs ^18.0.0'),
      }),
    );
    expect((await failure(loadModulePage(entry))).kind).toBe("incompatible");
  });

  it("classifies the observed missing-expose error as incompatible", async () => {
    // Text observed in the browser harness (`noexpose` fixture): the remote container's get().
    fed.loadModuleRemote.mockRejectedValue(new Error("[Module Federation] Module ./Missing does not exist in container."));
    const err = await failure(loadModulePage(entry));
    expect(err.kind).toBe("incompatible");
    expect(err.message).toContain("does not expose the configured page");
  });

  it("treats any other runtime error, including a different RUNTIME-015 text, as transient", async () => {
    fed.loadModuleRemote.mockRejectedValue(new Error("RUNTIME-015: something else"));
    expect((await failure(loadModulePage(entry))).kind).toBe("transient");
  });

  it("treats a network failure before the first load as transient, not stale", async () => {
    fed.loadModuleRemote.mockRejectedValue(
      new TypeError("Failed to fetch dynamically imported module: http://localhost/modules/demo/remoteEntry.js"),
    );
    expect((await failure(loadModulePage(entry))).kind).toBe("transient");
  });
});

describe("stale classification after a first successful load", () => {
  beforeEach(async () => {
    fed.loadModuleRemote.mockResolvedValueOnce(good);
    await loadModulePage(entry);
  });

  it.each([
    ["Chromium", "Failed to fetch dynamically imported module: http://localhost/modules/demo/assets/Sub-abc.js"],
    ["Firefox", "error loading dynamically imported module: http://localhost/modules/demo/assets/Sub-abc.js"],
    ["Safari, which names no URL", "Importing a module script failed."],
    // Text observed in the browser harness when a module's remoteEntry.js request fails.
    [
      "an MF script load error (observed text)",
      '[ Federation Runtime ]: Failed to load script resources. #RUNTIME-008 args: {"remoteName":"demo","resourceUrl":"http://localhost/modules/demo/remoteEntry.js"} Original Error Message: Failed to fetch dynamically imported module: http://localhost/modules/demo/remoteEntry.js',
    ],
    // The federation shape alone: no browser dynamic-import wording and no module URL, so
    // only the federation-specific branch of isStaleChunkError can classify these.
    [
      "an MF script load error with only the RUNTIME-008 code",
      '[ Federation Runtime ]: Failed to load script resources. #RUNTIME-008 args: {"remoteName":"demo"} Original Error Message: Load failed',
    ],
    ["an MF script load error with only the message text", "[ Federation Runtime ]: Failed to load script resources."],
  ])("%s is stale", async (_n, message) => {
    fed.loadModuleRemote.mockRejectedValue(new TypeError(message));
    expect((await failure(loadModulePage(entry))).kind).toBe("stale");
  });

  it("an unrelated error is transient, so the card offers Retry", async () => {
    fed.loadModuleRemote.mockRejectedValue(new Error("boom"));
    expect((await failure(loadModulePage(entry))).kind).toBe("transient");
  });

  it("a dynamic-import failure for a URL outside the module's prefix is not stale", () => {
    expect(isStaleChunkError(new Error("Failed to fetch dynamically imported module: http://localhost/assets/x.js"), "demo")).toBe(false);
    expect(isStaleChunkError(new Error("Failed to fetch dynamically imported module: http://localhost/modules/other/x.js"), "demo")).toBe(false);
  });

  it("a shared-version refusal stays incompatible, not stale", () => {
    expect(classifyLoadError(new Error("does not satisfy the requirement of"), "demo").kind).toBe("incompatible");
  });
});

describe("timeout", () => {
  it("defaults to 15 seconds", () => {
    expect(MODULE_LOAD_TIMEOUT_MS).toBe(15000);
  });

  it("with no override, a never-settling load rejects as transient at exactly 15000 ms", async () => {
    vi.useFakeTimers();
    try {
      fed.loadModuleRemote.mockReturnValue(new Promise(() => {}));
      let settled: ModuleLoadError | null = null;
      const p = failure(loadModulePage(entry)).then((e) => (settled = e));
      await vi.advanceTimersByTimeAsync(14999);
      expect(settled).toBeNull();
      await vi.advanceTimersByTimeAsync(1);
      await p;
      expect(settled).not.toBeNull();
      expect((settled as unknown as ModuleLoadError).kind).toBe("transient");
    } finally {
      vi.useRealTimers();
    }
  });

  it("rejects as transient after the override and discards a late arrival", async () => {
    (globalThis as { __UZI_MODULE_LOAD_TIMEOUT_MS__?: number }).__UZI_MODULE_LOAD_TIMEOUT_MS__ = 30;
    let arrive!: (v: unknown) => void;
    fed.loadModuleRemote.mockReturnValue(new Promise((r) => (arrive = r)));
    const err = await failure(loadModulePage(entry));
    expect(err.kind).toBe("transient");
    expect(err.message).toContain("Timed out after 30 ms");
    arrive(good);
    await new Promise((r) => setTimeout(r, 10));
    // The late arrival neither resolved anything nor marked the module as loaded.
    expect(hasModuleLoaded("demo")).toBe(false);
  });

  it("an invalid override falls back to the default (a fast load still works)", async () => {
    (globalThis as { __UZI_MODULE_LOAD_TIMEOUT_MS__?: unknown }).__UZI_MODULE_LOAD_TIMEOUT_MS__ = -5;
    fed.loadModuleRemote.mockResolvedValue(good);
    expect(await loadModulePage(entry)).toBe(Page);
  });
});

describe("retryModuleLoad", () => {
  it("re-registers with a fresh retry number each time, then loads", async () => {
    fed.loadModuleRemote.mockRejectedValueOnce(new Error("down")).mockRejectedValueOnce(new Error("down")).mockResolvedValue(good);
    await failure(loadModulePage(entry));
    await failure(retryModuleLoad(entry));
    expect(await retryModuleLoad(entry)).toBe(Page);
    expect(fed.reRegisterModule.mock.calls).toEqual([
      ["demo", 1],
      ["demo", 2],
    ]);
    expect(fed.loadModuleRemote).toHaveBeenCalledTimes(3);
  });
});
