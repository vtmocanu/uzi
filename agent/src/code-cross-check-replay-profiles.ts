/** Explicit executing-brief pins, separate from the byte-frozen corpus. */
export type ReplayProfile = "baseline" | "revised";
export const REPLAY_PROFILES = {
  baseline: {
    codex: "29e410c44476fb6e2560fd03c2d990de24cee2193e6df6c250085cb1b39252a5",
    claude: "27f677678fbf3ffb78c08ec6d5549ca4ceaf722b6997e8599a5fe03092413192",
  },
  revised: {
    codex: "9f6489adf9e43295e04fcfe688d55408b734f3fe0c35e72f02390ba48850d437",
    claude: "74a5d66bbdf4471ac75b5363191c37cfd74706dcf3cd5f76e28d9449f8fae879",
  },
} as const;
