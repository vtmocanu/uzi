// @vitest-environment jsdom
//
// EnableJobDialog tells each repo's label warning whether that repo is linked to an issue
// tracker (PRD 4173 M6), so the warning can drop its create-labels action.
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { EnableJobDialog } from "./EnableJobDialog";
import type { CatalogEntry, Repo } from "../lib/api";

const seen = new Map<string, boolean | undefined>();

vi.mock("./SweepLabelWarn", () => ({
  SweepLabelWarn: ({ repoId, linked }: { repoId: string; linked?: boolean }) => {
    seen.set(repoId, linked);
    return null;
  },
}));

afterEach(() => {
  cleanup();
  seen.clear();
});

const LINK = {
  connection_id: "itc-1",
  project_id: "0-4",
  project_short_name: "ATL",
  tag_id: "6-2",
  tag_name: "uzi",
  linked_at: "2026-11-19T10:00:00Z",
};

it("passes linked only for the repo linked to an issue tracker", () => {
  const repos = [
    { id: "repo-plain", path_with_namespace: "acme/plain" },
    { id: "repo-linked", path_with_namespace: "acme/linked", issue_tracker_link: LINK },
  ] as Repo[];
  const entry = { slug: "bug-triage", name: "Bug triage sweep", target: "sweep", labels: ["bug"] } as CatalogEntry;
  render(
    <EnableJobDialog
      entry={entry}
      repos={repos}
      enabledRepoIds={new Set()}
      busy={false}
      open
      onOpenChange={() => {}}
      onEnable={async (ids) => ({ enabled: ids, failed: [] })}
    />,
  );
  const dialog = screen.getByRole("dialog", { name: "Enable Bug triage sweep on" });
  fireEvent.click(within(dialog).getByRole("checkbox", { name: /acme\/plain/ }));
  fireEvent.click(within(dialog).getByRole("checkbox", { name: /acme\/linked/ }));
  expect(seen.get("repo-plain")).toBe(false);
  expect(seen.get("repo-linked")).toBe(true);
});
