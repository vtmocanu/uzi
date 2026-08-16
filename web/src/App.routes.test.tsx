// @vitest-environment jsdom
//
// Mock-mode route smoke test (PRD #311 M2, closes gap 2: "renders without
// throwing"). Iterates the SINGLE-SOURCE route table exported from App.tsx and
// mounts every route through the real AuthProvider + RouteGuards under
// VITE_UZI_MOCK=1, so a page that throws on the mock fixtures (or reaches for a
// live backend) fails here loudly. Because both the router and this test consume
// APP_ROUTES, a new route cannot be added without appearing here.
//
// Forcing mock mode FAITHFULLY (not `vi.mock("../lib/api")`): we stub the build
// flag and dynamically import App AFTER the stub, so the real
// `MOCK_MODE ? mockApi : realApi` wiring in lib/api.ts evaluates with MOCK_MODE
// true and swaps in mockApi exactly as the shipped bundle does. A static
// `import App from "./App"` (or importing anything that transitively loads
// ../lib/api) would capture MOCK_MODE=false before the stub. Vitest's default is
// unstubEnvs:false, so we unstub in afterAll to keep the flag from leaking to the
// ~60 other files that import ../lib/api.
import { Component, type ReactElement, type ReactNode } from "react";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// Populated by beforeAll from the mock-mode module graph. Never import these
// statically — see the file header.
let App: () => ReactElement;
let APP_ROUTES: typeof import("./App").APP_ROUTES;
let AuthProvider: typeof import("./auth/AuthContext").AuthProvider;
let mockApi: typeof import("./mocks/mockApi").mockApi;
// getDoc is the SYNC lib/docs resolver /docs/:slug uses (not a mockApi method);
// populated from the same mock-mode graph in beforeAll so the build flag is never
// captured early. See the "fixture id resolution" block below.
let getDoc: typeof import("./lib/docs").getDoc;

// The placeholder RouteGuards render while AuthProvider resolves the session.
// Matched exactly (real ellipsis glyph) — the settle waits for it to disappear
// and the non-vacuous assertion excludes it as "real content".
const LOADING = "Loading…";

// A test-local error boundary: records the first render error a route throws so
// the assertion can check a REAL signal (page rendered without throwing) rather
// than the absence of a string that can never appear.
class RouteErrorBoundary extends Component<
  { onError: (e: Error) => void; children: ReactNode },
  { caught: boolean }
> {
  state = { caught: false };
  static getDerivedStateFromError(): { caught: boolean } {
    return { caught: true };
  }
  componentDidCatch(error: Error) {
    this.props.onError(error);
  }
  render() {
    if (this.state.caught) return <div data-testid="route-threw" />;
    return this.props.children;
  }
}

beforeAll(async () => {
  vi.stubEnv("VITE_UZI_MOCK", "1");
  vi.resetModules();
  // All three come from the SAME fresh (mock-mode) module graph, so mockApi below
  // mutates the very `state.session` the App's AuthProvider reads through api.me().
  const appMod = await import("./App");
  App = appMod.default;
  APP_ROUTES = appMod.APP_ROUTES;
  ({ AuthProvider } = await import("./auth/AuthContext"));
  ({ mockApi } = await import("./mocks/mockApi"));
  // lib/docs imports only markdown/parsing helpers (no ../lib/api), but import it
  // here anyway so it comes from the mock-mode graph, consistent with the file's
  // "never capture the build flag early" discipline.
  ({ getDoc } = await import("./lib/docs"));
});

afterAll(() => {
  vi.unstubAllEnvs();
});

// mountRoute renders one path through the app's real providers (mirroring
// main.tsx: MemoryRouter → AuthProvider → App, no StrictMode), waits for the
// guards to settle, and returns the caught error (if any) plus the container.
// console.error is suppressed across the render because a caught render error is
// logged loudly by React; the Part C probe relies on that error still being
// RECORDED by the boundary, which it is.
async function mountRoute(path: string) {
  const holder: { error: Error | null } = { error: null };
  const errSpy = vi.spyOn(console, "error").mockImplementation(() => {});
  const { container, unmount } = render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider>
        <RouteErrorBoundary
          onError={(e) => {
            holder.error = e;
          }}
        >
          <App />
        </RouteErrorBoundary>
      </AuthProvider>
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.queryByText(LOADING)).toBeNull());
  errSpy.mockRestore();
  return { holder, container, unmount };
}

// The non-vacuous per-route assertion: the boundary caught nothing AND the
// container rendered real content (non-empty, not just the loading placeholder).
function expectRenderedCleanly(
  label: string,
  holder: { error: Error | null },
  container: HTMLElement,
) {
  expect(holder.error, `${label} threw: ${holder.error?.message ?? ""}`).toBeNull();
  const text = (container.textContent ?? "").trim();
  expect(text.length, `${label} rendered empty`).toBeGreaterThan(0);
  expect(text, `${label} stuck on loading placeholder`).not.toBe(LOADING);
}

describe("mock-mode route smoke", () => {
  // The GuestRoute pages (/,/login,/register) redirect a signed-in user to
  // /dashboard, so they must be mounted signed OUT to render at all. logout()
  // nulls the session; the authed cases below re-login, so no signed-out state
  // leaks across cases.
  it(
    "renders every guest route signed out",
    async () => {
      for (const route of APP_ROUTES.filter((r) => r.guard === "guest")) {
        await mockApi.logout();
        const { holder, container, unmount } = await mountRoute(route.sample ?? route.path);
        expectRenderedCleanly(`guest ${route.path}`, holder, container);
        unmount();
        cleanup();
      }
    },
    30000,
  );

  // Every non-guest route (protected/admin/public/redirect) renders as the
  // signed-in admin the mock seeds. Param routes mount at their fixture-valid
  // `sample` so a 404 empty state is not misread as a crash; redirects render
  // their <Navigate> target's page.
  it(
    "renders every authed/public route as admin",
    async () => {
      for (const route of APP_ROUTES.filter((r) => r.guard !== "guest")) {
        // Re-establish the signed-in admin session (mockApi's own login path; any
        // email that is not a seeded non-admin persona signs in as admin).
        await mockApi.login("vlad@uzi.local", "x");
        const { holder, container, unmount } = await mountRoute(route.sample ?? route.path);
        expectRenderedCleanly(`${route.guard} ${route.path}`, holder, container);
        unmount();
        cleanup();
      }
    },
    30000,
  );

  it("has unique paths across APP_ROUTES", () => {
    const paths = APP_ROUTES.map((r) => r.path);
    expect(new Set(paths).size, "duplicate path in APP_ROUTES").toBe(paths.length);
  });

  it("redirects an unknown path to the catch-all without throwing", async () => {
    await mockApi.login("vlad@uzi.local", "x");
    const { holder, container, unmount } = await mountRoute("/no-such-page");
    // "*" → <Navigate to="/">; "/" is a GuestRoute, so a signed-in admin lands on
    // /dashboard. The point is only that the catch-all resolves to a real page and
    // nothing throws on the way.
    expectRenderedCleanly("catch-all /no-such-page", holder, container);
    unmount();
    cleanup();
  });
});

// The route smoke test above mounts each param route at its `sample` and only
// checks it renders SOMETHING — but a stale/renamed sample 404s and STILL renders
// the page's not-found empty state, so that smoke would keep passing while no
// longer exercising the data-loaded path. This block closes that gap: it derives
// each id from APP_ROUTES via extractParams and resolves it against the SAME data
// source the page uses, so a fixture rename fails loudly here.

// extractParams zips the pattern's segments against the sample's and captures a
// value for every ":name" segment. Ids are DERIVED this way, never hardcoded, so a
// renamed sample flows through to the resolver automatically.
function extractParams(pathPattern: string, sample: string): Record<string, string> {
  const patternSegs = pathPattern.split("/");
  const sampleSegs = sample.split("/");
  const params: Record<string, string> = {};
  patternSegs.forEach((seg, i) => {
    if (seg.startsWith(":")) params[seg.slice(1)] = sampleSegs[i];
  });
  return params;
}

// One resolver per sampled param-route `path`, each mirroring exactly how that
// page resolves its id. The getters that THROW ApiError(404,…) on a miss reject;
// getDoc and the chat find() return undefined on a miss (see inline notes) — the
// negative control below handles both shapes.
const paramResolvers: Record<
  string,
  (params: Record<string, string>) => Promise<unknown>
> = {
  "/runs/:id": async (params) => mockApi.getRun(params.id),
  // AgentDetail: getAgentTemplate, then fall back to getBuiltinAgentTemplate.
  "/agents/:id": async (params) =>
    mockApi.getAgentTemplate(params.id).catch(() => mockApi.getBuiltinAgentTemplate(params.id)),
  "/repos/:id/board": async (params) => mockApi.getBoard(params.id),
  "/repos/:repoId/issues/:iid": async (params) =>
    mockApi.getIssue(params.repoId, Number(params.iid)),
  // /docs/:slug resolves via lib/docs.getDoc (SYNC, returns Doc|undefined, does NOT
  // throw) — it is NOT a mockApi method. Wrapped in this async fn for a uniform shape.
  "/docs/:slug": async (params) => getDoc(params.slug),
  // There is no getChat: ChatConversation resolves via listChats().chats.find(...),
  // which returns undefined (does NOT throw) on a miss.
  "/chat/:id": async (params) =>
    (await mockApi.listChats()).chats.find((c) => c.id === params.id),
};

// awaitsTruthy folds both miss shapes (reject OR falsy resolve) into a boolean, so
// the negative control can assert "did NOT resolve truthy" uniformly.
async function awaitsTruthy(p: Promise<unknown>): Promise<boolean> {
  try {
    return Boolean(await p);
  } catch {
    return false;
  }
}

describe("fixture id resolution for sampled param routes", () => {
  // Coverage guard: the resolver keys and the current sampled route paths must be
  // the SAME set. A new param route with a sample but no resolver, or a resolver
  // whose route lost its sample / was renamed, fails here loudly.
  it("has a resolver for exactly the sampled param routes", () => {
    const sampledPaths = APP_ROUTES.filter((r) => r.sample).map((r) => r.path);
    for (const path of sampledPaths) {
      expect(Object.keys(paramResolvers), `no resolver for sampled route ${path}`).toContain(path);
    }
    for (const key of Object.keys(paramResolvers)) {
      expect(sampledPaths, `stale resolver key ${key}`).toContain(key);
    }
  });

  it("resolves every sampled fixture id against the page's real data source", async () => {
    for (const route of APP_ROUTES.filter((r) => r.sample)) {
      const resolve = paramResolvers[route.path];
      const params = extractParams(route.path, route.sample!);
      await expect(
        resolve(params),
        `${route.path} sample ${route.sample} did not resolve`,
      ).resolves.toBeTruthy();
    }
  });

  // Non-vacuity control: with every id replaced by a value no fixture uses, every
  // resolver must MISS (reject or resolve falsy). Number("__nonexistent__") is NaN,
  // so getIssue misses too. If a resolver resolved truthy on bogus ids the positive
  // test above would be meaningless.
  it("does not resolve truthy for bogus ids", async () => {
    for (const route of APP_ROUTES.filter((r) => r.sample)) {
      const resolve = paramResolvers[route.path];
      const bogus: Record<string, string> = {};
      for (const name of Object.keys(extractParams(route.path, route.sample!))) {
        bogus[name] = "__nonexistent__";
      }
      expect(
        await awaitsTruthy(resolve(bogus)),
        `${route.path} unexpectedly resolved truthy for bogus ids`,
      ).toBe(false);
    }
  });
});
