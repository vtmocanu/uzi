// Client-side mirror of the server's registration domain check (handler/auth.go).
// Purely for pre-submit UX — the server remains authoritative — so this stays
// deliberately simple: the browser <input type="email"> constrains the format,
// and we only need the domain after the final '@'.

// emailDomain returns the lowercased domain of an email (the part after its final
// '@'), or "" if there is none.
export function emailDomain(email: string): string {
  const at = email.lastIndexOf("@");
  if (at < 0) return "";
  return email.slice(at + 1).trim().toLowerCase();
}

// emailDomainAllowed reports whether email's domain is permitted by the
// allowlist. An empty allowlist permits every domain (matches the server). Match
// is exact and case-insensitive — no subdomain wildcards.
export function emailDomainAllowed(email: string, allowed: string[]): boolean {
  if (allowed.length === 0) return true;
  const d = emailDomain(email);
  return allowed.some((a) => a.trim().toLowerCase() === d);
}
