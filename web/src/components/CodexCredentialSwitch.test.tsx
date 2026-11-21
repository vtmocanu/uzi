// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { api, type Run, type SecretMeta } from "../lib/api";
import { ApiError } from "../lib/apiError";
import { SwitchTokenAction, RunCredentialOverride } from "./RunCredentialOverride";
import { RunCredential } from "./RunCredential";

vi.mock("../lib/api", () => ({ api: { listSecrets: vi.fn(), setRunCredential: vi.fn() } }));
afterEach(() => { cleanup(); vi.resetAllMocks(); });
const run = (over: Partial<Run> = {}) => ({
  id: "run-1", harness: "codex", kind: "issue", status: "running",
  codex_secret_id: "credential-a", codex_secret_label: "Account A",
  codex_select_reason: "codex_binding", credential_switch: null, ...over,
}) as Run;
const secret = (over: Partial<SecretMeta> = {}) => ({
  id: "credential-a", label: "Account A", kind: "codex_auth", codex_status: "linked",
  enabled: true, is_default: false, auto_eligible: false, disabled_at: null,
  created_at: "", updated_at: "", ...over,
}) as SecretMeta;
const b = secret({ id: "credential-b", label: "Account B", kind: "openai_api_key", codex_status: "static" });
function open() { fireEvent.click(screen.getByRole("button", { name: "Switch credential" })); }
function choose(id: string) { fireEvent.change(screen.getByLabelText("Codex credential"), { target: { value: id } }); }
const confirm = () => screen.getByRole("button", { name: "Switch to this credential" }) as HTMLButtonElement;

describe("Codex credential switch", () => {
  it.each(["running", "awaiting_approval", "awaiting_input", "awaiting_followup"] as const)(
    "offers owner control and fetches credentials for %s", async status => {
      vi.mocked(api.listSecrets).mockResolvedValue({ secrets: [secret(), b] });
      render(<SwitchTokenAction run={run({ status })} canSteer />);
      open();
      await waitFor(() => expect((screen.getByLabelText("Codex credential") as HTMLSelectElement).disabled).toBe(false));
      expect(api.listSecrets).toHaveBeenCalledTimes(1);
      expect(confirm().disabled).toBe(true);
    },
  );
  it.each([
    { canSteer: false }, { status: "queued" }, { status: "claimed" }, { status: "paused" },
    { status: "completed" }, { status: "failed" }, { status: "cancelled" },
    { status: "limit_wait" }, { status: "pool_wait" }, { status: "recovery_wait" },
    { kind: "chat" }, { kind: "judge" }, { kind: "job" },
    { kind: "cross_check" }, { trigger_source: "task_review" },
  ])("hides ineligible control without fetching: %j", over => {
    const { canSteer = true, ...fields } = over as Partial<Run> & { canSteer?: boolean };
    const { container } = render(<SwitchTokenAction run={run(fields)} canSteer={canSteer} />);
    expect(container.firstChild).toBeNull();
    expect(api.listSecrets).not.toHaveBeenCalled();
  });
  it.each(["issue", "self_improve"] as const)("seeds by committed ID, distinguishes both auth kinds and sends only an explicit pinned ID for %s", async kind => {
    const current = secret({ label: "Same" });
    const other = { ...b, label: "Same" };
    const onSwitched = vi.fn();
    const returned = run({ kind, credential_switch: "requested" });
    vi.mocked(api.setRunCredential).mockResolvedValue({ run: returned });
    render(<SwitchTokenAction run={run({ kind, codex_secret_label: "Different snapshot" })} canSteer tokens={[other, current]} onSwitched={onSwitched} />);
    open();
    const select = screen.getByLabelText("Codex credential") as HTMLSelectElement;
    expect(select.value).toBe(current.id);
    expect(screen.getByRole("option", { name: /Same — Codex subscription · ential-a/ })).toBeTruthy();
    expect(screen.getByRole("option", { name: /Same — OpenAI API key · ential-b/ })).toBeTruthy();
    expect(confirm().disabled).toBe(true);
    choose(other.id);
    fireEvent.click(confirm());
    await waitFor(() => expect(onSwitched).toHaveBeenCalledWith(returned));
    expect(api.setRunCredential).toHaveBeenCalledExactlyOnceWith("run-1", { mode: "pinned", secret_id: other.id });
    // onSwitched runs before the closed, not-busy render commits; focus returns after that commit.
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Switch credential" })));
  });
  it("does not auto-select the first available credential or resolve a missing ID by label", () => {
    render(<SwitchTokenAction run={run({ codex_secret_id: "deleted", codex_secret_label: b.label })} canSteer tokens={[b]} />);
    open();
    expect((screen.getByLabelText("Codex credential") as HTMLSelectElement).value).toBe("deleted");
    expect(screen.getByRole("option", { name: "Account B — unavailable" }).hasAttribute("disabled")).toBe(true);
    expect(confirm().disabled).toBe(true);
    expect(api.setRunCredential).not.toHaveBeenCalled();
  });
  it("has no automatic, default or inherit option when the run has no binding", () => {
    render(<SwitchTokenAction run={run({ codex_secret_id: null })} canSteer tokens={[b]} />);
    open();
    expect((screen.getByLabelText("Codex credential") as HTMLSelectElement).value).toBe("");
    expect(screen.getAllByRole("option")).toHaveLength(2);
    expect(confirm().disabled).toBe(true);
  });
  it.each([
    { codex_status: "staging" }, { codex_status: "failed" }, { codex_status: undefined },
    { codex_status: "unknown" }, { enabled: false }, { codex_status: "static" },
    { kind: "openai_api_key", codex_status: "linked" },
  ])("makes unusable credentials visibly unavailable: %j", over => {
    render(<SwitchTokenAction run={run()} canSteer tokens={[secret(over), b]} />);
    open();
    const option = screen.getByRole("option", { name: /Account A.*unavailable/ });
    expect((option as HTMLOptionElement).disabled).toBe(true);
    expect(confirm().disabled).toBe(true);
  });
  it("excludes Anthropic credentials and sanitizes legacy labels in options, title and placeholder", () => {
    const dirty = "Account" + String.fromCodePoint(0x202e) + " B";
    const { container } = render(<SwitchTokenAction run={run({ codex_secret_label: dirty })} canSteer tokens={[{ ...b, label: dirty }, secret({ kind: "anthropic_token" })]} />);
    open();
    expect(screen.getByRole("option", { name: /Account B — OpenAI API key/ })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Account B — unavailable" })).toBeTruthy();
    expect(container.innerHTML).not.toContain(String.fromCodePoint(0x202e));
    choose(b.id);
    expect(screen.getByLabelText("Codex credential").getAttribute("title")).toBe("Account B");
  });
  it("describes loading, fetch failure and retry, then empty results", async () => {
    let reject!: (e: unknown) => void;
    vi.mocked(api.listSecrets).mockImplementationOnce(() => new Promise((_, no) => { reject = no; }));
    render(<SwitchTokenAction run={run()} canSteer />);
    open();
    expect(screen.getByText("Loading credentials…")).toBeTruthy();
    expect(confirm().disabled).toBe(true);
    reject(new ApiError(503, "Credentials temporarily unavailable"));
    await screen.findByText("Credentials temporarily unavailable");
    vi.mocked(api.listSecrets).mockResolvedValue({ secrets: [] });
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByText("No Codex credentials available.");
    expect(api.listSecrets).toHaveBeenCalledTimes(2);
    expect(confirm().disabled).toBe(true);
  });
  it.each([[409, "Run is now claimed"], [409, "Policy refuses this switch"], [409, "Worker does not support switching"], [422, "Queued runs cannot switch credentials"]])(
    "shows server refusal %s %s and preserves the editor", async (status, message) => {
      vi.mocked(api.setRunCredential).mockRejectedValue(new ApiError(Number(status), String(message)));
      const onSwitched = vi.fn();
      render(<SwitchTokenAction run={run()} canSteer tokens={[b]} onSwitched={onSwitched} />);
      open(); choose(b.id); fireEvent.click(confirm());
      await screen.findByText(String(message));
      expect(onSwitched).not.toHaveBeenCalled();
      expect(screen.getByLabelText("Codex credential")).toBeTruthy();
    },
  );
  it("disables confirmation and cancellation while busy and returns focus on cancel", async () => {
    let resolve!: (value: { run: Run }) => void;
    vi.mocked(api.setRunCredential).mockImplementation(() => new Promise(yes => { resolve = yes; }));
    render(<SwitchTokenAction run={run()} canSteer tokens={[b]} />);
    open(); choose(b.id); fireEvent.click(confirm());
    expect((screen.getByRole("button", { name: "Switching…" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled).toBe(true);
    resolve({ run: run() });
    await screen.findByRole("button", { name: "Switch credential" });
    await waitFor(() => expect(screen.queryByLabelText("Codex credential")).toBeNull());
    open(); fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Switch credential" }));
  });
  it.each(["issue", "self_improve"] as const)("keeps committed A through pending and displays B only from applied props for %s", async kind => {
    const a = run({ kind });
    const pending = run({ kind, credential_switch: "requested" });
    const applied = run({ kind, codex_secret_id: b.id, codex_secret_label: b.label });
    vi.mocked(api.setRunCredential).mockResolvedValue({ run: pending });
    const onSwitched = vi.fn();
    function Header({ value }: { value: Run }) {
      return <MemoryRouter><RunCredential run={value} /><RunCredentialOverride run={value} />
        <SwitchTokenAction run={value} canSteer tokens={[secret(), b]} onSwitched={onSwitched} /></MemoryRouter>;
    }
    const { rerender } = render(<Header value={a} />);
    expect(screen.getByText("Account A")).toBeTruthy();
    open(); choose(b.id); fireEvent.click(confirm());
    await waitFor(() => expect(onSwitched).toHaveBeenCalledWith(pending));
    expect(screen.getByText("Account A")).toBeTruthy();
    rerender(<Header value={pending} />);
    expect(screen.getByText("switch pending")).toBeTruthy();
    expect(screen.getByText("Account A")).toBeTruthy();
    expect(screen.queryByText("Account B")).toBeNull();
    rerender(<Header value={applied} />);
    expect(screen.getByText("Account B")).toBeTruthy();
    expect(screen.queryByText("switch pending")).toBeNull();
  });
});
