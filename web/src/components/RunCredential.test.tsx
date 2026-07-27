// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render as rtlRender, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunCredential } from "./RunCredential";

// A router wrapper, because the chip becomes a <Link> in its FALLBACK states — the
// one place the user has something to do about what they are reading. Only RunView
// renders this component and RunView is inside the router, so the dependency costs
// nothing in production; it costs this wrapper in tests.
function render(ui: Parameters<typeof rtlRender>[0]) {
  return rtlRender(<MemoryRouter>{ui}</MemoryRouter>);
}

afterEach(cleanup);

// cred builds the four credential fields. M5 added two, and every fixture below goes
// through here so a seventh field lands in one place rather than in nine literals.
function cred(over: {
  id?: string | null;
  label?: string | null;
  reason?: string | null;
  headroom?: number | null;
}) {
  return {
    anthropic_secret_id: over.id === undefined ? "sec-1" : over.id,
    anthropic_secret_label: over.label === undefined ? "console-key" : over.label,
    anthropic_select_reason: over.reason ?? null,
    anthropic_headroom_pct: over.headroom ?? null,
  };
}

describe("RunCredential (PRD #111 M1)", () => {
  it("names the credential the run spent", () => {
    render(
      <RunCredential run={cred({})} />,
    );
    expect(screen.getByText(/console-key/)).toBeTruthy();
    expect(screen.queryByText(/deleted/)).toBeNull();
  });

  // 🔴 THIS TEST'S JUSTIFICATION REVERSED INSIDE ONE PRD, AND THE REVERSAL IS WORTH
  // READING RATHER THAN THE CONCLUSION ALONE.
  //
  // It began as "still names the account after the token is deleted (id null, label
  // kept)" against a component whose prop type was `Pick<Run,
  // "anthropic_secret_label">` — so no id could reach it, no fixture could vary one,
  // and the two cases were the same input under two names. A review caught that, and
  // it was collapsed into one test with a comment arguing that the PROP TYPE was the
  // real guarantee against id-gating, and that widening it to assert the case at
  // runtime would REMOVE the guarantee in order to test it.
  //
  // That argument was correct for the component as it then was, and is now obsolete:
  // web-ux measured that a run whose credential was DELETED renders identically to
  // one whose credential exists (F8) — a thing a user needs told, and one that cannot
  // be said without reading the id. So the prop widened deliberately and the
  // component branches on it: the exact shape the previous comment called a bad
  // trade, made worth it by a requirement that did not exist when it was written.
  //
  // The distinction is genuine now — the two cases differ in the id, the component
  // reads it, and an implementation that ignored it fails this.
  it("marks the credential as deleted when the id is gone but the label survives", () => {
    render(
      <RunCredential run={cred({ id: null, label: "retired-key" })} />,
    );
    // The label is still named — that is what the snapshot column is FOR. The FK
    // nulls the id when the token is deleted, so a component gated on the id would
    // blank exactly the historical runs whose attribution is least recoverable.
    expect(screen.getByText(/retired-key/)).toBeTruthy();
    // …and it says the credential is gone, rather than pointing at a token the user
    // will not find in Settings.
    expect(screen.getByText(/\(deleted\)/)).toBeTruthy();
  });

  it("renders nothing when no credential was recorded", () => {
    const { container } = render(
      <RunCredential run={cred({ id: null, label: null })} />,
    );
    expect(container.textContent).toBe("");
  });

  // An empty label is not a state the store can produce (user_secrets.label is NOT
  // NULL with a 1..64 CHECK), which is exactly why it is worth pinning: the guard
  // must be falsy-based, not a `!== null` that would render a blank chip.
  it("renders nothing for an empty label", () => {
    const { container } = render(
      <RunCredential run={cred({ label: "" })} />,
    );
    expect(container.textContent).toBe("");
  });

  // The label is user-authored. React escapes it, so this asserts the text arrives
  // verbatim as TEXT — no element is created from it.
  it("renders a markup-shaped label as text, not markup", () => {
    const { container } = render(
      <RunCredential
        run={cred({ label: "<img src=x onerror=1>" })}
      />,
    );
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByText(/<img src=x onerror=1>/)).toBeTruthy();
  });

  // web-ux F12: escaping is NOT the hazard here. A bidi override survives escaping
  // intact and reorders the text around it, so a label can render as an account it is
  // not — measured in a browser against the mock build. The server now rejects these
  // on write, which is a statement about what the server ACCEPTS; this is a statement
  // about what the renderer does with what it is handed, and rows written before that
  // validator landed were never re-validated.
  it("strips invisible formatting characters from the label", () => {
    const { container } = render(
      <RunCredential
        run={cred({ label: "safe‮drowssap" })}
      />,
    );
    expect(container.textContent).not.toContain("‮");
    expect(container.textContent).toContain("safe");
  });
});

// --- PRD #111 M5: the chip names the MODE, not just the token -------------------

describe("RunCredential mode rendering (PRD #111 M5, D20)", () => {
  // 🔴 EVERY CASE HERE NAMES THE SAME TOKEN, and that is the fixture design, not an
  // accident. D20's whole argument is that the label cannot answer the user's
  // question because an auto pick and a default fallback can name the SAME
  // credential. A fixture using three different labels would pass against a component
  // that ignored the reason entirely, which is this repo's documented
  // broken-and-correct-agree trap.
  it.each([
    ["auto", 62, /auto, 62% headroom/],
    ["default", null, /default/],
    ["pinned", null, /pinned/],
    ["judge", null, /judge binding/],
    ["best_of_pool", 8, /auto \(best of pool\), 8% headroom/],
  ])("renders %s", (reason, headroom, want) => {
    render(<RunCredential run={cred({ reason: reason as string, headroom })} />);
    expect(screen.getByText(/console-key/).textContent).toMatch(want);
  });

  // The three fallbacks must not read as an ordinary default. The worker is set to
  // auto and the run did not get it, which is a different situation with a different
  // fix from a worker that was never auto in the first place.
  it.each(["pool_empty", "pool_stale", "open_failed"])(
    "%s says the worker is on auto and why it did not get a pick",
    (reason) => {
      render(<RunCredential run={cred({ reason })} />);
      expect(screen.getByText(/console-key/).textContent).toMatch(/default \(auto:/);
    },
  );

  // A run claimed before M1 recorded no mode. The bare label is the truthful
  // rendering; a guessed "default" would assert something nothing knows.
  it("shows the bare label for a run with no recorded reason", () => {
    render(<RunCredential run={cred({ reason: null })} />);
    const text = screen.getByText(/console-key/).textContent ?? "";
    expect(text).not.toMatch(/—/);
  });

  // The API ships separately from this bundle, so a newer server can send a ninth
  // reason. Rendering it verbatim is honest; dropping it or guessing is not.
  it("passes an unrecognised reason through rather than dropping it", () => {
    render(<RunCredential run={cred({ reason: "some_future_reason" })} />);
    expect(screen.getByText(/console-key/).textContent).toMatch(/some_future_reason/);
  });

  // The headroom rides only where the server measured one. D14's retry records
  // open_failed with a NULL headroom precisely because the reading described the
  // credential that would NOT open — attaching it to the one that did would
  // attribute a measurement to a token nothing measured.
  it("omits the headroom when the server recorded none", () => {
    render(<RunCredential run={cred({ reason: "open_failed", headroom: null })} />);
    expect(screen.getByText(/console-key/).textContent).not.toMatch(/headroom/);
  });

  // A deleted credential AND a mode, together: the two are independent fields and a
  // renderer that handled either alone would pass every test above.
  it("shows both the deleted marker and the mode", () => {
    render(<RunCredential run={cred({ id: null, label: "retired-key", reason: "pinned" })} />);
    const text = screen.getByText(/retired-key/).textContent ?? "";
    expect(text).toMatch(/\(deleted\)/);
    expect(text).toMatch(/pinned/);
  });
});

// --- the link to the page that fixes it ----------------------------------------

describe("RunCredential links only where there is something to do", () => {
  // PRD #104 M5 already ships the per-token meters and eligibility chips on
  // Settings → Anthropic tokens, so a fallback POINTS AT THEM rather than rebuilding
  // a meter in the run header.
  it.each(["pool_empty", "pool_stale", "open_failed"])("links on %s", (reason) => {
    const { container } = render(<RunCredential run={cred({ reason })} />);
    const link = container.querySelector("a");
    expect(link).not.toBeNull();
    // The user's OWN settings page. /admin/rate-limits is admin-only and would 403
    // for exactly the person reading their own run.
    expect(link?.getAttribute("href")).toBe("/settings");
  });

  // A link where nothing is wrong is a dead end dressed as an action.
  it.each(["auto", "default", "pinned", "judge", "best_of_pool"])("does not link on %s", (reason) => {
    const { container } = render(<RunCredential run={cred({ reason })} />);
    expect(container.querySelector("a")).toBeNull();
  });

  // Nothing left to look at: the credential is gone from Settings too.
  it("does not link a deleted credential", () => {
    const { container } = render(
      <RunCredential run={cred({ id: null, label: "retired-key", reason: "pinned" })} />,
    );
    expect(container.querySelector("a")).toBeNull();
  });
});
