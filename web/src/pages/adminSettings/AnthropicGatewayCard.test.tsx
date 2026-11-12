// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { AnthropicGatewayCard } from "./AnthropicGatewayCard";
import { api, type AppSettings, type SettingsResponse } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);
const settings = (s: Partial<AppSettings>): AppSettings => s as AppSettings;
function Harness({ initial }: { initial: Partial<AppSettings> }) {
  const [saved, setSaved] = useState(settings(initial));
  return <AnthropicGatewayCard settings={saved} onSaved={(resp) => setSaved(resp.settings)} />;
}
const url = () => screen.getByLabelText("Gateway base URL") as HTMLInputElement;
const model = (a: string) => screen.getByLabelText(`Gateway model for ${a}`) as HTMLInputElement;
const button = () => screen.getByRole("button", { name: "Save Claude gateway" }) as HTMLButtonElement;
afterEach(() => { cleanup(); vi.resetAllMocks(); });

describe("AnthropicGatewayCard", () => {
  it("shows the stored gateway and mapping", () => {
    render(<Harness initial={{ anthropic_gateway_base_url: "https://gw.example.com", anthropic_gateway_auth_header: "x-api-key", anthropic_gateway_models: "opus=gw-opus,haiku=gw-haiku" }} />);
    expect(url().value).toBe("https://gw.example.com");
    expect((screen.getByLabelText("Send the key as") as HTMLSelectElement).value).toBe("x-api-key");
    expect(model("opus").value).toBe("gw-opus");
    expect(model("sonnet").value).toBe("");
    expect(button().disabled).toBe(true);
  });

  it.each(["http://gw.example.com", "https://u:p@gw.example.com", "not a url"])("refuses %j without calling the API", (value) => {
    render(<Harness initial={{}} />);
    fireEvent.change(url(), { target: { value } });
    expect(button().disabled).toBe(true);
    fireEvent.submit(button().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("saves the three keys, the map encoded alias=model", async () => {
    const saved: Partial<AppSettings> = { anthropic_gateway_base_url: "https://gw.example.com", anthropic_gateway_auth_header: "authorization", anthropic_gateway_models: "opus=gw-opus,sonnet=gw-sonnet" };
    mockApi.updateSettings.mockResolvedValue({ settings: settings(saved), secrets: {}, sources: {}, slack_status: "disabled" } as SettingsResponse);
    render(<Harness initial={{}} />);
    fireEvent.change(url(), { target: { value: "https://gw.example.com" } });
    fireEvent.change(model("sonnet"), { target: { value: "gw-sonnet" } });
    fireEvent.change(model("opus"), { target: { value: " gw-opus " } });
    fireEvent.click(button());
    expect(await screen.findByText("Claude gateway settings saved.")).toBeTruthy();
    expect(mockApi.updateSettings).toHaveBeenCalledWith({
      anthropic_gateway_base_url: "https://gw.example.com",
      anthropic_gateway_auth_header: "authorization",
      anthropic_gateway_models: "opus=gw-opus,sonnet=gw-sonnet",
    });
    expect(button().disabled).toBe(true);
  });
});
