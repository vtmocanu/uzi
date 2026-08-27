package agentsource

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"golang.org/x/mod/semver"
)

// ls-remote-equivalent helpers (PRD #702 M2): a ref-advertisement-only reach at the
// source — NO pack fetch, NO full clone. This is the one NEW outbound-egress path the
// PRD adds; it stays behind the same SSRF allowlist recheck and cookie-only admin gate
// as sync (see handler.PostAgentSourceResolveLatest and Decision 5).

// RemoteRefs is the advertised ref set from a SINGLE ref-advertisement round trip
// (ls-remote-equivalent; no pack fetch, no full clone).
type RemoteRefs struct {
	HeadSHA  string            // advertised HEAD (default-branch) tip; "" if none advertised
	Tags     map[string]string // short tag name -> commit SHA (peeled for annotated tags)
	Branches map[string]string // short branch name -> tip SHA
}

// advertisement is the product of a single ref-advertisement round trip: the advertised
// refs plus the live upload-pack session and wire budget a caller needs either to STOP
// (ListRemoteRefs — ls-remote-equivalent) or to CONTINUE into a pack fetch
// (FetchRoleFiles). It is the shared transport setup for both entry points, so there is
// no duplicated URL-validate → transport → session → advertise logic. The caller must
// defer close(); it ends the upload-pack session and cancels the bounded context.
type advertisement struct {
	refs    *packp.AdvRefs
	session transport.UploadPackSession
	budget  *wireBudget
	scrub   func(string) string
	ctx     context.Context
	cancel  context.CancelFunc
}

func (a *advertisement) close() {
	if a.session != nil {
		_ = a.session.Close()
	}
	if a.cancel != nil {
		a.cancel()
	}
}

// wrap turns a go-git error into a clean, PAT-scrubbed agentsource error — and, when the
// bounded body tripped the wire cap, into the cap message rather than the opaque
// pack-decode read error the trip surfaces as. Shared by both callers so the fetch and
// the advertisement report wire-budget trips identically.
func (a *advertisement) wrap(stage string, e error) error {
	if a.budget != nil && a.budget.tripped() {
		return fmt.Errorf("agentsource: %s: source response exceeded the %d-byte clone wire budget", stage, int64(maxCloneWireBytes))
	}
	return fmt.Errorf("agentsource: %s: %s", stage, a.scrub(e.Error()))
}

// advertise runs ONE ref-advertisement round trip against opts.CloneURL and returns the
// advertised refs plus the still-open session. It reuses the SAME guarded transport as
// FetchRoleFiles — the per-operation *http.Client carrying the redirect allowlist guard
// and the cumulative wire-size cap, plus the 60s cloneTimeout and the PAT-scrub — so the
// ls-remote path keeps every control the fetch path has. It never calls fetchCommit, so
// no pack is fetched: this is the ls-remote-equivalent, a single tiny round trip.
//
// The URL is validated with go-git's own endpoint parser (so the dialed endpoint matches
// the allowlisted one, and a file:// fixture can drive it in tests). On any error the
// helper cleans up what it created and returns a PAT-scrubbed agentsource error.
func advertise(ctx context.Context, opts CloneOptions) (*advertisement, error) {
	scrub := scrubber(opts.Token)
	url := strings.TrimSpace(opts.CloneURL)

	ep, perr := transport.NewEndpoint(url)
	if perr != nil {
		return nil, fmt.Errorf("agentsource: invalid clone url: %s", scrub(perr.Error()))
	}

	var auth transport.AuthMethod
	if opts.Token != "" {
		user := strings.TrimSpace(opts.Username)
		if user == "" {
			user = defaultCloneUsername
		}
		auth = &githttp.BasicAuth{Username: user, Password: opts.Token}
	}

	tr, budget, terr := transportForEndpoint(ep, opts.RedirectAllowed)
	if terr != nil {
		return nil, fmt.Errorf("agentsource: clone transport: %s", scrub(terr.Error()))
	}

	cctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	adv := &advertisement{budget: budget, scrub: scrub, ctx: cctx, cancel: cancel}

	session, serr := tr.NewUploadPackSession(ep, auth)
	if serr != nil {
		cancel()
		return nil, adv.wrap("clone session", serr)
	}
	adv.session = session

	ar, aerr := session.AdvertisedReferencesContext(cctx)
	if aerr != nil {
		adv.close()
		return nil, adv.wrap("list refs", aerr)
	}
	adv.refs = ar
	return adv, nil
}

// ListRemoteRefs returns the source's advertised refs from a single ref-advertisement
// round trip — the ls-remote-equivalent. It NEVER fetches a pack (no fetchCommit), so it
// is a tiny, bounded, guarded round trip behind the same transport controls FetchRoleFiles
// uses. Annotated tags map to their peeled COMMIT sha; lightweight tags and branches map
// to their advertised object sha.
func ListRemoteRefs(ctx context.Context, opts CloneOptions) (RemoteRefs, error) {
	adv, err := advertise(ctx, opts)
	if err != nil {
		return RemoteRefs{}, err
	}
	defer adv.close()

	out := RemoteRefs{
		Tags:     map[string]string{},
		Branches: map[string]string{},
	}
	ar := adv.refs
	if ar.Head != nil {
		out.HeadSHA = ar.Head.String()
	}
	const tagPrefix = "refs/tags/"
	const headPrefix = "refs/heads/"
	// ar.References carries every advertised ref; ar.Peeled carries the COMMIT an
	// annotated tag points at (there is no literal wildcard key — iterate). For a tag,
	// prefer the peeled commit so an annotated tag resolves to its commit sha, not the
	// tag object's sha; a lightweight tag has no Peeled entry and maps straight to its
	// commit. Peeled entries always correspond to a References entry, so iterating
	// References is complete.
	for name, h := range ar.References {
		switch {
		case strings.HasPrefix(name, tagPrefix):
			short := strings.TrimPrefix(name, tagPrefix)
			if peeled, ok := ar.Peeled[name]; ok {
				out.Tags[short] = peeled.String()
			} else {
				out.Tags[short] = h.String()
			}
		case strings.HasPrefix(name, headPrefix):
			out.Branches[strings.TrimPrefix(name, headPrefix)] = h.String()
		}
	}
	return out, nil
}

// ResolveLatestTag returns the advertised tag name with the highest semver precedence,
// or "" (nil error) when the source advertises no valid-semver tag. It performs a single
// ref advertisement (via ListRemoteRefs) — no pack fetch.
func ResolveLatestTag(ctx context.Context, opts CloneOptions) (string, error) {
	refs, err := ListRemoteRefs(ctx, opts)
	if err != nil {
		return "", err
	}
	return pickLatestSemverTag(refs.Tags), nil
}

// pickLatestSemverTag selects the newest tag by semver precedence from the advertised tag
// map, returning the ORIGINAL advertised tag string (never the normalized candidate), or
// "" when none is a valid semver.
//
// Decision 4: golang.org/x/mod/semver treats a malformed or non-"v"-prefixed version as
// equal (Compare returns 0), failing SILENTLY AND OPEN. So each tag is re-prefixed with
// "v" and IsValid-guarded before it can enter a comparison, and BOTH operands of every
// Compare are guaranteed valid (the running best was itself an accepted, IsValid
// candidate). A tag that is not valid semver is skipped, never compared. This generalizes
// the in-repo forge/forgejo.go pattern, which guards its one untrusted operand against a
// trusted constant, to two untrusted operands.
func pickLatestSemverTag(tags map[string]string) string {
	best := ""
	bestCand := ""
	for name := range tags {
		cand := "v" + strings.TrimPrefix(name, "v")
		if !semver.IsValid(cand) {
			continue
		}
		if best == "" || semver.Compare(cand, bestCand) > 0 {
			best = name
			bestCand = cand
		}
	}
	return best
}
