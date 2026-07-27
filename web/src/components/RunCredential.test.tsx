// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RunCredential } from "./RunCredential";

afterEach(cleanup);

describe("RunCredential (PRD #111 M1)", () => {
  // One test, not two. It used to be two, the second named "still names the account
  // after the token is deleted (id null, label kept)" — a claim this fixture cannot
  // make and never made: neither case supplies an id, so the two were the same input
  // under different names and no mutation separated them.
  //
  // What actually forbids id-gating is the PROP TYPE. The component takes
  // `Pick<Run, "anthropic_secret_label">`, so the id is not in scope and a
  // `run.anthropic_secret_id && …` guard would not compile. That is a stronger
  // guarantee than a runtime assertion, and it is enforced by `npm run typecheck`,
  // not here. Widening the prop to test the deleted-token case at runtime would
  // REMOVE that guarantee in order to assert it, which is a bad trade.
  //
  // The reason it matters, kept because it is the component's whole purpose: the id
  // is a live FK the server nulls when the token is deleted, while the label is a
  // claim-time snapshot that survives. A component gated on the id would blank
  // exactly the historical runs whose attribution is least recoverable elsewhere.
  it("names the credential the run spent", () => {
    render(<RunCredential run={{ anthropic_secret_label: "console-key" }} />);
    expect(screen.getByText(/console-key/)).toBeTruthy();
  });

  // Two facts, one assertion each, because they arrive by different routes: a run
  // claimed before this shipped, and a run not yet claimed. Both must render NOTHING
  // rather than an empty or placeholder chip — a run that cannot say which account
  // it billed must not appear to have said something.
  it("renders nothing when no credential was recorded", () => {
    const { container } = render(<RunCredential run={{ anthropic_secret_label: null }} />);
    expect(container.textContent).toBe("");
  });

  // An empty label is not a state the store can produce (user_secrets.label is NOT
  // NULL with a 1..64 CHECK), which is exactly why it is worth pinning: the guard
  // must be falsy-based, not a `!== null` that would render a blank chip.
  it("renders nothing for an empty label", () => {
    const { container } = render(<RunCredential run={{ anthropic_secret_label: "" }} />);
    expect(container.textContent).toBe("");
  });

  // The label is user-authored. React escapes it, so this asserts the text arrives
  // verbatim as TEXT — no element is created from it.
  it("renders a markup-shaped label as text, not markup", () => {
    const { container } = render(
      <RunCredential run={{ anthropic_secret_label: "<img src=x onerror=1>" }} />,
    );
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByText(/<img src=x onerror=1>/)).toBeTruthy();
  });
});
