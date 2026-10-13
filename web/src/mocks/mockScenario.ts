// mockScenario reads a demo scenario from ?mock= (or the uzi_mock_scenario
// localStorage key) so MOCK_MODE demo builds and manual QA can reach the PRD #45
// OIDC UX, which is otherwise hidden (OIDC off / password on). Unknown/absent keeps
// the original behavior. Wrapped in try/catch for any non-browser context.
export function mockScenario(): string {
  try {
    const q = new URLSearchParams(window.location.search).get("mock");
    if (q) return q;
    return window.localStorage.getItem("uzi_mock_scenario") ?? "";
  } catch {
    return "";
  }
}

