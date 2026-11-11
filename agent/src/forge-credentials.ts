// Worker-private installation-token rotation. No App key is ever delivered here.
// A stable claim credential remains an alias so callers that captured it before
// a long model turn still resolve the latest token at the network boundary.
export interface ForgeCredentialResponse { forge_pat: string; expires_at: string }
interface SecretLogger { addSecret(value: string): void; removeSecret(value: string): void }
interface Lease {
  token: string;
  expiry: number;
  values: Set<string>;
  disposed: boolean;
}
const leases = new Map<string, Set<Lease>>();
const SAFETY_MS = 10_000;
const REFRESH_MARGIN_MS = 120_000;
const MAX_REFRESH_MS = 600_000;

function basic(token: string, username?: string): string {
  return Buffer.from(`${username?.trim() || "oauth2"}:${token}`).toString("base64");
}

/** Resolve only immediately before authenticating a network operation. */
export function resolveForgeCredential(value: string | undefined): string | undefined {
  if (!value) return value;
  const candidates = leases.get(value);
  if (!candidates) return value;
  let best: Lease | undefined;
  for (const lease of candidates) {
    if (!lease.disposed && lease.expiry > Date.now() + SAFETY_MS && (!best || lease.expiry > best.expiry)) best = lease;
  }
  if (!best) throw new Error("forge installation credential expired; refresh unavailable");
  return best.token;
}

/** Installation tokens use GitHub's machine credential username, independently of commit attribution. */
export function forgeGitUsername(value: string | undefined, username?: string): string | undefined {
  return value && leases.has(value) ? "x-access-token" : username;
}

/** Local redactors must scrub every issued generation, including its git Basic form. */
export function expandForgeCredentialSecrets(values: Array<string | undefined | null>): Array<string | undefined | null> {
  const expanded = [...values];
  for (const value of values) {
    if (!value) continue;
    for (const lease of leases.get(value) ?? []) expanded.push(...lease.values);
  }
  return expanded;
}

export function beginForgeCredentialLease(
  token: string, expiresAt: string, username: string | undefined,
  refresh: () => Promise<ForgeCredentialResponse>, log: SecretLogger,
) {
  const expiry = Date.parse(expiresAt);
  if (!token || !Number.isFinite(expiry) || expiry <= Date.now() + SAFETY_MS) throw new Error("invalid forge installation credential expiry");
  const lease: Lease = { token, expiry, values: new Set(), disposed: false };
  const remember = (value: string) => {
    for (const secret of [value, basic(value, username), basic(value, "x-access-token")]) {
      if (lease.values.has(secret)) continue;
      lease.values.add(secret);
      log.addSecret(secret);
      const group = leases.get(secret) ?? new Set<Lease>();
      group.add(lease); leases.set(secret, group);
    }
  };
  remember(token);
  let timer: ReturnType<typeof setTimeout> | undefined;
  let inFlight: Promise<boolean> | undefined;
  const schedule = (retry = false) => {
    if (lease.disposed) return;
    const delay = retry ? 10_000 : Math.max(1_000, Math.min(MAX_REFRESH_MS, lease.expiry - Date.now() - REFRESH_MARGIN_MS));
    timer = setTimeout(() => { void renew(); }, delay);
    timer.unref();
  };
  const renew = (): Promise<boolean> => {
    if (lease.disposed) return Promise.resolve(false);
    if (inFlight) return inFlight;
    if (timer) clearTimeout(timer);
    inFlight = (async () => {
      try {
        const response = await Promise.resolve().then(refresh);
        if (lease.disposed) return false;
        const nextExpiry = Date.parse(response.expires_at);
        if (!response.forge_pat || !Number.isFinite(nextExpiry) || nextExpiry <= Date.now() + SAFETY_MS) throw new Error("invalid forge credential response");
        remember(response.forge_pat); // Register before any network use or log can see it.
        lease.token = response.forge_pat;
        lease.expiry = nextExpiry;
        schedule();
        return true;
      } catch {
        // Neither upstream error bodies nor tokens may enter logs.
        schedule(true);
        return false;
      } finally { inFlight = undefined; }
    })();
    return inFlight;
  };
  schedule();
  return {
    refresh: renew,
    release() {
      if (lease.disposed) return;
      lease.disposed = true;
      if (timer) clearTimeout(timer);
      for (const alias of lease.values) {
        const group = leases.get(alias);
        group?.delete(lease);
        if (!group?.size) leases.delete(alias);
      }
      for (const value of lease.values) log.removeSecret(value);
    },
  };
}
