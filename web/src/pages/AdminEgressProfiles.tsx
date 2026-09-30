// Admin → Site lists (PRD #1906 M1w): the egress profiles an official-sources research run
// may read from. The only place a list is created or edited: the api's writes are
// cookie-only admin routes, so the CLI can list and show but never write.
//
// Server strings (names, descriptions, hosts, warning messages) render as React text nodes
// only; nothing here uses dangerouslySetInnerHTML.

import { useEffect, useRef, useState } from "react";
import { api, type EgressProfile } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { Alert, Badge, Button, Card, EmptyState, ListSkeleton } from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import { ShieldIcon } from "../components/icons";
import { SiteListEditor } from "./egressProfiles/SiteListEditor";

// How many hosts a collapsed row previews before "and N more".
const HOST_PREVIEW = 6;

type Editing = { kind: "create" } | { kind: "edit"; name: string } | null;

export function AdminEgressProfiles() {
  const { data, loading, error: loadError, reload } = useAsyncData<EgressProfile[]>(
    async () => (await api.adminListEgressProfiles()).egress_profiles,
    [],
    { fallback: "Failed to load the site lists" },
  );
  const profiles = data ?? [];
  const [editing, setEditing] = useState<Editing>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const confirmRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (confirmDelete) confirmRef.current?.querySelector<HTMLButtonElement>("button")?.focus();
  }, [confirmDelete]);

  const startCreate = () => {
    setNotice("");
    setError("");
    setConfirmDelete(null);
    setEditing({ kind: "create" });
  };

  const onSaved = async (p: EgressProfile, created: boolean) => {
    setEditing(null);
    setNotice(created ? `Site list ${p.name} created.` : `Changes to ${p.name} saved.`);
    await reload();
  };

  const remove = async (name: string) => {
    setError("");
    setNotice("");
    setDeleting(true);
    try {
      await api.adminDeleteEgressProfile(name);
      setConfirmDelete(null);
      setNotice(`Site list ${name} deleted.`);
      await reload();
    } catch (err) {
      setError(errorMessage(err, "Failed to delete the site list"));
    } finally {
      setDeleting(false);
    }
  };

  return (
    <AdminShell
      description={
        <>
          Site lists name the hosts an official-sources research run may read from. A run names one list, never
          individual hosts. Nothing uses them until the research lane is enabled.
        </>
      }
    >
      {(error || loadError) && <Alert message={error || loadError} />}
      {notice && <Alert tone="success" message={notice} />}

      {editing?.kind === "create" ? (
        <SiteListEditor mode={{ kind: "create" }} onSaved={onSaved} onCancel={() => setEditing(null)} />
      ) : (
        !loading &&
        profiles.length > 0 && (
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="text-sm text-muted">
              {profiles.length === 1 ? "1 site list" : `${profiles.length} site lists`}
            </p>
            <Button onClick={startCreate}>New site list</Button>
          </div>
        )
      )}

      {loading ? (
        <ListSkeleton rows={3} />
      ) : profiles.length === 0 ? (
        editing?.kind !== "create" && (
          <EmptyState
            icon={<ShieldIcon />}
            title="No site lists yet"
            description="Create one with the hosts a research run may read, such as a vendor's documentation site."
            action={<Button onClick={startCreate}>New site list</Button>}
          />
        )
      ) : (
        <ul className="space-y-3" aria-label="Site lists">
          {profiles.map((p) =>
            editing?.kind === "edit" && editing.name === p.name ? (
              <li key={p.id}>
                <SiteListEditor mode={{ kind: "edit", profile: p }} onSaved={onSaved} onCancel={() => setEditing(null)} />
              </li>
            ) : (
              <li key={p.id}>
                <ProfileRow
                  profile={p}
                  confirming={confirmDelete === p.name}
                  deleting={deleting}
                  confirmRef={confirmRef}
                  onEdit={() => {
                    setNotice("");
                    setError("");
                    setConfirmDelete(null);
                    setEditing({ kind: "edit", name: p.name });
                  }}
                  onAskDelete={() => {
                    setNotice("");
                    setConfirmDelete(p.name);
                  }}
                  onCancelDelete={() => setConfirmDelete(null)}
                  onDelete={() => remove(p.name)}
                />
              </li>
            ),
          )}
        </ul>
      )}
    </AdminShell>
  );
}

function ProfileRow({
  profile: p,
  confirming,
  deleting,
  confirmRef,
  onEdit,
  onAskDelete,
  onCancelDelete,
  onDelete,
}: {
  profile: EgressProfile;
  confirming: boolean;
  deleting: boolean;
  confirmRef: React.RefObject<HTMLDivElement | null>;
  onEdit: () => void;
  onAskDelete: () => void;
  onCancelDelete: () => void;
  onDelete: () => void;
}) {
  const overrides = p.warnings.filter((w) => w.code === "multi_publisher_override");
  const inactive = p.warnings.filter((w) => w.code !== "multi_publisher_override");
  const shown = p.hosts.slice(0, HOST_PREVIEW);
  const more = p.hosts.length - shown.length;

  return (
    <Card className="space-y-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <h2 className="[overflow-wrap:anywhere] font-mono text-sm font-semibold text-fg">{p.name}</h2>
          {p.description && <p className="text-sm text-muted">{p.description}</p>}
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          <span className="text-xs text-faint">{p.hosts.length === 1 ? "1 host" : `${p.hosts.length} hosts`}</span>
          {overrides.length > 0 && (
            <Badge tone="warning">
              {overrides.length === 1 ? "1 open host" : `${overrides.length} open hosts`}
            </Badge>
          )}
          {inactive.length > 0 && (
            <Badge tone="danger">
              {inactive.length === 1 ? "1 entry matches nothing" : `${inactive.length} entries match nothing`}
            </Badge>
          )}
          <Button variant="secondary" size="sm" onClick={onEdit} aria-label={`Edit ${p.name}`}>
            Edit
          </Button>
          <Button variant="danger" size="sm" onClick={onAskDelete} aria-label={`Delete ${p.name}`}>
            Delete
          </Button>
        </div>
      </div>

      <ul className="flex flex-wrap gap-1.5" aria-label={`Hosts in ${p.name}`}>
        {shown.map((h) => (
          <li key={h} className="rounded-md border border-edge bg-raised px-2 py-0.5 font-mono text-xs text-fg">
            {h}
          </li>
        ))}
        {more > 0 && <li className="px-1 py-0.5 text-xs text-faint">and {more} more</li>}
      </ul>

      {p.warnings.length > 0 && (
        <ul className="space-y-1 border-t border-edge pt-3 text-xs">
          {p.warnings.map((w, i) => (
            <li key={`${w.code}-${w.entry}-${i}`} className={w.code === "multi_publisher_override" ? "text-warn" : "text-danger"}>
              {w.message}
            </li>
          ))}
        </ul>
      )}

      {confirming && (
        <div
          ref={confirmRef}
          role="group"
          aria-label={`Confirm deleting ${p.name}`}
          className="flex flex-wrap items-center gap-2 rounded-lg border border-danger/40 bg-danger/5 px-3 py-2"
        >
          <p className="mr-auto text-sm text-fg">
            Delete <span className="font-mono">{p.name}</span>? This can't be undone.
          </p>
          <Button variant="dangerSolid" size="sm" onClick={onDelete} disabled={deleting}>
            {deleting ? "Deleting…" : "Delete site list"}
          </Button>
          <Button variant="ghost" size="sm" onClick={onCancelDelete} disabled={deleting}>
            Cancel
          </Button>
        </div>
      )}
    </Card>
  );
}
