// Settings and Admin tabs plus the Admin Settings sections, as a static list the
// command menu can search. Kept beside navItems.tsx: that file lists the sidebar
// destinations, this one lists the places inside them.
//
// Labels read "Settings › Appearance", "Admin › Settings › Agent source". The tab
// paths and labels mirror SettingsShell's and AdminShell's TABS (a test pins them),
// and every path must resolve to a real, non-redirect route in APP_ROUTES.

interface SectionDef {
  label: string;
  to: string;
  keywords: string[];
}

export interface SectionItem {
  key: string;
  label: string;
  to: string;
  keywords: string[];
  adminOnly?: boolean;
}

const SEP = " › ";

// /workers is canonical; /settings/workers only redirects to it.
const SETTINGS_TABS: SectionDef[] = [
  { label: "Account & tokens", to: "/settings", keywords: ["account", "token", "profile", "password", "api"] },
  { label: "Models", to: "/settings/models", keywords: ["model", "models", "effort", "harness", "planning", "cross-check", "judge", "summary"] },
  { label: "Run defaults", to: "/settings/run-defaults", keywords: ["run", "defaults", "harness", "overflow"] },
  { label: "Forge", to: "/settings/forge", keywords: ["github", "gitlab", "connection", "repo"] },
  { label: "Access", to: "/settings/access", keywords: ["tokens", "permissions", "sharing", "oauth"] },
  { label: "Memory", to: "/settings/memory", keywords: ["notes", "context", "remember"] },
  { label: "Appearance", to: "/settings/appearance", keywords: ["theme", "dark", "light", "typeface", "mode"] },
  { label: "Workers", to: "/workers", keywords: ["fleet", "hosted", "docker", "runner"] },
];

// The admin tab labelled "Instance" in AdminShell is advertised as "Settings".
const ADMIN_TABS: SectionDef[] = [
  { label: "Users", to: "/admin/users", keywords: ["accounts", "people", "invite"] },
  { label: "Rate limits", to: "/admin/rate-limits", keywords: ["throttle", "quota", "limit"] },
  { label: "Tool allowlist", to: "/admin/tool-allowlist", keywords: ["tools", "allow", "permissions"] },
  { label: "Blocked repos", to: "/admin/blocked-repos", keywords: ["block", "deny", "repositories"] },
  { label: "Settings", to: "/admin/settings", keywords: ["instance", "configuration", "config"] },
  { label: "Workers", to: "/admin/workers", keywords: ["fleet", "hosted", "docker", "ephemeral", "hibernation", "capability", "scheduling"] },
  { label: "Branding", to: "/admin/branding", keywords: ["logo", "company", "brand"] },
  { label: "Site lists", to: "/admin/egress-profiles", keywords: ["egress", "profiles", "domains", "network"] },
  { label: "Shared providers", to: "/admin/shared-providers", keywords: ["provider", "gateway", "claude", "shared", "keys"] },
  { label: "Products", to: "/admin/products", keywords: ["external", "jobs"] },
  { label: "Health", to: "/admin/health", keywords: ["status", "checks", "diagnostics"] },
];

// In page order. Labels follow the page's own section index where it has one, else
// the card's visible title. vault-migration and sso are deliberately absent: those
// cards render only under some configurations, so a menu entry could dead-end.
const ADMIN_SETTINGS_ANCHORS: { id: string; label: string; keywords: string[] }[] = [
  { id: "vault-policy", label: "Credential protection", keywords: ["vault", "policy", "encryption", "password"] },
  { id: "model-defaults", label: "Model defaults", keywords: ["model", "judge", "summary", "claude", "codex", "default"] },
  { id: "forge-labels", label: "Forge labels", keywords: ["labels", "uzi", "autopilot", "appearance"] },
  { id: "run-judge", label: "Run judge", keywords: ["judge", "recommendations", "model"] },
  { id: "run-summaries", label: "Run summaries", keywords: ["summary", "now", "kill-switch"] },
  { id: "agent-source", label: "Agent source", keywords: ["sync", "roles", "templates", "git"] },
  { id: "maintenance", label: "Maintenance mode", keywords: ["drain", "pause", "downtime"] },
  { id: "updates", label: "Updates", keywords: ["release", "version", "upgrade", "banner"] },
  { id: "slack", label: "Slack", keywords: ["notifications", "chat", "bot"] },
  { id: "run-health", label: "Run health", keywords: ["stall", "nudge", "timeout", "health"] },
  { id: "review-bots", label: "Review bots", keywords: ["trusted", "coderabbit", "greptile", "rework"] },
  { id: "completion-interlock", label: "Completion check", keywords: ["completion", "interlock", "milestones"] },
  { id: "github-project-sync", label: "GitHub Projects sync", keywords: ["github", "projects", "board"] },
  { id: "fetch-caps", label: "Research fetch caps", keywords: ["fetch", "download", "bytes", "limits"] },
];

// The worker-fleet cards, on Admin → Workers (they used to live on Instance, so the
// keywords keep the old and new vocabulary findable). In page order.
const ADMIN_WORKERS_ANCHORS: { id: string; label: string; keywords: string[] }[] = [
  { id: "docker-allowlist", label: "Docker workers", keywords: ["docker", "allowlist", "repos"] },
  { id: "worker-sizes", label: "Worker sizes", keywords: ["sizes", "cpu", "memory", "disk", "resources", "ceilings", "quota"] },
  { id: "hosted-worker-quota", label: "Hosted worker quota", keywords: ["workers", "quota", "limit", "default size", "size"] },
  { id: "custody-hold-limit", label: "Custody hold limit", keywords: ["custody", "recovery", "hold"] },
  { id: "ephemeral-workers", label: "Ephemeral workers", keywords: ["workers", "run-bound", "autoscale", "cap", "per-user", "lease", "ephemeral size", "default size"] },
  { id: "worker-hibernation", label: "Worker hibernation", keywords: ["idle", "sleep", "workers"] },
  { id: "capability-scheduling", label: "Capability scheduling", keywords: ["capabilities", "scheduler", "workers"] },
];

// The anchors AdminSettings may scroll to once its cards have rendered.
export const ADMIN_SETTINGS_ANCHOR_IDS: ReadonlySet<string> = new Set(
  ADMIN_SETTINGS_ANCHORS.map((a) => a.id),
);

// The anchors AdminWorkers may scroll to; AdminSettings also forwards these ids to it.
export const ADMIN_WORKERS_ANCHOR_IDS: ReadonlySet<string> = new Set(
  ADMIN_WORKERS_ANCHORS.map((a) => a.id),
);

const ALL_SECTION_ITEMS: SectionItem[] = [
  ...SETTINGS_TABS.map((t) => ({
    key: `settings:${t.to}`,
    label: `Settings${SEP}${t.label}`,
    to: t.to,
    keywords: t.keywords,
  })),
  ...ADMIN_TABS.map((t) => ({
    key: `admin:${t.to}`,
    label: `Admin${SEP}${t.label}`,
    to: t.to,
    keywords: t.keywords,
    adminOnly: true,
  })),
  ...ADMIN_SETTINGS_ANCHORS.map((a) => ({
    key: `admin-settings:${a.id}`,
    label: `Admin${SEP}Settings${SEP}${a.label}`,
    to: `/admin/settings#${a.id}`,
    keywords: a.keywords,
    adminOnly: true,
  })),
  ...ADMIN_WORKERS_ANCHORS.map((a) => ({
    key: `admin-workers:${a.id}`,
    label: `Admin${SEP}Workers${SEP}${a.label}`,
    to: `/admin/workers#${a.id}`,
    keywords: a.keywords,
    adminOnly: true,
  })),
];

// The flat, ordered list a viewer can reach; admin entries only for admins.
export function visibleSectionItems(isAdmin: boolean): SectionItem[] {
  return ALL_SECTION_ITEMS.filter((item) => !item.adminOnly || isAdmin);
}
