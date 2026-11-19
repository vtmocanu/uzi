package issuetrackersvc

// Live-DB tests of the repo link and the reconnect rule (PRD #4173 M4, D13). They need
// UZI_TEST_DATABASE_URL pointing at a throwaway Postgres and run through
// e2e/run-store-it.sh, which lists this package.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Assembled at runtime, like every token fixture here.
var (
	tokenA = testToken("link", "livedb", "A")
	tokenB = testToken("link", "livedb", "B")
)

// liveDriver reports the account its token stands for and can pause in VerifyToken.
type liveDriver struct {
	fakeDriver
	h *liveHarness
}

func (d *liveDriver) VerifyToken(ctx context.Context) (issuetracker.Account, error) {
	acct := linkBot
	if d.opts.Token == tokenB {
		acct = linkOther
	}
	if hook := d.h.afterVerify.Load(); hook != nil {
		(*hook)()
	}
	return acct, nil
}

type liveHarness struct {
	t         *testing.T
	pool      *pgxpool.Pool
	q         *store.Queries
	svc       *Service
	user      uuid.UUID
	forgeConn uuid.UUID
	conn      uuid.UUID
	projects  []issuetracker.Project
	tags      []issuetracker.Tag
	// afterVerify, when set, runs inside every driver VerifyToken after the account is chosen,
	// so a test can pause a link or reconnect between its tracker check and its transaction.
	afterVerify atomic.Pointer[func()]
	nextProject atomic.Int64
}

func newLiveHarness(t *testing.T) *liveHarness {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i*3 + 5)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	q := store.New(pool)
	h := &liveHarness{t: t, pool: pool, q: q, user: uuid.New(), forgeConn: uuid.New()}
	h.projects = []issuetracker.Project{{ID: "0-1", ShortName: "UZI", Name: "Uzi"}}
	h.tags = []issuetracker.Tag{{ID: "5-1", Name: "uzi-ready", Owner: linkBot, Use: issuetracker.SharingSettings{Groups: []issuetracker.Group{linkAll}}}}
	h.svc = New(q, box, func(u string) bool { return u == testHost },
		func(_ issuetracker.Kind, o issuetracker.Options) (issuetracker.Driver, error) {
			d := &liveDriver{h: h}
			d.opts, d.projects, d.tags = o, h.projects, h.tags
			return d, nil
		}).UseTransactions(pool, func(tx pgx.Tx) Querier { return q.WithTx(tx) })
	h.svc.log = slog.New(slog.NewTextHandler(io.Discard, nil))

	h.exec(`INSERT INTO users (id, email, password_hash, is_admin, vault_mode) VALUES ($1, $2, 'x', false, 'unprotected')`,
		h.user, fmt.Sprintf("itsvc-%s@e2e", h.user))
	t.Cleanup(func() {
		// Repos and connections cascade from the user.
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, h.user); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})
	h.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	        VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, h.forgeConn, h.user, "https://"+h.forgeConn.String()+".e2e", []byte{0x1})
	c, err := h.svc.Connect(ctx, h.user, "youtrack", testHost, tokenA)
	if err != nil {
		t.Fatal(err)
	}
	h.conn = c.ID
	return h
}

func (h *liveHarness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		h.t.Fatalf("exec %q: %v", sql, err)
	}
}

// freshRepo is a repo the way the membership listing creates it: awaiting its first enable.
func (h *liveHarness) freshRepo() uuid.UUID {
	h.t.Helper()
	r, err := h.q.UpsertRepo(context.Background(), store.UpsertRepoParams{
		ConnectionID: h.forgeConn, ForgeProjectID: 1000 + h.nextProject.Add(1),
		PathWithNamespace: "g/r", WebUrl: "https://forge.e2e/g/r", DefaultBranch: pgtype.Text{String: "main", Valid: true},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if !r.AwaitingFirstEnable {
		h.t.Fatal("UpsertRepo did not mark the repo awaiting its first enable")
	}
	return r.ID
}

func (h *liveHarness) setEnabled(repo uuid.UUID, enabled bool) {
	h.t.Helper()
	if _, err := h.q.SetRepoEnabledForUser(context.Background(), store.SetRepoEnabledForUserParams{ID: repo, Enabled: enabled, UserID: h.user}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *liveHarness) link(repo uuid.UUID) error {
	_, err := h.svc.LinkRepo(context.Background(), h.user, repo, h.conn, "0-1", "5-1")
	return err
}

func (h *liveHarness) repoRow(repo uuid.UUID) store.Repo {
	h.t.Helper()
	var r store.Repo
	rows, err := h.pool.Query(context.Background(), `SELECT * FROM repos WHERE id = $1`, repo)
	if err != nil {
		h.t.Fatal(err)
	}
	r, err = pgx.CollectOneRow(rows, pgx.RowToStructByPos[store.Repo])
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *liveHarness) connAccount() string {
	h.t.Helper()
	row, err := h.q.GetIssueTrackerConnectionForUser(context.Background(), store.GetIssueTrackerConnectionForUserParams{ID: h.conn, UserID: h.user})
	if err != nil {
		h.t.Fatal(err)
	}
	return row.AccountID
}

func TestLinkRepoLiveDB(t *testing.T) {
	t.Run("a fresh repo links", func(t *testing.T) {
		h := newLiveHarness(t)
		repo := h.freshRepo()
		if err := h.link(repo); err != nil {
			t.Fatal(err)
		}
		r := h.repoRow(repo)
		if r.IssueTrackerConnectionID.Bytes != h.conn || r.IssueTrackerProjectShort.String != "UZI" || r.EligibilityTagName.String != "uzi-ready" || !r.AwaitingFirstEnable {
			t.Fatalf("row = %+v", r)
		}
	})
	t.Run("relink is refused and changes nothing", func(t *testing.T) {
		h := newLiveHarness(t)
		repo := h.freshRepo()
		if err := h.link(repo); err != nil {
			t.Fatal(err)
		}
		h.tags[0].Name = "renamed"
		_ = wantKind(t, h.link(repo), KindConflict)
		if got := h.repoRow(repo).EligibilityTagName.String; got != "uzi-ready" {
			t.Fatalf("tag name after a refused relink = %q", got)
		}
	})
	t.Run("a repo that predates the marker is refused", func(t *testing.T) {
		h := newLiveHarness(t)
		repo := uuid.New()
		// Raw insert, the way every row that existed at the migration looks: not awaiting.
		h.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch)
		        VALUES ($1, $2, 7, 'g/old', 'https://forge.e2e/g/old', 'main')`, repo, h.forgeConn)
		_ = wantKind(t, h.link(repo), KindConflict)
		if h.repoRow(repo).IssueTrackerConnectionID.Valid {
			t.Fatal("the repo was linked")
		}
	})
	t.Run("a repo enabled since is refused", func(t *testing.T) {
		h := newLiveHarness(t)
		repo := h.freshRepo()
		h.setEnabled(repo, true)
		_ = wantKind(t, h.link(repo), KindConflict)
	})
	t.Run("a repo enabled and disabled again is refused", func(t *testing.T) {
		h := newLiveHarness(t)
		repo := h.freshRepo()
		h.setEnabled(repo, true)
		h.setEnabled(repo, false)
		if h.repoRow(repo).AwaitingFirstEnable {
			t.Fatal("disabling put the marker back")
		}
		_ = wantKind(t, h.link(repo), KindConflict)
	})
	t.Run("a repo of another user is not found", func(t *testing.T) {
		h := newLiveHarness(t)
		repo := h.freshRepo()
		_, err := h.svc.LinkRepo(context.Background(), uuid.New(), repo, h.conn, "0-1", "5-1")
		_ = wantKind(t, err, KindNotFound)
	})
}

func TestConnectionWithALinkedRepoLiveDB(t *testing.T) {
	ctx := context.Background()
	h := newLiveHarness(t)
	repo := h.freshRepo()
	if err := h.link(repo); err != nil {
		t.Fatal(err)
	}

	_ = wantKind(t, h.svc.Delete(ctx, h.user, h.conn), KindConflict)
	// The database refuses it too, which is what a delete racing the link meets.
	_, err := h.q.DeleteIssueTrackerConnectionForUser(ctx, store.DeleteIssueTrackerConnectionForUserParams{ID: h.conn, UserID: h.user})
	if !isForeignKeyViolation(err) {
		t.Fatalf("raw delete err = %v, want a foreign key violation", err)
	}
	if got := h.svc.txError("x", err); got.Kind != KindConflict {
		t.Fatalf("a foreign key violation maps to kind %d, want Conflict", got.Kind)
	}

	_, err = h.svc.Connect(ctx, h.user, "youtrack", testHost, tokenB)
	if e := wantKind(t, err, KindConflict); !strings.Contains(e.Message, "different account") {
		t.Errorf("message %q", e.Message)
	}
	if h.connAccount() != linkBot.ID {
		t.Fatal("a refused reconnect changed the account")
	}
	// The same account may rotate its token.
	if _, err := h.svc.Connect(ctx, h.user, "youtrack", testHost, tokenA+"-rotated"); err != nil {
		t.Fatalf("same-account reconnect: %v", err)
	}
}

func TestReconnectWithAnotherAccountWhenUnlinkedLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	if _, err := h.svc.Connect(context.Background(), h.user, "youtrack", testHost, tokenB); err != nil {
		t.Fatal(err)
	}
	if h.connAccount() != linkOther.ID {
		t.Fatal("the account did not change")
	}
	if err := h.svc.Delete(context.Background(), h.user, h.conn); err != nil {
		t.Fatalf("an unlinked connection deletes: %v", err)
	}
}

// Link racing the first enable: whichever statement takes the repo row lock first decides, and
// each iteration ends in exactly one of the two D13 outcomes.
func TestLinkRacingFirstEnableLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	ctx := context.Background()
	linkedFirst, enabledFirst := 0, 0
	for iter := range 30 {
		repo := h.freshRepo()
		var linkErr, enableErr error
		var ready, done sync.WaitGroup
		start := make(chan struct{})
		run := func(fn func()) {
			ready.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				ready.Done()
				<-start
				fn()
			}()
		}
		// Odd iterations release the enable when the link's tracker check has finished, so it
		// lands in the window of the link's own transaction; even ones start both together.
		checked := make(chan struct{})
		var once sync.Once
		hook := func() { once.Do(func() { close(checked) }) }
		h.afterVerify.Store(&hook)
		run(func() { linkErr = h.link(repo) })
		run(func() {
			if iter%2 == 1 {
				<-checked
			}
			_, enableErr = h.q.SetRepoEnabledForUser(ctx, store.SetRepoEnabledForUserParams{ID: repo, Enabled: true, UserID: h.user})
		})
		ready.Wait()
		close(start)
		done.Wait()

		if enableErr != nil {
			t.Fatalf("iteration %d: enable: %v", iter, enableErr)
		}
		r := h.repoRow(repo)
		if !r.Enabled || r.AwaitingFirstEnable {
			t.Fatalf("iteration %d: repo not enabled with the marker cleared: %+v", iter, r)
		}
		switch {
		case linkErr == nil && r.IssueTrackerConnectionID.Valid:
			linkedFirst++ // the link committed first and the enable then enabled a linked repo
		case linkErr != nil && !r.IssueTrackerConnectionID.Valid:
			_ = wantKind(t, linkErr, KindConflict) // the enable committed first and the link was refused
			enabledFirst++
		default:
			t.Fatalf("iteration %d: link err = %v but linked = %v", iter, linkErr, r.IssueTrackerConnectionID.Valid)
		}
	}
	t.Logf("link first %d, enable first %d", linkedFirst, enabledFirst)
}

// waitForLockWait polls until a backend is blocked on a lock in a statement containing needle.
func waitForLockWait(t *testing.T, pool *pgxpool.Pool, needle string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%' AND pid <> pg_backend_pid()`, needle).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no backend waited on a lock in a statement containing %q", needle)
}

// The deterministic halves of the race: the repo row lock is held by one transaction, the link
// waits behind it, and then sees whatever that transaction committed.
func TestLinkWaitsForTheRepoRowLockLiveDB(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		hold     func(t *testing.T, h *liveHarness, tx pgx.Tx, repo uuid.UUID)
		wantLink bool
	}{
		{"the enable commits first: the link is refused", func(t *testing.T, h *liveHarness, tx pgx.Tx, repo uuid.UUID) {
			if _, err := h.q.WithTx(tx).SetRepoEnabledForUser(ctx, store.SetRepoEnabledForUserParams{ID: repo, Enabled: true, UserID: h.user}); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"the holder changes nothing: the link proceeds", func(*testing.T, *liveHarness, pgx.Tx, uuid.UUID) {}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLiveHarness(t)
			repo := h.freshRepo()
			tx, err := h.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
			if _, err := h.q.WithTx(tx).LockRepoForLinkForUser(ctx, store.LockRepoForLinkForUserParams{ID: repo, UserID: h.user}); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- h.link(repo) }()

			waitForLockWait(t, h.pool, "FOR UPDATE OF r")
			select {
			case err := <-result:
				t.Fatalf("the link finished (%v) while the repo row lock was held", err)
			default:
			}
			tc.hold(t, h, tx, repo)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = <-result
			if tc.wantLink {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				_ = wantKind(t, err, KindConflict)
			}
			if got := h.repoRow(repo).IssueTrackerConnectionID.Valid; got != tc.wantLink {
				t.Fatalf("linked = %v, want %v", got, tc.wantLink)
			}
		})
	}
}

// A reconnect that changes the account takes the connection row FOR UPDATE; the link's
// transaction holds it FOR SHARE. A link whose tracker check saw the old account is refused when
// the reconnect committed in between, and a reconnect that starts after a link committed is refused.
func TestLinkRacingAReconnectLiveDB(t *testing.T) {
	ctx := context.Background()

	t.Run("the reconnect commits between the link's check and its transaction", func(t *testing.T) {
		h := newLiveHarness(t)
		repo := h.freshRepo()
		verified, release := make(chan struct{}), make(chan struct{})
		hook := func() {
			select {
			case <-verified:
			default:
				close(verified)
				<-release
			}
		}
		h.afterVerify.Store(&hook)
		result := make(chan error, 1)
		go func() { result <- h.link(repo) }()
		<-verified // the link has verified the old account and is paused

		noPause := func() {}
		h.afterVerify.Store(&noPause)
		if _, err := h.svc.Connect(ctx, h.user, "youtrack", testHost, tokenB); err != nil {
			t.Fatalf("reconnect with nothing linked: %v", err)
		}
		close(release)
		e := wantKind(t, <-result, KindConflict)
		if !strings.Contains(e.Message, "reconnect") {
			t.Errorf("message %q", e.Message)
		}
		if h.repoRow(repo).IssueTrackerConnectionID.Valid || h.connAccount() != linkOther.ID {
			t.Fatal("expected an unlinked repo and the new account")
		}
	})

	t.Run("the link commits while the reconnect waits for the connection row", func(t *testing.T) {
		h := newLiveHarness(t)
		repo := h.freshRepo()
		tx, err := h.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
		qtx := h.q.WithTx(tx)
		// The link's own steps, held open: the connection FOR SHARE, then the link write.
		if _, err := qtx.LockIssueTrackerConnectionForShareForUser(ctx, store.LockIssueTrackerConnectionForShareForUserParams{ID: h.conn, UserID: h.user}); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := h.svc.Connect(ctx, h.user, "youtrack", testHost, tokenB)
			result <- err
		}()
		waitForLockWait(t, h.pool, "FOR UPDATE")
		if _, err := qtx.SetRepoIssueTrackerLink(ctx, store.SetRepoIssueTrackerLinkParams{
			ConnectionID: h.conn, ProjectID: "0-1", ProjectShort: "UZI", TagID: "5-1", TagName: "uzi-ready", RepoID: repo, UserID: h.user,
		}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		_ = wantKind(t, <-result, KindConflict)
		if h.connAccount() != linkBot.ID || !h.repoRow(repo).IssueTrackerConnectionID.Valid {
			t.Fatal("expected a linked repo and the unchanged account")
		}
	})

	// Released together, 25 times: the final state is always one of the consistent ones. A
	// reconnect that began after the link's tracker check but committed first refuses the link; a
	// link that committed first refuses the reconnect; a reconnect that finished before the link's
	// check was read makes the link verify the new account, which is then the stored one.
	t.Run("released together", func(t *testing.T) {
		h := newLiveHarness(t)
		var refusedLink, refusedReconnect, both int
		for iter := range 25 {
			// Reset to the starting state: nothing linked, the connection stands for the bot.
			h.exec(`UPDATE repos SET issue_tracker_connection_id=NULL, issue_tracker_project_id=NULL, issue_tracker_project_short=NULL,
			        eligibility_tag_id=NULL, eligibility_tag_name=NULL, issue_tracker_linked_at=NULL WHERE issue_tracker_connection_id = $1`, h.conn)
			if _, err := h.svc.Connect(ctx, h.user, "youtrack", testHost, tokenA); err != nil {
				t.Fatalf("iteration %d: reset: %v", iter, err)
			}
			repo := h.freshRepo()
			var linkErr, connErr error
			var ready, done sync.WaitGroup
			start := make(chan struct{})
			run := func(fn func()) {
				ready.Add(1)
				done.Add(1)
				go func() {
					defer done.Done()
					ready.Done()
					<-start
					fn()
				}()
			}
			run(func() { linkErr = h.link(repo) })
			run(func() { _, connErr = h.svc.Connect(ctx, h.user, "youtrack", testHost, tokenB) })
			ready.Wait()
			close(start)
			done.Wait()

			linked := h.repoRow(repo).IssueTrackerConnectionID.Valid
			acct := h.connAccount()
			switch {
			case linkErr == nil && connErr != nil: // the link won
				_ = wantKind(t, connErr, KindConflict)
				if !linked || acct != linkBot.ID {
					t.Fatalf("iteration %d: link won but linked=%v account=%s", iter, linked, acct)
				}
				refusedReconnect++
			case linkErr != nil && connErr == nil: // the reconnect won
				_ = wantKind(t, linkErr, KindConflict)
				if linked || acct != linkOther.ID {
					t.Fatalf("iteration %d: reconnect won but linked=%v account=%s", iter, linked, acct)
				}
				refusedLink++
			case linkErr == nil && connErr == nil: // the reconnect finished before the link looked
				if !linked || acct != linkOther.ID {
					t.Fatalf("iteration %d: both succeeded but linked=%v account=%s", iter, linked, acct)
				}
				both++
			default:
				t.Fatalf("iteration %d: both failed: link %v, reconnect %v", iter, linkErr, connErr)
			}
		}
		t.Logf("link won %d, reconnect won %d, reconnect then link %d", refusedReconnect, refusedLink, both)
	})
}

// The link reads the connection FOR SHARE. A reconnect holds the row FOR UPDATE; the link
// must wait for it and then see the new account. A plain read would not wait and would link
// against the account it verified.
func TestLinkWaitsForAReconnectHoldingTheConnectionRowLiveDB(t *testing.T) {
	ctx := context.Background()
	h := newLiveHarness(t)
	repo := h.freshRepo()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if _, err := h.q.WithTx(tx).LockIssueTrackerConnectionByIdentityForUpdate(ctx, store.LockIssueTrackerConnectionByIdentityForUpdateParams{
		UserID: h.user, Kind: "youtrack", BaseUrl: testHost,
	}); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- h.link(repo) }()

	waitForLockWait(t, h.pool, "FOR SHARE")
	select {
	case err := <-result:
		t.Fatalf("the link finished (%v) while the connection row was locked FOR UPDATE", err)
	default:
	}
	if _, err := tx.Exec(ctx, `UPDATE issue_tracker_connections SET account_id = $2 WHERE id = $1`, h.conn, linkOther.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	e := wantKind(t, <-result, KindConflict)
	if !strings.Contains(e.Message, "reconnect") {
		t.Errorf("message %q", e.Message)
	}
	if h.repoRow(repo).IssueTrackerConnectionID.Valid {
		t.Fatal("the repo was linked")
	}
}

// blindQuerier hides the locked pre-check from Connect: the identity lock finds no row and the
// link count is zero, as when a link commits between those reads and the upsert.
type blindQuerier struct{ *store.Queries }

func (blindQuerier) LockIssueTrackerConnectionByIdentityForUpdate(context.Context, store.LockIssueTrackerConnectionByIdentityForUpdateParams) (store.IssueTrackerConnection, error) {
	return store.IssueTrackerConnection{}, pgx.ErrNoRows
}

// The upsert's WHERE is a last defence for the reconnect rule: with the locked pre-check
// blinded and no transactions wired, it still refuses when the link was committed before the
// statement began. (It cannot see a link that commits during its own wait; see
// TestFirstConnectRacingAnotherConnectAndALinkLiveDB.)
func TestUpsertWhereRefusesAnAccountChangeOnALinkedConnectionLiveDB(t *testing.T) {
	ctx := context.Background()
	h := newLiveHarness(t)
	bare := New(blindQuerier{h.q}, h.svc.box, h.svc.allowed, h.svc.newDriver)
	bare.log = h.svc.log
	if err := h.link(h.freshRepo()); err != nil {
		t.Fatal(err)
	}
	_, err := bare.Connect(ctx, h.user, "youtrack", testHost, tokenB)
	if e := wantKind(t, err, KindConflict); !strings.Contains(e.Message, "different account") {
		t.Errorf("message %q", e.Message)
	}
	if h.connAccount() != linkBot.ID {
		t.Fatal("the account changed")
	}
	if _, err := bare.Connect(ctx, h.user, "youtrack", testHost, testToken("rotated")); err != nil {
		t.Fatalf("same-account rotation: %v", err)
	}
}

// gateQuerier pauses the first identity lock after it has found no row, so a test can
// interleave other connects and a link between that read and the statement that follows.
type gateQuerier struct {
	*store.Queries
	once    *sync.Once
	locked  chan struct{}
	release chan struct{}
}

func (g gateQuerier) LockIssueTrackerConnectionByIdentityForUpdate(ctx context.Context, a store.LockIssueTrackerConnectionByIdentityForUpdateParams) (store.IssueTrackerConnection, error) {
	row, err := g.Queries.LockIssueTrackerConnectionByIdentityForUpdate(ctx, a)
	g.once.Do(func() {
		close(g.locked)
		<-g.release
	})
	return row, err
}

// T1 is a first connect (account A) whose identity lock found no row. T2, another connect of
// the same identity (account B), inserts and commits. T3 links a repo to that row and has not
// committed yet. T1 must not overwrite the row's account once T3 commits: with a plain upsert
// its NOT EXISTS subquery reads the snapshot from before it waited on T3 and does not see the
// link. The fix inserts with DO NOTHING and re-enters the locked path (lock, then a fresh count).
func TestFirstConnectRacingAnotherConnectAndALinkLiveDB(t *testing.T) {
	ctx := context.Background()
	h := newLiveHarness(t)

	// A second user with no connection yet, so T1's identity lock finds no row.
	user := uuid.New()
	h.exec(`INSERT INTO users (id, email, password_hash, is_admin, vault_mode) VALUES ($1, $2, 'x', false, 'unprotected')`,
		user, fmt.Sprintf("itsvc-%s@e2e", user))
	t.Cleanup(func() { _, _ = h.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user) })
	forgeConn := uuid.New()
	h.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	        VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, forgeConn, user, "https://"+forgeConn.String()+".e2e", []byte{0x1})
	repo, err := h.q.UpsertRepo(ctx, store.UpsertRepoParams{ConnectionID: forgeConn, ForgeProjectID: 4242, PathWithNamespace: "g/r", WebUrl: "https://forge.e2e/g/r"})
	if err != nil {
		t.Fatal(err)
	}

	g := gateQuerier{Queries: h.q, once: new(sync.Once), locked: make(chan struct{}), release: make(chan struct{})}
	t1 := New(g, h.svc.box, h.svc.allowed, h.svc.newDriver).
		UseTransactions(h.pool, func(tx pgx.Tx) Querier {
			return gateQuerier{Queries: h.q.WithTx(tx), once: g.once, locked: g.locked, release: g.release}
		})
	t1.log = h.svc.log
	result := make(chan error, 1)
	go func() {
		_, err := t1.Connect(ctx, user, "youtrack", testHost, tokenA) // account A (the bot)
		result <- err
	}()
	<-g.locked // T1 looked, found no row, and is paused

	c2, err := h.svc.Connect(ctx, user, "youtrack", testHost, tokenB) // T2: account B, committed
	if err != nil {
		t.Fatal(err)
	}
	tx, err := h.pool.Begin(ctx) // T3: the link, held open
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	qtx := h.q.WithTx(tx)
	if _, err := qtx.LockIssueTrackerConnectionForShareForUser(ctx, store.LockIssueTrackerConnectionForShareForUserParams{ID: c2.ID, UserID: user}); err != nil {
		t.Fatal(err)
	}
	if _, err := qtx.SetRepoIssueTrackerLink(ctx, store.SetRepoIssueTrackerLinkParams{
		ConnectionID: c2.ID, ProjectID: "0-1", ProjectShort: "UZI", TagID: "5-1", TagName: "uzi-ready", RepoID: repo.ID, UserID: user,
	}); err != nil {
		t.Fatal(err)
	}

	close(g.release) // T1 goes on and must now wait on T3's row lock
	// Whichever statement T1 runs next (the lock, or an upsert) waits on the connection row.
	waitForLockWait(t, h.pool, "issue_tracker_connections")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	e := wantKind(t, <-result, KindConflict)
	if !strings.Contains(e.Message, "different account") {
		t.Errorf("message %q", e.Message)
	}
	row, err := h.q.GetIssueTrackerConnectionForUser(ctx, store.GetIssueTrackerConnectionForUserParams{ID: c2.ID, UserID: user})
	if err != nil {
		t.Fatal(err)
	}
	if row.AccountID != linkOther.ID {
		t.Fatalf("the linked connection's account changed to %q", row.AccountID)
	}
}
