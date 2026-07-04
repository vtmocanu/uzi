// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, fireEvent } from "@testing-library/react";
import { isNearBottom, useFollowScroll } from "./useFollowScroll";

afterEach(cleanup);

describe("isNearBottom", () => {
  it("is true within the threshold of the bottom and false above it", () => {
    expect(isNearBottom({ scrollTop: 800, scrollHeight: 1000, clientHeight: 200 })).toBe(true);
    expect(isNearBottom({ scrollTop: 700, scrollHeight: 1000, clientHeight: 200 })).toBe(false);
    // Exactly at the threshold edge (48px gap) still counts as near-bottom.
    expect(isNearBottom({ scrollTop: 752, scrollHeight: 1000, clientHeight: 200 })).toBe(true);
  });
});

// Harness exposes the hook's follow state and drives a scroll container whose
// metrics we control (jsdom does not lay out, so scrollHeight/clientHeight are
// stubbed).
function Harness({ count }: { count: number }) {
  const f = useFollowScroll(count);
  return (
    <div>
      <div data-testid="state">{f.paused ? `paused:${f.newCount}` : "follow"}</div>
      <div data-testid="scroller" ref={f.ref} onScroll={f.onScroll}>
        {Array.from({ length: count }, (_, i) => (
          <p key={i}>m{i}</p>
        ))}
      </div>
      <button data-testid="jump" onClick={f.jumpToBottom}>
        jump
      </button>
    </div>
  );
}

function stubMetrics(el: HTMLElement, scrollHeight: number, clientHeight: number) {
  Object.defineProperty(el, "scrollHeight", { configurable: true, get: () => scrollHeight });
  Object.defineProperty(el, "clientHeight", { configurable: true, get: () => clientHeight });
}

describe("useFollowScroll", () => {
  it("scrolls to bottom on append while following, without accruing a new count", () => {
    const { getByTestId, rerender } = render(<Harness count={3} />);
    const scroller = getByTestId("scroller");
    stubMetrics(scroller, 1000, 200);

    rerender(<Harness count={5} />); // append 2 while following
    expect(getByTestId("state").textContent).toBe("follow");
    expect(scroller.scrollTop).toBe(1000); // pinned to the bottom (instant)
  });

  it("pauses when the user scrolls up and accrues the new-message count", () => {
    const { getByTestId, rerender } = render(<Harness count={3} />);
    const scroller = getByTestId("scroller");
    stubMetrics(scroller, 1000, 200);

    // User scrolls up, away from the bottom.
    scroller.scrollTop = 0;
    fireEvent.scroll(scroller);
    expect(getByTestId("state").textContent).toBe("paused:0");

    // Two messages arrive while paused: they are counted, not scrolled to.
    rerender(<Harness count={5} />);
    expect(getByTestId("state").textContent).toBe("paused:2");

    // Scrolling back to the bottom re-arms follow and clears the count.
    scroller.scrollTop = 800;
    fireEvent.scroll(scroller);
    expect(getByTestId("state").textContent).toBe("follow");
  });

  it("jumpToBottom re-arms follow and clears the count", () => {
    const { getByTestId, rerender } = render(<Harness count={3} />);
    const scroller = getByTestId("scroller");
    stubMetrics(scroller, 1000, 200);

    scroller.scrollTop = 0;
    fireEvent.scroll(scroller);
    rerender(<Harness count={6} />);
    expect(getByTestId("state").textContent).toBe("paused:3");

    fireEvent.click(getByTestId("jump"));
    expect(getByTestId("state").textContent).toBe("follow");
    expect(scroller.scrollTop).toBe(1000);
  });
});
