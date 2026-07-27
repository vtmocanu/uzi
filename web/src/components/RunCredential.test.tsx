// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RunCredential } from "./RunCredential";

afterEach(cleanup);

describe("RunCredential (PRD #111 M1)", () => {
  it("names the credential the run spent", () => {
    render(<RunCredential run={{ anthropic_secret_label: "console-key" }} />);
    expect(screen.getByText(/console-key/)).toBeTruthy();
  });

  // The case the whole component exists for. The id is a live FK the server nulls
  // when the token is deleted; the label is a claim-time snapshot that survives it.
  // A component gated on the id would blank exactly the historical runs whose
  // attribution is least recoverable from anywhere else.
  it("still names the account after the token is deleted (id null, label kept)", () => {
    render(<RunCredential run={{ anthropic_secret_label: "retired-key" }} />);
    expect(screen.getByText(/retired-key/)).toBeTruthy();
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
