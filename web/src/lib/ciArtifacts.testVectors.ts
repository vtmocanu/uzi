// Shared form/mock vectors for ciartifacts.NormalizeSelectors.
export const validArtifactSelectors = [
  { input: [" reports-* ", "reports/*.txt"], expected: ["reports-*", "reports/*.txt"] },
  { input: ["literal\\*", "[^a-z]", "[é-界]", "é".repeat(100)], expected: ["literal\\*", "[^a-z]", "[é-界]", "é".repeat(100)] },
  { input: ["\u00a0logs\u3000"], expected: ["logs"] },
  { input: Array.from({ length: 20 }, (_, i) => `report-${i}`), expected: Array.from({ length: 20 }, (_, i) => `report-${i}`) },
];
export const invalidArtifactSelectors: string[][] = [
  ["a", " a "], ["é".repeat(101)], ["["], ["[]"], ["[a-]"], ["trailing\\"],
  ["x\u0000"], ["x\u202e"], ["\tx"], ["x\u0085"], ["\ufefflogs"],
  ["x\ud800"], Array.from({ length: 21 }, (_, i) => `report-${i}`),
];
