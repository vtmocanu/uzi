// Chip text for a schedule or catalog model setting. Unpinned (null, empty or
// whitespace-only) reads "model: default"; a pinned value reads "model: <v> · pinned".
// The value is user-supplied, so callers render the result as React text only.
export function modelChipText(model: string | null | undefined): string {
  const v = model?.trim();
  return v ? `model: ${v} · pinned` : "model: default";
}
