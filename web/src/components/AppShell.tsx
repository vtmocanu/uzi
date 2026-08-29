// The app shell: a persistent grouped sidebar + content pane. Three tiers the
// old single-row topbar could not express: product identity up top, grouped
// destinations (Work / Factory, then an unlabeled Settings / Admin / Docs
// cluster) in the middle — with enabled repos' boards as real nav children
// instead of a <select> — and the signed-in user pinned to the footer.
// Active-state logic keeps a parent lit on child routes.

import { Link, useLocation, useNavigate } from "react-router-dom";
import { useEffect, useState, type ReactNode } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, MOCK_MODE, type Branding, type BuildInfo, type Repo } from "../lib/api";
import { prefs } from "../lib/prefs";
import { presetAssetForSlug, presetForSlug } from "../lib/brandPresets";
import { cx } from "./ui";
import { VaultBadge, VaultLockedBanner } from "./VaultControls";
import { RateLimitAnnouncer, SidebarRateLimits } from "./RateLimitMeters";
import { onNotificationsChanged } from "../lib/notifications";
import { useFavicon } from "../lib/useFavicon";
import { JudgeTodoContext, JudgeTodoValueContext } from "./JudgeTodoContext";
import { BuildInfoPopover } from "./BuildInfoPopover";
import { ChangelogDrawer } from "./ChangelogDrawer";
import {
  ActivityIcon,
  AlertIcon,
  BellIcon,
  BoardIcon,
  BookIcon,
  BotIcon,
  ChatIcon,
  ClockIcon,
  ChevronRightIcon,
  FactoryIcon,
  GearIcon,
  GitIcon,
  GitLabIcon,
  HomeIcon,
  LogOutIcon,
  MenuIcon,
  ScaleIcon,
  ServerIcon,
  ShieldIcon,
  SkillIcon,
  XIcon,
} from "./icons";

// localStorage key for the desktop sidebar's collapsed state (per browser).
const SIDEBAR_COLLAPSED_KEY = "uzi.sidebar.collapsed";

// 60s. Slower than the Workers page's 10s liveness poll on purpose: this one exists to
// notice a state that persists for minutes (a stuck roll), not to animate a gauge, and it
// runs on every page for every logged-in user.
const WORKERS_ATTENTION_POLL_MS = 60_000;

// The server build info, fetched once and shared. Memoised at module scope so the
// two SidebarContent mounts (desktop rail + mobile drawer) and any remount reuse a
// single unauthenticated GET /api/version. A failed fetch is SWALLOWED, never a
// thrown error in the shell — a 401 or a 500 on this endpoint must not take the
// chrome down with it.
//
// THREE STATES, DISCRIMINATED, and the discriminant is the point. `null` used to
// mean both "still in flight" and "the fetch failed", which made
// FleetUpgradePanel's third arm unreachable: a fleet page whose /api/version call
// 500s showed a permanent blank "pending" instead of telling the operator that
// upgrade classification was off. Resolving a failure to `null` and resolving a
// success to an object are indistinguishable to a consumer holding only the
// result, so the failure needs a value of its own rather than an absence.
//
//   snapshot === null   -> IN FLIGHT. Nothing has settled yet.
//   { status: "failed" } -> settled, and we will never know. Permanent.
//   { status: "ok", … }  -> settled with a body.
//
// fetchedAtMs rides along with the payload rather than being read at render time:
// `uptime_seconds` is a reading taken at the fetch, and the popover re-bases it
// against the wall clock so a session left open for hours does not keep reporting
// the uptime the API had at mount. Sampling the instant HERE (once, when the
// shared promise settles) rather than in each consumer is what keeps the two
// mounts agreeing.
type BuildInfoSnapshot =
  | { status: "ok"; info: BuildInfo; fetchedAtMs: number }
  | { status: "failed" };

const BUILD_INFO_FAILED: BuildInfoSnapshot = { status: "failed" };

let buildInfoPromise: Promise<BuildInfoSnapshot> | null = null;

function useBuildInfoSnapshot(): BuildInfoSnapshot | null {
  const [snapshot, setSnapshot] = useState<BuildInfoSnapshot | null>(null);
  useEffect(() => {
    if (!buildInfoPromise) {
      buildInfoPromise = api
        .version()
        .then((info): BuildInfoSnapshot => ({ status: "ok", info, fetchedAtMs: Date.now() }))
        // The swallow is unchanged in strength — nothing rethrows, and the shell
        // renders on. What changed is that it now resolves to a VALUE saying the
        // fetch failed, instead of to the same `null` an unsettled promise looks
        // like.
        .catch((): BuildInfoSnapshot => BUILD_INFO_FAILED);
    }
    let live = true;
    buildInfoPromise.then((s) => {
      if (live) setSnapshot(s);
    });
    return () => {
      live = false;
    };
  }, []);
  return snapshot;
}

// The whole build-info object (PRD #175). The seam the PRD's M2 bullet names.
//
// ⚠️ IT HAS ZERO PRODUCTION CALLERS, and that is a fact rather than an oversight —
// stated because a grep for callers now returns only AppShell.hooks.empty.test.tsx
// and the next reader deserves to know that was noticed. SidebarContent needs
// `fetchedAtMs` so it uses the private snapshot hook; useAppVersion needs the
// discriminant so it does too, and it was this hook's last caller until the
// tri-state landed.
//
// KEPT ANYWAY, deliberately: it is the seam the PRD specifies, and its intended
// consumers are that PRD's named follow-ups — an /about page and the CLI's
// `server` block — both of which want the whole object rather than a projection.
// Deleting it would delete the contract, not just an unused function. If those
// follow-ups are ever abandoned, this is the thing to remove with them.
//
// DELIBERATELY two-state, and it does not gain the third: there is nothing to
// render for a failed fetch and nothing to render while one is in flight, so both
// are `null` here. The distinction lives in useAppVersion below, whose consumer
// renders different copy for the two.
export function useBuildInfo(): BuildInfo | null {
  const s = useBuildInfoSnapshot();
  return s?.status === "ok" ? s.info : null;
}

// Exported so the Workers page can state the fleet's target release from the SAME
// coordinate the footer shows (PRD #113 M5). The promise is memoised at module scope, so
// reusing this hook costs no extra request and — more importantly — makes it impossible
// for the panel and the footer to disagree about what release the control plane is.
//
// THE TRI-STATE IS NOW REAL, and this hook is the only place it is produced:
//
//   null  -> IN FLIGHT.        FleetUpgradePanel renders &nbsp; — it asserts nothing
//                              while the answer is still coming.
//   ""    -> SETTLED, UNKNOWN. The fetch failed, or the body carried an empty
//                              version. The panel says "control-plane release
//                              unknown — targets unchecked", which is true and
//                              actionable; a permanent blank is neither.
//   "x"   -> SETTLED, STAMPED. The panel says "target release vx".
//
// Until this change the middle state was UNREACHABLE. `"" || null` is `null` and a
// failed fetch resolved the shared promise to `null` too, so `versionPending` was
// true both while the fetch was in flight AND forever after it had failed — the
// third arm of a panel written for three could never fire. The conflation it was
// written to prevent is a measured one: a full fleet bar rendered under a heading
// saying classification was off, at T+270ms, flipping at T+670ms.
//
// FLIPPING `||` TO `??` WOULD NOT HAVE FIXED IT, and remains the obvious wrong
// move if anyone revisits this: a failed fetch produced no BuildInfo at all, so no
// operator on `.version` could distinguish it. The fix had to be the discriminated
// snapshot above; this projection is downstream of it.
//
// `?? ""` rather than `|| ""` on the last line, and the difference is the whole
// point: `||` would fold a resolved empty version back into the same bucket as…
// itself, harmlessly, but `??` states the intent — only ABSENT becomes "", and a
// server-sent "" is already the settled-unknown value we want to pass through.
//
// Reads the SNAPSHOT directly rather than going through useBuildInfo, because it
// needs the discriminant and useBuildInfo deliberately discards it. (This comment
// said "kept as a projection over useBuildInfo" until the tri-state landed and the
// call moved; the INTENT below survived that change and the stated mechanism did
// not.)
//
// The intent, unchanged and still the reason this hook exists at all: ONE place
// owns the mapping, so every consumer sees the same three-state value instead of
// reimplementing it at its own call site — which is exactly how the two failure
// modes got conflated in the first place.
export function useAppVersion(): string | null {
  const s = useBuildInfoSnapshot();
  if (s === null) return null;
  return s.status === "ok" ? (s.info.version ?? "") : "";
}

// The instance branding config (GET /api/branding, PRD #685), fetched once and
// shared across every chrome surface. Memoised at module scope exactly like
// buildInfoPromise above, so the two SidebarContent mounts, the signed-out
// PublicShell and the mobile signed-in top bar all ride ONE unauthenticated GET
// rather than issuing a request each.
//
// A FAILED FETCH DEGRADES TO THE UNBRANDED DEFAULT, never throws: the shell must
// render its stock uzi mark when /api/branding 500s exactly as it does before the
// fetch settles, so the promise resolves to `null` on rejection and useBranding
// hands that `null` (its pending value too) to the surfaces, which read it as the
// default (app_logo_mode "default", keep_name true).
let brandingPromise: Promise<Branding | null> | null = null;

// Test-only reset of the module-memoised branding fetch. buildInfoPromise needs no
// such seam because its tests resolve ONE value per file (vitest isolates per
// file); the M3a chrome tests instead exercise DIFFERENT branding per test
// (default / custom / white-label) within one file, so the memo must be cleared
// between them or the first test's value would pin the whole file.
export function __resetBrandingForTests(): void {
  brandingPromise = null;
}

function useBranding(): Branding | null {
  const [branding, setBranding] = useState<Branding | null>(null);
  useEffect(() => {
    if (!brandingPromise) {
      // The swallow, both arms: a rejected fetch resolves to `null` (the unbranded
      // default) via .catch, and a SYNCHRONOUS throw from the call itself is caught
      // here too — either way the shell degrades to the default mark, it never
      // rethrows.
      try {
        brandingPromise = api.branding().catch((): Branding | null => null);
      } catch {
        brandingPromise = Promise.resolve(null);
      }
    }
    let live = true;
    brandingPromise.then((b) => {
      if (live) setBranding(b);
    });
    return () => {
      live = false;
    };
  }, []);
  return branding;
}

// The app-logo <img> src, or null when the mark should render the default inline
// FactoryIcon. Three modes (PRD #780): `custom` serves the uploaded logo when
// present and otherwise renders the FactoryIcon (no upload → no <img>); `preset`
// resolves the shipped asset from the web-owned catalog by slug, degrading to the
// FactoryIcon on an empty/unknown slug; `default` (and null/pending branding) is
// the FactoryIcon. NEVER inline SVG: a resolved mark is always an <img> so an
// uploaded/preset SVG stays passive.
function appMarkImgSrc(branding: Branding | null): string | null {
  if (!branding) return null;
  if (branding.app_logo_mode === "custom") {
    return branding.app_logo_present ? "/api/branding/logo/app" : null;
  }
  if (branding.app_logo_mode === "preset") {
    return presetAssetForSlug(branding.app_logo_preset);
  }
  return null;
}

// Whether to render the "uzi" / "uzinele întunecate" name beside the mark. Hidden
// only in a full white-label — a non-default mode (custom or preset) with
// keep_name=false; default mode and any keep-name co-brand keep it. `null`/pending
// branding is the default (shown). (PRD #780 D4.)
function appMarkShowName(branding: Branding | null): boolean {
  if (!branding) return true;
  if (branding.app_logo_mode === "custom" || branding.app_logo_mode === "preset") {
    return branding.app_logo_keep_name;
  }
  return true;
}

// The framed app mark, shared across all four chrome surfaces (PRD #685 M3a). Each
// surface passes its own frame sizing via `className` (h-8 sidebar, h-7 top bars);
// the frame carries the brand tint for the default FactoryIcon and a neutral bg for
// a custom logo, and clips the <img> to the rounded frame.
function AppMark({ branding, className }: { branding: Branding | null; className?: string }) {
  const src = appMarkImgSrc(branding);
  // In preset mode the mark IS the brand identity (a full white-label hides the
  // wordmark), so give the <img> the preset's label as its accessible name;
  // custom-logo mode keeps the generic "app logo". (PRD #780 M4 / a11y from M2.)
  const presetLabel =
    branding?.app_logo_mode === "preset"
      ? presetForSlug(branding.app_logo_preset)?.label
      : undefined;
  return (
    <span
      className={cx(
        "flex shrink-0 items-center justify-center overflow-hidden",
        src ? "bg-raised" : "bg-brand/15 text-brand",
        className,
      )}
    >
      {src ? (
        <img
          src={src}
          alt={presetLabel ?? "app logo"}
          data-testid="app-logo-img"
          className="h-full w-full object-contain"
        />
      ) : (
        <FactoryIcon data-testid="app-mark-fallback" />
      )}
    </span>
  );
}

// The durable license/author credit (PRD #685 Decision D3). A build-time CONSTANT,
// never a setting — a rebrand or full white-label cannot strip it. Rendered
// independently of the build-info fetch so it shows during load and on fetch
// failure, on both the signed-in sidebar footer and the signed-out shell. `©`
// (U+00A9), not `·`.
const LICENSE_CREDIT = "MIT © Vlad Mocanu";

function LicenseCredit({ className }: { className?: string }) {
  return (
    <span data-testid="license-credit" className={cx("font-mono text-[10px] text-faint", className)}>
      {LICENSE_CREDIT}
    </span>
  );
}

// The custom brand-logo <img> src for the POWERED BY block (PRD #685 M3b). Serves
// the uploaded brand logo when present, else the shipped preset /brand-default.svg.
// Only called in logo mode; like the app mark it is ALWAYS an <img> so an uploaded
// SVG stays passive (never inline SVG).
function brandLogoImgSrc(branding: Branding): string {
  return branding.brand_logo_present ? "/api/branding/logo/brand" : "/brand-default.svg";
}

// The POWERED BY brand block (PRD #685 M3b), reading the same module-memoised
// useBranding() the app mark uses. Renders ONLY in the expanded signed-in sidebar:
// a null/pending branding, brand_mode "none", or a collapsed rail all render
// nothing (the unbranded default — never throws).
//
// `slot` fixes where the block sits, and the component self-selects by
// brand_placement so each mount renders only when its slot matches:
//   "below"    — a single right-aligned line UNDER the header: a faint lowercase
//                "powered by" label inline with the company text or the logo,
//                tucked close under the wordmark, no separator (the default
//                placement).
//   "topright" — logo-only (~96px max, ~26px tall), NO label, sharing the header
//                row. Top-right is a LOGO-only option (Decision D6), so text mode
//                always renders below regardless of brand_placement — this keeps the
//                chrome in step with the admin live preview, which shows text below.
//
// A custom brand logo is ALWAYS an <img> (uploaded → /api/branding/logo/brand, else
// the shipped preset /brand-default.svg), never inline SVG — the XSS control. The
// logo dims to opacity-80 (a CSS constant, not a setting); brand_plaque adds a light
// rounded plaque behind it (bg-[#f6f6f8]) for dark-ink uploads.
function PoweredBy({
  branding,
  collapsed = false,
  slot,
}: {
  branding: Branding | null;
  collapsed?: boolean;
  slot: "below" | "topright";
}) {
  if (collapsed || !branding || branding.brand_mode === "none") return null;
  const isLogo = branding.brand_mode === "logo";
  // Top-right is logo-only (D6); text mode always renders below, so a stale
  // "topright" left over from a prior logo config never strands a bare company
  // label in the header row.
  const placement = isLogo && branding.brand_placement === "topright" ? "topright" : "below";
  if (placement !== slot) return null;

  const logoImg = (
    <img
      src={brandLogoImgSrc(branding)}
      alt="brand logo"
      data-testid="brand-logo-img"
      className={cx("w-auto object-contain opacity-80", slot === "topright" ? "h-[26px]" : "h-6")}
    />
  );

  if (slot === "topright") {
    // Reached only for logo mode (see the placement guard above), so this is
    // always the logo — top-right never carries text.
    return (
      <span className="ml-auto flex max-w-[96px] items-center">
        <span className={branding.brand_plaque ? "rounded-md bg-[#f6f6f8] px-1.5 py-1" : undefined}>
          {logoImg}
        </span>
      </span>
    );
  }

  return (
    <div className="flex items-center justify-end gap-1.5 px-4 pt-1 pb-2 text-right">
      <span className="text-[10px] font-medium tracking-wider text-faint">powered by</span>
      {isLogo ? (
        <span className={branding.brand_plaque ? "rounded-md bg-[#f6f6f8] px-1.5 py-1" : undefined}>
          {logoImg}
        </span>
      ) : (
        <span className="text-sm text-fg">{branding.brand_company}</span>
      )}
    </div>
  );
}

function isNavActive(pathname: string, href: string): boolean {
  return pathname === href || pathname.startsWith(href + "/");
}

// forgeIcon picks a board entry's glyph from its connection's forge type
// (Decision 2): GitLab gets the tanuki, anything else (or an unknown/missing
// type when the connections join is unavailable) falls back to the generic Git
// mark. Exported so the mapping is unit-testable without the DOM.
export function forgeIcon(forgeType: string | undefined): ReactNode {
  return forgeType === "gitlab" ? <GitLabIcon /> : <GitIcon />;
}

function NavItem({
  to,
  icon,
  label,
  exactOnly = false,
  indent = false,
  onNavigate,
  collapsed = false,
  badge = 0,
  badgeTone = "count",
  badgeLabel,
}: {
  to: string;
  icon?: ReactNode;
  label: string;
  exactOnly?: boolean;
  indent?: boolean;
  onNavigate?: () => void;
  // When collapsed the item is an icon-only rail button; the label moves to a
  // native title tooltip so the destination is still identifiable on hover.
  collapsed?: boolean;
  // badge is an unread count (PRD #46 M2): expanded, a count pill trails the label;
  // collapsed, a dot overlaps the icon since a rail has no room for a number. 0
  // renders nothing.
  badge?: number;
  // badgeTone distinguishes "go look" from "there is a queue" (PRD #113 Decision 2).
  // The default `count` tone is the brand pill every existing badge uses — an unread
  // count, a Judge backlog: things to get to. `alert` is red, and it means a worker
  // needs attention now.
  //
  // A new TONE rather than a new mechanism, deliberately: two badge implementations
  // would drift in position, size and collapsed-rail behaviour, and the rail's dot has
  // no room to distinguish them by anything but colour.
  badgeTone?: "count" | "alert";
  // badgeLabel is the NOUN the accessible count announces — "in progress" for the Runs
  // badge, so a screen reader says "Runs, 5 in progress" rather than the meaningless
  // "5 unread" (a run is never unread). Defaults per tone when unset: the `count` tone
  // falls back to "unread" (correct for Notifications) and `alert` to "needing attention"
  // (Workers), so every existing badge is byte-for-byte unchanged. The NavItem comment
  // above already states the label must say what the number MEANS; this makes that
  // reachable per-item instead of one hardcoded string for the whole tone.
  badgeLabel?: string;
}) {
  const { pathname } = useLocation();
  const active = exactOnly ? pathname === to : isNavActive(pathname, to);
  const hasBadge = badge > 0;
  const alert = badgeTone === "alert";
  // The accessible noun: explicit override, else the per-tone default.
  const badgeNoun = badgeLabel ?? (alert ? "needing attention" : "unread");
  return (
    <Link
      to={to}
      onClick={onNavigate}
      aria-current={active ? "page" : undefined}
      title={collapsed ? label : undefined}
      className={cx(
        "group flex items-center rounded-lg text-sm transition-colors",
        // lg:py-1 tightens the desktop rail only (the mobile drawer keeps its
        // 36px touch rows): 4px per row, plus the lg: chrome trims on the nav
        // container, group headers, divider and footer, is what lets an admin's
        // full nav — both groups, three board children and the bottom cluster —
        // fit a 900px-tall laptop. Measured 2026-08-14 at 1440x900, demo data,
        // default one-pair footer: content 574px in a 597px nav (23px slack);
        // before the trims the same nav needed ~660px. Each extra pinned token
        // meter costs ~66px of footer and can push the nav back into scroll —
        // that is the user's explicit choice (sidebar_token_ids), not a layout
        // regression.
        collapsed ? "justify-center px-2.5 py-2" : "gap-2.5 px-2.5 py-1.5 lg:py-1",
        indent && !collapsed && "ml-4",
        active ? "bg-raised font-medium text-fg" : "text-muted hover:bg-raised/60 hover:text-fg",
      )}
    >
      {icon && (
        <span className={cx("relative text-base", active ? "text-brand" : "text-faint group-hover:text-muted")}>
          {icon}
          {collapsed && hasBadge && (
            <span
              aria-hidden="true"
              className={cx(
                "absolute -right-1 -top-1 h-2 w-2 rounded-full ring-2 ring-surface",
                alert ? "bg-danger" : "bg-brand",
              )}
            />
          )}
        </span>
      )}
      {!collapsed && <span className="truncate">{label}</span>}
      {/* The count SURVIVES COLLAPSE. The pill below is gated on !collapsed and the rail's
          dot is aria-hidden, so without this an assistive-tech user got no count and no
          tone at all in the collapsed rail — measured: no aria-label, empty innerText, only
          title="Workers". That is the information not being there, not a visual
          degradation, and it happens in the layout where the operator has least context.
          "The sidebar was collapsed" is not a reason to withhold an incident count.
          sr-only rather than an aria-label on the Link, so the destination name and the
          count stay separate strings rather than one run-on label. */}
      {collapsed && hasBadge && (
        <span className="sr-only">{`${badge} ${badgeNoun}`}</span>
      )}
      {!collapsed && hasBadge && (
        <span
          // The label says what the number MEANS. "3 unread" for a worker count would be
          // wrong in a way a screen-reader user could not recover from — nothing else on
          // the page would explain it.
          aria-label={`${badge} ${badgeNoun}`}
          className={cx(
            "ml-auto min-w-[1.25rem] rounded-full px-1.5 py-0.5 text-center text-[10px] font-semibold leading-none",
            // text-on-brand, NOT text-white: measured 2.69:1 for white on bg-danger at 10px/600
            // against 8.27:1 for the Notifications and Judge badges. The badge whose entire
            // purpose is to be noticed mid-incident was the only sidebar badge failing AA.
            alert ? "bg-danger text-on-brand" : "bg-brand text-on-brand",
          )}
        >
          {badge > 99 ? "99+" : badge}
        </span>
      )}
    </Link>
  );
}

function NavGroup({
  label,
  children,
  collapsed = false,
}: {
  // Optional: the bottom cluster (Settings / Admin / Docs) is grouped by a rule
  // alone — naming it would just restate its members, and the old single-item
  // "Configure" and "Help" headers were labels costing a row each to introduce
  // one entry each.
  label?: string;
  children: ReactNode;
  collapsed?: boolean;
}) {
  return (
    <div className="space-y-0.5">
      {collapsed || !label ? (
        // No room for a group label on the rail (and none wanted for an
        // unlabeled group); a thin rule keeps the grouping.
        <div className="mx-2.5 my-2 border-t border-edge lg:my-1.5" aria-hidden="true" />
      ) : (
        <p className="px-2.5 pb-1 pt-4 text-[11px] font-semibold uppercase tracking-wider text-faint/80 lg:pt-3">
          {label}
        </p>
      )}
      {children}
    </div>
  );
}

function SidebarContent({
  onNavigate,
  collapsed = false,
  onToggleCollapse,
  unread = 0,
  judgeTodo = 0,
  runsInProgress = 0,
  schedulesEnabled = 0,
  workersAttention = 0,
  findingsOpen = 0,
  onOpenChangelog,
}: {
  onNavigate?: () => void;
  // Desktop-only icon-rail mode. The mobile sheet always renders expanded (it is
  // already a full-width overlay) and passes neither prop.
  collapsed?: boolean;
  onToggleCollapse?: () => void;
  // Judge to-triage count for the Judge nav badge (PRD #98). Owned by AppShell alongside
  // `unread`, sourced from /me/judge/stats.todo — the ONE canonical number, so the badge
  // agrees with the Judge page's To-triage tab and the judge notification to the digit.
  judgeTodo?: number;
  // In-progress run count for the Runs nav badge (PRD #239). Owned by AppShell alongside
  // `judgeTodo`, sourced from /me/runs/in-progress-count — the caller's non-terminal runs
  // (kind NOT IN chat/judge, Decision 4). Brand "count" tone, NOT the Workers `alert` red:
  // in-progress runs are healthy activity / a queue to get to, not "go look now".
  runsInProgress?: number;
  // Enabled-schedule count for the Schedules nav badge (PRD #241 M5). The caller's
  // enabled schedules (paused ones don't nag), brand "count" tone like Runs. 0 renders
  // nothing.
  schedulesEnabled?: number;
  // Count for the Workers nav badge (PRD #113 M6). 0 renders nothing at all — not a
  // badge showing zero, which would be a permanent ornament that means nothing.
  workersAttention?: number;
  // Open-findings count for the Findings nav badge (PRD #333 M7, D8). The caller's open
  // finding coordinates, sourced from the GET /api/findings response `open_count` meta — a
  // new count source, separate from the shared bell unread. Brand "count" tone, 0 hides it.
  findingsOpen?: number;
  // Notifications unread count for the bell badge (PRD #46 M2). Owned by the
  // parent AppShell so the single poll feeds both this badge and the status
  // favicon (PRD #70), and both sidebar instances (desktop + mobile) share it.
  unread?: number;
  // Opens the app-level changelog drawer (PRD #415 M2). AppShell passes the SAME
  // callback to both SidebarContent mounts so the two footer triggers open one
  // drawer instance mounted at AppShell scope — the drawer must NOT live here.
  onOpenChangelog?: () => void;
}) {
  const { user, logout } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const build = useBuildInfoSnapshot();
  const branding = useBranding();
  const showName = appMarkShowName(branding);
  const [repos, setRepos] = useState<Repo[]>([]);
  // connection_id → forge_type, joined web-side so board entries can show a forge
  // glyph (the Repo DTO has no forge_type). Kept separate from repos so a failed
  // connections call degrades to the Git fallback rather than blanking the boards.
  const [forgeTypeById, setForgeTypeById] = useState<Record<string, string>>({});

  // Boards in the nav mirror the user's enabled repos; refetched on navigation
  // so enabling/disabling a repo shows up without a reload.
  useEffect(() => {
    if (!user) {
      setRepos([]);
      return;
    }
    api
      .listRepos()
      .then(({ repos }) => setRepos(repos))
      .catch(() => setRepos([]));
  }, [user, location.pathname]);

  // Connections change rarely, so this join is fetched once per session, not per
  // navigation. Failure is non-fatal: the map stays empty and every board falls
  // back to the generic Git icon.
  useEffect(() => {
    if (!user) {
      setForgeTypeById({});
      return;
    }
    api
      .listConnections()
      .then(({ connections }) =>
        setForgeTypeById(Object.fromEntries(connections.map((c) => [c.id, c.forge_type]))),
      )
      .catch(() => setForgeTypeById({}));
  }, [user]);

  const handleLogout = async () => {
    onNavigate?.();
    await logout();
    navigate("/login");
  };

  return (
    <div className="flex h-full flex-col">
      <Link
        to="/dashboard"
        onClick={onNavigate}
        title={collapsed ? (showName ? "uzi · uzinele întunecate" : "app logo") : undefined}
        className={cx(
          "flex items-center border-b border-edge py-4",
          collapsed ? "justify-center px-2" : "gap-2.5 px-4",
        )}
      >
        <AppMark branding={branding} className="h-8 w-8 rounded-lg text-lg" />
        {!collapsed && (
          <>
            {showName && (
              <span className="min-w-0">
                <span className="block text-sm font-semibold leading-tight tracking-tight">uzi</span>
                <span className="block truncate text-[11px] leading-tight text-faint">
                  uzinele întunecate
                </span>
              </span>
            )}
            {MOCK_MODE && (
              <span
                title="This build runs entirely in your browser on demo data — no backend."
                className="ml-auto rounded-md border border-brand/40 bg-brand/10 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-brand"
              >
                demo
              </span>
            )}
          </>
        )}
        {/* POWERED BY, top-right placement (PRD #685 M3b): logo-only, no label,
            sharing the header row. Self-gates on placement/mode/collapsed. */}
        <PoweredBy branding={branding} collapsed={collapsed} slot="topright" />
      </Link>

      {/* POWERED BY, below-wordmark placement (PRD #685 M3b, the default): a row
          under the header carrying the faint uppercase label + text or logo.
          Self-gates on placement/mode/collapsed. */}
      <PoweredBy branding={branding} collapsed={collapsed} slot="below" />

      <nav className="flex-1 space-y-1 overflow-y-auto px-2 pb-4 lg:space-y-0.5 lg:pb-2">
        <div className="space-y-0.5 pt-3 lg:pt-2">
          <NavItem to="/dashboard" icon={<HomeIcon />} label="Overview" onNavigate={onNavigate} collapsed={collapsed} />
          <NavItem
            to="/notifications"
            icon={<BellIcon />}
            label="Notifications"
            badge={unread}
            onNavigate={onNavigate}
            collapsed={collapsed}
          />
        </div>

        <NavGroup label="Work" collapsed={collapsed}>
          <NavItem to="/repos" icon={<BoardIcon />} label="Boards" exactOnly onNavigate={onNavigate} collapsed={collapsed} />
          {/* Board children collapse into the "Boards" parent: on the rail every
              repo would render as an identical forge glyph, so hide them and let
              /repos stand in (Decision: reviewer #7). */}
          {!collapsed &&
            repos.map((r) => (
              <NavItem
                key={r.id}
                to={`/repos/${r.id}/board`}
                icon={forgeIcon(forgeTypeById[r.connection_id])}
                label={r.path_with_namespace}
                indent
                onNavigate={onNavigate}
              />
            ))}
          {/* Runs badge (PRD #239): the caller's in-progress run count. Default
              "count" tone — brand, "there is a queue", not the Workers alert red. */}
          <NavItem
            to="/runs"
            icon={<ActivityIcon />}
            label="Runs"
            badge={runsInProgress}
            // "in progress", not the count tone's default "unread": a run is never unread,
            // and the mock spec'd this noun. Brand tone (Decision 2), not the Workers alert red.
            badgeLabel="in progress"
            onNavigate={onNavigate}
            collapsed={collapsed}
          />
          {/* Findings (PRD #333 D8): off-task bugs workers flagged on YOUR code — Work, not the
              agent-grading Factory where Judge lives. Sits directly under Runs, the lane whose
              output it collects. Badge is the open-findings count from the GET /api/findings
              meta; brand "count" tone, the noun is "open". */}
          <NavItem
            to="/findings"
            icon={<AlertIcon />}
            label="Findings"
            badge={findingsOpen}
            badgeTone="count"
            badgeLabel="open"
            onNavigate={onNavigate}
            collapsed={collapsed}
          />
          {/* Schedules (PRD #241): the time-driven run origin. Badge is the caller's
              enabled-schedule count — brand "count" tone, paused ones excluded. */}
          <NavItem
            to="/schedules"
            icon={<ClockIcon />}
            label="Schedules"
            badge={schedulesEnabled}
            badgeLabel="enabled"
            onNavigate={onNavigate}
            collapsed={collapsed}
          />
          <NavItem to="/chat" icon={<ChatIcon />} label="Chat" onNavigate={onNavigate} collapsed={collapsed} />
        </NavGroup>

        <NavGroup label="Factory" collapsed={collapsed}>
          <NavItem to="/agents" icon={<BotIcon />} label="Agents" onNavigate={onNavigate} collapsed={collapsed} />
          <NavItem to="/skills" icon={<SkillIcon />} label="Skills" onNavigate={onNavigate} collapsed={collapsed} />
          {/* Workers' ONE nav home. It was also a Settings tab (as /settings/workers),
              which needed the excludeSubpath active-state hack to keep the two entries
              from lighting together — the fleet is operations, so it lives here alone. */}
          <NavItem
            to="/workers"
            icon={<ServerIcon />}
            label="Workers"
            badge={workersAttention}
            // alert, not the default count tone: red reads "go look", while the grey
            // Judge pill beside it reads "there is a queue" (Decision 2).
            badgeTone="alert"
            onNavigate={onNavigate}
            collapsed={collapsed}
          />
          {/* Judge (PRD #98): the cross-run recommendation workbench. Badge is the
              to-triage backlog count — the same number the page's To-triage tab shows. */}
          <NavItem to="/judge" icon={<ScaleIcon />} label="Judge" badge={judgeTodo} onNavigate={onNavigate} collapsed={collapsed} />
        </NavGroup>

        {/* The bottom cluster: system destinations, grouped by a rule rather than a
            label. Settings and Docs used to each carry a one-item labeled group
            ("Configure", "Help"), and the five admin pages each held a sidebar row —
            they are now one tabbed Admin destination (AdminShell), the same treatment
            the Settings tabs give the user-scoped surfaces. */}
        <NavGroup collapsed={collapsed}>
          {/* Forge has no standalone entry (Decision 3): it lives only under the
              Settings tabs, so Settings stays lit across /settings/*. */}
          <NavItem
            to="/settings"
            icon={<GearIcon />}
            label="Settings"
            onNavigate={onNavigate}
            collapsed={collapsed}
          />
          {user?.is_admin && (
            /* /admin redirects to the first tab; the prefix match keeps this entry
               lit on every /admin/* tab. */
            <NavItem to="/admin" icon={<ShieldIcon />} label="Admin" onNavigate={onNavigate} collapsed={collapsed} />
          )}
          <NavItem to="/docs" icon={<BookIcon />} label="Docs" onNavigate={onNavigate} collapsed={collapsed} />
        </NavGroup>
      </nav>

      <div className="border-t border-edge">
        {/* The desktop collapse toggle used to be its own full-width row pinned above
            this footer — a whole row spent on one chevron (ux-tweaks item 1). It now
            rides the footer's user cluster as its last element in both states, past a
            hairline divider so it reads as chrome, not as part of the user's identity.
            Still persistent (not hover-only), still keyboard-reachable, and
            aria-expanded tracks the sidebar, not a popup, so screen readers announce
            the rail's state. */}
        {user &&
          (collapsed ? (
            <div className="flex flex-col items-center gap-2 px-3 py-3">
              {/* Vault status glyph (PRD #32); the lock/unlock action lives in
                  Settings and the app-wide banner. */}
              <VaultBadge compact />
              <span
                title={`${user.display_name ?? user.email} · ${user.is_admin ? "Administrator" : "User"}`}
                className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-raised text-xs font-semibold text-muted"
              >
                {(user.display_name?.[0] ?? user.email[0] ?? "?").toUpperCase()}
              </span>
              <button
                onClick={handleLogout}
                title="Log out"
                aria-label="Log out"
                className="rounded-md p-1.5 text-faint transition-colors hover:bg-raised hover:text-fg"
              >
                <LogOutIcon />
              </button>
              {onToggleCollapse && (
                <>
                  <span aria-hidden="true" className="hidden h-px w-4 bg-edge lg:block" />
                  <button
                    type="button"
                    onClick={onToggleCollapse}
                    aria-expanded={false}
                    aria-label="Expand sidebar"
                    title="Expand sidebar"
                    className="hidden rounded-md p-1.5 text-faint transition-colors hover:bg-raised hover:text-fg lg:inline-flex"
                  >
                    <ChevronRightIcon />
                  </button>
                </>
              )}
            </div>
          ) : (
            <div className="px-3 py-3 lg:py-2">
              <div className="mb-2 lg:mb-1.5">
                <VaultBadge />
              </div>
              <div className="flex items-center gap-2">
                <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-raised text-xs font-semibold text-muted">
                  {(user.display_name?.[0] ?? user.email[0] ?? "?").toUpperCase()}
                </span>
                <span className="min-w-0 flex-1">
                  <span className="block truncate text-xs font-medium text-fg">
                    {user.display_name ?? user.email}
                  </span>
                  <span className="block truncate text-[11px] text-faint">
                    {user.is_admin ? "Administrator" : "User"}
                  </span>
                </span>
                <button
                  onClick={handleLogout}
                  title="Log out"
                  aria-label="Log out"
                  className="rounded-md p-1.5 text-faint transition-colors hover:bg-raised hover:text-fg"
                >
                  <LogOutIcon />
                </button>
                {onToggleCollapse && (
                  <>
                    <span aria-hidden="true" className="hidden h-4 w-px bg-edge lg:block" />
                    <button
                      type="button"
                      onClick={onToggleCollapse}
                      aria-expanded={true}
                      aria-label="Collapse sidebar"
                      title="Collapse sidebar"
                      className="hidden rounded-md p-1.5 text-faint transition-colors hover:bg-raised hover:text-fg lg:inline-flex"
                    >
                      <span className="inline-flex rotate-180">
                        <ChevronRightIcon />
                      </span>
                    </button>
                  </>
                )}
              </div>
              {/* Claude rate-limit micro-meters (PRD #53): two 5px bars under the
                  user block. Self-gates — renders nothing without a live reading. */}
              <SidebarRateLimits />
            </div>
          ))}

        {/* Footer row. Server build info (GET /api/version, PRD #175): the badge
            reads "v0.6.0" / "dev"; hovering, focusing or tapping it opens the rest of
            the coordinate set. The native `title` is GONE deliberately — a browser
            tooltip firing alongside a custom popover is two overlapping panels saying
            different things. It renders nothing until the fetch resolves with a
            version, exactly as the old badge did.

            The durable license/author credit (PRD #685 D3) is the RIGHT-HAND item of
            this one-row footer. It is a build-time constant, NOT gated on the build
            fetch, so it must render during the /api/version load and on its failure
            alike — a rebrand or full white-label cannot strip it. When the rail is
            collapsed there is no room for it: the footer falls back to just the
            build popover's collapsed variant. */}
        {collapsed ? (
          build?.status === "ok" &&
          build.info.version && (
            <BuildInfoPopover
              info={build.info}
              collapsed={true}
              fetchedAtMs={build.fetchedAtMs}
              onOpenChangelog={onOpenChangelog}
            />
          )
        ) : (
          <div className="flex items-center justify-between gap-2 px-3 pb-2 pt-1">
            {build?.status === "ok" && build.info.version ? (
              <BuildInfoPopover
                info={build.info}
                collapsed={false}
                fetchedAtMs={build.fetchedAtMs}
                onOpenChangelog={onOpenChangelog}
              />
            ) : (
              <span aria-hidden="true" />
            )}
            <LicenseCredit />
          </div>
        )}
      </div>
    </div>
  );
}

// Public (signed-out) chrome: a minimal top bar; the sidebar only exists for
// signed-in sessions.
function PublicShell({ children }: { children: ReactNode }) {
  const branding = useBranding();
  return (
    <div className="flex min-h-screen flex-col">
      <header className="border-b border-edge">
        <div className="mx-auto flex max-w-5xl items-center justify-between px-4 py-3">
          <Link to="/" className="flex items-center gap-2 text-sm font-semibold tracking-tight">
            <AppMark branding={branding} className="h-7 w-7 rounded-lg" />
            {appMarkShowName(branding) && (
              <span>
                uzi <span className="font-normal text-faint">· uzinele întunecate</span>
              </span>
            )}
          </Link>
          <nav className="flex items-center gap-4 text-sm">
            <Link to="/docs" className="text-muted hover:text-fg">
              Docs
            </Link>
            <Link to="/login" className="text-muted hover:text-fg">
              Log in
            </Link>
            <Link
              to="/register"
              className="rounded-lg bg-brand px-3 py-1.5 text-sm font-medium text-on-brand hover:bg-brand-hover"
            >
              Register
            </Link>
          </nav>
        </div>
      </header>
      <main className="mx-auto w-full max-w-5xl flex-1 px-4 py-10">{children}</main>
      {/* Durable credit (PRD #685 D3) on the signed-out shell, which has no version
          row: a footer carries it here. Constant, independent of any fetch. */}
      <footer className="border-t border-edge">
        <div className="mx-auto max-w-5xl px-4 py-4">
          <LicenseCredit />
        </div>
      </footer>
    </div>
  );
}

export function AppShell({ children }: { children: ReactNode }) {
  const { user } = useAuth();
  const location = useLocation();
  const [mobileOpen, setMobileOpen] = useState(false);
  // Notifications unread badge (PRD #46 M2). Owned here — above the guest early
  // return — so a single poll feeds both the bell badge (passed to each
  // SidebarContent) and the status favicon, and it survives across routes.
  const [unread, setUnread] = useState(0);
  // Judge to-triage badge (PRD #98). Owned here alongside `unread`, from
  // /me/judge/stats.todo — the ONE canonical to-triage number.
  //
  // Reading the same number is necessary but NOT sufficient for the badge to agree with the
  // Judge page's To-triage tab, and this comment used to claim otherwise (PRD #98 review
  // BLK-BADGE, measured: after a dispose the nav read 3 while the tab read 0). The poll
  // below fires on `[user, location.pathname]`, and a disposition changes neither — nor does
  // switching bucket tabs, which rewrites the SEARCH, not the pathname. Agreement therefore
  // needs a propagation channel as well as a shared source: JudgeTodoContext publishes this
  // setter, and the Judge page pushes the fresh canonical `triage.todo` it already has.
  const [judgeTodo, setJudgeTodo] = useState(0);
  // In-progress runs badge (PRD #239). Owned here alongside `judgeTodo`, from
  // /me/runs/in-progress-count — the caller's non-terminal, non-chat/judge run count.
  const [runsInProgress, setRunsInProgress] = useState(0);
  // Enabled-schedule badge (PRD #241 M5). Owned here alongside runsInProgress; the
  // caller's enabled schedules, from the owner-scoped listSchedules.
  const [schedulesEnabled, setSchedulesEnabled] = useState(0);
  // Workers needing attention (PRD #113 M6): upgrade_failed + outdated, minus muted,
  // counted server-side so this badge and the Workers page's badges cannot disagree.
  const [workersAttention, setWorkersAttention] = useState(0);
  // Open-findings badge (PRD #333 M7, D8). Owned here alongside the other nav counts, sourced
  // from the GET /api/findings response `open_count` meta (unfiltered → global open count).
  const [findingsOpen, setFindingsOpen] = useState(0);
  // Desktop sidebar collapse, persisted per browser. Initialised lazily from
  // localStorage so the first paint already matches the stored state — a
  // post-mount effect would flash the sidebar expanded then snap it collapsed.
  const [collapsed, setCollapsed] = useState(() => prefs.get(SIDEBAR_COLLAPSED_KEY, false));
  useEffect(() => {
    prefs.set(SIDEBAR_COLLAPSED_KEY, collapsed);
  }, [collapsed]);
  // Changelog drawer (PRD #415 M2). Owned HERE, not in SidebarContent, because the
  // sidebar is mounted twice (desktop rail + mobile drawer) and there must be a
  // single drawer instance; both footer triggers call the same setter. Read the
  // build snapshot from the shared module-scope promise so the drawer can mark the
  // running version — the memoisation makes this the same GET the footer already
  // issued, not a second request.
  const [changelogOpen, setChangelogOpen] = useState(false);
  const build = useBuildInfoSnapshot();
  // Branding for the mobile signed-in top bar's app mark (PRD #685 M3a). Shares the
  // one module-memoised GET with the two SidebarContent mounts.
  const branding = useBranding();

  // Unread badge: poll on navigation (no WS — PRD #46 M2) and refresh on the
  // in-app change event (e.g. after marking one read on the inbox page). A failed
  // fetch is non-fatal: keep the last known count rather than blanking the badge.
  useEffect(() => {
    if (!user) {
      setUnread(0);
      return;
    }
    let alive = true;
    const load = () =>
      api
        .unreadNotificationCount()
        .then(({ unread }) => {
          if (alive) setUnread(unread);
        })
        .catch(() => {});
    load();
    const off = onNotificationsChanged(load);
    return () => {
      alive = false;
      off();
    };
  }, [user, location.pathname]);

  // Judge to-triage poll (PRD #98): the same on-navigation cadence as the unread poll
  // above, reading the canonical /me/judge/stats.todo. A failed fetch keeps the last
  // known count rather than blanking the badge.
  useEffect(() => {
    if (!user) {
      setJudgeTodo(0);
      return;
    }
    let alive = true;
    api
      .getJudgeStats()
      .then((stats) => {
        if (alive) setJudgeTodo(stats.todo);
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [user, location.pathname]);

  // Runs-in-progress poll (PRD #239): the same on-navigation cadence as the Judge poll
  // above, reading /me/runs/in-progress-count. A failed fetch keeps the last known count
  // rather than blanking the badge.
  useEffect(() => {
    if (!user) {
      setRunsInProgress(0);
      return;
    }
    let alive = true;
    api
      .runsInProgressCount()
      .then(({ count }) => {
        if (alive) setRunsInProgress(count);
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [user, location.pathname]);

  // Enabled-schedule poll (PRD #241 M5): the same on-navigation cadence as the Runs
  // poll above, reading the owner-scoped listSchedules and counting enabled rows. A
  // failed fetch keeps the last known count rather than blanking the badge.
  useEffect(() => {
    if (!user) {
      setSchedulesEnabled(0);
      return;
    }
    let alive = true;
    api
      .listSchedules()
      .then((rows) => {
        if (alive) setSchedulesEnabled(rows.filter((s) => s.enabled).length);
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [user, location.pathname]);

  // Workers-attention poll (PRD #113 M6). On navigation AND on a fixed interval, which
  // is deliberately more than the Judge badge does: a worker's roll fails while the
  // operator is doing something else entirely, and a badge that only refreshes on
  // navigation would stay clean through the whole incident if they happen not to move.
  //
  // NOT visibility-gated, for the same reason. A hidden tab is precisely when nobody is
  // watching the Workers page, and the badge is what is supposed to pull them back.
  // One indexed user-scoped query per tick, no forge or model call.
  //
  // A failed fetch keeps the last known count rather than blanking the badge: dropping to
  // zero on a transient error would read as "resolved", which is the one wrong answer.
  useEffect(() => {
    if (!user) {
      setWorkersAttention(0);
      return;
    }
    let alive = true;
    const load = () =>
      api
        .workerUpgradeSummary()
        .then((s) => {
          if (alive) setWorkersAttention(s.attention);
        })
        .catch(() => {});
    load();
    const t = window.setInterval(load, WORKERS_ATTENTION_POLL_MS);
    return () => {
      alive = false;
      window.clearInterval(t);
    };
  }, [user, location.pathname]);

  // Open-findings poll (PRD #333 M7, D8): the same on-navigation cadence as the Judge poll,
  // reading the GET /api/findings `open_count` meta with NO repo filter (the global count). A
  // failed fetch keeps the last known count rather than blanking the badge.
  useEffect(() => {
    if (!user) {
      setFindingsOpen(0);
      return;
    }
    let alive = true;
    api
      .listFindings()
      .then((data) => {
        if (alive) setFindingsOpen(data.open_count);
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [user, location.pathname]);

  // Status favicon (PRD #70 M4): mounted here so it lives on every route incl.
  // guest and survives logout (enabled flips false → reset to the static mark).
  // Reuses the unread count above — no second unread poll — and owns its own
  // runs poll (which fires while the tab is hidden). Called before the guest
  // early return so the hook order stays stable.
  useFavicon({ unread, enabled: !!user });

  if (!user) return <PublicShell>{children}</PublicShell>;

  // The board is the one screen that wants every pixel of width; text-heavy
  // pages read better on a measure.
  const fullBleed = /^\/repos\/[^/]+\/board/.test(location.pathname);

  return (
    // Width/padding are literal class strings in the ternary so Tailwind's JIT
    // emits both — an interpolated `lg:pl-${n}` would never be scanned.
    <div className={cx("min-h-screen", collapsed ? "lg:pl-14" : "lg:pl-60")}>
      {/* App-wide screen-reader alert for rate-limit tone crossings (PRD #54,
          Decision 4): a visually-hidden aria-live region mounted once so a
          window crossing into warn/danger announces on any route, not only
          while the Settings meters are on screen. */}
      <RateLimitAnnouncer />
      {/* Desktop sidebar */}
      <aside
        className={cx(
          "fixed inset-y-0 left-0 z-30 hidden border-r border-edge bg-surface lg:block",
          collapsed ? "w-14" : "w-60",
        )}
      >
        <SidebarContent
          collapsed={collapsed}
          onToggleCollapse={() => setCollapsed((c) => !c)}
          unread={unread}
          judgeTodo={judgeTodo}
          runsInProgress={runsInProgress}
          schedulesEnabled={schedulesEnabled}
          workersAttention={workersAttention}
          findingsOpen={findingsOpen}
          onOpenChangelog={() => setChangelogOpen(true)}
        />
      </aside>

      {/* Mobile top bar + sheet */}
      <div className="sticky top-0 z-20 flex items-center gap-3 border-b border-edge bg-surface px-4 py-3 lg:hidden">
        <button
          onClick={() => setMobileOpen(true)}
          aria-label="Open navigation"
          className="rounded-md p-1 text-muted hover:text-fg"
        >
          <MenuIcon />
        </button>
        {/* Reconciled to the same branded source as the other three surfaces
            (PRD #685 M3a): was the divergent iconless `uzi · dark factory`; now the
            shared app mark + the `uzi · uzinele întunecate` wording, honoring
            custom/white-label like the sidebar. An intended default-mode diff. */}
        <Link to="/dashboard" className="flex items-center gap-2 text-sm font-semibold tracking-tight">
          <AppMark branding={branding} className="h-7 w-7 rounded-lg text-lg" />
          {appMarkShowName(branding) && (
            <span>
              uzi <span className="font-normal text-faint">· uzinele întunecate</span>
            </span>
          )}
        </Link>
      </div>
      {mobileOpen && (
        <div className="fixed inset-0 z-40 lg:hidden">
          <div
            className="absolute inset-0 bg-black/60"
            onClick={() => setMobileOpen(false)}
            aria-hidden="true"
          />
          <div className="absolute inset-y-0 left-0 w-64 border-r border-edge bg-surface shadow-2xl">
            <button
              onClick={() => setMobileOpen(false)}
              aria-label="Close navigation"
              className="absolute right-3 top-4 z-10 rounded-md p-1 text-muted hover:text-fg"
            >
              <XIcon />
            </button>
            <SidebarContent onNavigate={() => setMobileOpen(false)} unread={unread} judgeTodo={judgeTodo} runsInProgress={runsInProgress} schedulesEnabled={schedulesEnabled} workersAttention={workersAttention} findingsOpen={findingsOpen} onOpenChangelog={() => setChangelogOpen(true)} />
          </div>
        </div>
      )}

      <main className="min-w-0 px-4 py-6 sm:px-6 lg:py-8">
        <div className={cx(!fullBleed && "mx-auto w-full max-w-[68rem]")}>
          {/* Vault locked banner (PRD #32): app-wide so the user can unlock from
              any page. Self-gates — renders nothing while unlocked. */}
          <VaultLockedBanner />
          {/* Both halves of the one canonical to-triage number (PRD #98). The setter is
              how the Judge page keeps it fresh after a dispose; the value is how the judge
              notification in the inbox reads the SAME number the nav badge above is
              rendering, rather than polling for its own copy. */}
          <JudgeTodoContext.Provider value={setJudgeTodo}>
            <JudgeTodoValueContext.Provider value={judgeTodo}>{children}</JudgeTodoValueContext.Provider>
          </JudgeTodoContext.Provider>
        </div>
      </main>

      {/* The ONE changelog drawer instance (PRD #415 M2). Mounted at AppShell scope,
          not inside SidebarContent, because the sidebar renders twice; both footer
          triggers set the same state. `runningVersion` is the footer's own version
          coordinate, undefined until the shared /api/version fetch settles. */}
      {changelogOpen && (
        <ChangelogDrawer
          runningVersion={build?.status === "ok" ? build.info.version : undefined}
          onClose={() => setChangelogOpen(false)}
        />
      )}
    </div>
  );
}
