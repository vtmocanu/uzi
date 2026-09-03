package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1034 M3 handler coverage: MoveIssue now lifts the Closed guard and wires
// close/reopen through the forgesvc close/reopen service, forge-first, on a live DB
// behind an httptest GitLab. These are net-new — there was no MoveIssue handler test
// (the old 400-on-"closed" path had none). The assertions are on the WIRE payload the
// handler drove (state_event / add_labels / remove_labels), the rendered card, and the
// cache — not merely status codes.
//
// h.projectSync is left nil so ForwardMove is skipped; the Projects v2 Done-drive is
// covered by the forgesvc ForwardMove unit tests (TestForwardMoveClosed*).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// moveStub is an httptest GitLab that answers the REST calls MoveIssue's close/reopen
// paths make and captures what crossed the wire, distinguishing by request body:
//   - PUT .../issues/:iid with a state_event field → SetIssueState (close/reopen).
//   - PUT .../issues/:iid with add_labels/remove_labels → the AutoMove label move.
//   - GET/POST .../labels → EnsureLabels (list empty, then create).
type moveStub struct {
	mu sync.Mutex
	// stateEvents records each state_event value SetIssueState sent ("close"/"reopen").
	stateEvents []string
	// addLabels / removeLabels are the last AutoMove label move that crossed the wire.
	addLabels    []string
	removeLabels []string
	// failState makes the state_event PUT return 500, to exercise the 502 snap-back.
	failState bool
}

func (s *moveStub) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateEvents = nil
	s.addLabels = nil
	s.removeLabels = nil
	s.failState = false
}

func newMoveServer(t *testing.T, s *moveStub) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/labels"):
			// EnsureLabels lists first; return none so it proceeds to create.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/labels"):
			name, _ := m["name"].(string)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":1,"name":%q}`, name)
		case r.Method == http.MethodPut && strings.Contains(path, "/issues/"):
			if ev, ok := m["state_event"].(string); ok {
				s.mu.Lock()
				s.stateEvents = append(s.stateEvents, ev)
				fail := s.failState
				s.mu.Unlock()
				if fail {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"message":"boom"}`))
					return
				}
				state := "opened"
				if ev == "close" {
					state = "closed"
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":1,"iid":5,"project_id":1,"title":"t","state":%q,"web_url":"https://forge.example/x","labels":[]}`, state)
				return
			}
			// A label move (AutoMove inside reopen).
			s.mu.Lock()
			s.addLabels = splitLabels(m["add_labels"])
			s.removeLabels = splitLabels(m["remove_labels"])
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":1,"iid":5,"project_id":1,"title":"t","state":"opened","web_url":"https://forge.example/x","labels":[]}`)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// moveFixture is a real Handler on a live DB with a forge connection pointing at the
// move stub and a repo the user owns, seeded with a single "Planned" board column so a
// reopen target resolves. h.projectSync is nil.
type moveFixture struct {
	h      *Handler
	pool   *pgxpool.Pool
	user   store.User
	repoID uuid.UUID
}

func newMoveFixture(ctx context.Context, t *testing.T, stub *moveStub) moveFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)
	srv := newMoveServer(t, stub)

	f := moveFixture{
		pool:   pool,
		user:   store.User{ID: uuid.New(), Email: fmt.Sprintf("mv-%s@e2e", uuid.NewString()[:8])},
		repoID: uuid.New(),
	}
	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	connID := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.user.ID, f.user.Email)
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'bot-m', 9, $4)`, connID, f.user.ID, srv.URL, sealed)
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/w', 'https://forge.example/g/w', 'main', true)`, f.repoID, connID)
	mustExecT(ctx, t, pool,
		`INSERT INTO board_columns (repo_id, label_name, position) VALUES ($1, 'Planned', 0)`, f.repoID)

	f.h = &Handler{
		pool:     pool,
		q:        q,
		box:      box,
		cfg:      config.Config{},
		settings: settings.New(&settingsStore{}, time.Minute),
		svc:      forgesvc.New(q, box, 5*time.Second, nil),
		wsvc:     workersvc.New(q, box, workersvc.Params{}),
	}
	return f
}

// moveCard is the shape MoveIssue renders in its {"card": …} response.
type moveCard struct {
	Card struct {
		State  string   `json:"state"`
		Closed bool     `json:"closed"`
		Column string   `json:"column"`
		Labels []string `json:"labels"`
	} `json:"card"`
}

func decodeMoveCard(t *testing.T, rr *httptest.ResponseRecorder) moveCard {
	t.Helper()
	var c moveCard
	if err := json.Unmarshal(rr.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode card: %v; body=%s", err, rr.Body.String())
	}
	return c
}

// issueStateAndPos reads the cache row's state and board_position (the card DTO does not
// expose board_position, so read it raw).
func issueStateAndPos(ctx context.Context, t *testing.T, f moveFixture, iid int64) (string, *int32) {
	t.Helper()
	var state string
	var pos *int32
	if err := f.pool.QueryRow(ctx,
		`SELECT state, board_position FROM issues WHERE repo_id=$1 AND forge_issue_iid=$2`, f.repoID, iid,
	).Scan(&state, &pos); err != nil {
		t.Fatalf("read issue %d: %v", iid, err)
	}
	return state, pos
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestMoveIssueCloseReopenLiveDB(t *testing.T) {
	ctx := context.Background()
	stub := &moveStub{}
	f := newMoveFixture(ctx, t, stub)

	// 1 — drag open→Closed closes: an OPEN issue dragged to Closed is closed on the
	// forge (state_event=close on the wire), rendered in the Closed lane, and flipped
	// to closed in the cache.
	t.Run("open to Closed closes", func(t *testing.T) {
		stub.reset()
		mustExecT(ctx, t, f.pool,
			`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
			 VALUES ($1, 300, 't', 'opened', '[]'::jsonb, 'https://x', false, now(), now())`, f.repoID)

		rr := httptest.NewRecorder()
		f.h.MoveIssue(rr, boardWriterReq(f.user, f.repoID, "300", `{"to_column":"closed"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("move status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		stub.mu.Lock()
		gotEvents := append([]string(nil), stub.stateEvents...)
		stub.mu.Unlock()
		if len(gotEvents) != 1 || gotEvents[0] != "close" {
			t.Fatalf("wire state_event = %v, want exactly [close]", gotEvents)
		}
		card := decodeMoveCard(t, rr)
		if !card.Card.Closed || card.Card.State != "closed" {
			t.Fatalf("card = %+v, want closed=true state=closed", card.Card)
		}
		if state, _ := issueStateAndPos(ctx, t, f, 300); state != "closed" {
			t.Fatalf("cache state = %q, want closed", state)
		}
	})

	// 2 — drag closed→column reopens + moves: a CLOSED issue with no column label and a
	// non-null board_position dragged onto Planned is reopened (state_event=reopen) AND
	// labelled Planned (add_labels), renders open in the Planned column, and its
	// board_position is nulled so it lands at the bottom.
	t.Run("closed to column reopens and moves", func(t *testing.T) {
		stub.reset()
		mustExecT(ctx, t, f.pool,
			`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at, board_position)
			 VALUES ($1, 301, 't', 'closed', '[]'::jsonb, 'https://x', false, now(), now(), 5)`, f.repoID)

		rr := httptest.NewRecorder()
		f.h.MoveIssue(rr, boardWriterReq(f.user, f.repoID, "301", `{"to_column":"Planned"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("move status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		stub.mu.Lock()
		gotEvents := append([]string(nil), stub.stateEvents...)
		gotAdds := append([]string(nil), stub.addLabels...)
		stub.mu.Unlock()
		if len(gotEvents) != 1 || gotEvents[0] != "reopen" {
			t.Fatalf("wire state_event = %v, want exactly [reopen]", gotEvents)
		}
		if !contains(gotAdds, "Planned") {
			t.Fatalf("wire add_labels = %v, want to contain Planned", gotAdds)
		}
		card := decodeMoveCard(t, rr)
		if card.Card.Closed || card.Card.Column != "Planned" {
			t.Fatalf("card = %+v, want closed=false column=Planned", card.Card)
		}
		state, pos := issueStateAndPos(ctx, t, f, 301)
		if state != "opened" {
			t.Fatalf("cache state = %q, want opened", state)
		}
		if pos != nil {
			t.Fatalf("cache board_position = %v, want NULL after reopen", *pos)
		}
	})

	// 3 — drag closed→Backlog reopens + clears: a CLOSED issue carrying the "Planned"
	// column label dragged to Backlog ("open") is reopened (state_event=reopen) AND has
	// Planned removed (remove_labels), rendering open with no column.
	t.Run("closed to Backlog reopens and clears", func(t *testing.T) {
		stub.reset()
		mustExecT(ctx, t, f.pool,
			`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
			 VALUES ($1, 302, 't', 'closed', '["Planned"]'::jsonb, 'https://x', false, now(), now())`, f.repoID)

		rr := httptest.NewRecorder()
		f.h.MoveIssue(rr, boardWriterReq(f.user, f.repoID, "302", `{"to_column":"open"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("move status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		stub.mu.Lock()
		gotEvents := append([]string(nil), stub.stateEvents...)
		gotRemoves := append([]string(nil), stub.removeLabels...)
		stub.mu.Unlock()
		if len(gotEvents) != 1 || gotEvents[0] != "reopen" {
			t.Fatalf("wire state_event = %v, want exactly [reopen]", gotEvents)
		}
		if !contains(gotRemoves, "Planned") {
			t.Fatalf("wire remove_labels = %v, want to contain Planned", gotRemoves)
		}
		card := decodeMoveCard(t, rr)
		if card.Card.Closed || card.Card.Column != "" {
			t.Fatalf("card = %+v, want closed=false column empty", card.Card)
		}
		if contains(card.Card.Labels, "Planned") {
			t.Fatalf("card labels = %v, want Planned cleared", card.Card.Labels)
		}
	})

	// 3b — reopen to an unconfigured column 400s: the close case skips column validation,
	// but a reopen/move to a column that has no board_columns row must still be rejected
	// before any forge call — the guard is `!isClose && target != ""`.
	t.Run("reopen to unconfigured column is rejected", func(t *testing.T) {
		stub.reset()
		mustExecT(ctx, t, f.pool,
			`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
			 VALUES ($1, 304, 't', 'closed', '[]'::jsonb, 'https://x', false, now(), now())`, f.repoID)

		rr := httptest.NewRecorder()
		f.h.MoveIssue(rr, boardWriterReq(f.user, f.repoID, "304", `{"to_column":"Nonexistent"}`))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("move status = %d, want 400 for an unconfigured target column; body=%s", rr.Code, rr.Body.String())
		}
		// The reject must precede any forge mutation and leave the cache closed.
		stub.mu.Lock()
		gotEvents := append([]string(nil), stub.stateEvents...)
		stub.mu.Unlock()
		if len(gotEvents) != 0 {
			t.Fatalf("wire state_event = %v, want none — validation must reject before the forge call", gotEvents)
		}
		if state, _ := issueStateAndPos(ctx, t, f, 304); state != "closed" {
			t.Fatalf("cache state = %q, want still closed after a 400", state)
		}
	})

	// 4 — forge failure → 502, cache untouched: the state_event PUT returns 500, so the
	// close 502s and the cache is left opened (the AutoMove/SetIssueLabel snap-back).
	t.Run("forge failure 502 leaves cache untouched", func(t *testing.T) {
		stub.reset()
		stub.mu.Lock()
		stub.failState = true
		stub.mu.Unlock()
		mustExecT(ctx, t, f.pool,
			`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
			 VALUES ($1, 303, 't', 'opened', '[]'::jsonb, 'https://x', false, now(), now())`, f.repoID)

		rr := httptest.NewRecorder()
		f.h.MoveIssue(rr, boardWriterReq(f.user, f.repoID, "303", `{"to_column":"closed"}`))
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("move status = %d, want 502; body=%s", rr.Code, rr.Body.String())
		}
		if state, _ := issueStateAndPos(ctx, t, f, 303); state != "opened" {
			t.Fatalf("cache state = %q, want still opened after a forge failure", state)
		}
	})
}
