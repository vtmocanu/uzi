// @vitest-environment jsdom
//
// ModuleLoader, ModuleErrorBoundary and ModuleErrorCard (PRD #4686, "Failure isolation"):
// which card action each failure class gets, and that the two Retry paths do what the PRD
// pins (transient: re-register + load again; render error: remount only).
import { StrictMode, type ReactElement } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

const fed = vi.hoisted(() => ({
  loadModuleRemote: vi.fn(),
  reRegisterModule: vi.fn(),
  registerModuleRemotes: vi.fn(),
  setHostShared: vi.fn(),
}));
vi.mock("./federation", () => fed);

const auth = vi.hoisted(() => ({ isAdmin: true }));
vi.mock("../auth/AuthContext", () => ({
  useAuth: () => ({
    user: { id: "u1", email: "a@b.c", display_name: "Ada", is_admin: auth.isAdmin },
  }),
}));

import { ModuleLoader } from "./ModuleLoader";
import { resetModuleLoaderForTests } from "./loader";
import type { ModuleEntry } from "./manifest";
import type { HostSdkV1 } from "./sdk";

const entry: ModuleEntry = {
  id: "demo",
  version: "4.5.6",
  page: "Page",
  label: "Demo",
  icon: "package",
  navGroup: "work",
  adminOnly: false,
};

function Page({ sdk }: { sdk: HostSdkV1 }) {
  return <p>page contract {sdk.contract}</p>;
}
const good = { default: Page, hostContract: 1 };

const mount = (ui: ReactElement = <ModuleLoader entry={entry} />) => render(<MemoryRouter>{ui}</MemoryRouter>);
const retry = () => screen.queryByRole("button", { name: "Retry" });
const reload = () => screen.queryByRole("button", { name: "Reload page" });

beforeEach(() => {
  auth.isAdmin = true;
  resetModuleLoaderForTests();
  fed.loadModuleRemote.mockReset();
  fed.reRegisterModule.mockReset();
  vi.spyOn(console, "error").mockImplementation(() => {});
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  delete (globalThis as { __UZI_MODULE_LOAD_TIMEOUT_MS__?: number }).__UZI_MODULE_LOAD_TIMEOUT_MS__;
});

describe("loading", () => {
  it("renders the page with the sdk prop", async () => {
    fed.loadModuleRemote.mockResolvedValue(good);
    mount();
    expect(await screen.findByText("page contract 1")).toBeTruthy();
    expect(fed.loadModuleRemote).toHaveBeenCalledWith("demo/Page");
  });

  it("calls loadRemote once under StrictMode's double effect", async () => {
    fed.loadModuleRemote.mockResolvedValue(good);
    mount(
      <StrictMode>
        <ModuleLoader entry={entry} />
      </StrictMode>,
    );
    await screen.findByText("page contract 1");
    expect(fed.loadModuleRemote).toHaveBeenCalledTimes(1);
  });
});

describe("transient failure before the first load", () => {
  it("shows Retry, and Retry re-registers with a fresh URL and loads again", async () => {
    fed.loadModuleRemote.mockRejectedValueOnce(new TypeError("Failed to fetch")).mockResolvedValue(good);
    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    expect(reload()).toBeNull();
    fireEvent.click(retry()!);
    expect(await screen.findByText("page contract 1")).toBeTruthy();
    expect(fed.reRegisterModule).toHaveBeenCalledTimes(1);
    expect(fed.reRegisterModule).toHaveBeenCalledWith("demo", 1);
    expect(fed.loadModuleRemote).toHaveBeenCalledTimes(2);
  });

  it("a timeout (test hook) shows the card with Retry", async () => {
    (globalThis as { __UZI_MODULE_LOAD_TIMEOUT_MS__?: number }).__UZI_MODULE_LOAD_TIMEOUT_MS__ = 30;
    fed.loadModuleRemote.mockReturnValue(new Promise(() => {}));
    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    expect(screen.getByTestId("module-error-reason").textContent).toContain("Timed out after 30 ms");
  });
});

// A Retry only changes remoteEntry.js's URL; a child chunk keeps its URL. The pinned Chromium
// re-fetches it (browser harness), but an engine that keeps the failure cannot recover by
// Retry, so after a Retry that fails transient the card also offers Reload page. The record is per document and per module id, and a success clears it.
describe("transient failure after a Retry", () => {
  const down = () => new TypeError("Failed to fetch");

  it("offers Reload page next to Retry once Retry fails transient", async () => {
    fed.loadModuleRemote.mockRejectedValue(down());
    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    expect(reload()).toBeNull();
    fireEvent.click(retry()!);
    await waitFor(() => expect(reload()).not.toBeNull());
    expect(retry()).not.toBeNull();
    expect(fed.reRegisterModule).toHaveBeenCalledTimes(1);
  });

  it("keeps offering Reload page when the module is mounted again in the same document", async () => {
    fed.loadModuleRemote.mockRejectedValue(down());
    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    fireEvent.click(retry()!);
    await waitFor(() => expect(reload()).not.toBeNull());
    cleanup();

    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    expect(reload()).not.toBeNull();
  });

  it("offers Reload page when the re-registration itself fails", async () => {
    fed.loadModuleRemote.mockRejectedValue(down());
    fed.reRegisterModule.mockImplementation(() => {
      throw new Error("registerRemotes exploded");
    });
    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    fireEvent.click(retry()!);
    await waitFor(() => expect(reload()).not.toBeNull());
    expect(retry()).not.toBeNull();
  });

  it("clears the record on a successful load", async () => {
    fed.loadModuleRemote
      .mockRejectedValueOnce(down())
      .mockRejectedValueOnce(down())
      .mockResolvedValueOnce(good)
      .mockRejectedValue(down());
    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    fireEvent.click(retry()!);
    await waitFor(() => expect(reload()).not.toBeNull());

    fireEvent.click(retry()!);
    expect(await screen.findByText("page contract 1")).toBeTruthy();
    cleanup();

    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    expect(reload()).toBeNull();
  });

  it("an incompatible outcome of the Retry has neither Retry nor Reload page", async () => {
    fed.loadModuleRemote
      .mockRejectedValueOnce(down())
      .mockRejectedValue(new Error("does not satisfy the requirement of"));
    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    fireEvent.click(retry()!);
    await waitFor(() => expect(screen.getByText("This module is not compatible with this version of uzi.")).toBeTruthy());
    expect(retry()).toBeNull();
    expect(reload()).toBeNull();
    // An incompatible outcome is not recorded: a later transient failure gets Retry alone.
    cleanup();
    fed.loadModuleRemote.mockReset();
    fed.loadModuleRemote.mockRejectedValue(down());
    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    expect(reload()).toBeNull();
  });
});

describe("incompatible", () => {
  it.each([
    ["a shared-version refusal", () => fed.loadModuleRemote.mockRejectedValue(new Error("does not satisfy the requirement of"))],
    ["a contract major of 2", () => fed.loadModuleRemote.mockResolvedValue({ default: Page, hostContract: 2 })],
  ])("%s has no action and a clear message", async (_n, arrange) => {
    arrange();
    mount();
    await screen.findByRole("alert");
    expect(retry()).toBeNull();
    expect(reload()).toBeNull();
    expect(screen.getByText("This module is not compatible with this version of uzi.")).toBeTruthy();
    expect(screen.queryByText(/page contract/)).toBeNull();
  });
});

describe("who sees what", () => {
  it("admins see the reason, the module id and its version", async () => {
    fed.loadModuleRemote.mockResolvedValue({ default: Page, hostContract: 2 });
    mount();
    await screen.findByRole("alert");
    expect(screen.getByText("demo")).toBeTruthy();
    expect(screen.getByText("4.5.6")).toBeTruthy();
    expect(screen.getByTestId("module-error-reason").textContent).toContain("contract 2");
  });

  it("other users see only the generic message", async () => {
    auth.isAdmin = false;
    fed.loadModuleRemote.mockResolvedValue({ default: Page, hostContract: 2 });
    mount();
    await screen.findByRole("alert");
    expect(screen.getByText("This module is not compatible with this version of uzi.")).toBeTruthy();
    expect(screen.queryByText("4.5.6")).toBeNull();
    expect(screen.queryByTestId("module-error-reason")).toBeNull();
  });

  it("other users get a generic message for a transient failure too", async () => {
    auth.isAdmin = false;
    fed.loadModuleRemote.mockRejectedValue(new Error("secret internal detail"));
    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    expect(screen.queryByText(/secret internal detail/)).toBeNull();
  });
});

describe("render errors and stale chunks inside the loaded page", () => {
  it("a render error shows Retry; Retry only remounts (no re-register, no loadRemote)", async () => {
    let armed = false;
    function Flaky() {
      if (!armed) throw new Error("render exploded");
      return <p>recovered</p>;
    }
    fed.loadModuleRemote.mockResolvedValue({ default: Flaky, hostContract: 1 });
    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    expect(screen.getByTestId("module-error-reason").textContent).toBe("render exploded");
    expect(fed.loadModuleRemote).toHaveBeenCalledTimes(1);

    // Retry while still failing keeps the card.
    fireEvent.click(retry()!);
    await waitFor(() => expect(retry()).not.toBeNull());

    armed = true;
    fireEvent.click(retry()!);
    expect(await screen.findByText("recovered")).toBeTruthy();
    expect(fed.reRegisterModule).not.toHaveBeenCalled();
    expect(fed.loadModuleRemote).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["Chromium", "Failed to fetch dynamically imported module: http://localhost/modules/demo/assets/Sub-1.js"],
    ["Firefox", "error loading dynamically imported module: http://localhost/modules/demo/assets/Sub-1.js"],
    ["Safari, which names no URL", "Importing a module script failed."],
  ])("a lazy chunk failure (%s) after the load offers Reload page, not Retry", async (_n, message) => {
    function LazyBoom(): never {
      throw new TypeError(message);
    }
    fed.loadModuleRemote.mockResolvedValue({ default: LazyBoom, hostContract: 1 });
    mount();
    await waitFor(() => expect(reload()).not.toBeNull());
    expect(retry()).toBeNull();
  });

  it("an unrelated render error with a similar-looking message is still a Retry case", async () => {
    function Boom(): never {
      throw new Error("boom");
    }
    fed.loadModuleRemote.mockResolvedValue({ default: Boom, hostContract: 1 });
    mount();
    await waitFor(() => expect(retry()).not.toBeNull());
    expect(reload()).toBeNull();
  });
});

describe("values that cannot be stringified", () => {
  it.each([
    ["a null-prototype object", () => Object.create(null)],
    ["an object whose toString throws", () => ({ toString: () => { throw new Error("no"); } })],
  ])("%s thrown during render shows the module error card inside the shell", async (_n, make) => {
    function Boom(): never {
      throw make();
    }
    fed.loadModuleRemote.mockResolvedValue({ default: Boom, hostContract: 1 });
    render(
      <MemoryRouter>
        <header>shell</header>
        <ModuleLoader entry={entry} />
      </MemoryRouter>,
    );
    await waitFor(() => expect(retry()).not.toBeNull());
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText("shell")).toBeTruthy();
    expect(screen.getByTestId("module-error-reason").textContent).toBe("non-Error value thrown");
  });

  // A revoked Proxy cannot be thrown from render: React itself reads `.then` on a thrown object
  // before any boundary runs, and that read throws. The seam this host owns is a failed load.
  it("a revoked Proxy as the load failure shows the module error card inside the shell", async () => {
    const { proxy, revoke } = Proxy.revocable({}, {});
    revoke();
    fed.loadModuleRemote.mockRejectedValue(proxy);
    render(
      <MemoryRouter>
        <header>shell</header>
        <ModuleLoader entry={entry} />
      </MemoryRouter>,
    );
    await waitFor(() => expect(retry()).not.toBeNull());
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText("shell")).toBeTruthy();
    expect(screen.getByTestId("module-error-reason").textContent).toBe("non-Error value thrown");
  });
});
