import { describe, it, expect } from "vitest";
import { recommendationLabel, verdictLabel, verdictTone } from "./judge";

describe("judge display helpers (PRD #46 M4)", () => {
  it("maps each verdict to a tone and label", () => {
    expect(verdictTone("ideal")).toBe("ok");
    expect(verdictTone("ok")).toBe("info");
    expect(verdictTone("issues")).toBe("warning");
    expect(verdictLabel("ideal")).toBe("Ideal");
    expect(verdictLabel("ok")).toBe("OK");
    expect(verdictLabel("issues")).toBe("Issues found");
  });

  it("maps each recommendation category to user copy", () => {
    expect(recommendationLabel("install_worker_tool")).toBe("Install a worker tool");
    expect(recommendationLabel("improve_uzi")).toBe("Improve uzi");
    expect(recommendationLabel("add_agent")).toBe("Add a missing agent");
  });

  it("humanizes an unknown category rather than showing a raw enum", () => {
    expect(recommendationLabel("some_future_category")).toBe("some future category");
  });
});
