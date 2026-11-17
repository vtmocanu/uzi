package issuetrackersvc

// Live-DB tests of the removal markers (PRD #4173 D13, migration 00467): a repo that was ever
// enabled stays unlinkable after it is removed and listed again, and a repo that was linked
// before its removal can never be enabled as a forge-issue repo again. They need
// UZI_TEST_DATABASE_URL like the rest of link_livedb_test.go.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/provisionsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// removeAndRelist deletes the repo the way the repo-removal route does and lists its project
// again the way the membership listing does, returning the new row.
func (h *liveHarness) removeAndRelist(repo uuid.UUID) store.Repo {
	h.t.Helper()
	ctx := context.Background()
	old := h.repoRow(repo)
	n, err := h.q.DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{ID: repo, UserID: h.user})
	if err != nil {
		h.t.Fatal(err)
	}
	if n != 1 {
		h.t.Fatalf("DeleteRepoForUser affected %d rows, want 1", n)
	}
	r, err := h.q.UpsertRepo(ctx, store.UpsertRepoParams{
		ConnectionID: old.ConnectionID, ForgeProjectID: old.ForgeProjectID,
		PathWithNamespace: old.PathWithNamespace, WebUrl: old.WebUrl,
		DefaultBranch: pgtype.Text{String: "main", Valid: true},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if r.ID == repo {
		h.t.Fatal("the re-listed repo kept the removed row's id")
	}
	return r
}

func TestUsedRepoRemovedAndRelistedIsNotLinkableLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	repo := h.freshRepo()
	h.setEnabled(repo, true)
	h.setEnabled(repo, false)

	r := h.removeAndRelist(repo)
	if r.AwaitingFirstEnable {
		t.Fatal("a re-listed repo that had been enabled is awaiting its first enable again")
	}
	_ = wantKind(t, h.link(r.ID), KindConflict)
	if h.repoRow(r.ID).IssueTrackerConnectionID.Valid {
		t.Fatal("the repo was linked")
	}
}

func TestNeverEnabledRepoRemovedAndRelistedStaysLinkableLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	repo := h.freshRepo()

	r := h.removeAndRelist(repo)
	if !r.AwaitingFirstEnable {
		t.Fatal("a re-listed repo that was never enabled lost its marker")
	}
	if err := h.link(r.ID); err != nil {
		t.Fatalf("link: %v", err)
	}
}

func TestLinkedRepoRemovedAndRelistedCannotBeEnabledLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	ctx := context.Background()
	repo := h.freshRepo()
	if err := h.link(repo); err != nil {
		t.Fatal(err)
	}
	h.setEnabled(repo, true)
	h.setEnabled(repo, false)

	r := h.removeAndRelist(repo)
	if r.AwaitingFirstEnable || r.IssueTrackerConnectionID.Valid {
		t.Fatalf("re-listed row awaiting = %v, linked = %v; want both false", r.AwaitingFirstEnable, r.IssueTrackerConnectionID.Valid)
	}

	_, err := h.provisioner(&labelRecorder{}, &gateGuard{}).EnableRepo(ctx, h.user, r.ID)
	var pe *provisionsvc.Error
	if !errors.As(err, &pe) || pe.Kind != provisionsvc.KindConflict {
		t.Fatalf("EnableRepo err = %v, want a *provisionsvc.Error of kind KindConflict", err)
	}

	_, err = h.pool.Exec(ctx, `UPDATE repos SET enabled = true WHERE id = $1`, r.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "UZ414" {
		t.Fatalf("raw enable err = %v, want SQLSTATE UZ414", err)
	}
	if h.repoRow(r.ID).Enabled {
		t.Fatal("the repo is enabled")
	}
}

// markerCount counts the markers of the harness user for one forge project.
func (h *liveHarness) markerCount(project int64, linkedOnly bool) int {
	h.t.Helper()
	var n int
	err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM repo_removal_markers WHERE user_id = $1 AND forge_project_id = $2 AND (was_linked OR NOT $3)`,
		h.user, project, linkedOnly).Scan(&n)
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// reconnectForge deletes the harness's forge connection through the route's query and adds one
// for the same user, forge type and base URL: a new connection id for the same forge account.
func (h *liveHarness) reconnectForge() {
	h.t.Helper()
	ctx := context.Background()
	var baseURL string
	if err := h.pool.QueryRow(ctx, `SELECT base_url FROM forge_connections WHERE id = $1`, h.forgeConn).Scan(&baseURL); err != nil {
		h.t.Fatal(err)
	}
	n, err := h.q.DeleteForgeConnectionForUser(ctx, store.DeleteForgeConnectionForUserParams{ID: h.forgeConn, UserID: h.user})
	if err != nil || n != 1 {
		h.t.Fatalf("DeleteForgeConnectionForUser = %d, %v; want 1 row", n, err)
	}
	h.forgeConn = uuid.New()
	h.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	        VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, h.forgeConn, h.user, baseURL, []byte{0x1})
}

func (h *liveHarness) relist(old store.Repo) store.Repo {
	h.t.Helper()
	r, err := h.q.UpsertRepo(context.Background(), store.UpsertRepoParams{
		ConnectionID: h.forgeConn, ForgeProjectID: old.ForgeProjectID,
		PathWithNamespace: old.PathWithNamespace, WebUrl: old.WebUrl,
		DefaultBranch: pgtype.Text{String: "main", Valid: true},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func requireConflict(t *testing.T, err error) {
	t.Helper()
	var pe *provisionsvc.Error
	if !errors.As(err, &pe) || pe.Kind != provisionsvc.KindConflict {
		t.Fatalf("err = %v, want a *provisionsvc.Error of kind KindConflict", err)
	}
}

// The enable of a re-listed, formerly linked repo is refused before any label reaches the forge,
// and as a conflict, not as the 502 a failing label call would give.
func TestRelistedLinkedRepoEnableRefusedBeforeLabelsLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	repo := h.freshRepo()
	if err := h.link(repo); err != nil {
		t.Fatal(err)
	}
	h.setEnabled(repo, true)
	h.setEnabled(repo, false)
	r := h.removeAndRelist(repo)

	f := &labelRecorder{}
	_, err := h.provisioner(f, &gateGuard{}).EnableRepo(context.Background(), h.user, r.ID)
	requireConflict(t, err)
	if got := f.calls.Load(); got != 0 {
		t.Fatalf("EnsureLabels was called %d times before the refusal, want 0", got)
	}
	if h.columnCount(r.ID) != 0 || h.repoRow(r.ID).Enabled {
		t.Fatal("the refused enable left board columns or enabled the repo")
	}
}

// A used repo that was never linked can still be enabled after a removal; only linking is refused.
func TestRelistedUsedUnlinkedRepoCanBeEnabledLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	repo := h.freshRepo()
	h.setEnabled(repo, true)
	h.setEnabled(repo, false)
	r := h.removeAndRelist(repo)
	if h.markerCount(r.ForgeProjectID, true) != 0 {
		t.Fatal("an unlinked removal wrote a linked marker")
	}

	f := &labelRecorder{}
	got, err := h.provisioner(f, &gateGuard{}).EnableRepo(context.Background(), h.user, r.ID)
	if err != nil {
		t.Fatalf("EnableRepo: %v", err)
	}
	if !got.Enabled || f.calls.Load() == 0 {
		t.Fatalf("enabled = %v, EnsureLabels calls = %d; want an enabled repo with its labels seeded", got.Enabled, f.calls.Load())
	}
	_ = wantKind(t, h.link(r.ID), KindConflict)
}

func TestForgeConnectionReplacedKeepsUsedRepoUnlinkableLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	repo := h.freshRepo()
	h.setEnabled(repo, true)
	h.setEnabled(repo, false)
	old := h.repoRow(repo)

	h.reconnectForge()
	if h.markerCount(old.ForgeProjectID, false) != 1 {
		t.Fatal("deleting the forge connection wrote no marker for its used repo")
	}
	r := h.relist(old)
	if r.AwaitingFirstEnable {
		t.Fatal("a used repo listed through a re-added forge connection is awaiting its first enable")
	}
	_ = wantKind(t, h.link(r.ID), KindConflict)
}

func TestForgeConnectionReplacedKeepsLinkedRepoSourceLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	repo := h.freshRepo()
	if err := h.link(repo); err != nil {
		t.Fatal(err)
	}
	h.setEnabled(repo, true)
	old := h.repoRow(repo)

	h.reconnectForge()
	r := h.relist(old)
	if r.AwaitingFirstEnable || r.IssueTrackerConnectionID.Valid {
		t.Fatalf("re-listed row awaiting = %v, linked = %v; want both false", r.AwaitingFirstEnable, r.IssueTrackerConnectionID.Valid)
	}
	f := &labelRecorder{}
	_, err := h.provisioner(f, &gateGuard{}).EnableRepo(context.Background(), h.user, r.ID)
	requireConflict(t, err)
	if f.calls.Load() != 0 {
		t.Fatal("labels were seeded for a refused enable")
	}
}

// A never-enabled repo of a deleted connection writes no marker, so it stays linkable.
func TestForgeConnectionReplacedKeepsNeverEnabledRepoLinkableLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	old := h.repoRow(h.freshRepo())
	h.reconnectForge()
	r := h.relist(old)
	if !r.AwaitingFirstEnable {
		t.Fatal("a never-enabled repo lost its first-enable wait through a connection replacement")
	}
	if err := h.link(r.ID); err != nil {
		t.Fatalf("link: %v", err)
	}
}

// A removal that has not committed yet is waited for by a re-listing: the upsert must see the
// marker that removal commits.
func TestRelistWaitsForAnUncommittedRemovalLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	ctx := context.Background()
	repo := h.freshRepo()
	h.setEnabled(repo, true)
	h.setEnabled(repo, false)
	old := h.repoRow(repo)

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if n, err := h.q.WithTx(tx).DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{ID: repo, UserID: h.user}); err != nil || n != 1 {
		t.Fatalf("DeleteRepoForUser = %d, %v", n, err)
	}

	type result struct {
		r   store.Repo
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := h.q.UpsertRepo(ctx, store.UpsertRepoParams{
			ConnectionID: old.ConnectionID, ForgeProjectID: old.ForgeProjectID,
			PathWithNamespace: old.PathWithNamespace, WebUrl: old.WebUrl,
		})
		done <- result{r, err}
	}()
	waitForLockWait(t, h.pool, "INSERT INTO repos")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.r.AwaitingFirstEnable {
		t.Fatal("a re-listing that raced an uncommitted removal inserted the used repo as awaiting its first enable")
	}
	_ = wantKind(t, h.link(res.r.ID), KindConflict)
}

// Deleting a forge connection while one of its repos is removed on its own must not deadlock.
func TestConnectionDeleteRacingARepoDeleteLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	ctx := context.Background()
	for i := range 20 {
		conn := uuid.New()
		h.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		        VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, conn, h.user, "https://race-"+conn.String()+".e2e", []byte{0x1})
		var first uuid.UUID
		for p := range 3 {
			id := uuid.New()
			if p == 0 {
				first = id
			}
			// awaiting_first_enable defaults to false: every one of the three repos is used, so each removal owes a marker.
			h.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
			        VALUES ($1, $2, $3, 'g/r', 'https://forge.e2e/g/r', false)`, id, conn, 5000+p)
		}

		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			_, err := h.q.DeleteForgeConnectionForUser(ctx, store.DeleteForgeConnectionForUserParams{ID: conn, UserID: h.user})
			errs <- err
		})
		wg.Go(func() {
			<-start
			_, err := h.q.DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{ID: first, UserID: h.user})
			errs <- err
		})
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
				t.Fatalf("iteration %d: deadlock between the connection delete and a repo delete: %v", i, err)
			}
			if err != nil {
				t.Fatalf("iteration %d: %v", i, err)
			}
		}
		var markers int
		if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM repo_removal_markers WHERE user_id = $1 AND base_url = $2`,
			h.user, "https://race-"+conn.String()+".e2e").Scan(&markers); err != nil {
			t.Fatal(err)
		}
		if markers != 3 {
			t.Fatalf("iteration %d: %d markers after both deletes, want 3 (one per used repo)", i, markers)
		}
	}
}

// Deleting a user cascades through its forge connections to repos that were used and linked.
func TestDeletingAUserWithUsedAndLinkedReposLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	ctx := context.Background()
	used := h.freshRepo()
	h.setEnabled(used, true)
	linked := h.freshRepo()
	if err := h.link(linked); err != nil {
		t.Fatal(err)
	}
	h.setEnabled(linked, true)
	usedProject, linkedProject := h.repoRow(used).ForgeProjectID, h.repoRow(linked).ForgeProjectID

	h.exec(`DELETE FROM users WHERE id = $1`, h.user)

	var n int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM repos WHERE id IN ($1, $2)`, used, linked).Scan(&n); err != nil || n != 0 {
		t.Fatalf("repo rows after the user delete = %d, err %v", n, err)
	}
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM repo_removal_markers WHERE user_id = $1 AND forge_project_id IN ($2, $3)`,
		h.user, usedProject, linkedProject).Scan(&n); err != nil || n != 2 {
		t.Fatalf("inert markers of the deleted user = %d, err %v; want 2", n, err)
	}
	h.exec(`DELETE FROM repo_removal_markers WHERE user_id = $1`, h.user)
}

func TestDeletingAForgeConnectionWithUsedReposLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	ctx := context.Background()
	repo := h.freshRepo()
	h.setEnabled(repo, true)
	h.setEnabled(repo, false)
	project := h.repoRow(repo).ForgeProjectID

	h.exec(`DELETE FROM forge_connections WHERE id = $1`, h.forgeConn)

	var n int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM repos WHERE id = $1`, repo).Scan(&n); err != nil || n != 0 {
		t.Fatalf("repo rows after the connection delete = %d, err %v", n, err)
	}
	if got := h.markerCount(project, false); got != 1 {
		t.Fatalf("markers of the deleted connection's user and project = %d; want 1", got)
	}
}
