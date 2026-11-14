import type { Options as SdkOptions } from "@anthropic-ai/claude-agent-sdk";

/** Inline settings only: a settings file cannot be merged without loading another source. */
type InlineSettings = Exclude<NonNullable<SdkOptions["settings"]>, string>;

/** Vendor builtins are explicit runtime configuration, independent of settingSources/plugins. */
const DISABLED_BUILTINS = { "cc-plugin-privacy@builtin": false } as const;
const DISABLED_ISOLATED_BUILTINS = {
  "agents-md@builtin": false,
  "cc-plugin-plugin-authoring@builtin": false,
} as const;

/** Preserve caller settings while making the worker's builtin denials authoritative. */
export function claudeSdkSettings(settings: SdkOptions["settings"] = {}, isolated = false): InlineSettings {
  if (typeof settings === "string") throw new Error("Claude SDK settings must be inline to enforce builtin plugin denials");
  return {
    ...settings,
    enabledPlugins: {
      ...settings.enabledPlugins,
      ...(isolated ? DISABLED_ISOLATED_BUILTINS : {}),
      ...DISABLED_BUILTINS,
    },
  };
}
