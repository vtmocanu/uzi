// White-label browser-tab <title> (issue #688 M2): the tab title reflects the
// instance's brand when it is FULLY white-labeled, so a branded deployment reads
// as itself in the tab bar (and in bookmarks / history) rather than as "uzi".
// index.html carries the static default for pre-hydration and the unbranded case;
// AppShell overrides document.title from this at runtime once branding resolves.

import type { Branding } from "./api";

// The static default the SPA overrides at runtime. MUST stay byte-identical to the
// <title> in web/index.html:6 — the dash is a U+2014 EM DASH (—) with a normal
// ASCII space on each side, NOT a hyphen and NOT an en dash.
export const DEFAULT_TITLE = "Uzi — AI dark factory";

// brandTabTitle picks the tab title from branding. It reuses brand_company (already
// in the /api/branding response) as the title source per the issue's 2026-08-29
// triage steer — no new setting. Only a FULL white-label overrides the default:
// mirroring the appMarkShowName gate in AppShell.tsx, that is a non-default logo mode
// (custom or preset) with keep-name OFF. A co-brand (keep_name=true) keeps the uzi
// title; a white-label with an empty brand_company falls back to DEFAULT_TITLE (no
// other text source exists); `null`/pending branding is the default too.
export function brandTabTitle(branding: Branding | null): string {
  const isWhiteLabel =
    branding != null && branding.app_logo_mode !== "default" && branding.app_logo_keep_name === false;
  return isWhiteLabel && branding.brand_company.trim() !== "" ? branding.brand_company : DEFAULT_TITLE;
}
