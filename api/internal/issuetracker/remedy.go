package issuetracker

import "errors"

// remedied is an error that names, in the issue tracker's own terms, what to change
// there so a refused operation can succeed.
type remedied struct {
	err    error
	remedy string
}

func (e *remedied) Error() string  { return e.err.Error() }
func (e *remedied) Unwrap() error  { return e.err }
func (e *remedied) Remedy() string { return e.remedy }

// WithRemedy attaches a remedy to err: a fixed sentence the driver writes, naming
// what to change in its issue tracker (PRD #4765 A1). errors.Is and errors.As see
// through it to err. The remedy must be the driver's own text, never remote
// content. A nil err stays nil.
func WithRemedy(err error, remedy string) error {
	if err == nil {
		return nil
	}
	return &remedied{err: err, remedy: remedy}
}

// RemedyOf returns the remedy attached anywhere in err's chain, or "" when there is none.
func RemedyOf(err error) string {
	var r interface{ Remedy() string }
	if errors.As(err, &r) {
		return r.Remedy()
	}
	return ""
}
