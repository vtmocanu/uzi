// @vitest-environment jsdom
//
// The mock is the browsable spec, so its createConnection has to reproduce the
// server's duplicate-connection guard rather than overwriting whatever it finds:
// a create that targets an already-connected (forge type, base URL) is a 409
// unless replace=true, and replace=true rotates that connection in place instead
// of adding a second one. Comparing the target NORMALIZED is part of the
// contract, because a trailing slash or a capital letter is the same host.
import { afterEach, describe, it, expect, vi } from "vitest";

async function reload() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

afterEach(() => vi.resetModules());

const HOST = "https://gitlab.example.com";

describe("mockApi — forge connection duplicate guard", () => {
  it("refuses a create for an already-connected host with a coded 409", async () => {
    const api = await reload();
    const before = await api.listConnections();
    const existing = before.connections.find((c) => c.base_url === HOST);
    expect(existing).toBeTruthy();

    const err = await api.createConnection(HOST, "glpat-second").catch((e) => e);
    expect(err.status).toBe(409);
    expect(err.body).toMatchObject({
      code: "forge_connection_exists",
      connection_id: existing!.id,
    });

    // The refusal must not have touched the connection it protects.
    const after = await api.listConnections();
    expect(after.connections.find((c) => c.id === existing!.id)).toEqual(existing);
  });

  it("treats a spelling that normalizes onto the host as the same target", async () => {
    const api = await reload();
    for (const spelling of [`${HOST}/`, "https://GitLab.Example.com", `${HOST}/some/path`]) {
      const err = await api.createConnection(spelling, "glpat-second").catch((e) => e);
      expect(err.status, spelling).toBe(409);
    }
  });

  it("rotates the existing connection in place when replace=true", async () => {
    const api = await reload();
    const before = await api.listConnections();
    const existing = before.connections.find((c) => c.base_url === HOST)!;

    const { connection } = await api.createConnection(HOST, "glpat-rotated", "gitlab", true);
    expect(connection.id).toBe(existing.id);

    const after = await api.listConnections();
    expect(after.connections.filter((c) => c.base_url === HOST)).toHaveLength(1);
    expect(after.connections.find((c) => c.id === existing.id)?.id).toBe(existing.id);
  });

  it("does not refuse a different forge type on the same host", async () => {
    const api = await reload();
    const before = await api.listConnections();
    const gitlab = before.connections.find((c) => c.base_url === HOST)!;
    // forgejo is a different connection from the gitlab one on that host, so the
    // create must succeed: any rejection fails the test.
    const { connection } = await api.createConnection(HOST, "fjp-second", "forgejo");
    expect(connection.forge_type).toBe("forgejo");
    expect(connection.base_url).toBe(HOST);
    expect(connection.id).not.toBe(gitlab.id);
  });
});
