import { useCallback, useEffect, useRef, useState } from "react";
import { type Repo } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { stripUnsafeChars } from "../../lib/safeText";
import { useDemoMode } from "../../lib/demoMode";
import { maskRepoPath } from "../../lib/demoMask";
import { Alert, Badge, Button, Input, Select, Textarea, cx } from "../ui";
import { FileTextIcon } from "../icons";

// IssueDraftSeed is the normalised draft a surface loads. It flattens the two server drafts
// this card serves — the judge's IssueDraft (default_repo_id + default_note) and the finding's
// IncidentalFindingIssueDraft (no repo, the server resolves it) — so the card renders one shape
// and each surface's loader adapts its own wire into this. title/description are the
// uzi-supplied SEED text (templated from model output) and are the ONLY fields this card
// stripUnsafeChars's; the user's own edits are left alone (they re-run the server sanitiser at
// the POST), and provenance/labels are inert display text.
export interface IssueDraftSeed {
  title: string;
  description: string;
  labels: string[];
  provenance: string;
  // Repo-Select mode only: the server's resolved default repo (empty ⇒ none resolved) and the
  // hint shown as an info box until a repo is picked.
  defaultRepoId?: string;
  defaultNote?: string;
}

// IssueDraftValues is what a Create click sends back to the surface's onCreate. `repoId` is
// meaningful only in repo-Select mode; the finding surface ignores it (the coordinate fixes
// the repo server-side).
export interface IssueDraftValues {
  repoId: string;
  title: string;
  description: string;
  labels: string[];
}

// IssueDraftCard is the single "Draft issue" card for filing a forge issue, serving BOTH
// surfaces the PRD folds together: the judge/run page (a repo `Select`, seeded to the draft's
// default, with default_note as an info box while none is picked) and the Findings page (a
// fixed read-only repo — the finding draft carries no repo). It replaces OccurrenceFileIssue,
// RecommendationFiler and FindingCard's inline editor, which the wiring step deletes.
//
// The card LOADS its draft on mount (the "File issue" button that opens it lives in
// TriageActions), shows Loading / a Retry+Cancel recovery on a failed load, then the editable
// form. Every field is INERT text (never Markdown): the load-bearing sanitiser re-runs
// server-side at the POST, so the card shows raw markdown in an editable control by design.
// A successful create is the surface's cue to refetch and unmount this card (the filed state
// then shows as the "Filed #N ↗" chip, not a green box); a failed create keeps the card open
// with the user's edits intact and an inline error.
export function IssueDraftCard({
  loadDraft,
  onCreate,
  onCancel,
  repos,
  fixedRepoLabel,
  staleWarning,
}: {
  loadDraft: () => Promise<IssueDraftSeed>;
  onCreate: (values: IssueDraftValues) => Promise<void>;
  onCancel: () => void;
  // Present ⇒ repo-Select mode. Absent ⇒ no selector (finding); pair with fixedRepoLabel to
  // show the coordinate's repo read-only.
  repos?: Repo[];
  fixedRepoLabel?: string;
  // Optional "filed for an earlier version of this recommendation" line under the header.
  staleWarning?: string;
}) {
  const demo = useDemoMode();
  const needsRepo = repos !== undefined;

  const [seed, setSeed] = useState<IssueDraftSeed | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadErr, setLoadErr] = useState("");
  const [repoId, setRepoId] = useState("");
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [busy, setBusy] = useState(false);
  const [createErr, setCreateErr] = useState("");

  // loadDraft is a fresh closure each render on the surface side; capture it in a ref so `load`
  // can stay stable (empty deps) and the mount effect fires exactly once, with Retry re-running
  // the current loader.
  const loadRef = useRef(loadDraft);
  loadRef.current = loadDraft;

  const load = useCallback(async () => {
    setLoading(true);
    setLoadErr("");
    try {
      const s = await loadRef.current();
      setSeed(s);
      setRepoId(s.defaultRepoId ?? "");
      // Issue #124 / LOW-1: strip Cf/control chars from what UZI supplies, leave what the USER
      // types alone. These are controlled inputs, so the state IS what gets POSTed — filtering
      // `value=` would silently rewrite the user's own typing. The seed is the boundary that
      // may be cleaned; the server re-sanitises at the POST regardless.
      setTitle(stripUnsafeChars(s.title));
      setDescription(stripUnsafeChars(s.description));
    } catch (e) {
      setLoadErr(errorMessage(e, "Could not load the draft"));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const create = async () => {
    setCreateErr("");
    setBusy(true);
    try {
      await onCreate({ repoId, title, description, labels: seed?.labels ?? [] });
      // Success: the surface refetches and stops rendering this card. Nothing more to do here.
    } catch (e) {
      // The forge rejected the write: keep the card open with the edits intact.
      setCreateErr(errorMessage(e, "Could not file the issue"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="mt-2 overflow-hidden rounded-xl border border-brand/40 bg-brand/[0.06]">
      <div className="flex items-center justify-between gap-2 border-b border-brand/20 bg-brand/10 px-3 py-2">
        <span className="inline-flex items-center gap-1.5 text-xs font-semibold text-brand">
          <span aria-hidden="true">
            <FileTextIcon />
          </span>
          Draft issue
        </span>
        <Badge tone="brand">needs your review</Badge>
      </div>

      <div className="space-y-3 px-3 py-3">
        {staleWarning && <p className="text-xs text-warn">{staleWarning}</p>}

        {loading && (
          <p role="status" className="text-sm text-faint">
            Loading draft…
          </p>
        )}
        {loadErr && <Alert message={loadErr} />}
        {/* A failed load must not trap the card: with no seed there is no Cancel below and no
            way back out, so offer Retry + Cancel here. */}
        {loadErr && !seed && (
          <div className="flex flex-wrap gap-2">
            <Button size="sm" onClick={() => void load()}>
              Retry
            </Button>
            <Button size="sm" variant="secondary" onClick={onCancel}>
              Cancel
            </Button>
          </div>
        )}

        {seed && (
          <>
            {/* Provenance (#68 Decision 8): whose worker produced this attacker-influencable
                text, boxed + labeled so an admin filing another user's draft notices whose text
                they are about to publish. Rendered as escaped inert JSX. */}
            {seed.provenance && (
              <div className="rounded-md border border-edge bg-raised/50 px-2.5 py-1.5 text-xs text-muted">
                <span className="font-semibold text-fg">Source:</span> {seed.provenance}
              </div>
            )}
            {createErr && <Alert message={createErr} />}

            {repos ? (
              <div className="space-y-1">
                <label className="block text-xs text-muted">Repo</label>
                <Select value={repoId} onChange={(e) => setRepoId(e.target.value)}>
                  <option value="">Select a repo…</option>
                  {repos.map((r) => (
                    <option key={r.id} value={r.id}>
                      {maskRepoPath(r.path_with_namespace, demo)}
                    </option>
                  ))}
                </Select>
                {seed.defaultNote && (
                  <p
                    role="status"
                    className={cx(
                      "text-xs",
                      repoId ? "text-faint" : "rounded-md border border-info/40 bg-info/10 px-2.5 py-1.5 text-info",
                    )}
                  >
                    {seed.defaultNote}
                  </p>
                )}
              </div>
            ) : (
              fixedRepoLabel && (
                <div className="space-y-1">
                  <label className="block text-xs text-muted">Repo</label>
                  <div className="rounded-md border border-edge bg-raised/50 px-2.5 py-1.5 text-xs text-muted">
                    {fixedRepoLabel}
                  </div>
                </div>
              )
            )}

            {/* Inert text (never Markdown): the server re-sanitises at the POST boundary. */}
            <div className="space-y-1">
              <label className="block text-xs text-muted">Title</label>
              <Input value={title} onChange={(e) => setTitle(e.target.value)} />
            </div>

            <div className="space-y-1">
              <label className="block text-xs text-muted">Description</label>
              <Textarea
                rows={10}
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                className="max-h-72 font-mono text-xs"
              />
            </div>

            <div className="space-y-1">
              <label className="block text-xs text-muted">Labels</label>
              <div className="flex flex-wrap gap-1">
                {seed.labels.map((l) => (
                  <Badge key={l} tone="neutral">
                    {stripUnsafeChars(l)}
                  </Badge>
                ))}
              </div>
              <p className="text-xs text-faint">
                Lands on the board and is startable without a PRD file. No autopilot label — nothing runs until you click
                Start.
              </p>
            </div>

            <div className="flex flex-wrap gap-2 pt-0.5">
              <Button size="sm" disabled={busy || title.trim() === "" || (needsRepo && !repoId)} onClick={create}>
                Create issue
              </Button>
              <Button size="sm" variant="secondary" disabled={busy} onClick={onCancel}>
                Cancel
              </Button>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
