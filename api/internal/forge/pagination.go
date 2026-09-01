package forge

import "fmt"

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
	var out []T
	apiPage := 1
	for iter := 1; ; iter++ {
		items, next, err := fetch(apiPage)
		if err != nil {
			return nil, wrap(err)
		}
		out = append(out, items...)
		if len(out) > maxForgeItems {
			return nil, wrap(forgePaginationCapErr("item", maxForgeItems))
		}
		if next == 0 {
			break
		}
		if iter >= maxForgePages {
			return nil, wrap(forgePaginationCapErr("page", maxForgePages))
		}
		apiPage = next
	}
	return out, nil
}
