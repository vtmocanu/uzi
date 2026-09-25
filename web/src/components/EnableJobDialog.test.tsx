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

// A prompt entry: no label check, so Enable is offered as soon as a repo is checked.
const PROMPT = { slug: "docs-hygiene", name: "Docs hygiene", target: "prompt", labels: [] } as unknown as CatalogEntry;

type OnEnable = (repoIds: string[]) => Promise<EnableResult | null>;

function renderDialog(
  { entry = SWEEP, onEnable: impl }: { entry?: CatalogEntry; onEnable?: OnEnable } = {},
) {
  const onEnable = vi.fn<OnEnable>(impl ?? (async (ids) => ({ enabled: ids, failed: [] })));
  const ui = (repos: Repo[]) => (
    <EnableJobDialog
      entry={entry}
      repos={repos}
      enabledRepoIds={new Set()}
      busy={false}
      open
      onOpenChange={() => {}}
      onEnable={onEnable}
    />
  );
  const { rerender } = render(ui(REPOS));
  const dialog = screen.getByRole("dialog", { name: `Enable ${entry.name} on` });
  const primary = () => within(dialog).getByRole("button", { name: /^(Enable \d|Checking labels|Enabling)/ }) as HTMLButtonElement;
  const box = (name: RegExp) => within(dialog).getByRole("checkbox", { name }) as HTMLInputElement;
  return { dialog, primary, box, onEnable, setRepos: (repos: Repo[]) => rerender(ui(repos)) };
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

describe("EnableJobDialog — submit and list changes", () => {
  it("two clicks delivered before the re-render fan out once", async () => {
    let resolve!: (r: EnableResult) => void;
    const { box, primary, onEnable } = renderDialog({
      entry: PROMPT,
      onEnable: () => new Promise<EnableResult>((r) => (resolve = r)),
    });
    fireEvent.click(box(/vtmocanu\/atlas/));
    const btn = primary();
    expect(btn.disabled).toBe(false);
    // Both clicks land in one act, so no re-render disables Enable between them.
    act(() => {
      btn.click();
      btn.click();
    });
    expect(onEnable).toHaveBeenCalledTimes(1);
    expect(onEnable).toHaveBeenCalledWith(["repo-atlas"]);
    await act(async () => resolve({ enabled: ["repo-atlas"], failed: [] }));
  });

  it("a partial failure moves focus from the disabled Enable to the first failed repo's checkbox", async () => {
    const repos = [...REPOS, { id: "repo-www", path_with_namespace: "vtmocanu/www" } as Repo];
    const { box, primary, setRepos } = renderDialog({
      entry: PROMPT,
      onEnable: async () => ({
        enabled: ["repo-uzi"],
        // Reported out of list order: focus follows the list, not the response.
        failed: [
          { repoId: "repo-www", message: "forge unreachable" },
          { repoId: "repo-atlas", message: "forge unreachable" },
        ],
      }),
    });
    setRepos(repos);
    fireEvent.click(box(/vtmocanu\/uzi/));
    fireEvent.click(box(/vtmocanu\/atlas/));
    fireEvent.click(box(/vtmocanu\/www/));
    primary().focus();
    await act(async () => fireEvent.click(primary()));

    const atlas = box(/vtmocanu\/atlas/);
    expect(atlas.disabled).toBe(false);
    expect(document.activeElement).toBe(atlas);
    // Enable still offers the retry of the two failed repos.
    expect(primary().textContent).toBe("Enable 2");
  });

  it("a partial failure focuses the first failed repo the filter still shows, not one it hides", async () => {
    // Enough repos to show the filter, with atlas listed before www.
    const repos = [
      ...REPOS,
      { id: "repo-www", path_with_namespace: "vtmocanu/www" } as Repo,
      ...Array.from({ length: 6 }, (_, i) => ({ id: `repo-x${i}`, path_with_namespace: `vtmocanu/x${i}` }) as Repo),
    ];
    const { dialog, box, primary, setRepos } = renderDialog({
      entry: PROMPT,
      onEnable: async () => ({
        enabled: [],
        failed: [
          { repoId: "repo-atlas", message: "forge unreachable" },
          { repoId: "repo-www", message: "forge unreachable" },
        ],
      }),
    });
    setRepos(repos);
    fireEvent.click(box(/vtmocanu\/atlas/));
    fireEvent.click(box(/vtmocanu\/www/));
    // Filter atlas out of view; it stays selected.
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Filter repos" }), { target: { value: "www" } });
    expect(within(dialog).queryByRole("checkbox", { name: /vtmocanu\/atlas/ })).toBeNull();
    primary().focus();
    await act(async () => fireEvent.click(primary()));

    expect(document.activeElement).toBe(box(/vtmocanu\/www/));
  });

  it("a selected repo that drops out of the list leaves the selection", () => {
    const { box, primary, setRepos } = renderDialog();
    fireEvent.click(box(/vtmocanu\/uzi/));
    act(() => reporters.get("repo-uzi")!("done"));
    fireEvent.click(box(/vtmocanu\/atlas/)); // its check never reports
    expect(primary().textContent).toBe("Checking labels…");

    // A reload no longer lists atlas: it neither counts nor holds Enable.
    setRepos([REPOS[0]]);
    expect(primary().disabled).toBe(false);
    expect(primary().textContent).toBe("Enable 1");

    // Nor does it come back selected when the repo reappears.
    setRepos(REPOS);
    expect(box(/vtmocanu\/atlas/).checked).toBe(false);
    expect(primary().textContent).toBe("Enable 1");
  });
});

// Regression (PR #1662 review): the panel is right-aligned to its button, so a
// first-column card's panel overflows left. The clamp used the viewport's left edge,
// but on desktop the fixed sidebar covers it, so the panel slid under the sidebar
// (measured in a browser: panel 222..542 px, <main> starting at 240). The clamp must
// keep the panel inside the enclosing <main>.
describe("EnableJobDialog — keeps the panel inside the content area", () => {
  const rect = (left: number, right: number) =>
    ({ left, right, top: 0, bottom: 0, width: right - left, height: 0, x: left, y: 0, toJSON: () => ({}) }) as DOMRect;

  afterEach(() => {
    vi.restoreAllMocks();
    delete (HTMLElement.prototype as { offsetWidth?: number }).offsetWidth;
    delete (document.documentElement as { clientWidth?: number }).clientWidth;
  });

  it("shifts a first-column panel right so it clears the sidebar", () => {
    Object.defineProperty(document.documentElement, "clientWidth", { configurable: true, value: 1440 });
    Object.defineProperty(HTMLElement.prototype, "offsetWidth", {
      configurable: true,
      get(this: HTMLElement) {
        return this.getAttribute("role") === "dialog" ? 320 : 0;
      },
    });
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      if (this.tagName === "MAIN") return rect(240, 1440);
      // The dialog's host: the positioned wrapper around the "Enable on…" button.
      if (this.querySelector(":scope > button") && this.classList.contains("inline-block")) return rect(460, 541);
      return rect(0, 0);
    });
    render(
      <main>
        <EnableJobDialog
          entry={PROMPT}
          repos={REPOS}
          enabledRepoIds={new Set()}
          busy={false}
          open
          onOpenChange={() => {}}
          onEnable={async (ids) => ({ enabled: ids, failed: [] })}
        />
      </main>,
    );
    const dialog = screen.getByRole("dialog", { name: `Enable ${PROMPT.name} on` });
    // Natural left = 541 - 320 = 221; the content area starts at 240 + 8 px margin = 248.
    expect(dialog.style.transform).toBe("translateX(27px)");
  });
});
