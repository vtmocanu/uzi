// Settings → Access: the CLI-token lifecycle — mint (show-once), list with the
// full forensic surface (token_prefix / last_used_at / last_used_ip), single
// revoke, and the "Revoke all" panic button. The scope picker is offered ONLY to
// admins (a non-admin can mint only a 'user' token). See PRD #64 M6.

import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type CliToken, type CliTokenScope } from "../lib/api";
import { CLI_TOKEN_STALE_DAYS, isCliTokenStale } from "../lib/cliTokens";
import { Alert, Badge, Button, Card, EmptyState, Field, Input, SectionTitle, Select } from "./ui";
import { KeyIcon } from "./icons";

const scopeLabel = (scope: CliTokenScope) => (scope === "admin_ro" ? "admin (read-only)" : "user");

export function CliTokens() {
  const { user } = useAuth();
  const isAdmin = user?.is_admin ?? false;

  const [tokens, setTokens] = useState<CliToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const [name, setName] = useState("");
  const [scope, setScope] = useState<CliTokenScope>("user");
  const [busy, setBusy] = useState(false);

  // The plaintext token for the just-minted row. Shown exactly once (only its hash
  // is stored server-side) and cleared on the next action.
  const [minted, setMinted] = useState<{ token: string; row: CliToken } | null>(null);
  const [copied, setCopied] = useState(false);

  // Revoke-all is the only confirmed action here: single revoke is a one-click,
  // owner-only act (its blast radius is the owner's own tokens), but revoke-all is
  // the panic button and stops every CLI/CI job at once, so it asks first.
  const [confirmingAll, setConfirmingAll] = useState(false);
  const confirmRef = useRef<HTMLDivElement>(null);

  const load = useCallback(async () => {
    try {
      const { tokens } = await api.listCliTokens();
      setTokens(tokens);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load CLI tokens");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // Focus the warning when the confirm arms (not the destructive button — that is
  // how a confirmation becomes a formality), mirroring WorkersSettings.
  useEffect(() => {
    if (confirmingAll) confirmRef.current?.focus();
  }, [confirmingAll]);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      // A non-admin can never pick admin_ro (the control is hidden), but pin 'user'
      // regardless so a stale state value can't smuggle a scope the server 403s.
      const chosen: CliTokenScope = isAdmin ? scope : "user";
      const { token, cli_token } = await api.createCliToken(name.trim(), chosen);
      setMinted({ token, row: cli_token });
      setCopied(false);
      setName("");
      setScope("user");
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to create CLI token");
    } finally {
      setBusy(false);
    }
  };

  const copy = async () => {
    if (!minted) return;
    try {
      await navigator.clipboard.writeText(minted.token);
      setCopied(true);
    } catch {
      // Clipboard may be unavailable (insecure context); the token stays visible.
    }
  };

  const revoke = async (id: string) => {
    setError("");
    try {
      await api.revokeCliToken(id);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to revoke token");
    }
  };

  const revokeAll = async () => {
    setError("");
    setConfirmingAll(false);
    try {
      await api.revokeAllCliTokens();
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to revoke tokens");
    }
  };

  const activeCount = tokens.filter((t) => !t.revoked).length;

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>CLI tokens</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Bearer tokens the <code className="rounded bg-raised px-1 py-0.5 text-fg">uzi</code> CLI
          and CI jobs present instead of a browser session. Set one as{" "}
          <code className="rounded bg-raised px-1 py-0.5 text-fg">UZI_TOKEN</code>. A password change
          does <strong className="text-fg">not</strong> revoke them — revoke a lost token here.
        </p>
      </div>

      {error && <Alert message={error} />}

      {/* Show-once mint result. Only its hash is stored, so this is the one and only
          time the value appears — the copy affordance + warning mirror the worker
          join-token card. */}
      {minted && (
        <Card className="space-y-3 border-ok/40">
          <SectionTitle className="text-ok">Token “{minted.row.name}” created</SectionTitle>
          <p className="text-sm text-muted">
            Copy it now — it is shown <strong className="text-fg">once and never again</strong> (only
            its hash is stored). You won’t be able to see this value later.
          </p>
          <div className="flex items-center gap-2">
            <code className="flex-1 overflow-x-auto rounded-lg border border-edge bg-ink px-3 py-2 font-mono text-sm text-ok">
              {minted.token}
            </code>
            <Button variant="secondary" onClick={copy}>
              {copied ? "Copied" : "Copy"}
            </Button>
          </div>
          <div>
            <Button variant="ghost" onClick={() => setMinted(null)}>
              Done
            </Button>
          </div>
        </Card>
      )}

      <form onSubmit={create} className="flex flex-wrap items-end gap-3">
        <div className="min-w-[14rem] flex-1">
          <Field label="Name" htmlFor="cli-token-name">
            <Input
              id="cli-token-name"
              placeholder="e.g. laptop, ci-runner"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </Field>
        </div>
        {/* Scope picker: admin-only. A 'user' token is capped to the owner's own
            access; 'admin_ro' reads the whole factory and only an admin may mint
            it (the server also enforces this — the hidden control is UX, not the
            security boundary). */}
        {isAdmin && (
          <div className="min-w-[12rem]">
            <Field label="Scope" htmlFor="cli-token-scope">
              <Select
                id="cli-token-scope"
                value={scope}
                onChange={(e) => setScope(e.target.value as CliTokenScope)}
              >
                <option value="user">User — your own access</option>
                <option value="admin_ro">Admin (read-only) — whole factory</option>
              </Select>
            </Field>
          </div>
        )}
        <Button type="submit" disabled={busy || name.trim() === ""}>
          {busy ? "Creating…" : "Create token"}
        </Button>
      </form>

      <div className="space-y-3">
        <div className="flex items-center justify-between gap-2">
          <SectionTitle>Your tokens</SectionTitle>
          {/* The panic button. Enabled only when there is something to revoke. */}
          {activeCount > 0 && !confirmingAll && (
            <Button variant="danger" size="sm" onClick={() => setConfirmingAll(true)}>
              Revoke all
            </Button>
          )}
        </div>

        {confirmingAll && (
          <div
            ref={confirmRef}
            tabIndex={-1}
            role="group"
            aria-label="Confirm revoking all CLI tokens"
            aria-describedby="cli-revoke-all-warning"
            onKeyDown={(e) => {
              if (e.key === "Escape") setConfirmingAll(false);
            }}
            className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 outline-none"
          >
            <p id="cli-revoke-all-warning" className="text-xs text-warn">
              Revoke all {activeCount} active {activeCount === 1 ? "token" : "tokens"}? Every{" "}
              <code className="rounded bg-raised px-1 py-0.5">uzi</code> CLI and CI job using one
              stops working until you mint a new token. This cannot be undone.
            </p>
            <div className="flex items-center gap-1.5">
              <Button variant="danger" size="sm" onClick={revokeAll}>
                Revoke all anyway
              </Button>
              <Button variant="ghost" size="sm" onClick={() => setConfirmingAll(false)}>
                Cancel
              </Button>
            </div>
          </div>
        )}

        {loading ? (
          <div className="space-y-2">
            <Skeletons />
          </div>
        ) : tokens.length === 0 ? (
          <EmptyState
            icon={<KeyIcon />}
            title="No CLI tokens yet"
            description="Create one above, then set it as UZI_TOKEN for the uzi CLI or a CI job."
          />
        ) : (
          <ul className="space-y-2">
            {tokens.map((t) => (
              <TokenRow key={t.id} token={t} onRevoke={() => revoke(t.id)} />
            ))}
          </ul>
        )}
      </div>
    </Card>
  );
}

function Skeletons() {
  return (
    <>
      <div className="h-14 animate-pulse rounded-lg bg-raised" />
      <div className="h-14 animate-pulse rounded-lg bg-raised" />
    </>
  );
}

function TokenRow({ token, onRevoke }: { token: CliToken; onRevoke: () => void }) {
  const stale = isCliTokenStale(token);
  return (
    <li
      className={
        "flex flex-col gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2.5 text-sm" +
        (token.revoked ? " opacity-60" : "")
      }
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <code className="font-mono text-fg">{token.token_prefix}…</code>
            <span className="truncate font-medium text-fg">{token.name}</span>
            <Badge tone={token.scope === "admin_ro" ? "info" : "neutral"}>{scopeLabel(token.scope)}</Badge>
            {token.revoked && <Badge tone="danger">revoked</Badge>}
            {/* Render-time staleness hint only (no new column/endpoint/policy). */}
            {stale && !token.revoked && (
              <Badge
                tone="warning"
                title={`Unused for ${CLI_TOKEN_STALE_DAYS}+ days. If you don't recognise it, revoke it.`}
              >
                stale
              </Badge>
            )}
          </div>
          {/* The forensic surface (Risk 8): there is no per-request audit log, so
              these three answer "which token is this, and was it used by someone
              who isn't me?". last_used_ip is the only detection control. */}
          <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-faint">
            <span>created {new Date(token.created_at).toLocaleDateString()}</span>
            <span>
              ·{" "}
              {token.last_used_at
                ? `last used ${new Date(token.last_used_at).toLocaleString()}`
                : "never used"}
            </span>
            <span>· {token.last_used_ip ? `from ${token.last_used_ip}` : "no IP recorded"}</span>
            {token.expires_at && <span>· expires {new Date(token.expires_at).toLocaleDateString()}</span>}
          </div>
        </div>
        {!token.revoked && (
          <Button variant="danger" size="sm" onClick={onRevoke}>
            Revoke
          </Button>
        )}
      </div>
    </li>
  );
}
