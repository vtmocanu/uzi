package issuetrackersvc

import (
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Connection is the client-safe view of a stored connection. It deliberately
// has no token or ciphertext field: nothing the service returns can reveal the
// secret.
type Connection struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	Kind           string
	BaseURL        string
	AccountID      string
	AccountLogin   string
	CreatedAt      time.Time
	LastVerifiedAt time.Time
}

func toConnection(r store.IssueTrackerConnection) Connection {
	return Connection{
		ID:             r.ID,
		UserID:         r.UserID,
		Kind:           r.Kind,
		BaseURL:        r.BaseUrl,
		AccountID:      r.AccountID,
		AccountLogin:   r.AccountLogin,
		CreatedAt:      r.CreatedAt.Time,
		LastVerifiedAt: r.LastVerifiedAt.Time,
	}
}
