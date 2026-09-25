import { useState } from "react";
import type { Repo } from "../lib/api";
import { Button } from "./ui";
import { PlusIcon } from "./icons";
import { useDemoMode } from "../lib/demoMode";
import { maskRepoPath } from "../lib/demoMask";

// AddAnotherRepo — the "Add to another repo" picker a user row's More actions menu opens
// (PRD #1645 D4). Offers ONLY owned repos not already carrying a sibling of this schedule
// (a duplicate would 409 on the server's partial unique index), and calls onAddRepo with
// the chosen repo id. Issue-target schedules never reach it: the menu shows that item
// disabled with its reason instead.
export function AddAnotherRepo({
  name,
  repos,
  taken,
  busy,
  onAddRepo,
}: {
  // Used only to disambiguate the picker's accessible label.
  name: string;
  repos: Repo[];
  // Repo ids already in the group (or the standalone row's own repo) — excluded.
  taken: Set<string>;
  busy: boolean;
  onAddRepo: (repoId: string) => void;
}) {
  const demo = useDemoMode();
  const available = repos.filter((r) => !taken.has(r.id));
  const [repoId, setRepoId] = useState("");
  if (available.length === 0) {
    return <p className="px-1 text-[12px] text-faint">Running on every available repo.</p>;
  }
  return (
    <div className="flex flex-wrap items-center gap-2 px-1 pt-1">
      <span className="text-[12px] text-muted">Add another repo:</span>
      <select
        aria-label={`Add ${name} on another repo`}
        value={repoId}
        onChange={(e) => setRepoId(e.target.value)}
        className="rounded-md border border-edge bg-raised px-2 py-1 font-mono text-[12px] text-fg outline-hidden focus:border-brand/70 disabled:opacity-50"
      >
        <option value="">Choose a repo…</option>
        {available.map((r) => (
          <option key={r.id} value={r.id}>
            {maskRepoPath(r.path_with_namespace, demo)}
          </option>
        ))}
      </select>
      <Button
        size="sm"
        variant="secondary"
        disabled={repoId === "" || busy}
        onClick={() => {
          if (repoId) {
            onAddRepo(repoId);
            setRepoId("");
          }
        }}
      >
        <PlusIcon /> Add
      </Button>
    </div>
  );
}
