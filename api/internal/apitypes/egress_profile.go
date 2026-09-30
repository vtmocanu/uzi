package apitypes

import "time"

// EgressProfileWarningDTO is a note about a stored host entry (PRD #1906 Decision 4).
// Codes: "multi_publisher_override", the entry reaches a host where many unrelated
// publishers serve content and an admin admitted it with an explicit override;
// "multi_publisher_needs_override" (reads only), the current multi-publisher list flags
// the stored entry (the list grew after the profile was written) and it has no override,
// so it matches nothing until the override is added or the entry removed; and
// "stale_entry" (reads only), the current rules no longer accept the stored entry, so it
// matches nothing until the profile is edited.
type EgressProfileWarningDTO struct {
	Entry   string `json:"entry"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// EgressProfileDTO is the admin view of one egress profile (PRD #1906 M1): a named site
// list. Hosts are the normalized entries (an exact host or "*.base"; a wildcard matches
// proper subdomains of base, not base itself). MultiPublisherOverride is the subset of
// Hosts an admin accepted despite the multi-publisher warning, and Warnings repeats one
// warning per such entry on every read, not only on the write, plus a
// multi_publisher_needs_override or stale_entry warning on read for any stored entry the
// current rules flag without an override or refuse.
//
// Name, Description and Hosts are admin-authored display strings: the api validates them
// (termsafe, IDNA A-labels), and the CLI still renders them through its sanitizing cell
// path.
type EgressProfileDTO struct {
	ID                     string                    `json:"id"`
	Name                   string                    `json:"name"`
	Description            string                    `json:"description"`
	Hosts                  []string                  `json:"hosts"`
	MultiPublisherOverride []string                  `json:"multi_publisher_override"`
	Warnings               []EgressProfileWarningDTO `json:"warnings"`
	CreatedBy              *string                   `json:"created_by"`
	UpdatedBy              *string                   `json:"updated_by"`
	CreatedAt              time.Time                 `json:"created_at"`
	UpdatedAt              time.Time                 `json:"updated_at"`
}
