package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// MaxFindingsPerRun caps how many incidental findings a single run may capture (PRD
// #333 D11, mirroring MaxPendingProposalsPerRun). Past the cap the tool gets a soft
// error ("finding cap reached for this run") and the agent keeps working — a noisy or
// prompt-injected run cannot flood the per-repo backlog.
const MaxFindingsPerRun = 10

// Finding field caps (M2). title mirrors the proposal/forge issue-title cap;
// description reuses MaxIssueDescriptionBytes; location and confidence are short and
// bounded so a runaway tool call cannot store an unbounded blob. These bound the
// SANITISED text (sanitize+cap runs before the store sees anything).
const (
	MaxFindingTitleBytes      = MaxProposalTitleBytes
	MaxFindingLocationBytes   = 512
	MaxFindingConfidenceBytes = 32
)

// Finding sentinel errors, mapped to HTTP status by the handler.
var (
	// ErrFindingCapReached rejects a capture once the run already holds
	// MaxFindingsPerRun evidence rows (D11) → 429.
	ErrFindingCapReached = errors.New("finding cap reached for this run")
	// ErrFindingRepoRequired rejects a finding on a repo-less run (a chat run, or a
	// run whose repo_id is NULL): a finding's coordinate is (user_id, repo_id,
	// location), so a repo is structurally required (D3). Autonomous lanes always have
	// one, so this is defense-in-depth → 409.
	ErrFindingRepoRequired = errors.New("run has no repository; a finding needs one")
	// ErrFindingLocationInvalid rejects a location that canonicalises to empty (all
	// whitespace/punctuation) — it would be a meaningless coordinate → 400.
	ErrFindingLocationInvalid = errors.New("finding location is empty after canonicalisation")
)

// CreateFindingRequest is the already-cap-validated (raw byte bounds checked by the
// handler) payload of a capture. The service sanitises + canonicalises every text field
// before it reaches the store.
type CreateFindingRequest struct {
	Title       string
	Description string
	Location    string
	Labels      []string
	Confidence  string
}

// CreateFinding records one incidental finding for a run (PRD #333 M2). The worker
// NEVER sends a user/repo id: the service derives (user_id, repo_id) from the CLAIMED
// run (GetRunByIDForUser), exactly like every other worker write (D2/D3). It sanitises
// the untrusted agent text to inert form, canonicalises `location` to the D3 fixed
// coordinate, computes the D3 content_hash, inserts the per-run evidence row, and
// upserts the coordinate's `open` disposition — re-opening a resolved coordinate only
// when the content materially changed, and never resurrecting a matching-hash
// filed/dismissed one (the anti-nag ordering, R2). It NEVER writes the forge — filing
// is human-gated later (D4). Returns the created evidence row.
func (s *Service) CreateFinding(ctx context.Context, wkr store.Worker, runID uuid.UUID, req CreateFindingRequest) (store.IncidentalFinding, error) {
	// (1) Derive (user_id, repo_id) from the claimed run — never a client-sent id.
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: wkr.UserID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.IncidentalFinding{}, ErrRunNotFound
		}
		return store.IncidentalFinding{}, err
	}
	if !run.RepoID.Valid {
		return store.IncidentalFinding{}, ErrFindingRepoRequired
	}
	// A finding comes from an ACTIVE worker; a terminal run cannot be reporting one.
	if terminalStatuses[run.Status] {
		return store.IncidentalFinding{}, ErrRunTerminal
	}
	repoID := uuid.UUID(run.RepoID.Bytes)
	userID := run.UserID

	// (2) Per-run capture cap (D11).
	count, err := s.q.CountFindingsForRun(ctx, runID)
	if err != nil {
		return store.IncidentalFinding{}, err
	}
	if count >= MaxFindingsPerRun {
		return store.IncidentalFinding{}, ErrFindingCapReached
	}

	// (3) Ingest hygiene (D4): sanitise the untrusted self-reported text to INERT form
	// (control/Cf/ANSI/bidi strip + secret scrub + byte cap) BEFORE storing — the same
	// ingest sanitiser the judge worker / report_only path use. The issue-template FIELD
	// sanitisers (SanitizeTitle/FenceBlock/SafeInlineCode) are applied later at file-time
	// (M5), never here.
	title := sanitizeFindingText(req.Title, MaxFindingTitleBytes)
	description := sanitizeFindingText(req.Description, MaxIssueDescriptionBytes)

	// (4) Canonicalise `location` to the D3 fixed form AFTER sanitising (so the strip has
	// already run), matching the judge's canonicalizeTarget(ScrubSecrets(sanitize(...)))
	// ordering. A location that canonicalises to empty is a meaningless coordinate.
	location := canonicalizeLocation(sanitizeFindingText(req.Location, MaxFindingLocationBytes), MaxFindingLocationBytes)
	if location == "" {
		return store.IncidentalFinding{}, ErrFindingLocationInvalid
	}

	confidence := sanitizeFindingText(req.Confidence, MaxFindingConfidenceBytes)

	// (5) The D3 re-open discriminator: sha256 of a deterministic normalisation of
	// title+description. See findingContentHash for the normalisation.
	contentHash := findingContentHash(title, description)

	labelsJSON, err := marshalFindingLabels(req.Labels)
	if err != nil {
		return store.IncidentalFinding{}, err
	}

	// (6) Insert the per-run evidence row (already-canonical location + inert text).
	finding, err := s.q.InsertFinding(ctx, store.InsertFindingParams{
		RunID:         runID,
		UserID:        userID,
		RepoID:        repoID,
		Location:      location,
		Title:         title,
		DescriptionMd: description,
		Labels:        labelsJSON,
		Confidence:    confidence,
	})
	if err != nil {
		return store.IncidentalFinding{}, err
	}

	// (7) Claim the coordinate as `open` on the FIRST report (ON CONFLICT DO NOTHING).
	// An actual insert returns the row; a conflict (a row already exists at this
	// coordinate) returns pgx.ErrNoRows — that IS the did-I-insert signal.
	_, err = s.q.UpsertOpenDisposition(ctx, store.UpsertOpenDispositionParams{
		UserID:      userID,
		RepoID:      repoID,
		Location:    location,
		ContentHash: contentHash,
		LastTitle:   title,
	})
	switch {
	case err == nil:
		// A brand-new `open` coordinate was created.
	case errors.Is(err, pgx.ErrNoRows):
		// The coordinate already exists. Order matters for the anti-nag guarantee (R2):
		// try the guarded re-open FIRST — it re-opens ONLY a filed/dismissed row whose
		// content_hash DIFFERS. An identical-hash report on a filed/dismissed coordinate
		// matches zero rows there (stays resolved — a dismissed bug stays gone). If it did
		// not re-open, refresh an already-`open` row's last_title/hash so the backlog shows
		// the latest title (that UPDATE only touches status='open', so it is a no-op on a
		// resolved coordinate).
		reopened, rerr := s.q.ReopenDispositionOnHashMismatch(ctx, store.ReopenDispositionOnHashMismatchParams{
			ContentHash: contentHash,
			LastTitle:   title,
			UserID:      userID,
			RepoID:      repoID,
			Location:    location,
		})
		if rerr != nil {
			return store.IncidentalFinding{}, rerr
		}
		if reopened == 0 {
			if _, uerr := s.q.UpdateDispositionLastTitle(ctx, store.UpdateDispositionLastTitleParams{
				LastTitle:   title,
				ContentHash: contentHash,
				UserID:      userID,
				RepoID:      repoID,
				Location:    location,
			}); uerr != nil {
				return store.IncidentalFinding{}, uerr
			}
		}
	default:
		return store.IncidentalFinding{}, err
	}

	// (8) M3: enqueue coalesced notification here (inbox + one Slack DM per run, D6),
	// suppressed on a resolved matching coordinate. M3 owns that plumbing; M2 stops at the
	// stored evidence + open coordinate and returns without notifying.
	return finding, nil
}

// marshalFindingLabels JSON-encodes the label set for the evidence row, normalising a
// nil slice to the empty array (matching the proposal path). The handler has already
// bounded the count and each label's length.
func marshalFindingLabels(labels []string) ([]byte, error) {
	if labels == nil {
		labels = []string{}
	}
	return json.Marshal(labels)
}

// sanitizeFindingText renders one untrusted, agent-authored string INERT for storage
// (D4): strip terminal-control / bidi-override runes, bound the byte length rune-safely,
// then scrub secret shapes. Order matches judge_worker.go / report_only.go
// (ScrubSecrets(SanitizeBounded(...))): sanitise+cap the structural text FIRST so the
// scrubber sees whole runes and the cap applies before redaction rewrites. It is the
// write-side hygiene the store relies on; the field-level issue-template sanitisers run
// later at file-time (M5).
func sanitizeFindingText(s string, max int) string {
	return secretscrub.Scrub(termsafe.SanitizeBounded(s, max))
}

// findingContentHash is the D3 re-open discriminator: the sha256 (hex) of a
// deterministic normalisation of title+description. NORMALISATION: each of title and
// description is Unicode-lowercased and has every run of whitespace collapsed to a
// single space (strings.Fields + Join), then the two are joined with a '\n' separator
// so a word moving from the end of the title to the start of the description cannot
// collide two different findings. This hash is computed ONLY in Go (never compared to a
// DB expression), so Unicode-aware lowercasing is safe here — unlike canonicalizeTarget,
// which must byte-match a Postgres backfill.
func findingContentHash(title, description string) string {
	sum := sha256.Sum256([]byte(normalizeForHash(title) + "\n" + normalizeForHash(description)))
	return hex.EncodeToString(sum[:])
}

func normalizeForHash(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// findingLineNumRe matches a trailing line reference (`:12`, `:12:5`) the coordinate
// must exclude — line numbers drift and would defeat dedup (D3).
var findingLineNumRe = regexp.MustCompile(`(:[0-9]+)+$`)

// canonicalizeLocation folds an agent-supplied `location` to the D3 fixed coordinate so
// the same file#symbol phrased with cosmetic drift collapses to ONE coordinate. Modelled
// on canonicalizeTarget (handler/worker_protocol.go) but implementing the path+#symbol
// rules: repo-root-relative, forward slashes, leading `./` dropped, ASCII-lowercased,
// whitespace stripped, trailing line numbers removed, and at most ONE normalised symbol
// token after a single `#`. A missing symbol matches only other symbol-less reports for
// that path (the D3 default). Pure + unit-testable. `max` re-bounds rune-safely.
//
// The three D3 variants "api/internal/Sweep.go ", "./api/internal/sweep.go" and
// "api/internal/sweep.go" all collapse to "api/internal/sweep.go".
//
// `max` is the byte-bound contract (both callers pass MaxFindingLocationBytes today),
// kept as a parameter so the fold is testable at other bounds and uniform with the
// sibling canonicalizeTarget — same reason that function suppresses unparam here.
//
//nolint:unparam // max is the byte-bound contract, uniform with canonicalizeTarget
func canonicalizeLocation(s string, max int) string {
	s = strings.ReplaceAll(s, "\\", "/") // normalise Windows-style separators first

	path := s
	symbol := ""
	// Split on the FIRST '#': everything after it is the symbol token (at most one).
	if i := strings.IndexByte(s, '#'); i >= 0 {
		path = s[:i]
		symbol = s[i+1:]
	}

	path = canonicalizeLocationPath(path)
	// A symbol without a file path is not a coordinate (a finding is anchored to a
	// file). An empty path drops the whole location so the caller rejects it.
	if path == "" {
		return ""
	}
	symbol = canonicalizeLocationSymbol(symbol)

	out := path
	if symbol != "" {
		out = path + "#" + symbol
	}

	// Re-apply the byte bound rune-safely (back the cut off to the nearest rune start).
	if len(out) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut]
	}
	return out
}

func canonicalizeLocationPath(p string) string {
	p = asciiLowerASCII(removeAllSpace(p))
	// Drop repeated leading `./` and any leading `/` so the path is repo-root-relative.
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	p = strings.TrimLeft(p, "/")
	// Exclude trailing line numbers (D3): file.go:123 → file.go.
	p = findingLineNumRe.ReplaceAllString(p, "")
	return p
}

func canonicalizeLocationSymbol(sym string) string {
	// At most one token: take the first whitespace-delimited field.
	fields := strings.Fields(sym)
	if len(fields) == 0 {
		return ""
	}
	sym = asciiLowerASCII(removeAllSpace(fields[0]))
	// A purely numeric "symbol" is a line reference, not a symbol name — drop it.
	if sym == "" || isAllDigits(sym) {
		return ""
	}
	return sym
}

// removeAllSpace deletes every Unicode whitespace rune (a path/symbol coordinate holds
// no legitimate whitespace; stripping it entirely is the "whitespace-stripped" rule).
func removeAllSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// asciiLowerASCII lowercases ONLY ASCII 'A'–'Z' (leaving other bytes untouched). Kept
// ASCII-only in the same spirit as canonicalizeTarget — a file coordinate is ASCII by
// construction and Unicode-folding risks over-merge (the unsafe direction, D3/R1).
func asciiLowerASCII(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
