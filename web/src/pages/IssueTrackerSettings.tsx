// Settings → Issue trackers (PRD #4173 M6): connect, list and delete the issue tracker
// accounts (YouTrack today) that a linked repo takes its issues from. Linking a repo to
// a project and tag happens on the Repos page; this screen only owns the connections.
//
// The token follows ForgeSettings exactly: a masked PasswordInput with autoComplete off,
// cleared as soon as the create call returns, never shown again and never sent anywhere
// but that one call. Unlike ForgeSettings the field is cleared on a refusal too, so a
// token never outlives the submit that used it (PRD #4173, 2026-11-19 S4 entry).
//
// Every API-provided string (base URL, account login, kind, error text) is rendered as
// JSX text through stripUnsafeChars: no Markdown, no dangerouslySetInnerHTML.

import { useEffect, useRef, useState } from "react";
import { api, type IssueTrackerConnection } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { stripUnsafeChars } from "../lib/safeText";
import { issueTrackerKindLabel } from "../lib/issueTrackerText";
import { DOC_ISSUE_TRACKERS } from "../lib/doclinks";
import { useDemoMode } from "../lib/demoMode";
import { maskHost, maskUsername } from "../lib/demoMask";
import { Alert, Badge, Button, Card, EmptyState, Field, PasswordInput, SectionTitle, Select, Skeleton } from "../components/ui";
import { SettingsShell } from "../components/SettingsShell";
import { DocLink } from "../components/DocLink";
import { BoardIcon } from "../components/icons";

const TOKEN_FIELD_ID = "issue-tracker-token";

export function IssueTrackerSettings() {
  const demo = useDemoMode();
  const [kind, setKind] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  const [token, setToken] = useState("");
  const [connecting, setConnecting] = useState(false);
  const [connectError, setConnectError] = useState("");
  const [notice, setNotice] = useState("");
  const [confirmingId, setConfirmingId] = useState<string | null>(null);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  // A delete refusal (409 while a repo is linked) belongs to its row, not the page.
  const [rowError, setRowError] = useState<{ id: string; message: string } | null>(null);
  const confirmRef = useRef<HTMLDivElement>(null);
  // After a delete the row (and its focused confirm) is gone: focus moves to the list
  // heading, or to the empty state when the last connection went, once that renders.
  const listFocusRef = useRef<HTMLDivElement & HTMLHeadingElement>(null);
  const [focusList, setFocusList] = useState(false);
  // After Cancel, Escape or a refused delete the confirm closes and its focused element
  // goes away: focus returns to that row's Delete button, never to <body>.
  const deleteButtons = useRef(new Map<string, HTMLButtonElement>());
  const [returnFocusId, setReturnFocusId] = useState<string | null>(null);

  const { data, loading, error: loadError, reload } = useAsyncData(
    async () => {
      const [cfg, conns] = await Promise.all([api.issueTrackerConfig(), api.listIssueTrackerConnections()]);
      setKind((prev) => prev || cfg.kinds[0] || "");
      setBaseUrl((prev) => prev || cfg.allowed_base_urls[0] || "");
      return { allowedUrls: cfg.allowed_base_urls, kinds: cfg.kinds, connections: conns.connections };
    },
    [],
    { fallback: "Failed to load issue tracker settings" },
  );
  const allowedUrls = data?.allowedUrls ?? [];
  const kinds = data?.kinds ?? [];
  const connections = data?.connections ?? [];

  // Focus the warning, not the destructive button, when a confirm arms (OAuthConnections).
  useEffect(() => {
    if (confirmingId) confirmRef.current?.focus();
  }, [confirmingId]);

  useEffect(() => {
    if (!returnFocusId) return;
    deleteButtons.current.get(returnFocusId)?.focus();
    setReturnFocusId(null);
  }, [returnFocusId]);

  useEffect(() => {
    if (!focusList) return;
    listFocusRef.current?.focus();
    setFocusList(false);
  }, [focusList, data]);

  const connect = async (e: React.FormEvent) => {
    e.preventDefault();
    setConnectError("");
    setNotice("");
    setRowError(null);
    setConnecting(true);
    try {
      const { connection } = await api.createIssueTrackerConnection(kind, baseUrl, token);
      setToken("");
      setNotice(
        `Connected to ${maskHost(stripUnsafeChars(connection.base_url), demo)} as ${maskUsername(
          stripUnsafeChars(connection.account_login),
          "bot",
          demo,
        )}.`,
      );
      await reload();
    } catch (err) {
      setToken("");
      setConnectError(stripUnsafeChars(errorMessage(err, "Connect failed")));
    } finally {
      setConnecting(false);
    }
  };

  const remove = async (id: string) => {
    setNotice("");
    setConnectError("");
    setRowError(null);
    setDeletingId(id);
    try {
      await api.deleteIssueTrackerConnection(id);
      setConfirmingId(null);
      setNotice("Connection deleted.");
      await reload();
      setFocusList(true);
    } catch (err) {
      setConfirmingId(null);
      setRowError({ id, message: stripUnsafeChars(errorMessage(err, "Delete failed")) });
      setReturnFocusId(id);
    } finally {
      setDeletingId(null);
    }
  };

  const configLoaded = data !== null;
  const noAllowlist = configLoaded && allowedUrls.length === 0;

  return (
    <SettingsShell description="Connect the issue tracker account a linked repo takes its issues from.">
      {loadError && <Alert message={loadError} />}
      {notice && <Alert tone="success" message={notice} />}

      {loading && !configLoaded ? (
        <Card>
          <Skeleton className="h-5 w-1/3" />
          <Skeleton className="mt-4 h-9 w-full" />
          <Skeleton className="mt-3 h-9 w-full" />
        </Card>
      ) : noAllowlist ? (
        <Card className="border-warn/40">
          <SectionTitle>Connect an issue tracker</SectionTitle>
          <p className="mt-2 max-w-prose text-sm text-fg" data-testid="issue-tracker-no-allowlist">
            An operator must set{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">ISSUE_TRACKER_ALLOWED_BASE_URLS</code> (chart
            value <code className="rounded bg-raised px-1 py-0.5 text-fg">issueTracker.allowedBaseURLs</code>) before
            an issue tracker can be connected.
          </p>
          <p className="mt-2 max-w-prose text-sm text-muted">
            The operator steps are in the <DocLink slug={DOC_ISSUE_TRACKERS}>issue trackers setup guide</DocLink>.
          </p>
        </Card>
      ) : configLoaded ? (
        <Card>
          <SectionTitle>Connect an issue tracker</SectionTitle>
          <p className="mt-2 max-w-prose text-sm text-muted">
            uzi reads and writes a linked repo&apos;s issues as the account the token belongs to. In YouTrack, open
            Profile, Account Security, New token, and give it the scope YouTrack. The token is write-only: it is
            stored sealed and never shown again. Step-by-step instructions are in the{" "}
            <DocLink slug={DOC_ISSUE_TRACKERS}>issue trackers setup guide</DocLink>.
          </p>
          <form className="mt-4 space-y-4" onSubmit={connect}>
            {kinds.length > 1 && (
              <Field label="Kind" htmlFor="issue-tracker-kind">
                <Select id="issue-tracker-kind" value={kind} onChange={(e) => setKind(e.target.value)}>
                  {kinds.map((k) => (
                    <option key={k} value={k}>
                      {issueTrackerKindLabel(k)}
                    </option>
                  ))}
                </Select>
              </Field>
            )}
            <Field label="Base URL" htmlFor="issue-tracker-base-url">
              <Select id="issue-tracker-base-url" value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)}>
                {allowedUrls.map((u) => (
                  <option key={u} value={u}>
                    {stripUnsafeChars(u)}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Permanent token (scope: YouTrack)" htmlFor={TOKEN_FIELD_ID}>
              <PasswordInput
                id={TOKEN_FIELD_ID}
                autoComplete="off"
                placeholder="your YouTrack permanent token"
                value={token}
                onChange={(e) => setToken(e.target.value)}
              />
            </Field>
            {connectError && <Alert message={connectError} />}
            <Button type="submit" disabled={connecting || !kind || !baseUrl || !token}>
              {connecting ? "Verifying…" : "Connect"}
            </Button>
          </form>
        </Card>
      ) : null}

      {loading && !configLoaded ? null : connections.length === 0 ? (
        configLoaded && (
          <div ref={listFocusRef} tabIndex={-1} className="outline-hidden">
            <EmptyState
              icon={<BoardIcon />}
              title="No issue tracker connection yet"
              description="Once connected, a repo that was never enabled can be linked to one of its projects on the Repos page."
            />
          </div>
        )
      ) : (
        <Card>
          <h2
            ref={listFocusRef}
            tabIndex={-1}
            className="text-xs font-semibold uppercase tracking-wider text-faint outline-hidden"
          >
            Connections
          </h2>
          <ul className="mt-3 space-y-2">
            {connections.map((c) => (
              <ConnectionRow
                key={c.id}
                connection={c}
                demo={demo}
                confirming={confirmingId === c.id}
                deleting={deletingId === c.id}
                busy={deletingId !== null}
                error={rowError?.id === c.id ? rowError.message : ""}
                confirmRef={confirmRef}
                deleteButtonRef={(el) => {
                  if (el) deleteButtons.current.set(c.id, el);
                  else deleteButtons.current.delete(c.id);
                }}
                onAskDelete={() => {
                  setRowError(null);
                  setConfirmingId(c.id);
                }}
                onCancel={() => {
                  setConfirmingId(null);
                  setReturnFocusId(c.id);
                }}
                onDelete={() => remove(c.id)}
              />
            ))}
          </ul>
        </Card>
      )}

      <p className="max-w-prose text-xs text-faint">
        To replace a token, connect again with the same base URL. If a repo is linked to the connection, the new
        token must belong to the same account, and the connection cannot be deleted.
      </p>
    </SettingsShell>
  );
}

function ConnectionRow({
  connection: c,
  demo,
  confirming,
  deleting,
  busy,
  error,
  confirmRef,
  deleteButtonRef,
  onAskDelete,
  onCancel,
  onDelete,
}: {
  connection: IssueTrackerConnection;
  demo: boolean;
  confirming: boolean;
  deleting: boolean;
  busy: boolean;
  error: string;
  confirmRef: React.RefObject<HTMLDivElement | null>;
  deleteButtonRef: (el: HTMLButtonElement | null) => void;
  onAskDelete: () => void;
  onCancel: () => void;
  onDelete: () => void;
}) {
  const host = maskHost(stripUnsafeChars(c.base_url), demo);
  const login = maskUsername(stripUnsafeChars(c.account_login), "bot", demo);
  const kindLabel = issueTrackerKindLabel(c.kind);
  return (
    <li className="flex flex-col gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2.5 text-sm">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <Badge tone="info">{kindLabel}</Badge>
            <span className="break-all font-medium text-fg">{host}</span>
          </div>
          {/* The identity line is the row's point: whose account writes the issues. */}
          <p className="mt-1 text-xs text-muted">
            Acts as <span className="font-medium text-fg">{login}</span>
          </p>
          <dl className="mt-1 flex flex-wrap gap-x-4 gap-y-0.5 text-xs text-faint">
            <div className="flex gap-1">
              <dt>Connected</dt>
              <dd>{new Date(c.created_at).toLocaleDateString()}</dd>
            </div>
            <div className="flex gap-1">
              <dt>Last verified</dt>
              <dd>{new Date(c.last_verified_at).toLocaleString()}</dd>
            </div>
          </dl>
        </div>
        {!confirming && (
          <Button
            ref={deleteButtonRef}
            variant="danger"
            size="sm"
            disabled={busy}
            onClick={onAskDelete}
            aria-label={`Delete the ${kindLabel} connection to ${host}`}
          >
            Delete
          </Button>
        )}
      </div>
      {confirming && (
        <div
          ref={confirmRef}
          tabIndex={-1}
          role="group"
          aria-label={`Confirm deleting the ${kindLabel} connection to ${host}`}
          onKeyDown={(e) => {
            if (e.key === "Escape") onCancel();
          }}
          className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 outline-hidden"
        >
          <p className="text-xs text-warn">
            Delete the connection to {host}? Its token is removed. A connection with a linked repo cannot be deleted.
          </p>
          <div className="flex items-center gap-1.5">
            <Button variant="dangerSolid" size="sm" disabled={deleting} onClick={onDelete}>
              {deleting ? "Deleting…" : "Delete connection"}
            </Button>
            <Button variant="ghost" size="sm" disabled={deleting} onClick={onCancel}>
              Cancel
            </Button>
          </div>
        </div>
      )}
      {error && <Alert message={error} />}
    </li>
  );
}
