// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ProductSkillsPanel } from "./ProductSkills";
import { api, type Product, type ProductSkill, type ProductSkills, type User } from "../lib/api";
import { ApiError } from "../lib/apiError";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      adminGetProductSkills: vi.fn(),
      adminSyncProductSkills: vi.fn(),
      adminApplyProductSkills: vi.fn(),
      adminUpdateProduct: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const APPLIED_SHA = "a".repeat(40);
const STAGED_SHA = "0123456789abcdef0123456789abcdef01234567";
// Assembled at runtime so no token-shaped literal sits in tracked source.
const SECRET = "ghp" + "_" + "Zx9".repeat(12);
const RLO = "\u202E"; // RIGHT-TO-LEFT OVERRIDE
const ZWSP = "\u200B"; // ZERO WIDTH SPACE

const product: Product = {
  id: "prod-a",
  name: "Helpdesk assistant",
  description: "",
  enabled: true,
  deleted_at: null,
  created_at: new Date().toISOString(),
  active_token_count: 0,
  allowed_job_types: [],
};

const skill = (name: string, body = `# ${name}`, description = `${name} description`): ProductSkill => ({
  name,
  description,
  body,
});

function view(over: Partial<ProductSkills> = {}, config: Partial<ProductSkills["config"]> = {}): ProductSkills {
  return {
    config: {
      skills_repo_url: "https://github.com/acme/skills",
      skills_ref: "main",
      skills_token_set: true,
      enabled: true,
      ...config,
    },
    applied: {
      sha: APPLIED_SHA,
      applied_at: new Date().toISOString(),
      applied_by: "u-admin",
      skills: [skill("triage"), skill("tone"), skill("legacy")],
    },
    staged: null,
    ...over,
  };
}

function withStaged(): ProductSkills {
  return view({
    staged: {
      sha: STAGED_SHA,
      staged_at: new Date().toISOString(),
      staged_by: "u-other-admin-id",
      skills: [skill("triage"), skill("tone", "# tone v2"), skill("escalate", `go${RLO}back${ZWSP}here`)],
      dropped: [
        { name: "draft", reason: "invalid" },
        { name: "leaky", reason: "secret" },
        { name: "", reason: "over_limit", count: 3 },
      ],
      diff: { added: ["escalate"], changed: ["tone"], removed: ["legacy"], unchanged: ["triage"] },
    },
  });
}

async function openPanel() {
  const utils = render(<ProductSkillsPanel product={product} />);
  const details = utils.container.querySelector("details") as HTMLDetailsElement;
  details.open = true;
  fireEvent(details, new Event("toggle"));
  await waitFor(() => expect(mockApi.adminGetProductSkills).toHaveBeenCalledWith("prod-a"));
  return utils;
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({
    user: { id: "u-admin", is_admin: true } as User,
  } as unknown as ReturnType<typeof useAuth>);
  mockApi.adminGetProductSkills.mockResolvedValue(view());
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ProductSkillsPanel", () => {
  it("loads nothing until opened", () => {
    render(<ProductSkillsPanel product={product} />);
    expect(mockApi.adminGetProductSkills).not.toHaveBeenCalled();
  });

  it("shows the approved set with its sha and approver", async () => {
    await openPanel();
    const section = await screen.findByRole("region", { name: "Approved set" });
    expect(within(section).getByText("triage")).toBeTruthy();
    expect(within(section).getByTitle(APPLIED_SHA).textContent).toContain(APPLIED_SHA.slice(0, 12));
    expect(section.textContent).toMatch(/by you/);
  });

  it("never renders the clone token: set/not set only, a password field cleared after save", async () => {
    mockApi.adminUpdateProduct.mockResolvedValue({ product });
    const { container } = await openPanel();
    const form = await screen.findByRole("form", { name: "Skills source for Helpdesk assistant" });
    expect(within(form).getByText("set")).toBeTruthy();
    const input = within(form).getByLabelText("Replace clone token") as HTMLInputElement;
    expect(input.type).toBe("password");

    // A pasted token often carries a trailing newline or spaces: only the token is sent.
    fireEvent.change(input, { target: { value: `  ${SECRET}\n` } });
    fireEvent.click(within(form).getByRole("button", { name: "Save source" }));

    await waitFor(() =>
      expect(mockApi.adminUpdateProduct).toHaveBeenCalledWith("prod-a", { skills_token: SECRET }),
    );
    await waitFor(() => expect(input.value).toBe(""));
    expect(container.innerHTML).not.toContain(SECRET);
    expect(await screen.findByText(/cannot be shown again/)).toBeTruthy();
    // The save reloads the view (toHaveBeenCalledWith above pinned the exact PATCH body).
    expect(mockApi.adminGetProductSkills).toHaveBeenCalledTimes(2);
  });

  it("removes the token through clear_skills_token, and says not set", async () => {
    mockApi.adminUpdateProduct.mockResolvedValue({ product });
    await openPanel();
    const form = await screen.findByRole("form", { name: /Skills source/ });
    fireEvent.click(within(form).getByRole("button", { name: "Remove token" }));
    expect(within(form).getByText("removed on save")).toBeTruthy();
    mockApi.adminGetProductSkills.mockResolvedValue(view({}, { skills_token_set: false }));
    fireEvent.click(within(form).getByRole("button", { name: "Save source" }));
    await waitFor(() =>
      expect(mockApi.adminUpdateProduct).toHaveBeenCalledWith("prod-a", { clear_skills_token: true }),
    );
    expect(await within(form).findByText("not set")).toBeTruthy();
  });

  it("warns that moving the repo to another host drops the stored token", async () => {
    await openPanel();
    const url = (await screen.findByLabelText("Repo URL")) as HTMLInputElement;
    fireEvent.change(url, { target: { value: "https://gitlab.example.com/acme/skills" } });
    expect(screen.getByText(/saving removes the stored clone token/)).toBeTruthy();
    fireEvent.change(url, { target: { value: "https://github.com/acme/other-skills" } });
    expect(screen.queryByText(/saving removes the stored clone token/)).toBeNull();
    // An unsaved edit blocks the sync: it would read the old source.
    expect((screen.getByRole("button", { name: "Sync from repo" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("clears the repo URL with skills_repo_url set to empty, and says the token goes with it", async () => {
    mockApi.adminUpdateProduct.mockResolvedValue({ product });
    await openPanel();
    const url = (await screen.findByLabelText("Repo URL")) as HTMLInputElement;
    fireEvent.change(url, { target: { value: "" } });
    expect(screen.getByText("Removing the repo URL also removes the stored clone token.")).toBeTruthy();
    mockApi.adminGetProductSkills.mockResolvedValue(view({}, { skills_repo_url: "", skills_token_set: false }));
    fireEvent.click(screen.getByRole("button", { name: "Save source" }));
    await waitFor(() =>
      expect(mockApi.adminUpdateProduct).toHaveBeenCalledWith("prod-a", { skills_repo_url: "" }),
    );
    expect(
      (await screen.findByText(/^Source saved\./)).textContent,
    ).toBe("Source saved. The repo URL was removed. The clone token was removed.");
  });

  it("keeps the two removals available while the feature is off, and nothing that sets a source", async () => {
    mockApi.adminUpdateProduct.mockResolvedValue({ product });
    mockApi.adminGetProductSkills.mockResolvedValue(view({}, { enabled: false }));
    await openPanel();
    const form = await screen.findByRole("form", { name: /Skills source/ });
    const url = within(form).getByLabelText("Repo URL") as HTMLInputElement;
    expect(url.disabled).toBe(true);
    expect((within(form).getByLabelText("Branch, tag or commit") as HTMLInputElement).disabled).toBe(true);
    // No way to paste a new token while off.
    expect(within(form).queryByLabelText("Replace clone token")).toBeNull();
    const syncBtn = within(form).getByRole("button", { name: "Sync from repo" }) as HTMLButtonElement;
    const saveBtn = within(form).getByRole("button", { name: "Save source" }) as HTMLButtonElement;
    expect(syncBtn.disabled).toBe(true);
    expect(saveBtn.disabled).toBe(true);

    const removeToken = within(form).getByRole("button", { name: "Remove token" }) as HTMLButtonElement;
    expect(removeToken.matches(":disabled")).toBe(false);
    fireEvent.click(removeToken);
    fireEvent.click(within(form).getByRole("button", { name: "Remove repo URL" }));
    expect(url.value).toBe("");
    expect(saveBtn.disabled).toBe(false);
    expect(syncBtn.disabled).toBe(true);

    fireEvent.click(saveBtn);
    await waitFor(() =>
      expect(mockApi.adminUpdateProduct).toHaveBeenCalledWith("prod-a", {
        skills_repo_url: "",
        clear_skills_token: true,
      }),
    );
  });

  it("is disabled with an explanation when the instance allowlist is empty", async () => {
    mockApi.adminGetProductSkills.mockResolvedValue(
      view({}, { enabled: false, skills_repo_url: "", skills_token_set: false }),
    );
    await openPanel();
    expect(await screen.findByText(/Product skill sets are off on this instance/)).toBeTruthy();
    expect(screen.getByText("UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS")).toBeTruthy();
    // Disabled through its fieldset, so the property stays false: ask the selector engine.
    expect(screen.getByLabelText("Repo URL").matches(":disabled")).toBe(true);
    expect((screen.getByRole("button", { name: "Sync from repo" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Save source" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("syncs and shows the staged diff, the dropped skills with reasons, and the sha", async () => {
    mockApi.adminSyncProductSkills.mockResolvedValue(withStaged());
    await openPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Sync from repo" }));
    await waitFor(() => expect(mockApi.adminSyncProductSkills).toHaveBeenCalledWith("prod-a"));

    const review = await screen.findByRole("region", { name: "Waiting for approval" });
    expect(within(review).getByTitle(STAGED_SHA)).toBeTruthy();
    expect(within(review).getByTitle(APPLIED_SHA)).toBeTruthy();
    expect(within(review).getByText("Added")).toBeTruthy();
    expect(within(review).getByText("Changed")).toBeTruthy();
    expect(within(review).getByText("Removed")).toBeTruthy();
    expect(within(review).getByText("legacy")).toBeTruthy();
    expect(within(review).getByText(/Unchanged:/).textContent).toContain("triage");
    expect(review.textContent).toMatch(/draft.*not a valid SKILL\.md/);
    expect(review.textContent).toMatch(/leaky.*looks like a credential/);
    expect(review.textContent).toMatch(/3 more files not read/);
    expect(review.textContent).toMatch(/by user u-other-/);
    expect(screen.getByRole("status").textContent).toMatch(/Review the changes below/);
  });

  it("shows hidden characters in skill text as visible markers, never raw", async () => {
    mockApi.adminGetProductSkills.mockResolvedValue(withStaged());
    const { container } = await openPanel();
    const review = await screen.findByRole("region", { name: "Waiting for approval" });
    expect(review.textContent).toContain("U+202E");
    expect(review.textContent).toContain("U+200B");
    expect(container.textContent).not.toContain(RLO);
    expect(container.textContent).not.toContain(ZWSP);
    expect(within(review).getByText("2 hidden characters")).toBeTruthy();
    expect(review.textContent).toMatch(/Check each one before approving/);
  });

  it("approves with expected_sha set to the staged sha", async () => {
    mockApi.adminGetProductSkills.mockResolvedValue(withStaged());
    const after = view({
      applied: { sha: STAGED_SHA, applied_at: new Date().toISOString(), applied_by: "u-admin", skills: withStaged().staged!.skills },
    });
    mockApi.adminApplyProductSkills.mockResolvedValue(after);
    await openPanel();
    fireEvent.click(await screen.findByRole("button", { name: `Approve ${STAGED_SHA.slice(0, 12)}` }));
    await waitFor(() => expect(mockApi.adminApplyProductSkills).toHaveBeenCalledWith("prod-a", STAGED_SHA));
    expect(await screen.findByText(/3 skills now reach this product’s jobs/)).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Waiting for approval" })).toBeNull();
  });

  it("refetches after a 409 on approve so the admin reviews the current staged set", async () => {
    mockApi.adminGetProductSkills.mockResolvedValue(withStaged());
    mockApi.adminApplyProductSkills.mockRejectedValue(
      new ApiError(409, "the staged set changed since you reviewed it; review it again"),
    );
    await openPanel();
    fireEvent.click(await screen.findByRole("button", { name: /^Approve / }));
    expect((await screen.findByRole("alert")).textContent).toBe(
      "The staged set changed since you reviewed it; review it again.",
    );
    await waitFor(() => expect(mockApi.adminGetProductSkills).toHaveBeenCalledTimes(2));
  });

  it.each([
    [409, "this product has no skills repo configured", "This product has no skills repo configured."],
    [409, "the stored clone token cannot be decrypted; set it again", "The stored clone token cannot be decrypted; set it again."],
    [429, "another product skills sync is already running; try again shortly", "Another skills sync is running. Try again in about 30 seconds."],
    [502, "could not read the skills repo (check the URL, ref and token)", "Could not read the skills repo. Check the repo URL, ref and clone token, then sync again."],
  ])("explains a %i on sync", async (status, serverMsg, shown) => {
    mockApi.adminSyncProductSkills.mockRejectedValue(new ApiError(status, serverMsg));
    await openPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Sync from repo" }));
    expect((await screen.findByRole("alert")).textContent).toBe(shown);
  });

  it("counts and shows hidden characters in unchanged names and removed skills, never stripping them", async () => {
    mockApi.adminGetProductSkills.mockResolvedValue(
      view({
        applied: {
          sha: APPLIED_SHA,
          applied_at: new Date().toISOString(),
          applied_by: "u-admin",
          skills: [skill(`tri${ZWSP}age`), skill("legacy", `old${RLO}body`)],
        },
        staged: {
          sha: STAGED_SHA,
          staged_at: new Date().toISOString(),
          staged_by: "u-admin",
          skills: [skill(`tri${ZWSP}age`), skill("fresh")],
          dropped: [],
          diff: { added: ["fresh"], changed: [], removed: ["legacy"], unchanged: [`tri${ZWSP}age`] },
        },
      }),
    );
    const { container } = await openPanel();
    const review = await screen.findByRole("region", { name: "Waiting for approval" });
    // The unchanged name keeps its marker instead of reading as a plain "triage".
    expect(within(review).getByText(/Unchanged:/).textContent).toContain("tri");
    expect(within(review).getByText(/Unchanged:/).textContent).toContain("U+200B");
    // The removed skill is the applied copy, and its body marker is rendered and counted.
    expect(review.textContent).toContain("U+202E");
    expect(review.querySelectorAll("[data-hidden-char]").length).toBe(2);
    expect(review.textContent).toMatch(/This review contains 2 hidden characters/);
    expect(container.textContent).not.toContain(RLO);
    expect(container.textContent).not.toContain(ZWSP);
  });

  it("shows no hidden-character banner on a no-op sync whose unrendered names carry them", async () => {
    mockApi.adminGetProductSkills.mockResolvedValue(
      view({
        staged: {
          sha: STAGED_SHA,
          staged_at: new Date().toISOString(),
          staged_by: "u-admin",
          skills: [skill(`tri${ZWSP}age`)],
          dropped: [],
          diff: { added: [], changed: [], removed: [], unchanged: [`tri${ZWSP}age`] },
        },
      }),
    );
    await openPanel();
    const review = await screen.findByRole("region", { name: "Waiting for approval" });
    expect(review.textContent).toMatch(/Same skills as the approved set/);
    expect(review.querySelectorAll("[data-hidden-char]").length).toBe(0);
    expect(review.textContent).not.toMatch(/hidden character/);
  });

  it("shows the approved description as well as the approved body of a changed skill", async () => {
    mockApi.adminGetProductSkills.mockResolvedValue(
      view({
        applied: {
          sha: APPLIED_SHA,
          applied_at: new Date().toISOString(),
          applied_by: "u-admin",
          skills: [skill("triage"), skill("tone", "# tone", `tone${RLO} description`), skill("legacy")],
        },
        staged: {
          sha: STAGED_SHA,
          staged_at: new Date().toISOString(),
          staged_by: "u-admin",
          skills: [skill("triage"), skill("tone", "# tone v2", "Answer briskly"), skill("legacy")],
          dropped: [],
          diff: { added: [], changed: ["tone"], removed: [], unchanged: ["legacy", "triage"] },
        },
      }),
    );
    await openPanel();
    const review = await screen.findByRole("region", { name: "Waiting for approval" });
    const before = within(review).getByText("Show the approved version").closest("details") as HTMLElement;
    expect(before.textContent).toMatch(/tone.*U\+202E.* description/);
    expect(before.textContent).toContain("# tone");
    // The approved copy is rendered in the review, so its marker is counted there.
    expect(review.textContent).toMatch(/This review contains 1 hidden character,/);
    expect(before.textContent).not.toContain("Answer briskly");
    expect(review.textContent).toContain("Answer briskly");
  });

  it("drops a sync response on close, so a reopen shows the refetched view", async () => {
    mockApi.adminSyncProductSkills.mockResolvedValue(withStaged());
    const { container } = await openPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Sync from repo" }));
    expect(await screen.findByRole("region", { name: "Waiting for approval" })).toBeTruthy();

    // Another admin approved (or a source edit discarded the stage) while this was closed.
    const details = container.querySelector("details") as HTMLDetailsElement;
    details.open = false;
    fireEvent(details, new Event("toggle"));
    details.open = true;
    fireEvent(details, new Event("toggle"));
    await waitFor(() => expect(mockApi.adminGetProductSkills).toHaveBeenCalledTimes(2));
    await screen.findByRole("region", { name: "Approved set" });
    expect(screen.queryByRole("region", { name: "Waiting for approval" })).toBeNull();
    expect(screen.queryByText("Update waiting for approval")).toBeNull();
  });

  it("ignores a sync response that lands after the panel was closed and reloaded", async () => {
    // Each load is a new object, as JSON off the wire is (mockResolvedValue reuses one).
    mockApi.adminGetProductSkills.mockImplementation(() => Promise.resolve(view()));
    let finishSync: (v: ProductSkills) => void = () => {};
    mockApi.adminSyncProductSkills.mockReturnValue(new Promise((r) => (finishSync = r)));
    const { container } = await openPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Sync from repo" }));
    await waitFor(() => expect(mockApi.adminSyncProductSkills).toHaveBeenCalled());

    const details = container.querySelector("details") as HTMLDetailsElement;
    details.open = false;
    fireEvent(details, new Event("toggle"));
    details.open = true;
    fireEvent(details, new Event("toggle"));
    await waitFor(() => expect(mockApi.adminGetProductSkills).toHaveBeenCalledTimes(2));
    await screen.findByRole("region", { name: "Approved set" });

    finishSync(withStaged());
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByRole("region", { name: "Waiting for approval" })).toBeNull();
  });
});
