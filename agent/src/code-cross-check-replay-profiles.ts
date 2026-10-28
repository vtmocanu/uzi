/** Explicit executing-brief pins, separate from the byte-frozen corpus. */
export type ReplayProfile = "baseline" | "revised";
export const REPLAY_PROFILES = {
  baseline: {
    codex: "29e410c44476fb6e2560fd03c2d990de24cee2193e6df6c250085cb1b39252a5",
    claude: "27f677678fbf3ffb78c08ec6d5549ca4ceaf722b6997e8599a5fe03092413192",
  },
  revised: {
    codex: "67ea80eefc42d66f40db3774f6845f6ad9d2d5bd143bc1d28b9cc3fb3a2c85be",
    claude: "3ec4bad6fcf40e0a3e24aa16872cfc979cc9a886b6c38ef7d940339da2cf6651",
  },
} as const;
