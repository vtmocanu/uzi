// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";

// mockApi.reorderBoard is a SECOND IMPLEMENTATION of the server's freeze (PRD #102
// M5), so it is a contract rather than a convenience: mock mode is how this feature
// gets demoed and browser-reviewed without a live stack, and a mock that reorders
// nothing looks identical to a board nobody has dragged.
//
// Each case is authored, not snapshotted from the demo board. That is the point: the
// demo data contains no evicted iid and no unlisted open card, so a fixture built by
// snapshotting whatever `mockBoards` happens to hold would pass against a mock missing
// cases 2 and 3 entirely, while reading as full coverage.
//
// A fresh module registry per test isolates the mock's mutable board state — it
// reorders `b.cards` in place, so a leaked board would make these order-dependent.

const KEY = "uzi.mock.v2";

function installStorage(): void {
  const m = new Map<string, string>();
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: {
      getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
      setItem: (k: string, v: string) => void m.set(k, String(v)),
      removeItem: (k: string) => void m.delete(k),
      clear: () => m.clear(),
      key: (i: number) => [...m.keys()][i] ?? null,
      get length() {
        return m.size;
      },
    } as Storage,
  });
}

async function freshApi() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

const REPO = "repo-uzi";

beforeEach(() => {
  installStorage();
  window.localStorage.removeItem(KEY);
});
afterEach(() => {
  vi.resetModules();
});

// iidsOf reads the board's card order, which is what "Manual" mode renders: the mock
// has no board_position to inspect, and neither does the real client — the order IS
// the payload order (the server's ORDER BY does the work).
async function boardIids(api: Awaited<ReturnType<typeof freshApi>>, repoId = REPO) {
  const { board } = await api.getBoard(repoId);
  return board.cards.map((c) => c.iid);
}

describe("mockApi.reorderBoard — the four freeze behaviours the server has", () => {
  it("1. reorders the cards to the submitted iid order", async () => {
    const api = await freshApi();
    const before = await boardIids(api);
    expect(before.length).toBeGreaterThan(2);

    // Reverse the open cards: a demo drag has to visibly move a card, and a fixture
    // that submits the existing order cannot tell a working mock from a no-op one.
    const { board: b0 } = await api.getBoard(REPO);
    const open = b0.cards.filter((c) => !c.closed).map((c) => c.iid);
    const reversed = [...open].reverse();
    await api.reorderBoard(REPO, reversed);

    const after = await boardIids(api);
    expect(after.slice(0, reversed.length)).toEqual(reversed);
  });

  it("2. SKIPS an iid that is not on the board instead of throwing", async () => {
    // The server's freeze joins on forge_issue_iid, so an iid evicted between the
    // client's render and its submit updates zero rows. A mock that 404s here would
    // make a perfectly ordinary race look like a broken drag in the demo.
    const api = await freshApi();
    const { board: b0 } = await api.getBoard(REPO);
    const open = b0.cards.filter((c) => !c.closed).map((c) => c.iid);
    const ghost = Math.max(...b0.cards.map((c) => c.iid)) + 1000;

    await expect(api.reorderBoard(REPO, [ghost, ...open])).resolves.toBeTruthy();

    const after = await boardIids(api);
    expect(after).not.toContain(ghost);
    expect(after.slice(0, open.length)).toEqual(open);
  });

  it("3. sends open cards ABSENT from the list to the end, in iid order", async () => {
    // The mirror of the server's ClearBoardOrderExcept: an omitted open card is nulled
    // and therefore reads back after every positioned row, ordered by iid.
    const api = await freshApi();
    const { board: b0 } = await api.getBoard(REPO);
    const open = b0.cards.filter((c) => !c.closed).map((c) => c.iid);
    expect(open.length).toBeGreaterThan(3);

    const omitted = [open[0], open[1]];
    const submitted = open.slice(2);
    await api.reorderBoard(REPO, submitted);

    const after = await boardIids(api);
    expect(after.slice(0, submitted.length)).toEqual(submitted);
    // The two omitted ones are now in the tail, in iid order relative to each other.
    const tail = after.slice(submitted.length);
    const omittedInTail = tail.filter((iid) => omitted.includes(iid));
    expect(omittedInTail).toEqual([...omitted].sort((a, b) => a - b));
  });

  it("4. never gives a closed card a rank", async () => {
    // Closed is not a drop target, so a position there is unreachable by drag — but a
    // frozen one would ride an issue that later reopens and drop it at an arbitrary
    // point in its new column. The client filters closed cards out; the mock must not
    // rank one even if it is handed it.
    const api = await freshApi();
    const { board: b0 } = await api.getBoard(REPO);
    const closed = b0.cards.filter((c) => c.closed).map((c) => c.iid);
    expect(closed.length).toBeGreaterThan(0);

    await api.reorderBoard(REPO, closed);

    const after = await boardIids(api);
    // Ranking them would have hoisted them to the front; they must be in the unranked
    // tail instead, which on an otherwise-unfrozen board is iid order overall.
    expect(after.slice(0, closed.length)).not.toEqual(closed);
    expect(after).toEqual([...after].sort((a, b) => a - b));
  });

  it("treats an empty list as no-op, never as a wipe", async () => {
    // The server guards this because `<> ALL('{}')` matches every row; the mock has to
    // agree or the demo diverges from the product at exactly the dangerous case.
    const api = await freshApi();
    const { board: b0 } = await api.getBoard(REPO);
    const open = b0.cards.filter((c) => !c.closed).map((c) => c.iid);
    const reversed = [...open].reverse();
    await api.reorderBoard(REPO, reversed);
    const afterFreeze = await boardIids(api);

    await api.reorderBoard(REPO, []);
    expect(await boardIids(api)).toEqual(afterFreeze);
  });

  it("404s on an unknown repo, like every other board method", async () => {
    const api = await freshApi();
    await expect(api.reorderBoard("no-such-repo", [1])).rejects.toMatchObject({ status: 404 });
  });
});
