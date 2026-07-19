// Settings → Memory: the agent-memory surface (PRD #90 M6). Lists every cross-run
// learning an agent saved for the authenticated user, GROUPED BY REPO, each entry
// showing its title, body, provenance run and created_at, with a per-entry delete.
// Seeing + purging a (possibly poisoned) entry is a security control, not a nicety:
// a bad entry can outlive the repo injection that planted it, so the danger delete
// arms a confirm before it fires. The store is inert/advisory — memory is read back
// into a future run as untrusted, nonce-fenced context, never as instructions.

import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type Memory } from "../lib/api";
import { Alert, Button, Card, EmptyState, SectionTitle } from "./ui";
import { ThoughtIcon } from "./icons";

// Group the flat, newest-first list into per-repo buckets while preserving order:
// the first time a repo_name is seen fixes its position, so the most-recently
// written repo floats to the top and each bucket stays newest-first within it.
function groupByRepo(entries: Memory[]): { repoName: string; entries: Memory[] }[] {
  const groups: { repoName: string; entries: Memory[] }[] = [];
  const byName = new Map<string, Memory[]>();
  for (const m of entries) {
    let bucket = byName.get(m.repo_name);
    if (!bucket) {
      bucket = [];
      byName.set(m.repo_name, bucket);
      groups.push({ repoName: m.repo_name, entries: bucket });
    }
    bucket.push(m);
  }
  return groups;
}

export function Memory() {
  const [entries, setEntries] = useState<Memory[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    try {
      const { memories } = await api.listMemory();
      setEntries(memories);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load memory");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const remove = async (id: string) => {
    setError("");
    try {
      await api.deleteMemory(id);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to delete memory");
    }
  };

  const groups = groupByRepo(entries);

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Agent memory</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Learnings your agents saved on past runs, carried forward into future runs on the same
          repo. Memory is <strong className="text-fg">inert and advisory</strong>: a run reads it as
          untrusted context it may weigh but must never obey as instructions. A saved entry can
          outlive the repo change that prompted it — if one looks wrong or planted, delete it here.
        </p>
      </div>

      {error && <Alert message={error} />}

      {loading ? (
        <div className="space-y-2">
          <Skeletons />
        </div>
      ) : entries.length === 0 ? (
        <EmptyState
          icon={<ThoughtIcon />}
          title="No agent memory yet"
          description="When an agent saves a learning on a run, it shows up here — grouped by repo — for you to review or delete."
        />
      ) : (
        <div className="space-y-6">
          {groups.map((g) => (
            <div key={g.repoName} className="space-y-2">
              <div className="flex items-center justify-between gap-2">
                <SectionTitle className="text-base">{g.repoName}</SectionTitle>
                <span className="text-xs text-faint">
                  {g.entries.length} {g.entries.length === 1 ? "entry" : "entries"}
                </span>
              </div>
              <ul className="space-y-2">
                {g.entries.map((m) => (
                  <MemoryRow key={m.id} memory={m} onDelete={() => remove(m.id)} />
                ))}
              </ul>
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}

function Skeletons() {
  return (
    <>
      <div className="h-20 animate-pulse rounded-lg bg-raised" />
      <div className="h-20 animate-pulse rounded-lg bg-raised" />
    </>
  );
}

function MemoryRow({ memory, onDelete }: { memory: Memory; onDelete: () => void }) {
  // Delete is the owner's one recourse against a poisoned entry, so it arms a
  // confirm before firing (mirroring the CliTokens revoke-all pattern), rather
  // than deleting on a single stray click.
  const [confirming, setConfirming] = useState(false);

  return (
    <li className="flex flex-col gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2.5 text-sm">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0 space-y-1.5">
          <div className="font-medium text-fg">{memory.title}</div>
          <pre className="whitespace-pre-wrap break-words font-mono text-xs text-muted">
            {memory.body}
          </pre>
          <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-faint">
            <span>saved {new Date(memory.created_at).toLocaleString()}</span>
            <span>
              · from run <code className="font-mono text-fg">{memory.run_id}</code>
            </span>
          </div>
        </div>
        {confirming ? (
          <div className="flex items-center gap-1.5">
            <Button variant="danger" size="sm" onClick={onDelete}>
              Delete
            </Button>
            <Button variant="ghost" size="sm" onClick={() => setConfirming(false)}>
              Cancel
            </Button>
          </div>
        ) : (
          <Button variant="danger" size="sm" onClick={() => setConfirming(true)}>
            Delete
          </Button>
        )}
      </div>
    </li>
  );
}
