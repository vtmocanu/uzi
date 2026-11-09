/** Explicit executing-brief pins, separate from the byte-frozen corpus. */
export type ReplayProfile = "baseline" | "revised";
export const REPLAY_PROFILES = {
  baseline: {
    codex: "29e410c44476fb6e2560fd03c2d990de24cee2193e6df6c250085cb1b39252a5",
    claude: "27f677678fbf3ffb78c08ec6d5549ca4ceaf722b6997e8599a5fe03092413192",
  },
  revised: {
    codex: "25ca469838ac6d97169866525891266f94354d19710d5573bd5f5d2b055b8669",
    claude: "ad434dc642a7b82dfeaded95350f5f63977dafb6342fe3f7923bebc2c9f4779e",
  },
} as const;
