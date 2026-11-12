package issuetrackersvc

// Kind classifies a failure so callers can pick a status code.
type Kind int

const (
	// KindBadRequest: the input is invalid or refused (HTTP 400).
	KindBadRequest Kind = iota
	// KindNotFound: the connection is absent or not owned by the user (404).
	KindNotFound
	// KindBadGateway: the issue tracker refused or failed (502).
	KindBadGateway
	// KindInternal: a database or crypto failure, already logged (500).
	KindInternal
)

// Error is the typed failure of every Service method. Message is safe to show
// a client: issue tracker text is token-redacted by the driver, and an
// internal failure carries only a fixed message.
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

func internal() *Error { return &Error{Kind: KindInternal, Message: "internal error"} }

func notFound() *Error { return &Error{Kind: KindNotFound, Message: "connection not found"} }
