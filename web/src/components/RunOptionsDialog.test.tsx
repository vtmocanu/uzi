// @vitest-environment jsdom
//
// RunOptionsDialog (PRD #3250 M2, module 6): the optional dialog beside one-click Start
// run. Direct-rendered with the page's facts, gate and start callback, so each rule is
// asserted on the component: row visibility per effective harness, the per-harness model
// vocabulary, the harness-change reset, the poll that resets nothing, the blocked Other
// entry, the submitting guards, the announced gate reason and the in-dialog refusal.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { api } from "../lib/api";
import type { StartPreview, StartPreviewRequest } from "../lib/apiTypes";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../mocks/data/effectiveSettings";
import { StartPreviewProvider } from "../lib/startPreviewStore";
import { RunOptionsDialog, type RunOptionsStartCallbacks } from "./RunOptionsDialog";
import { CODEX_CURATED_MODELS, CODEX_ROLE_MODELS } from "../lib/codexModels";
import { runStartBody, type RunStartFacts, type RunStartOptions } from "../lib/startRun";
import type { SecretMeta } from "../lib/api";
import type { StartRunGate } from "../lib/runStream";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  // TokenPicker is always handed its tokens here; stub listSecrets so nothing fetches.
  return {
    ...actual,
    api: {
      ...actual.api,
      listSecrets: vi.fn(async () => ({ secrets: [] })),
      // Issue #3261: the dialog's preview reads these two through the shared store.
      getEffectiveSettings: vi.fn(),
      previewStartSettings: vi.fn(),
    },
  };
});
// The preview store reads the signed-in user; only the preview describe renders a provider.
vi.mock("../auth/AuthContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../auth/AuthContext")>()),
  useOptionalAuth: () => ({ user: { id: "u1" } }),
}));

afterEach(cleanup);

const CLAUDE_ONLY: RunStartFacts = { claudeUsable: true, codexUsable: false, defaultHarness: null };
const CODEX_ONLY: RunStartFacts = { claudeUsable: false, codexUsable: true, defaultHarness: null };
const BOTH: RunStartFacts = { claudeUsable: true, codexUsable: true, defaultHarness: null };
const OPEN: StartRunGate = { enabled: true, reason: "" };

const token: SecretMeta = {
  id: "sec-1",
  label: "console-key",
  is_default: false,
  enabled: true,
  disabled_at: null,
  auto_eligible: true,
  kind: "anthropic_token",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

type StartFn = (options: RunStartOptions, cb: RunOptionsStartCallbacks) => void | Promise<void>;

function mount({
  facts = CLAUDE_ONLY,
  gate = () => OPEN,
  onStart = vi.fn<StartFn>(),
  onClose = vi.fn(),
}: {
  facts?: RunStartFacts;
  gate?: (h: RunStartOptions["harness"]) => StartRunGate;
  onStart?: ReturnType<typeof vi.fn<StartFn>>;
  onClose?: () => void;
} = {}) {
  const props = { issueIid: 42, issueTitle: "Add CSV export", gate, tokens: [token], onStart, onClose };
  const utils = render(<RunOptionsDialog {...props} facts={facts} />);
  const dialog = screen.getByRole("dialog", { name: "Run options for #42" });
  return {
    ...utils,
    dialog,
    onStart,
    onClose,
    rerenderFacts: (next: RunStartFacts) => utils.rerender(<RunOptionsDialog {...props} facts={next} />),
  };
}

const field = (dialog: HTMLElement, label: string) => within(dialog).queryByLabelText(label) as HTMLSelectElement | null;
const optionValues = (select: HTMLSelectElement) => Array.from(select.options).map((o) => o.value);
const startButton = (dialog: HTMLElement) => within(dialog).getByRole("button", { name: /^(Start run|Starting…)$/ }) as HTMLButtonElement;

describe("RunOptionsDialog rows", () => {
  it("renders the rows in order with the agreed Default labels (U+00B7)", () => {
    const { dialog } = mount({ facts: BOTH });
    const labels = Array.from(dialog.querySelectorAll("label")).map((l) => l.textContent);
    expect(labels).toEqual(["Harness", "Anthropic token", "Model", "Reasoning effort"]);
    // No server prediction yet (no provider here): plain Default, never a claim about "your settings".
    expect(field(dialog, "Model")!.options[0].textContent).toBe("Default");
    expect(field(dialog, "Reasoning effort")!.options[0].textContent).toBe("Default");
    expect(within(dialog).queryByText(/your settings/i)).toBeNull();
    expect(within(dialog).getByText(/#42 Add CSV export/)).toBeTruthy();
  });

  it("Claude-only: no Harness row; token, model and effort rows", () => {
    const { dialog } = mount({ facts: CLAUDE_ONLY });
    expect(field(dialog, "Harness")).toBeNull();
    expect(field(dialog, "Anthropic token")).not.toBeNull();
    expect(field(dialog, "Model")).not.toBeNull();
    expect(field(dialog, "Reasoning effort")).not.toBeNull();
  });

  it("Codex-only: no Harness and no Anthropic token row", () => {
    const { dialog } = mount({ facts: CODEX_ONLY });
    expect(field(dialog, "Harness")).toBeNull();
    expect(field(dialog, "Anthropic token")).toBeNull();
  });

  it("both usable: picking Codex hides the token row; a usable Codex default hides it on inherit", () => {
    const { dialog } = mount({ facts: BOTH });
    expect(field(dialog, "Anthropic token")).not.toBeNull();
    fireEvent.change(field(dialog, "Harness")!, { target: { value: "codex" } });
    expect(field(dialog, "Anthropic token")).toBeNull();
    cleanup();
    const second = mount({ facts: { ...BOTH, defaultHarness: "codex" } });
    expect(field(second.dialog, "Harness")!.value).toBe("inherit");
    expect(field(second.dialog, "Anthropic token")).toBeNull();
  });

  it("the Claude model list carries no Codex role id and offers custom entry", () => {
    const { dialog } = mount({ facts: CLAUDE_ONLY });
    const values = optionValues(field(dialog, "Model")!);
    for (const id of CODEX_ROLE_MODELS) expect(values).not.toContain(id);
    expect(values).toContain("opus");
    expect(values).toContain("custom");
  });

  it("the Codex model list is the curated set, with no custom entry", () => {
    const { dialog } = mount({ facts: CODEX_ONLY });
    const values = optionValues(field(dialog, "Model")!);
    expect(values).toEqual(["inherit", ...CODEX_CURATED_MODELS]);
  });
});

describe("RunOptionsDialog model entry", () => {
  const chooseOther = (dialog: HTMLElement, text: string) => {
    fireEvent.change(field(dialog, "Model")!, { target: { value: "custom" } });
    const input = within(dialog).getByLabelText("Custom model ID") as HTMLInputElement;
    fireEvent.change(input, { target: { value: text } });
  };

  it("an empty Other disables Start run with an inline error the select references", () => {
    const { dialog } = mount();
    fireEvent.change(field(dialog, "Model")!, { target: { value: "custom" } });
    expect(startButton(dialog).disabled).toBe(true);
    const error = within(dialog).getByText("Enter a model ID, or choose Default.");
    expect(field(dialog, "Model")!.getAttribute("aria-describedby")).toContain(error.id);
    expect(startButton(dialog).getAttribute("aria-describedby")).toContain(error.id);
  });

  it.each([" opus", "opus ", "claude x", "   "])("whitespace in Other (%j) disables Start run", (text) => {
    const { dialog } = mount();
    chooseOther(dialog, text);
    expect(startButton(dialog).disabled).toBe(true);
    expect(within(dialog).getByText(/single word with no spaces|Enter a model ID/)).toBeTruthy();
  });

  it.each(CODEX_ROLE_MODELS)("a Codex role id (%s) in Claude's Other disables Start run", (id) => {
    const { dialog } = mount({ facts: BOTH });
    chooseOther(dialog, id);
    expect(startButton(dialog).disabled).toBe(true);
    expect(within(dialog).getByText(`${id} is a Codex model. Choose a Claude model, or start the run on Codex.`)).toBeTruthy();
  });

  it("Claude-only (no Harness row): the Codex-id error offers no impossible switch to Codex", () => {
    const id = CODEX_ROLE_MODELS[0];
    const { dialog } = mount({ facts: CLAUDE_ONLY });
    expect(field(dialog, "Harness")).toBeNull();
    chooseOther(dialog, id);
    expect(startButton(dialog).disabled).toBe(true);
    expect(within(dialog).getByText(`${id} is a Codex model. Choose a Claude model.`)).toBeTruthy();
    expect(within(dialog).queryByText(/start the run on Codex/)).toBeNull();
  });

  it("a well-formed custom Claude id starts with that exact value", () => {
    const onStart = vi.fn<StartFn>();
    const { dialog } = mount({ onStart });
    chooseOther(dialog, "claude-x-1");
    expect(startButton(dialog).disabled).toBe(false);
    fireEvent.click(startButton(dialog));
    expect(onStart).toHaveBeenCalledTimes(1);
    expect(onStart.mock.calls[0][0].model).toBe("claude-x-1");
  });
});

describe("RunOptionsDialog harness changes and the poll", () => {
  it("a user harness change resets Model to Default and keeps effort", () => {
    const onStart = vi.fn<StartFn>();
    const { dialog } = mount({ facts: BOTH, onStart });
    fireEvent.change(field(dialog, "Model")!, { target: { value: "opus" } });
    fireEvent.change(field(dialog, "Reasoning effort")!, { target: { value: "high" } });
    fireEvent.change(field(dialog, "Harness")!, { target: { value: "codex" } });
    expect(field(dialog, "Model")!.value).toBe("inherit");
    expect(field(dialog, "Reasoning effort")!.value).toBe("high");
    fireEvent.click(startButton(dialog));
    expect(onStart.mock.calls[0][0]).toMatchObject({ harness: "codex", model: "", effort: "high" });
  });

  it("an empty Other also returns to Default on a harness change", () => {
    const { dialog } = mount({ facts: BOTH });
    fireEvent.change(field(dialog, "Model")!, { target: { value: "custom" } });
    expect(startButton(dialog).disabled).toBe(true);
    fireEvent.change(field(dialog, "Harness")!, { target: { value: "claude" } });
    expect(field(dialog, "Model")!.value).toBe("inherit");
    expect(within(dialog).queryByLabelText("Custom model ID")).toBeNull();
    expect(startButton(dialog).disabled).toBe(false);
  });

  it("a precondition refresh re-evaluates rows but resets nothing; the builder drops the hidden values", () => {
    const onStart = vi.fn<StartFn>();
    const { dialog, rerenderFacts } = mount({ facts: BOTH, onStart });
    fireEvent.change(field(dialog, "Harness")!, { target: { value: "claude" } });
    fireEvent.change(field(dialog, "Anthropic token")!, { target: { value: "sec-1" } });
    fireEvent.change(field(dialog, "Model")!, { target: { value: "sonnet" } });
    fireEvent.change(field(dialog, "Reasoning effort")!, { target: { value: "max" } });
    // The poll finds Claude no longer usable: the Harness row and the token row go.
    rerenderFacts(CODEX_ONLY);
    expect(field(dialog, "Harness")).toBeNull();
    expect(field(dialog, "Anthropic token")).toBeNull();
    expect(field(dialog, "Reasoning effort")!.value).toBe("max");
    // The model value is kept (shown as unavailable on the Codex list), so Start run is
    // blocked until the person chooses; nothing was silently reset.
    expect(within(dialog).getByText("sonnet is not a Codex model. Choose one from the list, or Default.")).toBeTruthy();
    // The poll restores both harnesses: every choice is still there.
    rerenderFacts(BOTH);
    expect(field(dialog, "Harness")!.value).toBe("claude");
    expect(field(dialog, "Anthropic token")!.value).toBe("sec-1");
    expect(field(dialog, "Model")!.value).toBe("sonnet");
    fireEvent.click(startButton(dialog));
    const options = onStart.mock.calls[0][0];
    expect(options).toMatchObject({ harness: "claude", model: "sonnet", effort: "max" });
    // The builder drops what a hidden row would have carried.
    expect(runStartBody(options, CLAUDE_ONLY)).toEqual({
      credential_override: { mode: "pinned", secret_id: "sec-1" },
      model: "sonnet",
      effort: "max",
    });
  });
});

describe("RunOptionsDialog start", () => {
  it("effort only: the options build {effort} and nothing else (example 3)", () => {
    const onStart = vi.fn<StartFn>();
    const { dialog } = mount({ facts: BOTH, onStart });
    fireEvent.change(field(dialog, "Reasoning effort")!, { target: { value: "high" } });
    fireEvent.click(startButton(dialog));
    expect(runStartBody(onStart.mock.calls[0][0], BOTH)).toEqual({ effort: "high" });
  });

  it("while submitting: Start disabled, Escape, backdrop and Close do not dismiss", () => {
    const onClose = vi.fn();
    const onStart = vi.fn<StartFn>(() => new Promise(() => {}));
    const { dialog } = mount({ onStart, onClose });
    fireEvent.click(startButton(dialog));
    expect(startButton(dialog).textContent).toBe("Starting…");
    expect(startButton(dialog).disabled).toBe(true);
    fireEvent.keyDown(dialog, { key: "Escape" });
    fireEvent.mouseDown(dialog);
    expect((within(dialog).getByRole("button", { name: "Close" }) as HTMLButtonElement).disabled).toBe(true);
    expect((within(dialog).getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled).toBe(true);
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog")).toBe(dialog);
  });

  it("when not submitting, Escape and Cancel close", () => {
    const onClose = vi.fn();
    const { dialog } = mount({ onClose });
    fireEvent.keyDown(dialog, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it("the gate reason disables Start, is shown as text and is referenced by aria-describedby", () => {
    const reason = "A run is already in progress for this issue.";
    const gate = vi.fn(() => ({ enabled: false, reason }));
    const { dialog } = mount({ gate });
    const start = startButton(dialog);
    expect(start.disabled).toBe(true);
    const text = within(dialog).getByText(reason);
    expect(text.id).not.toBe("");
    expect(start.getAttribute("aria-describedby")?.split(" ")).toContain(text.id);
    expect(gate).toHaveBeenLastCalledWith("inherit");
  });

  it("asks the gate about the harness the dialog would start on", () => {
    const gate = vi.fn((h: RunStartOptions["harness"]) =>
      h === "codex" ? { enabled: false, reason: "Add a usable Codex credential first (Settings)." } : OPEN,
    );
    const { dialog } = mount({ facts: BOTH, gate });
    expect(startButton(dialog).disabled).toBe(false);
    fireEvent.change(field(dialog, "Harness")!, { target: { value: "codex" } });
    expect(startButton(dialog).disabled).toBe(true);
    expect(within(dialog).getByText("Add a usable Codex credential first (Settings).")).toBeTruthy();
  });

  it("a server refusal lands in the alert region; the dialog stays open with its choices", async () => {
    const text = "opus is not a model a Codex run can start with, and this run resolved to the codex harness; start it on the Claude harness or choose one of: gpt-5.6-sol";
    const onStart = vi.fn<StartFn>((_o, cb) => {
      cb.onError(text);
      cb.onSettled();
    });
    const onClose = vi.fn();
    const { dialog } = mount({ facts: BOTH, onStart, onClose });
    fireEvent.change(field(dialog, "Model")!, { target: { value: "opus" } });
    fireEvent.change(field(dialog, "Reasoning effort")!, { target: { value: "low" } });
    fireEvent.click(startButton(dialog));
    await waitFor(() => expect(within(dialog).getByRole("alert").textContent).toBe(text));
    expect(onClose).not.toHaveBeenCalled();
    expect(field(dialog, "Model")!.value).toBe("opus");
    expect(field(dialog, "Reasoning effort")!.value).toBe("low");
    expect(startButton(dialog).disabled).toBe(false);
    // A retry clears the previous refusal before it is answered.
    onStart.mockImplementationOnce(() => new Promise(() => {}));
    fireEvent.click(startButton(dialog));
    expect(within(dialog).getByRole("alert").textContent).toBe("");
  });

  it("returns focus to Start run when a start settles without navigating", async () => {
    let settle: (() => void) | undefined;
    const onStart = vi.fn<StartFn>(
      (_o, cb) =>
        new Promise<void>((resolve) => {
          settle = () => {
            cb.onError("worker pool is full");
            cb.onSettled();
            resolve();
          };
        }),
    );
    const { dialog } = mount({ onStart });
    const start = startButton(dialog);
    start.focus();
    fireEvent.click(start);
    // A real browser drops focus to <body> when it disables the focused button; jsdom does
    // not, so park focus on a throwaway element and remove it.
    const parked = document.createElement("input");
    document.body.appendChild(parked);
    parked.focus();
    parked.remove();
    expect(document.activeElement).toBe(document.body);
    settle!();
    await waitFor(() => expect(document.activeElement).toBe(startButton(dialog)));
    expect(within(dialog).getByRole("alert").textContent).toBe("worker pool is full");
  });
});

// Issue #3261: the read-only preview and the source-aware Default labels, inside the real store.
describe("RunOptionsDialog start preview", () => {
  const mockApi = vi.mocked(api);
  beforeEach(() => {
    mockApi.getEffectiveSettings.mockReset();
    mockApi.previewStartSettings.mockReset();
  });
  const SCENARIO = RECORDED_EFFECTIVE_SCENARIOS[0];
  const resp = (name: string): StartPreview => structuredClone(SCENARIO.start_previews.find((p) => p.name === name)!.response);
  const isDefaultChoice = (r: StartPreviewRequest) => !r.harness && !r.credential_override && !r.model && !r.effort;

  // `defaultResponse` answers the default-choice request; explicit harnesses get the recorded ones.
  function serve(defaultResponse: StartPreview) {
    mockApi.getEffectiveSettings.mockResolvedValue(structuredClone(SCENARIO.get));
    mockApi.previewStartSettings.mockImplementation(async (r: StartPreviewRequest) => {
      if (isDefaultChoice(r)) return structuredClone(defaultResponse);
      return resp(r.harness === "codex" ? "codex" : "claude");
    });
  }

  function mountWithPreview(facts: RunStartFacts = BOTH, onStart = vi.fn<StartFn>()) {
    const props = { issueIid: 42, issueTitle: "Add CSV export", gate: () => OPEN, tokens: [token], onStart, onClose: vi.fn() };
    render(
      <MemoryRouter>
        <StartPreviewProvider generation={1}>
          <RunOptionsDialog {...props} facts={facts} />
        </StartPreviewProvider>
      </MemoryRouter>,
    );
    return { dialog: screen.getByRole("dialog", { name: "Run options for #42" }), onStart };
  }

  const requests = () => mockApi.previewStartSettings.mock.calls.map((c) => c[0] as StartPreviewRequest);

  it("shows the open preview and a defaults hint that no longer claims 'your Settings'", async () => {
    serve(resp("claude"));
    const { dialog } = mountWithPreview();
    expect(await within(dialog).findByRole("button", { name: /If it started now: Claude · opus · medium/ })).toBeTruthy();
    expect(within(dialog).getByText(/Model and effort left on Default are resolved when a worker picks the run up/)).toBeTruthy();
    expect(within(dialog).queryByText(/follows your Settings/)).toBeNull();
  });

  it("does not repeat the preview's note lines inside the dialog", async () => {
    serve(resp("claude"));
    const { dialog } = mountWithPreview();
    expect(await within(dialog).findByRole("button", { name: /If it started now: Claude · opus · medium/ })).toBeTruthy();
    expect(within(dialog).queryByText(/A prediction/)).toBeNull();
    expect(within(dialog).queryByText(/A choice made here is fixed/)).toBeNull();
  });

  it("an undecided default names 'chosen when started' on the harness Default and depends-on-harness for model and effort", async () => {
    serve(resp("default start"));
    const { dialog } = mountWithPreview();
    await waitFor(() => expect(field(dialog, "Harness")!.options[0].textContent).toBe("Default · chosen when started"));
    expect(field(dialog, "Model")!.options[0].textContent).toBe("Default · depends on the harness chosen when started");
    expect(field(dialog, "Reasoning effort")!.options[0].textContent).toBe("Default · depends on the harness chosen when started");
  });

  it("a decided Claude default shows the server source, never the client-derived harness", async () => {
    serve(resp("auto token"));
    const { dialog } = mountWithPreview();
    await waitFor(() => expect(field(dialog, "Harness")!.options[0].textContent).toMatch(/^Default · Claude \(.+\)$/));
    expect(field(dialog, "Model")!.options[0].textContent).toMatch(/^Default · opus \(.+\)$/);
    expect(field(dialog, "Reasoning effort")!.options[0].textContent).toMatch(/^Default · medium \(.+\)$/);
  });

  it("explicit Codex with a Claude default: the harness Default stays Claude, model and effort Defaults show Codex", async () => {
    serve(resp("auto token"));
    const { dialog } = mountWithPreview();
    await waitFor(() => expect(field(dialog, "Harness")!.options[0].textContent).toMatch(/^Default · Claude/));
    fireEvent.change(field(dialog, "Harness")!, { target: { value: "codex" } });
    await waitFor(() => expect(field(dialog, "Model")!.options[0].textContent).toMatch(/gpt-6-sol/));
    expect(field(dialog, "Harness")!.options[0].textContent).toMatch(/^Default · Claude \(/);
    expect(field(dialog, "Reasoning effort")!.options[0].textContent).toMatch(/^Default · medium \(/);
  });

  it("explicit Codex with an undecided default: the harness Default stays 'chosen when started'", async () => {
    serve(resp("default start"));
    const { dialog } = mountWithPreview();
    await waitFor(() => expect(field(dialog, "Harness")!.options[0].textContent).toBe("Default · chosen when started"));
    fireEvent.change(field(dialog, "Harness")!, { target: { value: "codex" } });
    await waitFor(() => expect(field(dialog, "Model")!.options[0].textContent).toMatch(/gpt-6-sol/));
    expect(field(dialog, "Harness")!.options[0].textContent).toBe("Default · chosen when started");
  });

  it("the preview follows the model and effort choice and sends one POST per distinct choice", async () => {
    serve(resp("claude"));
    const { dialog } = mountWithPreview(CLAUDE_ONLY);
    await within(dialog).findByRole("button", { name: /If it started now/ });
    fireEvent.change(field(dialog, "Model")!, { target: { value: "opus" } });
    fireEvent.change(field(dialog, "Reasoning effort")!, { target: { value: "high" } });
    await waitFor(() => expect(requests()).toContainEqual({ harness: null, credential_override: null, model: "opus", effort: "high" }));
    fireEvent.change(field(dialog, "Reasoning effort")!, { target: { value: "inherit" } });
    fireEvent.change(field(dialog, "Reasoning effort")!, { target: { value: "high" } });
    await waitFor(() => expect(requests().length).toBeGreaterThanOrEqual(3));
    const keys = requests().map((r) => JSON.stringify(r));
    expect(new Set(keys).size).toBe(keys.length);
  });

  it("a token hidden by the Codex choice is not submitted to the preview", async () => {
    serve(resp("claude"));
    const { dialog } = mountWithPreview(BOTH);
    await within(dialog).findByRole("button", { name: /If it started now/ });
    fireEvent.change(field(dialog, "Anthropic token")!, { target: { value: "sec-1" } });
    await waitFor(() => expect(requests().some((r) => r.credential_override?.secret_id === "sec-1")).toBe(true));
    fireEvent.change(field(dialog, "Harness")!, { target: { value: "codex" } });
    await waitFor(() => expect(requests().some((r) => r.harness === "codex")).toBe(true));
    const codexReq = requests().find((r) => r.harness === "codex")!;
    expect(codexReq.credential_override).toBeNull();
  });

  it("sends no preview request while the model is invalid, and debounces a custom entry", async () => {
    serve(resp("claude"));
    const { dialog } = mountWithPreview(CLAUDE_ONLY);
    await within(dialog).findByRole("button", { name: /If it started now/ });
    const before = requests().length;
    fireEvent.change(field(dialog, "Model")!, { target: { value: "custom" } });
    const input = within(dialog).getByLabelText("Custom model ID") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "two words" } });
    await new Promise((r) => setTimeout(r, 600));
    expect(requests().length).toBe(before);
    // A valid entry reaches the request only after the pause in typing.
    fireEvent.change(input, { target: { value: "claude-x-1" } });
    expect(requests().some((r) => r.model === "claude-x-1")).toBe(false);
    await waitFor(() => expect(requests().some((r) => r.model === "claude-x-1")).toBe(true), { timeout: 3000 });
    // The stale invalid entry is never sent while the debounce catches up to the fix.
    expect(requests().filter((r) => r.model === "two words")).toEqual([]);
  });

  it("a preview failure leaves Start enabled and the server error visible", async () => {
    mockApi.getEffectiveSettings.mockRejectedValue(new Error("boom"));
    mockApi.previewStartSettings.mockRejectedValue(new Error("boom"));
    const onStart = vi.fn<StartFn>((_o, cb) => {
      cb.onError("server said no");
      cb.onSettled();
    });
    const { dialog } = mountWithPreview(BOTH, onStart);
    expect(await within(dialog).findByText("Preview unavailable; Start will validate your choices.")).toBeTruthy();
    expect(startButton(dialog).disabled).toBe(false);
    // Default labels fall back to plain Default, never "your settings".
    expect(field(dialog, "Model")!.options[0].textContent).toBe("Default");
    fireEvent.click(startButton(dialog));
    await waitFor(() => expect(within(dialog).getByRole("alert").textContent).toBe("server said no"));
    expect(onStart).toHaveBeenCalledTimes(1);
  });
});
