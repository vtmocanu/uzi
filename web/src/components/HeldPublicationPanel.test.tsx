// @vitest-environment jsdom
import { act } from "react";
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
    ["abandoned", "Not ours"],
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

  it("always shows the neutral intro and the expire hint only with an acknowledged row", async () => {
    serve([pub({ state: "refused", refusal_reason: "create_refused" })]);
    const { container, unmount } = render(<HeldPublicationPanel run={aRun()} />);
    expect(await screen.findByText("Not published")).toBeTruthy();
    expect(container.textContent).toContain("after a fresh secret scan, can be published to a run-scoped ref");
    expect(container.textContent).not.toContain("uzi run held expire");
    unmount();

    serve([pub({ state: "created" })]);
    const second = render(<HeldPublicationPanel run={aRun()} />);
    expect(await screen.findByText("Publishing")).toBeTruthy();
    expect(second.container.textContent).toContain("after a fresh secret scan, can be published to a run-scoped ref");
    expect(second.container.textContent).not.toContain("uzi run held expire");
    second.unmount();

    serve([pub({ state: "acknowledged" })]);
    const third = render(<HeldPublicationPanel run={aRun()} />);
    expect(await screen.findByText("Published")).toBeTruthy();
    expect(third.container.textContent).toContain("after a fresh secret scan, can be published to a run-scoped ref");
    expect(third.container.textContent).toContain("Delete this run's published refs now with uzi run held expire");
  });

  it.each(["deleted", "abandoned", "refused"])("hides Expires for a %s row with a past expiry", async (state) => {
    serve([pub({ state, expires_at: "2020-01-01T00:00:00Z" })]);
    render(<HeldPublicationPanel run={aRun()} />);
    await waitFor(() => expect(screen.getByText("Held work")).toBeTruthy());
    expect(screen.queryByText(/Expires:/)).toBeNull();
  });

  it("re-fetches every 20s while a row is in progress and stops once settled", async () => {
    vi.useFakeTimers();
    try {
      const spy = vi
        .spyOn(api, "getRunHeldPublications")
        .mockResolvedValueOnce({ publications: [pub({ state: "invoked" })] })
        .mockResolvedValue({ publications: [pub({ state: "acknowledged" })] });
      const { unmount } = render(<HeldPublicationPanel run={aRun()} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(screen.getByText("Confirming")).toBeTruthy();
      expect(spy).toHaveBeenCalledTimes(1);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(20_000);
      });
      expect(spy).toHaveBeenCalledTimes(2);
      expect(screen.getByText("Published")).toBeTruthy();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(60_000);
      });
      expect(spy).toHaveBeenCalledTimes(2);
      expect(screen.queryByText("Reload the page to refresh.")).toBeNull();
      unmount();
    } finally {
      vi.useRealTimers();
    }
  });

  it("stops polling on unmount", async () => {
    vi.useFakeTimers();
    try {
      const spy = serve([pub({ state: "created" })]);
      const { unmount } = render(<HeldPublicationPanel run={aRun()} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      unmount();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(60_000);
      });
      expect(spy).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  const ACK = "2026-01-01T00:05:00Z";

  it.each<[string, Partial<HeldPublication>, string]>([
    ["acknowledged", {}, "On the forge; deleted when retention expires."],
    ["created", {}, "On the forge; uzi either keeps it as the run's held copy or deletes it."],
    ["delete_unknown", {}, "Being removed from the forge."],
    ["deleted", { ref_created_at: ACK }, "Was on the forge; since removed."],
    ["deleted", { acknowledged_at: ACK }, "Was on the forge; since removed."],
    ["deleted", {}, "Not on the forge."],
    ["abandoned", {}, "The ref now points at a commit uzi did not publish; uzi leaves it untouched."],
    ["refused", {}, "The forge refused the publication; the recovery archive path applies."],
    ["invoked", {}, "Waiting for the forge to confirm; the recovery archive path applies."],
    ["create_unknown", {}, "Waiting for the forge to confirm; the recovery archive path applies."],
  ])("state %s %j shows the lead line %s", async (state, over, line) => {
    serve([pub({ state, ...over })]);
    render(<HeldPublicationPanel run={aRun()} />);
    expect(await screen.findByText(line)).toBeTruthy();
  });

  it.each(["prepared", "from_the_future"])("shows no lead line for a %s row", async (state) => {
    serve([pub({ state })]);
    const { container } = render(<HeldPublicationPanel run={aRun()} />);
    expect(await screen.findByText("Held")).toBeTruthy();
    expect(container.textContent).not.toMatch(/On the forge|Not on the forge|Was on the forge|forge refused|Waiting for the forge|Being removed/);
  });

  it("does not start the next poll while a request is pending", async () => {
    vi.useFakeTimers();
    try {
      let release: (v: { publications: HeldPublication[] }) => void = () => {};
      const spy = vi
        .spyOn(api, "getRunHeldPublications")
        .mockResolvedValueOnce({ publications: [pub({ state: "invoked" })] })
        .mockImplementationOnce(() => new Promise((res) => (release = res)))
        .mockResolvedValue({ publications: [pub({ state: "acknowledged" })] });
      const { unmount } = render(<HeldPublicationPanel run={aRun()} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(20_000);
      });
      expect(spy).toHaveBeenCalledTimes(2);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(120_000);
      });
      expect(spy).toHaveBeenCalledTimes(2);
      await act(async () => {
        release({ publications: [pub({ state: "invoked" })] });
        await vi.advanceTimersByTimeAsync(20_000);
      });
      expect(spy).toHaveBeenCalledTimes(3);
      unmount();
    } finally {
      vi.useRealTimers();
    }
  });

  it("stops polling after 30 re-fetches while a row stays in progress", async () => {
    vi.useFakeTimers();
    try {
      const spy = serve([pub({ state: "create_unknown" })]);
      const { unmount } = render(<HeldPublicationPanel run={aRun()} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(40 * 20_000);
      });
      expect(spy).toHaveBeenCalledTimes(31);
      expect(screen.getByText("Reload the page to refresh.")).toBeTruthy();
      unmount();
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps polling past an empty first answer and renders the publication once it appears", async () => {
    vi.useFakeTimers();
    try {
      const spy = vi
        .spyOn(api, "getRunHeldPublications")
        .mockResolvedValueOnce({ publications: [] })
        .mockResolvedValue({ publications: [pub({ state: "created" })] });
      const { unmount } = render(<HeldPublicationPanel run={aRun()} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(spy).toHaveBeenCalledTimes(1);
      expect(screen.queryByText("Held work")).toBeNull();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(20_000);
      });
      expect(spy).toHaveBeenCalledTimes(2);
      expect(screen.getByText("Held work")).toBeTruthy();
      expect(screen.getByText("Publishing")).toBeTruthy();
      unmount();
    } finally {
      vi.useRealTimers();
    }
  });

  it("stops polling once every row is terminal", async () => {
    vi.useFakeTimers();
    try {
      const spy = vi
        .spyOn(api, "getRunHeldPublications")
        .mockResolvedValueOnce({ publications: [] })
        .mockResolvedValue({ publications: [pub({ state: "acknowledged" })] });
      const { unmount } = render(<HeldPublicationPanel run={aRun()} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(20_000);
      });
      expect(spy).toHaveBeenCalledTimes(2);
      expect(screen.getByText("Published")).toBeTruthy();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10 * 20_000);
      });
      expect(spy).toHaveBeenCalledTimes(2);
      unmount();
    } finally {
      vi.useRealTimers();
    }
  });

  it("stops after 30 re-fetches when the list stays empty and renders nothing", async () => {
    vi.useFakeTimers();
    try {
      const spy = serve([]);
      const { unmount, container } = render(<HeldPublicationPanel run={aRun()} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(40 * 20_000);
      });
      expect(spy).toHaveBeenCalledTimes(31);
      expect(container.innerHTML).toBe("");
      unmount();
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps polling while a row is prepared and settles when it is published", async () => {
    vi.useFakeTimers();
    try {
      const spy = vi
        .spyOn(api, "getRunHeldPublications")
        .mockResolvedValueOnce({ publications: [pub({ state: "prepared" })] })
        .mockResolvedValue({ publications: [pub({ state: "acknowledged" })] });
      const { unmount } = render(<HeldPublicationPanel run={aRun()} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(screen.getByText("Held")).toBeTruthy();
      expect(spy).toHaveBeenCalledTimes(1);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(20_000);
      });
      expect(spy).toHaveBeenCalledTimes(2);
      expect(screen.getByText("Published")).toBeTruthy();
      unmount();
    } finally {
      vi.useRealTimers();
    }
  });

  it("clears the previous run's rows when the run changes and ignores its late response", async () => {
    const OTHER = "11111111-2222-4333-8444-555555555555";
    let releaseOld: (v: { publications: HeldPublication[] }) => void = () => {};
    vi.spyOn(api, "getRunHeldPublications")
      .mockImplementationOnce(() => new Promise((res) => (releaseOld = res)))
      .mockResolvedValue({ publications: [] });
    const { rerender, container } = render(<HeldPublicationPanel run={aRun()} />);
    rerender(<HeldPublicationPanel run={aRun({ id: OTHER })} />);
    await act(async () => {
      releaseOld({ publications: [pub()] });
    });
    expect(container.innerHTML).toBe("");
    expect(screen.queryByText("Published")).toBeNull();
  });

  it("drops the previous run's rendered rows while the next run's fetch is pending", async () => {
    const OTHER = "11111111-2222-4333-8444-555555555555";
    vi.spyOn(api, "getRunHeldPublications")
      .mockResolvedValueOnce({ publications: [pub({ state: "acknowledged" })] })
      .mockImplementationOnce(() => new Promise(() => {}));
    const { rerender, container } = render(<HeldPublicationPanel run={aRun()} />);
    expect(await screen.findByText("Published")).toBeTruthy();
    rerender(<HeldPublicationPanel run={aRun({ id: OTHER })} />);
    await waitFor(() => expect(container.innerHTML).toBe(""));
  });
});
