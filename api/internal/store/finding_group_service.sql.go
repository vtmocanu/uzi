package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrFindingGroupUnavailable deliberately covers missing, foreign, stale, and invalid
// members alike. Callers must not use claim failures as a cross-user oracle.
var (
	ErrFindingGroupUnavailable = errors.New("finding group unavailable")
	ErrFindingGroupMixedRepo   = errors.New("finding group spans repositories")
	ErrFindingGroupConflict    = errors.New("finding group member is not fileable")
)

type FindingGroupDB interface {
	Begin(context.Context) (pgx.Tx, error)
}

type FindingGroupClaimMember struct {
	DispositionID uuid.UUID
	FindingID     uuid.UUID
	RepoID        uuid.UUID
	Location      string
	Title         string
	Description   string
	Labels        []byte
}
type FindingGroupClaimOperation struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	RepoID    uuid.UUID
	Phase     string
	Deadline  time.Time
	CreatedAt time.Time
	IssueIID  *int64
	IssueURL  string
}

// FindingGroupDraftMember is one owned coordinate and its latest evidence, if any.
type FindingGroupDraftMember struct {
	DispositionID    uuid.UUID
	RepoID           uuid.UUID
	Location         string
	Status           string
	GroupOperationID pgtype.UUID
	FindingID        pgtype.UUID
}

// ReadFindingGroupDraftMembers returns owned dispositions in id order. The caller
// checks cardinality before inspecting status, so unknown and foreign ids coincide.
func ReadFindingGroupDraftMembers(ctx context.Context, db DBTX, user uuid.UUID, ids []uuid.UUID) ([]FindingGroupDraftMember, error) {
	rows, err := db.Query(ctx, `SELECT d.id,d.repo_id,d.location,d.status,d.group_operation_id,
   (SELECT f.id FROM findings f WHERE f.user_id=d.user_id AND f.repo_id=d.repo_id AND f.location=d.location
    ORDER BY f.created_at DESC,f.id DESC LIMIT 1)
   FROM finding_dispositions d WHERE d.user_id=$1 AND d.id=ANY($2::uuid[]) ORDER BY d.id`, user, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []FindingGroupDraftMember{}
	for rows.Next() {
		var m FindingGroupDraftMember
		if err := rows.Scan(&m.DispositionID, &m.RepoID, &m.Location, &m.Status, &m.GroupOperationID, &m.FindingID); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// FindingDispositionForEvidence resolves even an older evidence row by its coordinate.
func FindingDispositionForEvidence(ctx context.Context, db DBTX, user, evidence uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.QueryRow(ctx, `SELECT d.id FROM findings f JOIN finding_dispositions d
   ON d.user_id=f.user_id AND d.repo_id=f.repo_id AND d.location=f.location
   WHERE f.id=$1 AND f.user_id=$2`, evidence, user).Scan(&id)
	return id, err
}

// ClaimFindingGroup atomically claims 1-50 unique open dispositions in one repo.
// The newest existing evidence is captured for each member under ordered row locks.
func ClaimFindingGroup(ctx context.Context, db FindingGroupDB, user uuid.UUID, ids []uuid.UUID, deadline time.Time) (FindingGroupClaimOperation, []FindingGroupClaimMember, error) {
	var zero FindingGroupClaimOperation
	if len(ids) < 1 || len(ids) > 50 || !deadline.After(time.Now()) {
		return zero, nil, ErrFindingGroupUnavailable
	}
	unique := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			return zero, nil, ErrFindingGroupUnavailable
		}
		unique[id] = struct{}{}
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return zero, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT id, repo_id, location FROM finding_dispositions
        WHERE user_id=$1 AND id=ANY($2::uuid[]) ORDER BY id FOR UPDATE`, user, ids)
	if err != nil {
		return zero, nil, err
	}
	type coord struct {
		id, repo uuid.UUID
		location string
	}
	coords := make([]coord, 0, len(unique))
	for rows.Next() {
		var c coord
		if err = rows.Scan(&c.id, &c.repo, &c.location); err != nil {
			break
		}
		coords = append(coords, c)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return zero, nil, err
	}
	if len(coords) != len(unique) {
		return zero, nil, ErrFindingGroupUnavailable
	}
	members := make([]FindingGroupClaimMember, 0, len(coords))
	for _, c := range coords {
		var m FindingGroupClaimMember
		m.DispositionID, m.RepoID, m.Location = c.id, c.repo, c.location
		if c.repo != coords[0].repo {
			return zero, nil, ErrFindingGroupMixedRepo
		}
		err = tx.QueryRow(ctx, `SELECT f.id,f.title,f.description_md,f.labels FROM finding_dispositions d
            JOIN LATERAL (SELECT id,title,description_md,labels FROM findings
                WHERE user_id=d.user_id AND repo_id=d.repo_id AND location=d.location
                ORDER BY created_at DESC,id DESC LIMIT 1) f ON true
            WHERE d.id=$1 AND d.user_id=$2 AND d.status='open' AND d.group_operation_id IS NULL`, c.id, user).
			Scan(&m.FindingID, &m.Title, &m.Description, &m.Labels)
		if errors.Is(err, pgx.ErrNoRows) {
			return zero, nil, ErrFindingGroupConflict
		}
		if err != nil {
			return zero, nil, err
		}
		members = append(members, m)
	}
	var op FindingGroupClaimOperation
	err = tx.QueryRow(ctx, `INSERT INTO finding_group_operations(user_id,repo_id,deadline_at)
        SELECT $1,$2,$3 WHERE $3::timestamptz > now() RETURNING id,created_at`, user, coords[0].repo, deadline).Scan(&op.ID, &op.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, nil, ErrFindingGroupUnavailable
	}
	if err != nil {
		return zero, nil, err
	}
	op.UserID, op.RepoID, op.Phase, op.Deadline = user, coords[0].repo, "pre_call", deadline
	for _, m := range members {
		tag, e := tx.Exec(ctx, `UPDATE finding_dispositions SET status='filing',filing_since=now(),group_operation_id=$1
            WHERE id=$2 AND user_id=$3 AND status='open' AND group_operation_id IS NULL`, op.ID, m.DispositionID, user)
		if e != nil {
			return zero, nil, e
		}
		if tag.RowsAffected() != 1 {
			return zero, nil, ErrFindingGroupConflict
		}
		_, e = tx.Exec(ctx, `INSERT INTO finding_group_members(operation_id,disposition_id,finding_id) VALUES($1,$2,$3)`, op.ID, m.DispositionID, m.FindingID)
		if e != nil {
			return zero, nil, e
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return zero, nil, err
	}
	return op, members, nil
}

func groupPhase(ctx context.Context, db DBTX, user, id uuid.UUID, from, to string) (bool, error) {
	tag, err := db.Exec(ctx, `UPDATE finding_group_operations SET phase=$1 WHERE id=$2 AND user_id=$3 AND phase=$4`, to, id, user, from)
	return err == nil && tag.RowsAffected() == 1, err
}

// BeginFindingGroupCall is the operation-owned last durable write before CreateIssue.
func BeginFindingGroupCall(ctx context.Context, db DBTX, user, id uuid.UUID) (bool, error) {
	tag, err := db.Exec(ctx, `UPDATE finding_group_operations SET phase='in_flight'
        WHERE id=$1 AND user_id=$2 AND phase='pre_call' AND deadline_at > now()`, id, user)
	return err == nil && tag.RowsAffected() == 1, err
}
func MarkFindingGroupUncertain(ctx context.Context, db DBTX, user, id uuid.UUID) (bool, error) {
	return groupPhase(ctx, db, user, id, "in_flight", "returned_uncertain")
}
func RecordFindingGroupIssue(ctx context.Context, db DBTX, user, id uuid.UUID, iid int64, url string) (bool, error) {
	if iid <= 0 || url == "" {
		return false, ErrFindingGroupUnavailable
	}
	tag, err := db.Exec(ctx, `UPDATE finding_group_operations SET phase='issue_recorded',issue_iid=$1,issue_url=$2
        WHERE id=$3 AND user_id=$4 AND phase IN ('in_flight','returned_uncertain') AND issue_iid IS NULL`, iid, url, id, user)
	return err == nil && tag.RowsAffected() == 1, err
}

// SettleFindingGroup commits every member together after the durable issue record.
func SettleFindingGroup(ctx context.Context, db FindingGroupDB, user, id uuid.UUID) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var iid int64
	var url string
	var repo uuid.UUID
	err = tx.QueryRow(ctx, `SELECT repo_id,issue_iid,issue_url FROM finding_group_operations
        WHERE id=$1 AND user_id=$2 AND phase='issue_recorded' FOR UPDATE`, id, user).Scan(&repo, &iid, &url)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var expected, linked int64
	err = tx.QueryRow(ctx, `SELECT count(*) FROM finding_group_members WHERE operation_id=$1`, id).Scan(&expected)
	if err != nil {
		return false, err
	}
	err = tx.QueryRow(ctx, `SELECT count(*) FROM finding_dispositions WHERE group_operation_id=$1`, id).Scan(&linked)
	if err != nil {
		return false, err
	}
	if expected < 1 || linked != expected {
		return false, ErrFindingGroupUnavailable
	}
	tag, err := tx.Exec(ctx, `UPDATE finding_dispositions SET status='filed',filed_issue_iid=$1,filed_issue_url=$2,
        filing_since=NULL,group_operation_id=NULL,set_via=NULL,close_synced_at=NULL,resolved_at=now()
        WHERE group_operation_id=$3 AND user_id=$4 AND repo_id=$5 AND status='filing'
          AND id IN (SELECT disposition_id FROM finding_group_members WHERE operation_id=$3)`, iid, url, id, user, repo)
	if err != nil {
		return false, err
	}
	if expected < 1 || tag.RowsAffected() != expected {
		return false, ErrFindingGroupUnavailable
	}
	ok, err := groupPhase(ctx, tx, user, id, "issue_recorded", "settled")
	if err != nil || !ok {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// ReleaseFindingGroupDefinitive reopens a pre-call claim or a definitively rejected write.
func ReleaseFindingGroupDefinitive(ctx context.Context, db FindingGroupDB, user, id uuid.UUID) (bool, error) {
	return releaseFindingGroup(ctx, db, user, id, false)
}

// ReleaseFindingGroupAfterDeadline requires an owner-confirmed request. The DB
// enforces the deadline and absence of any recorded issue.
func ReleaseFindingGroupAfterDeadline(ctx context.Context, db FindingGroupDB, user, id uuid.UUID) (bool, error) {
	return releaseFindingGroup(ctx, db, user, id, true)
}
func releaseFindingGroup(ctx context.Context, db FindingGroupDB, user, id uuid.UUID, afterDeadline bool) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var phase string
	var expired bool
	err = tx.QueryRow(ctx, `SELECT phase,deadline_at<=now() FROM finding_group_operations
        WHERE id=$1 AND user_id=$2 AND issue_iid IS NULL FOR UPDATE`, id, user).Scan(&phase, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if (phase != "pre_call" && phase != "in_flight" && phase != "returned_uncertain") ||
		(afterDeadline && !expired) || (!afterDeadline && (expired || phase == "returned_uncertain")) {
		return false, nil
	}
	var n, linked int64
	err = tx.QueryRow(ctx, `SELECT count(*) FROM finding_group_members WHERE operation_id=$1`, id).Scan(&n)
	if err != nil {
		return false, err
	}
	err = tx.QueryRow(ctx, `SELECT count(*) FROM finding_dispositions WHERE group_operation_id=$1`, id).Scan(&linked)
	if err != nil {
		return false, err
	}
	if n < 1 || linked != n {
		return false, ErrFindingGroupUnavailable
	}
	tag, err := tx.Exec(ctx, `UPDATE finding_dispositions SET status='open',filing_since=NULL,group_operation_id=NULL
        WHERE group_operation_id=$1 AND user_id=$2 AND status='filing'
          AND id IN (SELECT disposition_id FROM finding_group_members WHERE operation_id=$1)`, id, user)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != n {
		return false, ErrFindingGroupUnavailable
	}
	tag, err = tx.Exec(ctx, `UPDATE finding_group_operations SET phase='released' WHERE id=$1 AND user_id=$2 AND issue_iid IS NULL`, id, user)
	if err != nil || tag.RowsAffected() != 1 {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// GetFindingGroupOperation includes terminal phases for owner-scoped release responses.
func GetFindingGroupOperation(ctx context.Context, db DBTX, user, id uuid.UUID) (FindingGroupClaimOperation, error) {
	var o FindingGroupClaimOperation
	err := db.QueryRow(ctx, `SELECT id,user_id,repo_id,phase,deadline_at,created_at,issue_iid,issue_url
        FROM finding_group_operations WHERE id=$1 AND user_id=$2`, id, user).
		Scan(&o.ID, &o.UserID, &o.RepoID, &o.Phase, &o.Deadline, &o.CreatedAt, &o.IssueIID, &o.IssueURL)
	return o, err
}

func GetPendingFindingGroup(ctx context.Context, db DBTX, user, id uuid.UUID) (FindingGroupClaimOperation, error) {
	var o FindingGroupClaimOperation
	err := db.QueryRow(ctx, `SELECT id,user_id,repo_id,phase,deadline_at,created_at,issue_iid,issue_url
        FROM finding_group_operations WHERE id=$1 AND user_id=$2 AND phase NOT IN ('released','settled')`, id, user).
		Scan(&o.ID, &o.UserID, &o.RepoID, &o.Phase, &o.Deadline, &o.CreatedAt, &o.IssueIID, &o.IssueURL)
	return o, err
}

// FindingGroupCursor identifies the last operation selected in a repo pass.
type FindingGroupCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// ListPendingFindingGroupsForRepo returns at most 100 owner-scoped pending
// operations after the cursor. An empty page tells the caller to wrap to the start.
func ListPendingFindingGroupsForRepo(ctx context.Context, db DBTX, repo uuid.UUID, after *FindingGroupCursor) ([]FindingGroupClaimOperation, error) {
	query := `SELECT o.id,o.user_id,o.repo_id,o.phase,o.deadline_at,o.created_at,o.issue_iid,o.issue_url
        FROM finding_group_operations o JOIN repos r ON r.id=o.repo_id
        JOIN forge_connections c ON c.id=r.connection_id AND c.user_id=o.user_id
        WHERE o.repo_id=$1 AND o.phase NOT IN ('released','settled')`
	args := []interface{}{repo}
	if after != nil {
		query += " AND (o.created_at,o.id)>($2,$3)"
		args = append(args, after.CreatedAt, after.ID)
	}
	query += " ORDER BY o.created_at,o.id LIMIT 100"
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ops := []FindingGroupClaimOperation{}
	for rows.Next() {
		var o FindingGroupClaimOperation
		if err := rows.Scan(&o.ID, &o.UserID, &o.RepoID, &o.Phase, &o.Deadline, &o.CreatedAt, &o.IssueIID, &o.IssueURL); err != nil {
			return nil, err
		}
		ops = append(ops, o)
	}
	return ops, rows.Err()
}

// ListPendingFindingGroupsByIDs checks only marker IDs observed in this fetch.
// The repo join enforces the connection owner; callers cap ids before querying.
func ListPendingFindingGroupsByIDs(ctx context.Context, db DBTX, repo uuid.UUID, ids []uuid.UUID) ([]FindingGroupClaimOperation, error) {
	if len(ids) == 0 || len(ids) > 1000 {
		return nil, ErrFindingGroupUnavailable
	}
	rows, err := db.Query(ctx, `SELECT o.id,o.user_id,o.repo_id,o.phase,o.deadline_at,o.created_at,o.issue_iid,o.issue_url
        FROM finding_group_operations o JOIN repos r ON r.id=o.repo_id
        JOIN forge_connections c ON c.id=r.connection_id AND c.user_id=o.user_id
        WHERE o.repo_id=$1 AND o.id=ANY($2::uuid[])
          AND o.phase IN ('in_flight','returned_uncertain') AND o.issue_iid IS NULL`, repo, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ops := make([]FindingGroupClaimOperation, 0)
	for rows.Next() {
		var o FindingGroupClaimOperation
		if err := rows.Scan(&o.ID, &o.UserID, &o.RepoID, &o.Phase, &o.Deadline, &o.CreatedAt, &o.IssueIID, &o.IssueURL); err != nil {
			return nil, err
		}
		ops = append(ops, o)
	}
	return ops, rows.Err()
}

// FindingGroupRepoPendingStats counts all owner-scoped pending operations without
// loading their rows into the sync pass.
func FindingGroupRepoPendingStats(ctx context.Context, db DBTX, repo uuid.UUID) (int64, *time.Time, error) {
	var count int64
	var oldest *time.Time
	err := db.QueryRow(ctx, `SELECT count(*),min(o.created_at) FROM finding_group_operations o
        JOIN repos r ON r.id=o.repo_id
        JOIN forge_connections c ON c.id=r.connection_id AND c.user_id=o.user_id
        WHERE o.repo_id=$1 AND o.phase NOT IN ('released','settled')`, repo).Scan(&count, &oldest)
	return count, oldest, err
}

func FindingGroupPendingStats(ctx context.Context, db DBTX, user uuid.UUID) (int64, *time.Time, error) {
	var count int64
	var oldest *time.Time
	err := db.QueryRow(ctx, `SELECT count(*),min(created_at) FROM finding_group_operations
        WHERE user_id=$1 AND phase NOT IN ('released','settled')`, user).Scan(&count, &oldest)
	return count, oldest, err
}
