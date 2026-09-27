package forge

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ancestryBodyLimit bounds every response body BranchHead / CompareAncestry reads
// (issue #1582 M1). The positive answers are tiny (an ancestor candidate adds no
// commits and no files to a head...candidate comparison), so a body past this ceiling
// can only belong to a comparison that is NOT a positive proof; it is refused as
// AncestryUnknown rather than parsed, and the read stops at limit+1 bytes so a hostile
// forge cannot stream an unbounded body into the api.
const ancestryBodyLimit = 1 << 20 // 1 MiB

// forgejoCompareBodyLimit is the Forgejo-specific ceiling: the reverse comparison
// (candidate...head) lists every commit the head adds on top of the candidate, which
// can legitimately be larger than a single-commit payload. Still bounded.
const forgejoCompareBodyLimit = 8 << 20 // 8 MiB

// errAncestryOversize is returned when a response body exceeds its ceiling. It names
// no URL and carries no token material.
var errAncestryOversize = errors.New("response body exceeds the ancestry read ceiling")

// ancestryClient builds the redirect-REFUSING client every GitHub and Forgejo
// BranchHead / CompareAncestry request goes through (issue #1582 M1), the same
// pattern as the GitLab driver's logClient. It shares the driver's per-call timeout,
// and CheckRedirect returns http.ErrUseLastResponse, so a 3xx comes back to the
// caller as a response instead of being followed: the request's credential header is
// never re-sent to the redirect target, and an answer served from the redirect target
// is never read as proof. The callers treat any 3xx as an error.
func ancestryClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeoutClient(timeout).Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// isCommitSHA reports whether s is a full 40-char LOWERCASE hex commit id. The
// ancestry surface accepts nothing else: never a ref name, an abbreviated id, or an
// uppercase spelling a forge could resolve differently from the stored audit value.
func isCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validateAncestryArgs is the shared pre-request gate every driver's CompareAncestry
// runs first. It returns (AncestryAncestor, true, nil) for head == candidate (no
// request is needed: a commit is its own ancestor), (AncestryUnknown, true, err) for a
// malformed argument, and (_, false, nil) when the driver must ask the forge.
func validateAncestryArgs(driver, head, candidate string) (Ancestry, bool, error) {
	if !isCommitSHA(head) || !isCommitSHA(candidate) {
		return AncestryUnknown, true, fmt.Errorf("%s: compare ancestry: arguments must be 40-char lowercase hex commit ids", driver)
	}
	if head == candidate {
		return AncestryAncestor, true, nil
	}
	return "", false, nil
}

// readBounded reads at most limit bytes from r and returns errAncestryOversize when
// the body is longer. It reads limit+1 so an exactly-limit body is accepted and a
// longer one is detected without reading the rest.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errAncestryOversize
	}
	return b, nil
}

// maxRefHeadLen bounds a full ref name RefHead will send to a forge (issue #1751 M2): the
// "refs/" prefix plus migration 00251's 255-byte release_branch ceiling.
const maxRefHeadLen = len("refs/") + 255

// validateFullRef is RefHead's pre-request gate (issue #1751 M2): ref must be a FULL ref name
// ("refs/" followed by at least one more component) that satisfies git-check-ref-format's
// rules, so no caller-supplied string can steer the request anywhere but one exact ref. It
// refuses an over-long ref, "..", "//", "@{", a component starting with ".", a trailing "/",
// "." or ".lock", and any control character, DEL, space or one of ~ ^ : ? * [ \ .
func validateFullRef(driver, ref string) error {
	bad := func() error {
		return fmt.Errorf("%s: ref head: %q is not a well-formed full ref name", driver, truncateForError(ref))
	}
	rest, ok := strings.CutPrefix(ref, "refs/")
	if !ok || rest == "" || len(ref) > maxRefHeadLen {
		return bad()
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") || strings.Contains(ref, "/.") {
		return bad()
	}
	if strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, ".lock") {
		return bad()
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if c < 0x20 || c == 0x7f || strings.IndexByte(" ~^:?*[\\", c) >= 0 {
			return bad()
		}
	}
	return nil
}

// refObjectBody is the ONLY part of a GitHub / Forgejo git reference object RefHead reads:
// the full `ref` name (to refuse an answer naming another ref) and object.type/object.sha.
type refObjectBody struct {
	Ref    *string `json:"ref"`
	Object *struct {
		Type *string `json:"type"`
		SHA  *string `json:"sha"`
	} `json:"object"`
}

// commitSHAOf returns the commit id a decoded reference object carries for exactly ref, or an
// error when it names another ref, points at a non-commit object, or carries no 40-hex id.
func (b refObjectBody) commitSHAOf(driver, ref string) (string, error) {
	if b.Ref == nil || *b.Ref != ref {
		return "", fmt.Errorf("%s: ref head: response does not name the requested ref", driver)
	}
	if b.Object == nil || b.Object.Type == nil || *b.Object.Type != "commit" {
		return "", fmt.Errorf("%s: ref head: requested ref does not point at a commit object", driver)
	}
	if b.Object.SHA == nil || !isCommitSHA(*b.Object.SHA) {
		return "", fmt.Errorf("%s: ref head: response carries no 40-hex commit sha", driver)
	}
	return *b.Object.SHA, nil
}
