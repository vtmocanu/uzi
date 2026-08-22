// Single source of truth for SPA → in-app-docs links (PRD #57 contextual doc
// links). Pages must NEVER hand-write "/docs/…" strings: import a slug constant
// from here and pass it to <DocLink> (which prepends "/docs/"). Each constant is
// a BARE slug (no "/docs/" prefix) and must name an `audience: user` doc under
// `docs/` — that invariant is enforced by doclinks.test.ts, which cross-checks
// every slug below against the bundled user-doc set. Add a slug here (and to
// ALL_DOC_SLUGS) rather than inlining a path at the call site.

export const DOC_WORKER_SETUP = "worker-setup";
export const DOC_ANTHROPIC_TOKEN = "anthropic-token";
export const DOC_SLACK = "slack";
export const DOC_REPO_AGENTS = "repo-agents";
export const DOC_AGENT_TEMPLATES = "agent-templates";
export const DOC_SKILLS = "skills";
export const DOC_WORKER_TOOLS = "worker-tools";
export const DOC_ADMIN_SETTINGS = "admin-settings";
export const DOC_RATE_LIMITS = "rate-limits";
export const DOC_BOT_SETUP_GITLAB = "gitlab-bot-setup";
export const DOC_BOT_SETUP_FORGEJO = "forgejo-bot-setup";
export const DOC_BOT_SETUP_GITHUB = "github-bot-setup";
export const DOC_GITHUB_PROJECT_SYNC = "github-project-sync";

// Every slug above, so doclinks.test.ts can iterate and assert validity.
export const ALL_DOC_SLUGS = [
  DOC_WORKER_SETUP,
  DOC_ANTHROPIC_TOKEN,
  DOC_SLACK,
  DOC_REPO_AGENTS,
  DOC_AGENT_TEMPLATES,
  DOC_SKILLS,
  DOC_WORKER_TOOLS,
  DOC_ADMIN_SETTINGS,
  DOC_RATE_LIMITS,
  DOC_BOT_SETUP_GITLAB,
  DOC_BOT_SETUP_FORGEJO,
  DOC_BOT_SETUP_GITHUB,
  DOC_GITHUB_PROJECT_SYNC,
] as const;
