// @vitest-environment jsdom
//
// OccurrenceFileIssue is M3's ONLY forge-writing web path and it shipped with zero tests:
// nothing opened the Judge occurrence expander at all, so every control below — the draft
// gate, the provenance box, the https guard, the error paths, the CSRF-bearing write — was
// covered only by the argument that it is a line-by-line duplicate of RunView's filer. That
// argument is about the CODE; these are about the BEHAVIOUR, and the duplication is exactly
// why it needs its own tests rather than a refactor (PRD #98 N2: a test on the duplicate is
// wanted, not a dedup).
//
// Two query styles are deliberately avoided here. `role="status"` is ambiguous in this
// component (the "Loading draft…" line and the repo `default_note` both carry it), and it is
// ambiguous app-wide besides — RateLimitAnnouncer is an always-present empty status region.
// Everything below queries by text or by an explicitly scoped row.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { OccurrenceFileIssue } from "./OccurrenceFileIssue";
import { Judge } from "../pages/Judge";
import {
  api,
  ApiError,
  type IssueDraft,
  type JudgeBacklog,
  type JudgeRecommendationGroup,
  type Repo,
} from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getJudgeBacklog: vi.fn(),
      bulkSetJudgeDisposition: vi.fn(),
      deleteDisposition: vi.fn(),
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
      getIssueDraft: vi.fn(),
      fileIssue: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function repoOpt(id: string, path: string): Repo {
  return {
    id,
    connection_id: "c1",
    forge_project_id: 1,
    path_with_namespace: path,
    web_url: `https://gitlab.example/${path}`,
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: false,
    repo_claudemd_enabled: false,
    repo_devbox_opt_in: false,
    pipeline: null,
  };
}

function draftFixture(over: Partial<IssueDraft> = {}): IssueDraft {
  return {
    default_repo_id: "repo1",
    title: "Improve the poller: api/internal/poller",
    description: "## What the judge found\n\n````\nrationale\n````",
    labels: ["PRD", "PRDLESS"],
    provenance: "from vlad's worker, run 8f2c1d04",
    default_note: "Defaulted to the judged run's repo.",
    ...over,
  };
}

function occ(over: Partial<JudgeRecommendationGroup["occurrences"][number]> = {}) {
  return {
    run_id: "run-a",
    run_title: "run a",
    review_id: "rev-1",
    rec_id: "rec-a",
    verdict: "issues" as const,
    confidence: "" as const,
    bucket: "todo" as const,
    ...over,
  };
}

// Two occurrences with DISTINCT (run_id, rec_id) pairs, and the tests below act on the
// SECOND one. With one shared pair, wiring the filer to `group.occurrences[0]` would satisfy
// every assertion here unchanged — the divergence is what makes them discriminate.
function backlog(over: Partial<JudgeBacklog> = {}): JudgeBacklog {
  return {
    bucket: "todo",
    run: "",
    groups: [
      {
        category: "improve_uzi",
        target: "api/internal/poller",
        bucket: "todo",
        open_count: 2,
        run_count: 2,
        rationale_preview: "Queue-to-claim latency dominated the run.",
        occurrences: [
          occ({ run_id: "run-a", rec_id: "rec-a", run_title: "run a" }),
          occ({ run_id: "run-b", rec_id: "rec-b", run_title: "run b" }),
        ],
      },
    ],
    truncated: false,
    triage: { total: 2, todo: 2, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
    ...over,
  };
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user: { judge_enabled: true } } as unknown as ReturnType<typeof useAuth>);
  mockApi.listRepos.mockResolvedValue({ repos: [repoOpt("repo1", "vtmocanu/uzi")] });
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.clearAllMocks();
  // BOTH LINES BELOW ARE LOAD-BEARING, AND NEITHER HAS A TEST — their gate is this
  // teardown, the way the `Set(hrefs)` line in Notifications.test.tsx has the fixture as
  // its. So the measurement is recorded here rather than left in a commit message: a probe
  // appended after the CSRF test was run four ways, then deleted (nothing in production
  // code can fail it, which makes it an instrument, not a test).
  //
  // REMOVE `mockReset()` → the probe fails `expected Promise{…} to be undefined`.
  // `clearAllMocks` clears CALL HISTORY, not implementations, and nothing global undoes
  // them (no `mockReset`/`restoreMocks` in vite.config.ts or src/test-setup.ts), so the
  // CSRF test's `mockImplementation(real.fileIssue)` survives into every test written after
  // it — silently running the real HTTP client, with `unstubAllGlobals` having already
  // removed the fetch stub that might have caught it. Its failure would read as a component
  // bug. This leak is UNCONDITIONAL.
  mockApi.fileIssue.mockReset();
  // REMOVE the line below → the probe fails `expected 'uzi_csrf=tok-abc123' not to contain
  // 'uzi_csrf'` (the exact needle depends on the probe's assertion; the leak is the point).
  // This one is CONDITIONAL: the cookie used to be cleared in the CSRF test's BODY, which
  // works right up until an assertion above it throws — measured by forcing exactly that.
  // Teardown runs either way; a test body does not.
  document.cookie = "uzi_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT";
  //
  // The other two runs are the positive controls: with both lines present the probe passes,
  // and it still passes when the CSRF test is forced to throw — which is the case the
  // in-body cleanup structurally could not cover.
  //
  // One trap for whoever re-derives this: `getMockImplementation()` being `undefined` is NOT
  // the check. vitest 2.1.9's `mockReset()` installs `() => void 0`, so that assertion fails
  // on correctly-reset code. Ask the behavioural question instead — does the mock still call
  // through (returns undefined, zero fetch calls against a spy).
});

// Renders the component on its own, with the draft already scripted, and opens it — the
// shape most of the controls below are asserted on. `runId`/`recId` are the occurrence's,
// which is what the page passes down.
async function openDraft(over: Partial<IssueDraft> = {}, repos: Repo[] = [repoOpt("repo1", "vtmocanu/uzi")]) {
  mockApi.getIssueDraft.mockResolvedValue({ draft: draftFixture(over) });
  const onFiled = vi.fn();
  const view = render(<OccurrenceFileIssue runId="run-b" recId="rec-b" repos={repos} onFiled={onFiled} />);
  fireEvent.click(screen.getByText("File issue"));
  await screen.findByText("Draft issue");
  return { ...view, onFiled };
}

// Issue #124 / LOW-1. The SAME `setTitle(draft.title)` / `setDescription(draft.description)`
// pair exists here and in RunView's RecommendationFiler, and this one is reachable only
// through the occurrence expander — which is exactly how it went unnoticed. Two components,
// two fixes; asserting only the page-level one would leave this path filing hostile text.
describe("OccurrenceFileIssue — the seeded draft carries no format characters (#124)", () => {
  it("strips Cf from title and description at the seed, not from what the user types", async () => {
    await openDraft({
      title: "Improve the \u202Ereviewer",
      description: "## What the judge found\n\n\u200Bmalicious\u202E line",
    });
    const title = screen.getByDisplayValue(/Improve the/) as HTMLInputElement;
    const body = screen.getByDisplayValue(/What the judge found/) as HTMLTextAreaElement;
    expect(title.value).not.toMatch(/[\p{Cf}]/u);
    expect(body.value).not.toMatch(/[\p{Cf}]/u);
    expect(title.value).toBe("Improve the reviewer");
    expect(body.value).toContain("\n");
  });
});

describe("OccurrenceFileIssue — reaching it through the Judge occurrence expander (PRD #98 M3)", () => {
  it("is behind the expander, and drafts for the occurrence clicked — not the group's first", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(backlog());
    mockApi.getIssueDraft.mockResolvedValue({ draft: draftFixture() });
    render(
      <MemoryRouter>
        <Judge />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    // Collapsed, the filer does not exist at all — the expander is the only way in, which is
    // why nothing in the suite had ever reached it.
    expect(screen.queryByText("File issue")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /Expand occurrences/ }));
    await waitFor(() => expect(screen.getAllByText("File issue")).toHaveLength(2));

    // The SECOND occurrence's button, scoped by its run title.
    const rowB = screen.getByText("run b").closest("li")!;
    fireEvent.click(within(rowB).getByText("File issue"));

    // The draft is read for that occurrence's own (run, rec) address. A filer wired to the
    // group's first occurrence would ask for ("run-a", "rec-a") here.
    expect(mockApi.getIssueDraft).toHaveBeenCalledTimes(1);
    expect(mockApi.getIssueDraft).toHaveBeenCalledWith("run-b", "rec-b");
    // ...and only the clicked row opened a draft; the sibling still offers its button.
    const rowA = screen.getByText("run a").closest("li")!;
    expect(within(rowA).getByText("File issue")).toBeTruthy();
  });

  it("re-reads the backlog after a successful file, so the group's rollup can move", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(backlog());
    mockApi.getIssueDraft.mockResolvedValue({ draft: draftFixture() });
    mockApi.fileIssue.mockResolvedValue({
      issue: { iid: 71, web_url: "https://gitlab.example/vtmocanu/uzi/-/issues/71", title: "t" },
    });
    render(
      <MemoryRouter>
        <Judge />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    expect(mockApi.getJudgeBacklog).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: /Expand occurrences/ }));

    const rowB = await waitFor(() => screen.getByText("run b").closest("li")!);
    fireEvent.click(within(rowB).getByText("File issue"));
    fireEvent.click(await screen.findByText("Create issue"));

    // The write carries the clicked occurrence's address and the draft's fields verbatim.
    await waitFor(() =>
      expect(mockApi.fileIssue).toHaveBeenCalledWith("run-b", "rec-b", {
        repo_id: "repo1",
        title: draftFixture().title,
        description: draftFixture().description,
      }),
    );
    // onFiled is the page's `load`: the coordinate moved to the `filed` rung, so the backlog
    // must be re-read rather than the row being patched client-side.
    await waitFor(() => expect(mockApi.getJudgeBacklog).toHaveBeenCalledTimes(2));
  });
});

describe("OccurrenceFileIssue — the filed row and its link guard", () => {
  it("links a settled https issue and drops the File-issue affordance", () => {
    render(
      <OccurrenceFileIssue
        runId="run-b"
        recId="rec-b"
        filed={{ issue_iid: 91, issue_url: "https://gitlab.example/vtmocanu/uzi/-/issues/91", filed_at: "2026-07-20T10:00:00Z" }}
        repos={[repoOpt("repo1", "vtmocanu/uzi")]}
      />,
    );
    const link = screen.getByRole("link", { name: /#91/ });
    expect(link.getAttribute("href")).toBe("https://gitlab.example/vtmocanu/uzi/-/issues/91");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    // A filed coordinate offers no second filing.
    expect(screen.queryByText("File issue")).toBeNull();
  });

  it("renders a non-https filed URL as plain text, never as a link", () => {
    const { container } = render(
      <OccurrenceFileIssue
        runId="run-b"
        recId="rec-b"
        // The issue URL is judge-adjacent data that reaches the DOM as an href — this is the
        // sink isHttpsUrl exists for. A `javascript:` value must produce NO anchor at all.
        filed={{ issue_iid: 91, issue_url: "javascript:alert(1)", filed_at: "2026-07-20T10:00:00Z" }}
        repos={[repoOpt("repo1", "vtmocanu/uzi")]}
      />,
    );
    // THE SECURITY PROPERTY FIRST: no anchor was rendered, so nothing carries the scheme.
    expect(container.querySelector("a")).toBeNull();
    // ...and the issue is still identified, as inert text (the affordance degrades, it does
    // not vanish). Both halves are needed: dropping the row entirely would satisfy the first.
    expect(screen.getByText("#91")).toBeTruthy();
  });
});

describe("OccurrenceFileIssue — the draft gate", () => {
  it("blocks Create until a repo is chosen when the server resolved no default", async () => {
    await openDraft({ default_repo_id: "", default_note: "No uzi repo is configured." });

    const create = () => screen.getByText("Create issue") as HTMLButtonElement;
    expect(create().disabled).toBe(true);
    expect(screen.getByText("No uzi repo is configured.")).toBeTruthy();

    fireEvent.change(screen.getByRole("combobox"), { target: { value: "repo1" } });
    expect(create().disabled).toBe(false);
  });

  it("blocks Create on a whitespace-only title, not merely an empty one", async () => {
    await openDraft();
    const create = () => screen.getByText("Create issue") as HTMLButtonElement;
    // The draft opened with a default repo and a templated title, so it starts enabled.
    expect(create().disabled).toBe(false);

    const titleInput = screen.getByDisplayValue(draftFixture().title);
    // "   " is the discriminating value: a `title === ""` check would let this through and
    // POST a blank-titled issue to the forge.
    fireEvent.change(titleInput, { target: { value: "   " } });
    expect(create().disabled).toBe(true);

    fireEvent.change(titleInput, { target: { value: "a real title" } });
    expect(create().disabled).toBe(false);
  });
});

describe("OccurrenceFileIssue — the provenance box (#68 Decision 8)", () => {
  it("names whose worker produced the text, and renders it as escaped characters", async () => {
    // Provenance is worker-influenced text on a surface an ADMIN may use to publish another
    // user's review to a forge, so it is both prominent and inert.
    const { container } = await openDraft({ provenance: "from <img src=x onerror=alert(1)>'s worker, run 8f2c1d04" });

    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByText(/Source:/)).toBeTruthy();
    expect(screen.getByText(/from <img src=x onerror=alert\(1\)>'s worker, run 8f2c1d04/)).toBeTruthy();
  });

  it("renders no Source box at all when the draft carries no provenance", async () => {
    await openDraft({ provenance: "" });
    // An empty box labelled "Source:" would state a provenance nobody supplied.
    expect(screen.queryByText(/Source:/)).toBeNull();
    // The rest of the draft is still there — this is the empty-provenance case, not a
    // failed draft load.
    expect(screen.getByDisplayValue(draftFixture().title)).toBeTruthy();
  });
});

describe("OccurrenceFileIssue — the write's failure paths", () => {
  it("keeps the draft open with the user's edits when the forge rejects the write", async () => {
    await openDraft();
    mockApi.fileIssue.mockRejectedValue(new ApiError(502, "could not create the issue on the forge: the forge rejected the request (403)"));

    fireEvent.change(screen.getByDisplayValue(draftFixture().title), { target: { value: "my edited title" } });
    fireEvent.click(screen.getByText("Create issue"));

    expect(await screen.findByText(/the forge rejected the request/i)).toBeTruthy();
    // The edit survives, and the draft did NOT collapse to a filed row — a failed write must
    // not look like a filing.
    expect(screen.getByDisplayValue("my edited title")).toBeTruthy();
    expect(screen.queryByText("Filed.")).toBeNull();
  });

  it("surfaces the forge limiter's own message when the write is rate limited", async () => {
    await openDraft();
    // The per-user forge limiter answers 429 on this route; the component must show the
    // server's message rather than a generic failure, because "wait and retry" and "this
    // will never work" are different instructions to the user.
    mockApi.fileIssue.mockRejectedValue(new ApiError(429, "too many forge requests, slow down"));

    fireEvent.click(screen.getByText("Create issue"));
    expect(await screen.findByText(/too many forge requests, slow down/i)).toBeTruthy();
    expect(screen.getByText("Create issue")).toBeTruthy();
  });

  it("falls back to a generic message for a non-API failure", async () => {
    await openDraft();
    // A transport-level failure is not an ApiError and carries no server message.
    mockApi.fileIssue.mockRejectedValue(new TypeError("network down"));

    fireEvent.click(screen.getByText("Create issue"));
    expect(await screen.findByText("Could not file the issue")).toBeTruthy();
    // The raw exception text never reaches the user.
    expect(screen.queryByText(/network down/)).toBeNull();
  });

  it("recovers from a failed draft load with Retry, and with Cancel", async () => {
    mockApi.getIssueDraft.mockRejectedValueOnce(new ApiError(500, "boom"));
    render(<OccurrenceFileIssue runId="run-b" recId="rec-b" repos={[repoOpt("repo1", "vtmocanu/uzi")]} />);

    fireEvent.click(screen.getByText("File issue"));
    expect(await screen.findByText("boom")).toBeTruthy();

    // Retry re-reads and actually recovers — asserting the button EXISTS would not show that
    // it does anything.
    mockApi.getIssueDraft.mockResolvedValue({ draft: draftFixture() });
    fireEvent.click(screen.getByText("Retry"));
    expect(await screen.findByText("Draft issue")).toBeTruthy();
    expect(mockApi.getIssueDraft).toHaveBeenCalledTimes(2);
  });

  it("Cancel dismisses a failed draft and restores the idle button (no dead end)", async () => {
    mockApi.getIssueDraft.mockRejectedValue(new ApiError(500, "boom"));
    render(<OccurrenceFileIssue runId="run-b" recId="rec-b" repos={[repoOpt("repo1", "vtmocanu/uzi")]} />);

    fireEvent.click(screen.getByText("File issue"));
    fireEvent.click(await screen.findByText("Cancel"));
    expect(await screen.findByText("File issue")).toBeTruthy();
  });
});

describe("OccurrenceFileIssue — a created-with-warning is a success, not a retry signal", () => {
  it("shows the link and the warning together", async () => {
    const { onFiled } = await openDraft();
    mockApi.fileIssue.mockResolvedValue({
      issue: { iid: 71, web_url: "https://gitlab.example/vtmocanu/uzi/-/issues/71", title: "t" },
      warning: "the issue was created but its local link could not be settled",
    });

    fireEvent.click(screen.getByText("Create issue"));

    const link = await screen.findByRole("link", { name: /#71/ });
    expect(link.getAttribute("href")).toBe("https://gitlab.example/vtmocanu/uzi/-/issues/71");
    expect(screen.getByText(/local link could not be settled/)).toBeTruthy();
    // It is a SUCCESS: the page is told to re-read, and no error is shown alongside.
    expect(onFiled).toHaveBeenCalledTimes(1);
    expect(screen.getByText("Filed.")).toBeTruthy();
  });
});

describe("OccurrenceFileIssue — the write is issued through the app's API client", () => {
  // READ THE BOUND BEFORE CITING THIS TEST. It asserts the BROWSER half of the write and
  // nothing else. The other tests here stub `api.fileIssue`, so they can only show WHICH
  // call is made, never how it goes out; this one runs the REAL client (api.ts's request())
  // over a stubbed fetch, so the POST route, the same-origin credentials and the
  // X-CSRF-Token echo of the readable cookie are observed rather than assumed. A
  // hand-rolled `fetch()` added to this component later is what it would catch.
  //
  // IT PROVES NOTHING ABOUT ENFORCEMENT, WHICH IS ENTIRELY SERVER-SIDE. Re-derived from
  // api/internal/handler/handler.go: the draft GET
  // (`/{id}/review/recommendations/{recID}/issue-draft`) sits in the RequireUser group and
  // is NOT forge-limited, while the POST (`…/issue`) is mounted inside the RequireAuth
  // cookie+CSRF group `.With(forgeLimiter.PerUserMiddleware)`. No test that mocks or stubs
  // the transport can observe a middleware chain — it can only see that a function was
  // called. So this must never be cited as covering "the CSRF path" or "the forge limiter".
  //
  // Where those controls ARE covered, measured rather than assumed
  // (api/internal/handler/review_issue_livedb_test.go):
  //   - owner/admin scoping: covered (NonOwnerNotFound, AdminNonOwnedRepoNotFound, …)
  //   - the write-boundary sanitizer: covered (WriteBoundarySanitizesClientBody,
  //     TitleSanitizedAtPost, EmptyTitleRejected)
  //   - the forge limiter's MOUNT: not covered — and the gap is far wider than this route.
  //
  // THE DURABLE CLAIM IS THE MECHANISM; the numbers below are one tree's inventory, measured
  // against api/ as of 4c87d0f1 (line numbers belong to that SHA):
  //
  // (a) MOUNT vs BEHAVIOUR — and the answer is 24 of 24, not a subset. handler.go carries
  //     24 per-user limiter mounts across SIX limiter instances: forge 13, auth 3, chat 3,
  //     hosted 2, slackDM 2, judge 1. (A bare grep says 27; :399, :456 and :618 are
  //     comments.) Stripping ALL 24 `.With(<x>Limiter.PerUserMiddleware)` wrappers leaves
  //     `go vet` clean and `go test ./...` at zero failures across all 41 TEST-RUNNING
  //     packages (42 exist; internal/httpx has no test files), against an equally green
  //     baseline — re-run here, not inherited. Every per-user limiter mount in this codebase
  //     is unasserted.
  //
  // (b) THE AUTH CARVE-OUT THAT LOOKS LIKE COVERAGE IS A DIFFERENT METHOD. It is tempting to
  //     except authLimiter, because e2e/run-e2e.sh hammers `POST /api/auth/login` and asserts
  //     a 429. But login is mounted `authLimiter.Middleware` (handler.go:275) — the IP-keyed
  //     method. The three PerUserMiddleware auth mounts are `/cli/approve` (:300),
  //     `/vault/unlock` (:403) and `/vault/passphrase` (:406). One limiter object, two
  //     methods, DISJOINT mount sets, so that 429 closes none of the 24. handler.go:396-400
  //     says as much in prose — the per-user key space is "not the login route's key space" —
  //     which is precisely why an e2e run against login cannot speak for it.
  //     Worth naming, because it is the sharpest instance and it sits in the half that was
  //     nearly written off as covered: `/vault/unlock` and `/vault/passphrase` are the
  //     brute-force controls on a PASSPHRASE endpoint, and nothing asserts either route
  //     carries a limiter at all.
  //
  // (c) NEITHER EXISTING ASSERTION IS ON A forgeLimiter ROUTE. The only `PerUserMiddleware`
  //     assertions in the entire api test tree are chat_test.go:212 and slack_test.go:322;
  //     `/{id}/continue` rides chatLimiter (handler.go:759) and `/test-dm` rides
  //     slackDMLimiter (:467). So `forgeLimiter` — the instance guarding all 13 forge routes,
  //     including this one — has ZERO assertions of any kind. What those two prove is that
  //     the shared TYPE `mw.Limiter.PerUserMiddleware` 429s past its budget (chat_test.go:209
  //     with a budget of 1, slack_test.go:315 with 2), through two other instances. Do not
  //     read "the middleware is exercised" as "forgeLimiter is". Worse, each hand-writes its
  //     own `chi.NewRouter()` and CONSTRUCTS the very `.With(…)` it then observes working — a
  //     tautology with respect to the mount, so those two routes' own mounts are unasserted
  //     too, and both suites stayed green when they were stripped.
  //
  // (d) WHY NOTHING CATCHES IT. review_issue_livedb_test.go calls `h.FileIssue(rr, req)` as
  //     a plain function and never constructs a router (zero `Routes()` / `chi.NewRouter`
  //     hits in that file), so no middleware chain executes and it could not observe a
  //     missing `.With(…)` even in principle. And `chi.Walk` appears NOWHERE under api/, so
  //     there is no route-table snapshot test and none can be trivially derived.
  //
  // Named here because a web test that looked like it covered this would be worse than no
  // test. Out of scope to fix from web/, and a Go-side change besides.
  it("POSTs to the recommendation's issue route with the CSRF header and the session cookie", async () => {
    const real = (await vi.importActual<typeof import("../lib/api")>("../lib/api")).api;
    mockApi.fileIssue.mockImplementation(real.fileIssue);
    document.cookie = "uzi_csrf=tok-abc123";

    const fetchStub = vi.fn(async () => ({
      ok: true,
      status: 201,
      text: async () =>
        JSON.stringify({ issue: { iid: 71, web_url: "https://gitlab.example/vtmocanu/uzi/-/issues/71", title: "t" } }),
    }));
    vi.stubGlobal("fetch", fetchStub);

    await openDraft();
    fireEvent.click(screen.getByText("Create issue"));

    await waitFor(() => expect(fetchStub).toHaveBeenCalledTimes(1));
    const [url, init] = (fetchStub.mock.calls[0] ?? []) as unknown as [string, RequestInit];
    // The route is the occurrence's own (run, rec) address — the same #68 endpoint RunView
    // drives, not a judge-specific one.
    expect(url).toBe("/api/runs/run-b/review/recommendations/rec-b/issue");
    expect(init.method).toBe("POST");
    // The session travels as the HttpOnly cookie, so the request must be same-origin
    // credentialed; the readable CSRF cookie is echoed back in the header the API checks.
    expect(init.credentials).toBe("same-origin");
    expect((init.headers as Record<string, string>)["X-CSRF-Token"]).toBe("tok-abc123");
    expect(JSON.parse(init.body as string)).toEqual({
      repo_id: "repo1",
      title: draftFixture().title,
      description: draftFixture().description,
    });
    // No cleanup here on purpose — the cookie and this test's mockImplementation are both
    // undone in the file's afterEach, which runs even when an assertion above throws.
  });
});
