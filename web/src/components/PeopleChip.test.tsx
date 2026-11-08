// @vitest-environment jsdom
// Issue #4042 M2: the People chip and its popover, drawn from a hand-built PeopleModel.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { PeopleChip } from "./PeopleChip";
import type { ActivePerson, PeopleModel } from "../lib/overviewPeople";

afterEach(cleanup);

function person(over: Partial<ActivePerson> & { name: string }): ActivePerson {
  return {
    key: over.name,
    initials: over.name.slice(0, 2).toUpperCase(),
    you: false,
    deactivated: false,
    parts: { running: 1, queued: 0, waiting: 0, owner: 0 },
    pill: "1 running",
    tone: "ok",
    ...over,
  };
}

function model(over: Partial<PeopleModel> = {}): PeopleModel {
  const people = [
    person({ name: "Admin", you: true }),
    person({ name: "Mira", pill: "1 queued", tone: "neutral" }),
    person({ name: "Andrei", pill: "waits on them", tone: "warn" }),
    person({ name: "Dan", deactivated: true }),
  ];
  return {
    active: { state: "ok", people, atLeast: false, count: 4 },
    total: 9,
    signedIn: [{ key: "ioana", name: "Ioana", initials: "IO", you: false, ago: "3h ago" }],
    idle: { count: 3, minDays: 2, maxDays: 9, never: 1 },
    week: 5,
    usageAction: "available",
    ...over,
  };
}

const chip = () => screen.getByRole("button", { name: /^People:/ });
const open = () => fireEvent.click(chip());
const dialog = () => screen.getByRole("dialog", { name: "People using uzi" });

describe("PeopleChip trigger", () => {
  it("reads the count against the total", () => {
    render(<PeopleChip model={model()} onShowUsageByUser={() => {}} />);
    expect(chip().textContent).toBe("AD" + "MI" + "AN" + "+1" + "4 active now of 9");
    expect(chip().getAttribute("aria-label")).toBe("People: 4 active now of 9");
    expect(chip().getAttribute("aria-haspopup")).toBe("dialog");
    expect(chip().getAttribute("aria-expanded")).toBe("false");
  });

  it("says at least when the list may be cut off, and drops the total when it is unknown", () => {
    const people = [person({ name: "A" }), person({ name: "B" }), person({ name: "C" }), person({ name: "D" })];
    const { rerender } = render(
      <PeopleChip
        model={model({ active: { state: "ok", people, atLeast: true, count: 4 } })}
        onShowUsageByUser={() => {}}
      />,
    );
    expect(chip().getAttribute("aria-label")).toBe("People: at least 4 active now of 9");
    rerender(<PeopleChip model={model({ total: null })} onShowUsageByUser={() => {}} />);
    expect(chip().getAttribute("aria-label")).toBe("People: 4 active now");
  });

  it("says active now is unavailable, and renders nothing while loading", () => {
    const { rerender, container } = render(
      <PeopleChip model={model({ active: { state: "unavailable" } })} onShowUsageByUser={() => {}} />,
    );
    expect(chip().getAttribute("aria-label")).toBe("People: Active now unavailable");
    rerender(<PeopleChip model={model({ active: { state: "loading" } })} onShowUsageByUser={() => {}} />);
    expect(container.firstChild).toBeNull();
  });
});

describe("PeopleChip popover", () => {
  it("lists each active person with a pill, and marks you and a deactivated account", () => {
    render(<PeopleChip model={model()} onShowUsageByUser={() => {}} />);
    open();
    const d = within(dialog());
    expect(d.getByRole("heading", { name: "4 of 9 people active now" })).toBeTruthy();
    const rows = d.getAllByRole("listitem").map((li) => li.textContent);
    expect(rows).toContain("ADAdmin (you)1 running");
    expect(rows).toContain("MIMira1 queued");
    expect(rows).toContain("ANAndrei" + "waits on them");
    expect(rows).toContain("DADan (deactivated)1 running");
    expect(d.getByText("waits on them").className).toContain("text-warn");
    expect(d.getAllByText("1 running")[0].className).toContain("text-ok");
    expect(d.getByText("1 queued").className).not.toContain("text-ok");
  });

  it("shows the signed-in group with ages, the idle line and the week figure", () => {
    render(<PeopleChip model={model()} onShowUsageByUser={() => {}} />);
    open();
    const d = within(dialog());
    expect(d.getByText("Signed in within 24 hours, no listed active run")).toBeTruthy();
    expect(d.getByText("3h ago")).toBeTruthy();
    expect(d.getByText("3 people, last seen 2 to 9 days ago · 1 never signed in")).toBeTruthy();
    expect(d.getByText("This week: 5 people had runs with recorded usage")).toBeTruthy();
    expect(d.getByRole("button", { name: "Usage by user →" })).toBeTruthy();
    expect(dialog().textContent).not.toMatch(/online/i);
  });

  it("marks the viewer in the signed-in group and says 1 person in the singular", () => {
    render(
      <PeopleChip
        model={model({
          signedIn: [{ key: "me", name: "Admin", initials: "AD", you: true, ago: "1h ago" }],
          week: 1,
        })}
        onShowUsageByUser={() => {}}
      />,
    );
    open();
    expect(within(dialog()).getByText("1h ago").parentElement?.textContent).toBe("ADAdmin (you)1h ago");
    expect(within(dialog()).getByText("This week: 1 person had runs with recorded usage")).toBeTruthy();
  });

  it("omits the signed-in and idle groups when the user list is unavailable, and the week when unknown", () => {
    render(
      <PeopleChip model={model({ total: null, signedIn: null, idle: null, week: null })} onShowUsageByUser={() => {}} />,
    );
    open();
    const d = within(dialog());
    expect(d.getByRole("heading", { name: "4 people active now" })).toBeTruthy();
    expect(d.queryByText(/Signed in within 24 hours/)).toBeNull();
    expect(d.queryByText("Idle")).toBeNull();
    expect(d.getByText("This week: unavailable")).toBeTruthy();
  });

  it("says nobody has listed work, and unavailable when the admin run list failed", () => {
    const { rerender } = render(
      <PeopleChip
        model={model({ active: { state: "ok", people: [], atLeast: false, count: 0 }, signedIn: [], idle: null })}
        onShowUsageByUser={() => {}}
      />,
    );
    open();
    expect(within(dialog()).getByText("Nobody has listed active work.")).toBeTruthy();
    expect(within(dialog()).getByRole("heading", { name: "0 of 9 people active now" })).toBeTruthy();
    rerender(<PeopleChip model={model({ active: { state: "unavailable" } })} onShowUsageByUser={() => {}} />);
    expect(within(dialog()).getByText("The admin run list could not be read.")).toBeTruthy();
    expect(within(dialog()).getByRole("heading", { name: "Active now unavailable" })).toBeTruthy();
  });

  it("states what the count covers in the header tooltip", () => {
    render(<PeopleChip model={model()} onShowUsageByUser={() => {}} />);
    open();
    const tip =
      "Counts people with listed active work: chat, judge and cross-check runs are not listed, and the list stops at 500 runs.";
    expect(within(dialog()).getByText(tip)).toBeTruthy();
    expect(dialog().querySelector(`[title="${tip}"]`)).not.toBeNull();
  });

  it("disables the usage action when the usage target is unavailable", () => {
    const onShow = vi.fn();
    render(<PeopleChip model={model({ usageAction: "unavailable" })} onShowUsageByUser={onShow} />);
    open();
    const btn = within(dialog()).getByRole("button", { name: "Usage by user unavailable" }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(btn);
    expect(onShow).not.toHaveBeenCalled();
  });
});

describe("PeopleChip keyboard and focus", () => {
  it("opens from the keyboard with focus inside the dialog, and Escape returns focus to the chip", () => {
    render(<PeopleChip model={model()} onShowUsageByUser={() => {}} />);
    chip().focus();
    // A native Enter on a focused button fires a click.
    fireEvent.click(chip());
    expect(chip().getAttribute("aria-expanded")).toBe("true");
    expect(chip().getAttribute("aria-controls")).toBe(dialog().id);
    expect(dialog().contains(document.activeElement)).toBe(true);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(chip().getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(chip());
  });

  it("closes on a mousedown outside, but not inside", () => {
    render(
      <div>
        <PeopleChip model={model()} onShowUsageByUser={() => {}} />
        <p>elsewhere</p>
      </div>,
    );
    open();
    fireEvent.mouseDown(dialog());
    expect(screen.queryByRole("dialog")).not.toBeNull();
    fireEvent.mouseDown(screen.getByText("elsewhere"));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(chip().getAttribute("aria-expanded")).toBe("false");
  });

  it("keeps focus on a control clicked outside, but returns it to the chip after a click on nothing", async () => {
    const nextFrame = () => new Promise<void>((r) => requestAnimationFrame(() => r()));
    render(
      <div>
        <PeopleChip model={model()} onShowUsageByUser={() => {}} />
        <input aria-label="other" />
        <p>elsewhere</p>
      </div>,
    );
    const input = screen.getByLabelText("other");
    open();
    fireEvent.mouseDown(input);
    input.focus();
    await nextFrame();
    await nextFrame();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(input);

    open();
    fireEvent.mouseDown(screen.getByText("elsewhere"));
    (document.activeElement as HTMLElement).blur();
    await nextFrame();
    await nextFrame();
    expect(document.activeElement).toBe(chip());
  });

  it("closes without taking focus back when Usage by user is activated, then calls the handler", () => {
    const onShow = vi.fn();
    const raf = vi.spyOn(window, "requestAnimationFrame");
    render(<PeopleChip model={model()} onShowUsageByUser={onShow} />);
    open();
    fireEvent.click(within(dialog()).getByRole("button", { name: "Usage by user →" }));
    expect(onShow).toHaveBeenCalledTimes(1);
    expect(chip().getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).not.toBe(chip());
    expect(raf).not.toHaveBeenCalled();
    raf.mockRestore();
  });
});
