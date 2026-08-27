// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Docs } from "./Docs";
import { useAuth } from "../auth/AuthContext";

vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));
const mockUseAuth = vi.mocked(useAuth);

// Docs only reads `user?.is_admin`, so a bare user object is enough.
function setAuth(isAdmin: boolean) {
  mockUseAuth.mockReturnValue({ user: { is_admin: isAdmin } } as ReturnType<typeof useAuth>);
}

// These run against the REAL bundled docs/ corpus (the same glob the index
// uses), so they double as the behavioral gate for search: a body-only term is
// found with a marked snippet. `UZI_WORKER_TOKEN` lives only in worker-setup.md
// among the `audience: user` pages, and only in its body (not title/summary).
const BODY_ONLY_TERM = "UZI_WORKER_TOKEN";

// An `audience: operator` body-only term: "reverse proxy" lives in
// configuration.md (operator) and no `audience: user` page.
const OPERATOR_BODY_TERM = "reverse proxy";

beforeEach(() => setAuth(false));

function renderPage() {
  return render(
    <MemoryRouter>
      <Docs />
    </MemoryRouter>,
  );
}

function searchBox(): HTMLInputElement {
  return screen.getByRole("searchbox", { name: "Search docs" }) as HTMLInputElement;
}

afterEach(() => cleanup());

describe("Docs index — default list", () => {
  it("lists the user howtos and shows no body-only terms up front", () => {
    renderPage();
    expect(screen.getByRole("heading", { name: "Worker setup", level: 2 })).toBeTruthy();
    // A body-only term is not surfaced by the plain title+summary index.
    expect(screen.queryByText(new RegExp(BODY_ONLY_TERM))).toBeNull();
    // No result-count line and no <mark>s before a search.
    expect(screen.queryByText(/docs? match/)).toBeNull();
    expect(document.querySelector("mark")).toBeNull();
  });
});

describe("Docs index — search", () => {
  it("finds a body-only term, marks it in the snippet, and links to the page", () => {
    const { container } = renderPage();
    fireEvent.change(searchBox(), { target: { value: BODY_ONLY_TERM } });

    // Exactly one user doc (worker-setup) carries the term.
    expect(screen.getByText("1 doc matches")).toBeTruthy();
    const result = screen.getByRole("heading", { name: "Worker setup", level: 2 }).closest("a")!;
    expect(result.getAttribute("href")).toBe("/docs/worker-setup");

    // The snippet marks the matched term (React <mark>, not raw HTML).
    const marks = Array.from(container.querySelectorAll("mark"));
    expect(marks.length).toBeGreaterThan(0);
    expect(marks.some((m) => m.textContent === BODY_ONLY_TERM)).toBe(true);
    // The snippet text (not just the title) is what surfaced the term.
    expect(within(result).getByText(new RegExp(BODY_ONLY_TERM))).toBeTruthy();
  });

  it("announces the count in an aria-live region scoped to the count line, not the list", () => {
    const { container } = renderPage();
    fireEvent.change(searchBox(), { target: { value: BODY_ONLY_TERM } });
    const live = container.querySelector('[aria-live="polite"]');
    expect(live).toBeTruthy();
    expect(live!.textContent).toBe("1 doc matches");
    // The snippet list is outside the live region — not re-announced per keystroke.
    expect(live!.textContent).not.toContain(BODY_ONLY_TERM);
    expect(live!.querySelector("mark")).toBeNull();
  });

  it("shows a no-results card for a query nothing matches", () => {
    renderPage();
    fireEvent.change(searchBox(), { target: { value: "zzzznotarealterm" } });
    expect(screen.getByText(/No docs match/)).toBeTruthy();
    expect(document.querySelector("mark")).toBeNull();
  });

  it("keeps the full index for an empty or too-short query", () => {
    renderPage();
    const box = searchBox();
    fireEvent.change(box, { target: { value: "w" } }); // 1 char < MIN_QUERY_LENGTH
    expect(screen.queryByText(/docs? match/)).toBeNull();
    expect(screen.getByRole("heading", { name: "Worker setup", level: 2 })).toBeTruthy();
  });

  it("clears the query with the change handler and returns to the full list", () => {
    renderPage();
    const box = searchBox();
    fireEvent.change(box, { target: { value: BODY_ONLY_TERM } });
    expect(screen.getByText("1 doc matches")).toBeTruthy();
    fireEvent.change(box, { target: { value: "" } });
    expect(screen.queryByText(/docs? match/)).toBeNull();
    expect(document.querySelector("mark")).toBeNull();
  });

  it("clears the query on Escape", () => {
    renderPage();
    const box = searchBox();
    fireEvent.change(box, { target: { value: BODY_ONLY_TERM } });
    expect(screen.getByText("1 doc matches")).toBeTruthy();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(box.value).toBe("");
    expect(screen.queryByText(/docs? match/)).toBeNull();
  });

  it("focuses the search box when '/' is pressed outside a field", () => {
    renderPage();
    const box = searchBox();
    expect(box.getAttribute("aria-keyshortcuts")).toBe("/");
    expect(document.activeElement).not.toBe(box);
    fireEvent.keyDown(document, { key: "/" });
    expect(document.activeElement).toBe(box);
  });

  it("shows the '/' shortcut badge until a query is typed", () => {
    renderPage();
    const kbd = () => document.querySelector("kbd");
    expect(kbd()?.textContent).toBe("/");
    fireEvent.change(searchBox(), { target: { value: BODY_ONLY_TERM } });
    expect(kbd()).toBeNull();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(kbd()?.textContent).toBe("/");
  });
});

describe("Docs index — role-aware operator docs (issue #75 M1)", () => {
  it("hides the Admin / operator section and operator search results from a non-admin", () => {
    setAuth(false);
    renderPage();
    expect(screen.queryByRole("heading", { name: "Admin / operator" })).toBeNull();
    // The operator doc "Configuration" is not listed for a non-admin.
    expect(screen.queryByRole("heading", { name: "Configuration", level: 2 })).toBeNull();
    // An operator-only body term returns nothing from the non-admin corpus.
    fireEvent.change(searchBox(), { target: { value: OPERATOR_BODY_TERM } });
    expect(screen.getByText(/No docs match/)).toBeTruthy();
  });

  it("shows the Admin / operator section listing an operator doc for an admin", () => {
    setAuth(true);
    renderPage();
    expect(screen.getByRole("heading", { name: "Admin / operator" })).toBeTruthy();
    // A real operator doc (configuration.md, title "Configuration") is listed;
    // its card heading also carries the "Operator" pill, so match on a substring.
    expect(screen.getByRole("heading", { name: /Configuration/, level: 2 })).toBeTruthy();
  });

  it("finds an operator doc by a body-only term for an admin and links to it", () => {
    setAuth(true);
    renderPage();
    fireEvent.change(searchBox(), { target: { value: OPERATOR_BODY_TERM } });
    const heading = screen.getByRole("heading", { name: /Configuration/, level: 2 });
    const result = heading.closest("a")!;
    expect(result.getAttribute("href")).toBe("/docs/configuration");
    // The result carries the "Operator" pill so it is recognizable in the
    // combined search results.
    expect(within(result).getByText("Operator")).toBeTruthy();
  });
});
