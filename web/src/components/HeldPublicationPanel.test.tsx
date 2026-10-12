// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { HeldPublicationPanel } from "./HeldPublicationPanel";
import { api, type HeldPublication, type Run } from "../lib/api";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const RUN_ID = "5f0c2a4e-8d1b-4c7a-9e3f-2b6d8a1c4e70";
const TIP = "9c41e07b2d5a8f3e61b0c7d94a2e5f18b3c6d0a7";

function aRun(over: Partial<Run> = {}): Run {
  return { id: RUN_ID, status: "failed", ...over } as Run;
}

function pub(over: Partial<HeldPublication> = {}): HeldPublication {
  return {
    id: "pub-1",
    run_id: RUN_ID,
    generation: 2,
    ref: `refs/uzi-held/${RUN_ID}/2`,
    tip: TIP,
    state: "acknowledged",
    created_at: "2026-01-01T00:00:00Z",
    expires_at: new Date(Date.now() + 3 * 86_400_000 + 60_000).toISOString(),
    ...over,
  };
}

function serve(publications: HeldPublication[]) {
  return vi.spyOn(api, "getRunHeldPublications").mockResolvedValue({ publications });
}

const fetchCmd = () => screen.queryByText(/^git fetch origin /);

describe("HeldPublicationPanel", () => {
  it("renders an acknowledged publication with ref, short tip, expiry and fetch command", async () => {
    serve([pub()]);
    render(<HeldPublicationPanel run={aRun()} />);
    expect(await screen.findByText("Published")).toBeTruthy();
    expect(screen.getByText(`refs/uzi-held/${RUN_ID}/2 @ ${TIP.slice(0, 12)}`)).toBeTruthy();
    expect(fetchCmd()?.textContent).toBe(`git fetch origin refs/uzi-held/${RUN_ID}/2`);
    expect(screen.getByText(/Expires:/)).toBeTruthy();
    expect(screen.getByText(/uzi run held expire/).textContent).toContain(RUN_ID);
    expect(screen.queryByRole("button", { name: /expire/i })).toBeNull();
  });

  it.each(["refs/uzi-held/other-run/2", `refs/uzi-held/${RUN_ID}/2; rm -rf /`, `refs/uzi-salvage/${RUN_ID}`, `refs/uzi-held/${RUN_ID}/x`, ""])(
    "hides the ref and fetch command for an unsafe or foreign ref %j",
    async (ref) => {
      serve([pub({ ref })]);
      render(<HeldPublicationPanel run={aRun()} />);
      expect(await screen.findByText("Published")).toBeTruthy();
      expect(fetchCmd()).toBeNull();
      expect(screen.queryByText(/refs\//, { selector: "span" })).toBeNull();
    },
  );

  it("offers no fetch command once the work is no longer fetchable", async () => {
    serve([pub({ state: "deleted" })]);
    render(<HeldPublicationPanel run={aRun()} />);
    expect(await screen.findByText("Removed")).toBeTruthy();
    expect(fetchCmd()).toBeNull();
  });

  it.each([
    ["acknowledged", "Published"],
    ["created", "Publishing"],
    ["invoked", "Confirming"],
    ["create_unknown", "Confirming"],
    ["delete_unknown", "Removing"],
    ["deleted", "Removed"],
    ["refused", "Not published"],
    ["abandoned", "Gone"],
    ["from_the_future", "Held"],
  ])("state %s shows the badge %s", async (state, badge) => {
    serve([pub({ state })]);
    render(<HeldPublicationPanel run={aRun()} />);
    expect(await screen.findByText(badge)).toBeTruthy();
  });

  it("renders last_error and refusal_reason as escaped text", async () => {
    serve([pub({ state: "refused", last_error: "<b>boom</b>‮", refusal_reason: "create_refused" })]);
    const { container } = render(<HeldPublicationPanel run={aRun()} />);
    expect(await screen.findByText("create_refused")).toBeTruthy();
    expect(container.querySelector("b")).toBeNull();
    expect(container.textContent).toContain("<b>boom</b>");
    expect(container.textContent).not.toContain("‮");
  });

  it("fetches nothing and renders nothing for a non-failed run", () => {
    const spy = serve([pub()]);
    const { container } = render(<HeldPublicationPanel run={aRun({ status: "running" })} />);
    expect(container.innerHTML).toBe("");
    expect(spy).not.toHaveBeenCalled();
  });

  it("renders nothing for an empty list", async () => {
    const spy = serve([]);
    const { container } = render(<HeldPublicationPanel run={aRun()} />);
    await waitFor(() => expect(spy).toHaveBeenCalled());
    expect(container.innerHTML).toBe("");
  });

  it("renders nothing when the fetch fails", async () => {
    const spy = vi.spyOn(api, "getRunHeldPublications").mockRejectedValue(new Error("404"));
    const { container } = render(<HeldPublicationPanel run={aRun()} />);
    await waitFor(() => expect(spy).toHaveBeenCalled());
    expect(container.innerHTML).toBe("");
  });
});
