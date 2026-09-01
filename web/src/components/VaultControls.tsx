// Vault surface (PRD #32): the header status badge, the locked-state unlock
// banner, and the lock action hook. State rides useAuth().vaultUnlocked (from the
// session payload); unlock/lock call the API then refresh the session so the
// badge, banner, and "waiting for vault unlock" run state all reconcile at once.

import { useCallback, useState, type FormEvent } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError } from "../lib/api";
import { errorMessage } from "../lib/apiError";
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

// VaultLockedBanner is the authenticated-but-locked surface. It dispatches on the
// PRD #45 session bits: a passwordless (SSO) user who has not set a vault passphrase
// yet gets the create dialog; everyone else gets the unlock banner, whose wording
// switches to "passphrase" for passwordless users. Renders nothing while unlocked or
// signed out. The `=== false` checks are deliberate: an undefined bit (older server
// or a partial mock) never falls into the passwordless branch.
export function VaultLockedBanner() {
  const { user, vaultUnlocked, hasPassword, vaultExists } = useAuth();
  if (!user || vaultUnlocked) return null;
  if (hasPassword === false && vaultExists === false) return <VaultPassphraseCreate />;
  return <VaultUnlockBanner credential={hasPassword === false ? "passphrase" : "password"} />;
}

// VaultUnlockBanner re-derives the DEK from the user's password or passphrase (a
// full re-login also unlocks a password user, but this is the cheaper path — the JWT
// cookie survives a restart, the DEK cache does not).
function VaultUnlockBanner({ credential }: { credential: "password" | "passphrase" }) {
  const { refresh } = useAuth();
  const [secret, setSecret] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // Consecutive failed unlocks. The server answers an identical 403 for a wrong
  // secret AND for a missing vault row (no oracle) — but a PASSWORD user whose vault
  // row failed to create at register/login has the CORRECT password yet is stuck on
  // "Incorrect password" forever, since UnlockExisting never creates. After a couple
  // of failures we surface the one recovery path (a full re-login re-creates the
  // vault) without weakening the server's indistinguishability. That recovery does
  // NOT apply to a passphrase user (SSO login never touches the vault; passphrase
  // recovery is a documented limitation), so the hint is password-only.
  const [failures, setFailures] = useState(0);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      await api.vaultUnlock(secret);
      setSecret("");
      setFailures(0);
      await refresh();
    } catch (err) {
      const next = failures + 1;
      setFailures(next);
      const base =
        err instanceof ApiError && err.status === 403
          ? credential === "password"
            ? "Incorrect password."
            : "Incorrect passphrase."
          : err instanceof ApiError
            ? err.message
            : "Failed to unlock the vault.";
      setError(
        next >= 2 && credential === "password"
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
            Enter your {credential} to resume your agents. While locked, your new runs wait as
            &ldquo;waiting for vault unlock&rdquo; instead of failing — the merge-request review stays
            your gate.
          </p>
        </div>
        <form onSubmit={submit} className="flex shrink-0 items-start gap-2">
          <div className="w-48">
            <Input
              type="password"
              autoComplete="current-password"
              aria-label={`Vault ${credential}`}
              placeholder={`Your ${credential}`}
              value={secret}
              onChange={(e) => setSecret(e.target.value)}
            />
            {error && <p className="mt-1 text-xs text-danger">{error}</p>}
          </div>
          <Button type="submit" disabled={busy || secret.trim() === ""}>
            {busy ? "Unlocking…" : "Unlock"}
          </Button>
        </form>
      </div>
    </div>
  );
}

// VAULT_PASSPHRASE_MIN mirrors the server floor (auth.MinPasswordLen); the server
// re-enforces it, this is pre-submit feedback only.
const VAULT_PASSPHRASE_MIN = 12;

// VaultPassphraseCreate is shown to a passwordless (SSO) user who has no vault yet
// (PRD #45, Decision 6): they have no login password for the KEK to derive from, so
// they choose a dedicated vault passphrase. Create-only server-side; on success the
// vault is created and unlocked, and the session refresh clears this banner.
function VaultPassphraseCreate() {
  const { refresh } = useAuth();
  const [passphrase, setPassphrase] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const tooShort = passphrase.length > 0 && passphrase.length < VAULT_PASSPHRASE_MIN;
  const mismatch = confirm.length > 0 && confirm !== passphrase;
  const canSubmit = passphrase.length >= VAULT_PASSPHRASE_MIN && confirm === passphrase && !busy;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    if (passphrase.length < VAULT_PASSPHRASE_MIN) {
      setError(`Passphrase must be at least ${VAULT_PASSPHRASE_MIN} characters.`);
      return;
    }
    if (confirm !== passphrase) {
      setError("Passphrases do not match.");
      return;
    }
    setBusy(true);
    try {
      await api.vaultCreatePassphrase(passphrase);
      setPassphrase("");
      setConfirm("");
      await refresh();
    } catch (err) {
      setError(
        errorMessage(err, "Failed to set your vault passphrase."),
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
      <div className="flex flex-col gap-3">
        <div className="min-w-0">
          <p className="text-sm font-semibold text-fg">
            <span aria-hidden="true">🔑</span> Set a vault passphrase
          </p>
          <p className="mt-0.5 text-sm text-muted">
            You sign in with SSO, so there is no login password to protect your stored secrets
            (like your Anthropic token). Choose a vault passphrase — at least {VAULT_PASSPHRASE_MIN}{" "}
            characters — to encrypt them. You will enter it to unlock your vault after a restart. It
            cannot be recovered if lost, so store it somewhere safe.
          </p>
        </div>
        <form onSubmit={submit} className="flex flex-col gap-2 sm:flex-row sm:items-start">
          <div className="w-56">
            <Input
              type="password"
              autoComplete="new-password"
              aria-label="Vault passphrase"
              placeholder={`New passphrase (min ${VAULT_PASSPHRASE_MIN})`}
              value={passphrase}
              onChange={(e) => setPassphrase(e.target.value)}
            />
            {tooShort && (
              <p className="mt-1 text-xs text-danger">At least {VAULT_PASSPHRASE_MIN} characters.</p>
            )}
          </div>
          <div className="w-56">
            <Input
              type="password"
              autoComplete="new-password"
              aria-label="Confirm vault passphrase"
              placeholder="Confirm passphrase"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
            />
            {mismatch && <p className="mt-1 text-xs text-danger">Passphrases do not match.</p>}
            {error && <p className="mt-1 text-xs text-danger">{error}</p>}
          </div>
          <Button type="submit" disabled={!canSubmit}>
            {busy ? "Saving…" : "Set passphrase"}
          </Button>
        </form>
      </div>
    </div>
  );
}
