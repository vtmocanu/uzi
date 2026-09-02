import { useEffect, useState, type FormEvent } from "react";
import { api, type AppSettings, type Repo, type SettingSource, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Field, SectionTitle } from "../../components/ui";
import { useDemoMode } from "../../lib/demoMode";
import { maskRepoPath } from "../../lib/demoMask";

// parseAllowlist splits the comma-separated repo-id allowlist into a deduped id
// list, dropping empty tokens. normalizeAllowlist canonicalizes for comparison
// (deduped + sorted) so dirty-checking is order/dup-insensitive.
function parseAllowlist(value: string): string[] {
  return value
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}
function normalizeAllowlist(ids: string[]): string {
  return [...new Set(ids)].sort().join(",");
}

// DockerAllowlistCard is the admin surface for the docker-worker repo allowlist
// (PRD #89 M-allow). A docker-capable worker reaches a root Docker daemon, so it may
// only claim runs for repos an admin has explicitly trusted; the gate binds at claim
// time. This is a security control: an EMPTY list is fail-closed (a docker worker
// then claims no repo-bearing run), and non-docker workers are entirely unaffected.
//
// The stored value is a comma-separated list of repo UUIDs, but admins pick repos by
// path — the card resolves paths from the repos API and writes the ids. The repos API
// (`listRepos`) is scoped to the CALLING admin's own repos, and docker_repo_allowlist
// is a GLOBAL setting that can hold repo ids from OTHER admins. So stored ids that do
// not resolve to a repo this admin can see are PRESERVED verbatim on save (surfaced as
// a labeled count), never dropped — otherwise admin A saving would silently clobber
// admin B's allowlisted repo just because A can't see it (auditor Low, PRD #89).
export function DockerAllowlistCard({
  settings,
  sources,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const demo = useDemoMode();
  const [repos, setRepos] = useState<Repo[]>([]);
  // reposLoaded gates the out-of-visibility indicator: until listRepos succeeds we
  // cannot know which stored ids are genuinely outside this admin's visibility vs
  // simply not fetched yet, so the count would be spurious (every id looks "unknown"
  // while repos is empty). reposError distinguishes a failed fetch from a genuinely
  // empty repo list so the copy can differ.
  const [reposLoaded, setReposLoaded] = useState(false);
  const [reposError, setReposError] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(
    () => new Set(parseAllowlist(settings.docker_repo_allowlist)),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = sources["docker_repo_allowlist"] === "env";

  useEffect(() => {
    api
      .listRepos()
      .then(({ repos }) => {
        setRepos(repos);
        setReposLoaded(true);
      })
      .catch(() => setReposError(true));
  }, []);

  const toggle = (id: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  // Selected ids that resolve to no repo THIS admin can see (listRepos is per-user):
  // another admin's allowlisted repo, or a deleted one. Kept in `selected` and written
  // back untouched on save so a global setting is never clobbered by an admin who
  // simply can't see the entry.
  const knownIds = new Set(repos.map((r) => r.id));
  const outsideVisibilityCount = [...selected].filter((id) => !knownIds.has(id)).length;

  const dirty =
    normalizeAllowlist([...selected]) !== normalizeAllowlist(parseAllowlist(settings.docker_repo_allowlist));

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (isEnv) return;
    // Persist EVERY selected id, including ones outside this admin's visibility — a
    // global setting must not be clobbered by an admin who cannot see another admin's
    // repos. Only the checkboxes this admin can see change; the rest ride through.
    const value = normalizeAllowlist([...selected]);
    setBusy(true);
    try {
      const resp = await api.updateSettings({ docker_repo_allowlist: value });
      onSaved(resp);
      setSelected(new Set(parseAllowlist(resp.settings.docker_repo_allowlist)));
      setNotice("Docker worker repo allowlist saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save the docker repo allowlist"));
    } finally {
      setBusy(false);
    }
  };

  const selectedKnown = [...selected].filter((id) => knownIds.has(id)).length;

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Docker worker repo allowlist</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Docker-capable workers reach a root Docker daemon, so they may only run repos you
          explicitly trust here. Only the repos ticked below can be claimed by a docker worker;
          every other repo waits for a non-docker worker.
        </p>
        <p className="mt-2 text-sm text-warn">
          An <strong className="text-fg">empty list is fail-closed</strong> — a docker worker then
          claims no repo-bearing run. Non-docker workers are unaffected by this list.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}
      {isEnv && (
        <Alert tone="info" message="This setting is fixed by an environment variable and cannot be changed here." />
      )}

      <form onSubmit={save} className="space-y-4">
        <Field label={`Trusted repositories (${selectedKnown} selected)`}>
          {reposError ? (
            <p className="text-sm text-warn">
              Could not load repositories. The stored allowlist is preserved unchanged; reload to edit it.
            </p>
          ) : !reposLoaded ? (
            <p className="text-sm text-faint">Loading repositories…</p>
          ) : repos.length === 0 ? (
            <p className="text-sm text-faint">No connected repositories.</p>
          ) : (
            <div className="max-h-64 space-y-1 overflow-y-auto rounded border border-edge p-2">
              {repos.map((r) => (
                <label
                  key={r.id}
                  className="flex cursor-pointer select-none items-center gap-2 rounded px-1 py-1 text-sm hover:bg-raised"
                >
                  <input
                    type="checkbox"
                    checked={selected.has(r.id)}
                    disabled={isEnv}
                    onChange={() => toggle(r.id)}
                    className="h-4 w-4 rounded border-edge accent-brand"
                  />
                  <span className="truncate text-fg">{maskRepoPath(r.path_with_namespace, demo)}</span>
                </label>
              ))}
            </div>
          )}
        </Field>

        {/* Gated on reposLoaded: an unresolved id is only meaningfully "outside your
            visibility" once the repo list actually loaded — during loading or after a
            fetch failure every id looks unknown, which would be a false alarm. Purely
            informational (these ids are preserved on save), so it renders regardless of
            the dirty state without promising any removal. */}
        {reposLoaded && outsideVisibilityCount > 0 && (
          <p className="text-xs text-faint">
            {outsideVisibilityCount} allowlisted repo{outsideVisibilityCount === 1 ? "" : "s"} outside your
            visibility (preserved) — repos on other admins&rsquo; connections, or since removed. They stay in
            the allowlist when you save.
          </p>
        )}

        <Button type="submit" disabled={!dirty || busy || isEnv}>
          {busy ? "Saving…" : "Save repo allowlist"}
        </Button>
      </form>
    </Card>
  );
}
