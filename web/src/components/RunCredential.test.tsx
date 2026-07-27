// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RunCredential } from "./RunCredential";

afterEach(cleanup);

describe("RunCredential (PRD #111 M1)", () => {
  it("names the credential the run spent", () => {
    render(
      <RunCredential run={{ anthropic_secret_id: "sec-1", anthropic_secret_label: "console-key" }} />,
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
      <RunCredential run={{ anthropic_secret_id: null, anthropic_secret_label: "retired-key" }} />,
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
      <RunCredential run={{ anthropic_secret_id: null, anthropic_secret_label: null }} />,
    );
    expect(container.textContent).toBe("");
  });

  // An empty label is not a state the store can produce (user_secrets.label is NOT
  // NULL with a 1..64 CHECK), which is exactly why it is worth pinning: the guard
  // must be falsy-based, not a `!== null` that would render a blank chip.
  it("renders nothing for an empty label", () => {
    const { container } = render(
      <RunCredential run={{ anthropic_secret_id: "sec-1", anthropic_secret_label: "" }} />,
    );
    expect(container.textContent).toBe("");
  });

  // The label is user-authored. React escapes it, so this asserts the text arrives
  // verbatim as TEXT — no element is created from it.
  it("renders a markup-shaped label as text, not markup", () => {
    const { container } = render(
      <RunCredential
        run={{ anthropic_secret_id: "sec-1", anthropic_secret_label: "<img src=x onerror=1>" }}
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
        run={{ anthropic_secret_id: "sec-1", anthropic_secret_label: "safe‮drowssap" }}
      />,
    );
    expect(container.textContent).not.toContain("‮");
    expect(container.textContent).toContain("safe");
  });
});
