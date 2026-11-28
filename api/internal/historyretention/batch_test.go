package historyretention

import "testing"

func TestBatchControllerHalvesWithFloorAndWarnsEveryFifth(t *testing.T) {
	c := NewBatchController()
	var warns []int
	for i := 1; i <= 10; i++ {
		if c.TimedOut(c.Limit()) {
			warns = append(warns, i)
		}
	}
	if len(warns) != 2 || warns[0] != 5 || warns[1] != 10 {
		t.Fatalf("warned at %v, want 5 and 10", warns)
	}
	if c.Limit() != minBatch {
		t.Fatalf("limit = %d, want the floor %d", c.Limit(), minBatch)
	}
}

func TestBatchControllerGrowsBelowCeilingThenProbes(t *testing.T) {
	c := NewBatchController()
	c.TimedOut(1000) // ceiling 1000, batch 500
	for i := 0; i < holdTicks; i++ {
		b := c.Limit()
		if b >= 1000 {
			t.Fatalf("tick %d: limit %d reached the ceiling during the hold", i, b)
		}
		c.Succeeded(b, int64(b))
	}
	if got := c.Limit(); got < 990 || got >= 1000 {
		t.Fatalf("limit at the end of the hold = %d, want just below 1000", got)
	}
	b := c.Limit()
	c.Succeeded(b, int64(b))
	if c.Limit() != maxBatch {
		t.Fatalf("limit after the hold = %d, want a probe at %d", c.Limit(), maxBatch)
	}
}

func TestBatchControllerBehindWarning(t *testing.T) {
	c := NewBatchController()
	c.TimedOut(1000) // batch 500
	warned := 0
	for i := 0; i < 2*behindWarnAfter; i++ {
		b := int32(100)
		if c.Succeeded(b, int64(b)) {
			warned++
		}
	}
	if warned != 2 {
		t.Fatalf("behind warnings = %d, want 2 over %d full batches", warned, 2*behindWarnAfter)
	}
	// a short batch resets the run
	c.Succeeded(100, 3)
	if c.Behind() != 0 {
		t.Fatalf("behind = %d after a short batch, want 0", c.Behind())
	}
	// a full batch at the maximum is not behind
	if c.Succeeded(maxBatch, maxBatch) || c.Behind() != 0 {
		t.Fatal("a full batch at maxBatch counted as behind")
	}
}
