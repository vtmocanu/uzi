// @vitest-environment jsdom
//
// ForgeIssueAnchor (NB3): the shared low-level guarded forge-issue anchor. When the
// web URL is a valid https URL it renders an external anchor (target=_blank,
// rel=noreferrer, aria-label/title=label, an ExternalLinkIcon, and `#<iid>` text);
// when it is not https (http, or null) it renders a plain `#<iid>` span with no
// anchor at all. The anchor path applies `className`; the span path applies
// `fallbackClassName`.
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { ForgeIssueAnchor } from "./ForgeIssueAnchor";

afterEach(cleanup);

const URL42 = "https://gitlab.example.com/vtmocanu/uzi/-/issues/42";
const LABEL = "Open issue #42 on the forge";

describe("ForgeIssueAnchor — https url (link branch)", () => {
  it("positive: renders an external anchor with href/target/rel, aria-label === label, and #<iid> visible", () => {
    render(<ForgeIssueAnchor webUrl={URL42} iid={42} label={LABEL} />);
    const link = screen.getByRole("link");
    expect(link.getAttribute("href")).toBe(URL42);
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noreferrer");
    expect(link.getAttribute("aria-label")).toBe(LABEL);
    expect(link.textContent).toContain("#42");
  });
});

describe("ForgeIssueAnchor — non-https url (span fallback)", () => {
  it("http url: NO anchor element, #<iid> present as plain text", () => {
    const { container } = render(
      <ForgeIssueAnchor webUrl="http://insecure.example.com/x" iid={42} label={LABEL} />,
    );
    expect(container.querySelector("a")).toBeNull();
    expect(screen.getByText("#42")).toBeTruthy();
  });

  it("null url: NO anchor element, #<iid> present as plain text", () => {
    const { container } = render(<ForgeIssueAnchor webUrl={null} iid={42} label={LABEL} />);
    expect(container.querySelector("a")).toBeNull();
    expect(screen.getByText("#42")).toBeTruthy();
  });

  it("fallback span carries fallbackClassName, NOT the anchor className", () => {
    render(
      <ForgeIssueAnchor
        webUrl={null}
        iid={42}
        label={LABEL}
        className="anchor-only-cls"
        fallbackClassName="fallback-cls"
      />,
    );
    const span = screen.getByText("#42");
    expect(span.className).toBe("fallback-cls");
    expect(span.className).not.toContain("anchor-only-cls");
  });
});
