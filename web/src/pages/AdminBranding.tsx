// Admin → Branding (PRD #685 M2): the runtime instance-branding surface. An admin
// swaps the app mark (top-left logo) and adds a "POWERED BY" brand (text or logo)
// with no redeploy. Admin-only (AdminRoute + admin-only API).
//
// This page works entirely in STRING-space: it reads the six branding config keys
// off getSettings() (the API serves every setting as a string) and writes them back
// through updateSettings(), coercing to bools only for the live preview. Logo BYTES
// never touch the settings surface (Decision D7) — they go through the dedicated
// uploadBrandingLogo/deleteBrandingLogo endpoints, and their presence is read from
// the public branding() endpoint's *_present flags. See the string↔bool split note
// on AppSettings / Branding in lib/api.ts.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  api,
  ApiError,
  type AppSettings,
  type UpdateSettingsPayload,
} from "../lib/api";
import {
  Alert,
  Button,
  Card,
  Field,
  Input,
  SectionTitle,
  Select,
  Skeleton,
  Toggle,
} from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import { FactoryIcon } from "../components/icons";

// The server's raw-image cap (256 KiB); the upload handler rejects anything above
// it. We block over-cap files here too so the admin gets an instant, clear message
// instead of a round-trip 400.
const MAX_LOGO_BYTES = 262144;
// The type allowlist the server enforces; the client refuses others up front.
const ACCEPTED_TYPES = ["image/png", "image/webp", "image/svg+xml"];
const ACCEPT_ATTR = ".png,.webp,.svg,image/png,image/webp,image/svg+xml";

type Slot = "app" | "brand";

export function AdminBranding() {
  // The last-saved snapshot, for dirty tracking and revert-on-load.
  const [saved, setSaved] = useState<AppSettings | null>(null);
  const [appLogoMode, setAppLogoMode] = useState("default");
  const [appLogoKeepName, setAppLogoKeepName] = useState(true);
  const [brandMode, setBrandMode] = useState("none");
  const [brandCompany, setBrandCompany] = useState("");
  const [brandPlacement, setBrandPlacement] = useState("below");
  const [brandPlaque, setBrandPlaque] = useState(false);
  // Whether an uploaded asset exists for each slot (from branding()'s *_present).
  const [appPresent, setAppPresent] = useState(false);
  const [brandPresent, setBrandPresent] = useState(false);
  // Bumped after every upload/delete so the preview <img> re-fetches past the cache.
  const [logoRev, setLogoRev] = useState(0);

  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const applySettings = useCallback((settings: AppSettings) => {
    setSaved(settings);
    setAppLogoMode(settings.app_logo_mode);
    setAppLogoKeepName(settings.app_logo_keep_name === "true");
    setBrandMode(settings.brand_mode);
    setBrandCompany(settings.brand_company);
    setBrandPlacement(settings.brand_placement);
    setBrandPlaque(settings.brand_plaque === "true");
  }, []);

  const load = useCallback(async () => {
    try {
      const [resp, brand] = await Promise.all([api.getSettings(), api.branding()]);
      applySettings(resp.settings);
      setAppPresent(brand.app_logo_present);
      setBrandPresent(brand.brand_logo_present);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load branding");
    } finally {
      setLoading(false);
    }
  }, [applySettings]);

  useEffect(() => {
    load();
  }, [load]);

  const dirty =
    saved !== null &&
    (appLogoMode !== saved.app_logo_mode ||
      (appLogoKeepName ? "true" : "false") !== saved.app_logo_keep_name ||
      brandMode !== saved.brand_mode ||
      brandCompany !== saved.brand_company ||
      brandPlacement !== saved.brand_placement ||
      (brandPlaque ? "true" : "false") !== saved.brand_plaque);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    setBusy(true);
    try {
      // Send ONLY the six branding keys — the rest of the settings surface is owned
      // by the Instance tab and untouched here.
      const payload: UpdateSettingsPayload = {
        app_logo_mode: appLogoMode,
        app_logo_keep_name: appLogoKeepName ? "true" : "false",
        brand_mode: brandMode,
        brand_company: brandCompany,
        brand_placement: brandPlacement,
        brand_plaque: brandPlaque ? "true" : "false",
      };
      const resp = await api.updateSettings(payload);
      applySettings(resp.settings);
      setNotice("Branding saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save branding");
    } finally {
      setBusy(false);
    }
  };

  // Validate the picked file client-side, then upload it. An over-cap or
  // wrong-type file is rejected with a visible message and never reaches the API.
  const pickLogo = async (slot: Slot, file: File | undefined) => {
    if (!file) return;
    setError("");
    setNotice("");
    if (!ACCEPTED_TYPES.includes(file.type)) {
      setError("Logo must be a PNG, WebP or SVG image.");
      return;
    }
    if (file.size > MAX_LOGO_BYTES) {
      setError("Logo is too large — the maximum size is 256 KiB (262144 bytes).");
      return;
    }
    setBusy(true);
    try {
      await api.uploadBrandingLogo(slot, file);
      if (slot === "app") setAppPresent(true);
      else setBrandPresent(true);
      setLogoRev((r) => r + 1);
      setNotice("Logo uploaded.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to upload logo");
    } finally {
      setBusy(false);
    }
  };

  const clearLogo = async (slot: Slot) => {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      await api.deleteBrandingLogo(slot);
      if (slot === "app") setAppPresent(false);
      else setBrandPresent(false);
      setLogoRev((r) => r + 1);
      setNotice("Logo removed.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to remove logo");
    } finally {
      setBusy(false);
    }
  };

  // Preview asset URLs. An uploaded asset loads from its slot route (cache-busted
  // by logoRev); a custom app mark with no upload falls back to the shipped preset,
  // matching the chrome's "enable-with-one-click" behavior.
  const appLogoSrc = appPresent
    ? `/api/branding/logo/app?v=${logoRev}`
    : "/brand-default.svg";
  const brandLogoSrc = brandPresent
    ? `/api/branding/logo/brand?v=${logoRev}`
    : "/brand-default.svg";

  return (
    <AdminShell description="Replace the app mark and add a POWERED BY brand. Fresh installs are unbranded; the license/author credit in the chrome is fixed and cannot be removed here.">
      {loading ? (
        <Card>
          <Skeleton className="h-40 w-full" />
        </Card>
      ) : (
        <form onSubmit={save} className="space-y-6">
          {error && <Alert message={error} />}
          {notice && <Alert message={notice} tone="success" />}

          <div className="grid gap-6 lg:grid-cols-[1fr_20rem]">
            <div className="space-y-6">
              {/* ── App logo ─────────────────────────────────────────── */}
              <Card className="space-y-4">
                <SectionTitle>App logo</SectionTitle>
                <p className="text-sm text-muted">
                  The mark in the top-left of the sidebar. Custom mode swaps the uzi
                  factory icon for your own; keeping the name co-brands, dropping it
                  is a full white-label.
                </p>
                <Field label="Mode" htmlFor="app-logo-mode">
                  <Select
                    id="app-logo-mode"
                    value={appLogoMode}
                    onChange={(e) => setAppLogoMode(e.target.value)}
                  >
                    <option value="default">Default (uzi factory icon)</option>
                    <option value="custom">Custom logo</option>
                  </Select>
                </Field>
                {appLogoMode === "custom" && (
                  <>
                    <Field label="Logo image (PNG, WebP or SVG, max 256 KiB)" htmlFor="app-logo-file">
                      <Input
                        id="app-logo-file"
                        type="file"
                        accept={ACCEPT_ATTR}
                        aria-label="Upload app logo"
                        disabled={busy}
                        onChange={(e) => {
                          void pickLogo("app", e.target.files?.[0]);
                          e.target.value = "";
                        }}
                      />
                    </Field>
                    <div className="flex items-center gap-3">
                      <span className="text-sm text-muted">
                        {appPresent ? "Custom logo uploaded." : "No logo uploaded — the preset is shown."}
                      </span>
                      {appPresent && (
                        <Button
                          type="button"
                          variant="danger"
                          size="sm"
                          disabled={busy}
                          onClick={() => void clearLogo("app")}
                        >
                          Remove logo
                        </Button>
                      )}
                    </div>
                    <label className="flex items-center gap-3">
                      <Toggle
                        checked={appLogoKeepName}
                        onChange={setAppLogoKeepName}
                        label="Keep the app name next to the logo"
                      />
                      <span className="text-sm text-fg">Keep the app name next to the logo</span>
                    </label>
                  </>
                )}
              </Card>

              {/* ── POWERED BY ───────────────────────────────────────── */}
              <Card className="space-y-4">
                <SectionTitle>POWERED BY</SectionTitle>
                <p className="text-sm text-muted">
                  An optional co-brand in the sidebar — a company name or a logo,
                  placed under the wordmark or tucked top-right of the header.
                </p>
                <Field label="Mode" htmlFor="brand-mode">
                  <Select
                    id="brand-mode"
                    value={brandMode}
                    onChange={(e) => setBrandMode(e.target.value)}
                  >
                    <option value="none">None</option>
                    <option value="text">Text</option>
                    <option value="logo">Logo</option>
                  </Select>
                </Field>
                {brandMode === "text" && (
                  <Field label="Company name" htmlFor="brand-company">
                    <Input
                      id="brand-company"
                      value={brandCompany}
                      maxLength={64}
                      placeholder="Acme, Inc."
                      onChange={(e) => setBrandCompany(e.target.value)}
                    />
                  </Field>
                )}
                {brandMode === "logo" && (
                  <>
                    <Field label="Logo image (PNG, WebP or SVG, max 256 KiB)" htmlFor="brand-logo-file">
                      <Input
                        id="brand-logo-file"
                        type="file"
                        accept={ACCEPT_ATTR}
                        aria-label="Upload brand logo"
                        disabled={busy}
                        onChange={(e) => {
                          void pickLogo("brand", e.target.files?.[0]);
                          e.target.value = "";
                        }}
                      />
                    </Field>
                    <div className="flex items-center gap-3">
                      <span className="text-sm text-muted">
                        {brandPresent ? "Brand logo uploaded." : "No logo uploaded — the preset is shown."}
                      </span>
                      {brandPresent && (
                        <Button
                          type="button"
                          variant="danger"
                          size="sm"
                          disabled={busy}
                          onClick={() => void clearLogo("brand")}
                        >
                          Remove logo
                        </Button>
                      )}
                    </div>
                    <Field label="Placement" htmlFor="brand-placement">
                      <Select
                        id="brand-placement"
                        value={brandPlacement}
                        onChange={(e) => setBrandPlacement(e.target.value)}
                      >
                        <option value="below">Below the wordmark</option>
                        <option value="topright">Top-right of the header</option>
                      </Select>
                    </Field>
                    <label className="flex items-center gap-3">
                      <Toggle
                        checked={brandPlaque}
                        onChange={setBrandPlaque}
                        label="Show a light plaque behind the logo"
                      />
                      <span className="text-sm text-fg">
                        Show a light plaque behind the logo (for dark-ink uploads)
                      </span>
                    </label>
                  </>
                )}
              </Card>
            </div>

            {/* ── Live preview ───────────────────────────────────────── */}
            <div className="space-y-3">
              <SectionTitle>Preview</SectionTitle>
              <BrandingPreview
                appLogoMode={appLogoMode}
                appLogoKeepName={appLogoKeepName}
                appLogoSrc={appLogoSrc}
                brandMode={brandMode}
                brandCompany={brandCompany}
                brandPlacement={brandPlacement}
                brandPlaque={brandPlaque}
                brandLogoSrc={brandLogoSrc}
              />
            </div>
          </div>

          <div className="flex items-center gap-3">
            <Button type="submit" disabled={busy || !dirty}>
              {busy ? "Saving…" : "Save branding"}
            </Button>
            {dirty && <span className="text-sm text-faint">Unsaved changes</span>}
          </div>
        </form>
      )}
    </AdminShell>
  );
}

// BrandingPreview is a SIMPLIFIED, inline rendering of the sidebar header chunk that
// reflects the current (unsaved) selections. It does not import AppShell — it only
// needs to show the mark, the name, and the POWERED BY block the way the mock spells
// them out (logo via <img>, 0.8 dim on the POWERED BY logo, ~24/26px heights, an
// optional light plaque). The chrome itself (M3a/M3b) is the source of truth for the
// real rendering; this is the operator's in-app equivalent of the design mock.
function BrandingPreview({
  appLogoMode,
  appLogoKeepName,
  appLogoSrc,
  brandMode,
  brandCompany,
  brandPlacement,
  brandPlaque,
  brandLogoSrc,
}: {
  appLogoMode: string;
  appLogoKeepName: boolean;
  appLogoSrc: string;
  brandMode: string;
  brandCompany: string;
  brandPlacement: string;
  brandPlaque: boolean;
  brandLogoSrc: string;
}) {
  const custom = appLogoMode === "custom";
  const showName = !custom || appLogoKeepName;
  const brandLogo = brandMode === "logo";
  const topRight = brandLogo && brandPlacement === "topright";

  return (
    <div className="rounded-xl border border-edge bg-surface p-4" data-testid="branding-preview">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className="flex h-[38px] w-[38px] items-center justify-center rounded-lg bg-raised">
            {custom ? (
              <img
                src={appLogoSrc}
                alt="app logo"
                className="h-[26px] w-[26px] object-contain"
              />
            ) : (
              <FactoryIcon className="h-[22px] w-[22px] text-brand" />
            )}
          </span>
          {showName && (
            <span className="leading-tight">
              <span className="block text-sm font-semibold text-fg">uzi</span>
              <span className="block text-xs text-muted">uzinele întunecate</span>
            </span>
          )}
        </div>
        {/* Top-right POWERED BY: logo only, ~26px, no label. */}
        {topRight && (
          <span
            className={brandPlaque ? "rounded-md bg-[#f6f6f8] px-1.5 py-1" : undefined}
          >
            <img
              src={brandLogoSrc}
              alt="brand logo"
              style={{ opacity: 0.8 }}
              className="h-[26px] w-auto object-contain"
            />
          </span>
        )}
      </div>

      {/* Below-wordmark POWERED BY: a faint label + text or logo. */}
      {brandMode !== "none" && !topRight && (
        <div className="mt-3 border-t border-edge pt-3">
          <span className="block text-[10px] font-medium uppercase tracking-wider text-faint">
            Powered by
          </span>
          {brandMode === "text" ? (
            <span className="mt-1 block text-sm text-fg">{brandCompany}</span>
          ) : (
            <span
              className={
                brandPlaque
                  ? "mt-1 inline-block rounded-md bg-[#f6f6f8] px-1.5 py-1"
                  : "mt-1 inline-block"
              }
            >
              <img
                src={brandLogoSrc}
                alt="brand logo"
                style={{ opacity: 0.8 }}
                className="h-[24px] w-auto object-contain"
              />
            </span>
          )}
        </div>
      )}

      {/* The durable, non-editable license/author credit (Decision D3). */}
      <div className="mt-3 font-mono text-[10px] text-faint">MIT © Vlad Mocanu</div>
    </div>
  );
}
