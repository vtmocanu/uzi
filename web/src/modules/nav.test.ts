// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { NAV_SECTIONS, visibleNavItems } from "../components/navItems";
import type { ModuleEntry } from "./manifest";
import { mergeModuleNav } from "./nav";

const mod = (over: Partial<ModuleEntry> & { id: string }): ModuleEntry => ({
  version: "1",
  page: "Page",
  label: over.id.toUpperCase(),
  icon: "package",
  navGroup: "modules",
  adminOnly: false,
  ...over,
});

describe("mergeModuleNav", () => {
  it("returns the static sections untouched when there are no modules", () => {
    expect(mergeModuleNav(NAV_SECTIONS, [])).toBe(NAV_SECTIONS);
  });

  it("appends work and factory entries, in manifest order, to those sections", () => {
    const merged = mergeModuleNav(NAV_SECTIONS, [
      mod({ id: "w1", navGroup: "work" }),
      mod({ id: "f1", navGroup: "factory" }),
      mod({ id: "w2", navGroup: "work" }),
    ]);
    const keys = (id: string) => merged.find((s) => s.id === id)!.items.map((i) => i.key);
    expect(keys("work").slice(-2)).toEqual(["module:w1", "module:w2"]);
    expect(keys("work").slice(0, -2)).toEqual(NAV_SECTIONS.find((s) => s.id === "work")!.items.map((i) => i.key));
    expect(keys("factory").slice(-1)).toEqual(["module:f1"]);
    expect(merged.some((s) => s.id === "modules")).toBe(false);
  });

  it("puts the Modules section after Factory and before the unlabelled bottom cluster", () => {
    const merged = mergeModuleNav(NAV_SECTIONS, [mod({ id: "m1" }), mod({ id: "m2" })]);
    expect(merged.map((s) => s.id)).toEqual(["leading", "work", "factory", "modules", "system"]);
    const section = merged.find((s) => s.id === "modules")!;
    expect(section.group).toBe("Modules");
    expect(section.items.map((i) => i.key)).toEqual(["module:m1", "module:m2"]);
  });

  it("targets /m/<id> and propagates adminOnly", () => {
    const merged = mergeModuleNav(NAV_SECTIONS, [mod({ id: "a", adminOnly: true }), mod({ id: "b" })]);
    const [a, b] = merged.find((s) => s.id === "modules")!.items;
    expect(a).toMatchObject({ to: "/m/a", adminOnly: true, label: "A" });
    expect(b.to).toBe("/m/b");
    expect(b.adminOnly).toBeUndefined();
  });

  it("does not mutate the static sections", () => {
    const before = JSON.stringify(NAV_SECTIONS.map((s) => [s.id, s.items.map((i) => i.key)]));
    mergeModuleNav(NAV_SECTIONS, [mod({ id: "w1", navGroup: "work" }), mod({ id: "m1" })]);
    expect(JSON.stringify(NAV_SECTIONS.map((s) => [s.id, s.items.map((i) => i.key)]))).toBe(before);
  });

  it("feeds visibleNavItems, hiding adminOnly entries from non-admins", () => {
    const merged = mergeModuleNav(NAV_SECTIONS, [
      mod({ id: "pub", navGroup: "work" }),
      mod({ id: "adm", adminOnly: true }),
    ]);
    const asUser = visibleNavItems(false, merged).map((i) => i.key);
    const asAdmin = visibleNavItems(true, merged);
    expect(asUser).toContain("module:pub");
    expect(asUser).not.toContain("module:adm");
    expect(asAdmin.find((i) => i.key === "module:adm")).toMatchObject({ group: "Modules", hint: "Modules" });
    expect(asAdmin.find((i) => i.key === "module:pub")).toMatchObject({ group: "Work", hint: "Work" });
  });
});
