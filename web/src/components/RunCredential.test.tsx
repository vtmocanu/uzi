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

// chipText is the VISIBLE text of the chip: everything inside the badge minus the
// sr-only description. Reading container.textContent directly would fold the hint in
// and make /auto/ match on a run whose hint merely mentions auto-selection — a
// fixture where a correct and a broken renderer agree, which is the trap this file
// keeps being about.
function chipText(container: HTMLElement): string {
  const badge = container.querySelector("span");
  if (!badge) return "";
  const sr = badge.querySelector(".sr-only");
  const full = badge.textContent ?? "";
  return sr ? full.replace(sr.textContent ?? "", "") : full;
}

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
    const { container } = render(<RunCredential run={cred({ reason: reason as string, headroom })} />);
    expect(chipText(container)).toMatch(want);
  });

  // The three fallbacks must not read as an ordinary default. The worker is set to
  // auto and the run did not get it, which is a different situation with a different
  // fix from a worker that was never auto in the first place.
  it.each(["pool_empty", "pool_stale", "open_failed"])(
    "%s says the worker is on auto and why it did not get a pick",
    (reason) => {
      const { container } = render(<RunCredential run={cred({ reason })} />);
      expect(chipText(container)).toMatch(/default \(auto:/);
    },
  );

  // A run claimed before M1 recorded no mode. The bare label is the truthful
  // rendering; a guessed "default" would assert something nothing knows.
  it("shows the bare label for a run with no recorded reason", () => {
    const { container } = render(<RunCredential run={cred({ reason: null })} />);
    expect(chipText(container)).not.toMatch(/—/);
  });

  // The API ships separately from this bundle, so a newer server can send a ninth
  // reason. Rendering it verbatim is honest; dropping it or guessing is not.
  it("passes an unrecognised reason through rather than dropping it", () => {
    const { container } = render(<RunCredential run={cred({ reason: "some_future_reason" })} />);
    expect(chipText(container)).toMatch(/some_future_reason/);
  });

  // The headroom rides only where the server measured one. D14's retry records
  // open_failed with a NULL headroom precisely because the reading described the
  // credential that would NOT open — attaching it to the one that did would
  // attribute a measurement to a token nothing measured.
  it("omits the headroom when the server recorded none", () => {
    const { container } = render(<RunCredential run={cred({ reason: "open_failed", headroom: null })} />);
    expect(chipText(container)).not.toMatch(/headroom/);
  });

  // A deleted credential AND a mode, together: the two are independent fields and a
  // renderer that handled either alone would pass every test above.
  it("shows both the deleted marker and the mode", () => {
    const { container } = render(
      <RunCredential run={cred({ id: null, label: "retired-key", reason: "pinned" })} />,
    );
    const text = chipText(container);
    expect(text).toMatch(/retired-key/);
    expect(text).toMatch(/\(deleted\)/);
    expect(text).toMatch(/pinned/);
  });
});

// --- the link to the page that fixes it ----------------------------------------

describe("RunCredential links only where there is something to do", () => {
  // PRD #104 M5 already ships the per-token meters and eligibility chips on
  // Settings → Anthropic tokens, so a fallback POINTS AT THEM rather than rebuilding
  // a meter in the run header.
  it.each(["pool_empty", "pool_stale", "open_failed", "best_of_pool"])("links on %s", (reason) => {
    const { container } = render(<RunCredential run={cred({ reason })} />);
    const link = container.querySelector("a");
    expect(link).not.toBeNull();
    // The user's OWN settings page. /admin/rate-limits is admin-only and would 403
    // for exactly the person reading their own run.
    expect(link?.getAttribute("href")).toBe("/settings");
  });

  // A link where nothing is wrong is a dead end dressed as an action.
  it.each(["auto", "default", "pinned", "judge"])("does not link on %s", (reason) => {
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


// --- PRD #295: the compact variant for the Runs list ---------------------------

// The dot is the ONLY aria-hidden element the Badge renders (ui.tsx), and it is
// present iff the tone is non-neutral — the same signal the full chip carries with a
// link. Reading it directly is how "neutral shows no dot" is told apart from
// "non-neutral shows one", which the text cannot see.
function hasDot(container: HTMLElement): boolean {
  return container.querySelector('span[aria-hidden="true"]') !== null;
}

describe("RunCredential compact variant (PRD #295)", () => {
  // auto is a calm pick: neutral tone → no dot, and no link because there is nothing
  // to act on, exactly as the full chip's "link iff non-neutral" rule.
  it("auto renders neutral: no dot and not a link", () => {
    const { container } = render(
      <RunCredential run={cred({ reason: "auto", headroom: 62 })} variant="compact" />,
    );
    expect(screen.getByText(/console-key/)).toBeTruthy();
    expect(hasDot(container)).toBe(false);
    expect(container.querySelector("a")).toBeNull();
  });

  // best_of_pool is info: the pool is nearly exhausted, worth a look — the tone dot
  // carries that signal. The compact badge lives inside the Runs-list row <Link>, so it
  // must NOT introduce a nested anchor: dot yes, link never (the blocking-bug guard).
  it("best_of_pool renders info: a dot, and is NOT a link", () => {
    const { container } = render(
      <RunCredential run={cred({ reason: "best_of_pool", headroom: 8 })} variant="compact" />,
    );
    expect(hasDot(container)).toBe(true);
    expect(container.querySelector("a")).toBeNull();
  });

  // pool_empty is warning: the worker is on auto and the pool is empty — a dot signals
  // it, but again no inner link (it would nest inside the row <Link>).
  it("pool_empty renders warning: a dot, and is NOT a link", () => {
    const { container } = render(
      <RunCredential run={cred({ reason: "pool_empty" })} variant="compact" />,
    );
    expect(hasDot(container)).toBe(true);
    expect(container.querySelector("a")).toBeNull();
  });

  // A deleted credential (id null, label kept) still names the account and marks it
  // gone, same as the full chip.
  it("marks a deleted credential", () => {
    render(
      <RunCredential run={cred({ id: null, label: "retired-key", reason: "pinned" })} variant="compact" />,
    );
    expect(screen.getByText(/retired-key/)).toBeTruthy();
    expect(screen.getByText(/\(deleted\)/)).toBeTruthy();
  });

  // The compact badge is NEVER a link, on any tone — it is embedded inside the row
  // <Link> and a nested <a> is illegal HTML. This is the direct guard for the blocking
  // nested-anchor bug the earlier compact path introduced. The full run-detail chip,
  // which is not inside a row link, keeps its /settings link (asserted separately).
  it.each(["best_of_pool", "pool_empty", "pool_stale", "open_failed"])(
    "compact never renders a link, even on the non-neutral %s state",
    (reason) => {
      const { container } = render(
        <RunCredential run={cred({ reason })} variant="compact" />,
      );
      expect(screen.getByText(/console-key/)).toBeTruthy();
      // The dot still signals the state; there is simply no anchor.
      expect(hasDot(container)).toBe(true);
      expect(container.querySelector("a")).toBeNull();
    },
  );

  // No label: a run claimed before PRD #111 M1, or not yet claimed, renders nothing —
  // no placeholder badge on the row.
  it("renders nothing when there is no label", () => {
    const { container } = render(
      <RunCredential run={cred({ id: null, label: null, reason: "auto" })} variant="compact" />,
    );
    expect(container.textContent).toBe("");
  });
});

// --- the chip's STRUCTURE, which the text assertions above cannot see -------------

describe("RunCredential chip structure (web-ux F19/F20/F21)", () => {
  // F19. `token default — default` was D20's own motivating input and it rendered as
  // the same word twice with nothing marking which was which — a token NAME then a
  // MODE, reading as a stutter or a bug. The roles are told apart typographically, so
  // the assertion is that the label sits in its own element carrying weight, not that
  // the text happens to differ.
  it("sets the label apart from the mode by punctuation, not only by weight", () => {
    const { container } = render(<RunCredential run={cred({ label: "default", reason: "default" })} />);
    const weighted = container.querySelector("span.font-semibold");
    expect(weighted, "the label needs its own element").not.toBeNull();
    expect(weighted?.textContent).toBe("default");
    expect(weighted?.textContent).not.toMatch(/—/);
    // 🔴 THE QUOTES ARE THE ASSERTION. A weight step alone was MEASURED as
    // imperceptible at 11px in muted grey — presence in the markup is not efficacy on
    // screen, and this test existed while the chip still read `token default — default`
    // indistinguishably. Quotes survive any size and any contrast.
    expect(chipText(container)).toMatch(/token “default” — default/);
  });

  // F20. Measured at 375px: `token nearly-spent — default (auto: the chosen token
  // would not open)` is 389px wide and overflows the document. The em dash is not the
  // cause; a pill carrying a sentence-length reason and refusing to wrap is.
  it("lets the chip wrap rather than overflowing the viewport", () => {
    const { container } = render(<RunCredential run={cred({ reason: "open_failed" })} />);
    const badge = container.querySelector("span");
    expect(badge?.className).not.toMatch(/whitespace-nowrap/);
    expect(badge?.className).toMatch(/whitespace-normal/);
  });

  // F21. WCAG 2.5.3 Label in Name: an aria-label here REPLACES the visible text, so a
  // voice-control user saying "click token default" matches nothing. The explanation
  // is DESCRIBED instead, leaving the visible text as the accessible name.
  it("describes the chip without replacing its accessible name", () => {
    const { container } = render(<RunCredential run={cred({ reason: "pool_stale" })} />);
    const badge = container.querySelector("span[aria-describedby]");
    expect(badge, "the chip should carry aria-describedby").not.toBeNull();
    expect(badge?.getAttribute("aria-label"), "an aria-label would REPLACE the visible name").toBeNull();
    const described = container.querySelector(`#${badge?.getAttribute("aria-describedby")}`);
    expect(described?.textContent).toMatch(/no pooled token had a current usage reading/);
    // The visible label survives in the name, which is the whole property.
    expect(badge?.textContent).toMatch(/console-key/);
  });
});
