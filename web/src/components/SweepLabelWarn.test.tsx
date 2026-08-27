// @vitest-environment jsdom
//
// SweepLabelWarn (PRD #589 M4, success criterion 6): a sweep selector label missing on
// the target repo is warned about (advisory, never blocking), with a one-click "Create
// label" that creates it on the forge and clears the warning.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { SweepLabelWarn } from "./SweepLabelWarn";
import { api } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      checkRepoLabels: vi.fn(),
      ensureRepoLabels: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

beforeEach(() => {
  mockApi.checkRepoLabels.mockResolvedValue({ missing: [] });
  mockApi.ensureRepoLabels.mockResolvedValue({ ensured: [] });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("SweepLabelWarn", () => {
  it("warns when a selector label is missing on the repo, naming the label and repo", async () => {
    mockApi.checkRepoLabels.mockResolvedValue({ missing: ["bug"] });
    render(<SweepLabelWarn repoId="repo-atlas" repoPath="vtmocanu/atlas-api" labels={["bug"]} />);

    // The debounced check runs and the warning names the missing label + the repo.
    await waitFor(() => expect(mockApi.checkRepoLabels).toHaveBeenCalledWith("repo-atlas", ["bug"]));
    await waitFor(() =>
      expect(screen.getByText(/Label “bug” doesn't exist on vtmocanu\/atlas-api/)).toBeTruthy(),
    );
  });

  it("Create label calls ensureRepoLabels with the missing set and clears the warning", async () => {
    mockApi.checkRepoLabels.mockResolvedValue({ missing: ["bug"] });
    mockApi.ensureRepoLabels.mockResolvedValue({ ensured: ["bug"] });
    render(<SweepLabelWarn repoId="repo-atlas" repoPath="vtmocanu/atlas-api" labels={["bug"]} />);

    const create = await screen.findByRole("button", { name: /Create label “bug”/ });
    fireEvent.click(create);

    await waitFor(() => expect(mockApi.ensureRepoLabels).toHaveBeenCalledWith("repo-atlas", ["bug"]));
    // After ensuring, the warning is gone (the label now exists).
    await waitFor(() =>
      expect(screen.queryByText(/Label “bug” doesn't exist/)).toBeNull(),
    );
  });

  it("confirms with a success state after ensureRepoLabels instead of vanishing silently", async () => {
    mockApi.checkRepoLabels.mockResolvedValue({ missing: ["bug"] });
    mockApi.ensureRepoLabels.mockResolvedValue({ ensured: ["bug"] });
    render(<SweepLabelWarn repoId="repo-atlas" repoPath="vtmocanu/atlas-api" labels={["bug"]} />);

    const create = await screen.findByRole("button", { name: /Create label “bug”/ });
    fireEvent.click(create);

    await waitFor(() => expect(mockApi.ensureRepoLabels).toHaveBeenCalledWith("repo-atlas", ["bug"]));
    // The warn resolves, but rather than unmounting silently the component confirms the forge
    // mutation with a success state that names the created label + repo (non-vacuous).
    await waitFor(() =>
      expect(screen.getByText(/Label “bug” created on vtmocanu\/atlas-api/)).toBeTruthy(),
    );
    // And the warning itself is gone (it's the created state now, not the missing state).
    expect(screen.queryByText(/doesn't exist/)).toBeNull();
  });

  it("renders nothing when every selector label already exists (paired negative)", async () => {
    mockApi.checkRepoLabels.mockResolvedValue({ missing: [] });
    const { container } = render(
      <SweepLabelWarn repoId="repo-uzi" repoPath="vtmocanu/uzi" labels={["bug"]} />,
    );
    await waitFor(() => expect(mockApi.checkRepoLabels).toHaveBeenCalled());
    // A present label produces no warn and no Create button — non-vacuous vs the arm above.
    expect(screen.queryByText(/doesn't exist/)).toBeNull();
    expect(screen.queryByRole("button", { name: /Create label/ })).toBeNull();
    expect(container.textContent).toBe("");
  });

  it("does not check when the selector is empty (a no-label sweep matches the PRD label)", async () => {
    render(<SweepLabelWarn repoId="repo-uzi" labels={[]} />);
    // Nothing to check: the endpoint is never called and nothing renders.
    await new Promise((r) => setTimeout(r, 350));
    expect(mockApi.checkRepoLabels).not.toHaveBeenCalled();
  });
});
