// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RunCreatedIssues } from "./RunCreatedIssues";

afterEach(cleanup);

it.each([undefined, []])("renders no block for %s", issues => {
  const { container } = render(<RunCreatedIssues issues={issues} />);
  expect(container.childElementCount).toBe(0);
});

it("renders multiple exact labels with safe external links and separate plain titles", () => {
  const { container } = render(<RunCreatedIssues issues={[
    { iid: 41, title: "First issue", web_url: "https://github.com/org/repo/issues/41" },
    { iid: 42, title: "", web_url: "https://forge.example/group/repo/-/issues/42" },
  ]} />);
  const links = screen.getAllByRole("link");
  expect(links.map(link => link.textContent)).toEqual(["Created issue #41", "Created issue #42"]);
  expect(links.map(link => link.getAttribute("href"))).toEqual([
    "https://github.com/org/repo/issues/41", "https://forge.example/group/repo/-/issues/42",
  ]);
  for (const link of links) {
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
  }
  expect(screen.getByText("First issue").tagName).toBe("SPAN");
  expect(container.querySelectorAll("a")).toHaveLength(2);
});

it.each([
  "https://forge.example/prefix/a%20b/r%C3%A9po/issues/1",
  "https://forge.example/group/r%2Egit/-/issues/1",
  "https://forge.example/group/r%3Fname%23part/issues/1",
])("preserves valid repository escaping in %j", web_url => {
  render(<RunCreatedIssues issues={[{ iid: 1, title: "", web_url }]} />);
  expect(screen.getByRole("link", { name: "Created issue #1" }).getAttribute("href")).toBe(web_url);
});

it.each([
  "https://FORGE.EXAMPLE/group/repo/issues/1",
  "https://forge.example:443/group/repo/issues/1",
  "https://FORGE.EXAMPLE:443/group/repo/issues/1",
  "https://[2001:0DB8:0000:0000:0000:0000:0000:0001]/group/repo/issues/1",
  "https://[2001:0DB8:0000:0000:0000:0000:0000:0001]:443/group/repo/issues/1",
])("preserves valid authority serialization in %j", web_url => {
  render(<RunCreatedIssues issues={[{ iid: 1, title: "", web_url }]} />);
  expect(screen.getByRole("link", { name: "Created issue #1" }).getAttribute("href")).toBe(web_url);
});

it.each([
  "", " ", "http://forge.example/issues/1", "javascript:alert(1)", "//forge.example/issues/1",
  "https://user:password@forge.example/issues/1", "https://@forge.example/issues/1",
  " https://forge.example/issues/1", "https://forge.example/issues/1 ",
  "https://forge.example/iss\nues/1", "https://forge.example/issues/1\u0000",
  "https://forge.example/\\issues/1", "https://forge.example/issues/1?next=evil",
  "https://forge.example/issues/1#", "https://forge.example/issues/1#fragment",
  "https://forge.example/a/../issues/1", "https://forge.example//issues/1",
  "https://forge.example/%2e%2e/issues/1", "https://forge.example/issues%2f1",
  "https://forge.example/issues%5c1", "https://forge.example/issues/%00",
  "https://forge.example/issues/%0a1", "https://forge.example/issues/%C2%85",
  "https://forge.example/issues/%E2%80%AE", "https://forge.example/issues/%252f1",
  "https://forge.example/issues/%", "https:///forge.example/issues/1",
  "https://forge.example", "https://forge.example:443", "https://forge.example/",
  "https:////forge.example/issues/1", "https://forge.example/issues/1/",
  "https://FORGE.EXAMPLE:443/a/%2E/issues/1",
  "https://[2001:DB8::1]:443/a/.%2e/issues/1",
  "https://127.1/issues/1", "https://2130706433/issues/1",
  "https://0177.0.0.1/issues/1", "https://0x7f000001/issues/1",
  "https://%66orge.example/issues/1", "https://forge%2eexample/issues/1",
  "https://ｆorge.example/issues/1", "https://forgé.example/issues/1",
  "https://forge.example:/issues/1", "https://forge.example:0443/issues/1",
])("uses a plain coordinate for unsafe URL %j", web_url => {
  render(<RunCreatedIssues issues={[{ iid: 7, title: "Still visible", web_url }]} />);
  expect(screen.getByText("Created issue #7").tagName).toBe("SPAN");
  expect(screen.queryByRole("link")).toBeNull();
  expect(screen.getByText("Still visible")).toBeTruthy();
});

it("strips terminal sequences, controls, bidi, format and line separators and escapes HTML", () => {
  const { container } = render(<RunCreatedIssues issues={[{
    iid: 8,
    title: "A\u001b[31mred\u001b[0m\u001b]0;hidden\u0007\u202e\u2066\u200b\n\t\u0000\u0085\u2028\u2029<img src=x>B",
    web_url: "",
  }]} />);
  expect(screen.getByText("Ared<img src=x>B").textContent).toBe("Ared<img src=x>B");
  expect(container.querySelector("img")).toBeNull();
  expect(container.textContent).toBe("Created issue #8Ared<img src=x>B");
});
