// @vitest-environment jsdom
//
// IssueDraftCard serves BOTH filing surfaces — the judge/run page (a repo Select, seeded to the
// draft's default, with default_note as an info box) and the Findings page (a fixed read-only
// repo). These cover both modes, the #124 seed strip (on the title/description the card seeds,
// not on the user's own edits), the failed-load Retry recovery, the optional stale warning, and
// that a Create failure keeps the card open with the edits intact.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { IssueDraftCard, type IssueDraftSeed } from "./IssueDraftCard";
import { ApiError, type Repo } from "../../lib/api";

afterEach(cleanup);

function repoOpt(id: string, path: string): Repo {
  return {
    id,
    connection_id: "c1",
    forge_project_id: 1,
    path_with_namespace: path,
    web_url: `https://forge.example/${path}`,
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: false,
    repo_claudemd_enabled: false,
    repo_devbox_opt_in: false,
    repo_fold_improve_uzi_backlog: false,
    pipeline: null,
    guardrail_blocked: false,
    docker_allowlisted: false,
    docker_blocked: false,
  };
}

function seedFixture(over: Partial<IssueDraftSeed> = {}): IssueDraftSeed {
  return {
    title: "Improve the poller: api/internal/poller",
    description: "## What the judge found\n\nQueue-to-claim latency dominated the run.",
    labels: ["uzi", "judge"],
    provenance: "from vlad's worker, run 8f2c1d04",
    defaultRepoId: "repo1",
    defaultNote: "Defaulted to the judged run's repo.",
    ...over,
  };
}

const repos = [repoOpt("repo1", "vtmocanu/uzi"), repoOpt("repo2", "vtmocanu/other")];

// Renders in repo-Select mode with a scripted draft and waits for the form.
async function renderSelectMode(seedOver: Partial<IssueDraftSeed> = {}, onCreate = vi.fn().mockResolvedValue(undefined)) {
  const loadDraft = vi.fn().mockResolvedValue(seedFixture(seedOver));
  const onCancel = vi.fn();
  const view = render(<IssueDraftCard loadDraft={loadDraft} onCreate={onCreate} onCancel={onCancel} repos={repos} />);
  await screen.findByText("Draft issue");
  // Wait on the Create button (present only once the seed loads) rather than a specific
  // title — some tests override the seed title, so a fixed display-value wait would hang.
  await screen.findByRole("button", { name: "Create issue" });
  return { ...view, loadDraft, onCreate, onCancel };
}

describe("IssueDraftCard — repo Select mode (judge/run page)", () => {
  it("loads the draft on mount, shows the repo Select seeded to the default, and the provenance", async () => {
    await renderSelectMode();
    const select = screen.getByRole("combobox") as HTMLSelectElement;
    expect(select.value).toBe("repo1");
    expect(screen.getByText(/Source:/)).toBeTruthy();
    expect(screen.getByText(/from vlad's worker/)).toBeTruthy();
    // Labels render.
    expect(screen.getByText("uzi")).toBeTruthy();
    expect(screen.getByText("judge")).toBeTruthy();
  });

  it("shows default_note as an info box while no repo is picked, and quiet once one is", async () => {
    // No default resolved, so the picker starts empty and the note is the loud info box.
    await renderSelectMode({ defaultRepoId: "", defaultNote: "No uzi repo is configured." });
    const note = screen.getByText("No uzi repo is configured.");
    expect(note.className).toContain("border-info");
    // Picking a repo demotes it to the quiet hint.
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "repo1" } });
    expect(screen.getByText("No uzi repo is configured.").className).toContain("text-faint");
  });

  it("blocks Create until a repo is chosen when the server resolved no default", async () => {
    await renderSelectMode({ defaultRepoId: "", defaultNote: "No uzi repo is configured." });
    const create = () => screen.getByRole("button", { name: "Create issue" }) as HTMLButtonElement;
    expect(create().disabled).toBe(true);
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "repo1" } });
    expect(create().disabled).toBe(false);
  });

  it("blocks Create on a whitespace-only title, not merely an empty one", async () => {
    await renderSelectMode();
    const create = () => screen.getByRole("button", { name: "Create issue" }) as HTMLButtonElement;
    expect(create().disabled).toBe(false);
    const title = screen.getByDisplayValue(/Improve the poller/);
    fireEvent.change(title, { target: { value: "   " } });
    expect(create().disabled).toBe(true);
    fireEvent.change(title, { target: { value: "a real title" } });
    expect(create().disabled).toBe(false);
  });

  it("posts the edited values through onCreate", async () => {
    const onCreate = vi.fn().mockResolvedValue(undefined);
    await renderSelectMode({}, onCreate);
    fireEvent.change(screen.getByDisplayValue(/Improve the poller/), { target: { value: "edited title" } });
    fireEvent.click(screen.getByRole("button", { name: "Create issue" }));
    await waitFor(() =>
      expect(onCreate).toHaveBeenCalledWith({
        repoId: "repo1",
        title: "edited title",
        description: seedFixture().description,
        labels: ["uzi", "judge"],
      }),
    );
  });
});

describe("IssueDraftCard — fixed read-only repo mode (Findings)", () => {
  it("renders no repo selector, shows the fixed repo read-only, and Creates without a repo choice", async () => {
    const onCreate = vi.fn().mockResolvedValue(undefined);
    const loadDraft = vi.fn().mockResolvedValue(seedFixture({ defaultRepoId: undefined, defaultNote: undefined }));
    render(
      <IssueDraftCard loadDraft={loadDraft} onCreate={onCreate} onCancel={vi.fn()} fixedRepoLabel="vtmocanu/uzi" />,
    );
    await screen.findByText("Draft issue");
    // No selector at all — the coordinate fixes the repo server-side.
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.getByText("vtmocanu/uzi")).toBeTruthy();
    // Create is enabled from the seeded title with no repo to pick.
    const create = screen.getByRole("button", { name: "Create issue" }) as HTMLButtonElement;
    expect(create.disabled).toBe(false);
    fireEvent.click(create);
    await waitFor(() => expect(onCreate).toHaveBeenCalledTimes(1));
    // repoId is empty in this mode; the finding wiring ignores it.
    expect(onCreate.mock.calls[0][0].repoId).toBe("");
  });
});

describe("IssueDraftCard — the #124 seed strip", () => {
  it("strips Cf from the title and description SEED, keeping newlines", async () => {
    // \u202E is RIGHT-TO-LEFT OVERRIDE, \u200B a ZERO WIDTH SPACE — built from escapes, never
    // pasted raw (they are invisible in review).
    await renderSelectMode({
      title: "Improve the \u202Ereviewer",
      description: "## What the judge found\n\n\u200Bmalicious\u202E line",
    });
    const title = screen.getByDisplayValue(/Improve the/) as HTMLInputElement;
    const body = screen.getByDisplayValue(/What the judge found/) as HTMLTextAreaElement;
    expect(title.value).not.toMatch(/[\p{Cf}]/u);
    expect(body.value).not.toMatch(/[\p{Cf}]/u);
    expect(title.value).toBe("Improve the reviewer");
    // The pre-wrap surface keeps \n (only Cf/control chars are dropped).
    expect(body.value).toContain("\n");
  });
});

describe("IssueDraftCard — recovery paths", () => {
  it("recovers from a failed draft load with Retry", async () => {
    const loadDraft = vi
      .fn()
      .mockRejectedValueOnce(new ApiError(500, "boom"))
      .mockResolvedValueOnce(seedFixture());
    render(<IssueDraftCard loadDraft={loadDraft} onCreate={vi.fn()} onCancel={vi.fn()} repos={repos} />);
    expect(await screen.findByText("boom")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Draft issue")).toBeTruthy();
    await screen.findByDisplayValue(/Improve the poller/);
    expect(loadDraft).toHaveBeenCalledTimes(2);
  });

  it("Cancel on a failed load calls onCancel (no dead end)", async () => {
    const loadDraft = vi.fn().mockRejectedValue(new ApiError(500, "boom"));
    const onCancel = vi.fn();
    render(<IssueDraftCard loadDraft={loadDraft} onCreate={vi.fn()} onCancel={onCancel} repos={repos} />);
    await screen.findByText("boom");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("keeps the card open with edits and an inline error when the forge rejects the create", async () => {
    const onCreate = vi.fn().mockRejectedValue(new ApiError(502, "the forge rejected the request (403)"));
    await renderSelectMode({}, onCreate);
    fireEvent.change(screen.getByDisplayValue(/Improve the poller/), { target: { value: "my edited title" } });
    fireEvent.click(screen.getByRole("button", { name: "Create issue" }));
    expect(await screen.findByText(/the forge rejected the request/i)).toBeTruthy();
    // The edit survives and the card did not collapse.
    expect(screen.getByDisplayValue("my edited title")).toBeTruthy();
    expect(screen.getByText("Draft issue")).toBeTruthy();
  });

  it("shows the optional stale-link warning line when given", async () => {
    const loadDraft = vi.fn().mockResolvedValue(seedFixture());
    render(
      <IssueDraftCard
        loadDraft={loadDraft}
        onCreate={vi.fn()}
        onCancel={vi.fn()}
        repos={repos}
        staleWarning="Filed for an earlier version of this recommendation — re-running the judge changed it since."
      />,
    );
    expect(await screen.findByText(/Filed for an earlier version/)).toBeTruthy();
  });
});
