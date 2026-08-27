package agentsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// sourceAgentsDir is the repo-root directory the source's role files live in — the
// ecosystem convention agent/src/repoagents.ts reads (`.claude/agents/*.md`). Only
// the immediate `.md` entries are taken (non-recursive, regular files only).
const sourceAgentsDir = ".claude/agents"

// cloneTimeout bounds a whole fetch (list + clone + read), mirroring pushbroker's
// 60s ceiling. A hostile or dead source can never hang the reconcile loop past this.
const cloneTimeout = 60 * time.Second

// defaultCloneUsername is the BasicAuth username used when a token is configured
// without an explicit username. GitHub/GitLab both accept an arbitrary non-empty
// username with a PAT as the password.
const defaultCloneUsername = "oauth2"

// maxTotalBytes bounds the SUM of role-file bytes read out of a clone, so a repo of
// many just-under-cap files cannot OOM the api even within the MaxFiles count cap.
const maxTotalBytes = MaxFiles * MaxBytes

// maxCloneWireBytes bounds the TOTAL bytes the clone reads off the wire from an
// http(s) source (ref advertisement + the fetched pack), cumulative across every
// HTTP round trip of one fetch. It is the memory-bound half of FINDING 1 (PRD #602
// M3b review): go-git decodes the whole tip snapshot into an in-memory storer BEFORE
// readRoleFiles' per-file caps can run, so a tip carrying one multi-GB blob would OOM
// the api before any cap fired. This cap makes the plain giant-blob vector error the
// clone cleanly instead. It is deliberately generous — 16 role files × 64KB is ~1MiB,
// and a real roster repo's pack plus git/protocol overhead is a few MB — so a
// legitimate source never trips it, while a hostile one is bounded to tens of MiB.
//
// RESIDUAL (documented, see adr/0602 threat model): this bounds COMPRESSED wire bytes,
// not the RECONSTRUCTED/inflated size. A zlib decompression-bomb pack (small on the
// wire, huge inflated) still inflates into the storer under this cap. Closing that half
// needs a reconstructed-size pre-scan of the pack analogous to
// pushbroker.scanPackBudget; it is a deliberate follow-up, not implemented here. The
// mitigating preconditions make the residual acceptable: the source must be on the
// admin-configured AGENT_SOURCE_ALLOWED_BASE_URLS allowlist AND the feature explicitly
// enabled (both off by default), reconcile is single-flight on one goroutine, and the
// 60s cloneTimeout bounds the inflation wall-clock.
const maxCloneWireBytes = 48 << 20 // 48 MiB cumulative off-the-wire ceiling

// maxCloneRedirects caps redirect hops on the http(s) clone. go-git's own policy
// already permits a redirect only on the initial ref-advertisement request; this is a
// belt-and-braces hop ceiling under our allowlisted-target CheckRedirect (FINDING 2).
const maxCloneRedirects = 5

// sha40Re matches a full 40-hex git object id — the "pinned SHA" ref form.
var sha40Re = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// errCloneWireBudget is returned from the bounded response body once the cumulative
// wire cap is crossed. It surfaces through go-git's pack decode as a read error; the
// fetch wrapper detects the trip flag and reports a clean, PAT-free message.
var errCloneWireBudget = errors.New("agentsource: clone exceeded wire budget")

// CloneOptions carries the (already-trimmed, already-allowlist-rechecked) inputs a
// single fetch needs. Token is the sealed clone credential decrypted by the caller;
// empty means a public clone (no BasicAuth is attached).
type CloneOptions struct {
	CloneURL string
	Ref      string
	Username string
	Token    string
	// Dir is the repo-relative subfolder to read role files from (PRD #702 M1). It
	// selects a subtree of the already-cloned, already-allowlisted repo — no new
	// egress. Empty defaults to sourceAgentsDir (".claude/agents"), so a caller that
	// does not set it reads exactly the historical location.
	Dir string
	// RedirectAllowed re-checks a redirect TARGET against the SSRF allowlist (FINDING
	// 2). It is the reconciler's cfg.AgentSourceBaseURLAllowed, threaded in so an
	// http(s) redirect (e.g. an allowlisted host answering 302 Location:
	// http://169.254.169.254/…) is refused unless the resolved target still passes the
	// allowlist. Nil (the file:// fixture path, or an unset caller) refuses ALL
	// redirects on the http(s) transport — fail-closed.
	RedirectAllowed func(rawURL string) bool
}

// FetchRoleFiles shallow-clones the configured source at the pinned ref, reads the
// `.claude/agents/*.md` role files at the repo root, and returns the resolved HEAD
// commit SHA plus the file bytes for ParseSet.
//
// It is the SECOND place go-git is used (pushbroker is the first). Unlike pushbroker
// (which pushes through go-git's default, process-global transport), the sync clones an
// UNTRUSTED remote, so it drives the upload-pack session through a transport SCOPED to
// this one operation — a per-fetch *http.Client carrying two guards go-git's global
// client does not: an allowlist-checked CheckRedirect (FINDING 2) and a cumulative
// wire-size cap (FINDING 1). Because the client is per-operation and never installed
// into go-git's global protocol registry (client.InstallProtocol), pushbroker's push
// pipeline is entirely unaffected. The URL is validated with go-git's own endpoint
// parser; the ref is validated with go-git plumbing (git-check-ref-format via
// ReferenceName.Validate, or a 40-hex SHA) and never string-interpolated into a
// refspec (the fetch asks for a resolved object HASH, not a name). Every returned error
// is PAT-scrubbed.
func FetchRoleFiles(ctx context.Context, opts CloneOptions) (sha string, files []SourceFile, err error) {
	scrub := scrubber(opts.Token)

	// The ref is validated with go-git plumbing (git-check-ref-format via
	// ReferenceName.Validate, or a 40-hex SHA) BEFORE any network op — a hostile ref
	// name never reaches the refspec layer (the fetch asks for a resolved HASH).
	refName, isSHA, rerr := classifyRef(opts.Ref)
	if rerr != nil {
		return "", nil, rerr
	}

	// advertise performs the single ref-advertisement round trip (URL validate →
	// guarded transport → upload-pack session → AdvertisedReferencesContext), the setup
	// FetchRoleFiles shares with ListRemoteRefs. It resolves both jobs the previous
	// implementation used two round trips for: resolving a named branch/tag to a hash,
	// and driving the fetch.
	adv, aerr := advertise(ctx, opts)
	if aerr != nil {
		return "", nil, aerr
	}
	defer adv.close()

	want, werr := resolveWant(adv.refs, refName)
	if werr != nil {
		return "", nil, werr
	}

	st := memory.NewStorage()
	if ferr := fetchCommit(adv.ctx, adv.session, adv.refs.Capabilities, want, st); ferr != nil {
		return "", nil, adv.wrap("clone failed", ferr)
	}

	commit, commitErr := resolveCommit(st, want)
	if commitErr != nil {
		return "", nil, fmt.Errorf("agentsource: resolve commit: %s", scrub(commitErr.Error()))
	}
	resolved := commit.Hash.String()

	// A full-SHA pin is only satisfiable at shallow depth if it IS the fetched tip
	// (Depth 1 fetches only the default branch's tip snapshot — an arbitrary historical
	// SHA's objects are not present). Fail with a clear, non-hanging error otherwise.
	if isSHA && !strings.EqualFold(resolved, strings.TrimSpace(opts.Ref)) {
		return "", nil, fmt.Errorf(
			"agentsource: pinned commit %s is not the fetched tip %s (a full-SHA pin must be the source's current default-branch tip at shallow clone depth; pin a tag or branch instead)",
			strings.TrimSpace(opts.Ref), resolved)
	}

	files, ferr := readRoleFiles(commit, opts.Dir)
	if ferr != nil {
		return "", nil, fmt.Errorf("agentsource: read role files: %s", scrub(ferr.Error()))
	}
	return resolved, files, nil
}

// transportForEndpoint builds the transport that drives ONE fetch. For an http(s)
// endpoint (the only production shape — the allowlist is https-only) it returns a
// transport backed by a per-operation *http.Client carrying the redirect allowlist
// guard and the cumulative wire-size cap, plus the *wireBudget so the caller can tell a
// cap trip from an ordinary error. For any other scheme (file:// — the trusted local
// test-fixture path, which has no redirects and no untrusted remote) it returns the
// stock client for that protocol; the budget is nil.
//
// The http(s) client is NEVER installed into go-git's process-global protocol registry
// (client.InstallProtocol), so it cannot alter pushbroker's push pipeline, which keeps
// using go-git's default client.
func transportForEndpoint(ep *transport.Endpoint, redirectAllowed func(string) bool) (transport.Transport, *wireBudget, error) {
	switch ep.Protocol {
	case "http", "https":
		budget := &wireBudget{remaining: maxCloneWireBytes}
		httpClient := &http.Client{
			Transport: &boundedRoundTripper{base: http.DefaultTransport, budget: budget},
			// go-git composes this CheckRedirect AFTER its own policy (which already only
			// allows a redirect on the initial info/refs request); this adds the host
			// re-check go-git omits — FINDING 2.
			CheckRedirect: redirectGuard(redirectAllowed),
		}
		return githttp.NewClient(httpClient), budget, nil
	default:
		tr, err := client.NewClient(ep)
		return tr, nil, err
	}
}

// redirectGuard builds the http.Client CheckRedirect that enforces FINDING 2: a
// redirect is refused unless its TARGET still passes the SSRF allowlist predicate, and
// the hop count is bounded. A nil predicate (no allowlist threaded in — the file://
// fixture path never installs this client, so nil means an unconfigured caller) refuses
// ALL redirects, fail-closed. It runs as the `next` link after go-git's own redirect
// policy, so both must pass for a redirect to be followed.
func redirectGuard(redirectAllowed func(string) bool) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxCloneRedirects {
			return fmt.Errorf("agentsource: too many redirects (%d)", len(via))
		}
		if redirectAllowed == nil || !redirectAllowed(req.URL.String()) {
			return fmt.Errorf("agentsource: refusing redirect to non-allowlisted target %q", req.URL.Redacted())
		}
		return nil
	}
}

// wireBudget is a per-fetch cumulative byte allowance shared by every response body of
// one clone. remaining and trip are touched from the HTTP transport's read goroutine,
// so both are accessed atomically.
type wireBudget struct {
	remaining int64 // bytes left before the cap is crossed
	trip      int32 // set to 1 once the cap is crossed
}

func (b *wireBudget) tripped() bool { return atomic.LoadInt32(&b.trip) == 1 }

// boundedRoundTripper wraps every response body in a boundedBody so reads draw down the
// shared wireBudget. It delegates the actual request to base (http.DefaultTransport).
type boundedRoundTripper struct {
	base   http.RoundTripper
	budget *wireBudget
}

func (t *boundedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	resp.Body = &boundedBody{rc: resp.Body, budget: t.budget}
	return resp, nil
}

// boundedBody draws each read down from the shared budget and returns errCloneWireBudget
// the moment the cumulative total crosses the cap, so an oversized source response
// fails the clone cleanly rather than being silently truncated (an EOF would surface as
// an opaque pack-corruption error instead).
type boundedBody struct {
	rc     io.ReadCloser
	budget *wireBudget
}

func (b *boundedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 && atomic.AddInt64(&b.budget.remaining, -int64(n)) < 0 {
		atomic.StoreInt32(&b.budget.trip, 1)
		return n, errCloneWireBudget
	}
	return n, err
}

func (b *boundedBody) Close() error { return b.rc.Close() }

// fetchCommit performs a shallow (depth-1) upload-pack fetch of exactly `want` into st.
// It mirrors go-git's own Remote.fetchPack: build the request from the advertised
// capabilities, ask for depth 1, then demux the sideband (if negotiated) and decode the
// pack into the storer. Depth 1 bounds history to the single tip snapshot — exactly the
// tree readRoleFiles needs — and coexists with the wire-size cap the transport enforces.
func fetchCommit(ctx context.Context, session transport.UploadPackSession, adv *capability.List, want plumbing.Hash, st storer.Storer) error {
	req := packp.NewUploadPackRequestFromCapabilities(adv)
	req.Depth = packp.DepthCommits(1)
	if err := req.Capabilities.Set(capability.Shallow); err != nil {
		return err
	}
	if adv.Supports(capability.NoProgress) {
		_ = req.Capabilities.Set(capability.NoProgress)
	}
	req.Wants = []plumbing.Hash{want}

	resp, err := session.UploadPack(ctx, req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Close() }()

	return packfile.UpdateObjectStorage(st, buildSideband(req.Capabilities, resp))
}

// buildSideband wraps the upload-pack response in a sideband demuxer when the negotiated
// capabilities muxed the pack onto a band (a copy of go-git's unexported
// buildSidebandIfSupported, with progress discarded — we request no-progress anyway).
func buildSideband(l *capability.List, reader io.Reader) io.Reader {
	switch {
	case l.Supports(capability.Sideband):
		return sideband.NewDemuxer(sideband.Sideband, reader)
	case l.Supports(capability.Sideband64k):
		return sideband.NewDemuxer(sideband.Sideband64k, reader)
	default:
		return reader
	}
}

// classifyRef validates the configured ref and reports how to clone it: an empty ref
// tracks the default branch (returns "", false), a 40-hex SHA is a pinned commit
// (returns "", true), and any other value is a branch/tag NAME resolveWant pins against
// the advertised refs (returns the bare name, false). It rejects a name that fails
// git-check-ref-format as BOTH a branch and a tag.
func classifyRef(ref string) (name plumbing.ReferenceName, isSHA bool, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false, nil
	}
	if sha40Re.MatchString(ref) {
		return "", true, nil
	}
	// Construct the two candidate full ref names and validate them with go-git
	// plumbing (git-check-ref-format). If NEITHER is a legal ref name, reject — this
	// is the check that stops a hostile ref from ever reaching the refspec layer.
	branchOK := plumbing.NewBranchReferenceName(ref).Validate() == nil
	tagOK := plumbing.NewTagReferenceName(ref).Validate() == nil
	if !branchOK && !tagOK {
		return "", false, fmt.Errorf("agentsource: invalid ref (not a valid git ref name or 40-hex SHA)")
	}
	// Return the bare name; resolveWant picks tags-vs-heads from the advertisement.
	return plumbing.ReferenceName(ref), false, nil
}

// resolveWant maps the classified ref onto the object hash to fetch, using the remote's
// advertised refs. An empty ref (default branch or a SHA pin — both classify to name
// "") resolves to the advertised HEAD; a named ref prefers a tag (the recommended
// pinned form, peeled to its commit when annotated) over a same-named branch. Because
// the fetch then asks for a resolved HASH rather than a name, a hostile ref name can
// never reach a refspec.
func resolveWant(ar *packp.AdvRefs, refName plumbing.ReferenceName) (plumbing.Hash, error) {
	name := refName.String()
	if name == "" {
		if ar.Head != nil {
			return *ar.Head, nil
		}
		return plumbing.ZeroHash, fmt.Errorf("agentsource: source did not advertise a default branch (HEAD)")
	}
	tagName := plumbing.NewTagReferenceName(name).String()
	branchName := plumbing.NewBranchReferenceName(name).String()
	if h, ok := ar.Peeled[tagName]; ok { // annotated tag → its commit
		return h, nil
	}
	if h, ok := ar.References[tagName]; ok { // lightweight tag → commit
		return h, nil
	}
	if h, ok := ar.References[branchName]; ok {
		return h, nil
	}
	return plumbing.ZeroHash, fmt.Errorf("agentsource: ref %q not found on source (neither a branch nor a tag)", name)
}

// resolveCommit turns a fetched hash into a commit, following an annotated tag object to
// its commit when the ref pointed at one (a lightweight tag / branch / HEAD already
// resolves straight to a commit).
func resolveCommit(s storer.EncodedObjectStorer, h plumbing.Hash) (*object.Commit, error) {
	if c, err := object.GetCommit(s, h); err == nil {
		return c, nil
	}
	if tag, err := object.GetTag(s, h); err == nil {
		return tag.Commit()
	}
	return nil, fmt.Errorf("hash %s is neither a commit nor an annotated tag", h)
}

// readRoleFiles reads the immediate `.claude/agents/*.md` regular files from the
// commit's tree (non-recursive; dirs, symlinks and submodules skipped), bounded to
// MaxFiles files and maxTotalBytes total. An oversized single file is skipped before
// its blob is read. A missing directory is a valid empty source (no error).
//
// These caps run AFTER the pack has been decoded into the storer, so they are NOT the
// OOM defense against a hostile tip — that is the transport's maxCloneWireBytes wire cap
// (see its doc for the residual inflate-bomb note). These caps bound what is handed to
// ParseSet.
func readRoleFiles(commit *object.Commit, dir string) ([]SourceFile, error) {
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	d := dir
	explicit := strings.TrimSpace(d) != ""
	if !explicit {
		d = sourceAgentsDir
	}
	sub, err := tree.Tree(d)
	if err != nil {
		if errors.Is(err, object.ErrDirectoryNotFound) || errors.Is(err, object.ErrEntryNotFound) {
			// An explicitly-configured folder that is absent is almost always a typo
			// (e.g. "product-agent" for "product-agents"). Returning an empty set there
			// would let Reconcile stage a diff marking every previously-synced role as
			// removed, with the sync status still "ok" and no signal to the admin that
			// the path is wrong. Only the default (unset) folder is a valid empty source.
			if explicit {
				return nil, fmt.Errorf("agentsource: configured source folder %q not found in the source repo", d)
			}
			return nil, nil
		}
		return nil, err
	}

	var files []SourceFile
	total := 0
	for i := range sub.Entries {
		if len(files) >= MaxFiles {
			break
		}
		e := sub.Entries[i]
		// Regular files only — skip directories, symlinks (a symlink out of the tree
		// is never followed) and submodules.
		if e.Mode != filemode.Regular && e.Mode != filemode.Executable {
			continue
		}
		if !strings.HasSuffix(e.Name, ".md") {
			continue
		}
		f, ferr := sub.TreeEntryFile(&e)
		if ferr != nil {
			return nil, ferr
		}
		// Skip an oversized file WITHOUT reading its blob (ParseSet would only mark it
		// too_large anyway); this is the OOM guard on a hostile single file.
		if f.Size > MaxBytes {
			continue
		}
		if total+int(f.Size) > maxTotalBytes {
			break
		}
		content, cerr := f.Contents()
		if cerr != nil {
			return nil, cerr
		}
		total += len(content)
		files = append(files, SourceFile{Name: e.Name, Data: []byte(content)})
	}
	return files, nil
}

// scrubber returns a function that redacts the clone token from any string (a
// defensive PAT scrub over go-git error text). An empty token yields an identity
// function. The URL cannot carry userinfo (rejected at write time), but the token is
// still stripped from every error surfaced to logs/status.
func scrubber(token string) func(string) string {
	if token == "" {
		return func(s string) string { return s }
	}
	return func(s string) string {
		return strings.ReplaceAll(s, token, "***REDACTED***")
	}
}
