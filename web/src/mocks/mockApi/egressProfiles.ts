import type {
  EgressProfile,
  EgressProfileProblem,
  EgressProfileWarning,
  EgressProfileWriteInput,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { daysAgo } from "../data";
import { delay, requireAdmin } from "./shared";

// Egress profiles, "site lists" (PRD #1906 M1/M1w), for the demo build. A deliberately
// SMALL second implementation of api/internal/egressprofile: it mirrors the 422 envelope
// (reason "invalid_egress_profile", one problem per field/entry), the stable codes the
// editor branches on, and the multi-publisher override rule, over a short excerpt of the
// built-in list. It does not reproduce IDNA or the Public Suffix List; the Go package is
// the source of truth and the Go tests pin it.

// A short excerpt of multipublisher.go: exact hosts, and whole domains whose subdomains are
// shared too.
const MULTI_PUBLISHER_HOSTS: Record<string, string> = {
  "github.com": "code hosting",
  "api.github.com": "code hosting",
  "gist.github.com": "code hosting",
  "gitlab.com": "code hosting",
  "readthedocs.io": "documentation hosting",
  "pypi.org": "package hosting",
  "docs.google.com": "document sharing",
  "storage.googleapis.com": "object storage",
};
const MULTI_PUBLISHER_DOMAINS: Record<string, string> = {
  "githubusercontent.com": "code hosting",
  "huggingface.co": "model and dataset hosting",
  "stackoverflow.com": "a community forum",
  "medium.com": "blog hosting",
};
// Stand-ins for the Public Suffix List checks.
const PUBLIC_SUFFIXES = new Set(["com", "org", "net", "io", "co.uk", "github.io", "cloudfront.net"]);

const NAME_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;
const LABEL_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

type Refusal = { code: string; message: string };

function normalizeEntry(raw: string): string | Refusal {
  const s = raw.trim().toLowerCase().replace(/\.$/, "");
  if (s === "") return { code: "empty", message: "an entry must not be empty" };
  if (/\s/.test(s)) return { code: "whitespace", message: "an entry must not contain spaces" };
  if (s.includes("://")) return { code: "scheme", message: "list a host name, not a URL: drop the scheme" };
  if (s.includes("@")) return { code: "userinfo", message: "an entry must not carry user information" };
  if (/[/?#]/.test(s)) return { code: "path", message: "list a host name only: drop the path, query or fragment" };
  if (/^\[.*\]$/.test(s) || /^\d{1,3}(\.\d{1,3}){3}$/.test(s)) {
    return { code: "ip_address", message: "IP addresses are not allowed; list a host name" };
  }
  if (/:\d+$/.test(s)) return { code: "port", message: "an entry must not carry a port" };
  if (s === "*") {
    return { code: "bare_wildcard", message: 'a bare "*" matches every host and is not allowed; use "*.example.com"' };
  }
  const wildcard = s.startsWith("*.");
  const base = wildcard ? s.slice(2) : s;
  if (base.includes("*")) {
    return { code: "wildcard_position", message: '"*" is allowed only as the whole leftmost label, as in "*.example.com"' };
  }
  const labels = base.split(".");
  if (labels.some((l) => l === "")) return { code: "empty_label", message: "an entry must not have an empty label" };
  if (labels.length < 2) return { code: "single_label", message: `${base} is a single-label name, not a public host` };
  if (!labels.every((l) => LABEL_RE.test(l))) {
    return { code: "invalid_host", message: `${base} is not a valid host name` };
  }
  if (PUBLIC_SUFFIXES.has(base)) {
    return wildcard
      ? {
          code: "public_suffix_wildcard",
          message: `"*.${base}" is refused: ${base} is a public suffix, so the wildcard would cover sites run by unrelated owners`,
        }
      : { code: "public_suffix_host", message: `"${base}" is a top-level or registry domain, not a site` };
  }
  return wildcard ? `*.${base}` : base;
}

function multiPublisher(entry: string): string | null {
  const flaggedDomain = (host: string) =>
    Object.keys(MULTI_PUBLISHER_DOMAINS).find((d) => host === d || host.endsWith(`.${d}`));
  if (entry.startsWith("*.")) {
    const base = entry.slice(2);
    const d = flaggedDomain(base);
    if (d) return `${entry} is under ${d} (${MULTI_PUBLISHER_DOMAINS[d]}, many publishers)`;
    const h = Object.keys(MULTI_PUBLISHER_HOSTS).find((x) => x.endsWith(`.${base}`));
    if (h) return `${entry} covers ${h} (${MULTI_PUBLISHER_HOSTS[h]}, many publishers)`;
    return null;
  }
  if (MULTI_PUBLISHER_HOSTS[entry]) return `${entry} is ${MULTI_PUBLISHER_HOSTS[entry]}: many publishers share this host`;
  const d = flaggedDomain(entry);
  if (d === entry) return `${entry} is ${MULTI_PUBLISHER_DOMAINS[d]}: many publishers share this host`;
  if (d) return `${entry} is under ${d} (${MULTI_PUBLISHER_DOMAINS[d]}, many publishers)`;
  return null;
}

// warningsFor mirrors egressprofile.WarningsFor for stored (already normalized) entries.
function warningsFor(hosts: string[], overrides: string[]): EgressProfileWarning[] {
  const out: EgressProfileWarning[] = [];
  for (const h of hosts) {
    const reason = multiPublisher(h);
    if (!reason) continue;
    out.push(
      overrides.includes(h)
        ? {
            entry: h,
            code: "multi_publisher_override",
            message: `${reason}; admitted by an explicit override, so every publisher on it is reachable`,
          }
        : {
            entry: h,
            code: "multi_publisher_needs_override",
            message: `${reason}; it has no multi_publisher_override, so it matches nothing. Add the override to accept every publisher on it, or remove it`,
          },
    );
  }
  return out;
}

type StoredProfile = Omit<EgressProfile, "warnings">;

function seed(
  id: string,
  name: string,
  description: string,
  hosts: string[],
  overrides: string[],
  ageDays: number,
): StoredProfile {
  return {
    id,
    name,
    description,
    hosts,
    multi_publisher_override: overrides,
    created_by: "u-admin",
    updated_by: "u-admin",
    created_at: daysAgo(ageDays),
    updated_at: daysAgo(Math.max(0, ageDays - 2)),
  };
}

// Three seeds: a clean list, one with an accepted multi-publisher override, and one whose
// stored entry the (grown) built-in list now flags without an override, so the demo shows
// every read-time warning the page renders.
let profiles: StoredProfile[] = [
  seed(
    "ep-kernel",
    "linux-kernel-sources",
    "Kernel release notes and the upstream mirror of the stable tree",
    ["www.kernel.org", "docs.kernel.org", "github.com"],
    ["github.com"],
    12,
  ),
  seed(
    "ep-models",
    "ml-model-cards",
    "Model cards and the papers they cite",
    ["arxiv.org", "huggingface.co"],
    [],
    30,
  ),
  seed(
    "ep-vendor",
    "vendor-x-docs",
    "Vendor X official documentation and datasheets",
    ["docs.vendor-x.com", "*.cdn.vendor-x.com", "vendor-x.com"],
    [],
    5,
  ),
];

const withWarnings = (p: StoredProfile): EgressProfile => ({
  ...p,
  warnings: warningsFor(p.hosts, p.multi_publisher_override),
});

function invalid(problems: EgressProfileProblem[]): ApiError {
  return new ApiError(422, "the egress profile is invalid: see problems", {
    error: "the egress profile is invalid: see problems",
    reason: "invalid_egress_profile",
    problems,
  });
}

// validate mirrors egressprofile.Validate: every problem at once, entries normalized and
// de-duplicated in order, an override kept only for a flagged entry.
function validate(
  input: EgressProfileWriteInput & { name?: string },
  checkName: boolean,
): { hosts: string[]; overrides: string[] } {
  const problems: EgressProfileProblem[] = [];
  if (checkName && !NAME_RE.test(input.name ?? "")) {
    problems.push({
      field: "name",
      code: "invalid_name",
      message:
        "name must be 1-64 characters of lowercase letters, digits and hyphens, starting with a letter or digit",
    });
  }
  if ([...input.description].length > 500) {
    problems.push({ field: "description", code: "description_too_long", message: "description must be at most 500 characters" });
  } else if (input.description !== input.description.trim()) {
    problems.push({ field: "description", code: "unsafe_text", message: "description must not start or end with spaces" });
  }
  if (input.hosts.length === 0) {
    problems.push({ field: "hosts", code: "no_entries", message: "a profile needs at least one host entry" });
  } else if (input.hosts.length > 200) {
    problems.push({ field: "hosts", code: "too_many_entries", message: `a profile may list at most 200 host entries (got ${input.hosts.length})` });
  }
  const override = new Set<string>();
  input.multi_publisher_override.forEach((raw, i) => {
    const n = normalizeEntry(raw);
    if (typeof n === "string") override.add(n);
    else problems.push({ field: `multi_publisher_override[${i}]`, entry: raw, ...n });
  });
  const hosts: string[] = [];
  const overrides: string[] = [];
  input.hosts.forEach((raw, i) => {
    const n = normalizeEntry(raw);
    if (typeof n !== "string") {
      problems.push({ field: `hosts[${i}]`, entry: raw, ...n });
      return;
    }
    if (hosts.includes(n)) return;
    const reason = multiPublisher(n);
    if (reason && !override.has(n)) {
      problems.push({
        field: `hosts[${i}]`,
        entry: raw,
        code: "multi_publisher_needs_override",
        message: `${reason}; allowing it admits every publisher on it. Add it to multi_publisher_override to accept that`,
      });
      return;
    }
    if (reason) overrides.push(n);
    hosts.push(n);
  });
  input.multi_publisher_override.forEach((raw, i) => {
    const n = normalizeEntry(raw);
    if (typeof n === "string" && !input.hosts.some((h) => normalizeEntry(h) === n)) {
      problems.push({
        field: `multi_publisher_override[${i}]`,
        entry: raw,
        code: "override_not_in_hosts",
        message: `override "${n}" does not name one of the profile's host entries`,
      });
    }
  });
  if (problems.length > 0) throw invalid(problems);
  return { hosts, overrides };
}

export const egressProfilesApi = {
  adminListEgressProfiles: async () => {
    requireAdmin();
    const sorted = [...profiles].sort((a, b) => a.name.localeCompare(b.name));
    return delay({ egress_profiles: sorted.map(withWarnings) });
  },
  adminCreateEgressProfile: async (input: EgressProfileWriteInput & { name: string }) => {
    const actor = requireAdmin();
    const { hosts, overrides } = validate(input, true);
    if (profiles.some((p) => p.name === input.name)) {
      throw new ApiError(409, "an egress profile with that name already exists");
    }
    const now = new Date().toISOString();
    const p: StoredProfile = {
      id: `ep-${Date.now().toString(36)}`,
      name: input.name,
      description: input.description,
      hosts,
      multi_publisher_override: overrides,
      created_by: actor.id,
      updated_by: actor.id,
      created_at: now,
      updated_at: now,
    };
    profiles = [...profiles, p];
    return delay({ egress_profile: withWarnings(p) });
  },
  adminUpdateEgressProfile: async (name: string, input: EgressProfileWriteInput) => {
    const actor = requireAdmin();
    const existing = profiles.find((p) => p.name === name);
    if (!existing) throw new ApiError(404, "egress profile not found");
    const { hosts, overrides } = validate(input, false);
    const next: StoredProfile = {
      ...existing,
      description: input.description,
      hosts,
      multi_publisher_override: overrides,
      updated_by: actor.id,
      updated_at: new Date().toISOString(),
    };
    profiles = profiles.map((p) => (p.name === name ? next : p));
    return delay({ egress_profile: withWarnings(next) });
  },
  adminDeleteEgressProfile: async (name: string) => {
    requireAdmin();
    if (!profiles.some((p) => p.name === name)) throw new ApiError(404, "egress profile not found");
    profiles = profiles.filter((p) => p.name !== name);
    return delay(null);
  },
};
