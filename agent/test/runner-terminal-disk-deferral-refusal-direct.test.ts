import { describe, it } from "node:test";
import { installHarness } from "./runner-harness.js";
import {
  assertTrustedRefusalCase,
  terminalRefusalCases,
  splitRefusalRepresentations,
} from "./runner-terminal-disk-deferral-fixture.js";

installHarness();

describe("terminal execution disk deferral", () => {
  for (const entry of terminalRefusalCases.filter(entry => entry.representation === splitRefusalRepresentations[0])) {
    it(`trusted refusal: ${entry.reason} [${entry.representation}]`, () => assertTrustedRefusalCase(entry));
  }
});
