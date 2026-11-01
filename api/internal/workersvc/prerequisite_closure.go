package workersvc

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	prerequisitePassBudget    = 10 * time.Second
	prerequisiteItemBudget    = 8 * time.Second
	prerequisiteProofBudget   = 5 * time.Second
	prerequisiteMarkerReserve = 4500 * time.Millisecond
	// Reservation/lock transaction, binding read, 1s listing and 1.5s outcome.
	prerequisiteListingReserve = 3500 * time.Millisecond
	prerequisiteDBTimeout      = 500 * time.Millisecond
	prerequisiteOutcomeBudget  = 5 * time.Second
	prerequisiteMaxItems       = 5
	prerequisiteScanLimit      = 10
)

type prerequisitePass struct {
	id          pgtype.UUID
	end         time.Time
	tailEnd     time.Time
	cleanupOnly bool
	auditOnly   bool
}

func newPrerequisitePass(now time.Time) prerequisitePass {
	return prerequisitePass{id: pgtype.UUID{Bytes: uuid.New(), Valid: true}, end: now.Add(prerequisitePassBudget), tailEnd: time.Now().Add(prerequisitePassBudget + prerequisiteOutcomeBudget)}
}
func prerequisiteTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
func prerequisiteText(s string) pgtype.Text   { return pgtype.Text{String: s, Valid: true} }
func prerequisiteID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// SweepPrerequisiteClosures advances existing rows regardless of admission knobs.
// Persistent tuple cursors and a rotating first class bound both scans and
// head-of-line blocking. Failures do not block sibling classes; at most ten
// candidates per class and five admitted items are considered in a ten second pass.
func (s *Service) SweepPrerequisiteClosures(parent context.Context) (int64, error) {
	r := s.prerequisite
	if !r.sweep.TryLock() {
		return 0, nil
	}
	defer r.sweep.Unlock()
	r.mu.Lock()
	if r.stopping || r.root.Err() != nil {
		r.mu.Unlock()
		return 0, nil
	}
	r.wg.Add(1)
	root := r.root
	r.mu.Unlock()
	defer r.wg.Done()
	q, err := s.prerequisiteQueries()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(parent, prerequisitePassBudget)
	defer cancel()
	if root != nil {
		stop := context.AfterFunc(root, cancel)
		defer stop()
	}
	p := newPrerequisitePass(s.now())
	first := r.lead
	r.lead = (r.lead + 1) % 5
	var pages [5][]store.RecoveryPrerequisiteClosure
	var indices [5]int
	var errs []error
	var loaded [5]bool
	seen := map[uuid.UUID]bool{}
	var changed int64
	admitted := 0
	for round := 0; round < prerequisiteScanLimit && admitted < prerequisiteMaxItems; round++ {
		for offset := 0; offset < 5 && admitted < prerequisiteMaxItems; offset++ {
			if ctx.Err() != nil || !s.now().Before(p.end) {
				return changed, errors.Join(errs...)
			}
			lane := (first + offset) % 5
			if !loaded[lane] {
				loaded[lane] = true
				rows, e := s.prerequisitePage(ctx, q, lane, r.cursor[lane], p.end)
				if e != nil {
					errs = append(errs, e)
					continue
				}
				if len(rows) == 0 && r.cursor[lane].due.Valid {
					r.cursor[lane] = prerequisiteCursor{}
					rows, e = s.prerequisitePage(ctx, q, lane, r.cursor[lane], p.end)
					if e != nil {
						errs = append(errs, e)
						continue
					}
				}
				pages[lane] = rows
			}
			if indices[lane] >= len(pages[lane]) {
				continue
			}
			row := pages[lane][indices[lane]]
			indices[lane]++
			due := row.NextAttemptAt
			if lane == 1 {
				due = row.InvokedDeadline
			}
			if lane == 2 && row.State == "pinned" {
				r.auditCursor = prerequisiteCursor{due: row.AuditNextAt, id: row.ID}
			} else {
				r.cursor[lane] = prerequisiteCursor{due: due, id: row.ID}
			}
			// Class filtering precedes deduplication: initial cleanup cannot hide a retry.
			if lane == 3 && row.CleanupAttempts != 0 || lane == 4 && row.CleanupAttempts == 0 {
				continue
			}
			if seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			// Advance rotation and cursor even when another kick currently owns I/O.
			select {
			case r.slot <- struct{}{}:
			default:
				return changed, errors.Join(errs...)
			}
			if row.State == "pending" && row.Attempts < 8 && p.end.Sub(s.now()) < prerequisiteItemBudget {
				<-r.slot
				continue // no reservation, no attempt and no item selection consumed
			}
			if !prerequisiteListingAdmissible(ctx, row, p.end.Sub(s.now())) {
				<-r.slot
				continue
			}
			admitted++
			itemPass := p
			itemPass.cleanupOnly = lane == 3 || lane == 4
			itemPass.auditOnly = lane == 2 && row.State == "pinned"
			n, e := s.prerequisiteOne(ctx, q, row, itemPass)
			<-r.slot
			changed += n
			if e != nil {
				errs = append(errs, e)
			}
		}
	}
	return changed, errors.Join(errs...)
}

func (s *Service) prerequisitePage(ctx context.Context, q PrerequisiteStore, lane int, c prerequisiteCursor, end time.Time) ([]store.RecoveryPrerequisiteClosure, error) {
	ctx, cancel := context.WithTimeout(ctx, prerequisiteDBTimeout)
	defer cancel()
	if !c.due.Valid {
		c.due = pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.NegativeInfinity}
	}
	due := s.now()
	if end.Before(due) {
		due = end
	}
	a := store.ListPrerequisitePendingDueParams{DueAt: prerequisiteTime(due), AfterDueAt: c.due, AfterID: c.id, Lim: prerequisiteScanLimit}
	switch lane {
	case 0:
		return q.ListPrerequisitePendingDue(ctx, a)
	case 1:
		return q.ListPrerequisiteStaleInvokedDue(ctx, store.ListPrerequisiteStaleInvokedDueParams(a))
	case 2:
		// Two persistent subcursors, five raw rows each, share the listing class.
		// There are no eligible-fill pages; even ineligible rows consume the scan cap.
		a.Lim = prerequisiteScanLimit / 2
		unknown, err := q.ListPrerequisiteReconcileDue(ctx, store.ListPrerequisiteReconcileDueParams(a))
		if err != nil {
			return nil, err
		}
		ac := s.prerequisite.auditCursor
		if !ac.due.Valid {
			ac.due = pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.NegativeInfinity}
		}
		aa := store.ListPrerequisiteAuditDueParams(a)
		aa.AfterDueAt = ac.due
		aa.AfterID = ac.id
		audit, err := q.ListPrerequisiteAuditDue(ctx, aa)
		if err != nil {
			return nil, err
		}
		if len(audit) == 0 && s.prerequisite.auditCursor.due.Valid {
			s.prerequisite.auditCursor = prerequisiteCursor{}
		}
		if len(unknown) == 0 && s.prerequisite.cursor[2].due.Valid {
			s.prerequisite.cursor[2] = prerequisiteCursor{}
		}
		rows := make([]store.RecoveryPrerequisiteClosure, 0, len(unknown)+len(audit))
		s.prerequisite.auditFirst = !s.prerequisite.auditFirst
		if s.prerequisite.auditFirst {
			unknown, audit = audit, unknown
		}
		for i := 0; i < max(len(unknown), len(audit)); i++ {
			if i < len(unknown) {
				rows = append(rows, unknown[i])
			}
			if i < len(audit) {
				rows = append(rows, audit[i])
			}
		}
		return rows, nil
	default:
		return q.ListPrerequisiteCleanupDue(ctx, store.ListPrerequisiteCleanupDueParams(a))
	}
}

// The detached context is for one in-flight database outcome only. No forge
// operation receives it. All outcomes share the pass's absolute tail deadline.
func prerequisiteOutcomeContext(ctx context.Context, p prerequisitePass) (context.Context, context.CancelFunc) {
	end := time.Now().Add(prerequisiteOutcomeBudget)
	if p.tailEnd.Before(end) {
		end = p.tailEnd
	}
	return context.WithDeadline(context.WithoutCancel(ctx), end)
}

func prerequisiteListingAdmissible(ctx context.Context, row store.RecoveryPrerequisiteClosure, remaining time.Duration) bool {
	if deadline, ok := ctx.Deadline(); ok {
		remaining = min(remaining, time.Until(deadline))
	}
	return (row.State != "create_unknown" && row.State != "pinned") ||
		(ctx.Err() == nil && remaining >= prerequisiteListingReserve)
}

func (s *Service) prerequisiteOne(ctx context.Context, q PrerequisiteStore, row store.RecoveryPrerequisiteClosure, p prerequisitePass) (int64, error) {
	end := s.now().Add(prerequisiteItemBudget)
	if p.end.Before(end) {
		end = p.end
	}
	ctx, cancel := context.WithTimeout(ctx, max(time.Duration(0), end.Sub(s.now())))
	defer cancel()
	if !prerequisiteListingAdmissible(ctx, row, end.Sub(s.now())) {
		return 0, nil
	}
	switch row.State {
	case "pending":
		if row.Attempts == 8 {
			c, cc := context.WithTimeout(ctx, prerequisiteDBTimeout)
			defer cc()
			return q.ExpirePrerequisitePreparation(c, store.ExpirePrerequisitePreparationParams{ID: row.ID, Generation: row.Attempts})
		}
		if p.end.Sub(s.now()) < prerequisiteItemBudget {
			return 0, nil
		}
		return s.prerequisitePrepare(ctx, q, row, p, end)
	case "invoked":
		c, cc := context.WithTimeout(ctx, prerequisiteDBTimeout)
		defer cc()
		return q.SettleStalePrerequisiteInvocation(c, row.ID)
	case "create_unknown":
		return s.prerequisiteReconcile(ctx, q, row, p, end)
	case "pinned":
		return s.prerequisitePinned(ctx, q, row, p, end)
	}
	return 0, nil
}

type prerequisiteBinding struct {
	repo       store.GetRepoForUserRow
	connection store.ForgeConnection
	clone      string
	pat        string
}

func (b prerequisiteBinding) same(c prerequisiteBinding) bool {
	return b.repo.ConnectionID == c.repo.ConnectionID && b.repo.ForgeProjectID == c.repo.ForgeProjectID &&
		b.repo.DefaultBranch == c.repo.DefaultBranch && b.connection.ForgeType == c.connection.ForgeType &&
		strings.TrimRight(b.connection.BaseUrl, "/") == strings.TrimRight(c.connection.BaseUrl, "/") && b.clone == c.clone
}
func (b prerequisiteBinding) frozen(r store.RecoveryPrerequisiteClosure) bool {
	return r.ConnectionID == prerequisiteID(b.repo.ConnectionID) && r.ForgeKind.String == b.connection.ForgeType &&
		r.NormalizedBaseUrl.String == strings.TrimRight(b.connection.BaseUrl, "/") &&
		r.ProjectID.Int64 == b.repo.ForgeProjectID && r.DefaultBranch == b.repo.DefaultBranch
}
func (s *Service) prerequisiteBinding(ctx context.Context, q PrerequisiteStore, r store.RecoveryPrerequisiteClosure, decrypt bool) (prerequisiteBinding, error) {
	ctx, cancel := context.WithTimeout(ctx, prerequisiteDBTimeout)
	defer cancel()
	repo, err := q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: r.RepoID, UserID: r.UserID})
	if err != nil {
		return prerequisiteBinding{}, err
	}
	conn, err := q.GetForgeConnectionForUser(ctx, store.GetForgeConnectionForUserParams{ID: repo.ConnectionID, UserID: r.UserID})
	if err != nil {
		return prerequisiteBinding{}, err
	}
	b := prerequisiteBinding{repo: repo, connection: conn, clone: strings.TrimSuffix(repo.WebUrl, "/") + ".git"}
	repoURL, err := prerequisiteURL(repo.WebUrl)
	_, baseErr := prerequisiteURL(conn.BaseUrl)
	if err != nil || baseErr != nil || repo.ForgeProjectID <= 0 || repoURL.Path == "" || repoURL.Path == "/" ||
		s.forgeBaseURLAllowed == nil || !s.forgeBaseURLAllowed(conn.BaseUrl) ||
		!s.forgeBaseURLAllowed(repoURL.Scheme+"://"+repoURL.Host) {
		return prerequisiteBinding{}, errors.New("prerequisite: forge URL not allowed")
	}
	if decrypt {
		if s.box == nil {
			return prerequisiteBinding{}, errors.New("prerequisite: secret box unavailable")
		}
		pat, err := s.box.Open(conn.TokenCiphertext)
		if err != nil {
			return prerequisiteBinding{}, errors.New("prerequisite: credential unavailable")
		}
		b.pat = string(pat)
	}
	return b, nil
}
func prerequisiteExternalContext(ctx context.Context, s *Service, end time.Time, reserve time.Duration) (context.Context, context.CancelFunc) {
	remaining := end.Sub(s.now()) - reserve
	return context.WithTimeout(ctx, min(time.Second, max(time.Duration(0), remaining)))
}

func prerequisitePreparationEligible(row store.RecoveryPrerequisiteClosure, run store.Run, hold store.RecoveryCustodyHold, cap store.RecoveryCapture) bool {
	return run.UserID == row.UserID && hold.State == "open" && hold.InventoryGuarded && cap.State == "available" &&
		cap.ManifestBound && cap.Checksum.String == row.Checksum && slices.Equal(cap.PrerequisiteShas, row.PrerequisiteShas) &&
		hold.RunID == row.RunID && cap.RunID == row.RunID && cap.HoldID == row.HoldID
}

func (s *Service) prerequisitePrepare(ctx context.Context, q PrerequisiteStore, row store.RecoveryPrerequisiteClosure, p prerequisitePass, end time.Time) (int64, error) {
	c, cc := context.WithTimeout(ctx, prerequisiteDBTimeout)
	reserved, err := q.ReservePrerequisitePreparation(c, store.ReservePrerequisitePreparationParams{ID: row.ID, LeaseSeconds: 8})
	cc()
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	row = reserved
	retry := func(positive bool) (int64, error) {
		w, wc := prerequisiteOutcomeContext(ctx, p)
		defer wc()
		return q.RecordPrerequisitePreparationRetry(w, store.RecordPrerequisitePreparationRetryParams{ID: row.ID, Generation: row.Attempts, BudgetShortfall: positive, LastError: "prerequisite preparation deferred"})
	}
	refuse := func(reason string) (int64, error) {
		w, wc := prerequisiteOutcomeContext(ctx, p)
		defer wc()
		return q.RefusePrerequisitePreparation(w, store.RefusePrerequisitePreparationParams{ID: row.ID, Generation: row.Attempts, Reason: reason})
	}
	// Eligibility precedes credentials and all remote work. Recheck at S2 as
	// well: these short-lived locks do not authorize the later marker.
	eligible, current := false, false
	c, cc = context.WithTimeout(ctx, prerequisiteDBTimeout)
	err = s.prerequisiteTx(c, func(tx PrerequisiteStore) error {
		run, e := tx.LockPrerequisiteRun(c, row.RunID)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		runPresent := e == nil
		hold, e := tx.LockPrerequisiteHold(c, row.HoldID)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		holdPresent := e == nil
		cap, e := tx.LockPrerequisiteCapture(c, row.CaptureID)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		capturePresent := e == nil
		locked, e := tx.LockPrerequisiteClosure(c, row.ID)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		current = locked.State == "pending" && locked.Attempts == row.Attempts
		eligible = runPresent && holdPresent && capturePresent && prerequisitePreparationEligible(row, run, hold, cap)
		return nil
	})
	cc()
	if err != nil {
		return 0, err
	}
	if !current {
		return 0, nil
	}
	if !eligible {
		return refuse("archive_unavailable")
	}
	b, err := s.prerequisiteBinding(ctx, q, row, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse("archive_unavailable")
	}
	if err != nil {
		return retry(false)
	}
	if s.forges == nil {
		return retry(false)
	}
	// The actual ForgeBuilder interface decrypts the sealed connection. The URL
	// gate above must precede both that builder and the broker credential decrypt.
	driver, err := s.prerequisiteDriver(b)
	if err != nil {
		return retry(false)
	}
	proofEnd := s.now().Add(prerequisiteProofBudget)
	if end.Before(proofEnd) {
		proofEnd = end
	}
	proof, pc := context.WithTimeout(ctx, max(time.Duration(0), proofEnd.Sub(s.now())))
	defer pc()
	op, oc := prerequisiteExternalContext(pushbroker.WithoutRedirects(proof), s, proofEnd, 0)
	if err := b.validate(op, driver); err != nil {
		oc()
		return retry(false)
	}
	prepared, err := pushbroker.PreparePrereqCreate(op, pushbroker.PrereqCreateOptions{
		CloneURL: b.clone, Username: b.connection.BotUsername, PAT: b.pat, DefaultBranch: b.repo.DefaultBranch.String, CaptureID: row.CaptureID,
	})
	identityErr := b.validate(op, driver)
	oc()
	if identityErr != nil {
		return retry(false)
	}
	if errors.Is(err, pushbroker.ErrRefExists) {
		return refuse("pin_foreign")
	}
	if err != nil {
		return retry(false)
	}
	if len(row.PrerequisiteShas) < 1 || len(row.PrerequisiteShas) > 64 {
		return refuse("archive_unavailable")
	}
	// One reused driver, every declared prerequisite, no proof cache.
	for _, sha := range row.PrerequisiteShas {
		if proof.Err() != nil || !s.now().Before(proofEnd) {
			return retry(false)
		}
		op, oc := prerequisiteExternalContext(proof, s, proofEnd, 0)
		a, e := driver.CompareAncestry(op, b.repo.ForgeProjectID, prepared.Tip(), sha)
		identityErr := b.validate(op, driver)
		oc()
		if identityErr != nil {
			return retry(false)
		}
		if e != nil || a == forge.AncestryUnknown {
			return retry(false)
		}
		if a == forge.AncestryNotAncestor {
			return refuse("prerequisite_not_on_default")
		}
		if a != forge.AncestryAncestor {
			return retry(false)
		}
	}
	// A complete positive proof may be late. It spends this attempt; only that
	// case is classified as preparation budget exhaustion.
	if end.Sub(s.now()) < prerequisiteMarkerReserve || ctx.Err() != nil {
		return retry(true)
	}
	fresh, err := s.prerequisiteBinding(ctx, q, row, false)
	if err != nil || !b.same(fresh) {
		return retry(false)
	}
	if end.Sub(s.now()) < prerequisiteMarkerReserve || ctx.Err() != nil {
		return retry(true)
	}
	marked := false
	budgetShortfall := false
	unavailable := false
	c, cc = context.WithTimeout(ctx, prerequisiteDBTimeout)
	err = s.prerequisiteTx(c, func(tx PrerequisiteStore) error {
		run, e := tx.LockPrerequisiteRun(c, row.RunID)
		if errors.Is(e, pgx.ErrNoRows) {
			unavailable = true
			return nil
		}
		if e != nil {
			return e
		}
		hold, e := tx.LockPrerequisiteHold(c, row.HoldID)
		if errors.Is(e, pgx.ErrNoRows) {
			unavailable = true
			return nil
		}
		if e != nil {
			return e
		}
		cap, e := tx.LockPrerequisiteCapture(c, row.CaptureID)
		if e != nil {
			return e
		}
		current, e := tx.LockPrerequisiteClosure(c, row.ID)
		if e != nil {
			return e
		}
		if current.State != "pending" || current.Attempts != row.Attempts {
			return nil
		}
		if !prerequisitePreparationEligible(row, run, hold, cap) {
			unavailable = true
			return nil
		}
		latest, e := s.prerequisiteBinding(c, tx, row, false)
		if e != nil {
			return e
		}
		if !b.same(latest) {
			return nil
		}
		// Recheck after the last binding read, immediately before the durable marker.
		// Reserve 500ms for commit, 500ms for post-commit binding, 1s send,
		// 1s readback and 1.5s outcome.
		if end.Sub(s.now()) < prerequisiteMarkerReserve || c.Err() != nil {
			budgetShortfall = true
			return nil
		}
		_, e = tx.MarkPrerequisiteInvoked(c, store.MarkPrerequisiteInvokedParams{
			ID: row.ID, Generation: row.Attempts, SendSeconds: 4, PinTip: prerequisiteText(prepared.Tip()),
			ConnectionID: prerequisiteID(b.repo.ConnectionID), ForgeKind: prerequisiteText(b.connection.ForgeType),
			NormalizedBaseUrl: prerequisiteText(strings.TrimRight(b.connection.BaseUrl, "/")),
			ProjectID:         pgtype.Int8{Int64: b.repo.ForgeProjectID, Valid: true}, DefaultBranch: b.repo.DefaultBranch,
		})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		marked = e == nil
		return e
	})
	cc()
	if err != nil {
		return 0, err
	}
	if unavailable {
		return refuse("archive_unavailable")
	}
	if !marked {
		return retry(budgetShortfall)
	}
	// Commit precedes forge I/O. A deleted or rebound connection after the marker
	// leaves invoked durable; the stale lane later converts it to listing-only unknown.
	fresh, err = s.prerequisiteBinding(ctx, q, row, false)
	if err != nil || !b.same(fresh) || ctx.Err() != nil || end.Sub(s.now()) < 3500*time.Millisecond {
		return 1, nil
	}
	send, sc := context.WithTimeout(pushbroker.WithoutRedirects(ctx), max(time.Duration(0), end.Sub(s.now())-1500*time.Millisecond))
	if err := b.validate(send, driver); err != nil {
		sc()
		return 1, nil
	}
	outcome, _ := pushbroker.SendPrereqCreate(send, prepared)
	if err := b.validate(send, driver); err != nil {
		outcome = pushbroker.PrereqCreateUnknown
	}
	sc()
	return s.prerequisiteOutcome(ctx, row, p, func(w context.Context, tx PrerequisiteStore) (int64, error) {
		switch outcome {
		case pushbroker.PrereqCreateCreated:
			return tx.RecordPrerequisitePinned(w, store.RecordPrerequisitePinnedParams{ID: row.ID, Generation: row.Attempts})
		case pushbroker.PrereqCreateRefused:
			return tx.RecordPrerequisiteCreateRefused(w, store.RecordPrerequisiteCreateRefusedParams{ID: row.ID, Generation: row.Attempts})
		case pushbroker.PrereqCreateForeignTip:
			return tx.RecordPrerequisiteForeign(w, store.RecordPrerequisiteForeignParams{ID: row.ID, Generation: row.Attempts})
		default:
			return tx.MarkPrerequisiteCreateUnknown(w, store.MarkPrerequisiteCreateUnknownParams{ID: row.ID, Generation: row.Attempts})
		}
	})
}

// Pointer-clearing outcomes always take capture then closure, never a run lock.
func (s *Service) prerequisiteOutcome(ctx context.Context, row store.RecoveryPrerequisiteClosure, p prerequisitePass, fn func(context.Context, PrerequisiteStore) (int64, error)) (int64, error) {
	w, wc := prerequisiteOutcomeContext(ctx, p)
	defer wc()
	var n int64
	err := s.prerequisiteTx(w, func(tx PrerequisiteStore) error {
		if _, e := tx.LockPrerequisiteCapture(w, row.CaptureID); e != nil {
			return e
		}
		if _, e := tx.LockPrerequisiteClosure(w, row.ID); e != nil {
			return e
		}
		var e error
		n, e = fn(w, tx)
		return e
	})
	return n, err
}
