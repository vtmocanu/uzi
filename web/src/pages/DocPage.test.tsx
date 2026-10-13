// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Link, MemoryRouter, Routes, Route } from "react-router-dom";
import { DocPage } from "./DocPage";
import { useAuth } from "../auth/AuthContext";

vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));
const mockUseAuth = vi.mocked(useAuth);

// DocPage only reads `user?.is_admin`, so a bare user object is enough.
function setAuth(isAdmin: boolean) {
  mockUseAuth.mockReturnValue({ user: { is_admin: isAdmin } } as ReturnType<typeof useAuth>);
}

// Renders DocPage at a real bundled slug via the same `/docs/:slug` route the
// App wires, so useParams resolves the slug.
function renderAt(slug: string) {
  return render(
    <MemoryRouter initialEntries={[`/docs/${slug}`]}>
      <Routes>
        <Route path="/docs/:slug" element={<DocPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

const originalScroll = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollIntoView");
const scroll = vi.fn();
beforeEach(() => {
  setAuth(false);
  scroll.mockReset();
  Object.defineProperty(HTMLElement.prototype, "scrollIntoView", { configurable: true, value: scroll });
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  if (originalScroll) Object.defineProperty(HTMLElement.prototype, "scrollIntoView", originalScroll);
  else Reflect.deleteProperty(HTMLElement.prototype, "scrollIntoView");
});

// `configuration` is an `audience: operator` page (title "Configuration"),
// routable in-app only for admins (issue #75 M1).
describe("DocPage fragment navigation", () => {
  it("uses global DOM lookup for #root, even on not-found routes and unchanged hashes", () => {
    render(
      <div id="root">
        <MemoryRouter initialEntries={["/docs/missing#root"]}>
          <Link to="/docs/cli#root">Next doc</Link>
          <Routes><Route path="/docs/:slug" element={<DocPage />} /></Routes>
        </MemoryRouter>
      </div>,
    );
    expect(screen.getByRole("heading", { name: "Doc not found" })).toBeTruthy();
    expect(scroll).toHaveBeenCalledExactlyOnceWith({ block: "start" });
    expect(scroll.mock.instances[0]).toBe(document.getElementById("root"));
    fireEvent.click(screen.getByRole("link", { name: "Next doc" }));
    expect(screen.getByRole("heading", { name: "uzi CLI", level: 1 })).toBeTruthy();
    expect(scroll).toHaveBeenCalledTimes(2);
    expect(scroll.mock.instances[1]).toBe(document.getElementById("root"));
    expect(scroll.mock.calls[1]).toEqual([{ block: "start" }]);
  });

  it("scrolls exactly once to a real bundled heading with start alignment", () => {
    setAuth(true);
    renderAt("configuration#forge-integration");
    const heading = screen.getByRole("heading", { name: "Forge integration", level: 2 });
    expect(heading.id).toBe("forge-integration");
    expect(scroll).toHaveBeenCalledExactlyOnceWith({ block: "start" });
    expect(scroll.mock.instances[0]).toBe(heading);
  });

  it.each(["", "#unknown-heading", "#%E0%A4%A"])("does not scroll for %s", (hash) => {
    setAuth(true);
    renderAt("configuration" + hash);
    expect(screen.getByRole("heading", { name: "Forge integration" })).toBeTruthy();
    expect(scroll).not.toHaveBeenCalled();
  });

  it("handles hash-only and slug navigation, including decoded hashes", () => {
    setAuth(true);
    render(
      <MemoryRouter initialEntries={["/docs/configuration"]}>
        <Link to="/docs/configuration#forge%2Dintegration">Hash</Link>
        <Link to="/docs/cli#uzi-cli">Slug</Link>
        <Routes><Route path="/docs/:slug" element={<DocPage />} /></Routes>
      </MemoryRouter>,
    );
    expect(scroll).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("link", { name: "Hash" }));
    expect(scroll).toHaveBeenCalledExactlyOnceWith({ block: "start" });
    expect(scroll.mock.instances[0]).toBe(screen.getByRole("heading", { name: "Forge integration" }));
    fireEvent.click(screen.getByRole("link", { name: "Slug" }));
    expect(scroll).toHaveBeenCalledTimes(2);
    expect(scroll.mock.instances[1]).toBe(screen.getByRole("heading", { name: "uzi CLI", level: 1 }));
    expect(scroll.mock.calls[1]).toEqual([{ block: "start" }]);
  });
});

describe("DocPage — role-aware operator routing", () => {
  it("shows a not-found state for an operator doc to a non-admin", () => {
    setAuth(false);
    renderAt("configuration");
    expect(screen.getByRole("heading", { name: "Doc not found" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Configuration", level: 1 })).toBeNull();
  });

  it("renders the operator doc article for an admin", () => {
    setAuth(true);
    const { container } = renderAt("configuration");
    expect(screen.queryByRole("heading", { name: "Doc not found" })).toBeNull();
    // The doc body (its H1) renders inside the article shell.
    expect(container.querySelector("article")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Configuration", level: 1 })).toBeTruthy();
  });
});
