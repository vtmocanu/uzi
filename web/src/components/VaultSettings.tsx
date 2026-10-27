import { useState, type FormEvent } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { emitSidebarTokensChanged } from "../lib/sidebarTokens";
import { prefs } from "../lib/prefs";
import { Alert, Button, Card, Input, SectionTitle } from "./ui";
import { VaultBadge, useVaultLock } from "./VaultControls";

export function CredentialStorageHint() {
  const { vaultMode, hasPassword } = useAuth();
  return <p className="text-xs text-faint">{vaultMode === "unprotected"
    ? "Encrypted with the server key, like forge bot tokens. Your login password does not protect these credentials."
    : `Encrypted with your ${hasPassword === false ? "vault passphrase" : "login password"}. If you forget it, these credentials cannot be recovered and must be re-entered.`}</p>;
}

export function VaultIntroduction() {
  const { user, vaultMode } = useAuth();
  const key = `uzi.vault.introduction.${user?.id ?? "anonymous"}`;
  const [dismissed, setDismissed] = useState(() => prefs.get(key, false));
  if (vaultMode !== "unprotected" || dismissed) return null;
  return <Card>
    <SectionTitle>Choose credential protection</SectionTitle>
    <p>Your Claude, OpenAI and Codex credentials currently use server-key encryption. You can add password protection with the vault below.</p>
    <Button variant="secondary" onClick={() => { prefs.set(key, true); setDismissed(true); }}>Dismiss vault introduction</Button>
  </Card>;
}

export function VaultSettings({ onChanged }: { onChanged: () => Promise<void> }) {
  const { vaultMode, vaultPolicy, vaultUnlocked, hasPassword, refresh } = useAuth();
  const off = vaultMode === "unprotected";
  const required = vaultPolicy !== "optional";
  const [enrolling, setEnrolling] = useState(false);
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const { lock, locking } = useVaultLock(() => setNotice("Vault locked. Runs already in flight finish; new runs wait until you unlock from the banner above."));
  const changed = async () => {
    await refresh(true);
    emitSidebarTokensChanged();
    await onChanged();
  };
  const enable = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true); setError(""); setNotice("");
    try {
      const result = await api.vaultEnable(password);
      setPassword(""); setConfirm(""); setEnrolling(false);
      setNotice(result.rotation_advice || "Protection is on. Older database backups may still contain server-key encrypted credentials. Rotate credentials for protection against those backups.");
      await changed();
    } catch (err) {
      setError(err instanceof ApiError && err.status === 403 ? "Incorrect login password. Protection was not enabled." : errorMessage(err, "Failed to enable protection."));
    } finally { setBusy(false); }
  };
  const disable = async () => {
    setBusy(true); setError(""); setNotice("");
    try {
      await api.vaultDisable();
      setNotice("Protection is off. Credentials now use server-key encryption.");
      await changed();
    } catch (err) { setError(errorMessage(err, "Failed to disable protection.")); }
    finally { setBusy(false); }
  };
  const passphraseValid = hasPassword !== false || (
    new TextEncoder().encode(password).length >= 12 &&
    new TextEncoder().encode(password).length <= 1024 && password === confirm
  );
  return <Card className="space-y-4">
    <SectionTitle>Protect my credentials with my password (vault)</SectionTitle>
    <p className="text-sm text-muted">TL;DR: On encrypts your Claude, OpenAI and Codex credentials with your login password (or a dedicated passphrase for SSO). It protects against theft of the database together with the server key. It cannot protect against a compromised running server while unlocked. On costs an unlock after each server release or restart; new runs wait until you unlock. Off uses the server key, just like forge bot tokens: theft of the database and server key exposes the credentials. <a href="/docs/anthropic-token#vault-choice">Learn more</a>.</p>
    {off && required && <Alert tone="warning" message="Your administrator requires credential protection. Enable your vault to enroll; protection is still off until you do." />}
    {error && <Alert message={error} />}
    {notice && <Alert tone="info" message={notice} />}
    <VaultBadge />
    {off ? <Button disabled={busy || enrolling} onClick={() => setEnrolling(true)}>Enable protection</Button> : <>
      {vaultUnlocked ? <Button variant="secondary" disabled={busy || locking} onClick={() => void lock().catch(err => setError(errorMessage(err, "Failed to lock vault.")))}>Lock vault</Button> : <p>Unlock your vault from the banner above before disabling protection.</p>}
      <Button variant="secondary" disabled={busy || required || !vaultUnlocked} onClick={disable}>Disable protection</Button>
      {required && <p>Off is unavailable because your administrator requires credential protection.</p>}
    </>}
    {off && enrolling && <form onSubmit={enable} className="space-y-3">
      <label>{hasPassword === false ? "New vault passphrase (12–1024 bytes)" : "Verify login password"}
        <Input type="password" autoComplete={hasPassword === false ? "new-password" : "current-password"} aria-label={hasPassword === false ? "New vault passphrase" : "Verify login password"} value={password} onChange={e => setPassword(e.target.value)} />
      </label>
      {hasPassword === false && <label>Confirm vault passphrase<Input type="password" autoComplete="new-password" aria-label="Confirm new vault passphrase" value={confirm} onChange={e => setConfirm(e.target.value)} /></label>}
      <Button type="submit" disabled={busy || !password || !passphraseValid}>{busy ? "Enabling…" : "Confirm enable protection"}</Button>
      <Button variant="secondary" disabled={busy} onClick={() => { setEnrolling(false); setPassword(""); setConfirm(""); }}>Cancel</Button>
    </form>}
  </Card>;
}
