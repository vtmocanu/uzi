import assert from "node:assert/strict";

const required = [
  "TestRealObservationSupervisedTrees",
  "TestRealObservationSupervisedTrees/repeat-and-detached-doublefork",
  "TestRealObservationSupervisedTrees/two-oversized-siblings-stay-alive",
];

/** Exit zero alone also describes an empty or skipped Go test selection. */
export function requireSupervisorObservationPasses(output: string): void {
  assert.doesNotMatch(output, /^\s*--- (?:SKIP|FAIL): TestRealObservationSupervisedTrees(?:[\s/]|$)/m);
  for (const name of required) {
    assert.match(output, new RegExp(`^\\s*--- PASS: ${name}(?:\\s|$)`, "m"),
      `required real supervisor fixture did not pass: ${name}`);
  }
}
