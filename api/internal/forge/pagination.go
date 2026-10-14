package forge

import (
	"context"
	"fmt"
)

// maxForgeItems and maxForgePages backstop every driver pagination loop against a
// buggy or compromised, PAT-authenticated forge that returns a perpetually non-zero
// next page. maxForgeItems bounds the memory-growth attack (full pages forever);
// maxForgePages bounds the spin attack where the accumulator never grows (empty or
// non-advancing pages returned forever with a non-zero next page), where the item
// cap would never trip. Mirrors api/internal/uzicli/client.go's maxLogsMessages.
//
// Both are sized far above any real forge list (projects/labels/issues/label-events/
// jobs are all orders of magnitude smaller), so only a misbehaving forge hits them.
// They are vars, not consts, purely so backstop tests can lower and restore them —
// the same rationale RunLogs records for maxLogsMessages.
var (
	maxForgeItems = 1_000_000
	maxForgePages = 20_000
)

// forgePaginationCapErr builds the backstop error WITHOUT a driver/op prefix, so
// each call site can wrap it in that driver's own redacting error idiom. kind is
// "item" or "page".
func forgePaginationCapErr(kind string, limit int) error {
	return fmt.Errorf("pagination exceeded %s backstop of %d; aborting (possible hostile or buggy forge)", kind, limit)
}

// paginate accumulates every page of a forge list into one slice, applying the
// shared item/page backstops from this file. fetch is called once per page with the
// forge page number to request (1-based); it maps that page's raw results into []T
// and returns the forge's next-page pointer (0 == last page). wrap is the driver's
// own redacting error idiom bound to the operation (e.g.
// func(e error) error { return g.wrapErr("list projects", e) }); it wraps BOTH a
// fetch error and a cap error so every failure carries the same driver+op prefix the
// inline loops produced. fetch must return the RAW error (unwrapped) so paginate does
// not double-wrap.
func paginate[T any](wrap func(error) error, fetch func(page int) (items []T, next int, err error)) ([]T, error) {
	return paginateLimits(wrap, fetch, maxForgeItems, maxForgePages, nil)
}

// paginateLimits bounds attempts and accumulated mapped items; a failed page
// discards the entire snapshot. Strict CI reads also reject nonadvancing pointers
// and empty continuing pages, so one bad snapshot never becomes complete evidence.
func paginateLimits[T any](wrap func(error) error, fetch func(int) ([]T, int, error), itemLimit, pageLimit int, budget *ciRepairBudget) ([]T, error) {
	capErr := func(kind string, limit int) error {
		if budget != nil {
			return budget.fail("job pagination " + kind)
		}
		return forgePaginationCapErr(kind, limit)
	}
	var out []T
	apiPage := 1
	for iter := 1; ; iter++ {
		items, next, err := fetch(apiPage)
		if err != nil {
			return nil, wrap(err)
		}
		if len(items) > itemLimit-len(out) {
			return nil, wrap(capErr("item", itemLimit))
		}
		out = append(out, items...)
		if next == 0 {
			break
		}
		if budget != nil && (next <= apiPage || len(items) == 0) {
			return nil, wrap(capErr("progress", pageLimit))
		}
		if iter >= pageLimit {
			return nil, wrap(capErr("page", pageLimit))
		}
		if budget != nil && len(out) == itemLimit {
			return nil, wrap(capErr("item", itemLimit))
		}
		apiPage = next
	}
	return out, nil
}

// paginateCIJobs retains ordinary pagination defaults outside the CI-watch lane.
func paginateCIJobs(ctx context.Context, pageSize int, wrap func(error) error, fetch func(int) ([]Job, int, error)) ([]Job, error) {
	b := ciBudget(ctx)
	if b == nil {
		return paginate(wrap, fetch)
	}
	return paginateLimits(wrap, func(page int) ([]Job, int, error) {
		items, next, err := fetch(page)
		if err = ciReadError(ctx, err); err != nil {
			return nil, 0, err
		}
		if len(items) > pageSize {
			return nil, 0, b.fail("oversized job page")
		}
		for _, job := range items {
			if job.ID <= 0 {
				return nil, 0, b.fail("invalid job identity")
			}
		}
		return items, next, nil
	}, 1000, 10, b)
}
