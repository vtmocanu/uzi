// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { convertChangesToXML, diffWords } from "diff";
import { AgentDetail } from "./AgentDetail";
import { ApiError, api, type AgentTemplate, type BuiltinDefinition, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getAgentTemplate: vi.fn(),
      getBuiltinAgentTemplate: vi.fn(),
      updateAgentTemplate: vi.fn(),
      resetAgentTemplate: vi.fn(),
      deleteAgentTemplate: vi.fn(),
      // The skills panel renders alongside the editor; stub its reads so an
      // unhandled rejection there cannot look like a failure of this page.
      listSkills: vi.fn().mockResolvedValue({ skills: [] }),
      getTemplateSkills: vi.fn().mockResolvedValue({ skills: [] }),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const mockUseAuth = vi.mocked(useAuth);

const ADMIN: User = {
  id: "u-admin",
  email: "vlad@uzi.local",
  display_name: "Vlad",
  is_admin: true,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: false,
  wait_on_limit: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

const SHIPPED: BuiltinDefinition = {
  name: "coder",
  description: "Implements features.",
  model: null,
  tools: ["Bash", "Read"],
  prompt_body: "You are the coder.\nStay in the repo you were given.\nNever touch main.\n",
};

function row(over: Partial<AgentTemplate> = {}): AgentTemplate {
  return {
    id: "t-coder",
    name: "coder",
    description: SHIPPED.description,
    model: SHIPPED.model,
    tools: SHIPPED.tools,
    prompt_body: SHIPPED.prompt_body,
    is_builtin: true,
    scope: "builtin",
    user_id: null,
    updated_by: null,
    differs_from_builtin: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-02T00:00:00Z",
    ...over,
  };
}

// The drifted row: one edited line in the prompt body plus a changed description,
// so the diff has something to show in two of the four columns.
const DRIFTED = row({
  differs_from_builtin: true,
  description: "Implements features, carefully.",
  prompt_body: "You are the coder.\nStay in the repo you were given.\nAlways run the gate.\n",
});

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/agents/t-coder"]}>
      <Routes>
        <Route path="/agents/:id" element={<AgentDetail />} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  // The confirm spy is per-test; restoring stops one test's stub deciding
  // another's Reset click.
  vi.restoreAllMocks();
});

describe("AgentDetail builtin drift signal (issue #201 M4a)", () => {
  it("badges a drifted builtin and shows a shipped-vs-stored diff", async () => {
    mockUseAuth.mockReturnValue({ user: ADMIN } as ReturnType<typeof useAuth>);
    mockApi.getAgentTemplate.mockResolvedValue({ template: DRIFTED });
    mockApi.getBuiltinAgentTemplate.mockResolvedValue({ builtin: SHIPPED });

    renderPage();

    expect(await screen.findByText("differs from shipped")).toBeTruthy();
    // The diff names which columns moved...
    expect(await screen.findByText(/description, prompt body/)).toBeTruthy();
    // ...and carries both sides as TEXT NODES. The shipped line the edit removed
    // is only obtainable from the /builtin read, so its presence proves the diff
    // is against the shipped definition and not against the row itself.
    // The selector is needed because every ancestor's textContent contains the
    // line too; matching the leaf span is what proves the line is its own node.
    expect(screen.getByText(/Never touch main\./, { selector: "span" })).toBeTruthy();
    expect(screen.getByText(/Always run the gate\./, { selector: "span" })).toBeTruthy();
  });

  it("says so, instead of diffing, when the row matches the shipped definition", async () => {
    mockUseAuth.mockReturnValue({ user: ADMIN } as ReturnType<typeof useAuth>);
    mockApi.getAgentTemplate.mockResolvedValue({ template: row() });
    mockApi.getBuiltinAgentTemplate.mockResolvedValue({ builtin: SHIPPED });

    renderPage();

    expect(await screen.findByText(/Matches the shipped definition/)).toBeTruthy();
    expect(screen.queryByText("differs from shipped")).toBeNull();
  });

  it("clears the badge from the reset RESPONSE, with no refetch", async () => {
    mockUseAuth.mockReturnValue({ user: ADMIN } as ReturnType<typeof useAuth>);
    mockApi.getAgentTemplate.mockResolvedValue({ template: DRIFTED });
    mockApi.getBuiltinAgentTemplate.mockResolvedValue({ builtin: SHIPPED });
    mockApi.resetAgentTemplate.mockResolvedValue({
      template: row({ updated_at: "2026-01-03T00:00:00Z" }),
    });

    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    renderPage();
    expect(await screen.findByText("differs from shipped")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Reset to default" }));

    // The confirmation NAMES what it will discard, rather than asking a generic
    // are-you-sure — the diff that justifies this click is several viewport
    // heights above the button and cannot be read at the moment of pressing it.
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(confirm.mock.calls[0][0]).toContain("description");
    expect(confirm.mock.calls[0][0]).toContain("prompt body");
    expect(confirm.mock.calls[0][0]).toContain("cannot be undone");

    await waitFor(() => expect(screen.queryByText("differs from shipped")).toBeNull());
    expect(await screen.findByText(/Matches the shipped definition/)).toBeTruthy();
    // The interaction an admin judges this by: the badge goes away because the
    // reset response IS the DTO, not because the page reloaded the row. It holds
    // only while reset keeps returning the template.
    expect(mockApi.getAgentTemplate).toHaveBeenCalledTimes(1);
  });

  it("renders diffed markup as TEXT, never as elements, in all four renderers", async () => {
    // A NARROWER COMPLEMENT TO criterion 7's grep, not the control behind it. The
    // grep is strictly stronger: it covers every renderer and both unsafe forms.
    // Stated the other way round — as it was — a future reader could widen the
    // grep's exceptions believing this test still covered them.
    //
    // What it catches, precisely, because the two unsafe forms are NOT alike:
    //
    //   - unescaped concatenation (`__html: `<span>${p.value}</span>``) creates a
    //     real <img>, caught by the querySelector below. This is the dangerous
    //     form.
    //   - jsdiff's own convertChangesToXML does NOT: it escapes the value
    //     (dist/diff.js:2261) before wrapping it, so the payload never becomes an
    //     element and the img assertion passes against it. It gives itself away by
    //     OUTPUT SHAPE instead — it emits <ins>/<del> wrappers UNESCAPED, which is
    //     what the second assertion reads and what escaping cannot hide.
    //
    // The fixture differs in all FOUR columns, with markup in the description and
    // in a tool name, so InlineDiff (diffWords), the model span, ToolsDiff
    // (diffArrays) and LineDiff all mount. With only prompt_body drifted, one
    // renderer of four was covered — while description is admin-editable on
    // exactly the same footing.
    const markup = '<img src=x onerror="alert(1)">';
    const toolMarkup = "<b>Bash</b>";
    mockUseAuth.mockReturnValue({ user: ADMIN } as ReturnType<typeof useAuth>);
    mockApi.getAgentTemplate.mockResolvedValue({
      template: row({
        differs_from_builtin: true,
        description: `Implements features. ${markup}`,
        model: "sonnet", // SHIPPED inherits (null), so the model span mounts too
        tools: [toolMarkup, "Read"],
        prompt_body: `You are the coder.\n${markup}\n`,
      }),
    });
    mockApi.getBuiltinAgentTemplate.mockResolvedValue({ builtin: SHIPPED });

    const { container } = renderPage();

    expect(await screen.findByText("differs from shipped")).toBeTruthy();
    // All four renderers are actually mounted — without this the assertions below
    // could pass over a panel that rendered nothing.
    expect(screen.getByText(/description, model, tools, prompt body/)).toBeTruthy();

    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("b")).toBeNull();
    expect(container.querySelectorAll("ins, del")).toHaveLength(0);
    // ...and the payload is present as text rather than dropped, which is what
    // separates a text-node render from one that merely swallowed the content.
    expect(screen.getAllByText(new RegExp("onerror"), { selector: "span" }).length).toBeGreaterThan(0);
    expect(container.textContent).toContain(toolMarkup);
  });

  it("does not offer Reset for a builtin this release no longer ships", async () => {
    mockUseAuth.mockReturnValue({ user: ADMIN } as ReturnType<typeof useAuth>);
    mockApi.getAgentTemplate.mockResolvedValue({
      template: row({ id: "t-retired", name: "retired-role", differs_from_builtin: false }),
    });
    // The 409 the server answers for a builtin with no shipped definition — the
    // same state Reset itself would answer 409 to.
    mockApi.getBuiltinAgentTemplate.mockRejectedValue(
      new ApiError(409, "no builtin definition to reset to"),
    );

    renderPage();

    expect(await screen.findByText(/no longer ships a definition/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Reset to default" })).toBeNull();
    // The 409 above must be LOAD-BEARING, not decorative. It was not: while the
    // catch took no parameter this same test passed on a 500, a 502 or a bare
    // Error, because every rejection produced the identical null. The 500 case
    // below is what makes flipping this status redden.
    // ...and the page still renders normally: the failed shipped-definition read
    // is not an error about the template, which loaded fine.
    expect(screen.queryByText("differs from shipped")).toBeNull();
    expect(screen.getByRole("button", { name: "Save changes" })).toBeTruthy();
  });

  it("does not draw a phantom changed line for a trailing-newline mismatch", async () => {
    // diffLines keeps the newline inside its token, so a body ending "…main." and
    // one ending "…main.\n" came back as one removed and one added line of
    // BYTE-IDENTICAL text — one green, one red, nothing saying why. Reachable on
    // real data: every builtin .md ends with a newline and a textarea adds none.
    mockUseAuth.mockReturnValue({ user: ADMIN } as ReturnType<typeof useAuth>);
    mockApi.getAgentTemplate.mockResolvedValue({
      template: row({
        differs_from_builtin: true,
        prompt_body: SHIPPED.prompt_body.replace(/\n$/, ""),
      }),
    });
    mockApi.getBuiltinAgentTemplate.mockResolvedValue({ builtin: SHIPPED });

    const { container } = renderPage();

    // The column is still NAMED — nothing upstream trims, so the row really does
    // differ and really does badge. Only the RENDERER stops drawing a diff it
    // cannot explain, and it says which case this is instead of showing nothing.
    expect(await screen.findByText(/Identical except for trailing whitespace/)).toBeTruthy();
    expect(screen.getByText(/prompt body/)).toBeTruthy();
    // No line is shown as both removed and added.
    const rows = Array.from(container.querySelectorAll("span.block"));
    const added = rows.filter((r) => r.className.includes("text-danger")).map((r) => r.textContent);
    const removed = rows.filter((r) => r.className.includes("text-ok")).map((r) => r.textContent);
    expect(added.filter((a) => removed.some((r) => r?.slice(1) === a?.slice(1)))).toHaveLength(0);
  });

  it("does NOT reset when the confirmation is dismissed", async () => {
    // The discriminating half: without this, a confirm that always returned true
    // — or one wired to nothing at all — would pass the test above unchanged.
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    mockUseAuth.mockReturnValue({ user: ADMIN } as ReturnType<typeof useAuth>);
    mockApi.getAgentTemplate.mockResolvedValue({ template: DRIFTED });
    mockApi.getBuiltinAgentTemplate.mockResolvedValue({ builtin: SHIPPED });

    renderPage();
    expect(await screen.findByText("differs from shipped")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Reset to default" }));

    expect(confirm).toHaveBeenCalledTimes(1);
    expect(mockApi.resetAgentTemplate).not.toHaveBeenCalled();
    expect(screen.getByText("differs from shipped")).toBeTruthy();
  });

  it("names only the columns that actually differ", async () => {
    // A confirmation that always listed all four would be as uninformative as a
    // generic one, and would misstate what is at stake.
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    mockUseAuth.mockReturnValue({ user: ADMIN } as ReturnType<typeof useAuth>);
    mockApi.getAgentTemplate.mockResolvedValue({
      template: row({ differs_from_builtin: true, model: "haiku" }),
    });
    mockApi.getBuiltinAgentTemplate.mockResolvedValue({ builtin: SHIPPED });

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Reset to default" }));

    const message = confirm.mock.calls[0][0] as string;
    expect(message).toContain("model");
    expect(message).not.toContain("prompt body");
    expect(message).not.toContain("description");
    expect(message).not.toContain("tools");
  });

  it("the ins/del assertion can actually fire — control on the instrument, not the component", () => {
    // AN ASSERTION THAT CANNOT PRODUCE THE DISCONFIRMING ANSWER IS NOT EVIDENCE,
    // and the img assertion above is exactly that against one of the two unsafe
    // forms. This builds what convertChangesToXML would put into the DOM and shows
    // the two assertions disagreeing: no <img> (it escaped the payload), but
    // <ins>/<del> present (it does NOT escape its own wrappers).
    //
    // Written as a DOM control rather than by mutating AgentTemplateEditor on
    // purpose. The real mutation would place a genuine dangerouslySetInnerHTML in
    // web/src, and criterion 7 is a repo-wide grep that other agents run — a
    // transient call site would corrupt someone else's measurement, silently.
    const markup = '<img src=x onerror="alert(1)">';
    const unsafeHTML = convertChangesToXML(diffWords("You are the coder.", `You are ${markup}`));

    const el = document.createElement("div");
    el.innerHTML = unsafeHTML;

    expect(el.querySelector("img")).toBeNull(); // escaping hides the payload...
    expect(el.querySelectorAll("ins, del").length).toBeGreaterThan(0); // ...but not the shape
  });

  it("KEEPS Reset when the shipped-definition read merely FAILS, and claims nothing", async () => {
    // The discriminating half of the pair above, and the reason the 409 there is
    // not decorative. A transient failure says nothing about whether the
    // definition exists — so the page must not print the no-longer-ships
    // sentence, and must not withdraw the button. Before the status was bound,
    // this exact fixture produced the same screen as the 409.
    mockUseAuth.mockReturnValue({ user: ADMIN } as ReturnType<typeof useAuth>);
    mockApi.getAgentTemplate.mockResolvedValue({ template: DRIFTED });
    mockApi.getBuiltinAgentTemplate.mockRejectedValue(new ApiError(500, "internal error"));

    renderPage();

    // Reset survives: on origin/main it was offered for every builtin row, so
    // losing it to a 500 would be a regression in reach this milestone introduced.
    expect(await screen.findByRole("button", { name: "Reset to default" })).toBeTruthy();
    expect(screen.queryByText(/no longer ships a definition/)).toBeNull();
    expect(screen.getByText(/could not be loaded/)).toBeTruthy();
    // The row's own drift verdict came from the template read and is unaffected.
    expect(screen.getByText("differs from shipped")).toBeTruthy();
  });

  it("never asks for the shipped definition as a non-admin — the request is a guaranteed 403", async () => {
    // canEdit for a builtin row is exactly isAdmin, mirroring the server's
    // authorizeTemplateWrite, so a non-admin's request could only ever be refused.
    // Firing it anyway costs a round-trip and buries a real 403 in routine ones.
    mockUseAuth.mockReturnValue({
      user: { ...ADMIN, id: "u-mira", is_admin: false },
    } as ReturnType<typeof useAuth>);
    mockApi.getAgentTemplate.mockResolvedValue({ template: DRIFTED });

    renderPage();

    expect(await screen.findByText("differs from shipped")).toBeTruthy();
    expect(mockApi.getBuiltinAgentTemplate).not.toHaveBeenCalled();
  });
});
