package issuetrackersvc

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Bounds the repos_issue_tracker_link_shape CHECK (migration 00474) enforces.
const (
	maxProjectShortLen = 44
	maxTagNameLen      = 256
)

// Link is the stored link of one repo to one issue tracker project.
type Link struct {
	RepoID       uuid.UUID
	ConnectionID uuid.UUID
	ProjectID    string
	ProjectShort string
	TagID        string
	TagName      string
	LinkedAt     time.Time
}

// LinkRepo links a repo that has never been enabled to one project of one of
// the user's issue tracker connections, with the tag that marks eligible issues
// (PRD #4173 D13, D14). It is refused unless the repo is still awaiting its
// first enable and is not linked yet; there is no relink and no unlink.
//
// The check against the issue tracker is read-only (token, project list, tag
// list); it never writes to the tracker. The tag must be one the connection's
// account can apply and that someone else can apply too. That is a statement
// about the tag's sharing settings only: it does not prove that anyone can edit
// issues in the project, and no message here says it does. The database part runs
// in one transaction holding the repo row lock the first enable also takes, so
// the link and the first enable end in exactly one of the two outcomes.
func (s *Service) LinkRepo(ctx context.Context, userID, repoID, connID uuid.UUID, projectID, tagID string) (Link, error) {
	src, err := s.q.GetRepoIssueSourceForUser(ctx, store.GetRepoIssueSourceForUserParams{ID: repoID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, &Error{Kind: KindNotFound, Message: "repo not found"}
	}
	if err != nil {
		s.log.Error("read repo for issue tracker link", "error", err)
		return Link{}, internal()
	}
	if err := linkable(src.AwaitingFirstEnable, src.IssueTrackerConnectionID.Valid); err != nil {
		return Link{}, err
	}
	row, err := s.getRow(ctx, userID, connID)
	if err != nil {
		return Link{}, err
	}
	if !issuetracker.ValidProjectID(projectID) {
		return Link{}, &Error{Kind: KindBadRequest, Message: "invalid project id"}
	}

	account, project, tag, err := s.checkLinkTarget(ctx, userID, connID, row, projectID, tagID)
	if err != nil {
		return Link{}, err
	}

	var repo store.Repo
	err = s.inTx(ctx, func(q Querier) error {
		lock, err := q.LockRepoForLinkForUser(ctx, store.LockRepoForLinkForUserParams{ID: repoID, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{Kind: KindNotFound, Message: "repo not found"}
		}
		if err != nil {
			return err
		}
		if err := linkable(lock.AwaitingFirstEnable, lock.IssueTrackerConnectionID.Valid); err != nil {
			return err
		}
		// FOR SHARE: a reconnect of this connection that changes its account holds
		// the row FOR UPDATE, so it either finished before this read (and the
		// account below differs) or waits for this transaction.
		crow, err := q.LockIssueTrackerConnectionForShareForUser(ctx, store.LockIssueTrackerConnectionForShareForUserParams{ID: connID, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound()
		}
		if err != nil {
			return err
		}
		if crow.AccountID != account.ID {
			return errAccountChanged()
		}
		stores, err := q.RepoStoresIssueNumber(ctx, repoID)
		if err != nil {
			return err
		}
		if stores {
			return conflict("this repo already stores issue numbers and cannot be linked to an issue tracker")
		}
		repo, err = q.SetRepoIssueTrackerLink(ctx, store.SetRepoIssueTrackerLinkParams{
			ConnectionID: connID,
			ProjectID:    project.ID,
			ProjectShort: project.ShortName,
			TagID:        tag.ID,
			TagName:      tag.Name,
			RepoID:       repoID,
			UserID:       userID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict("the repo could not be linked: it was enabled, linked or removed meanwhile")
		}
		return err
	})
	if err != nil {
		return Link{}, s.txError("link repo to issue tracker", err)
	}
	return Link{
		RepoID:       repo.ID,
		ConnectionID: connID,
		ProjectID:    repo.IssueTrackerProjectID.String,
		ProjectShort: repo.IssueTrackerProjectShort.String,
		TagID:        repo.EligibilityTagID.String,
		TagName:      repo.EligibilityTagName.String,
		LinkedAt:     repo.IssueTrackerLinkedAt.Time,
	}, nil
}

// linkable refuses a repo that was enabled at some point or is already linked.
func linkable(awaiting, linked bool) *Error {
	if linked {
		return conflict("this repo is already linked to an issue tracker project; a link cannot be changed")
	}
	if !awaiting {
		return conflict("only a repo that has never been enabled can be linked to an issue tracker; this one was enabled before")
	}
	return nil
}

func errAccountChanged() *Error {
	return conflict("the connection's token now belongs to a different account than the one this connection was stored for; reconnect first")
}

// checkLinkTarget is the read-only part of LinkRepo. It returns the verified
// account and the project and tag exactly as the tracker listed them.
func (s *Service) checkLinkTarget(ctx context.Context, userID, connID uuid.UUID, row store.IssueTrackerConnection, projectID, tagID string) (issuetracker.Account, issuetracker.Project, issuetracker.Tag, error) {
	var (
		noAccount issuetracker.Account
		noProject issuetracker.Project
		noTag     issuetracker.Tag
	)
	d, err := s.DriverFor(ctx, userID, connID, issuetracker.Binding{}, issuetracker.StateMapping{})
	if err != nil {
		return noAccount, noProject, noTag, err
	}
	account, err := d.VerifyToken(ctx)
	if err != nil {
		return noAccount, noProject, noTag, &Error{Kind: KindBadGateway, Message: "token verification failed: " + err.Error()}
	}
	// The token is opened again only to refuse an account id or login that holds it
	// (validateAccount); DriverFor opened its own copy to build the driver.
	kind, ok := issuetracker.ParseKind(row.Kind)
	if !ok {
		s.log.Error("stored issue tracker connection has an unknown kind", "connection", connID.String(), "kind", row.Kind)
		return noAccount, noProject, noTag, internal()
	}
	token, err := openToken(s.box, userID, kind, row.BaseUrl, row.TokenCiphertext)
	if err != nil {
		s.log.Error("open issue tracker token", "connection", connID.String(), "error", err)
		return noAccount, noProject, noTag, internal()
	}
	if err := validateAccount(account, token); err != nil {
		return noAccount, noProject, noTag, &Error{Kind: KindBadGateway, Message: err.Error()}
	}
	if account.ID != row.AccountID {
		return noAccount, noProject, noTag, errAccountChanged()
	}

	projects, err := d.ListProjects(ctx)
	if err != nil {
		return noAccount, noProject, noTag, &Error{Kind: KindBadGateway, Message: "could not list projects: " + err.Error()}
	}
	var project issuetracker.Project
	found := false
	for _, p := range projects {
		if p.ID == projectID {
			project, found = p, true
			break
		}
	}
	if !found {
		return noAccount, noProject, noTag, &Error{Kind: KindBadRequest, Message: "project not found among the projects this connection can see"}
	}
	if utf8.RuneCountInString(project.ShortName) > maxProjectShortLen ||
		issuetracker.ValidProjectRef(issuetracker.ProjectRef{ID: project.ID, ShortName: project.ShortName}) != nil {
		return noAccount, noProject, noTag, &Error{Kind: KindBadRequest, Message: "the project's short name cannot be used for a link: it must start with a letter, hold only letters, digits and underscores, and be at most 44 characters"}
	}

	tags, err := d.ListTags(ctx)
	if err != nil {
		return noAccount, noProject, noTag, &Error{Kind: KindBadGateway, Message: "could not list tags: " + err.Error()}
	}
	var tag issuetracker.Tag
	found = false
	for _, t := range tags {
		if t.ID == tagID {
			tag, found = t, true
			break
		}
	}
	if !found {
		return noAccount, noProject, noTag, &Error{Kind: KindBadRequest, Message: "tag not found among the tags this connection's account can see"}
	}
	if !issuetracker.ValidTagID(tag.ID) {
		return noAccount, noProject, noTag, &Error{Kind: KindBadRequest, Message: "the tag's id is too long or malformed to store as an eligibility tag"}
	}
	if utf8.RuneCountInString(tag.Name) > maxTagNameLen ||
		issuetracker.ValidTagRef(issuetracker.TagRef{ID: tag.ID, Name: tag.Name}) != nil {
		return noAccount, noProject, noTag, &Error{Kind: KindBadRequest, Message: "the tag's name cannot be used as an eligibility tag: rename it in the issue tracker (no braces, no control characters)"}
	}
	if !issuetracker.AccountCanApply(tag, account) {
		return noAccount, noProject, noTag, conflict("the connection's account cannot apply this tag; in the issue tracker, share the tag's use settings with this account, " +
			"or with All Users, or pick a tag the account owns")
	}
	if !issuetracker.OthersCanApply(tag, account) {
		return noAccount, noProject, noTag, conflict("nobody other than the connection's account can apply this tag; in the issue tracker, " +
			"share the tag's use settings with named users or with All Users. " +
			"This only checks who can apply the tag; it does not show who can edit issues in the project")
	}
	return account, project, tag, nil
}

// TagView is a listed tag with the two sharing verdicts the link requires,
// judged for the connection's stored account.
type TagView struct {
	Tag             issuetracker.Tag
	AccountCanApply bool
	OthersCanApply  bool
}

// ListTagsWithVerdicts lists the tags the connection's token can see, each with
// whether the connection's account can apply it and whether anyone else can.
func (s *Service) ListTagsWithVerdicts(ctx context.Context, userID, connID uuid.UUID) ([]TagView, error) {
	row, err := s.getRow(ctx, userID, connID)
	if err != nil {
		return nil, err
	}
	tags, err := s.ListTags(ctx, userID, connID)
	if err != nil {
		return nil, err
	}
	account := issuetracker.Account{ID: row.AccountID, Login: row.AccountLogin}
	out := make([]TagView, 0, len(tags))
	for _, t := range tags {
		out = append(out, TagView{
			Tag:             t,
			AccountCanApply: issuetracker.AccountCanApply(t, account),
			OthersCanApply:  issuetracker.OthersCanApply(t, account),
		})
	}
	return out, nil
}
