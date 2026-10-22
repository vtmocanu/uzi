export function createCriteria(onFailure: (label: string) => void) {
  const criterionFailures: string[] = [];
  return {
    requireCriterion(label: string, passed: boolean) {
      if (!passed) {
        criterionFailures.push(label);
        onFailure(label);
      }
    },
    result() {
      return {
        status: criterionFailures.length ? "FAIL" : "PASS",
        exitCode: criterionFailures.length ? 1 : 0,
        criterionFailures: [...criterionFailures],
      };
    },
  };
}
