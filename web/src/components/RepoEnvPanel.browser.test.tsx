/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { userEvent } from "vitest/browser";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { RepoEnvPanel } from "./RepoEnvPanel";
import { api, type RunEnvList } from "../lib/api";

// Value-field behaviour that depends on a real engine: native password masking of the
// single-line input, and the textarea's LF normalisation of typed line breaks.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { listRepoEnv: vi.fn(), putRepoEnv: vi.fn() },
  };
});

const mockApi = vi.mocked(api);

const EMPTY: RunEnvList = {
  variables: [],
  counts: { total: 0, disabled: 0, pending_deletion: 0, limit: 50 },
  vault_locked: false,
};

beforeEach(() => {
  mockApi.listRepoEnv.mockResolvedValue(EMPTY);
  mockApi.putRepoEnv.mockResolvedValue({ variable: {} as never });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

async function openAdd() {
  render(<RepoEnvPanel repoId="repo-1" repoLabel="o/r" onLiveCount={() => {}} />);
  const add = await screen.findByRole("button", { name: "Add variable" });
  await waitFor(() => expect((add as HTMLButtonElement).disabled).toBe(false));
  await userEvent.click(add);
}

describe("RepoEnvPanel value field in a real browser", () => {
  it("masks a single-line secret with a native password input and reveals a plain one", async () => {
    await openAdd();
    const value = screen.getByLabelText("Value") as HTMLInputElement;
    expect(value.tagName).toBe("INPUT");
    expect(value.type).toBe("password");
    await userEvent.click(screen.getByRole("switch", { name: "Secret" }));
    expect((screen.getByLabelText("Value") as HTMLInputElement).type).toBe("text");
  });

  it("submits typed multiline input with LF line endings", async () => {
    await openAdd();
    await userEvent.type(screen.getByLabelText("Name"), "KEY_PEM");
    await userEvent.click(screen.getByRole("switch", { name: "Multiline" }));
    const value = screen.getByLabelText("Value") as HTMLTextAreaElement;
    expect(value.tagName).toBe("TEXTAREA");
    await userEvent.click(value);
    await userEvent.keyboard("line1{Enter}line2{Enter}");
    await userEvent.click(screen.getByRole("button", { name: "Save variable" }));
    await waitFor(() => expect(mockApi.putRepoEnv).toHaveBeenCalled());
    const sent = mockApi.putRepoEnv.mock.calls[0][2];
    expect(sent.value).toBe("line1\nline2\n");
    expect(sent.value).not.toContain("\r");
  });
});
