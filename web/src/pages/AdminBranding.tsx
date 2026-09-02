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
// on AppSettings / Branding in lib/apiTypes.ts.

import {
  useCallback,
  useRef,
  useState,
  type FormEvent,
  type KeyboardEvent,
} from "react";
import {
  api,
  type AppSettings,
  type UpdateSettingsPayload,
} from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
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
import { FactoryIcon, PlusIcon } from "../components/icons";
import { licenseCreditEnabled } from "../lib/flags";
import {
  BRAND_PRESETS,
  presetAssetForSlug,
  presetForSlug,
} from "../lib/brandPresets";

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
  const [appLogoPreset, setAppLogoPreset] = useState("");
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

  const [busy, setBusy] = useState(false);
  // Kept local: the save / upload / clear handlers below still set this, so it is
  // merged with the hook's load error at the one page-level Alert.
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const applySettings = useCallback((settings: AppSettings) => {
    setSaved(settings);
    setAppLogoMode(settings.app_logo_mode);
    setAppLogoPreset(settings.app_logo_preset);
    setAppLogoKeepName(settings.app_logo_keep_name === "true");
    setBrandMode(settings.brand_mode);
    setBrandCompany(settings.brand_company);
    setBrandPlacement(settings.brand_placement);
    setBrandPlaque(settings.brand_plaque === "true");
  }, []);

  // Every loaded value is editable/derived form state the handlers below also write,
  // so the fetcher seeds them as side effects exactly as the old load did; the hook
  // only owns loading + the load error.
  const { loading, error: loadError } = useAsyncData(
    async ({ isCurrent }) => {
      const [resp, brand] = await Promise.all([api.getSettings(), api.branding()]);
      if (!isCurrent()) return;
      applySettings(resp.settings);
      setAppPresent(brand.app_logo_present);
      setBrandPresent(brand.brand_logo_present);
    },
    [applySettings],
    { fallback: "Failed to load branding" },
  );

  const dirty =
    saved !== null &&
    (appLogoMode !== saved.app_logo_mode ||
      appLogoPreset !== saved.app_logo_preset ||
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
        app_logo_preset: appLogoPreset,
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
      setError(errorMessage(err, "Failed to save branding"));
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
      setError(errorMessage(err, "Failed to upload logo"));
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
      setError(errorMessage(err, "Failed to remove logo"));
    } finally {
      setBusy(false);
    }
  };

  // App-mark preview source, mirroring AppShell.appMarkImgSrc's three-mode logic so
  // the preview never diverges from the chrome: `preset` resolves the slug through
  // the shared brandPresets registry (unknown/empty → FactoryIcon); `custom` serves
  // the uploaded asset (cache-busted by logoRev) or the FactoryIcon when none is
  // uploaded — there is NO /brand-default.svg fallback for the app slot; `default`
  // is the FactoryIcon. A null src means "render the FactoryIcon".
  const appMarkSrc: string | null =
    appLogoMode === "preset"
      ? presetAssetForSlug(appLogoPreset)
      : appLogoMode === "custom"
        ? appPresent
          ? `/api/branding/logo/app?v=${logoRev}`
          : null
        : null;
  // The POWERED BY brand slot keeps its shipped-preset fallback (unchanged).
  const brandLogoSrc = brandPresent
    ? `/api/branding/logo/brand?v=${logoRev}`
    : "/brand-default.svg";

  // App-logo tile picker (M3): a radiogroup of "uzi" (default), one tile per catalog
  // preset, then "Custom". Selecting a tile sets mode + preset atomically. The
  // selected key is derived from state so it stays in sync with load/revert and the
  // preview; an unknown preset slug leaves nothing selected (degrades visually).
  type AppLogoTile = {
    key: string;
    label: string;
    mode: "default" | "preset" | "custom";
    slug: string;
  };
  const appLogoTiles: AppLogoTile[] = [
    { key: "default", label: "uzi", mode: "default", slug: "" },
    ...BRAND_PRESETS.map((p) => ({
      key: p.slug,
      label: p.label,
      mode: "preset" as const,
      slug: p.slug,
    })),
    { key: "custom", label: "Custom", mode: "custom", slug: "" },
  ];
  const selectedTileKey =
    appLogoMode === "custom"
      ? "custom"
      : appLogoMode === "preset"
        ? (presetForSlug(appLogoPreset)?.slug ?? "")
        : "default";
  const selectAppLogoTile = (tile: AppLogoTile) => {
    if (tile.mode === "preset") {
      setAppLogoMode("preset");
      setAppLogoPreset(tile.slug);
    } else if (tile.mode === "custom") {
      setAppLogoMode("custom");
      setAppLogoPreset("");
    } else {
      setAppLogoMode("default");
      setAppLogoPreset("");
    }
  };
  // Roving-tabindex radiogroup (WAI-ARIA APG radio pattern): exactly one tile is a Tab
  // stop (the selected one; the first tile when nothing is selected, e.g. mode=preset
  // with an unknown/empty slug), and arrow/Home/End keys move selection AND DOM focus
  // between tiles without leaving the group. Refs let a key-move focus the destination.
  const tileRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const selectedTileIndex = appLogoTiles.findIndex(
    (t) => t.key === selectedTileKey,
  );
  const rovingTabIndex = selectedTileIndex < 0 ? 0 : selectedTileIndex;
  const onTileKeyDown = (e: KeyboardEvent<HTMLButtonElement>, index: number) => {
    const last = appLogoTiles.length - 1;
    let next: number | null = null;
    switch (e.key) {
      case "ArrowRight":
      case "ArrowDown":
        next = index === last ? 0 : index + 1;
        break;
      case "ArrowLeft":
      case "ArrowUp":
        next = index === 0 ? last : index - 1;
        break;
      case "Home":
        next = 0;
        break;
      case "End":
        next = last;
        break;
      default:
        return;
    }
    e.preventDefault();
    selectAppLogoTile(appLogoTiles[next]);
    tileRefs.current[next]?.focus();
  };

  return (
    <AdminShell description="Replace the app mark and add a POWERED BY brand. Fresh installs are unbranded; the license/author credit is controlled by a build-time flag, not a branding setting, so it cannot be changed here.">
      {loading ? (
        <Card>
          <Skeleton className="h-40 w-full" />
        </Card>
      ) : (
        <form onSubmit={save} className="space-y-6">
          {(error || loadError) && <Alert message={error || loadError} />}
          {notice && <Alert message={notice} tone="success" />}

          <div className="grid gap-6 lg:grid-cols-[1fr_20rem]">
            <div className="space-y-6">
              {/* ── App logo ─────────────────────────────────────────── */}
              <Card className="space-y-4">
                <SectionTitle>App logo</SectionTitle>
                <p className="text-sm text-muted">
                  The mark in the top-left of the sidebar. Pick a built-in mark, or
                  Custom to upload your own; keeping the name co-brands, dropping it
                  is a full white-label.
                </p>
                <div
                  role="radiogroup"
                  aria-label="App logo"
                  className="flex flex-wrap gap-3"
                >
                  {appLogoTiles.map((tile, index) => {
                    const selected = tile.key === selectedTileKey;
                    const presetSrc =
                      tile.mode === "preset" ? presetAssetForSlug(tile.slug) : null;
                    return (
                      <button
                        key={tile.key}
                        ref={(el) => {
                          tileRefs.current[index] = el;
                        }}
                        type="button"
                        role="radio"
                        aria-checked={selected}
                        aria-label={tile.label}
                        tabIndex={index === rovingTabIndex ? 0 : -1}
                        onClick={() => selectAppLogoTile(tile)}
                        onKeyDown={(e) => onTileKeyDown(e, index)}
                        className={
                          "flex w-24 flex-col items-center gap-2 rounded-lg border p-3 text-center transition " +
                          (selected
                            ? "border-brand ring-2 ring-brand"
                            : "border-edge hover:border-brand/50")
                        }
                      >
                        <span className="flex h-[38px] w-[38px] items-center justify-center rounded-lg bg-raised">
                          {tile.mode === "default" ? (
                            <FactoryIcon className="h-[22px] w-[22px] text-brand" />
                          ) : tile.mode === "preset" && presetSrc ? (
                            <img
                              src={presetSrc}
                              alt=""
                              className="h-[26px] w-[26px] object-contain"
                            />
                          ) : (
                            <PlusIcon className="h-[20px] w-[20px] text-muted" />
                          )}
                        </span>
                        <span className="text-xs font-medium text-fg">
                          {tile.label}
                        </span>
                      </button>
                    );
                  })}
                </div>
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
                    {appPresent && (
                      <div className="flex items-center gap-3">
                        <span className="text-sm text-muted">
                          Custom logo uploaded.
                        </span>
                        <Button
                          type="button"
                          variant="danger"
                          size="sm"
                          disabled={busy}
                          onClick={() => void clearLogo("app")}
                        >
                          Remove logo
                        </Button>
                      </div>
                    )}
                  </>
                )}
                {(appLogoMode === "custom" || appLogoMode === "preset") && (
                  <div className="flex items-center gap-3">
                    <Toggle
                      checked={appLogoKeepName}
                      onChange={setAppLogoKeepName}
                      label="Keep the app name next to the logo"
                    />
                    <span aria-hidden="true" className="text-sm text-fg">
                      Keep the app name next to the logo
                    </span>
                  </div>
                )}
              </Card>

              {/* ── POWERED BY ───────────────────────────────────────── */}
              <Card className="space-y-4">
                <SectionTitle>POWERED BY</SectionTitle>
                <p className="text-sm text-muted">
                  An optional co-brand in the sidebar — a company name or a logo,
                  placed under the wordmark or tucked top-right of the header.
                </p>
                <Field label="POWERED BY mode" htmlFor="brand-mode">
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
                    <div className="flex items-center gap-3">
                      <Toggle
                        checked={brandPlaque}
                        onChange={setBrandPlaque}
                        label="Show a light plaque behind the logo (for dark-ink uploads)"
                      />
                      <span aria-hidden="true" className="text-sm text-fg">
                        Show a light plaque behind the logo (for dark-ink uploads)
                      </span>
                    </div>
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
                appMarkSrc={appMarkSrc}
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
// real rendering; this is the operator's in-app equivalent of the design mock. The
// app mark is resolved upstream (appMarkSrc) exactly as AppShell.appMarkImgSrc does,
// so the preview cannot diverge from the chrome: a non-null src renders an <img>,
// null renders the FactoryIcon.
function BrandingPreview({
  appLogoMode,
  appLogoKeepName,
  appMarkSrc,
  brandMode,
  brandCompany,
  brandPlacement,
  brandPlaque,
  brandLogoSrc,
}: {
  appLogoMode: string;
  appLogoKeepName: boolean;
  appMarkSrc: string | null;
  brandMode: string;
  brandCompany: string;
  brandPlacement: string;
  brandPlaque: boolean;
  brandLogoSrc: string;
}) {
  // Mirror appMarkShowName: the name is hidden only in a full white-label — a
  // custom or preset mark with keep-name off.
  const whiteLabelable = appLogoMode === "custom" || appLogoMode === "preset";
  const showName = !whiteLabelable || appLogoKeepName;
  const brandLogo = brandMode === "logo";
  const topRight = brandLogo && brandPlacement === "topright";

  return (
    <div className="rounded-xl border border-edge bg-surface p-4" data-testid="branding-preview">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className="flex h-[38px] w-[38px] items-center justify-center rounded-lg bg-raised">
            {appMarkSrc ? (
              <img
                src={appMarkSrc}
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

      {/* Below-wordmark POWERED BY: a single right-aligned line, faint lowercase
          "powered by" inline with the text or logo, no separator. */}
      {brandMode !== "none" && !topRight && (
        <div className="mt-1 flex items-center justify-end gap-1.5 text-right">
          <span className="text-[10px] font-medium tracking-wider text-faint">powered by</span>
          {brandMode === "text" ? (
            <span className="text-sm text-fg">{brandCompany}</span>
          ) : (
            <span
              className={
                brandPlaque ? "inline-block rounded-md bg-[#f6f6f8] px-1.5 py-1" : "inline-block"
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

      {/* The license/author credit, gated on the build-time source flag
          SHOW_LICENSE_CREDIT (lib/flags.ts), default OFF/hidden — not admin-editable. */}
      {licenseCreditEnabled() && (
        <div className="mt-3 font-mono text-[10px] text-faint">MIT © Vlad Mocanu</div>
      )}
    </div>
  );
}
