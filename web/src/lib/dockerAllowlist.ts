// Match google/uuid.Parse v1.6.0, including arbitrary single-byte wrappers.
// ASCII wrapper checks account for Go's UTF-8 byte length versus JS's UTF-16 length.
export function uuidIdentity(value: string): string | null {
  let s = value;
  if (s.length === 45 && /^urn:uuid:/i.test(s)) s = s.slice(9);
  else if (s.length === 38 && s.charCodeAt(0) <= 0x7f && s.charCodeAt(37) <= 0x7f) s = s.slice(1, 37);
  if (/^[0-9a-f]{32}$/i.test(s)) {
    s = s.slice(0, 8) + "-" + s.slice(8, 12) + "-" + s.slice(12, 16) + "-" + s.slice(16, 20) + "-" + s.slice(20);
  }
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(s) ? s.toLowerCase() : null;
}

// strings.TrimSpace uses Unicode White_Space; JS trim also strips BOM and misses NEL.
export function trimAllowlistToken(value: string): string {
  return value.replace(/^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/g, "");
}
