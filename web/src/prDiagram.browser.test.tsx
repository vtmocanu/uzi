import { describe, expect, it } from "vitest";
import mermaid from "mermaid";
import flow from "../../fixtures/pr-diagram/flow.md?raw";
import sequenceTwo from "../../fixtures/pr-diagram/sequence-two.md?raw";
import sequenceMany from "../../fixtures/pr-diagram/sequence-many.md?raw";
import sequenceEnd from "../../fixtures/pr-diagram/sequence-end.md?raw";
import unicode from "../../fixtures/pr-diagram/unicode.md?raw";
import maxSize from "../../fixtures/pr-diagram/max-size.md?raw";

const goldens = [
  ["flow", flow],
  ["sequence-two", sequenceTwo],
  ["sequence-many", sequenceMany],
  ["sequence-end", sequenceEnd],
  ["unicode", unicode],
  ["max-size", maxSize],
] as const;

function emittedMermaid(region: string): string {
  const blocks = [...region.matchAll(/^```mermaid\r?\n([\s\S]*?)^```[ \t]*$/gm)];
  expect(blocks).toHaveLength(1);
  return blocks[0][1];
}

describe("PR description Mermaid goldens", () => {
  for (const [name, region] of goldens) {
    it(`parses ${name} in Chromium`, async () => {
      const result = await mermaid.parse(emittedMermaid(region));
      expect(result).toEqual(expect.objectContaining({ diagramType: expect.any(String) }));
    });
  }
});
