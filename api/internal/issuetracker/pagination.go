package issuetracker

import "errors"

// pageSize is the $top of every list request.
const pageSize = 100

// errItemCeiling is returned when a listing carries more than the item budget
// (maxItems) or a page carries more than pageSize items. It carries no remote
// content.
var errItemCeiling = errors.New("issuetracker: listing exceeds the item ceiling")

// errPageCeiling and errByteCeiling are the page and byte siblings of errItemCeiling.
// Like it they are matched by errors.Is through redactor.sentinel (see
// listingCeilings), so a caller can tell a ceiling from any other failure.
var (
	errPageCeiling = errors.New("issuetracker: listing exceeds the page ceiling")
	errByteCeiling = errors.New("issuetracker: listing exceeds the byte ceiling")
)

// listingCeilings are the unexported sentinels isPackageSentinel accepts besides
// packageSentinels.
var listingCeilings = []error{errItemCeiling, errPageCeiling, errByteCeiling}

// maxItems, maxPages and maxTotalBytes are sanity ceilings against a server
// that never ends a listing. maxItems is one combined budget: it counts the
// top-level items of every page plus the nested entries (tags) they carry. Vars
// so tests can lower them.
var (
	maxItems      = 100_000
	maxPages      = 2_000
	maxTotalBytes = 64 << 20
)

// paginate calls fetch with a growing skip until a short page. fetch returns
// the page and its raw byte count; the counts are summed across pages.
// Exceeding maxItems, maxPages or maxTotalBytes is an error carrying no remote
// content.
func paginate[T any](fetch func(skip, top int) ([]T, int, error)) ([]T, error) {
	var all []T
	total := 0
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, errPageCeiling
		}
		items, n, err := fetch(len(all), pageSize)
		if err != nil {
			return nil, err
		}
		total += n
		if total > maxTotalBytes {
			return nil, errByteCeiling
		}
		all = append(all, items...)
		if len(all) > maxItems {
			return nil, errItemCeiling
		}
		if len(items) < pageSize {
			return all, nil
		}
	}
}
