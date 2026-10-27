/** Explicit executing-brief pins, separate from the byte-frozen corpus. */
export type ReplayProfile = "baseline" | "revised";
export const REPLAY_PROFILES = {
  baseline: {
    codex: "29e410c44476fb6e2560fd03c2d990de24cee2193e6df6c250085cb1b39252a5",
    claude: "27f677678fbf3ffb78c08ec6d5549ca4ceaf722b6997e8599a5fe03092413192",
  },
  revised: {
    codex: "d8dd1257d920c6aa5f467d54f7a7b169ef445facf05932d45790d7f500fc5841",
    claude: "60dbc88ffad1ff0f7cef763ca71d6263541ffe9573be2082435406db32822321",
  },
} as const;
