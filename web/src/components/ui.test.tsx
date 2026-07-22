// @vitest-environment jsdom
// PRD #118: the Select primitive must MERGE a caller-supplied className with its
// base field styling (like Input/Textarea), not clobber the base. This positive
// pin proves the two compose — base tokens AND caller classes both survive.

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Select } from "./ui";

afterEach(cleanup);

describe("Select", () => {
  it("merges a caller className with the base field styling", () => {
    render(
      <Select className="h-8 custom-x">
        <option value="">x</option>
      </Select>,
    );
    const select = screen.getByRole("combobox");
    // Base styling from INPUT_CLASS survives...
    expect(select.className).toContain("border-edge");
    expect(select.className).toContain("bg-raised");
    // ...and so do the caller-supplied classes.
    expect(select.className).toContain("h-8");
    expect(select.className).toContain("custom-x");
  });
});
