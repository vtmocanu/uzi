// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { IssueCard } from "./IssueCard";
import { api, type Card } from "../../lib/api";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../../mocks/data/effectiveSettings";
import { StartPreviewProvider } from "../../lib/startPreviewStore";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { getEffectiveSettings: vi.fn(), previewStartSettings: vi.fn() } };
});
// The preview store reads the signed-in user; only the preview describe renders a provider.
vi.mock("../../auth/AuthContext", () => ({ useOptionalAuth: () => ({ user: { id: "u1" } }) }));

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const FAILED_PIPELINE: NonNullable<Card["pipeline"]> = {
  status: "failed",
  web_url: "https://gitlab.example.com/o/r/-/pipelines/1",
  ref: "agent/issue-7",
  pipeline_id: 1,
  synced_at: new Date().toISOString(),
};

function aCard(over: Partial<Card> = {}): Card {
  return {
    iid: 7,
    title: "Fix the flaky build",
    state: "opened",
    labels: ["uzi"],
    web_url: "https://gitlab.example.com/o/r/-/issues/7",
    forge_type: "gitlab",
    author: "mira",
    has_prd_link: false,
    column: "Review",
    closed: false,
    conflict: false,
    forge_updated_at: new Date().toISOString(),
    latest_run: null,
    pipeline: null,
    ...over,
  };
}

function renderCard(
  over: Partial<Card> = {},
  props: Partial<Pick<Parameters<typeof IssueCard>[0], "gate" | "starting" | "onStart" | "onOptions">> = {},
) {
  return render(
    <MemoryRouter>
      <IssueCard
        card={aCard(over)}
        repoId="repo-1"
        chips={[]}
        laneLabel="Review"
        canMoveUp={false}
        canMoveDown={false}
        onMoveUp={vi.fn()}
        onMoveDown={vi.fn()}
        insertionEdge={null}
        gate={props.gate ?? { enabled: true, reason: "" }}
        starting={props.starting ?? false}
        onStart={props.onStart ?? vi.fn()}
        onOptions={props.onOptions}
        fixCiBusy={false}
        onFixCi={vi.fn()}
        uziLabel="uzi"
        isEligible
        canPromote={false}
        promoting={false}
        onPromote={vi.fn()}
        onDragStart={vi.fn()}
        onDragEnd={vi.fn()}
        dimmed={false}
      />
    </MemoryRouter>,
  );
}

// PRD #1650 D3a: once automatic CI fixing halts on the card's branch, fixing CI is up
// to the user, and the card says so with an "Autofix stopped" marker. The accessible name
// starts with the visible text (WCAG 2.5.3), and the copy never promises a Fix CI button:
// that button renders only for a failed cached pipeline, the marker in every halt state.
const haltLabel = (n: number) =>
  `Autofix stopped after ${n} ${n === 1 ? "attempt" : "attempts"}: fixing this failure is up to you.`;

describe("IssueCard — CI auto-fix halted marker (#1650 D3a)", () => {
  it("shows the marker beside the pipeline badge, with the attempt count in its name and title", () => {
    renderCard({ pipeline: FAILED_PIPELINE, ci_autofix_halted: true, ci_autofix_attempts: 3 });
    const expected = haltLabel(3);
    expect(expected).toBe(
      "Autofix stopped after 3 attempts: fixing this failure is up to you.",
    );
    const marker = screen.getByRole("img", { name: expected });
    expect(marker.textContent).toBe("Autofix stopped");
    expect(marker.getAttribute("title")).toBe(expected);
    // The pipeline badge and the Fix CI button still render next to it.
    expect(screen.getByRole("link", { name: /CI failed/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Fix CI" })).toBeTruthy();
  });

  it("uses the singular for one attempt", () => {
    renderCard({ pipeline: FAILED_PIPELINE, ci_autofix_halted: true, ci_autofix_attempts: 1 });
    expect(
      screen.getByRole("img", {
        name: "Autofix stopped after 1 attempt: fixing this failure is up to you.",
      }),
    ).toBeTruthy();
  });

  it("shows the marker where the badge would be when the card has no cached pipeline", () => {
    renderCard({ pipeline: null, ci_autofix_halted: true, ci_autofix_attempts: 2 });
    const marker = screen.getByRole("img", { name: haltLabel(2) });
    expect(marker.textContent).toBe("Autofix stopped");
    expect(marker.getAttribute("title")).toBe(haltLabel(2));
    expect(screen.queryByRole("link", { name: /^CI / })).toBeNull();
    // No Fix CI button renders without a failed pipeline, so the copy must not name one.
    expect(screen.queryByRole("button", { name: "Fix CI" })).toBeNull();
    expect(marker.getAttribute("aria-label")).not.toMatch(/Fix CI/);
  });

  it("does not promise Fix CI while a new pipeline runs", () => {
    renderCard({
      pipeline: { ...FAILED_PIPELINE, status: "running" },
      ci_autofix_halted: true,
      ci_autofix_attempts: 3,
    });
    const marker = screen.getByRole("img", { name: haltLabel(3) });
    expect(marker.textContent).toBe("Autofix stopped");
    expect(screen.queryByRole("button", { name: "Fix CI" })).toBeNull();
  });

  it("is absent when ci_autofix_halted is false", () => {
    renderCard({ pipeline: FAILED_PIPELINE, ci_autofix_halted: false, ci_autofix_attempts: 1 });
    // Positive anchor: the card and its pipeline row did render.
    expect(screen.getByText("#7")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Fix CI" })).toBeTruthy();
    expect(screen.queryByText("Autofix stopped")).toBeNull();
    expect(screen.queryByRole("img", { name: /^Autofix stopped/ })).toBeNull();
  });

  it("is absent when the halt fields are missing (an older API replica)", () => {
    renderCard({ pipeline: FAILED_PIPELINE });
    expect(screen.getByText("#7")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Fix CI" })).toBeTruthy();
    expect(screen.queryByText("Autofix stopped")).toBeNull();
    expect(screen.queryByRole("img", { name: /^Autofix stopped/ })).toBeNull();
  });
});


it.each([["rate_limit", "a rate limit"], ["overload", "overload"]] as const)("Codex admission board card shows %s spread", (cls, words) => {
  const at = new Date(Date.now() + 60_037).toISOString();
  renderCard({ latest_run: {
    id: "held", status: "queued", health: "waiting_worker", health_since: null,
    health_reason: null, codex_admission_class: cls, codex_admission_eligible_at: at,
    created_at: new Date().toISOString(), updated_at: new Date().toISOString(),
    mr_iid: null, mr_web_url: null, mr_state: null, failure_reason: null,
    stop_kind: null, stop_reason: null, fail_origin: null, owner_name: "Demo", worker_name: null, is_mine: true, run_count: 1,
  } });
  expect(screen.getByText(`waiting: provider reported ${words}; account admission eligible after ${at}`)).toBeTruthy();
});


it.each([true, false])("active account wait card respects owner metadata (mine=%s)", is_mine => {
  const at = new Date(Date.now() + 60_037).toISOString();
  const r = { id: "same-run", status: "claimed", health: "waiting_account_cooldown", health_since: null, health_reason: null,
    ...(is_mine ? { account_cooldown_class: "overload", account_cooldown_eligible_at: at } : {}),
    created_at: new Date().toISOString(), updated_at: new Date().toISOString(), mr_iid: null, mr_web_url: null, mr_state: null,
    failure_reason: null, stop_kind: null, stop_reason: null, fail_origin: null, owner_name: "Demo", worker_name: null, is_mine, run_count: 1 } as NonNullable<Card["latest_run"]>;
  const { container } = renderCard({ latest_run: r });
  expect(container.textContent).toContain(is_mine ? `waiting for account cooldown · overload · earliest eligibility ${at}` : "claimed");
  expect(container.textContent).not.toMatch(/⚠|stuck|provider reported/);
  if (!is_mine) expect(container.textContent).not.toContain("waiting for account cooldown");
  expect(r.id).toBe("same-run");
});

// PRD #3250 M2 (user decision P1): the card keeps one-click Start run and adds a secondary
// Options button that opens the board's ONE Run options dialog. The card itself renders
// no pickers and no dialog (D9: never inside the draggable card).
describe("IssueCard — Start run and Options (PRD #3250 M2)", () => {
  it("renders Options with the accessible name Run options for #<iid> beside Start run", () => {
    const onOptions = vi.fn();
    const onStart = vi.fn();
    renderCard({}, { onOptions, onStart });
    const options = screen.getByRole("button", { name: "Run options for #7" });
    expect(options.textContent).toBe("Options");
    options.click();
    expect(onOptions).toHaveBeenCalledTimes(1);
    expect(onStart).not.toHaveBeenCalled();
    screen.getByRole("button", { name: "Start run" }).click();
    expect(onStart).toHaveBeenCalledTimes(1);
    expect(onOptions).toHaveBeenCalledTimes(1);
  });

  it("renders no inline harness or token picker and no dialog", () => {
    renderCard({}, { onOptions: vi.fn() });
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("puts Options under the same gate as Start run", () => {
    renderCard({}, { onOptions: vi.fn(), gate: { enabled: false, reason: "Connect a worker first (Settings → Workers)." } });
    const options = screen.getByRole("button", { name: "Run options for #7" }) as HTMLButtonElement;
    expect(options.disabled).toBe(true);
    expect(options.title).toBe("Connect a worker first (Settings → Workers).");
    expect((screen.getByRole("button", { name: "Start run (gated)" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("disables Options while this card's one-click start is in flight", () => {
    renderCard({}, { onOptions: vi.fn(), starting: true });
    expect((screen.getByRole("button", { name: "Run options for #7" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("renders no Options button when the board does not wire it", () => {
    renderCard();
    expect(screen.queryByRole("button", { name: /Run options/ })).toBeNull();
    expect(screen.getByRole("button", { name: "Start run" })).toBeTruthy();
  });
});

it("shows the issue key instead of #iid when the card carries one", () => {
  const { container } = renderCard({ issue_key: "ABC-12" });
  expect(container.textContent).toContain("ABC-12");
  expect(container.textContent).not.toContain("#7");
});

it("shows #iid when the card has no issue key", () => {
  const { container } = renderCard();
  expect(container.textContent).toContain("#7");
});

// Issue #3261: the quiet "if started now" line under the start row, and the gate reason as text.
describe("IssueCard — run-start preview", () => {
  const SCENARIO = RECORDED_EFFECTIVE_SCENARIOS[0];
  const mockApi = vi.mocked(api);

  function renderWithPreview(
    over: Partial<Card> = {},
    props: { gate?: { enabled: boolean; reason: string }; isEligible?: boolean; canPromote?: boolean } = {},
  ) {
    return render(
      <MemoryRouter>
        <StartPreviewProvider generation={1}>
          <IssueCard
            card={aCard(over)}
            repoId="repo-1"
            chips={[]}
            laneLabel="Review"
            canMoveUp={false}
            canMoveDown={false}
            onMoveUp={vi.fn()}
            onMoveDown={vi.fn()}
            insertionEdge={null}
            gate={props.gate ?? { enabled: true, reason: "" }}
            starting={false}
            onStart={vi.fn()}
            onOptions={vi.fn()}
            fixCiBusy={false}
            onFixCi={vi.fn()}
            uziLabel="uzi"
            isEligible={props.isEligible ?? true}
            canPromote={props.canPromote ?? false}
            promoting={false}
            onPromote={vi.fn()}
            onDragStart={vi.fn()}
            onDragEnd={vi.fn()}
            dimmed={false}
          />
        </StartPreviewProvider>
      </MemoryRouter>,
    );
  }

  beforeEach(() => {
    mockApi.getEffectiveSettings.mockResolvedValue(structuredClone(SCENARIO.get));
    mockApi.previewStartSettings.mockResolvedValue(
      structuredClone(SCENARIO.start_previews.find((p) => p.name === "claude")!.response),
    );
  });

  it("shows a closed disclosure with the server prediction on an eligible open card", async () => {
    renderWithPreview();
    const button = await screen.findByRole("button", { name: /If started now: Claude · opus · medium/ });
    expect(button.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(button);
    expect(button.getAttribute("aria-expanded")).toBe("true");
  });

  it("a gated card shows its reason as visible full-contrast text and still shows the preview", async () => {
    renderWithPreview({}, { gate: { enabled: false, reason: "Add a worker first." } });
    const reason = screen.getByText("Add a worker first.");
    expect(reason.tagName).toBe("P");
    expect(reason.className).toContain("text-xs");
    expect(reason.className).not.toMatch(/text-(faint|muted)/);
    expect(await screen.findByRole("button", { name: /If started now/ })).toBeTruthy();
  });

  it("an enabled card shows no gate line", async () => {
    renderWithPreview();
    await screen.findByRole("button", { name: /If started now/ });
    expect(screen.queryByText("Add a worker first.")).toBeNull();
  });

  it("a promotable card, a closed card and an ineligible card show no preview", async () => {
    renderWithPreview({}, { canPromote: true, isEligible: false });
    renderWithPreview({ closed: true });
    renderWithPreview({}, { isEligible: false });
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText(/If started now|Resolving/)).toBeNull();
    expect(mockApi.previewStartSettings).not.toHaveBeenCalled();
  });

  it("a failed preview shows the error text and leaves Start enabled", async () => {
    mockApi.previewStartSettings.mockRejectedValue(new Error("boom"));
    renderWithPreview();
    expect(await screen.findByText("Preview unavailable; Start will validate your choices.")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Start run" }) as HTMLButtonElement).disabled).toBe(false);
  });
});

// PRD 4173 M6: on a repo linked to an issue tracker the card's screen-reader labels and
// its forge link name the issue key, never "#iid" or a forge.
describe("IssueCard — a card carrying an issue key (PRD 4173 M6)", () => {
  // renderCard hands the card no neighbours, so its move buttons do not render.
  function renderMovable(over: Partial<Card>) {
    return render(
      <MemoryRouter>
        <IssueCard
          card={aCard(over)}
          repoId="repo-1"
          chips={[]}
          laneLabel="Review"
          canMoveUp
          canMoveDown
          onMoveUp={vi.fn()}
          onMoveDown={vi.fn()}
          insertionEdge={null}
          gate={{ enabled: true, reason: "" }}
          starting={false}
          onStart={vi.fn()}
          fixCiBusy={false}
          onFixCi={vi.fn()}
          uziLabel="uzi"
          isEligible
          canPromote={false}
          promoting={false}
          onPromote={vi.fn()}
          onDragStart={vi.fn()}
          onDragEnd={vi.fn()}
          dimmed={false}
        />
      </MemoryRouter>,
    );
  }

  it("names the key in the move buttons' aria-label and title", () => {
    renderMovable({ issue_key: "ABC-12" });
    for (const dir of ["up", "down"]) {
      const btn = screen.getByLabelText(`Move issue ABC-12 ${dir} in Review`);
      expect(btn.getAttribute("title")).toBe(`Move issue ABC-12 ${dir} in Review`);
    }
    expect(screen.queryByLabelText(/Move issue #7/)).toBeNull();
  });

  it("keeps #iid in the move labels when the card has no key", () => {
    renderMovable({});
    expect(screen.getByLabelText("Move issue #7 up in Review")).toBeTruthy();
  });

  it("names the key in the Options button", () => {
    renderCard({ issue_key: "ABC-12" }, { onOptions: vi.fn() });
    expect(screen.getByRole("button", { name: "Run options for ABC-12" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Run options for #7" })).toBeNull();
  });

  it("reads the forge link as Open in issue tracker, not Open on a forge", () => {
    renderCard({ issue_key: "ABC-12" });
    const link = screen.getByRole("link", { name: "Open in issue tracker" });
    expect(link.getAttribute("title")).toBe("Open in issue tracker");
    expect(screen.queryByRole("link", { name: "Open on GitLab" })).toBeNull();
  });

  it("keeps Open on GitLab on an unlinked card", () => {
    renderCard();
    expect(screen.getByRole("link", { name: "Open on GitLab" })).toBeTruthy();
  });

  it("strips format characters out of the key it renders and names", () => {
    renderCard({ issue_key: "AB‮C-12" }, { onOptions: vi.fn() });
    expect(screen.getByText("ABC-12")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Run options for ABC-12" })).toBeTruthy();
  });
});

describe("IssueCard — Promote title names a tag on a card of a linked repo (PRD 4173 M6)", () => {
  function renderPromotable(over: Partial<Card> = {}) {
    return render(
      <MemoryRouter>
        <IssueCard
          card={aCard({ labels: [], ...over })}
          repoId="repo-1"
          chips={[]}
          laneLabel="Review"
          canMoveUp={false}
          canMoveDown={false}
          onMoveUp={vi.fn()}
          onMoveDown={vi.fn()}
          insertionEdge={null}
          gate={{ enabled: true, reason: "" }}
          starting={false}
          onStart={vi.fn()}
          fixCiBusy={false}
          onFixCi={vi.fn()}
          uziLabel="send-to-uzi"
          isEligible={false}
          canPromote
          promoting={false}
          onPromote={vi.fn()}
          onDragStart={vi.fn()}
          onDragEnd={vi.fn()}
          dimmed={false}
        />
      </MemoryRouter>,
    );
  }

  it("says tag on a keyed card", () => {
    renderPromotable({ issue_key: "ABC-12" });
    expect(screen.getByRole("button", { name: /Promote to send-to-uzi/ }).getAttribute("title")).toBe(
      "Add the send-to-uzi tag so uzi can work this issue",
    );
  });

  it("keeps label on an unkeyed card", () => {
    renderPromotable();
    expect(screen.getByRole("button", { name: /Promote to send-to-uzi/ }).getAttribute("title")).toBe(
      "Add the send-to-uzi label so uzi can work this issue",
    );
  });
});
