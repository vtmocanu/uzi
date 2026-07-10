// Vault surface (PRD #32): the header status badge, the locked-state unlock
// banner, and the lock action hook. State rides useAuth().vaultUnlocked (from the
// session payload); unlock/lock call the API then refresh the session so the
// badge, banner, and "waiting for vault unlock" run state all reconcile at once.

import { useCallback, useState, type FormEvent } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError } from "../lib/api";
import { Badge, Button, Input, cx } from "./ui";

// VaultBadge is the compact lock/unlock status pill. `compact` (the collapsed
// sidebar rail) renders just the glyph with the description as a tooltip.
export function VaultBadge({ compact = false }: { compact?: boolean }) {
  const { vaultUnlocked } = useAuth();
  const glyph = vaultUnlocked ? "🔓" : "🔒";
  const label = vaultUnlocked ? "Vault unlocked" : "Vault locked";
  const title = vaultUnlocked
    ? "Your secret vault is unlocked — your agents can run."
    : "Your secret vault is locked — unlock it with your password to resume your agents.";

  if (compact) {
    return (
      <span title={title} aria-label={label} className="text-sm leading-none">
        {glyph}
      </span>
    );
  }
  return (
    <Badge tone={vaultUnlocked ? "ok" : "warning"} title={title}>
      <span aria-hidden="true">{glyph}</span>
      {label}
    </Badge>
  );
}

// useVaultLock exposes a lock action: it evicts the server-side DEK then refreshes
// the session so the UI reflects the locked state. onLocked fires on success (the
// caller shows a confirmation toast).
export function useVaultLock(onLocked?: () => void) {
  const { refresh } = useAuth();
  const [locking, setLocking] = useState(false);
  const lock = useCallback(async () => {
    setLocking(true);
    try {
      await api.vaultLock();
      await refresh();
      onLocked?.();
    } finally {
      setLocking(false);
    }
  }, [refresh, onLocked]);
  return { lock, locking };
}

// VaultLockedBanner shows only when authenticated-but-locked: a password field
// that re-derives the DEK (a full re-login also unlocks, but this is the cheaper
// path — the JWT cookie survives a restart, the DEK cache does not). Renders
// nothing while unlocked or signed out.
export function VaultLockedBanner() {
  const { user, vaultUnlocked, refresh } = useAuth();
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // Consecutive failed unlocks. The server answers an identical 403 for a wrong
  // password AND for a missing vault row (no oracle) — but a user whose vault row
  // failed to create at register/login has the CORRECT password yet is stuck on
  // "Incorrect password" forever, since UnlockExisting never creates. After a
  // couple of failures we surface the one recovery path (a full re-login re-creates
  // the vault) without weakening the server's indistinguishability.
  const [failures, setFailures] = useState(0);

  if (!user || vaultUnlocked) return null;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      await api.vaultUnlock(password);
      setPassword("");
      setFailures(0);
      await refresh();
    } catch (err) {
      const next = failures + 1;
      setFailures(next);
      // 403 is the deliberate wrong-password / no-vault answer; keep it specific.
      const base =
        err instanceof ApiError && err.status === 403
          ? "Incorrect password."
          : err instanceof ApiError
            ? err.message
            : "Failed to unlock the vault.";
      setError(
        next >= 2
          ? base + " Still locked with the right password? Sign out and back in to re-create your vault."
          : base,
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <div
      role="alert"
      className={cx(
        "mb-6 rounded-lg border px-4 py-3",
        "border-warn/40 bg-warn/10 text-warn",
      )}
    >
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="min-w-0">
          <p className="text-sm font-semibold text-fg">
            <span aria-hidden="true">🔒</span> Vault locked
          </p>
          <p className="mt-0.5 text-sm text-muted">
            Enter your password to resume your agents. While locked, your new runs wait as
            &ldquo;waiting for vault unlock&rdquo; instead of failing — the merge-request review stays
            your gate.
          </p>
        </div>
        <form onSubmit={submit} className="flex shrink-0 items-start gap-2">
          <div className="w-48">
            <Input
              type="password"
              autoComplete="current-password"
              aria-label="Vault password"
              placeholder="Your password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
            {error && <p className="mt-1 text-xs text-danger">{error}</p>}
          </div>
          <Button type="submit" disabled={busy || password.trim() === ""}>
            {busy ? "Unlocking…" : "Unlock"}
          </Button>
        </form>
      </div>
    </div>
  );
}
