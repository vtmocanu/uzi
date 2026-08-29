// The web-owned named-logo preset catalog (PRD #780 D1). This module is the SINGLE
// source of truth for the built-in app-logo presets: both the chrome (AppShell) and
// the admin picker resolve a slug through here, so neither hardcodes a preset path
// and the admin preview cannot drift from what the chrome renders (D1).
//
// The server stores only a slug and validates its *shape* (D2); the mapping slug →
// asset lives here. Adding a preset later is one entry below plus one file in
// `web/public/brand-presets/` — no backend, schema, or migration.
//
// Resolution is a MAP LOOKUP only, never string interpolation of the slug into a
// path: an unknown/empty/hostile slug can never traverse and degrades to the stock
// uzi mark (D2/R3).

export interface BrandPreset {
  slug: string;
  label: string;
  asset: string;
}

// Ordered catalog. The picker renders these tiles in order (M3). Currently one
// entry; future presets are appended here with their shipped asset. The catalog is
// D1's public surface for the admin picker (M3); the M2 chrome consumes it only
// through `presetAssetForSlug`, so knip's error-tier unused-exports gate would flag
// it until M3 imports it. The @public tag suppresses that, not a de-export: the
// array IS the single source of truth this module exists to expose.
/** @public */
export const BRAND_PRESETS: readonly BrandPreset[] = [
  { slug: "metaminds", label: "Metaminds", asset: "/brand-presets/metaminds.svg" },
];

// slug → preset, built once at module load. The lookup key space is exactly the
// catalog slugs, so an attacker-supplied slug can only ever miss.
const PRESET_BY_SLUG: Map<string, BrandPreset> = new Map(
  BRAND_PRESETS.map((p) => [p.slug, p]),
);

// The preset for a slug, or null when the slug is empty/unknown. Exposed for the
// admin picker (M3), which resolves the selected tile through this; M2 reaches it
// only via `presetAssetForSlug`, so the @public tag keeps knip's unused-exports
// gate off it before M3 lands.
/** @public */
export function presetForSlug(slug: string | null | undefined): BrandPreset | null {
  if (!slug) return null;
  return PRESET_BY_SLUG.get(slug) ?? null;
}

// The preset asset path for a slug, or null when empty/unknown (chrome degrades to
// the FactoryIcon). Map lookup only — the slug is never interpolated into a path.
export function presetAssetForSlug(slug: string | null | undefined): string | null {
  return presetForSlug(slug)?.asset ?? null;
}
