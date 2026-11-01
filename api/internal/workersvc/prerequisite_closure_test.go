package workersvc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const prerequisiteTestTip = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type prerequisiteTestForge struct {
	forgetest.BaseFake
	getProject func(context.Context, int64) (forge.Project, error)
	compare    func(context.Context, int64, string, string) (forge.Ancestry, error)
}

func (f *prerequisiteTestForge) GetProject(ctx context.Context, id int64) (forge.Project, error) {
	if f.getProject == nil {
		return f.BaseFake.GetProject(ctx, id)
	}
	return f.getProject(ctx, id)
}

func prerequisiteProjectStub(t *testing.T, m *prerequisiteMemory) func(context.Context, int64) (forge.Project, error) {
	t.Helper()
	return func(ctx context.Context, id int64) (forge.Project, error) {
		if m.inTX.Load() {
			t.Error("project lookup in transaction")
		}
		if id != m.repo.ForgeProjectID {
			t.Errorf("lookup ID=%d want=%d", id, m.repo.ForgeProjectID)
		}
		u := strings.TrimPrefix(strings.TrimPrefix(m.repo.WebUrl, m.conn.BaseUrl), "/")
		return forge.Project{ForgeProjectID: id, PathWithNamespace: strings.TrimSuffix(u, "/"), WebURL: m.repo.WebUrl}, ctx.Err()
	}
}

func (f *prerequisiteTestForge) CompareAncestry(c context.Context, p int64, h, s string) (forge.Ancestry, error) {
	return f.compare(c, p, h, s)
}

type prerequisiteTestBuilder struct {
	f      forge.Forge
	builds int
}

func (b *prerequisiteTestBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	b.builds++
	return b.f, nil
}

// This fake implements persistence at the exported executor boundary. Unused
// methods panic through the embedded interface, so unexpected I/O fails loudly.
type prerequisiteMemory struct {
	PrerequisiteStore
	now              time.Time
	row              store.RecoveryPrerequisiteClosure
	capture          store.RecoveryCapture
	hold             store.RecoveryCustodyHold
	run              store.Run
	repo             store.GetRepoForUserRow
	conn             store.ForgeConnection
	inTX             atomic.Bool
	locks            []string
	marked           int
	retries          []bool
	missingRun       bool
	transactionError error
}

func (m *prerequisiteMemory) transaction(ctx context.Context, fn func(PrerequisiteStore) error) error {
	if !m.inTX.CompareAndSwap(false, true) {
		return errors.New("nested transaction")
	}
	defer m.inTX.Store(false)
	if m.transactionError != nil {
		return m.transactionError
	}
	return fn(m)
}
func (m *prerequisiteMemory) GetRepoForUser(context.Context, store.GetRepoForUserParams) (store.GetRepoForUserRow, error) {
	return m.repo, nil
}
func (m *prerequisiteMemory) GetForgeConnectionForUser(context.Context, store.GetForgeConnectionForUserParams) (store.ForgeConnection, error) {
	return m.conn, nil
}
func (m *prerequisiteMemory) GetPrerequisiteClosure(context.Context, store.GetPrerequisiteClosureParams) (store.RecoveryPrerequisiteClosure, error) {
	return m.row, nil
}
func (m *prerequisiteMemory) ReservePrerequisitePreparation(_ context.Context, a store.ReservePrerequisitePreparationParams) (store.RecoveryPrerequisiteClosure, error) {
	r := &m.row
	attempt := r.Attempts
	if r.State != "pending" || attempt < 0 || attempt >= 8 || r.NextAttemptAt.Time.After(m.now) || r.AttemptDeadline.Valid && r.AttemptDeadline.Time.After(m.now) {
		return *r, pgx.ErrNoRows
	}
	r.Attempts++
	r.AttemptDeadline = prerequisiteTime(m.now.Add(time.Duration(a.LeaseSeconds * float64(time.Second))))
	r.NextAttemptAt = prerequisiteTime(r.AttemptDeadline.Time.Add(min(time.Hour, 30*time.Second<<uint(attempt))))
	return *r, nil
}
func (m *prerequisiteMemory) RecordPrerequisitePreparationRetry(_ context.Context, a store.RecordPrerequisitePreparationRetryParams) (int64, error) {
	r := &m.row
	if r.State != "pending" || r.Attempts != a.Generation || !r.AttemptDeadline.Time.After(m.now) {
		return 0, nil
	}
	m.retries = append(m.retries, a.BudgetShortfall)
	r.AttemptDeadline = pgtype.Timestamptz{}
	if !a.BudgetShortfall {
		r.Reason = "prerequisite_ancestry_unknown"
	}
	if r.Attempts == 8 {
		r.State = "refused"
		if a.BudgetShortfall {
			r.Reason = "prerequisite_prepare_budget_exhausted"
		}
	}
	return 1, nil
}
func (m *prerequisiteMemory) RefusePrerequisitePreparation(_ context.Context, a store.RefusePrerequisitePreparationParams) (int64, error) {
	if m.row.State != "pending" || m.row.Attempts != a.Generation || !m.row.AttemptDeadline.Time.After(m.now) {
		return 0, nil
	}
	m.row.State = "refused"
	m.row.Reason = a.Reason
	return 1, nil
}
func (m *prerequisiteMemory) ExpirePrerequisitePreparation(_ context.Context, a store.ExpirePrerequisitePreparationParams) (int64, error) {
	if m.row.State != "pending" || m.row.Attempts != 8 || a.Generation != 8 || m.row.AttemptDeadline.Time.After(m.now) {
		return 0, nil
	}
	m.row.State = "refused"
	m.row.Reason = "prerequisite_ancestry_unknown"
	return 1, nil
}
func (m *prerequisiteMemory) LockPrerequisiteRun(context.Context, uuid.UUID) (store.Run, error) {
	m.locks = append(m.locks, "run")
	if m.missingRun {
		return store.Run{}, pgx.ErrNoRows
	}
	return m.run, nil
}
func (m *prerequisiteMemory) LockPrerequisiteHold(context.Context, uuid.UUID) (store.RecoveryCustodyHold, error) {
	m.locks = append(m.locks, "hold")
	return m.hold, nil
}
func (m *prerequisiteMemory) LockPrerequisiteCapture(context.Context, uuid.UUID) (store.RecoveryCapture, error) {
	m.locks = append(m.locks, "capture")
	return m.capture, nil
}
func (m *prerequisiteMemory) LockPrerequisiteClosure(context.Context, uuid.UUID) (store.RecoveryPrerequisiteClosure, error) {
	m.locks = append(m.locks, "closure")
	return m.row, nil
}
func (m *prerequisiteMemory) MarkPrerequisiteInvoked(_ context.Context, a store.MarkPrerequisiteInvokedParams) (store.RecoveryPrerequisiteClosure, error) {
	if m.row.State != "pending" || m.row.Attempts != a.Generation || !m.row.AttemptDeadline.Time.After(m.now) {
		return m.row, pgx.ErrNoRows
	}
	m.marked++
	r := &m.row
	r.State = "invoked"
	r.PinTip = a.PinTip
	r.ConnectionID = a.ConnectionID
	r.ForgeKind = a.ForgeKind
	r.NormalizedBaseUrl = a.NormalizedBaseUrl
	r.ProjectID = a.ProjectID
	r.DefaultBranch = a.DefaultBranch
	r.LiveRunID = prerequisiteID(r.RunID)
	r.InvokedDeadline = prerequisiteTime(m.now.Add(4 * time.Second))
	return *r, nil
}
func (m *prerequisiteMemory) RecordPrerequisitePinned(_ context.Context, a store.RecordPrerequisitePinnedParams) (int64, error) {
	if m.row.Attempts != a.Generation || m.row.State != "invoked" && m.row.State != "create_unknown" {
		return 0, nil
	}
	m.row.State = "pinned"
	m.row.Reason = ""
	m.row.NextAttemptAt = prerequisiteTime(m.now)
	m.row.AuditNextAt = prerequisiteTime(m.now.Add(24 * time.Hour))
	return 1, nil
}
func (m *prerequisiteMemory) MarkPrerequisiteCreateUnknown(_ context.Context, a store.MarkPrerequisiteCreateUnknownParams) (int64, error) {
	if m.row.State != "invoked" || m.row.Attempts != a.Generation {
		return 0, nil
	}
	m.row.State = "create_unknown"
	return 1, nil
}
func (m *prerequisiteMemory) RecordPrerequisiteCreateRefused(_ context.Context, a store.RecordPrerequisiteCreateRefusedParams) (int64, error) {
	if m.row.State != "invoked" || m.row.Attempts != a.Generation {
		return 0, nil
	}
	m.row.State = "refused"
	m.row.Reason = "pin_refused"
	m.row.LiveRunID = pgtype.UUID{}
	return 1, nil
}
func (m *prerequisiteMemory) RecordPrerequisiteForeign(_ context.Context, a store.RecordPrerequisiteForeignParams) (int64, error) {
	if m.row.Attempts != a.Generation {
		return 0, nil
	}
	m.row.State = "foreign"
	m.row.LiveRunID = pgtype.UUID{}
	return 1, nil
}
func (m *prerequisiteMemory) SettleStalePrerequisiteInvocation(_ context.Context, _ uuid.UUID) (int64, error) {
	if m.row.State != "invoked" || m.row.InvokedDeadline.Time.After(m.now) {
		return 0, nil
	}
	m.row.State = "create_unknown"
	return 1, nil
}
func (m *prerequisiteMemory) DeferPrerequisiteReconciliation(context.Context, store.DeferPrerequisiteReconciliationParams) (int64, error) {
	m.row.NextAttemptAt = prerequisiteTime(m.now.Add(time.Hour))
	return 1, nil
}

// A small real HTTP broker fixture exercises Prepare and Send rather than
// substituting prepared evidence. No native subprocess or wall-clock sleeps.
type prerequisiteHTTP struct {
	mu          sync.Mutex
	tip         string
	sends       int
	deletes     int
	refusal     bool
	lost        bool
	refName     string
	transaction *atomic.Bool
	requests    atomic.Int32
	ioInTX      atomic.Bool
	beforePost  func()
}

func (f *prerequisiteHTTP) serve(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if f.transaction != nil && f.transaction.Load() {
		f.ioInTX.Store(true)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	enc := pktline.NewEncoder(w)
	if r.Method == http.MethodPost {
		if f.beforePost != nil {
			f.beforePost()
		}
		f.sends++
		{
			// The request body always starts with the one command packet.
			scan := pktline.NewScanner(r.Body)
			if scan.Scan() {
				fields := strings.Fields(strings.SplitN(string(scan.Bytes()), "\x00", 2)[0])
				if len(fields) == 3 {
					if fields[1] == strings.Repeat("0", 40) {
						f.deletes++
						f.tip = ""
					} else if !f.refusal {
						f.tip = fields[1]
					}
				}
			}
		}
		if f.lost {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		_ = enc.EncodeString("unpack ok\n")
		status := "ok " + f.ref()
		if f.refusal {
			status = "ng " + f.ref() + " policy refusal"
		}
		_ = enc.EncodeString(status + "\n")
		_ = enc.Flush()
		return
	}
	svc := r.URL.Query().Get("service")
	w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
	_ = enc.EncodeString("# service=" + svc + "\n")
	_ = enc.Flush()
	_ = enc.EncodeString(prerequisiteTestTip + " refs/heads/main\x00report-status delete-refs\n")
	if f.tip != "" {
		_ = enc.EncodeString(f.tip + " " + f.ref() + "\n")
	}
	_ = enc.Flush()
}
func (f *prerequisiteHTTP) ref() string { return f.refName }

func newPrerequisiteMemory(t *testing.T, n int) (*Service, *prerequisiteMemory, *prerequisiteHTTP, *prerequisiteTestBuilder) {
	t.Helper()
	m := &prerequisiteMemory{now: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)}
	r := store.RecoveryPrerequisiteClosure{ID: uuid.New(), CaptureID: uuid.New(), HoldID: uuid.New(), RunID: uuid.New(), RepoID: uuid.New(), UserID: uuid.New(), State: "pending", Checksum: strings.Repeat("c", 64), NextAttemptAt: prerequisiteTime(m.now)}
	for i := 0; i < n; i++ {
		r.PrerequisiteShas = append(r.PrerequisiteShas, fmt.Sprintf("%040x", i+1))
	}
	r.PinRef = pushbroker.PrereqRef(r.CaptureID)
	m.row = r
	m.capture = store.RecoveryCapture{ID: r.CaptureID, HoldID: r.HoldID, RunID: r.RunID, UserID: r.UserID, State: "available", ManifestBound: true, Checksum: prerequisiteText(r.Checksum), PrerequisiteShas: r.PrerequisiteShas}
	m.hold = store.RecoveryCustodyHold{ID: r.HoldID, RunID: r.RunID, UserID: r.UserID, InventoryGuarded: true, State: "open"}
	m.run = store.Run{ID: r.RunID, UserID: r.UserID}
	h := &prerequisiteHTTP{transaction: &m.inTX, refName: r.PinRef}
	server := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(server.Close)
	box := newBox(t)
	sealed, err := box.Seal([]byte("fixture-credential"))
	if err != nil {
		t.Fatal(err)
	}
	m.conn = store.ForgeConnection{ID: uuid.New(), UserID: r.UserID, ForgeType: "github", BaseUrl: server.URL, BotUsername: "bot", TokenCiphertext: sealed}
	m.repo = store.GetRepoForUserRow{ID: r.RepoID, ConnectionID: m.conn.ID, ForgeProjectID: 1, DefaultBranch: prerequisiteText("main"), WebUrl: server.URL + "/repo"}
	s := New(nil, box, testParams())
	s.SetPrerequisiteStore(m, m.transaction)
	s.SetClock(func() time.Time { return m.now })
	s.SetForgeBaseURLAllowed(func(string) bool { return true })
	b := &prerequisiteTestBuilder{f: &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, m), compare: func(context.Context, int64, string, string) (forge.Ancestry, error) {
		return forge.AncestryAncestor, nil
	}}}
	s.SetForges(b)
	return s, m, h, b
}
func prerequisiteRun(t *testing.T, s *Service, m *prerequisiteMemory) {
	t.Helper()
	_, err := s.prerequisiteOne(context.Background(), m, m.row, newPrerequisitePass(m.now))
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrerequisiteEveryDeclaredAncestor(t *testing.T) {
	for _, n := range []int{1, 6, 16, 64} {
		for _, tail := range []forge.Ancestry{forge.AncestryAncestor, forge.AncestryNotAncestor, forge.AncestryUnknown} {
			t.Run(fmt.Sprintf("%d/%s", n, tail), func(t *testing.T) {
				s, m, h, b := newPrerequisiteMemory(t, n)
				var got []string
				b.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, m), compare: func(ctx context.Context, _ int64, head, sha string) (forge.Ancestry, error) {
					if m.inTX.Load() {
						t.Error("proof in transaction")
					}
					if head != prerequisiteTestTip {
						t.Error("wrong default")
					}
					if d, ok := ctx.Deadline(); !ok || time.Until(d) > time.Second {
						t.Error("unbounded proof call")
					}
					got = append(got, sha)
					if len(got) == n {
						return tail, nil
					}
					return forge.AncestryAncestor, nil
				}}
				prerequisiteRun(t, s, m)
				if !slices.Equal(got, m.row.PrerequisiteShas) || b.builds != 1 {
					t.Fatalf("comparisons=%v builds=%d", got, b.builds)
				}
				want := "pinned"
				if tail == forge.AncestryNotAncestor {
					want = "refused"
				}
				if tail == forge.AncestryUnknown {
					want = "pending"
				}
				if m.row.State != want {
					t.Fatalf("row=%+v", m.row)
				}
				if tail != forge.AncestryAncestor && (h.sends != 0 || m.marked != 0) {
					t.Fatal("unproved send")
				}
				if h.ioInTX.Load() {
					t.Fatal("broker I/O inside transaction")
				}
			})
		}
	}
}

func TestPrerequisiteRepeatedPrepareBudgetExhaustion(t *testing.T) {
	s, m, h, b := newPrerequisiteMemory(t, 6)
	b.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, m), compare: func(context.Context, int64, string, string) (forge.Ancestry, error) {
		m.now = m.now.Add(750 * time.Millisecond)
		return forge.AncestryAncestor, nil
	}}
	for attempt := 1; attempt <= 8; attempt++ {
		prerequisiteRun(t, s, m)
		if m.row.Attempts != int32(attempt) || h.sends != 0 || m.marked != 0 {
			t.Fatalf("attempt %d row=%+v sends=%d", attempt, m.row, h.sends)
		}
		if !m.row.NextAttemptAt.Time.After(m.now) {
			t.Fatal("missing positive backoff")
		}
		m.now = m.row.NextAttemptAt.Time
	}
	if m.row.Reason != "prerequisite_prepare_budget_exhausted" || m.row.State != "refused" || len(m.retries) != 8 {
		t.Fatalf("row=%+v retries=%v", m.row, m.retries)
	}
	for _, positive := range m.retries {
		if !positive {
			t.Fatal("complete positive proofs classified unknown")
		}
	}
}

func TestPrerequisiteNoBudgetNoAttemptThenFullTurn(t *testing.T) {
	s, m, h, _ := newPrerequisiteMemory(t, 1)
	p := newPrerequisitePass(m.now)
	p.end = m.now.Add(7999 * time.Millisecond)
	if _, err := s.prerequisiteOne(context.Background(), m, m.row, p); err != nil {
		t.Fatal(err)
	}
	if m.row.Attempts != 0 || h.sends != 0 {
		t.Fatal("pre-S1 decline spent attempt")
	}
	prerequisiteRun(t, s, m)
	if m.row.State != "pinned" || m.row.Attempts != 1 {
		t.Fatalf("row=%+v", m.row)
	}
}

func TestPrerequisiteMarkerEligibilityAndLockOrder(t *testing.T) {
	for _, mode := range []string{"normal", "deleted-run", "closed-hold", "binding-changed"} {
		t.Run(mode, func(t *testing.T) {
			s, m, h, b := newPrerequisiteMemory(t, 1)
			if mode == "deleted-run" {
				m.missingRun = true
			}
			if mode == "closed-hold" {
				m.hold.State = "released"
			}
			if mode == "binding-changed" {
				b.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, m), compare: func(context.Context, int64, string, string) (forge.Ancestry, error) {
					m.repo.DefaultBranch = prerequisiteText("other")
					return forge.AncestryAncestor, nil
				}}
			}
			prerequisiteRun(t, s, m)
			if mode == "normal" {
				if !slices.Equal(m.locks, []string{"run", "hold", "capture", "closure", "run", "hold", "capture", "closure", "capture", "closure"}) {
					t.Fatalf("locks=%v", m.locks)
				}
			} else {
				if h.sends != 0 || m.marked != 0 {
					t.Fatal("ineligible marker")
				}
				if mode != "binding-changed" && m.row.Reason != "archive_unavailable" {
					t.Fatalf("row=%+v", m.row)
				}
			}
		})
	}
}

// This regression uses only the original executor and fixture interfaces so it
// can also run against the implementation before the eligibility preflight.
func TestPrerequisiteIneligibleBeforeAncestry(t *testing.T) {
	for _, mode := range []string{"expiredcap", "closedhold", "missingRun"} {
		t.Run(mode, func(t *testing.T) {
			s, m, h, b := newPrerequisiteMemory(t, 1)
			switch mode {
			case "expiredcap":
				m.capture.State = "expired"
			case "closedhold":
				m.hold.State = "released"
			case "missingRun":
				m.missingRun = true
			}
			compares := 0
			b.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, m), compare: func(context.Context, int64, string, string) (forge.Ancestry, error) {
				compares++
				return forge.AncestryUnknown, nil
			}}
			prerequisiteRun(t, s, m)
			if m.row.State != "refused" || m.row.Reason != "archive_unavailable" {
				t.Fatalf("state=%s reason=%s", m.row.State, m.row.Reason)
			}
			if b.builds != 0 || compares != 0 || h.sends != 0 || h.requests.Load() != 0 || m.marked != 0 || m.row.LiveRunID.Valid {
				t.Fatalf("builds=%d compares=%d requests=%d sends=%d markers=%d live=%t", b.builds, compares, h.requests.Load(), h.sends, m.marked, m.row.LiveRunID.Valid)
			}
		})
	}
}
