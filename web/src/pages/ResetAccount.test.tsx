// @vitest-environment jsdom
// /reset (PRD #3989). These tests drive the REAL api client against a stubbed fetch,
// so the token's placement (POST body only, never a URL) is asserted on the wire shape,
// not on a mocked api method.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ResetAccount } from "./ResetAccount";
import { APP_ROUTES } from "../App";

// A token-shaped value that is not a provider token: 43 base64url chars.
const TOKEN = "rst" + "Q9x_".repeat(10);
const INVALID = "This link is invalid or has expired. Ask an administrator for a new one.";
const EXPIRES = "2026-11-06T17:00:00Z";
const CONFIG = "/api/auth/config";
const SSO_ONLY = { registration_enabled: false, allowed_email_domains: [], oidc_enabled: true, password_login_enabled: false };
const PASSWORD_ON = { registration_enabled: true, allowed_email_domains: [], password_login_enabled: true };

type Reply = { status: number; body?: unknown };
type Call = { url: string; method: string; body: unknown };

let calls: Call[];
// GETs of the public auth config, kept apart from `calls` so the token assertions stay
// about the reset POSTs. With no queued reply the config fetch fails (500).
let configCalls: number;
let replies: Record<string, Reply[]>;

// reply queues a response for the next POST to path ("/api/auth/reset/preview" etc.).
function reply(path: string, r: Reply) {
  (replies[path] ??= []).push(r);
}

beforeEach(() => {
  calls = [];
  configCalls = 0;
  replies = {};
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const body = typeof init?.body === "string" ? JSON.parse(init.body) : undefined;
      if (url === CONFIG) configCalls++;
      else calls.push({ url, method: init?.method ?? "GET", body });
      const r = replies[url]?.shift() ?? { status: 500, body: { error: `unexpected ${url}` } };
      const text = r.body === undefined ? "" : JSON.stringify(r.body);
      return { ok: r.status >= 200 && r.status < 300, status: r.status, text: async () => text } as Response;
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.replaceState(null, "", "/");
});

// openAt puts the link in the real address bar (the page reads window.location, which
// the router does not own) and mounts the page under a router with a /login target.
function openAt(url: string) {
  window.history.replaceState(null, "", url);
  return render(
    <MemoryRouter initialEntries={["/reset"]}>
      <Routes>
        <Route path="/reset" element={<ResetAccount />} />
        <Route path="/login" element={<div>LOGIN PAGE</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

function expectTokenNeverInUrl() {
  expect(calls.length).toBeGreaterThan(0);
  for (const c of calls) {
    expect(c.url).not.toContain(TOKEN);
    expect(c.method).toBe("POST");
  }
}

async function passwordForm(config?: Record<string, unknown>) {
  if (config) reply(CONFIG, { status: 200, body: config });
  reply("/api/auth/reset/preview", { status: 200, body: { kind: "password", expires_at: EXPIRES } });
  openAt(`/reset?x=1#${TOKEN}`);
  return screen.findByLabelText("New password");
}

function fill(password: string, confirm: string) {
  fireEvent.change(screen.getByLabelText("New password"), { target: { value: password } });
  fireEvent.change(screen.getByLabelText("Confirm new password"), { target: { value: confirm } });
}

const ack = () => screen.getByRole("checkbox", { name: "I understand my credentials will be deleted" });
const submit = () => screen.getByRole("button", { name: "Reset and set new password" });

describe("ResetAccount token handling", () => {
  it("clears the fragment at once and sends the token only in the preview POST body", async () => {
    await passwordForm();
    // The address bar keeps its path and query but loses the token.
    expect(window.location.hash).toBe("");
    expect(window.location.pathname + window.location.search).toBe("/reset?x=1");
    expect(window.location.href).not.toContain(TOKEN);
    expect(calls).toEqual([{ url: "/api/auth/reset/preview", method: "POST", body: { token: TOKEN } }]);
    expectTokenNeverInUrl();
  });

  it("redeems with the token and password in the body, never the URL", async () => {
    await passwordForm();
    reply("/api/auth/reset/redeem", { status: 204 });
    fill("correct horse battery", "correct horse battery");
    fireEvent.click(ack());
    fireEvent.click(submit());
    expect(await screen.findByText(/Your account is reset/)).toBeTruthy();
    expect(calls[1]).toEqual({
      url: "/api/auth/reset/redeem",
      method: "POST",
      body: { token: TOKEN, password: "correct horse battery" },
    });
    expectTokenNeverInUrl();
  });
});

describe("ResetAccount remount", () => {
  it("keeps the token when the shell remounts the page after the fragment is cleared", async () => {
    reply("/api/auth/reset/preview", { status: 200, body: { kind: "password", expires_at: EXPIRES } });
    reply("/api/auth/reset/preview", { status: 200, body: { kind: "password", expires_at: EXPIRES } });
    window.history.replaceState(null, "", `/reset#${TOKEN}`);
    // Two different wrapper element types: switching between them unmounts the first
    // page instance and mounts a fresh one, as AppShell does when the session resolves.
    const page = (signedIn: boolean) => (
      <MemoryRouter initialEntries={["/reset"]}>
        {signedIn ? <main><ResetAccount /></main> : <section><ResetAccount /></section>}
      </MemoryRouter>
    );
    const { rerender, unmount } = render(page(false));
    await screen.findByLabelText("New password");
    expect(window.location.hash).toBe("");
    rerender(page(true));
    expect(await screen.findByLabelText("New password")).toBeTruthy();
    expect(screen.queryByText(INVALID)).toBeNull();
    expect(calls.map((c) => c.body)).toEqual([{ token: TOKEN }, { token: TOKEN }]);
    // With no successor, an unmount drops the token: a later visit with no fragment is invalid.
    unmount();
    openAt("/reset");
    expect(await screen.findByText(INVALID)).toBeTruthy();
  });
});

describe("ResetAccount is a public route", () => {
  it("is registered at /reset with the public guard", () => {
    const route = APP_ROUTES.find((r) => r.path === "/reset");
    expect(route?.guard).toBe("public");
  });

  it("renders signed out: no auth provider and no session read", async () => {
    // No AuthProvider is mounted; the only request is the preview.
    await passwordForm();
    expect(calls.map((c) => c.url)).toEqual(["/api/auth/reset/preview"]);
  });
});

describe("ResetAccount invalid links", () => {
  it("shows the generic invalid state without calling the server when the fragment is missing", async () => {
    openAt("/reset");
    expect(await screen.findByText(INVALID)).toBeTruthy();
    expect(calls).toEqual([]);
  });

  it("shows the same state for a 404 preview", async () => {
    reply("/api/auth/reset/preview", { status: 404, body: { error: "this link is invalid or expired" } });
    openAt(`/reset#${TOKEN}`);
    expect(await screen.findByText(INVALID)).toBeTruthy();
    expect(screen.queryByLabelText("New password")).toBeNull();
    expect(window.location.hash).toBe("");
  });

  it("switches to the invalid state when redeem answers 404", async () => {
    await passwordForm();
    reply("/api/auth/reset/redeem", { status: 404, body: { error: "this link is invalid or expired" } });
    fill("correct horse battery", "correct horse battery");
    fireEvent.click(ack());
    fireEvent.click(submit());
    expect(await screen.findByText(INVALID)).toBeTruthy();
    expect(screen.queryByLabelText("New password")).toBeNull();
  });
});

describe("ResetAccount password form", () => {
  it("warns what is deleted and what happens to sessions and CLI logins", async () => {
    await passwordForm(PASSWORD_ON);
    const text = document.body.textContent ?? "";
    expect(text).toMatch(/Anthropic and OpenAI tokens/);
    expect(text).toMatch(/Codex logins/);
    expect(text).toMatch(/Everything in your vault/);
    expect(text).toMatch(/Every browser session ends/);
    expect(text).toMatch(/A CLI login you already approved but have not finished is refused/);
    expect(text).toMatch(/waiting for approval stays pending, but your old sessions can no longer approve it/);
    expect(text).not.toMatch(/cancelled/);
    expect(text).toMatch(/Runs pinned to a deleted credential use your default credential/);
    expect(text).toMatch(/Queued Codex runs will fail/);
  });

  it("password login enabled: no single sign-on hint, and done says sign in with the new password", async () => {
    await passwordForm(PASSWORD_ON);
    await waitFor(() => expect(configCalls).toBe(1));
    expect(document.body.textContent).not.toMatch(/single sign-on/);
    reply("/api/auth/reset/redeem", { status: 204 });
    fill("correct horse battery", "correct horse battery");
    fireEvent.click(ack());
    fireEvent.click(submit());
    expect(
      await screen.findByText("Your account is reset. Sign in with your new password; uzi creates a fresh vault when you do."),
    ).toBeTruthy();
    expect(document.body.textContent).not.toMatch(/single sign-on/);
    expect(screen.getByRole("link", { name: "Sign in" })).toBeTruthy();
    // Password login on: the credentials reminder is shown.
    expect(document.body.textContent).toMatch(/Add your model credentials again in Settings once you are signed in\./);
  });

  it("SSO-only instance: hint and done say sign in with SSO, then enable the vault in Settings", async () => {
    await passwordForm(SSO_ONLY);
    expect(
      await screen.findByText(
        /This instance signs in with single sign-on only, so the web app cannot create your fresh vault yet\.\s+uzi\s+creates it at your first password sign-in, once an administrator turns password sign-in on\./,
      ),
    ).toBeTruthy();
    expect(document.body.textContent).not.toMatch(/Settings/);
    reply("/api/auth/reset/redeem", { status: 204 });
    fill("correct horse battery", "correct horse battery");
    fireEvent.click(ack());
    fireEvent.click(submit());
    const doneMsg = await screen.findByText(
      "Your account is reset. This instance uses single sign-on only, so the web app cannot create your fresh vault yet; uzi creates it at your first password sign-in, once an administrator turns password sign-in on. Until then you can sign in with single sign-on, but you cannot store new model credentials.",
    );
    expect(doneMsg.textContent).not.toMatch(/Settings/);
    // Password login off: no credentials reminder, since credentials cannot be stored yet.
    expect(document.body.textContent).not.toMatch(/Add your model credentials again in Settings/);
    // The done state must not send them to a password sign-in that cannot work.
    expect(document.body.textContent).not.toMatch(/Sign in with your new password/);
    expect(screen.getByRole("link", { name: "Sign in with SSO" })).toBeTruthy();
  });

  it("config unavailable: neutral wording that claims neither mode", async () => {
    await passwordForm(); // no config reply queued: the fetch fails
    await waitFor(() => expect(configCalls).toBe(1));
    const text = document.body.textContent ?? "";
    expect(text).toMatch(/If your instance signs in with single sign-on only/);
    expect(text).not.toMatch(/This instance signs in with single sign-on only/);
    reply("/api/auth/reset/redeem", { status: 204 });
    fill("correct horse battery", "correct horse battery");
    fireEvent.click(ack());
    fireEvent.click(submit());
    const done = await screen.findByText(/Your account is reset\./);
    expect(done.textContent).toMatch(/If your instance allows password sign-in, sign in with your new password;/);
    expect(done.textContent).toMatch(/If it uses single sign-on only, the web app cannot create your fresh vault until an administrator turns password sign-in on/);
    expect(done.textContent).not.toMatch(/Settings/);
  });

  it("keeps submit disabled until the confirmation is checked", async () => {
    await passwordForm();
    fill("correct horse battery", "correct horse battery");
    expect(submit()).toHaveProperty("disabled", true);
    fireEvent.click(ack());
    expect(submit()).toHaveProperty("disabled", false);
    fireEvent.click(ack());
    expect(submit()).toHaveProperty("disabled", true);
  });

  it("refuses a short password without calling redeem", async () => {
    await passwordForm();
    fill("short", "short");
    fireEvent.click(ack());
    fireEvent.click(submit());
    expect(await screen.findByRole("alert")).toHaveProperty(
      "textContent",
      "Use at least 12 characters for the new password.",
    );
    expect(calls.map((c) => c.url)).toEqual(["/api/auth/reset/preview"]);
  });

  it("refuses mismatched passwords without calling redeem", async () => {
    await passwordForm();
    fill("correct horse battery", "correct horse batterY");
    fireEvent.click(ack());
    fireEvent.click(submit());
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", "The two passwords do not match.");
    expect(calls.map((c) => c.url)).toEqual(["/api/auth/reset/preview"]);
  });

  it("links to sign in after success", async () => {
    await passwordForm();
    reply("/api/auth/reset/redeem", { status: 204 });
    fill("correct horse battery", "correct horse battery");
    fireEvent.click(ack());
    fireEvent.click(submit());
    fireEvent.click(await screen.findByRole("link", { name: "Sign in" }));
    expect(await screen.findByText("LOGIN PAGE")).toBeTruthy();
  });

  it("shows the server's message for a 503 and keeps the form", async () => {
    await passwordForm();
    const msg = "the reset could not be confirmed; try signing in with the new password, or ask for a new link";
    reply("/api/auth/reset/redeem", { status: 503, body: { error: msg } });
    fill("correct horse battery", "correct horse battery");
    fireEvent.click(ack());
    fireEvent.click(submit());
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", msg);
    expect(screen.getByLabelText("New password")).toBeTruthy();
  });
});

describe("ResetAccount vault_only variant", () => {
  it("shows the server's kind-specific 503 message as is and keeps the form", async () => {
    reply("/api/auth/reset/preview", { status: 200, body: { kind: "vault_only", expires_at: EXPIRES } });
    openAt(`/reset#${TOKEN}`);
    const button = await screen.findByRole("button", { name: "Reset" });
    const msg = "the reset could not be confirmed; try signing in with single sign-on, or ask for a new link";
    reply("/api/auth/reset/redeem", { status: 503, body: { error: msg } });
    fireEvent.click(ack());
    fireEvent.click(button);
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", msg);
    expect(screen.getByRole("button", { name: "Reset" })).toBeTruthy();
  });

  it("asks for no password and redeems without one", async () => {
    reply("/api/auth/reset/preview", { status: 200, body: { kind: "vault_only", expires_at: EXPIRES } });
    openAt(`/reset#${TOKEN}`);
    const button = await screen.findByRole("button", { name: "Reset" });
    expect(screen.queryByLabelText("New password")).toBeNull();
    expect(screen.queryByText(/If your instance only allows single sign-on/)).toBeNull();
    expect(document.body.textContent).toMatch(/Everything in your vault/);
    expect(button).toHaveProperty("disabled", true);
    expect(document.querySelector('input[type="password"]')).toBeNull();
    // Pre-redeem copy: no password to set, and the next SSO sign-in asks for a passphrase.
    const pre = document.body.textContent ?? "";
    expect(pre).toMatch(/this link asks for none/);
    expect(pre).toMatch(/Your next single sign-on sign-in asks you to set a new vault passphrase/);
    expect(pre).not.toMatch(/set a (new )?password/i);

    reply("/api/auth/reset/redeem", { status: 204 });
    fireEvent.click(ack());
    fireEvent.click(button);
    expect(await screen.findByRole("link", { name: "Sign in with SSO" })).toHaveProperty(
      "href",
      `${window.location.origin}/login`,
    );
    await waitFor(() => expect(calls).toHaveLength(2));
    // vault_only redemption sends no password key at all.
    expect(calls[1].body).toEqual({ token: TOKEN });
    expect(Object.keys(calls[1].body as object)).toEqual(["token"]);
    const done = document.body.textContent ?? "";
    expect(done).toMatch(/vault passphrase/);
    expect(done).toMatch(/uzi then asks you to set a new vault passphrase/);
    expect(done).not.toMatch(/Settings.*passphrase/);
    expect(done).not.toMatch(/passphrase in Settings/);
    expect(done).toMatch(/Once it is set, add your model credentials again in Settings/);
    // The generic password-kind line must not repeat.
    expect(done).not.toMatch(/Settings once you are signed in/);
    expectTokenNeverInUrl();
  });
});
