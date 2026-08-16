// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router-dom";
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

beforeEach(() => setAuth(false));
afterEach(() => cleanup());

// `configuration` is an `audience: operator` page (title "Configuration"),
// routable in-app only for admins (issue #75 M1).
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
