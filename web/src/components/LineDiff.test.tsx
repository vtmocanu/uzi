// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { diffLines } from "diff";
import { LineDiff } from "./LineDiff";
afterEach(cleanup);
it.each(["drift", "revision"] as const)("renders inert rows with %s colors and accessible labels", (tone) => {
  const { container } = render(<LineDiff parts={diffLines("old\n", "<img src=x onerror=alert(1)>\t\u202E\u0007\n\n")}
    tone={tone} addedLabel="Inserted" removedLabel="Deleted" />);
  expect(container.querySelector("img")).toBeNull();
  expect(container.textContent).toContain("<img src=x onerror=alert(1)>");
  expect(container.textContent).toContain("\t");
  expect(container.textContent).toContain("U+202E");
  expect(container.textContent).toContain("U+0007");
  expect(container.textContent).not.toContain("\u202E");
  expect(container.textContent).not.toContain("\u0007");
  expect(container.textContent).toContain("\u00a0");
  expect(container.querySelectorAll("[data-hidden-char] .sr-only")).toHaveLength(2);
  const added = [...container.querySelectorAll("span.block")].find((s) => s.textContent?.includes("Inserted"))!;
  const removed = [...container.querySelectorAll("span.block")].find((s) => s.textContent?.includes("Deleted"))!;
  expect(added.className).toContain(tone === "revision" ? "text-ok" : "text-danger");
  expect(removed.className).toContain(tone === "revision" ? "text-danger" : "text-ok");
  expect(added.querySelector("[aria-hidden=true]")?.textContent).toBe("+ ");
});
it("retains three context lines on each side and counts omitted lines", () => {
  const before = Array.from({ length: 10 }, (_, i) => "before" + i).join("\n") + "\n";
  const after = Array.from({ length: 10 }, (_, i) => "after" + i).join("\n") + "\n";
  const { container } = render(<LineDiff parts={diffLines(before + "old\n" + after, before + "new\n" + after)}
    tone="revision" addedLabel="Inserted" removedLabel="Deleted" />);
  expect(container.textContent).toContain("7 unchanged lines");
  expect(container.textContent).toContain("before7");
  expect(container.textContent).toContain("before9");
  expect(container.textContent).toContain("after0");
  expect(container.textContent).toContain("after2");
  expect(container.textContent).not.toContain("before6");
  expect(container.textContent).not.toContain("after3");
});
it("renders zero rows for empty input", () => {
  const { container } = render(<LineDiff parts={diffLines("", "")} tone="revision" addedLabel="Inserted" removedLabel="Deleted" />);
  expect(container.childElementCount).toBe(0);
});
