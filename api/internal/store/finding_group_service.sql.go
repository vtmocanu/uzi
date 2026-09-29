package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// ReleaseFindingGroupDefinitive is for a confirmed no-create outcome. The caller
// must only invoke it after the forge has definitively rejected the write.
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
		(afterDeadline && !expired) || (!afterDeadline && phase == "returned_uncertain") {
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

func GetPendingFindingGroup(ctx context.Context, db DBTX, user, id uuid.UUID) (FindingGroupClaimOperation, error) {
	var o FindingGroupClaimOperation
	err := db.QueryRow(ctx, `SELECT id,user_id,repo_id,phase,deadline_at,created_at,issue_iid,issue_url
        FROM finding_group_operations WHERE id=$1 AND user_id=$2 AND phase NOT IN ('released','settled')`, id, user).
		Scan(&o.ID, &o.UserID, &o.RepoID, &o.Phase, &o.Deadline, &o.CreatedAt, &o.IssueIID, &o.IssueURL)
	return o, err
}
func FindingGroupPendingStats(ctx context.Context, db DBTX, user uuid.UUID) (int64, *time.Time, error) {
	var count int64
	var oldest *time.Time
	err := db.QueryRow(ctx, `SELECT count(*),min(created_at) FROM finding_group_operations
        WHERE user_id=$1 AND phase NOT IN ('released','settled')`, user).Scan(&count, &oldest)
	return count, oldest, err
}
