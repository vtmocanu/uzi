// Admin → Shared providers (PRD #4324): Claude or Codex gateway definitions every user can
// pick as their source for that engine, with their OWN key. The admin fixes the engine,
// endpoint and auth style at creation and may later change only the name and model map;
// there is no key here at all.
// Every write is a cookie-only admin route, so the CLI can list and show but never write.
//
// A Claude definition may instead be organization-funded (PRD #4540, credential_mode
// "operator_key"): the admin sets one key that every granted user's runs receive. The create
// form then shows six disclosures the admin must acknowledge, and the card carries the key
// controls (set or replace, enable or disable, delete, Test). The key is typed into a password
// input, cleared after every attempt and never rendered back; the server never returns it.
//
// The page answers two questions per definition: what is fixed (endpoint, auth style: a
// locked block, never an input once created) and who depends on it (how many users selected
// it, how many personal keys are attached). Delete is guarded on both: its confirmation says
// how many keys go with it and, while anyone has it selected, leads with the refusal instead.
// Every confirmation opens with focus on Cancel.
//
// Server strings (names, URLs, model names) render as React text nodes only; nothing here
// uses dangerouslySetInnerHTML.

import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { api, type AdminSharedProvider, type ProviderAuthStyle, type ProviderDTO, type ProviderModelMap, type ProviderProbe, type User } from "../lib/api";
import { ApiError, errorMessage } from "../lib/apiError";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { useAsyncData } from "../lib/useAsyncData";
import { Alert, Badge, Button, Card, EmptyState, Field, Input, ListSkeleton, Select, Toggle } from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import { KeyIcon, LockIcon } from "../components/icons";
import {
  AUTH_LABELS,
  AuthStyleSelect,
  BaseUrlSelect,
  buildModelMap,
  emptyRows,
  MappedModels,
  ModelMapFields,
  rowsFromMap,
  sameModelMap,
  validName,
  type ModelRows,
} from "../components/providerFields";

type Engine = ProviderDTO["engine"];

const ENGINE_LABELS: Record<Engine, string> = { claude: "Claude", codex: "Codex" };
const PROTOCOL_LABELS: Record<Engine, string> = { claude: "Anthropic Messages", codex: "OpenAI Responses" };

const REASONS: Record<string, (engine: Engine) => string> = {
  provider_name_taken: () => "A shared provider with that name already exists. Shared names are unique regardless of case.",
  provider_limit_reached: () => "This instance already has 50 shared providers, retired ones included. Delete one first.",
  provider_in_use_as_source: (engine) =>
    `Delete refused: at least one user still has this definition selected as their ${ENGINE_LABELS[engine]} source. They must choose another source first.`,
  provider_retired: () => "This shared provider is retired and can no longer be edited.",
  provider_base_url_not_allowed: () => "That base URL is not on the operator's allowlist. Pick one from the list.",
  provider_not_operator_key: () =>
    "This definition is not organization-funded: each user attaches their own key to it, so there is no organization key to manage.",
  provider_probe_rate_limited: () => "Tests are rate limited. Wait a moment, then test again.",
  provider_grant_cap_reached: () =>
    "This definition already has 10,000 named recipients, the limit. Remove one first, or give access to all users instead.",
  user_not_found: () => "That user no longer exists, so access was not granted. The user list has been refreshed; pick another.",
};

// Default-deny (PRD #4540 M2): an organization-funded definition has no recipients until the
// admin grants access, and every granted user's runs receive the one organization key.
const DENY_COPY = "No user can see or use this definition until you grant access; granted users' runs receive the organization key.";

// The server's stale-Test 409 carries no reason code, only its message.
const STALE_TEST_COPY = "The provider or key changed while testing; test again.";

function sharedError(err: unknown, fallback: string, engine: Engine): string {
  const reason = err instanceof ApiError ? (err.body as { reason?: string } | null)?.reason : undefined;
  if (err instanceof ApiError && err.status === 409 && !reason && /changed while it was being tested/.test(err.message)) {
    return STALE_TEST_COPY;
  }
  return (reason && REASONS[reason]?.(engine)) || errorMessage(err, fallback);
}

// The six things an admin accepts before an organization key exists (PRD #4540). Each is one
// list item with its own test id, so a test can pin them one by one.
const OPERATOR_DISCLOSURES: { id: string; text: string }[] = [
  {
    id: "disclosure-copy",
    text: "Granted users, and code in their runs, receive the key and can copy it. Revoking access in uzi does not take copies back; only rotating the key at the gateway revokes them. Rotate at the gateway when access ends, and set per-key spend limits there.",
  },
  {
    id: "disclosure-redirect",
    text: "A redirecting gateway can forward request bodies to the redirect host, and can send the key to a same-host sibling path, or to another host with the x_api_key auth style. Bearer is recommended.",
  },
  {
    id: "disclosure-product-token",
    text: "Jobs created through an external product's job token for a granted user can spend the key.",
  },
  {
    id: "disclosure-shared-identity",
    text: "One gateway key is a shared identity: requests, logs or batches the gateway stores may expose recipients' work to one another.",
  },
  {
    id: "disclosure-secret-key",
    text: "Rotating UZI_SECRET_KEY invalidates the stored key; you must set it again.",
  },
  {
    id: "disclosure-authorized-users",
    text: "Use is restricted to the organization's authorized users.",
  },
];

const AUTH_RESULT_TEXT: Record<ProviderProbe["auth_result"], string> = {
  ok: "Authentication worked",
  rejected: "Authentication rejected by the gateway",
  inconclusive: "Authentication check inconclusive",
};
const TOOL_RESULT_TEXT: Record<NonNullable<ProviderProbe["tool_result"]>, string> = {
  passed: "Tool use passed",
  not_supported: "Tool use not supported by the gateway",
  inconclusive: "Tool use check inconclusive",
  not_run: "Tool use check not run",
};

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

type Confirm = { kind: "retire" | "delete"; id: string } | null;

interface PageData {
  providers: AdminSharedProvider[];
  // From the admin's own /me/providers read: the operator allowlist. null when it could not be read.
  enabled: boolean | null;
  allowed: string[];
}

export function AdminSharedProviders() {
  const { data, loading, error: loadError, reload } = useAsyncData<PageData>(
    async () => {
      const [shared, mine] = await Promise.all([
        api.adminListSharedProviders(),
        api.listProviders().catch(() => null),
      ]);
      return { providers: shared.providers, enabled: mine?.enabled ?? null, allowed: mine?.allowed_base_urls ?? [] };
    },
    [],
    { fallback: "Failed to load the shared providers" },
  );
  // Authoritative server answers to key writes, shown over the loaded list until a list load
  // succeeds (a new `data`), so a failed refresh never leaves the pre-write state on screen.
  const [applied, setApplied] = useState<Record<string, AdminSharedProvider>>({});
  useEffect(() => { setApplied({}); }, [data]);
  const providers = (data?.providers ?? []).map((p) => applied[p.id] ?? p);
  const apply = (p: AdminSharedProvider) => setApplied((prev) => ({ ...prev, [p.id]: p }));
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<Confirm>(null);
  const [notice, setNotice] = useState("");
  // The admin user list, read once when an organization-funded card is on screen: grant rows carry
  // only a user id, and the picker offers these users. undefined = not read yet, null = read failed.
  const hasOperator = providers.some((p) => p.credential_mode === "operator_key");
  const [users, setUsers] = useState<User[] | null | undefined>(undefined);
  const loadUsers = useCallback(async () => {
    try {
      setUsers((await api.listUsers()).users);
    } catch {
      setUsers(null);
    }
  }, []);
  useEffect(() => {
    if (hasOperator && users === undefined) void loadUsers();
  }, [hasOperator, users, loadUsers]);

  const reset = () => {
    setNotice("");
    setCreating(false);
    setEditing(null);
    setConfirm(null);
  };
  const done = async (message: string) => {
    reset();
    setNotice(message);
    await reload();
  };
  const canCreate = data?.enabled !== false && (data?.allowed.length ?? 0) > 0;
  const newButton = (
    <Button onClick={() => { reset(); setCreating(true); }} disabled={!canCreate}>
      New shared provider
    </Button>
  );

  return (
    <AdminShell
      description={
        <>
          Shared providers are Claude or Codex gateways any user can pick as their source for that engine. You fix the
          engine, endpoint and auth style and map the models. With a per-user key, each user attaches their own key, which
          you never see. With an organization key, you set one key for the definition and choose which users can use it.
        </>
      }
    >
      {loadError && <Alert message={loadError} />}
      {notice && <Alert tone="success" message={notice} />}
      {data?.enabled === false && (
        <p className="rounded-lg border border-edge bg-raised/60 px-4 py-3 text-sm text-muted">
          Custom providers are off, so no definition can be created and users cannot select one. The operator enables
          them by setting <code className="rounded bg-raised px-1 py-0.5 text-fg">UZI_PROVIDER_ALLOWED_BASE_URLS</code>.
        </p>
      )}
      {data && data.enabled === null && (
        <p className="text-sm text-warn">The base URL allowlist could not be read, so a new definition cannot be created right now.</p>
      )}

      {creating ? (
        <CreateForm allowed={data?.allowed ?? []} onCancel={() => setCreating(false)} onCreated={(p) => done(
          p.credential_mode === "operator_key"
            ? `Shared provider ${sanitizeLabel(p.name)} created. Set its organization key and grant access next: no user can see or use it until you do.`
            : `Shared provider ${sanitizeLabel(p.name)} created. Users can now attach their own key to it.`,
        )} />
      ) : (
        !loading &&
        providers.length > 0 && (
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="text-sm text-muted">{plural(providers.length, "shared provider", "shared providers")}</p>
            {newButton}
          </div>
        )
      )}

      {loading ? (
        <ListSkeleton rows={2} />
      ) : providers.length === 0 ? (
        !creating && (
          <EmptyState
            icon={<KeyIcon />}
            title="No shared providers yet"
            description="Create one so users can run Claude or Codex through your gateway with a key of their own."
            action={newButton}
          />
        )
      ) : (
        <ul className="space-y-3" aria-label="Shared providers">
          {providers.map((p) => (
            <li key={p.id}>
              {editing === p.id ? (
                <EditForm provider={p} onCancel={() => setEditing(null)} onSaved={(n) => done(`Changes to ${sanitizeLabel(n.name)} saved.`)} />
              ) : (
                <ProviderCard
                  provider={p}
                  confirm={confirm?.id === p.id ? confirm.kind : null}
                  onEdit={() => { reset(); setEditing(p.id); }}
                  onAsk={(kind) => { setNotice(""); setConfirm({ kind, id: p.id }); }}
                  onCancel={() => setConfirm(null)}
                  onDone={done}
                  onApply={apply}
                  onStale={reload}
                  users={users}
                  onUsersStale={loadUsers}
                />
              )}
            </li>
          ))}
        </ul>
      )}
    </AdminShell>
  );
}

// FixedFacts is the locked block: endpoint and auth style, which no edit can change.
function FixedFacts({ baseUrl, auth }: { baseUrl: string; auth: ProviderAuthStyle }) {
  return (
    <div className="rounded-lg border border-edge bg-raised/50 px-3 py-2" data-testid="fixed-facts">
      <p className="flex items-center gap-1.5 text-xs text-faint">
        <LockIcon className="h-3.5 w-3.5" aria-hidden="true" />
        Fixed at creation
      </p>
      <dl className="mt-1 grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[7rem_1fr]">
        <dt className="text-muted">Endpoint</dt>
        <dd className="break-all"><code>{sanitizeLabel(baseUrl)}</code></dd>
        <dt className="text-muted">Auth style</dt>
        <dd>{AUTH_LABELS[auth]}</dd>
      </dl>
    </div>
  );
}

function ProviderCard({
  provider: p,
  confirm,
  onEdit,
  onAsk,
  onCancel,
  onDone,
  onApply,
  onStale,
  users,
  onUsersStale,
}: {
  provider: AdminSharedProvider;
  users: User[] | null | undefined;
  onUsersStale: () => Promise<void>;
  confirm: "retire" | "delete" | null;
  onEdit: () => void;
  onAsk: (kind: "retire" | "delete") => void;
  onCancel: () => void;
  onDone: (message: string) => Promise<void>;
  onApply: (p: AdminSharedProvider) => void;
  onStale: () => Promise<void>;
}) {
  const name = sanitizeLabel(p.name);
  const [working, setWorking] = useState(false);
  const [error, setError] = useState("");
  const confirmRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    setError("");
    // Focus the safe choice, never the destructive one: an Enter right after opening must not act.
    if (confirm) confirmRef.current?.querySelector<HTMLButtonElement>("button[data-confirm-cancel]")?.focus();
  }, [confirm]);

  const act = async (fn: () => Promise<string>, fallback: string) => {
    setError("");
    setWorking(true);
    try {
      await onDone(await fn());
    } catch (err) {
      setError(sharedError(err, fallback, p.engine));
      // A 409 means the counts on screen were stale, a 404 that another admin already deleted the
      // definition: either way refresh the list, so no stale count or ghost card stays behind.
      if (err instanceof ApiError && (err.status === 409 || err.status === 404)) await onStale();
    } finally {
      setWorking(false);
    }
  };

  const selected = p.selected_user_count;
  const keys = p.attached_key_count;
  const operatorMode = p.credential_mode === "operator_key";
  // What a delete takes with it, in the words that fit the definition's credential mode.
  const hasOrgKey = operatorMode && p.operator_key?.present === true;
  const deletes = operatorMode
    ? "its organization key and that key's Test result"
    : `${plural(keys, "personal key", "personal keys")} users attached to it, and their Test results`;

  return (
    <Card className="space-y-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <h2 className="[overflow-wrap:anywhere] text-sm font-semibold text-fg">{name}</h2>
          <p className="text-xs text-faint">
            {ENGINE_LABELS[p.engine]}, {PROTOCOL_LABELS[p.engine]}, revision {p.current_revision}
          </p>
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          {p.retired && <Badge tone="neutral">Retired</Badge>}
          {!p.retired && (
            <>
              <Button variant="secondary" size="sm" onClick={onEdit} aria-label={`Edit ${name}`}>
                Edit
              </Button>
              <Button variant="secondary" size="sm" onClick={() => onAsk("retire")} aria-label={`Retire ${name}`}>
                Retire
              </Button>
            </>
          )}
          <Button variant="danger" size="sm" onClick={() => onAsk("delete")} aria-label={`Delete ${name}`}>
            Delete
          </Button>
        </div>
      </div>

      <ul className="flex flex-wrap gap-2 text-sm" aria-label={`Who uses ${name}`}>
        <li className="rounded-md border border-edge px-2 py-0.5" data-testid={`selected-count-${p.id}`}>
          Selected by {plural(selected, "user", "users")}
        </li>
        {p.credential_mode === "per_user_key" && (
          <li className="rounded-md border border-edge px-2 py-0.5" data-testid={`key-count-${p.id}`}>
            {plural(keys, "personal key", "personal keys")} attached
          </li>
        )}
      </ul>

      {p.credential_mode === "operator_key" && (
        <>
          <OperatorKeyPanel provider={p} name={name} onDone={onDone} onApply={onApply} onStale={onStale} />
          <RecipientsPanel provider={p} name={name} users={users} onUsersStale={onUsersStale} onDone={onDone} onStale={onStale} />
        </>
      )}

      <FixedFacts baseUrl={p.base_url} auth={p.auth_style} />

      <div className="text-sm">
        <p className="text-muted">Models</p>
        <MappedModels engine={p.engine} map={p.model_map} blankLabel=" (no context window)" />
      </div>

      {confirm === "retire" && (
        <ConfirmBox
          refEl={confirmRef}
          label={`Confirm retiring ${name}`}
          error={error}
          actions={
            <>
              <Button variant="dangerSolid" size="sm" disabled={working} onClick={() => void act(async () => {
                await api.adminRetireSharedProvider(p.id);
                return `${name} retired.`;
              }, "Failed to retire the shared provider")}>
                {working ? "Retiring…" : "Retire definition"}
              </Button>
              <Button variant="ghost" size="sm" disabled={working} onClick={onCancel} data-confirm-cancel>Cancel</Button>
            </>
          }
        >
          <p>
            Retire {name}? No new run can start on it and nobody can newly select it. {plural(selected, "user keeps", "users keep")} it
            selected but cannot use it until they choose another source; runs already started keep their binding. This cannot be undone.
          </p>
          {operatorMode && (
            <p className="text-muted" data-testid={`retire-effect-${p.id}`}>
              Retire stops fresh use only: runs already started keep the organization key. To stop everyone, disable or delete the key.
            </p>
          )}
        </ConfirmBox>
      )}

      {confirm === "delete" && (
        <ConfirmBox
          refEl={confirmRef}
          label={`Confirm deleting ${name}`}
          error={error}
          actions={
            <>
              <Button variant="dangerSolid" size="sm" disabled={working || selected > 0} onClick={() => void act(async () => {
                const r = await api.adminDeleteSharedProvider(p.id);
                if (operatorMode) return hasOrgKey ? `${name} deleted, with its organization key.` : `${name} deleted.`;
                return `${name} deleted, with ${plural(r.deleted_key_count, "attached key", "attached keys")}.`;
              }, "Failed to delete the shared provider")}>
                {working ? "Deleting…" : operatorMode ? (hasOrgKey ? "Delete definition and key" : "Delete definition") : "Delete definition and keys"}
              </Button>
              <Button variant="ghost" size="sm" disabled={working} onClick={onCancel} data-confirm-cancel>Cancel</Button>
            </>
          }
        >
          {/* While anyone has it selected the server refuses, so the box leads with the refusal
              instead of asking a question it will not act on. */}
          {selected > 0 ? (
            <>
              <p data-testid={`delete-selected-${p.id}`} className="text-warn">
                {`${name} can't be deleted yet: ${plural(selected, "user has", "users have")} it selected as their ${ENGINE_LABELS[p.engine]} source. ${p.retired ? "Ask them" : "Retire it, then ask them"} to choose another source.`}
              </p>
              <p data-testid={`delete-keys-${p.id}`} className="text-muted">
                {operatorMode && !hasOrgKey
                  ? "Once nobody has it selected, it can be deleted. No organization key is set."
                  : `Once nobody has it selected, deleting it would also delete ${deletes}.`}
              </p>
            </>
          ) : (
            <>
              <p data-testid={`delete-keys-${p.id}`}>
                {operatorMode && !hasOrgKey
                  ? `Delete ${name}? This cannot be undone.`
                  : `Delete ${name}? This also deletes ${deletes}. This cannot be undone.`}
              </p>
              <p data-testid={`delete-selected-${p.id}`} className="text-muted">
                No user has it selected as their {ENGINE_LABELS[p.engine]} source.
              </p>
            </>
          )}
        </ConfirmBox>
      )}
    </Card>
  );
}

// OperatorKeyPanel is the organization-key block of an operator_key card: status, last Test
// result and the four key controls, each labelled with what it does. The typed key lives only
// in this component's state, is cleared whether the save succeeds or not, and is never shown.
function OperatorKeyPanel({
  provider: p,
  name,
  onDone,
  onApply,
  onStale,
}: {
  provider: AdminSharedProvider;
  name: string;
  onDone: (message: string) => Promise<void>;
  onApply: (p: AdminSharedProvider) => void;
  onStale: () => Promise<void>;
}) {
  const key = p.operator_key ?? { present: false, enabled: false };
  const [draft, setDraft] = useState("");
  const [working, setWorking] = useState(false);
  const [testing, setTesting] = useState(false);
  const [error, setError] = useState("");
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const confirmRef = useRef<HTMLDivElement>(null);
  const inputId = useId();
  // Only the server's result is ever shown: a Test reloads the list, and the server hides the
  // stored result after a key write, a model-map edit or a retire.
  const shownProbe = key.probe ?? null;
  const testable = key.present && key.enabled && !p.retired;

  useEffect(() => {
    if (confirmingDelete) confirmRef.current?.querySelector<HTMLButtonElement>("button[data-confirm-cancel]")?.focus();
  }, [confirmingDelete]);

  const run = async (fn: () => Promise<void>, fallback: string) => {
    setError("");
    setWorking(true);
    try {
      await fn();
    } catch (err) {
      setError(sharedError(err, fallback, p.engine));
      // A 409 means the key or definition changed under this card; a 404 that it is gone.
      if (err instanceof ApiError && (err.status === 409 || err.status === 404)) await onStale();
    } finally {
      setWorking(false);
    }
  };

  const save = () => {
    const value = draft;
    // Clear first: the key must not outlive the attempt, whatever its outcome.
    setDraft("");
    return run(async () => {
      onApply((await api.adminPutOperatorKey(p.id, value)).provider);
      await onDone(`Organization key for ${name} saved.`);
    }, "Failed to save the organization key");
  };

  return (
    <div className="space-y-3 rounded-lg border border-edge px-3 py-2 text-sm" data-testid={`operator-key-${p.id}`}>
      <div className="flex flex-wrap items-center gap-2">
        <Badge tone="neutral"><span data-testid={`credential-mode-${p.id}`}>Organization key</span></Badge>
        <p className="text-muted" data-testid={`key-status-${p.id}`}>
          {!key.present
            ? "No key set."
            : `Key set, ${key.enabled ? "enabled" : "disabled"}.${key.set_at ? ` Set ${new Date(key.set_at).toLocaleString()}.` : ""}`}
        </p>
      </div>

      <div data-testid={`probe-results-${p.id}`} aria-live="polite" className="space-y-0.5 text-xs">
        {shownProbe ? (
          <>
            <p>{AUTH_RESULT_TEXT[shownProbe.auth_result]}</p>
            <p>{TOOL_RESULT_TEXT[shownProbe.tool_result ?? "not_run"]}</p>
            <p className="text-faint">
              Neither result certifies full compatibility. Last tested {new Date(shownProbe.probed_at).toLocaleString()}.
            </p>
          </>
        ) : (
          <p className="text-faint">Not tested yet.</p>
        )}
      </div>

      <div className="space-y-3">
        <div className="space-y-1">
          <label htmlFor={inputId} className="block text-muted">Organization key</label>
          <div className="flex flex-wrap gap-2">
            <Input
              id={inputId}
              type="password"
              autoComplete="new-password"
              spellCheck={false}
              className="min-w-0 flex-1"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
            />
            <Button
              size="sm"
              disabled={working || draft === ""}
              onClick={() => void save()}
              aria-label={`${key.present ? "Replace" : "Set"} organization key for ${name}`}
            >
              {key.present ? "Replace key" : "Set key"}
            </Button>
          </div>
          <p className="text-xs text-faint">
            {key.present
              ? "Replacing the key clears the last Test result. Copies recipients already obtained keep working until the old key is rotated at the gateway."
              : "Stored encrypted and never shown again."}
          </p>
        </div>

        <div className="flex flex-wrap items-start gap-x-6 gap-y-3">
          {key.present && (
            <div className="space-y-1">
              <Button
                variant="secondary"
                size="sm"
                disabled={working}
                onClick={() => void run(async () => {
                  onApply((await api.adminSetOperatorKeyEnabled(p.id, !key.enabled)).provider);
                  await onDone(`Organization key for ${name} ${key.enabled ? "disabled" : "enabled"}.`);
                }, "Failed to change the organization key")}
                aria-label={`${key.enabled ? "Disable" : "Enable"} organization key for ${name}`}
              >
                {key.enabled ? "Disable key" : "Enable key"}
              </Button>
              <p className="max-w-xs text-xs text-faint" data-testid={`disable-effect-${p.id}`}>
                {key.enabled
                  ? "Stops everyone at the next claim: no recipient's run receives the key. Copies recipients already obtained are not revoked; rotate the key at the gateway for that."
                  : "Makes the key usable again: recipients' runs receive it from their next claim."}
              </p>
            </div>
          )}
          <div className="space-y-1">
            <Button
              variant="secondary"
              size="sm"
              disabled={working || !testable}
              onClick={() => void (async () => {
                setTesting(true);
                try {
                  await run(async () => {
                    // The 200 body is the stored result for the current tuple: show it now, then reload.
                    const { probe } = await api.adminTestOperatorKey(p.id);
                    onApply({ ...p, operator_key: { ...key, probe } });
                    await onStale();
                  }, "Failed to test the organization key");
                } finally {
                  setTesting(false);
                }
              })()}
              aria-label={`Test organization key for ${name}`}
            >
              {testing ? "Testing…" : "Test key"}
            </Button>
            <p className="max-w-xs text-xs text-faint" data-testid={`test-effect-${p.id}`}>
              {!key.present
                ? "Set a key to test it."
                : p.retired
                  ? "A retired definition cannot be tested."
                  : key.enabled
                    ? "Makes up to two small requests to the gateway with this key: an authentication check, then a single-tool check."
                    : "Enable the key to test it."}
            </p>
          </div>
          {key.present && (
            <div className="space-y-1">
              <Button
                variant="danger"
                size="sm"
                disabled={working}
                onClick={() => { setError(""); setConfirmingDelete(true); }}
                aria-label={`Delete organization key for ${name}`}
              >
                Delete key
              </Button>
              <p className="max-w-xs text-xs text-faint" data-testid={`delete-key-effect-${p.id}`}>
                Stops everyone at the next claim and removes the key from uzi; the definition and its recipients stay.
              </p>
            </div>
          )}
        </div>
      </div>

      {confirmingDelete && (
        <ConfirmBox
          refEl={confirmRef}
          label={`Confirm deleting the organization key of ${name}`}
          error={error}
          actions={
            <>
              <Button variant="dangerSolid" size="sm" disabled={working} onClick={() => void run(async () => {
                onApply((await api.adminDeleteOperatorKey(p.id)).provider);
                setConfirmingDelete(false);
                await onDone(`Organization key for ${name} deleted.`);
              }, "Failed to delete the organization key")}>
                {working ? "Deleting…" : "Delete key"}
              </Button>
              <Button variant="ghost" size="sm" disabled={working} onClick={() => setConfirmingDelete(false)} data-confirm-cancel>Cancel</Button>
            </>
          }
        >
          <p>
            Delete the organization key of {name}? Everyone stops at the next claim. The definition and its recipients stay;
            set a key again to use it later. Copies recipients already obtained are not taken back; only rotating the key at the gateway revokes them.
          </p>
        </ConfirmBox>
      )}
      {!confirmingDelete && error && <Alert message={error} />}
    </div>
  );
}

// RecipientsPanel is who may use an organization-funded definition (PRD #4540 M2): the all-users
// switch, the named grants (paged; each row joined to the admin user list for its name) and the
// incident stop. Default-deny: with neither, nobody sees the definition. Every write is idempotent
// on the server; after one the page reloads, and a changed count refetches the first grant page.
// On a retired definition only removal stays: no new grant and no switching all users on, while
// removing a grant, turning all users off and revoking everyone stay available (R11).
const GRANTS_PAGE = 50;

function userLabel(u: User | undefined, id: string): { name: string; detail: string } {
  if (!u) return { name: "Unknown user", detail: id };
  const display = u.display_name?.trim();
  return display
    ? { name: sanitizeLabel(display), detail: sanitizeLabel(u.email) }
    : { name: sanitizeLabel(u.email), detail: u.is_active ? "" : "inactive" };
}

function RecipientsPanel({
  provider: p,
  name,
  users,
  onUsersStale,
  onDone,
  onStale,
}: {
  provider: AdminSharedProvider;
  name: string;
  users: User[] | null | undefined;
  onUsersStale: () => Promise<void>;
  onDone: (message: string) => Promise<void>;
  onStale: () => Promise<void>;
}) {
  const [grants, setGrants] = useState<string[]>([]);
  const [next, setNext] = useState<string | null>(null);
  const [loadingGrants, setLoadingGrants] = useState(true);
  const [grantsError, setGrantsError] = useState("");
  const [pick, setPick] = useState("");
  const [working, setWorking] = useState(false);
  const [error, setError] = useState("");
  const [confirmingRevoke, setConfirmingRevoke] = useState(false);
  const confirmRef = useRef<HTMLDivElement>(null);
  const pickerId = useId();
  const allUsersHelpId = useId();
  const allUsers = p.operator_all_users;
  const named = p.named_grant_count;
  const key = p.operator_key;
  const keyUsable = key?.present === true && key.enabled;
  const byId = new Map((users ?? []).map((u) => [u.id, u]));

  const loadPage = useCallback(async (cursor?: string) => {
    setLoadingGrants(true);
    setGrantsError("");
    try {
      const page = await api.adminListProviderGrants(p.id, { cursor, limit: GRANTS_PAGE });
      const ids = page.grants.map((g) => g.user_id);
      setGrants((prev) => (cursor ? [...prev, ...ids] : ids));
      setNext(page.next_cursor);
    } catch (err) {
      setGrantsError(sharedError(err, "Failed to load the recipients", p.engine));
    } finally {
      setLoadingGrants(false);
    }
  }, [p.id, p.engine]);

  // The first page again whenever the definition's recipient counts change (a write here, or a
  // reload that saw another admin's).
  useEffect(() => { void loadPage(); }, [loadPage, named, allUsers]);

  useEffect(() => {
    if (confirmingRevoke) confirmRef.current?.querySelector<HTMLButtonElement>("button[data-confirm-cancel]")?.focus();
  }, [confirmingRevoke]);

  const run = async (fn: () => Promise<void>, fallback: string) => {
    setError("");
    setWorking(true);
    try {
      await fn();
    } catch (err) {
      setError(sharedError(err, fallback, p.engine));
      const reason = err instanceof ApiError ? (err.body as { reason?: string } | null)?.reason : undefined;
      if (reason === "user_not_found") {
        setPick("");
        await onUsersStale();
      } else if (err instanceof ApiError && (err.status === 409 || err.status === 404)) {
        await onStale();
      }
    } finally {
      setWorking(false);
    }
  };

  const granted = new Set(grants);
  const candidates = (users ?? []).filter((u) => !granted.has(u.id));
  const summary = allUsers
    ? named > 0
      ? `All users have access. ${plural(named, "named grant is", "named grants are")} kept and still apply if all users is turned off.`
      : "All users have access."
    : named > 0
      ? `${plural(named, "named user has", "named users have")} access.`
      : "No recipients yet.";

  return (
    <section
      className="space-y-3 rounded-lg border border-edge px-3 py-2 text-sm"
      aria-label={`Recipients of ${name}`}
      data-testid={`recipients-${p.id}`}
    >
      <div className="space-y-1">
        <h3 className="font-medium text-fg">Recipients</h3>
        <p className="text-muted" data-testid={`access-notice-${p.id}`}>{DENY_COPY}</p>
        <p className="text-fg" data-testid={`recipient-summary-${p.id}`}>{summary}</p>
        {(allUsers || named > 0) && !keyUsable && !p.retired && (
          <p className="text-xs text-warn" data-testid={`recipients-blocked-${p.id}`}>
            Recipients can see this definition but cannot run on it until its key is set and enabled.
          </p>
        )}
        {p.retired && (
          <p className="text-xs text-faint">Retired: no new grants. You can still remove recipients.</p>
        )}
      </div>

      <div className="flex items-start gap-3">
        <Toggle
          checked={allUsers}
          disabled={working || (p.retired && !allUsers)}
          label={`All users can use ${name}`}
          aria-describedby={allUsersHelpId}
          onChange={(enabled) => void run(async () => {
            await api.adminSetProviderAllUsers(p.id, enabled);
            await onDone(enabled ? `Every user can now use ${name}.` : `All users turned off for ${name}; named grants still apply.`);
          }, "Failed to change all-users access")}
        />
        <div className="min-w-0 space-y-0.5">
          <p className="text-fg">All users</p>
          <p id={allUsersHelpId} className="text-xs text-faint" data-testid={`all-users-effect-${p.id}`}>
            {allUsers
              ? "Every user, including users added later, can select this definition. Turning it off leaves only the named users below, from their next claim."
              : "Off: only the named users below have access. On gives every user, including users added later, the organization key in their runs."}
          </p>
        </div>
      </div>

      <div className="space-y-2">
        <p className="text-muted" data-testid={`named-count-${p.id}`}>
          Named users: {named.toLocaleString()}
        </p>
        {grantsError && <Alert message={grantsError} />}
        {users === null && (
          <p className="text-xs text-warn">The user list could not be read, so names are not shown and no user can be picked.</p>
        )}
        {grants.length > 0 && (
          <ul className="divide-y divide-edge rounded-md border border-edge" aria-label={`Users granted ${name}`}>
            {grants.map((id) => {
              const label = userLabel(byId.get(id), id);
              return (
                <li key={id} className="flex flex-wrap items-center justify-between gap-2 px-2 py-1.5">
                  <span className="min-w-0 [overflow-wrap:anywhere]">
                    <span className="text-fg">{label.name}</span>
                    {label.detail && <span className="ml-2 text-xs text-faint">{label.detail}</span>}
                  </span>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={working}
                    aria-label={`Remove ${label.name} from ${name}`}
                    onClick={() => void run(async () => {
                      await api.adminDeleteProviderGrant(p.id, id);
                      await onDone(`${label.name} no longer has access to ${name}. Their runs stop receiving the key at the next claim.`);
                    }, "Failed to remove the recipient")}
                  >
                    Remove
                  </Button>
                </li>
              );
            })}
          </ul>
        )}
        {loadingGrants && grants.length === 0 && named > 0 && <p className="text-xs text-faint">Loading recipients…</p>}
        {next && (
          <Button variant="secondary" size="sm" disabled={loadingGrants} onClick={() => void loadPage(next)}>
            {loadingGrants ? "Loading…" : "Show more recipients"}
          </Button>
        )}

        {!p.retired && users && (
          <div className="space-y-1">
            <label htmlFor={pickerId} className="block text-muted">Grant access to a user</label>
            <div className="flex flex-wrap gap-2">
              <Select id={pickerId} className="min-w-0 flex-1" value={pick} onChange={(e) => setPick(e.target.value)}>
                <option value="">{candidates.length ? "Choose a user" : "Every listed user is already named"}</option>
                {candidates.map((u) => {
                  const l = userLabel(u, u.id);
                  return <option key={u.id} value={u.id}>{l.detail ? `${l.name} (${l.detail})` : l.name}</option>;
                })}
              </Select>
              <Button
                size="sm"
                disabled={working || pick === ""}
                aria-label={`Grant ${name} to the chosen user`}
                onClick={() => {
                  const id = pick;
                  const label = userLabel(byId.get(id), id).name;
                  void run(async () => {
                    await api.adminPutProviderGrant(p.id, id);
                    setPick("");
                    await onDone(`${label} can now use ${name}; their runs receive the organization key.`);
                  }, "Failed to grant access");
                }}
              >
                Grant access
              </Button>
            </div>
          </div>
        )}
      </div>

      <div className="space-y-1 border-t border-edge pt-3" data-testid={`incident-stop-${p.id}`}>
        <h4 className="font-medium text-fg">Stop access</h4>
        <div className="flex flex-wrap items-start gap-x-6 gap-y-2">
          <div className="space-y-1">
            <Button
              variant="danger"
              size="sm"
              disabled={working}
              onClick={() => { setError(""); setConfirmingRevoke(true); }}
              aria-label={`Revoke all recipients of ${name}`}
            >
              Revoke all recipients
            </Button>
            <p className="max-w-sm text-xs text-faint" data-testid={`revoke-effect-${p.id}`}>
              Turns all users off and removes every named grant. Takes effect at the next claim; copies already
              obtained are revoked only by rotating the key at the gateway.
            </p>
          </div>
          <p className="max-w-sm text-xs text-faint" data-testid={`stop-scope-${p.id}`}>
            Disabling or deleting the key stops everyone at the next claim. Retire stops fresh use only: runs
            already started keep the key.
          </p>
        </div>
      </div>

      {confirmingRevoke && (
        <ConfirmBox
          refEl={confirmRef}
          label={`Confirm revoking all recipients of ${name}`}
          error={error}
          actions={
            <>
              <Button variant="dangerSolid" size="sm" disabled={working} onClick={() => void run(async () => {
                const r = await api.adminRevokeProviderRecipients(p.id);
                setConfirmingRevoke(false);
                await onDone(
                  `Revoked every recipient of ${name}: all users turned off and ${plural(r.named_grants_removed, "named grant", "named grants")} removed. ` +
                  "Runs stop receiving the key at their next claim; rotate the key at the gateway to revoke copies already obtained.",
                );
              }, "Failed to revoke the recipients")}>
                {working ? "Revoking…" : "Revoke all recipients"}
              </Button>
              <Button variant="ghost" size="sm" disabled={working} onClick={() => setConfirmingRevoke(false)} data-confirm-cancel>Cancel</Button>
            </>
          }
        >
          <p>
            Revoke every recipient of {name}? All users is turned off and {plural(named, "named grant is", "named grants are")} removed.
            Nobody can start a run on it from the next claim. Copies already obtained are revoked only by rotating the key at the gateway.
          </p>
        </ConfirmBox>
      )}
      {!confirmingRevoke && error && <Alert message={error} />}
    </section>
  );
}

function ConfirmBox({
  refEl,
  label,
  error,
  actions,
  children,
}: {
  refEl: React.RefObject<HTMLDivElement | null>;
  label: string;
  error: string;
  actions: ReactNode;
  children: ReactNode;
}) {
  return (
    <div ref={refEl} role="group" aria-label={label} className="space-y-2 rounded-lg border border-danger/40 bg-danger/5 px-3 py-2 text-sm text-fg">
      {children}
      {error && <Alert message={error} />}
      <div className="flex flex-wrap gap-2">{actions}</div>
    </div>
  );
}

function CreateForm({
  allowed,
  onCancel,
  onCreated,
}: {
  allowed: string[];
  onCancel: () => void;
  onCreated: (p: AdminSharedProvider) => Promise<void>;
}) {
  const uid = useId();
  const [engine, setEngine] = useState<Engine>("claude");
  const [name, setName] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  const [auth, setAuth] = useState<ProviderAuthStyle>("bearer");
  const [rows, setRows] = useState<ModelRows>(emptyRows);
  const [mode, setMode] = useState<"per_user_key" | "operator_key">("per_user_key");
  const [acknowledged, setAcknowledged] = useState(false);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const operatorMode = mode === "operator_key";

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (operatorMode && !acknowledged) return;
    const built = buildModelMap(rows, engine);
    const err = validName(name) ?? (baseUrl === "" ? "Pick a base URL." : null) ?? ("error" in built ? built.error : null);
    if (err) return setError(err);
    setError("");
    setSaving(true);
    try {
      const { provider } = await api.adminCreateSharedProvider({
        engine,
        name: name.trim(),
        base_url: baseUrl,
        auth_style: auth,
        model_map: (built as { map: ProviderModelMap }).map,
        ...(operatorMode ? { credential_mode: "operator_key" as const, acknowledge_recipient_trust: true } : {}),
      });
      await onCreated(provider);
    } catch (err) {
      setError(sharedError(err, "Failed to create the shared provider", engine));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card>
      <form onSubmit={submit} className="space-y-3" aria-label="New shared provider">
        <h2 className="text-sm font-semibold text-fg">New shared provider</h2>
        <p className="text-sm text-muted">
          {operatorMode
            ? `You set one organization key after creating it. ${DENY_COPY}`
            : "No key here: each user attaches their own."}{" "}
          The engine, endpoint and auth style cannot be changed after you create it; a different gateway needs a new
          definition, which users select again on purpose.
        </p>
        {/* Switching engine starts the map and auth over: the two engines share no model slots,
            and Codex is bearer-only. */}
        <Field label="Engine">
          <Select
            aria-label="Engine"
            value={engine}
            onChange={(e) => {
              setEngine(e.target.value as Engine);
              setRows(emptyRows());
              setAuth("bearer");
              // An organization key is Claude only: leaving Claude drops the choice and its acknowledgment.
              setMode("per_user_key");
              setAcknowledged(false);
              setError("");
            }}
          >
            <option value="claude">Claude</option>
            <option value="codex">Codex</option>
          </Select>
        </Field>
        {engine === "claude" && (
          <Field label="Credential source">
            <Select
              aria-label="Credential source"
              value={mode}
              onChange={(e) => {
                setMode(e.target.value as typeof mode);
                setAcknowledged(false);
                setError("");
              }}
            >
              <option value="per_user_key">Each user attaches their own key</option>
              <option value="operator_key">One organization key</option>
            </Select>
          </Field>
        )}
        <p className="text-xs text-muted" data-testid="engine-protocol">Protocol: {PROTOCOL_LABELS[engine]}</p>
        <Field label="Name">
          <Input aria-label="Provider name" placeholder="platform-gateway" value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <BaseUrlSelect value={baseUrl} onChange={setBaseUrl} allowed={allowed} />
        <AuthStyleSelect engine={engine} value={auth} onChange={setAuth} />
        <p className={auth === "x_api_key" ? "text-xs text-warn" : "text-xs text-faint"} data-testid="auth-advice">
          {engine === "codex"
            ? "Codex sends each user's key as a Bearer token. Codex runtime redirects may forward request bodies and authentication headers, so share only a gateway you trust not to redirect: allowlist admission is not confinement to that host."
            : auth === "x_api_key"
              ? "With x-api-key, a redirecting gateway can forward each user's key to another host. Bearer is recommended."
              : "Bearer is recommended: on a redirect the key can reach another path on the same host, but not another host. Request bodies can still follow a redirect."}
        </p>
        <ModelMapFields engine={engine} rows={rows} onChange={setRows} idPrefix={`${uid}-new`} />
        {operatorMode && (
          <div
            role="group"
            aria-label="Organization key disclosures"
            className="space-y-2 rounded-lg border border-warn/50 bg-warn/5 px-3 py-2 text-sm text-fg"
          >
            <p className="font-medium text-warn">Before you create an organization-funded definition</p>
            <p data-testid="access-notice-new">
              {DENY_COPY} These terms apply to every user you grant access to.
            </p>
            <ul className="list-disc space-y-1.5 pl-5">
              {OPERATOR_DISCLOSURES.map((d) => (
                <li key={d.id} data-testid={d.id}>{d.text}</li>
              ))}
            </ul>
            <label className="flex items-start gap-2 pt-1">
              <input
                type="checkbox"
                className="mt-1"
                checked={acknowledged}
                onChange={(e) => setAcknowledged(e.target.checked)}
              />
              <span>I acknowledge these terms for every user I grant this key to.</span>
            </label>
          </div>
        )}
        {error && <Alert message={error} />}
        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={saving || (operatorMode && !acknowledged)}>{saving ? "Creating…" : "Create shared provider"}</Button>
          <Button type="button" variant="ghost" onClick={onCancel} disabled={saving}>Cancel</Button>
        </div>
      </form>
    </Card>
  );
}

function EditForm({
  provider: p,
  onCancel,
  onSaved,
}: {
  provider: AdminSharedProvider;
  onCancel: () => void;
  onSaved: (p: AdminSharedProvider) => Promise<void>;
}) {
  const uid = useId();
  const [name, setName] = useState(p.name);
  const [rows, setRows] = useState<ModelRows>(() => rowsFromMap(p.model_map));
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const built = buildModelMap(rows, p.engine);
    const err = validName(name) ?? ("error" in built ? built.error : null);
    if (err) return setError(err);
    // Send only what changed: a model map is a new revision for every user who selected it.
    const nextMap = (built as { map: ProviderModelMap }).map;
    const body: { name?: string; model_map?: ProviderModelMap } = {};
    if (name.trim() !== p.name) body.name = name.trim();
    if (!sameModelMap(nextMap, p.model_map)) body.model_map = nextMap;
    if (Object.keys(body).length === 0) return onCancel();
    setError("");
    setSaving(true);
    try {
      await onSaved((await api.adminPatchSharedProvider(p.id, body)).provider);
    } catch (err) {
      setError(sharedError(err, "Failed to save the shared provider", p.engine));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card>
      <form onSubmit={submit} className="space-y-3" aria-label={`Edit ${sanitizeLabel(p.name)}`}>
        <h2 className="text-sm font-semibold text-fg">Edit {sanitizeLabel(p.name)}</h2>
        <Field label="Name">
          <Input aria-label="Provider name" value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <FixedFacts baseUrl={p.base_url} auth={p.auth_style} />
        <p className="text-xs text-faint">
          {ENGINE_LABELS[p.engine]}, {PROTOCOL_LABELS[p.engine]}. To use a different engine, endpoint or auth style,
          create a new definition.
        </p>
        <ModelMapFields engine={p.engine} rows={rows} onChange={setRows} idPrefix={`${uid}-edit`} />
        <p className="text-xs text-faint">
          A model map change applies to new runs of {plural(p.selected_user_count, "user", "users")} who selected it and resets
          every user's Test result. Runs already started keep their map.
        </p>
        {error && <Alert message={error} />}
        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={saving}>{saving ? "Saving…" : "Save changes"}</Button>
          <Button type="button" variant="ghost" onClick={onCancel} disabled={saving}>Cancel</Button>
        </div>
      </form>
    </Card>
  );
}
