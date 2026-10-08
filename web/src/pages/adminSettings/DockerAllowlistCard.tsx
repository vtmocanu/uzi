import { useEffect, useState, type FormEvent } from "react";
import { api, type AppSettings, type AdminDockerAllowlistRepo, type SettingSource, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, SectionTitle } from "../../components/ui";
import { useDemoMode } from "../../lib/demoMode";
import { maskEmail, maskHost, maskRepoPath } from "../../lib/demoMask";

// Match google/uuid.Parse v1.6.0, including arbitrary single-byte wrappers.
// ASCII wrapper checks account for Go's UTF-8 byte length versus JS's UTF-16 length.
function uuidIdentity(value: string): string | null {
  let s = value;
  if (s.length === 45 && /^urn:uuid:/i.test(s)) s = s.slice(9);
  else if (s.length === 38 && s.charCodeAt(0) <= 0x7f && s.charCodeAt(37) <= 0x7f) s = s.slice(1, 37);
  if (/^[0-9a-f]{32}$/i.test(s)) {
    s = s.slice(0, 8) + "-" + s.slice(8, 12) + "-" + s.slice(12, 16) + "-" + s.slice(16, 20) + "-" + s.slice(20);
  }
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(s) ? s.toLowerCase() : null;
}

// Cross-user trust labels must expose hidden controls instead of allowing them
// to reorder the identity an admin sees. This affects presentation, never IDs.
function displayLabel(value: string): string {
  return value.replace(/[\p{Cc}\p{Cf}]/gu, (char) =>
    "\\u{" + char.codePointAt(0)!.toString(16) + "}");
}

function parseAllowlist(value: string): string[] {
  // strings.TrimSpace uses Unicode White_Space; JS trim also strips BOM and misses NEL.
  return value.split(",").map((s) => s.replace(/^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/g, "")).filter(Boolean);
}
function identity(value: string): string {
  return uuidIdentity(value) ?? value;
}
// Keep the first stored spelling of each identity, including unresolved tokens.
function chosenSpellings(ids: Iterable<string>): string[] {
  const chosen = new Map<string, string>();
  for (const id of ids) if (!chosen.has(identity(id))) chosen.set(identity(id), id);
  return [...chosen.values()].sort();
}
function normalizeAllowlist(ids: Iterable<string>): string {
  return [...new Set([...ids].map(identity))].sort().join(",");
}

// Writes start from the complete stored set, never just the fetched rows.
// Deleted/unresolved IDs remain intact; revocation removes every UUID spelling.
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
  const [repos, setRepos] = useState<AdminDockerAllowlistRepo[]>([]);
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
      .adminListDockerAllowlistRepos()
      .then(({ repos }) => {
        setRepos(repos);
        setReposLoaded(true);
      })
      .catch(() => setReposError(true));
  }, []);

  const locked = busy || isEnv || !reposLoaded;
  const selectedIds = new Set([...selected].map(identity));
  const toggle = (id: string) => {
    if (locked) return;
    setSelected((prev) => {
      const next = new Set(prev);
      const key = identity(id);
      if ([...prev].some((token) => identity(token) === key)) {
        for (const token of prev) if (identity(token) === key) next.delete(token);
      } else next.add(uuidIdentity(id) ?? id);
      return next;
    });
  };

  const knownIds = new Set(repos.map((r) => identity(r.id)));
  const unresolvedCount = [...selectedIds].filter((id) => !knownIds.has(id)).length;

  const dirty =
    normalizeAllowlist([...selected]) !== normalizeAllowlist(parseAllowlist(settings.docker_repo_allowlist));

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (locked || !dirty) return;
    setError("");
    setNotice("");
    const value = chosenSpellings(selected).join(",");
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

  const selectedKnown = [...selectedIds].filter((id) => knownIds.has(id)).length;

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
        <fieldset className="space-y-1.5">
          <legend className="text-sm font-medium text-muted">Trusted repositories ({selectedKnown} selected)</legend>
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
                    checked={selectedIds.has(identity(r.id))}
                    disabled={locked}
                    onChange={() => toggle(r.id)}
                    className="h-4 w-4 rounded border-edge accent-brand"
                  />
                  <span className="truncate text-fg">{maskRepoPath(displayLabel(r.path_with_namespace), demo)}</span>{" "}
                  <span className="text-muted">{maskEmail(displayLabel(r.owner_email), demo)} · {r.forge_type} · {maskHost(displayLabel(r.base_url), demo)}</span>
                  {!r.enabled && <span className="text-faint">Disabled</span>}
                </label>
              ))}
            </div>
          )}
        </fieldset>

        {reposLoaded && unresolvedCount > 0 && (
          <p className="text-xs text-faint">
            {unresolvedCount} unresolved/deleted repositories (preserved) — they stay in the allowlist when you save.
          </p>
        )}

        <Button type="submit" disabled={!dirty || locked}>
          {busy ? "Saving…" : "Save repo allowlist"}
        </Button>
      </form>
    </Card>
  );
}
