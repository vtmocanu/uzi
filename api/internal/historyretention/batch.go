package historyretention

const (
	maxBatch = 1000
	minBatch = 50
	// warnAfterTimeouts is the number of consecutive statement timeouts on one table after which
	// a warning is logged (and again at every further multiple).
	warnAfterTimeouts = 5
	// holdTicks is how many successful batches a table keeps growing below the size that last
	// timed out before it probes that size again.
	holdTicks = 20
	// behindWarnAfter is the number of full batches below maxBatch (a run broken only by a short
	// batch or one at maxBatch; statement timeouts do not break it) after which a
	// "may be falling behind" warning is logged (and again at every further multiple).
	behindWarnAfter = 20
)

// BatchController is the adaptive delete-batch size of one table. It is pure state with no
// locking and no logging: the caller serialises access and acts on the returned warn flags.
//
// A statement timeout halves the batch (floor minBatch) and remembers the size that timed out as
// a ceiling. While the ceiling holds (at most holdTicks successes) a success grows the batch
// halfway toward the ceiling and never to it, so a table that times out at 1000 and succeeds at
// 500 settles just under 1000 instead of alternating between 0 and 500 rows. When the hold ends,
// or a success is at or above the ceiling, the ceiling is cleared and the next success doubles
// the batch toward maxBatch.
type BatchController struct {
	batch    int32
	timeouts int
	ceiling  int32 // 0 = none
	hold     int
	behind   int
}

// NewBatchController starts at maxBatch with no ceiling.
func NewBatchController() *BatchController { return &BatchController{batch: maxBatch} }

// Limit is the batch size to use for the next delete.
func (c *BatchController) Limit() int32 { return c.batch }

// Timeouts is the current run of consecutive statement timeouts.
func (c *BatchController) Timeouts() int { return c.timeouts }

// Behind is the current run of full batches below maxBatch. Only a short batch or a batch at
// maxBatch ends the run; a statement timeout does not, so a table that times out more often than
// once per behindWarnAfter successes still warns.
func (c *BatchController) Behind() int { return c.behind }

// TimedOut records a statement timeout of a delete that used batch size b. It reports whether a
// "repeated statement timeouts" warning is due (every warnAfterTimeouts-th consecutive one).
func (c *BatchController) TimedOut(b int32) (warn bool) {
	c.timeouts++
	c.ceiling = b
	c.hold = holdTicks
	c.batch = max(b/2, minBatch)
	return c.timeouts%warnAfterTimeouts == 0
}

// Succeeded records a delete of n rows that used batch size b. It reports whether a "deleting
// below the maximum batch" warning is due: every behindWarnAfter-th full batch of the run
// (n == b) while b < maxBatch. A short batch, or one at maxBatch, resets that run.
func (c *BatchController) Succeeded(b int32, n int64) (warnBehind bool) {
	c.timeouts = 0
	if c.ceiling == 0 || b >= c.ceiling {
		c.ceiling, c.hold = 0, 0
		c.batch = min(b*2, maxBatch)
	} else {
		c.hold--
		c.batch = max(b, b+(c.ceiling-b)/2)
		if c.hold <= 0 {
			c.ceiling = 0
		}
	}
	if n == int64(b) && b < maxBatch {
		c.behind++
		return c.behind%behindWarnAfter == 0
	}
	c.behind = 0
	return false
}
