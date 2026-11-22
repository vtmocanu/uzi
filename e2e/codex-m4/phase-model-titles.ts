export const PHASE_MODEL_TITLES = {
  switch: "Codex phase case 1: resumed model and effort switch carries nonce history",
  reclaim: "Codex phase case 2: a cold process resumes the same thread on model B",
  replan: "Codex phase case 3: same-thread re-plan switches back to model A",
  modelOmitted: "Codex phase case 4: model omission is inapplicable to production requests",
  effortOmitted: "Codex phase case 5: omitted and null effort persist warm and revert cold",
  barrier: "Codex phase case 9: resume completes before provider work and turn start admits it",
} as const;
