package fetchctl

// refusalReasons is every reason code the fetcher may put in a refused attempt's record:
// the Reason* constants of api/internal/fetcher. It is a copy, not an import, on purpose:
// importing the fetcher would link its untrusted-content client into the api binary
// (Decision 2 keeps that code in its own Deployment), for a list of string constants.
// TestRefusalReasonsMatchFetcher parses the fetcher's source and fails when the two lists
// differ in either direction, so a reason added to the fetcher cannot make the api refuse
// to log it.
var refusalReasons = map[string]bool{
	"bad_request":         true,
	"url_too_long":        true,
	"credential_invalid":  true,
	"admission_refused":   true,
	"control_unavailable": true,
	"log_failed":          true,
	"busy":                true,
	"invalid_url":         true,
	"not_https":           true,
	"userinfo":            true,
	"ip_literal":          true,
	"port":                true,
	"off_list":            true,
	"redirect_off_list":   true,
	"private_address":     true,
	"too_many_redirects":  true,
	"redirect_invalid":    true,
	"content_encoding":    true,
	"dns_failed":          true,
	"connect_failed":      true,
	"tls":                 true,
	"timeout":             true,
	"cancelled":           true,
	"headers_too_large":   true,
	"upstream_status":     true,
	"upstream_error":      true,
	"too_large":           true,
}

// KnownReason reports whether code is one of the fetcher's refusal reasons.
func KnownReason(code string) bool { return refusalReasons[code] }
