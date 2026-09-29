// Client-side mirror of the server's product-text gate (PRD #1907): a product name, a
// product description and a product-token name are trimmed, capped in UTF-8 BYTES (Go's
// len), and refused when they hold any control or invisible formatting character
// (termsafe.Validate: unicode.IsControl || Cf, newline and tab included, because these
// strings are printed in listings where a line break could forge a row). The server stays
// the authority; this only turns a guaranteed 400 into an inline hint before submit.

import { byteLength } from "./skills";

// `u` is load-bearing: without it `\p{Cc}` is a class of literal letters (safeText.ts).
const UNSAFE_CHAR = /[\p{Cc}\p{Cf}]/u;

export const PRODUCT_NAME_MAX_BYTES = 200;
export const PRODUCT_DESCRIPTION_MAX_BYTES = 1000;

export function hasUnsafeProductChar(s: string): boolean {
  return UNSAFE_CHAR.test(s);
}

// The problem with `value` as the user would read it, or null when the server will accept
// it. `field` is the sentence-case label ("Name", "Description"). An empty value is not an
// error here: callers gate a required field on emptiness themselves, without nagging
// before the user has typed.
export function productTextError(field: string, value: string, maxBytes: number): string | null {
  const v = value.trim();
  if (hasUnsafeProductChar(v)) {
    return `${field} can’t contain tabs, line breaks or invisible formatting characters.`;
  }
  const bytes = byteLength(v);
  if (bytes > maxBytes) {
    return `${field} is ${bytes} bytes; the limit is ${maxBytes}. Accented letters and emoji count as 2 to 4 bytes each.`;
  }
  return null;
}
