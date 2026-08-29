// @vitest-environment jsdom
//
// ScheduleGroupRow disclosure-count default (issue #676): the expand toggle shows
// the "N repos" count text by default (showDisclosureCount omitted); the Default-jobs tab
// opts out via showDisclosureCount={false} (covered in DefaultJobs.test.tsx).
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { ScheduleGroupRow } from "./ScheduleGroupRow";

afterEach(cleanup);

function renderRow(showDisclosureCount?: boolean) {
  return render(
    <table>
      <tbody>
        <ScheduleGroupRow
          name="My schedule"
          repoCount={2}
          showDisclosureCount={showDisclosureCount}
          expanded={false}
          onToggleExpand={() => {}}
          disclosureId="disc-1"
          expandLabelName="My schedule"
          cols={6}
        />
      </tbody>
    </table>,
  );
}

describe("ScheduleGroupRow — disclosure count", () => {
  it("shows the 'N repos' count text in the toggle by default (showDisclosureCount omitted)", () => {
    renderRow();
    const toggle = screen.getByRole("button", { name: /Show repos for My schedule/ });
    expect(within(toggle).getByText(/2 repos/)).toBeTruthy();
  });
});
