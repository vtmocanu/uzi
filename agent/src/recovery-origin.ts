/** A deny-only classification, never authentication or permission to produce. */
export type RecoveryOrigin = "ordinary" | "producer" | "malformed";

/**
 * Call only after the existing recovery record MAC has been authenticated.
 * Producer grammar is exactly maintenance-v1- followed by a lowercase v4 UUID
 * (51 ASCII characters). Identity is never trimmed, repaired or case folded.
 * The reserved family is ASCII-case-insensitive "maintenance" plus any suffix,
 * or any nonempty truncated prefix of that namespace itself. Thus "maintenan",
 * "maintenance" and "maintenance-v1-" are malformed, never ordinary first use.
 */
export function classifyRecoveryOrigin(captureId: string): RecoveryOrigin {
  if (captureId.length === 51 &&
    /^maintenance-v1-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}(?![\s\S])/.test(captureId))
    return "producer";
  // Fold ASCII letters only for deny-family detection; Unicode folding could
  // normalize a different identity into the reserved namespace.
  const family = captureId.replace(/[A-Z]/g, letter => letter.toLowerCase());
  return family.startsWith("maintenance") || (family.length > 0 && "maintenance".startsWith(family))
    ? "malformed" : "ordinary";
}
