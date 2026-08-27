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
