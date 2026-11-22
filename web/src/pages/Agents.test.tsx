// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { EffectiveSettings } from "../lib/apiTypes";
import { MemoryRouter } from "react-router-dom";
import { Agents } from "./Agents";
import { api, type AgentTemplate, type TemplateAllocation, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listAgentTemplates: vi.fn(),
      getTemplateAllocations: vi.fn(),
      setTemplateAllocations: vi.fn(),
      getEffectiveSettings: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const mockUseAuth = vi.mocked(useAuth);

const MEMBER: User = {
  id: "u-mira",
  email: "mira@uzi.local",
  display_name: "Alex",
  is_admin: false,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: false,
  ci_autofix_enabled: false,
  attribution_enabled: true,
  ephemeral_workers_enabled: false,
  ephemeral_docker_enabled: false,
  wait_on_limit: false,
  notify_early_limit_reset: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  judge_anthropic_bind_mode: "default",
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function tmpl(over: Partial<AgentTemplate> & Pick<AgentTemplate, "id" | "name" | "scope">): AgentTemplate {
  return {
    description: `desc ${over.name}`,
    model: null,
    codex_model: null,
    tools: null,
    prompt_body: "body",
    is_builtin: over.scope === "builtin",
    user_id: over.scope === "user" ? MEMBER.id : null,
    updated_by: null,
    differs_from_builtin: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function alloc(over: Partial<TemplateAllocation> & Pick<TemplateAllocation, "id" | "name" | "scope">): TemplateAllocation {
  return {
    description: `desc ${over.name}`,
    is_builtin: over.scope === "builtin",
    global_default: over.scope !== "user",
    my_override: null,
    effective: over.scope !== "user",
    ...over,
  };
}

beforeEach(() => {
  // mockReset drops queued Once implementations a prior test left behind.
  mockApi.getEffectiveSettings.mockReset();
  mockApi.getEffectiveSettings.mockResolvedValue({ version: 1, harnesses: [], start: [], models: [], efforts: [] } as unknown as EffectiveSettings);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Agents page always-visible agent-templates guide link (PRD #57 M3)", () => {
  it("renders the agent templates guide link in the page intro", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    mockApi.listAgentTemplates.mockResolvedValue({ templates: [] });
    mockApi.getTemplateAllocations.mockResolvedValue({ templates: [] });

    render(
      <MemoryRouter>
        <Agents />
      </MemoryRouter>,
    );

    const docLink = await screen.findByRole("link", { name: "agent templates" });
    expect(docLink.getAttribute("href")).toBe("/docs/agent-templates");
  });
});

describe("Agents page unset model label (#3255)", () => {
  it("shows what an unset model resolves to for the lead and for a subagent", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    mockApi.listAgentTemplates.mockResolvedValue({
      templates: [
        tmpl({ id: "b-lead", name: "lead", scope: "builtin" }),
        tmpl({ id: "b-coder", name: "coder", scope: "builtin" }),
      ],
    });
    mockApi.getTemplateAllocations.mockResolvedValue({
      templates: [
        alloc({ id: "b-lead", name: "lead", scope: "builtin" }),
        alloc({ id: "b-coder", name: "coder", scope: "builtin" }),
      ],
    });
    render(<MemoryRouter><Agents /></MemoryRouter>);
    const leadRow = (await screen.findByRole("link", { name: "lead" })).closest("tr")!;
    // An all-unset lead is never collapsed: both families show their defaults.
    const leadClaude = within(leadRow).getByLabelText("Claude model and effort");
    expect(within(leadClaude).getByText("Claude: Default · SDK/account default (built-in)")).toBeTruthy();
    expect(within(leadClaude).getByText("effort: Default · built-in (medium)")).toBeTruthy();
    const leadCodex = within(leadRow).getByLabelText("Codex model and effort");
    expect(within(leadCodex).getByText("Codex: Default · built-in (gpt-6.1-sol)")).toBeTruthy();
    expect(within(leadCodex).getByText("effort: Default · built-in (medium)")).toBeTruthy();
    expect(within(leadRow).queryByText("both harnesses")).toBeNull();
    const coderRow = screen.getByRole("link", { name: "coder" }).closest("tr")!;
    const collapsed = within(coderRow).getByRole("group", { name: "Claude and Codex model and effort" });
    expect(within(collapsed).getByText("Default · follows lead")).toBeTruthy();
    expect(within(collapsed).getByText("effort: Default · follows the run's effort")).toBeTruthy();
    expect(within(collapsed).getByText("both harnesses")).toBeTruthy();
    expect(screen.queryByText("inherit")).toBeNull();
  });
});

describe("Agents page per-harness pins", () => {
  it("renders the Model & effort header", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    mockApi.listAgentTemplates.mockResolvedValue({ templates: [] });
    mockApi.getTemplateAllocations.mockResolvedValue({ templates: [] });
    render(<MemoryRouter><Agents /></MemoryRouter>);
    expect(await screen.findByRole("columnheader", { name: "Model & effort" })).toBeTruthy();
  });

  it.each([
    {
      pins: { model: "sonnet", codex_model: "gpt-6-sol", effort: "high", codex_effort: "low" },
      claude: ["Claude: sonnet", "effort: high"],
      codex: ["Codex: gpt-6-sol", "effort: low"],
    },
    {
      pins: { model: null, codex_model: "gpt-6-luna", effort: null, codex_effort: "high" },
      claude: ["Claude: Default · follows lead", "effort: Default · follows the run's effort"],
      codex: ["Codex: gpt-6-luna", "effort: high"],
    },
  ])("shows stored pins in both families: $claude / $codex", async ({ pins, claude, codex }) => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    mockApi.listAgentTemplates.mockResolvedValue({ templates: [tmpl({ id: "b-coder", name: "coder", scope: "builtin", ...pins })] });
    mockApi.getTemplateAllocations.mockResolvedValue({ templates: [] });
    render(<MemoryRouter><Agents /></MemoryRouter>);
    const lines = (el: HTMLElement) => [...el.children].map((c) => c.textContent);
    expect(lines(await screen.findByLabelText("Claude model and effort"))).toEqual(claude);
    expect(lines(screen.getByLabelText("Codex model and effort"))).toEqual(codex);
    expect(screen.queryByText("both harnesses")).toBeNull();
  });
});

type Cell = { model: string; msrc: string; effort: string | null; esrc: string; defaultModel?: string; catalog?: object };
function eff(claude: Cell | null, codex: Cell | null): EffectiveSettings {
  const cells = [["claude", claude], ["codex", codex]] as const;
  return {
    version: 1,
    harnesses: cells.map(([harness, c]) => ({ harness, usable: c !== null, refusal: null, refusal_detail: null, credential: null })),
    models: cells.flatMap(([harness, c]) =>
      c
        ? [{ harness, model: null, default_model: c.defaultModel ?? "", default_source: "builtin", resolved_model: c.model, source: c.msrc, setting: "worker_model", fixed_at: "claim", ignored: null, notes: [] }]
        : [],
    ),
    efforts: cells.flatMap(([harness, c]) =>
      c
        ? [{
            harness, effort: null, default_effort: "", default_source: "builtin", resolved_effort: c.effort, source: c.esrc,
            mode: c.effort === null ? "runtime_default" : "explicit", setting: "worker_effort", fixed_at: "claim", notes: [],
            ...(c.catalog ? { catalog_default: c.catalog } : {}),
          }]
        : [],
    ),
  } as unknown as EffectiveSettings;
}

const LEAD_TMPLS = [
  tmpl({ id: "b-lead", name: "lead", scope: "builtin" }),
  tmpl({ id: "b-coder", name: "coder", scope: "builtin" }),
];
const LEAD_ALLOCS = [
  alloc({ id: "b-lead", name: "lead", scope: "builtin" }),
  alloc({ id: "b-coder", name: "coder", scope: "builtin" }),
];
const CLAUDE_A = { model: "opus-a", msrc: "user_default", effort: "high", esrc: "user_default" };
const CODEX_A = { model: "gpt-a", msrc: "builtin", effort: null, esrc: "builtin" };
const LINE_CLAUDE_A = "For your Claude runs: opus-a · your choice; effort high · your choice";
const LINE_CODEX_A =
  "For your Codex runs: gpt-a · built-in; effort Runtime default · catalog default unknown until a Codex run reports it · built-in";

function seedLead(templates = LEAD_TMPLS, allocs = LEAD_ALLOCS) {
  mockApi.listAgentTemplates.mockResolvedValue({ templates });
  mockApi.getTemplateAllocations.mockResolvedValue({ templates: allocs });
}
function renderPage() {
  return render(<MemoryRouter><Agents /></MemoryRouter>);
}
function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe("Agents page personalized lead line (#3262)", () => {
  it("shows the caller's worker model/effort and sources on the single enabled lead row only", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    seedLead();
    mockApi.getEffectiveSettings.mockResolvedValue(eff(CLAUDE_A, CODEX_A));
    renderPage();
    const leadRow = (await screen.findByRole("link", { name: "lead" })).closest("tr")!;
    expect(await within(leadRow).findByText(LINE_CLAUDE_A)).toBeTruthy();
    expect(within(leadRow).getByText(LINE_CODEX_A)).toBeTruthy();
    const coderRow = screen.getByRole("link", { name: "coder" }).closest("tr")!;
    expect(within(coderRow).queryByText(/For your/)).toBeNull();
    expect(screen.queryByTestId("personal-lead-note")).toBeNull();
  });

  it("shows only usable harnesses and the sdk_default note for an empty model", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    seedLead();
    mockApi.getEffectiveSettings.mockResolvedValue(eff({ ...CLAUDE_A, model: "" }, null));
    renderPage();
    expect(await screen.findByText("For your Claude runs: the SDK decides · your choice; effort high · your choice")).toBeTruthy();
    expect(screen.queryByText(/For your Codex runs/)).toBeNull();
  });

  it("shows the Codex catalog default for a runtime-default effort", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    seedLead();
    const catalog = { state: "known", effort: "medium", model: "gpt-6.1-sol", runtime_version: "0.164.0" };
    mockApi.getEffectiveSettings.mockResolvedValue(eff(CLAUDE_A, { ...CODEX_A, catalog }));
    renderPage();
    expect(await screen.findByText(
      "For your Codex runs: gpt-a · built-in; effort Runtime default · catalog default: medium (gpt-6.1-sol, Codex 0.164.0) · built-in",
    )).toBeTruthy();
  });

  it("shows the sdk_default note for an empty model on both harnesses, with or without a Codex default model", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    seedLead();
    mockApi.getEffectiveSettings.mockResolvedValue(eff({ ...CLAUDE_A, model: "" }, { ...CODEX_A, model: "", defaultModel: "gpt-d" }));
    const first = renderPage();
    expect(await screen.findByText(/^For your Codex runs: the SDK decides · built-in;/)).toBeTruthy();
    expect(screen.getByText(/^For your Claude runs: the SDK decides · your choice;/)).toBeTruthy();
    first.unmount();
    mockApi.getEffectiveSettings.mockResolvedValue(eff(CLAUDE_A, { ...CODEX_A, model: "" }));
    renderPage();
    expect(await screen.findByText(/^For your Codex runs: the SDK decides · built-in;/)).toBeTruthy();
    expect(screen.queryByText(/For your Codex runs: (gpt-d|built-in ·)/)).toBeNull();
    expect(screen.queryByText(/For your Claude runs: SDK\/account default/)).toBeNull();
  });

  it.each([
    ["rejected", () => mockApi.getEffectiveSettings.mockRejectedValue(new Error("nope"))],
    ["undefined", () => mockApi.getEffectiveSettings.mockResolvedValue(undefined as never)],
    ["models not an array", () => mockApi.getEffectiveSettings.mockResolvedValue({ models: {}, efforts: [], harnesses: [{ harness: "claude", usable: true }] } as never)],
    ["efforts not an array", () => mockApi.getEffectiveSettings.mockResolvedValue({ models: [], efforts: {}, harnesses: [{ harness: "claude", usable: true }] } as never)],
    ["harnesses not an array", () => mockApi.getEffectiveSettings.mockResolvedValue({ models: [], efforts: [], harnesses: {} } as never)],
  ])("shows no line when the response is %s, and the list still renders", async (_n, arrange) => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    seedLead();
    arrange();
    renderPage();
    await screen.findByRole("link", { name: "lead" });
    await waitFor(() => expect(mockApi.getEffectiveSettings).toHaveBeenCalled());
    expect(screen.queryByText(/For your/)).toBeNull();
  });

  it("puts a labelled note above the table when the lead row is shadowed", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    seedLead(
      [
        tmpl({ id: "b-lead", name: "lead", scope: "builtin" }),
        tmpl({ id: "u-lead", name: "lead", scope: "user" }),
      ],
      [
        alloc({ id: "b-lead", name: "lead", scope: "builtin", effective: false, global_default: false }),
        alloc({ id: "u-lead", name: "lead", scope: "user", effective: true, my_override: true, global_default: false }),
      ],
    );
    mockApi.getEffectiveSettings.mockResolvedValue(eff(CLAUDE_A, null));
    renderPage();
    const note = await screen.findByTestId("personal-lead-note");
    expect(within(note).getByText("For your lead runs:")).toBeTruthy();
    expect(within(note).getByText("Claude: opus-a · your choice; effort high · your choice")).toBeTruthy();
    expect(screen.queryByTestId("personal-lead-line")).toBeNull();
    expect(note.compareDocumentPosition(screen.getByRole("table")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("leaves the stored-pin cells identical for two viewers with different settings", async () => {
    seedLead();
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    mockApi.getEffectiveSettings.mockResolvedValue(eff(CLAUDE_A, CODEX_A));
    const first = renderPage();
    await screen.findByText(LINE_CLAUDE_A);
    const cellsA = screen.getAllByLabelText(/model and effort/).map((e) => e.textContent);
    first.unmount();
    mockUseAuth.mockReturnValue({ user: { ...MEMBER, id: "u-other" } } as ReturnType<typeof useAuth>);
    mockApi.getEffectiveSettings.mockResolvedValue(eff({ model: "opus-b", msrc: "template", effort: "low", esrc: "template" }, null));
    renderPage();
    await screen.findByText("For your Claude runs: opus-b · lead template; effort low · lead template");
    expect(screen.getAllByLabelText(/model and effort/).map((e) => e.textContent)).toEqual(cellsA);
  });

  describe("refetch after allocation changes", () => {
    it("refetches after a successful My runs toggle and hides the line until it resolves", async () => {
      mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
      seedLead();
      const second = deferred<EffectiveSettings>();
      mockApi.getEffectiveSettings
        .mockResolvedValueOnce(eff(CLAUDE_A, null))
        .mockImplementationOnce(() => second.promise);
      mockApi.setTemplateAllocations.mockResolvedValue({ templates: LEAD_ALLOCS.map((a) => (a.id === "b-coder" ? { ...a, effective: false } : a)) });

      renderPage();
      await screen.findByText(LINE_CLAUDE_A);
      await act(async () => { fireEvent.click(screen.getByLabelText("Include coder in my runs")); });
      await waitFor(() => expect(mockApi.getEffectiveSettings).toHaveBeenCalledTimes(2));
      // Hidden between the commit of the toggle and the refetch resolving.
      expect(screen.queryByText(/For your/)).toBeNull();
      await act(async () => {
        second.resolve(eff({ ...CLAUDE_A, model: "opus-new" }, null));
      });
      expect(await screen.findByText("For your Claude runs: opus-new · your choice; effort high · your choice")).toBeTruthy();
    });

    it("refetches after an admin Global default toggle", async () => {
      mockUseAuth.mockReturnValue({ user: { ...MEMBER, is_admin: true } } as ReturnType<typeof useAuth>);
      seedLead();
      mockApi.getEffectiveSettings.mockResolvedValue(eff(CLAUDE_A, null));
      mockApi.setTemplateAllocations.mockResolvedValue({ templates: LEAD_ALLOCS });

      renderPage();
      await screen.findByText(LINE_CLAUDE_A);
      const second = deferred<EffectiveSettings>();
      mockApi.getEffectiveSettings.mockImplementationOnce(() => second.promise);
      await act(async () => { fireEvent.click(screen.getByLabelText("Make coder a global default")); });
      await waitFor(() => expect(mockApi.getEffectiveSettings).toHaveBeenCalledTimes(2));
      expect(screen.queryByText(/For your/)).toBeNull();
      await act(async () => {
        second.resolve(eff({ ...CLAUDE_A, model: "opus-new" }, null));
      });
      expect(await screen.findByText("For your Claude runs: opus-new · your choice; effort high · your choice")).toBeTruthy();
    });

    it("does not refetch after a failed PUT and keeps the line", async () => {
      mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
      seedLead();
      mockApi.getEffectiveSettings.mockResolvedValue(eff(CLAUDE_A, null));
      mockApi.setTemplateAllocations.mockRejectedValue(new Error("put failed"));

      renderPage();
      await screen.findByText(LINE_CLAUDE_A);
      await act(async () => { fireEvent.click(screen.getByLabelText("Include coder in my runs")); });
      await screen.findByText(/put failed|Failed to update allocation/);
      expect(mockApi.getEffectiveSettings).toHaveBeenCalledTimes(1);
      expect(screen.getByText(LINE_CLAUDE_A)).toBeTruthy();
    });

    it("leaves no line when the refetch fails", async () => {
      mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
      seedLead();
      mockApi.getEffectiveSettings
        .mockResolvedValueOnce(eff(CLAUDE_A, null))
        .mockRejectedValueOnce(new Error("refetch failed"));
      mockApi.setTemplateAllocations.mockResolvedValue({ templates: LEAD_ALLOCS });

      renderPage();
      await screen.findByText(LINE_CLAUDE_A);
      await act(async () => { fireEvent.click(screen.getByLabelText("Include coder in my runs")); });
      await waitFor(() => expect(mockApi.getEffectiveSettings).toHaveBeenCalledTimes(2));
      await act(async () => {});
      expect(screen.queryByText(/For your/)).toBeNull();
    });

    it("ignores a late first response once a toggle moved the lead and shows the new lead's line", async () => {
      mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
      const two = [
        tmpl({ id: "b-lead", name: "lead", scope: "builtin" }),
        tmpl({ id: "g-orch", name: "orchestrator", scope: "global" }),
      ];
      seedLead(two, [
        alloc({ id: "b-lead", name: "lead", scope: "builtin" }),
        alloc({ id: "g-orch", name: "orchestrator", scope: "global", global_default: false, effective: false }),
      ]);
      const first = deferred<EffectiveSettings>();
      const second = deferred<EffectiveSettings>();
      mockApi.getEffectiveSettings
        .mockImplementationOnce(() => first.promise)
        .mockImplementationOnce(() => second.promise);
      mockApi.setTemplateAllocations.mockResolvedValue({
        templates: [
          alloc({ id: "b-lead", name: "lead", scope: "builtin", effective: false, global_default: false, my_override: false }),
          alloc({ id: "g-orch", name: "orchestrator", scope: "global", effective: true, my_override: true, global_default: false }),
        ],
      });

      renderPage();
      await screen.findByRole("link", { name: "orchestrator" });
      await act(async () => { fireEvent.click(screen.getByLabelText("Include orchestrator in my runs")); });
      await waitFor(() => expect(mockApi.getEffectiveSettings).toHaveBeenCalledTimes(2));
      await act(async () => {
        second.resolve(eff({ ...CLAUDE_A, model: "opus-new" }, null));
      });
      await act(async () => {
        first.resolve(eff({ ...CLAUDE_A, model: "opus-old" }, null));
      });
      expect(screen.queryByText(/opus-old/)).toBeNull();
      const orchRow = screen.getByRole("link", { name: "orchestrator" }).closest("tr")!;
      expect(within(orchRow).getByText("For your Claude runs: opus-new · your choice; effort high · your choice")).toBeTruthy();
      const leadRow = screen.getByRole("link", { name: "lead" }).closest("tr")!;
      expect(within(leadRow).queryByText(/For your/)).toBeNull();
    });
  });

  it("hides the previous viewer's line while the new viewer's fetch is pending", async () => {
    seedLead();
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    const pending = deferred<EffectiveSettings>();
    mockApi.getEffectiveSettings
      .mockResolvedValueOnce(eff(CLAUDE_A, null))
      .mockImplementationOnce(() => pending.promise);
    const view = renderPage();
    await screen.findByText(LINE_CLAUDE_A);
    mockUseAuth.mockReturnValue({ user: { ...MEMBER, id: "u-other" } } as ReturnType<typeof useAuth>);
    view.rerender(<MemoryRouter><Agents /></MemoryRouter>);
    await waitFor(() => expect(mockApi.getEffectiveSettings).toHaveBeenCalledTimes(2));
    expect(screen.queryByText(/For your/)).toBeNull();
    expect(screen.queryByText(/opus-a/)).toBeNull();
  });

  it("places a new viewer's line by that viewer's allocations, never the previous viewer's", async () => {
    const tmpls = [
      tmpl({ id: "b-lead", name: "lead", scope: "builtin" }),
      tmpl({ id: "g-orch", name: "orchestrator", scope: "global" }),
    ];
    const allocsA = [
      alloc({ id: "b-lead", name: "lead", scope: "builtin" }),
      alloc({ id: "g-orch", name: "orchestrator", scope: "global", global_default: false, effective: false }),
    ];
    const allocsB = [
      alloc({ id: "b-lead", name: "lead", scope: "builtin", effective: false, my_override: false }),
      alloc({ id: "g-orch", name: "orchestrator", scope: "global", global_default: false, effective: true, my_override: true }),
    ];
    mockApi.listAgentTemplates.mockResolvedValue({ templates: tmpls });
    const allocB = deferred<{ templates: TemplateAllocation[] }>();
    mockApi.getTemplateAllocations
      .mockResolvedValueOnce({ templates: allocsA })
      .mockImplementationOnce(() => allocB.promise);
    mockApi.getEffectiveSettings
      .mockResolvedValueOnce(eff(CLAUDE_A, null))
      .mockResolvedValueOnce(eff({ ...CLAUDE_A, model: "opus-b" }, null));
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    const view = renderPage();
    await screen.findByText(LINE_CLAUDE_A);
    const leadRowA = screen.getByRole("link", { name: "lead" }).closest("tr")!;
    expect(within(leadRowA).getByText(LINE_CLAUDE_A)).toBeTruthy();

    mockUseAuth.mockReturnValue({ user: { ...MEMBER, id: "u-other" } } as ReturnType<typeof useAuth>);
    view.rerender(<MemoryRouter><Agents /></MemoryRouter>);
    await waitFor(() => expect(mockApi.getEffectiveSettings).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(mockApi.getTemplateAllocations).toHaveBeenCalledTimes(2));
    await act(async () => {});
    // B's prediction has landed but B's allocations are still pending.
    expect(screen.queryByText(/For your/)).toBeNull();
    // Toggles are disabled until B's own allocations load, so A's rows can never be written as B's.
    expect((screen.getByLabelText("Include lead in my runs") as HTMLInputElement).disabled).toBe(true);

    await act(async () => {
      allocB.resolve({ templates: allocsB });
    });
    const orchRow = (await screen.findByRole("link", { name: "orchestrator" })).closest("tr")!;
    expect(within(orchRow).getByText("For your Claude runs: opus-b · your choice; effort high · your choice")).toBeTruthy();
    const leadRowB = screen.getByRole("link", { name: "lead" }).closest("tr")!;
    expect(within(leadRowB).queryByText(/For your/)).toBeNull();
    expect((screen.getByLabelText("Include lead in my runs") as HTMLInputElement).disabled).toBe(false);
  });

  it("ignores an allocation PUT that resolves after the viewer changed", async () => {
    const tmpls = [
      tmpl({ id: "b-lead", name: "lead", scope: "builtin" }),
      tmpl({ id: "b-coder", name: "coder", scope: "builtin" }),
    ];
    const allocsA = [
      alloc({ id: "b-lead", name: "lead", scope: "builtin" }),
      alloc({ id: "b-coder", name: "coder", scope: "builtin" }),
    ];
    const allocsB = [
      alloc({ id: "b-lead", name: "lead", scope: "builtin", effective: false, my_override: false }),
      alloc({ id: "b-coder", name: "coder", scope: "builtin", effective: true, my_override: true }),
    ];
    mockApi.listAgentTemplates.mockResolvedValue({ templates: tmpls });
    mockApi.getTemplateAllocations
      .mockResolvedValueOnce({ templates: allocsA })
      .mockResolvedValueOnce({ templates: allocsB });
    const putA = deferred<{ templates: TemplateAllocation[] }>();
    mockApi.setTemplateAllocations.mockImplementationOnce(() => putA.promise);
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    const view = renderPage();
    await screen.findByRole("link", { name: "lead" });
    await act(async () => { fireEvent.click(screen.getByLabelText("Include coder in my runs")); });
    expect(mockApi.setTemplateAllocations).toHaveBeenCalledTimes(1);

    mockUseAuth.mockReturnValue({ user: { ...MEMBER, id: "u-other" } } as ReturnType<typeof useAuth>);
    view.rerender(<MemoryRouter><Agents /></MemoryRouter>);
    await waitFor(() => expect(mockApi.getTemplateAllocations).toHaveBeenCalledTimes(2));
    await waitFor(() => expect((screen.getByLabelText("Include coder in my runs") as HTMLInputElement).checked).toBe(true));

    // A's PUT lands late with A's rows (coder switched off).
    await act(async () => {
      putA.resolve({ templates: allocsA.map((a) => (a.id === "b-coder" ? { ...a, effective: false, my_override: false } : a)) });
    });
    expect((screen.getByLabelText("Include coder in my runs") as HTMLInputElement).checked).toBe(true);
    expect((screen.getByLabelText("Include lead in my runs") as HTMLInputElement).checked).toBe(false);

    // B's next toggle sends B's set, not A's.
    mockApi.setTemplateAllocations.mockResolvedValueOnce({ templates: allocsB });
    await act(async () => { fireEvent.click(screen.getByLabelText("Include lead in my runs")); });
    expect(mockApi.setTemplateAllocations).toHaveBeenLastCalledWith({
      my_overrides: [
        { template_id: "b-lead", enabled: true },
        { template_id: "b-coder", enabled: true },
      ],
    });
  });

  it("shows no personal line or note when the list fails to load", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    mockApi.listAgentTemplates.mockRejectedValue(new Error("list failed"));
    mockApi.getTemplateAllocations.mockResolvedValue({ templates: [] });
    mockApi.getEffectiveSettings.mockResolvedValue(eff(CLAUDE_A, CODEX_A));
    renderPage();
    await screen.findByText(/list failed|Failed to load templates/);
    await waitFor(() => expect(mockApi.getEffectiveSettings).toHaveBeenCalled());
    await act(async () => {});
    expect(screen.queryByText(/For your/)).toBeNull();
    expect(screen.queryByTestId("personal-lead-note")).toBeNull();
    expect(screen.queryByTestId("personal-lead-line")).toBeNull();
  });

  it("drops a late response for the previous viewer", async () => {
    seedLead();
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    const old = deferred<EffectiveSettings>();
    mockApi.getEffectiveSettings
      .mockImplementationOnce(() => old.promise)
      .mockResolvedValueOnce(eff({ ...CLAUDE_A, model: "opus-new-user" }, null));
    const view = renderPage();
    await screen.findByRole("link", { name: "lead" });
    mockUseAuth.mockReturnValue({ user: { ...MEMBER, id: "u-other" } } as ReturnType<typeof useAuth>);
    view.rerender(<MemoryRouter><Agents /></MemoryRouter>);
    await screen.findByText("For your Claude runs: opus-new-user · your choice; effort high · your choice");
    await act(async () => {
      old.resolve(eff({ ...CLAUDE_A, model: "opus-old-user" }, null));
    });
    expect(screen.queryByText(/opus-old-user/)).toBeNull();
  });
});

describe("Agents page shadowed hint (PRD #18 M7/M8)", () => {
  it("badges a user template whose name collides with a builtin, not a unique one", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    const templates = [
      tmpl({ id: "b-coder", name: "coder", scope: "builtin" }),
      tmpl({ id: "u-coder", name: "coder", scope: "user" }),
      tmpl({ id: "u-helper", name: "unique-helper", scope: "user" }),
    ];
    mockApi.listAgentTemplates.mockResolvedValue({ templates });
    mockApi.getTemplateAllocations.mockResolvedValue({
      templates: [
        alloc({ id: "b-coder", name: "coder", scope: "builtin" }),
        alloc({ id: "u-coder", name: "coder", scope: "user", my_override: true, effective: true, global_default: false }),
        alloc({ id: "u-helper", name: "unique-helper", scope: "user", my_override: true, effective: true, global_default: false }),
      ],
    });

    render(
      <MemoryRouter>
        <Agents />
      </MemoryRouter>,
    );

    // The user-scope "coder" row carries the shadowed badge; the builtin "coder"
    // and the uniquely-named user template do not.
    const userCoderRow = await waitFor(() => {
      const link = screen.getByRole("link", { name: "unique-helper" });
      // once the table has rendered, find the user coder row by its shadowed badge
      const badges = screen.getAllByText("shadowed");
      expect(badges).toHaveLength(1);
      return link;
    });
    expect(userCoderRow).toBeTruthy();

    const helperRow = screen.getByRole("link", { name: "unique-helper" }).closest("tr")!;
    expect(within(helperRow).queryByText("shadowed")).toBeNull();
  });
});

describe("Agents page summary line (#4335)", () => {
  const seed = () => {
    mockApi.listAgentTemplates.mockResolvedValue({
      templates: [
        tmpl({ id: "b-lead", name: "lead", scope: "builtin" }),
        tmpl({ id: "b-coder", name: "coder", scope: "builtin" }),
        tmpl({ id: "g-ops", name: "ops", scope: "global" }),
        tmpl({ id: "u-mine", name: "mine", scope: "user" }),
      ],
    });
    mockApi.getTemplateAllocations.mockResolvedValue({
      templates: [
        alloc({ id: "b-lead", name: "lead", scope: "builtin" }),
        alloc({ id: "b-coder", name: "coder", scope: "builtin", effective: false }),
        alloc({ id: "g-ops", name: "ops", scope: "global" }),
        alloc({ id: "u-mine", name: "mine", scope: "user", global_default: false }),
      ],
    });
  };

  it("describes the page as defining the lead and subagents", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    seed();
    render(<MemoryRouter><Agents /></MemoryRouter>);
    expect(
      await screen.findByText(/Agent templates define the lead and the subagents your runs use\./),
    ).toBeTruthy();
  });

  it("shows counts for a non-admin without global defaults", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    seed();
    render(<MemoryRouter><Agents /></MemoryRouter>);
    const summary = await screen.findByTestId("agents-summary");
    expect(summary.textContent).toContain("4 agents");
    expect(summary.textContent).toContain("2 ride your runs");
    expect(summary.textContent).toContain("2 builtin · 1 global · 1 mine");
    expect(summary.textContent).not.toContain("global default");
  });

  it("shows global defaults for an admin", async () => {
    mockUseAuth.mockReturnValue({ user: { ...MEMBER, is_admin: true } } as ReturnType<typeof useAuth>);
    seed();
    render(<MemoryRouter><Agents /></MemoryRouter>);
    const summary = await screen.findByTestId("agents-summary");
    expect(summary.textContent).toContain("3 global defaults");
  });

  it("is hidden while loading", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    mockApi.listAgentTemplates.mockReturnValue(new Promise(() => {}));
    mockApi.getTemplateAllocations.mockReturnValue(new Promise(() => {}));
    render(<MemoryRouter><Agents /></MemoryRouter>);
    expect(screen.queryByTestId("agents-summary")).toBeNull();
  });

  it("is hidden on a load error", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    mockApi.listAgentTemplates.mockRejectedValue(new Error("boom"));
    mockApi.getTemplateAllocations.mockResolvedValue({ templates: [] });
    render(<MemoryRouter><Agents /></MemoryRouter>);
    await screen.findByText(/boom|Failed to load templates/);
    expect(screen.queryByTestId("agents-summary")).toBeNull();
  });
});

describe("Agents page builtin drift badge (issue #201 M4a)", () => {
  it("badges only the row the server says differs, and reads 'differs from shipped'", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    const templates = [
      tmpl({ id: "b-coder", name: "coder", scope: "builtin", differs_from_builtin: true }),
      tmpl({ id: "b-tester", name: "tester", scope: "builtin", differs_from_builtin: false }),
    ];
    mockApi.listAgentTemplates.mockResolvedValue({ templates });
    mockApi.getTemplateAllocations.mockResolvedValue({
      templates: [
        alloc({ id: "b-coder", name: "coder", scope: "builtin" }),
        alloc({ id: "b-tester", name: "tester", scope: "builtin" }),
      ],
    });

    render(
      <MemoryRouter>
        <Agents />
      </MemoryRouter>,
    );

    const coderRow = (await screen.findByRole("link", { name: "coder" })).closest("tr")!;
    expect(within(coderRow).getByText("differs from shipped")).toBeTruthy();

    const testerRow = screen.getByRole("link", { name: "tester" }).closest("tr")!;
    expect(within(testerRow).queryByText("differs from shipped")).toBeNull();

    // The copy is the contract, not decoration: M4b auto-updates rows this badge
    // marks, and "customized"/"edited" would claim a human touched them.
    expect(screen.queryByText(/customized/i)).toBeNull();
    expect(screen.queryByText(/edited/i)).toBeNull();
  });

  it("renders the badge from the server field alone — the page never recomputes drift", async () => {
    mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
    // A user-scope row whose name collides with a builtin, flagged false by the
    // server. A page that inferred drift from the name would badge it here.
    const templates = [
      tmpl({ id: "b-coder", name: "coder", scope: "builtin", differs_from_builtin: false }),
      tmpl({ id: "u-coder", name: "coder", scope: "user", differs_from_builtin: false, prompt_body: "totally different" }),
    ];
    mockApi.listAgentTemplates.mockResolvedValue({ templates });
    mockApi.getTemplateAllocations.mockResolvedValue({
      templates: [
        alloc({ id: "b-coder", name: "coder", scope: "builtin" }),
        alloc({ id: "u-coder", name: "coder", scope: "user", my_override: true, effective: true, global_default: false }),
      ],
    });

    render(
      <MemoryRouter>
        <Agents />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getAllByRole("link", { name: "coder" })).toHaveLength(2));
    expect(screen.queryByText("differs from shipped")).toBeNull();
  });
});
