// @vitest-environment jsdom
//
// The module list provider and the federation wiring (PRD #4686, "Loader"), with only the
// federation RUNTIME package mocked: ./federation and ./registry are real.
import { StrictMode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

const rt = vi.hoisted(() => ({
  init: vi.fn(),
  registerRemotes: vi.fn(),
  loadRemote: vi.fn(),
}));
vi.mock("@module-federation/runtime", () => rt);

import { loadModuleList, ModulesProvider, resetModuleListForTests, useModules } from "./registry";
import { dependencies } from "../../package.json";

const entry = { id: "demo", version: "1", page: "Page", label: "Demo", icon: "package", navGroup: "work" };

let fetchMock: ReturnType<typeof vi.fn>;
let errors: ReturnType<typeof vi.spyOn>;
const reply = (status: number, body: string) => () => Promise.resolve(new Response(body, { status }));

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  errors = vi.spyOn(console, "error").mockImplementation(() => {});
  rt.init.mockReset();
  rt.registerRemotes.mockReset();
  rt.loadRemote.mockReset();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  resetModuleListForTests();
});

describe("loadModuleList", () => {
  it("fetches /modules.json once with no-cache, however often it is called", async () => {
    fetchMock.mockImplementation(reply(200, JSON.stringify([entry])));
    const [a, b] = await Promise.all([loadModuleList(), loadModuleList()]);
    await loadModuleList();
    expect(a).toBe(b);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith("/modules.json", { cache: "no-cache" });
  });

  it.each([
    ["a rejected request", () => Promise.reject(new TypeError("offline"))],
    ["a non-2xx response", reply(503, "[]")],
    ["a malformed body", reply(200, "<html>not json")],
    ["a non-array body", reply(200, '{"modules": []}')],
  ])("gives an empty list and exactly one console error for %s", async (_n, impl) => {
    fetchMock.mockImplementation(impl);
    expect(await loadModuleList()).toEqual([]);
    expect(errors).toHaveBeenCalledTimes(1);
    expect(rt.registerRemotes).not.toHaveBeenCalled();
    expect(rt.init).not.toHaveBeenCalled();
  });

  it("with zero modules never loads or initialises the federation runtime", async () => {
    fetchMock.mockImplementation(reply(200, "[]"));
    expect(await loadModuleList()).toEqual([]);
    expect(rt.init).not.toHaveBeenCalled();
    expect(rt.registerRemotes).not.toHaveBeenCalled();
    expect(rt.loadRemote).not.toHaveBeenCalled();
  });

  it("registers the list once, forced, same-origin, type module, and loads no remote", async () => {
    fetchMock.mockImplementation(reply(200, JSON.stringify([entry, { ...entry, id: "other", navGroup: "factory" }])));
    await loadModuleList();
    expect(rt.registerRemotes).toHaveBeenCalledTimes(1);
    expect(rt.registerRemotes).toHaveBeenCalledWith(
      [
        { name: "demo", entry: "/modules/demo/remoteEntry.js", type: "module" },
        { name: "other", entry: "/modules/other/remoteEntry.js", type: "module" },
      ],
      { force: true },
    );
    expect(rt.loadRemote).not.toHaveBeenCalled();
  });
});

describe("ModulesProvider", () => {
  function Probe() {
    const { status, modules } = useModules();
    return <p data-testid="probe">{`${status}:${modules.map((m) => m.id).join(",")}`}</p>;
  }

  it("without a provider the list is ready and empty", () => {
    render(<Probe />);
    expect(screen.getByTestId("probe").textContent).toBe("ready:");
  });

  it("is loading, then ready with the parsed list; StrictMode still fetches once", async () => {
    fetchMock.mockImplementation(reply(200, JSON.stringify([entry])));
    render(
      <StrictMode>
        <ModulesProvider>
          <Probe />
        </ModulesProvider>
      </StrictMode>,
    );
    expect(screen.getByTestId("probe").textContent).toBe("loading:");
    expect((await screen.findByText("ready:demo")).textContent).toBe("ready:demo");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("a remounted provider starts ready from the memoized list", async () => {
    fetchMock.mockImplementation(reply(200, JSON.stringify([entry])));
    const first = render(
      <ModulesProvider>
        <Probe />
      </ModulesProvider>,
    );
    await screen.findByText("ready:demo");
    first.unmount();
    render(
      <ModulesProvider>
        <Probe />
      </ModulesProvider>,
    );
    expect(screen.getByTestId("probe").textContent).toBe("ready:demo");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});

describe("federation wiring", () => {
  const list = [{ ...entry, adminOnly: false, icon: "package" as const, navGroup: "work" as const }];
  // A fresh module graph per test: federation.ts initialises the runtime once per document.
  const fresh = async () => {
    vi.resetModules();
    return import("./federation");
  };

  it("initialises the host as uzi_host with the four shared singletons, strict, from package.json ranges", async () => {
    (await fresh()).registerModuleRemotes(list);
    expect(rt.init).toHaveBeenCalledTimes(1);
    const opts = rt.init.mock.calls[0][0];
    expect(opts).toMatchObject({ name: "uzi_host", remotes: [], shareStrategy: "loaded-first" });
    expect(Object.keys(opts.shared).sort()).toEqual(["react", "react-dom", "react-router-dom", "react/jsx-runtime"]);
    for (const [name, cfg] of Object.entries(opts.shared) as [string, { version: string; lib: () => unknown; shareConfig: Record<string, unknown> }][]) {
      expect(cfg.version).toMatch(/^\d+\.\d+\.\d+/);
      expect(cfg.lib()).toBeTruthy();
      const range = name === "react/jsx-runtime" ? dependencies.react : dependencies[name as "react"];
      expect(cfg.shareConfig).toEqual({ singleton: true, requiredVersion: range, strictVersion: true });
    }
  });

  it("initialises exactly once across register, re-register and load", async () => {
    const f = await fresh();
    f.registerModuleRemotes(list);
    f.reRegisterModule("demo", 1);
    void f.loadModuleRemote("demo/Page");
    expect(rt.init).toHaveBeenCalledTimes(1);
  });

  it("re-registers one remote with a fresh retry URL, forced", async () => {
    (await fresh()).reRegisterModule("demo", 3);
    expect(rt.registerRemotes).toHaveBeenLastCalledWith(
      [{ name: "demo", entry: "/modules/demo/remoteEntry.js?retry=3", type: "module" }],
      { force: true },
    );
  });

  it("loadModuleRemote forwards the request id to the runtime", async () => {
    rt.loadRemote.mockResolvedValue({ default: 1 });
    await expect((await fresh()).loadModuleRemote("demo/Page")).resolves.toEqual({ default: 1 });
    expect(rt.loadRemote).toHaveBeenCalledWith("demo/Page");
  });
});
