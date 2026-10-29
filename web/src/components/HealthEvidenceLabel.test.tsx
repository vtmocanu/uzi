// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { HealthEvidenceLabel, HealthEvidenceValue } from "./HealthEvidenceLabel";

afterEach(cleanup);

it("does not put malformed owner metadata into a title attribute", () => {
  render(<HealthEvidenceValue evidence={{ label: "Waiting run", value: "Owner name unavailable", owner_id: "owner\\u202e\\n" }} />);
  expect(screen.getByText("Owner name unavailable")).toBeTruthy();
  expect(document.querySelector("[title]")).toBeNull();
});

it("keeps legacy labels and rejects malformed run IDs before rendering attributes", () => {
  const { rerender } = render(
    <MemoryRouter><HealthEvidenceLabel evidence={{ label: "Waiting run", value: "Legacy diagnostics" }} /></MemoryRouter>,
  );
  expect(screen.getByText("Waiting run")).toBeTruthy();
  expect(screen.queryByRole("link")).toBeNull();
  rerender(
    <MemoryRouter><HealthEvidenceLabel evidence={{ label: "Waiting run", value: "Diagnostics", run_id: "../admin/" + String.fromCodePoint(0x202e) + "\n" }} /></MemoryRouter>,
  );
  expect(screen.getByText("Waiting run")).toBeTruthy();
  expect(screen.queryByRole("link")).toBeNull();
  expect(document.querySelector("[title]")).toBeNull();
});
