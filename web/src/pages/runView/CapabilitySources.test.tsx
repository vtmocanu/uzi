// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { CapabilitySources } from "./CapabilitySources";
import { setDemoMode } from "../../lib/demoMode";
import type { Run } from "../../lib/api";

afterEach(() => { cleanup(); setDemoMode(false); });
function evidence(over: Partial<NonNullable<Run["capability_provenance"]>["capabilities"][number]> = {}): NonNullable<Run["capability_provenance"]> {
  return { hints_known:true,inference_known:true,capabilities:[{
    capability:"jvm",sources:["repo_hint","inferred"],required:true,inference_ignored:true,
    triggers:[{path:"private/customer/android/build.gradle",reason:"jvm_manifest"}],truncated:false,...over,
  }] };
}
describe("capability provenance",()=>{
  it("keeps missing legacy history explicit",()=>{
    const p=evidence();p.hints_known=false;p.inference_known=false;
    render(<CapabilitySources provenance={p} />);
    expect(screen.getByText("Repo-hint history unavailable.")).toBeTruthy();
    expect(screen.getByText("Inference evidence unavailable from this worker.")).toBeTruthy();
  });
  it("shows overlapping sources, ignored inference and a still-required hint",()=>{
    render(<CapabilitySources provenance={evidence()} />);
    expect(screen.getByText(/repo hint \+ inferred; inference ignored; required/)).toBeTruthy();
    expect(screen.getByText("private/customer/android/build.gradle")).toBeTruthy();
    expect(screen.getByText(/JVM build manifest/)).toBeTruthy();
  });
  it("shows unknown evidence and current overrides without claiming a requirement",()=>{
    render(<CapabilitySources provenance={evidence({sources:["unknown"],required:false,triggers:[]})} />);
    expect(screen.getByText(/source unknown; inference ignored; not currently required/)).toBeTruthy();
  });
  it("hides paths completely in demo mode while preserving source flags",()=>{
    setDemoMode(true);
    const {container}=render(<CapabilitySources provenance={evidence()} />);
    expect(screen.getByText(/repo hint \+ inferred/)).toBeTruthy();
    expect(container.innerHTML).not.toContain("private/customer");
    expect(container.innerHTML).not.toContain("build.gradle");
  });
  it("sanitizes untrusted path text and signals bounded samples",()=>{
    const {container}=render(<CapabilitySources provenance={evidence({triggers:[{path:"src/\u202eDocker\u001bfile",reason:"docker_marker"}],truncated:true})} />);
    expect(screen.getByText("src/Dockerfile")).toBeTruthy();
    expect(screen.getByText("Additional or invalid trigger paths omitted.")).toBeTruthy();
    expect(container.innerHTML).not.toMatch(/[\p{Cc}\p{Cf}]/u);
  });
});
