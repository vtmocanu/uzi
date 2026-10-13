// @vitest-environment jsdom
// PRD #2170 M3: the mock PATCH for the code cross-check owner inputs mirrors
// PatchRepo — its own exclusive group, an 8192-byte guidance cap, null clears.
import { afterEach, beforeEach, expect, it, vi } from "vitest";

beforeEach(() => {
  window.localStorage.clear();
  vi.resetModules();
});
afterEach(() => vi.resetModules());

it("sets, caps and clears the guidance and the checker CLAUDE.md consent", async () => {
  const { mockApi } = await import("./mockApi");
  const set = await mockApi.setRepoCodeCrossCheckInputs("repo-atlas", {
    code_cross_check_guidance: "é".repeat(4096),
    cross_check_repo_instructions_enabled: true,
  });
  expect(set.repo.code_cross_check_guidance).toBe("é".repeat(4096));
  expect(set.repo.cross_check_repo_instructions_enabled).toBe(true);

  await expect(
    mockApi.setRepoCodeCrossCheckInputs("repo-atlas", { code_cross_check_guidance: "é".repeat(4096) + "a" }),
  ).rejects.toMatchObject({ status: 400, message: "invalid code cross-check guidance" });

  // Omitted fields are untouched; null clears.
  const toggled = await mockApi.setRepoCodeCrossCheckInputs("repo-atlas", { cross_check_repo_instructions_enabled: false });
  expect(toggled.repo.code_cross_check_guidance).toBe("é".repeat(4096));
  const cleared = await mockApi.setRepoCodeCrossCheckInputs("repo-atlas", { code_cross_check_guidance: null });
  expect(cleared.repo.code_cross_check_guidance).toBeNull();
  expect(cleared.repo.cross_check_repo_instructions_enabled).toBe(false);

  // A lone surrogate is accepted: the real server decodes the JSON escape to U+FFFD and returns 200.
  await expect(
    mockApi.setRepoCodeCrossCheckInputs("repo-atlas", { code_cross_check_guidance: "bad \uD800 half" }),
  ).resolves.toMatchObject({ repo: { id: "repo-atlas" } });
});

it("refuses another group's field and an empty request", async () => {
  const { mockApi } = await import("./mockApi");
  const mixed = { cross_check_repo_instructions_enabled: true, repo_skills_enabled: true } as Parameters<
    typeof mockApi.setRepoCodeCrossCheckInputs
  >[1];
  await expect(mockApi.setRepoCodeCrossCheckInputs("repo-atlas", mixed)).rejects.toMatchObject({
    status: 400,
    message: expect.stringContaining("cannot be combined"),
  });
  await expect(mockApi.setRepoCodeCrossCheckInputs("repo-atlas", {})).rejects.toMatchObject({ status: 400 });
  await expect(
    mockApi.setRepoCodeCrossCheckInputs("missing", { cross_check_repo_instructions_enabled: true }),
  ).rejects.toMatchObject({ status: 404 });
});
