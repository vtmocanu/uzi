// Stable titles shared by the production-root tests, evidence and P catalog.
export const PROVIDER_ROOT_TITLES = {
  direct: "provider fake issue composes executor to pinned app-server and authenticated HTTP destination",
  same307: "provider production root characterizes HTTP 307 same-origin accepted receiver method body and authorization",
  same308: "provider production root characterizes HTTP 308 same-origin accepted receiver method body and authorization",
  cross307: "provider production root characterizes HTTP 307 cross-port unauthorized receiver method body and authorization",
  cross308: "provider production root characterizes HTTP 308 cross-port unauthorized receiver method body and authorization",
} as const;

/** PRD #4323: the provider-bound advice pass and summary pass through the production advice factory. */
export const PROVIDER_ADVICE_SUMMARY_TITLE =
  "provider advice and summary passes run on the frozen binding through the production factory against the pinned app-server";

/** Issue #4574: the provider-bound Codex judge through the production advice factory. */
export const PROVIDER_ADVICE_JUDGE_TITLE =
  "provider judge runs on the frozen binding through the production factory and settles a gateway 429 and 529 as a provider gateway limit against the pinned app-server";
