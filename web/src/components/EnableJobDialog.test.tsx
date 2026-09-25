// @vitest-environment jsdom
//
// EnableJobDialog's OWN pre-enable guard (PRD #1645 D7), isolated from SweepLabelWarn.
// The real warn reports "checking" from its first effect, which would hold Enable even
// if the dialog did not; here it is stubbed so it reports nothing until the test says
// so, leaving the dialog's own state as the only thing that can hold Enable. The
// end-to-end behaviour with the real warn is in JobCatalog.test.tsx.
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { EnableJobDialog, type EnableResult } from "./EnableJobDialog";
import type { CatalogEntry, Repo } from "../lib/api";

// repoId -> the dialog's onCheckStateChange for that repo's (stubbed) warn.
const reporters = new Map<string, (state: "checking" | "done") => void>();

vi.mock("./SweepLabelWarn", () => ({
  SweepLabelWarn: ({ repoId, onCheckStateChange }: { repoId: string; onCheckStateChange?: (s: "checking" | "done") => void }) => {
    if (onCheckStateChange) reporters.set(repoId, onCheckStateChange);
    return null;
  },
}));

afterEach(() => {
  cleanup();
  reporters.clear();
});

const REPOS: Repo[] = [
  { id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" } as Repo,
  { id: "repo-atlas", path_with_namespace: "vtmocanu/atlas" } as Repo,
];

const SWEEP = {
  slug: "bug-triage",
  name: "Bug triage sweep",
  target: "sweep",
  labels: ["bug"],
} as CatalogEntry;

function renderDialog() {
  const onEnable = vi.fn<(repoIds: string[]) => Promise<EnableResult | null>>(async (ids) => ({ enabled: ids, failed: [] }));
  render(
    <EnableJobDialog
      entry={SWEEP}
      repos={REPOS}
      enabledRepoIds={new Set()}
      busy={false}
      open
      onOpenChange={() => {}}
      onEnable={onEnable}
    />,
  );
  const dialog = screen.getByRole("dialog", { name: "Enable Bug triage sweep on" });
  const primary = () => within(dialog).getByRole("button", { name: /^(Enable \d|Checking labels|Enabling)/ }) as HTMLButtonElement;
  return { dialog, primary, onEnable };
}

describe("EnableJobDialog — its own label-check guard, with the warn silent", () => {
  it("holds Enable on the render right after a sweep repo is checked, before the warn reports anything", async () => {
    const { dialog, primary, onEnable } = renderDialog();
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/atlas/ }));

    // The stub mounted but has not reported: only the dialog can be holding Enable.
    expect(reporters.has("repo-atlas")).toBe(true);
    expect(primary().disabled).toBe(true);
    expect(primary().textContent).toBe("Checking labels…");
    fireEvent.click(primary());
    expect(onEnable).not.toHaveBeenCalled();

    // Once the warn reports the check settled, Enable is offered and fans out.
    act(() => reporters.get("repo-atlas")!("done"));
    expect(primary().disabled).toBe(false);
    expect(primary().textContent).toBe("Enable 1");
    await act(async () => fireEvent.click(primary()));
    expect(onEnable).toHaveBeenCalledWith(["repo-atlas"]);
  });

  it("a repo unchecked and checked again is held again until its new check reports", () => {
    const { dialog, primary } = renderDialog();
    const atlas = within(dialog).getByRole("checkbox", { name: /vtmocanu\/atlas/ });
    fireEvent.click(atlas);
    act(() => reporters.get("repo-atlas")!("done"));
    expect(primary().disabled).toBe(false);

    fireEvent.click(atlas); // off
    fireEvent.click(atlas); // on again: the earlier "done" must not carry over
    expect(primary().disabled).toBe(true);
    expect(primary().textContent).toBe("Checking labels…");
  });
});
